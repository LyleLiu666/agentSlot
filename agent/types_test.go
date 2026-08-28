package agent_test

import (
	"encoding/json"
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
		{"client message", agent.ClientMessageID("client-message-1").Valid},
		{"tool call", agent.ToolCallID("call-1").Valid},
		{"fact", agent.FactID("fact-1").Valid},
		{"attempt", agent.AttemptID("attempt-1").Valid},
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

func TestToolCallRejectsAmbiguousDuplicateJSONMembers(t *testing.T) {
	call := agent.ToolCall{
		ID: "call-1", MessageID: "message-1", SessionID: "session-1", RunID: "run-1", StepID: "step-1", Name: "example",
		Arguments: json.RawMessage(`{"command":"first","command":"second"}`),
	}
	if call.Valid() {
		t.Fatal("ToolCall accepted duplicate JSON object members")
	}
}

func TestActorIdentityUsesFiniteOriginVocabulary(t *testing.T) {
	if !(agent.ActorIdentity{Kind: agent.ActorRemoteUser, ID: "user-1"}).Valid() {
		t.Fatal("valid remote actor was rejected")
	}
	if (agent.ActorIdentity{Kind: "admin", ID: "user-1"}).Valid() || (agent.ActorIdentity{Kind: agent.ActorService}).Valid() {
		t.Fatal("invalid actor identity was accepted")
	}
}

func TestMessageAndToolCallKeepStableContainment(t *testing.T) {
	message := agent.Message{
		ID: "message-1", ClientMessageID: "client-message-1",
		SessionID: "session-1", RunID: "run-1", StepID: "step-1", Role: agent.RoleAssistant,
		Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "done"}},
	}
	if !message.Valid() {
		t.Fatalf("message reported invalid: %#v", message)
	}
	invalidClient := message
	invalidClient.ClientMessageID = " invalid "
	if invalidClient.Valid() {
		t.Fatal("message accepted an invalid optional client correlation identity")
	}
	call := agent.ToolCall{
		ID:        "call-1",
		MessageID: message.ID,
		SessionID: message.SessionID,
		RunID:     message.RunID,
		StepID:    message.StepID,
		Name:      "example",
		Arguments: []byte(`{}`),
	}
	if !call.Valid() {
		t.Fatalf("tool call reported invalid: %#v", call)
	}
	if call.MessageID != message.ID || call.SessionID != message.SessionID || call.RunID != message.RunID || call.StepID != message.StepID {
		t.Fatalf("tool call containment = %#v, want message containment", call)
	}
}

func TestOpaqueModelContinuationBelongsOnlyToAnAssistantMessage(t *testing.T) {
	continuation := &agent.ModelContinuation{
		ProviderKey: "provider", ModelID: "model",
		State: json.RawMessage(`[{"type":"opaque","signature":"provider-owned"}]`),
	}
	message := agent.Message{
		ID: "message-1", SessionID: "session-1", RunID: "run-1", StepID: "step-1",
		Role: agent.RoleAssistant, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "done"}},
		ModelContinuation: continuation,
	}
	if !message.Valid() {
		t.Fatalf("assistant continuation was rejected: %#v", message)
	}
	message.Role = agent.RoleUser
	if message.Valid() {
		t.Fatal("user message accepted provider-owned continuation")
	}
	message.Role = agent.RoleAssistant
	message.ModelContinuation.State = json.RawMessage(`not-json`)
	if message.Valid() {
		t.Fatal("message accepted malformed continuation state")
	}
}

func TestMessagePartsSeparateTextFromAttachmentReferences(t *testing.T) {
	if !(agent.MessagePart{Kind: agent.PartText, Text: "hello"}).Valid() {
		t.Fatal("text part reported invalid")
	}
	if !(agent.MessagePart{Kind: agent.PartAttachment, AttachmentID: "attachment-1", MediaType: "image/png"}).Valid() {
		t.Fatal("attachment reference reported invalid")
	}
	if (agent.MessagePart{Kind: agent.PartText, AttachmentID: "attachment-1"}).Valid() {
		t.Fatal("mixed part payload reported valid")
	}
}

func TestMessageInputCarriesContentWithoutDurableIdentity(t *testing.T) {
	input := agent.MessageInput{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "hello"}}}
	if !input.Valid() {
		t.Fatalf("MessageInput reported invalid: %#v", input)
	}
	if (agent.MessageInput{}).Valid() {
		t.Fatal("empty MessageInput reported valid")
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

func TestCodedErrorsExposeStableDomainReasons(t *testing.T) {
	err := agent.NewCodedError(
		agent.ErrorConflict,
		agent.CodeRevisionConflict,
		"session.commit",
		"revision changed",
		nil,
	)
	if !agent.IsKind(err, agent.ErrorConflict) || !agent.IsCode(err, agent.CodeRevisionConflict) {
		t.Fatalf("classified error = %v, kind=%q code=%q", err, agent.KindOf(err), agent.CodeOf(err))
	}
}
