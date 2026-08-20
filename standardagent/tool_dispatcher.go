package standardagent

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/observe"
	"github.com/LyleLiu666/agentSlot/policy"
	"github.com/LyleLiu666/agentSlot/tool"
)

// toolDispatcher is fixed Runtime machinery, not a replaceable Slot. Tools
// themselves remain independently replaceable keyed components.
type toolDispatcher struct {
	tools              map[string]installedTool
	orderedDefinitions []tool.Definition
	guards             []policy.PolicyGuard
	approval           policy.ApprovalService
	observations       *observationHub
}

func (d *toolDispatcher) withObservations(observations *observationHub) *toolDispatcher {
	copy := *d
	copy.observations = observations
	return &copy
}

type installedTool struct {
	value      tool.Tool
	definition tool.Definition
	safety     tool.ParallelSafety
}

func newToolDispatcher(installed []agentslot.Named[tool.Tool], guards []policy.PolicyGuard, approval policy.ApprovalService) (*toolDispatcher, error) {
	installed = append([]agentslot.Named[tool.Tool](nil), installed...)
	sort.Slice(installed, func(i, j int) bool { return installed[i].Key < installed[j].Key })
	dispatcher := &toolDispatcher{
		tools:    make(map[string]installedTool, len(installed)),
		guards:   append([]policy.PolicyGuard(nil), guards...),
		approval: approval,
	}
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
	if failure := d.authorize(ctx, call); failure != nil {
		return *failure
	}
	d.observeToolStarted(call)
	defer func() {
		if recover() != nil {
			result = failedToolResult(call.ID, "tool_internal", "tool execution failed")
		}
		d.observeToolCompleted(call, result)
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

// authorize is part of the fixed dispatcher so policy components can decide
// whether an action may run without becoming a second execution controller.
// Every extension receives a detached Action; only the dispatcher retains the
// invocation that can reach the Tool.
func (d *toolDispatcher) authorize(ctx context.Context, call agent.ToolCall) *tool.ToolResult {
	action := policy.Action{
		Kind: policy.ActionTool,
		Tool: &policy.ToolAction{
			ToolKey: call.Name,
			Call: tool.Call{
				ID:        call.ID,
				Name:      call.Name,
				Arguments: append([]byte(nil), call.Arguments...),
			},
			SessionID: call.SessionID,
			RunID:     call.RunID,
			StepID:    call.StepID,
		},
	}
	if err := action.Validate(); err != nil {
		d.auditTool(call, "error")
		result := failedToolResult(call.ID, "policy_error", "tool authorization failed")
		return &result
	}

	var approvalReasons []string
	for _, guard := range d.guards {
		decision, err := evaluateGuard(ctx, guard, action.Clone())
		if err != nil || decision.Validate() != nil {
			d.auditTool(call, "error")
			result := failedToolResult(call.ID, "policy_error", "tool authorization failed")
			return &result
		}
		switch decision.Effect {
		case policy.Deny:
			d.auditTool(call, "deny")
			result := failedToolResult(call.ID, "policy_denied", "tool execution was denied by policy")
			return &result
		case policy.RequireApproval:
			approvalReasons = append(approvalReasons, decision.Reason)
		}
	}
	if len(approvalReasons) == 0 {
		d.auditTool(call, "allow")
		return nil
	}
	if d.approval == nil {
		d.auditTool(call, "approval_required")
		result := failedToolResult(call.ID, "approval_required", "tool execution requires approval")
		return &result
	}
	decision, err := decideApproval(ctx, d.approval, policy.ApprovalRequest{
		Action: action.Clone(),
		Reason: strings.Join(approvalReasons, "; "),
	})
	if err != nil {
		d.auditTool(call, "approval_error")
		result := failedToolResult(call.ID, "approval_error", "tool approval failed")
		return &result
	}
	if !decision.Approved {
		d.auditTool(call, "approval_denied")
		result := failedToolResult(call.ID, "approval_denied", "tool execution approval was denied")
		return &result
	}
	d.auditTool(call, "approved")
	return nil
}

func (d *toolDispatcher) auditTool(call agent.ToolCall, decision string) {
	d.observations.publishAudit(observe.AuditRecord{
		Kind: observe.AuditToolDecision, At: time.Now().UTC(),
		Identity: observe.Identity{SessionID: call.SessionID, RunID: call.RunID, StepID: call.StepID, ToolCallID: call.ID},
		Action:   call.Name, Decision: decision,
	})
}

func (d *toolDispatcher) observeToolStarted(call agent.ToolCall) {
	now := time.Now().UTC()
	identity := observe.Identity{SessionID: call.SessionID, RunID: call.RunID, StepID: call.StepID, ToolCallID: call.ID}
	d.observations.publishTrace(observe.TraceRecord{Kind: observe.TraceToolStarted, At: now, Identity: identity})
}

func (d *toolDispatcher) observeToolCompleted(call agent.ToolCall, result tool.ToolResult) {
	now := time.Now().UTC()
	identity := observe.Identity{SessionID: call.SessionID, RunID: call.RunID, StepID: call.StepID, ToolCallID: call.ID}
	d.observations.publishTrace(observe.TraceRecord{Kind: observe.TraceToolCompleted, At: now, Identity: identity})
	d.observations.publishMetric(observe.MetricRecord{
		Name: observe.MetricToolCallTotal, Kind: observe.MetricCounter, Value: 1, At: now,
		Attributes: map[string]string{"tool": call.Name, "outcome": string(result.Status)},
	})
}

func evaluateGuard(ctx context.Context, guard policy.PolicyGuard, action policy.Action) (decision policy.Decision, err error) {
	defer func() {
		if recover() != nil {
			decision = policy.Decision{}
			err = errors.New("policy guard panicked")
		}
	}()
	return guard.Evaluate(ctx, action)
}

func decideApproval(ctx context.Context, approval policy.ApprovalService, request policy.ApprovalRequest) (decision policy.ApprovalDecision, err error) {
	defer func() {
		if recover() != nil {
			decision = policy.ApprovalDecision{}
			err = errors.New("approval service panicked")
		}
	}()
	return approval.Decide(ctx, request)
}

func failedToolResult(callID agent.ToolCallID, code, message string) tool.ToolResult {
	return tool.ToolResult{CallID: callID, Status: tool.ResultFailed, Error: &tool.StructuredError{Code: code, Message: message}}
}
