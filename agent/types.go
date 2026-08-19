// Package agent defines stable, provider-neutral AgentSlot domain values.
//
// It intentionally contains no storage, model, tool, UI, or runtime
// implementation. Contract packages may depend on these values without
// creating a dependency cycle through the fixed AgentRuntime layer.
package agent

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// The identity types are deliberately distinct even though they use the same
// wire representation. This prevents accidentally passing a SessionID where a
// RunID is required.
type (
	AgentID     string
	WorkspaceID string
	SessionID   string
	RunID       string
	StepID      string
	MessageID   string
	ToolCallID  string
)

func (id AgentID) Valid() bool     { return validID(string(id)) }
func (id WorkspaceID) Valid() bool { return validID(string(id)) }
func (id SessionID) Valid() bool   { return validID(string(id)) }
func (id RunID) Valid() bool       { return validID(string(id)) }
func (id StepID) Valid() bool      { return validID(string(id)) }
func (id MessageID) Valid() bool   { return validID(string(id)) }
func (id ToolCallID) Valid() bool  { return validID(string(id)) }

func validID(value string) bool {
	return value != "" && strings.TrimSpace(value) == value
}

// Revision is the monotonically increasing version of a durable aggregate.
type Revision uint64

// Next returns the next revision without mutating the receiver.
func (r Revision) Next() Revision { return r + 1 }

// Role is the stable semantic role of a persisted message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is a durable message fact. Provider-specific wire blocks are not
// represented here; Context is responsible for projecting facts into a
// provider's legal request format.
type Message struct {
	ID        MessageID
	SessionID SessionID
	RunID     RunID
	StepID    StepID
	Role      Role
	CreatedAt time.Time
}

// ToolCall is the durable identity and containment record for one model
// requested tool invocation. Its arguments remain provider-neutral JSON in a
// later Tool contract; this round only fixes its identity relationships.
type ToolCall struct {
	ID        ToolCallID
	MessageID MessageID
	SessionID SessionID
	RunID     RunID
	StepID    StepID
	Name      string
}

// Agent identifies one configured capability set.
type Agent struct {
	ID AgentID
}

// Workspace identifies a product-owned scope under one Agent.
type Workspace struct {
	ID      WorkspaceID
	AgentID AgentID
}

// Session identifies one isolated conversation and execution aggregate.
type Session struct {
	ID          SessionID
	AgentID     AgentID
	WorkspaceID WorkspaceID
	Revision    Revision
}

// Run identifies one execution inside one Session.
type Run struct {
	ID        RunID
	SessionID SessionID
}

// Step identifies one model call or tool batch inside one Run. The concrete
// step kind vocabulary is intentionally kept small until execution contracts
// are introduced in a later round.
type Step struct {
	ID    StepID
	RunID RunID
}

// ErrorKind is the provider-neutral classification used at component
// boundaries. It describes how a caller may react, not an implementation's
// private diagnostic.
type ErrorKind string

const (
	ErrorInvalidInput ErrorKind = "invalid_input"
	ErrorConflict     ErrorKind = "conflict"
	ErrorNotFound     ErrorKind = "not_found"
	ErrorUnauthorized ErrorKind = "unauthorized"
	ErrorForbidden    ErrorKind = "forbidden"
	ErrorCanceled     ErrorKind = "canceled"
	ErrorDeadline     ErrorKind = "deadline_exceeded"
	ErrorUnavailable  ErrorKind = "unavailable"
	ErrorInternal     ErrorKind = "internal"
)

// ClassifiedError carries a safe operation-level message and an optional
// implementation cause for logs and errors.Is. Callers should branch on Kind,
// not on provider or database error strings.
type ClassifiedError struct {
	Kind    ErrorKind
	Op      string
	Message string
	Cause   error
}

func (e *ClassifiedError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Op == "" {
		return fmt.Sprintf("%s: %s", e.Kind, e.Message)
	}
	return fmt.Sprintf("%s: %s: %s", e.Op, e.Kind, e.Message)
}

func (e *ClassifiedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// NewError creates a classified boundary error. Empty kinds are normalized to
// internal instead of creating an unclassifiable public error.
func NewError(kind ErrorKind, op, message string, cause error) error {
	if kind == "" {
		kind = ErrorInternal
	}
	return &ClassifiedError{Kind: kind, Op: op, Message: message, Cause: cause}
}

// KindOf returns the public reaction category of err. Unknown implementation
// errors are intentionally treated as internal failures.
func KindOf(err error) ErrorKind {
	var classified *ClassifiedError
	if errors.As(err, &classified) && classified.Kind != "" {
		return classified.Kind
	}
	return ErrorInternal
}

// IsKind reports whether err carries the requested public reaction category.
func IsKind(err error, kind ErrorKind) bool { return KindOf(err) == kind }
