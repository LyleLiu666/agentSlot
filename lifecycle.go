package agentslot

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var (
	// ErrPlanStarted means lifecycle startup was already attempted for a Plan.
	ErrPlanStarted = errors.New("agentslot: plan startup was already attempted")
)

// Lifecycle is an optional Module capability. Successfully started modules are
// stopped in reverse order. A failed start rolls back earlier modules.
type Lifecycle interface {
	Start(context.Context) error
	Stop(context.Context) error
}

type activeModule struct {
	id        string
	lifecycle Lifecycle
}

// Runtime owns the successfully started module lifecycles for one Plan.
type Runtime struct {
	mu      sync.Mutex
	plan    *Plan
	active  []activeModule
	stopped bool
	stopErr error
}

// Start starts lifecycle-aware modules in the dependency order computed by
// Build. Each Plan allows one startup attempt because modules may own
// non-repeatable resources.
func (p *Plan) Start(ctx context.Context) (*Runtime, error) {
	if p == nil {
		panic("agentslot: nil Plan")
	}
	p.startMu.Lock()
	if p.startAttempted {
		p.startMu.Unlock()
		return nil, ErrPlanStarted
	}
	p.startAttempted = true
	p.startMu.Unlock()

	active := make([]activeModule, 0, len(p.modules))
	for _, installed := range p.modules {
		lifecycle, ok := installed.module.(Lifecycle)
		if !ok {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, rollbackStart(context.WithoutCancel(ctx), active, installed.id, err)
		}
		if err := lifecycle.Start(ctx); err != nil {
			return nil, rollbackStart(context.WithoutCancel(ctx), active, installed.id, err)
		}
		active = append(active, activeModule{id: installed.id, lifecycle: lifecycle})
	}
	return &Runtime{plan: p, active: active}, nil
}

// Plan returns the immutable assembled plan owned by this runtime.
func (r *Runtime) Plan() *Plan {
	if r == nil {
		return nil
	}
	return r.plan
}

func rollbackStart(ctx context.Context, active []activeModule, failedID string, startErr error) error {
	errs := []error{fmt.Errorf("start module %q: %w", failedID, startErr)}
	for i := len(active) - 1; i >= 0; i-- {
		if err := active[i].lifecycle.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("roll back module %q: %w", active[i].id, err))
		}
	}
	return errors.Join(errs...)
}

// Stop stops every started module in reverse order. Repeated calls do not run
// lifecycle code again and return the first call's result.
func (r *Runtime) Stop(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return r.stopErr
	}
	r.stopped = true

	var errs []error
	for i := len(r.active) - 1; i >= 0; i-- {
		if err := r.active[i].lifecycle.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop module %q: %w", r.active[i].id, err))
		}
	}
	r.stopErr = errors.Join(errs...)
	return r.stopErr
}
