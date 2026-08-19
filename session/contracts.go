// Package session defines the provider-neutral Session contracts used by the
// standard Agent profile. It also provides an explicitly installed in-memory
// reference implementation; production persistence remains replaceable through
// StoreSlot.
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
	ModelConfig     *SessionModelConfig
}

// SummaryRequest starts a Session from a caller-provided summary projection.
// The summary is not treated as a hidden history rewrite.
type SummaryRequest struct {
	SourceSessionID agent.SessionID
	AgentID         agent.AgentID
	WorkspaceID     agent.WorkspaceID
	Messages        []agent.MessageInput
	ModelConfig     *SessionModelConfig
}

// Snapshot is the store-facing immutable view returned after a successful
// create, load, or commit. Implementations return detached slices; mutating a
// caller's copy never changes persisted state.
type Snapshot struct {
	Session     agent.Session
	Revision    agent.Revision
	History     []HistoryFact
	Context     ContextView
	Queue       []QueueItem
	RunJournal  []JournalEntry
	ModelConfig SessionModelConfig
	RunState    RunState
	ActiveRunID agent.RunID
}

// HistoryFact is one ordered, append-only fact in a Session's canonical
// ledger. Exactly one payload is present. Context is responsible for
// projecting these facts into a provider-valid message sequence.
type HistoryFact struct {
	Message    *agent.Message
	ToolCall   *agent.ToolCall
	ToolResult *tool.ToolResult
}

// Validate checks the payload and its Session containment. Tool results do
// not carry a SessionID themselves; the store pairs them with their call.
func (f HistoryFact) Validate(sessionID agent.SessionID) error {
	count := 0
	if f.Message != nil {
		count++
	}
	if f.ToolCall != nil {
		count++
	}
	if f.ToolResult != nil {
		count++
	}
	if count != 1 {
		return fmt.Errorf("session: history fact requires exactly one payload")
	}
	switch {
	case f.Message != nil:
		if !f.Message.Valid() || f.Message.SessionID != sessionID {
			return fmt.Errorf("session: history message does not belong to session")
		}
	case f.ToolCall != nil:
		if !f.ToolCall.Valid() || f.ToolCall.SessionID != sessionID {
			return fmt.Errorf("session: history tool call is invalid")
		}
	case f.ToolResult != nil:
		if err := f.ToolResult.Validate(); err != nil {
			return err
		}
	}
	return nil
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
	Message   agent.Message
	Delivery  Delivery
	ClaimedBy agent.RunID
}

// Claimed reports whether an active or interrupted Run owns this item.
func (i QueueItem) Claimed() bool { return i.ClaimedBy.Valid() }

// QueueClaim assigns an unclaimed item to one Run inside the same aggregate
// transaction that starts or advances that Run.
type QueueClaim struct {
	MessageID agent.MessageID
	RunID     agent.RunID
}

// QueueConsume removes one item after the owning Run has committed it into
// Context or otherwise durably accounted for the input.
type QueueConsume struct {
	MessageID agent.MessageID
	RunID     agent.RunID
}

// QueueEdit replaces the content and delivery class of an unclaimed item
// while retaining its durable MessageID.
type QueueEdit struct {
	MessageID agent.MessageID
	Input     agent.MessageInput
	Delivery  Delivery
}

// QueueDelete identifies an unclaimed item to remove from the pending view.
type QueueDelete struct{ MessageID agent.MessageID }

// QueueReclassify changes only the delivery class of an unclaimed item.
type QueueReclassify struct {
	MessageID agent.MessageID
	Delivery  Delivery
}

// RunState is the persisted execution state of a Session. A Session can have
// at most one running Run, regardless of how many process callers use it.
type RunState string

const (
	RunIdle    RunState = "idle"
	RunRunning RunState = "running"
)

func (s RunState) Valid() bool { return s == RunIdle || s == RunRunning }

type RunStateChange struct {
	RunID agent.RunID
	State RunState
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

// Validate enforces the one-call/one-outcome journal shape. The journal is
// recovery state, while the matching call and result remain canonical History
// facts.
func (e JournalEntry) Validate(sessionID agent.SessionID) error {
	if !e.RunID.Valid() || !e.StepID.Valid() || !e.Status.Valid() || e.ToolCall == nil {
		return fmt.Errorf("session: journal entry requires run, step, call, and status")
	}
	if !e.ToolCall.Valid() || e.ToolCall.SessionID != sessionID || e.ToolCall.RunID != e.RunID || e.ToolCall.StepID != e.StepID {
		return fmt.Errorf("session: journal tool call containment is invalid")
	}
	if e.Status == JournalPending {
		if e.ToolResult != nil {
			return fmt.Errorf("session: pending journal cannot carry a result")
		}
		return nil
	}
	if e.ToolResult == nil || e.ToolResult.CallID != e.ToolCall.ID {
		return fmt.Errorf("session: terminal journal requires the matching result")
	}
	if err := e.ToolResult.Validate(); err != nil {
		return err
	}
	switch e.Status {
	case JournalSucceeded:
		if e.ToolResult.Status != tool.ResultSucceeded {
			return fmt.Errorf("session: succeeded journal requires a succeeded result")
		}
	case JournalFailed:
		if e.ToolResult.Status != tool.ResultFailed {
			return fmt.Errorf("session: failed journal requires a failed result")
		}
	case JournalOutcomeUnknown:
		if e.ToolResult.Status != tool.ResultUnknown {
			return fmt.Errorf("session: unknown journal requires an unknown result")
		}
	}
	return nil
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
	Recover(context.Context, SessionRef) (Snapshot, error)
	Commit(context.Context, CommitRequest) (Commit, error)
}

// NewSession is the complete initial aggregate supplied by SessionManager.
// The Store persists it atomically and does not invent product defaults or
// stable IDs.
type NewSession struct {
	Session     agent.Session
	History     []HistoryFact
	Context     ContextView
	Queue       []QueueItem
	RunJournal  []JournalEntry
	ModelConfig SessionModelConfig
	RunState    RunState
	ActiveRunID agent.RunID
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
	ConsumeQueue     ChangeKind = "consume_queue"
	EditQueue        ChangeKind = "edit_queue"
	DeleteQueue      ChangeKind = "delete_queue"
	ReclassifyQueue  ChangeKind = "reclassify_queue"
	SetContext       ChangeKind = "set_context"
	SetModelConfig   ChangeKind = "set_model_config"
	SetRunState      ChangeKind = "set_run_state"
	UpdateRunJournal ChangeKind = "update_run_journal"
)

// Change is a provider-neutral, single-payload aggregate update accepted by
// Store.Commit. Unknown kinds are rejected.
type Change struct {
	Kind                  ChangeKind
	Message               *agent.Message
	ToolCall              *agent.ToolCall
	ToolResult            *tool.ToolResult
	QueueItem             *QueueItem
	QueueClaim            *QueueClaim
	QueueConsume          *QueueConsume
	QueueEdit             *QueueEdit
	QueueDelete           *QueueDelete
	QueueReclassification *QueueReclassify
	Context               *ContextView
	ModelConfig           *SessionModelConfig
	RunState              *RunStateChange
	Journal               *JournalEntry
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
		if c.ToolCall == nil || !c.ToolCall.Valid() || c.ToolCall.SessionID != sessionID {
			return fmt.Errorf("session: appended tool call does not belong to session")
		}
	case AppendToolResult:
		if c.ToolResult == nil {
			return fmt.Errorf("session: appended tool result is missing")
		}
		if err := c.ToolResult.Validate(); err != nil {
			return err
		}
	case EnqueueMessage:
		if c.QueueItem == nil || !c.QueueItem.Message.Valid() || c.QueueItem.Message.SessionID != sessionID || c.QueueItem.Message.Role != agent.RoleUser || c.QueueItem.Message.RunID != "" || c.QueueItem.Message.StepID != "" || !c.QueueItem.Delivery.Valid() || c.QueueItem.ClaimedBy != "" {
			return fmt.Errorf("session: queue change does not belong to session")
		}
	case ClaimQueue:
		if c.QueueClaim == nil || !c.QueueClaim.MessageID.Valid() || !c.QueueClaim.RunID.Valid() {
			return fmt.Errorf("session: queue claim requires message and run IDs")
		}
	case ConsumeQueue:
		if c.QueueConsume == nil || !c.QueueConsume.MessageID.Valid() || !c.QueueConsume.RunID.Valid() {
			return fmt.Errorf("session: queue consume requires message and run IDs")
		}
	case EditQueue:
		if c.QueueEdit == nil || !c.QueueEdit.MessageID.Valid() || !c.QueueEdit.Input.Valid() || !c.QueueEdit.Delivery.Valid() {
			return fmt.Errorf("session: queue edit is invalid")
		}
	case DeleteQueue:
		if c.QueueDelete == nil || !c.QueueDelete.MessageID.Valid() {
			return fmt.Errorf("session: queue delete requires a message ID")
		}
	case ReclassifyQueue:
		if c.QueueReclassification == nil || !c.QueueReclassification.MessageID.Valid() || !c.QueueReclassification.Delivery.Valid() {
			return fmt.Errorf("session: queue reclassification is invalid")
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
	case SetRunState:
		if c.RunState == nil || !c.RunState.RunID.Valid() || !c.RunState.State.Valid() {
			return fmt.Errorf("session: invalid run state change")
		}
	case UpdateRunJournal:
		if c.Journal == nil {
			return fmt.Errorf("session: journal change is missing")
		}
		if err := c.Journal.Validate(sessionID); err != nil {
			return err
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
		c.QueueClaim != nil,
		c.QueueConsume != nil,
		c.QueueEdit != nil,
		c.QueueDelete != nil,
		c.QueueReclassification != nil,
		c.Context != nil,
		c.ModelConfig != nil,
		c.RunState != nil,
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
