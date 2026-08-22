package memory_test

import (
	"math"
	"testing"
	"time"

	"github.com/LyleLiu666/agentSlot/memory"
)

func TestRememberCandidateKeepsTypedSessionSummaryAndGovernance(t *testing.T) {
	request := memory.RememberRequest{
		Operation:     memoryOperation(),
		InvocationID:  "call-1",
		Scope:         memory.Scope{Kind: memory.ScopeSession, ID: "session-1"},
		SourceKind:    memory.SourceAssistantMessage,
		SourceRef:     "message-1",
		Confidence:    0.85,
		Visibility:    memory.VisibilitySession,
		WritebackMode: memory.WritebackSummaryOnly,
		Payload: &memory.SessionSummaryPayload{
			CurrentState:      "implementation is in progress",
			ValidatedFindings: []string{"the public contract was incomplete"},
			NextActions:       []string{"finish the adapter"},
			Blockers:          []string{"none"},
			KeyRefs:           []string{"fact-1"},
		},
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	if request.Payload.Kind() != memory.KindSessionSummary {
		t.Fatalf("kind = %q", request.Payload.Kind())
	}
}

func TestRememberRejectsLossyOrInventedCandidateSemantics(t *testing.T) {
	valid := memory.RememberRequest{
		Operation: memoryOperation(), InvocationID: "call-1",
		Scope:      memory.Scope{Kind: memory.ScopeSession, ID: "session-1"},
		SourceKind: memory.SourceAssistantMessage, SourceRef: "message-1", Confidence: 0.8,
		Visibility: memory.VisibilitySession, WritebackMode: memory.WritebackSummaryOnly,
		Payload: &memory.SessionSummaryPayload{
			CurrentState: "state", ValidatedFindings: []string{"finding"}, NextActions: []string{"next"},
		},
	}
	tests := map[string]func(*memory.RememberRequest){
		"summary outside session scope": func(request *memory.RememberRequest) {
			request.Scope = memory.Scope{Kind: memory.ScopeWorkspace, ID: "workspace-1"}
			request.Visibility = memory.VisibilityWorkspace
		},
		"summary missing findings": func(request *memory.RememberRequest) {
			request.Payload = &memory.SessionSummaryPayload{CurrentState: "state", NextActions: []string{"next"}}
		},
		"semantic missing topics": func(request *memory.RememberRequest) {
			request.Payload = &memory.SemanticPayload{Title: "title", Summary: "summary"}
		},
		"evidence missing governance": func(request *memory.RememberRequest) {
			request.Payload = &memory.EvidencePayload{EvidenceKind: memory.EvidenceToolOutput, BodyText: "body", MIMEType: "text/plain"}
		},
		"temporal invalid interval": func(request *memory.RememberRequest) {
			from := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
			to := from
			request.Payload = &memory.TemporalPayload{Subject: "project", Predicate: "state", Object: "active", ValidFrom: from, ValidTo: &to}
		},
		"non finite confidence": func(request *memory.RememberRequest) { request.Confidence = math.NaN() },
		"invalid source kind":   func(request *memory.RememberRequest) { request.SourceKind = "prompt_guess" },
		"missing source ref":    func(request *memory.RememberRequest) { request.SourceRef = "" },
		"missing visibility":    func(request *memory.RememberRequest) { request.Visibility = "" },
		"missing writeback":     func(request *memory.RememberRequest) { request.WritebackMode = "" },
		"missing payload":       func(request *memory.RememberRequest) { request.Payload = nil },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			request := valid
			mutate(&request)
			if err := request.Validate(); err == nil {
				t.Fatal("invalid remember request was accepted")
			}
		})
	}
}

func TestRememberAcceptsCompletePortableCandidateKinds(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		scope      memory.Scope
		visibility memory.Visibility
		payload    memory.CandidatePayload
		want       memory.Kind
	}{
		{
			name: "semantic", scope: memory.Scope{Kind: memory.ScopeUser, ID: "user-1"}, visibility: memory.VisibilityUser,
			payload: &memory.SemanticPayload{Title: "preference", Summary: "use Chinese", TopicKeys: []string{"communication"}, EvidenceRefs: []string{"message-1"}},
			want:    memory.KindSemantic,
		},
		{
			name: "evidence", scope: memory.Scope{Kind: memory.ScopeOrg, ID: "org-1"}, visibility: memory.VisibilityOrg,
			payload: &memory.EvidencePayload{EvidenceKind: memory.EvidenceDocumentChunk, BodyText: "approved policy", MIMEType: "text/plain", RedactionState: memory.RedactionClean},
			want:    memory.KindEvidence,
		},
		{
			name: "temporal", scope: memory.Scope{Kind: memory.ScopeWorkspace, ID: "workspace-1"}, visibility: memory.VisibilityWorkspace,
			payload: &memory.TemporalPayload{Subject: "release", Predicate: "deadline", Object: "tomorrow", ValidFrom: now, EvidenceRefs: []string{"fact-1"}},
			want:    memory.KindTemporal,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := memory.RememberRequest{
				Operation: memoryOperation(), InvocationID: "call-1", Scope: test.scope,
				SourceKind: memory.SourceUserMessage, SourceRef: "message-1", Confidence: 0.7,
				Visibility: test.visibility, WritebackMode: memory.WritebackFull, Payload: test.payload,
			}
			if err := request.Validate(); err != nil {
				t.Fatal(err)
			}
			if request.Payload.Kind() != test.want {
				t.Fatalf("kind = %q, want %q", request.Payload.Kind(), test.want)
			}
		})
	}
}

func TestRecallContractKeepsIntentEvidenceAndGovernanceFacts(t *testing.T) {
	request := memory.RecallRequest{
		Operation: memoryOperation(), Query: "deployment decision", Intent: memory.RecallEvidenceLookup,
		IncludeEvidence: true, Limit: 3,
		Scopes:           []memory.Scope{{Kind: memory.ScopeWorkspace, ID: "workspace-1"}},
		VisibilityFilter: []memory.Visibility{memory.VisibilityWorkspace, memory.VisibilityUser},
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	result := memory.RecallResult{Items: []memory.Item{{
		ID: "memory-1", Kind: memory.KindEvidence,
		Scope:   memory.Scope{Kind: memory.ScopeWorkspace, ID: "workspace-1"},
		Summary: "deployment approved", SourceRef: "call-9", Score: 0.9,
		ValidityState: memory.ValidityActive, AgentID: "agent-1", AgentRole: "reviewer",
		ParentRunID: "run-parent", RootRunID: "run-root", JobID: "job-1", Visibility: memory.VisibilityWorkspace,
	}}}
	if err := result.Validate(request); err != nil {
		t.Fatal(err)
	}
	result.Items[0].ValidityState = memory.ValidityInvalidated
	if err := result.Validate(request); err == nil {
		t.Fatal("recall accepted an invalidated memory")
	}
}

func TestRecallRejectsUnknownIntentVisibilityAndOverLimitResults(t *testing.T) {
	valid := memory.RecallRequest{
		Operation: memoryOperation(), Query: "fact", Intent: memory.RecallGeneral, Limit: 1,
		Scopes:           []memory.Scope{{Kind: memory.ScopeUser, ID: "user-1"}},
		VisibilityFilter: []memory.Visibility{memory.VisibilityUser},
	}
	unknownIntent := valid
	unknownIntent.Intent = "guess"
	if err := unknownIntent.Validate(); err == nil {
		t.Fatal("unknown recall intent was accepted")
	}
	unknownVisibility := valid
	unknownVisibility.VisibilityFilter = []memory.Visibility{"everyone"}
	if err := unknownVisibility.Validate(); err == nil {
		t.Fatal("unknown recall visibility was accepted")
	}
	result := memory.RecallResult{Items: []memory.Item{{}, {}}}
	if err := result.Validate(valid); err == nil {
		t.Fatal("over-limit recall result was accepted")
	}
}

func TestRecallResultCannotEscapeRequestedScopeOrVisibility(t *testing.T) {
	request := memory.RecallRequest{
		Operation: memoryOperation(), Query: "fact", Intent: memory.RecallGeneral, Limit: 1,
		Scopes:           []memory.Scope{{Kind: memory.ScopeWorkspace, ID: "workspace-1"}},
		VisibilityFilter: []memory.Visibility{memory.VisibilityWorkspace},
	}
	validItem := memory.Item{
		ID: "memory-1", Kind: memory.KindSemantic,
		Scope:   memory.Scope{Kind: memory.ScopeWorkspace, ID: "workspace-1"},
		Summary: "fact", SourceRef: "message-1", Score: 1,
		ValidityState: memory.ValidityActive, Visibility: memory.VisibilityWorkspace,
	}
	for name, mutate := range map[string]func(*memory.Item){
		"scope": func(item *memory.Item) {
			item.Scope = memory.Scope{Kind: memory.ScopeWorkspace, ID: "workspace-other"}
		},
		"visibility filter": func(item *memory.Item) {
			item.Visibility = memory.VisibilitySession
		},
		"summary scope": func(item *memory.Item) {
			item.Kind = memory.KindSessionSummary
		},
	} {
		t.Run(name, func(t *testing.T) {
			item := validItem
			mutate(&item)
			if err := (memory.RecallResult{Items: []memory.Item{item}}).Validate(request); err == nil {
				t.Fatal("out-of-contract recall item was accepted")
			}
		})
	}
}

func TestOperationAndWritePolicyRejectAmbiguousIdentityOrGovernance(t *testing.T) {
	operation := memoryOperation()
	operation.StepID = ""
	if err := operation.Validate(true); err == nil {
		t.Fatal("write operation without StepID was accepted")
	}
	policy := memory.WritePolicy{ScopeKind: memory.ScopeOrg, Visibility: memory.VisibilityOrg, WritebackMode: memory.WritebackFull}
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
	policy.Visibility = memory.VisibilitySession
	if err := policy.Validate(); err == nil {
		t.Fatal("cross-scope visibility was accepted")
	}
}

func memoryOperation() memory.OperationContext {
	return memory.OperationContext{
		SessionID: "session-1", RunID: "run-1", StepID: "step-1", AgentID: "agent-1", WorkspaceID: "workspace-1",
		AgentRole: "primary", ParentRunID: "run-parent", RootRunID: "run-root", JobID: "job-1",
	}
}
