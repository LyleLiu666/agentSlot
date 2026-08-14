package agentslot_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
)

type dependentModule struct {
	testModule
	requirements []agentslot.Requirement
	events       *[]string
}

func (m *dependentModule) RequiredSlots() []agentslot.Requirement {
	return m.requirements
}

func (m *dependentModule) Start(context.Context) error {
	if m.events != nil {
		*m.events = append(*m.events, "start:"+m.id)
	}
	return nil
}

func (m *dependentModule) Stop(context.Context) error {
	if m.events != nil {
		*m.events = append(*m.events, "stop:"+m.id)
	}
	return nil
}

func TestRequiredSlotsOrderLifecycleAfterProviders(t *testing.T) {
	model := agentslot.One[string]("model")
	var events []string
	builder := agentslot.NewBuilder()

	consumer := &dependentModule{
		testModule:   testModule{id: "consumer"},
		requirements: []agentslot.Requirement{agentslot.RequireOne(model)},
		events:       &events,
	}
	provider := &dependentModule{
		testModule: testModule{
			id:            "provider",
			contributions: []agentslot.Contribution{agentslot.Set(model, "model")},
		},
		events: &events,
	}
	for _, module := range []agentslot.Module{consumer, provider} {
		if err := builder.Install(module); err != nil {
			t.Fatalf("install %s: %v", module.ID(), err)
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

	want := []string{"start:provider", "start:consumer", "stop:consumer", "stop:provider"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

func TestMissingModuleRequirementFailsBuildWithoutFreezing(t *testing.T) {
	model := agentslot.One[string]("model")
	builder := agentslot.NewBuilder()
	if err := builder.Install(&dependentModule{
		testModule:   testModule{id: "consumer"},
		requirements: []agentslot.Requirement{agentslot.RequireOne(model)},
	}); err != nil {
		t.Fatalf("install consumer: %v", err)
	}

	_, err := builder.Build()
	if !errors.Is(err, agentslot.ErrRequirementUnsatisfied) {
		t.Fatalf("Build() error = %v, want ErrRequirementUnsatisfied", err)
	}
	if err := builder.Install(testModule{
		id:            "provider",
		contributions: []agentslot.Contribution{agentslot.Set(model, "model")},
	}); err != nil {
		t.Fatalf("install provider after failed build: %v", err)
	}
	if _, err := builder.Build(); err != nil {
		t.Fatalf("second build: %v", err)
	}
}

func TestBuildRejectsModuleDependencyCycle(t *testing.T) {
	aSlot := agentslot.One[string]("a")
	bSlot := agentslot.One[string]("b")
	builder := agentslot.NewBuilder()
	for _, module := range []agentslot.Module{
		&dependentModule{
			testModule:   testModule{id: "a", contributions: []agentslot.Contribution{agentslot.Set(aSlot, "a")}},
			requirements: []agentslot.Requirement{agentslot.RequireOne(bSlot)},
		},
		&dependentModule{
			testModule:   testModule{id: "b", contributions: []agentslot.Contribution{agentslot.Set(bSlot, "b")}},
			requirements: []agentslot.Requirement{agentslot.RequireOne(aSlot)},
		},
	} {
		if err := builder.Install(module); err != nil {
			t.Fatalf("install %s: %v", module.ID(), err)
		}
	}

	_, err := builder.Build()
	if !errors.Is(err, agentslot.ErrDependencyCycle) {
		t.Fatalf("Build() error = %v, want ErrDependencyCycle", err)
	}
}

func TestRequireKeyDependsOnlyOnSelectedManyContribution(t *testing.T) {
	providers := agentslot.Many[string]("model.provider")
	consumerReady := agentslot.One[string]("consumer.ready")
	builder := agentslot.NewBuilder()

	consumer := &dependentModule{
		testModule: testModule{
			id:            "consumer",
			contributions: []agentslot.Contribution{agentslot.Set(consumerReady, "ready")},
		},
		requirements: []agentslot.Requirement{agentslot.RequireKey(providers, "selected")},
	}
	unselected := &dependentModule{
		testModule: testModule{
			id:            "provider.unselected",
			contributions: []agentslot.Contribution{agentslot.Add(providers, "other", "other")},
		},
		requirements: []agentslot.Requirement{agentslot.RequireOne(consumerReady)},
	}
	selected := &dependentModule{
		testModule: testModule{
			id:            "provider.selected",
			contributions: []agentslot.Contribution{agentslot.Add(providers, "selected", "selected")},
		},
	}
	for _, module := range []agentslot.Module{consumer, unselected, selected} {
		if err := builder.Install(module); err != nil {
			t.Fatalf("install %s: %v", module.ID(), err)
		}
	}

	plan, err := builder.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got := plan.Describe().Modules
	want := []agentslot.ModuleDescription{
		{ID: "provider.selected"},
		{ID: "consumer", Requires: []agentslot.RequirementDescription{{Slot: "model.provider", Kind: "many", Key: "selected", Minimum: 1}}},
		{ID: "provider.unselected", Requires: []agentslot.RequirementDescription{{Slot: "consumer.ready", Kind: "one", Minimum: 1}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("modules = %#v, want %#v", got, want)
	}
}

func TestInvalidRequirementFailsAtBuild(t *testing.T) {
	tools := agentslot.Many[string]("tool")
	for name, requirement := range map[string]agentslot.Requirement{
		"zero minimum": agentslot.RequireMany(tools, 0),
		"empty key":    agentslot.RequireKey(tools, ""),
		"padded key":   agentslot.RequireKey(tools, " shell "),
	} {
		t.Run(name, func(t *testing.T) {
			builder := agentslot.NewBuilder()
			_, err := builder.Build(requirement)
			if !errors.Is(err, agentslot.ErrInvalidRequirement) {
				t.Fatalf("Build() error = %v, want ErrInvalidRequirement", err)
			}
		})
	}
}
