// Package workflow defines portable multi-agent task, scheduling, durable job,
// and addressed mailbox boundaries. The fixed per-Session AgentRuntime remains
// unchanged; workflow components coordinate work outside that ownership.
package workflow

import (
	"context"
	"errors"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
)

var (
	AgentProviderSlot = agentslot.Many[AgentProvider]("agent.provider")
	SchedulerSlot     = agentslot.One[Scheduler]("workflow.scheduler")
	JobStoreSlot      = agentslot.One[JobStore]("job.store")
	MailboxSlot       = agentslot.One[Mailbox]("mailbox")
)

type JobStatus string

const (
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running"
	JobCompleted JobStatus = "completed"
	JobFailed    JobStatus = "failed"
	JobCanceled  JobStatus = "canceled"
)

func (s JobStatus) Valid() bool {
	return s == JobQueued || s == JobRunning || s == JobCompleted || s == JobFailed || s == JobCanceled
}

func (s JobStatus) Terminal() bool { return s == JobCompleted || s == JobFailed || s == JobCanceled }

type Parent struct {
	SessionID agent.SessionID
	RunID     agent.RunID
	StepID    agent.StepID
}

func (p Parent) Valid() bool { return p.SessionID.Valid() && p.RunID.Valid() && p.StepID.Valid() }

type Task struct {
	JobID       string
	ProviderKey string
	Parent      Parent
	Instruction string
	Metadata    map[string]string
}

func (t Task) Validate() error {
	if t.JobID == "" || t.ProviderKey == "" || !t.Parent.Valid() || t.Instruction == "" {
		return errors.New("workflow: invalid task")
	}
	return nil
}

type Result struct {
	Summary   string
	Artifacts []string
}

type AgentProvider interface {
	Execute(context.Context, Task, Inbox) (Result, error)
}

type Inbox interface {
	Receive(context.Context, uint64) ([]Message, error)
}

type Job struct {
	ID             string
	ProviderKey    string
	Parent         Parent
	Instruction    string
	Status         JobStatus
	Version        uint64
	Result         Result
	ErrorCode      string
	TerminalReason string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (j Job) Validate() error {
	if j.ID == "" || j.ProviderKey == "" || !j.Parent.Valid() || j.Instruction == "" || !j.Status.Valid() ||
		j.Version == 0 || j.CreatedAt.IsZero() || j.UpdatedAt.IsZero() {
		return errors.New("workflow: invalid job")
	}
	if j.Status == JobFailed && j.ErrorCode == "" {
		return errors.New("workflow: failed job requires an error code")
	}
	if j.Status != JobFailed && j.ErrorCode != "" {
		return errors.New("workflow: only failed jobs may contain an error code")
	}
	if j.Status == JobCanceled && j.TerminalReason == "" {
		return errors.New("workflow: canceled job requires a terminal reason")
	}
	if j.Status != JobCanceled && j.TerminalReason != "" {
		return errors.New("workflow: only canceled jobs may contain a terminal reason")
	}
	return nil
}

type JobQuery struct {
	ParentSessionID agent.SessionID
	Status          JobStatus
}

type JobUpdate struct {
	JobID           string
	ExpectedVersion uint64
	Status          JobStatus
	Result          Result
	ErrorCode       string
	TerminalReason  string
}

type JobStore interface {
	Create(context.Context, Job) error
	Get(context.Context, string) (Job, bool, error)
	List(context.Context, JobQuery) ([]Job, error)
	Update(context.Context, JobUpdate) (Job, error)
	Wait(context.Context, string, uint64) (Job, error)
}

type AddressKind string

const (
	AddressSession AddressKind = "session"
	AddressJob     AddressKind = "job"
)

func (k AddressKind) Valid() bool { return k == AddressSession || k == AddressJob }

type Address struct {
	Kind AddressKind
	ID   string
}

func (a Address) Valid() bool { return a.Kind.Valid() && a.ID != "" }

type Message struct {
	ID        string
	Sequence  uint64
	From      Address
	To        Address
	Body      string
	CreatedAt time.Time
}

func (m Message) Validate() error {
	if m.ID == "" || m.Sequence == 0 || !m.From.Valid() || !m.To.Valid() || m.Body == "" || m.CreatedAt.IsZero() {
		return errors.New("workflow: invalid mailbox message")
	}
	return nil
}

type Mailbox interface {
	Publish(context.Context, Message) (Message, error)
	List(context.Context, Address, uint64) ([]Message, error)
	Wait(context.Context, Address, uint64) ([]Message, error)
}

type SpawnRequest struct {
	ProviderKey string
	Parent      Parent
	Instruction string
	Metadata    map[string]string
}

type SendRequest struct {
	JobID string
	Body  string
}

type CloseRequest struct {
	JobID  string
	Reason string
}

type Scheduler interface {
	Spawn(context.Context, SpawnRequest) (Job, error)
	Get(context.Context, string) (Job, bool, error)
	List(context.Context, JobQuery) ([]Job, error)
	Wait(context.Context, string, uint64) (Job, error)
	Send(context.Context, SendRequest) (Message, error)
	Close(context.Context, CloseRequest) (Job, error)
}
