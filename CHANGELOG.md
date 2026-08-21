# Changelog

This file records published AgentSlot releases. The project is pre-1.0: a
validation release is usable through Go modules, but its public API may change
when real consumers expose a flawed boundary.

## Unreleased

No changes yet.

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
