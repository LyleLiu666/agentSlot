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
	ReasoningXHigh   Reasoning = "xhigh"
	ReasoningMax     Reasoning = "max"
)

// Valid reports whether reasoning belongs to the finite portable vocabulary.
func (r Reasoning) Valid() bool {
	switch r {
	case ReasoningDefault, ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMax:
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

// TokenUsage is the provider-neutral accounting result for one physical
// attempt. Cached input and reasoning tokens are subsets and must not be added
// to TotalTokens a second time.
type TokenUsage struct {
	InputTokens       int64
	OutputTokens      int64
	CachedInputTokens int64
	CacheWriteTokens  int64
	ReasoningTokens   int64
	TotalTokens       int64
	Estimated         bool
	EstimateSource    string
}

// Validate enforces portable accounting relationships. Provider-specific
// billing interpretation remains in the adapter.
func (u TokenUsage) Validate() error {
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.CachedInputTokens < 0 ||
		u.CacheWriteTokens < 0 || u.ReasoningTokens < 0 || u.TotalTokens < 0 {
		return errors.New("model: token usage cannot be negative")
	}
	if u.CachedInputTokens > u.InputTokens {
		return errors.New("model: cached input tokens exceed input tokens")
	}
	if u.ReasoningTokens > u.OutputTokens {
		return errors.New("model: reasoning tokens exceed output tokens")
	}
	if u.TotalTokens < u.InputTokens+u.OutputTokens {
		return errors.New("model: total tokens are smaller than input plus output")
	}
	if u.Estimated != (u.EstimateSource != "") {
		return errors.New("model: estimated token usage requires exactly one estimate source")
	}
	return nil
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
	SystemPrompt *string
	Message      *agent.Message
	ToolCall     *agent.ToolCall
	ToolResult   *tool.ToolResult
}

func (i Input) Valid() bool {
	count := 0
	if i.SystemPrompt != nil {
		count++
	}
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
	if i.SystemPrompt != nil {
		return *i.SystemPrompt != ""
	}
	if i.Message != nil {
		return i.Message.Valid()
	}
	if i.ToolCall != nil {
		return i.ToolCall.Valid()
	}
	return i.ToolResult.Validate() == nil
}

// ValidateInputs enforces the provider-neutral protocol projection. A fixed
// SystemPrompt may appear once at the beginning. Tool exchanges are contiguous
// groups: one assistant parent, all calls, then exactly one result per call.
func ValidateInputs(inputs []Input) error {
	messages := make(map[agent.MessageID]bool)
	allCalls := make(map[agent.ToolCallID]bool)
	start := 0
	if len(inputs) > 0 && inputs[0].SystemPrompt != nil {
		if !inputs[0].Valid() {
			return errors.New("model: invalid system prompt")
		}
		start = 1
	}
	for index := start; index < len(inputs); {
		input := inputs[index]
		if !input.Valid() {
			return errors.New("model: invalid input item")
		}
		if input.SystemPrompt != nil {
			return errors.New("model: system prompt must be the first and only fixed input")
		}
		if input.Message == nil {
			return errors.New("model: tool exchange must begin with an assistant message")
		}
		message := input.Message
		if messages[message.ID] {
			return errors.New("model: duplicate message input")
		}
		messages[message.ID] = true
		index++
		if message.Role != agent.RoleAssistant || index >= len(inputs) || inputs[index].ToolCall == nil {
			continue
		}
		calls := make(map[agent.ToolCallID]bool)
		for index < len(inputs) && inputs[index].ToolCall != nil {
			call := inputs[index].ToolCall
			if !inputs[index].Valid() || call.MessageID != message.ID || call.RunID != message.RunID || call.StepID != message.StepID || calls[call.ID] || allCalls[call.ID] {
				return errors.New("model: tool call has invalid assistant containment")
			}
			calls[call.ID] = true
			allCalls[call.ID] = true
			index++
		}
		results := make(map[agent.ToolCallID]bool, len(calls))
		for index < len(inputs) && inputs[index].ToolResult != nil {
			result := inputs[index].ToolResult
			if !inputs[index].Valid() || !calls[result.CallID] || results[result.CallID] {
				return errors.New("model: tool result is unpaired or duplicated")
			}
			results[result.CallID] = true
			index++
		}
		if len(results) != len(calls) {
			return errors.New("model: tool call has no terminal result")
		}
	}
	return nil
}

// Executor performs one logical model call and owns provider retry or
// continuation differences.
type ModelExecutor interface {
	Execute(context.Context, ModelRequest, AttemptRecorder) (ModelStream, error)
	// Inspect validates a selection and returns authoritative capabilities.
	Inspect(context.Context, Config) (ExecutionCapabilities, error)
	// CountTokens evaluates the complete fixed request without mutating it.
	CountTokens(context.Context, ModelRequest) (int, error)
}

// AttemptOutcome is the terminal result of one physical provider request.
// A logical Execute call can contain multiple Attempts, but each Attempt has
// exactly one Started and one Finished record.
type AttemptOutcome string

const (
	AttemptSucceeded AttemptOutcome = "succeeded"
	AttemptFailed    AttemptOutcome = "failed"
	AttemptCanceled  AttemptOutcome = "canceled"
)

func (o AttemptOutcome) Valid() bool {
	return o == AttemptSucceeded || o == AttemptFailed || o == AttemptCanceled
}

// AttemptStart is supplied before any bytes of the physical provider request
// are sent. AttemptID is local, stable identity; ProviderRequestID is recorded
// later if the remote endpoint returns one.
type AttemptStart struct {
	AttemptID   agent.AttemptID
	ProviderKey string
	ModelID     string
}

func (a AttemptStart) Validate() error {
	if !a.AttemptID.Valid() || a.ModelID == "" {
		return errors.New("model: invalid attempt start")
	}
	return nil
}

// AttemptFinish is supplied before an Executor retries or exposes a logical
// terminal result. ErrorCode is a stable, non-sensitive adapter category.
type AttemptFinish struct {
	AttemptID         agent.AttemptID
	Outcome           AttemptOutcome
	ProviderRequestID string
	Usage             TokenUsage
	ErrorCode         string
}

func (a AttemptFinish) Validate() error {
	if !a.AttemptID.Valid() || !a.Outcome.Valid() {
		return errors.New("model: invalid attempt finish")
	}
	if err := a.Usage.Validate(); err != nil {
		return err
	}
	if a.Outcome == AttemptSucceeded && a.ErrorCode != "" {
		return errors.New("model: succeeded attempt cannot contain an error code")
	}
	if a.Outcome != AttemptSucceeded && a.ErrorCode == "" {
		return errors.New("model: unsuccessful attempt requires a safe error code")
	}
	return nil
}

// TokenBudget is the current logical Run accounting snapshot. MaxTokens zero
// means unlimited. Cached-input and reasoning subsets are already included in
// UsedTokens through TokenUsage.TotalTokens.
type TokenBudget struct {
	MaxTokens  int64
	UsedTokens int64
}

func (b TokenBudget) Validate() error {
	if b.MaxTokens < 0 || b.UsedTokens < 0 {
		return errors.New("model: token budget cannot be negative")
	}
	return nil
}

func (b TokenBudget) Exhausted() bool { return b.MaxTokens > 0 && b.UsedTokens >= b.MaxTokens }

func (b TokenBudget) RemainingTokens() int64 {
	if b.MaxTokens == 0 {
		return 0
	}
	if b.UsedTokens >= b.MaxTokens {
		return 0
	}
	return b.MaxTokens - b.UsedTokens
}

// AttemptRecorder is the only durable capability given to ModelExecutor. It
// cannot read or mutate Session state. Started must return only after the fact
// is committed; Finished must return only after the terminal fact is committed.
type AttemptRecorder interface {
	Started(context.Context, AttemptStart) error
	Finished(context.Context, AttemptFinish) error
	Budget() TokenBudget
}

// ExecutionCapabilities is the authoritative Executor view used by Runtime before a
// provider call or model switch.
type ExecutionCapabilities struct {
	Media               Capabilities
	Reasoning           []Reasoning
	ContextWindowTokens int
	MaxOutputTokens     int
}

// CompatibilityWarning describes durable Session content that a target model
// cannot consume directly. The content remains in History; callers may confirm
// the switch and let Context project stable attachment references instead.
type CompatibilityWarning struct {
	Modality Modality
	Count    int
}

// CompatibilityError carries machine-readable warnings for a model switch
// that requires explicit caller confirmation.
type CompatibilityError struct {
	Warnings []CompatibilityWarning
}

func (e *CompatibilityError) Error() string {
	return "model: switching requires explicit compatibility-loss confirmation"
}

func (c ExecutionCapabilities) Validate() error {
	if c.ContextWindowTokens <= 0 || c.MaxOutputTokens < 0 || c.MaxOutputTokens > c.ContextWindowTokens {
		return errors.New("model: invalid token capabilities")
	}
	if err := c.Media.Validate(); err != nil {
		return err
	}
	if len(c.Reasoning) == 0 {
		return errors.New("model: reasoning capabilities are required")
	}
	seenReasoning := make(map[Reasoning]bool)
	for _, reasoning := range c.Reasoning {
		if !reasoning.Valid() || seenReasoning[reasoning] {
			return errors.New("model: invalid or duplicate reasoning capability")
		}
		seenReasoning[reasoning] = true
	}
	return nil
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
	Parts        []agent.MessagePart
	ToolCalls    []ToolCallRequest
	Continuation json.RawMessage
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
	if len(c.Continuation) > 0 && !json.Valid(c.Continuation) {
		return false
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

// ErrTokenBudgetExceeded tells Runtime that an Executor declined another
// physical retry because the current Run budget is exhausted.
var ErrTokenBudgetExceeded = errors.New("model: run token budget exhausted")
