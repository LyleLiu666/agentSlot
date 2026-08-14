package agentslot

import (
	"fmt"
	"sort"
)

type constructorFunc func(Resolver) (any, error)

func wrapConstructor[T any](constructor func(Resolver) (T, error)) constructorFunc {
	if constructor == nil {
		return nil
	}
	return func(resolver Resolver) (any, error) {
		return constructor(resolver)
	}
}

// Resolver is a build-scoped view of one module's declared dependencies. It
// is valid only while AgentSlot calls that module's component constructor.
// Retaining it does not create a runtime service locator: every later resolve
// fails with ErrResolverClosed.
type Resolver struct {
	scope *resolverScope
}

type resolverScope struct {
	state        *registryState
	requirements []Requirement
	open         bool
}

type materializationTarget struct {
	record  *slotRecord
	index   int
	ordinal uint64
}

func materializeConstructors(state *registryState, modules []installedModule) error {
	for _, module := range modules {
		targets := constructorTargetsForOwner(state, module.id)
		for _, target := range targets {
			registered := &target.record.values[target.index]
			scope := &resolverScope{
				state:        state,
				requirements: append([]Requirement(nil), module.requirements...),
				open:         true,
			}
			value, err := registered.constructor(Resolver{scope: scope})
			scope.open = false
			if err != nil {
				return fmt.Errorf(
					"construct module %q contribution to slot %q: %w",
					module.id,
					target.record.spec.id,
					err,
				)
			}
			if isNil(value) {
				return fmt.Errorf(
					"construct module %q contribution to slot %q: %w: nil value",
					module.id,
					target.record.spec.id,
					ErrInvalidContribution,
				)
			}
			registered.value = value
			registered.constructor = nil
		}
	}
	return nil
}

func constructorTargetsForOwner(state *registryState, owner string) []materializationTarget {
	var targets []materializationTarget
	for _, record := range state.byID {
		for index := range record.values {
			registered := record.values[index]
			if registered.owner != owner || registered.constructor == nil {
				continue
			}
			targets = append(targets, materializationTarget{
				record:  record,
				index:   index,
				ordinal: registered.ordinal,
			})
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		return targets[i].ordinal < targets[j].ordinal
	})
	return targets
}

func (r Resolver) resolve(spec slotSpec, key string, keyed bool) ([]registeredValue, error) {
	if r.scope == nil || !r.scope.open {
		return nil, ErrResolverClosed
	}
	requirement, ok := declaredRequirement(r.scope.requirements, spec, key, keyed)
	if !ok {
		return nil, fmt.Errorf("%w: slot %q", ErrDependencyUndeclared, spec.id)
	}
	values, err := resolveRequirement(*r.scope.state, requirement)
	if err != nil {
		return nil, err
	}
	for _, registered := range values {
		if registered.constructor != nil || isNil(registered.value) {
			return nil, fmt.Errorf(
				"%w: slot %q contribution from module %q",
				ErrDependencyNotReady,
				spec.id,
				registered.owner,
			)
		}
	}
	return values, nil
}

func declaredRequirement(requirements []Requirement, spec slotSpec, key string, keyed bool) (Requirement, bool) {
	for _, requirement := range requirements {
		if requirement.spec != spec || requirement.keyed != keyed {
			continue
		}
		if keyed && requirement.key != key {
			continue
		}
		return requirement, true
	}
	return Requirement{}, false
}

// ResolveOne resolves a OneSlot declared with RequireOne by the current
// constructor's owning module.
func ResolveOne[T any](resolver Resolver, slot OneSlot[T]) (T, error) {
	values, err := resolver.resolve(slot.spec, "", false)
	if err != nil {
		var zero T
		return zero, err
	}
	return resolvedValue[T](values[0])
}

// ResolveKey resolves one ManySlot key declared with RequireKey by the current
// constructor's owning module.
func ResolveKey[T any](resolver Resolver, slot ManySlot[T], key string) (T, error) {
	values, err := resolver.resolve(slot.spec, key, true)
	if err != nil {
		var zero T
		return zero, err
	}
	return resolvedValue[T](values[0])
}

// ResolveMany resolves all ManySlot contributions when the owning module
// declared a non-keyed RequireMany requirement for the slot.
func ResolveMany[T any](resolver Resolver, slot ManySlot[T]) ([]Named[T], error) {
	values, err := resolver.resolve(slot.spec, "", false)
	if err != nil {
		return nil, err
	}
	result := make([]Named[T], 0, len(values))
	for _, registered := range values {
		value, err := resolvedValue[T](registered)
		if err != nil {
			return nil, err
		}
		result = append(result, Named[T]{Key: registered.key, Value: value})
	}
	return result, nil
}

// ResolveChain resolves all ChainSlot contributions when the owning module
// declared RequireChain for the slot.
func ResolveChain[T any](resolver Resolver, slot ChainSlot[T]) ([]T, error) {
	values, err := resolver.resolve(slot.spec, "", false)
	if err != nil {
		return nil, err
	}
	result := make([]T, 0, len(values))
	for _, registered := range values {
		value, err := resolvedValue[T](registered)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, nil
}

func resolvedValue[T any](registered registeredValue) (T, error) {
	value, ok := registered.value.(T)
	if !ok {
		var zero T
		return zero, fmt.Errorf(
			"%w: materialized value from module %q violates its slot type",
			ErrInvalidContribution,
			registered.owner,
		)
	}
	return value, nil
}
