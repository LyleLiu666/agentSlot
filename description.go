package agentslot

import "sort"

// AssemblyDescriptionSchema identifies the exported Assembly description format.
const AssemblyDescriptionSchema = "agentslot.assembly/v0"

// AssemblyDescription is a deterministic, JSON-safe view of an assembled Assembly. It
// contains ownership and type metadata but never component values.
type AssemblyDescription struct {
	Schema  string                   `json:"schema"`
	Modules []ModuleDescription      `json:"modules"`
	Slots   []SlotDescription        `json:"slots"`
	Profile []RequirementDescription `json:"profile,omitempty"`
}

// ModuleDescription identifies one module in lifecycle start order.
type ModuleDescription struct {
	ID       string                   `json:"id"`
	Requires []RequirementDescription `json:"requires,omitempty"`
}

// SlotDescription identifies one typed component ecosystem and its owners.
type SlotDescription struct {
	ID            string                    `json:"id"`
	Kind          string                    `json:"kind"`
	ValueType     string                    `json:"value_type"`
	Contributions []ContributionDescription `json:"contributions"`
}

// ContributionDescription identifies one component without exposing its value.
type ContributionDescription struct {
	Module string `json:"module"`
	Key    string `json:"key,omitempty"`
}

// RequirementDescription identifies a required slot cardinality or key.
type RequirementDescription struct {
	Slot    string `json:"slot"`
	Kind    string `json:"kind"`
	Key     string `json:"key,omitempty"`
	Minimum int    `json:"minimum"`
}

// Describe returns a fresh deterministic description of the Assembly. Module order
// is lifecycle start order; slot order is lexical by stable slot ID.
func (a *Assembly) Describe() AssemblyDescription {
	if a == nil {
		panic("agentslot: nil Assembly")
	}
	description := AssemblyDescription{
		Schema:  AssemblyDescriptionSchema,
		Modules: make([]ModuleDescription, 0, len(a.modules)),
		Profile: describeRequirements(a.profile),
	}
	for _, module := range a.modules {
		description.Modules = append(description.Modules, ModuleDescription{
			ID:       module.id,
			Requires: describeRequirements(module.requirements),
		})
	}

	ids := make([]string, 0, len(a.state.byID))
	for id := range a.state.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	description.Slots = make([]SlotDescription, 0, len(ids))
	for _, id := range ids {
		record := a.state.byID[id]
		slot := SlotDescription{
			ID:            id,
			Kind:          record.spec.kind.String(),
			ValueType:     record.spec.valueType.String(),
			Contributions: make([]ContributionDescription, 0, len(record.values)),
		}
		for _, contribution := range record.values {
			slot.Contributions = append(slot.Contributions, ContributionDescription{
				Module: contribution.owner,
				Key:    contribution.key,
			})
		}
		description.Slots = append(description.Slots, slot)
	}
	return description
}

func describeRequirements(requirements []Requirement) []RequirementDescription {
	if len(requirements) == 0 {
		return nil
	}
	descriptions := make([]RequirementDescription, 0, len(requirements))
	for _, requirement := range requirements {
		descriptions = append(descriptions, RequirementDescription{
			Slot:    requirement.spec.id,
			Kind:    requirement.spec.kind.String(),
			Key:     requirement.key,
			Minimum: requirement.minimum,
		})
	}
	sort.Slice(descriptions, func(i, j int) bool {
		left, right := descriptions[i], descriptions[j]
		if left.Slot != right.Slot {
			return left.Slot < right.Slot
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.Key != right.Key {
			return left.Key < right.Key
		}
		return left.Minimum < right.Minimum
	})
	return descriptions
}
