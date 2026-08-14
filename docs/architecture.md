# Architecture

## Purpose

AgentSlot defines how independently developed agent components are registered, validated, resolved, started, and stopped. It does not define how an agent reasons, calls a model, executes a tool, stores a session, or renders a UI.

The central distinction is:

- A **module** owns registration and lifecycle.
- A **slot** owns one component interface and its composition rules.

One module may provide a model provider, several tools, and a hook. Those contributions remain members of different ecosystems even though one module owns their cleanup.

## Composition states

1. `Builder` is mutable and accepts modules.
2. Each `Module.Register` call works against an isolated registry copy.
3. `Build` validates profile requirements and every module's optional `RequiredSlots` declaration.
4. Slot providers create dependency edges; a stable topological sort rejects cycles before construction or startup.
5. Deferred contributions are constructed in dependency order through a resolver limited to the owning module's declared requirements.
6. Successful build returns an immutable `Plan` and freezes the builder.
7. `Plan.Start` creates one `Runtime` and starts lifecycle-aware modules in dependency order.
8. `Runtime.Stop` releases them in reverse order.

A failed module registration never leaks partial contributions. A failed
build, including constructor failure, does not freeze the builder or publish a
partially materialized plan. Constructors can therefore run again on a later
build attempt and must remain free of lifecycle side effects. A failed start
rolls back only modules whose `Start` completed successfully.

The current scope unit is one plan. A product creates a separate builder and plan for each independently owned runtime or session. Hierarchical inheritance is deferred until real products prove that separate plans plus explicit parent inputs are insufficient.

## Slot kinds

### One

`One[T]` allows zero or one value. Optionality and uniqueness are separate rules: the slot enforces uniqueness, while the selected product profile decides whether the value is required.

Use it for a selected agent loop, policy arbiter, scheduler, or execution environment.

### Many

`Many[T]` allows multiple values with unique stable keys. Registration order is deterministic, but consumers select by key unless their interface specifies another rule.

Use it for tools, model providers, protocol adapters, and named stores.

`RequireKey` selects one named provider. `RequireMany` means the consumer observes the whole registry, so its lifecycle follows every current provider.

### Chain

`Chain[T]` allows ordered values. Installation order is semantic and is preserved by the plan.

Use it only when every contributor participates in an explicit pipeline, such as hooks, policy checks, or prompt contributors. Do not use it as an unordered registry.

## Dependency rules

Modules optionally implement `SlotRequirer`. Requirements name slots, not provider modules:

- `RequireOne` adds an edge from the sole provider.
- `RequireKey` adds an edge from the selected keyed provider.
- `RequireMany` and `RequireChain` add edges from every registered contributor.
- A module's contribution can satisfy its own requirement without creating a self-edge.

Independent modules retain installation precedence. Missing providers, invalid requirements, and cycles fail during `Build`; no lifecycle method has run at that point.

Requirements do not silently inject fields. Ordinary Go constructors can
still receive dependencies explicitly. When the provider module may be chosen
only after installation, `SetWith`, `AddWith`, and `AppendWith` register an
explicit build-time constructor. Its `Resolver` can resolve only dependencies
listed by that module's `RequiredSlots`, and only for the duration of the
constructor call. This keeps construction auditable and prevents the resolver
from becoming a hidden runtime service locator.

Construction follows the same stable topological order used by lifecycle.
Each dependency contribution must be fully materialized before a consumer can
resolve it. Constructor functions prepare component values only; goroutines,
listeners, locks, and other non-repeatable resources belong to `Start` and
`Stop` because a failed build may retry construction.

## Plan description

`Plan.Describe()` returns the versioned `agentslot.plan/v0` format. It lists modules in lifecycle start order and slots in lexical ID order. Contributions contain only module ownership and optional keys. Component values and configuration are intentionally absent so the description can be logged or exported without serializing implementations or leaking credentials.

## Package layers

The intended dependency direction is:

```text
products and profiles
        |
adapters and implementations
        |
standard component contracts
        |
AgentSlot composition core
```

The core must remain usable without an LLM SDK, tool SDK, database, UI framework, or wire protocol.

Standard component contracts will be introduced in leaf packages after real adapters establish common behavior. An adapter may depend on both the contract package and an external SDK; the core never imports the adapter.

## Interface admission rule

A method-level component contract becomes standard only when all of the following exist:

1. Two independent implementations that are not wrappers around the same implementation.
2. One assembled application that consumes either implementation without branching on its concrete type.
3. A conformance suite covering behavior, failures, cancellation, and lifecycle ownership.
4. Evidence that the contract does not discard semantics required by either implementation.

This keeps unstable provider and product details out of the universal layer.

## Stable release gate

The composition API is ready for a stable release only after:

1. At least two independent SDK ecosystems declare real slots over their existing interfaces.
2. One assembled product can exchange implementations through those slots without branching on concrete provider types.
3. Shared conformance tests verify registration, requirements, lifecycle, and exported plan descriptions.
4. The evidence either proves that one plan per scope is sufficient or justifies a minimal parent-plan mechanism.

Until those proofs exist, keep domain contracts outside the core and keep the plan schema at `v0`.

## Deferred capabilities

The first foundation intentionally defers:

- hierarchical scopes and per-session generations;
- configuration schemas and secret resolution;
- out-of-process discovery or loading;
- standard model, tool, state, policy, and presentation method contracts.

These are added only when a real adapter needs them. Dependency order controls lifecycle; registration order continues to control `Many` enumeration and `Chain` semantics.
