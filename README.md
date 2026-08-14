# AgentSlot

Typed component slots and deterministic composition for agent systems.

> **Start with the AgentSlot Standard Component Map:**
> [English](COMPONENT_MAP.md) | [简体中文](COMPONENT_MAP.zh-CN.md). It is the
> authoritative inventory of customizable agent components, their Slot IDs,
> cardinality, profile requirements, and implementation maturity.

> A module unifies registration and lifecycle. A slot defines the component ecosystem, interface, cardinality, and ordering rule.

An agent loop and a tool are not interchangeable plugins. A runnable profile normally needs exactly one selected loop, while it can accept many tools. AgentSlot makes that difference explicit and validates the assembled system before startup.

## Status

AgentSlot is a pre-1.0 foundation. Tagged releases are consumable through Go
modules. The composition core works today; the standard domain interface map is
normative, while its method-level contracts are being admitted in evidence-led
stages. Every published tag is immutable; compatible fixes and additions
receive a new semantic version.

The project's architectural result is the quality of its component map, not a
large interface count. Each accepted ecosystem must have a clear boundary,
cardinality, dependency model, lifecycle, conformance suite, and inspectable
place in the final assembled plan.

## Core model

| Concept | Meaning |
| --- | --- |
| `Application` | Named root host that automatically mounts its module list and provides standard build, start, and run entry points. |
| `Module` | Registration and lifecycle owner. A module may contribute to several slots. |
| `One[T]` | Zero or one implementation. A profile can require exactly one with `RequireOne`. |
| `Many[T]` | Zero or more implementations with unique keys, such as tools or model providers. |
| `Chain[T]` | Zero or more ordered contributors, such as hooks or prompt contributors. |
| `Requirement` | A profile constraint or a module dependency expressed against a slot, never a concrete provider type. |
| `Plan` | Immutable result of validated composition. |
| `PlanDescription` | Versioned JSON-safe assembly description containing no component values. |
| `Runtime` | Started module lifecycles owned by one plan. |

The generic `Module` interface is deliberately not the component interface. The slot's `T` is the component interface:

```go
type AgentLoop interface {
	Run(context.Context, string) (string, error)
}

type Tool interface {
	Name() string
}

var (
	AgentLoopSlot = agentslot.One[AgentLoop]("agent.loop")
	ToolSlot      = agentslot.Many[Tool]("tool")
)
```

These declarations are application-local examples. The maturity of
AgentSlot-owned standard domain contracts is tracked separately in the
[component map](COMPONENT_MAP.md).

Modules then contribute implementations:

```go
func (m Bundle) Register(r agentslot.Registrar) error {
	return r.Contribute(
		agentslot.Set(AgentLoopSlot, m.loop),
		agentslot.Add(ToolSlot, "shell", m.shell),
		agentslot.Add(ToolSlot, "files", m.files),
	)
}
```

When one contribution must be constructed from other installed components,
use a build-time constructor instead of manually assembling it before
`Build`:

```go
func (m RunnerModule) Register(r agentslot.Registrar) error {
	return r.Contribute(agentslot.SetWith(AgentLoopSlot, func(resolver agentslot.Resolver) (AgentLoop, error) {
		model, err := agentslot.ResolveKey(resolver, ModelSlot, m.modelKey)
		if err != nil {
			return nil, err
		}
		tools, err := agentslot.ResolveMany(resolver, ToolSlot)
		if err != nil {
			return nil, err
		}
		return NewAgentLoop(model, tools), nil
	}))
}

func (m RunnerModule) RequiredSlots() []agentslot.Requirement {
	return []agentslot.Requirement{
		agentslot.RequireKey(ModelSlot, m.modelKey),
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
		runnerModule,
		modelModule,
		shellToolModule,
	},
	agentslot.RequireOne(AgentLoopSlot),
	agentslot.RequireMany(ToolSlot, 1),
)

plan, err := application.Build()
runtime, err := application.Start(ctx)
defer runtime.Stop(shutdownCtx)

loop, _ := agentslot.Get(plan, AgentLoopSlot)
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
		agentslot.RequireOne(AgentLoopSlot),
		agentslot.RequireKey(ToolSlot, "shell"),
	}
}
```

`Build` validates these requirements, rejects dependency cycles, constructs
deferred contributions in dependency order, and computes a stable lifecycle
order. `plan.Describe()` exposes that order, slot kinds, Go value types,
contribution owners, keys, module requirements, and profile requirements
without serializing component values.

See the complete runnable [basic example](examples/basic/main.go).

## Lifecycle guarantees

- One module registration is transactional: one rejected contribution discards all contributions from that module.
- A successful build freezes the builder; a failed build can be corrected and retried.
- Build-time constructors resolve only declared slot dependencies; failed
  construction does not publish a plan or freeze the builder.
- `Application.Start` builds automatically when `Build` was not called explicitly.
- Modules start in stable dependency order; independent modules retain installation precedence.
- A failed start stops every previously started module in reverse order.
- Normal shutdown attempts every started module in reverse order and joins their errors.

For module dependencies, `RequireOne` depends on the sole provider, `RequireKey` depends only on the named provider, and `RequireMany` or `RequireChain` depends on every contribution visible in that slot. Product-only profile requirements validate cardinality but do not add lifecycle edges.

Constructors may be retried after a failed build, so they must not start
goroutines, open listeners, or acquire non-repeatable resources. Those effects
belong to module lifecycle methods after a plan has been built successfully.

## Standard component map

The [AgentSlot Standard Component Map](COMPONENT_MAP.md) is the source of truth
for the agent architecture. It currently maps runtime, model, tools, context,
history, memory, execution, policy, multi-agent workflow, gateway, billing, and
operations ecosystems.

A runnable standard LLM agent requires exactly one agent loop, exactly one
session manager, at least one model provider, and at least one interaction
entrypoint. Tools and persistent history are optional globally; stricter
profiles may require them.

Mapping a Slot and standardizing its Go method contract are different maturity
steps. A proposed method-level interface needs two independent implementations,
one branch-free real consumer, and a conformance suite before it is described
as proven portable. The map records this evidence instead of hiding unfinished
coverage behind a family name.

The first fixed domain vocabularies are available in the [`model`](model) and
[`tool`](tool) leaf packages: model input/output modalities are text, image, and
audio; model-facing tool inputs use self-contained JSON Schema Draft 2020-12.
These stable value types do not imply that the complete `ModelProvider` or
`Tool` component interfaces have reached contracted maturity.

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

Read [docs/architecture.md](docs/architecture.md) before changing the public composition model.

## License

Licensed under the [Apache License 2.0](LICENSE).
