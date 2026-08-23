// Package loop defines the replaceable Agent run-control boundary.
package loop

import (
	"context"
	"errors"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
)

// AgentLoopSlot is the unique run-control implementation selected by an Agent
// application.
var AgentLoopSlot = agentslot.One[AgentLoop]("agent.loop")

// Outcome is the Loop's decision after advancing one durable Run.
type Outcome string

const (
	OutcomeContinue       Outcome = "continue"
	OutcomeCompleted      Outcome = "completed"
	OutcomeFailed         Outcome = "failed"
	OutcomeCanceled       Outcome = "canceled"
	OutcomeBudgetExceeded Outcome = "budget_exceeded"
	OutcomeWaiting        Outcome = "waiting"
)

// Valid reports whether the outcome belongs to the standard finite protocol.
func (o Outcome) Valid() bool {
	return o == OutcomeContinue || o == OutcomeCompleted || o == OutcomeFailed || o == OutcomeCanceled || o == OutcomeBudgetExceeded || o == OutcomeWaiting
}

// Terminal reports whether the outcome ends the current durable Run.
func (o Outcome) Terminal() bool { return o.Valid() && o != OutcomeContinue }

// ActionKind is the finite set of decisions an AgentLoop may submit to the
// fixed Runtime for one Run.
type ActionKind string

const (
	ActionRequestModel ActionKind = "request_model"
	ActionExecuteTools ActionKind = "execute_tools"
	ActionContinue     ActionKind = "continue"
	ActionFinish       ActionKind = "finish"
	ActionWait         ActionKind = "wait"
)

// Action is one ordered, Run-scoped strategy decision. Outcome is set only for
// ActionFinish; waiting has its own explicit action and outcome.
type Action struct {
	Kind    ActionKind
	Outcome Outcome
}

func (a Action) Validate() error {
	switch a.Kind {
	case ActionRequestModel, ActionExecuteTools, ActionContinue, ActionWait:
		if a.Outcome != "" {
			return errors.New("loop: only finish actions carry an outcome")
		}
		return nil
	case ActionFinish:
		if !a.Outcome.Terminal() || a.Outcome == OutcomeWaiting {
			return errors.New("loop: finish requires a non-waiting terminal outcome")
		}
		return nil
	default:
		return errors.New("loop: unknown action")
	}
}

// State is the fixed Runtime's response after one controlled action.
type State string

const (
	StateReadyForModel  State = "ready_for_model"
	StateToolsReady     State = "tools_ready"
	StateContinueReady  State = "continue_ready"
	StateCompleted      State = "completed"
	StateFailed         State = "failed"
	StateCanceled       State = "canceled"
	StateBudgetExceeded State = "budget_exceeded"
	StateWaiting        State = "waiting"
)

func (s State) Valid() bool {
	switch s {
	case StateReadyForModel, StateToolsReady, StateContinueReady, StateCompleted, StateFailed, StateCanceled, StateBudgetExceeded, StateWaiting:
		return true
	default:
		return false
	}
}

// Run is the framework-owned controlled execution boundary supplied to an
// AgentLoop. Act validates action ordering and performs only the requested
// fixed Runtime capability. Calls are sequential; implementations must not
// invoke Act concurrently.
type Run interface {
	SessionID() agent.SessionID
	RunID() agent.RunID
	// State reports the current framework-owned state, including a recovered
	// prepared-tool state presented to a newly constructed Loop instance.
	State() State
	Act(context.Context, Action) (State, error)
}

// AgentLoop owns the decision to continue or stop a durable Run. Implementors
// return a terminal Outcome; returning OutcomeContinue is a protocol error.
type AgentLoop interface {
	Run(context.Context, Run) (Outcome, error)
}
