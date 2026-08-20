package session

// This file is the reference in-memory implementation of the Session
// contracts. It is deliberately a small aggregate store, not a production
// database: it proves the transaction, revision, fork, and recovery semantics
// that durable implementations must preserve.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/tool"
)

type memoryAggregate struct {
	snapshot    Snapshot
	idempotency map[string]memoryCommit
}

type memoryCommit struct {
	fingerprint string
	commit      Commit
}

// MemoryStore is a concurrency-safe reference SessionStore. Each successful
// Commit publishes one detached aggregate revision atomically. The map and
// aggregate values are never exposed to callers.
type MemoryStore struct {
	mu       sync.Mutex
	sessions map[agent.SessionID]*memoryAggregate
}

var _ SessionStore = (*MemoryStore)(nil)

// NewMemoryStore creates an empty in-memory SessionStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{sessions: make(map[agent.SessionID]*memoryAggregate)}
}

func (s *MemoryStore) Create(ctx context.Context, initial NewSession) (Snapshot, error) {
	if err := contextErr(ctx, "session.create"); err != nil {
		return Snapshot{}, err
	}
	if err := validateNewSession(initial); err != nil {
		return Snapshot{}, err
	}
	if initial.RunState == "" {
		initial.RunState = RunIdle
	}
	history, err := prepareInitialHistory(initial.Session.ID, initial.History)
	if err != nil {
		return Snapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.sessions[initial.Session.ID]; exists {
		return Snapshot{}, agent.NewCodedError(agent.ErrorConflict, agent.CodeSessionAlreadyExists, "session.create", "session already exists", nil)
	}
	if s.sessions == nil {
		s.sessions = make(map[agent.SessionID]*memoryAggregate)
	}
	copy := cloneSnapshot(Snapshot{
		Session:     initial.Session,
		Revision:    1,
		History:     history,
		Context:     initial.Context,
		Queue:       initial.Queue,
		RunJournal:  initial.RunJournal,
		ModelConfig: initial.ModelConfig,
		RunState:    initial.RunState,
		ActiveRunID: initial.ActiveRunID,
	})
	copy.Session.Revision = copy.Revision
	s.sessions[copy.Session.ID] = &memoryAggregate{snapshot: copy, idempotency: make(map[string]memoryCommit)}
	return cloneSnapshot(copy), nil
}

func (s *MemoryStore) HistoryPage(ctx context.Context, request HistoryPageRequest) (HistoryPage, error) {
	if err := contextErr(ctx, "session.history_page"); err != nil {
		return HistoryPage{}, err
	}
	if !request.SessionID.Valid() {
		return HistoryPage{}, invalid("session.history_page", "session ID is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	aggregate, ok := s.sessions[request.SessionID]
	if !ok {
		return HistoryPage{}, agent.NewCodedError(agent.ErrorNotFound, agent.CodeSessionNotFound, "session.history_page", "session not found", nil)
	}
	return historyPage(aggregate.snapshot.History, request)
}

func (s *MemoryStore) Load(ctx context.Context, ref SessionRef) (Snapshot, error) {
	if err := contextErr(ctx, "session.load"); err != nil {
		return Snapshot{}, err
	}
	if !ref.SessionID.Valid() {
		return Snapshot{}, invalid("session.load", "session ID is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	aggregate, ok := s.sessions[ref.SessionID]
	if !ok {
		return Snapshot{}, agent.NewCodedError(agent.ErrorNotFound, agent.CodeSessionNotFound, "session.load", "session not found", nil)
	}
	return cloneSnapshot(aggregate.snapshot), nil
}

// Recover atomically resolves work that may have crossed a process-crash
// boundary. Ordinary Load is deliberately read-only so viewing a legitimately
// running Session cannot be mistaken for crash recovery.
func (s *MemoryStore) Recover(ctx context.Context, ref SessionRef) (Snapshot, error) {
	if err := contextErr(ctx, "session.recover"); err != nil {
		return Snapshot{}, err
	}
	if !ref.SessionID.Valid() {
		return Snapshot{}, invalid("session.recover", "session ID is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	aggregate, ok := s.sessions[ref.SessionID]
	if !ok {
		return Snapshot{}, agent.NewCodedError(agent.ErrorNotFound, agent.CodeSessionNotFound, "session.recover", "session not found", nil)
	}
	changed, err := recoverAggregate(&aggregate.snapshot)
	if err != nil {
		return Snapshot{}, err
	}
	if changed {
		aggregate.snapshot.Revision++
		aggregate.snapshot.Session.Revision = aggregate.snapshot.Revision
	}
	return cloneSnapshot(aggregate.snapshot), nil
}

func (s *MemoryStore) Commit(ctx context.Context, request CommitRequest) (Commit, error) {
	if err := contextErr(ctx, "session.commit"); err != nil {
		return Commit{}, err
	}
	if err := request.Validate(); err != nil {
		return Commit{}, invalid("session.commit", err.Error())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	aggregate, ok := s.sessions[request.SessionID]
	if !ok {
		return Commit{}, agent.NewCodedError(agent.ErrorNotFound, agent.CodeSessionNotFound, "session.commit", "session not found", nil)
	}
	fingerprint, err := requestFingerprint(request)
	if err != nil {
		return Commit{}, agent.NewError(agent.ErrorInternal, "session.commit", "cannot fingerprint idempotent request", err)
	}
	if previous, exists := aggregate.idempotency[request.IdempotencyKey]; exists {
		if previous.fingerprint != fingerprint {
			return Commit{}, agent.NewCodedError(agent.ErrorConflict, agent.CodeRevisionConflict, "session.commit", "idempotency key was already used for another request", nil)
		}
		result := previous.commit
		result.Applied = false
		return result, nil
	}
	if request.ExpectedRevision != aggregate.snapshot.Revision {
		return Commit{}, agent.NewCodedError(agent.ErrorConflict, agent.CodeRevisionConflict, "session.commit", "expected revision does not match", nil)
	}
	working := cloneSnapshot(aggregate.snapshot)
	if err := applyChanges(&working, request); err != nil {
		return Commit{}, err
	}
	working.Revision++
	working.Session.Revision = working.Revision
	aggregate.snapshot = working
	result := Commit{SessionID: request.SessionID, Revision: working.Revision, Applied: true}
	aggregate.idempotency[request.IdempotencyKey] = memoryCommit{fingerprint: fingerprint, commit: result}
	return result, nil
}

func validateNewSession(initial NewSession) error {
	if !initial.Session.ID.Valid() || !initial.Session.AgentID.Valid() || !initial.Session.WorkspaceID.Valid() {
		return invalid("session.create", "session identity is incomplete")
	}
	if initial.Session.Revision != 0 {
		return invalid("session.create", "new session revision must be zero")
	}
	if initial.Session.ParentSessionID == initial.Session.ID {
		return invalid("session.create", "session cannot be its own parent")
	}
	if initial.Session.ParentSessionID != "" && !initial.Session.ParentSessionID.Valid() {
		return invalid("session.create", "parent session ID is invalid")
	}
	if initial.Session.ParentSessionID == "" && initial.Session.ParentRevision != 0 {
		return invalid("session.create", "parent revision requires a parent session")
	}
	if initial.Session.ParentSessionID != "" && initial.Session.ParentRevision == 0 {
		return invalid("session.create", "derived session requires a parent revision")
	}
	if err := initial.ModelConfig.Validate(); err != nil {
		return invalid("session.create", fmt.Sprintf("invalid model config: %v", err))
	}
	if initial.RunState == "" {
		initial.RunState = RunIdle
	}
	if !initial.RunState.Valid() {
		return invalid("session.create", "invalid run state")
	}
	if initial.RunState == RunRunning && !initial.ActiveRunID.Valid() {
		return invalid("session.create", "running session requires an active run")
	}
	if initial.RunState == RunIdle && initial.ActiveRunID != "" {
		return invalid("session.create", "idle session cannot have an active run")
	}
	for _, fact := range initial.History {
		if err := fact.validatePayload(initial.Session.ID); err != nil {
			return invalid("session.create", err.Error())
		}
	}
	for _, item := range initial.Queue {
		if !validQueueItem(item, initial.Session.ID) {
			return invalid("session.create", "invalid queue item")
		}
		if item.ClaimedBy != "" && !item.ClaimedBy.Valid() {
			return invalid("session.create", "queue item has an invalid claiming run")
		}
		if item.Claimed() && (initial.RunState != RunRunning || item.ClaimedBy != initial.ActiveRunID) {
			return invalid("session.create", "claimed queue item must belong to the active run")
		}
	}
	if duplicateQueueMessage(initial.Queue) {
		return invalid("session.create", "queue contains duplicate message IDs")
	}
	for _, entry := range initial.RunJournal {
		if err := entry.Validate(initial.Session.ID); err != nil {
			return invalid("session.create", err.Error())
		}
		if entry.Status == JournalPending && (initial.RunState != RunRunning || entry.RunID != initial.ActiveRunID) {
			return invalid("session.create", "pending journal must belong to the active run")
		}
	}
	if err := validateContext(initial.Context, initial.Session.ID); err != nil {
		return err
	}
	if initial.Context.Version != 0 || initial.Context.SourceRevision != 0 || initial.Context.SourceHistorySequence != 0 || initial.Context.TokenCount != 0 || len(initial.Context.Inputs) != 0 {
		return invalid("session.create", "new session context must be empty and unversioned")
	}
	if err := validateHistoryConsistency(initial.Session.ID, initial.History, initial.RunJournal); err != nil {
		return invalid("session.create", err.Error())
	}
	return nil
}

func contextErr(ctx context.Context, op string) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		err := ctx.Err()
		if errors.Is(err, context.DeadlineExceeded) {
			return agent.NewError(agent.ErrorDeadline, op, "operation deadline exceeded", err)
		}
		return agent.NewCodedError(agent.ErrorCanceled, agent.CodeCanceled, op, "operation canceled", err)
	default:
		return nil
	}
}

func invalid(op, message string) error {
	return agent.NewError(agent.ErrorInvalidInput, op, message, nil)
}

func requestFingerprint(request CommitRequest) (string, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

func applyChanges(snapshot *Snapshot, request CommitRequest) error {
	for _, change := range request.Changes {
		if err := change.Validate(request.SessionID); err != nil {
			return invalid("session.commit", err.Error())
		}
	}
	if err := validateToolTransactions(snapshot.History, request.Changes); err != nil {
		return err
	}
	if err := validateModelConfigTransaction(snapshot.ModelConfig, request.Changes); err != nil {
		return err
	}
	for _, change := range request.Changes {
		switch change.Kind {
		case AppendMessage:
			if containsMessage(snapshot.History, change.Message.ID) {
				return historyConflict("message ID already exists")
			}
			message := cloneMessage(*change.Message)
			if err := appendHistoryFact(&snapshot.History, request.SessionID, HistoryFact{Message: &message}, request.Actor); err != nil {
				return historyConflict(err.Error())
			}
		case AppendToolCall:
			if containsToolCall(snapshot.History, change.ToolCall.ID) {
				return historyConflict("tool call ID already exists")
			}
			parent := findMessage(snapshot.History, change.ToolCall.MessageID)
			if parent == nil {
				return historyConflict("tool call references an unknown assistant message")
			}
			if parent.Role != agent.RoleAssistant || parent.RunID != change.ToolCall.RunID || parent.StepID != change.ToolCall.StepID {
				return historyConflict("tool call containment differs from its assistant message")
			}
			call := cloneToolCall(*change.ToolCall)
			if err := appendHistoryFact(&snapshot.History, request.SessionID, HistoryFact{ToolCall: &call}, request.Actor); err != nil {
				return historyConflict(err.Error())
			}
		case AppendToolResult:
			if _, ok := findToolCall(snapshot.History, change.ToolResult.CallID); !ok {
				return historyConflict("tool result has no preceding tool call")
			}
			if hasToolResult(snapshot.History, change.ToolResult.CallID) {
				return historyConflict("tool call already has a terminal result")
			}
			result := cloneToolResult(*change.ToolResult)
			if err := appendHistoryFact(&snapshot.History, request.SessionID, HistoryFact{ToolResult: &result}, request.Actor); err != nil {
				return historyConflict(err.Error())
			}
		case AppendRunFact:
			if err := appendRunFact(snapshot, *change.RunFact, request.Actor); err != nil {
				return err
			}
		case AppendModelAttempt:
			if err := appendModelAttemptFact(snapshot, *change.ModelAttempt, request.Actor); err != nil {
				return err
			}
		case AppendContextContribution:
			contribution := *change.ContextContribution
			if err := appendHistoryFact(&snapshot.History, request.SessionID, HistoryFact{ContextContribution: &contribution}, request.Actor); err != nil {
				return historyConflict(err.Error())
			}
		case AppendRunBudgetExceeded:
			budget := *change.RunBudgetExceeded
			if err := appendHistoryFact(&snapshot.History, request.SessionID, HistoryFact{RunBudgetExceeded: &budget}, request.Actor); err != nil {
				return historyConflict(err.Error())
			}
		case AppendSessionEvent:
			event := cloneSessionEvent(*change.SessionEvent)
			event.Revision = snapshot.Revision.Next()
			snapshot.Events = append(snapshot.Events, event)
			modelChange := *event.ModelConfigChanged
			if err := appendHistoryFact(&snapshot.History, request.SessionID, HistoryFact{ModelConfigChanged: &modelChange}, request.Actor); err != nil {
				return historyConflict(err.Error())
			}
		case EnqueueMessage:
			if change.QueueItem.Claimed() {
				return invalid("session.commit", "new queue item cannot already be claimed")
			}
			if queueIndex(snapshot.Queue, change.QueueItem.Message.ID) >= 0 {
				return agent.NewCodedError(agent.ErrorConflict, agent.CodeQueueItemAlreadyExists, "session.commit", "queue message ID already exists", nil)
			}
			snapshot.Queue = append(snapshot.Queue, cloneQueueItem(*change.QueueItem))
		case ClaimQueue:
			index := queueIndex(snapshot.Queue, change.QueueClaim.MessageID)
			if index < 0 {
				return queueNotFound("queue item not found")
			}
			if snapshot.Queue[index].Claimed() {
				return agent.NewCodedError(agent.ErrorConflict, agent.CodeQueueItemClaimed, "session.commit", "queue item is already claimed", nil)
			}
			if snapshot.RunState != RunRunning || snapshot.ActiveRunID != change.QueueClaim.RunID {
				return agent.NewCodedError(agent.ErrorConflict, agent.CodeNoActiveRun, "session.commit", "queue claim must belong to the active run", nil)
			}
			snapshot.Queue[index].ClaimedBy = change.QueueClaim.RunID
		case ConsumeQueue:
			index := queueIndex(snapshot.Queue, change.QueueConsume.MessageID)
			if index < 0 {
				return queueNotFound("queue item not found")
			}
			if snapshot.Queue[index].ClaimedBy != change.QueueConsume.RunID {
				return agent.NewCodedError(agent.ErrorConflict, agent.CodeQueueItemClaimed, "session.commit", "queue item is not claimed by this run", nil)
			}
			snapshot.Queue = append(snapshot.Queue[:index], snapshot.Queue[index+1:]...)
		case EditQueue:
			index := queueIndex(snapshot.Queue, change.QueueEdit.MessageID)
			if index < 0 {
				return queueNotFound("queue item not found")
			}
			if snapshot.Queue[index].Claimed() {
				return agent.NewCodedError(agent.ErrorConflict, agent.CodeQueueItemClaimed, "session.commit", "claimed queue item cannot be edited", nil)
			}
			snapshot.Queue[index].Message.Parts = cloneParts(change.QueueEdit.Input.Parts)
			snapshot.Queue[index].Delivery = change.QueueEdit.Delivery
		case DeleteQueue:
			index := queueIndex(snapshot.Queue, change.QueueDelete.MessageID)
			if index < 0 {
				return queueNotFound("queue item not found")
			}
			if snapshot.Queue[index].Claimed() {
				return agent.NewCodedError(agent.ErrorConflict, agent.CodeQueueItemClaimed, "session.commit", "claimed queue item cannot be deleted", nil)
			}
			snapshot.Queue = append(snapshot.Queue[:index], snapshot.Queue[index+1:]...)
		case ReclassifyQueue:
			index := queueIndex(snapshot.Queue, change.QueueReclassification.MessageID)
			if index < 0 {
				return queueNotFound("queue item not found")
			}
			if snapshot.Queue[index].Claimed() {
				return agent.NewCodedError(agent.ErrorConflict, agent.CodeQueueItemClaimed, "session.commit", "claimed queue item cannot be reclassified", nil)
			}
			snapshot.Queue[index].Delivery = change.QueueReclassification.Delivery
		case SetContext:
			if change.Context.SourceRevision != request.ExpectedRevision {
				return agent.NewCodedError(agent.ErrorConflict, agent.CodeRevisionConflict, "session.commit", "context source revision must match the committed aggregate", nil)
			}
			if change.Context.Version != snapshot.Context.Version+1 {
				return agent.NewCodedError(agent.ErrorConflict, agent.CodeRevisionConflict, "session.commit", "context version must advance exactly once", nil)
			}
			if err := validateContext(*change.Context, request.SessionID); err != nil {
				return err
			}
			snapshot.Context = cloneContext(*change.Context)
		case SetModelConfig:
			if snapshot.RunState == RunRunning {
				return agent.NewCodedError(agent.ErrorConflict, agent.CodeActiveRun, "session.commit", "model config cannot change while a run is active", nil)
			}
			snapshot.ModelConfig = cloneModelConfig(*change.ModelConfig)
		case SetRunState:
			if err := applyRunState(snapshot, *change.RunState); err != nil {
				return err
			}
		case UpdateRunJournal:
			if err := applyJournal(snapshot, *change.Journal, request.SessionID); err != nil {
				return err
			}
		}
	}
	if snapshot.RunState == RunIdle && hasPendingJournal(snapshot.RunJournal) {
		return journalConflict("idle session cannot retain pending tool execution")
	}
	if err := validateQueueClaims(snapshot.Queue, snapshot.RunState, snapshot.ActiveRunID); err != nil {
		return err
	}
	return nil
}

func validateModelConfigTransaction(current SessionModelConfig, changes []Change) error {
	var selected *SessionModelConfig
	var event *SessionEvent
	for _, change := range changes {
		switch change.Kind {
		case SetModelConfig:
			if selected != nil {
				return invalid("session.commit", "model config can change at most once per transaction")
			}
			selected = change.ModelConfig
		case AppendSessionEvent:
			if event != nil || change.SessionEvent.Kind != EventModelConfigChanged {
				return invalid("session.commit", "model config transaction requires one change event")
			}
			event = change.SessionEvent
		}
	}
	if selected == nil && event == nil {
		return nil
	}
	if selected == nil || event == nil || event.ModelConfigChanged == nil {
		return invalid("session.commit", "model config and ModelConfigChanged event must be committed together")
	}
	if !sameModelConfig(current, event.ModelConfigChanged.Previous) || !sameModelConfig(*selected, event.ModelConfigChanged.Current) {
		return invalid("session.commit", "ModelConfigChanged event does not match the atomic transition")
	}
	return nil
}

func applyRunState(snapshot *Snapshot, change RunStateChange) error {
	switch change.State {
	case RunRunning:
		if snapshot.RunState == RunRunning {
			return agent.NewCodedError(agent.ErrorConflict, agent.CodeActiveRun, "session.commit", "session already has an active run", nil)
		}
		snapshot.RunState = RunRunning
		snapshot.ActiveRunID = change.RunID
	case RunIdle:
		if snapshot.RunState != RunRunning || snapshot.ActiveRunID != change.RunID {
			return agent.NewCodedError(agent.ErrorConflict, agent.CodeNoActiveRun, "session.commit", "run is not active", nil)
		}
		snapshot.RunState = RunIdle
		snapshot.ActiveRunID = ""
	default:
		return invalid("session.commit", "invalid run state")
	}
	return nil
}

func appendRunFact(snapshot *Snapshot, fact RunFact, actor agent.ActorIdentity) error {
	started, terminal := runFacts(snapshot.History, fact.RunID)
	switch fact.Kind {
	case RunStarted:
		if started != nil {
			return historyConflict("run already has a start fact")
		}
		if snapshot.RunState != RunRunning || snapshot.ActiveRunID != fact.RunID {
			return historyConflict("run start fact requires the active run")
		}
	case RunCompleted, RunCanceled, RunFailed, RunInterrupted:
		if started == nil || terminal != nil {
			return historyConflict("run terminal fact requires one unterminated start fact")
		}
		if snapshot.RunState != RunRunning || snapshot.ActiveRunID != fact.RunID {
			return historyConflict("run terminal fact requires the active run")
		}
		if started.ConfigRevision != fact.ConfigRevision || !sameModelConfig(started.ModelConfig, fact.ModelConfig) {
			return historyConflict("run terminal fact changed the frozen model config")
		}
	}
	copy := fact
	copy.ModelConfig = cloneModelConfig(fact.ModelConfig)
	return appendHistoryFact(&snapshot.History, snapshot.Session.ID, HistoryFact{Run: &copy}, actor)
}

func appendModelAttemptFact(snapshot *Snapshot, fact ModelAttemptFact, actor agent.ActorIdentity) error {
	var started, terminal *ModelAttemptFact
	for index := range snapshot.History {
		attempt := snapshot.History[index].ModelAttempt
		if attempt == nil || attempt.AttemptID != fact.AttemptID {
			continue
		}
		if attempt.Kind == AttemptStarted {
			started = attempt
		} else {
			terminal = attempt
		}
	}
	if fact.Kind == AttemptStarted {
		if started != nil || terminal != nil {
			return historyConflict("model attempt already started")
		}
	} else {
		if started == nil || terminal != nil {
			return historyConflict("model attempt terminal requires one unterminated start")
		}
		if started.RunID != fact.RunID || started.StepID != fact.StepID || started.ProviderKey != fact.ProviderKey || started.ModelID != fact.ModelID {
			return historyConflict("model attempt terminal changed identity")
		}
	}
	copy := fact
	return appendHistoryFact(&snapshot.History, snapshot.Session.ID, HistoryFact{ModelAttempt: &copy}, actor)
}

func runFacts(history []HistoryFact, runID agent.RunID) (*RunFact, *RunFact) {
	var started, terminal *RunFact
	for index := range history {
		fact := history[index].Run
		if fact == nil || fact.RunID != runID {
			continue
		}
		if fact.Kind == RunStarted {
			started = fact
		} else {
			terminal = fact
		}
	}
	return started, terminal
}

func applyJournal(snapshot *Snapshot, entry JournalEntry, sessionID agent.SessionID) error {
	if err := entry.Validate(sessionID); err != nil {
		return journalConflict(err.Error())
	}
	index := journalIndex(snapshot.RunJournal, entry)
	if index < 0 {
		if entry.Status != JournalPending || entry.ToolCall == nil {
			return journalConflict("terminal journal has no pending entry")
		}
		if snapshot.RunState != RunRunning || snapshot.ActiveRunID != entry.RunID {
			return journalConflict("pending tool execution must belong to the active run")
		}
		snapshot.RunJournal = append(snapshot.RunJournal, cloneJournalEntry(entry))
		return nil
	}
	current := &snapshot.RunJournal[index]
	if current.Status != JournalPending {
		return journalConflict("journal entry already has a terminal outcome")
	}
	if entry.Status == JournalPending {
		return journalConflict("journal entry is already pending")
	}
	if entry.ToolResult == nil {
		return journalConflict("terminal journal requires a result")
	}
	if current.ToolCall != nil && entry.ToolResult.CallID != current.ToolCall.ID {
		return journalConflict("journal result does not match tool call")
	}
	if current.RunID != entry.RunID || current.StepID != entry.StepID || current.ToolCall == nil || !sameToolCall(*current.ToolCall, *entry.ToolCall) {
		return journalConflict("journal update changed the pending tool identity")
	}
	current.ToolResult = cloneToolResultPtr(entry.ToolResult)
	current.Status = entry.Status
	return nil
}

func recoverAggregate(snapshot *Snapshot) (bool, error) {
	changed := false
	interruptedRunID := snapshot.ActiveRunID
	startedAttempts := make(map[agent.AttemptID]ModelAttemptFact)
	terminalAttempts := make(map[agent.AttemptID]bool)
	for _, fact := range snapshot.History {
		if fact.ModelAttempt == nil {
			continue
		}
		if fact.ModelAttempt.Kind == AttemptStarted {
			startedAttempts[fact.ModelAttempt.AttemptID] = *fact.ModelAttempt
		} else {
			terminalAttempts[fact.ModelAttempt.AttemptID] = true
		}
	}
	for attemptID, started := range startedAttempts {
		if terminalAttempts[attemptID] {
			continue
		}
		terminal := started
		terminal.Kind = AttemptOutcomeUnknown
		terminal.ErrorCode = "process_interrupted"
		if err := appendHistoryFact(&snapshot.History, snapshot.Session.ID, HistoryFact{ModelAttempt: &terminal}, agent.ActorIdentity{}); err != nil {
			return false, agent.NewError(agent.ErrorInternal, "session.recover", "cannot terminate orphaned model attempt", err)
		}
		changed = true
	}
	for index := range snapshot.RunJournal {
		entry := &snapshot.RunJournal[index]
		if entry.Status != JournalPending || entry.ToolCall == nil {
			continue
		}
		if !hasToolResult(snapshot.History, entry.ToolCall.ID) {
			result := toolUnknown(entry.ToolCall.ID)
			if err := appendHistoryFact(&snapshot.History, snapshot.Session.ID, HistoryFact{ToolResult: &result}, agent.ActorIdentity{}); err != nil {
				return false, agent.NewError(agent.ErrorInternal, "session.recover", "cannot append unknown tool result", err)
			}
		}
		result := toolUnknown(entry.ToolCall.ID)
		entry.ToolResult = &result
		entry.Status = JournalOutcomeUnknown
		changed = true
	}
	if snapshot.RunState == RunRunning {
		if started, terminal := runFacts(snapshot.History, interruptedRunID); started != nil && terminal == nil {
			interrupted := *started
			interrupted.Kind = RunInterrupted
			interrupted.ModelConfig = cloneModelConfig(started.ModelConfig)
			if err := appendHistoryFact(&snapshot.History, snapshot.Session.ID, HistoryFact{Run: &interrupted}, agent.ActorIdentity{}); err != nil {
				return false, agent.NewError(agent.ErrorInternal, "session.recover", "cannot append interrupted run", err)
			}
		}
		snapshot.RunState = RunIdle
		snapshot.ActiveRunID = ""
		changed = true
	}
	for index := range snapshot.Queue {
		item := &snapshot.Queue[index]
		if !interruptedRunID.Valid() {
			continue
		}
		if item.ClaimedBy == interruptedRunID {
			item.ClaimedBy = ""
			changed = true
		}
		// Every steer belongs to the interrupted Run, including one that
		// was durably queued but not yet claimed at the safe-step boundary.
		if item.Delivery == DeliverySteer {
			item.Delivery = DeliveryHeld
			changed = true
		}
	}
	return changed, nil
}

func validateContext(contextView ContextView, sessionID agent.SessionID) error {
	if contextView.TokenCount < 0 {
		return invalid("session.context", "context token count cannot be negative")
	}
	for _, input := range contextView.Inputs {
		if !input.Valid() || input.SystemPrompt != nil {
			return invalid("session.context", "context contains an invalid or fixed input")
		}
		if input.Message != nil && input.Message.SessionID != sessionID {
			return invalid("session.context", "context message does not belong to session")
		}
		if input.ToolCall != nil && input.ToolCall.SessionID != sessionID {
			return invalid("session.context", "context tool call does not belong to session")
		}
	}
	if err := model.ValidateInputs(contextView.Inputs); err != nil {
		return invalid("session.context", err.Error())
	}
	return nil
}

func validateToolTransactions(history []HistoryFact, changes []Change) error {
	pending := make(map[agent.ToolCallID]*JournalEntry)
	terminal := make(map[agent.ToolCallID]*JournalEntry)
	appendedCalls := make(map[agent.ToolCallID]*agent.ToolCall)
	appendedResults := make(map[agent.ToolCallID]*tool.ToolResult)
	for _, change := range changes {
		switch change.Kind {
		case AppendToolCall:
			appendedCalls[change.ToolCall.ID] = change.ToolCall
		case AppendToolResult:
			appendedResults[change.ToolResult.CallID] = change.ToolResult
		case UpdateRunJournal:
			if change.Journal.Status == JournalPending {
				pending[change.Journal.ToolCall.ID] = change.Journal
			} else {
				terminal[change.Journal.ToolCall.ID] = change.Journal
			}
		}
	}
	for _, change := range changes {
		if change.Kind != AppendMessage || len(change.Message.Parts) != 0 {
			continue
		}
		hasCall := false
		for _, candidate := range changes {
			if candidate.Kind == AppendToolCall && candidate.ToolCall.MessageID == change.Message.ID {
				hasCall = true
				break
			}
		}
		if !hasCall {
			return historyConflict("empty assistant message requires a tool call in the same transaction")
		}
	}
	for _, change := range changes {
		switch change.Kind {
		case AppendToolCall:
			entry := pending[change.ToolCall.ID]
			if entry == nil {
				return journalConflict("tool call and pending journal must be committed together")
			}
			if !sameToolCall(*change.ToolCall, *entry.ToolCall) {
				return journalConflict("pending journal does not match the appended tool call")
			}
		case AppendToolResult:
			entry := terminal[change.ToolResult.CallID]
			if entry == nil {
				return journalConflict("tool result and terminal journal must be committed together")
			}
			if !sameToolResult(*change.ToolResult, *entry.ToolResult) {
				return journalConflict("terminal journal does not match the appended tool result")
			}
		case UpdateRunJournal:
			callID := change.Journal.ToolCall.ID
			if change.Journal.Status == JournalPending {
				call := appendedCalls[callID]
				if call == nil {
					return journalConflict("pending journal has no committed tool call")
				}
				if !sameToolCall(*call, *change.Journal.ToolCall) {
					return journalConflict("pending journal changed the appended tool identity")
				}
			} else {
				result := appendedResults[callID]
				if result == nil {
					return journalConflict("terminal journal has no committed tool result")
				}
				if call, ok := findToolCall(history, callID); !ok || !sameToolCall(*call, *change.Journal.ToolCall) {
					return journalConflict("terminal journal changed the history tool identity")
				}
				if !sameToolResult(*result, *change.Journal.ToolResult) {
					return journalConflict("terminal journal changed the history tool result")
				}
			}
		}
	}
	return nil
}

func validateHistoryConsistency(sessionID agent.SessionID, history []HistoryFact, journal []JournalEntry) error {
	messages := make(map[agent.MessageID]*agent.Message)
	calls := make(map[agent.ToolCallID]bool)
	results := make(map[agent.ToolCallID]bool)
	pending := make(map[agent.ToolCallID]bool)
	journalEntries := make(map[agent.ToolCallID]bool)
	runStarts := make(map[agent.RunID]*RunFact)
	runTerminals := make(map[agent.RunID]bool)
	attemptStarts := make(map[agent.AttemptID]*ModelAttemptFact)
	attemptTerminals := make(map[agent.AttemptID]bool)
	for _, fact := range history {
		if err := fact.validatePayload(sessionID); err != nil {
			return historyConflict(err.Error())
		}
		if fact.Message != nil {
			if messages[fact.Message.ID] != nil {
				return historyConflict("duplicate message in initial history")
			}
			messages[fact.Message.ID] = fact.Message
		}
		if fact.ToolCall != nil {
			if calls[fact.ToolCall.ID] {
				return historyConflict("duplicate tool call in initial history")
			}
			parent := messages[fact.ToolCall.MessageID]
			if parent == nil {
				return historyConflict("initial tool call references an unknown assistant message")
			}
			if parent.Role != agent.RoleAssistant || parent.RunID != fact.ToolCall.RunID || parent.StepID != fact.ToolCall.StepID {
				return historyConflict("initial tool call containment differs from its assistant message")
			}
			calls[fact.ToolCall.ID] = true
		}
		if fact.ToolResult != nil {
			if !calls[fact.ToolResult.CallID] || results[fact.ToolResult.CallID] {
				return historyConflict("unpaired or duplicate tool result in initial history")
			}
			results[fact.ToolResult.CallID] = true
		}
		if fact.Run != nil {
			if fact.Run.Kind == RunStarted {
				if runStarts[fact.Run.RunID] != nil {
					return historyConflict("duplicate initial run start fact")
				}
				runStarts[fact.Run.RunID] = fact.Run
			} else {
				started := runStarts[fact.Run.RunID]
				if started == nil || runTerminals[fact.Run.RunID] {
					return historyConflict("initial run terminal has no unique start")
				}
				if started.ConfigRevision != fact.Run.ConfigRevision || !sameModelConfig(started.ModelConfig, fact.Run.ModelConfig) {
					return historyConflict("initial run terminal changed frozen model config")
				}
				runTerminals[fact.Run.RunID] = true
			}
		}
		if fact.ModelAttempt != nil {
			attempt := fact.ModelAttempt
			if attempt.Kind == AttemptStarted {
				if attemptStarts[attempt.AttemptID] != nil || attemptTerminals[attempt.AttemptID] {
					return historyConflict("duplicate initial model attempt start")
				}
				attemptStarts[attempt.AttemptID] = attempt
			} else {
				started := attemptStarts[attempt.AttemptID]
				if started == nil || attemptTerminals[attempt.AttemptID] {
					return historyConflict("initial model attempt terminal has no unique start")
				}
				if started.RunID != attempt.RunID || started.StepID != attempt.StepID || started.ProviderKey != attempt.ProviderKey || started.ModelID != attempt.ModelID {
					return historyConflict("initial model attempt terminal changed identity")
				}
				attemptTerminals[attempt.AttemptID] = true
			}
		}
	}
	for _, entry := range journal {
		if journalEntries[entry.ToolCall.ID] {
			return journalConflict("duplicate initial journal entry")
		}
		journalEntries[entry.ToolCall.ID] = true
		if !calls[entry.ToolCall.ID] {
			return journalConflict("initial journal has no history tool call")
		}
		if entry.Status == JournalPending && results[entry.ToolCall.ID] {
			return journalConflict("pending journal already has a history result")
		}
		if entry.Status == JournalPending {
			pending[entry.ToolCall.ID] = true
		}
		if entry.Status != JournalPending && !results[entry.ToolCall.ID] {
			return journalConflict("terminal journal has no history result")
		}
		if entry.Status != JournalPending {
			result := findToolResult(history, entry.ToolCall.ID)
			if result == nil || !sameToolResult(*result, *entry.ToolResult) {
				return journalConflict("terminal journal result differs from initial history")
			}
		}
	}
	for callID := range calls {
		if !results[callID] && !pending[callID] {
			return journalConflict("unpaired initial tool call has no pending journal")
		}
	}
	return nil
}

func sameModelConfig(left, right SessionModelConfig) bool {
	if left.ProviderKey != right.ProviderKey || left.ModelID != right.ModelID || left.Reasoning != right.Reasoning {
		return false
	}
	return sameFloat(left.Parameters.Temperature, right.Parameters.Temperature) && sameInt(left.Parameters.MaxTokens, right.Parameters.MaxTokens)
}

func sameFloat(left, right *float64) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func sameInt(left, right *int) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func containsMessage(history []HistoryFact, id agent.MessageID) bool {
	for _, fact := range history {
		if fact.Message != nil && fact.Message.ID == id {
			return true
		}
	}
	return false
}

func findMessage(history []HistoryFact, id agent.MessageID) *agent.Message {
	for _, fact := range history {
		if fact.Message != nil && fact.Message.ID == id {
			return fact.Message
		}
	}
	return nil
}

func containsToolCall(history []HistoryFact, id agent.ToolCallID) bool {
	_, ok := findToolCall(history, id)
	return ok
}

func findToolCall(history []HistoryFact, id agent.ToolCallID) (*agent.ToolCall, bool) {
	for _, fact := range history {
		if fact.ToolCall != nil && fact.ToolCall.ID == id {
			return fact.ToolCall, true
		}
	}
	return nil, false
}

func hasToolResult(history []HistoryFact, id agent.ToolCallID) bool {
	for _, fact := range history {
		if fact.ToolResult != nil && fact.ToolResult.CallID == id {
			return true
		}
	}
	return false
}

func findToolResult(history []HistoryFact, id agent.ToolCallID) *tool.ToolResult {
	for _, fact := range history {
		if fact.ToolResult != nil && fact.ToolResult.CallID == id {
			return fact.ToolResult
		}
	}
	return nil
}

func queueIndex(queue []QueueItem, id agent.MessageID) int {
	for index := range queue {
		if queue[index].Message.ID == id {
			return index
		}
	}
	return -1
}

func duplicateQueueMessage(queue []QueueItem) bool {
	seen := make(map[agent.MessageID]struct{}, len(queue))
	for _, item := range queue {
		if _, exists := seen[item.Message.ID]; exists {
			return true
		}
		seen[item.Message.ID] = struct{}{}
	}
	return false
}

func journalIndex(entries []JournalEntry, target JournalEntry) int {
	for index := range entries {
		entry := entries[index]
		if target.ToolCall != nil && entry.ToolCall != nil && target.ToolCall.ID == entry.ToolCall.ID {
			return index
		}
		if target.ToolCall == nil && entry.ToolCall == nil && entry.RunID == target.RunID && entry.StepID == target.StepID {
			return index
		}
	}
	return -1
}

func hasPendingJournal(entries []JournalEntry) bool {
	for _, entry := range entries {
		if entry.Status == JournalPending {
			return true
		}
	}
	return false
}

func sameToolCall(left, right agent.ToolCall) bool {
	return left.ID == right.ID && left.CorrelationID == right.CorrelationID && left.MessageID == right.MessageID && left.SessionID == right.SessionID &&
		left.RunID == right.RunID && left.StepID == right.StepID && left.Name == right.Name && bytes.Equal(left.Arguments, right.Arguments)
}

func sameToolResult(left, right tool.ToolResult) bool {
	if left.CallID != right.CallID || left.Status != right.Status || !bytes.Equal(left.Output, right.Output) {
		return false
	}
	if left.Error == nil || right.Error == nil {
		return left.Error == nil && right.Error == nil
	}
	return *left.Error == *right.Error
}

func validQueueItem(item QueueItem, sessionID agent.SessionID) bool {
	return item.Message.Valid() && item.Message.SessionID == sessionID && item.Message.Role == agent.RoleUser &&
		item.Message.RunID == "" && item.Message.StepID == "" && item.Delivery.Valid()
}

func validateQueueClaims(queue []QueueItem, state RunState, activeRunID agent.RunID) error {
	for _, item := range queue {
		if item.ClaimedBy == "" {
			continue
		}
		if state != RunRunning || item.ClaimedBy != activeRunID {
			return agent.NewCodedError(agent.ErrorConflict, agent.CodeQueueItemClaimed, "session.commit", "claimed queue item must belong to the active run", nil)
		}
	}
	return nil
}

func toolUnknown(id agent.ToolCallID) tool.ToolResult {
	return tool.ToolResult{CallID: id, Status: tool.ResultUnknown}
}

func historyConflict(message string) error {
	return agent.NewCodedError(agent.ErrorConflict, agent.CodeHistoryInvariant, "session.commit", message, nil)
}

func queueNotFound(message string) error {
	return agent.NewCodedError(agent.ErrorNotFound, agent.CodeQueueItemNotFound, "session.commit", message, nil)
}

func journalConflict(message string) error {
	return agent.NewCodedError(agent.ErrorConflict, agent.CodeJournalInvariant, "session.commit", message, nil)
}
