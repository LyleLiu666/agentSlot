// Package standardagent assembles the fixed Agent application spine on top of
// the generic AgentSlot composition core.
package standardagent

import (
	"context"
	"fmt"
	"reflect"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

// ApplicationSpec declares one standard Agent application. The standard
// package adds its private Runtime/Gateway module; product modules remain
// explicit and are mounted through the same generic Application lifecycle.
type ApplicationSpec struct {
	Name         string
	Modules      []agentslot.Module
	Requirements []agentslot.Requirement
}

// NewApplication returns the generic Application with the fixed standard
// Agent module and profile mounted. It has no registration side effects.
func NewApplication(spec ApplicationSpec) *agentslot.Application {
	modules := make([]agentslot.Module, 0, len(spec.Modules)+2)
	modules = append(modules, newRuntimeModule())
	modules = append(modules, spec.Modules...)
	modules = append(modules, entrypointValidationModule{})
	requirements := []agentslot.Requirement{
		agentslot.RequireOne(session.ManagerSlot),
		agentslot.RequireOne(session.StoreSlot),
		agentslot.RequireOne(model.ExecutorSlot),
		agentslot.RequireMany(interaction.EntrypointSlot, 1),
	}
	requirements = append(requirements, spec.Requirements...)
	return agentslot.NewApplication(spec.Name, modules, requirements...)
}

// entrypointValidationModule turns use of NewEntrypointModule into a build
// invariant without adding a framework-private Slot to the public profile.
type entrypointValidationModule struct{}

func (entrypointValidationModule) ID() string { return "standardagent.internal.entrypoint-validation" }

func (entrypointValidationModule) RequiredSlots() []agentslot.Requirement {
	return []agentslot.Requirement{
		agentslot.RequireMany(interaction.EntrypointSlot, 1),
		agentslot.RequireMany(mountedEntrypointSlot, 1),
	}
}

func (entrypointValidationModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.SetWith(entrypointValidationSlot,
		func(resolver agentslot.Resolver) (entrypointValidation, error) {
			entrypoints, err := agentslot.ResolveMany(resolver, interaction.EntrypointSlot)
			if err != nil {
				return entrypointValidation{}, err
			}
			mounted, err := agentslot.ResolveMany(resolver, mountedEntrypointSlot)
			if err != nil {
				return entrypointValidation{}, err
			}
			mountedKeys := make(map[string]struct{}, len(mounted))
			for _, contribution := range mounted {
				mountedKeys[contribution.Key] = struct{}{}
			}
			if len(entrypoints) != len(mountedKeys) {
				return entrypointValidation{}, fmt.Errorf("standardagent: every Entrypoint must be installed with NewEntrypointModule")
			}
			for _, entrypoint := range entrypoints {
				if _, ok := mountedKeys[entrypoint.Key]; !ok {
					return entrypointValidation{}, fmt.Errorf("standardagent: Entrypoint %q was not installed with NewEntrypointModule", entrypoint.Key)
				}
			}
			return entrypointValidation{}, nil
		}))
}

// NewEntrypointModule wraps one caller-facing adapter with the fixed
// GatewayAccess binding. The wrapper is important: its lifecycle starts after
// the application Runtime has created the Gateway, and it never receives the
// private RuntimeAccess or SessionStore.
func NewEntrypointModule(id, key string, entrypoint interaction.Entrypoint) agentslot.Module {
	return &entrypointModule{id: id, key: key, entrypoint: entrypoint}
}

func isNilEntrypoint(value interaction.Entrypoint) bool {
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

type entrypointModule struct {
	id         string
	key        string
	entrypoint interaction.Entrypoint
}

func (m *entrypointModule) ID() string { return m.id }

func (m *entrypointModule) RequiredSlots() []agentslot.Requirement {
	return []agentslot.Requirement{agentslot.RequireOne(gatewayAccessSlot)}
}

func (m *entrypointModule) Register(reg agentslot.Registrar) error {
	if m.id == "" || m.key == "" || isNilEntrypoint(m.entrypoint) {
		return fmt.Errorf("standardagent: invalid Entrypoint module %q/%q", m.id, m.key)
	}
	return reg.Contribute(
		agentslot.Add(mountedEntrypointSlot, m.key, mountedEntrypoint{}),
		agentslot.AddWith(interaction.EntrypointSlot, m.key,
			func(resolver agentslot.Resolver) (interaction.Entrypoint, error) {
				access, err := agentslot.ResolveOne(resolver, gatewayAccessSlot)
				if err != nil {
					return nil, err
				}
				if err := m.entrypoint.Attach(access); err != nil {
					return nil, fmt.Errorf("attach Entrypoint %q: %w", m.key, err)
				}
				return m.entrypoint, nil
			}),
	)
}

// The wrapper owns the adapter's optional lifecycle while preserving the
// fixed start order established by its private GatewayAccess dependency.
func (m *entrypointModule) Start(ctx context.Context) error {
	if lifecycle, ok := m.entrypoint.(agentslot.Lifecycle); ok {
		return lifecycle.Start(ctx)
	}
	return nil
}

func (m *entrypointModule) Stop(ctx context.Context) error {
	if lifecycle, ok := m.entrypoint.(agentslot.Lifecycle); ok {
		return lifecycle.Stop(ctx)
	}
	return nil
}
