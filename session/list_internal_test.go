package session

import (
	"testing"
	"time"

	agent "github.com/LyleLiu666/agentSlot/agent"
)

func TestSessionListCursorPreservesSessionIDTieBreakAcrossPages(t *testing.T) {
	index := sessionListIndex{}
	updatedAt := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	listed := []listedSession{
		{summary: listSummary("session-c", updatedAt)},
		{summary: listSummary("session-a", updatedAt)},
		{summary: listSummary("session-b", updatedAt)},
	}
	request := ListRequest{AgentID: "agent-1", WorkspaceID: "workspace-1", Limit: 2}
	first, err := index.paginate(request, listed)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Sessions) != 2 || first.Sessions[0].SessionID != "session-a" || first.Sessions[1].SessionID != "session-b" || first.NextCursor == "" {
		t.Fatalf("first page = %#v", first)
	}
	request.Cursor = first.NextCursor
	second, err := index.paginate(request, listed)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Sessions) != 1 || second.Sessions[0].SessionID != "session-c" || second.NextCursor != "" {
		t.Fatalf("second page = %#v", second)
	}
}

func listSummary(id agent.SessionID, updatedAt time.Time) SessionSummary {
	return SessionSummary{
		SessionID: id, AgentID: "agent-1", WorkspaceID: "workspace-1",
		Revision: 1, UpdatedAt: updatedAt,
	}
}
