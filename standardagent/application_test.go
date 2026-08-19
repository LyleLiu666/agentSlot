package standardagent

import (
	"context"
	"errors"
	"sync"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestNewApplicationBuildsStandardProfileAndAttachesGateway(t *testing.T) {
	entry := &captureEntrypoint{}
	application := NewApplication(ApplicationSpec{
		Name: "test-agent",
		Modules: []agentslot.Module{
			componentsModule{manager: newFakeManager()},
			NewEntrypointModule("entrypoint.test", "test", entry),
		},
	})
	if _, ok := any(application).(*agentslot.Application); !ok {
		t.Fatal("NewApplication did not return *agentslot.Application")
	}
	assembly, err := application.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if assembly == nil {
		t.Fatal("Build returned nil Assembly")
	}
	access := entry.Access()
	if access == nil {
		t.Fatal("entrypoint did not receive GatewayAccess during Build")
	}
	if _, ok := access.(*gatewayBinding); !ok {
		t.Fatalf("entrypoint received %T, want the isolated GatewayAccess binding", access)
	}
	_, err = access.Snapshot(context.Background(), interaction.SnapshotRequest{SessionID: "session-1"})
	if !agent.IsCode(err, agent.CodeApplicationNotStarted) {
		t.Fatalf("pre-start Snapshot error = %v, code=%q", err, agent.CodeOf(err))
	}
}

func TestGatewaySnapshotUsesKnownRevisionForReconnectNotCAS(t *testing.T) {
	manager := newFakeManager()
	entry := &captureEntrypoint{}
	application := NewApplication(ApplicationSpec{
		Name: "snapshot-agent",
		Modules: []agentslot.Module{
			componentsModule{manager: manager},
			NewEntrypointModule("entrypoint.test", "test", entry),
		},
	})
	runtime, err := application.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Stop(context.Background()) })
	access := entry.Access()
	if _, err := access.ResumeSession(context.Background(), interaction.ResumeSessionRequest{SessionID: "session-1"}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	snapshot, err := access.Snapshot(context.Background(), interaction.SnapshotRequest{
		SessionID: "session-1", KnownRevision: 1,
	})
	if err != nil || snapshot.Revision != 3 {
		t.Fatalf("behind Snapshot = %#v, %v; want current revision 3", snapshot, err)
	}
	_, err = access.Snapshot(context.Background(), interaction.SnapshotRequest{
		SessionID: "session-1", KnownRevision: 4,
	})
	if !agent.IsCode(err, agent.CodeRevisionConflict) {
		t.Fatalf("ahead Snapshot error = %v, code=%q", err, agent.CodeOf(err))
	}
}

func TestGatewayRejectsInvalidResumeBeforeCallingManager(t *testing.T) {
	manager := newFakeManager()
	entry := &captureEntrypoint{}
	application := NewApplication(ApplicationSpec{
		Name: "validation-agent",
		Modules: []agentslot.Module{
			componentsModule{manager: manager},
			NewEntrypointModule("entrypoint.test", "test", entry),
		},
	})
	runtime, err := application.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Stop(context.Background()) })
	_, err = entry.Access().ResumeSession(context.Background(), interaction.ResumeSessionRequest{})
	if !agent.IsKind(err, agent.ErrorInvalidInput) {
		t.Fatalf("ResumeSession error = %v, kind=%q", err, agent.KindOf(err))
	}
	if got := manager.ResumeCalls(); got != 0 {
		t.Fatalf("SessionManager.Resume calls = %d, want 0", got)
	}
}

func TestResumeRejectsTypedNilSessionFromManager(t *testing.T) {
	entry := &captureEntrypoint{}
	application := NewApplication(ApplicationSpec{
		Name: "nil-session-agent",
		Modules: []agentslot.Module{
			componentsModule{manager: typedNilResumeManager{fakeManager: newFakeManager()}},
			NewEntrypointModule("entrypoint.test", "test", entry),
		},
	})
	runtime, err := application.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Stop(context.Background()) })
	_, err = entry.Access().ResumeSession(context.Background(), interaction.ResumeSessionRequest{SessionID: "session-1"})
	if !agent.IsKind(err, agent.ErrorInternal) {
		t.Fatalf("ResumeSession error = %v, kind=%q", err, agent.KindOf(err))
	}
}

func TestStandardProfileRejectsMissingRequiredComponents(t *testing.T) {
	application := NewApplication(ApplicationSpec{Name: "missing", Modules: []agentslot.Module{
		NewEntrypointModule("entrypoint.test", "test", &captureEntrypoint{}),
	}})
	_, err := application.Build()
	if !errors.Is(err, agentslot.ErrRequirementUnsatisfied) {
		t.Fatalf("Build error = %v, want ErrRequirementUnsatisfied", err)
	}
}

func TestStandardApplicationRejectsEntrypointThatBypassesGatewayWrapper(t *testing.T) {
	application := NewApplication(ApplicationSpec{Name: "raw-entrypoint", Modules: []agentslot.Module{
		componentsModule{manager: newFakeManager()},
		rawEntrypointModule{entrypoint: &captureEntrypoint{}},
	}})
	_, err := application.Build()
	if !errors.Is(err, agentslot.ErrRequirementUnsatisfied) {
		t.Fatalf("Build error = %v, want ErrRequirementUnsatisfied", err)
	}
}

func TestStandardApplicationRejectsRawEntrypointAlongsideWrappedEntrypoint(t *testing.T) {
	application := NewApplication(ApplicationSpec{Name: "mixed-entrypoints", Modules: []agentslot.Module{
		componentsModule{manager: newFakeManager()},
		NewEntrypointModule("entrypoint.wrapped", "wrapped", &captureEntrypoint{}),
		rawEntrypointModule{entrypoint: &captureEntrypoint{}},
	}})
	if _, err := application.Build(); err == nil {
		t.Fatal("Build succeeded with a raw Entrypoint contribution")
	}
}

func TestConcurrentResumeCreatesOneRuntimePerSession(t *testing.T) {
	manager := newFakeManager()
	entry := &captureEntrypoint{}
	application := NewApplication(ApplicationSpec{
		Name: "resume-agent",
		Modules: []agentslot.Module{
			componentsModule{manager: manager},
			NewEntrypointModule("entrypoint.test", "test", entry),
		},
	})
	runtime, err := application.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Stop(context.Background()) })

	access := entry.Access()
	const callers = 32
	var wait sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			opened, err := access.ResumeSession(context.Background(), interaction.ResumeSessionRequest{SessionID: "session-1"})
			if err == nil && opened.SessionID != "session-1" {
				err = errors.New("wrong session opened")
			}
			errs <- err
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
	}
	if got := manager.ResumeCalls(); got != 1 {
		t.Fatalf("SessionManager.Resume calls = %d, want 1", got)
	}

	snapshot, err := access.Snapshot(context.Background(), interaction.SnapshotRequest{SessionID: "session-1"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.SessionID != "session-1" || snapshot.Revision != 3 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestSessionRuntimesShareOneAssembledComponentSet(t *testing.T) {
	entry := &captureEntrypoint{}
	application := NewApplication(ApplicationSpec{
		Name: "component-sharing-agent",
		Modules: []agentslot.Module{
			componentsModule{manager: newFakeManager()},
			NewEntrypointModule("entrypoint.test", "test", entry),
		},
	})
	runtime, err := application.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Stop(context.Background()) })
	for _, id := range []agent.SessionID{"session-1", "session-2"} {
		if _, err := entry.Access().ResumeSession(context.Background(), interaction.ResumeSessionRequest{SessionID: id}); err != nil {
			t.Fatalf("resume %s: %v", id, err)
		}
	}
	binding := entry.Access().(*gatewayBinding)
	target, err := binding.access()
	if err != nil {
		t.Fatalf("binding access: %v", err)
	}
	fixedGateway := target.(*gateway)
	coordinator, release, err := fixedGateway.runtime.acquire()
	if err != nil {
		t.Fatalf("acquire coordinator: %v", err)
	}
	defer release()
	first, err := coordinator.runtime("session-1")
	if err != nil {
		t.Fatalf("first Runtime: %v", err)
	}
	second, err := coordinator.runtime("session-2")
	if err != nil {
		t.Fatalf("second Runtime: %v", err)
	}
	firstRuntime := first.(*runtimeInstance)
	secondRuntime := second.(*runtimeInstance)
	if firstRuntime.components == nil || firstRuntime.components != secondRuntime.components {
		t.Fatal("Session Runtimes did not share one assembled component set")
	}
	if firstRuntime.components.store == nil || firstRuntime.components.executor == nil {
		t.Fatal("required Runtime components were not injected")
	}
}

func TestCanceledResumeLeaderDoesNotCancelOtherCallers(t *testing.T) {
	manager := &cancelFirstResumeManager{fakeManager: newFakeManager(), firstEntered: make(chan struct{})}
	entry := &captureEntrypoint{}
	application := NewApplication(ApplicationSpec{
		Name: "resume-cancellation-agent",
		Modules: []agentslot.Module{
			componentsModule{manager: manager},
			NewEntrypointModule("entrypoint.test", "test", entry),
		},
	})
	runtime, err := application.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Stop(context.Background()) })

	leaderContext, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := entry.Access().ResumeSession(leaderContext, interaction.ResumeSessionRequest{SessionID: "session-1"})
		leaderDone <- err
	}()
	<-manager.firstEntered
	followerDone := make(chan error, 1)
	go func() {
		_, err := entry.Access().ResumeSession(context.Background(), interaction.ResumeSessionRequest{SessionID: "session-1"})
		followerDone <- err
	}()
	cancelLeader()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader error = %v, want context.Canceled", err)
	}
	if err := <-followerDone; err != nil {
		t.Fatalf("follower resume: %v", err)
	}
	if got := manager.ResumeCalls(); got != 2 {
		t.Fatalf("SessionManager.Resume calls = %d, want 2", got)
	}
}

func TestCreateRejectsSessionIDCollisionInsteadOfReturningExistingRuntime(t *testing.T) {
	entry := &captureEntrypoint{}
	application := NewApplication(ApplicationSpec{
		Name: "create-collision-agent",
		Modules: []agentslot.Module{
			componentsModule{manager: newFakeManager()},
			NewEntrypointModule("entrypoint.test", "test", entry),
		},
	})
	runtime, err := application.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Stop(context.Background()) })
	request := interaction.CreateSessionRequest{AgentID: "agent-1", WorkspaceID: "workspace-1"}
	if _, err := entry.Access().CreateSession(context.Background(), request); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err = entry.Access().CreateSession(context.Background(), request)
	if !agent.IsCode(err, agent.CodeSessionAlreadyOpen) {
		t.Fatalf("second create error = %v, code=%q", err, agent.CodeOf(err))
	}
}

func TestCloseRemovesRuntimeWithoutDeletingSession(t *testing.T) {
	manager := newFakeManager()
	entry := &captureEntrypoint{}
	application := NewApplication(ApplicationSpec{
		Name: "close-agent",
		Modules: []agentslot.Module{
			componentsModule{manager: manager},
			NewEntrypointModule("entrypoint.test", "test", entry),
		},
	})
	runtime, err := application.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	access := entry.Access()
	if _, err := access.ResumeSession(context.Background(), interaction.ResumeSessionRequest{SessionID: "session-1"}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := access.CloseSession(context.Background(), interaction.CloseSessionRequest{SessionID: "session-1"}); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := access.ResumeSession(context.Background(), interaction.ResumeSessionRequest{SessionID: "session-1"}); err != nil {
		t.Fatalf("resume after close: %v", err)
	}
	if got := manager.ResumeCalls(); got != 2 {
		t.Fatalf("SessionManager.Resume calls = %d, want 2", got)
	}
	if err := runtime.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	_, err = access.Snapshot(context.Background(), interaction.SnapshotRequest{SessionID: "session-1"})
	if !agent.IsCode(err, agent.CodeApplicationNotStarted) {
		t.Fatalf("post-stop Snapshot error = %v, code=%q", err, agent.CodeOf(err))
	}
}

func TestCloseWaitsForConcurrentResumeAndLeavesNoRuntimeBehind(t *testing.T) {
	manager := &blockingResumeManager{
		fakeManager: newFakeManager(),
		entered:     make(chan struct{}),
		release:     make(chan struct{}),
	}
	entry := &captureEntrypoint{}
	application := NewApplication(ApplicationSpec{
		Name: "resume-close-agent",
		Modules: []agentslot.Module{
			componentsModule{manager: manager},
			NewEntrypointModule("entrypoint.test", "test", entry),
		},
	})
	runtime, err := application.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Stop(context.Background()) })

	resumeDone := make(chan error, 1)
	go func() {
		_, err := entry.Access().ResumeSession(context.Background(), interaction.ResumeSessionRequest{SessionID: "session-1"})
		resumeDone <- err
	}()
	<-manager.entered
	closeDone := make(chan error, 1)
	go func() {
		closeDone <- entry.Access().CloseSession(context.Background(), interaction.CloseSessionRequest{SessionID: "session-1"})
	}()
	close(manager.release)
	if err := <-resumeDone; err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("close concurrent with resume: %v", err)
	}
	_, err = entry.Access().Snapshot(context.Background(), interaction.SnapshotRequest{SessionID: "session-1"})
	if !agent.IsCode(err, agent.CodeSessionNotOpen) {
		t.Fatalf("Snapshot after concurrent close error = %v, code=%q", err, agent.CodeOf(err))
	}
}

type captureEntrypoint struct {
	mu     sync.Mutex
	access interaction.GatewayAccess
}

func (e *captureEntrypoint) Attach(access interaction.GatewayAccess) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.access = access
	return nil
}

func (e *captureEntrypoint) Access() interaction.GatewayAccess {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.access
}

type componentsModule struct {
	manager session.SessionManager
}

type rawEntrypointModule struct {
	entrypoint interaction.Entrypoint
}

func (rawEntrypointModule) ID() string { return "entrypoint.raw" }

func (m rawEntrypointModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Add(interaction.EntrypointSlot, "raw", m.entrypoint))
}

func (m componentsModule) ID() string { return "test.components" }
func (m componentsModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(
		agentslot.Set(session.ManagerSlot, session.SessionManager(m.manager)),
		agentslot.Set(session.StoreSlot, session.SessionStore(fakeStore{})),
		agentslot.Set(model.ExecutorSlot, model.ModelExecutor(fakeExecutor{})),
	)
}

type fakeManager struct {
	mu          sync.Mutex
	resumeCalls int
}

type blockingResumeManager struct {
	*fakeManager
	entered chan struct{}
	release chan struct{}
}

type cancelFirstResumeManager struct {
	*fakeManager
	firstEntered chan struct{}
}

type typedNilResumeManager struct {
	*fakeManager
}

func (typedNilResumeManager) Resume(context.Context, session.ResumeRequest) (session.Session, error) {
	var result *fakeSession
	return result, nil
}

func (m *cancelFirstResumeManager) Resume(ctx context.Context, request session.ResumeRequest) (session.Session, error) {
	m.mu.Lock()
	m.resumeCalls++
	call := m.resumeCalls
	m.mu.Unlock()
	if call == 1 {
		close(m.firstEntered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return fakeSession{id: request.SessionID, revision: 3}, nil
}

func (m *blockingResumeManager) Resume(ctx context.Context, request session.ResumeRequest) (session.Session, error) {
	m.mu.Lock()
	m.resumeCalls++
	m.mu.Unlock()
	close(m.entered)
	select {
	case <-m.release:
		return fakeSession{id: request.SessionID, revision: 3}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func newFakeManager() *fakeManager { return &fakeManager{} }

func (m *fakeManager) Create(context.Context, session.CreateRequest) (session.Session, error) {
	return fakeSession{id: "created-session", revision: 1}, nil
}
func (m *fakeManager) Resume(_ context.Context, request session.ResumeRequest) (session.Session, error) {
	m.mu.Lock()
	m.resumeCalls++
	m.mu.Unlock()
	return fakeSession{id: request.SessionID, revision: 3}, nil
}
func (m *fakeManager) Fork(context.Context, session.ForkRequest) (session.Session, error) {
	return fakeSession{id: "forked-session", revision: 1}, nil
}
func (m *fakeManager) StartFromSummary(context.Context, session.SummaryRequest) (session.Session, error) {
	return fakeSession{id: "summary-session", revision: 1}, nil
}
func (m *fakeManager) ResumeCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resumeCalls
}

type fakeSession struct {
	id       agent.SessionID
	revision agent.Revision
}

func (s fakeSession) ID() agent.SessionID      { return s.id }
func (s fakeSession) Revision() agent.Revision { return s.revision }
func (s fakeSession) View(context.Context) (session.Snapshot, error) {
	return session.Snapshot{
		Session:  agent.Session{ID: s.id, Revision: s.revision},
		Revision: s.revision,
	}, nil
}

type fakeStore struct{}

func (fakeStore) Create(context.Context, session.NewSession) (session.Snapshot, error) {
	return session.Snapshot{}, nil
}
func (fakeStore) Load(context.Context, session.SessionRef) (session.Snapshot, error) {
	return session.Snapshot{}, nil
}
func (fakeStore) Commit(context.Context, session.CommitRequest) (session.Commit, error) {
	return session.Commit{}, nil
}

type fakeExecutor struct{}

func (fakeExecutor) Execute(context.Context, model.ModelRequest) (model.ModelStream, error) {
	return nil, errors.New("not used in application skeleton test")
}
