package session_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestSessionStoresPageWithOpaqueScopeBoundCursor(t *testing.T) {
	for _, storeKind := range []string{"memory", "file"} {
		storeKind := storeKind
		t.Run(storeKind, func(t *testing.T) {
			store, _ := newPaginationStore(t, storeKind)
			ctx := context.Background()
			for index := 1; index <= 5; index++ {
				createListedSession(t, ctx, store, agent.SessionID(fmt.Sprintf("session-%d", index)), "agent-1", "workspace-1")
			}
			createListedSession(t, ctx, store, "foreign-agent", "agent-2", "workspace-1")
			createListedSession(t, ctx, store, "foreign-workspace", "agent-1", "workspace-2")

			first, err := store.ListSessions(ctx, session.ListRequest{
				AgentID: "agent-1", WorkspaceID: "workspace-1", Limit: 2,
			})
			if err != nil {
				t.Fatalf("ListSessions(first) error = %v", err)
			}
			assertSessionIDs(t, first.Sessions, "session-5", "session-4")
			if first.NextCursor == "" {
				t.Fatal("ListSessions(first) NextCursor is empty")
			}
			if cap(first.Sessions) != len(first.Sessions) {
				t.Fatalf("ListSessions(first) capacity = %d, want bounded capacity %d", cap(first.Sessions), len(first.Sessions))
			}

			second, err := store.ListSessions(ctx, session.ListRequest{
				AgentID: "agent-1", WorkspaceID: "workspace-1", Limit: 2, Cursor: first.NextCursor,
			})
			if err != nil {
				t.Fatalf("ListSessions(second) error = %v", err)
			}
			assertSessionIDs(t, second.Sessions, "session-3", "session-2")
			if second.NextCursor == "" {
				t.Fatal("ListSessions(second) NextCursor is empty")
			}

			last, err := store.ListSessions(ctx, session.ListRequest{
				AgentID: "agent-1", WorkspaceID: "workspace-1", Limit: 2, Cursor: second.NextCursor,
			})
			if err != nil {
				t.Fatalf("ListSessions(last) error = %v", err)
			}
			assertSessionIDs(t, last.Sessions, "session-1")
			if last.NextCursor != "" {
				t.Fatalf("ListSessions(last) NextCursor = %q, want empty", last.NextCursor)
			}

			for _, request := range []session.ListRequest{
				{AgentID: "agent-2", WorkspaceID: "workspace-1", Limit: 2, Cursor: first.NextCursor},
				{AgentID: "agent-1", WorkspaceID: "workspace-2", Limit: 2, Cursor: first.NextCursor},
			} {
				if _, err := store.ListSessions(ctx, request); !agent.IsKind(err, agent.ErrorInvalidInput) {
					t.Fatalf("cross-scope cursor error = %v, want invalid_input", err)
				}
			}

			tampered := first.NextCursor
			if tampered[0] == 'A' {
				tampered = "B" + tampered[1:]
			} else {
				tampered = "A" + tampered[1:]
			}
			if _, err := store.ListSessions(ctx, session.ListRequest{
				AgentID: "agent-1", WorkspaceID: "workspace-1", Limit: 2, Cursor: tampered,
			}); !agent.IsKind(err, agent.ErrorInvalidInput) {
				t.Fatalf("signed cursor tamper error = %v, want invalid_input", err)
			}
		})
	}
}

func TestSessionStoresExcludeConcurrentCreatesFromCursorTraversal(t *testing.T) {
	for _, storeKind := range []string{"memory", "file"} {
		storeKind := storeKind
		t.Run(storeKind, func(t *testing.T) {
			store, _ := newPaginationStore(t, storeKind)
			ctx := context.Background()
			for index := 1; index <= 3; index++ {
				createListedSession(t, ctx, store, agent.SessionID(fmt.Sprintf("session-%d", index)), "agent-1", "workspace-1")
			}
			first, err := store.ListSessions(ctx, session.ListRequest{AgentID: "agent-1", WorkspaceID: "workspace-1", Limit: 2})
			if err != nil {
				t.Fatal(err)
			}
			assertSessionIDs(t, first.Sessions, "session-3", "session-2")

			createListedSession(t, ctx, store, "session-new", "agent-1", "workspace-1")
			second, err := store.ListSessions(ctx, session.ListRequest{
				AgentID: "agent-1", WorkspaceID: "workspace-1", Limit: 2, Cursor: first.NextCursor,
			})
			if err != nil {
				t.Fatal(err)
			}
			assertSessionIDs(t, second.Sessions, "session-1")
			if second.NextCursor != "" {
				t.Fatalf("NextCursor = %q, want empty", second.NextCursor)
			}

			fresh, err := store.ListSessions(ctx, session.ListRequest{AgentID: "agent-1", WorkspaceID: "workspace-1", Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			assertSessionIDs(t, fresh.Sessions, "session-new")
		})
	}
}

func TestSessionStoresDoNotRepeatUpdatedSessionAcrossPages(t *testing.T) {
	for _, storeKind := range []string{"memory", "file"} {
		storeKind := storeKind
		t.Run(storeKind, func(t *testing.T) {
			store, _ := newPaginationStore(t, storeKind)
			ctx := context.Background()
			firstSnapshot := createListedSession(t, ctx, store, "session-1", "agent-1", "workspace-1")
			createListedSession(t, ctx, store, "session-2", "agent-1", "workspace-1")
			createListedSession(t, ctx, store, "session-3", "agent-1", "workspace-1")

			first, err := store.ListSessions(ctx, session.ListRequest{AgentID: "agent-1", WorkspaceID: "workspace-1", Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			assertSessionIDs(t, first.Sessions, "session-3")

			updated := touchListedSession(t, ctx, store, firstSnapshot, "touch-session-1")
			second, err := store.ListSessions(ctx, session.ListRequest{
				AgentID: "agent-1", WorkspaceID: "workspace-1", Limit: 5, Cursor: first.NextCursor,
			})
			if err != nil {
				t.Fatal(err)
			}
			assertSessionIDs(t, second.Sessions, "session-2")
			fresh, err := store.ListSessions(ctx, session.ListRequest{AgentID: "agent-1", WorkspaceID: "workspace-1", Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			assertSessionIDs(t, fresh.Sessions, "session-1")
			if !fresh.Sessions[0].UpdatedAt.After(first.Sessions[0].UpdatedAt) || fresh.Sessions[0].Revision != updated.Revision {
				t.Fatalf("updated summary = %#v, first page = %#v", fresh.Sessions[0], first.Sessions[0])
			}
		})
	}
}

func TestSessionListLimitCursorAndLifecycleValidation(t *testing.T) {
	for _, storeKind := range []string{"memory", "file"} {
		storeKind := storeKind
		t.Run(storeKind, func(t *testing.T) {
			store, reopen := newPaginationStore(t, storeKind)
			ctx := context.Background()
			for index := 0; index < session.DefaultSessionListLimit+1; index++ {
				createListedSession(t, ctx, store, agent.SessionID(fmt.Sprintf("session-%03d", index)), "agent-1", "workspace-1")
			}
			first, err := store.ListSessions(ctx, session.ListRequest{AgentID: "agent-1", WorkspaceID: "workspace-1"})
			if err != nil {
				t.Fatal(err)
			}
			if len(first.Sessions) != session.DefaultSessionListLimit || first.NextCursor == "" {
				t.Fatalf("default page = %d cursor=%q", len(first.Sessions), first.NextCursor)
			}

			invalidRequests := []session.ListRequest{
				{AgentID: "agent-1", WorkspaceID: "workspace-1", Limit: -1},
				{AgentID: "agent-1", WorkspaceID: "workspace-1", Limit: session.MaxSessionListLimit + 1},
				{AgentID: "agent-1", WorkspaceID: "workspace-1", Cursor: strings.Repeat("x", session.MaxSessionListCursorBytes+1)},
				{AgentID: "agent-1", WorkspaceID: "workspace-1", Cursor: first.NextCursor + "tampered"},
			}
			for _, request := range invalidRequests {
				if _, err := store.ListSessions(ctx, request); !agent.IsKind(err, agent.ErrorInvalidInput) {
					t.Fatalf("ListSessions(%#v) error = %v, want invalid_input", request, err)
				}
			}

			if reopen != nil {
				reopen()
				if _, err := store.ListSessions(ctx, session.ListRequest{
					AgentID: "agent-1", WorkspaceID: "workspace-1", Cursor: first.NextCursor,
				}); !agent.IsKind(err, agent.ErrorInvalidInput) {
					t.Fatalf("cursor after reopen error = %v, want invalid_input", err)
				}
			} else {
				other := session.NewMemoryStore()
				if _, err := other.ListSessions(ctx, session.ListRequest{
					AgentID: "agent-1", WorkspaceID: "workspace-1", Cursor: first.NextCursor,
				}); !agent.IsKind(err, agent.ErrorInvalidInput) {
					t.Fatalf("cursor on another store error = %v, want invalid_input", err)
				}
			}
		})
	}
}

func TestSessionStoresRejectCanceledListWithoutReturningData(t *testing.T) {
	for _, storeKind := range []string{"memory", "file"} {
		storeKind := storeKind
		t.Run(storeKind, func(t *testing.T) {
			store, _ := newPaginationStore(t, storeKind)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			result, err := store.ListSessions(ctx, session.ListRequest{AgentID: "agent-1", WorkspaceID: "workspace-1"})
			if !agent.IsCode(err, agent.CodeCanceled) || len(result.Sessions) != 0 || result.NextCursor != "" {
				t.Fatalf("ListSessions(canceled) = %#v, %v", result, err)
			}
		})
	}
}

func newPaginationStore(t *testing.T, kind string) (session.SessionStore, func()) {
	t.Helper()
	if kind == "memory" {
		return session.NewMemoryStore(), nil
	}
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store, func() {
		if err := store.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := store.Open(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func touchListedSession(t *testing.T, ctx context.Context, store session.SessionStore, snapshot session.Snapshot, key string) session.Commit {
	t.Helper()
	messageID := agent.MessageID("message-" + key)
	commit, err := store.Commit(ctx, session.CommitRequest{
		SessionID: snapshot.Session.ID, ExpectedRevision: snapshot.Revision, IdempotencyKey: key,
		Actor: agent.ActorIdentity{Kind: agent.ActorLocalUser, ID: "test"},
		Changes: []session.Change{{Kind: session.AppendMessage, Message: &agent.Message{
			ID: messageID, SessionID: snapshot.Session.ID, Role: agent.RoleUser,
			Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "touch"}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return commit
}

func assertSessionIDs(t *testing.T, summaries []session.SessionSummary, expected ...agent.SessionID) {
	t.Helper()
	if len(summaries) != len(expected) {
		t.Fatalf("Session summaries = %#v, want IDs %v", summaries, expected)
	}
	for index, want := range expected {
		if summaries[index].SessionID != want {
			t.Fatalf("Session summaries[%d].SessionID = %q, want %q", index, summaries[index].SessionID, want)
		}
	}
}
