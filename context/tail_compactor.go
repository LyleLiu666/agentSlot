package context

import (
	stdcontext "context"
	"errors"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
)

// TailCompactor is a deterministic reference compactor that retains recent
// protocol-complete groups. It is explicit, not the framework-wide default;
// Agent projects may replace it through CompactorSlot.
type TailCompactor struct{ maxInputs int }

var _ ContextCompactor = (*TailCompactor)(nil)

func NewTailCompactor(maxInputs int) (*TailCompactor, error) {
	if maxInputs <= 0 {
		return nil, errors.New("context: max inputs must be positive")
	}
	return &TailCompactor{maxInputs: maxInputs}, nil
}

func (c *TailCompactor) Compact(ctx stdcontext.Context, input CompactionInput) (CompactionOutput, error) {
	if err := ctx.Err(); err != nil {
		return CompactionOutput{}, err
	}
	if err := model.ValidateInputs(input.Inputs); err != nil {
		return CompactionOutput{}, err
	}
	groups := protocolGroups(input.Inputs)
	start, count := len(groups), 0
	for start > 0 {
		groupSize := groups[start-1][1] - groups[start-1][0]
		if count > 0 && count+groupSize > c.maxInputs {
			break
		}
		start--
		count += groupSize
	}
	first := 0
	if start < len(groups) {
		first = groups[start][0]
	}
	return CompactionOutput{SourceRevision: input.Revision, Inputs: cloneInputs(input.Inputs[first:])}, nil
}

func protocolGroups(inputs []model.Input) [][2]int {
	groups := make([][2]int, 0)
	for start := 0; start < len(inputs); {
		end := start + 1
		if inputs[start].Message != nil && inputs[start].Message.Role == agent.RoleAssistant {
			for end < len(inputs) && (inputs[end].ToolCall != nil || inputs[end].ToolResult != nil) {
				end++
			}
		}
		groups = append(groups, [2]int{start, end})
		start = end
	}
	return groups
}

func cloneInputs(source []model.Input) []model.Input {
	result := make([]model.Input, len(source))
	for index, input := range source {
		result[index] = input
		if input.SystemPrompt != nil {
			value := *input.SystemPrompt
			result[index].SystemPrompt = &value
		}
		if input.Message != nil {
			value := *input.Message
			value.Parts = append([]agent.MessagePart(nil), input.Message.Parts...)
			result[index].Message = &value
		}
		if input.ToolCall != nil {
			value := *input.ToolCall
			value.Arguments = append([]byte(nil), input.ToolCall.Arguments...)
			result[index].ToolCall = &value
		}
		if input.ToolResult != nil {
			value := *input.ToolResult
			value.Output = append([]byte(nil), input.ToolResult.Output...)
			if input.ToolResult.Error != nil {
				errorValue := *input.ToolResult.Error
				value.Error = &errorValue
			}
			result[index].ToolResult = &value
		}
	}
	return result
}
