package standardagent

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestStandardApplicationLifecycleSurroundsGatewayChannels(t *testing.T) {
	events := &eventLog{}
	entry := &lifecycleChannel{captureChannel: captureChannel{}, events: events}
	application := NewApplication(ApplicationSpec{
		Name: "lifecycle-agent", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			&lifecycleComponentsModule{componentsModule: componentsModule{store: newSeededStore()}, events: events},
			NewGatewayChannelModule("entrypoint.test", "test", entry),
		},
	})
	runtime, err := application.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := runtime.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	want := []string{"start:components", "start:entrypoint", "stop:entrypoint", "stop:components"}
	if got := events.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("lifecycle events = %#v, want %#v", got, want)
	}
}

func TestGatewayChannelStartFailureRollsBackGatewayAndComponents(t *testing.T) {
	events := &eventLog{}
	startErr := errors.New("listener failed")
	entry := &lifecycleChannel{captureChannel: captureChannel{}, events: events, startErr: startErr}
	application := NewApplication(ApplicationSpec{
		Name: "rollback-agent", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			&lifecycleComponentsModule{componentsModule: componentsModule{store: newSeededStore()}, events: events},
			NewGatewayChannelModule("entrypoint.test", "test", entry),
		},
	})
	_, err := application.Start(context.Background())
	if !errors.Is(err, startErr) {
		t.Fatalf("start error = %v, want %v", err, startErr)
	}
	_, err = entry.Access().View(context.Background(), interaction.SessionViewRequest{SessionID: "session-1"})
	if !agent.IsCode(err, agent.CodeApplicationNotStarted) {
		t.Fatalf("Snapshot after rollback error = %v, code=%q", err, agent.CodeOf(err))
	}
	want := []string{"start:components", "start:entrypoint", "stop:components"}
	if got := events.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("rollback events = %#v, want %#v", got, want)
	}
}

func TestStopCancelsInFlightGatewayOperationBeforeWaitingForDrain(t *testing.T) {
	block := make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(block) })
	executor := model.NewFakeModelExecutor(model.FakeExecution{
		Block: block, Events: []model.ModelEvent{complete("late")},
	})
	entry := &captureChannel{}
	application := NewApplication(ApplicationSpec{
		Name: "stop-active-run", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			session.NewMemoryModule(),
			executorModule{executor: executor},
			NewGatewayChannelModule("entrypoint.stop-active-run", "test", entry),
		},
	})
	running, err := application.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	opened, err := entry.Access().CreateSession(context.Background(), interaction.CreateSessionRequest{
		AgentID: "agent-1", WorkspaceID: "workspace-1",
	})
	if err != nil {
		t.Fatalf("create Session: %v", err)
	}
	executeDone := make(chan error, 1)
	go func() {
		_, err := entry.Access().SendAndWait(context.Background(), interaction.SendRequest{
			SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("wait"),
		})
		executeDone <- err
	}()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	if err := executor.WaitForRequests(waitCtx, 1); err != nil {
		t.Fatalf("wait for model request: %v", err)
	}

	stopDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		stopDone <- running.Stop(ctx)
	}()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		release.Do(func() { close(block) })
		<-stopDone
		t.Fatal("Stop waited for the Gateway operation before canceling its active Session Runtime")
	}
	select {
	case <-executeDone:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("canceled Gateway operation did not drain after Stop")
	}
}

func TestStopCancelsGatewayOperationBeforeSessionRuntimeIsRegistered(t *testing.T) {
	store := &blockingRecoverStore{
		seededStore: newSeededStore(), entered: make(chan struct{}), release: make(chan struct{}),
	}
	entry := &captureChannel{}
	application := NewApplication(ApplicationSpec{
		Name: "stop-active-resume", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: store},
			NewGatewayChannelModule("entrypoint.stop-active-resume", "test", entry),
		},
	})
	running, err := application.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	resumeDone := make(chan error, 1)
	go func() {
		_, err := entry.Access().ResumeSession(context.Background(), interaction.ResumeSessionRequest{SessionID: "session-1"})
		resumeDone <- err
	}()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("Session recovery did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := running.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case err := <-resumeDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("resume error = %v, want lifecycle cancellation", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("in-flight Session recovery remained blocked after Stop")
	}
}

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(event string) {
	l.mu.Lock()
	l.events = append(l.events, event)
	l.mu.Unlock()
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

type lifecycleComponentsModule struct {
	componentsModule
	events *eventLog
}

func (m *lifecycleComponentsModule) Start(context.Context) error {
	m.events.add("start:components")
	return nil
}

func (m *lifecycleComponentsModule) Stop(context.Context) error {
	m.events.add("stop:components")
	return nil
}

type lifecycleChannel struct {
	captureChannel
	events   *eventLog
	startErr error
}

func (e *lifecycleChannel) Start(context.Context) error {
	e.events.add("start:entrypoint")
	return e.startErr
}

func (e *lifecycleChannel) Stop(context.Context) error {
	e.events.add("stop:entrypoint")
	return nil
}
