package hook

import (
	"context"
	"fmt"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
)

// CompletionGateSlot is the ordered, optional fail-closed chain evaluated at
// a natural Run completion. Runtime remains the only authority that may
// continue or complete the Run.
var CompletionGateSlot = agentslot.Chain[CompletionGate]("hook.completion_gate")

type CompletionGate interface {
	Descriptor() ExtensionDescriptor
	Evaluate(context.Context, CompletionView) (CompletionGateResult, error)
}

type CompletionDecision string

const (
	CompletionComplete CompletionDecision = "complete"
	CompletionContinue CompletionDecision = "continue"
)

func (d CompletionDecision) Valid() bool {
	return d == CompletionComplete || d == CompletionContinue
}

// CompletionGoalCandidate identifies an active Goal decision that Runtime has
// evaluated as done but has deliberately not committed yet. A gate can observe
// the candidate, but cannot alter or commit it.
type CompletionGoalCandidate struct {
	GoalID  string
	Version uint64
}

func (c CompletionGoalCandidate) Validate() error {
	if !validSafeIdentity(c.GoalID, MaxExtensionKeyBytes) || c.Version == 0 {
		return fmt.Errorf("hook: invalid completion goal candidate")
	}
	return nil
}

// CompletionView is detached from Session state. StepID is the naturally
// completed model step and NextStepID is the sole target a continuation may
// use. LastAssistantMessage is a copy; changing it cannot mutate History.
type CompletionView struct {
	InvocationID         InvocationID
	SessionID            agent.SessionID
	AgentID              agent.AgentID
	WorkspaceID          agent.WorkspaceID
	Revision             agent.Revision
	RunID                agent.RunID
	StepID               agent.StepID
	NextStepID           agent.StepID
	LastAssistantMessage agent.Message
	Budget               model.TokenBudget
	FollowOns            int
	GoalCandidate        *CompletionGoalCandidate `json:",omitempty"`
}

func (v CompletionView) Validate() error {
	if !v.InvocationID.Valid() || !v.SessionID.Valid() || !v.AgentID.Valid() || !v.WorkspaceID.Valid() ||
		v.Revision == 0 || !v.RunID.Valid() || !v.StepID.Valid() || !v.NextStepID.Valid() || v.StepID == v.NextStepID ||
		v.FollowOns < 0 || v.Budget.Validate() != nil || !v.LastAssistantMessage.Valid() ||
		v.LastAssistantMessage.SessionID != v.SessionID || v.LastAssistantMessage.RunID != v.RunID ||
		v.LastAssistantMessage.StepID != v.StepID || v.LastAssistantMessage.Role != agent.RoleAssistant ||
		v.LastAssistantMessage.ClientMessageID != "" || v.LastAssistantMessage.ModelContinuation != nil || !v.LastAssistantMessage.CreatedAt.IsZero() {
		return fmt.Errorf("hook: invalid completion view")
	}
	if v.GoalCandidate != nil {
		if err := v.GoalCandidate.Validate(); err != nil {
			return err
		}
	}
	return nil
}

type CompletionGateResult struct {
	Decision CompletionDecision
	Reason   string
	Context  []model.Input
}

func (r CompletionGateResult) Validate(view CompletionView) error {
	if !r.Decision.Valid() {
		return fmt.Errorf("hook: completion gate decision must be complete or continue")
	}
	if err := ValidateSafeReason(r.Reason); err != nil {
		return err
	}
	if r.Decision == CompletionComplete {
		if len(r.Context) != 0 {
			return fmt.Errorf("hook: completed gate cannot contribute context")
		}
		return nil
	}
	if len(r.Context) == 0 {
		return fmt.Errorf("hook: continued gate requires context")
	}
	if err := validateContextProposal(r.Context, view.SessionID); err != nil {
		return fmt.Errorf("hook: invalid completion context: %w", err)
	}
	for _, input := range r.Context {
		if input.Message == nil || input.SystemPrompt != nil || input.ToolCall != nil || input.ToolResult != nil ||
			input.Message.RunID != view.RunID || input.Message.StepID != view.NextStepID || input.Message.Role != agent.RoleUser {
			return fmt.Errorf("hook: completion context is not bound to the allocated next step")
		}
	}
	return nil
}
