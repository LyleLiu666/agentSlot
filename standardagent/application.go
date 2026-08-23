// Package standardagent assembles the fixed Agent application spine on top of
// the generic AgentSlot composition core.
package standardagent

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/loop"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

// ApplicationSpec declares one standard Agent application. The standard
// package adds its private Runtime/Gateway module and standard Loop fallback;
// product modules remain
// explicit and are mounted through the same generic Application lifecycle.
type ApplicationSpec struct {
	Name               string
	Modules            []agentslot.Module
	Requirements       []agentslot.Requirement
	RuntimeConfig      AgentRuntimeConfig
	DefaultModelConfig model.Config
}

// AgentRuntimeConfig is copied into every Session Runtime. ToolKeys is a strict
// allowlist: nil, empty, and omitted configurations expose no Tools.
type AgentRuntimeConfig struct {
	SystemPrompt         string
	ToolKeys             []string
	Context              ContextConfig
	ContextRetentionMode ContextRetentionMode
	MaxTokensPerRun      int64
	// MaxInlineToolResultBytes is required when ToolKeys is non-empty.
	MaxInlineToolResultBytes int
}

type ContextRetentionMode string

const (
	ContextLatestOnly ContextRetentionMode = "latest_only"
	ContextRetainAll  ContextRetentionMode = "retain_all"
)

func (m ContextRetentionMode) Valid() bool { return m == ContextLatestOnly || m == ContextRetainAll }

type ContextConfig struct {
	// HardTokenLimit may further reduce the selected model's declared context
	// window. Zero uses the model limit without an additional product cap.
	HardTokenLimit int
}

// NewApplication returns the generic Application with the fixed standard
// Agent module and profile mounted. It has no registration side effects.
func NewApplication(spec ApplicationSpec) *agentslot.Application {
	modules := make([]agentslot.Module, 0, len(spec.Modules)+3)
	modules = append(modules, newRuntimeModule(spec.RuntimeConfig, spec.DefaultModelConfig))
	modules = append(modules, standardLoopModule{})
	modules = append(modules, spec.Modules...)
	modules = append(modules, channelValidationModule{})
	requirements := []agentslot.Requirement{
		agentslot.RequireOne(loop.AgentLoopSlot),
		agentslot.RequireOne(session.StoreSlot),
		agentslot.RequireOne(model.ExecutorSlot),
		agentslot.RequireOne(model.TokenCounterSlot),
		agentslot.RequireMany(interaction.ChannelSlot, 1),
	}
	requirements = append(requirements, spec.Requirements...)
	return agentslot.NewApplication(spec.Name, modules, requirements...)
}

// channelValidationModule turns use of NewGatewayChannelModule into a build
// invariant without adding a framework-private Slot to the public profile.
type channelValidationModule struct{}

func (channelValidationModule) ID() string { return "standardagent.internal.channel-validation" }

func (channelValidationModule) RequiredSlots() []agentslot.Requirement {
	return []agentslot.Requirement{
		agentslot.RequireMany(interaction.ChannelSlot, 1),
		agentslot.RequireMany(mountedChannelSlot, 1),
	}
}

func (channelValidationModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.SetWith(channelValidationSlot,
		func(resolver agentslot.Resolver) (channelValidation, error) {
			channels, err := agentslot.ResolveMany(resolver, interaction.ChannelSlot)
			if err != nil {
				return channelValidation{}, err
			}
			mounted, err := agentslot.ResolveMany(resolver, mountedChannelSlot)
			if err != nil {
				return channelValidation{}, err
			}
			mountedKeys := make(map[string]struct{}, len(mounted))
			for _, contribution := range mounted {
				mountedKeys[contribution.Key] = struct{}{}
			}
			if len(channels) != len(mountedKeys) {
				return channelValidation{}, fmt.Errorf("standardagent: every GatewayChannel must be installed with NewGatewayChannelModule")
			}
			for _, channel := range channels {
				if _, ok := mountedKeys[channel.Key]; !ok {
					return channelValidation{}, fmt.Errorf("standardagent: GatewayChannel %q was not installed with NewGatewayChannelModule", channel.Key)
				}
			}
			return channelValidation{}, nil
		}))
}

// NewGatewayChannelModule wraps one caller-facing adapter with the fixed
// GatewayAccess binding. The wrapper is important: its lifecycle starts after
// the application Runtime has created the Gateway, and it never receives the
// private RuntimeAccess or SessionStore.
func NewGatewayChannelModule(id, key string, channel interaction.GatewayChannel) agentslot.Module {
	return &gatewayChannelModule{id: id, key: key, channel: channel}
}

func isNilGatewayChannel(value interaction.GatewayChannel) bool {
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

type gatewayChannelModule struct {
	id      string
	key     string
	channel interaction.GatewayChannel
	bindMu  sync.Mutex
	bound   bool
}

func (m *gatewayChannelModule) ID() string { return m.id }

func (m *gatewayChannelModule) RequiredSlots() []agentslot.Requirement {
	return []agentslot.Requirement{agentslot.RequireOne(gatewayAccessSlot)}
}

func (m *gatewayChannelModule) Register(reg agentslot.Registrar) error {
	if m.id == "" || m.key == "" || isNilGatewayChannel(m.channel) {
		return fmt.Errorf("standardagent: invalid GatewayChannel module %q/%q", m.id, m.key)
	}
	return reg.Contribute(
		agentslot.Add(mountedChannelSlot, m.key, mountedChannel{}),
		agentslot.AddWith(interaction.ChannelSlot, m.key,
			func(resolver agentslot.Resolver) (interaction.GatewayChannel, error) {
				m.bindMu.Lock()
				defer m.bindMu.Unlock()
				if m.bound {
					return m.channel, nil
				}
				access, err := agentslot.ResolveOne(resolver, gatewayAccessSlot)
				if err != nil {
					return nil, err
				}
				if err := m.channel.Bind(access); err != nil {
					return nil, fmt.Errorf("bind GatewayChannel %q: %w", m.key, err)
				}
				m.bound = true
				return m.channel, nil
			}),
	)
}

// The wrapper owns the adapter's optional lifecycle while preserving the
// fixed start order established by its private GatewayAccess dependency.
func (m *gatewayChannelModule) Start(ctx context.Context) error {
	if lifecycle, ok := m.channel.(agentslot.Lifecycle); ok {
		return lifecycle.Start(ctx)
	}
	return nil
}

func (m *gatewayChannelModule) Stop(ctx context.Context) error {
	if lifecycle, ok := m.channel.(agentslot.Lifecycle); ok {
		return lifecycle.Stop(ctx)
	}
	return nil
}
