package openaicompat

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
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
	Content    string         `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
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

func buildRequest(request model.ModelRequest) ([]byte, error) {
	if err := model.ValidateInputs(request.Inputs); err != nil {
		return nil, err
	}
	messages, err := buildMessages(request.Inputs)
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

func buildMessages(inputs []model.Input) ([]chatMessage, error) {
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
		message := chatMessage{Role: string(input.Message.Role), Content: messageContent(*input.Message)}
		if input.Message.Role == agent.RoleTool {
			return nil, errors.New("openaicompat: tool messages require a correlated ToolResult")
		}
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
			if result.Error != nil {
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

func messageContent(message agent.Message) string {
	var content strings.Builder
	for _, part := range message.Parts {
		switch part.Kind {
		case agent.PartText:
			content.WriteString(part.Text)
		case agent.PartAttachment:
			if content.Len() > 0 {
				content.WriteByte('\n')
			}
			fmt.Fprintf(&content, "[attachment id=%s media_type=%s name=%s]", part.AttachmentID, part.MediaType, part.Name)
		}
	}
	return content.String()
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
