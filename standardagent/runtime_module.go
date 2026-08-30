package standardagent

import (
	stdcontext "context"
	"fmt"
	"reflect"

	agentslot "github.com/LyleLiu666/agentSlot"
	agentcontext "github.com/LyleLiu666/agentSlot/context"
	"github.com/LyleLiu666/agentSlot/goal"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/interaction"
	agentloop "github.com/LyleLiu666/agentSlot/loop"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/observe"
	"github.com/LyleLiu666/agentSlot/policy"
	"github.com/LyleLiu666/agentSlot/session"
	"github.com/LyleLiu666/agentSlot/tool"
	"github.com/LyleLiu666/agentSlot/workspace"
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
		agentslot.RequireOne(agentloop.AgentLoopSlot),
		agentslot.RequireOne(session.StoreSlot),
		agentslot.RequireOne(model.ExecutorSlot),
		agentslot.RequireOne(model.TokenCounterSlot),
		agentslot.OptionalMany(tool.ToolSlot),
		agentslot.OptionalMany(model.CatalogSlot),
		agentslot.OptionalChain(model.AttemptObserverSlot),
		agentslot.OptionalChain(agentcontext.SourceSlot),
		agentslot.OptionalOne(agentcontext.CompactorSlot),
		agentslot.OptionalChain(hook.HookSlot),
		agentslot.OptionalChain(hook.InputGateSlot),
		agentslot.OptionalChain(hook.ToolPreflightSlot),
		agentslot.OptionalChain(hook.ToolResultHookSlot),
		agentslot.OptionalChain(hook.CompletionGateSlot),
		agentslot.OptionalOne(goal.StoreSlot),
		agentslot.OptionalOne(goal.EvaluatorSlot),
		agentslot.OptionalChain(session.CommitObserverSlot),
		agentslot.OptionalChain(policy.GuardSlot),
		agentslot.OptionalOne(policy.ApprovalSlot),
		agentslot.OptionalChain(observe.TraceSlot),
		agentslot.OptionalChain(observe.MetricSlot),
		agentslot.OptionalChain(observe.AuditSlot),
		agentslot.OptionalChain(observe.UsageSlot),
		agentslot.OptionalMany(interaction.CommandSlot),
		agentslot.OptionalOne(workspace.ManagerSlot),
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
			if m.config.MaxInlineToolResultBytes < 0 {
				return nil, fmt.Errorf("standardagent: MaxInlineToolResultBytes cannot be negative")
			}
			if !m.config.ContextRetentionMode.Valid() {
				return nil, fmt.Errorf("standardagent: invalid ContextRetentionMode %q", m.config.ContextRetentionMode)
			}
			selectedLoop, err := agentslot.ResolveOne(resolver, agentloop.AgentLoopSlot)
			if err != nil {
				return nil, err
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
			counter, err := agentslot.ResolveOne(resolver, model.TokenCounterSlot)
			if err != nil {
				return nil, err
			}
			attemptObservers, err := agentslot.ResolveChain(resolver, model.AttemptObserverSlot)
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
			if len(selectedTools) > 0 && m.config.MaxInlineToolResultBytes == 0 {
				return nil, fmt.Errorf("standardagent: MaxInlineToolResultBytes must be positive when tools are selected")
			}
			guards, err := agentslot.ResolveChain(resolver, policy.GuardSlot)
			if err != nil {
				return nil, err
			}
			approval, _, err := agentslot.ResolveOptionalOne(resolver, policy.ApprovalSlot)
			if err != nil {
				return nil, err
			}
			dispatcher, err := newToolDispatcher(selectedTools, guards, approval, m.config.MaxInlineToolResultBytes)
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
			inputGates, err := agentslot.ResolveChain(resolver, hook.InputGateSlot)
			if err != nil {
				return nil, err
			}
			frozenInputGates := make([]inputGateBinding, 0, len(inputGates))
			inputGateKeys := make(map[string]struct{}, len(inputGates))
			for _, gate := range inputGates {
				if isNilInputGate(gate) {
					return nil, fmt.Errorf("standardagent: InputGate contribution is nil")
				}
				descriptor := gate.Descriptor()
				if err := descriptor.Validate(); err != nil {
					return nil, err
				}
				if _, duplicate := inputGateKeys[descriptor.Key]; duplicate {
					return nil, fmt.Errorf("standardagent: InputGate descriptor key %q is duplicated", descriptor.Key)
				}
				inputGateKeys[descriptor.Key] = struct{}{}
				frozenInputGates = append(frozenInputGates, inputGateBinding{gate: gate, descriptor: descriptor})
			}
			toolPreflights, err := agentslot.ResolveChain(resolver, hook.ToolPreflightSlot)
			if err != nil {
				return nil, err
			}
			frozenToolPreflights := make([]toolPreflightBinding, 0, len(toolPreflights))
			toolPreflightKeys := make(map[string]struct{}, len(toolPreflights))
			for _, preflight := range toolPreflights {
				if isNilToolPreflight(preflight) {
					return nil, fmt.Errorf("standardagent: ToolPreflight contribution is nil")
				}
				descriptor := preflight.Descriptor()
				if err := descriptor.Validate(); err != nil {
					return nil, err
				}
				if _, duplicate := toolPreflightKeys[descriptor.Key]; duplicate {
					return nil, fmt.Errorf("standardagent: ToolPreflight descriptor key %q is duplicated", descriptor.Key)
				}
				scope := preflight.Scope()
				if err := scope.Validate(); err != nil {
					return nil, err
				}
				toolPreflightKeys[descriptor.Key] = struct{}{}
				frozenToolPreflights = append(frozenToolPreflights, toolPreflightBinding{
					preflight: preflight, descriptor: descriptor, scope: cloneToolScope(scope),
				})
			}
			toolResultHooks, err := agentslot.ResolveChain(resolver, hook.ToolResultHookSlot)
			if err != nil {
				return nil, err
			}
			frozenToolResultHooks := make([]toolResultHookBinding, 0, len(toolResultHooks))
			toolResultHookKeys := make(map[string]struct{}, len(toolResultHooks))
			for _, resultHook := range toolResultHooks {
				if isNilToolResultHook(resultHook) {
					return nil, fmt.Errorf("standardagent: ToolResultHook contribution is nil")
				}
				descriptor := resultHook.Descriptor()
				if err := descriptor.Validate(); err != nil {
					return nil, err
				}
				if _, duplicate := toolResultHookKeys[descriptor.Key]; duplicate {
					return nil, fmt.Errorf("standardagent: ToolResultHook descriptor key %q is duplicated", descriptor.Key)
				}
				scope := resultHook.Scope()
				if err := scope.Validate(); err != nil {
					return nil, err
				}
				toolResultHookKeys[descriptor.Key] = struct{}{}
				frozenToolResultHooks = append(frozenToolResultHooks, toolResultHookBinding{
					hook: resultHook, descriptor: descriptor, scope: cloneToolResultScope(scope),
				})
			}
			completionGates, err := agentslot.ResolveChain(resolver, hook.CompletionGateSlot)
			if err != nil {
				return nil, err
			}
			frozenCompletionGates := make([]completionGateBinding, 0, len(completionGates))
			completionGateKeys := make(map[string]struct{}, len(completionGates))
			for _, gate := range completionGates {
				if isNilCompletionGate(gate) {
					return nil, fmt.Errorf("standardagent: CompletionGate contribution is nil")
				}
				descriptor := gate.Descriptor()
				if err := descriptor.Validate(); err != nil {
					return nil, err
				}
				if _, duplicate := completionGateKeys[descriptor.Key]; duplicate {
					return nil, fmt.Errorf("standardagent: CompletionGate descriptor key %q is duplicated", descriptor.Key)
				}
				completionGateKeys[descriptor.Key] = struct{}{}
				frozenCompletionGates = append(frozenCompletionGates, completionGateBinding{gate: gate, descriptor: descriptor})
			}
			goalStore, hasGoalStore, err := agentslot.ResolveOptionalOne(resolver, goal.StoreSlot)
			if err != nil {
				return nil, err
			}
			goalEvaluator, hasGoalEvaluator, err := agentslot.ResolveOptionalOne(resolver, goal.EvaluatorSlot)
			if err != nil {
				return nil, err
			}
			if hasGoalStore != hasGoalEvaluator {
				return nil, fmt.Errorf("standardagent: goal.store and goal.evaluator must be installed together")
			}
			commitObservers, err := agentslot.ResolveChain(resolver, session.CommitObserverSlot)
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
			workspaceManager, _, err := agentslot.ResolveOptionalOne(resolver, workspace.ManagerSlot)
			if err != nil {
				return nil, err
			}
			state := newApplicationRuntime(runtimeDependencies{
				agentLoop: selectedLoop,
				manager:   manager, store: store, executor: executor, counter: counter, attemptObservers: attemptObservers,
				commands: commands, commandDescriptors: commandDescriptors,
				tools: selectedTools, dispatcher: dispatcher, catalogs: catalogs, config: cloneAgentRuntimeConfig(m.config), sources: sources,
				compactor: compactor, hooks: hooks, inputGates: frozenInputGates, toolPreflights: frozenToolPreflights, toolResultHooks: frozenToolResultHooks,
				completionGates: frozenCompletionGates,
				goalStore:       goalStore, goalEvaluator: goalEvaluator, commitObservers: commitObservers,
				traces: traces, metrics: metrics, audits: audits, usages: usages, workspaceManager: workspaceManager,
			})
			m.state = state
			return state, nil
		}),
	)
}

func isNilInputGate(gate hook.InputGate) bool {
	if gate == nil {
		return true
	}
	value := reflect.ValueOf(gate)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func isNilToolPreflight(preflight hook.ToolPreflight) bool {
	if preflight == nil {
		return true
	}
	value := reflect.ValueOf(preflight)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func isNilToolResultHook(resultHook hook.ToolResultHook) bool {
	if resultHook == nil {
		return true
	}
	value := reflect.ValueOf(resultHook)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func isNilCompletionGate(gate hook.CompletionGate) bool {
	if gate == nil {
		return true
	}
	value := reflect.ValueOf(gate)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
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
	agentLoop          agentloop.AgentLoop
	manager            *session.Manager
	store              session.SessionStore
	executor           model.ModelExecutor
	counter            model.TokenCounter
	attemptObservers   []model.AttemptObserver
	commands           []agentslot.Named[interaction.InteractionCommand]
	commandDescriptors []interaction.CommandDescriptor
	tools              []agentslot.Named[tool.Tool]
	dispatcher         *toolDispatcher
	catalogs           []agentslot.Named[model.ModelCatalog]
	config             AgentRuntimeConfig
	sources            []agentcontext.ContextSource
	compactor          agentcontext.ContextCompactor
	hooks              []hook.AgentHook
	inputGates         []inputGateBinding
	toolPreflights     []toolPreflightBinding
	toolResultHooks    []toolResultHookBinding
	completionGates    []completionGateBinding
	goalStore          goal.Store
	goalEvaluator      goal.Evaluator
	commitObservers    []session.SessionCommitObserver
	traces             []observe.TraceSink
	metrics            []observe.MetricSink
	audits             []observe.AuditSink
	usages             []observe.UsageRecorder
	workspaceManager   workspace.Manager
}

// runtimeComponents is one immutable application-level dependency set shared
// by every Session Runtime. Runtime instances retain component references;
// they do not copy tools, stores, executors, or clients per Session.
type runtimeComponents struct {
	agentLoop        agentloop.AgentLoop
	store            session.SessionStore
	executor         model.ModelExecutor
	counter          model.TokenCounter
	attemptObservers []model.AttemptObserver
	tools            []agentslot.Named[tool.Tool]
	sources          []agentcontext.ContextSource
	compactor        agentcontext.ContextCompactor
	hooks            []hook.AgentHook
	inputGates       []inputGateBinding
	toolPreflights   []toolPreflightBinding
	toolResultHooks  []toolResultHookBinding
	completionGates  []completionGateBinding
	goalStore        goal.Store
	goalEvaluator    goal.Evaluator
	commitObservers  []session.SessionCommitObserver
	dispatcher       *toolDispatcher
	catalogs         []agentslot.Named[model.ModelCatalog]
	config           AgentRuntimeConfig
	observations     *observationHub
	workspaceManager workspace.Manager
}

func (d runtimeDependencies) runtimeComponents(observations *observationHub) *runtimeComponents {
	dispatcher := d.dispatcher.withObservations(observations)
	return &runtimeComponents{
		agentLoop:        d.agentLoop,
		store:            d.store,
		executor:         d.executor,
		counter:          d.counter,
		attemptObservers: append([]model.AttemptObserver(nil), d.attemptObservers...),
		tools:            append([]agentslot.Named[tool.Tool](nil), d.tools...),
		sources:          append([]agentcontext.ContextSource(nil), d.sources...),
		compactor:        d.compactor,
		hooks:            append([]hook.AgentHook(nil), d.hooks...),
		inputGates:       append([]inputGateBinding(nil), d.inputGates...),
		toolPreflights:   append([]toolPreflightBinding(nil), d.toolPreflights...),
		toolResultHooks:  append([]toolResultHookBinding(nil), d.toolResultHooks...),
		completionGates:  append([]completionGateBinding(nil), d.completionGates...),
		goalStore:        d.goalStore,
		goalEvaluator:    d.goalEvaluator,
		commitObservers:  append([]session.SessionCommitObserver(nil), d.commitObservers...),
		dispatcher:       dispatcher,
		catalogs:         append([]agentslot.Named[model.ModelCatalog](nil), d.catalogs...),
		config:           cloneAgentRuntimeConfig(d.config),
		observations:     observations,
		workspaceManager: d.workspaceManager,
	}
}

func selectRuntimeTools(installed []agentslot.Named[tool.Tool], keys []string) ([]agentslot.Named[tool.Tool], error) {
	if len(keys) == 0 {
		return nil, nil
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
