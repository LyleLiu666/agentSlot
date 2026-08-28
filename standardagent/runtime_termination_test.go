package standardagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestRuntimePersistsClassifiedModelTermination(t *testing.T) {
	providerFailure := agent.NewCodedError(
		agent.ErrorUnavailable, "provider_unavailable", "test.provider",
		"model route is unavailable", errors.New("private transport detail"),
	)
	executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{{Kind: model.EventFailed, Err: providerFailure}}})
	access, stop := startRuntimeTestApplication(t, executor)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("fail"),
	}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	terminal := latestRunTerminal(t, access, opened.SessionID)
	want := session.RunTermination{
		Source: session.TerminationModel, Kind: agent.ErrorUnavailable,
		Code: "provider_unavailable", SafeMessage: "model route is unavailable",
	}
	if terminal.Kind != session.RunFailed || terminal.Termination == nil || *terminal.Termination != want {
		t.Fatalf("model terminal = %#v, want %#v", terminal, want)
	}
}

func TestRuntimeDoesNotPersistUnclassifiedModelErrorText(t *testing.T) {
	secret := "credential=must-not-be-persisted"
	executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{{Kind: model.EventFailed, Err: errors.New(secret)}}})
	access, stop := startRuntimeTestApplication(t, executor)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("fail safely"),
	}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	terminal := latestRunTerminal(t, access, opened.SessionID)
	if terminal.Termination == nil || terminal.Termination.Source != session.TerminationModel ||
		terminal.Termination.Kind != agent.ErrorInternal || terminal.Termination.Code != agent.CodeModelExecutionFailed ||
		terminal.Termination.SafeMessage != "" || strings.Contains(terminal.Termination.SafeMessage, secret) {
		t.Fatalf("unclassified terminal = %#v", terminal)
	}
}

func TestRuntimeRejectsModelStreamClosedWithoutTerminal(t *testing.T) {
	executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{{
		Kind: model.EventDelta, AttemptID: "attempt-1", Text: "temporary only",
	}}})
	access, stop := startRuntimeTestApplication(t, executor)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("incomplete"),
	}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	terminal := latestRunTerminal(t, access, opened.SessionID)
	if terminal.Kind != session.RunFailed || terminal.Termination == nil ||
		terminal.Termination.Source != session.TerminationModel || terminal.Termination.Code != agent.CodeModelStreamInvalid {
		t.Fatalf("incomplete stream terminal = %#v", terminal)
	}
	view, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if messages := historyMessageFacts(view.RecentHistory); len(messages) != 1 {
		t.Fatalf("temporary model output became durable: %#v", messages)
	}
}

func TestRuntimeRejectsInvalidCompletionBeforeDurableToolAction(t *testing.T) {
	executor := model.NewFakeModelExecutor(model.FakeExecution{
		Events: []model.ModelEvent{{
			Kind:      model.EventComplete,
			AttemptID: "attempt-1",
			Output: &model.Completion{ToolCalls: []model.ToolCallRequest{{
				Name: "unsafe", Arguments: json.RawMessage(`{"command":"first","command":"second"}`),
			}}},
		}},
	})
	access, stop := startRuntimeTestApplication(t, executor)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("invalid completion"),
	}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	terminal := latestRunTerminal(t, access, opened.SessionID)
	if terminal.Termination == nil || terminal.Termination.Source != session.TerminationModel ||
		terminal.Termination.Code != agent.CodeModelStreamInvalid {
		t.Fatalf("invalid Completion terminal = %#v", terminal)
	}
	view, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range view.RecentHistory {
		if fact.ToolCall != nil || fact.Message != nil && fact.Message.Role == agent.RoleAssistant {
			t.Fatalf("invalid Completion became durable action state: %#v", fact)
		}
	}
}

func TestRuntimeCancellationHasRuntimeTermination(t *testing.T) {
	block := make(chan struct{})
	executor := model.NewFakeModelExecutor(model.FakeExecution{Block: block, Events: []model.ModelEvent{complete("late")}})
	access, stop := startRuntimeTestApplication(t, executor)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	receipt, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("cancel"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := executor.WaitForRequests(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := access.Cancel(context.Background(), interaction.CancelRequest{SessionID: opened.SessionID, ExpectedRevision: receipt.Revision}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	terminal := latestRunTerminal(t, access, opened.SessionID)
	if terminal.Kind != session.RunCanceled || terminal.Termination == nil ||
		terminal.Termination.Source != session.TerminationRuntime || terminal.Termination.Kind != agent.ErrorCanceled ||
		terminal.Termination.Code != agent.CodeCanceled {
		t.Fatalf("canceled terminal = %#v", terminal)
	}
}

func latestRunTerminal(t *testing.T, access interaction.GatewayAccess, sessionID agent.SessionID) session.RunFact {
	t.Helper()
	view, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	runs := historyRunFacts(view.RecentHistory)
	if len(runs) < 2 {
		t.Fatalf("run facts = %#v", runs)
	}
	return runs[len(runs)-1]
}
