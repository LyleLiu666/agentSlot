package agentslot

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var (
	// ErrAssemblyStarted means lifecycle startup was already attempted for an Assembly.
	ErrAssemblyStarted = errors.New("agentslot: assembly startup was already attempted")
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

// Runtime owns the successfully started module lifecycles for one Assembly.
type Runtime struct {
	mu       sync.Mutex
	assembly *Assembly
	active   []activeModule
	stopped  bool
	stopErr  error
}

// Start starts lifecycle-aware modules in the dependency order computed by
// Build. Each Assembly allows one startup attempt because modules may own
// non-repeatable resources.
func (a *Assembly) Start(ctx context.Context) (*Runtime, error) {
	if a == nil {
		panic("agentslot: nil Assembly")
	}
	a.startMu.Lock()
	if a.startAttempted {
		a.startMu.Unlock()
		return nil, ErrAssemblyStarted
	}
	a.startAttempted = true
	a.startMu.Unlock()

	active := make([]activeModule, 0, len(a.modules))
	for _, installed := range a.modules {
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
	return &Runtime{assembly: a, active: active}, nil
}

// Assembly returns the immutable Assembly owned by this runtime.
func (r *Runtime) Assembly() *Assembly {
	if r == nil {
		return nil
	}
	return r.assembly
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
