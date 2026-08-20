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
	entry := &captureChannel{}
	application := NewApplication(ApplicationSpec{
		Name: "test-agent", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: newSeededStore()},
			NewGatewayChannelModule("entrypoint.test", "test", entry),
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
		t.Fatal("GatewayChannel did not receive GatewayAccess during Build")
	}
	if _, ok := access.(*gatewayBinding); !ok {
		t.Fatalf("GatewayChannel received %T, want the isolated GatewayAccess binding", access)
	}
	_, err = access.View(context.Background(), interaction.SessionViewRequest{SessionID: "session-1"})
	if !agent.IsCode(err, agent.CodeApplicationNotStarted) {
		t.Fatalf("pre-start View error = %v, code=%q", err, agent.CodeOf(err))
	}
}

func TestGatewayViewReturnsCurrentAuthoritativeRevision(t *testing.T) {
	store := newSeededStore()
	entry := &captureChannel{}
	application := NewApplication(ApplicationSpec{
		Name: "view-agent", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: store},
			NewGatewayChannelModule("entrypoint.test", "test", entry),
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
	snapshot, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: "session-1"})
	if err != nil || snapshot.Revision != 1 {
		t.Fatalf("View = %#v, %v; want current revision 1", snapshot, err)
	}
	if len(snapshot.RecentHistory) != 1 || snapshot.RecentHistory[0].Message == nil || snapshot.RecentHistory[0].Message.SessionID != "session-1" {
		t.Fatalf("View history = %#v, want recent Session history projection", snapshot.RecentHistory)
	}
}

func TestGatewayRejectsInvalidResumeBeforeCallingStore(t *testing.T) {
	store := newSeededStore()
	entry := &captureChannel{}
	application := NewApplication(ApplicationSpec{
		Name: "validation-agent", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: store},
			NewGatewayChannelModule("entrypoint.test", "test", entry),
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
		NewGatewayChannelModule("entrypoint.test", "test", &captureChannel{}),
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
			NewGatewayChannelModule("entrypoint.test", "test", &captureChannel{}),
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
			NewGatewayChannelModule("entrypoint.test", "test", &captureChannel{}),
		},
	})
	if _, err := application.Build(); err == nil {
		t.Fatal("Build succeeded without a valid Application default model configuration")
	}
}

func TestStandardApplicationRejectsGatewayChannelThatBypassesFrameworkWrapper(t *testing.T) {
	application := NewApplication(ApplicationSpec{Name: "raw-entrypoint", DefaultModelConfig: testDefaultModel(), Modules: []agentslot.Module{
		componentsModule{store: newSeededStore()},
		rawChannelModule{channel: &captureChannel{}},
	}})
	_, err := application.Build()
	if !errors.Is(err, agentslot.ErrRequirementUnsatisfied) {
		t.Fatalf("Build error = %v, want ErrRequirementUnsatisfied", err)
	}
}

func TestStandardApplicationRejectsRawChannelAlongsideWrappedChannel(t *testing.T) {
	application := NewApplication(ApplicationSpec{Name: "mixed-entrypoints", DefaultModelConfig: testDefaultModel(), Modules: []agentslot.Module{
		componentsModule{store: newSeededStore()},
		NewGatewayChannelModule("entrypoint.wrapped", "wrapped", &captureChannel{}),
		rawChannelModule{channel: &captureChannel{}},
	}})
	if _, err := application.Build(); err == nil {
		t.Fatal("Build succeeded with a raw GatewayChannel contribution")
	}
}

func TestGatewayChannelBindingSurvivesARetryableBuildFailure(t *testing.T) {
	channel := &singleBindChannel{}
	retry := &failFirstBuildModule{}
	application := NewApplication(ApplicationSpec{
		Name: "retry-channel-build", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: newSeededStore()},
			NewGatewayChannelModule("channel.retry", "retry", channel),
			retry,
		},
		Requirements: []agentslot.Requirement{agentslot.RequireOne(retryBuildSlot)},
	})
	if _, err := application.Build(); err == nil {
		t.Fatal("first Build succeeded; want transient constructor failure")
	}
	if _, err := application.Build(); err != nil {
		t.Fatalf("retry Build: %v", err)
	}
	if channel.BindCalls() != 1 {
		t.Fatalf("GatewayChannel Bind calls = %d, want 1 across Build retry", channel.BindCalls())
	}
}

func TestStandardApplicationRejectsInvalidToolWhitelistAtBuild(t *testing.T) {
	tests := []struct {
		name string
		keys []string
	}{
		{name: "empty key", keys: []string{""}},
		{name: "duplicate key", keys: []string{"echo", "echo"}},
		{name: "unknown key", keys: []string{"missing"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			installed := &countingTool{definition: testToolDefinition(t, "echo")}
			application := NewApplication(ApplicationSpec{
				Name: "invalid-tools-" + test.name, DefaultModelConfig: testDefaultModel(),
				RuntimeConfig: AgentRuntimeConfig{ToolKeys: test.keys},
				Modules: []agentslot.Module{
					componentsModule{store: newSeededStore()},
					toolModule{key: "echo", value: installed},
					NewGatewayChannelModule("channel.invalid-tools", "test", &captureChannel{}),
				},
			})
			if _, err := application.Build(); err == nil {
				t.Fatalf("Build accepted ToolKeys %#v", test.keys)
			}
		})
	}
}

func TestConcurrentResumeCreatesOneRuntimePerSession(t *testing.T) {
	store := newSeededStore()
	entry := &captureChannel{}
	application := NewApplication(ApplicationSpec{
		Name: "resume-agent", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: store},
			NewGatewayChannelModule("entrypoint.test", "test", entry),
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

	snapshot, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: "session-1"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.SessionID != "session-1" || snapshot.Revision != 1 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestSessionRuntimesShareOneAssembledComponentSet(t *testing.T) {
	entry := &captureChannel{}
	application := NewApplication(ApplicationSpec{
		Name: "component-sharing-agent", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: newSeededStore()},
			NewGatewayChannelModule("entrypoint.test", "test", entry),
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
	coordinator, _, release, err := fixedGateway.runtime.acquire(context.Background())
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
	entry := &captureChannel{}
	application := NewApplication(ApplicationSpec{
		Name: "resume-cancellation-agent", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: store},
			NewGatewayChannelModule("entrypoint.test", "test", entry),
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
	entry := &captureChannel{}
	application := NewApplication(ApplicationSpec{
		Name: "create-identity-agent", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: newSeededStore()},
			NewGatewayChannelModule("entrypoint.test", "test", entry),
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
	entry := &captureChannel{}
	application := NewApplication(ApplicationSpec{
		Name: "close-agent", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: store},
			NewGatewayChannelModule("entrypoint.test", "test", entry),
		},
	})
	runtime, err := application.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	access := entry.Access()
	opened, err := access.ResumeSession(context.Background(), interaction.ResumeSessionRequest{SessionID: "session-1"})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := access.CloseSession(context.Background(), interaction.CloseSessionRequest{SessionID: "session-1", ExpectedRevision: opened.Revision}); err != nil {
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
	_, err = access.View(context.Background(), interaction.SessionViewRequest{SessionID: "session-1"})
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
	entry := &captureChannel{}
	application := NewApplication(ApplicationSpec{
		Name: "resume-close-agent", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: store},
			NewGatewayChannelModule("entrypoint.test", "test", entry),
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
		closeDone <- entry.Access().CloseSession(context.Background(), interaction.CloseSessionRequest{SessionID: "session-1", ExpectedRevision: 1})
	}()
	close(store.release)
	if err := <-resumeDone; err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("close concurrent with resume: %v", err)
	}
	_, err = entry.Access().View(context.Background(), interaction.SessionViewRequest{SessionID: "session-1"})
	if !agent.IsCode(err, agent.CodeSessionNotOpen) {
		t.Fatalf("Snapshot after concurrent close error = %v, code=%q", err, agent.CodeOf(err))
	}
}

type captureChannel struct {
	mu     sync.Mutex
	access interaction.GatewayAccess
}

type singleBindChannel struct {
	mu    sync.Mutex
	calls int
}

func (c *singleBindChannel) Bind(interaction.GatewayAccess) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls > 1 {
		return errors.New("channel was bound more than once")
	}
	return nil
}

func (c *singleBindChannel) BindCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

var retryBuildSlot = agentslot.One[string]("standardagent.test.retry-build")

type failFirstBuildModule struct {
	mu       sync.Mutex
	attempts int
}

func (*failFirstBuildModule) ID() string { return "test.fail-first-build" }

func (m *failFirstBuildModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.SetWith(retryBuildSlot, func(agentslot.Resolver) (string, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.attempts++
		if m.attempts == 1 {
			return "", errors.New("transient constructor failure")
		}
		return "ready", nil
	}))
}

func (e *captureChannel) Bind(access interaction.GatewayAccess) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.access = access
	return nil
}

func (e *captureChannel) Access() interaction.GatewayAccess {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.access
}

type componentsModule struct {
	store    session.SessionStore
	executor model.ModelExecutor
}

type rawChannelModule struct {
	channel interaction.GatewayChannel
}

func (rawChannelModule) ID() string { return "entrypoint.raw" }

func (m rawChannelModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Add(interaction.ChannelSlot, "raw", m.channel))
}

func (m componentsModule) ID() string { return "test.components" }
func (m componentsModule) Register(reg agentslot.Registrar) error {
	executor := m.executor
	if executor == nil {
		executor = fakeExecutor{}
	}
	return reg.Contribute(
		agentslot.Set(session.StoreSlot, m.store),
		agentslot.Set(model.ExecutorSlot, executor),
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

func (fakeExecutor) Execute(context.Context, model.ModelRequest, model.AttemptRecorder) (model.ModelStream, error) {
	return nil, errors.New("not used in application skeleton test")
}
func (fakeExecutor) Inspect(context.Context, model.Config) (model.ExecutionCapabilities, error) {
	return model.ExecutionCapabilities{
		Media:     model.Capabilities{InputModalities: []model.Modality{model.ModalityText}, OutputModalities: []model.Modality{model.ModalityText}},
		Reasoning: []model.Reasoning{model.ReasoningDefault}, ContextWindowTokens: 1000, MaxOutputTokens: 100,
	}, nil
}
func (fakeExecutor) CountTokens(context.Context, model.ModelRequest) (int, error) { return 0, nil }
