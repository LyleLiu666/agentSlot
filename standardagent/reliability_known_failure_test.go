package standardagent

import (
	"context"
	"os"
	"testing"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestKnownFailureRuntimeDoesNotIdleOnRunningRecoveredSnapshot(t *testing.T) {
	if os.Getenv("AGENTSLOT_RUN_KNOWN_FAILURES") != "1" {
		t.Skip("set AGENTSLOT_RUN_KNOWN_FAILURES=1 to execute documented red regressions")
	}
	runContext, cancelRun := context.WithCancel(context.Background())
	run := &activeRun{id: "run-1", ctx: runContext, cancel: cancelRun, done: make(chan struct{})}
	manager, err := session.NewManager(session.NewMemoryStore(), testDefaultModel())
	if err != nil {
		t.Fatal(err)
	}
	managedSession, err := manager.Create(context.Background(), session.CreateRequest{AgentID: "agent-1", WorkspaceID: "workspace-1"})
	if err != nil {
		t.Fatal(err)
	}
	recovered := session.Snapshot{
		Session: agent.Session{ID: managedSession.ID(), AgentID: "agent-1", WorkspaceID: "workspace-1"}, Revision: agent.Revision(2),
		RunState: session.RunRunning, ActiveRunID: run.id,
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
	if runtime.state != runtimeClosed {
		t.Fatalf("Runtime state = %q after recovering a still-running snapshot, want closed", runtime.state)
	}
}

type recoveredSnapshotStore struct {
	session.SessionStore
	Snapshot session.Snapshot
}

func (s recoveredSnapshotStore) Recover(context.Context, session.SessionRef) (session.Snapshot, error) {
	return s.Snapshot, nil
}
