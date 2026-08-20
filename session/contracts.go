// Package session defines the provider-neutral Session contracts used by the
// standard Agent profile. It also provides an explicitly installed in-memory
// reference implementation; production persistence remains replaceable through
// StoreSlot.
package session

import (
	"context"
	"fmt"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/tool"
)

// SessionModelConfig is the durable model selection owned by a Session. It
// aliases the provider-neutral model value while giving the aggregate a
// stable domain name; provider addresses and credentials are not included.
type SessionModelConfig = model.Config

// StoreSlot is the standard durable Session aggregate ecosystem.
var StoreSlot = agentslot.One[SessionStore]("session.store")

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
	CutoffSequence  HistorySequence
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
	Session          agent.Session
	Revision         agent.Revision
	History          []HistoryFact
	Context          ContextView
	RetainedContexts []ContextView
	Queue            []QueueItem
	RunJournal       []JournalEntry
	Events           []SessionEvent
	ModelConfig      SessionModelConfig
	RunState         RunState
	ActiveRunID      agent.RunID
	Fork             *ForkProvenance
}

type ForkProvenance struct {
	ParentSessionID agent.SessionID
	CutoffSequence  HistorySequence
}

// SessionEvent is durable aggregate metadata that must not be projected into
// model Context. Revision is assigned by SessionStore to the atomic commit.
type SessionEvent struct {
	Kind               SessionEventKind
	Revision           agent.Revision
	ModelConfigChanged *ModelConfigChange
}

type SessionEventKind string

const EventModelConfigChanged SessionEventKind = "model_config_changed"

// ModelConfigChange preserves the explicit selection transition for audit and
// reconnect projections without pretending it was a conversation message.
type ModelConfigChange struct {
	Previous SessionModelConfig
	Current  SessionModelConfig
}

func (e SessionEvent) Validate() error {
	if e.Kind != EventModelConfigChanged || e.ModelConfigChanged == nil {
		return fmt.Errorf("session: invalid Session event")
	}
	if err := e.ModelConfigChanged.Previous.Validate(); err != nil {
		return fmt.Errorf("session: invalid previous model config: %w", err)
	}
	if err := e.ModelConfigChanged.Current.Validate(); err != nil {
		return fmt.Errorf("session: invalid current model config: %w", err)
	}
	return nil
}

// HistorySequence is the immutable order of a fact inside one Session.
type HistorySequence uint64

// ContextVersion identifies one durable logical model-request projection.
type ContextVersion uint64

// HistoryFactKind is the closed public vocabulary of complete Session
// History. Context decides which facts form a legal model protocol sequence.
type HistoryFactKind string

const (
	FactMessage             HistoryFactKind = "message"
	FactToolCall            HistoryFactKind = "tool_call"
	FactToolResult          HistoryFactKind = "tool_result"
	FactRun                 HistoryFactKind = "run"
	FactModelAttempt        HistoryFactKind = "model_attempt"
	FactModelConfigChanged  HistoryFactKind = "model_config_changed"
	FactContextContribution HistoryFactKind = "context_contribution"
	FactRunBudgetExceeded   HistoryFactKind = "run_budget_exceeded"
)

// HistoryFact is one ordered, append-only fact in a complete Session History.
// Store assigns its envelope; callers supply exactly one typed payload.
type HistoryFact struct {
	FactID       agent.FactID
	OriginFactID agent.FactID
	Sequence     HistorySequence
	SessionID    agent.SessionID
	RunID        agent.RunID
	StepID       agent.StepID
	At           time.Time
	Actor        agent.ActorIdentity
	Kind         HistoryFactKind

	Message             *agent.Message
	ToolCall            *agent.ToolCall
	ToolResult          *tool.ToolResult
	Run                 *RunFact
	ModelAttempt        *ModelAttemptFact
	ModelConfigChanged  *ModelConfigChange
	ContextContribution *ContextContributionFact
	RunBudgetExceeded   *RunBudgetExceededFact
}

// Validate checks the payload and its Session containment. Tool results do
// not carry a SessionID themselves; the store pairs them with their call.
func (f HistoryFact) Validate(sessionID agent.SessionID) error {
	if f.OriginFactID != "" && !f.OriginFactID.Valid() {
		return fmt.Errorf("session: history origin fact ID is invalid")
	}
	if f.FactID.Valid() || f.Sequence != 0 || f.SessionID.Valid() || !f.At.IsZero() || f.Actor.Valid() || f.Kind != "" || f.RunID.Valid() || f.StepID.Valid() {
		if !f.FactID.Valid() || f.Sequence == 0 || f.SessionID != sessionID || f.At.IsZero() || !f.Actor.Valid() || f.Kind != f.payloadKind() {
			return fmt.Errorf("session: history fact envelope is invalid")
		}
	}
	return f.validatePayload(sessionID)
}

func (f HistoryFact) validatePayload(sessionID agent.SessionID) error {
	count := 0
	for _, present := range []bool{
		f.Message != nil, f.ToolCall != nil, f.ToolResult != nil, f.Run != nil,
		f.ModelAttempt != nil, f.ModelConfigChanged != nil,
		f.ContextContribution != nil, f.RunBudgetExceeded != nil,
	} {
		if present {
			count++
		}
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
	case f.Run != nil:
		if err := f.Run.Validate(sessionID); err != nil {
			return err
		}
	case f.ModelAttempt != nil:
		if err := f.ModelAttempt.Validate(); err != nil {
			return err
		}
	case f.ModelConfigChanged != nil:
		if err := (SessionEvent{Kind: EventModelConfigChanged, ModelConfigChanged: f.ModelConfigChanged}).Validate(); err != nil {
			return err
		}
	case f.ContextContribution != nil:
		if err := f.ContextContribution.Validate(sessionID); err != nil {
			return err
		}
	case f.RunBudgetExceeded != nil:
		if err := f.RunBudgetExceeded.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (f HistoryFact) payloadKind() HistoryFactKind {
	switch {
	case f.Message != nil:
		return FactMessage
	case f.ToolCall != nil:
		return FactToolCall
	case f.ToolResult != nil:
		return FactToolResult
	case f.Run != nil:
		return FactRun
	case f.ModelAttempt != nil:
		return FactModelAttempt
	case f.ModelConfigChanged != nil:
		return FactModelConfigChanged
	case f.ContextContribution != nil:
		return FactContextContribution
	case f.RunBudgetExceeded != nil:
		return FactRunBudgetExceeded
	default:
		return ""
	}
}

// ModelAttemptKind records a physical provider request boundary.
type ModelAttemptKind string

const (
	AttemptStarted        ModelAttemptKind = "started"
	AttemptSucceeded      ModelAttemptKind = "succeeded"
	AttemptFailed         ModelAttemptKind = "failed"
	AttemptCanceled       ModelAttemptKind = "canceled"
	AttemptOutcomeUnknown ModelAttemptKind = "outcome_unknown"
)

func (k ModelAttemptKind) Valid() bool {
	return k == AttemptStarted || k == AttemptSucceeded || k == AttemptFailed || k == AttemptCanceled || k == AttemptOutcomeUnknown
}

type ModelAttemptFact struct {
	AttemptID         agent.AttemptID
	RunID             agent.RunID
	StepID            agent.StepID
	Kind              ModelAttemptKind
	ProviderKey       string
	ModelID           string
	ProviderRequestID string
	Usage             model.TokenUsage
	ErrorCode         string
}

func (f ModelAttemptFact) Validate() error {
	if !f.AttemptID.Valid() || !f.RunID.Valid() || !f.StepID.Valid() || !f.Kind.Valid() || f.ModelID == "" {
		return fmt.Errorf("session: invalid model attempt fact")
	}
	if err := f.Usage.Validate(); err != nil {
		return err
	}
	if f.Kind == AttemptStarted && (f.Usage != (model.TokenUsage{}) || f.ProviderRequestID != "" || f.ErrorCode != "") {
		return fmt.Errorf("session: started attempt cannot contain terminal outcome")
	}
	if f.Kind != AttemptStarted && f.Kind != AttemptSucceeded && f.ErrorCode == "" {
		return fmt.Errorf("session: unsuccessful attempt requires a safe error code")
	}
	if f.Kind == AttemptSucceeded && f.ErrorCode != "" {
		return fmt.Errorf("session: succeeded attempt cannot contain an error code")
	}
	return nil
}

type ContextContributionFact struct {
	RunID     agent.RunID
	StepID    agent.StepID
	SourceKey string
	Inputs    []model.Input
}

func (f ContextContributionFact) Validate(sessionID agent.SessionID) error {
	if !f.RunID.Valid() || !f.StepID.Valid() || f.SourceKey == "" {
		return fmt.Errorf("session: invalid context contribution fact")
	}
	for _, input := range f.Inputs {
		wrongMessageSession := input.Message != nil && input.Message.SessionID != sessionID
		wrongCallSession := input.ToolCall != nil && input.ToolCall.SessionID != sessionID
		if !input.Valid() || input.SystemPrompt != nil || wrongMessageSession || wrongCallSession {
			return fmt.Errorf("session: invalid context contribution input")
		}
	}
	return nil
}

type RunBudgetExceededFact struct {
	RunID      agent.RunID
	UsedTokens int64
	MaxTokens  int64
}

func (f RunBudgetExceededFact) Validate() error {
	if !f.RunID.Valid() || f.MaxTokens <= 0 || f.UsedTokens < f.MaxTokens {
		return fmt.Errorf("session: invalid run budget fact")
	}
	return nil
}

// RunFactKind is the finite lifecycle vocabulary recorded in canonical
// History. Run facts are auditable execution facts, not model-facing messages.
type RunFactKind string

const (
	RunStarted     RunFactKind = "started"
	RunCompleted   RunFactKind = "completed"
	RunCanceled    RunFactKind = "canceled"
	RunFailed      RunFactKind = "failed"
	RunInterrupted RunFactKind = "interrupted"
)

func (k RunFactKind) Valid() bool {
	return k == RunStarted || k == RunCompleted || k == RunCanceled || k == RunFailed || k == RunInterrupted
}

// RunFact records the exact model configuration frozen when a Run started and
// repeats it on the terminal fact. ConfigRevision is the Session revision from
// which the snapshot was taken; later model switches cannot rewrite it.
type RunFact struct {
	SessionID      agent.SessionID
	RunID          agent.RunID
	Kind           RunFactKind
	ModelConfig    SessionModelConfig
	ConfigRevision agent.Revision
}

func (f RunFact) Validate(sessionID agent.SessionID) error {
	if f.SessionID != sessionID || !f.RunID.Valid() || !f.Kind.Valid() {
		return fmt.Errorf("session: run fact containment is invalid")
	}
	if err := f.ModelConfig.Validate(); err != nil {
		return fmt.Errorf("session: run fact model config is invalid: %w", err)
	}
	return nil
}

// ContextView is one complete logical model request and its source revision.
// It includes the exact SystemPrompt, projected inputs, Tool definitions and
// frozen ModelConfig visible to the model, but never credentials or headers.
type ContextView struct {
	Version               ContextVersion
	SourceRevision        agent.Revision
	SourceHistorySequence HistorySequence
	TokenCount            int
	Request               model.ModelRequest
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
	HistoryPage(context.Context, HistoryPageRequest) (HistoryPage, error)
}

type HistoryPageRequest struct {
	SessionID             agent.SessionID
	BeforeHistorySequence HistorySequence
	StepLimit             int
}

type HistoryPage struct {
	Facts   []HistoryFact
	HasMore bool
}

// NewSession is the complete initial aggregate supplied by the fixed framework Manager.
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
	Fork        *ForkProvenance
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
	Actor            agent.ActorIdentity
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
	if (r.Actor.Kind != "" || r.Actor.ID != "") && !r.Actor.Valid() {
		return fmt.Errorf("session: commit actor identity is invalid")
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
	AppendMessage             ChangeKind = "append_message"
	AppendToolCall            ChangeKind = "append_tool_call"
	AppendToolResult          ChangeKind = "append_tool_result"
	AppendRunFact             ChangeKind = "append_run_fact"
	AppendModelAttempt        ChangeKind = "append_model_attempt"
	AppendContextContribution ChangeKind = "append_context_contribution"
	AppendRunBudgetExceeded   ChangeKind = "append_run_budget_exceeded"
	AppendSessionEvent        ChangeKind = "append_session_event"
	EnqueueMessage            ChangeKind = "enqueue_message"
	ClaimQueue                ChangeKind = "claim_queue"
	ConsumeQueue              ChangeKind = "consume_queue"
	EditQueue                 ChangeKind = "edit_queue"
	DeleteQueue               ChangeKind = "delete_queue"
	ReclassifyQueue           ChangeKind = "reclassify_queue"
	SetContext                ChangeKind = "set_context"
	SetModelConfig            ChangeKind = "set_model_config"
	SetRunState               ChangeKind = "set_run_state"
	UpdateRunJournal          ChangeKind = "update_run_journal"
)

// Change is a provider-neutral, single-payload aggregate update accepted by
// Store.Commit. Unknown kinds are rejected.
type Change struct {
	Kind                  ChangeKind
	Message               *agent.Message
	ToolCall              *agent.ToolCall
	ToolResult            *tool.ToolResult
	RunFact               *RunFact
	ModelAttempt          *ModelAttemptFact
	ContextContribution   *ContextContributionFact
	RunBudgetExceeded     *RunBudgetExceededFact
	SessionEvent          *SessionEvent
	QueueItem             *QueueItem
	QueueClaim            *QueueClaim
	QueueConsume          *QueueConsume
	QueueEdit             *QueueEdit
	QueueDelete           *QueueDelete
	QueueReclassification *QueueReclassify
	Context               *ContextView
	RetainPreviousContext bool
	ModelConfig           *SessionModelConfig
	RunState              *RunStateChange
	Journal               *JournalEntry
}

// Validate checks a durable change's stable identity and containment.
func (c Change) Validate(sessionID agent.SessionID) error {
	if c.payloadCount() != 1 {
		return fmt.Errorf("session: change %q requires exactly one payload", c.Kind)
	}
	if c.RetainPreviousContext && c.Kind != SetContext {
		return fmt.Errorf("session: context retention applies only to a context change")
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
	case AppendRunFact:
		if c.RunFact == nil {
			return fmt.Errorf("session: appended run fact is missing")
		}
		if err := c.RunFact.Validate(sessionID); err != nil {
			return err
		}
	case AppendModelAttempt:
		if c.ModelAttempt == nil {
			return fmt.Errorf("session: appended model attempt is missing")
		}
		if err := c.ModelAttempt.Validate(); err != nil {
			return err
		}
	case AppendContextContribution:
		if c.ContextContribution == nil {
			return fmt.Errorf("session: appended context contribution is missing")
		}
		if err := c.ContextContribution.Validate(sessionID); err != nil {
			return err
		}
	case AppendRunBudgetExceeded:
		if c.RunBudgetExceeded == nil {
			return fmt.Errorf("session: appended run budget fact is missing")
		}
		if err := c.RunBudgetExceeded.Validate(); err != nil {
			return err
		}
	case AppendSessionEvent:
		if c.SessionEvent == nil || c.SessionEvent.Revision != 0 {
			return fmt.Errorf("session: appended Session event must have store-assigned revision")
		}
		if err := c.SessionEvent.Validate(); err != nil {
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
		c.RunFact != nil,
		c.ModelAttempt != nil,
		c.ContextContribution != nil,
		c.RunBudgetExceeded != nil,
		c.SessionEvent != nil,
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
