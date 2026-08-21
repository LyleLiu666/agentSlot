package agentslot

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
)

var (
	// ErrBuilderFrozen means a successful build has made further mutation invalid.
	ErrBuilderFrozen = errors.New("agentslot: builder is frozen")
	// ErrInvalidModule means a module is nil or has an invalid ID.
	ErrInvalidModule = errors.New("agentslot: invalid module")
	// ErrDuplicateModule means two installed modules use the same ID.
	ErrDuplicateModule = errors.New("agentslot: duplicate module")
	// ErrRegistrationClosed means a module retained and reused its Registrar.
	ErrRegistrationClosed = errors.New("agentslot: registration is closed")
	// ErrInvalidContribution means a contribution is nil or carries a nil value.
	ErrInvalidContribution = errors.New("agentslot: invalid contribution")
	// ErrInvalidKey means a ManySlot contribution has an invalid key.
	ErrInvalidKey = errors.New("agentslot: invalid contribution key")
	// ErrSlotConflict means one slot ID was declared with different kinds or value types.
	ErrSlotConflict = errors.New("agentslot: conflicting slot definition")
	// ErrSlotOccupied means a OneSlot already has a contribution.
	ErrSlotOccupied = errors.New("agentslot: one slot is already occupied")
	// ErrDuplicateKey means a ManySlot already contains the contribution key.
	ErrDuplicateKey = errors.New("agentslot: duplicate contribution key")
	// ErrInvalidRequirement means a slot requirement has invalid cardinality or key data.
	ErrInvalidRequirement = errors.New("agentslot: invalid requirement")
	// ErrRequirementUnsatisfied means a build profile's minimum cardinality was not met.
	ErrRequirementUnsatisfied = errors.New("agentslot: requirement is unsatisfied")
	// ErrDependencyCycle means slot requirements create a module lifecycle cycle.
	ErrDependencyCycle = errors.New("agentslot: module dependency cycle")
	// ErrDependencyUndeclared means a constructor tried to resolve a slot that
	// its owning module did not declare through RequiredSlots.
	ErrDependencyUndeclared = errors.New("agentslot: dependency is not declared")
	// ErrResolverClosed means a build-scoped Resolver was retained and used
	// outside the constructor call that received it.
	ErrResolverClosed = errors.New("agentslot: resolver is closed")
	// ErrDependencyNotReady means a constructor tried to resolve a component
	// whose own constructor has not completed.
	ErrDependencyNotReady = errors.New("agentslot: dependency is not ready")
)

// Module is a registration and lifecycle ownership envelope. Its Register
// method contributes implementations to typed slots; Module itself does not
// identify the component ecosystem.
type Module interface {
	ID() string
	Register(Registrar) error
}

// SlotRequirer is an optional Module capability. Declared slot dependencies
// are validated at build time, and installed provider modules start before the
// requiring module.
type SlotRequirer interface {
	RequiredSlots() []Requirement
}

// Registrar accepts contributions during one Module.Register call.
// It becomes invalid when that call returns.
type Registrar interface {
	Contribute(...Contribution) error
}

type registeredValue struct {
	owner       string
	key         string
	value       any
	constructor constructorFunc
	ordinal     uint64
	defaulted   bool
}

type slotRecord struct {
	spec   slotSpec
	values []registeredValue
}

type registryState struct {
	byID        map[string]*slotRecord
	nextOrdinal uint64
}

func newRegistryState() registryState {
	return registryState{byID: make(map[string]*slotRecord)}
}

func (s registryState) clone() registryState {
	cloned := newRegistryState()
	cloned.nextOrdinal = s.nextOrdinal
	for id, record := range s.byID {
		values := make([]registeredValue, len(record.values))
		copy(values, record.values)
		cloned.byID[id] = &slotRecord{spec: record.spec, values: values}
	}
	return cloned
}

type installedModule struct {
	id           string
	module       Module
	requirements []Requirement
	contributes  bool
}

// Builder transactionally installs modules and freezes them into an Assembly.
type Builder struct {
	mu      sync.Mutex
	state   registryState
	modules []installedModule
	frozen  bool
}

// NewBuilder returns an empty mutable composition builder.
func NewBuilder() *Builder {
	return &Builder{state: newRegistryState()}
}

// Install registers all contributions from module as one transaction.
func (b *Builder) Install(module Module) error {
	if isNil(module) {
		return fmt.Errorf("%w: nil", ErrInvalidModule)
	}
	id := module.ID()
	if id == "" || strings.TrimSpace(id) != id {
		return fmt.Errorf("%w: ID %q must be non-empty without surrounding whitespace", ErrInvalidModule, id)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.frozen {
		return ErrBuilderFrozen
	}
	for _, installed := range b.modules {
		if installed.id == id {
			return fmt.Errorf("%w: %q", ErrDuplicateModule, id)
		}
	}

	working := b.state.clone()
	beforeOrdinal := working.nextOrdinal
	registration := &registration{owner: id, state: &working, open: true}
	registerErr := module.Register(registration)
	registration.open = false
	if registerErr == nil {
		registerErr = registration.failed
	}
	if registerErr != nil {
		return fmt.Errorf("register module %q: %w", id, registerErr)
	}

	b.state = working
	b.modules = append(b.modules, installedModule{id: id, module: module, contributes: working.nextOrdinal > beforeOrdinal})
	return nil
}

type registration struct {
	owner  string
	state  *registryState
	open   bool
	failed error
}

func (r *registration) Contribute(contributions ...Contribution) error {
	if !r.open {
		return ErrRegistrationClosed
	}
	if r.failed != nil {
		return r.failed
	}
	for _, contribution := range contributions {
		if err := r.apply(contribution); err != nil {
			r.failed = err
			return err
		}
	}
	return nil
}

func (r *registration) apply(contribution Contribution) error {
	if isNil(contribution) {
		return fmt.Errorf("%w: nil", ErrInvalidContribution)
	}
	data := contribution.contributionData()
	if data.constructor == nil && isNil(data.value) {
		return fmt.Errorf("%w: slot %q has a nil value", ErrInvalidContribution, data.spec.id)
	}
	if data.constructor != nil && !isNil(data.value) {
		return fmt.Errorf("%w: slot %q cannot have both a value and constructor", ErrInvalidContribution, data.spec.id)
	}
	if data.spec.kind == manyKind && (data.key == "" || strings.TrimSpace(data.key) != data.key) {
		return fmt.Errorf("%w: slot %q key %q must be non-empty without surrounding whitespace", ErrInvalidKey, data.spec.id, data.key)
	}

	record, exists := r.state.byID[data.spec.id]
	if exists && (record.spec.kind != data.spec.kind || record.spec.valueType != data.spec.valueType) {
		return fmt.Errorf(
			"%w: %q is %s[%s], contribution declares %s[%s]",
			ErrSlotConflict,
			data.spec.id,
			record.spec.kind,
			record.spec.valueType,
			data.spec.kind,
			data.spec.valueType,
		)
	}
	if !exists {
		record = &slotRecord{spec: data.spec}
		r.state.byID[data.spec.id] = record
	}

	switch data.spec.kind {
	case oneKind:
		for _, registered := range record.values {
			if !registered.defaulted && !data.defaulted {
				return fmt.Errorf("%w: slot %q is provided by module %q", ErrSlotOccupied, data.spec.id, registered.owner)
			}
		}
	case manyKind:
		for _, registered := range record.values {
			if registered.key == data.key && !registered.defaulted && !data.defaulted {
				return fmt.Errorf("%w: slot %q key %q is provided by module %q", ErrDuplicateKey, data.spec.id, data.key, registered.owner)
			}
		}
	case chainKind:
		// Registration order is the chain order.
	default:
		return fmt.Errorf("%w: slot %q has unknown kind", ErrInvalidContribution, data.spec.id)
	}

	r.state.nextOrdinal++
	record.values = append(record.values, registeredValue{
		owner:       r.owner,
		key:         data.key,
		value:       data.value,
		constructor: data.constructor,
		ordinal:     r.state.nextOrdinal,
		defaulted:   data.defaulted,
	})
	return nil
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// Requirement selects a slot cardinality or key for a build profile or module
// dependency declaration.
type Requirement struct {
	spec     slotSpec
	key      string
	keyed    bool
	minimum  int
	optional bool
}

// RequireOne requires the contribution to a OneSlot.
func RequireOne[T any](slot OneSlot[T]) Requirement {
	return Requirement{spec: slot.spec, minimum: 1}
}

// OptionalOne declares an optional OneSlot module dependency. When no value
// is installed, ResolveOptionalOne returns false without requiring a provider.
func OptionalOne[T any](slot OneSlot[T]) Requirement {
	return Requirement{spec: slot.spec, optional: true}
}

// RequireMany requires a minimum contribution count from a ManySlot.
func RequireMany[T any](slot ManySlot[T], minimum int) Requirement {
	return Requirement{spec: slot.spec, minimum: minimum}
}

// OptionalMany declares an optional ManySlot module dependency. Installed
// providers still precede the consumer in lifecycle order.
func OptionalMany[T any](slot ManySlot[T]) Requirement {
	return Requirement{spec: slot.spec, optional: true}
}

// RequireKey requires one named contribution from a ManySlot. Module
// dependencies created from it target only the selected provider.
func RequireKey[T any](slot ManySlot[T], key string) Requirement {
	return Requirement{spec: slot.spec, key: key, keyed: true, minimum: 1}
}

// RequireChain requires a minimum contribution count from a ChainSlot.
func RequireChain[T any](slot ChainSlot[T], minimum int) Requirement {
	return Requirement{spec: slot.spec, minimum: minimum}
}

// OptionalChain declares an optional ChainSlot module dependency.
func OptionalChain[T any](slot ChainSlot[T]) Requirement {
	return Requirement{spec: slot.spec, optional: true}
}

// Assembly is an immutable set of resolved components and installed modules.
type Assembly struct {
	state   registryState
	modules []installedModule
	profile []Requirement

	startMu        sync.Mutex
	startAttempted bool
}

// Build validates requirements and freezes the builder after success.
// A failed build remains mutable so missing components can be installed.
func (b *Builder) Build(requirements ...Requirement) (*Assembly, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.frozen {
		return nil, ErrBuilderFrozen
	}
	state, err := resolveDefaultContributions(b.state)
	if err != nil {
		return nil, err
	}
	profile := append([]Requirement(nil), requirements...)
	for _, requirement := range profile {
		if _, err := resolveRequirement(state, requirement); err != nil {
			return nil, err
		}
	}
	modules, err := orderModules(state, activeModules(state, b.modules))
	if err != nil {
		return nil, err
	}
	if err := materializeConstructors(&state, modules); err != nil {
		return nil, err
	}

	b.frozen = true
	return &Assembly{state: state, modules: modules, profile: profile}, nil
}

func (p *Assembly) find(spec slotSpec) (*slotRecord, bool) {
	if p == nil {
		panic("agentslot: nil Assembly")
	}
	record, ok := p.state.byID[spec.id]
	if !ok {
		return nil, false
	}
	if record.spec.kind != spec.kind || record.spec.valueType != spec.valueType {
		panic(fmt.Sprintf("agentslot: slot %q was resolved with a conflicting definition", spec.id))
	}
	return record, true
}
