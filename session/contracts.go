// Package session defines the provider-neutral Session contracts used by the
// standard Agent profile. It contains no persistence implementation.
package session

import (
	"context"
	"fmt"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/tool"
)

// SessionModelConfig is the durable model selection owned by a Session. It
// aliases the provider-neutral model value while giving the aggregate a
// stable domain name; provider addresses and credentials are not included.
type SessionModelConfig = model.Config

// ManagerSlot is the standard Session creation and recovery ecosystem.
var ManagerSlot = agentslot.One[SessionManager]("session.manager")

// StoreSlot is the standard durable Session aggregate ecosystem.
var StoreSlot = agentslot.One[SessionStore]("session.store")

// Manager creates, restores, and derives complete Session aggregates. It does
// not execute a model or tool loop.
type SessionManager interface {
	Create(context.Context, CreateRequest) (Session, error)
	Resume(context.Context, ResumeRequest) (Session, error)
	Fork(context.Context, ForkRequest) (Session, error)
	StartFromSummary(context.Context, SummaryRequest) (Session, error)
}

// CreateRequest describes the stable identity needed to create a Session.
// Product-specific defaults are resolved by the standard Agent layer.
type CreateRequest struct {
	AgentID     agent.AgentID
	WorkspaceID agent.WorkspaceID
	ModelConfig *SessionModelConfig
}

// ResumeRequest identifies a previously persisted Session.
type ResumeRequest struct {
	SessionID agent.SessionID
}

// ForkRequest identifies a source Session and the new Session scope. The
// choice between a complete history fork and a summary start is explicit.
type ForkRequest struct {
	SourceSessionID agent.SessionID
	AgentID         agent.AgentID
	WorkspaceID     agent.WorkspaceID
	Mode            ForkMode
	ModelConfig     *SessionModelConfig
}

// ForkMode is intentionally closed so a caller cannot silently change the
// meaning of a derived Session.
type ForkMode string

const (
	ForkCompleteHistory ForkMode = "complete_history"
	ForkSummary         ForkMode = "summary"
)

// Valid reports whether a derived Session operation has explicit semantics.
func (m ForkMode) Valid() bool {
	return m == ForkCompleteHistory || m == ForkSummary
}

// SummaryRequest starts a Session from a caller-provided summary projection.
// The summary is not treated as a hidden history rewrite.
type SummaryRequest struct {
	AgentID     agent.AgentID
	WorkspaceID agent.WorkspaceID
	Messages    []agent.MessageInput
	ModelConfig *SessionModelConfig
}

// Snapshot is the store-facing immutable view returned after a successful
// create, load, or commit. Implementations return detached slices; mutating a
// caller's copy never changes persisted state.
type Snapshot struct {
	Session     agent.Session
	Revision    agent.Revision
	History     []agent.Message
	Context     ContextView
	Queue       []QueueItem
	RunJournal  []JournalEntry
	ModelConfig SessionModelConfig
}

// ContextView is the current legal model-message projection and its source
// revision. It is not the History fact ledger.
type ContextView struct {
	Revision agent.Revision
	Messages []agent.Message
}

// Delivery classifies queued input before it is claimed by a Run.
type Delivery string

const (
	DeliveryNormal Delivery = "normal"
	DeliverySteer  Delivery = "steer"
	DeliveryHeld   Delivery = "held"
)

// Valid reports whether a queued message has an explicit delivery class.
func (d Delivery) Valid() bool {
	return d == DeliveryNormal || d == DeliverySteer || d == DeliveryHeld
}

// QueueItem is mutable only while unclaimed and only through Store CAS.
type QueueItem struct {
	Message  agent.Message
	Delivery Delivery
	Claimed  bool
}

// JournalStatus records execution recovery state without copying dialogue
// facts from History.
type JournalStatus string

const (
	JournalPending        JournalStatus = "pending"
	JournalSucceeded      JournalStatus = "succeeded"
	JournalFailed         JournalStatus = "failed"
	JournalOutcomeUnknown JournalStatus = "outcome_unknown"
)

// Valid reports whether a journal entry has one standard terminal or pending
// status.
func (s JournalStatus) Valid() bool {
	return s == JournalPending || s == JournalSucceeded || s == JournalFailed || s == JournalOutcomeUnknown
}

// JournalEntry identifies a recoverable run or tool operation.
type JournalEntry struct {
	RunID      agent.RunID
	StepID     agent.StepID
	ToolCall   *agent.ToolCall
	ToolResult *tool.ToolResult
	Status     JournalStatus
}

// Session is the narrow handle exposed to callers after a successful
// create/load operation. It does not expose Store mutation methods.
type Session interface {
	ID() agent.SessionID
	Revision() agent.Revision
	View(context.Context) (Snapshot, error)
}

// Store is the only persistence boundary for a Session aggregate. Commit uses
// compare-and-swap on ExpectedRevision and an idempotency key for safe retry.
type SessionStore interface {
	Create(context.Context, NewSession) (Snapshot, error)
	Load(context.Context, SessionRef) (Snapshot, error)
	Commit(context.Context, CommitRequest) (Commit, error)
}

// NewSession is the complete initial aggregate supplied by SessionManager.
// The Store persists it atomically and does not invent product defaults or
// stable IDs.
type NewSession struct {
	Session     agent.Session
	History     []agent.Message
	Context     ContextView
	Queue       []QueueItem
	ModelConfig SessionModelConfig
}

// SessionRef is the narrow durable identity accepted by Store.Load.
type SessionRef struct {
	SessionID agent.SessionID
}

// CommitRequest is deliberately structured around a typed aggregate change
// set. Later contracts may add more typed changes; callers cannot bypass
// the expected-revision check by writing storage-specific commands.
type CommitRequest struct {
	SessionID        agent.SessionID
	ExpectedRevision agent.Revision
	IdempotencyKey   string
	Changes          []Change
}

// Validate checks the cross-component parts of a commit before a Store sees
// it. It does not make the CAS decision; only the Store can do that atomically.
func (r CommitRequest) Validate() error {
	if !r.SessionID.Valid() {
		return fmt.Errorf("session: commit requires a valid session ID")
	}
	if r.IdempotencyKey == "" {
		return fmt.Errorf("session: commit requires an idempotency key")
	}
	if len(r.Changes) == 0 {
		return fmt.Errorf("session: commit requires at least one change")
	}
	for _, change := range r.Changes {
		if err := change.Validate(r.SessionID); err != nil {
			return err
		}
	}
	return nil
}

// ChangeKind is the initial cross-component vocabulary for durable facts. It
// does not expose a database operation or provider wire format.
type ChangeKind string

const (
	AppendMessage    ChangeKind = "append_message"
	AppendToolCall   ChangeKind = "append_tool_call"
	AppendToolResult ChangeKind = "append_tool_result"
	EnqueueMessage   ChangeKind = "enqueue_message"
	ClaimQueue       ChangeKind = "claim_queue"
	SetContext       ChangeKind = "set_context"
	SetModelConfig   ChangeKind = "set_model_config"
	UpdateRunJournal ChangeKind = "update_run_journal"
)

// Change is a provider-neutral, single-payload aggregate update accepted by
// Store.Commit. Unknown kinds are rejected.
type Change struct {
	Kind        ChangeKind
	Message     *agent.Message
	ToolCall    *agent.ToolCall
	ToolResult  *tool.ToolResult
	QueueItem   *QueueItem
	Context     *ContextView
	ModelConfig *SessionModelConfig
	Journal     *JournalEntry
}

// Validate checks a durable change's stable identity and containment.
func (c Change) Validate(sessionID agent.SessionID) error {
	if c.payloadCount() != 1 {
		return fmt.Errorf("session: change %q requires exactly one payload", c.Kind)
	}
	switch c.Kind {
	case AppendMessage:
		if c.Message == nil || !c.Message.Valid() || c.Message.SessionID != sessionID {
			return fmt.Errorf("session: appended message does not belong to session")
		}
	case AppendToolCall:
		if c.ToolCall == nil || !c.ToolCall.ID.Valid() || c.ToolCall.SessionID != sessionID {
			return fmt.Errorf("session: appended tool call does not belong to session")
		}
	case AppendToolResult:
		if c.ToolResult == nil || !c.ToolResult.CallID.Valid() {
			return fmt.Errorf("session: appended tool result requires a call ID")
		}
	case EnqueueMessage, ClaimQueue:
		if c.QueueItem == nil || !c.QueueItem.Message.Valid() || c.QueueItem.Message.SessionID != sessionID || !c.QueueItem.Delivery.Valid() {
			return fmt.Errorf("session: queue change does not belong to session")
		}
	case SetContext:
		if c.Context == nil {
			return fmt.Errorf("session: context change is missing")
		}
	case SetModelConfig:
		if c.ModelConfig == nil {
			return fmt.Errorf("session: model config change is missing")
		}
		if err := c.ModelConfig.Validate(); err != nil {
			return fmt.Errorf("session: invalid model config change: %w", err)
		}
	case UpdateRunJournal:
		if c.Journal == nil || !c.Journal.RunID.Valid() || !c.Journal.Status.Valid() {
			return fmt.Errorf("session: journal change requires a run ID")
		}
	default:
		return fmt.Errorf("session: unsupported change kind %q", c.Kind)
	}
	return nil
}

func (c Change) payloadCount() int {
	count := 0
	for _, present := range []bool{
		c.Message != nil,
		c.ToolCall != nil,
		c.ToolResult != nil,
		c.QueueItem != nil,
		c.Context != nil,
		c.ModelConfig != nil,
		c.Journal != nil,
	} {
		if present {
			count++
		}
	}
	return count
}

// Commit reports the new aggregate revision. Applied is false when an
// idempotency key identifies an already committed equivalent request.
type Commit struct {
	SessionID agent.SessionID
	Revision  agent.Revision
	Applied   bool
}
