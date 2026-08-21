package workflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
)

func NewSchedulerModule(id string) (agentslot.Module, error) {
	return newSchedulerModule(id, false)
}

// NewDefaultSchedulerModule installs the reference Scheduler only when the
// application has not selected an explicit Scheduler implementation.
func NewDefaultSchedulerModule(id string) (agentslot.Module, error) {
	return newSchedulerModule(id, true)
}

func newSchedulerModule(id string, conditional bool) (agentslot.Module, error) {
	if id == "" {
		return nil, errors.New("workflow: scheduler module ID is required")
	}
	return &schedulerModule{id: id, conditional: conditional}, nil
}

type schedulerModule struct {
	id          string
	scheduler   *referenceScheduler
	conditional bool
}

func (m *schedulerModule) ID() string { return m.id }
func (m *schedulerModule) RequiredSlots() []agentslot.Requirement {
	return []agentslot.Requirement{
		agentslot.RequireMany(AgentProviderSlot, 1),
		agentslot.RequireOne(JobStoreSlot),
		agentslot.RequireOne(MailboxSlot),
	}
}
func (m *schedulerModule) Register(reg agentslot.Registrar) error {
	constructor := func(resolver agentslot.Resolver) (Scheduler, error) {
		providers, err := agentslot.ResolveMany(resolver, AgentProviderSlot)
		if err != nil {
			return nil, err
		}
		store, err := agentslot.ResolveOne(resolver, JobStoreSlot)
		if err != nil {
			return nil, err
		}
		mailbox, err := agentslot.ResolveOne(resolver, MailboxSlot)
		if err != nil {
			return nil, err
		}
		indexed := make(map[string]AgentProvider, len(providers))
		for _, provider := range providers {
			if provider.Value == nil {
				return nil, fmt.Errorf("workflow: nil AgentProvider %q", provider.Key)
			}
			indexed[provider.Key] = provider.Value
		}
		created := &referenceScheduler{
			providers: indexed, store: store, mailbox: mailbox,
			running: make(map[string]context.CancelFunc), cancelReasons: make(map[string]string),
		}
		m.scheduler = created
		return created, nil
	}
	if m.conditional {
		return reg.Contribute(agentslot.SetDefaultWith(SchedulerSlot, constructor))
	}
	return reg.Contribute(agentslot.SetWith(SchedulerSlot, constructor))
}

func (m *schedulerModule) Start(context.Context) error {
	if m.scheduler == nil {
		return errors.New("workflow: scheduler was not built")
	}
	m.scheduler.mu.Lock()
	defer m.scheduler.mu.Unlock()
	if m.scheduler.closed {
		return errors.New("workflow: scheduler is closed")
	}
	m.scheduler.started = true
	return nil
}

func (m *schedulerModule) Stop(ctx context.Context) error {
	if m.scheduler != nil {
		return m.scheduler.stop(ctx)
	}
	return nil
}

type referenceScheduler struct {
	providers map[string]AgentProvider
	store     JobStore
	mailbox   Mailbox
	sequence  atomic.Uint64

	mu            sync.Mutex
	running       map[string]context.CancelFunc
	cancelReasons map[string]string
	started       bool
	closed        bool
	wait          sync.WaitGroup
}

func (s *referenceScheduler) Spawn(ctx context.Context, request SpawnRequest) (Job, error) {
	if request.ProviderKey == "" || !request.Parent.Valid() || request.Instruction == "" {
		return Job{}, errors.New("workflow: invalid spawn request")
	}
	provider, ok := s.providers[request.ProviderKey]
	if !ok {
		return Job{}, fmt.Errorf("workflow: unknown AgentProvider %q", request.ProviderKey)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Job{}, errors.New("workflow: scheduler is closed")
	}
	if !s.started {
		s.mu.Unlock()
		return Job{}, errors.New("workflow: scheduler is not started")
	}
	jobID := fmt.Sprintf("job-%d", s.sequence.Add(1))
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.running[jobID] = cancel
	s.wait.Add(1)
	s.mu.Unlock()
	now := time.Now().UTC()
	job := Job{
		ID: jobID, ProviderKey: request.ProviderKey, Parent: request.Parent, Instruction: request.Instruction,
		Status: JobQueued, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.Create(ctx, job); err != nil {
		cancel()
		s.removeRunning(jobID)
		s.wait.Done()
		return Job{}, err
	}
	task := Task{
		JobID: jobID, ProviderKey: request.ProviderKey, Parent: request.Parent,
		Instruction: request.Instruction, Metadata: cloneMetadata(request.Metadata),
	}
	go func() {
		defer s.wait.Done()
		s.execute(runCtx, provider, task)
	}()
	return job, nil
}

func (s *referenceScheduler) execute(ctx context.Context, provider AgentProvider, task Task) {
	job, ok, err := s.store.Get(context.Background(), task.JobID)
	if err != nil || !ok {
		s.removeRunning(task.JobID)
		return
	}
	job, err = s.store.Update(context.Background(), JobUpdate{JobID: job.ID, ExpectedVersion: job.Version, Status: JobRunning})
	if err != nil {
		s.removeRunning(task.JobID)
		return
	}
	result, executeErr := executeProvider(ctx, provider, task, jobInbox{mailbox: s.mailbox, jobID: task.JobID})
	status := JobCompleted
	errorCode := ""
	terminalReason := ""
	if executeErr != nil {
		status = JobFailed
		errorCode = "agent_provider_failed"
		if errors.Is(executeErr, context.Canceled) {
			status = JobCanceled
			errorCode = ""
			terminalReason = s.cancelReason(task.JobID)
			if terminalReason == "" {
				terminalReason = "provider_canceled"
			}
		}
	}
	terminal, updateErr := s.store.Update(context.Background(), JobUpdate{
		JobID: job.ID, ExpectedVersion: job.Version, Status: status, Result: result,
		ErrorCode: errorCode, TerminalReason: terminalReason,
	})
	if updateErr == nil {
		body := terminal.Result.Summary
		if body == "" {
			body = string(terminal.Status)
		}
		_, _ = s.mailbox.Publish(context.Background(), Message{
			ID:   "completion-" + terminal.ID,
			From: Address{Kind: AddressJob, ID: terminal.ID},
			To:   Address{Kind: AddressSession, ID: string(terminal.Parent.SessionID)}, Body: body,
		})
	}
	s.removeRunning(task.JobID)
}

func executeProvider(ctx context.Context, provider AgentProvider, task Task, inbox Inbox) (result Result, err error) {
	defer func() {
		if recover() != nil {
			result = Result{}
			err = errors.New("workflow: AgentProvider panicked")
		}
	}()
	return provider.Execute(ctx, task, inbox)
}

func (s *referenceScheduler) Get(ctx context.Context, id string) (Job, bool, error) {
	return s.store.Get(ctx, id)
}

func (s *referenceScheduler) List(ctx context.Context, query JobQuery) ([]Job, error) {
	return s.store.List(ctx, query)
}

func (s *referenceScheduler) Wait(ctx context.Context, id string, after uint64) (Job, error) {
	return s.store.Wait(ctx, id, after)
}

func (s *referenceScheduler) Send(ctx context.Context, request SendRequest) (Message, error) {
	if request.JobID == "" || request.Body == "" {
		return Message{}, errors.New("workflow: invalid send request")
	}
	job, ok, err := s.store.Get(ctx, request.JobID)
	if err != nil || !ok {
		if err == nil {
			err = errors.New("workflow: job not found")
		}
		return Message{}, err
	}
	if job.Status.Terminal() {
		return Message{}, errors.New("workflow: cannot send to a terminal job")
	}
	return s.mailbox.Publish(ctx, Message{
		ID:   fmt.Sprintf("message-%s-%d", request.JobID, s.sequence.Add(1)),
		From: Address{Kind: AddressSession, ID: string(job.Parent.SessionID)},
		To:   Address{Kind: AddressJob, ID: request.JobID}, Body: request.Body,
	})
}

func (s *referenceScheduler) Close(ctx context.Context, request CloseRequest) (Job, error) {
	if request.JobID == "" || request.Reason == "" {
		return Job{}, errors.New("workflow: invalid close request")
	}
	job, ok, err := s.store.Get(ctx, request.JobID)
	if err != nil || !ok {
		if err == nil {
			err = errors.New("workflow: job not found")
		}
		return Job{}, err
	}
	if job.Status.Terminal() {
		return job, nil
	}
	s.mu.Lock()
	cancel := s.running[request.JobID]
	if cancel != nil {
		s.cancelReasons[request.JobID] = request.Reason
	}
	s.mu.Unlock()
	if cancel == nil {
		return Job{}, errors.New("workflow: job runtime is detached")
	}
	cancel()
	return s.store.Wait(ctx, request.JobID, job.Version)
}

func (s *referenceScheduler) removeRunning(id string) {
	s.mu.Lock()
	delete(s.running, id)
	delete(s.cancelReasons, id)
	s.mu.Unlock()
}

func (s *referenceScheduler) cancelReason(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancelReasons[id]
}

func (s *referenceScheduler) stop(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	cancels := make([]context.CancelFunc, 0, len(s.running))
	for id, cancel := range s.running {
		s.cancelReasons[id] = "scheduler_stopped"
		cancels = append(cancels, cancel)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		s.wait.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type jobInbox struct {
	mailbox Mailbox
	jobID   string
}

func (i jobInbox) Receive(ctx context.Context, after uint64) ([]Message, error) {
	return i.mailbox.Wait(ctx, Address{Kind: AddressJob, ID: i.jobID}, after)
}

func cloneMetadata(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
