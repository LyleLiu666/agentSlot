package standardagent

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
	"github.com/LyleLiu666/agentSlot/tool"
)

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
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("hello"),
	})
	if err != nil || !receipt.MessageID.Valid() {
		t.Fatalf("Send = %#v, %v", receipt, err)
	}
	if err := access.WhenIdle(context.Background(), interaction.WhenIdleRequest{SessionID: opened.SessionID}); err != nil {
		t.Fatalf("WhenIdle: %v", err)
	}
	snapshot, err := access.Snapshot(context.Background(), interaction.SnapshotRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	messages := historyMessageFacts(snapshot.History)
	if snapshot.RunState != session.RunIdle || snapshot.ActiveRunID != "" || len(messages) != 2 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if got := messages[1].Parts[0].Text; got != "finished" {
		t.Fatalf("assistant text = %q", got)
	}
	if messages[1].ID == "" || messages[1].Role != agent.RoleAssistant {
		t.Fatalf("assistant identity was not allocated by Runtime: %#v", messages[1])
	}
	runs := historyRunFacts(snapshot.History)
	if len(runs) != 2 || runs[0].Kind != session.RunStarted || runs[1].Kind != session.RunCompleted || runs[0].ModelConfig.ModelID != "default" {
		t.Fatalf("run facts = %#v", runs)
	}
	requests := fake.Requests()
	if len(requests) != 1 || requests[0].Inputs[0].Message.ID != receipt.MessageID || requests[0].Inputs[0].Message.Parts[0].Text != "hello" {
		t.Fatalf("model requests = %#v", requests)
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
	if err := access.Cancel(context.Background(), interaction.CancelRequest{SessionID: opened.SessionID}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := access.WhenIdle(ctx, interaction.WhenIdleRequest{SessionID: opened.SessionID}); err != nil {
		t.Fatalf("WhenIdle after cancel: %v", err)
	}
	snapshot, err := access.Snapshot(context.Background(), interaction.SnapshotRequest{SessionID: opened.SessionID})
	if err != nil || snapshot.RunState != session.RunIdle || len(historyMessageFacts(snapshot.History)) != 1 {
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
	fake := model.NewFakeModelExecutor(
		model.FakeExecution{Events: []model.ModelEvent{{Kind: model.EventComplete, Output: &model.Completion{
			ToolCalls: []model.ToolCallRequest{{CorrelationID: "provider-call-1", Name: "echo", Arguments: []byte(`{"value":"hello"}`)}},
		}}}},
		model.FakeExecution{Events: []model.ModelEvent{complete("after tool")}},
	)
	access, stop := startRuntimeTestApplication(t, fake, toolModule{key: "echo", value: installed})
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
	snapshot, err := access.Snapshot(ctx, interaction.SnapshotRequest{SessionID: opened.SessionID})
	if err != nil || len(snapshot.History) < 6 {
		t.Fatalf("snapshot = %#v, %v", snapshot, err)
	}
	var callCount, resultCount int
	for _, fact := range snapshot.History {
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
	snapshot, err := access.Snapshot(context.Background(), interaction.SnapshotRequest{SessionID: opened.SessionID})
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
	if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: first.SessionID, ExpectedRevision: first.Revision, Input: textInput("one")}); err != nil {
		t.Fatal(err)
	}
	if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: second.SessionID, ExpectedRevision: second.Revision, Input: textInput("two")}); err != nil {
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
	if err := access.Cancel(context.Background(), interaction.CancelRequest{SessionID: first.SessionID}); err != nil {
		t.Fatal(err)
	}
	if err := access.Cancel(context.Background(), interaction.CancelRequest{SessionID: second.SessionID}); err != nil {
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
	if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("close")}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := fake.WaitForRequests(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := access.CloseSession(ctx, interaction.CloseSessionRequest{SessionID: opened.SessionID}); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	resumed, err := access.ResumeSession(ctx, interaction.ResumeSessionRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatalf("ResumeSession after close: %v", err)
	}
	snapshot, err := access.Snapshot(ctx, interaction.SnapshotRequest{SessionID: resumed.SessionID})
	if err != nil || snapshot.RunState != session.RunIdle || len(historyMessageFacts(snapshot.History)) != 1 {
		t.Fatalf("resumed snapshot = %#v, %v", snapshot, err)
	}
}

func TestRuntimeFailsClosedWhenDurableRunRecoveryFails(t *testing.T) {
	runContext, cancelRun := context.WithCancel(context.Background())
	run := &activeRun{id: "run-1", ctx: runContext, cancel: cancelRun, done: make(chan struct{})}
	runtime := &runtimeInstance{
		session:    fakeSession{id: "session-1", revision: 1},
		components: &runtimeComponents{store: recoveryFailStore{fakeStore: fakeStore{}}},
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
	err := runtime.cancel(context.Background(), interaction.CancelRequest{SessionID: "session-1"})
	if !agent.IsCode(err, agent.CodeRuntimeClosed) {
		t.Fatalf("command after failed recovery error = %v, code=%q", err, agent.CodeOf(err))
	}
}

type recoveryFailStore struct{ fakeStore }

func (recoveryFailStore) Recover(context.Context, session.SessionRef) (session.Snapshot, error) {
	return session.Snapshot{}, errors.New("recovery failed")
}

func startRuntimeTestApplication(t *testing.T, executor model.ModelExecutor, extra ...agentslot.Module) (interaction.GatewayAccess, func()) {
	t.Helper()
	memory, err := session.NewMemoryModule(model.Config{ModelID: "default", Reasoning: model.ReasoningDefault})
	if err != nil {
		t.Fatal(err)
	}
	entry := &captureEntrypoint{}
	modules := []agentslot.Module{memory, executorModule{executor: executor}}
	modules = append(modules, extra...)
	modules = append(modules, NewEntrypointModule("entrypoint.runtime-test", "test", entry))
	application := NewApplication(ApplicationSpec{Name: "runtime-test", Modules: modules})
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

type executorModule struct{ executor model.ModelExecutor }

func (executorModule) ID() string { return "test.executor" }
func (m executorModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Set(model.ExecutorSlot, m.executor))
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

func historyRunFacts(history []session.HistoryFact) []session.RunFact {
	runs := make([]session.RunFact, 0)
	for _, fact := range history {
		if fact.Run != nil {
			runs = append(runs, *fact.Run)
		}
	}
	return runs
}
