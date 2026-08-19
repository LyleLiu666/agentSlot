package agentslot

import (
	"fmt"
	"strings"
)

func resolveRequirement(state registryState, requirement Requirement) ([]registeredValue, error) {
	if err := validateRequirement(requirement); err != nil {
		return nil, err
	}
	record := state.byID[requirement.spec.id]
	if record == nil {
		if requirement.optional {
			return nil, nil
		}
		return nil, unsatisfiedRequirementError(requirement, 0)
	}
	if record.spec.kind != requirement.spec.kind || record.spec.valueType != requirement.spec.valueType {
		return nil, fmt.Errorf("%w: requirement for slot %q conflicts with its installed definition", ErrSlotConflict, requirement.spec.id)
	}
	if requirement.keyed {
		for _, registered := range record.values {
			if registered.key == requirement.key {
				return []registeredValue{registered}, nil
			}
		}
		return nil, fmt.Errorf(
			"%w: slot %q requires key %q",
			ErrRequirementUnsatisfied,
			requirement.spec.id,
			requirement.key,
		)
	}
	if len(record.values) < requirement.minimum {
		return nil, unsatisfiedRequirementError(requirement, len(record.values))
	}
	return record.values, nil
}

func validateRequirement(requirement Requirement) error {
	if requirement.spec.id == "" || requirement.spec.valueType == nil {
		return fmt.Errorf("%w: missing slot definition", ErrInvalidRequirement)
	}
	switch requirement.spec.kind {
	case oneKind:
		validMinimum := requirement.minimum == 1 && !requirement.optional
		validOptional := requirement.minimum == 0 && requirement.optional
		if requirement.keyed || requirement.key != "" || (!validMinimum && !validOptional) {
			return fmt.Errorf("%w: OneSlot %q must be required with minimum 1 or explicitly optional, without a key", ErrInvalidRequirement, requirement.spec.id)
		}
	case manyKind:
		if requirement.minimum < 1 && !requirement.optional {
			return fmt.Errorf("%w: ManySlot %q minimum must be positive", ErrInvalidRequirement, requirement.spec.id)
		}
		if requirement.optional && requirement.minimum != 0 {
			return fmt.Errorf("%w: optional ManySlot %q minimum must be zero", ErrInvalidRequirement, requirement.spec.id)
		}
		if requirement.keyed && (requirement.key == "" || strings.TrimSpace(requirement.key) != requirement.key) {
			return fmt.Errorf("%w: ManySlot %q key %q must be non-empty without surrounding whitespace", ErrInvalidRequirement, requirement.spec.id, requirement.key)
		}
		if !requirement.keyed && requirement.key != "" {
			return fmt.Errorf("%w: ManySlot %q has a key without keyed selection", ErrInvalidRequirement, requirement.spec.id)
		}
	case chainKind:
		if requirement.keyed || requirement.key != "" || (requirement.minimum < 1 && !requirement.optional) || (requirement.optional && requirement.minimum != 0) {
			return fmt.Errorf("%w: ChainSlot %q must have a positive minimum or be explicitly optional, without a key", ErrInvalidRequirement, requirement.spec.id)
		}
	default:
		return fmt.Errorf("%w: slot %q has unknown kind", ErrInvalidRequirement, requirement.spec.id)
	}
	return nil
}

func unsatisfiedRequirementError(requirement Requirement, actual int) error {
	return fmt.Errorf(
		"%w: slot %q requires at least %d contribution(s), got %d",
		ErrRequirementUnsatisfied,
		requirement.spec.id,
		requirement.minimum,
		actual,
	)
}

func orderModules(state registryState, installed []installedModule) ([]installedModule, error) {
	modules := make([]installedModule, len(installed))
	copy(modules, installed)
	ownerIndex := make(map[string]int, len(modules))
	for index, module := range modules {
		ownerIndex[module.id] = index
	}

	edges := make([]map[int]struct{}, len(modules))
	indegree := make([]int, len(modules))
	for consumerIndex := range modules {
		requirer, ok := modules[consumerIndex].module.(SlotRequirer)
		if !ok {
			continue
		}
		requirements := append([]Requirement(nil), requirer.RequiredSlots()...)
		modules[consumerIndex].requirements = requirements
		for _, requirement := range requirements {
			providers, err := resolveRequirement(state, requirement)
			if err != nil {
				return nil, fmt.Errorf("module %q requirement: %w", modules[consumerIndex].id, err)
			}
			for _, provider := range providers {
				providerIndex, exists := ownerIndex[provider.owner]
				if !exists {
					return nil, fmt.Errorf("module %q requirement references unknown provider module %q", modules[consumerIndex].id, provider.owner)
				}
				if providerIndex == consumerIndex {
					continue
				}
				if edges[providerIndex] == nil {
					edges[providerIndex] = make(map[int]struct{})
				}
				if _, duplicate := edges[providerIndex][consumerIndex]; duplicate {
					continue
				}
				edges[providerIndex][consumerIndex] = struct{}{}
				indegree[consumerIndex]++
			}
		}
	}

	ordered := make([]installedModule, 0, len(modules))
	emitted := make([]bool, len(modules))
	for len(ordered) < len(modules) {
		candidate := -1
		for index := range modules {
			if !emitted[index] && indegree[index] == 0 {
				candidate = index
				break
			}
		}
		if candidate == -1 {
			cycle := make([]string, 0, len(modules)-len(ordered))
			for index, module := range modules {
				if !emitted[index] {
					cycle = append(cycle, module.id)
				}
			}
			return nil, fmt.Errorf("%w among %s", ErrDependencyCycle, strings.Join(cycle, ", "))
		}

		emitted[candidate] = true
		ordered = append(ordered, modules[candidate])
		for consumer := range edges[candidate] {
			indegree[consumer]--
		}
	}
	return ordered, nil
}
