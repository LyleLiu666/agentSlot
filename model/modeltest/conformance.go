// Package modeltest provides reusable black-box checks for ModelExecutor and
// ModelStream implementations. It contains testing support, not a production
// Executor wrapper or Provider policy.
package modeltest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
)

// TestingT is the subset of testing.T used by Run.
type TestingT interface {
	Helper()
	Fatalf(string, ...any)
}

// Report is the complete portable evidence observed for one logical Execute.
type Report struct {
	Events   []model.ModelEvent
	Starts   []model.AttemptStart
	Finishes []model.AttemptFinish
}

// Run executes one black-box logical model call and checks the common
// capabilities, stream lifecycle, terminal closure, and one-start/one-finish
// Attempt invariants. Protocol-specific fixtures remain the caller's job.
func Run(t TestingT, ctx context.Context, executor model.ModelExecutor, request model.ModelRequest, budget model.TokenBudget) Report {
	t.Helper()
	if executor == nil {
		t.Fatalf("model conformance: Executor is nil")
	}
	capabilities, err := executor.Inspect(ctx, request.Config)
	if err != nil {
		t.Fatalf("model conformance: Inspect: %v", err)
	}
	if err := capabilities.Validate(); err != nil {
		t.Fatalf("model conformance: invalid capabilities: %v", err)
	}
	recorder := &attemptRecorder{budget: budget, started: make(map[agent.AttemptID]bool), finished: make(map[agent.AttemptID]bool)}
	stream, err := executor.Execute(ctx, request, recorder)
	if err != nil {
		t.Fatalf("model conformance: Execute: %v", err)
	}
	if stream == nil {
		t.Fatalf("model conformance: Execute returned a nil stream")
	}
	closed := false
	defer func() {
		if !closed {
			_ = stream.Close()
		}
	}()
	state := model.StreamState{}
	report := Report{}
	for !state.Terminal() {
		event, recvErr := stream.Recv(ctx)
		if recvErr != nil {
			t.Fatalf("model conformance: Recv before terminal: %v", state.End(recvErr))
		}
		if err := state.Accept(event); err != nil {
			t.Fatalf("model conformance: invalid stream event: %v", err)
		}
		report.Events = append(report.Events, event)
	}
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	if event, recvErr := stream.Recv(probeCtx); !errors.Is(recvErr, model.ErrStreamClosed) {
		t.Fatalf("model conformance: stream after terminal = event %#v, error %v; want ErrStreamClosed", event, recvErr)
	}
	if err := state.End(model.ErrStreamClosed); err != nil {
		t.Fatalf("model conformance: terminal closure: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("model conformance: Close: %v", err)
	}
	closed = true
	report.Starts, report.Finishes, err = recorder.report()
	if err != nil {
		t.Fatalf("model conformance: Attempt facts: %v", err)
	}
	return report
}

type attemptRecorder struct {
	mu            sync.Mutex
	budget        model.TokenBudget
	starts        []model.AttemptStart
	finishes      []model.AttemptFinish
	started       map[agent.AttemptID]bool
	finished      map[agent.AttemptID]bool
	contractError error
}

func (r *attemptRecorder) Started(_ context.Context, value model.AttemptStart) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := value.Validate(); err != nil {
		r.contractError = errors.Join(r.contractError, err)
		return err
	}
	if r.started[value.AttemptID] || r.finished[value.AttemptID] {
		err := fmt.Errorf("Attempt %q started more than once", value.AttemptID)
		r.contractError = errors.Join(r.contractError, err)
		return err
	}
	r.started[value.AttemptID] = true
	r.starts = append(r.starts, value)
	return nil
}

func (r *attemptRecorder) Finished(_ context.Context, value model.AttemptFinish) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := value.Validate(); err != nil {
		r.contractError = errors.Join(r.contractError, err)
		return err
	}
	if !r.started[value.AttemptID] || r.finished[value.AttemptID] {
		err := fmt.Errorf("Attempt %q finished without exactly one start", value.AttemptID)
		r.contractError = errors.Join(r.contractError, err)
		return err
	}
	r.finished[value.AttemptID] = true
	r.finishes = append(r.finishes, value)
	next := r.budget.UsedTokens + value.Usage.TotalTokens
	if next < r.budget.UsedTokens {
		err := errors.New("Attempt usage overflowed the shared token budget")
		r.contractError = errors.Join(r.contractError, err)
		return err
	}
	r.budget.UsedTokens = next
	return nil
}

func (r *attemptRecorder) Budget() model.TokenBudget {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.budget
}

func (r *attemptRecorder) report() ([]model.AttemptStart, []model.AttemptFinish, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for attemptID := range r.started {
		if !r.finished[attemptID] {
			r.contractError = errors.Join(r.contractError, fmt.Errorf("Attempt %q has no terminal fact", attemptID))
		}
	}
	return append([]model.AttemptStart(nil), r.starts...), append([]model.AttemptFinish(nil), r.finishes...), r.contractError
}

var _ model.AttemptRecorder = (*attemptRecorder)(nil)
