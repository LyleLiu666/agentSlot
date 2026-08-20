// Package context defines provider-neutral context source and compaction
// contracts for the standard Agent profile.
package context

import (
	stdcontext "context"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
)

// SourceSlot is the ordered context contribution ecosystem.
var SourceSlot = agentslot.Chain[ContextSource]("context.source")

// CompactorSlot is the replaceable context compression ecosystem.
var CompactorSlot = agentslot.One[ContextCompactor]("context.compactor")

// ContextSource contributes an ordered, provider-neutral projection without
// mutating History. Runtime validates the complete protocol after every source.
type ContextSource interface {
	// Key is stable fact identity for this ordered source implementation.
	Key() string
	Contribute(stdcontext.Context, ContextInput) ([]model.Input, error)
}

// ContextInput is the complete context available to a source. Fixed
// SystemPrompt and Tool definitions are assembled separately by Runtime.
type ContextInput struct {
	SessionID agent.SessionID
	Revision  agent.Revision
	Inputs    []model.Input
	Config    model.Config
}

// Compactor returns a smaller legal message projection. It never writes the
// SessionStore or allocates a ContextVersion itself.
type ContextCompactor interface {
	Compact(stdcontext.Context, CompactionInput) (CompactionOutput, error)
}

// CompactionInput is the full current Context supplied to a compactor.
type CompactionInput struct {
	SessionID agent.SessionID
	Revision  agent.Revision
	Inputs    []model.Input
	Config    model.Config
}

// CompactionOutput is the compactor result plus the source revision it was based on.
type CompactionOutput struct {
	SourceRevision agent.Revision
	Inputs         []model.Input
}
