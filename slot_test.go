package agentslot_test

import (
	"errors"
	"reflect"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
)

type testModule struct {
	id            string
	contributions []agentslot.Contribution
	afterRegister error
}

func (m testModule) ID() string { return m.id }

func (m testModule) Register(registrar agentslot.Registrar) error {
	for _, contribution := range m.contributions {
		if err := registrar.Contribute(contribution); err != nil {
			return err
		}
	}
	return m.afterRegister
}

func TestOneSlotAcceptsAtMostOneContribution(t *testing.T) {
	loop := agentslot.One[string]("example.loop")
	builder := agentslot.NewBuilder()

	if err := builder.Install(testModule{
		id:            "loop.react",
		contributions: []agentslot.Contribution{agentslot.Set(loop, "react")},
	}); err != nil {
		t.Fatalf("install first loop: %v", err)
	}

	err := builder.Install(testModule{
		id:            "loop.plan-act",
		contributions: []agentslot.Contribution{agentslot.Set(loop, "plan-act")},
	})
	if !errors.Is(err, agentslot.ErrSlotOccupied) {
		t.Fatalf("install second loop error = %v, want ErrSlotOccupied", err)
	}

	assembly, err := builder.Build(agentslot.RequireOne(loop))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got, ok := agentslot.Get(assembly, loop)
	if !ok || got != "react" {
		t.Fatalf("Get() = %q, %v; want react, true", got, ok)
	}
}

func TestManySlotAcceptsDistinctKeysAndRejectsDuplicates(t *testing.T) {
	tools := agentslot.Many[string]("tool")
	builder := agentslot.NewBuilder()

	for _, module := range []testModule{
		{id: "tool.shell", contributions: []agentslot.Contribution{agentslot.Add(tools, "shell", "shell-tool")}},
		{id: "tool.files", contributions: []agentslot.Contribution{agentslot.Add(tools, "files", "files-tool")}},
	} {
		if err := builder.Install(module); err != nil {
			t.Fatalf("install %s: %v", module.id, err)
		}
	}

	err := builder.Install(testModule{
		id:            "tool.shell.other",
		contributions: []agentslot.Contribution{agentslot.Add(tools, "shell", "other-shell")},
	})
	if !errors.Is(err, agentslot.ErrDuplicateKey) {
		t.Fatalf("install duplicate tool error = %v, want ErrDuplicateKey", err)
	}

	assembly, err := builder.Build(agentslot.RequireMany(tools, 2))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	want := []agentslot.Named[string]{
		{Key: "shell", Value: "shell-tool"},
		{Key: "files", Value: "files-tool"},
	}
	if got := agentslot.All(assembly, tools); !reflect.DeepEqual(got, want) {
		t.Fatalf("All() = %#v, want %#v", got, want)
	}
	if got, ok := agentslot.Lookup(assembly, tools, "files"); !ok || got != "files-tool" {
		t.Fatalf("Lookup(files) = %q, %v; want files-tool, true", got, ok)
	}
}

func TestChainSlotPreservesContributionOrder(t *testing.T) {
	hooks := agentslot.Chain[string]("agent.hook")
	builder := agentslot.NewBuilder()

	for _, module := range []testModule{
		{id: "hook.audit", contributions: []agentslot.Contribution{agentslot.Append(hooks, "audit")}},
		{id: "hook.metrics", contributions: []agentslot.Contribution{agentslot.Append(hooks, "metrics")}},
		{id: "hook.guard", contributions: []agentslot.Contribution{agentslot.Append(hooks, "guard")}},
	} {
		if err := builder.Install(module); err != nil {
			t.Fatalf("install %s: %v", module.id, err)
		}
	}

	assembly, err := builder.Build(agentslot.RequireChain(hooks, 3))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got, want := agentslot.Ordered(assembly, hooks), []string{"audit", "metrics", "guard"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Ordered() = %#v, want %#v", got, want)
	}
}

func TestRequirementsFailWithoutFreezingBuilder(t *testing.T) {
	modelProviders := agentslot.Many[string]("model.provider")
	builder := agentslot.NewBuilder()

	_, err := builder.Build(agentslot.RequireMany(modelProviders, 1))
	if !errors.Is(err, agentslot.ErrRequirementUnsatisfied) {
		t.Fatalf("empty build error = %v, want ErrRequirementUnsatisfied", err)
	}
	if err := builder.Install(testModule{
		id:            "model.deepseek",
		contributions: []agentslot.Contribution{agentslot.Add(modelProviders, "deepseek", "client")},
	}); err != nil {
		t.Fatalf("install after failed build: %v", err)
	}
	if _, err := builder.Build(agentslot.RequireMany(modelProviders, 1)); err != nil {
		t.Fatalf("second build: %v", err)
	}
}

func TestRegistrationIsTransactional(t *testing.T) {
	loop := agentslot.One[string]("example.loop")
	tools := agentslot.Many[string]("tool")
	builder := agentslot.NewBuilder()

	err := builder.Install(testModule{
		id: "broken.bundle",
		contributions: []agentslot.Contribution{
			agentslot.Set(loop, "temporary"),
			agentslot.Add(tools, "", "invalid"),
		},
	})
	if !errors.Is(err, agentslot.ErrInvalidKey) {
		t.Fatalf("broken install error = %v, want ErrInvalidKey", err)
	}

	if err := builder.Install(testModule{
		id:            "loop.real",
		contributions: []agentslot.Contribution{agentslot.Set(loop, "real")},
	}); err != nil {
		t.Fatalf("install after rolled-back registration: %v", err)
	}
	plan, err := builder.Build(agentslot.RequireOne(loop))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got, ok := agentslot.Get(plan, loop); !ok || got != "real" {
		t.Fatalf("Get() = %q, %v; want real, true", got, ok)
	}
}

func TestSlotIDCannotChangeKindOrValueType(t *testing.T) {
	builder := agentslot.NewBuilder()
	textOne := agentslot.One[string]("shared")
	textMany := agentslot.Many[string]("shared")

	if err := builder.Install(testModule{
		id:            "first",
		contributions: []agentslot.Contribution{agentslot.Set(textOne, "value")},
	}); err != nil {
		t.Fatalf("install first: %v", err)
	}
	if err := builder.Install(testModule{
		id:            "wrong-kind",
		contributions: []agentslot.Contribution{agentslot.Add(textMany, "value", "value")},
	}); !errors.Is(err, agentslot.ErrSlotConflict) {
		t.Fatalf("wrong-kind error = %v, want ErrSlotConflict", err)
	}

	numberOne := agentslot.One[int]("shared")
	if err := builder.Install(testModule{
		id:            "wrong-type",
		contributions: []agentslot.Contribution{agentslot.Set(numberOne, 1)},
	}); !errors.Is(err, agentslot.ErrSlotConflict) {
		t.Fatalf("wrong-type error = %v, want ErrSlotConflict", err)
	}
}

func TestSuccessfulBuildFreezesBuilder(t *testing.T) {
	builder := agentslot.NewBuilder()
	if _, err := builder.Build(); err != nil {
		t.Fatalf("build: %v", err)
	}
	err := builder.Install(testModule{id: "late"})
	if !errors.Is(err, agentslot.ErrBuilderFrozen) {
		t.Fatalf("late install error = %v, want ErrBuilderFrozen", err)
	}
}
