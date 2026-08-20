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
	"github.com/LyleLiu666/agentSlot/tool"
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

func TestCommitReportsTheExactAppendedHistoryRange(t *testing.T) {
	store, snapshot := newStoredSession(t, "commit-range")
	factMessage := message("message-range", snapshot.Session.ID, agent.RoleUser, "fact")
	commit := commitChanges(t, store, snapshot.Session.ID, snapshot.Revision, "fact-range",
		session.Change{Kind: session.AppendMessage, Message: &factMessage},
	)
	if commit.FirstHistorySequence != 1 || commit.LastHistorySequence != 1 {
		t.Fatalf("fact commit range = %d..%d", commit.FirstHistorySequence, commit.LastHistorySequence)
	}
	replayed := commitChanges(t, store, snapshot.Session.ID, snapshot.Revision, "fact-range",
		session.Change{Kind: session.AppendMessage, Message: &factMessage},
	)
	if replayed.Applied || replayed.FirstHistorySequence != commit.FirstHistorySequence || replayed.LastHistorySequence != commit.LastHistorySequence {
		t.Fatalf("idempotent fact range = %#v, want original range %#v", replayed, commit)
	}
	queued := message("message-queued", snapshot.Session.ID, agent.RoleUser, "queued")
	queueOnly := commitChanges(t, store, snapshot.Session.ID, commit.Revision, "queue-only-range",
		session.Change{Kind: session.EnqueueMessage, QueueItem: &session.QueueItem{Message: queued, Delivery: session.DeliveryNormal}},
	)
	if queueOnly.FirstHistorySequence != 0 || queueOnly.LastHistorySequence != 0 {
		t.Fatalf("queue-only commit range = %d..%d, want zero range", queueOnly.FirstHistorySequence, queueOnly.LastHistorySequence)
	}
}

func TestMemoryStorePersistsTypedOperationalFacts(t *testing.T) {
	store, snapshot := newStoredSession(t, "typed-facts")
	snapshot = startStoredRun(t, store, snapshot, "run-1")
	attempt := session.ModelAttemptFact{
		AttemptID: "attempt-1", RunID: "run-1", StepID: "step-1", Kind: session.AttemptStarted,
		ProviderKey: snapshot.ModelConfig.ProviderKey, ModelID: snapshot.ModelConfig.ModelID,
	}
	terminal := attempt
	terminal.Kind = session.AttemptFailed
	terminal.ErrorCode = "provider_failure"
	terminal.Usage = model.TokenUsage{InputTokens: 100, OutputTokens: 1, TotalTokens: 101}
	contribution := session.ContextContributionFact{RunID: "run-1", StepID: "step-1", SourceKey: "memory", Inputs: []model.Input{}}
	budget := session.RunBudgetExceededFact{RunID: "run-1", UsedTokens: 101, MaxTokens: 100}
	commit := commitChanges(t, store, snapshot.Session.ID, snapshot.Revision, "typed-facts",
		session.Change{Kind: session.AppendModelAttempt, ModelAttempt: &attempt},
		session.Change{Kind: session.AppendModelAttempt, ModelAttempt: &terminal},
		session.Change{Kind: session.AppendContextContribution, ContextContribution: &contribution},
		session.Change{Kind: session.AppendRunBudgetExceeded, RunBudgetExceeded: &budget},
	)
	loaded := load(t, store, snapshot.Session.ID)
	if loaded.Revision != commit.Revision || len(loaded.History) != 5 {
		t.Fatalf("loaded = revision %d history %d", loaded.Revision, len(loaded.History))
	}
	want := []session.HistoryFactKind{session.FactRun, session.FactModelAttempt, session.FactModelAttempt, session.FactContextContribution, session.FactRunBudgetExceeded}
	for index := range want {
		assertFactMetadata(t, loaded.History[index], session.HistorySequence(index+1), want[index])
	}
}

func TestModelAttemptRequiresStartedThenOneTerminal(t *testing.T) {
	store, snapshot := newStoredSession(t, "attempt-pair")
	snapshot = startStoredRun(t, store, snapshot, "run-1")
	terminal := session.ModelAttemptFact{
		AttemptID: "attempt-1", RunID: "run-1", StepID: "step-1", Kind: session.AttemptFailed,
		ProviderKey: snapshot.ModelConfig.ProviderKey, ModelID: snapshot.ModelConfig.ModelID, ErrorCode: "network",
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

func TestContextSnapshotMustMatchTheFrozenRunModel(t *testing.T) {
	store, snapshot := newStoredSession(t, "context-run-model")
	snapshot = startStoredRun(t, store, snapshot, "run-1")
	started := snapshot.History[0].Run
	if started == nil {
		t.Fatal("missing RunStarted fact")
	}
	view := session.ContextView{
		Version: 1, SourceRevision: snapshot.Revision,
		SourceHistorySequence: snapshot.History[len(snapshot.History)-1].Sequence,
		Request: model.ModelRequest{
			SessionID: snapshot.Session.ID, RunID: "run-1", StepID: "step-1",
			Config: started.ModelConfig, ConfigRevision: started.ConfigRevision,
		},
	}
	view.Request.Config.ModelID = "different-model"
	_, err := store.Commit(context.Background(), session.CommitRequest{
		SessionID: snapshot.Session.ID, ExpectedRevision: snapshot.Revision, IdempotencyKey: "wrong-context-model",
		Changes: []session.Change{{Kind: session.SetContext, Context: &view}},
	})
	if err == nil {
		t.Fatal("Context using a model other than the frozen Run model was accepted")
	}

	view.Request.Config = started.ModelConfig
	committed := commitChanges(t, store, snapshot.Session.ID, snapshot.Revision, "valid-context-model",
		session.Change{Kind: session.SetContext, Context: &view},
	)
	if committed.Revision != snapshot.Revision.Next() {
		t.Fatalf("context commit revision = %d", committed.Revision)
	}
}

func TestRunBudgetFactMustMatchDurableAttemptUsage(t *testing.T) {
	store, snapshot := newStoredSession(t, "budget-attempt-usage")
	snapshot = startStoredRun(t, store, snapshot, "run-1")
	started := session.ModelAttemptFact{
		AttemptID: "attempt-1", RunID: "run-1", StepID: "step-1", Kind: session.AttemptStarted,
		ProviderKey: snapshot.ModelConfig.ProviderKey, ModelID: snapshot.ModelConfig.ModelID,
	}
	terminal := started
	terminal.Kind = session.AttemptFailed
	terminal.ErrorCode = "transport"
	terminal.Usage = model.TokenUsage{InputTokens: 4, OutputTokens: 1, TotalTokens: 5}
	committed := commitChanges(t, store, snapshot.Session.ID, snapshot.Revision, "budget-attempt",
		session.Change{Kind: session.AppendModelAttempt, ModelAttempt: &started},
		session.Change{Kind: session.AppendModelAttempt, ModelAttempt: &terminal},
	)
	budget := session.RunBudgetExceededFact{RunID: "run-1", UsedTokens: 6, MaxTokens: 5}
	_, err := store.Commit(context.Background(), session.CommitRequest{
		SessionID: snapshot.Session.ID, ExpectedRevision: committed.Revision, IdempotencyKey: "wrong-budget-usage",
		Changes: []session.Change{{Kind: session.AppendRunBudgetExceeded, RunBudgetExceeded: &budget}},
	})
	if err == nil {
		t.Fatal("RunBudgetExceeded usage not backed by durable Attempts was accepted")
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
	snapshot = startStoredRun(t, store, snapshot, "run-1")
	started := session.ModelAttemptFact{
		AttemptID: "attempt-1", RunID: "run-1", StepID: "step-1", Kind: session.AttemptStarted,
		ProviderKey: snapshot.ModelConfig.ProviderKey, ModelID: snapshot.ModelConfig.ModelID,
	}
	committed := commitChanges(t, store, snapshot.Session.ID, snapshot.Revision, "attempt-started",
		session.Change{Kind: session.AppendModelAttempt, ModelAttempt: &started},
	)
	recovered, err := store.Recover(context.Background(), session.SessionRef{SessionID: snapshot.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Revision != committed.Revision.Next() || len(recovered.History) != 4 {
		t.Fatalf("recovered = revision %d history %d", recovered.Revision, len(recovered.History))
	}
	terminal := recovered.History[2].ModelAttempt
	if terminal == nil || terminal.Kind != session.AttemptOutcomeUnknown || terminal.AttemptID != started.AttemptID {
		t.Fatalf("terminal attempt = %#v", terminal)
	}
	again, err := store.Recover(context.Background(), session.SessionRef{SessionID: snapshot.Session.ID})
	if err != nil || again.Revision != recovered.Revision || len(again.History) != 4 {
		t.Fatalf("second recovery = revision %d history %d, %v", again.Revision, len(again.History), err)
	}
}

func startStoredRun(t *testing.T, store session.SessionStore, snapshot session.Snapshot, runID agent.RunID) session.Snapshot {
	t.Helper()
	running := session.RunStateChange{RunID: runID, State: session.RunRunning}
	started := session.RunFact{
		SessionID: snapshot.Session.ID, RunID: runID, Kind: session.RunStarted,
		ModelConfig: snapshot.ModelConfig, ConfigRevision: snapshot.Revision,
	}
	commitChanges(t, store, snapshot.Session.ID, snapshot.Revision, "start-"+string(runID),
		session.Change{Kind: session.SetRunState, RunState: &running},
		session.Change{Kind: session.AppendRunFact, RunFact: &started},
	)
	return load(t, store, snapshot.Session.ID)
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

func TestHistoryPageCountsLogicalStepsWithoutChargingRunFacts(t *testing.T) {
	store := session.NewMemoryStore()
	history := []session.HistoryFact{
		{Run: &session.RunFact{SessionID: "history-step-quota", RunID: "run-1", Kind: session.RunStarted, ModelConfig: defaultConfig(), ConfigRevision: 1}},
		{Message: historyStepMessage("message-1", "history-step-quota", "run-1", "step-1")},
		{Run: &session.RunFact{SessionID: "history-step-quota", RunID: "run-1", Kind: session.RunCompleted, ModelConfig: defaultConfig(), ConfigRevision: 1}},
		{Run: &session.RunFact{SessionID: "history-step-quota", RunID: "run-2", Kind: session.RunStarted, ModelConfig: defaultConfig(), ConfigRevision: 2}},
		{Message: historyStepMessage("message-2", "history-step-quota", "run-2", "step-2")},
		{Run: &session.RunFact{SessionID: "history-step-quota", RunID: "run-2", Kind: session.RunCompleted, ModelConfig: defaultConfig(), ConfigRevision: 2}},
	}
	created, err := store.Create(context.Background(), session.NewSession{
		Session: agent.Session{ID: "history-step-quota", AgentID: "agent-1", WorkspaceID: "workspace-1"},
		History: history, ModelConfig: defaultConfig(), RunState: session.RunIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.HistoryPage(context.Background(), session.HistoryPageRequest{SessionID: created.Session.ID, StepLimit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Facts) != 6 || page.HasMore {
		t.Fatalf("two-Step page = %#v", page)
	}
	latest, err := store.HistoryPage(context.Background(), session.HistoryPageRequest{SessionID: created.Session.ID, StepLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(latest.Facts) != 3 || latest.Facts[0].Run == nil || latest.Facts[0].Run.RunID != "run-2" {
		t.Fatalf("latest Step leaked lifecycle facts from an older Run: %#v", latest)
	}
}

func TestHistoryPageNeverSplitsToolProtocolWithinOneStep(t *testing.T) {
	store := session.NewMemoryStore()
	older := historyStepMessage("message-old", "history-tool-page", "run-1", "step-1")
	assistant := historyStepMessage("message-tool", "history-tool-page", "run-1", "step-2")
	assistant.Role = agent.RoleAssistant
	call := agent.ToolCall{
		ID: "call-1", CorrelationID: "provider-call-1", MessageID: assistant.ID,
		SessionID: assistant.SessionID, RunID: assistant.RunID, StepID: assistant.StepID,
		Name: "example", Arguments: []byte(`{"value":1}`),
	}
	result := tool.ToolResult{CallID: call.ID, Status: tool.ResultSucceeded, Output: []byte(`{"ok":true}`)}
	created, err := store.Create(context.Background(), session.NewSession{
		Session: agent.Session{ID: assistant.SessionID, AgentID: "agent-1", WorkspaceID: "workspace-1"},
		History: []session.HistoryFact{
			{Message: older}, {Message: assistant}, {ToolCall: &call}, {ToolResult: &result},
		},
		ModelConfig: defaultConfig(), RunState: session.RunIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.HistoryPage(context.Background(), session.HistoryPageRequest{SessionID: created.Session.ID, StepLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Facts) != 3 || !page.HasMore {
		t.Fatalf("tool Step page = %#v", page)
	}
	for _, fact := range page.Facts {
		if fact.StepID != assistant.StepID {
			t.Fatalf("tool protocol was split across pages: %#v", page)
		}
	}
}

func historyStepMessage(id agent.MessageID, sessionID agent.SessionID, runID agent.RunID, stepID agent.StepID) *agent.Message {
	return &agent.Message{
		ID: id, SessionID: sessionID, RunID: runID, StepID: stepID, Role: agent.RoleUser,
		Parts: []agent.MessagePart{{Kind: agent.PartText, Text: string(stepID)}},
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
