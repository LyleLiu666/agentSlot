package tool

import (
	"context"
	"encoding/json"
	"fmt"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
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
// arguments. It does not expose Runtime, SessionStore, or Gateway internals.
type ToolInvocation struct {
	Call      Call
	SessionID agent.SessionID
	RunID     agent.RunID
	StepID    agent.StepID
}

// ToolResult is the structured durable outcome passed back to the model.
type ToolResult struct {
	CallID agent.ToolCallID
	Status ResultStatus
	Output json.RawMessage
	Error  *StructuredError
}

// Validate ensures a result has exactly one terminal status and a matching
// structured error when it failed.
func (r ToolResult) Validate() error {
	if !r.CallID.Valid() {
		return fmt.Errorf("tool: result requires a call ID")
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
