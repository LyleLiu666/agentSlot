// Package agentslot is the generic composition core for AgentSlot projects.
// It defines typed slots, module registration, dependency validation, and the
// common Build/Start/Run/Stop lifecycle. It deliberately does not define an
// AgentRuntime, model protocol, tool protocol, session format, or Gateway.
// Those are layered on top through standard contracts and product modules.
//
// Application mounts a declared module set and builds one immutable Assembly.
// Build validates profile cardinality, typed-slot dependencies, and lifecycle
// ordering before resolving contributions. Assembly descriptions expose only
// safe metadata; component values, configuration, and credentials are never
// serialized.
//
// Module is the registration and lifecycle envelope. OneSlot, ManySlot, and
// ChainSlot represent component ecosystems with different cardinality and
// ordering rules.
package agentslot
