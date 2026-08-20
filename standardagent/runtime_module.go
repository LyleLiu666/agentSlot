package standardagent

import (
	stdcontext "context"
	"fmt"

	agentslot "github.com/LyleLiu666/agentSlot"
	agentcontext "github.com/LyleLiu666/agentSlot/context"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
	"github.com/LyleLiu666/agentSlot/tool"
)

const (
	runtimeModuleID            = "standardagent.internal.runtime"
	gatewayAccessSlotID        = "standardagent.internal.gateway-access"
	runtimeStateSlotID         = "standardagent.internal.runtime-state"
	mountedEntrypointSlotID    = "standardagent.internal.mounted-entrypoint"
	entrypointValidationSlotID = "standardagent.internal.entrypoint-validation-state"
)

var (
	gatewayAccessSlot        = agentslot.One[interaction.GatewayAccess](gatewayAccessSlotID)
	runtimeStateSlot         = agentslot.One[*applicationRuntime](runtimeStateSlotID)
	mountedEntrypointSlot    = agentslot.Many[mountedEntrypoint](mountedEntrypointSlotID)
	entrypointValidationSlot = agentslot.One[entrypointValidation](entrypointValidationSlotID)
)

type mountedEntrypoint struct{}
type entrypointValidation struct{}

type runtimeModule struct {
	binding *gatewayBinding
	state   *applicationRuntime
}

func newRuntimeModule() *runtimeModule {
	return &runtimeModule{binding: &gatewayBinding{}}
}

func (m *runtimeModule) ID() string { return runtimeModuleID }

func (m *runtimeModule) RequiredSlots() []agentslot.Requirement {
	return []agentslot.Requirement{
		agentslot.RequireOne(session.ManagerSlot),
		agentslot.RequireOne(session.StoreSlot),
		agentslot.RequireOne(model.ExecutorSlot),
		agentslot.OptionalMany(tool.ToolSlot),
		agentslot.OptionalChain(agentcontext.SourceSlot),
		agentslot.OptionalOne(agentcontext.CompactorSlot),
		agentslot.OptionalChain(hook.HookSlot),
		agentslot.OptionalMany(interaction.CommandSlot),
	}
}

func (m *runtimeModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(
		agentslot.Set(gatewayAccessSlot, interaction.GatewayAccess(m.binding)),
		agentslot.SetWith(runtimeStateSlot, func(resolver agentslot.Resolver) (*applicationRuntime, error) {
			manager, err := agentslot.ResolveOne(resolver, session.ManagerSlot)
			if err != nil {
				return nil, err
			}
			store, err := agentslot.ResolveOne(resolver, session.StoreSlot)
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
			dispatcher, err := newToolDispatcher(tools)
			if err != nil {
				return nil, err
			}
			sources, err := agentslot.ResolveChain(resolver, agentcontext.SourceSlot)
			if err != nil {
				return nil, err
			}
			compactor, _, err := agentslot.ResolveOptionalOne(resolver, agentcontext.CompactorSlot)
			if err != nil {
				return nil, err
			}
			hooks, err := agentslot.ResolveChain(resolver, hook.HookSlot)
			if err != nil {
				return nil, err
			}
			state := newApplicationRuntime(runtimeDependencies{
				manager: manager, store: store, executor: executor,
				commands: commands, commandDescriptors: commandDescriptors,
				tools: tools, dispatcher: dispatcher, sources: sources,
				compactor: compactor, hooks: hooks,
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
	manager            session.SessionManager
	store              session.SessionStore
	executor           model.ModelExecutor
	commands           []agentslot.Named[interaction.InteractionCommand]
	commandDescriptors []interaction.CommandDescriptor
	tools              []agentslot.Named[tool.Tool]
	dispatcher         *toolDispatcher
	sources            []agentcontext.ContextSource
	compactor          agentcontext.ContextCompactor
	hooks              []hook.AgentHook
}

// runtimeComponents is one immutable application-level dependency set shared
// by every Session Runtime. Runtime instances retain component references;
// they do not copy tools, stores, executors, or clients per Session.
type runtimeComponents struct {
	store      session.SessionStore
	executor   model.ModelExecutor
	tools      []agentslot.Named[tool.Tool]
	sources    []agentcontext.ContextSource
	compactor  agentcontext.ContextCompactor
	hooks      []hook.AgentHook
	dispatcher *toolDispatcher
}

func (d runtimeDependencies) runtimeComponents() *runtimeComponents {
	return &runtimeComponents{
		store:      d.store,
		executor:   d.executor,
		tools:      append([]agentslot.Named[tool.Tool](nil), d.tools...),
		sources:    append([]agentcontext.ContextSource(nil), d.sources...),
		compactor:  d.compactor,
		hooks:      append([]hook.AgentHook(nil), d.hooks...),
		dispatcher: d.dispatcher,
	}
}
