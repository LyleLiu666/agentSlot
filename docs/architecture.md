# Architecture

## Purpose

AgentSlot defines both the standard map of independently replaceable agent
components and the generic protocol by which those components are registered,
validated, resolved, started, and stopped. The composition core does not
implement how an agent reasons, calls a model, executes a tool, stores history,
or renders a UI. AgentSlot-owned leaf contract packages standardize those
boundaries only as their portability is proven.

The authoritative inventory of boundaries, Slot IDs, cardinality, profile
requirements, and maturity is the [Standard Component Map](../COMPONENT_MAP.md).

The central distinction is:

- An **application** is the root host that selects a profile and mounts modules.
- A **module** owns registration and lifecycle.
- A **slot** owns one component interface and its composition rules.

One module may provide a model provider, several tools, and a hook. Those contributions remain members of different ecosystems even though one module owns their cleanup.

## Composition states

1. `Application` declares one name, module list, and profile. `Build`
   automatically mounts that list into its internal mutable `Builder`.
2. Each `Module.Register` call works against an isolated registry copy.
3. `Build` validates profile requirements and every module's optional `RequiredSlots` declaration.
4. Slot providers create dependency edges; a stable topological sort rejects cycles before construction or startup.
5. Deferred contributions are constructed in dependency order through a resolver limited to the owning module's declared requirements.
6. Successful build returns an immutable `Plan` and freezes the builder.
7. `Application.Start` builds when needed, then delegates to `Plan.Start`.
   `Plan.Start` creates one `Runtime` and starts lifecycle-aware modules in dependency order.
8. `Runtime.Stop` releases them in reverse order.

A failed module registration never leaks partial contributions. A failed
build, including constructor failure, does not freeze the builder or publish a
partially materialized plan. Constructors can therefore run again on a later
build attempt and must remain free of lifecycle side effects. A failed start
rolls back only modules whose `Start` completed successfully.

An Application Plan is the application scope, not a Session scope. One plan can
serve many Workspaces and Sessions. The `agent.loop` Slot installs an
`AgentLoopFactory`; after a SessionManager opens a Session, the Factory creates
one isolated Loop for that Session. Hierarchical Workspace or Session
inheritance is still deferred, but it must not be modelled as repeated Plans.

The Plan owns application-level lifecycle components such as the Factory,
Provider adapters, shared Gateway, and SessionStore services. A per-Session
Loop is a runtime child created after `Plan.Start`; closing that child releases
its in-memory execution state without deleting the Session or its durable
views.

## Session-scoped runtime

The runtime boundary below the Plan is explicit:

1. `SessionManager` creates or opens a stable Session and performs recovery.
2. `AgentLoopFactory.Open` receives the already-open Session and an immutable
   AgentDefinition snapshot.
3. The resulting Loop owns exactly one Session and at most one active Run.
4. Different Sessions receive different Loop objects and may run concurrently.
5. History, Context, Queue, and RunJournal are Session views coordinated by an
   atomic SessionStore transaction; the Loop does not create or fork Sessions.
6. Gateway is shared by the Application and routes commands/events using
   `AgentID + WorkspaceID + SessionID + RunID`, without exposing Loop objects.

## Application host

`Application` is deliberately a thin owner of module selection, build, and
startup. It does not add another registry beside the plan:

- `NewApplication` copies the named product's complete module list and profile.
- `Build` automatically installs every declared module and all of its contributions.
- `Build` is idempotent after success and returns the same immutable plan.
- `Start` automatically builds, then starts the plan once.
- `Run` treats context cancellation as a normal shutdown request.
- `Runtime.Plan` exposes the exact plan owned by the running application.

There is no package scan, global self-registration, hidden field injection, or
runtime service locator. Every product uses the same `Build`, `Start`, and
`Stop` entry points; only its name, declared modules, configuration, and profile
differ. Automatic mounting means both that `Build` installs the declared module
list and that explicit build-time constructors receive the contributions named
by `RequiredSlots`. Importing a package alone never changes the application.
This keeps the host convenient while preserving a complete, inspectable
assembly before any lifecycle side effect begins.

## Slot kinds

### One

`One[T]` allows zero or one value. Optionality and uniqueness are separate rules: the slot enforces uniqueness, while the selected product profile decides whether the value is required.

Use it for a selected agent loop factory, policy arbiter, scheduler, or execution environment.

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

Standard component contracts are introduced in AgentSlot-owned leaf packages
after real implementations establish common behavior. An adapter may depend on
both the contract package and an external SDK; the core never imports the
adapter. Previous-generation SDK interfaces provide evidence and compatibility
paths, but they do not replace the standard component map.

## Standardization boundary

AgentSlot deliberately fixes a small semantic vocabulary when all of the
following are true:

1. the concept has the same meaning across independent implementations;
2. adapters cannot interoperate if every implementation names or shapes it
   differently;
3. the useful set is finite and changes much more slowly than providers or
   products;
4. a future addition can be made as an explicit standard revision.

Model media modality is one such boundary: `text`, `image`, and `audio` are
standard values, while provider wire blocks and model IDs are not. Tool input
is another: JSON Schema Draft 2020-12 is standard, while provider-specific
keyword and size limits are not.

Replaceable behavior, policy, storage, transport, and provider implementation
remain Slot components. Stable semantics are not made configurable merely to
avoid making an architectural decision; volatile implementation details are
not frozen merely because one SDK already chose a representation.

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
4. The evidence proves that one application plan can safely serve multiple
   Session-scoped Loop generations, or justifies a minimal parent-plan
   mechanism without duplicating application components.

Until those proofs exist, keep domain contracts outside the core and keep the plan schema at `v0`.

## Current implementation frontier

The published composition foundation currently defers implementation of:

- the Session-scoped runtime objects and their standard domain method contracts;
- configuration schemas and secret resolution;
- out-of-process discovery or loading;
- the mapped standard domain method contracts and their conformance suites.

These are implemented in the order and maturity process defined by the
[Standard Component Map](../COMPONENT_MAP.md). Dependency order controls
lifecycle; registration order continues to control `Many` enumeration and
`Chain` semantics.
