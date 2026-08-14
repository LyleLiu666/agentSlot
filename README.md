# AgentSlot

Typed component slots and deterministic composition for agent systems.

> A module unifies registration and lifecycle. A slot defines the component ecosystem, interface, cardinality, and ordering rule.

An agent loop and a tool are not interchangeable plugins. A runnable profile normally needs exactly one selected loop, while it can accept many tools. AgentSlot makes that difference explicit and validates the assembled system before startup.

## Status

AgentSlot is a pre-release foundation. The composition core works and is tested; standard model, tool, session, policy, and presentation interfaces are not frozen yet. Public compatibility starts only with the first tagged release.

The project will not surpass mature harnesses by accumulating interfaces. Its target is smaller and stricter: component ecosystems, cardinality, dependencies, lifecycle, and the final assembled plan must all be explicit, inspectable, and exportable.

## Core model

| Concept | Meaning |
| --- | --- |
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

The product chooses the required profile:

```go
builder := agentslot.NewBuilder()
_ = builder.Install(bundle)

plan, err := builder.Build(
	agentslot.RequireOne(AgentLoopSlot),
	agentslot.RequireMany(ToolSlot, 1),
)
```

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
- Modules start in stable dependency order; independent modules retain installation precedence.
- A failed start stops every previously started module in reverse order.
- Normal shutdown attempts every started module in reverse order and joins their errors.

For module dependencies, `RequireOne` depends on the sole provider, `RequireKey` depends only on the named provider, and `RequireMany` or `RequireChain` depends on every contribution visible in that slot. Product-only profile requirements validate cardinality but do not add lifecycle edges.

Constructors may be retried after a failed build, so they must not start
goroutines, open listeners, or acquire non-repeatable resources. Those effects
belong to module lifecycle methods after a plan has been built successfully.

## Intended standard component families

The following ecosystem classification is the design target. Method-level contracts will be admitted only after independent implementations prove the common semantics.

| Family | Typical cardinality |
| --- | --- |
| Agent driver / loop | one selected implementation |
| Model provider | many keyed providers; explicit selection |
| Tool / skill / command | many keyed implementations |
| Execution environment | one selected world per runtime scope |
| Context and state | scoped stores and ordered context contributors |
| Policy and human interaction | ordered policy contributors plus one final arbiter |
| Subagent and workflow | many providers plus one scheduler per scope |
| Presentation and external protocol | many keyed adapters |

A proposed standard interface must have at least two independent implementations, one real consumer, and a conformance suite. Until then it stays in an adapter or experimental package. This rule prevents a universal API from becoming either a lowest-common-denominator wrapper or a collection of product-specific assumptions.

## Relationship to runtime SDKs

AgentSlot is not another agent runtime SDK:

- Runtime SDKs own loop execution and runtime types. An adapter can expose a runner through an AgentSlot driver slot.
- Domain SDKs for tools, sessions, memory, and gateways own their behavior. They can contribute implementations without being absorbed into this module.
- Frameworks and applications choose a profile, install modules, and own product defaults.

The dependency direction stays one-way: AgentSlot core imports no product or SDK. Adapters import AgentSlot and the SDK they connect.

## Development

```sh
gofmt -w .
go test -race ./...
go vet ./...
```

Read [docs/architecture.md](docs/architecture.md) before changing the public composition model.

## License

Licensed under the [Apache License 2.0](LICENSE).
