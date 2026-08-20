package standardagent

import (
	stdcontext "context"
	"fmt"

	agentslot "github.com/LyleLiu666/agentSlot"
	agentcontext "github.com/LyleLiu666/agentSlot/context"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/observe"
	"github.com/LyleLiu666/agentSlot/policy"
	"github.com/LyleLiu666/agentSlot/session"
	"github.com/LyleLiu666/agentSlot/tool"
)

const (
	runtimeModuleID         = "standardagent.internal.runtime"
	gatewayAccessSlotID     = "standardagent.internal.gateway-access"
	runtimeStateSlotID      = "standardagent.internal.runtime-state"
	mountedChannelSlotID    = "standardagent.internal.mounted-channel"
	channelValidationSlotID = "standardagent.internal.channel-validation-state"
)

var (
	gatewayAccessSlot     = agentslot.One[interaction.GatewayAccess](gatewayAccessSlotID)
	runtimeStateSlot      = agentslot.One[*applicationRuntime](runtimeStateSlotID)
	mountedChannelSlot    = agentslot.Many[mountedChannel](mountedChannelSlotID)
	channelValidationSlot = agentslot.One[channelValidation](channelValidationSlotID)
)

type mountedChannel struct{}
type channelValidation struct{}

type runtimeModule struct {
	binding      *gatewayBinding
	state        *applicationRuntime
	config       AgentRuntimeConfig
	defaultModel model.Config
}

func newRuntimeModule(config AgentRuntimeConfig, defaultModel model.Config) *runtimeModule {
	config = cloneAgentRuntimeConfig(config)
	return &runtimeModule{binding: &gatewayBinding{}, config: config, defaultModel: defaultModel}
}

func (m *runtimeModule) ID() string { return runtimeModuleID }

func (m *runtimeModule) RequiredSlots() []agentslot.Requirement {
	return []agentslot.Requirement{
		agentslot.RequireOne(session.StoreSlot),
		agentslot.RequireOne(model.ExecutorSlot),
		agentslot.OptionalMany(tool.ToolSlot),
		agentslot.OptionalMany(model.CatalogSlot),
		agentslot.OptionalChain(agentcontext.SourceSlot),
		agentslot.OptionalOne(agentcontext.CompactorSlot),
		agentslot.OptionalChain(hook.HookSlot),
		agentslot.OptionalChain(policy.GuardSlot),
		agentslot.OptionalOne(policy.ApprovalSlot),
		agentslot.OptionalChain(observe.TraceSlot),
		agentslot.OptionalChain(observe.MetricSlot),
		agentslot.OptionalChain(observe.AuditSlot),
		agentslot.OptionalChain(observe.UsageSlot),
		agentslot.OptionalMany(interaction.CommandSlot),
	}
}

func (m *runtimeModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(
		agentslot.Set(gatewayAccessSlot, interaction.GatewayAccess(m.binding)),
		agentslot.SetWith(runtimeStateSlot, func(resolver agentslot.Resolver) (*applicationRuntime, error) {
			if m.config.Context.HardTokenLimit < 0 {
				return nil, fmt.Errorf("standardagent: Context HardTokenLimit cannot be negative")
			}
			if m.config.MaxTokensPerRun < 0 {
				return nil, fmt.Errorf("standardagent: MaxTokensPerRun cannot be negative")
			}
			if !m.config.ContextRetentionMode.Valid() {
				return nil, fmt.Errorf("standardagent: invalid ContextRetentionMode %q", m.config.ContextRetentionMode)
			}
			store, err := agentslot.ResolveOne(resolver, session.StoreSlot)
			if err != nil {
				return nil, err
			}
			manager, err := session.NewManager(store, m.defaultModel)
			if err != nil {
				return nil, err
			}
			executor, err := agentslot.ResolveOne(resolver, model.ExecutorSlot)
			if err != nil {
				return nil, err
			}
			commands, err := agentslot.ResolveMany(resolver, interaction.CommandSlot)
			if err != nil {
				return nil, err
			}
			commandDescriptors := make([]interaction.CommandDescriptor, 0, len(commands))
			for _, command := range commands {
				descriptor := command.Value.Describe()
				if descriptor.Key == "" || descriptor.Key != command.Key {
					return nil, fmt.Errorf("standardagent: interaction command slot key %q does not match descriptor key %q", command.Key, descriptor.Key)
				}
				commandDescriptors = append(commandDescriptors, cloneCommandDescriptor(descriptor))
			}
			tools, err := agentslot.ResolveMany(resolver, tool.ToolSlot)
			if err != nil {
				return nil, err
			}
			selectedTools, err := selectRuntimeTools(tools, m.config.ToolKeys)
			if err != nil {
				return nil, err
			}
			guards, err := agentslot.ResolveChain(resolver, policy.GuardSlot)
			if err != nil {
				return nil, err
			}
			approval, _, err := agentslot.ResolveOptionalOne(resolver, policy.ApprovalSlot)
			if err != nil {
				return nil, err
			}
			dispatcher, err := newToolDispatcher(selectedTools, guards, approval)
			if err != nil {
				return nil, err
			}
			catalogs, err := agentslot.ResolveMany(resolver, model.CatalogSlot)
			if err != nil {
				return nil, err
			}
			sources, err := agentslot.ResolveChain(resolver, agentcontext.SourceSlot)
			if err != nil {
				return nil, err
			}
			sourceKeys := make(map[string]bool, len(sources))
			for _, source := range sources {
				if source == nil || source.Key() == "" || sourceKeys[source.Key()] {
					return nil, fmt.Errorf("standardagent: ContextSource keys must be non-empty and unique")
				}
				sourceKeys[source.Key()] = true
			}
			compactor, _, err := agentslot.ResolveOptionalOne(resolver, agentcontext.CompactorSlot)
			if err != nil {
				return nil, err
			}
			hooks, err := agentslot.ResolveChain(resolver, hook.HookSlot)
			if err != nil {
				return nil, err
			}
			traces, err := agentslot.ResolveChain(resolver, observe.TraceSlot)
			if err != nil {
				return nil, err
			}
			metrics, err := agentslot.ResolveChain(resolver, observe.MetricSlot)
			if err != nil {
				return nil, err
			}
			audits, err := agentslot.ResolveChain(resolver, observe.AuditSlot)
			if err != nil {
				return nil, err
			}
			usages, err := agentslot.ResolveChain(resolver, observe.UsageSlot)
			if err != nil {
				return nil, err
			}
			state := newApplicationRuntime(runtimeDependencies{
				manager: manager, store: store, executor: executor,
				commands: commands, commandDescriptors: commandDescriptors,
				tools: selectedTools, dispatcher: dispatcher, catalogs: catalogs, config: cloneAgentRuntimeConfig(m.config), sources: sources,
				compactor: compactor, hooks: hooks,
				traces: traces, metrics: metrics, audits: audits, usages: usages,
			})
			m.state = state
			return state, nil
		}),
	)
}

func (m *runtimeModule) Start(ctx stdcontext.Context) error {
	if m.state == nil {
		return fmt.Errorf("standardagent: runtime state was not constructed")
	}
	if err := m.state.start(ctx); err != nil {
		return err
	}
	m.binding.bind(m.state.gateway)
	return nil
}

func (m *runtimeModule) Stop(ctx stdcontext.Context) error {
	m.binding.unbind()
	if m.state == nil {
		return nil
	}
	return m.state.stop(ctx)
}

type runtimeDependencies struct {
	manager            *session.Manager
	store              session.SessionStore
	executor           model.ModelExecutor
	commands           []agentslot.Named[interaction.InteractionCommand]
	commandDescriptors []interaction.CommandDescriptor
	tools              []agentslot.Named[tool.Tool]
	dispatcher         *toolDispatcher
	catalogs           []agentslot.Named[model.ModelCatalog]
	config             AgentRuntimeConfig
	sources            []agentcontext.ContextSource
	compactor          agentcontext.ContextCompactor
	hooks              []hook.AgentHook
	traces             []observe.TraceSink
	metrics            []observe.MetricSink
	audits             []observe.AuditSink
	usages             []observe.UsageRecorder
}

// runtimeComponents is one immutable application-level dependency set shared
// by every Session Runtime. Runtime instances retain component references;
// they do not copy tools, stores, executors, or clients per Session.
type runtimeComponents struct {
	store        session.SessionStore
	executor     model.ModelExecutor
	tools        []agentslot.Named[tool.Tool]
	sources      []agentcontext.ContextSource
	compactor    agentcontext.ContextCompactor
	hooks        []hook.AgentHook
	dispatcher   *toolDispatcher
	catalogs     []agentslot.Named[model.ModelCatalog]
	config       AgentRuntimeConfig
	observations *observationHub
}

func (d runtimeDependencies) runtimeComponents(observations *observationHub) *runtimeComponents {
	dispatcher := d.dispatcher.withObservations(observations)
	return &runtimeComponents{
		store:        d.store,
		executor:     d.executor,
		tools:        append([]agentslot.Named[tool.Tool](nil), d.tools...),
		sources:      append([]agentcontext.ContextSource(nil), d.sources...),
		compactor:    d.compactor,
		hooks:        append([]hook.AgentHook(nil), d.hooks...),
		dispatcher:   dispatcher,
		catalogs:     append([]agentslot.Named[model.ModelCatalog](nil), d.catalogs...),
		config:       cloneAgentRuntimeConfig(d.config),
		observations: observations,
	}
}

func selectRuntimeTools(installed []agentslot.Named[tool.Tool], keys []string) ([]agentslot.Named[tool.Tool], error) {
	if keys == nil {
		return append([]agentslot.Named[tool.Tool](nil), installed...), nil
	}
	byKey := make(map[string]agentslot.Named[tool.Tool], len(installed))
	for _, named := range installed {
		byKey[named.Key] = named
	}
	selected := make([]agentslot.Named[tool.Tool], 0, len(keys))
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if key == "" || seen[key] {
			return nil, fmt.Errorf("standardagent: ToolKeys must be non-empty and unique")
		}
		named, ok := byKey[key]
		if !ok {
			return nil, fmt.Errorf("standardagent: selected Tool %q is not installed", key)
		}
		seen[key] = true
		selected = append(selected, named)
	}
	return selected, nil
}

func cloneAgentRuntimeConfig(source AgentRuntimeConfig) AgentRuntimeConfig {
	cloned := source
	if cloned.ContextRetentionMode == "" {
		cloned.ContextRetentionMode = ContextLatestOnly
	}
	if source.ToolKeys != nil {
		cloned.ToolKeys = make([]string, len(source.ToolKeys))
		copy(cloned.ToolKeys, source.ToolKeys)
	}
	return cloned
}
