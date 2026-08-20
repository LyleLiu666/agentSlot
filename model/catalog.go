package model

import (
	"context"
	"errors"

	agentslot "github.com/LyleLiu666/agentSlot"
)

// CatalogSlot contains optional UI-facing model catalogs keyed by provider.
// Runtime correctness uses ModelExecutor.Inspect; catalogs never authorize a
// configuration or expose credentials.
var CatalogSlot = agentslot.Many[ModelCatalog]("model.catalog")

type ModelCatalog interface {
	Models(context.Context) ([]Descriptor, error)
}

type Descriptor struct {
	ProviderKey  string
	ModelID      string
	Title        string
	Capabilities ExecutionCapabilities
}

func (d Descriptor) Validate() error {
	if d.ProviderKey == "" || d.ModelID == "" || d.Title == "" {
		return errors.New("model: descriptor identity and title are required")
	}
	return d.Capabilities.Validate()
}

// StaticCatalog is a detached deterministic reference implementation.
type StaticCatalog struct{ descriptors []Descriptor }

func NewStaticCatalog(descriptors ...Descriptor) (*StaticCatalog, error) {
	copy := cloneDescriptors(descriptors)
	seen := make(map[string]bool)
	for _, descriptor := range copy {
		if err := descriptor.Validate(); err != nil {
			return nil, err
		}
		key := descriptor.ProviderKey + "\x00" + descriptor.ModelID
		if seen[key] {
			return nil, errors.New("model: duplicate catalog model")
		}
		seen[key] = true
	}
	return &StaticCatalog{descriptors: copy}, nil
}

func (c *StaticCatalog) Models(ctx context.Context) ([]Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return cloneDescriptors(c.descriptors), nil
}

func cloneDescriptors(source []Descriptor) []Descriptor {
	copy := make([]Descriptor, len(source))
	for index, descriptor := range source {
		copy[index] = descriptor
		copy[index].Capabilities.Media.InputModalities = append([]Modality(nil), descriptor.Capabilities.Media.InputModalities...)
		copy[index].Capabilities.Media.OutputModalities = append([]Modality(nil), descriptor.Capabilities.Media.OutputModalities...)
		copy[index].Capabilities.Reasoning = append([]Reasoning(nil), descriptor.Capabilities.Reasoning...)
	}
	return copy
}
