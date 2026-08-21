// Package loop defines the replaceable Agent run-control boundary.
package loop

import (
	"context"

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
)

// Valid reports whether the outcome belongs to the standard finite protocol.
func (o Outcome) Valid() bool {
	return o == OutcomeContinue || o == OutcomeCompleted || o == OutcomeFailed || o == OutcomeCanceled || o == OutcomeBudgetExceeded
}

// Terminal reports whether the outcome ends the current durable Run.
func (o Outcome) Terminal() bool { return o.Valid() && o != OutcomeContinue }

// Run is the framework-owned durable execution boundary supplied to an
// AgentLoop. Step performs one complete model/tool/continuation step. Calls are
// sequential; implementations must not invoke Step concurrently.
type Run interface {
	SessionID() agent.SessionID
	RunID() agent.RunID
	Step(context.Context) (Outcome, error)
}

// AgentLoop owns the decision to continue or stop a durable Run. Implementors
// return a terminal Outcome; returning OutcomeContinue is a protocol error.
type AgentLoop interface {
	Run(context.Context, Run) (Outcome, error)
}
