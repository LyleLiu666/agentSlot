package hook_test

import (
	"testing"

	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/model"
)

func TestInputGateContractKeepsInputImmutableAndUsesFiniteDecisions(t *testing.T) {
	view := hook.InputGateView{
		InvocationID: "invocation-1", Operation: hook.InputSend,
		SessionID: "session-1", AgentID: "agent-1", WorkspaceID: "workspace-1", Revision: 4,
		MessageID: "message-1", Input: agent.MessageInput{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "original"}}},
	}
	if err := view.Validate(); err != nil {
		t.Fatalf("valid InputGateView: %v", err)
	}
	contextMessage := agent.Message{ID: "context-1", SessionID: view.SessionID, Role: agent.RoleUser, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "separate context"}}}
	accepted := hook.InputGateResult{
		Decision: hook.DecisionAccept,
		Context:  []model.Input{{Message: &contextMessage}},
	}
	if err := accepted.Validate(view.SessionID); err != nil {
		t.Fatalf("valid accept result: %v", err)
	}
	rejectedWithContext := hook.InputGateResult{
		Decision: hook.DecisionReject, Reason: "blocked",
		Context: []model.Input{{Message: &contextMessage}},
	}
	if err := rejectedWithContext.Validate(view.SessionID); err == nil {
		t.Fatal("InputGate reject accepted additional context")
	}
	if err := (hook.InputGateResult{Decision: hook.DecisionAllow}).Validate(view.SessionID); err == nil {
		t.Fatal("InputGate accepted a Tool decision")
	}
	duplicateContext := hook.InputGateResult{
		Decision: hook.DecisionAccept,
		Context:  []model.Input{{Message: &contextMessage}, {Message: &contextMessage}},
	}
	if err := duplicateContext.Validate(view.SessionID); err == nil {
		t.Fatal("InputGate accepted context that is not a valid standalone model protocol fragment")
	}
}

func TestInputGateEditViewRequiresThePreviousInputFingerprint(t *testing.T) {
	view := hook.InputGateView{
		InvocationID: "invocation-1", Operation: hook.InputEditQueued,
		SessionID: "session-1", AgentID: "agent-1", WorkspaceID: "workspace-1", Revision: 4,
		MessageID: "message-1", Input: agent.MessageInput{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "edited"}}},
	}
	if err := view.Validate(); err == nil {
		t.Fatal("edit InputGateView accepted without previous input digest")
	}
	fingerprint, err := hook.FingerprintTypedInput(agent.MessageInput{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "before"}}})
	if err != nil {
		t.Fatal(err)
	}
	view.PreviousInputDigest = fingerprint.Digest
	if err := view.Validate(); err != nil {
		t.Fatalf("valid edit InputGateView: %v", err)
	}
}
