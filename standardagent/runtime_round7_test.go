package standardagent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	agentcontext "github.com/LyleLiu666/agentSlot/context"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestRuntimeBuildsVersionedContextWithoutRewritingHistory(t *testing.T) {
	executor := newRound7Executor(
		func(model.Config) (model.ExecutionCapabilities, error) { return textCapabilities(100), nil },
		func(request model.ModelRequest) (int, error) { return len(request.Inputs), nil },
		model.FakeExecution{Events: []model.ModelEvent{complete("done")}},
	)
	source := &recordingContextSource{}
	access, store, stop := startRound7Application(t, executor, AgentRuntimeConfig{SystemPrompt: "fixed system"},
		contextSourceModule{source: source})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	input := agent.MessageInput{Parts: []agent.MessagePart{{
		Kind: agent.PartAttachment, AttachmentID: "image-1", MediaType: "image/png", Name: "screen.png",
	}}}
	if _, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: input,
	}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)

	requests := executor.fake.Requests()
	if len(requests) != 1 || len(requests[0].Inputs) != 3 || requests[0].Inputs[0].SystemPrompt == nil || *requests[0].Inputs[0].SystemPrompt != "fixed system" {
		t.Fatalf("assembled request = %#v", requests)
	}
	projected := requests[0].Inputs[1].Message.Parts
	if len(projected) != 1 || projected[0].Kind != agent.PartText || !strings.Contains(projected[0].Text, "image-1") {
		t.Fatalf("unsupported image projection = %#v", projected)
	}
	if requests[0].Inputs[2].Message.Parts[0].Text != "source" || source.calls != 1 {
		t.Fatalf("context source was not applied in order: %#v, calls=%d", requests[0].Inputs, source.calls)
	}

	snapshot, err := store.Load(context.Background(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	messages := historyMessageFacts(snapshot.History)
	if len(messages) != 2 || messages[0].Parts[0].Kind != agent.PartAttachment || messages[0].Parts[0].AttachmentID != "image-1" {
		t.Fatalf("History was rewritten by projection: %#v", messages)
	}
	if snapshot.Context.Version != 1 || snapshot.Context.SourceRevision == 0 || snapshot.Context.TokenCount != 3 || len(snapshot.Context.Request.Inputs) != 3 {
		t.Fatalf("persisted Context = %#v", snapshot.Context)
	}
	if snapshot.Context.Request.Inputs[0].SystemPrompt == nil || *snapshot.Context.Request.Inputs[0].SystemPrompt != "fixed system" {
		t.Fatal("complete Context did not retain the fixed SystemPrompt")
	}
}

func TestRuntimeUsesReplaceableCompactorBeforeHardTokenLimit(t *testing.T) {
	executor := newRound7Executor(
		func(model.Config) (model.ExecutionCapabilities, error) { return textCapabilities(100), nil },
		func(request model.ModelRequest) (int, error) { return len(request.Inputs), nil },
		model.FakeExecution{Events: []model.ModelEvent{complete("done")}},
	)
	compactor, err := agentcontext.NewTailCompactor(1)
	if err != nil {
		t.Fatal(err)
	}
	access, store, stop := startRound7Application(t, executor, AgentRuntimeConfig{
		SystemPrompt: "system", Context: ContextConfig{HardTokenLimit: 2},
	}, contextSourceModule{source: fixedContextSource("derived-1", "one")},
		contextSourceModule{id: "context.source.two", source: fixedContextSource("derived-2", "two")},
		compactorModule{compactor: compactor})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("original")}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	requests := executor.fake.Requests()
	if len(requests) != 1 || len(requests[0].Inputs) != 2 || requests[0].Inputs[1].Message.Parts[0].Text != "two" {
		t.Fatalf("compacted request = %#v", requests)
	}
	snapshot, err := store.Load(context.Background(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(historyMessageFacts(snapshot.History)) != 2 || len(snapshot.Context.Request.Inputs) != 2 {
		t.Fatalf("History/Context separation failed: %#v", snapshot)
	}
}

func TestRuntimeRejectsOversizedContextBeforeProviderCall(t *testing.T) {
	executor := newRound7Executor(
		func(model.Config) (model.ExecutionCapabilities, error) { return textCapabilities(1), nil },
		func(request model.ModelRequest) (int, error) { return len(request.Inputs), nil },
		model.FakeExecution{Events: []model.ModelEvent{complete("must not run")}},
	)
	access, _, stop := startRound7Application(t, executor, AgentRuntimeConfig{SystemPrompt: "system"})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("too large")}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	if got := len(executor.fake.Requests()); got != 0 {
		t.Fatalf("provider called %d times despite hard context limit", got)
	}
	snapshot, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	runs := historyRunFacts(snapshot.RecentHistory)
	if len(runs) != 2 || runs[1].Kind != session.RunFailed {
		t.Fatalf("run facts = %#v", runs)
	}
}

func TestRuntimeCanExplicitlySelectNoTools(t *testing.T) {
	executor := newRound7Executor(nil, nil, model.FakeExecution{Events: []model.ModelEvent{complete("done")}})
	installed := &countingTool{definition: testToolDefinition(t, "echo")}
	access, _, stop := startRound7Application(t, executor, AgentRuntimeConfig{ToolKeys: []string{}}, toolModule{key: "echo", value: installed})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("hello")}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	requests := executor.fake.Requests()
	if len(requests) != 1 || requests[0].Tools != nil {
		t.Fatalf("non-nil empty ToolKeys did not disable all Tools: %#v", requests)
	}
}

func TestRuntimeWithUnsetToolKeysExposesNoInstalledTools(t *testing.T) {
	executor := newRound7Executor(nil, nil, model.FakeExecution{Events: []model.ModelEvent{complete("done")}})
	installed := &countingTool{definition: testToolDefinition(t, "echo")}
	access, _, stop := startRound7Application(t, executor, AgentRuntimeConfig{}, toolModule{key: "echo", value: installed})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("hello")}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	requests := executor.fake.Requests()
	if len(requests) != 1 || requests[0].Tools != nil {
		t.Fatalf("unset ToolKeys exposed installed Tools: %#v", requests)
	}
}

func TestCancelIsNotBlockedByContextSource(t *testing.T) {
	source := &cancelableContextSource{entered: make(chan struct{})}
	executor := newRound7Executor(nil, nil, model.FakeExecution{Events: []model.ModelEvent{complete("unused")}})
	access, _, stop := startRound7Application(t, executor, AgentRuntimeConfig{}, contextSourceModule{source: source})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	sent := make(chan error, 1)
	go func() {
		_, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("start")})
		sent <- err
	}()
	select {
	case <-source.entered:
	case <-time.After(time.Second):
		t.Fatal("ContextSource was not entered")
	}
	view, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	canceled := make(chan error, 1)
	go func() {
		canceled <- access.Cancel(context.Background(), interaction.CancelRequest{SessionID: opened.SessionID, ExpectedRevision: view.Revision})
	}()
	select {
	case err := <-canceled:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Cancel was blocked behind replaceable ContextSource")
	}
	if err := <-sent; err != nil {
		t.Fatalf("Send acceptance failed after cancellation: %v", err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
}

func TestBeforeRunCompleteMayOnlyProposeInput(t *testing.T) {
	executor := newRound7Executor(nil, nil,
		model.FakeExecution{Events: []model.ModelEvent{complete("first")}},
		model.FakeExecution{Events: []model.ModelEvent{complete("second")}},
	)
	h := &recordingHook{proposal: textInput("hook follow-on")}
	access, _, stop := startRound7Application(t, executor, AgentRuntimeConfig{}, hookModule{hook: h})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("start")}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	requests := executor.fake.Requests()
	if len(requests) != 2 || requests[0].RunID != requests[1].RunID {
		t.Fatalf("hook did not continue the same Run: %#v", requests)
	}
	last := requests[1].Inputs[len(requests[1].Inputs)-1]
	if last.Message == nil || last.Message.Role != agent.RoleUser || last.Message.Parts[0].Text != "hook follow-on" {
		t.Fatalf("hook proposal was not normalized by Runtime: %#v", requests[1].Inputs)
	}
	if h.beforeCalls() != 2 {
		t.Fatalf("BeforeRunComplete calls = %d, want 2", h.beforeCalls())
	}
}

func TestModelSwitchRequiresExecutorSupportAndExplicitAttachmentLossConfirmation(t *testing.T) {
	executor := newRound7Executor(func(config model.Config) (model.ExecutionCapabilities, error) {
		switch config.ModelID {
		case "default":
			capabilities := textCapabilities(100)
			capabilities.Media.InputModalities = []model.Modality{model.ModalityText, model.ModalityImage}
			return capabilities, nil
		case "text-only":
			return textCapabilities(100), nil
		default:
			return model.ExecutionCapabilities{}, errors.New("unknown model")
		}
	}, nil, model.FakeExecution{Events: []model.ModelEvent{complete("seen")}})
	access, store, stop := startRound7Application(t, executor, AgentRuntimeConfig{})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	image := agent.MessageInput{Parts: []agent.MessagePart{{Kind: agent.PartAttachment, AttachmentID: "image-1", MediaType: "image/png"}}}
	if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: image}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	current, err := access.ModelConfig(context.Background(), interaction.ModelConfigRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	target := model.Config{ModelID: "text-only", Reasoning: model.ReasoningDefault}
	_, err = access.UpdateModelConfig(context.Background(), interaction.UpdateModelConfigRequest{
		SessionID: opened.SessionID, ExpectedRevision: current.Revision, Config: target,
	})
	if !agent.IsCode(err, agent.CodeCompatibilityConfirmationRequired) {
		t.Fatalf("unconfirmed switch error = %v, code=%q", err, agent.CodeOf(err))
	}
	commit, err := access.UpdateModelConfig(context.Background(), interaction.UpdateModelConfigRequest{
		SessionID: opened.SessionID, ExpectedRevision: current.Revision, Config: target, AcceptCompatibilityLoss: true,
	})
	if err != nil || commit.Revision <= current.Revision {
		t.Fatalf("confirmed switch = %#v, %v", commit, err)
	}
	persisted, err := store.Load(context.Background(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil || len(persisted.Events) != 1 || persisted.Events[0].Kind != session.EventModelConfigChanged || persisted.Events[0].Revision != commit.Revision || persisted.Events[0].ModelConfigChanged.Previous.ModelID != "default" || persisted.Events[0].ModelConfigChanged.Current.ModelID != "text-only" {
		t.Fatalf("model config event = %#v, %v", persisted.Events, err)
	}
	_, err = access.UpdateModelConfig(context.Background(), interaction.UpdateModelConfigRequest{
		SessionID: opened.SessionID, ExpectedRevision: commit.Revision,
		Config: model.Config{ModelID: "missing", Reasoning: model.ReasoningDefault},
	})
	if !agent.IsCode(err, agent.CodeModelNotSupported) {
		t.Fatalf("unknown model error = %v, code=%q", err, agent.CodeOf(err))
	}
}

type round7Executor struct {
	fake    *model.FakeModelExecutor
	inspect func(model.Config) (model.ExecutionCapabilities, error)
	count   func(model.ModelRequest) (int, error)
}

func newRound7Executor(inspect func(model.Config) (model.ExecutionCapabilities, error), count func(model.ModelRequest) (int, error), executions ...model.FakeExecution) *round7Executor {
	return &round7Executor{fake: model.NewFakeModelExecutor(executions...), inspect: inspect, count: count}
}

func (e *round7Executor) Execute(ctx context.Context, request model.ModelRequest, recorder model.AttemptRecorder) (model.ModelStream, error) {
	return e.fake.Execute(ctx, request, recorder)
}
func (e *round7Executor) Inspect(ctx context.Context, config model.Config) (model.ExecutionCapabilities, error) {
	if e.inspect != nil {
		return e.inspect(config)
	}
	return e.fake.Inspect(ctx, config)
}
func (e *round7Executor) CountTokens(ctx context.Context, request model.ModelRequest) (int, error) {
	if e.count != nil {
		return e.count(request)
	}
	return e.fake.CountTokens(ctx, request)
}

func textCapabilities(limit int) model.ExecutionCapabilities {
	return model.ExecutionCapabilities{
		Media:     model.Capabilities{InputModalities: []model.Modality{model.ModalityText}, OutputModalities: []model.Modality{model.ModalityText}, ToolCalling: true},
		Reasoning: []model.Reasoning{model.ReasoningDefault}, ContextWindowTokens: limit, MaxOutputTokens: limit,
	}
}

type sessionPairModule struct {
	store *session.MemoryStore
}

func (sessionPairModule) ID() string { return "test.session.round7" }
func (m sessionPairModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Set(session.StoreSlot, session.SessionStore(m.store)))
}

func startRound7Application(t *testing.T, executor model.ModelExecutor, config AgentRuntimeConfig, extras ...agentslot.Module) (interaction.GatewayAccess, *session.MemoryStore, func()) {
	t.Helper()
	store := session.NewMemoryStore()
	entry := &captureChannel{}
	modules := []agentslot.Module{sessionPairModule{store: store}, executorModule{executor: executor}}
	modules = append(modules, extras...)
	modules = append(modules, NewGatewayChannelModule("entrypoint.round7", "round7", entry))
	running, err := NewApplication(ApplicationSpec{Name: "round7", Modules: modules, RuntimeConfig: config, DefaultModelConfig: model.Config{ModelID: "default", Reasoning: model.ReasoningDefault}}).Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return entry.Access(), store, func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := running.Stop(ctx); err != nil {
			t.Errorf("stop: %v", err)
		}
	}
}

type contextSourceModule struct {
	id     string
	source agentcontext.ContextSource
}

func (m contextSourceModule) ID() string {
	if m.id != "" {
		return m.id
	}
	return "context.source.one"
}
func (m contextSourceModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Append(agentcontext.SourceSlot, m.source))
}

type recordingContextSource struct{ calls int }

func (*recordingContextSource) Key() string { return "recording" }

func (s *recordingContextSource) Contribute(_ context.Context, input agentcontext.ContextInput) ([]model.Input, error) {
	s.calls++
	message := &agent.Message{ID: "source-message", SessionID: input.SessionID, Role: agent.RoleUser, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "source"}}}
	return []model.Input{{Message: message}}, nil
}

type cancelableContextSource struct{ entered chan struct{} }

func (*cancelableContextSource) Key() string { return "cancelable" }

func (s *cancelableContextSource) Contribute(ctx context.Context, _ agentcontext.ContextInput) ([]model.Input, error) {
	close(s.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

type fixedContextSourceImpl struct{ id, text string }

func fixedContextSource(id, text string) agentcontext.ContextSource {
	return fixedContextSourceImpl{id: id, text: text}
}
func (s fixedContextSourceImpl) Key() string { return s.id }
func (s fixedContextSourceImpl) Contribute(_ context.Context, input agentcontext.ContextInput) ([]model.Input, error) {
	message := &agent.Message{ID: agent.MessageID(s.id), SessionID: input.SessionID, Role: agent.RoleUser, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: s.text}}}
	return []model.Input{{Message: message}}, nil
}

type compactorModule struct{ compactor agentcontext.ContextCompactor }

func (compactorModule) ID() string { return "context.compactor.test" }
func (m compactorModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Set(agentcontext.CompactorSlot, m.compactor))
}

type hookModule struct{ hook hook.AgentHook }

func (hookModule) ID() string { return "hook.round7" }
func (m hookModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Append(hook.HookSlot, m.hook))
}

type recordingHook struct {
	mu       sync.Mutex
	proposal agent.MessageInput
	before   int
}

func (h *recordingHook) BeforeRunComplete(context.Context, hook.RunCompleteView) (hook.FollowOnProposal, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.before++
	if h.before == 1 {
		return hook.FollowOnProposal{Messages: []agent.MessageInput{h.proposal}}, nil
	}
	return hook.FollowOnProposal{}, nil
}
func (h *recordingHook) beforeCalls() int { h.mu.Lock(); defer h.mu.Unlock(); return h.before }

func waitRuntimeIdle(t *testing.T, access interaction.GatewayAccess, id agent.SessionID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := access.WhenIdle(ctx, interaction.WhenIdleRequest{SessionID: id}); err != nil {
		t.Fatal(err)
	}
}
