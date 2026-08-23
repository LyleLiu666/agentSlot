# Changelog

This file records published AgentSlot releases. The project is pre-1.0: a
validation release is usable through Go modules, but its public API may change
when real consumers expose a flawed boundary.

## v0.1.2 - 2026-08-23

### Fixed

- OpenAI-compatible executors now use the approved structured HTTP retry
  defaults: 408, 429, 502, 503, and 504 may retry, while 500, 501, 505, and
  other permanent statuses stop. A valid provider `Retry-After` delay takes
  precedence over local backoff without changing the configured attempt cap.

## v0.1.1 - 2026-08-23

### Fixed

- OpenAI-compatible executors now classify a credential-resolution failure as
  `credential_unavailable` before provider dispatch instead of the ambiguous
  `transport` failure code. This lets accounting adapters avoid fabricating a
  physical provider occurrence when no request bytes were sent.

## v0.1.0 - 2026-08-23

### Added

- Added the versioned public `componentcatalog` package as the structured
  source for all 41 standard component ecosystems, including localized
  responsibilities, cardinality, profile requirements, contract availability,
  maturity, evidence identities, and known gaps.
- Added deterministic English and Chinese component-map generation plus a
  repository drift test. The catalog is documentation data only and does not
  participate in Runtime assembly.
- Added the independent `model.token-counter` typed Slot and made it the fifth
  required ecosystem in the standard Agent profile. Context planning now
  fails closed before provider dispatch when counting fails or returns a
  negative value.
- Added explicit fake and OpenAI-compatible TokenCounter implementations. The
  OpenAI-compatible provider module contributes its counter as a replaceable
  default while keeping post-call usage estimation private to its Executor.
- Added the optional `workspace.manager` Slot with a path-neutral Scope and
  opaque Boundary contract validated by local-filesystem and non-filesystem
  fixtures.
- Tool invocations and policy actions now receive trusted AgentID/WorkspaceID
  values derived from the authoritative Session. Installed Workspace managers
  reject missing boundaries before Session creation or recovery and never
  fall back to process-global resources.
- Added an explicit per-invocation inline ToolResult byte budget and standard
  immutable Artifact metadata references. History and Fork preserve those
  references while OpenAI-compatible model projection exposes only safe
  metadata.
- Runtime now converts invalid or oversized Tool results into a durable
  structured contract failure, stops the Run, aborts later serial calls, and
  never silently truncates or retries the possibly effectful Tool.
- Replaced the former one-step AgentLoop driver with ordered, Run-scoped
  Runtime actions for model requests, prepared tool batches, continuation,
  waiting, and termination. A recovered prepared ToolCall re-enters the same
  public Loop protocol without changing its identity. The deprecated
  `Run.Step` entry point remains source-compatible and delegates to those
  controlled actions; new Loop implementations should use `State` and `Act`.
- Runtime rejects concurrent, out-of-order, forged-terminal, and post-terminal
  Loop actions; cancellation, Loop errors, and panics converge to durable Run
  terminal facts without granting the Loop Store or Gateway access.
- Added the optional `credential.resolver` contract with non-secret Ref,
  callback-scoped bearer/basic material, and an opaque non-reversible identity.
  Development memory and AES-256-GCM encrypted-file implementations validate
  two distinct credential shapes and rotation without rebuilding an Assembly.
- OpenAI-compatible Executors no longer retain an API key string. Direct
  construction accepts a Resolver, while Modules explicitly depend on
  `credential.resolver` and resolve the configured Ref for every physical HTTP
  attempt.
- Added the keyed, read-only `session_history` Tool with current-Session,
  same-Workspace, and explicitly authorized full-access ceilings. It uses only
  a narrow HistoryReader and returns safe revision/sequence projections without
  provider attempt identities, continuation state, actors, or Context internals.
- Session history pages now atomically report revision and Agent/Workspace
  scope. The Tool preserves complete logical Steps while fitting its inline
  output budget and returns `result_too_large` instead of silently clipping one
  oversized Step.
- Added crash-safe local reference implementations for `workspace.manager` and
  `artifact.store`. Workspace roots stay private behind opaque scoped
  boundaries; immutable Artifacts use content-derived IDs, self-describing
  files, atomic rename, file and directory sync, and no backing-path exposure.
- Extended ComponentCatalog with constructible implementation identities,
  non-secret configuration fields, dependencies, conflicts, Tool keys, and
  deterministic `local-coding` and `minimal-chat` presets.
- Added `agentslot init`. It generates reviewable, version-pinned Go assembly
  without local replaces or credential values, refuses existing targets,
  validates Workspace/storage separation before writing, supports explicit
  implementation customization, and reports automatically selected
  dependencies. Generated preset fixtures pass build, race, and vet checks.
- Added the runnable `gateway.channel/remote-grpc/v1` example and matching
  out-of-process `GatewayAccess` client. All Gateway operations, revision and
  History integers, event streaming, classified errors, authentication-derived
  Actor identity, authorization scope, overflow, and disconnect behavior are
  mapped without adding a transport Slot or a second Session owner.
- Added the stable ACP v1 inbound profile
  `gateway.channel/inbound-acp/v1`. It binds a transport-authenticated remote
  identity and fixed Agent/Workspace/CWD scope to Gateway, negotiates only its
  implemented session and content capabilities, maps complete durable replies
  to ACP updates, and preserves Runs across peer disconnect while honoring
  explicit ACP cancellation.

### Changed

- Removed planning-time `CountTokens` from `ModelExecutor`; execution,
  capability inspection, retry/continuation, and post-call usage remain the
  Executor's responsibility. The concrete fake and OpenAI-compatible
  executors retain deprecated `CountTokens` methods for source compatibility;
  composition resolves the independent `model.token-counter` Slot.

### Migration

- Existing `v0.0.x` consumers remain pinned until they opt in. Standard Agent
  assemblies must now provide `model.token-counter`; the fake and
  OpenAI-compatible modules provide matching implementations.
- OpenAI-compatible provider credentials move from `Config.APIKey` to a
  non-secret `CredentialRef` plus `credential.resolver`. This is intentionally
  not shimmed because retaining the old field would retain credential material
  inside the Executor.
- Existing AgentLoop implementations using `Run.Step` continue to compile and
  run. New implementations should migrate to the explicit `State`/`Act`
  protocol to control model, tool, continuation, waiting, and finish actions.

## v0.0.10 - 2026-08-22

### Fixed

- `FileStore` now restores a fork after the child Session appends its own
  History facts. Only the copied source prefix carries `OriginFactID`; later
  child facts are no longer misclassified as corrupt lineage during reload.
- `SessionStore.Create` rejects a fork whose `CutoffSequence` does not match
  the complete copied History prefix, so durable lineage can be validated
  without guessing which facts belong to the parent.

### Compatibility and maturity

- This release corrects fork validation without changing the public Go type
  shapes. Callers that supplied an inconsistent cutoff now receive invalid
  input instead of creating an aggregate that cannot be validated reliably.
- `session.store` remains Contracted. Conformance results are recorded
  separately and are not claimed by this implementation fix.

## v0.0.9 - 2026-08-22

### Changed

- `SessionStore.ListSessions` now returns bounded cursor pages. Limit zero uses
  50 entries, explicit limits are bounded at 200, and cursors are capped at
  4096 bytes.
- Session summaries have one deterministic order: `UpdatedAt` descending and
  `SessionID` ascending. Cursors bind that position to the exact
  Agent/Workspace scope and issuing Store lifecycle.
- A traversal excludes Sessions created after its first page and does not
  repeat positions already returned. Concurrent deletion may remove a pending
  Session; an update may move one before the cursor until a fresh traversal.
- The fixed Gateway carries `Limit`, opaque `Cursor`, and `NextCursor` without
  interpreting or dropping the Store continuation.

### Safety

- Reference Store cursors are authenticated opaque values. Malformed,
  modified, cross-scope, cross-Store, and expired-lifecycle cursors fail as
  invalid input.
- Session listing remains a persisted query and never creates, loads,
  recovers, or starts an AgentRuntime.

### Compatibility and maturity

- This is an intentional pre-1.0 `ListRequest`/`ListResult` contract change;
  Store implementations and consumers must adopt pagination together.
- `session.store` remains Contracted. This release does not promote it to
  Conformant, Proven, or Assembled.

## v0.0.8 - 2026-08-22

### Changed

- Corrected the pre-1.0 `memory.store` contract so Recall preserves intent,
  evidence selection, authoritative execution provenance, visibility filters,
  and governed result facts.
- Replaced lossy flat Remember fields with a closed union of typed session
  summary, semantic, evidence, and temporal payloads. Source, confidence,
  visibility, writeback policy, and full Session/Run/Step/Invocation identity
  are explicit rather than guessed by adapters.
- Updated the optional memory tools to expose the corrected schema, inject
  authoritative identity and governance from `RuntimeScope`, and validate both
  requests and implementation results at the Store boundary.

### Safety

- Model-callable memory tools cannot provide execution provenance, visibility,
  writeback policy, or the trusted `worker_consolidation` source kind.
- Memory implementation failures remain hidden from model-facing errors, and
  invalid, inactive, or over-limit Store results are rejected before exposure.

### Compatibility and maturity

- This intentionally breaks the earlier pre-1.0 Memory request shapes; callers
  and adapters must move to the typed contract together. No compatibility shim
  is provided.
- `memory.store` remains Contracted. This release does not promote any ecosystem
  to Conformant, Proven, or Assembled.

## v0.0.7 - 2026-08-22

### Fixed

- The standard model command no longer advertises one static reasoning
  dropdown for every model. Channels obtain each model's supported subset from
  the command query result and can render only valid choices.

## v0.0.6 - 2026-08-22

### Added

- The closed portable reasoning vocabulary now includes `xhigh` and `max`.
  Each model continues to advertise only the subset it actually supports;
  adding vocabulary does not claim universal Provider support.

## v0.0.5 - 2026-08-21

### Added

- Model completions may return one opaque JSON continuation owned by the
  selected Provider/model. The fixed Runtime persists it on the durable
  assistant Message, Context preserves it byte-for-byte through tool loops and
  compaction, and adapters for other models can ignore it safely.

### Safety

- Opaque continuation state is never interpreted or rendered as message
  content, cannot be attached to user messages, and carries its Provider/model
  ownership so another selected model must not consume it.

## v0.0.4 - 2026-08-21

### Fixed

- OpenAI Chat Compatible fallback usage now applies the same semantic image
  estimate as `CountTokens`; it no longer bills base64 transport bytes as input
  tokens when a Provider omits usage.

## v0.0.3 - 2026-08-21

### Added

- Goal Store and Evaluator contracts attached to the fixed Runtime completion
  boundary, including structured continue/blocked/done decisions, bounded
  follow-ons, CAS-protected state, and a strict model-backed evaluator.
- Keyed long-term Memory Store plus optional recall/remember/forget tools and
  a context source that recalls from the latest actual user intent.
- Agent Provider, Scheduler, Job Store, and addressed append-only Mailbox
  contracts, reference in-memory stores and scheduler, and optional agent tools.
- Price Resolver, Quota Guard, Billing Ledger, and a synchronous physical-model
  Attempt Observer used for pre-dispatch quota and durable accounting intent.
- Model configuration on the completion Hook view so consumers receive the
  frozen provider-neutral Run selection without reading product configuration.
- `AttemptModuleOptions` for a host-supplied default quota reservation size
  when the selected model Config intentionally omits `max_tokens`.
- An immutable `ArtifactStore` contract for writing content and resolving the
  stable references kept in History, without exposing local paths or binary
  data in provider-neutral messages.
- Real image attachment projection in the OpenAI Chat Completions-compatible
  Executor. Image-capable modules now declare `artifact.store` as a typed Build
  dependency instead of silently converting attachments to placeholder text.

### Safety

- A user steer accepted during Goal evaluation invalidates that stale decision.
- Attempt-observer start rejection compensates earlier observers; finish
  observer failure cannot erase the physical terminal Attempt fact.
- Workflow shutdown waits for cancellation-aware providers and persists an
  explicit cancellation reason. Model-facing Memory and Workflow errors do not
  expose implementation error text.

### Compatibility and maturity

- The component map now contains 41 standard ecosystems and 29 public
  AgentSlot-owned domain contracts.
- The new rows remain `Contracted`; no row is promoted to `Conformant`,
  `Proven`, or `Assembled` by this change alone.

## v0.0.2 - 2026-08-20

`v0.0.2` is the first end-to-end framework validation release. It is intended
for integration into real Agent projects; it is not yet a production-stability
or ecosystem-maturity claim.

### Added

- Fixed per-Session `AgentRuntime` and application-owned Runtime registry.
- Fixed Gateway with UI-neutral commands, strict Session revision/CAS writes,
  streaming notifications, `SessionView`, and cursor-based History recovery.
- Standard Slot contracts for Session storage, model execution and catalogs,
  Gateway channels, interaction commands, tools, context, hooks, policy,
  approval, and passive observation.
- Append-only typed Session History, model-attempt recording, Context snapshots,
  historical Session fork, Run journal, persistent Queue, and per-Session model
  configuration.
- Reference in-memory and crash-safe file Session stores, deterministic and
  OpenAI-compatible executors, CLI and in-process channels, Bash/file/HTTP tools,
  tail compaction, policy/approval implementations, and JSON Lines observation.
- An end-to-end reference Agent demonstrating the path from Gateway channel to
  Runtime, Provider, tool execution, persistent Session, resume, View, and
  History pagination.

### Changed

- The former `Plan` vocabulary and API were replaced by `Assembly`,
  `AssemblyDescription`, and the application `Runtime` lifecycle.
- The standard Agent loop is fixed framework behavior rather than a replaceable
  `agent.loop` Slot. Developers replace focused components through typed Slots.
- `history.store` became `session.store`, which persists the complete Session
  aggregate rather than History alone.
- The file Session format is `agentslot.session-file/v1`. Pre-release v0 files
  are rejected; no migration shim is provided.
- Tool keys are a strict allowlist: missing or empty keys expose no tools, and
  unknown or duplicate keys fail during Build.

### Compatibility and maturity

- Go 1.25 or newer is required.
- At the `v0.0.2` tag, the component map contained 37 standard ecosystems and 16 public AgentSlot
  domain contracts.
- All 16 implemented contracts are `Contracted`; none are yet `Conformant` or
  `Proven`. Real-project validation and independent implementations are still
  required before a stable recommendation.

## v0.0.1

Initial composition foundation and standard component-map baseline.
