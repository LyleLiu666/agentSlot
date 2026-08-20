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
		Name: "test-agent", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: newSeededStore()},
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
	store := newSeededStore()
	entry := &captureEntrypoint{}
	application := NewApplication(ApplicationSpec{
		Name: "snapshot-agent", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: store},
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
	if err != nil || snapshot.Revision != 1 {
		t.Fatalf("behind Snapshot = %#v, %v; want current revision 1", snapshot, err)
	}
	if len(snapshot.History) != 1 || snapshot.History[0].Message == nil || snapshot.History[0].Message.SessionID != "session-1" {
		t.Fatalf("Snapshot history = %#v, want complete Session history projection", snapshot.History)
	}
	_, err = access.Snapshot(context.Background(), interaction.SnapshotRequest{
		SessionID: "session-1", KnownRevision: 4,
	})
	if !agent.IsCode(err, agent.CodeRevisionConflict) {
		t.Fatalf("ahead Snapshot error = %v, code=%q", err, agent.CodeOf(err))
	}
}

func TestGatewayRejectsInvalidResumeBeforeCallingStore(t *testing.T) {
	store := newSeededStore()
	entry := &captureEntrypoint{}
	application := NewApplication(ApplicationSpec{
		Name: "validation-agent", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: store},
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
	if got := store.RecoverCalls(); got != 0 {
		t.Fatalf("SessionStore.Recover calls = %d, want 0", got)
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

func TestStandardApplicationRejectsNegativeContextLimitAtBuild(t *testing.T) {
	application := NewApplication(ApplicationSpec{
		Name: "invalid-context-limit", DefaultModelConfig: testDefaultModel(),
		RuntimeConfig: AgentRuntimeConfig{Context: ContextConfig{HardTokenLimit: -1}},
		Modules: []agentslot.Module{
			componentsModule{store: newSeededStore()},
			NewEntrypointModule("entrypoint.test", "test", &captureEntrypoint{}),
		},
	})
	if _, err := application.Build(); err == nil {
		t.Fatal("negative Context hard limit was accepted")
	}
}

func TestStandardApplicationRequiresValidDefaultModelConfigAtBuild(t *testing.T) {
	application := NewApplication(ApplicationSpec{
		Name: "missing-default-model",
		Modules: []agentslot.Module{
			componentsModule{store: newSeededStore()},
			NewEntrypointModule("entrypoint.test", "test", &captureEntrypoint{}),
		},
	})
	if _, err := application.Build(); err == nil {
		t.Fatal("Build succeeded without a valid Application default model configuration")
	}
}

func TestStandardApplicationRejectsEntrypointThatBypassesGatewayWrapper(t *testing.T) {
	application := NewApplication(ApplicationSpec{Name: "raw-entrypoint", DefaultModelConfig: testDefaultModel(), Modules: []agentslot.Module{
		componentsModule{store: newSeededStore()},
		rawEntrypointModule{entrypoint: &captureEntrypoint{}},
	}})
	_, err := application.Build()
	if !errors.Is(err, agentslot.ErrRequirementUnsatisfied) {
		t.Fatalf("Build error = %v, want ErrRequirementUnsatisfied", err)
	}
}

func TestStandardApplicationRejectsRawEntrypointAlongsideWrappedEntrypoint(t *testing.T) {
	application := NewApplication(ApplicationSpec{Name: "mixed-entrypoints", DefaultModelConfig: testDefaultModel(), Modules: []agentslot.Module{
		componentsModule{store: newSeededStore()},
		NewEntrypointModule("entrypoint.wrapped", "wrapped", &captureEntrypoint{}),
		rawEntrypointModule{entrypoint: &captureEntrypoint{}},
	}})
	if _, err := application.Build(); err == nil {
		t.Fatal("Build succeeded with a raw Entrypoint contribution")
	}
}

func TestConcurrentResumeCreatesOneRuntimePerSession(t *testing.T) {
	store := newSeededStore()
	entry := &captureEntrypoint{}
	application := NewApplication(ApplicationSpec{
		Name: "resume-agent", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: store},
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
	if got := store.RecoverCalls(); got != 1 {
		t.Fatalf("SessionStore.Recover calls = %d, want 1", got)
	}

	snapshot, err := access.Snapshot(context.Background(), interaction.SnapshotRequest{SessionID: "session-1"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.SessionID != "session-1" || snapshot.Revision != 1 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestSessionRuntimesShareOneAssembledComponentSet(t *testing.T) {
	entry := &captureEntrypoint{}
	application := NewApplication(ApplicationSpec{
		Name: "component-sharing-agent", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: newSeededStore()},
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
	store := &cancelFirstRecoverStore{seededStore: newSeededStore(), firstEntered: make(chan struct{})}
	entry := &captureEntrypoint{}
	application := NewApplication(ApplicationSpec{
		Name: "resume-cancellation-agent", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: store},
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
	<-store.firstEntered
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
	if got := store.RecoverCalls(); got != 2 {
		t.Fatalf("SessionStore.Recover calls = %d, want 2", got)
	}
}

func TestCreateAllocatesDistinctSessionIDs(t *testing.T) {
	entry := &captureEntrypoint{}
	application := NewApplication(ApplicationSpec{
		Name: "create-identity-agent", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: newSeededStore()},
			NewEntrypointModule("entrypoint.test", "test", entry),
		},
	})
	runtime, err := application.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Stop(context.Background()) })
	request := interaction.CreateSessionRequest{AgentID: "agent-1", WorkspaceID: "workspace-1"}
	first, err := entry.Access().CreateSession(context.Background(), request)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	second, err := entry.Access().CreateSession(context.Background(), request)
	if err != nil || second.SessionID == first.SessionID {
		t.Fatalf("second create = %#v, %v; first = %#v", second, err, first)
	}
}

func TestCloseRemovesRuntimeWithoutDeletingSession(t *testing.T) {
	store := newSeededStore()
	entry := &captureEntrypoint{}
	application := NewApplication(ApplicationSpec{
		Name: "close-agent", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: store},
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
	if got := store.RecoverCalls(); got != 2 {
		t.Fatalf("SessionStore.Recover calls = %d, want 2", got)
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
	store := &blockingRecoverStore{
		seededStore: newSeededStore(),
		entered:     make(chan struct{}),
		release:     make(chan struct{}),
	}
	entry := &captureEntrypoint{}
	application := NewApplication(ApplicationSpec{
		Name: "resume-close-agent", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: store},
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
	<-store.entered
	closeDone := make(chan error, 1)
	go func() {
		closeDone <- entry.Access().CloseSession(context.Background(), interaction.CloseSessionRequest{SessionID: "session-1"})
	}()
	close(store.release)
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
	store session.SessionStore
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
		agentslot.Set(session.StoreSlot, m.store),
		agentslot.Set(model.ExecutorSlot, model.ModelExecutor(fakeExecutor{})),
	)
}

type blockingRecoverStore struct {
	*seededStore
	entered chan struct{}
	release chan struct{}
}

type cancelFirstRecoverStore struct {
	*seededStore
	firstEntered chan struct{}
}

func (s *cancelFirstRecoverStore) Recover(ctx context.Context, request session.SessionRef) (session.Snapshot, error) {
	call := s.markRecover()
	if call == 1 {
		close(s.firstEntered)
		<-ctx.Done()
		return session.Snapshot{}, ctx.Err()
	}
	return s.inner.Recover(ctx, request)
}

func (s *blockingRecoverStore) Recover(ctx context.Context, request session.SessionRef) (session.Snapshot, error) {
	s.markRecover()
	close(s.entered)
	select {
	case <-s.release:
		return s.inner.Recover(ctx, request)
	case <-ctx.Done():
		return session.Snapshot{}, ctx.Err()
	}
}

type seededStore struct {
	inner        *session.MemoryStore
	mu           sync.Mutex
	recoverCalls int
}

func newSeededStore() *seededStore {
	store := &seededStore{inner: session.NewMemoryStore()}
	for _, id := range []agent.SessionID{"session-1", "session-2"} {
		message := agent.Message{ID: agent.MessageID(id + "-message"), SessionID: id, Role: agent.RoleUser, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "hello"}}}
		if _, err := store.inner.Create(context.Background(), session.NewSession{
			Session: agent.Session{ID: id, AgentID: "agent-1", WorkspaceID: "workspace-1"},
			History: []session.HistoryFact{{Message: &message}}, ModelConfig: testDefaultModel(), RunState: session.RunIdle,
		}); err != nil {
			panic(err)
		}
	}
	return store
}

func testDefaultModel() model.Config {
	return model.Config{ModelID: "default", Reasoning: model.ReasoningDefault}
}

func (s *seededStore) markRecover() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recoverCalls++
	return s.recoverCalls
}

func (s *seededStore) RecoverCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recoverCalls
}

func (s *seededStore) Create(ctx context.Context, initial session.NewSession) (session.Snapshot, error) {
	return s.inner.Create(ctx, initial)
}
func (s *seededStore) Load(ctx context.Context, ref session.SessionRef) (session.Snapshot, error) {
	return s.inner.Load(ctx, ref)
}
func (s *seededStore) Recover(ctx context.Context, ref session.SessionRef) (session.Snapshot, error) {
	s.markRecover()
	return s.inner.Recover(ctx, ref)
}
func (s *seededStore) Commit(ctx context.Context, request session.CommitRequest) (session.Commit, error) {
	return s.inner.Commit(ctx, request)
}
func (s *seededStore) HistoryPage(ctx context.Context, request session.HistoryPageRequest) (session.HistoryPage, error) {
	return s.inner.HistoryPage(ctx, request)
}

type fakeExecutor struct{}

func (fakeExecutor) Execute(context.Context, model.ModelRequest) (model.ModelStream, error) {
	return nil, errors.New("not used in application skeleton test")
}
func (fakeExecutor) Inspect(context.Context, model.Config) (model.ExecutionCapabilities, error) {
	return model.ExecutionCapabilities{
		Media:     model.Capabilities{InputModalities: []model.Modality{model.ModalityText}, OutputModalities: []model.Modality{model.ModalityText}},
		Reasoning: []model.Reasoning{model.ReasoningDefault}, ContextWindowTokens: 1000, MaxOutputTokens: 100,
	}, nil
}
func (fakeExecutor) CountTokens(context.Context, model.ModelRequest) (int, error) { return 0, nil }
