# Changelog

This file records published releases and the explicitly marked next release
candidate. The project is pre-1.0: a validation release is usable through Go
modules, but its public API may change when real consumers expose a flawed
boundary.

## v0.0.2 - Unreleased

`v0.0.2` is planned as the first end-to-end framework validation release. The
candidate is intended for integration into real Agent projects before tagging;
it is not a production-stability or ecosystem-maturity claim.

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
- The component map contains 37 standard ecosystems and 16 public AgentSlot
  domain contracts.
- All 16 implemented contracts are `Contracted`; none are yet `Conformant` or
  `Proven`. Real-project validation and independent implementations are still
  required before a stable recommendation.

## v0.0.1

Initial composition foundation and standard component-map baseline.
