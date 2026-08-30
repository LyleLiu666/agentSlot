package hook

import (
	"context"
	"encoding/json"
	"fmt"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/internal/jsonvalue"
)

// ToolPreflightSlot is the ordered, optional authorization-advice chain that
// runs after a ToolCall has been durably prepared and before policy approval
// or Tool execution. The fixed Runtime remains the only execution authority.
var ToolPreflightSlot = agentslot.Chain[ToolPreflight]("hook.tool_preflight")

type ToolPreflight interface {
	Descriptor() ExtensionDescriptor
	Scope() ToolScope
	Evaluate(context.Context, ToolPreflightView) (ToolPreflightResult, error)
}

// ToolScope is a build-time, deterministic selector. It deliberately supports
// only an explicit finite key set or all tools; dynamic predicates would make
// durable replay depend on mutable process state.
type ToolScope struct {
	All      bool
	ToolKeys []string
}

func (s ToolScope) Validate() error {
	if s.All {
		if len(s.ToolKeys) != 0 {
			return fmt.Errorf("hook: all-tools scope cannot also name tool keys")
		}
		return nil
	}
	if len(s.ToolKeys) == 0 || len(s.ToolKeys) > 64 {
		return fmt.Errorf("hook: tool scope requires a bounded explicit key set")
	}
	seen := make(map[string]struct{}, len(s.ToolKeys))
	for _, key := range s.ToolKeys {
		if !validSafeIdentity(key, MaxExtensionKeyBytes) {
			return fmt.Errorf("hook: tool scope key is invalid")
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("hook: tool scope key %q is duplicated", key)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func (s ToolScope) Matches(toolKey string) bool {
	if s.All {
		return true
	}
	for _, key := range s.ToolKeys {
		if key == toolKey {
			return true
		}
	}
	return false
}

// ToolPreflightView is a detached, immutable description of the already
// prepared call. Results cannot rewrite the call or its arguments.
type ToolPreflightView struct {
	InvocationID InvocationID
	SessionID    agent.SessionID
	AgentID      agent.AgentID
	WorkspaceID  agent.WorkspaceID
	Revision     agent.Revision
	RunID        agent.RunID
	StepID       agent.StepID
	MessageID    agent.MessageID
	ToolCallID   agent.ToolCallID
	ToolKey      string
	Arguments    json.RawMessage
}

func (v ToolPreflightView) Validate() error {
	if !v.InvocationID.Valid() || !v.SessionID.Valid() || !v.AgentID.Valid() || !v.WorkspaceID.Valid() ||
		v.Revision == 0 || !v.RunID.Valid() || !v.StepID.Valid() || !v.MessageID.Valid() || !v.ToolCallID.Valid() ||
		!validSafeIdentity(v.ToolKey, MaxExtensionKeyBytes) || !jsonvalue.Valid(v.Arguments) {
		return fmt.Errorf("hook: invalid tool preflight view")
	}
	return nil
}

type ToolPreflightResult struct {
	Decision Decision
	Reason   string
}

func (r ToolPreflightResult) Validate() error {
	if r.Decision != DecisionAllow && r.Decision != DecisionDeny && r.Decision != DecisionRequireApproval {
		return fmt.Errorf("hook: tool preflight decision must be allow, deny, or require_approval")
	}
	if err := ValidateSafeReason(r.Reason); err != nil {
		return err
	}
	if (r.Decision == DecisionDeny || r.Decision == DecisionRequireApproval) && r.Reason == "" {
		return fmt.Errorf("hook: restrictive tool preflight decision requires a reason")
	}
	return nil
}
