package standardagent

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
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
