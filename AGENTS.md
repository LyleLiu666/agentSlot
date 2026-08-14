# AgentSlot contributor rules

AgentSlot exists to reduce composition entropy across agent projects. Keep the core smaller and more stable than any product that consumes it.

## Governing objective

AgentSlot's primary architectural asset is its standard component interface
map: it tells agent developers which responsibilities can be implemented and
replaced independently. Its score is the quality and proven portability of
those component ecosystems, not the raw number of interfaces. Keep the generic
composition core small while making **component boundaries, cardinality,
dependencies, lifecycle, conformance maturity, and the final assembled plan
explicit, inspectable, and exportable**. Reject additions that increase API
surface without clarifying or mechanically enforcing one of those properties.

## Non-negotiable design rules

- `Module` is only the registration and lifecycle envelope. Component ecosystems are represented by typed slots.
- Express module dependencies against typed slots, never concrete module IDs. Build must reject missing providers and cycles before startup.
- `Plan.Describe()` may expose IDs, kinds, types, keys, ownership, requirements, and order. It must never expose component values, configurations, credentials, or other secrets.
- Keep the composition core free of product, provider, UI, storage, and transport dependencies.
- Keep core documentation product-neutral. Describe capability roles here; document named framework migrations and adapters in their consuming repositories.
- Fix finite, cross-provider semantic vocabulary in AgentSlot leaf packages;
  keep provider wire formats, configuration, limits, and product policy in
  implementations and adapters. Do not make stable semantics configurable to
  avoid an architectural decision.
- Keep `COMPONENT_MAP.md` as the authoritative inventory and maturity scorecard
  for standard Slot ecosystems. Do not describe a mapped responsibility as an
  implemented interface.
- Keep `COMPONENT_MAP.md` and `COMPONENT_MAP.zh-CN.md` semantically synchronized
  in every component-map change.
- Do not call a standard domain interface proven from one implementation.
  Require two independent implementations, one real consumer, and a
  conformance suite to reach proven maturity.
- Keep profile requirements explicit. Do not silently select a loop, provider, tool, policy arbiter, or execution environment.
- Registration is transactional. Startup failure rolls back in reverse order. Shutdown attempts every started module.
- Prefer compile-time typing. Reflection is limited to internal slot-definition identity checks.
- Do not use Go dynamic plugins (`plugin` package). Use normal Go linking in-process and explicit wire protocols out-of-process.

## Change discipline

- Use TDD for behavior changes.
- Update README or architecture documentation with public API changes.
- Run `gofmt -w .`, `go test -race ./...`, and `go vet ./...` before publishing.
- Keep commits focused. Do not add compatibility shims before the first tagged release; fix the foundation and all callers together.
