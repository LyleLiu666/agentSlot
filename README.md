# AgentSlot

AgentSlot requires Go 1.25 or newer. The official file tools use the
root-confined atomic rename support introduced with `os.Root.Rename`.

Typed component slots and deterministic composition for agent systems.

> **Start with the AgentSlot Standard Component Map:**
> [English](COMPONENT_MAP.md) | [简体中文](COMPONENT_MAP.zh-CN.md). It is the
> authoritative inventory of customizable agent components, their Slot IDs,
> cardinality, profile requirements, and implementation maturity.

> **Release history:** [CHANGELOG](CHANGELOG.md). Pre-1.0 validation releases
> are intended for real-project feedback and do not promise API stability.

> A module unifies registration and lifecycle. A slot defines the component ecosystem, interface, cardinality, and ordering rule.

The versioned `ComponentCatalog` in the public
[`componentcatalog`](componentcatalog) package is the structured source for
Slot identity, cardinality, profile requirements, contract availability,
maturity, and evidence gaps. The English and Chinese component maps are its
generated public views and remain the human-readable maturity scorecard. The
catalog contains no component instances and is never consulted by Runtime
assembly.

The standard AgentRuntime fixes Session ownership, CAS, cancellation, and
Gateway control. Its Run-control policy is the unique replaceable `agent.loop`
Slot. The other replaceable parts include one SessionStore, one ModelExecutor,
one independent TokenCounter, zero or more Tools, ordered Context and Hook
components, one or more GatewayChannels, optional keyed InteractionCommands,
Goal completion evaluation, long-term Memory, multi-agent Workflow,
Policy/Approval, synchronous usage/billing guards, and passive observation chains.
AgentSlot makes those cardinalities explicit and validates the assembled system
before startup.

The standard profile includes an independent `model.token-counter` for
authoritative pre-call planning counts. `agent.loop` is a constrained
execution-strategy Slot: a Loop receives only the current Run identity, state,
and ordered actions for model requests, prepared tool batches, continuation,
waiting, and termination. Runtime retains Session truth, budgets, cancellation,
recovery, and terminal commits.

`gateway.channel` is also the protocol extension seam. gRPC, WebSocket, SSH,
and inbound ACP may be implemented independently without adding a transport
Slot. Outbound ACP, where this Agent calls another Agent, belongs to
`agent.provider`; sharing a codec does not merge their lifecycles or evidence.

Workspace is now a Contracted optional resource-isolation scope rather than a
filesystem-root API. Runtime propagates the Session-owned Agent/Workspace
identity to policy and Tool calls; an installed Manager resolves an opaque
boundary and rejects missing resources without falling back to process state.
Long-lived tool content reuses `artifact.store`; the duplicate
`tool.output-store` map entry has been removed. ToolInvocation now carries an
explicit inline-output byte budget, ToolResult carries standard Artifact
metadata references, and Runtime rejects violations before continuing the Run.
`credential.resolver` is now Contracted after validation with bearer and basic
credential shapes. Outbound adapters retain only a non-secret Ref, resolve
material inside one physical-operation callback, and may copy only the opaque,
non-reversible identity into billing or audit facts. The OpenAI-compatible
adapter resolves its bearer credential afresh for every physical retry.

`session_history` is a standard keyed, read-only Tool rather than another Slot.
It holds only `HistoryReader`, returns a model-safe revision/sequence projection,
preserves complete logical Steps under the inline output budget, and defaults
to the current Workspace. Current-Session and explicitly authorized full-access
ceilings are available at construction. The `agentslot init` terminal wizard
consumes ComponentCatalog and generates explicit Go assembly code. Its default
`local-coding` preset keeps writes, shell commands, and external side effects
behind approval; `minimal-chat` installs no Tools, Workspace, Artifact, or
Approval components. The generator reads and writes no credential material.

The generic core remains product-neutral. A standard LLM Agent explicitly uses
`standardagent.NewApplication`, which returns the same `*agentslot.Application`
and automatically mounts the fixed AgentRuntime/Gateway layer, an inspectable
standard AgentLoop default, and the standard Agent profile. It does not infer a
profile from installed slots. All standard Agent
projects therefore keep the same `Build`, `Start`, `Run`, and `Runtime.Stop`
entry points; only the declared modules, configuration, and application name
vary.

Calling `standardagent.NewApplication` is the explicit choice of that standard
profile and its standard Loop fallback. The generic `agentslot.NewApplication`
never selects a Loop or any other domain component.

## Status

AgentSlot is a pre-1.0 foundation. `v0.1.10` is the current validation release
and is intended to be consumed by real Agent projects. It adds checked model
stream lifecycles, portable Executor conformance, durable Run termination,
independent token counting, controlled Loop actions, late-bound credentials, trusted
Workspace boundaries, bounded Tool results, durable Artifact and Workspace
references, safe Session history access, explicit project generation, and
remote gRPC/ACP Gateway channels. It is not a
stable-production claim, and public APIs may still change based on integration
feedback. The composition core works today; the standard
component map is normative, while its ecosystems remain at their explicitly
recorded maturity. Every published tag is immutable; later changes receive a
new semantic version.

All implemented public boundaries remain at least `Contracted`.
`model.token-counter` is Contracted and independently replaceable from
`model.executor`. `session.store` is `Conformant` against the exact `v0.0.10`
contract; `model.executor` is `Conformant` against `model.executor/v1`.
No ecosystem is yet `Proven`.

## Generate an explicit Agent project

A released `agentslot` binary pins its own exact AgentSlot version. A source
checkout must supply the release explicitly:

```bash
go run ./cmd/agentslot init --agentslot-version v0.1.10 ./my-agent
```

An interactive terminal selects a preset and collects only non-secret provider,
model, `CredentialRef`, Workspace, and storage settings. Non-interactive use
defaults to `local-coding` and exposes the same choices as flags. Repeat
`--add-implementation` or `--remove-implementation` to customize the Catalog
selection; required dependencies are either added with a visible reason or
rejected before any file is written.

The generated project contains a fixed `go.mod`, explicit Module assembly,
standard Profile requirements, a strict Tool allowlist, approval policy, and a
build test. It contains no local `replace` directive and never overwrites an
existing target directory.

## Out-of-process Gateway example

[`interaction/grpcchannel`](interaction/grpcchannel) implements the complete
`GatewayAccess` surface as `gateway.channel/remote-grpc/v1`. It uses a bounded
protobuf `BytesValue` envelope so uint64 revisions and History sequences remain
exact, and provides a matching `GatewayAccess` client. Install the server with
`standardagent.NewGatewayChannelModule`; the standard wrapper owns its listener
lifecycle after the fixed Gateway is available.

Authentication and authorization callbacks are mandatory. Authenticated
`remote_user`, `service`, or `agent` identity replaces every caller-supplied
Actor value before a write reaches Gateway. TLS, credentials, rate limits, and
deployment remain product configuration through gRPC server options and the
callbacks. A disconnected `SendAndWait` client does not cancel the durable Run;
application shutdown still does.

[`interaction/acpchannel`](interaction/acpchannel) implements the stable ACP v1
inbound surface as `gateway.channel/inbound-acp/v1`. The product-owned transport
must supply an already-authenticated remote identity and an authorization
callback. The channel fixes Agent, Workspace, and working-directory scope;
supports ACP initialize, session new/list/resume/prompt/cancel/close; and maps
complete durable assistant messages to ACP updates. Peer disconnect does not
cancel the durable Run, while ACP `session/cancel` does.

The profile advertises only what it implements. It accepts ACP's required text
and `resource_link` prompt blocks, projecting links to deterministic readable
text. It does not advertise image, audio, embedded context, session load, MCP,
ACP authentication, modes, or configuration options. Outbound ACP remains an
`agent.provider` concern.

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

An application profile may also install an explicit fallback module without
forcing every caller to remove that module before supplying a replacement:

```go
func (m DefaultGeneratorModule) Register(r agentslot.Registrar) error {
	return r.Contribute(agentslot.SetDefault(GeneratorSlot, m.generator))
}
```

Defaults are resolved from the complete module list during `Build`, so module
installation order does not decide which implementation wins:

- an explicit `One` contribution replaces all defaults for that slot;
- an explicit `Many` contribution replaces defaults with the same key;
- any explicit `Chain` contribution replaces the complete default chain;
- multiple defaults for the same `One` or `Many` key still fail when no
  explicit contribution resolves the ambiguity.

`SetDefaultWith`, `AddDefaultWith`, and `AppendDefaultWith` provide the same
behavior for build-time constructors. A fully overridden default module is
absent from the final Assembly, its constructor is not called, and its
lifecycle is not started. Defaults are never discovered or injected globally:
the application must still list every fallback module explicitly.

Official reference packages expose this choice without changing their older
explicit wrappers. For example, Goal, Memory, and Workflow provide named
`NewDefault...Module` constructors for profiles that want replaceable reference
defaults; `New...Module` continues to mean an explicit component selection.

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

Build-time constructors run only after defaults, requirements, and cycles have
been resolved. Their resolver can read only the owning module's declared
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

`Build` resolves explicit-over-default selection, validates requirements,
rejects dependency cycles, constructs deferred contributions in dependency
order, and computes a stable lifecycle order. `assembly.Describe()` exposes
that order, slot kinds, Go value types, contribution owners, keys, whether each
selected contribution was explicit or default, module requirements, and
profile requirements without serializing component values.

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

The framework-side requirements for the next reliability work are defined in
the [AgentSlot 运行可靠性设计](docs/reliability/README.zh-CN.md), with separate
designs for Session/Runtime consistency and the Model execution boundary.

A runnable standard LLM agent requires exactly one AgentLoop, exactly one
SessionStore, exactly one ModelExecutor, and at least one GatewayChannel. The
fixed SessionManager, AgentRuntime, and in-process Gateway are supplied by the
framework. The standard Loop is a conditional default: an explicit Loop Module
replaces it without deleting or naming the default Module.
Standard applications install each Channel with
`standardagent.NewGatewayChannelModule`; Build rejects a raw GatewayChannel
contribution that would bypass GatewayAccess binding or lifecycle ordering.
Tools, ModelProviders, Context components, and AgentHooks are optional globally;
an installed ModelExecutor may explicitly require one or more ModelProviders.
Installed Tools are not implicitly authorized: `AgentRuntimeConfig.ToolKeys` is
a strict allowlist, and nil, empty, or omitted values expose no Tools. Selecting
any Tool also requires a positive `MaxInlineToolResultBytes`; Runtime supplies
that budget to every invocation and fails the Run on a violating result. AgentHook
only proposes follow-on input before Run completion; applied Session commits are
observed separately through the asynchronous `session.commit.observer` chain.
`interaction.command` is an optional `Many` Slot for structured commands that
register only with the Gateway. Channels render the Gateway's UI-neutral
command directory as slash commands, menus, buttons, forms, or command palettes.
`Application.Start` creates a started application Runtime that owns one
process-local Session-to-AgentRuntime registry. Every created or resumed
AgentRuntime registered by that application lives in the same process; durable
Sessions that have not been opened do not occupy a Runtime. A framework-internal
Runtime coordinator operates the registry; only the Gateway receives private
Runtime access. Every GatewayChannel receives the same carrier-neutral Gateway
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

Forking copies stable Session History into a new independent Session. Every
`ForkRequest` and `ForkSessionRequest` selects an explicit mode:
`ForkFullHistory` copies all History and requires an idle source, while
`ForkHistoryPrefix` copies through `CutoffSequence`; prefix sequence zero means
an empty History. A stable prefix can be copied while the source Run continues.
Pending Queue, RunJournal, and active Run state are never copied.

`workspace.Manager` is optional. When installed, create, fork, summary-start,
and resume resolve the exact Session scope before execution; unknown or
unavailable boundaries fail explicitly. The portable Boundary exposes only
its AgentID/WorkspaceID scope—not a root path, URI, credential, or operation—so
filesystem, shell, note, and object-storage capabilities remain separate.

`SessionStore.ListSessions` returns bounded cursor pages for one exact
Agent/Workspace scope. Results are ordered by `UpdatedAt` descending and then
`SessionID` ascending. A cursor is opaque, scoped to the Store lifecycle that
issued it, and excludes Sessions created after the traversal's first page.
Concurrent updates may move an unreturned Session before the cursor, so callers
start a fresh traversal to refresh a resume picker; this deliberately avoids
pretending that every Store can provide a distributed snapshot. An empty
`NextCursor` completes the traversal. The fixed Gateway preserves the same
`Limit`, opaque `Cursor`, and `NextCursor` fields for every Channel.

The `session` package includes a reference `MemoryStore`; the standard framework
constructs its fixed SessionManager over the installed Store and the
Application default model. Together they implement append-only History facts, Context, Queue, RunJournal,
SessionModelConfig inheritance, Run lifecycle facts, revision/CAS commits, and
the basic crash recovery transition. Journal identity compares tool arguments
by exact JSON value rather than serialization bytes, and duplicate object
members are rejected before admission. A Runtime returns to idle after recovery
only when the durable Run has a unique terminal fact; otherwise it fails closed
for explicit resume. These are development and
contract-reference implementations, not a production database; production
storage remains a replaceable `session.store` Slot. Development
applications can explicitly install `session.NewMemoryModule`; importing the
package never selects it as a hidden default.

The separate `memory.store` Slot defines portable Recall, Remember, and Forget
semantics; it does not choose a database, vector index, ranking algorithm, or
retention system. Application developers provide that implementation. The
standard contract preserves typed session-summary, semantic, evidence, and
temporal candidates, source and confidence facts, execution provenance,
visibility, writeback policy, recall intent, and evidence selection. The host
injects execution identity and governance through `memory.RuntimeScope`; model
tool arguments cannot choose those authoritative values.

Mapping a Slot and standardizing its Go method contract are different maturity
steps. A proposed method-level interface needs two independent implementations,
one branch-free real consumer, and a conformance suite before it is described
as proven portable. The map records this evidence instead of hiding unfinished
coverage behind a family name.

The first fixed domain vocabularies are available in the [`model`](model) and
[`tool`](tool) leaf packages: model input/output modalities are text, image, and
audio, serialized by those stable names rather than numeric or byte encodings;
model-facing tool inputs use self-contained JSON Schema Draft 2020-12.
Caller and Hook input uses `agent.MessageInput`, which carries content only;
the fixed Runtime allocates MessageID, Session/Run/Step containment, role, and
timestamp atomically when it creates a durable `agent.Message` fact.
Thirty-two standard component contracts are now available in the `loop`,
`session`, `model`, `tool`, `context`, `hook`, `interaction`, `policy`, `observe`,
`goal`, `memory`, `workflow`, `billing`, `artifact`, `workspace`, and `credential`
packages. They are at least Contracted; `session.store` has a reusable
black-box `session.store/v1` result against the exact `v0.0.10` contract and is
Conformant, while no domain ecosystem is yet Proven.

The `standardagent` package implements the application-scoped Runtime registry,
coordinator, fixed Gateway, GatewayAccess binding, GatewayChannel wrapper,
automatic standard profile, the standard AgentLoop default, and the fixed
Session AgentRuntime transaction state machine. Send,
SendAndWait, Steer, RunPending, Cancel, WhenIdle, Close, Queue mutation, and
model-config commands now execute through the Gateway. `Subscribe` publishes
live chunk/reset events with Run, Step, physical Attempt, and the Runtime-reserved
assistant Message identity shared with the eventual durable Message. The
reserved identity does not make temporary output or client cursors persistent.
Every newly applied commit emits
only a `SessionID + Revision` notification, after which the client reads the
authoritative SessionView. SessionView contains Queue, model configuration,
state, and at most the latest 100 complete logical Steps; older History uses an
exclusive sequence cursor. Temporary events may be dropped under pressure. A
subscriber that cannot receive a durable revision notification is closed so it
can reconnect through View instead of growing an unbounded queue. Disconnecting
a subscriber never cancels its Run. Every external write carries
ExpectedRevision; stale writes return a typed conflict and are never retried
implicitly.
`SendAndWait` wraps the same Run and returns only that Run's durable assistant
text messages rather than executing a second model path. Run start/terminal
facts retain the frozen model configuration. The
`model` package includes an explicitly installed deterministic
`FakeModelExecutor` for development and contract tests, while
[`model/openaicompat`](model/openaicompat) provides a real streaming Chat
Completions-compatible Executor with physical Attempt IDs, bounded output,
retry/reset handling, durable started/terminal Attempt facts, Provider-reported
token usage, and marked local estimates when a failed request has no usage. If a
Provider returns a recognized structured error message, the adapter may record
only that bounded, single-line, sanitized message next to the stable safe error
code. Arbitrary response bodies, credentials, headers, and request content are
not displayable diagnostics. If a
configured model declares image input, that module explicitly requires
`artifact.store`; attachment references are opened through that contract and
projected as real image content blocks rather than placeholder text. The fixed
Runtime also preserves optional opaque JSON continuation state returned by a
ModelExecutor. Runtime binds that state to the selected Provider/model on the durable
assistant Message and carries it unchanged through later Context projection
without rendering or interpreting it. Protocol adapters can therefore resume a
tool turn without adding their private wire vocabulary to the standard
interfaces. The fixed Runtime also owns ToolDispatcher semantics: call/pending
and result/terminal commits,
Serial/ParallelSafe batches, safe structured failures, and mandatory model
continuation. [`tool/bash`](tool/bash) is the first explicitly installed
built-in Tool; it fixes working directory, explicit environment, timeout,
process-group cancellation, and separate output limits. It is never installed
by import or by `standardagent.NewApplication`. The official
[`tool/files`](tool/files) package adds root-confined read/CAS-write/exact-edit
tools, and [`tool/http`](tool/http) adds an allowlisted bounded HTTP client.
The Runtime now persists each ContextSource contribution before use and stores
complete versioned logical requests. `LatestOnly` keeps only the newest request;
`RetainAll` also preserves every prior Step request, including its prompt, model
configuration, tools, and attachment projection. It runs ordered ContextSource and Hook
chains, enforces model protocol, hard context limits, and the optional per-Run
token budget, projects unsupported
attachments without rewriting History, and validates idle-only model switches
through ModelExecutor capabilities. `model.catalog` has a typed contract and
an explicit StaticCatalog reference implementation. Applications can
explicitly install `interaction.NewModelCommandModule`; slash, menu, and
structured clients then invoke that one Gateway command backend. The
`interaction/inprocess` package provides a function-style GatewayChannel, and
[`interaction/cli`](interaction/cli) provides a lifecycle-owned line protocol;
both expose only GatewayAccess. `policy.guard` and `approval.service` now gate
tool execution without gaining loop control. A durable ToolCall is `prepared`
while policy or approval is pending, and crosses to `pending` immediately
before invocation. Recovery can resume only the original prepared call;
pending calls become `outcome_unknown` and are never replayed. Trace, Metric, Audit, and Usage
chains are passive, and [`observe/jsonlines`](observe/jsonlines) is an explicit
default sink. [`session.FileStore`](session/file_store.go) is a crash-safe
single-process persistent implementation. FileStore passes the separately
maintained `session.store/v1` public black-box suite, making that ecosystem
Conformant. MemoryStore and FileStore share one implementation codebase, so
they do not establish Proven maturity; no Web/RPC transport, distributed
Session lease, or reliable-delivery ACK is implied. Importing any package still
has no registration or startup side effect.

## Reference agent

[`examples/reference`](examples/reference) assembles the complete public path:
file-backed Sessions, the OpenAI-compatible Executor, fixed Runtime/Gateway,
CLI, model command, Policy/Approval, Bash, file and HTTP tools, and JSON Lines
observations. It never obtains an AgentRuntime or branches on a concrete
component type. Its Runtime explicitly selects `LatestOnly`, an unlimited
per-Run token budget, a strict ToolKeys allowlist, and a local CLI ActorIdentity.
The end-to-end test also attaches an in-process Channel to the same Gateway and
verifies persisted Context, paired physical Attempts, tool facts, resume, View,
and cursor-based History pagination.

```sh
AGENTSLOT_API_KEY=... \
AGENTSLOT_MODEL=... \
go run ./examples/reference
```

`AGENTSLOT_PROVIDER_URL` defaults to `https://api.openai.com/v1`.
`AGENTSLOT_WORKSPACE`, `AGENTSLOT_SESSION_DIR`, and the comma-separated
`AGENTSLOT_HTTP_HOSTS` configure explicit local and network boundaries. Tool
effects are denied unless `AGENTSLOT_APPROVE_EFFECTS=true`; `file_read` remains
read-only and does not require that approval. In the CLI, ordinary lines are
user messages, while `/help`, `/model`, `/cancel`, `/pending`, and `/quit` are
explicit protocol entries. `/model` without arguments returns structured
choices; a JSON object supplies a selection.

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
go build ./...
go vet ./...
```

Before a reliability-sensitive release, run `scripts/reliability-gate.sh`.
It executes the deterministic FileStore fault matrix and the complete
race-enabled test, vet, and build gates without Provider credentials or
network access. `scripts/reliability-gate.sh --list` prints the frozen stages.

Use the standard component map and exported Go documentation as the public
reference for the composition model.

## License

Licensed under the [Apache License 2.0](LICENSE).
