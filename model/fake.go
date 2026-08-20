package model

import (
	"context"
	"encoding/json"
	"sync"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/tool"
)

// FakeExecution scripts one logical call for FakeModelExecutor. Block, when
// non-nil, delays stream delivery until it is closed or the call is canceled.
type FakeExecution struct {
	Events       []ModelEvent
	ExecuteError error
	Block        <-chan struct{}
}

// FakeModelExecutor is a deterministic development implementation of
// ModelExecutor. It is explicitly installed by tests and examples; the
// standard Agent application never selects it implicitly.
type FakeModelExecutor struct {
	mu         sync.Mutex
	executions []FakeExecution
	requests   []ModelRequest
	changed    chan struct{}
}

var _ ModelExecutor = (*FakeModelExecutor)(nil)

// NewFakeModelExecutor returns an executor that consumes scripts in FIFO
// order. A call made after the scripts are exhausted fails explicitly.
func NewFakeModelExecutor(executions ...FakeExecution) *FakeModelExecutor {
	copy := make([]FakeExecution, len(executions))
	for index, execution := range executions {
		copy[index] = cloneFakeExecution(execution)
	}
	return &FakeModelExecutor{executions: copy, changed: make(chan struct{})}
}

func (f *FakeModelExecutor) Execute(ctx context.Context, request ModelRequest) (ModelStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
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
	return &fakeModelStream{events: execution.Events, block: execution.Block}, nil
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
	mu     sync.Mutex
	events []ModelEvent
	block  <-chan struct{}
	closed bool
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
	if event.Kind == EventComplete || event.Kind == EventFailed {
		s.closed = true
	}
	return event, nil
}

func (s *fakeModelStream) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

func cloneFakeExecution(source FakeExecution) FakeExecution {
	copy := source
	copy.Events = make([]ModelEvent, len(source.Events))
	for index, event := range source.Events {
		copy.Events[index] = cloneModelEvent(event)
	}
	return copy
}

func cloneModelEvent(source ModelEvent) ModelEvent {
	copy := source
	if source.Output != nil {
		output := Completion{Parts: append([]agent.MessagePart(nil), source.Output.Parts...)}
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
	if source.Message != nil {
		message := *source.Message
		message.Parts = append([]agent.MessagePart(nil), source.Message.Parts...)
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
		if source.ToolResult.Error != nil {
			errorCopy := *source.ToolResult.Error
			result.Error = &errorCopy
		}
		copy.ToolResult = &result
	}
	return copy
}
