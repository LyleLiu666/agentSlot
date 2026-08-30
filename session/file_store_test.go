package session_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestFileStorePersistsOpaqueModelContinuationExactly(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "sessions")
	store := openFileStore(t, directory)
	created, err := store.Create(context.Background(), session.NewSession{
		Session:     agent.Session{ID: "session-continuation", AgentID: "agent-1", WorkspaceID: "workspace-1"},
		ModelConfig: model.Config{ProviderKey: "provider", ModelID: "model", Reasoning: model.ReasoningDefault},
		RunState:    session.RunIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	state := json.RawMessage(`[{"type":"thinking","signature":"opaque-provider-value"}]`)
	message := agent.Message{
		ID: "message-continuation", SessionID: created.Session.ID, RunID: "run-1", StepID: "step-1", Role: agent.RoleAssistant,
		Parts:             []agent.MessagePart{{Kind: agent.PartText, Text: "calling a tool"}},
		ModelContinuation: &agent.ModelContinuation{ProviderKey: "provider", ModelID: "model", State: state},
	}
	if _, err := store.Commit(context.Background(), session.CommitRequest{
		SessionID: created.Session.ID, ExpectedRevision: created.Revision, IdempotencyKey: "append-continuation",
		Changes: []session.Change{{Kind: session.AppendMessage, Message: &message}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened := openFileStore(t, directory)
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	loaded, err := reopened.Load(context.Background(), session.SessionRef{SessionID: created.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	got := loaded.History[0].Message.ModelContinuation
	if got == nil || got.ProviderKey != "provider" || got.ModelID != "model" || !bytes.Equal(got.State, state) {
		t.Fatalf("continuation = %#v", got)
	}
}

func TestFileStorePersistsSnapshotAndIdempotencyAcrossReopen(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "sessions")
	store := openFileStore(t, directory)
	initial := session.NewSession{
		Session:     agent.Session{ID: "session-1", AgentID: "agent-1", WorkspaceID: "workspace-1"},
		ModelConfig: model.Config{ProviderKey: "provider", ModelID: "model", Reasoning: model.ReasoningDefault},
		RunState:    session.RunIdle,
	}
	created, err := store.Create(context.Background(), initial)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	message := agent.Message{
		ID: "message-1", ClientMessageID: "client-message-1", SessionID: created.Session.ID, Role: agent.RoleUser,
		Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "persist me"}},
	}
	request := session.CommitRequest{
		SessionID: created.Session.ID, ExpectedRevision: created.Revision, IdempotencyKey: "append-message-1",
		Changes: []session.Change{{Kind: session.AppendMessage, Message: &message}},
	}
	committed, err := store.Commit(context.Background(), request)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := store.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := openFileStore(t, directory)
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	loaded, err := reopened.Load(context.Background(), session.SessionRef{SessionID: created.Session.ID})
	if err != nil {
		t.Fatalf("Load after reopen: %v", err)
	}
	if loaded.Revision != committed.Revision || len(loaded.History) != 1 || loaded.History[0].Message.Parts[0].Text != "persist me" ||
		loaded.History[0].Message.ClientMessageID != "client-message-1" {
		t.Fatalf("loaded snapshot = %#v", loaded)
	}
	replayed, err := reopened.Commit(context.Background(), request)
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if replayed.Applied || replayed.Revision != committed.Revision ||
		replayed.FirstHistorySequence != committed.FirstHistorySequence || replayed.LastHistorySequence != committed.LastHistorySequence {
		t.Fatalf("idempotent replay = %#v", replayed)
	}
}

func TestFileStoreJournalIdentitySurvivesEquivalentJSONAfterReopen(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "sessions")
	store := openFileStore(t, directory)
	created, call := createPreparedJournal(t, store)
	if err := store.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := openFileStore(t, directory)
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	advanceJournalWithEquivalentArguments(t, reopened, created, call)
}

func TestMemoryStoreJournalIdentityMatchesFileStoreSemantics(t *testing.T) {
	store := session.NewMemoryStore()
	created, call := createPreparedJournal(t, store)
	advanceJournalWithEquivalentArguments(t, store, created, call)
}

func createPreparedJournal(t *testing.T, store session.SessionStore) (session.Snapshot, agent.ToolCall) {
	t.Helper()
	configuration := model.Config{ProviderKey: "provider", ModelID: "model", Reasoning: model.ReasoningDefault}
	assistant := agent.Message{
		ID: "message-tool", SessionID: "session-tool", RunID: "run-tool", StepID: "step-tool", Role: agent.RoleAssistant,
		Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "calling tool"}},
	}
	call := agent.ToolCall{
		ID: "call-tool", MessageID: assistant.ID, SessionID: assistant.SessionID, RunID: assistant.RunID, StepID: assistant.StepID,
		Name: "read", Arguments: []byte(`{"path":"a/b", "count":1e0}`),
	}
	started := session.RunFact{
		SessionID: assistant.SessionID, RunID: call.RunID, Kind: session.RunStarted,
		ModelConfig: configuration, ConfigRevision: 1,
	}
	prepared := session.JournalEntry{RunID: call.RunID, StepID: call.StepID, ToolCall: &call, Status: session.JournalPrepared}
	created, err := store.Create(context.Background(), session.NewSession{
		Session: agent.Session{ID: assistant.SessionID, AgentID: "agent-1", WorkspaceID: "workspace-1"},
		History: []session.HistoryFact{{Run: &started}, {Message: &assistant}, {ToolCall: &call}}, RunJournal: []session.JournalEntry{prepared},
		ModelConfig: configuration, RunState: session.RunRunning, ActiveRunID: call.RunID,
	})
	if err != nil {
		t.Fatalf("Create prepared journal: %v", err)
	}
	return created, call
}

func advanceJournalWithEquivalentArguments(t *testing.T, store session.SessionStore, created session.Snapshot, call agent.ToolCall) {
	t.Helper()
	restoredCall := call
	restoredCall.Arguments = []byte(`{"count":1.0,"path":"a\u002fb"}`)
	pending := session.JournalEntry{RunID: call.RunID, StepID: call.StepID, ToolCall: &restoredCall, Status: session.JournalPending}
	committed, err := store.Commit(context.Background(), session.CommitRequest{
		SessionID: created.Session.ID, ExpectedRevision: created.Revision, IdempotencyKey: "mark-tool-pending",
		Changes: []session.Change{{Kind: session.UpdateRunJournal, Journal: &pending}},
	})
	if err != nil {
		t.Fatalf("semantically identical journal update after reopen: %v", err)
	}
	loaded, err := store.Load(context.Background(), session.SessionRef{SessionID: created.Session.ID})
	if err != nil {
		t.Fatalf("Load committed journal: %v", err)
	}
	if loaded.Revision != committed.Revision || len(loaded.RunJournal) != 1 || loaded.RunJournal[0].Status != session.JournalPending {
		t.Fatalf("committed journal = %#v", loaded.RunJournal)
	}
}

func TestFixedManagerForkPersistsHistoryLineageInFileStore(t *testing.T) {
	directory := t.TempDir()
	store := openFileStore(t, directory)
	configuration := model.Config{ProviderKey: "provider", ModelID: "model", Reasoning: model.ReasoningDefault}
	manager, err := session.NewManager(store, configuration)
	if err != nil {
		t.Fatal(err)
	}
	source, err := manager.Create(context.Background(), session.CreateRequest{AgentID: "agent-1", WorkspaceID: "workspace-1"})
	if err != nil {
		t.Fatal(err)
	}
	message := agent.Message{
		ID: "message-1", SessionID: source.ID(), Role: agent.RoleUser,
		Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "persisted lineage"}},
	}
	if _, err := store.Commit(context.Background(), session.CommitRequest{
		SessionID: source.ID(), ExpectedRevision: source.Revision(), IdempotencyKey: "source-message",
		Changes: []session.Change{{Kind: session.AppendMessage, Message: &message}},
	}); err != nil {
		t.Fatal(err)
	}
	sourceView, err := store.Load(context.Background(), session.SessionRef{SessionID: source.ID()})
	if err != nil {
		t.Fatal(err)
	}
	forked, err := manager.Fork(context.Background(), session.ForkRequest{
		SourceSessionID: source.ID(), Mode: session.ForkFullHistory, AgentID: "agent-1", WorkspaceID: "workspace-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	reopened := openFileStore(t, directory)
	defer reopened.Close(context.Background())
	forkView, err := reopened.Load(context.Background(), session.SessionRef{SessionID: forked.ID()})
	if err != nil {
		t.Fatal(err)
	}
	if forkView.Fork == nil || forkView.Fork.ParentSessionID != source.ID() || len(forkView.History) != 1 {
		t.Fatalf("fork snapshot = %#v", forkView)
	}
	if forkView.History[0].OriginFactID != sourceView.History[0].FactID || forkView.History[0].FactID == forkView.History[0].OriginFactID {
		t.Fatalf("fork lineage = %#v, source = %#v", forkView.History[0], sourceView.History[0])
	}
}

func TestFixedManagerEmptyPrefixForkSurvivesFileStoreReopen(t *testing.T) {
	directory := t.TempDir()
	store := openFileStore(t, directory)
	manager, err := session.NewManager(store, defaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	source, err := manager.Create(context.Background(), session.CreateRequest{AgentID: "agent-1", WorkspaceID: "workspace-1"})
	if err != nil {
		t.Fatal(err)
	}
	active := message("message-active", source.ID(), agent.RoleUser, "first request")
	active.RunID, active.StepID = "run-active", "step-active"
	if _, err := store.Commit(context.Background(), session.CommitRequest{
		SessionID:        source.ID(),
		ExpectedRevision: source.Revision(),
		IdempotencyKey:   "start-active-run",
		Changes: []session.Change{
			{Kind: session.AppendMessage, Message: &active},
			{Kind: session.SetRunState, RunState: &session.RunStateChange{RunID: active.RunID, State: session.RunRunning}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	forked, err := manager.Fork(context.Background(), session.ForkRequest{
		SourceSessionID: source.ID(),
		Mode:            session.ForkHistoryPrefix,
		CutoffSequence:  0,
		AgentID:         "agent-1",
		WorkspaceID:     "workspace-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	reopened := openFileStore(t, directory)
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	child, err := reopened.Load(context.Background(), session.SessionRef{SessionID: forked.ID()})
	if err != nil {
		t.Fatal(err)
	}
	if len(child.History) != 0 || child.RunState != session.RunIdle || child.ActiveRunID.Valid() {
		t.Fatalf("reopened empty-prefix child = history %d state %q active %q", len(child.History), child.RunState, child.ActiveRunID)
	}
	if child.Fork == nil || child.Fork.ParentSessionID != source.ID() || child.Fork.CutoffSequence != 0 {
		t.Fatalf("reopened empty-prefix provenance = %#v", child.Fork)
	}
}

func TestFileStoreReopensForkAfterChildAppendsNewFact(t *testing.T) {
	directory := t.TempDir()
	store := openFileStore(t, directory)
	initialMessage := message("child-source-copy", "fork-child-append", agent.RoleUser, "copied from parent")
	created, err := store.Create(context.Background(), session.NewSession{
		Session: agent.Session{
			ID: "fork-child-append", AgentID: "agent-1", WorkspaceID: "workspace-1",
			ParentSessionID: "fork-parent", ParentRevision: 3,
		},
		History:     []session.HistoryFact{{OriginFactID: "parent-fact-1", Message: &initialMessage}},
		ModelConfig: defaultConfig(), RunState: session.RunIdle,
		Fork: &session.ForkProvenance{ParentSessionID: "fork-parent", CutoffSequence: 1},
	})
	if err != nil {
		t.Fatalf("Create fork: %v", err)
	}
	childMessage := message("child-new-fact", created.Session.ID, agent.RoleUser, "created by child")
	commitChanges(t, store, created.Session.ID, created.Revision, "child-append",
		session.Change{Kind: session.AppendMessage, Message: &childMessage},
	)
	if err := store.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened := openFileStore(t, directory)
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	loaded, err := reopened.Load(context.Background(), session.SessionRef{SessionID: created.Session.ID})
	if err != nil {
		t.Fatalf("Load reopened fork: %v", err)
	}
	if len(loaded.History) != 2 || loaded.History[0].OriginFactID != "parent-fact-1" || loaded.History[1].OriginFactID != "" {
		t.Fatalf("reopened fork History lineage = %#v", loaded.History)
	}
}

func TestFileStoreRejectsForkCutoffThatDoesNotMatchCopiedPrefix(t *testing.T) {
	store := openFileStore(t, t.TempDir())
	initialMessage := message("child-source-copy", "fork-cutoff-mismatch", agent.RoleUser, "copied from parent")
	_, err := store.Create(context.Background(), session.NewSession{
		Session: agent.Session{
			ID: "fork-cutoff-mismatch", AgentID: "agent-1", WorkspaceID: "workspace-1",
			ParentSessionID: "fork-parent", ParentRevision: 3,
		},
		History:     []session.HistoryFact{{OriginFactID: "parent-fact-1", Message: &initialMessage}},
		ModelConfig: defaultConfig(), RunState: session.RunIdle,
		Fork: &session.ForkProvenance{ParentSessionID: "fork-parent", CutoffSequence: 2},
	})
	if !agent.IsKind(err, agent.ErrorInvalidInput) {
		t.Fatalf("Create mismatched fork cutoff error = %v, want invalid_input", err)
	}
}

func TestFileStoreCASPublishesOnlyOneConcurrentCommit(t *testing.T) {
	store := openFileStore(t, t.TempDir())
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	created, err := store.Create(context.Background(), session.NewSession{
		Session:     agent.Session{ID: "session-cas", AgentID: "agent-1", WorkspaceID: "workspace-1"},
		ModelConfig: model.Config{ModelID: "model", Reasoning: model.ReasoningDefault},
		RunState:    session.RunIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errorsSeen := make(chan error, 2)
	for index := 0; index < 2; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			message := agent.Message{
				ID: agent.MessageID("message-" + string(rune('a'+index))), SessionID: created.Session.ID, Role: agent.RoleUser,
				Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "concurrent"}},
			}
			_, err := store.Commit(context.Background(), session.CommitRequest{
				SessionID: created.Session.ID, ExpectedRevision: created.Revision,
				IdempotencyKey: "concurrent-" + string(rune('a'+index)),
				Changes:        []session.Change{{Kind: session.AppendMessage, Message: &message}},
			})
			errorsSeen <- err
		}()
	}
	wait.Wait()
	close(errorsSeen)
	var success, conflict int
	for err := range errorsSeen {
		switch {
		case err == nil:
			success++
		case agent.IsCode(err, agent.CodeRevisionConflict):
			conflict++
		default:
			t.Fatalf("unexpected commit error: %v", err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
}

func TestFileStoreFailedTransactionDoesNotChangeDurableFile(t *testing.T) {
	directory := t.TempDir()
	store := openFileStore(t, directory)
	created, err := store.Create(context.Background(), session.NewSession{
		Session:     agent.Session{ID: "session-safe", AgentID: "agent-1", WorkspaceID: "workspace-1"},
		ModelConfig: model.Config{ModelID: "model", Reasoning: model.ReasoningDefault},
		RunState:    session.RunIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	invalid := agent.Message{ID: "message-invalid", SessionID: "other-session", Role: agent.RoleUser, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "bad"}}}
	if _, err := store.Commit(context.Background(), session.CommitRequest{
		SessionID: created.Session.ID, ExpectedRevision: created.Revision, IdempotencyKey: "invalid",
		Changes: []session.Change{{Kind: session.AppendMessage, Message: &invalid}},
	}); err == nil {
		t.Fatal("invalid transaction succeeded")
	}
	if err := store.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened := openFileStore(t, directory)
	defer reopened.Close(context.Background())
	loaded, err := reopened.Load(context.Background(), session.SessionRef{SessionID: created.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != created.Revision || len(loaded.History) != 0 {
		t.Fatalf("failed transaction changed durable state: %#v", loaded)
	}
}

func TestFileStoreRequiresOpenAndUsesPrivateFiles(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "store")
	store, err := session.NewFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Load(context.Background(), session.SessionRef{SessionID: "session-1"})
	if !agent.IsCode(err, agent.CodeApplicationNotStarted) {
		t.Fatalf("Load before Open = %v, code=%q", err, agent.CodeOf(err))
	}
	if err := store.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(context.Background(), session.NewSession{
		Session:     agent.Session{ID: "session-permissions", AgentID: "agent-1", WorkspaceID: "workspace-1"},
		ModelConfig: model.Config{ModelID: "model", Reasoning: model.ReasoningDefault},
		RunState:    session.RunIdle,
	})
	if err != nil || created.Revision != 1 {
		t.Fatalf("Create = %#v, %v", created, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("store files = %v", entries)
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("session file permissions = %o", info.Mode().Perm())
	}
	if err := store.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), session.SessionRef{SessionID: created.Session.ID}); !agent.IsCode(err, agent.CodeApplicationNotStarted) {
		t.Fatalf("Load after Close = %v", err)
	}
}

func TestFileStoreRejectsOversizedAggregateBeforeDecoding(t *testing.T) {
	directory := t.TempDir()
	store := openFileStore(t, directory)
	created, err := store.Create(context.Background(), session.NewSession{
		Session:     agent.Session{ID: "session-large", AgentID: "agent-1", WorkspaceID: "workspace-1"},
		ModelConfig: model.Config{ModelID: "model", Reasoning: model.ReasoningDefault},
		RunState:    session.RunIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v, %v", entries, err)
	}
	path := filepath.Join(directory, entries[0].Name())
	if err := os.Truncate(path, (256<<20)+1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), session.SessionRef{SessionID: created.Session.ID}); !agent.IsCode(err, agent.CodeSessionUnrecoverable) {
		t.Fatalf("oversized load error = %v, code=%q", err, agent.CodeOf(err))
	}
}

func openFileStore(t *testing.T, directory string) *session.FileStore {
	t.Helper()
	store, err := session.NewFileStore(directory)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	return store
}

var _ session.SessionStore = (*session.FileStore)(nil)
