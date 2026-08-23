// Package policy defines the deterministic decision boundary applied before
// the fixed Runtime executes an external action.
package policy

import (
	"context"
	"errors"
	"fmt"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/tool"
)

var (
	GuardSlot    = agentslot.Chain[PolicyGuard]("policy.guard")
	ApprovalSlot = agentslot.One[ApprovalService]("approval.service")
)

type ActionKind string

const ActionTool ActionKind = "tool"

// Action is an immutable proposal. A Guard can inspect but cannot replace the
// call that the fixed dispatcher will execute.
type Action struct {
	Kind ActionKind
	Tool *ToolAction
}

// ToolAction carries the trusted Session scope alongside the detached model
// proposal so policy never needs to infer ownership from tool arguments.
type ToolAction struct {
	ToolKey     string
	Call        tool.Call
	SessionID   agent.SessionID
	AgentID     agent.AgentID
	WorkspaceID agent.WorkspaceID
	RunID       agent.RunID
	StepID      agent.StepID
}

func (a Action) Validate() error {
	if a.Kind != ActionTool || a.Tool == nil {
		return errors.New("policy: action must contain one tool proposal")
	}
	if a.Tool.ToolKey == "" || a.Tool.Call.Name != a.Tool.ToolKey || !a.Tool.Call.ID.Valid() || !a.Tool.SessionID.Valid() || !a.Tool.AgentID.Valid() || !a.Tool.WorkspaceID.Valid() || !a.Tool.RunID.Valid() || !a.Tool.StepID.Valid() {
		return errors.New("policy: tool proposal identity is incomplete")
	}
	return nil
}

func (a Action) Clone() Action {
	copy := a
	if a.Tool != nil {
		proposal := *a.Tool
		proposal.Call.Arguments = append([]byte(nil), a.Tool.Call.Arguments...)
		copy.Tool = &proposal
	}
	return copy
}

type Effect string

const (
	Allow           Effect = "allow"
	Deny            Effect = "deny"
	RequireApproval Effect = "require_approval"
)

type Decision struct {
	Effect Effect
	Reason string
}

func (d Decision) Validate() error {
	switch d.Effect {
	case Allow:
		return nil
	case Deny, RequireApproval:
		if d.Reason == "" {
			return errors.New("policy: deny and approval decisions require a reason")
		}
		return nil
	default:
		return fmt.Errorf("policy: unknown effect %q", d.Effect)
	}
}

type PolicyGuard interface {
	// Evaluate may be called concurrently for different Sessions and must not
	// retain or mutate shared request state without its own synchronization.
	Evaluate(context.Context, Action) (Decision, error)
}

type GuardFunc func(context.Context, Action) (Decision, error)

func (f GuardFunc) Evaluate(ctx context.Context, action Action) (Decision, error) {
	if f == nil {
		return Decision{}, errors.New("policy: nil guard function")
	}
	return f(ctx, action)
}

type ApprovalRequest struct {
	Action Action
	Reason string
}

type ApprovalDecision struct {
	Approved bool
	Reason   string
}

type ApprovalService interface {
	// Decide may be called concurrently for different Sessions.
	Decide(context.Context, ApprovalRequest) (ApprovalDecision, error)
}

type ApprovalFunc func(context.Context, ApprovalRequest) (ApprovalDecision, error)

func (f ApprovalFunc) Decide(ctx context.Context, request ApprovalRequest) (ApprovalDecision, error) {
	if f == nil {
		return ApprovalDecision{}, errors.New("policy: nil approval function")
	}
	return f(ctx, request)
}

type ToolRule struct {
	ToolKey  string
	Decision Decision
}

// NewToolRuleGuard returns a deterministic reference Guard. Rules override the
// default by exact Tool Slot key; there is no natural-language matching.
func NewToolRuleGuard(fallback Decision, rules ...ToolRule) (PolicyGuard, error) {
	if err := fallback.Validate(); err != nil {
		return nil, err
	}
	decisions := make(map[string]Decision, len(rules))
	for _, rule := range rules {
		if rule.ToolKey == "" {
			return nil, errors.New("policy: tool rule key is required")
		}
		if err := rule.Decision.Validate(); err != nil {
			return nil, err
		}
		if _, duplicate := decisions[rule.ToolKey]; duplicate {
			return nil, fmt.Errorf("policy: duplicate rule for %q", rule.ToolKey)
		}
		decisions[rule.ToolKey] = rule.Decision
	}
	return &toolRuleGuard{fallback: fallback, decisions: decisions}, nil
}

type toolRuleGuard struct {
	fallback  Decision
	decisions map[string]Decision
}

func (g *toolRuleGuard) Evaluate(ctx context.Context, action Action) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	if action.Tool == nil {
		return Decision{}, errors.New("policy: tool rule guard received a non-tool action")
	}
	if decision, ok := g.decisions[action.Tool.ToolKey]; ok {
		return decision, nil
	}
	return g.fallback, nil
}
