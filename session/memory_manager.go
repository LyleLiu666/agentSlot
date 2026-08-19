package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
)

const memoryModuleID = "session.memory"

// NewMemoryModule returns an explicitly installable development module that
// contributes one paired MemoryStore and MemoryManager. Standard applications
// never install it implicitly; products remain free to provide either Slot
// with an independent implementation.
func NewMemoryModule(defaultConfig SessionModelConfig) (agentslot.Module, error) {
	store := NewMemoryStore()
	manager, err := NewMemoryManager(store, defaultConfig)
	if err != nil {
		return nil, err
	}
	return memoryModule{store: store, manager: manager}, nil
}

type memoryModule struct {
	store   *MemoryStore
	manager *MemoryManager
}

func (memoryModule) ID() string { return memoryModuleID }

func (m memoryModule) Register(registrar agentslot.Registrar) error {
	return registrar.Contribute(
		agentslot.Set(StoreSlot, SessionStore(m.store)),
		agentslot.Set(ManagerSlot, SessionManager(m.manager)),
	)
}

// MemoryManager is the reference SessionManager paired with MemoryStore. It
// owns only ID allocation and aggregate derivation; all durable state remains
// behind SessionStore.
type MemoryManager struct {
	store         SessionStore
	defaultConfig SessionModelConfig
	idPrefix      string
	sequence      atomic.Uint64
}

var _ SessionManager = (*MemoryManager)(nil)

// NewMemoryManager validates the default model configuration once at assembly
// time. A Session may explicitly override it at creation or derivation.
func NewMemoryManager(store SessionStore, defaultConfig SessionModelConfig) (*MemoryManager, error) {
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
	return &MemoryManager{store: store, defaultConfig: cloneModelConfig(defaultConfig), idPrefix: prefix}, nil
}

func (m *MemoryManager) Create(ctx context.Context, request CreateRequest) (Session, error) {
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

func (m *MemoryManager) Resume(ctx context.Context, request ResumeRequest) (Session, error) {
	if !request.SessionID.Valid() {
		return nil, invalid("session.manager.resume", "session ID is required")
	}
	snapshot, err := m.store.Recover(ctx, SessionRef{SessionID: request.SessionID})
	if err != nil {
		return nil, err
	}
	return newMemorySession(m.store, snapshot), nil
}

func (m *MemoryManager) Fork(ctx context.Context, request ForkRequest) (Session, error) {
	if !request.SourceSessionID.Valid() || !request.AgentID.Valid() || !request.WorkspaceID.Valid() {
		return nil, invalid("session.manager.fork", "source session, agent, and workspace are required")
	}
	source, err := m.store.Load(ctx, SessionRef{SessionID: request.SourceSessionID})
	if err != nil {
		return nil, err
	}
	if source.RunState == RunRunning {
		return nil, agent.NewCodedError(agent.ErrorConflict, agent.CodeActiveRun, "session.manager.fork", "cannot fork a session while a run is active", nil)
	}
	config, err := chooseConfig(request.ModelConfig, source.ModelConfig)
	if err != nil {
		return nil, err
	}
	newID := agent.SessionID(m.nextID("session"))
	derived := cloneSnapshot(source)
	derived = rewriteForFork(derived, newID, request.AgentID, request.WorkspaceID, m)
	derived.ModelConfig = config
	derived.Session.ParentSessionID = source.Session.ID
	derived.Session.ParentRevision = source.Revision
	derived.Session.Revision = 0
	derived.Revision = 0
	derived.RunState = RunIdle
	derived.ActiveRunID = ""
	created, err := m.store.Create(ctx, NewSession{
		Session:     derived.Session,
		History:     derived.History,
		Context:     derived.Context,
		Queue:       derived.Queue,
		RunJournal:  derived.RunJournal,
		ModelConfig: derived.ModelConfig,
		RunState:    derived.RunState,
		ActiveRunID: derived.ActiveRunID,
	})
	if err != nil {
		return nil, err
	}
	return newMemorySession(m.store, created), nil
}

func (m *MemoryManager) StartFromSummary(ctx context.Context, request SummaryRequest) (Session, error) {
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

func (m *MemoryManager) nextID(prefix string) string {
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

func rewriteForFork(source Snapshot, newSessionID agent.SessionID, agentID agent.AgentID, workspaceID agent.WorkspaceID, manager *MemoryManager) Snapshot {
	messageIDs := make(map[agent.MessageID]agent.MessageID)
	callIDs := make(map[agent.ToolCallID]agent.ToolCallID)
	runIDs := make(map[agent.RunID]agent.RunID)
	stepIDs := make(map[agent.StepID]agent.StepID)
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
	for index := range source.History {
		fact := &source.History[index]
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
	}
	// A complete-history fork copies canonical conversation facts. Model-facing
	// Context is re-derived for the child's selected model; pending delivery and
	// execution recovery state never leak into the child.
	source.Context = ContextView{}
	source.Queue = nil
	source.RunJournal = nil
	source.Session = agent.Session{ID: newSessionID, AgentID: agentID, WorkspaceID: workspaceID}
	source.Revision = 0
	source.Session.Revision = 0
	source.RunState = RunIdle
	source.ActiveRunID = ""
	return source
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
