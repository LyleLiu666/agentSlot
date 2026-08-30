package standardagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/artifact"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/policy"
	"github.com/LyleLiu666/agentSlot/session"
	"github.com/LyleLiu666/agentSlot/tool"
)

const testMaxInlineToolResultBytes = 64 << 10

func TestRuntimeSendCommitsOnlyCompleteModelOutput(t *testing.T) {
	fake := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{
		{Kind: model.EventDelta, Text: "temporary"},
		{Kind: model.EventReset},
		complete("finished"),
	}})
	access, stop := startRuntimeTestApplication(t, fake)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	receipt, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision,
		ClientMessageID: "client-user-1", Input: textInput("hello"),
	})
	if err != nil || !receipt.MessageID.Valid() {
		t.Fatalf("Send = %#v, %v", receipt, err)
	}
	if err := access.WhenIdle(context.Background(), interaction.WhenIdleRequest{SessionID: opened.SessionID}); err != nil {
		t.Fatalf("WhenIdle: %v", err)
	}
	snapshot, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	messages := historyMessageFacts(snapshot.RecentHistory)
	if snapshot.RunState != session.RunIdle || snapshot.ActiveRunID != "" || len(messages) != 2 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if got := messages[1].Parts[0].Text; got != "finished" {
		t.Fatalf("assistant text = %q", got)
	}
	if messages[1].ID == "" || messages[1].Role != agent.RoleAssistant {
		t.Fatalf("assistant identity was not allocated by Runtime: %#v", messages[1])
	}
	if messages[0].ClientMessageID != "client-user-1" {
		t.Fatalf("user ClientMessageID = %q, want durable client correlation", messages[0].ClientMessageID)
	}
	runs := historyRunFacts(snapshot.RecentHistory)
	if len(runs) != 2 || runs[0].Kind != session.RunStarted || runs[1].Kind != session.RunCompleted || runs[0].ModelConfig.ModelID != "default" {
		t.Fatalf("run facts = %#v", runs)
	}
	requests := fake.Requests()
	if len(requests) != 1 || requests[0].Inputs[0].Message.ID != receipt.MessageID || requests[0].Inputs[0].Message.Parts[0].Text != "hello" {
		t.Fatalf("model requests = %#v", requests)
	}
}

func TestRuntimeRejectsAnInvalidOptionalClientMessageID(t *testing.T) {
	access, stop := startRuntimeTestApplication(t, model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{complete("unused")}}))
	defer stop()
	opened := createRuntimeTestSession(t, access)
	_, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision,
		ClientMessageID: " invalid ", Input: textInput("hello"),
	})
	if !agent.IsKind(err, agent.ErrorInvalidInput) {
		t.Fatalf("Send() error = %v, want invalid input", err)
	}
}

func TestRuntimeResumesPreparedToolCallAfterRecoveryWithoutReplacingItsIdentity(t *testing.T) {
	store := session.NewMemoryStore()
	config := model.Config{ModelID: "default", Reasoning: model.ReasoningDefault}
	assistant := agent.Message{
		ID: "message-approval", SessionID: "session-approval", RunID: "run-approval", StepID: "step-approval",
		Role: agent.RoleAssistant,
	}
	call := agent.ToolCall{
		ID: "call-approval", MessageID: assistant.ID, SessionID: assistant.SessionID,
		RunID: assistant.RunID, StepID: assistant.StepID, Name: "effect", Arguments: []byte(`{"value":"original"}`),
	}
	started := session.RunFact{
		SessionID: assistant.SessionID, RunID: call.RunID, Kind: session.RunStarted,
		ModelConfig: config, ConfigRevision: 1,
	}
	prepared := session.JournalEntry{
		RunID: call.RunID, StepID: call.StepID, ToolCall: &call, Status: session.JournalPrepared,
	}
	if _, err := store.Create(context.Background(), session.NewSession{
		Session:    agent.Session{ID: assistant.SessionID, AgentID: "agent", WorkspaceID: "workspace"},
		History:    []session.HistoryFact{{Run: &started}, {Message: &assistant}, {ToolCall: &call}},
		RunJournal: []session.JournalEntry{prepared}, ModelConfig: config,
		RunState: session.RunRunning, ActiveRunID: call.RunID,
	}); err != nil {
		t.Fatalf("seed prepared call: %v", err)
	}

	effect := &journalCheckingTool{
		definition: testToolDefinition(t, "effect"), store: store, sessionID: assistant.SessionID, t: t,
	}
	var approvalCalls atomic.Int64
	guard := policy.GuardFunc(func(context.Context, policy.Action) (policy.Decision, error) {
		return policy.Decision{Effect: policy.RequireApproval, Reason: "external effect"}, nil
	})
	approval := policy.ApprovalFunc(func(_ context.Context, request policy.ApprovalRequest) (policy.ApprovalDecision, error) {
		approvalCalls.Add(1)
		if request.Action.Tool.Call.ID != call.ID {
			t.Errorf("approval CallID = %q, want %q", request.Action.Tool.Call.ID, call.ID)
		}
		return policy.ApprovalDecision{Approved: true}, nil
	})
	executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{complete("continued")}})
	entry := &captureChannel{}
	application := NewApplication(ApplicationSpec{
		Name: "approval-recovery", DefaultModelConfig: config,
		RuntimeConfig: AgentRuntimeConfig{ToolKeys: []string{"effect"}, MaxInlineToolResultBytes: testMaxInlineToolResultBytes},
		Modules: []agentslot.Module{
			componentsModule{store: store, executor: executor},
			toolModule{key: "effect", value: effect},
			policyModule{guard: guard, approval: approval},
			NewGatewayChannelModule("entrypoint.approval-recovery", "test", entry),
		},
	})
	running, err := application.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = running.Stop(context.Background()) }()
	if _, err := entry.Access().ResumeSession(context.Background(), interaction.ResumeSessionRequest{SessionID: assistant.SessionID}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := entry.Access().WhenIdle(ctx, interaction.WhenIdleRequest{SessionID: assistant.SessionID}); err != nil {
		t.Fatalf("wait for resumed call: %v", err)
	}
	snapshot, err := store.Load(context.Background(), session.SessionRef{SessionID: assistant.SessionID})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if approvalCalls.Load() != 1 || effect.calls.Load() != 1 {
		t.Fatalf("approval/tool calls = %d/%d, want one each", approvalCalls.Load(), effect.calls.Load())
	}
	if len(snapshot.RunJournal) != 1 || snapshot.RunJournal[0].ToolCall.ID != call.ID || snapshot.RunJournal[0].Status != session.JournalSucceeded {
		t.Fatalf("resumed journal = %#v", snapshot.RunJournal)
	}
}

func TestRuntimeRejectsModelConfigChangeWhileRunningAndCancelsCleanly(t *testing.T) {
	block := make(chan struct{})
	fake := model.NewFakeModelExecutor(model.FakeExecution{Block: block, Events: []model.ModelEvent{complete("late")}})
	access, stop := startRuntimeTestApplication(t, fake)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	receipt, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("wait"),
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := fake.WaitForRequests(ctx, 1); err != nil {
		t.Fatalf("wait for model request: %v", err)
	}
	_, err = access.UpdateModelConfig(context.Background(), interaction.UpdateModelConfigRequest{
		SessionID: opened.SessionID, ExpectedRevision: receipt.Revision,
		Config: model.Config{ModelID: "other", Reasoning: model.ReasoningDefault},
	})
	if !agent.IsCode(err, agent.CodeActiveRun) {
		t.Fatalf("UpdateModelConfig error = %v, code=%q", err, agent.CodeOf(err))
	}
	if err := access.Cancel(context.Background(), interaction.CancelRequest{SessionID: opened.SessionID, ExpectedRevision: receipt.Revision}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := access.WhenIdle(ctx, interaction.WhenIdleRequest{SessionID: opened.SessionID}); err != nil {
		t.Fatalf("WhenIdle after cancel: %v", err)
	}
	snapshot, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil || snapshot.RunState != session.RunIdle || len(historyMessageFacts(snapshot.RecentHistory)) != 1 {
		t.Fatalf("snapshot after cancel = %#v, %v", snapshot, err)
	}
}

func TestRuntimeUpdatesModelConfigWhileIdleAndFreezesItForNextRun(t *testing.T) {
	fake := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{complete("done")}})
	access, stop := startRuntimeTestApplication(t, fake)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	updated, err := access.UpdateModelConfig(context.Background(), interaction.UpdateModelConfigRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision,
		Config: model.Config{ProviderKey: "provider-2", ModelID: "model-2", Reasoning: model.ReasoningHigh},
	})
	if err != nil {
		t.Fatalf("UpdateModelConfig: %v", err)
	}
	_, err = access.UpdateModelConfig(context.Background(), interaction.UpdateModelConfigRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision,
		Config: model.Config{ModelID: "stale", Reasoning: model.ReasoningDefault},
	})
	if !agent.IsCode(err, agent.CodeRevisionConflict) {
		t.Fatalf("stale UpdateModelConfig error = %v, code=%q", err, agent.CodeOf(err))
	}
	if _, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: updated.Revision, Input: textInput("use new model"),
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := access.WhenIdle(ctx, interaction.WhenIdleRequest{SessionID: opened.SessionID}); err != nil {
		t.Fatal(err)
	}
	requests := fake.Requests()
	if len(requests) != 1 || requests[0].Config.ProviderKey != "provider-2" || requests[0].Config.ModelID != "model-2" || requests[0].ConfigRevision != updated.Revision {
		t.Fatalf("frozen request config = %#v", requests)
	}
}

func TestRuntimeSteerContinuesSameRunWithFrozenModelConfig(t *testing.T) {
	firstBlock := make(chan struct{})
	fake := model.NewFakeModelExecutor(
		model.FakeExecution{Block: firstBlock, Events: []model.ModelEvent{complete("first")}},
		model.FakeExecution{Events: []model.ModelEvent{complete("second")}},
	)
	access, stop := startRuntimeTestApplication(t, fake)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	first, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("start"),
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := fake.WaitForRequests(ctx, 1); err != nil {
		t.Fatal(err)
	}
	steer, err := access.Steer(context.Background(), interaction.SteerRequest{
		SessionID: opened.SessionID, ExpectedRevision: first.Revision, Input: textInput("correct this"),
	})
	if err != nil {
		t.Fatalf("Steer: %v", err)
	}
	close(firstBlock)
	if err := access.WhenIdle(ctx, interaction.WhenIdleRequest{SessionID: opened.SessionID}); err != nil {
		t.Fatalf("WhenIdle: %v", err)
	}
	requests := fake.Requests()
	if len(requests) != 2 || requests[0].RunID != requests[1].RunID || requests[0].Config != requests[1].Config || requests[0].ConfigRevision != requests[1].ConfigRevision {
		t.Fatalf("run/config changed across steer: %#v", requests)
	}
	if requests[1].Inputs[len(requests[1].Inputs)-1].Message.ID != steer.MessageID {
		t.Fatalf("steer was not included in next step: %#v", requests[1].Inputs)
	}
}

func TestRuntimeCommitsToolCallAndResultThenContinuesModel(t *testing.T) {
	installed := &countingTool{definition: testToolDefinition(t, "echo")}
	continuation := json.RawMessage(`[{"type":"opaque","signature":"provider-owned"}]`)
	fake := model.NewFakeModelExecutor(
		model.FakeExecution{Events: []model.ModelEvent{{Kind: model.EventComplete, Output: &model.Completion{
			ToolCalls:    []model.ToolCallRequest{{CorrelationID: "provider-call-1", Name: "echo", Arguments: []byte(`{"value":"hello"}`)}},
			Continuation: continuation,
		}}}},
		model.FakeExecution{Events: []model.ModelEvent{complete("after tool")}},
	)
	access, stop := startRuntimeTestApplicationWithConfig(t, fake, AgentRuntimeConfig{ToolKeys: []string{"echo"}}, toolModule{key: "echo", value: installed})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("use tool")}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := access.WhenIdle(ctx, interaction.WhenIdleRequest{SessionID: opened.SessionID}); err != nil {
		t.Fatal(err)
	}
	if installed.calls.Load() != 1 {
		t.Fatalf("tool calls = %d", installed.calls.Load())
	}
	requests := fake.Requests()
	if len(requests) != 2 || requests[0].RunID != requests[1].RunID || len(requests[0].Tools) != 1 {
		t.Fatalf("model requests = %#v", requests)
	}
	if len(requests[1].Inputs) < 4 || requests[1].Inputs[len(requests[1].Inputs)-2].ToolCall == nil || requests[1].Inputs[len(requests[1].Inputs)-2].ToolCall.CorrelationID != "provider-call-1" || requests[1].Inputs[len(requests[1].Inputs)-1].ToolResult == nil {
		t.Fatalf("second request lacks tool exchange: %#v", requests[1].Inputs)
	}
	assistant := requests[1].Inputs[len(requests[1].Inputs)-3].Message
	if assistant == nil || assistant.ModelContinuation == nil ||
		assistant.ModelContinuation.ProviderKey != requests[0].Config.ProviderKey ||
		assistant.ModelContinuation.ModelID != requests[0].Config.ModelID ||
		!bytes.Equal(assistant.ModelContinuation.State, continuation) {
		t.Fatalf("second request lost provider-owned continuation: %#v", assistant)
	}
	snapshot, err := access.View(ctx, interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil || len(snapshot.RecentHistory) < 6 {
		t.Fatalf("snapshot = %#v, %v", snapshot, err)
	}
	var callCount, resultCount int
	for _, fact := range snapshot.RecentHistory {
		if fact.ToolCall != nil {
			callCount++
		}
		if fact.ToolResult != nil {
			resultCount++
		}
	}
	if callCount != 1 || resultCount != 1 {
		t.Fatalf("tool facts = calls %d results %d", callCount, resultCount)
	}
}

func TestRuntimeRejectsOversizedToolResultWithoutRetryingSideEffect(t *testing.T) {
	installed := &budgetProbeTool{definition: testToolDefinition(t, "oversized"), oversize: true}
	fake := model.NewFakeModelExecutor(
		model.FakeExecution{Events: []model.ModelEvent{{Kind: model.EventComplete, Output: &model.Completion{ToolCalls: []model.ToolCallRequest{{Name: "oversized", Arguments: []byte(`{"value":"run"}`)}}}}}},
		model.FakeExecution{Events: []model.ModelEvent{complete("must not continue")}},
	)
	access, stop := startRuntimeTestApplicationWithConfig(t, fake, AgentRuntimeConfig{ToolKeys: []string{"oversized"}, MaxInlineToolResultBytes: 16}, toolModule{key: "oversized", value: installed})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("run")}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	if installed.calls.Load() != 1 || installed.budget.Load() != 16 || len(fake.Requests()) != 1 {
		t.Fatalf("calls=%d budget=%d model requests=%d", installed.calls.Load(), installed.budget.Load(), len(fake.Requests()))
	}
	snapshot, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	results := historyToolResultFacts(snapshot.RecentHistory)
	if len(results) != 1 || results[0].Status != tool.ResultFailed || results[0].Error == nil || results[0].Error.Code != "invalid_tool_result" {
		t.Fatalf("persisted contract violation = %#v", results)
	}
	if lastRunTerminal(snapshot.RecentHistory) != session.RunFailed {
		t.Fatalf("run terminal = %q, want failed", lastRunTerminal(snapshot.RecentHistory))
	}
}

func TestRuntimeAndForkPreserveStandardArtifactReferences(t *testing.T) {
	reference := artifact.Metadata{ID: "artifact-full-output", MediaType: "text/plain", Name: "full.txt", Size: 4096}
	installed := &budgetProbeTool{definition: testToolDefinition(t, "bounded"), reference: &reference}
	fake := model.NewFakeModelExecutor(
		model.FakeExecution{Events: []model.ModelEvent{{Kind: model.EventComplete, Output: &model.Completion{ToolCalls: []model.ToolCallRequest{{Name: "bounded", Arguments: []byte(`{"value":"run"}`)}}}}}},
		model.FakeExecution{Events: []model.ModelEvent{complete("done")}},
	)
	access, stop := startRuntimeTestApplicationWithConfig(t, fake, AgentRuntimeConfig{ToolKeys: []string{"bounded"}, MaxInlineToolResultBytes: 64}, toolModule{key: "bounded", value: installed})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("run")}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	source, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	results := historyToolResultFacts(source.RecentHistory)
	if len(results) != 1 || len(results[0].Artifacts) != 1 || results[0].Artifacts[0] != reference {
		t.Fatalf("source Artifact references = %#v", results)
	}
	forked, err := access.ForkSession(context.Background(), interaction.ForkSessionRequest{SourceSessionID: opened.SessionID, Mode: session.ForkFullHistory, AgentID: "agent-1", WorkspaceID: "workspace-1"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: forked.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	childResults := historyToolResultFacts(child.RecentHistory)
	if len(childResults) != 1 || len(childResults[0].Artifacts) != 1 || childResults[0].Artifacts[0] != reference {
		t.Fatalf("fork Artifact references = %#v", childResults)
	}
}

func TestGatewayForksEmptyHistoryPrefixWhileSourceRunIsActive(t *testing.T) {
	block := make(chan struct{})
	fake := model.NewFakeModelExecutor(model.FakeExecution{
		Block:  block,
		Events: []model.ModelEvent{complete("done")},
	})
	access, stop := startRuntimeTestApplication(t, fake)
	defer func() {
		close(block)
		stop()
	}()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("first request"),
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := fake.WaitForRequests(ctx, 1); err != nil {
		t.Fatal(err)
	}
	forked, err := access.ForkSession(context.Background(), interaction.ForkSessionRequest{
		SourceSessionID: opened.SessionID,
		Mode:            session.ForkHistoryPrefix,
		CutoffSequence:  0,
		AgentID:         "agent-1",
		WorkspaceID:     "workspace-1",
	})
	if err != nil {
		t.Fatalf("fork empty prefix through Gateway: %v", err)
	}
	child, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: forked.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(child.RecentHistory) != 0 || child.RunState != session.RunIdle || child.ActiveRunID.Valid() {
		t.Fatalf("Gateway child = history %d state %q active %q", len(child.RecentHistory), child.RunState, child.ActiveRunID)
	}
}

func TestRuntimeFailureLeavesQueuedNormalForExplicitRunPending(t *testing.T) {
	block := make(chan struct{})
	fake := model.NewFakeModelExecutor(
		model.FakeExecution{Block: block, Events: []model.ModelEvent{{Kind: model.EventFailed, Err: errors.New("provider down")}}},
		model.FakeExecution{Events: []model.ModelEvent{complete("recovered")}},
	)
	access, stop := startRuntimeTestApplication(t, fake)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	first, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("first"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := fake.WaitForRequests(ctx, 1); err != nil {
		t.Fatal(err)
	}
	queued, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: first.Revision, Input: textInput("second"),
	})
	if err != nil {
		t.Fatal(err)
	}
	close(block)
	if err := access.WhenIdle(ctx, interaction.WhenIdleRequest{SessionID: opened.SessionID}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil || len(snapshot.Queue) != 1 || snapshot.Queue[0].Message.ID != queued.MessageID {
		t.Fatalf("failed run queue = %#v, %v", snapshot.Queue, err)
	}
	if _, err := access.RunPending(context.Background(), interaction.RunPendingRequest{
		SessionID: opened.SessionID, ExpectedRevision: snapshot.Revision,
	}); err != nil {
		t.Fatalf("RunPending: %v", err)
	}
	if err := access.WhenIdle(ctx, interaction.WhenIdleRequest{SessionID: opened.SessionID}); err != nil {
		t.Fatal(err)
	}
	if got := len(fake.Requests()); got != 2 {
		t.Fatalf("model request count = %d", got)
	}
}

func TestRuntimeSerializesRunsWithinSessionAndAutoStartsNextNormal(t *testing.T) {
	firstBlock := make(chan struct{})
	fake := model.NewFakeModelExecutor(
		model.FakeExecution{Block: firstBlock, Events: []model.ModelEvent{complete("one")}},
		model.FakeExecution{Events: []model.ModelEvent{complete("two")}},
	)
	access, stop := startRuntimeTestApplication(t, fake)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	first, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("one"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := fake.WaitForRequests(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: first.Revision, Input: textInput("two"),
	}); err != nil {
		t.Fatal(err)
	}
	if got := len(fake.Requests()); got != 1 {
		t.Fatalf("second normal started a concurrent Run; requests = %d", got)
	}
	close(firstBlock)
	if err := access.WhenIdle(ctx, interaction.WhenIdleRequest{SessionID: opened.SessionID}); err != nil {
		t.Fatal(err)
	}
	requests := fake.Requests()
	if len(requests) != 2 || requests[0].RunID == requests[1].RunID {
		t.Fatalf("normal inputs did not produce two serialized Runs: %#v", requests)
	}
}

func TestRuntimeAllowsDifferentSessionsToRunConcurrently(t *testing.T) {
	firstBlock := make(chan struct{})
	secondBlock := make(chan struct{})
	fake := model.NewFakeModelExecutor(
		model.FakeExecution{Block: firstBlock, Events: []model.ModelEvent{complete("one")}},
		model.FakeExecution{Block: secondBlock, Events: []model.ModelEvent{complete("two")}},
	)
	access, stop := startRuntimeTestApplication(t, fake)
	defer stop()
	first := createRuntimeTestSession(t, access)
	second := createRuntimeTestSession(t, access)
	firstReceipt, err := access.Send(context.Background(), interaction.SendRequest{SessionID: first.SessionID, ExpectedRevision: first.Revision, Input: textInput("one")})
	if err != nil {
		t.Fatal(err)
	}
	secondReceipt, err := access.Send(context.Background(), interaction.SendRequest{SessionID: second.SessionID, ExpectedRevision: second.Revision, Input: textInput("two")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := fake.WaitForRequests(ctx, 2); err != nil {
		t.Fatalf("second Session did not execute concurrently: %v", err)
	}
	requests := fake.Requests()
	if requests[0].SessionID == requests[1].SessionID {
		t.Fatalf("requests were not isolated by Session: %#v", requests)
	}
	if err := access.Cancel(context.Background(), interaction.CancelRequest{SessionID: first.SessionID, ExpectedRevision: firstReceipt.Revision}); err != nil {
		t.Fatal(err)
	}
	if err := access.Cancel(context.Background(), interaction.CancelRequest{SessionID: second.SessionID, ExpectedRevision: secondReceipt.Revision}); err != nil {
		t.Fatal(err)
	}
	if err := access.WhenIdle(ctx, interaction.WhenIdleRequest{SessionID: first.SessionID}); err != nil {
		t.Fatal(err)
	}
	if err := access.WhenIdle(ctx, interaction.WhenIdleRequest{SessionID: second.SessionID}); err != nil {
		t.Fatal(err)
	}
}

func TestCloseSessionCancelsRunWithoutDeletingDurableSession(t *testing.T) {
	block := make(chan struct{})
	fake := model.NewFakeModelExecutor(model.FakeExecution{Block: block, Events: []model.ModelEvent{complete("late")}})
	access, stop := startRuntimeTestApplication(t, fake)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	receipt, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("close")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := fake.WaitForRequests(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := access.CloseSession(ctx, interaction.CloseSessionRequest{SessionID: opened.SessionID, ExpectedRevision: receipt.Revision}); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	resumed, err := access.ResumeSession(ctx, interaction.ResumeSessionRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatalf("ResumeSession after close: %v", err)
	}
	snapshot, err := access.View(ctx, interaction.SessionViewRequest{SessionID: resumed.SessionID})
	if err != nil || snapshot.RunState != session.RunIdle || len(historyMessageFacts(snapshot.RecentHistory)) != 1 {
		t.Fatalf("resumed snapshot = %#v, %v", snapshot, err)
	}
}

func TestRuntimeFailsClosedWhenDurableRunRecoveryFails(t *testing.T) {
	runContext, cancelRun := context.WithCancel(context.Background())
	run := &activeRun{id: "run-1", ctx: runContext, cancel: cancelRun, done: make(chan struct{})}
	manager, err := session.NewManager(session.NewMemoryStore(), testDefaultModel())
	if err != nil {
		t.Fatalf("new fixed Manager: %v", err)
	}
	managedSession, err := manager.Create(context.Background(), session.CreateRequest{AgentID: "agent-1", WorkspaceID: "workspace-1"})
	if err != nil {
		t.Fatalf("create Session: %v", err)
	}
	runtime := &runtimeInstance{
		session:    managedSession,
		components: &runtimeComponents{store: recoveryFailStore{SessionStore: session.NewMemoryStore()}},
		state:      runtimeRunning, active: run, idleSignal: make(chan struct{}), closeDone: make(chan struct{}),
	}
	runtime.mu.Lock()
	runtime.recoverAfterRunFailureLocked(run)
	runtime.mu.Unlock()
	if runtime.state != runtimeClosed {
		t.Fatalf("Runtime state = %q, want closed", runtime.state)
	}
	select {
	case <-runtime.closeDone:
	default:
		t.Fatal("failed-closed Runtime did not close its lifecycle signal")
	}
	err = runtime.cancel(context.Background(), interaction.CancelRequest{SessionID: managedSession.ID()})
	if !agent.IsCode(err, agent.CodeRuntimeClosed) {
		t.Fatalf("command after failed recovery error = %v, code=%q", err, agent.CodeOf(err))
	}
}

type recoveryFailStore struct{ session.SessionStore }

func (recoveryFailStore) Recover(context.Context, session.SessionRef) (session.Snapshot, error) {
	return session.Snapshot{}, errors.New("recovery failed")
}

func startRuntimeTestApplication(t *testing.T, executor model.ModelExecutor, extra ...agentslot.Module) (interaction.GatewayAccess, func()) {
	t.Helper()
	return startRuntimeTestApplicationWithConfig(t, executor, AgentRuntimeConfig{}, extra...)
}

func startRuntimeTestApplicationWithConfig(t *testing.T, executor model.ModelExecutor, config AgentRuntimeConfig, extra ...agentslot.Module) (interaction.GatewayAccess, func()) {
	t.Helper()
	if len(config.ToolKeys) > 0 && config.MaxInlineToolResultBytes == 0 {
		config.MaxInlineToolResultBytes = testMaxInlineToolResultBytes
	}
	memory := session.NewMemoryModule()
	entry := &captureChannel{}
	modules := []agentslot.Module{memory, executorModule{executor: executor}}
	modules = append(modules, extra...)
	modules = append(modules, NewGatewayChannelModule("entrypoint.runtime-test", "test", entry))
	application := NewApplication(ApplicationSpec{Name: "runtime-test", Modules: modules, RuntimeConfig: config, DefaultModelConfig: model.Config{ModelID: "default", Reasoning: model.ReasoningDefault}})
	running, err := application.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return entry.Access(), func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := running.Stop(ctx); err != nil {
			t.Errorf("stop: %v", err)
		}
	}
}

type executorModule struct {
	executor model.ModelExecutor
	counter  model.TokenCounter
}

func (executorModule) ID() string { return "test.executor" }
func (m executorModule) Register(reg agentslot.Registrar) error {
	counter := m.counter
	if counter == nil {
		counter = model.NewFakeTokenCounter()
	}
	return reg.Contribute(
		agentslot.Set(model.ExecutorSlot, m.executor),
		agentslot.Set(model.TokenCounterSlot, counter),
	)
}

type toolModule struct {
	key   string
	value tool.Tool
}

func (m toolModule) ID() string { return "test.tool." + m.key }
func (m toolModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Add(tool.ToolSlot, m.key, m.value))
}

type countingTool struct {
	definition tool.Definition
	calls      atomic.Int64
}

type budgetProbeTool struct {
	definition tool.Definition
	oversize   bool
	reference  *artifact.Metadata
	calls      atomic.Int64
	budget     atomic.Int64
}

func (t *budgetProbeTool) Definition() tool.Definition       { return t.definition }
func (*budgetProbeTool) ParallelSafety() tool.ParallelSafety { return tool.Serial }
func (t *budgetProbeTool) Invoke(_ context.Context, invocation tool.ToolInvocation) tool.ToolResult {
	t.calls.Add(1)
	t.budget.Store(int64(invocation.MaxInlineOutputBytes))
	output := json.RawMessage(`{"preview":"ok"}`)
	if t.oversize {
		output = json.RawMessage(`{"output":"this result is deliberately too large"}`)
	}
	result := tool.ToolResult{CallID: invocation.Call.ID, Status: tool.ResultSucceeded, Output: output}
	if t.reference != nil {
		result.Artifacts = []artifact.Metadata{*t.reference}
	}
	return result
}

type journalCheckingTool struct {
	definition tool.Definition
	store      session.SessionStore
	sessionID  agent.SessionID
	t          *testing.T
	calls      atomic.Int64
}

func (t *journalCheckingTool) Definition() tool.Definition       { return t.definition }
func (*journalCheckingTool) ParallelSafety() tool.ParallelSafety { return tool.Serial }
func (t *journalCheckingTool) Invoke(ctx context.Context, invocation tool.ToolInvocation) tool.ToolResult {
	t.calls.Add(1)
	snapshot, err := t.store.Load(ctx, session.SessionRef{SessionID: t.sessionID})
	if err != nil {
		t.t.Errorf("load journal at invocation: %v", err)
	} else if len(snapshot.RunJournal) != 1 || snapshot.RunJournal[0].Status != session.JournalPending {
		t.t.Errorf("journal at invocation = %#v, want pending", snapshot.RunJournal)
	}
	return tool.ToolResult{CallID: invocation.Call.ID, Status: tool.ResultSucceeded}
}

func (t *countingTool) Definition() tool.Definition       { return t.definition }
func (*countingTool) ParallelSafety() tool.ParallelSafety { return tool.ParallelSafe }
func (t *countingTool) Invoke(_ context.Context, invocation tool.ToolInvocation) tool.ToolResult {
	t.calls.Add(1)
	return tool.ToolResult{CallID: invocation.Call.ID, Status: tool.ResultSucceeded, Output: json.RawMessage(`{"echo":"hello"}`)}
}

func testToolDefinition(t *testing.T, name string) tool.Definition {
	t.Helper()
	schema, err := tool.ParseInputSchema([]byte(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`))
	if err != nil {
		t.Fatal(err)
	}
	return tool.Definition{Name: name, Description: "test tool", InputSchema: schema}
}

func createRuntimeTestSession(t *testing.T, access interaction.GatewayAccess) interaction.SessionOpened {
	t.Helper()
	opened, err := access.CreateSession(context.Background(), interaction.CreateSessionRequest{AgentID: "agent-1", WorkspaceID: "workspace-1"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return opened
}

func textInput(value string) agent.MessageInput {
	return agent.MessageInput{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: value}}}
}

func complete(value string) model.ModelEvent {
	return model.ModelEvent{Kind: model.EventComplete, Output: &model.Completion{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: value}}}}
}

func historyMessageFacts(history []session.HistoryFact) []agent.Message {
	messages := make([]agent.Message, 0)
	for _, fact := range history {
		if fact.Message != nil {
			messages = append(messages, *fact.Message)
		}
	}
	return messages
}

func historyToolResultFacts(history []session.HistoryFact) []tool.ToolResult {
	var results []tool.ToolResult
	for _, fact := range history {
		if fact.ToolResult != nil {
			results = append(results, *fact.ToolResult)
		}
	}
	return results
}

func historyRunFacts(history []session.HistoryFact) []session.RunFact {
	runs := make([]session.RunFact, 0)
	for _, fact := range history {
		if fact.Run != nil {
			runs = append(runs, *fact.Run)
		}
	}
	return runs
}
