// Package goal defines objective lifecycle and completion evaluation contracts.
// A Goal is not a CRUD-only record: when installed, the fixed AgentRuntime asks
// its evaluator before completing an otherwise finished Run.
package goal

import (
	"context"
	"errors"
	"strings"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
)

var (
	StoreSlot     = agentslot.One[Store]("goal.store")
	EvaluatorSlot = agentslot.One[Evaluator]("goal.evaluator")
)

type Status string

const (
	StatusActive    Status = "active"
	StatusPaused    Status = "paused"
	StatusCompleted Status = "completed"
	StatusCanceled  Status = "canceled"
)

func (s Status) Valid() bool {
	return s == StatusActive || s == StatusPaused || s == StatusCompleted || s == StatusCanceled
}

type Goal struct {
	ID           string
	SessionID    agent.SessionID
	Objective    string
	Status       Status
	Version      uint64
	FollowOns    int
	MaxFollowOns int
	UpdatedAt    time.Time
}

func (g Goal) Validate() error {
	if g.ID == "" || !g.SessionID.Valid() || strings.TrimSpace(g.Objective) == "" || strings.TrimSpace(g.Objective) != g.Objective || !g.Status.Valid() || g.Version == 0 ||
		g.FollowOns < 0 || g.MaxFollowOns <= 0 || g.FollowOns > g.MaxFollowOns || g.UpdatedAt.IsZero() {
		return errors.New("goal: invalid goal")
	}
	return nil
}

type SetRequest struct {
	SessionID       agent.SessionID
	Objective       string
	ExpectedVersion uint64
	MaxFollowOns    int
}

type StateChangeRequest struct {
	SessionID       agent.SessionID
	ExpectedVersion uint64
	Status          Status
}

type Decision string

const (
	DecisionContinue Decision = "continue"
	DecisionBlocked  Decision = "blocked"
	DecisionDone     Decision = "done"
)

func (d Decision) Valid() bool {
	return d == DecisionContinue || d == DecisionBlocked || d == DecisionDone
}

type ReasonCode string

const (
	ReasonProgressPossible ReasonCode = "progress_possible"
	ReasonObjectiveMet     ReasonCode = "objective_met"
	ReasonNeedsInput       ReasonCode = "needs_input"
	ReasonExternalBlocker  ReasonCode = "external_blocker"
	ReasonFollowOnLimit    ReasonCode = "follow_on_limit"
	ReasonEvaluatorFailure ReasonCode = "evaluator_failure"
)

func (r ReasonCode) Valid() bool {
	switch r {
	case ReasonProgressPossible, ReasonObjectiveMet, ReasonNeedsInput, ReasonExternalBlocker,
		ReasonFollowOnLimit, ReasonEvaluatorFailure:
		return true
	default:
		return false
	}
}

type Evaluation struct {
	Decision        Decision
	Reason          ReasonCode
	NextInstruction agent.MessageInput
}

func (e Evaluation) Validate() error {
	if !e.Decision.Valid() || !e.Reason.Valid() {
		return errors.New("goal: invalid evaluation decision")
	}
	if e.Decision == DecisionContinue {
		if e.Reason != ReasonProgressPossible || !e.NextInstruction.Valid() {
			return errors.New("goal: continue requires a valid next instruction")
		}
		return nil
	}
	if e.Decision == DecisionDone && e.Reason != ReasonObjectiveMet {
		return errors.New("goal: done requires objective_met")
	}
	if e.Decision == DecisionBlocked && e.Reason != ReasonNeedsInput && e.Reason != ReasonExternalBlocker &&
		e.Reason != ReasonFollowOnLimit && e.Reason != ReasonEvaluatorFailure {
		return errors.New("goal: blocked requires a blocking reason")
	}
	if e.NextInstruction.Valid() {
		return errors.New("goal: terminal evaluation cannot carry next instruction")
	}
	return nil
}

type EvaluationRequest struct {
	Goal        Goal
	RunID       agent.RunID
	StepID      agent.StepID
	Revision    agent.Revision
	ModelConfig model.Config
	Messages    []agent.Message
}

type DecisionRecord struct {
	ID              string
	GoalID          string
	SessionID       agent.SessionID
	RunID           agent.RunID
	StepID          agent.StepID
	ExpectedVersion uint64
	Evaluation      Evaluation
	RecordedAt      time.Time
}

type Store interface {
	Current(context.Context, agent.SessionID) (Goal, bool, error)
	Set(context.Context, SetRequest) (Goal, error)
	ChangeStatus(context.Context, StateChangeRequest) (Goal, error)
	RecordDecision(context.Context, DecisionRecord) (Goal, error)
}

// Evaluator must make a semantic decision from the complete evidence supplied
// by Runtime. Model-backed implementations use the supplied AttemptRecorder so
// their additional provider calls remain budgeted, auditable, and billable.
type Evaluator interface {
	Evaluate(context.Context, EvaluationRequest, model.AttemptRecorder) (Evaluation, error)
}
