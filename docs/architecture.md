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
3. `Build` validates profile requirements.
4. Successful build returns an immutable `Plan` and freezes the builder.
5. `Plan.Start` creates one `Runtime` and starts lifecycle-aware modules.
6. `Runtime.Stop` releases them in reverse order.

A failed module registration never leaks partial contributions. A failed build does not freeze the builder. A failed start rolls back only modules whose `Start` completed successfully.

## Slot kinds

### One

`One[T]` allows zero or one value. Optionality and uniqueness are separate rules: the slot enforces uniqueness, while the selected product profile decides whether the value is required.

Use it for a selected agent loop, policy arbiter, scheduler, or execution environment.

### Many

`Many[T]` allows multiple values with unique stable keys. Registration order is deterministic, but consumers select by key unless their interface specifies another rule.

Use it for tools, model providers, protocol adapters, and named stores.

### Chain

`Chain[T]` allows ordered values. Installation order is semantic and is preserved by the plan.

Use it only when every contributor participates in an explicit pipeline, such as hooks, policy checks, or prompt contributors. Do not use it as an unordered registry.

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

## Deferred capabilities

The first foundation intentionally defers:

- hierarchical scopes and per-session generations;
- dependency graphs and automatic topological ordering;
- configuration schemas and secret resolution;
- out-of-process discovery or loading;
- standard model, tool, state, policy, and presentation method contracts;
- machine-readable plan export.

These are added only when a real adapter needs them. Installation order is the current explicit lifecycle order.
