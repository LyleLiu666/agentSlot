package memory_test

import (
	"testing"
	"time"

	"github.com/LyleLiu666/agentSlot/memory"
)

func TestMemoryRequestsKeepLongTermMemoryOutsideSessionHistory(t *testing.T) {
	operation := memoryOperation()
	recall := memory.RecallRequest{
		Operation: operation, Query: "where did we stop", Intent: memory.RecallTaskContinuity,
		Scopes:           []memory.Scope{{Kind: memory.ScopeSession, ID: "session-1"}},
		VisibilityFilter: []memory.Visibility{memory.VisibilitySession}, Limit: 5,
	}
	if err := recall.Validate(); err != nil {
		t.Fatal(err)
	}
	remember := memory.RememberRequest{
		Operation: operation, InvocationID: "call-1",
		Scope:      memory.Scope{Kind: memory.ScopeWorkspace, ID: "workspace-1"},
		SourceKind: memory.SourceAssistantMessage, SourceRef: "message-1", Confidence: 0.8,
		Visibility: memory.VisibilityWorkspace, WritebackMode: memory.WritebackFull,
		Payload: &memory.SemanticPayload{Title: "architecture", Summary: "stable fact", TopicKeys: []string{"architecture"}},
	}
	if err := remember.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (memory.ForgetRequest{
		Operation: operation, TargetID: "memory-1",
		Scope: memory.Scope{Kind: memory.ScopeWorkspace, ID: "workspace-1"}, Mode: memory.ForgetInvalidate, Reason: "obsolete",
	}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (memory.RememberRequest{
		Operation: operation, InvocationID: "call-2",
		Scope:      memory.Scope{Kind: memory.ScopeWorkspace, ID: "workspace-1"},
		SourceKind: memory.SourceAssistantMessage, SourceRef: "message-1", Confidence: 0.8,
		Visibility: memory.VisibilityWorkspace, WritebackMode: memory.WritebackFull,
		Payload: &memory.TemporalPayload{Subject: "project", Predicate: "status", Object: "active"},
	}).Validate(); err == nil {
		t.Fatal("temporal memory accepted an invented implicit validity time")
	}
	if err := (memory.RememberRequest{
		Operation: operation, InvocationID: "call-2",
		Scope:      memory.Scope{Kind: memory.ScopeWorkspace, ID: "workspace-1"},
		SourceKind: memory.SourceAssistantMessage, SourceRef: "message-1", Confidence: 0.8,
		Visibility: memory.VisibilityWorkspace, WritebackMode: memory.WritebackFull,
		Payload: &memory.TemporalPayload{
			Subject: "project", Predicate: "status", Object: "active", ValidFrom: time.Now().UTC(),
		},
	}).Validate(); err != nil {
		t.Fatal(err)
	}
}
