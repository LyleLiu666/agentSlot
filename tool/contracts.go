package tool

import (
	"context"
	"encoding/json"
	"fmt"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/artifact"
	"github.com/LyleLiu666/agentSlot/workspace"
)

// ToolSlot is the standard model-callable tool ecosystem.
var ToolSlot = agentslot.Many[Tool]("tool")

// ParallelSafety controls whether the fixed dispatcher may place calls in the
// same execution batch.
type ParallelSafety string

const (
	ParallelSafe ParallelSafety = "parallel_safe"
	Serial       ParallelSafety = "serial"
)

// Valid reports whether a tool declares one supported scheduling mode.
func (s ParallelSafety) Valid() bool {
	return s == ParallelSafe || s == Serial
}

// Tool is one independently replaceable model-callable capability.
type Tool interface {
	Definition() Definition
	ParallelSafety() ParallelSafety
	Invoke(context.Context, ToolInvocation) ToolResult
}

// ToolInvocation contains stable execution identity and already schema-validated
// arguments. AgentID and WorkspaceID are trusted values derived from the
// authoritative Session; model-supplied arguments cannot replace them. The
// invocation does not expose Runtime, SessionStore, or Gateway internals.
type ToolInvocation struct {
	Call        Call
	SessionID   agent.SessionID
	AgentID     agent.AgentID
	WorkspaceID agent.WorkspaceID
	// WorkspaceBoundary is the opaque binding returned by an installed
	// Workspace Manager. It is nil when the optional Manager is absent.
	WorkspaceBoundary workspace.Boundary
	// MaxInlineOutputBytes is the exact byte budget for ToolResult.Output.
	// Tools may use a lower limit or persist full content before returning.
	MaxInlineOutputBytes int
	RunID                agent.RunID
	StepID               agent.StepID
}

// ToolResult is the structured durable outcome passed back to the model.
type ToolResult struct {
	CallID agent.ToolCallID
	Status ResultStatus
	Output json.RawMessage
	Error  *StructuredError
	// Artifacts are stable references to immutable content already committed
	// through an ArtifactStore before this result is returned.
	Artifacts []artifact.Metadata
}

// Validate ensures a result has exactly one terminal status and a matching
// structured error when it failed.
func (r ToolResult) Validate() error {
	if !r.CallID.Valid() {
		return fmt.Errorf("tool: result requires a call ID")
	}
	if len(r.Output) > 0 && !json.Valid(r.Output) {
		return fmt.Errorf("tool: result output must be valid JSON")
	}
	seenArtifacts := make(map[string]bool, len(r.Artifacts))
	for _, reference := range r.Artifacts {
		if err := reference.Validate(); err != nil {
			return fmt.Errorf("tool: invalid artifact reference: %w", err)
		}
		if seenArtifacts[reference.ID] {
			return fmt.Errorf("tool: duplicate artifact reference %q", reference.ID)
		}
		seenArtifacts[reference.ID] = true
	}
	switch r.Status {
	case ResultSucceeded:
		if r.Error != nil {
			return fmt.Errorf("tool: succeeded result cannot carry an error")
		}
	case ResultFailed:
		if r.Error == nil || r.Error.Code == "" || r.Error.Message == "" {
			return fmt.Errorf("tool: failed result requires a structured error")
		}
	case ResultUnknown:
		if r.Error != nil {
			return fmt.Errorf("tool: unknown result cannot carry a normal error")
		}
	default:
		return fmt.Errorf("tool: unknown result status %q", r.Status)
	}
	return nil
}

// ValidateWithin validates the durable result and enforces the exact inline
// byte budget supplied with the invocation. It never truncates or rewrites the
// result.
func (r ToolResult) ValidateWithin(maxInlineOutputBytes int) error {
	if maxInlineOutputBytes <= 0 {
		return fmt.Errorf("tool: inline output budget must be positive")
	}
	if err := r.Validate(); err != nil {
		return err
	}
	if len(r.Output) > maxInlineOutputBytes {
		return fmt.Errorf("tool: inline output is %d bytes and exceeds budget %d", len(r.Output), maxInlineOutputBytes)
	}
	return nil
}

// ResultStatus is intentionally small and independent of process exit codes.
type ResultStatus string

const (
	ResultSucceeded ResultStatus = "succeeded"
	ResultFailed    ResultStatus = "failed"
	ResultUnknown   ResultStatus = "outcome_unknown"
)

// StructuredError is safe to expose in model context. Internal causes remain
// in logs and are never copied into this value.
type StructuredError struct {
	Code    string
	Message string
}
