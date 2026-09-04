// Package session defines the provider-neutral Session contracts used by the
// standard Agent profile. It also provides an explicitly installed in-memory
// reference implementation; production persistence remains replaceable through
// StoreSlot.
package session

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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

// CommitObserverSlot is the ordered, optional post-commit observation chain.
// Observers never participate in Store transactions and cannot roll them back.
var CommitObserverSlot = agentslot.Chain[SessionCommitObserver]("session.commit.observer")

// CommitNotice identifies one successfully applied Session commit. A zero
// History range means that the commit changed only Context, Queue, Journal, or
// another non-History part of the aggregate.
type CommitNotice struct {
	SessionID            agent.SessionID
	Revision             agent.Revision
	FirstHistorySequence HistorySequence
	LastHistorySequence  HistorySequence
}

func (n CommitNotice) Validate() error {
	if !n.SessionID.Valid() || n.Revision == 0 {
		return fmt.Errorf("session: commit notice requires Session and revision identity")
	}
	if (n.FirstHistorySequence == 0) != (n.LastHistorySequence == 0) || n.FirstHistorySequence > n.LastHistorySequence {
		return fmt.Errorf("session: commit notice History range is invalid")
	}
	return nil
}

// SessionCommitObserver asynchronously observes already committed Session
// facts. The fixed Runtime guarantees revision order within one Session;
// implementations must tolerate concurrent calls for different Sessions.
type SessionCommitObserver interface {
	ObserveSessionCommit(context.Context, CommitNotice) error
}

type CommitObserverFunc func(context.Context, CommitNotice) error

func (f CommitObserverFunc) ObserveSessionCommit(ctx context.Context, notice CommitNotice) error {
	if f == nil {
		return fmt.Errorf("session: nil commit observer function")
	}
	return f(ctx, notice)
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

// ForkMode states whether a fork copies the complete source History or an
// explicit stable prefix. Prefix sequence zero intentionally means no History.
type ForkMode string

const (
	ForkFullHistory   ForkMode = "full_history"
	ForkHistoryPrefix ForkMode = "history_prefix"
)

func (m ForkMode) Valid() bool {
	return m == ForkFullHistory || m == ForkHistoryPrefix
}

// ForkRequest identifies a source Session, the History boundary to copy, and
// the new Session scope. Summary starts remain a separate operation.
type ForkRequest struct {
	SourceSessionID agent.SessionID
	Mode            ForkMode
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
	ExtensionJournal []ExtensionJournalEntry `json:",omitempty"`
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
	FactRunLimitExceeded    HistoryFactKind = "run_limit_exceeded"
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
	RunLimitExceeded    *RunLimitExceededFact
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
		f.ContextContribution != nil, f.RunBudgetExceeded != nil, f.RunLimitExceeded != nil,
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
	case f.RunLimitExceeded != nil:
		if err := f.RunLimitExceeded.Validate(); err != nil {
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
	case f.RunLimitExceeded != nil:
		return FactRunLimitExceeded
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
	// ErrorMessage is the optional, adapter-sanitized provider diagnostic
	// allowed to cross the durable Session and user-presentation boundary.
	ErrorMessage string
}

func (f ModelAttemptFact) Validate() error {
	if !f.AttemptID.Valid() || !f.RunID.Valid() || !f.StepID.Valid() || !f.Kind.Valid() || f.ModelID == "" {
		return fmt.Errorf("session: invalid model attempt fact")
	}
	if err := f.Usage.Validate(); err != nil {
		return err
	}
	if err := model.ValidateErrorMessage(f.ErrorMessage); err != nil {
		return err
	}
	if f.Kind == AttemptStarted && (f.Usage != (model.TokenUsage{}) || f.ProviderRequestID != "" || f.ErrorCode != "" || f.ErrorMessage != "") {
		return fmt.Errorf("session: started attempt cannot contain terminal outcome")
	}
	if f.Kind != AttemptStarted && f.Kind != AttemptSucceeded && f.ErrorCode == "" {
		return fmt.Errorf("session: unsuccessful attempt requires a safe error code")
	}
	if f.Kind == AttemptSucceeded && (f.ErrorCode != "" || f.ErrorMessage != "") {
		return fmt.Errorf("session: succeeded attempt cannot contain an error")
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

// RunLimitKind names a countable, provider-neutral unit that may be bounded
// for one Run. A zero Runtime limit disables enforcement; persisted facts only
// describe a positive limit that rejected the next operation.
type RunLimitKind string

const (
	RunLimitModelAttempts RunLimitKind = "model_attempts"
	RunLimitToolCalls     RunLimitKind = "tool_calls"
)

func (k RunLimitKind) Valid() bool {
	return k == RunLimitModelAttempts || k == RunLimitToolCalls
}

// RunLimitExceededFact records the exact durable count before an operation
// was rejected. Requested is kept separate so an oversized ToolCall batch can
// be rejected atomically without pretending that part of the batch ran.
type RunLimitExceededFact struct {
	RunID             agent.RunID
	StepID            agent.StepID
	Kind              RunLimitKind
	Used              int64
	Max               int64
	Requested         int64
	TriggerAttemptID  agent.AttemptID
	TriggerToolCallID agent.ToolCallID
}

func (f RunLimitExceededFact) Validate() error {
	if !f.RunID.Valid() || !f.StepID.Valid() || !f.Kind.Valid() || f.Used < 0 || f.Max <= 0 || f.Requested <= 0 {
		return fmt.Errorf("session: invalid run limit fact")
	}
	if f.Used < f.Max && f.Requested <= f.Max-f.Used {
		return fmt.Errorf("session: run limit fact does not exceed its maximum")
	}
	switch f.Kind {
	case RunLimitModelAttempts:
		if !f.TriggerAttemptID.Valid() || f.TriggerToolCallID != "" {
			return fmt.Errorf("session: model attempt limit requires only an attempt trigger")
		}
	case RunLimitToolCalls:
		if !f.TriggerToolCallID.Valid() || f.TriggerAttemptID != "" {
			return fmt.Errorf("session: tool call limit requires only a tool call trigger")
		}
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

// RunCompletionMode qualifies how a successful Run reached its final
// assistant response. Empty means an ordinary model-selected completion and
// preserves the meaning of histories written before this field existed.
type RunCompletionMode string

const (
	RunCompletionAttemptLimitFinalization RunCompletionMode = "attempt_limit_finalization"
)

func (m RunCompletionMode) Valid() bool {
	return m == "" || m == RunCompletionAttemptLimitFinalization
}

// MaxRunTerminationMessageBytes bounds the optional, already-sanitized line
// that can be stored on a non-successful Run terminal fact.
const MaxRunTerminationMessageBytes = 1024

// TerminationSource identifies the stable framework phase that prevented a
// Run from completing. It deliberately excludes Provider and product names.
type TerminationSource string

const (
	TerminationModel     TerminationSource = "model"
	TerminationContext   TerminationSource = "context"
	TerminationLoop      TerminationSource = "loop"
	TerminationTool      TerminationSource = "tool"
	TerminationPolicy    TerminationSource = "policy"
	TerminationBudget    TerminationSource = "budget"
	TerminationSession   TerminationSource = "session"
	TerminationRuntime   TerminationSource = "runtime"
	TerminationExtension TerminationSource = "extension"
)

func (s TerminationSource) Valid() bool {
	switch s {
	case TerminationModel, TerminationContext, TerminationLoop, TerminationTool,
		TerminationPolicy, TerminationBudget, TerminationSession, TerminationRuntime, TerminationExtension:
		return true
	default:
		return false
	}
}

// RunTermination is the minimal durable reason why a Run did not complete.
// Retry policy, unknown tool effects, Provider bodies, and UI recovery advice
// are intentionally not part of this immutable fact.
type RunTermination struct {
	Source      TerminationSource
	Kind        agent.ErrorKind
	Code        agent.ErrorCode
	SafeMessage string
}

func (t RunTermination) Validate() error {
	if !t.Source.Valid() || !t.Kind.Valid() || t.Code == "" {
		return fmt.Errorf("session: invalid run termination classification")
	}
	if t.SafeMessage == "" {
		return nil
	}
	if len(t.SafeMessage) > MaxRunTerminationMessageBytes || !utf8.ValidString(t.SafeMessage) || strings.TrimSpace(t.SafeMessage) != t.SafeMessage {
		return fmt.Errorf("session: run termination message must be bounded, valid UTF-8, and trimmed")
	}
	for _, character := range t.SafeMessage {
		if unicode.IsControl(character) || unicode.In(character, unicode.Cf, unicode.Zl, unicode.Zp) {
			return fmt.Errorf("session: run termination message must be a single safe display line")
		}
	}
	return nil
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
	CompletionMode RunCompletionMode `json:",omitempty"`
	Termination    *RunTermination   `json:",omitempty"`
}

func (f RunFact) Validate(sessionID agent.SessionID) error {
	if f.SessionID != sessionID || !f.RunID.Valid() || !f.Kind.Valid() {
		return fmt.Errorf("session: run fact containment is invalid")
	}
	if err := f.ModelConfig.Validate(); err != nil {
		return fmt.Errorf("session: run fact model config is invalid: %w", err)
	}
	if !f.CompletionMode.Valid() {
		return fmt.Errorf("session: invalid run completion mode %q", f.CompletionMode)
	}
	if f.Kind != RunCompleted && f.CompletionMode != "" {
		return fmt.Errorf("session: only a completed run can contain a completion mode")
	}
	if f.Kind == RunStarted || f.Kind == RunCompleted {
		if f.Termination != nil {
			return fmt.Errorf("session: successful or started run cannot contain a termination")
		}
	} else if f.Termination != nil {
		if err := f.Termination.Validate(); err != nil {
			return err
		}
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
	// JournalPrepared means the ToolCall is durable but the execution boundary
	// has not been crossed. Policy and approval may be evaluated in this state,
	// so recovery can safely resume the original call.
	JournalPrepared JournalStatus = "prepared"
	// JournalPending means execution may have started. Recovery must never
	// invoke this call again and resolves it as outcome_unknown instead.
	JournalPending        JournalStatus = "pending"
	JournalSucceeded      JournalStatus = "succeeded"
	JournalFailed         JournalStatus = "failed"
	JournalOutcomeUnknown JournalStatus = "outcome_unknown"
)

// Valid reports whether a journal entry has one standard terminal or pending
// status.
func (s JournalStatus) Valid() bool {
	return s == JournalPrepared || s == JournalPending || s == JournalSucceeded || s == JournalFailed || s == JournalOutcomeUnknown
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
	if e.Status == JournalPrepared || e.Status == JournalPending {
		if e.ToolResult != nil {
			return fmt.Errorf("session: unfinished journal cannot carry a result")
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
	ExtensionDiagnostics(context.Context, ExtensionPageRequest) (ExtensionPage, error)
	ListSessions(context.Context, ListRequest) (ListResult, error)
}

const (
	// DefaultSessionListLimit is used when ListRequest.Limit is zero.
	DefaultSessionListLimit = 50
	// MaxSessionListLimit bounds one persisted Session query.
	MaxSessionListLimit = 200
	// MaxSessionListCursorBytes bounds an opaque cursor before it is decoded.
	MaxSessionListCursorBytes = 4096
)

// ListRequest selects one bounded page of persisted Sessions in an exact
// Agent/Workspace scope. The store, rather than currently open runtimes, is
// the authority for this query. Cursor is opaque and may only be reused with
// the same Store lifecycle and scope that issued it. Limit zero selects the
// default; an explicit limit must be between one and MaxSessionListLimit.
type ListRequest struct {
	AgentID     agent.AgentID
	WorkspaceID agent.WorkspaceID
	Limit       int
	Cursor      string
}

// Validate checks the portable request bounds. A Store additionally validates
// the opaque Cursor against its scope and current lifecycle.
func (r ListRequest) Validate() error {
	if !r.AgentID.Valid() || !r.WorkspaceID.Valid() {
		return fmt.Errorf("agent ID and workspace ID are required")
	}
	if r.Limit < 0 || r.Limit > MaxSessionListLimit {
		return fmt.Errorf("limit must be zero or between 1 and %d", MaxSessionListLimit)
	}
	if len(r.Cursor) > MaxSessionListCursorBytes {
		return fmt.Errorf("cursor exceeds %d bytes", MaxSessionListCursorBytes)
	}
	return nil
}

// SessionSummary is the stable persisted projection used by entrypoints for
// resume pickers. It contains no message content or product configuration.
type SessionSummary struct {
	SessionID   agent.SessionID
	AgentID     agent.AgentID
	WorkspaceID agent.WorkspaceID
	Revision    agent.Revision
	UpdatedAt   time.Time
}

// ListResult is ordered by UpdatedAt descending and then SessionID ascending.
// NextCursor is empty when this traversal is complete. A traversal excludes
// Sessions created after its first page and never repeats a position already
// returned. Concurrent deletion may remove a pending Session; updating a
// pending Session may move it before the cursor and omit it until a fresh
// traversal. Listing never creates, loads, recovers, or starts a Runtime.
type ListResult struct {
	Sessions   []SessionSummary
	NextCursor string
}

type HistoryPageRequest struct {
	SessionID             agent.SessionID
	BeforeHistorySequence HistorySequence
	StepLimit             int
}

type HistoryPage struct {
	AgentID     agent.AgentID
	WorkspaceID agent.WorkspaceID
	Revision    agent.Revision
	Facts       []HistoryFact
	HasMore     bool
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
	AppendRunLimitExceeded    ChangeKind = "append_run_limit_exceeded"
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
	UpdateExtensionJournal    ChangeKind = "update_extension_journal"
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
	RunLimitExceeded      *RunLimitExceededFact
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
	Extension             *ExtensionJournalEntry
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
		if c.RunFact.Kind != RunStarted && c.RunFact.Kind != RunCompleted && c.RunFact.Termination == nil {
			return fmt.Errorf("session: appended non-successful run terminal requires a termination")
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
	case AppendRunLimitExceeded:
		if c.RunLimitExceeded == nil {
			return fmt.Errorf("session: appended run limit fact is missing")
		}
		if err := c.RunLimitExceeded.Validate(); err != nil {
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
	case UpdateExtensionJournal:
		if c.Extension == nil {
			return fmt.Errorf("session: extension journal change is missing")
		}
		if err := c.Extension.Validate(sessionID); err != nil {
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
		c.RunLimitExceeded != nil,
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
		c.Extension != nil,
	} {
		if present {
			count++
		}
	}
	return count
}

// Commit reports the new aggregate revision and exact History range appended
// by that transaction. A non-History commit reports a zero range. Applied is
// false when an idempotency key identifies an already committed equivalent
// request; such a replay preserves the original transaction's range.
type Commit struct {
	SessionID            agent.SessionID
	Revision             agent.Revision
	FirstHistorySequence HistorySequence
	LastHistorySequence  HistorySequence
	Applied              bool
}
