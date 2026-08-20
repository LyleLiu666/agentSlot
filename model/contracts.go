package model

import (
	"context"
	"encoding/json"
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
	// ConfigRevision identifies the durable Session revision from which Config
	// was frozen when the Run started.
	ConfigRevision agent.Revision
	Inputs         []Input
	Tools          []tool.Definition
}

// Input is one ordered model-facing projection item. It preserves canonical
// message/call/result semantics without exposing a Provider wire format.
type Input struct {
	Message    *agent.Message
	ToolCall   *agent.ToolCall
	ToolResult *tool.ToolResult
}

func (i Input) Valid() bool {
	count := 0
	if i.Message != nil {
		count++
	}
	if i.ToolCall != nil {
		count++
	}
	if i.ToolResult != nil {
		count++
	}
	if count != 1 {
		return false
	}
	if i.Message != nil {
		return i.Message.Valid()
	}
	if i.ToolCall != nil {
		return i.ToolCall.Valid()
	}
	return i.ToolResult.Validate() == nil
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
	Output    *Completion
	Err       error
}

// Completion is one identity-free logical model result. The fixed Runtime,
// not an Executor or Provider adapter, allocates durable Message containment.
// Tool-call requests extend this value in the ToolDispatcher round.
type Completion struct {
	Parts     []agent.MessagePart
	ToolCalls []ToolCallRequest
}

// Valid reports whether the completed content can become a durable message.
func (c Completion) Valid() bool {
	if len(c.Parts) == 0 && len(c.ToolCalls) == 0 {
		return false
	}
	for _, part := range c.Parts {
		if !part.Valid() {
			return false
		}
	}
	for _, call := range c.ToolCalls {
		if !call.Valid() {
			return false
		}
	}
	return true
}

// ToolCallRequest is an identity-free tool invocation requested by a model.
// Runtime allocates the durable ToolCallID and containment on commit.
type ToolCallRequest struct {
	CorrelationID string
	Name          string
	Arguments     json.RawMessage
}

func (c ToolCallRequest) Valid() bool {
	return c.Name != "" && json.Valid(c.Arguments)
}

// Validate enforces the stream invariant that only complete events carry
// identity-free completed content and only failed events carry an error.
func (e ModelEvent) Validate() error {
	switch e.Kind {
	case EventDelta:
		if e.Text == "" || e.Output != nil || e.Err != nil {
			return errors.New("model: delta event requires text and no terminal payload")
		}
	case EventReset:
		if e.Text != "" || e.Output != nil || e.Err != nil {
			return errors.New("model: reset event cannot carry content or error")
		}
	case EventComplete:
		if e.Text != "" || e.Output == nil || !e.Output.Valid() || e.Err != nil {
			return errors.New("model: complete event requires output and no error")
		}
	case EventFailed:
		if e.Text != "" || e.Err == nil || e.Output != nil {
			return errors.New("model: failed event requires an error and no message")
		}
	default:
		return fmt.Errorf("model: unknown event kind %q", e.Kind)
	}
	return nil
}

// ErrStreamClosed is returned after a stream reaches a terminal event.
var ErrStreamClosed = errors.New("model: stream is closed")
