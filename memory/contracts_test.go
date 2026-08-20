package memory_test

import (
	"testing"
	"time"

	"github.com/LyleLiu666/agentSlot/memory"
)

func TestMemoryRequestsKeepLongTermMemoryOutsideSessionHistory(t *testing.T) {
	recall := memory.RecallRequest{
		Query: "where did we stop", Scopes: []memory.Scope{{Kind: memory.ScopeSession, ID: "session-1"}}, Limit: 5,
		SessionID: "session-1", RunID: "run-1", AgentID: "agent-1", WorkspaceID: "workspace-1",
	}
	if err := recall.Validate(); err != nil {
		t.Fatal(err)
	}
	remember := memory.RememberRequest{
		InvocationID: "call-1", SessionID: "session-1", RunID: "run-1", AgentID: "agent-1",
		Scope: memory.Scope{Kind: memory.ScopeWorkspace, ID: "workspace-1"}, Kind: memory.KindSemantic, Summary: "stable fact",
	}
	if err := remember.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (memory.ForgetRequest{
		SessionID: "session-1", RunID: "run-1", TargetID: "memory-1",
		Scope: memory.Scope{Kind: memory.ScopeWorkspace, ID: "workspace-1"}, Mode: memory.ForgetInvalidate, Reason: "obsolete",
	}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (memory.RememberRequest{
		InvocationID: "call-2", SessionID: "session-1", RunID: "run-1", AgentID: "agent-1",
		Scope: memory.Scope{Kind: memory.ScopeWorkspace, ID: "workspace-1"}, Kind: memory.KindTemporal,
		Subject: "project", Predicate: "status", Object: "active",
	}).Validate(); err == nil {
		t.Fatal("temporal memory accepted an invented implicit validity time")
	}
	if err := (memory.RememberRequest{
		InvocationID: "call-2", SessionID: "session-1", RunID: "run-1", AgentID: "agent-1",
		Scope: memory.Scope{Kind: memory.ScopeWorkspace, ID: "workspace-1"}, Kind: memory.KindTemporal,
		Subject: "project", Predicate: "status", Object: "active", ValidFrom: time.Now().UTC(),
	}).Validate(); err != nil {
		t.Fatal(err)
	}
}
