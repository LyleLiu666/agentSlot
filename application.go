package agentslot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

var (
	// ErrInvalidApplication means an application has no stable, trimmed name.
	ErrInvalidApplication = errors.New("agentslot: invalid application")
)

// Application is the standard build and startup host for one named agent
// product. Build mounts the declared modules, validates their composition, and
// constructs their contributions. Start owns the resulting lifecycle through
// a Runtime.
//
// Every product uses the same Build, Start, and Run entry points. Product
// differences stay in the application name, module list, and profile
// requirements rather than in custom bootstrap control flow.
type Application struct {
	mu      sync.Mutex
	name    string
	modules []Module
	mounted int
	builder *Builder
	profile []Requirement
	plan    *Plan
}

// NewApplication declares one named agent application. The module list and
// requirements are copied so later changes to the caller's slices cannot
// change what Build assembles.
func NewApplication(name string, modules []Module, requirements ...Requirement) *Application {
	return &Application{
		name:    name,
		modules: append([]Module(nil), modules...),
		builder: NewBuilder(),
		profile: append([]Requirement(nil), requirements...),
	}
}

// Name returns the stable application name used in build and startup
// diagnostics.
func (a *Application) Name() string {
	if a == nil {
		return ""
	}
	return a.name
}

// Build automatically mounts every declared module, validates their slot
// requirements, and assembles their contributions. After the first successful
// build, repeated calls return the same immutable Plan.
func (a *Application) Build() (*Plan, error) {
	if a == nil {
		panic("agentslot: nil Application")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.plan != nil {
		return a.plan, nil
	}
	if a.name == "" || strings.TrimSpace(a.name) != a.name {
		return nil, fmt.Errorf(
			"%w: name %q must be non-empty without surrounding whitespace",
			ErrInvalidApplication,
			a.name,
		)
	}
	for a.mounted < len(a.modules) {
		module := a.modules[a.mounted]
		if err := a.builder.Install(module); err != nil {
			return nil, fmt.Errorf("build application %q: mount module #%d: %w", a.name, a.mounted+1, err)
		}
		a.mounted++
	}
	plan, err := a.builder.Build(a.profile...)
	if err != nil {
		return nil, fmt.Errorf("build application %q: %w", a.name, err)
	}
	a.plan = plan
	return plan, nil
}

// Start builds the application when needed, then starts every lifecycle-aware
// module in dependency order. Startup failure rolls back already-started
// modules through the same guarantees as Plan.Start.
func (a *Application) Start(ctx context.Context) (*Runtime, error) {
	plan, err := a.Build()
	if err != nil {
		return nil, err
	}
	runtime, err := plan.Start(ctx)
	if err != nil {
		return nil, fmt.Errorf("start application %q: %w", a.name, err)
	}
	return runtime, nil
}

// Run starts the application, waits for ctx cancellation, and then stops all
// started modules. Cancellation is treated as the normal request to stop;
// Run returns only startup or shutdown failures.
func (a *Application) Run(ctx context.Context) error {
	runtime, err := a.Start(ctx)
	if err != nil {
		return err
	}
	<-ctx.Done()
	if err := runtime.Stop(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("stop application %q: %w", a.name, err)
	}
	return nil
}
