package agentslot

import (
	"fmt"
	"reflect"
	"strings"
)

type slotKind uint8

const (
	oneKind slotKind = iota + 1
	manyKind
	chainKind
)

func (k slotKind) String() string {
	switch k {
	case oneKind:
		return "one"
	case manyKind:
		return "many"
	case chainKind:
		return "chain"
	default:
		return "unknown"
	}
}

type slotSpec struct {
	id        string
	kind      slotKind
	valueType reflect.Type
}

func newSlotSpec[T any](id string, kind slotKind) slotSpec {
	if id == "" || strings.TrimSpace(id) != id {
		panic(fmt.Sprintf("agentslot: slot ID %q must be non-empty without surrounding whitespace", id))
	}
	return slotSpec{
		id:        id,
		kind:      kind,
		valueType: reflect.TypeOf((*T)(nil)).Elem(),
	}
}

// OneSlot accepts zero or one contribution. A build profile can make the
// contribution mandatory with RequireOne.
type OneSlot[T any] struct {
	spec slotSpec
}

// One declares a typed zero-or-one component slot.
func One[T any](id string) OneSlot[T] {
	return OneSlot[T]{spec: newSlotSpec[T](id, oneKind)}
}

// ID returns the stable slot identifier.
func (s OneSlot[T]) ID() string { return s.spec.id }

// ManySlot accepts multiple contributions with unique keys.
type ManySlot[T any] struct {
	spec slotSpec
}

// Many declares a typed keyed component slot.
func Many[T any](id string) ManySlot[T] {
	return ManySlot[T]{spec: newSlotSpec[T](id, manyKind)}
}

// ID returns the stable slot identifier.
func (s ManySlot[T]) ID() string { return s.spec.id }

// ChainSlot accepts multiple ordered contributions.
type ChainSlot[T any] struct {
	spec slotSpec
}

// Chain declares a typed ordered component slot.
func Chain[T any](id string) ChainSlot[T] {
	return ChainSlot[T]{spec: newSlotSpec[T](id, chainKind)}
}

// ID returns the stable slot identifier.
func (s ChainSlot[T]) ID() string { return s.spec.id }

// Contribution is a typed slot value prepared for module registration.
// Values can only be created with Set, Add, or Append.
type Contribution interface {
	contributionData() contributionData
}

type contributionData struct {
	spec        slotSpec
	key         string
	value       any
	constructor constructorFunc
	defaulted   bool
}

func (c contributionData) contributionData() contributionData { return c }

// Set prepares the sole contribution to a OneSlot.
func Set[T any](slot OneSlot[T], value T) Contribution {
	return contributionData{spec: slot.spec, value: value}
}

// Add prepares a keyed contribution to a ManySlot.
func Add[T any](slot ManySlot[T], key string, value T) Contribution {
	return contributionData{spec: slot.spec, key: key, value: value}
}

// Append prepares an ordered contribution to a ChainSlot.
func Append[T any](slot ChainSlot[T], value T) Contribution {
	return contributionData{spec: slot.spec, value: value}
}

// SetDefault prepares a fallback OneSlot contribution. It is active only when
// the final build contains no explicit contribution to the slot.
func SetDefault[T any](slot OneSlot[T], value T) Contribution {
	return contributionData{spec: slot.spec, value: value, defaulted: true}
}

// AddDefault prepares a fallback ManySlot contribution. It is active only
// when the final build contains no explicit contribution with the same key.
func AddDefault[T any](slot ManySlot[T], key string, value T) Contribution {
	return contributionData{spec: slot.spec, key: key, value: value, defaulted: true}
}

// AppendDefault prepares a fallback ChainSlot contribution. Default chain
// entries are active only when the final build contains no explicit entries.
func AppendDefault[T any](slot ChainSlot[T], value T) Contribution {
	return contributionData{spec: slot.spec, value: value, defaulted: true}
}

// SetWith prepares a OneSlot contribution whose value is constructed during
// Build. The constructor can resolve only slots declared by its owning
// module's RequiredSlots method.
func SetWith[T any](slot OneSlot[T], constructor func(Resolver) (T, error)) Contribution {
	return contributionData{spec: slot.spec, constructor: wrapConstructor(constructor)}
}

// AddWith prepares a keyed ManySlot contribution whose value is constructed
// during Build.
func AddWith[T any](slot ManySlot[T], key string, constructor func(Resolver) (T, error)) Contribution {
	return contributionData{spec: slot.spec, key: key, constructor: wrapConstructor(constructor)}
}

// AppendWith prepares a ChainSlot contribution whose value is constructed
// during Build.
func AppendWith[T any](slot ChainSlot[T], constructor func(Resolver) (T, error)) Contribution {
	return contributionData{spec: slot.spec, constructor: wrapConstructor(constructor)}
}

// SetDefaultWith is SetDefault with build-time construction.
func SetDefaultWith[T any](slot OneSlot[T], constructor func(Resolver) (T, error)) Contribution {
	return contributionData{spec: slot.spec, constructor: wrapConstructor(constructor), defaulted: true}
}

// AddDefaultWith is AddDefault with build-time construction.
func AddDefaultWith[T any](slot ManySlot[T], key string, constructor func(Resolver) (T, error)) Contribution {
	return contributionData{spec: slot.spec, key: key, constructor: wrapConstructor(constructor), defaulted: true}
}

// AppendDefaultWith is AppendDefault with build-time construction.
func AppendDefaultWith[T any](slot ChainSlot[T], constructor func(Resolver) (T, error)) Contribution {
	return contributionData{spec: slot.spec, constructor: wrapConstructor(constructor), defaulted: true}
}

// Named is one keyed value resolved from a ManySlot.
type Named[T any] struct {
	Key   string
	Value T
}

// Get resolves the optional contribution to a OneSlot.
func Get[T any](assembly *Assembly, slot OneSlot[T]) (T, bool) {
	record, ok := assembly.find(slot.spec)
	if !ok || len(record.values) == 0 {
		var zero T
		return zero, false
	}
	value, ok := record.values[0].value.(T)
	if !ok {
		panic("agentslot: internal OneSlot value type invariant violated")
	}
	return value, true
}

// Lookup resolves one keyed contribution from a ManySlot.
func Lookup[T any](assembly *Assembly, slot ManySlot[T], key string) (T, bool) {
	record, ok := assembly.find(slot.spec)
	if ok {
		for _, registered := range record.values {
			if registered.key == key {
				value, typeOK := registered.value.(T)
				if !typeOK {
					panic("agentslot: internal ManySlot value type invariant violated")
				}
				return value, true
			}
		}
	}
	var zero T
	return zero, false
}

// All resolves a copy of all keyed contributions in registration order.
func All[T any](assembly *Assembly, slot ManySlot[T]) []Named[T] {
	record, ok := assembly.find(slot.spec)
	if !ok {
		return nil
	}
	result := make([]Named[T], 0, len(record.values))
	for _, registered := range record.values {
		value, typeOK := registered.value.(T)
		if !typeOK {
			panic("agentslot: internal ManySlot value type invariant violated")
		}
		result = append(result, Named[T]{Key: registered.key, Value: value})
	}
	return result
}

// Ordered resolves a copy of all ChainSlot contributions in registration order.
func Ordered[T any](assembly *Assembly, slot ChainSlot[T]) []T {
	record, ok := assembly.find(slot.spec)
	if !ok {
		return nil
	}
	result := make([]T, 0, len(record.values))
	for _, registered := range record.values {
		value, typeOK := registered.value.(T)
		if !typeOK {
			panic("agentslot: internal ChainSlot value type invariant violated")
		}
		result = append(result, value)
	}
	return result
}
