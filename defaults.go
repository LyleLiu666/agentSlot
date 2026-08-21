package agentslot

import (
	"fmt"
	"sort"
)

// resolveDefaultContributions selects the final component set without
// constructing component values. Explicit contributions win regardless of
// module installation order.
func resolveDefaultContributions(source registryState) (registryState, error) {
	resolved := newRegistryState()
	resolved.nextOrdinal = source.nextOrdinal
	ids := make([]string, 0, len(source.byID))
	for id := range source.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		record := source.byID[id]
		values, err := resolveRecordDefaults(record)
		if err != nil {
			return registryState{}, err
		}
		resolved.byID[id] = &slotRecord{spec: record.spec, values: values}
	}
	return resolved, nil
}

func resolveRecordDefaults(record *slotRecord) ([]registeredValue, error) {
	switch record.spec.kind {
	case oneKind:
		explicit := valuesByDefault(record.values, false)
		if len(explicit) > 0 {
			return explicit, nil
		}
		defaults := valuesByDefault(record.values, true)
		if len(defaults) > 1 {
			return nil, fmt.Errorf("%w: slot %q has multiple default contributions from modules %q and %q", ErrSlotOccupied, record.spec.id, defaults[0].owner, defaults[1].owner)
		}
		return defaults, nil
	case manyKind:
		return resolveManyDefaults(record)
	case chainKind:
		explicit := valuesByDefault(record.values, false)
		if len(explicit) > 0 {
			return explicit, nil
		}
		return valuesByDefault(record.values, true), nil
	default:
		return nil, fmt.Errorf("%w: slot %q has unknown kind", ErrInvalidContribution, record.spec.id)
	}
}

func resolveManyDefaults(record *slotRecord) ([]registeredValue, error) {
	explicitKeys := make(map[string]bool)
	defaultCounts := make(map[string]int)
	for _, value := range record.values {
		if value.defaulted {
			defaultCounts[value.key]++
		} else {
			explicitKeys[value.key] = true
		}
	}
	for key, count := range defaultCounts {
		if count > 1 && !explicitKeys[key] {
			return nil, fmt.Errorf("%w: slot %q key %q has multiple default contributions", ErrDuplicateKey, record.spec.id, key)
		}
	}
	resolved := make([]registeredValue, 0, len(record.values))
	for _, value := range record.values {
		if value.defaulted && explicitKeys[value.key] {
			continue
		}
		resolved = append(resolved, value)
	}
	return resolved, nil
}

func valuesByDefault(values []registeredValue, defaulted bool) []registeredValue {
	selected := make([]registeredValue, 0, len(values))
	for _, value := range values {
		if value.defaulted == defaulted {
			selected = append(selected, value)
		}
	}
	return selected
}

func activeModules(state registryState, installed []installedModule) []installedModule {
	owners := make(map[string]bool)
	for _, record := range state.byID {
		for _, value := range record.values {
			owners[value.owner] = true
		}
	}
	active := make([]installedModule, 0, len(installed))
	for _, module := range installed {
		if !module.contributes || owners[module.id] {
			active = append(active, module)
		}
	}
	return active
}
