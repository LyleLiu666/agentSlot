package credential

import (
	"context"
	"errors"
	"fmt"
)

// Record configures development and test resolvers. Production products should
// use an external secret service or the encrypted-file resolver instead of
// embedding records in normal application configuration.
type Record struct {
	Ref      Ref
	Identity Identity
	Material Material
}

func (r Record) validate() error {
	if err := r.Ref.Validate(); err != nil {
		return err
	}
	if err := r.Identity.Validate(); err != nil {
		return err
	}
	return r.Material.Validate()
}

type MemoryResolver struct {
	records map[string]Record
}

func NewMemoryResolver(records ...Record) (*MemoryResolver, error) {
	resolved := &MemoryResolver{records: make(map[string]Record, len(records))}
	for _, record := range records {
		if err := record.validate(); err != nil {
			return nil, fmt.Errorf("credential: invalid memory record: %w", err)
		}
		if _, duplicate := resolved.records[record.Ref.ID]; duplicate {
			return nil, errors.New("credential: duplicate memory record")
		}
		copy := record
		copy.Material = cloneMaterial(record.Material)
		resolved.records[record.Ref.ID] = copy
	}
	return resolved, nil
}

func (r *MemoryResolver) Resolve(ctx context.Context, request Request, consume Consumer) (Identity, error) {
	if err := ctx.Err(); err != nil {
		return Identity{}, err
	}
	if err := request.Validate(); err != nil {
		return Identity{}, err
	}
	if consume == nil {
		return Identity{}, errors.New("credential: Consumer is required")
	}
	if r == nil {
		return Identity{}, ErrUnavailable
	}
	record, ok := r.records[request.Ref.ID]
	if !ok {
		return Identity{}, ErrNotFound
	}
	if record.Material.Kind != request.Kind {
		return Identity{}, ErrKindMismatch
	}
	material := cloneMaterial(record.Material)
	defer material.Clear()
	if err := consume(material); err != nil {
		return record.Identity, err
	}
	return record.Identity, nil
}

var _ Resolver = (*MemoryResolver)(nil)
