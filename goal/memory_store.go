package goal

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
)

var ErrVersionConflict = errors.New("goal: version conflict")

type MemoryStore struct {
	mu        sync.Mutex
	sequence  atomic.Uint64
	goals     map[agent.SessionID]Goal
	decisions map[string]decisionEntry
}

type decisionEntry struct {
	record DecisionRecord
	result Goal
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{goals: make(map[agent.SessionID]Goal), decisions: make(map[string]decisionEntry)}
}

func NewMemoryModule() agentslot.Module { return memoryModule{store: NewMemoryStore()} }

// NewDefaultMemoryModule contributes the in-memory Goal Store only when an
// application has not installed an explicit Store implementation.
func NewDefaultMemoryModule() agentslot.Module {
	return memoryModule{store: NewMemoryStore(), conditional: true}
}

type memoryModule struct {
	store       *MemoryStore
	conditional bool
}

func (memoryModule) ID() string { return "goal.store.memory" }
func (m memoryModule) Register(reg agentslot.Registrar) error {
	if m.conditional {
		return reg.Contribute(agentslot.SetDefault(StoreSlot, Store(m.store)))
	}
	return reg.Contribute(agentslot.Set(StoreSlot, Store(m.store)))
}

func (s *MemoryStore) Current(_ context.Context, sessionID agent.SessionID) (Goal, bool, error) {
	if !sessionID.Valid() {
		return Goal{}, false, errors.New("goal: invalid SessionID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.goals[sessionID]
	return value, ok, nil
}

func (s *MemoryStore) Set(_ context.Context, request SetRequest) (Goal, error) {
	if !request.SessionID.Valid() || strings.TrimSpace(request.Objective) == "" || strings.TrimSpace(request.Objective) != request.Objective || request.MaxFollowOns <= 0 {
		return Goal{}, errors.New("goal: invalid set request")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.goals[request.SessionID]
	if (!exists && request.ExpectedVersion != 0) || (exists && current.Version != request.ExpectedVersion) {
		return Goal{}, ErrVersionConflict
	}
	value := Goal{
		ID: fmt.Sprintf("goal-%d", s.sequence.Add(1)), SessionID: request.SessionID,
		Objective: request.Objective, Status: StatusActive, Version: request.ExpectedVersion + 1,
		MaxFollowOns: request.MaxFollowOns, UpdatedAt: time.Now().UTC(),
	}
	s.goals[request.SessionID] = value
	return value, nil
}

func (s *MemoryStore) ChangeStatus(_ context.Context, request StateChangeRequest) (Goal, error) {
	if !request.SessionID.Valid() || !request.Status.Valid() {
		return Goal{}, errors.New("goal: invalid state change")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.goals[request.SessionID]
	if !ok {
		return Goal{}, errors.New("goal: not found")
	}
	if current.Version != request.ExpectedVersion {
		return Goal{}, ErrVersionConflict
	}
	if current.Status == request.Status {
		return current, nil
	}
	if !validStatusTransition(current.Status, request.Status) {
		return Goal{}, errors.New("goal: invalid status transition")
	}
	current.Status = request.Status
	current.Version++
	current.UpdatedAt = time.Now().UTC()
	s.goals[request.SessionID] = current
	return current, nil
}

func (s *MemoryStore) RecordDecision(_ context.Context, record DecisionRecord) (Goal, error) {
	if record.ID == "" || record.GoalID == "" || !record.SessionID.Valid() || !record.RunID.Valid() ||
		!record.StepID.Valid() || record.ExpectedVersion == 0 || record.RecordedAt.IsZero() || record.Evaluation.Validate() != nil {
		return Goal{}, errors.New("goal: invalid decision record")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.decisions[record.ID]; ok {
		if reflect.DeepEqual(existing.record, record) {
			return existing.result, nil
		}
		return Goal{}, errors.New("goal: decision idempotency conflict")
	}
	current, ok := s.goals[record.SessionID]
	if !ok || current.ID != record.GoalID {
		return Goal{}, errors.New("goal: not found")
	}
	if current.Version != record.ExpectedVersion {
		return Goal{}, ErrVersionConflict
	}
	if current.Status != StatusActive {
		return Goal{}, errors.New("goal: only an active goal may accept a decision")
	}
	if record.RecordedAt.Before(current.UpdatedAt) {
		return Goal{}, errors.New("goal: decision time precedes current state")
	}
	switch record.Evaluation.Decision {
	case DecisionContinue:
		if current.FollowOns >= current.MaxFollowOns {
			return Goal{}, errors.New("goal: follow-on limit reached")
		}
		current.FollowOns++
	case DecisionBlocked:
		current.Status = StatusPaused
	case DecisionDone:
		current.Status = StatusCompleted
	}
	current.Version++
	current.UpdatedAt = record.RecordedAt.UTC()
	s.goals[record.SessionID] = current
	s.decisions[record.ID] = decisionEntry{record: record, result: current}
	return current, nil
}

func validStatusTransition(from, to Status) bool {
	return from == StatusActive && (to == StatusPaused || to == StatusCompleted || to == StatusCanceled) ||
		from == StatusPaused && (to == StatusActive || to == StatusCanceled)
}
