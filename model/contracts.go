package model

import (
	"context"
	"errors"
	"fmt"
	"math"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/tool"
)

// ExecutorSlot is the standard logical model execution ecosystem.
var ExecutorSlot = agentslot.One[ModelExecutor]("model.executor")

// Reasoning is a provider-neutral reasoning mode. Provider adapters map it to
// their wire vocabulary without changing the Session contract.
type Reasoning string

const (
	ReasoningDefault Reasoning = "default"
	ReasoningLow     Reasoning = "low"
	ReasoningMedium  Reasoning = "medium"
	ReasoningHigh    Reasoning = "high"
)

// Valid reports whether reasoning belongs to the finite portable vocabulary.
func (r Reasoning) Valid() bool {
	switch r {
	case ReasoningDefault, ReasoningLow, ReasoningMedium, ReasoningHigh:
		return true
	default:
		return false
	}
}

// Parameters contains only portable model parameters. Provider-specific
// options belong in the Provider/Executor implementation.
type Parameters struct {
	Temperature *float64
	MaxTokens   *int
}

// Config is the provider-neutral configuration frozen for one logical Run.
type Config struct {
	ProviderKey string
	ModelID     string
	Reasoning   Reasoning
	Parameters  Parameters
}

// Validate checks only portable model invariants. Provider existence and
// capability compatibility belong to the selected ModelExecutor.
func (c Config) Validate() error {
	if c.ModelID == "" {
		return errors.New("model: model ID is required")
	}
	if !c.Reasoning.Valid() {
		return fmt.Errorf("model: invalid reasoning mode %q", c.Reasoning)
	}
	if c.Parameters.Temperature != nil && (math.IsNaN(*c.Parameters.Temperature) || math.IsInf(*c.Parameters.Temperature, 0) || *c.Parameters.Temperature < 0) {
		return errors.New("model: temperature must be finite and non-negative")
	}
	if c.Parameters.MaxTokens != nil && *c.Parameters.MaxTokens <= 0 {
		return errors.New("model: max tokens must be positive")
	}
	return nil
}

// ModelRequest is one logical model call. Executor implementations may perform
// multiple physical provider attempts behind this boundary.
type ModelRequest struct {
	SessionID agent.SessionID
	RunID     agent.RunID
	StepID    agent.StepID
	Config    Config
	Messages  []agent.Message
	Tools     []tool.Definition
}

// Executor performs one logical model call and owns provider retry or
// continuation differences.
type ModelExecutor interface {
	Execute(context.Context, ModelRequest) (ModelStream, error)
}

// ModelStream carries ordered temporary and terminal events. Implementations must
// return ErrStreamClosed after the terminal event or Close.
type ModelStream interface {
	Recv(context.Context) (ModelEvent, error)
	Close() error
}

// ModelEventKind is the finite stream vocabulary understood by the fixed Runtime.
type ModelEventKind string

const (
	EventDelta    ModelEventKind = "delta"
	EventReset    ModelEventKind = "reset"
	EventComplete ModelEventKind = "complete"
	EventFailed   ModelEventKind = "failed"
)

// ModelEvent is a provider-neutral stream event. Delta and reset are temporary;
// only a complete event can become a durable assistant Message.
type ModelEvent struct {
	Kind      ModelEventKind
	AttemptID string
	Text      string
	Message   *agent.Message
	Err       error
}

// Validate enforces the stream invariant that only complete events carry a
// durable assistant message and only failed events carry an error.
func (e ModelEvent) Validate() error {
	switch e.Kind {
	case EventDelta, EventReset:
		if e.Message != nil || e.Err != nil {
			return fmt.Errorf("model: %s event cannot carry a complete message or error", e.Kind)
		}
	case EventComplete:
		if e.Message == nil || !e.Message.Valid() || !e.Message.RunID.Valid() || !e.Message.StepID.Valid() || e.Err != nil {
			return errors.New("model: complete event requires a message and no error")
		}
	case EventFailed:
		if e.Err == nil || e.Message != nil {
			return errors.New("model: failed event requires an error and no message")
		}
	default:
		return fmt.Errorf("model: unknown event kind %q", e.Kind)
	}
	return nil
}

// ErrStreamClosed is returned after a stream reaches a terminal event.
var ErrStreamClosed = errors.New("model: stream is closed")
