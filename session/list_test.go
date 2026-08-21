package session_test

import (
	"context"
	"testing"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestMemoryStoreListsSessionsByScopeAndMostRecentUpdate(t *testing.T) {
	ctx := context.Background()
	store := session.NewMemoryStore()
	first := createListedSession(t, ctx, store, "session-first", "agent-1", "workspace-1")
	_ = createListedSession(t, ctx, store, "session-other", "agent-2", "workspace-1")
	second := createListedSession(t, ctx, store, "session-second", "agent-1", "workspace-1")
	if _, err := store.Commit(ctx, session.CommitRequest{
		SessionID: first.Session.ID, ExpectedRevision: first.Revision, IdempotencyKey: "touch-first",
		Actor: agent.ActorIdentity{Kind: agent.ActorLocalUser, ID: "test"},
		Changes: []session.Change{{Kind: session.AppendMessage, Message: &agent.Message{
			ID: "message-1", SessionID: first.Session.ID, Role: agent.RoleUser,
			Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "hello"}},
		}}},
	}); err != nil {
		t.Fatal(err)
	}

	listed, err := store.ListSessions(ctx, session.ListRequest{AgentID: "agent-1", WorkspaceID: "workspace-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Sessions) != 2 || listed.Sessions[0].SessionID != first.Session.ID || listed.Sessions[1].SessionID != second.Session.ID {
		t.Fatalf("ListSessions() = %#v", listed.Sessions)
	}
	if listed.Sessions[0].UpdatedAt.IsZero() || listed.Sessions[0].Revision != first.Revision.Next() {
		t.Fatalf("latest summary = %#v", listed.Sessions[0])
	}
}

func TestFileStoreListsSessionsAfterReopen(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	store, err := session.NewFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Open(ctx); err != nil {
		t.Fatal(err)
	}
	created := createListedSession(t, ctx, store, "session-file", "agent-1", "workspace-1")
	if err := store.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Open(ctx); err != nil {
		t.Fatal(err)
	}
	defer store.Close(ctx)

	listed, err := store.ListSessions(ctx, session.ListRequest{AgentID: "agent-1", WorkspaceID: "workspace-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Sessions) != 1 || listed.Sessions[0].SessionID != created.Session.ID || listed.Sessions[0].UpdatedAt.IsZero() {
		t.Fatalf("ListSessions() = %#v", listed.Sessions)
	}
}

func createListedSession(t *testing.T, ctx context.Context, store session.SessionStore, id agent.SessionID, agentID agent.AgentID, workspaceID agent.WorkspaceID) session.Snapshot {
	t.Helper()
	created, err := store.Create(ctx, session.NewSession{
		Session:     agent.Session{ID: id, AgentID: agentID, WorkspaceID: workspaceID},
		ModelConfig: model.Config{ProviderKey: "provider", ModelID: "model", Reasoning: model.ReasoningDefault},
		RunState:    session.RunIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}
