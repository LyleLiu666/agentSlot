// Package interaction defines the carrier-neutral boundary between user
// entrypoints and the fixed Agent Gateway.
package interaction

import (
	"context"
	"encoding/json"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/session"
)

// EntrypointSlot contains the caller-facing adapters that all use one
// application Gateway. Keys identify adapters, not Sessions.
var EntrypointSlot = agentslot.Many[Entrypoint]("interaction.entrypoint")

// CommandSlot contains optional UI-neutral commands shared by every
// Entrypoint. Duplicate command keys are rejected by Assembly Build.
var CommandSlot = agentslot.Many[InteractionCommand]("interaction.command")

// Entrypoint is an adapter binding operation. The supplied GatewayAccess is
// the only Agent capability it may retain; it never receives Runtime or Store
// objects. Listener lifecycle remains the owning Module's responsibility.
type Entrypoint interface {
	Attach(GatewayAccess) error
}

// GatewayAccess is the fixed, transport-neutral backend boundary. Every
// method returns IDs, revisions, snapshots, receipts, or events—never an
// AgentRuntime pointer or a storage implementation.
type GatewayAccess interface {
	ListSessions(context.Context, ListSessionsRequest) (SessionList, error)
	CreateSession(context.Context, CreateSessionRequest) (SessionOpened, error)
	ResumeSession(context.Context, ResumeSessionRequest) (SessionOpened, error)
	ForkSession(context.Context, ForkSessionRequest) (SessionOpened, error)
	StartSessionFromSummary(context.Context, SummarySessionRequest) (SessionOpened, error)
	Send(context.Context, SendRequest) (EnqueueReceipt, error)
	Steer(context.Context, SteerRequest) (EnqueueReceipt, error)
	RunPending(context.Context, RunPendingRequest) (RunReceipt, error)
	Cancel(context.Context, CancelRequest) error
	WhenIdle(context.Context, WhenIdleRequest) error
	EditQueued(context.Context, EditQueuedRequest) (CommitReceipt, error)
	DeleteQueued(context.Context, DeleteQueuedRequest) (CommitReceipt, error)
	ReclassifyQueued(context.Context, ReclassifyQueuedRequest) (CommitReceipt, error)
	ModelConfig(context.Context, ModelConfigRequest) (ModelConfigView, error)
	UpdateModelConfig(context.Context, UpdateModelConfigRequest) (CommitReceipt, error)
	Snapshot(context.Context, SnapshotRequest) (SessionSnapshot, error)
	Subscribe(context.Context, SubscribeRequest) (EventStream, error)
	Commands(context.Context, CommandScope) ([]CommandDescriptor, error)
	InvokeCommand(context.Context, CommandInvocation) (CommandResult, error)
	CloseSession(context.Context, CloseSessionRequest) error
}

type ListSessionsRequest struct {
	AgentID     agent.AgentID
	WorkspaceID agent.WorkspaceID
}

type SessionList struct {
	Sessions []SessionSummary
}

type SessionSummary struct {
	SessionID   agent.SessionID
	AgentID     agent.AgentID
	WorkspaceID agent.WorkspaceID
	Revision    agent.Revision
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
	AgentID         agent.AgentID
	WorkspaceID     agent.WorkspaceID
	Mode            session.ForkMode
	ModelConfig     *session.SessionModelConfig
}

type SummarySessionRequest struct {
	AgentID     agent.AgentID
	WorkspaceID agent.WorkspaceID
	Messages    []agent.Message
	ModelConfig *session.SessionModelConfig
}

type SessionOpened struct {
	SessionID agent.SessionID
	Revision  agent.Revision
}

type SendRequest struct {
	SessionID        agent.SessionID
	ExpectedRevision agent.Revision
	Message          agent.Message
}

type SteerRequest struct {
	SessionID        agent.SessionID
	ExpectedRevision agent.Revision
	Message          agent.Message
}

type EnqueueReceipt struct {
	MessageID agent.MessageID
	Revision  agent.Revision
}

type RunPendingRequest struct {
	SessionID agent.SessionID
}

type RunReceipt struct {
	SessionID agent.SessionID
	RunID     agent.RunID
	Revision  agent.Revision
}

type CancelRequest struct {
	SessionID agent.SessionID
}

type WhenIdleRequest struct {
	SessionID agent.SessionID
}

type EditQueuedRequest struct {
	SessionID        agent.SessionID
	MessageID        agent.MessageID
	ExpectedRevision agent.Revision
	Message          agent.Message
}

type DeleteQueuedRequest struct {
	SessionID        agent.SessionID
	MessageID        agent.MessageID
	ExpectedRevision agent.Revision
}

type ReclassifyQueuedRequest struct {
	SessionID        agent.SessionID
	MessageID        agent.MessageID
	ExpectedRevision agent.Revision
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
	Config                  session.SessionModelConfig
	AcceptCompatibilityLoss bool
}

type CommitReceipt struct {
	SessionID agent.SessionID
	Revision  agent.Revision
}

type SnapshotRequest struct {
	SessionID agent.SessionID
	Revision  agent.Revision
}

type SubscribeRequest struct {
	SessionID     agent.SessionID
	AfterRevision agent.Revision
}

// EventStream is the reconnectable Gateway event boundary. Temporary chunks
// are not durable cursors; a reconnect starts from the requested snapshot
// revision and then receives available durable events.
type EventStream interface {
	Recv(context.Context) (Event, error)
	Close() error
}

type Event struct {
	Kind      EventKind
	SessionID agent.SessionID
	Revision  agent.Revision
	Message   *agent.Message
	Text      string
}

type EventKind string

const (
	EventChunk     EventKind = "chunk"
	EventReset     EventKind = "reset"
	EventCommitted EventKind = "committed"
	EventState     EventKind = "state"
)

// SessionSnapshot is a client-facing projection, not the Session aggregate.
// Implementations may add event metadata without exposing internal state.
type SessionSnapshot struct {
	SessionID agent.SessionID
	Revision  agent.Revision
	Messages  []agent.Message
}

type CloseSessionRequest struct {
	SessionID agent.SessionID
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
	Arguments        json.RawMessage
}

// CommandActions exposes only fixed backend actions. It intentionally has no
// InvokeCommand method, preventing a command from recursively dispatching
// another command through the same Gateway.
type CommandActions interface {
	Apply(context.Context, ActionRequest) (ActionResult, error)
}

type ActionRequest struct {
	Kind                    ActionKind
	Scope                   CommandScope
	ExpectedRevision        agent.Revision
	Message                 agent.Message
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
