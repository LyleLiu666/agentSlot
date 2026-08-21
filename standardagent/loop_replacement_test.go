package standardagent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/loop"
	"github.com/LyleLiu666/agentSlot/model"
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

type recordingLoop struct {
	calls atomic.Int64
	steps atomic.Int64
}

func (l *recordingLoop) Run(ctx context.Context, run loop.Run) (loop.Outcome, error) {
	l.calls.Add(1)
	for {
		outcome, err := run.Step(ctx)
		l.steps.Add(1)
		if err != nil || outcome != loop.OutcomeContinue {
			return outcome, err
		}
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
