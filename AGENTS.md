# AgentSlot contributor rules

AgentSlot exists to reduce composition entropy across agent projects. Keep the core smaller and more stable than any product that consumes it.

## Governing objective

AgentSlot does not surpass another harness by accumulating dozens of interfaces. That would reproduce the structural entropy this project exists to remove. It succeeds by making **component ecosystems, cardinality, dependencies, lifecycle, and the final assembled plan explicit, inspectable, and exportable while keeping the core small**. Reject additions that increase API surface without making one of those properties clearer or mechanically enforceable.

## Non-negotiable design rules

- `Module` is only the registration and lifecycle envelope. Component ecosystems are represented by typed slots.
- Express module dependencies against typed slots, never concrete module IDs. Build must reject missing providers and cycles before startup.
- `Plan.Describe()` may expose IDs, kinds, types, keys, ownership, requirements, and order. It must never expose component values, configurations, credentials, or other secrets.
- Keep the composition core free of product, provider, UI, storage, and transport dependencies.
- Keep core documentation product-neutral. Describe capability roles here; document named framework migrations and adapters in their consuming repositories.
- Do not add a standard domain interface from one implementation. Require two independent implementations, one real consumer, and a conformance suite.
- Keep profile requirements explicit. Do not silently select a loop, provider, tool, policy arbiter, or execution environment.
- Registration is transactional. Startup failure rolls back in reverse order. Shutdown attempts every started module.
- Prefer compile-time typing. Reflection is limited to internal slot-definition identity checks.
- Do not use Go dynamic plugins (`plugin` package). Use normal Go linking in-process and explicit wire protocols out-of-process.

## Change discipline

- Use TDD for behavior changes.
- Update README or architecture documentation with public API changes.
- Run `gofmt -w .`, `go test -race ./...`, and `go vet ./...` before publishing.
- Keep commits focused. Do not add compatibility shims before the first tagged release; fix the foundation and all callers together.
