package model

import (
	"context"
	"errors"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
)

// AttemptObserverSlot is the ordered synchronous boundary around every
// physical provider dispatch. Unlike passive telemetry sinks, an observer may
// fail closed. Started completes before the Executor may send request bytes;
// Finished completes before it may retry or expose the logical result.
var AttemptObserverSlot = agentslot.Chain[AttemptObserver]("model.attempt.observer")

// AttemptIdentity is the stable, secret-free correlation shared by both
// notifications for one physical provider attempt.
type AttemptIdentity struct {
	SessionID      agent.SessionID
	RunID          agent.RunID
	StepID         agent.StepID
	AttemptID      agent.AttemptID
	ConfigRevision agent.Revision
	Config         Config
}

func (i AttemptIdentity) Validate() error {
	if !i.SessionID.Valid() || !i.RunID.Valid() || !i.StepID.Valid() || !i.AttemptID.Valid() ||
		i.ConfigRevision == 0 || i.Config.Validate() != nil {
		return errors.New("model: invalid attempt identity")
	}
	return nil
}

type AttemptStarted struct {
	Identity AttemptIdentity
}

func (a AttemptStarted) Validate() error { return a.Identity.Validate() }

type AttemptFinished struct {
	Identity          AttemptIdentity
	Outcome           AttemptOutcome
	ProviderRequestID string
	Usage             TokenUsage
	ErrorCode         string
}

func (a AttemptFinished) Validate() error {
	if err := a.Identity.Validate(); err != nil {
		return err
	}
	return (AttemptFinish{
		AttemptID: a.Identity.AttemptID, Outcome: a.Outcome,
		ProviderRequestID: a.ProviderRequestID, Usage: a.Usage, ErrorCode: a.ErrorCode,
	}).Validate()
}

type AttemptObserver interface {
	AttemptStarted(context.Context, AttemptStarted) error
	AttemptFinished(context.Context, AttemptFinished) error
}

type AttemptObserverFuncs struct {
	Started  func(context.Context, AttemptStarted) error
	Finished func(context.Context, AttemptFinished) error
}

func (f AttemptObserverFuncs) AttemptStarted(ctx context.Context, event AttemptStarted) error {
	if f.Started == nil {
		return errors.New("model: nil attempt-start observer")
	}
	return f.Started(ctx, event)
}

func (f AttemptObserverFuncs) AttemptFinished(ctx context.Context, event AttemptFinished) error {
	if f.Finished == nil {
		return errors.New("model: nil attempt-finish observer")
	}
	return f.Finished(ctx, event)
}
