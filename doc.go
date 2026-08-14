// Package agentslot composes typed agent components without defining an agent
// loop, model protocol, tool protocol, or session format. Build validates
// profile cardinality, module slot dependencies, and lifecycle cycles before
// constructing deferred contributions and returning an immutable plan.
// Build-scoped resolvers expose only dependencies declared by the current
// module. Plan descriptions expose the assembled system without serializing
// component values.
//
// A Module is only a registration and lifecycle envelope. OneSlot, ManySlot,
// and ChainSlot represent distinct component ecosystems with distinct
// cardinality and ordering rules.
package agentslot
