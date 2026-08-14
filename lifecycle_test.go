package agentslot_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
)

type lifecycleModule struct {
	testModule
	events   *[]string
	startErr error
	stopErr  error
}

func (m *lifecycleModule) Start(context.Context) error {
	*m.events = append(*m.events, "start:"+m.id)
	return m.startErr
}

func (m *lifecycleModule) Stop(context.Context) error {
	*m.events = append(*m.events, "stop:"+m.id)
	return m.stopErr
}

func TestStartFailureRollsBackStartedModulesInReverseOrder(t *testing.T) {
	var events []string
	builder := agentslot.NewBuilder()
	for _, module := range []*lifecycleModule{
		{testModule: testModule{id: "a"}, events: &events},
		{testModule: testModule{id: "b"}, events: &events},
		{testModule: testModule{id: "c"}, events: &events, startErr: errors.New("cannot start")},
	} {
		if err := builder.Install(module); err != nil {
			t.Fatalf("install %s: %v", module.id, err)
		}
	}
	plan, err := builder.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	runtime, err := plan.Start(context.Background())
	if err == nil || runtime != nil {
		t.Fatalf("Start() = %#v, %v; want nil runtime and error", runtime, err)
	}
	want := []string{"start:a", "start:b", "start:c", "stop:b", "stop:a"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

func TestRuntimeStopsModulesInReverseOrderAndOnlyOnce(t *testing.T) {
	var events []string
	builder := agentslot.NewBuilder()
	for _, module := range []*lifecycleModule{
		{testModule: testModule{id: "a"}, events: &events},
		{testModule: testModule{id: "b"}, events: &events},
	} {
		if err := builder.Install(module); err != nil {
			t.Fatalf("install %s: %v", module.id, err)
		}
	}
	plan, err := builder.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	runtime, err := plan.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := runtime.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := runtime.Stop(context.Background()); err != nil {
		t.Fatalf("second stop: %v", err)
	}

	want := []string{"start:a", "start:b", "stop:b", "stop:a"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
	if _, err := plan.Start(context.Background()); !errors.Is(err, agentslot.ErrPlanStarted) {
		t.Fatalf("second Start() error = %v, want ErrPlanStarted", err)
	}
}

func TestStopAttemptsEveryModuleAndJoinsErrors(t *testing.T) {
	var events []string
	errA := errors.New("stop a")
	errB := errors.New("stop b")
	builder := agentslot.NewBuilder()
	for _, module := range []*lifecycleModule{
		{testModule: testModule{id: "a"}, events: &events, stopErr: errA},
		{testModule: testModule{id: "b"}, events: &events, stopErr: errB},
	} {
		if err := builder.Install(module); err != nil {
			t.Fatalf("install %s: %v", module.id, err)
		}
	}
	plan, err := builder.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	runtime, err := plan.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	err = runtime.Stop(context.Background())
	if !errors.Is(err, errA) || !errors.Is(err, errB) {
		t.Fatalf("Stop() error = %v, want both stop errors", err)
	}
	want := []string{"start:a", "start:b", "stop:b", "stop:a"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}
