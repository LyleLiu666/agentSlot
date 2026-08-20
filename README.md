# AgentSlot

Typed component slots and deterministic composition for agent systems.

> **Start with the AgentSlot Standard Component Map:**
> [English](COMPONENT_MAP.md) | [简体中文](COMPONENT_MAP.zh-CN.md). It is the
> authoritative inventory of customizable agent components, their Slot IDs,
> cardinality, profile requirements, and implementation maturity.

> **Implementation roadmap:**
> [AgentSlot 组件接口标准化路线图（中文）](ROADMAP.zh-CN.md). It defines the
> business outcomes, admission rules, reference-agent layers, implementation
> order, and release gate for turning mapped responsibilities into proven
> component ecosystems.

> **Agent runtime decisions and implementation design:**
> [Agent 设计的架构讨论](docs/agent-architecture-discussion.zh-CN.md) |
> [Agent 框架全景架构](docs/agent-framework-architecture.zh-CN.md) |
> [AgentRuntime 与标准 Slot 实施计划](docs/agent-runtime-standard-slots-implementation-plan.zh-CN.md).

> A module unifies registration and lifecycle. A slot defines the component ecosystem, interface, cardinality, and ordering rule.

The standard AgentRuntime and its model/tool loop are framework behavior, not a
replaceable Slot. The replaceable parts are narrower: one SessionManager, one
SessionStore, one ModelExecutor, zero or more Tools, ordered Context and Hook
components, one or more Entrypoints, and optional keyed InteractionCommands.
AgentSlot makes those cardinalities explicit and validates the assembled system
before startup.

The generic core remains product-neutral. A standard LLM Agent explicitly uses
`standardagent.NewApplication`, which returns the same `*agentslot.Application`
and automatically mounts the fixed AgentRuntime/Gateway layer and standard Agent
profile. It does not infer a profile from installed slots. All standard Agent
projects therefore keep the same `Build`, `Start`, `Run`, and `Runtime.Stop`
entry points; only the declared modules, configuration, and application name
vary.

## Status

AgentSlot is a pre-1.0 foundation. Tagged releases are consumable through Go
modules. The composition core works today; the standard domain interface map is
normative, while its method-level contracts are being admitted in evidence-led
stages. Every published tag is immutable; compatible fixes and additions
receive a new semantic version.

The project's architectural result is the quality of its component map, not a
large interface count. Each accepted ecosystem must have a clear boundary,
cardinality, dependency model, lifecycle, conformance suite, and inspectable
place in the final Assembly.

## Core model

| Concept | Meaning |
| --- | --- |
| `Application` | Named root host that automatically mounts its module list and provides standard build, start, and run entry points. |
| `Module` | Registration and lifecycle owner. A module may contribute to several slots. |
| `One[T]` | Zero or one implementation. A profile can require exactly one with `RequireOne`. |
| `Many[T]` | Zero or more implementations with unique keys, such as tools or model providers. |
| `Chain[T]` | Zero or more ordered contributors, such as hooks or prompt contributors. |
| `Requirement` | A profile constraint or a module dependency expressed against a slot, never a concrete provider type. |
| `Assembly` | Immutable result of validated composition. `Application.Build()` returns this shared application-level assembly. |
| `AssemblyDescription` | Versioned JSON-safe assembly description returned by `Assembly.Describe()`. |
| `Runtime` | Started module lifecycles owned by one Assembly; `standardagent` attaches its fixed Gateway and Runtime registry to this lifecycle. |
| `AgentRuntime` | Framework-owned per-Session command and execution object created by explicit Session create/resume; it is not a Slot. |

The code examples use `assembly`, `Assembly`, `AssemblyDescription`, and
`Runtime.Assembly()`. No compatibility aliases for the removed `Plan` names are
provided before 1.0.

The generic `Module` interface is deliberately not the component interface. The slot's `T` is the component interface:

```go
type TextGenerator interface {
	Generate(context.Context, string) (string, error)
}

type Tool interface {
	Name() string
}

type ToolCatalog interface {
	Keys() []string
}

var (
	GeneratorSlot = agentslot.One[TextGenerator]("example.text-generator")
	ToolSlot      = agentslot.Many[Tool]("example.tool")
	CatalogSlot   = agentslot.One[ToolCatalog]("example.tool-catalog")
)
```

These are deliberately application-local examples of the generic composition
API, not AgentSlot-owned standard domain contracts. The standard Slot IDs and
their maturity are tracked only in the [component map](COMPONENT_MAP.md).

Modules then contribute implementations:

```go
func (m Bundle) Register(r agentslot.Registrar) error {
	return r.Contribute(
		agentslot.Set(GeneratorSlot, m.generator),
		agentslot.Add(ToolSlot, "shell", m.shell),
		agentslot.Add(ToolSlot, "files", m.files),
	)
}
```

When one contribution must be constructed from other installed components,
use a build-time constructor instead of manually assembling it before
`Build`:

```go
func (m CatalogModule) Register(r agentslot.Registrar) error {
	return r.Contribute(agentslot.SetWith(CatalogSlot, func(resolver agentslot.Resolver) (ToolCatalog, error) {
		tools, err := agentslot.ResolveMany(resolver, ToolSlot)
		if err != nil {
			return nil, err
		}
		return NewToolCatalog(tools), nil
	}))
}

func (m CatalogModule) RequiredSlots() []agentslot.Requirement {
	return []agentslot.Requirement{
		agentslot.RequireMany(ToolSlot, 1),
	}
}
```

`SetWith`, `AddWith`, and `AppendWith` run only after requirements and cycles
have been validated. Their resolver can read only the owning module's declared
requirements and closes when the constructor returns. Constructors prepare
components; lifecycle resources still belong in `Start` and `Stop`.

Every product declares its name, module list, and required profile through the
same application entry:

```go
application := agentslot.NewApplication(
	"my-agent",
	[]agentslot.Module{
		catalogModule,
		generatorModule,
		shellToolModule,
	},
	agentslot.RequireOne(GeneratorSlot),
	agentslot.RequireOne(CatalogSlot),
	agentslot.RequireMany(ToolSlot, 1),
)

assembly, err := application.Build()
runtime, err := application.Start(ctx)
defer runtime.Stop(shutdownCtx)

catalog, _ := agentslot.Get(assembly, CatalogSlot)
_ = catalog
```

`Application.Start` also calls `Build` automatically when the caller does not
need a separate inspection step. `Application.Run` starts a service-style
application, waits for context cancellation, and shuts it down. Every product
therefore uses the same `Build`, `Start`, and `Stop` control flow; only its name,
modules, configuration, and profile differ. `Builder` remains the lower-level
composition primitive used by the application host.

The module list is explicit, but mounting it is automatic during `Build`.
AgentSlot does not scan packages or use `init()` side effects. Declared
constructors automatically receive the slot contributions they depend on, so a
tool module can join the selected loop without a product-level bootstrap
function wiring the concrete tool into the concrete loop.

A module can declare dependencies without naming their implementation modules:

```go
func (m RunnerModule) RequiredSlots() []agentslot.Requirement {
	return []agentslot.Requirement{
		agentslot.RequireOne(GeneratorSlot),
		agentslot.RequireKey(ToolSlot, "shell"),
	}
}
```

`Build` validates these requirements, rejects dependency cycles, constructs
deferred contributions in dependency order, and computes a stable lifecycle
order. `assembly.Describe()` exposes that order, slot kinds, Go value types,
contribution owners, keys, module requirements, and profile requirements
without serializing component values.

Optional component ecosystems are still declared dependencies rather than
looked up dynamically. `OptionalOne`, `OptionalMany`, and `OptionalChain` add
lifecycle edges whenever providers are installed, while allowing a build with
no provider. Constructors resolve them with `ResolveOptionalOne`,
`ResolveMany`, or `ResolveChain`; the build-scoped resolver remains closed
outside construction.

See the complete runnable [basic example](examples/basic/main.go).

## Lifecycle guarantees

- One module registration is transactional: one rejected contribution discards all contributions from that module.
- A successful build freezes the builder; a failed build can be corrected and retried.
- Build-time constructors resolve only declared slot dependencies; failed
  construction does not publish an Assembly or freeze the builder.
- `Application.Start` builds automatically when `Build` was not called explicitly.
- Modules start in stable dependency order; independent modules retain installation precedence.
- A failed start stops every previously started module in reverse order.
- Normal shutdown attempts every started module in reverse order and joins their errors.

For module dependencies, `RequireOne` depends on the sole provider, `RequireKey` depends only on the named provider, and `RequireMany` or `RequireChain` depends on every contribution visible in that slot. Product-only profile requirements validate cardinality but do not add lifecycle edges.

Constructors may be retried after a failed build, so they must not start
goroutines, open listeners, or acquire non-repeatable resources. Those effects
belong to module lifecycle methods after an Assembly has been built successfully.

## Standard component map

The [AgentSlot Standard Component Map](COMPONENT_MAP.md) is the source of truth
for the agent architecture. It currently maps runtime, model, tools, context,
history, memory, execution, policy, multi-agent workflow, gateway, billing, and
operations ecosystems.

A runnable standard LLM agent requires exactly one SessionManager, exactly one
SessionStore, exactly one ModelExecutor, and at least one Entrypoint. The fixed
AgentRuntime and fixed in-process Gateway are supplied by the framework rather
than selected through Slots.
Standard applications install each Entrypoint with
`standardagent.NewEntrypointModule`; Build rejects a raw Entrypoint contribution
that would bypass GatewayAccess attachment or lifecycle ordering.
Tools, ModelProviders, Context components, and AgentHooks are optional globally;
an installed ModelExecutor may explicitly require one or more ModelProviders.
`interaction.command` is an optional `Many` Slot for structured commands that
register only with the Gateway. Entrypoints render the Gateway's UI-neutral
command directory as slash commands, menus, buttons, forms, or command palettes.
`Application.Start` creates a started application Runtime that owns one
process-local Session-to-AgentRuntime registry. Every created or resumed
AgentRuntime registered by that application lives in the same process; durable
Sessions that have not been opened do not occupy a Runtime. A framework-internal
Runtime coordinator operates the registry; only the Gateway receives private
Runtime access. Every Entrypoint receives the same carrier-neutral Gateway
access and cannot obtain AgentRuntime pointers. The Gateway, registry,
coordinator, and private assembly anchors are framework mechanics, not public
Slots. This single-process
ownership boundary is a deliberate part of the standard architecture, not a
first-version limitation.

One Application Assembly serves multiple Workspaces and Sessions. It does not
build a separate Assembly per Session. Explicit create/resume initializes one
AgentRuntime for that Session; listing Sessions does not. History, Context,
Queue, RunJournal, and SessionModelConfig are durable state inside the
SessionStore aggregate. Runtime-fixed SystemPrompt, ToolKeys, and Context
settings do not change during one Runtime lifetime; the Session's provider,
model, reasoning, and model parameters can be changed explicitly while idle
and are snapshotted for each Run.

The `session` package includes a reference `MemoryStore` and `MemoryManager`.
They implement append-only History facts, Context, Queue, RunJournal,
SessionModelConfig inheritance, Run lifecycle facts, revision/CAS commits, and
the basic crash recovery transition. These are development and
contract-reference implementations, not a production database; production
storage remains a replaceable `session.store` Slot. Development
applications can explicitly install `session.NewMemoryModule`; importing the
package never selects it as a hidden default.

Mapping a Slot and standardizing its Go method contract are different maturity
steps. A proposed method-level interface needs two independent implementations,
one branch-free real consumer, and a conformance suite before it is described
as proven portable. The map records this evidence instead of hiding unfinished
coverage behind a family name.

The first fixed domain vocabularies are available in the [`model`](model) and
[`tool`](tool) leaf packages: model input/output modalities are text, image, and
audio; model-facing tool inputs use self-contained JSON Schema Draft 2020-12.
Caller and Hook input uses `agent.MessageInput`, which carries content only;
the fixed Runtime allocates MessageID, Session/Run/Step containment, role, and
timestamp atomically when it creates a durable `agent.Message` fact.
Ten standard component contracts are now available in the
`session`, `model`, `tool`, `context`, `hook`, and `interaction` packages. They
are Contracted, but no domain ecosystem is yet Conformant or Proven.

The `standardagent` package implements the application-scoped Runtime registry,
coordinator, fixed Gateway, GatewayAccess binding, Entrypoint wrapper, automatic
standard profile, and the fixed Session AgentRuntime state machine. Send,
SendAndWait, Steer, RunPending, Cancel, WhenIdle, Close, Queue mutation, and
model-config commands now execute through the Gateway. `Subscribe` publishes
live delta/reset events with Run, Step, and physical Attempt identity plus
durable commit/state notifications; temporary output and client cursors are not
persisted. A subscriber first obtains the current Snapshot revision, and a slow
subscriber is closed explicitly so it can reconnect instead of growing an
unbounded in-memory queue. Disconnecting a subscriber never cancels its Run.
`SendAndWait` wraps the same Run and returns only that Run's durable assistant
text messages rather than executing a second model path. Run start/terminal
facts retain the frozen model configuration. The
`model` package includes an explicitly installed deterministic
`FakeModelExecutor` for development and contract tests. The fixed Runtime also
owns ToolDispatcher semantics: call/pending and result/terminal commits,
Serial/ParallelSafe batches, safe structured failures, and mandatory model
continuation. [`tool/bash`](tool/bash) is the first explicitly installed
built-in Tool; it fixes working directory, explicit environment, timeout,
process-group cancellation, and separate output limits. It is never installed
by import or by `standardagent.NewApplication`. The Runtime now builds and
persists versioned Context projections, runs ordered ContextSource and Hook
chains, enforces model protocol and hard token limits, projects unsupported
attachments without rewriting History, and validates idle-only model switches
through ModelExecutor capabilities. `model.catalog` has a typed contract and
an explicit StaticCatalog reference implementation. Applications can
explicitly install `interaction.NewModelCommandModule`; slash, menu, and
structured clients then invoke that one Gateway command backend. The
`interaction/inprocess` package provides the first function-style Entrypoint
and exposes only GatewayAccess. These reference implementations do not make the
corresponding Slots Conformant or Proven, and no Web/RPC transport or reliable
delivery ACK is implied. Importing any package still has no registration or
startup side effect.

## Relationship to previous-generation SDKs

AgentSlot defines the portable component boundaries and, as evidence permits,
the standard domain contracts. Existing runtime, tool, session, memory, and
gateway SDKs remain valuable implementation sources and compatibility targets:

- an SDK can expose several independent AgentSlot components while preserving
  its existing assembly API;
- current products can keep iterating while additive adapters support new
  AgentSlot assemblies;
- provider- and product-specific behavior stays in the SDK or adapter rather
  than leaking into the generic composition core.

The core imports no product or SDK. Adapters depend on AgentSlot and the SDK
they connect; applications select a profile, modules, configuration, and
defaults.

## Development

```sh
gofmt -w .
go test -race ./...
go vet ./...
```

Read [docs/architecture.md](docs/architecture.md) and the [Agent framework panorama](docs/agent-framework-architecture.zh-CN.md) before changing the public composition model.

## License

Licensed under the [Apache License 2.0](LICENSE).
