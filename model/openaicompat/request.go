package openaicompat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"reflect"
	"strings"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/artifact"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/tool"
)

type chatRequest struct {
	Model           string        `json:"model"`
	Messages        []chatMessage `json:"messages"`
	Tools           []chatTool    `json:"tools,omitempty"`
	Stream          bool          `json:"stream"`
	StreamOptions   streamOptions `json:"stream_options"`
	Temperature     *float64      `json:"temperature,omitempty"`
	MaxTokens       *int          `json:"max_tokens,omitempty"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
}

type chatImageURL struct {
	URL string `json:"url"`
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatToolFunction `json:"function"`
}

type chatToolFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type chatTool struct {
	Type     string             `json:"type"`
	Function chatToolDefinition `json:"function"`
}

type chatToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

func buildRequest(ctx context.Context, request model.ModelRequest, capabilities model.ExecutionCapabilities, artifacts artifact.ArtifactStore, maxAttachmentBytes int64) ([]byte, error) {
	if err := model.ValidateInputs(request.Inputs); err != nil {
		return nil, err
	}
	messages, err := buildMessages(ctx, request.Inputs, capabilities, artifacts, maxAttachmentBytes)
	if err != nil {
		return nil, err
	}
	tools := make([]chatTool, len(request.Tools))
	for index, definition := range request.Tools {
		if err := definition.Validate(); err != nil {
			return nil, err
		}
		tools[index] = chatTool{Type: "function", Function: chatToolDefinition{
			Name: definition.Name, Description: definition.Description, Parameters: definition.InputSchema.JSON(),
		}}
	}
	wire := chatRequest{
		Model: request.Config.ModelID, Messages: messages, Tools: tools, Stream: true,
		StreamOptions: streamOptions{IncludeUsage: true},
		Temperature:   cloneFloat(request.Config.Parameters.Temperature),
		MaxTokens:     cloneInt(request.Config.Parameters.MaxTokens),
	}
	if request.Config.Reasoning != model.ReasoningDefault {
		wire.ReasoningEffort = string(request.Config.Reasoning)
	}
	return json.Marshal(wire)
}

func buildMessages(ctx context.Context, inputs []model.Input, capabilities model.ExecutionCapabilities, artifacts artifact.ArtifactStore, maxAttachmentBytes int64) ([]chatMessage, error) {
	messages := make([]chatMessage, 0, len(inputs))
	for index := 0; index < len(inputs); {
		input := inputs[index]
		if input.SystemPrompt != nil {
			messages = append(messages, chatMessage{Role: "system", Content: *input.SystemPrompt})
			index++
			continue
		}
		if input.Message == nil {
			return nil, errors.New("openaicompat: protocol item must begin with a message")
		}
		if input.Message.Role == agent.RoleTool {
			return nil, errors.New("openaicompat: tool messages require a correlated ToolResult")
		}
		content, err := messageContent(ctx, *input.Message, capabilities, artifacts, maxAttachmentBytes)
		if err != nil {
			return nil, err
		}
		message := chatMessage{Role: string(input.Message.Role), Content: content}
		index++
		if input.Message.Role != agent.RoleAssistant || index >= len(inputs) || inputs[index].ToolCall == nil {
			messages = append(messages, message)
			continue
		}
		correlations := make(map[agent.ToolCallID]string)
		for index < len(inputs) && inputs[index].ToolCall != nil {
			call := inputs[index].ToolCall
			correlation := call.CorrelationID
			if correlation == "" {
				correlation = string(call.ID)
			}
			correlations[call.ID] = correlation
			message.ToolCalls = append(message.ToolCalls, chatToolCall{
				ID: correlation, Type: "function",
				Function: chatToolFunction{Name: call.Name, Arguments: append(json.RawMessage(nil), call.Arguments...)},
			})
			index++
		}
		messages = append(messages, message)
		for index < len(inputs) && inputs[index].ToolResult != nil {
			result := inputs[index].ToolResult
			correlation, ok := correlations[result.CallID]
			if !ok {
				return nil, fmt.Errorf("openaicompat: result %q has no correlated call", result.CallID)
			}
			content := string(result.Output)
			if len(result.Artifacts) > 0 {
				encoded, err := json.Marshal(toolResultProjectionFor(*result))
				if err != nil {
					return nil, err
				}
				content = string(encoded)
			} else if result.Error != nil {
				encoded, err := json.Marshal(result.Error)
				if err != nil {
					return nil, err
				}
				content = string(encoded)
			}
			messages = append(messages, chatMessage{Role: "tool", ToolCallID: correlation, Content: content})
			index++
		}
	}
	return messages, nil
}

type toolResultArtifactReference struct {
	ID        string `json:"id"`
	MediaType string `json:"media_type"`
	Name      string `json:"name,omitempty"`
	Size      int64  `json:"size"`
}

type toolResultProjection struct {
	Output    json.RawMessage               `json:"output,omitempty"`
	Error     *tool.StructuredError         `json:"error,omitempty"`
	Artifacts []toolResultArtifactReference `json:"artifacts"`
}

func toolResultProjectionFor(result tool.ToolResult) toolResultProjection {
	projection := toolResultProjection{Output: result.Output, Error: result.Error, Artifacts: make([]toolResultArtifactReference, len(result.Artifacts))}
	for index, reference := range result.Artifacts {
		projection.Artifacts[index] = toolResultArtifactReference{ID: reference.ID, MediaType: reference.MediaType, Name: reference.Name, Size: reference.Size}
	}
	return projection
}

func messageContent(ctx context.Context, message agent.Message, capabilities model.ExecutionCapabilities, artifacts artifact.ArtifactStore, maxAttachmentBytes int64) (any, error) {
	hasAttachment := false
	for _, part := range message.Parts {
		hasAttachment = hasAttachment || part.Kind == agent.PartAttachment
	}
	if !hasAttachment {
		var content strings.Builder
		for _, part := range message.Parts {
			content.WriteString(part.Text)
		}
		if content.Len() == 0 {
			return nil, nil
		}
		return content.String(), nil
	}
	if !capabilities.Media.SupportsInput(model.ModalityImage) {
		return nil, errors.New("openaicompat: selected model does not accept image input")
	}
	if nilArtifactStore(artifacts) {
		return nil, errors.New("openaicompat: ArtifactStore is required for attachment input")
	}
	content := make([]chatContentPart, 0, len(message.Parts))
	for _, part := range message.Parts {
		switch part.Kind {
		case agent.PartText:
			content = append(content, chatContentPart{Type: "text", Text: part.Text})
		case agent.PartAttachment:
			url, err := imageDataURL(ctx, artifacts, part, maxAttachmentBytes)
			if err != nil {
				return nil, err
			}
			content = append(content, chatContentPart{Type: "image_url", ImageURL: &chatImageURL{URL: url}})
		}
	}
	return content, nil
}

func imageDataURL(ctx context.Context, artifacts artifact.ArtifactStore, part agent.MessagePart, maxAttachmentBytes int64) (string, error) {
	data, err := readImageAttachment(ctx, artifacts, part, maxAttachmentBytes)
	if err != nil {
		return "", err
	}
	return "data:" + part.MediaType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func readImageAttachment(ctx context.Context, artifacts artifact.ArtifactStore, part agent.MessagePart, maxAttachmentBytes int64) ([]byte, error) {
	if !supportedImageMediaType(part.MediaType) {
		return nil, fmt.Errorf("openaicompat: unsupported image media type %q", part.MediaType)
	}
	opened, err := artifacts.Open(ctx, part.AttachmentID)
	if err != nil {
		if errors.Is(err, artifact.ErrNotFound) {
			return nil, fmt.Errorf("openaicompat: attachment %q: %w", part.AttachmentID, artifact.ErrNotFound)
		}
		return nil, errors.New("openaicompat: attachment store failed")
	}
	if err := opened.Validate(); err != nil {
		if !nilReadCloser(opened.Body) {
			_ = opened.Body.Close()
		}
		return nil, fmt.Errorf("openaicompat: invalid opened attachment: %w", err)
	}
	if opened.Metadata.ID != part.AttachmentID || opened.Metadata.MediaType != part.MediaType {
		_ = opened.Body.Close()
		return nil, errors.New("openaicompat: opened attachment metadata does not match message reference")
	}
	if opened.Metadata.Size > maxAttachmentBytes {
		_ = opened.Body.Close()
		return nil, fmt.Errorf("openaicompat: attachment exceeds %d byte limit", maxAttachmentBytes)
	}
	data, readErr := io.ReadAll(io.LimitReader(opened.Body, maxAttachmentBytes+1))
	closeErr := opened.Body.Close()
	if readErr != nil {
		return nil, errors.New("openaicompat: attachment read failed")
	}
	if closeErr != nil {
		return nil, errors.New("openaicompat: attachment close failed")
	}
	if int64(len(data)) != opened.Metadata.Size {
		return nil, errors.New("openaicompat: opened attachment size does not match metadata")
	}
	return data, nil
}

func requestForTokenEstimate(ctx context.Context, request model.ModelRequest, artifacts artifact.ArtifactStore, maxAttachmentBytes int64) (model.ModelRequest, int, error) {
	estimate := request
	estimate.Inputs = make([]model.Input, len(request.Inputs))
	imageTokens := 0
	for index, input := range request.Inputs {
		estimate.Inputs[index] = input
		if input.Message == nil {
			continue
		}
		message := *input.Message
		message.Parts = append([]agent.MessagePart(nil), input.Message.Parts...)
		for partIndex, part := range message.Parts {
			if part.Kind != agent.PartAttachment {
				continue
			}
			if nilArtifactStore(artifacts) {
				return model.ModelRequest{}, 0, errors.New("openaicompat: ArtifactStore is required for attachment input")
			}
			data, err := readImageAttachment(ctx, artifacts, part, maxAttachmentBytes)
			if err != nil {
				return model.ModelRequest{}, 0, err
			}
			imageTokens += estimateImageTokens(data)
			message.Parts[partIndex] = agent.MessagePart{Kind: agent.PartText, Text: "[image]"}
		}
		estimate.Inputs[index].Message = &message
	}
	return estimate, imageTokens, nil
}

func estimateImageTokens(data []byte) int {
	decoded, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || decoded.Width <= 0 || decoded.Height <= 0 {
		estimate := (len(data) + 511) / 512
		if estimate < 256 {
			return 256
		}
		if estimate > 32_768 {
			return 32_768
		}
		return estimate
	}
	width := min(decoded.Width, 4_096)
	height := min(decoded.Height, 4_096)
	return 256 + (width*height+511)/512
}

func supportedImageMediaType(value string) bool {
	switch value {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

func nilReadCloser(value io.ReadCloser) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func cloneFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
