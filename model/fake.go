package model

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/tool"
)

// FakeExecution scripts one logical call for FakeModelExecutor. Block, when
// non-nil, delays stream delivery until it is closed or the call is canceled.
type FakeExecution struct {
	Events       []ModelEvent
	ExecuteError error
	Block        <-chan struct{}
	// ErrorMessage is the optional displayable diagnostic recorded for scripted
	// failed physical attempts. Tests must supply a contract-valid safe value.
	ErrorMessage string
	// Usage applies to a single-attempt script. AttemptUsage is used when a
	// script contains reset-delimited physical attempts.
	Usage        TokenUsage
	AttemptUsage map[agent.AttemptID]TokenUsage
}

// FakeModelExecutor is a deterministic development implementation of
// ModelExecutor. It is explicitly installed by tests and examples; the
// standard Agent application never selects it implicitly.
type FakeModelExecutor struct {
	mu         sync.Mutex
	executions []FakeExecution
	requests   []ModelRequest
	changed    chan struct{}
	sequence   atomic.Uint64
}

var _ ModelExecutor = (*FakeModelExecutor)(nil)

// FakeTokenCounter is a deterministic development counter for logical fake
// requests. It is not a provider tokenizer and must not be used as a fallback
// for a real provider adapter.
type FakeTokenCounter struct{}

var _ TokenCounter = FakeTokenCounter{}

// NewFakeTokenCounter returns the counter paired with FakeModelExecutor in
// tests and examples. Applications must still install it explicitly.
func NewFakeTokenCounter() TokenCounter { return FakeTokenCounter{} }

// NewFakeModelExecutor returns an executor that consumes scripts in FIFO
// order. A call made after the scripts are exhausted fails explicitly.
func NewFakeModelExecutor(executions ...FakeExecution) *FakeModelExecutor {
	copy := make([]FakeExecution, len(executions))
	for index, execution := range executions {
		copy[index] = cloneFakeExecution(execution)
	}
	return &FakeModelExecutor{executions: copy, changed: make(chan struct{})}
}

func (f *FakeModelExecutor) Execute(ctx context.Context, request ModelRequest, recorder AttemptRecorder) (ModelStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if recorder == nil {
		return nil, agent.NewError(agent.ErrorInvalidInput, "model.fake.execute", "AttemptRecorder is required", nil)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, cloneModelRequest(request))
	close(f.changed)
	f.changed = make(chan struct{})
	if len(f.executions) == 0 {
		return nil, agent.NewError(agent.ErrorUnavailable, "model.fake.execute", "no scripted execution remains", nil)
	}
	execution := f.executions[0]
	f.executions = f.executions[1:]
	if execution.ExecuteError != nil {
		return nil, execution.ExecuteError
	}
	events, attempts, err := f.normalizeAttempts(execution.Events)
	if err != nil {
		return nil, err
	}
	stream := &fakeModelStream{
		events: events, block: execution.Block, recorder: recorder,
		request: cloneModelRequest(request), usage: cloneAttemptUsage(execution.AttemptUsage),
		defaultUsage: execution.Usage, attempts: attempts, errorMessage: execution.ErrorMessage,
	}
	if len(attempts) > 0 {
		if err := stream.start(ctx, attempts[0]); err != nil {
			return nil, err
		}
	}
	return stream, nil
}

func (f *FakeModelExecutor) normalizeAttempts(source []ModelEvent) ([]ModelEvent, []agent.AttemptID, error) {
	events := make([]ModelEvent, len(source))
	attempts := make([]agent.AttemptID, 0)
	var current agent.AttemptID
	for index, event := range source {
		events[index] = cloneModelEvent(event)
		candidate := agent.AttemptID(event.AttemptID)
		if current == "" {
			if candidate == "" {
				candidate = agent.AttemptID(fmt.Sprintf("fake-attempt-%d", f.sequence.Add(1)))
			}
			if !candidate.Valid() {
				return nil, nil, agent.NewError(agent.ErrorInvalidInput, "model.fake.execute", "script contains an invalid AttemptID", nil)
			}
			current = candidate
			attempts = append(attempts, current)
		} else if candidate != "" && candidate != current {
			return nil, nil, agent.NewError(agent.ErrorInvalidInput, "model.fake.execute", "script changes AttemptID without reset", nil)
		}
		events[index].AttemptID = string(current)
		if event.Kind == EventReset {
			current = ""
		}
	}
	return events, attempts, nil
}

func (*FakeModelExecutor) Inspect(_ context.Context, config Config) (ExecutionCapabilities, error) {
	if err := config.Validate(); err != nil {
		return ExecutionCapabilities{}, err
	}
	return ExecutionCapabilities{
		Media: Capabilities{
			InputModalities:  []Modality{ModalityText, ModalityImage, ModalityAudio},
			OutputModalities: []Modality{ModalityText},
			ToolCalling:      true,
		},
		Reasoning:           []Reasoning{ReasoningDefault, ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMax},
		ContextWindowTokens: 1_000_000, MaxOutputTokens: 100_000,
	}, nil
}

func (FakeTokenCounter) CountTokens(_ context.Context, request ModelRequest) (int, error) {
	return fakeTokenEstimate(request), nil
}

// CountTokens preserves the concrete FakeModelExecutor API used before token
// counting became an independently replaceable Slot. Runtime composition must
// resolve TokenCounterSlot instead of relying on this convenience method.
//
// Deprecated: install NewFakeTokenCounter through TokenCounterSlot.
func (*FakeModelExecutor) CountTokens(ctx context.Context, request ModelRequest) (int, error) {
	return FakeTokenCounter{}.CountTokens(ctx, request)
}

func fakeTokenEstimate(request ModelRequest) int {
	count := 0
	for _, input := range request.Inputs {
		switch {
		case input.SystemPrompt != nil:
			count += portableTokenEstimate(*input.SystemPrompt)
		case input.Message != nil:
			for _, part := range input.Message.Parts {
				count += portableTokenEstimate(part.Text)
				if part.AttachmentID != "" {
					count += 8
				}
			}
		case input.ToolCall != nil:
			count += portableTokenEstimate(input.ToolCall.Name) + portableTokenEstimate(string(input.ToolCall.Arguments))
		case input.ToolResult != nil:
			count += portableTokenEstimate(string(input.ToolResult.Output))
		}
	}
	for _, definition := range request.Tools {
		count += portableTokenEstimate(definition.Name) + portableTokenEstimate(definition.Description) + portableTokenEstimate(string(definition.InputSchema.JSON()))
	}
	return count
}

func portableTokenEstimate(value string) int {
	if value == "" {
		return 0
	}
	return (len([]byte(value)) + 3) / 4
}

// WaitForRequests blocks until at least count logical calls were captured.
func (f *FakeModelExecutor) WaitForRequests(ctx context.Context, count int) error {
	for {
		f.mu.Lock()
		if len(f.requests) >= count {
			f.mu.Unlock()
			return nil
		}
		changed := f.changed
		f.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Requests returns detached logical requests in call order.
func (f *FakeModelExecutor) Requests() []ModelRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	requests := make([]ModelRequest, len(f.requests))
	for index, request := range f.requests {
		requests[index] = cloneModelRequest(request)
	}
	return requests
}

type fakeModelStream struct {
	mu           sync.Mutex
	events       []ModelEvent
	block        <-chan struct{}
	closed       bool
	recorder     AttemptRecorder
	request      ModelRequest
	usage        map[agent.AttemptID]TokenUsage
	defaultUsage TokenUsage
	attempts     []agent.AttemptID
	started      map[agent.AttemptID]bool
	finished     map[agent.AttemptID]bool
	outputBytes  map[agent.AttemptID]int
	errorMessage string
}

func (s *fakeModelStream) Recv(ctx context.Context) (ModelEvent, error) {
	s.mu.Lock()
	if s.closed || len(s.events) == 0 {
		s.mu.Unlock()
		return ModelEvent{}, ErrStreamClosed
	}
	block := s.block
	s.block = nil
	s.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			if err := s.finishCanceled(); err != nil {
				return ModelEvent{}, err
			}
			return ModelEvent{}, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(s.events) == 0 {
		return ModelEvent{}, ErrStreamClosed
	}
	event := cloneModelEvent(s.events[0])
	s.events = s.events[1:]
	attemptID := agent.AttemptID(event.AttemptID)
	if !s.started[attemptID] {
		if err := s.start(ctx, attemptID); err != nil {
			return ModelEvent{}, err
		}
	}
	if event.Kind == EventDelta {
		s.outputBytes[attemptID] += len([]byte(event.Text))
	}
	if event.Kind == EventComplete && event.Output != nil && s.outputBytes[attemptID] == 0 {
		for _, part := range event.Output.Parts {
			s.outputBytes[attemptID] += len([]byte(part.Text))
		}
		for _, call := range event.Output.ToolCalls {
			s.outputBytes[attemptID] += len(call.Name) + len(call.Arguments)
		}
	}
	if event.Kind == EventReset || event.Kind == EventComplete || event.Kind == EventFailed {
		outcome := AttemptFailed
		errorCode := "scripted_failure"
		errorMessage := s.errorMessage
		if event.Kind == EventComplete {
			outcome, errorCode, errorMessage = AttemptSucceeded, "", ""
		}
		if err := s.finish(context.WithoutCancel(ctx), attemptID, outcome, errorCode, errorMessage); err != nil {
			return ModelEvent{}, err
		}
	}
	if event.Kind == EventComplete || event.Kind == EventFailed {
		s.closed = true
	}
	return event, nil
}

func (s *fakeModelStream) Close() error {
	err := s.finishCanceled()
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return err
}

func (s *fakeModelStream) start(ctx context.Context, id agent.AttemptID) error {
	if s.started == nil {
		s.started = make(map[agent.AttemptID]bool)
		s.finished = make(map[agent.AttemptID]bool)
		s.outputBytes = make(map[agent.AttemptID]int)
	}
	if s.started[id] {
		return nil
	}
	if len(s.started) > 0 && s.recorder.Budget().Exhausted() {
		return ErrTokenBudgetExceeded
	}
	if err := s.recorder.Started(ctx, AttemptStart{AttemptID: id, ProviderKey: s.request.Config.ProviderKey, ModelID: s.request.Config.ModelID}); err != nil {
		return err
	}
	s.started[id] = true
	return nil
}

func (s *fakeModelStream) finish(ctx context.Context, id agent.AttemptID, outcome AttemptOutcome, errorCode, errorMessage string) error {
	if s.finished[id] {
		return nil
	}
	usage, ok := s.usage[id]
	if !ok {
		usage = s.defaultUsage
	}
	if usage == (TokenUsage{}) {
		input := fakeTokenEstimate(s.request)
		output := int64((s.outputBytes[id] + 3) / 4)
		usage = TokenUsage{InputTokens: int64(input), OutputTokens: output, TotalTokens: int64(input) + output, Estimated: true, EstimateSource: "model.fake.portable_estimate"}
	}
	finish := AttemptFinish{AttemptID: id, Outcome: outcome, Usage: usage, ErrorCode: errorCode, ErrorMessage: errorMessage}
	if err := finish.Validate(); err != nil {
		return err
	}
	if err := s.recorder.Finished(ctx, finish); err != nil {
		return err
	}
	s.finished[id] = true
	return nil
}

func (s *fakeModelStream) finishCanceled() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.attempts {
		if s.started[id] && !s.finished[id] {
			return s.finish(context.Background(), id, AttemptCanceled, "canceled", "")
		}
	}
	return nil
}

func cloneFakeExecution(source FakeExecution) FakeExecution {
	copy := source
	copy.Events = make([]ModelEvent, len(source.Events))
	for index, event := range source.Events {
		copy.Events[index] = cloneModelEvent(event)
	}
	copy.AttemptUsage = cloneAttemptUsage(source.AttemptUsage)
	return copy
}

func cloneAttemptUsage(source map[agent.AttemptID]TokenUsage) map[agent.AttemptID]TokenUsage {
	if source == nil {
		return nil
	}
	copy := make(map[agent.AttemptID]TokenUsage, len(source))
	for id, usage := range source {
		copy[id] = usage
	}
	return copy
}

func cloneModelEvent(source ModelEvent) ModelEvent {
	copy := source
	if source.Output != nil {
		output := Completion{
			Parts:        append([]agent.MessagePart(nil), source.Output.Parts...),
			Continuation: append(json.RawMessage(nil), source.Output.Continuation...),
		}
		output.ToolCalls = make([]ToolCallRequest, len(source.Output.ToolCalls))
		for index, call := range source.Output.ToolCalls {
			output.ToolCalls[index] = call
			output.ToolCalls[index].Arguments = append(json.RawMessage(nil), call.Arguments...)
		}
		copy.Output = &output
	}
	return copy
}

func cloneModelRequest(source ModelRequest) ModelRequest {
	copy := source
	copy.Inputs = make([]Input, len(source.Inputs))
	for index, input := range source.Inputs {
		copy.Inputs[index] = cloneInput(input)
	}
	copy.Tools = append([]tool.Definition(nil), source.Tools...)
	return copy
}

func cloneInput(source Input) Input {
	copy := source
	if source.SystemPrompt != nil {
		prompt := *source.SystemPrompt
		copy.SystemPrompt = &prompt
	}
	if source.Message != nil {
		message := *source.Message
		message.Parts = append([]agent.MessagePart(nil), source.Message.Parts...)
		if source.Message.ModelContinuation != nil {
			continuation := *source.Message.ModelContinuation
			continuation.State = append(json.RawMessage(nil), source.Message.ModelContinuation.State...)
			message.ModelContinuation = &continuation
		}
		copy.Message = &message
	}
	if source.ToolCall != nil {
		call := *source.ToolCall
		call.Arguments = append([]byte(nil), source.ToolCall.Arguments...)
		copy.ToolCall = &call
	}
	if source.ToolResult != nil {
		result := *source.ToolResult
		result.Output = append([]byte(nil), source.ToolResult.Output...)
		result.Artifacts = append(source.ToolResult.Artifacts[:0:0], source.ToolResult.Artifacts...)
		if source.ToolResult.Error != nil {
			errorCopy := *source.ToolResult.Error
			result.Error = &errorCopy
		}
		copy.ToolResult = &result
	}
	return copy
}
