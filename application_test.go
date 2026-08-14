package agentslot_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
)

func TestApplicationBuildsMountedModulesAndReusesPlan(t *testing.T) {
	dependency := agentslot.One[string]("application.dependency")
	entry := agentslot.One[string]("application.entry")
	application := agentslot.NewApplication(
		"test-agent",
		[]agentslot.Module{
			testModule{
				id:            "dependency",
				contributions: []agentslot.Contribution{agentslot.Set(dependency, "mounted")},
			},
			&dependentModule{
				testModule: testModule{
					id: "entry",
					contributions: []agentslot.Contribution{
						agentslot.SetWith(entry, func(resolver agentslot.Resolver) (string, error) {
							return agentslot.ResolveOne(resolver, dependency)
						}),
					},
				},
				requirements: []agentslot.Requirement{agentslot.RequireOne(dependency)},
			},
		},
		agentslot.RequireOne(entry),
	)
	if got := application.Name(); got != "test-agent" {
		t.Fatalf("Name() = %q, want test-agent", got)
	}

	first, err := application.Build()
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	second, err := application.Build()
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	if first != second {
		t.Fatal("Build() did not reuse the successfully assembled plan")
	}
	if got, ok := agentslot.Get(first, entry); !ok || got != "mounted" {
		t.Fatalf("entry = %q, %v; want mounted, true", got, ok)
	}
}

func TestApplicationCopiesTheDeclaredModuleList(t *testing.T) {
	entry := agentslot.One[string]("application.copied.entry")
	modules := []agentslot.Module{
		testModule{id: "original", contributions: []agentslot.Contribution{agentslot.Set(entry, "original")}},
	}
	application := agentslot.NewApplication("copied-agent", modules, agentslot.RequireOne(entry))
	modules[0] = testModule{id: "replacement", contributions: []agentslot.Contribution{agentslot.Set(entry, "replacement")}}

	plan, err := application.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got, ok := agentslot.Get(plan, entry); !ok || got != "original" {
		t.Fatalf("entry = %q, %v; want original, true", got, ok)
	}
}

func TestApplicationStartAutomaticallyBuildsAndExposesPlan(t *testing.T) {
	entry := agentslot.One[string]("application.start.entry")
	var events []string
	application := agentslot.NewApplication(
		"start-agent",
		[]agentslot.Module{
			&lifecycleModule{
				testModule: testModule{
					id:            "entry",
					contributions: []agentslot.Contribution{agentslot.Set(entry, "ready")},
				},
				events: &events,
			},
		},
		agentslot.RequireOne(entry),
	)

	runtime, err := application.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if got, ok := agentslot.Get(runtime.Plan(), entry); !ok || got != "ready" {
		t.Fatalf("runtime entry = %q, %v; want ready, true", got, ok)
	}
	if err := runtime.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if want := []string{"start:entry", "stop:entry"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

func TestApplicationBuildReportsItsNameAndPreservesCause(t *testing.T) {
	entry := agentslot.One[string]("application.missing.entry")
	application := agentslot.NewApplication(
		"broken-agent",
		nil,
		agentslot.RequireOne(entry),
	)

	_, err := application.Build()
	if !errors.Is(err, agentslot.ErrRequirementUnsatisfied) {
		t.Fatalf("Build() error = %v, want ErrRequirementUnsatisfied", err)
	}
	if !strings.Contains(err.Error(), `build application "broken-agent"`) {
		t.Fatalf("Build() error = %q, want application name", err)
	}
}

func TestApplicationRejectsInvalidName(t *testing.T) {
	application := agentslot.NewApplication(" unnamed ", nil)
	_, err := application.Build()
	if !errors.Is(err, agentslot.ErrInvalidApplication) {
		t.Fatalf("Build() error = %v, want ErrInvalidApplication", err)
	}
}

func TestApplicationRunStartsWaitsAndStops(t *testing.T) {
	var events []string
	started := make(chan struct{})
	module := &signalingLifecycleModule{
		testModule: testModule{id: "service"},
		events:     &events,
		started:    started,
	}
	application := agentslot.NewApplication("service-agent", []agentslot.Module{module})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- application.Run(ctx) }()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Run() did not start mounted modules")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not stop after context cancellation")
	}
	if want := []string{"start:service", "stop:service"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

type signalingLifecycleModule struct {
	testModule
	events  *[]string
	started chan<- struct{}
}

func (m *signalingLifecycleModule) Start(context.Context) error {
	*m.events = append(*m.events, "start:"+m.id)
	close(m.started)
	return nil
}

func (m *signalingLifecycleModule) Stop(context.Context) error {
	*m.events = append(*m.events, "stop:"+m.id)
	return nil
}
