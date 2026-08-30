package hook

import (
	"context"
	"encoding/json"
	"fmt"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/tool"
)

// ToolResultHookSlot is the ordered, optional post-execution context chain.
// It sees only durable terminal results from tools that actually entered the
// pending state. The fixed Runtime remains the only Session writer.
var ToolResultHookSlot = agentslot.Chain[ToolResultHook]("hook.tool_result")

type ToolResultHook interface {
	Descriptor() ExtensionDescriptor
	Scope() ToolResultScope
	Evaluate(context.Context, ToolResultView) (ToolResultHookResult, error)
}

// ToolResultScope is immutable build-time metadata. ResultUnknown is excluded
// because an uncertain external effect must never trigger another external
// post-effect automatically.
type ToolResultScope struct {
	All      bool
	ToolKeys []string
	Statuses []tool.ResultStatus
}

func (s ToolResultScope) Validate() error {
	if s.All && len(s.ToolKeys) != 0 {
		return fmt.Errorf("hook: all-tools result scope cannot also name tool keys")
	}
	if !s.All && (len(s.ToolKeys) == 0 || len(s.ToolKeys) > 64) {
		return fmt.Errorf("hook: tool result scope requires a bounded explicit key set")
	}
	seenKeys := make(map[string]struct{}, len(s.ToolKeys))
	for _, key := range s.ToolKeys {
		if !validSafeIdentity(key, MaxExtensionKeyBytes) {
			return fmt.Errorf("hook: tool result scope key is invalid")
		}
		if _, duplicate := seenKeys[key]; duplicate {
			return fmt.Errorf("hook: tool result scope key %q is duplicated", key)
		}
		seenKeys[key] = struct{}{}
	}
	if len(s.Statuses) == 0 || len(s.Statuses) > 2 {
		return fmt.Errorf("hook: tool result scope requires a finite terminal status set")
	}
	seenStatuses := make(map[tool.ResultStatus]struct{}, len(s.Statuses))
	for _, status := range s.Statuses {
		if status != tool.ResultSucceeded && status != tool.ResultFailed {
			return fmt.Errorf("hook: tool result scope status %q is not post-execution eligible", status)
		}
		if _, duplicate := seenStatuses[status]; duplicate {
			return fmt.Errorf("hook: tool result scope status %q is duplicated", status)
		}
		seenStatuses[status] = struct{}{}
	}
	return nil
}

func (s ToolResultScope) Matches(toolKey string, status tool.ResultStatus) bool {
	toolMatch := s.All
	for _, key := range s.ToolKeys {
		if key == toolKey {
			toolMatch = true
			break
		}
	}
	if !toolMatch {
		return false
	}
	for _, candidate := range s.Statuses {
		if candidate == status {
			return true
		}
	}
	return false
}

// ToolResultView is detached from Session state. StepID is the ToolCall step;
// NextStepID is the exact context target allocated in the result commit.
type ToolResultView struct {
	InvocationID InvocationID
	SessionID    agent.SessionID
	AgentID      agent.AgentID
	WorkspaceID  agent.WorkspaceID
	Revision     agent.Revision
	RunID        agent.RunID
	StepID       agent.StepID
	NextStepID   agent.StepID
	MessageID    agent.MessageID
	ToolCallID   agent.ToolCallID
	ToolKey      string
	Arguments    json.RawMessage
	Result       tool.ToolResult
}

func (v ToolResultView) Validate() error {
	if !v.InvocationID.Valid() || !v.SessionID.Valid() || !v.AgentID.Valid() || !v.WorkspaceID.Valid() ||
		v.Revision == 0 || !v.RunID.Valid() || !v.StepID.Valid() || !v.NextStepID.Valid() || !v.MessageID.Valid() ||
		!v.ToolCallID.Valid() || !validSafeIdentity(v.ToolKey, MaxExtensionKeyBytes) || !json.Valid(v.Arguments) ||
		v.Result.Validate() != nil || v.Result.CallID != v.ToolCallID || v.Result.Status == tool.ResultUnknown {
		return fmt.Errorf("hook: invalid tool result view")
	}
	return nil
}

type ToolResultHookResult struct {
	Context []model.Input
}

func (r ToolResultHookResult) Validate(sessionID agent.SessionID) error {
	if err := validateContextProposal(r.Context, sessionID); err != nil {
		return fmt.Errorf("hook: invalid tool result context: %w", err)
	}
	return nil
}
