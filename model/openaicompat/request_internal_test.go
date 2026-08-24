package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/artifact"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/tool"
)

func TestToolResultProjectionExposesStableArtifactMetadataWithoutStorageLocation(t *testing.T) {
	projection := toolResultProjectionFor(tool.ToolResult{
		Output:    json.RawMessage(`{"preview":"bounded"}`),
		Artifacts: []artifact.Metadata{{ID: "artifact-1", MediaType: "text/plain", Name: "full.txt", Size: 4096}},
	})
	encoded, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, expected := range []string{`"id":"artifact-1"`, `"media_type":"text/plain"`, `"name":"full.txt"`, `"size":4096`, `"preview":"bounded"`} {
		if !strings.Contains(text, expected) {
			t.Fatalf("projection %s lacks %s", text, expected)
		}
	}
	if strings.Contains(text, "path") || strings.Contains(text, "url") || strings.Contains(text, "key") {
		t.Fatalf("projection exposed storage location: %s", text)
	}
}

func TestBuildMessagesProjectsToolImageArtifactAsModelImageContent(t *testing.T) {
	callID := agent.ToolCallID("call-image")
	messageID := agent.MessageID("assistant-image")
	runID := agent.RunID("run-image")
	stepID := agent.StepID("step-image")
	inputs := []model.Input{
		{Message: &agent.Message{ID: messageID, SessionID: "session-image", RunID: runID, StepID: stepID, Role: agent.RoleAssistant, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "I will inspect it."}}}},
		{ToolCall: &agent.ToolCall{ID: callID, CorrelationID: "provider-call-image", MessageID: messageID, SessionID: "session-image", RunID: runID, StepID: stepID, Name: "files", Arguments: json.RawMessage(`{"argv":["read","diagram.png"]}`)}},
		{ToolResult: &tool.ToolResult{
			CallID: callID, Status: tool.ResultSucceeded, Output: json.RawMessage(`{"ok":true}`),
			Artifacts: []artifact.Metadata{{ID: "artifact-image", MediaType: "image/png", Name: "diagram.png", Size: 4}},
		}},
	}
	store := internalFixtureStore{body: []byte{0x89, 'P', 'N', 'G'}}
	messages, err := buildMessages(context.Background(), inputs, model.ExecutionCapabilities{
		Media: model.Capabilities{
			InputModalities: []model.Modality{model.ModalityText, model.ModalityImage}, OutputModalities: []model.Modality{model.ModalityText},
		},
		Reasoning: []model.Reasoning{model.ReasoningDefault}, ContextWindowTokens: 4096, MaxOutputTokens: 512,
	}, store, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || messages[0].Role != "assistant" || messages[1].Role != "tool" || messages[2].Role != "user" {
		t.Fatalf("messages = %#v", messages)
	}
	content, ok := messages[2].Content.([]chatContentPart)
	if !ok || len(content) != 1 || content[0].Type != "image_url" || content[0].ImageURL == nil || content[0].ImageURL.URL != "data:image/png;base64,iVBORw==" {
		t.Fatalf("tool image content = %#v", messages[2].Content)
	}
	if toolContent, ok := messages[1].Content.(string); !ok || !strings.Contains(toolContent, `"id":"artifact-image"`) {
		t.Fatalf("tool metadata projection = %#v", messages[1].Content)
	}
}

func TestTokenEstimateCountsToolImageSemanticsWithoutBase64Payload(t *testing.T) {
	body := append([]byte{0x89, 'P', 'N', 'G'}, bytes.Repeat([]byte{0}, 100_000)...)
	request := model.ModelRequest{
		Config: model.Config{ProviderKey: "openai", ModelID: "vision", Reasoning: model.ReasoningDefault},
		Inputs: toolImageInputs(int64(len(body))),
	}
	estimate, imageTokens, err := requestForTokenEstimate(context.Background(), request, internalFixtureStore{body: body}, 200_000)
	if err != nil {
		t.Fatal(err)
	}
	if imageTokens <= 0 || imageTokens >= 4096 {
		t.Fatalf("imageTokens = %d", imageTokens)
	}
	if got := estimate.Inputs[2].ToolResult.Artifacts; len(got) != 0 {
		t.Fatalf("estimate still carries tool image artifacts: %#v", got)
	}
}

func toolImageInputs(size int64) []model.Input {
	callID := agent.ToolCallID("call-image")
	messageID := agent.MessageID("assistant-image")
	runID := agent.RunID("run-image")
	stepID := agent.StepID("step-image")
	return []model.Input{
		{Message: &agent.Message{ID: messageID, SessionID: "session-image", RunID: runID, StepID: stepID, Role: agent.RoleAssistant, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "I will inspect it."}}}},
		{ToolCall: &agent.ToolCall{ID: callID, CorrelationID: "provider-call-image", MessageID: messageID, SessionID: "session-image", RunID: runID, StepID: stepID, Name: "files", Arguments: json.RawMessage(`{"argv":["read","diagram.png"]}`)}},
		{ToolResult: &tool.ToolResult{
			CallID: callID, Status: tool.ResultSucceeded, Output: json.RawMessage(`{"ok":true}`),
			Artifacts: []artifact.Metadata{{ID: "artifact-image", MediaType: "image/png", Name: "diagram.png", Size: size}},
		}},
	}
}

type internalFixtureStore struct{ body []byte }

func (internalFixtureStore) Write(context.Context, artifact.WriteRequest) (artifact.Metadata, error) {
	return artifact.Metadata{}, errors.New("not implemented")
}

func (s internalFixtureStore) Open(context.Context, string) (artifact.Content, error) {
	return artifact.Content{
		Metadata: artifact.Metadata{ID: "artifact-image", MediaType: "image/png", Name: "diagram.png", Size: int64(len(s.body))},
		Body:     io.NopCloser(bytes.NewReader(s.body)),
	}, nil
}
