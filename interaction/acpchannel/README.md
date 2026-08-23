# Inbound ACP channel

`acpchannel` is the stable ACP v1 implementation of
`gateway.channel/inbound-acp/v1`. It lets an ACP client operate the current
Agent through `interaction.GatewayAccess`; it is not an outbound Agent provider
and it does not own Session or History state.

The embedding product owns transport security and authentication. Construction
requires its trusted `remote_user`, `service`, or `agent` identity, a per-call
authorization callback, fixed Agent and Workspace IDs, an absolute working
directory, and owned input/output/close functions. Client metadata never
overrides that identity or scope.

Implemented ACP v1 surface:

- `initialize`
- `session/new`, `session/list`, `session/resume`, and `session/close`
- `session/prompt`, `session/update`, and `session/cancel`
- prompt text and `resource_link` blocks

The channel intentionally does not advertise session load, additional
directories, ACP-managed MCP servers, modes, configuration options, ACP
authentication, image, audio, or embedded context. A resource link is retained
as deterministic Markdown text containing its URI and optional description; it
does not grant file access. Workspace and Tool policy remain authoritative.

`session/prompt` uses the current Gateway revision and publishes only complete
assistant messages from the durable `RunResult`. Temporary chunks, private
AgentSlot fields, and a second history projection are not introduced. Peer
disconnect does not cancel `SendAndWait`; explicit `session/cancel` and
application shutdown do.
