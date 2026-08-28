package standardagent

import (
	"context"
	"testing"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestKnownFailureRuntimeDoesNotIdleOnRunningRecoveredSnapshot(t *testing.T) {
	runtime := runtimeRecoveringFrom(t, session.Snapshot{RunState: session.RunRunning, ActiveRunID: "run-1"})
	if runtime.state != runtimeClosed {
		t.Fatalf("Runtime state = %q after recovering a still-running snapshot, want closed", runtime.state)
	}
}

func TestRuntimeDoesNotIdleWhenRecoveredSnapshotHasNoTerminalFact(t *testing.T) {
	runtime := runtimeRecoveringFrom(t, session.Snapshot{RunState: session.RunIdle})
	if runtime.state != runtimeClosed {
		t.Fatalf("Runtime state = %q after recovering an unterminated Run, want closed", runtime.state)
	}
}

func TestRuntimeReturnsIdleOnlyWhenRecoveredRunIsSettled(t *testing.T) {
	config := testDefaultModel()
	started := session.RunFact{SessionID: "session-1", RunID: "run-1", Kind: session.RunStarted, ModelConfig: config, ConfigRevision: 1}
	completed := started
	completed.Kind = session.RunCompleted
	runtime := runtimeRecoveringFrom(t, session.Snapshot{
		RunState: session.RunIdle,
		History:  []session.HistoryFact{{Run: &started}, {Run: &completed}},
	})
	if runtime.state != runtimeIdle {
		t.Fatalf("Runtime state = %q after recovering a settled Run, want idle", runtime.state)
	}
}

func runtimeRecoveringFrom(t *testing.T, recovered session.Snapshot) *runtimeInstance {
	t.Helper()
	runContext, cancelRun := context.WithCancel(context.Background())
	run := &activeRun{id: "run-1", config: testDefaultModel(), configRevision: 1, ctx: runContext, cancel: cancelRun, done: make(chan struct{})}
	manager, err := session.NewManager(session.NewMemoryStore(), testDefaultModel())
	if err != nil {
		t.Fatal(err)
	}
	managedSession, err := manager.Create(context.Background(), session.CreateRequest{AgentID: "agent-1", WorkspaceID: "workspace-1"})
	if err != nil {
		t.Fatal(err)
	}
	recovered.Session = agent.Session{ID: managedSession.ID(), AgentID: "agent-1", WorkspaceID: "workspace-1"}
	recovered.Revision = agent.Revision(2)
	for _, fact := range recovered.History {
		if fact.Run != nil {
			fact.Run.SessionID = managedSession.ID()
		}
	}
	runtime := &runtimeInstance{
		session: managedSession,
		components: &runtimeComponents{store: recoveredSnapshotStore{
			SessionStore: session.NewMemoryStore(), Snapshot: recovered,
		}},
		state: runtimeRunning, active: run, idleSignal: make(chan struct{}), closeDone: make(chan struct{}),
	}
	runtime.mu.Lock()
	runtime.recoverAfterRunFailureLocked(run)
	runtime.mu.Unlock()
	return runtime
}

type recoveredSnapshotStore struct {
	session.SessionStore
	Snapshot session.Snapshot
}

func (s recoveredSnapshotStore) Recover(context.Context, session.SessionRef) (session.Snapshot, error) {
	return s.Snapshot, nil
}
