package workflow

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
)

var ErrJobVersionConflict = errors.New("workflow: job version conflict")

func NewMemoryJobStoreModule() agentslot.Module {
	return memoryJobStoreModule{store: NewMemoryJobStore()}
}

func NewMemoryMailboxModule() agentslot.Module {
	return memoryMailboxModule{mailbox: NewMemoryMailbox()}
}

type memoryJobStoreModule struct{ store *MemoryJobStore }

func (memoryJobStoreModule) ID() string { return "job.store.memory" }
func (m memoryJobStoreModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Set(JobStoreSlot, JobStore(m.store)))
}

type memoryMailboxModule struct{ mailbox *MemoryMailbox }

func (memoryMailboxModule) ID() string { return "mailbox.memory" }
func (m memoryMailboxModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Set(MailboxSlot, Mailbox(m.mailbox)))
}

type MemoryJobStore struct {
	mu      sync.Mutex
	jobs    map[string]Job
	changed map[string]chan struct{}
}

func NewMemoryJobStore() *MemoryJobStore {
	return &MemoryJobStore{jobs: make(map[string]Job), changed: make(map[string]chan struct{})}
}

func (s *MemoryJobStore) Create(_ context.Context, job Job) error {
	if err := job.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.jobs[job.ID]; exists {
		return errors.New("workflow: duplicate job")
	}
	s.jobs[job.ID] = cloneJob(job)
	s.signalLocked(job.ID)
	return nil
}

func (s *MemoryJobStore) Get(_ context.Context, id string) (Job, bool, error) {
	if id == "" {
		return Job{}, false, errors.New("workflow: JobID is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	return cloneJob(job), ok, nil
}

func (s *MemoryJobStore) List(_ context.Context, query JobQuery) ([]Job, error) {
	if query.ParentSessionID != "" && !query.ParentSessionID.Valid() {
		return nil, errors.New("workflow: invalid parent SessionID")
	}
	if query.Status != "" && !query.Status.Valid() {
		return nil, errors.New("workflow: invalid JobStatus")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Job, 0)
	for _, job := range s.jobs {
		if query.ParentSessionID != "" && job.Parent.SessionID != query.ParentSessionID || query.Status != "" && job.Status != query.Status {
			continue
		}
		result = append(result, cloneJob(job))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result, nil
}

func (s *MemoryJobStore) Update(_ context.Context, update JobUpdate) (Job, error) {
	if update.JobID == "" || update.ExpectedVersion == 0 || !update.Status.Valid() {
		return Job{}, errors.New("workflow: invalid job update")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[update.JobID]
	if !ok {
		return Job{}, errors.New("workflow: job not found")
	}
	if job.Version != update.ExpectedVersion {
		return Job{}, ErrJobVersionConflict
	}
	if job.Status.Terminal() {
		if job.Status == update.Status {
			return cloneJob(job), nil
		}
		return Job{}, errors.New("workflow: terminal job cannot transition")
	}
	if !validJobTransition(job.Status, update.Status) {
		return Job{}, errors.New("workflow: invalid job transition")
	}
	job.Status = update.Status
	job.Result = cloneResult(update.Result)
	job.ErrorCode = update.ErrorCode
	job.TerminalReason = update.TerminalReason
	job.Version++
	job.UpdatedAt = time.Now().UTC()
	if err := job.Validate(); err != nil {
		return Job{}, err
	}
	s.jobs[job.ID] = job
	s.signalLocked(job.ID)
	return cloneJob(job), nil
}

func (s *MemoryJobStore) Wait(ctx context.Context, id string, after uint64) (Job, error) {
	for {
		s.mu.Lock()
		job, ok := s.jobs[id]
		if !ok {
			s.mu.Unlock()
			return Job{}, errors.New("workflow: job not found")
		}
		if job.Version > after || job.Status.Terminal() {
			s.mu.Unlock()
			return cloneJob(job), nil
		}
		changed := s.changed[id]
		if changed == nil {
			changed = make(chan struct{})
			s.changed[id] = changed
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return Job{}, ctx.Err()
		case <-changed:
		}
	}
}

func (s *MemoryJobStore) signalLocked(id string) {
	if changed := s.changed[id]; changed != nil {
		close(changed)
	}
	s.changed[id] = make(chan struct{})
}

type MemoryMailbox struct {
	mu       sync.Mutex
	sequence atomic.Uint64
	messages map[Address][]Message
	changed  map[Address]chan struct{}
}

func NewMemoryMailbox() *MemoryMailbox {
	return &MemoryMailbox{messages: make(map[Address][]Message), changed: make(map[Address]chan struct{})}
}

func (m *MemoryMailbox) Publish(_ context.Context, message Message) (Message, error) {
	if message.ID == "" || !message.From.Valid() || !message.To.Valid() || message.Body == "" {
		return Message{}, errors.New("workflow: invalid mailbox publish")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.messages[message.To] {
		if existing.ID == message.ID {
			if existing.From == message.From && existing.To == message.To && existing.Body == message.Body {
				return existing, nil
			}
			return Message{}, errors.New("workflow: mailbox idempotency conflict")
		}
	}
	message.Sequence = m.sequence.Add(1)
	message.CreatedAt = time.Now().UTC()
	m.messages[message.To] = append(m.messages[message.To], message)
	if changed := m.changed[message.To]; changed != nil {
		close(changed)
	}
	m.changed[message.To] = make(chan struct{})
	return message, nil
}

func (m *MemoryMailbox) List(_ context.Context, address Address, after uint64) ([]Message, error) {
	if !address.Valid() {
		return nil, errors.New("workflow: invalid mailbox address")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return messagesAfter(m.messages[address], after), nil
}

func (m *MemoryMailbox) Wait(ctx context.Context, address Address, after uint64) ([]Message, error) {
	if !address.Valid() {
		return nil, errors.New("workflow: invalid mailbox address")
	}
	for {
		m.mu.Lock()
		messages := messagesAfter(m.messages[address], after)
		if len(messages) > 0 {
			m.mu.Unlock()
			return messages, nil
		}
		changed := m.changed[address]
		if changed == nil {
			changed = make(chan struct{})
			m.changed[address] = changed
		}
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func messagesAfter(source []Message, after uint64) []Message {
	result := make([]Message, 0)
	for _, message := range source {
		if message.Sequence > after {
			result = append(result, message)
		}
	}
	return result
}

func validJobTransition(from, to JobStatus) bool {
	return from == JobQueued && (to == JobRunning || to == JobCanceled) ||
		from == JobRunning && (to == JobCompleted || to == JobFailed || to == JobCanceled)
}

func cloneJob(job Job) Job {
	job.Result = cloneResult(job.Result)
	return job
}

func cloneResult(result Result) Result {
	result.Artifacts = append([]string(nil), result.Artifacts...)
	return result
}
