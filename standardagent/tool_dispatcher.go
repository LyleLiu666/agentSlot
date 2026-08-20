package standardagent

import (
	"context"
	"sort"
	"sync"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/tool"
)

// toolDispatcher is fixed Runtime machinery, not a replaceable Slot. Tools
// themselves remain independently replaceable keyed components.
type toolDispatcher struct {
	tools              map[string]installedTool
	orderedDefinitions []tool.Definition
}

type installedTool struct {
	value      tool.Tool
	definition tool.Definition
	safety     tool.ParallelSafety
}

func newToolDispatcher(installed []agentslot.Named[tool.Tool]) (*toolDispatcher, error) {
	installed = append([]agentslot.Named[tool.Tool](nil), installed...)
	sort.Slice(installed, func(i, j int) bool { return installed[i].Key < installed[j].Key })
	dispatcher := &toolDispatcher{tools: make(map[string]installedTool, len(installed))}
	for _, named := range installed {
		definition := named.Value.Definition()
		if err := definition.Validate(); err != nil {
			return nil, invalidInput("standardagent.tools", err.Error())
		}
		if definition.Name != named.Key {
			return nil, invalidInput("standardagent.tools", "Tool Slot key must match Definition name")
		}
		safety := named.Value.ParallelSafety()
		if !safety.Valid() {
			return nil, invalidInput("standardagent.tools", "Tool must declare Serial or ParallelSafe")
		}
		dispatcher.tools[named.Key] = installedTool{value: named.Value, definition: definition, safety: safety}
		dispatcher.orderedDefinitions = append(dispatcher.orderedDefinitions, definition)
	}
	return dispatcher, nil
}

func (d *toolDispatcher) definitions() []tool.Definition {
	return append([]tool.Definition(nil), d.orderedDefinitions...)
}

func (d *toolDispatcher) dispatch(ctx context.Context, calls []agent.ToolCall) []tool.ToolResult {
	results := make([]tool.ToolResult, len(calls))
	for index := 0; index < len(calls); {
		installed, ok := d.tools[calls[index].Name]
		if !ok || installed.safety == tool.Serial {
			results[index] = d.invoke(ctx, calls[index])
			index++
			continue
		}
		end := index
		for end < len(calls) {
			candidate, exists := d.tools[calls[end].Name]
			if !exists || candidate.safety != tool.ParallelSafe {
				break
			}
			end++
		}
		var wait sync.WaitGroup
		for callIndex := index; callIndex < end; callIndex++ {
			wait.Add(1)
			go func(callIndex int) {
				defer wait.Done()
				results[callIndex] = d.invoke(ctx, calls[callIndex])
			}(callIndex)
		}
		wait.Wait()
		index = end
	}
	return results
}

func (d *toolDispatcher) invoke(ctx context.Context, call agent.ToolCall) (result tool.ToolResult) {
	installed, ok := d.tools[call.Name]
	if !ok {
		return failedToolResult(call.ID, "tool_not_found", "requested tool is not installed")
	}
	if err := installed.definition.InputSchema.ValidateArguments(call.Arguments); err != nil {
		return failedToolResult(call.ID, "invalid_arguments", "tool arguments do not match the declared schema")
	}
	defer func() {
		if recover() != nil {
			result = failedToolResult(call.ID, "tool_internal", "tool execution failed")
		}
	}()
	result = installed.value.Invoke(ctx, tool.ToolInvocation{
		Call:      tool.Call{ID: call.ID, Name: call.Name, Arguments: append([]byte(nil), call.Arguments...)},
		SessionID: call.SessionID, RunID: call.RunID, StepID: call.StepID,
	})
	if result.Status == tool.ResultUnknown {
		return failedToolResult(call.ID, "invalid_tool_result", "outcome_unknown is reserved for crash recovery")
	}
	if result.CallID != call.ID || result.Validate() != nil {
		return failedToolResult(call.ID, "invalid_tool_result", "tool returned an invalid result")
	}
	return result
}

func failedToolResult(callID agent.ToolCallID, code, message string) tool.ToolResult {
	return tool.ToolResult{CallID: callID, Status: tool.ResultFailed, Error: &tool.StructuredError{Code: code, Message: message}}
}
