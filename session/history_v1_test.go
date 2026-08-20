package session_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestMemoryStoreAssignsStableHistoryMetadata(t *testing.T) {
	store := session.NewMemoryStore()
	first := message("message-1", "history-v1", agent.RoleUser, "one")
	created, err := store.Create(context.Background(), session.NewSession{
		Session:     agent.Session{ID: first.SessionID, AgentID: "agent-1", WorkspaceID: "workspace-1"},
		History:     []session.HistoryFact{{Message: &first}},
		ModelConfig: defaultConfig(),
		RunState:    session.RunIdle,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	assertFactMetadata(t, created.History[0], 1, session.FactMessage)
	if created.History[0].Actor.Kind != agent.ActorService {
		t.Fatalf("default actor = %#v, want service", created.History[0].Actor)
	}

	second := message("message-2", first.SessionID, agent.RoleAssistant, "two")
	second.RunID, second.StepID = "run-1", "step-1"
	_, err = store.Commit(context.Background(), session.CommitRequest{
		SessionID: first.SessionID, ExpectedRevision: created.Revision, IdempotencyKey: "append-second",
		Actor:   agent.ActorIdentity{Kind: agent.ActorAgent, ID: "agent-1"},
		Changes: []session.Change{{Kind: session.AppendMessage, Message: &second}},
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	loaded, err := store.Load(context.Background(), session.SessionRef{SessionID: first.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	assertFactMetadata(t, loaded.History[1], 2, session.FactMessage)
	if loaded.History[1].Actor.Kind != agent.ActorAgent || loaded.History[1].Actor.ID != "agent-1" {
		t.Fatalf("actor = %#v", loaded.History[1].Actor)
	}
	if loaded.History[1].RunID != second.RunID || loaded.History[1].StepID != second.StepID {
		t.Fatalf("containment = %q/%q", loaded.History[1].RunID, loaded.History[1].StepID)
	}
	if loaded.History[0].FactID == loaded.History[1].FactID {
		t.Fatal("facts received duplicate IDs")
	}
}

func TestMemoryStorePersistsTypedOperationalFacts(t *testing.T) {
	store, snapshot := newStoredSession(t, "typed-facts")
	attempt := session.ModelAttemptFact{
		AttemptID: "attempt-1", RunID: "run-1", StepID: "step-1", Kind: session.AttemptStarted,
		ProviderKey: "provider", ModelID: "model",
	}
	contribution := session.ContextContributionFact{RunID: "run-1", StepID: "step-1", SourceKey: "memory", Inputs: []model.Input{}}
	budget := session.RunBudgetExceededFact{RunID: "run-1", UsedTokens: 101, MaxTokens: 100}
	commit := commitChanges(t, store, snapshot.Session.ID, snapshot.Revision, "typed-facts",
		session.Change{Kind: session.AppendModelAttempt, ModelAttempt: &attempt},
		session.Change{Kind: session.AppendContextContribution, ContextContribution: &contribution},
		session.Change{Kind: session.AppendRunBudgetExceeded, RunBudgetExceeded: &budget},
	)
	loaded := load(t, store, snapshot.Session.ID)
	if loaded.Revision != commit.Revision || len(loaded.History) != 3 {
		t.Fatalf("loaded = revision %d history %d", loaded.Revision, len(loaded.History))
	}
	want := []session.HistoryFactKind{session.FactModelAttempt, session.FactContextContribution, session.FactRunBudgetExceeded}
	for index := range want {
		assertFactMetadata(t, loaded.History[index], session.HistorySequence(index+1), want[index])
	}
}

func TestModelAttemptRequiresStartedThenOneTerminal(t *testing.T) {
	store, snapshot := newStoredSession(t, "attempt-pair")
	terminal := session.ModelAttemptFact{
		AttemptID: "attempt-1", RunID: "run-1", StepID: "step-1", Kind: session.AttemptFailed,
		ProviderKey: "provider", ModelID: "model", ErrorCode: "network",
	}
	_, err := store.Commit(context.Background(), session.CommitRequest{
		SessionID: snapshot.Session.ID, ExpectedRevision: snapshot.Revision, IdempotencyKey: "terminal-first",
		Changes: []session.Change{{Kind: session.AppendModelAttempt, ModelAttempt: &terminal}},
	})
	if !agent.IsCode(err, agent.CodeHistoryInvariant) {
		t.Fatalf("terminal without started error = %v", err)
	}
	started := terminal
	started.Kind, started.ErrorCode = session.AttemptStarted, ""
	first := commitChanges(t, store, snapshot.Session.ID, snapshot.Revision, "started",
		session.Change{Kind: session.AppendModelAttempt, ModelAttempt: &started},
	)
	second := commitChanges(t, store, snapshot.Session.ID, first.Revision, "terminal",
		session.Change{Kind: session.AppendModelAttempt, ModelAttempt: &terminal},
	)
	_, err = store.Commit(context.Background(), session.CommitRequest{
		SessionID: snapshot.Session.ID, ExpectedRevision: second.Revision, IdempotencyKey: "second-terminal",
		Changes: []session.Change{{Kind: session.AppendModelAttempt, ModelAttempt: &terminal}},
	})
	if !agent.IsCode(err, agent.CodeHistoryInvariant) {
		t.Fatalf("duplicate terminal error = %v", err)
	}
}

func TestCreateRejectsUnpairedModelAttemptHistory(t *testing.T) {
	store := session.NewMemoryStore()
	terminal := session.ModelAttemptFact{
		AttemptID: "attempt-1", RunID: "run-1", StepID: "step-1", Kind: session.AttemptFailed,
		ProviderKey: "provider", ModelID: "model", ErrorCode: "network",
	}
	_, err := store.Create(context.Background(), session.NewSession{
		Session: agent.Session{ID: "invalid-attempt-history", AgentID: "agent-1", WorkspaceID: "workspace-1"},
		History: []session.HistoryFact{{ModelAttempt: &terminal}}, ModelConfig: defaultConfig(), RunState: session.RunIdle,
	})
	if err == nil {
		t.Fatalf("unpaired attempt history error = %v", err)
	}
}

func TestRecoveryTerminatesOrphanedModelAttempt(t *testing.T) {
	store, snapshot := newStoredSession(t, "attempt-recovery")
	started := session.ModelAttemptFact{
		AttemptID: "attempt-1", RunID: "run-1", StepID: "step-1", Kind: session.AttemptStarted,
		ProviderKey: "provider", ModelID: "model",
	}
	committed := commitChanges(t, store, snapshot.Session.ID, snapshot.Revision, "attempt-started",
		session.Change{Kind: session.AppendModelAttempt, ModelAttempt: &started},
	)
	recovered, err := store.Recover(context.Background(), session.SessionRef{SessionID: snapshot.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Revision != committed.Revision.Next() || len(recovered.History) != 2 {
		t.Fatalf("recovered = revision %d history %d", recovered.Revision, len(recovered.History))
	}
	terminal := recovered.History[1].ModelAttempt
	if terminal == nil || terminal.Kind != session.AttemptOutcomeUnknown || terminal.AttemptID != started.AttemptID {
		t.Fatalf("terminal attempt = %#v", terminal)
	}
	again, err := store.Recover(context.Background(), session.SessionRef{SessionID: snapshot.Session.ID})
	if err != nil || again.Revision != recovered.Revision || len(again.History) != 2 {
		t.Fatalf("second recovery = revision %d history %d, %v", again.Revision, len(again.History), err)
	}
}

func TestHistoryPageUsesExclusiveSequenceCursorAndKeepsStepsWhole(t *testing.T) {
	store := session.NewMemoryStore()
	history := make([]session.HistoryFact, 0, 4)
	for index, step := range []agent.StepID{"step-1", "step-1", "step-2", "step-2"} {
		item := message(agent.MessageID("message-"+string(rune('a'+index))), "history-page", agent.RoleAssistant, "part")
		item.RunID, item.StepID = "run-1", step
		history = append(history, session.HistoryFact{Message: &item})
	}
	created, err := store.Create(context.Background(), session.NewSession{
		Session: agent.Session{ID: "history-page", AgentID: "agent-1", WorkspaceID: "workspace-1"},
		History: history, ModelConfig: defaultConfig(), RunState: session.RunIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.HistoryPage(context.Background(), session.HistoryPageRequest{
		SessionID: created.Session.ID, StepLimit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Facts) != 2 || page.Facts[0].StepID != "step-2" || !page.HasMore {
		t.Fatalf("latest page = %#v", page)
	}
	older, err := store.HistoryPage(context.Background(), session.HistoryPageRequest{
		SessionID: created.Session.ID, BeforeHistorySequence: page.Facts[0].Sequence, StepLimit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(older.Facts) != 2 || older.Facts[0].StepID != "step-1" || older.HasMore {
		t.Fatalf("older page = %#v", older)
	}
	middle, err := store.HistoryPage(context.Background(), session.HistoryPageRequest{
		SessionID: created.Session.ID, BeforeHistorySequence: 4, StepLimit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(middle.Facts) != 2 || middle.Facts[0].StepID != "step-1" {
		t.Fatalf("cursor split logical step: %#v", middle)
	}
	if _, err := store.HistoryPage(context.Background(), session.HistoryPageRequest{SessionID: created.Session.ID, StepLimit: 101}); err == nil {
		t.Fatal("page larger than 100 steps was accepted")
	}
}

func TestFileStoreWritesV1AndRejectsV0(t *testing.T) {
	directory := t.TempDir()
	store := openFileStore(t, directory)
	created, err := store.Create(context.Background(), session.NewSession{
		Session:     agent.Session{ID: "format-v1", AgentID: "agent-1", WorkspaceID: "workspace-1"},
		ModelConfig: defaultConfig(), RunState: session.RunIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v, %v", entries, err)
	}
	path := filepath.Join(directory, entries[0].Name())
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var header struct {
		Format string `json:"format"`
	}
	if err := json.Unmarshal(data, &header); err != nil || header.Format != "agentslot.session-file/v1" {
		t.Fatalf("format = %q, %v", header.Format, err)
	}
	if err := store.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	v0 := strings.Replace(string(data), "agentslot.session-file/v1", "agentslot.session-file/v0", 1)
	if err := os.WriteFile(path, []byte(v0), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened := openFileStore(t, directory)
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	_, err = reopened.Load(context.Background(), session.SessionRef{SessionID: created.Session.ID})
	if !agent.IsCode(err, agent.CodeSessionUnrecoverable) || !strings.Contains(err.Error(), "unsupported format") {
		t.Fatalf("v0 load error = %v", err)
	}
}

func assertFactMetadata(t *testing.T, fact session.HistoryFact, sequence session.HistorySequence, kind session.HistoryFactKind) {
	t.Helper()
	if !fact.FactID.Valid() || fact.Sequence != sequence || fact.Kind != kind || !fact.SessionID.Valid() || fact.At.IsZero() {
		t.Fatalf("fact metadata = %#v", fact)
	}
	if time.Since(fact.At) > time.Minute {
		t.Fatalf("fact timestamp = %v", fact.At)
	}
}
