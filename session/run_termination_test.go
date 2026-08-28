package session_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestRunTerminationUsesFiniteSafeProviderNeutralVocabulary(t *testing.T) {
	valid := session.RunTermination{
		Source: session.TerminationModel, Kind: agent.ErrorUnavailable,
		Code: agent.CodeModelExecutionFailed, SafeMessage: "model service is unavailable",
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*session.RunTermination){
		func(value *session.RunTermination) { value.Source = "provider_anthropic" },
		func(value *session.RunTermination) { value.Kind = "retryable" },
		func(value *session.RunTermination) { value.Code = "" },
		func(value *session.RunTermination) { value.SafeMessage = " unsafe" },
		func(value *session.RunTermination) { value.SafeMessage = "line one\nline two" },
		func(value *session.RunTermination) {
			value.SafeMessage = strings.Repeat("x", session.MaxRunTerminationMessageBytes+1)
		},
	} {
		candidate := valid
		mutate(&candidate)
		if err := candidate.Validate(); err == nil {
			t.Fatalf("invalid RunTermination accepted: %#v", candidate)
		}
	}
	typeOfTermination := reflect.TypeOf(valid)
	for _, forbidden := range []string{"Retryable", "EffectState", "ProviderBody", "RecoveryAdvice"} {
		if _, exists := typeOfTermination.FieldByName(forbidden); exists {
			t.Fatalf("RunTermination persisted product/derived field %q", forbidden)
		}
	}
}

func TestRunFactTerminationIsForbiddenForSuccessAndOptionalForLegacyRead(t *testing.T) {
	started := session.RunFact{
		SessionID: "session-legacy", RunID: "run-1", Kind: session.RunStarted,
		ModelConfig: defaultConfig(), ConfigRevision: 1,
	}
	legacyFailed := started
	legacyFailed.Kind = session.RunFailed
	store := session.NewMemoryStore()
	if _, err := store.Create(context.Background(), session.NewSession{
		Session:     agent.Session{ID: started.SessionID, AgentID: "agent-1", WorkspaceID: "workspace-1"},
		History:     []session.HistoryFact{{Run: &started}, {Run: &legacyFailed}},
		ModelConfig: defaultConfig(), RunState: session.RunIdle,
	}); err != nil {
		t.Fatalf("legacy Session without RunTermination is unreadable: %v", err)
	}

	termination := session.RunTermination{Source: session.TerminationModel, Kind: agent.ErrorUnavailable, Code: agent.CodeModelExecutionFailed}
	for _, kind := range []session.RunFactKind{session.RunStarted, session.RunCompleted} {
		fact := started
		fact.Kind, fact.Termination = kind, &termination
		if err := fact.Validate(started.SessionID); err == nil {
			t.Fatalf("Run %q accepted a termination", kind)
		}
	}
}

func TestNewNonSuccessfulRunCommitRequiresTermination(t *testing.T) {
	store, snapshot := newStoredSession(t, "run-termination-required")
	started := session.RunFact{
		SessionID: snapshot.Session.ID, RunID: "run-1", Kind: session.RunStarted,
		ModelConfig: defaultConfig(), ConfigRevision: snapshot.Revision,
	}
	running := commitChanges(t, store, snapshot.Session.ID, snapshot.Revision, "start-run",
		session.Change{Kind: session.SetRunState, RunState: &session.RunStateChange{RunID: started.RunID, State: session.RunRunning}},
		session.Change{Kind: session.AppendRunFact, RunFact: &started},
	)
	failed := started
	failed.Kind = session.RunFailed
	if _, err := store.Commit(context.Background(), session.CommitRequest{
		SessionID: snapshot.Session.ID, ExpectedRevision: running.Revision, IdempotencyKey: "missing-termination",
		Changes: []session.Change{{Kind: session.AppendRunFact, RunFact: &failed}},
	}); !agent.IsKind(err, agent.ErrorInvalidInput) {
		t.Fatalf("new failed Run without termination error = %v, kind=%q", err, agent.KindOf(err))
	}
}

func TestRecoveryAddsGenericInterruptedTermination(t *testing.T) {
	store, snapshot := newStoredSession(t, "run-termination-recovery")
	started := session.RunFact{
		SessionID: snapshot.Session.ID, RunID: "run-1", Kind: session.RunStarted,
		ModelConfig: defaultConfig(), ConfigRevision: snapshot.Revision,
	}
	running := commitChanges(t, store, snapshot.Session.ID, snapshot.Revision, "start-run",
		session.Change{Kind: session.SetRunState, RunState: &session.RunStateChange{RunID: started.RunID, State: session.RunRunning}},
		session.Change{Kind: session.AppendRunFact, RunFact: &started},
	)
	recovered, err := store.Recover(context.Background(), session.SessionRef{SessionID: snapshot.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Revision != running.Revision.Next() {
		t.Fatalf("recovery revision = %d, want %d", recovered.Revision, running.Revision.Next())
	}
	terminal := recovered.History[len(recovered.History)-1].Run
	if terminal == nil || terminal.Kind != session.RunInterrupted || terminal.Termination == nil ||
		terminal.Termination.Source != session.TerminationRuntime || terminal.Termination.Kind != agent.ErrorUnavailable ||
		terminal.Termination.Code != agent.CodeRuntimeInterrupted {
		t.Fatalf("recovered terminal = %#v", terminal)
	}
}

func TestFileStorePersistsRunTerminationAcrossReopen(t *testing.T) {
	directory := t.TempDir()
	store := openFileStore(t, directory)
	created, err := store.Create(context.Background(), session.NewSession{
		Session:     agent.Session{ID: "run-termination-file", AgentID: "agent-1", WorkspaceID: "workspace-1"},
		ModelConfig: defaultConfig(), RunState: session.RunIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	started := session.RunFact{
		SessionID: created.Session.ID, RunID: "run-1", Kind: session.RunStarted,
		ModelConfig: defaultConfig(), ConfigRevision: created.Revision,
	}
	running := commitChanges(t, store, created.Session.ID, created.Revision, "start-run",
		session.Change{Kind: session.SetRunState, RunState: &session.RunStateChange{RunID: started.RunID, State: session.RunRunning}},
		session.Change{Kind: session.AppendRunFact, RunFact: &started},
	)
	termination := session.RunTermination{
		Source: session.TerminationModel, Kind: agent.ErrorUnavailable,
		Code: agent.CodeModelExecutionFailed, SafeMessage: "model service is unavailable",
	}
	failed := started
	failed.Kind, failed.Termination = session.RunFailed, &termination
	commitChanges(t, store, created.Session.ID, running.Revision, "finish-run",
		session.Change{Kind: session.AppendRunFact, RunFact: &failed},
		session.Change{Kind: session.SetRunState, RunState: &session.RunStateChange{RunID: started.RunID, State: session.RunIdle}},
	)
	if err := store.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	reopened := openFileStore(t, directory)
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	loaded, err := reopened.Load(context.Background(), session.SessionRef{SessionID: created.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	terminal := loaded.History[len(loaded.History)-1].Run
	if terminal == nil || terminal.Termination == nil || *terminal.Termination != termination {
		t.Fatalf("reopened terminal = %#v, want %#v", terminal, termination)
	}
}
