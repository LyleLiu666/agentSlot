package agent_test

import (
	"errors"
	"testing"

	"github.com/LyleLiu666/agentSlot/agent"
)

func TestStableIdentityTypesValidateNonEmptyTrimmedValues(t *testing.T) {
	valid := []struct {
		name  string
		valid func() bool
	}{
		{"agent", agent.AgentID("agent-1").Valid},
		{"workspace", agent.WorkspaceID("workspace-1").Valid},
		{"session", agent.SessionID("session-1").Valid},
		{"run", agent.RunID("run-1").Valid},
		{"step", agent.StepID("step-1").Valid},
		{"message", agent.MessageID("message-1").Valid},
		{"tool call", agent.ToolCallID("call-1").Valid},
	}
	for _, test := range valid {
		t.Run(test.name, func(t *testing.T) {
			if !test.valid() {
				t.Fatal("valid identity reported invalid")
			}
		})
	}

	if agent.SessionID(" ").Valid() || agent.RunID("").Valid() {
		t.Fatal("empty or whitespace identity reported valid")
	}
}

func TestMessageAndToolCallKeepStableContainment(t *testing.T) {
	message := agent.Message{
		ID:        "message-1",
		SessionID: "session-1",
		RunID:     "run-1",
		StepID:    "step-1",
		Role:      agent.RoleAssistant,
	}
	call := agent.ToolCall{
		ID:        "call-1",
		MessageID: message.ID,
		SessionID: message.SessionID,
		RunID:     message.RunID,
		StepID:    message.StepID,
		Name:      "example",
	}
	if call.MessageID != message.ID || call.SessionID != message.SessionID || call.RunID != message.RunID || call.StepID != message.StepID {
		t.Fatalf("tool call containment = %#v, want message containment", call)
	}
}

func TestRevisionAdvancesMonotonically(t *testing.T) {
	var revision agent.Revision
	if got := revision.Next(); got != 1 {
		t.Fatalf("Revision.Next() = %d, want 1", got)
	}
	if revision != 0 {
		t.Fatalf("Revision.Next() mutated receiver: %d", revision)
	}
}

func TestClassifiedErrorSupportsErrorsAs(t *testing.T) {
	base := errors.New("provider unavailable")
	err := agent.NewError(agent.ErrorUnavailable, "model.execute", "provider is unavailable", base)
	if !errors.Is(err, base) {
		t.Fatalf("classified error does not unwrap cause: %v", err)
	}
	if !agent.IsKind(err, agent.ErrorUnavailable) {
		t.Fatalf("error kind = %q, want %q", agent.KindOf(err), agent.ErrorUnavailable)
	}
	if agent.KindOf(base) != agent.ErrorInternal {
		t.Fatalf("unclassified error kind = %q, want %q", agent.KindOf(base), agent.ErrorInternal)
	}
}
