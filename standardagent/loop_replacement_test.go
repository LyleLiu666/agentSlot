package standardagent

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/loop"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestStandardAgentLoopIsAnInspectableDefault(t *testing.T) {
	application := NewApplication(ApplicationSpec{
		Name: "default-loop", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: newSeededStore()},
			NewGatewayChannelModule("entrypoint.default-loop", "test", &captureChannel{}),
		},
	})
	assembly, err := application.Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, slot := range assembly.Describe().Slots {
		if slot.ID == loop.AgentLoopSlot.ID() && len(slot.Contributions) == 1 &&
			slot.Contributions[0].Module == standardLoopModuleID && slot.Contributions[0].Source == "default" {
			return
		}
	}
	t.Fatalf("standard Loop default is absent from Assembly: %#v", assembly.Describe().Slots)
}

func TestExplicitAgentLoopReplacesStandardDefaultWithoutRuntimeBranch(t *testing.T) {
	replacement := &recordingLoop{}
	replacementModule, err := loop.NewModule("test.agent-loop", replacement)
	if err != nil {
		t.Fatal(err)
	}
	executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{complete("replacement loop ran")}})
	entry := &captureChannel{}
	application := NewApplication(ApplicationSpec{
		Name: "replace-loop", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: newSeededStore(), executor: executor},
			replacementModule,
			NewGatewayChannelModule("entrypoint.loop-replacement", "test", entry),
		},
	})
	assembly, err := application.Build()
	if err != nil {
		t.Fatal(err)
	}
	description := assembly.Describe()
	for _, module := range description.Modules {
		if module.ID == standardLoopModuleID {
			t.Fatalf("overridden standard Loop remains active: %#v", description.Modules)
		}
	}
	found := false
	for _, slot := range description.Slots {
		if slot.ID == loop.AgentLoopSlot.ID() && len(slot.Contributions) == 1 &&
			slot.Contributions[0].Module == "test.agent-loop" && slot.Contributions[0].Source == "explicit" {
			found = true
		}
	}
	if !found {
		t.Fatalf("replacement Loop is absent from Assembly: %#v", description.Slots)
	}
	running, err := application.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = running.Stop(context.Background()) })
	opened, err := entry.Access().CreateSession(context.Background(), interaction.CreateSessionRequest{AgentID: "agent", WorkspaceID: "workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Access().Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("run"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := entry.Access().WhenIdle(context.Background(), interaction.WhenIdleRequest{SessionID: opened.SessionID}); err != nil {
		t.Fatal(err)
	}
	if replacement.calls.Load() != 1 || replacement.steps.Load() != 1 {
		t.Fatalf("replacement calls/steps = %d/%d", replacement.calls.Load(), replacement.steps.Load())
	}
}

func TestLegacyStepLoopRemainsUsableThroughControlledRuntime(t *testing.T) {
	replacement := &legacyStepLoop{}
	module, err := loop.NewModule("test.legacy-step-loop", replacement)
	if err != nil {
		t.Fatal(err)
	}
	installed := &countingTool{definition: testToolDefinition(t, "echo")}
	executor := model.NewFakeModelExecutor(
		model.FakeExecution{Events: []model.ModelEvent{{Kind: model.EventComplete, Output: &model.Completion{ToolCalls: []model.ToolCallRequest{{Name: "echo", Arguments: []byte(`{"value":"one"}`)}}}}}},
		model.FakeExecution{Events: []model.ModelEvent{complete("done")}},
	)
	access, stop := startRuntimeTestApplicationWithConfig(t, executor, AgentRuntimeConfig{ToolKeys: []string{"echo"}, MaxInlineToolResultBytes: 1024}, module, toolModule{key: "echo", value: installed})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("run")}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	if replacement.steps.Load() != 2 || installed.calls.Load() != 1 || len(executor.Requests()) != 2 {
		t.Fatalf("legacy steps=%d tool calls=%d model requests=%d", replacement.steps.Load(), installed.calls.Load(), len(executor.Requests()))
	}
}

type legacyStepLoop struct{ steps atomic.Int64 }

func (l *legacyStepLoop) Run(ctx context.Context, run loop.Run) (loop.Outcome, error) {
	for {
		outcome, err := run.Step(ctx)
		l.steps.Add(1)
		if err != nil || outcome != loop.OutcomeContinue {
			return outcome, err
		}
	}
}

type recordingLoop struct {
	calls   atomic.Int64
	steps   atomic.Int64
	mu      sync.Mutex
	actions []loop.ActionKind
}

func (l *recordingLoop) act(ctx context.Context, run loop.Run, action loop.Action) (loop.State, error) {
	l.mu.Lock()
	l.actions = append(l.actions, action.Kind)
	l.mu.Unlock()
	return run.Act(ctx, action)
}

func (l *recordingLoop) recordedActions() []loop.ActionKind {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]loop.ActionKind(nil), l.actions...)
}

func (l *recordingLoop) Run(ctx context.Context, run loop.Run) (loop.Outcome, error) {
	l.calls.Add(1)
	state := run.State()
	for {
		action := loop.Action{Kind: loop.ActionRequestModel}
		switch state {
		case loop.StateToolsReady:
			action.Kind = loop.ActionExecuteTools
		case loop.StateContinueReady:
			action.Kind = loop.ActionContinue
		case loop.StateReadyForModel:
		default:
			outcome := loopOutcomeFromState(state)
			if !outcome.Terminal() {
				return loop.OutcomeFailed, errors.New("unknown state")
			}
			if _, err := l.act(ctx, run, loop.Action{Kind: loop.ActionFinish, Outcome: outcome}); err != nil {
				return loop.OutcomeFailed, err
			}
			return outcome, nil
		}
		var err error
		state, err = l.act(ctx, run, action)
		l.steps.Add(1)
		if err != nil {
			return loop.OutcomeFailed, err
		}
	}
}

func TestAgentLoopControlsModelToolContinueAndFinishActionsInOrder(t *testing.T) {
	replacement := &recordingLoop{}
	module, err := loop.NewModule("test.controlled-loop", replacement)
	if err != nil {
		t.Fatal(err)
	}
	installed := &countingTool{definition: testToolDefinition(t, "echo")}
	executor := model.NewFakeModelExecutor(
		model.FakeExecution{Events: []model.ModelEvent{{Kind: model.EventComplete, Output: &model.Completion{ToolCalls: []model.ToolCallRequest{{Name: "echo", Arguments: []byte(`{"value":"one"}`)}}}}}},
		model.FakeExecution{Events: []model.ModelEvent{complete("done")}},
	)
	access, stop := startRuntimeTestApplicationWithConfig(t, executor, AgentRuntimeConfig{ToolKeys: []string{"echo"}, MaxInlineToolResultBytes: 1024}, module, toolModule{key: "echo", value: installed})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("run")}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	want := []loop.ActionKind{loop.ActionRequestModel, loop.ActionExecuteTools, loop.ActionContinue, loop.ActionRequestModel, loop.ActionFinish}
	if got := replacement.recordedActions(); !reflect.DeepEqual(got, want) {
		t.Fatalf("controlled actions = %#v, want %#v", got, want)
	}
	if installed.calls.Load() != 1 || len(executor.Requests()) != 2 {
		t.Fatalf("tool calls=%d model requests=%d", installed.calls.Load(), len(executor.Requests()))
	}
}

func TestAgentLoopCanWaitWithoutCallingProvider(t *testing.T) {
	waiting := agentLoopFunc(func(ctx context.Context, run loop.Run) (loop.Outcome, error) {
		state, err := run.Act(ctx, loop.Action{Kind: loop.ActionWait})
		if err != nil || state != loop.StateWaiting {
			return loop.OutcomeFailed, err
		}
		return loop.OutcomeWaiting, nil
	})
	module, err := loop.NewModule("test.waiting-loop", waiting)
	if err != nil {
		t.Fatal(err)
	}
	executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{complete("must not run")}})
	access, stop := startRuntimeTestApplication(t, executor, module)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("wait")}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	if len(executor.Requests()) != 0 {
		t.Fatalf("waiting strategy made %d model requests", len(executor.Requests()))
	}
	view, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if lastRunTerminal(view.RecentHistory) != session.RunInterrupted {
		t.Fatalf("waiting run terminal = %q", lastRunTerminal(view.RecentHistory))
	}
}

func TestAgentLoopCanFinishAReadyRunWithoutCallingProvider(t *testing.T) {
	finishing := agentLoopFunc(func(ctx context.Context, run loop.Run) (loop.Outcome, error) {
		if run.State() != loop.StateReadyForModel {
			return loop.OutcomeFailed, errors.New("unexpected initial state")
		}
		if _, err := run.Act(ctx, loop.Action{Kind: loop.ActionFinish, Outcome: loop.OutcomeCompleted}); err != nil {
			return loop.OutcomeFailed, err
		}
		return loop.OutcomeCompleted, nil
	})
	module, err := loop.NewModule("test.finishing-loop", finishing)
	if err != nil {
		t.Fatal(err)
	}
	executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{complete("must not run")}})
	access, stop := startRuntimeTestApplication(t, executor, module)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("finish")}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	if len(executor.Requests()) != 0 {
		t.Fatalf("finishing strategy made %d model requests", len(executor.Requests()))
	}
	view, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if lastRunTerminal(view.RecentHistory) != session.RunCompleted {
		t.Fatalf("finished run terminal = %q", lastRunTerminal(view.RecentHistory))
	}
}

func TestAgentLoopIllegalActionsFailWithoutCallingProvider(t *testing.T) {
	tests := []struct {
		name string
		loop loop.AgentLoop
	}{
		{name: "tools before preparation", loop: agentLoopFunc(func(ctx context.Context, run loop.Run) (loop.Outcome, error) {
			_, err := run.Act(ctx, loop.Action{Kind: loop.ActionExecuteTools})
			return loop.OutcomeFailed, err
		})},
		{name: "spoof budget terminal", loop: agentLoopFunc(func(ctx context.Context, run loop.Run) (loop.Outcome, error) {
			_, err := run.Act(ctx, loop.Action{Kind: loop.ActionFinish, Outcome: loop.OutcomeBudgetExceeded})
			return loop.OutcomeFailed, err
		})},
		{name: "action after wait", loop: agentLoopFunc(func(ctx context.Context, run loop.Run) (loop.Outcome, error) {
			if _, err := run.Act(ctx, loop.Action{Kind: loop.ActionWait}); err != nil {
				return loop.OutcomeFailed, err
			}
			_, err := run.Act(ctx, loop.Action{Kind: loop.ActionRequestModel})
			return loop.OutcomeFailed, err
		})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			module, err := loop.NewModule("test.illegal-loop", test.loop)
			if err != nil {
				t.Fatal(err)
			}
			executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{complete("must not run")}})
			access, stop := startRuntimeTestApplication(t, executor, module)
			defer stop()
			opened := createRuntimeTestSession(t, access)
			if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("illegal")}); err != nil {
				t.Fatal(err)
			}
			waitRuntimeIdle(t, access, opened.SessionID)
			if len(executor.Requests()) != 0 {
				t.Fatalf("illegal strategy made %d model requests", len(executor.Requests()))
			}
			view, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: opened.SessionID})
			if err != nil {
				t.Fatal(err)
			}
			if lastRunTerminal(view.RecentHistory) != session.RunFailed {
				t.Fatalf("illegal run terminal = %q", lastRunTerminal(view.RecentHistory))
			}
		})
	}
}

func TestAgentLoopConcurrentActionsAreRejectedAndJoined(t *testing.T) {
	block := make(chan struct{})
	executor := model.NewFakeModelExecutor(model.FakeExecution{Block: block, Events: []model.ModelEvent{complete("done")}})
	concurrentErr := make(chan error, 1)
	strategy := agentLoopFunc(func(ctx context.Context, run loop.Run) (loop.Outcome, error) {
		type result struct {
			state loop.State
			err   error
		}
		first := make(chan result, 1)
		go func() {
			state, err := run.Act(ctx, loop.Action{Kind: loop.ActionRequestModel})
			first <- result{state: state, err: err}
		}()
		if err := executor.WaitForRequests(ctx, 1); err != nil {
			close(block)
			<-first
			return loop.OutcomeFailed, err
		}
		_, err := run.Act(ctx, loop.Action{Kind: loop.ActionRequestModel})
		concurrentErr <- err
		close(block)
		completed := <-first
		if completed.err != nil || completed.state != loop.StateCompleted {
			return loop.OutcomeFailed, errors.New("first action did not complete")
		}
		return loop.OutcomeFailed, err
	})
	module, err := loop.NewModule("test.concurrent-loop", strategy)
	if err != nil {
		t.Fatal(err)
	}
	access, stop := startRuntimeTestApplication(t, executor, module)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("concurrent")}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	select {
	case err := <-concurrentErr:
		if err == nil || err.Error() != "standardagent: AgentLoop submitted concurrent actions" {
			t.Fatalf("concurrent action error = %v", err)
		}
	default:
		t.Fatal("concurrent action did not report its rejection")
	}
	view, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if lastRunTerminal(view.RecentHistory) != session.RunFailed {
		t.Fatalf("concurrent run terminal = %q", lastRunTerminal(view.RecentHistory))
	}
}

func TestAgentLoopReturnCancelsAndJoinsAnEscapedAction(t *testing.T) {
	block := make(chan struct{})
	executor := model.NewFakeModelExecutor(model.FakeExecution{Block: block, Events: []model.ModelEvent{complete("late")}})
	type actionResult struct {
		state loop.State
		err   error
	}
	actionDone := make(chan actionResult, 1)
	strategy := agentLoopFunc(func(ctx context.Context, run loop.Run) (loop.Outcome, error) {
		go func() {
			state, err := run.Act(ctx, loop.Action{Kind: loop.ActionRequestModel})
			actionDone <- actionResult{state: state, err: err}
		}()
		if err := executor.WaitForRequests(ctx, 1); err != nil {
			return loop.OutcomeFailed, err
		}
		return loop.OutcomeFailed, errors.New("strategy returned before its action")
	})
	module, err := loop.NewModule("test.escaped-action-loop", strategy)
	if err != nil {
		t.Fatal(err)
	}
	access, stop := startRuntimeTestApplication(t, executor, module)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("escape")}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	select {
	case result := <-actionDone:
		if result.err != nil || result.state != loop.StateCanceled {
			t.Fatalf("escaped action result = %#v", result)
		}
	default:
		t.Fatal("Runtime became idle before the escaped action was joined")
	}
	view, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if lastRunTerminal(view.RecentHistory) != session.RunFailed {
		t.Fatalf("escaped-action run terminal = %q", lastRunTerminal(view.RecentHistory))
	}
}

var _ loop.AgentLoop = (*recordingLoop)(nil)

func TestAgentLoopProtocolFailuresBecomeFailedRuns(t *testing.T) {
	tests := []struct {
		name string
		loop loop.AgentLoop
	}{
		{name: "panic", loop: agentLoopFunc(func(context.Context, loop.Run) (loop.Outcome, error) {
			panic("private panic value")
		})},
		{name: "non-terminal return", loop: agentLoopFunc(func(context.Context, loop.Run) (loop.Outcome, error) {
			return loop.OutcomeContinue, nil
		})},
		{name: "terminal return without finish", loop: agentLoopFunc(func(context.Context, loop.Run) (loop.Outcome, error) {
			return loop.OutcomeCompleted, nil
		})},
		{name: "error", loop: agentLoopFunc(func(context.Context, loop.Run) (loop.Outcome, error) {
			return loop.OutcomeFailed, errors.New("loop failed")
		})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			module, err := loop.NewModule("test.agent-loop", test.loop)
			if err != nil {
				t.Fatal(err)
			}
			executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{complete("unused")}})
			access, stop := startRuntimeTestApplication(t, executor, module)
			defer stop()
			opened := createRuntimeTestSession(t, access)
			if _, err := access.Send(context.Background(), interaction.SendRequest{
				SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("run"),
			}); err != nil {
				t.Fatal(err)
			}
			if err := access.WhenIdle(context.Background(), interaction.WhenIdleRequest{SessionID: opened.SessionID}); err != nil {
				t.Fatal(err)
			}
			view, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: opened.SessionID})
			if err != nil {
				t.Fatal(err)
			}
			runs := historyRunFacts(view.RecentHistory)
			if len(runs) != 2 || runs[1].Kind != "failed" {
				t.Fatalf("run facts = %#v", runs)
			}
		})
	}
}

type agentLoopFunc func(context.Context, loop.Run) (loop.Outcome, error)

func (f agentLoopFunc) Run(ctx context.Context, run loop.Run) (loop.Outcome, error) {
	return f(ctx, run)
}
