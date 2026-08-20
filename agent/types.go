// Package agent defines stable, provider-neutral AgentSlot domain values.
//
// It intentionally contains no storage, model, tool, UI, or runtime
// implementation. Contract packages may depend on these values without
// creating a dependency cycle through the fixed AgentRuntime layer.
package agent

import (
	"encoding/json"
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

// Valid reports whether role is a persisted History role.
func (r Role) Valid() bool {
	return r == RoleUser || r == RoleAssistant || r == RoleTool
}

// Message is a durable message fact. Provider-specific wire blocks are not
// represented here; Context is responsible for projecting facts into a
// provider's legal request format.
type Message struct {
	ID        MessageID
	SessionID SessionID
	RunID     RunID
	StepID    StepID
	Role      Role
	Parts     []MessagePart
	CreatedAt time.Time
}

// Valid reports whether a durable message has stable identity and a known
// role. A contained assistant message may be content-empty only so it can own
// tool calls; SessionStore must require those calls in the same transaction.
func (m Message) Valid() bool {
	if !m.ID.Valid() || !m.SessionID.Valid() || !m.Role.Valid() {
		return false
	}
	if len(m.Parts) == 0 {
		return m.Role == RoleAssistant && m.RunID.Valid() && m.StepID.Valid()
	}
	for _, part := range m.Parts {
		if !part.Valid() {
			return false
		}
	}
	return true
}

// MessagePartKind is the finite provider-neutral content vocabulary. Binary
// data is never embedded in History; attachment parts carry stable references.
type MessagePartKind string

const (
	PartText       MessagePartKind = "text"
	PartAttachment MessagePartKind = "attachment"
)

// MessagePart is one text fragment or durable attachment reference. Provider
// adapters project these facts into their own wire blocks.
type MessagePart struct {
	Kind         MessagePartKind
	Text         string
	AttachmentID string
	MediaType    string
	Name         string
}

// Valid reports whether a message part has exactly the payload required by
// its kind.
func (p MessagePart) Valid() bool {
	switch p.Kind {
	case PartText:
		return p.Text != "" && p.AttachmentID == "" && p.MediaType == "" && p.Name == ""
	case PartAttachment:
		return p.Text == "" && p.AttachmentID != "" && p.MediaType != ""
	default:
		return false
	}
}

// MessageInput is unpersisted inbound content. It deliberately has no
// MessageID, SessionID, RunID, StepID, role, or timestamp: only the fixed
// Runtime may allocate durable identity and containment during commit.
type MessageInput struct {
	Parts []MessagePart
}

// Valid reports whether every inbound content part is provider-neutral and
// the input is non-empty.
func (m MessageInput) Valid() bool {
	if len(m.Parts) == 0 {
		return false
	}
	for _, part := range m.Parts {
		if !part.Valid() {
			return false
		}
	}
	return true
}

// ToolCall is the durable identity, arguments, and containment record for one
// model-requested tool invocation.
type ToolCall struct {
	ID ToolCallID
	// CorrelationID is the model protocol's call/result correlation token. It
	// is content supplied by ModelExecutor, not the durable ToolCall identity.
	CorrelationID string
	MessageID     MessageID
	SessionID     SessionID
	RunID         RunID
	StepID        StepID
	Name          string
	Arguments     json.RawMessage
}

// Valid reports whether a tool call has stable containment, a tool name, and
// syntactically valid JSON arguments. Schema validation remains the fixed
// dispatcher's responsibility.
func (c ToolCall) Valid() bool {
	return c.ID.Valid() && c.MessageID.Valid() && c.SessionID.Valid() &&
		c.RunID.Valid() && c.StepID.Valid() && c.Name != "" && json.Valid(c.Arguments)
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
	ID              SessionID
	AgentID         AgentID
	WorkspaceID     WorkspaceID
	ParentSessionID SessionID
	ParentRevision  Revision
	Revision        Revision
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

// ErrorCode is the stable domain reason inside a broad reaction category.
type ErrorCode string

const (
	CodeRevisionConflict       ErrorCode = "revision_conflict"
	CodeQueueItemClaimed       ErrorCode = "queue_item_claimed"
	CodeNoActiveRun            ErrorCode = "no_active_run"
	CodeNoPendingWork          ErrorCode = "no_pending_work"
	CodeRuntimeClosed          ErrorCode = "runtime_closed"
	CodeCanceled               ErrorCode = "canceled"
	CodeSessionUnrecoverable   ErrorCode = "session_unrecoverable"
	CodeApplicationNotStarted  ErrorCode = "application_not_started"
	CodeSessionNotOpen         ErrorCode = "session_not_open"
	CodeSessionAlreadyOpen     ErrorCode = "session_already_open"
	CodeRuntimeUnavailable     ErrorCode = "runtime_unavailable"
	CodeCommandNotFound        ErrorCode = "command_not_found"
	CodeSessionNotFound        ErrorCode = "session_not_found"
	CodeSessionAlreadyExists   ErrorCode = "session_already_exists"
	CodeActiveRun              ErrorCode = "active_run"
	CodeQueueItemNotFound      ErrorCode = "queue_item_not_found"
	CodeQueueItemAlreadyExists ErrorCode = "queue_item_already_exists"
	CodeJournalInvariant       ErrorCode = "journal_invariant"
	CodeHistoryInvariant       ErrorCode = "history_invariant"
)

// ClassifiedError carries a safe operation-level message and an optional
// implementation cause for logs and errors.Is. Callers should branch on Kind,
// not on provider or database error strings.
type ClassifiedError struct {
	Kind    ErrorKind
	Code    ErrorCode
	Op      string
	Message string
	Cause   error
}

func (e *ClassifiedError) Error() string {
	if e == nil {
		return "<nil>"
	}
	classification := string(e.Kind)
	if e.Code != "" {
		classification += "/" + string(e.Code)
	}
	if e.Op == "" {
		return fmt.Sprintf("%s: %s", classification, e.Message)
	}
	return fmt.Sprintf("%s: %s: %s", e.Op, classification, e.Message)
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

// NewCodedError creates a classified error with a stable domain reason.
func NewCodedError(kind ErrorKind, code ErrorCode, op, message string, cause error) error {
	if kind == "" {
		kind = ErrorInternal
	}
	return &ClassifiedError{Kind: kind, Code: code, Op: op, Message: message, Cause: cause}
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

// CodeOf returns the stable domain reason, or an empty code for an
// unclassified implementation error.
func CodeOf(err error) ErrorCode {
	var classified *ClassifiedError
	if errors.As(err, &classified) {
		return classified.Code
	}
	return ""
}

// IsCode reports whether err carries the requested domain reason.
func IsCode(err error, code ErrorCode) bool { return CodeOf(err) == code }
