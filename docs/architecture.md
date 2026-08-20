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
This document defines the public ownership model, composition core, fixed
Gateway and AgentRuntime boundary, and package dependency direction.

The central distinction is:

- An **application** is the root host that selects a profile and mounts modules.
- A **module** owns registration and lifecycle.
- A **slot** owns one component interface and its composition rules.

One module may provide a model provider, several tools, and a hook. Those contributions remain members of different ecosystems even though one module owns their cleanup.

The generic core stays product-neutral. A standard LLM Agent explicitly enters
through the separate `standardagent.NewApplication` package. That constructor
returns the same `*agentslot.Application` used by the generic core, while
automatically mounting the fixed AgentRuntime/Gateway module and standard Agent
profile. It does not infer an Agent profile from installed slots, and importing
the package has no registration side effect. Every standard Agent therefore
keeps the same `Build`, `Start`, `Run`, and `Runtime.Stop` lifecycle.

## Composition states

1. `Application` declares one name, module list, and profile. `Build`
   automatically mounts that list into its internal mutable `Builder`.
2. Each `Module.Register` call works against an isolated registry copy.
3. `Build` validates profile requirements and every module's `RequiredSlots`
   declaration, including explicitly optional component dependencies.
4. Slot providers create dependency edges; a stable topological sort rejects cycles before construction or startup.
5. Deferred contributions are constructed in dependency order through a resolver limited to the owning module's declared requirements.
6. Successful build returns an immutable `Assembly` and freezes the builder.
7. `Application.Start` builds when needed, then delegates to `Assembly.Start`.
   `Assembly.Start` creates one `Runtime` and starts lifecycle-aware modules in dependency order.
8. `Runtime.Stop` releases them in reverse order.

The Go implementation exposes this object as `Assembly`, with
`AssemblyDescription` and the `agentslot.assembly/v0` schema. The old `Plan`
names are not compatibility aliases; `Plan` conflicts with agent task planning.

A failed module registration never leaks partial contributions. A failed
build, including constructor failure, does not freeze the builder or publish a
partially materialized Assembly. Constructors can therefore run again on a later
build attempt and must remain free of lifecycle side effects. A failed start
rolls back only modules whose `Start` completed successfully.

An Application Assembly is the application scope, not a Session scope. One Assembly can
serve many Workspaces and Sessions. It owns the fixed SessionManager and shared
components such as SessionStore, ModelExecutor, optional Provider adapters, Tools,
Context components, Hooks, InteractionCommands, and GatewayChannels. When the Assembly starts, the resulting application Runtime owns one
process-local Session-to-AgentRuntime registry and one fixed in-process Gateway.
A framework-internal Runtime coordinator operates the registry; the Gateway is
the sole user-interaction backend. None of these three framework objects is a
replaceable component ecosystem. Hierarchical Workspace or Session inheritance
is still deferred, but it must not be modelled as repeated Assemblies.

`AgentRuntime` is fixed framework behavior below the started Assembly, not a Slot.
Explicitly creating or resuming a Session initializes one Runtime bound to that
Session; listing or viewing Sessions does not. The Runtime remains resident
while idle and is released only by explicit Close or application shutdown.
Closing it never deletes the Session or its durable views.

## Session-scoped runtime

The runtime boundary below the Assembly is explicit:

1. The fixed framework `SessionManager` creates, resumes, fully forks, or
   summary-starts a stable Session and performs recovery through the replaceable
   `SessionStore`. It is constructed from the Store, the Application default
   model configuration, and an internal ID generator; it is not a Slot.
2. The Session aggregate durably owns History, Context, Queue, RunJournal, and
   SessionModelConfig together with revision/CAS transaction state.
   `SessionStore.Recover` is the explicit resume-time crash boundary; ordinary
   `Load` is read-only and cannot terminate a legitimately active Run.
3. Successful `CreateSession` or `ResumeSession` initializes one AgentRuntime
   with an immutable AgentRuntimeConfig snapshot and the components selected by
   Assembly. Create initializes SessionModelConfig from the Agent default; resume
   restores the Session's persisted provider, model, reasoning, and parameters.
4. All AgentRuntimes registered by one started application Runtime live in that
   same process. The same SessionID has one Runtime in its registry; concurrent
   resume calls converge on it. Persisted Sessions that have not been created or
   resumed in this process do not occupy a Runtime. The Session has at most one
   active Run, while different Sessions may run concurrently.
5. Runtime commands are `Send`, `Steer`, `RunPending`, Queue mutations,
   `ModelConfig`, `UpdateModelConfig`, `Cancel`, `WhenIdle`, queries, and
   `Close`. Model configuration can change only while idle and is snapshotted
   for each Run. Resume only means restoring a Session; it is not an execution
   command.
   `Send`, `Steer`, Queue edits, summary starts, and Hook follow-on proposals
   carry identity-free `MessageInput`; only the fixed Runtime may allocate a
   durable MessageID and Session/Run/Step containment.
   A complete fork copies canonical History at a stable idle revision, rewrites
   child fact identities, and re-derives Context for the child's final model;
   it never copies source Queue or RunJournal execution state. A summary start
   persists only its explicit summary input.
6. The fixed Gateway is shared by the Application. GatewayChannels receive only
   carrier-neutral Gateway access and never receive Runtime objects. A Channel
   owns its communication protocol, remote authentication and authorization,
   routing, encoding, and rate limiting. In-process Channels call GatewayAccess
   directly; only out-of-process Channels require a wire protocol. Live
   subscriptions begin at the current SessionView revision; persistence emits
   only revision notifications, and clients refresh the authoritative View.
   Disconnect and stream overflow do not cancel the Run. Non-streaming calls
   aggregate the same durable Run facts rather than invoking a second path.

## Application host

`Application` is deliberately a thin owner of module selection, build, and
startup. The generic core does not add a component service locator beside the
Assembly. The standard Agent layer uses a narrow Runtime registry whose only
contents are `SessionID → AgentRuntime` instances. That registry is owned by the
application Runtime returned from `Start`; the internal Runtime coordinator only
operates it:

- `NewApplication` copies the named product's complete module list and profile.
- `Build` automatically installs every declared module and all of its contributions.
- `Build` is idempotent after success and returns the same immutable Assembly.
- `Start` automatically builds, then starts the Assembly once.
- `Run` treats context cancellation as a normal shutdown request.
- `Runtime.Assembly()` exposes the exact Assembly owned by the running
  application;
- the internal Runtime module resolves only declared typed Slot dependencies at
  Build time; the started Runtime binds package-private Runtime access only to
  the fixed Gateway and binds carrier-neutral Gateway access to standard
  GatewayChannel Module wrappers;
- standard Agent Channels are installed through
  `standardagent.NewGatewayChannelModule`; an internal build validator rejects an
  otherwise profile-valid raw contribution that skipped Gateway attachment;
- lifecycle dependency order starts the Runtime module before GatewayChannel
  Modules. Shutdown first prevents new Channel commands, then closes
  AgentRuntimes and the registry, then closes Gateway/adapters and shared
  dependencies.

One started application Runtime is intentionally one process execution boundary:
all AgentRuntimes in its registry run in that process. This is a standard
architecture decision, not a staged implementation compromise. Supporting
cross-process Session ownership, leases, or migration would require a separate
architecture decision rather than an implicit extension of this registry.

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

Use it for a selected SessionStore, ModelExecutor, policy
arbiter, scheduler, or execution environment. Framework-owned AgentRuntime is
not represented by `One[T]` because it is not replaceable.

### Many

`Many[T]` allows multiple values with unique stable keys. Registration order is deterministic, but consumers select by key unless their interface specifies another rule.

Use it for tools, model providers, interaction commands, protocol adapters, and
named stores.

`RequireKey` selects one named provider. `RequireMany` means the consumer observes the whole registry, so its lifecycle follows every current provider.

### Chain

`Chain[T]` allows ordered values. Installation order is semantic and is preserved by the Assembly.

Use it only when every contributor participates in an explicit pipeline, such as hooks, policy checks, or prompt contributors. Do not use it as an unordered registry.

## Dependency rules

Modules optionally implement `SlotRequirer`. Requirements name slots, not provider modules:

- `RequireOne` adds an edge from the sole provider.
- `RequireKey` adds an edge from the selected keyed provider.
- `RequireMany` and `RequireChain` add edges from every registered contributor.
- `OptionalOne`, `OptionalMany`, and `OptionalChain` permit no provider, but
  add the same provider-to-consumer lifecycle edges when contributions exist.
- A module's contribution can satisfy its own requirement without creating a self-edge.

Independent modules retain installation precedence. Missing providers, invalid requirements, and cycles fail during `Build`; no lifecycle method has run at that point.

Optional One dependencies use `ResolveOptionalOne`; optional Many and Chain
dependencies use the same `ResolveMany` and `ResolveChain` calls and receive an
empty slice when absent. Requirements do not silently inject fields. Ordinary Go constructors can
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

## Assembly description

The target `Assembly.Describe()` returns the versioned
`agentslot.assembly/v0` description. It
lists modules in lifecycle start order and slots in lexical ID order.
Contributions contain only module ownership and optional keys. Component values
and configuration are intentionally absent so the description can be logged or
exported without serializing implementations or leaking credentials. The
implementation exposes `Assembly.Describe()` and `agentslot.assembly/v0`.

## Package layers

The intended dependency direction is:

```text
             products and profiles
                /          \
fixed AgentRuntime/Gateway  entrypoints, adapters, implementations
                \          /
          standard component contracts
                       |
          AgentSlot composition core
```

The core must remain usable without an LLM SDK, tool SDK, database, UI framework, or wire protocol.

The fixed Runtime/Gateway layer depends on standard contracts and the generic
core; the generic core imports neither AgentRuntime nor Gateway. This preserves
a product-neutral composition package while still giving conforming LLM Agent
projects one loop, one interaction backend, one command vocabulary, and one set
of transaction invariants.

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

Pre-1.0 validation tags may be published so real Agent projects can exercise
the framework and produce the evidence required below. Such a tag is not a
claim of API stability or ecosystem maturity. A release commit must first be
pushed to `main` and pass remote CI; an annotated tag is then created on that
exact `main` commit and pushed without later movement.

The composition API is ready for a stable release only after:

1. At least two independent SDK ecosystems declare real slots over their existing interfaces.
2. One assembled product can exchange implementations through those slots without branching on concrete provider types.
3. Shared conformance tests verify registration, requirements, lifecycle, and exported Assembly descriptions.
4. The evidence proves that one Application Assembly can safely serve multiple
   isolated per-Session AgentRuntime instances without duplicating
   application-level components.

Until those proofs exist, keep domain contracts outside the core. The
`agentslot.assembly/v0` schema remains pre-stable even though it is the current
implementation format.

## Current implementation frontier

The published foundation now includes sixteen standard domain contracts and
typed Slots for Session management/storage, model execution/catalogs, tools,
context, hooks, interaction, tool policy/approval, and passive observation.
The `standardagent` package supplies the
fixed Gateway, private RuntimeAccess binding, per-Session AgentRuntime state
machine, ToolDispatcher, Context assembly, controlled Hook execution, and
idle-only model switching. Reference implementations currently include the
in-memory and crash-safe file Session aggregates, deterministic and OpenAI Chat
Compatible model executors, static model catalog, tail compactor, explicitly
installed Bash/file/HTTP tools, the built-in `model` InteractionCommand,
function-style in-process and line-oriented CLI GatewayChannels, deterministic
tool policy/approval, and a JSON Lines observation module. The fixed Gateway
provides temporary chunk/reset events, durable revision notifications,
SessionView/history-cursor recovery, and non-stream aggregation. The branch-free reference Agent under
`examples/reference` verifies the CLI → Gateway → Runtime → real Provider →
Bash → Provider → persistent Session chain. The remaining frontier includes:

- configuration schemas and secret-resolution components;
- out-of-process Web/RPC/ACP adapters, caller identity, and reliable delivery;
- distributed Session execution leases and deployment coordination;
- reusable conformance suites and Proven component ecosystems.

These are implemented in the order and maturity process defined by the
[Standard Component Map](../COMPONENT_MAP.md). Dependency order controls
lifecycle; registration order continues to control `Many` enumeration and
`Chain` semantics.
