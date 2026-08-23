package sessionhistory

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/artifact"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
	"github.com/LyleLiu666/agentSlot/tool"
)

func TestToolProjectsCompleteModelSafeFactsAndTraceability(t *testing.T) {
	reader := &fakeReader{pages: map[agent.SessionID]session.HistoryPage{
		"session-current": historyFixture("agent-1", "workspace-1"),
	}}
	installed, err := New(reader, Config{Scope: ScopeSameWorkspace})
	if err != nil {
		t.Fatal(err)
	}
	result := installed.Invoke(context.Background(), invocation("session-current", `{"before_sequence":20,"step_limit":3}`, 64<<10))
	if result.Status != tool.ResultSucceeded {
		t.Fatalf("Invoke = %#v", result)
	}
	if reader.last.SessionID != "session-current" || reader.last.BeforeHistorySequence != 20 || reader.last.StepLimit != 3 {
		t.Fatalf("HistoryPage request = %#v", reader.last)
	}
	text := string(result.Output)
	for _, expected := range []string{"session-current", `"revision":7`, `"sequence":2`, `"role":"assistant"`, `"name":"echo"`, `"arguments":{"value":"one"}`, "artifact-result", `"used_tokens":100`} {
		if !strings.Contains(text, expected) {
			t.Errorf("safe projection missing %q: %s", expected, text)
		}
	}
	for _, forbidden := range []string{"continuation-secret", "provider-request-secret", "actor-secret", "context-secret", "attempt-secret"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("safe projection exposed %q: %s", forbidden, text)
		}
	}
	var decoded map[string]any
	if err := json.Unmarshal(result.Output, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["first_sequence"] != float64(1) || decoded["last_sequence"] != float64(9) || decoded["before_sequence"] != float64(1) || decoded["has_more"] != true {
		t.Fatalf("traceability = %#v", decoded)
	}
}

func TestToolEnforcesConfiguredScopeAndNonDisclosure(t *testing.T) {
	pages := map[agent.SessionID]session.HistoryPage{
		"session-current": minimalPage("agent-1", "workspace-1", "current"),
		"session-peer":    minimalPage("agent-1", "workspace-1", "peer"),
		"session-other":   minimalPage("agent-1", "workspace-2", "other"),
	}
	t.Run("current session does not probe another target", func(t *testing.T) {
		reader := &fakeReader{pages: pages}
		installed, err := New(reader, Config{Scope: ScopeCurrentSession})
		if err != nil {
			t.Fatal(err)
		}
		result := installed.Invoke(context.Background(), invocation("session-current", `{"session_id":"session-peer"}`, 4096))
		assertFailureCode(t, result, "access_denied")
		if reader.calls != 0 {
			t.Fatalf("denied target caused %d reads", reader.calls)
		}
	})
	t.Run("same workspace permits peer and authorizer may narrow", func(t *testing.T) {
		reader := &fakeReader{pages: pages}
		var access AccessRequest
		installed, err := New(reader, Config{Scope: ScopeSameWorkspace, Authorizer: AuthorizerFunc(func(_ context.Context, request AccessRequest) error {
			access = request
			return nil
		})})
		if err != nil {
			t.Fatal(err)
		}
		result := installed.Invoke(context.Background(), invocation("session-current", `{"session_id":"session-peer"}`, 4096))
		if result.Status != tool.ResultSucceeded || access.TargetSessionID != "session-peer" || access.Actor.ID != "agent-1" {
			t.Fatalf("peer result=%#v access=%#v", result, access)
		}
	})
	t.Run("cross workspace and unknown targets are indistinguishable", func(t *testing.T) {
		reader := &fakeReader{pages: pages}
		installed, err := New(reader, Config{Scope: ScopeSameWorkspace})
		if err != nil {
			t.Fatal(err)
		}
		cross := installed.Invoke(context.Background(), invocation("session-current", `{"session_id":"session-other"}`, 4096))
		unknown := installed.Invoke(context.Background(), invocation("session-current", `{"session_id":"session-unknown"}`, 4096))
		assertFailureCode(t, cross, "access_denied")
		assertFailureCode(t, unknown, "access_denied")
		if *cross.Error != *unknown.Error {
			t.Fatalf("denials differ: %#v / %#v", cross.Error, unknown.Error)
		}
	})
	t.Run("full access requires and obeys explicit authorization", func(t *testing.T) {
		if _, err := New(&fakeReader{pages: pages}, Config{Scope: ScopeFullAccess}); err == nil {
			t.Fatal("full_access accepted no Authorizer")
		}
		installed, err := New(&fakeReader{pages: pages}, Config{Scope: ScopeFullAccess, Authorizer: AuthorizerFunc(func(context.Context, AccessRequest) error {
			return errors.New("denied privately")
		})})
		if err != nil {
			t.Fatal(err)
		}
		result := installed.Invoke(context.Background(), invocation("session-current", `{"session_id":"session-other"}`, 4096))
		assertFailureCode(t, result, "access_denied")
		if strings.Contains(result.Error.Message, "privately") {
			t.Fatalf("authorization reason leaked: %#v", result.Error)
		}
	})
}

func TestToolFitsOnlyCompleteNewestFactUnits(t *testing.T) {
	old := messageFact(1, "run-1", "step-old", strings.Repeat("old", 1000))
	latest := messageFact(2, "run-1", "step-new", "latest")
	onePage := session.HistoryPage{AgentID: "agent-1", WorkspaceID: "workspace-1", Revision: 2, Facts: []session.HistoryFact{latest}, HasMore: true}
	one, err := fitResponse("session-current", onePage, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	reader := &fakeReader{pages: map[agent.SessionID]session.HistoryPage{
		"session-current": {AgentID: "agent-1", WorkspaceID: "workspace-1", Revision: 2, Facts: []session.HistoryFact{old, latest}},
	}}
	installed, err := New(reader, Config{})
	if err != nil {
		t.Fatal(err)
	}
	result := installed.Invoke(context.Background(), invocation("session-current", `{}`, len(one)+8))
	if result.Status != tool.ResultSucceeded || strings.Contains(string(result.Output), strings.Repeat("old", 20)) || !strings.Contains(string(result.Output), "latest") {
		t.Fatalf("bounded result = %#v", result)
	}
	var decoded response
	if err := json.Unmarshal(result.Output, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.HasMore || decoded.FirstSequence != 2 || decoded.BeforeSequence != 2 {
		t.Fatalf("bounded traceability = %#v", decoded)
	}

	huge := messageFact(3, "run-2", "step-huge", strings.Repeat("x", 4000))
	reader.pages["session-current"] = session.HistoryPage{AgentID: "agent-1", WorkspaceID: "workspace-1", Revision: 3, Facts: []session.HistoryFact{huge}}
	result = installed.Invoke(context.Background(), invocation("session-current", `{}`, 256))
	assertFailureCode(t, result, "result_too_large")
}

func TestToolCancellationAndModuleAssembly(t *testing.T) {
	reader := &fakeReader{pages: map[agent.SessionID]session.HistoryPage{"session-current": minimalPage("agent-1", "workspace-1", "ok")}}
	installed, err := New(reader, Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assertFailureCode(t, installed.Invoke(ctx, invocation("session-current", `{}`, 4096)), "canceled")
	if reader.calls != 0 {
		t.Fatalf("canceled invocation caused %d reads", reader.calls)
	}

	application := agentslot.NewApplication("session-history-module", []agentslot.Module{session.NewMemoryModule(), NewModule(Config{})}, agentslot.RequireKey(tool.ToolSlot, Key))
	assembly, err := application.Build()
	if err != nil {
		t.Fatal(err)
	}
	resolved, ok := agentslot.Lookup(assembly, tool.ToolSlot, Key)
	if !ok || resolved.Definition().Name != Key {
		t.Fatalf("resolved Tool = %#v, %v", resolved, ok)
	}
}

type fakeReader struct {
	pages map[agent.SessionID]session.HistoryPage
	last  session.HistoryPageRequest
	calls int
}

func (r *fakeReader) HistoryPage(ctx context.Context, request session.HistoryPageRequest) (session.HistoryPage, error) {
	if err := ctx.Err(); err != nil {
		return session.HistoryPage{}, err
	}
	r.calls++
	r.last = request
	page, ok := r.pages[request.SessionID]
	if !ok {
		return session.HistoryPage{}, agent.NewCodedError(agent.ErrorNotFound, agent.CodeSessionNotFound, "test.history", "session not found", nil)
	}
	return page, nil
}

func invocation(current agent.SessionID, arguments string, budget int) tool.ToolInvocation {
	return tool.ToolInvocation{
		Call:      tool.Call{ID: "call-1", Name: Key, Arguments: json.RawMessage(arguments)},
		SessionID: current, AgentID: "agent-1", WorkspaceID: "workspace-1",
		Actor: agent.ActorIdentity{Kind: agent.ActorAgent, ID: "agent-1"},
		RunID: "run-current", StepID: "step-current", MaxInlineOutputBytes: budget,
	}
}

func assertFailureCode(t *testing.T, result tool.ToolResult, code string) {
	t.Helper()
	if result.Status != tool.ResultFailed || result.Error == nil || result.Error.Code != code {
		t.Fatalf("result = %#v, want failure %q", result, code)
	}
}

func minimalPage(agentID agent.AgentID, workspaceID agent.WorkspaceID, text string) session.HistoryPage {
	return session.HistoryPage{AgentID: agentID, WorkspaceID: workspaceID, Revision: 1, Facts: []session.HistoryFact{messageFact(1, "run-1", "step-1", text)}}
}

func messageFact(sequence session.HistorySequence, runID agent.RunID, stepID agent.StepID, text string) session.HistoryFact {
	return session.HistoryFact{
		Sequence: sequence, Kind: session.FactMessage, RunID: runID, StepID: stepID,
		Message: &agent.Message{ID: agent.MessageID("message-" + string(rune('0'+sequence))), SessionID: "session-current", RunID: runID, StepID: stepID, Role: agent.RoleAssistant, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: text}}, CreatedAt: time.Now()},
	}
}

func historyFixture(agentID agent.AgentID, workspaceID agent.WorkspaceID) session.HistoryPage {
	config := model.Config{ProviderKey: "provider", ModelID: "model", Reasoning: model.ReasoningDefault}
	message := messageFact(2, "run-1", "step-1", "hello")
	message.Actor = agent.ActorIdentity{Kind: agent.ActorService, ID: "actor-secret"}
	message.Message.ModelContinuation = &agent.ModelContinuation{ProviderKey: "provider", ModelID: "model", State: json.RawMessage(`{"private":"continuation-secret"}`)}
	return session.HistoryPage{
		AgentID: agentID, WorkspaceID: workspaceID, Revision: 7, HasMore: true,
		Facts: []session.HistoryFact{
			{Sequence: 1, Kind: session.FactRun, RunID: "run-1", Run: &session.RunFact{SessionID: "session-current", RunID: "run-1", Kind: session.RunStarted, ModelConfig: config, ConfigRevision: 1}},
			message,
			{Sequence: 3, Kind: session.FactToolCall, RunID: "run-1", StepID: "step-1", ToolCall: &agent.ToolCall{ID: "tool-1", MessageID: "message-1", SessionID: "session-current", RunID: "run-1", StepID: "step-1", Name: "echo", Arguments: json.RawMessage(`{"value":"one"}`)}},
			{Sequence: 4, Kind: session.FactToolResult, RunID: "run-1", StepID: "step-1", ToolResult: &tool.ToolResult{CallID: "tool-1", Status: tool.ResultSucceeded, Output: json.RawMessage(`{"ok":true}`), Artifacts: []artifact.Metadata{{ID: "artifact-result", MediaType: "text/plain", Name: "result.txt", Size: 10}}}},
			{Sequence: 5, Kind: session.FactModelAttempt, RunID: "run-1", StepID: "step-1", ModelAttempt: &session.ModelAttemptFact{AttemptID: "attempt-secret", RunID: "run-1", StepID: "step-1", Kind: session.AttemptSucceeded, ProviderKey: "provider", ModelID: "model", ProviderRequestID: "provider-request-secret"}},
			{Sequence: 6, Kind: session.FactContextContribution, RunID: "run-1", StepID: "step-1", ContextContribution: &session.ContextContributionFact{RunID: "run-1", StepID: "step-1", SourceKey: "context-secret"}},
			{Sequence: 7, Kind: session.FactRunBudgetExceeded, RunID: "run-1", RunBudgetExceeded: &session.RunBudgetExceededFact{RunID: "run-1", UsedTokens: 100, MaxTokens: 100}},
			{Sequence: 8, Kind: session.FactModelConfigChanged, ModelConfigChanged: &session.ModelConfigChange{Previous: config, Current: model.Config{ProviderKey: "provider", ModelID: "model-2", Reasoning: model.ReasoningHigh}}},
			{Sequence: 9, Kind: session.FactRun, RunID: "run-1", Run: &session.RunFact{SessionID: "session-current", RunID: "run-1", Kind: session.RunCompleted, ModelConfig: config, ConfigRevision: 1}},
		},
	}
}
