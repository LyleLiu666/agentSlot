// Package interaction defines the carrier-neutral boundary between user
// channels and the fixed Agent Gateway.
package interaction

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

// ChannelSlot contains caller-facing channels bound to one fixed application
// Gateway. Keys identify channels, not Sessions or network routes.
var ChannelSlot = agentslot.Many[GatewayChannel]("gateway.channel")

// CommandSlot contains optional UI-neutral commands shared by every Gateway
// channel. Duplicate command keys are rejected by Assembly Build.
var CommandSlot = agentslot.Many[InteractionCommand]("interaction.command")

// GatewayChannel is an adapter binding operation. The supplied GatewayAccess is
// the only Agent capability it may retain; it never receives Runtime or Store
// objects. Bind retains the capability but must not open listeners or start
// goroutines; listener lifecycle remains the owning Module's responsibility.
type GatewayChannel interface {
	Bind(GatewayAccess) error
}

// GatewayAccess is the fixed, transport-neutral backend boundary. Every
// method returns IDs, revisions, views, receipts, or events—never an
// AgentRuntime pointer or a storage implementation.
type GatewayAccess interface {
	ListSessions(context.Context, ListSessionsRequest) (SessionList, error)
	CreateSession(context.Context, CreateSessionRequest) (SessionOpened, error)
	ResumeSession(context.Context, ResumeSessionRequest) (SessionOpened, error)
	ForkSession(context.Context, ForkSessionRequest) (SessionOpened, error)
	StartSessionFromSummary(context.Context, SummarySessionRequest) (SessionOpened, error)
	Send(context.Context, SendRequest) (EnqueueReceipt, error)
	SendAndWait(context.Context, SendRequest) (RunResult, error)
	Steer(context.Context, SteerRequest) (EnqueueReceipt, error)
	RunPending(context.Context, RunPendingRequest) (RunReceipt, error)
	Cancel(context.Context, CancelRequest) error
	WhenIdle(context.Context, WhenIdleRequest) error
	EditQueued(context.Context, EditQueuedRequest) (CommitReceipt, error)
	DeleteQueued(context.Context, DeleteQueuedRequest) (CommitReceipt, error)
	ReclassifyQueued(context.Context, ReclassifyQueuedRequest) (CommitReceipt, error)
	ModelConfig(context.Context, ModelConfigRequest) (ModelConfigView, error)
	UpdateModelConfig(context.Context, UpdateModelConfigRequest) (CommitReceipt, error)
	View(context.Context, SessionViewRequest) (SessionView, error)
	History(context.Context, HistoryRequest) (HistoryPage, error)
	ExtensionDiagnostics(context.Context, ExtensionDiagnosticsRequest) (ExtensionDiagnosticsPage, error)
	Subscribe(context.Context, SubscribeRequest) (EventStream, error)
	Commands(context.Context, CommandScope) ([]CommandDescriptor, error)
	InvokeCommand(context.Context, CommandInvocation) (CommandResult, error)
	CloseSession(context.Context, CloseSessionRequest) error
}

// ListSessionsRequest carries the Session Store's bounded opaque-cursor query
// through the transport-neutral Gateway. Channels must return Cursor unchanged
// rather than interpreting it.
type ListSessionsRequest struct {
	AgentID     agent.AgentID
	WorkspaceID agent.WorkspaceID
	Limit       int
	Cursor      string
}

// SessionList preserves the Store's deterministic order and continuation.
// An empty NextCursor means the traversal is complete.
type SessionList struct {
	Sessions   []SessionSummary
	NextCursor string
}

type SessionSummary struct {
	SessionID   agent.SessionID
	AgentID     agent.AgentID
	WorkspaceID agent.WorkspaceID
	Revision    agent.Revision
	UpdatedAt   time.Time
}

type CreateSessionRequest struct {
	AgentID     agent.AgentID
	WorkspaceID agent.WorkspaceID
	ModelConfig *session.SessionModelConfig
}

type ResumeSessionRequest struct {
	SessionID agent.SessionID
}

type ForkSessionRequest struct {
	SourceSessionID agent.SessionID
	Mode            session.ForkMode
	CutoffSequence  session.HistorySequence
	AgentID         agent.AgentID
	WorkspaceID     agent.WorkspaceID
	ModelConfig     *session.SessionModelConfig
}

type SummarySessionRequest struct {
	SourceSessionID agent.SessionID
	AgentID         agent.AgentID
	WorkspaceID     agent.WorkspaceID
	Messages        []agent.MessageInput
	ModelConfig     *session.SessionModelConfig
}

type SessionOpened struct {
	SessionID agent.SessionID
	Revision  agent.Revision
}

type SendRequest struct {
	SessionID        agent.SessionID
	ExpectedRevision agent.Revision
	Actor            agent.ActorIdentity
	// ClientMessageID is an optional caller identity copied to the durable
	// user Message so clients can reconcile optimistic and canonical views.
	ClientMessageID agent.ClientMessageID
	Input           agent.MessageInput
}

type SteerRequest struct {
	SessionID        agent.SessionID
	ExpectedRevision agent.Revision
	Actor            agent.ActorIdentity
	ClientMessageID  agent.ClientMessageID
	Input            agent.MessageInput
}

type EnqueueReceipt struct {
	MessageID agent.MessageID
	Revision  agent.Revision
}

// RunResult is the non-streaming Gateway wrapper around the same durable Run.
// AssistantMessages contains every complete assistant message from that Run;
// temporary chunks never appear here.
type RunResult struct {
	SessionID         agent.SessionID
	RunID             agent.RunID
	InputMessageID    agent.MessageID
	Revision          agent.Revision
	Outcome           session.RunFactKind
	AssistantMessages []agent.Message
}

type RunPendingRequest struct {
	SessionID        agent.SessionID
	ExpectedRevision agent.Revision
	Actor            agent.ActorIdentity
}

type RunReceipt struct {
	SessionID agent.SessionID
	RunID     agent.RunID
	Revision  agent.Revision
}

type CancelRequest struct {
	SessionID        agent.SessionID
	ExpectedRevision agent.Revision
	Actor            agent.ActorIdentity
}

type WhenIdleRequest struct {
	SessionID agent.SessionID
}

type EditQueuedRequest struct {
	SessionID        agent.SessionID
	MessageID        agent.MessageID
	ExpectedRevision agent.Revision
	Actor            agent.ActorIdentity
	Input            agent.MessageInput
}

type DeleteQueuedRequest struct {
	SessionID        agent.SessionID
	MessageID        agent.MessageID
	ExpectedRevision agent.Revision
	Actor            agent.ActorIdentity
}

type ReclassifyQueuedRequest struct {
	SessionID        agent.SessionID
	MessageID        agent.MessageID
	ExpectedRevision agent.Revision
	Actor            agent.ActorIdentity
	Delivery         session.Delivery
}

type ModelConfigRequest struct {
	SessionID agent.SessionID
}

type ModelConfigView struct {
	SessionID agent.SessionID
	Revision  agent.Revision
	Config    session.SessionModelConfig
}

type UpdateModelConfigRequest struct {
	SessionID               agent.SessionID
	ExpectedRevision        agent.Revision
	Actor                   agent.ActorIdentity
	Config                  session.SessionModelConfig
	AcceptCompatibilityLoss bool
}

type CommitReceipt struct {
	SessionID agent.SessionID
	Revision  agent.Revision
}

// RevisionConflictError is returned when a caller writes from a stale Session
// revision. The command is never retried implicitly; callers must refresh the
// authoritative SessionView before deciding whether to submit a new command.
type RevisionConflictError struct {
	CurrentRevision  agent.Revision
	SnapshotRequired bool
	Cause            error
}

func (e *RevisionConflictError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return "interaction: session revision conflict"
}

func (e *RevisionConflictError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// InputGateError reports a durable pre-submission rejection or extension
// failure. CurrentRevision is authoritative because preparing and finalizing
// the invocation advances Session CAS even though the user input was not
// mutated. Diagnostics are safe, bounded projections rather than raw input or
// component output.
type InputGateError struct {
	SessionID       agent.SessionID
	CurrentRevision agent.Revision
	Diagnostics     []session.ExtensionDiagnostic
	Cause           error
}

func (e *InputGateError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return "interaction: input gate did not accept the proposed input"
}

func (e *InputGateError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type SessionViewRequest struct {
	SessionID agent.SessionID
}

type HistoryRequest struct {
	SessionID             agent.SessionID
	BeforeHistorySequence session.HistorySequence
	StepLimit             int
}

type HistoryPage struct {
	SessionID agent.SessionID
	Revision  agent.Revision
	Facts     []session.HistoryFact
	HasMore   bool
}

type ExtensionDiagnosticsRequest struct {
	SessionID               agent.SessionID
	BeforeExtensionSequence session.ExtensionSequence
	Limit                   int
}

type ExtensionDiagnosticsPage struct {
	SessionID   agent.SessionID
	Revision    agent.Revision
	Diagnostics []session.ExtensionDiagnostic
	HasMore     bool
}

type SubscribeRequest struct {
	SessionID agent.SessionID
	// AfterRevision must equal the current revision returned by SessionView.
	// A conflict means a commit occurred between View and Subscribe and
	// the caller must repeat that handshake.
	AfterRevision agent.Revision
}

// EventStream is the reconnectable Gateway event boundary. Temporary chunks
// are not durable cursors; a reconnect starts from the requested SessionView
// revision and then receives live events. A slow subscriber may receive
// ErrEventStreamOverflow and must repeat View/Subscribe; neither overflow
// nor Close cancels the Run.
type EventStream interface {
	Recv(context.Context) (Event, error)
	Close() error
}

type Event struct {
	Kind      EventKind
	SessionID agent.SessionID
	RunID     agent.RunID
	StepID    agent.StepID
	// MessageID is the Runtime-reserved assistant identity shared by temporary
	// chunk/reset events and the eventual durable Message. Its presence does
	// not make temporary output durable; only a later Revision exposes commit.
	MessageID agent.MessageID
	// AttemptID identifies temporary output from one physical provider
	// attempt. It is not a durable Session fact.
	AttemptID string
	Revision  agent.Revision
	Text      string
}

var (
	ErrEventStreamClosed   = errors.New("interaction: event stream closed")
	ErrEventStreamOverflow = errors.New("interaction: event stream subscriber fell behind")
)

type EventKind string

const (
	EventChunk    EventKind = "chunk"
	EventReset    EventKind = "reset"
	EventRevision EventKind = "revision"
)

// SessionView is the current client-facing durable projection. RecentHistory
// contains at most 100 complete logical Steps; older facts are read through
// History using the first returned HistorySequence as an exclusive cursor.
type SessionView struct {
	SessionID      agent.SessionID
	Revision       agent.Revision
	RecentHistory  []session.HistoryFact
	HasMoreHistory bool
	Queue          []session.QueueItem
	ModelConfig    session.SessionModelConfig
	RunState       session.RunState
	ActiveRunID    agent.RunID
}

type CloseSessionRequest struct {
	SessionID        agent.SessionID
	ExpectedRevision agent.Revision
	Actor            agent.ActorIdentity
}

type CommandScope struct {
	AgentID     agent.AgentID
	WorkspaceID agent.WorkspaceID
	SessionID   agent.SessionID
}

// InteractionCommand is a UI-independent, structured command registered with
// Gateway. It does not parse slash text or access Runtime/Store objects.
type InteractionCommand interface {
	Describe() CommandDescriptor
	Invoke(context.Context, CommandInvocation, CommandActions) (CommandResult, error)
}

type CommandDescriptor struct {
	Key          string
	Title        string
	Description  string
	Fields       []FieldDescriptor
	Confirmation bool
}

type FieldDescriptor struct {
	Key         string
	Title       string
	Description string
	Type        FieldType
	Required    bool
	Choices     []Choice
}

type FieldType string

const (
	FieldText    FieldType = "text"
	FieldBoolean FieldType = "boolean"
	FieldSingle  FieldType = "single_choice"
	FieldMulti   FieldType = "multi_choice"
)

type Choice struct {
	Value string
	Title string
}

type CommandInvocation struct {
	Scope            CommandScope
	Key              string
	ExpectedRevision agent.Revision
	Actor            agent.ActorIdentity
	Arguments        json.RawMessage
}

// CommandActions exposes only fixed backend actions bound to the current
// CommandInvocation scope. It intentionally has no target field or
// InvokeCommand method, preventing cross-Session or recursive dispatch.
type CommandActions interface {
	CurrentModelConfig(context.Context) (ModelConfigView, error)
	AvailableModels(context.Context) ([]model.Descriptor, error)
	Apply(context.Context, ActionRequest) (ActionResult, error)
}

type ActionRequest struct {
	Kind                    ActionKind
	ExpectedRevision        agent.Revision
	ClientMessageID         agent.ClientMessageID
	Input                   agent.MessageInput
	Config                  session.SessionModelConfig
	AcceptCompatibilityLoss bool
}

type ActionKind string

const (
	ActionSend              ActionKind = "send"
	ActionSteer             ActionKind = "steer"
	ActionRunPending        ActionKind = "run_pending"
	ActionCancel            ActionKind = "cancel"
	ActionUpdateModelConfig ActionKind = "update_model_config"
)

type ActionResult struct {
	Revision  agent.Revision
	MessageID agent.MessageID
	RunID     agent.RunID
}

type CommandResult struct {
	Revision agent.Revision
	Data     json.RawMessage
	Next     []ActionDescriptor
}

type ActionDescriptor struct {
	Kind  ActionKind
	Title string
}
