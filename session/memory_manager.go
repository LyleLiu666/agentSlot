package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
)

const memoryModuleID = "session.memory"

// NewMemoryModule returns an explicitly installable development Store module.
// The standard framework constructs its fixed Manager from this Store and the
// Application default model configuration.
func NewMemoryModule() agentslot.Module {
	return memoryModule{store: NewMemoryStore()}
}

type memoryModule struct {
	store *MemoryStore
}

func (memoryModule) ID() string { return memoryModuleID }

func (m memoryModule) Register(registrar agentslot.Registrar) error {
	return registrar.Contribute(agentslot.Set(StoreSlot, SessionStore(m.store)))
}

// Manager is the fixed Session lifecycle implementation. It owns only ID
// allocation and derivation rules; all durable state remains behind Store.
type Manager struct {
	store         SessionStore
	defaultConfig SessionModelConfig
	idPrefix      string
	sequence      atomic.Uint64
}

// NewManager validates the fixed Manager dependencies once at assembly time.
func NewManager(store SessionStore, defaultConfig SessionModelConfig) (*Manager, error) {
	if store == nil {
		return nil, invalid("session.manager", "store is required")
	}
	if err := defaultConfig.Validate(); err != nil {
		return nil, invalid("session.manager", fmt.Sprintf("invalid default model config: %v", err))
	}
	prefix, err := randomIDPrefix()
	if err != nil {
		return nil, agent.NewError(agent.ErrorInternal, "session.manager", "cannot initialize ID allocator", err)
	}
	return &Manager{store: store, defaultConfig: cloneModelConfig(defaultConfig), idPrefix: prefix}, nil
}

func (m *Manager) Create(ctx context.Context, request CreateRequest) (Session, error) {
	if err := validateScope(request.AgentID, request.WorkspaceID); err != nil {
		return nil, invalid("session.manager.create", err.Error())
	}
	config, err := chooseConfig(request.ModelConfig, m.defaultConfig)
	if err != nil {
		return nil, err
	}
	id := agent.SessionID(m.nextID("session"))
	snapshot, err := m.store.Create(ctx, NewSession{
		Session:     agent.Session{ID: id, AgentID: request.AgentID, WorkspaceID: request.WorkspaceID},
		ModelConfig: config,
		RunState:    RunIdle,
	})
	if err != nil {
		return nil, err
	}
	return newMemorySession(m.store, snapshot), nil
}

func (m *Manager) Resume(ctx context.Context, request ResumeRequest) (Session, error) {
	if !request.SessionID.Valid() {
		return nil, invalid("session.manager.resume", "session ID is required")
	}
	snapshot, err := m.store.Recover(ctx, SessionRef{SessionID: request.SessionID})
	if err != nil {
		return nil, err
	}
	return newMemorySession(m.store, snapshot), nil
}

func (m *Manager) Fork(ctx context.Context, request ForkRequest) (Session, error) {
	if !request.SourceSessionID.Valid() || !request.AgentID.Valid() || !request.WorkspaceID.Valid() {
		return nil, invalid("session.manager.fork", "source session, agent, and workspace are required")
	}
	if !request.Mode.Valid() {
		return nil, invalid("session.manager.fork", "fork mode is required")
	}
	if request.Mode == ForkFullHistory && request.CutoffSequence != 0 {
		return nil, invalid("session.manager.fork", "full-history fork cannot specify a cutoff")
	}
	source, err := m.store.Load(ctx, SessionRef{SessionID: request.SourceSessionID})
	if err != nil {
		return nil, err
	}
	if request.Mode == ForkFullHistory && source.RunState == RunRunning {
		return nil, agent.NewCodedError(agent.ErrorConflict, agent.CodeActiveRun, "session.manager.fork", "cannot fork a session while a run is active", nil)
	}
	cutoff, history, err := selectForkHistory(source.History, source.ActiveRunID, request.Mode, request.CutoffSequence)
	if err != nil {
		return nil, err
	}
	config, err := chooseConfig(request.ModelConfig, source.ModelConfig)
	if err != nil {
		return nil, err
	}
	newID := agent.SessionID(m.nextID("session"))
	derived := cloneSnapshot(source)
	derived.History = history
	derived = rewriteForFork(derived, newID, request.AgentID, request.WorkspaceID, m)
	derived.ModelConfig = config
	derived.Session.ParentSessionID = source.Session.ID
	derived.Session.ParentRevision = source.Revision
	derived.Session.Revision = 0
	derived.Revision = 0
	derived.RunState = RunIdle
	derived.ActiveRunID = ""
	derived.Fork = &ForkProvenance{ParentSessionID: source.Session.ID, CutoffSequence: cutoff}
	created, err := m.store.Create(ctx, NewSession{
		Session:     derived.Session,
		History:     derived.History,
		Context:     derived.Context,
		Queue:       derived.Queue,
		RunJournal:  derived.RunJournal,
		ModelConfig: derived.ModelConfig,
		RunState:    derived.RunState,
		ActiveRunID: derived.ActiveRunID,
		Fork:        derived.Fork,
	})
	if err != nil {
		return nil, err
	}
	return newMemorySession(m.store, created), nil
}

func (m *Manager) StartFromSummary(ctx context.Context, request SummaryRequest) (Session, error) {
	if err := validateScope(request.AgentID, request.WorkspaceID); err != nil {
		return nil, invalid("session.manager.summary", err.Error())
	}
	if len(request.Messages) == 0 {
		return nil, invalid("session.manager.summary", "at least one summary message is required")
	}
	fallback := m.defaultConfig
	parentID := agent.SessionID("")
	parentRevision := agent.Revision(0)
	if request.SourceSessionID != "" {
		if !request.SourceSessionID.Valid() {
			return nil, invalid("session.manager.summary", "source session ID is invalid")
		}
		source, loadErr := m.store.Load(ctx, SessionRef{SessionID: request.SourceSessionID})
		if loadErr != nil {
			return nil, loadErr
		}
		fallback = source.ModelConfig
		parentID = source.Session.ID
		parentRevision = source.Revision
	}
	config, err := chooseConfig(request.ModelConfig, fallback)
	if err != nil {
		return nil, err
	}
	sessionID := agent.SessionID(m.nextID("session"))
	history := make([]HistoryFact, 0, len(request.Messages))
	for _, input := range request.Messages {
		if !input.Valid() {
			return nil, invalid("session.manager.summary", "summary contains invalid message input")
		}
		history = append(history, HistoryFact{Message: &agent.Message{
			ID:        agent.MessageID(m.nextID("message")),
			SessionID: sessionID,
			Role:      agent.RoleUser,
			Parts:     cloneParts(input.Parts),
		}})
	}
	created, err := m.store.Create(ctx, NewSession{
		Session: agent.Session{
			ID: sessionID, AgentID: request.AgentID, WorkspaceID: request.WorkspaceID,
			ParentSessionID: parentID, ParentRevision: parentRevision,
		},
		History:     history,
		ModelConfig: config,
		RunState:    RunIdle,
	})
	if err != nil {
		return nil, err
	}
	return newMemorySession(m.store, created), nil
}

func (m *Manager) nextID(prefix string) string {
	return fmt.Sprintf("%s-%s-%d", prefix, m.idPrefix, m.sequence.Add(1))
}

type memorySession struct {
	store    SessionStore
	id       agent.SessionID
	revision atomic.Uint64
}

var _ Session = (*memorySession)(nil)

func newMemorySession(store SessionStore, snapshot Snapshot) *memorySession {
	session := &memorySession{store: store, id: snapshot.Session.ID}
	session.revision.Store(uint64(snapshot.Revision))
	return session
}

func (s *memorySession) ID() agent.SessionID { return s.id }

func (s *memorySession) Revision() agent.Revision {
	return agent.Revision(s.revision.Load())
}

func (s *memorySession) View(ctx context.Context) (Snapshot, error) {
	snapshot, err := s.store.Load(ctx, SessionRef{SessionID: s.id})
	if err != nil {
		return Snapshot{}, err
	}
	s.revision.Store(uint64(snapshot.Revision))
	return snapshot, nil
}

func rewriteForFork(source Snapshot, newSessionID agent.SessionID, agentID agent.AgentID, workspaceID agent.WorkspaceID, manager *Manager) Snapshot {
	messageIDs := make(map[agent.MessageID]agent.MessageID)
	callIDs := make(map[agent.ToolCallID]agent.ToolCallID)
	runIDs := make(map[agent.RunID]agent.RunID)
	stepIDs := make(map[agent.StepID]agent.StepID)
	attemptIDs := make(map[agent.AttemptID]agent.AttemptID)
	messageID := func(old agent.MessageID) agent.MessageID {
		if old == "" {
			return ""
		}
		if mapped, ok := messageIDs[old]; ok {
			return mapped
		}
		mapped := agent.MessageID(manager.nextID("message"))
		messageIDs[old] = mapped
		return mapped
	}
	callID := func(old agent.ToolCallID) agent.ToolCallID {
		if mapped, ok := callIDs[old]; ok {
			return mapped
		}
		mapped := agent.ToolCallID(manager.nextID("call"))
		callIDs[old] = mapped
		return mapped
	}
	runID := func(old agent.RunID) agent.RunID {
		if old == "" {
			return ""
		}
		if mapped, ok := runIDs[old]; ok {
			return mapped
		}
		mapped := agent.RunID(manager.nextID("run"))
		runIDs[old] = mapped
		return mapped
	}
	stepID := func(old agent.StepID) agent.StepID {
		if old == "" {
			return ""
		}
		if mapped, ok := stepIDs[old]; ok {
			return mapped
		}
		mapped := agent.StepID(manager.nextID("step"))
		stepIDs[old] = mapped
		return mapped
	}
	attemptID := func(old agent.AttemptID) agent.AttemptID {
		if mapped, ok := attemptIDs[old]; ok {
			return mapped
		}
		mapped := agent.AttemptID(manager.nextID("attempt"))
		attemptIDs[old] = mapped
		return mapped
	}
	for index := range source.History {
		fact := &source.History[index]
		fact.OriginFactID = fact.FactID
		if fact.Message != nil {
			message := cloneMessage(*fact.Message)
			message.ID = messageID(message.ID)
			message.SessionID = newSessionID
			message.RunID = runID(message.RunID)
			message.StepID = stepID(message.StepID)
			fact.Message = &message
		}
		if fact.ToolCall != nil {
			call := cloneToolCall(*fact.ToolCall)
			call.ID = callID(call.ID)
			call.SessionID = newSessionID
			call.MessageID = messageID(call.MessageID)
			call.RunID = runID(call.RunID)
			call.StepID = stepID(call.StepID)
			fact.ToolCall = &call
		}
		if fact.ToolResult != nil {
			result := cloneToolResult(*fact.ToolResult)
			result.CallID = callID(result.CallID)
			fact.ToolResult = &result
		}
		if fact.Run != nil {
			run := *fact.Run
			run.SessionID = newSessionID
			run.RunID = runID(run.RunID)
			run.ModelConfig = cloneModelConfig(run.ModelConfig)
			fact.Run = &run
		}
		if fact.ModelAttempt != nil {
			attempt := *fact.ModelAttempt
			attempt.AttemptID = attemptID(attempt.AttemptID)
			attempt.RunID = runID(attempt.RunID)
			attempt.StepID = stepID(attempt.StepID)
			fact.ModelAttempt = &attempt
		}
		if fact.ContextContribution != nil {
			contribution := *fact.ContextContribution
			contribution.RunID = runID(contribution.RunID)
			contribution.StepID = stepID(contribution.StepID)
			contribution.Inputs = rewriteForkInputs(contribution.Inputs, newSessionID, messageID, callID, runID, stepID)
			fact.ContextContribution = &contribution
		}
		if fact.RunBudgetExceeded != nil {
			budget := *fact.RunBudgetExceeded
			budget.RunID = runID(budget.RunID)
			fact.RunBudgetExceeded = &budget
		}
	}
	// A complete-history fork copies canonical conversation facts. Model-facing
	// Context is re-derived for the child's selected model; pending delivery and
	// execution recovery state never leak into the child.
	source.Context = ContextView{}
	source.RetainedContexts = nil
	source.Queue = nil
	source.RunJournal = nil
	source.Events = nil
	source.Session = agent.Session{ID: newSessionID, AgentID: agentID, WorkspaceID: workspaceID}
	source.Revision = 0
	source.Session.Revision = 0
	source.RunState = RunIdle
	source.ActiveRunID = ""
	return source
}

func selectForkHistory(history []HistoryFact, activeRunID agent.RunID, mode ForkMode, requested HistorySequence) (HistorySequence, []HistoryFact, error) {
	if mode == ForkHistoryPrefix && requested == 0 {
		return 0, nil, nil
	}
	if len(history) == 0 {
		if mode == ForkFullHistory {
			return 0, nil, nil
		}
		return 0, nil, invalid("session.manager.fork", "fork cutoff does not exist")
	}
	cutoff := requested
	if mode == ForkFullHistory {
		cutoff = history[len(history)-1].Sequence
	}
	end := -1
	for index := range history {
		if history[index].Sequence == cutoff {
			end = index
			break
		}
	}
	if end < 0 {
		return 0, nil, invalid("session.manager.fork", "fork cutoff does not exist")
	}
	if mode == ForkHistoryPrefix && !completedForkBoundary(history, end, activeRunID) {
		return 0, nil, invalid("session.manager.fork", "fork cutoff is not a completed Step boundary")
	}
	selected := make([]HistoryFact, end+1)
	for index := range selected {
		selected[index] = cloneHistoryFact(history[index])
	}
	if err := validateForkProtocol(selected); err != nil {
		return 0, nil, invalid("session.manager.fork", err.Error())
	}
	return cutoff, selected, nil
}

func completedForkBoundary(history []HistoryFact, index int, activeRunID agent.RunID) bool {
	fact := history[index]
	if index == len(history)-1 && activeRunID.Valid() && fact.RunID == activeRunID {
		return false
	}
	if fact.StepID.Valid() {
		return index == len(history)-1 || history[index+1].StepID != fact.StepID
	}
	return fact.Run != nil && fact.Run.Kind != RunStarted
}

func validateForkProtocol(history []HistoryFact) error {
	calls := make(map[agent.ToolCallID]bool)
	results := make(map[agent.ToolCallID]bool)
	attempts := make(map[agent.AttemptID]bool)
	terminals := make(map[agent.AttemptID]bool)
	for _, fact := range history {
		if fact.ToolCall != nil {
			calls[fact.ToolCall.ID] = true
		}
		if fact.ToolResult != nil {
			results[fact.ToolResult.CallID] = true
		}
		if fact.ModelAttempt != nil {
			if fact.ModelAttempt.Kind == AttemptStarted {
				attempts[fact.ModelAttempt.AttemptID] = true
			} else {
				terminals[fact.ModelAttempt.AttemptID] = true
			}
		}
	}
	for id := range calls {
		if !results[id] {
			return fmt.Errorf("fork cutoff leaves tool call %q unpaired", id)
		}
	}
	for id := range attempts {
		if !terminals[id] {
			return fmt.Errorf("fork cutoff leaves model attempt %q unterminated", id)
		}
	}
	return nil
}

func rewriteForkInputs(
	inputs []model.Input,
	sessionID agent.SessionID,
	messageID func(agent.MessageID) agent.MessageID,
	callID func(agent.ToolCallID) agent.ToolCallID,
	runID func(agent.RunID) agent.RunID,
	stepID func(agent.StepID) agent.StepID,
) []model.Input {
	result := make([]model.Input, len(inputs))
	for index, input := range inputs {
		result[index] = cloneModelInput(input)
		if result[index].Message != nil {
			result[index].Message.ID = messageID(result[index].Message.ID)
			result[index].Message.SessionID = sessionID
			result[index].Message.RunID = runID(result[index].Message.RunID)
			result[index].Message.StepID = stepID(result[index].Message.StepID)
		}
		if result[index].ToolCall != nil {
			result[index].ToolCall.ID = callID(result[index].ToolCall.ID)
			result[index].ToolCall.MessageID = messageID(result[index].ToolCall.MessageID)
			result[index].ToolCall.SessionID = sessionID
			result[index].ToolCall.RunID = runID(result[index].ToolCall.RunID)
			result[index].ToolCall.StepID = stepID(result[index].ToolCall.StepID)
		}
		if result[index].ToolResult != nil {
			result[index].ToolResult.CallID = callID(result[index].ToolResult.CallID)
		}
	}
	return result
}

func validateScope(agentID agent.AgentID, workspaceID agent.WorkspaceID) error {
	if !agentID.Valid() || !workspaceID.Valid() {
		return fmt.Errorf("agent and workspace IDs are required")
	}
	return nil
}

func chooseConfig(explicit *SessionModelConfig, fallback SessionModelConfig) (SessionModelConfig, error) {
	if explicit != nil {
		if err := explicit.Validate(); err != nil {
			return SessionModelConfig{}, invalid("session.model_config", err.Error())
		}
		return cloneModelConfig(*explicit), nil
	}
	return cloneModelConfig(fallback), nil
}

func randomIDPrefix() (string, error) {
	data := make([]byte, 12)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}
