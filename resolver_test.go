package agentslot_test

import (
	"errors"
	"reflect"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
)

func TestConstructorsResolveOnlyDeclaredDependenciesDuringBuild(t *testing.T) {
	model := agentslot.Many[string]("model")
	tools := agentslot.Many[string]("tool")
	hooks := agentslot.Chain[string]("hook")
	store := agentslot.One[string]("store")
	driver := agentslot.One[string]("driver")

	builder := agentslot.NewBuilder()
	consumer := &dependentModule{
		testModule: testModule{
			id: "driver",
			contributions: []agentslot.Contribution{
				agentslot.SetWith(driver, func(resolver agentslot.Resolver) (string, error) {
					selected, err := agentslot.ResolveKey(resolver, model, "selected")
					if err != nil {
						return "", err
					}
					resolvedTools, err := agentslot.ResolveMany(resolver, tools)
					if err != nil {
						return "", err
					}
					resolvedHooks, err := agentslot.ResolveChain(resolver, hooks)
					if err != nil {
						return "", err
					}
					resolvedStore, err := agentslot.ResolveOne(resolver, store)
					if err != nil {
						return "", err
					}
					return selected + ":" + resolvedTools[0].Value + ":" + resolvedHooks[0] + ":" + resolvedStore, nil
				}),
			},
		},
		requirements: []agentslot.Requirement{
			agentslot.RequireKey(model, "selected"),
			agentslot.RequireMany(tools, 1),
			agentslot.RequireChain(hooks, 1),
			agentslot.RequireOne(store),
		},
	}
	for _, module := range []agentslot.Module{
		consumer,
		testModule{id: "model", contributions: []agentslot.Contribution{agentslot.AddWith(model, "selected", func(agentslot.Resolver) (string, error) { return "model", nil })}},
		testModule{id: "tool", contributions: []agentslot.Contribution{agentslot.AddWith(tools, "echo", func(agentslot.Resolver) (string, error) { return "echo", nil })}},
		testModule{id: "hook", contributions: []agentslot.Contribution{agentslot.AppendWith(hooks, func(agentslot.Resolver) (string, error) { return "audit", nil })}},
		testModule{id: "store", contributions: []agentslot.Contribution{agentslot.Set(store, "memory")}},
	} {
		if err := builder.Install(module); err != nil {
			t.Fatalf("install %s: %v", module.ID(), err)
		}
	}

	plan, err := builder.Build(agentslot.RequireOne(driver))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got, ok := agentslot.Get(plan, driver)
	if !ok || got != "model:echo:audit:memory" {
		t.Fatalf("driver = %q, %v", got, ok)
	}
}

func TestConstructorCannotResolveUndeclaredSlot(t *testing.T) {
	model := agentslot.One[string]("model")
	driver := agentslot.One[string]("driver")
	builder := agentslot.NewBuilder()
	for _, module := range []agentslot.Module{
		testModule{id: "model", contributions: []agentslot.Contribution{agentslot.Set(model, "model")}},
		testModule{id: "driver", contributions: []agentslot.Contribution{
			agentslot.SetWith(driver, func(resolver agentslot.Resolver) (string, error) {
				return agentslot.ResolveOne(resolver, model)
			}),
		}},
	} {
		if err := builder.Install(module); err != nil {
			t.Fatalf("install %s: %v", module.ID(), err)
		}
	}

	_, err := builder.Build()
	if !errors.Is(err, agentslot.ErrDependencyUndeclared) {
		t.Fatalf("Build() error = %v, want ErrDependencyUndeclared", err)
	}
}

func TestResolverClosesAfterConstructorReturns(t *testing.T) {
	value := agentslot.One[string]("value")
	built := agentslot.One[string]("built")
	var retained agentslot.Resolver
	builder := agentslot.NewBuilder()
	for _, module := range []agentslot.Module{
		testModule{id: "value", contributions: []agentslot.Contribution{agentslot.Set(value, "value")}},
		&dependentModule{
			testModule: testModule{id: "built", contributions: []agentslot.Contribution{
				agentslot.SetWith(built, func(resolver agentslot.Resolver) (string, error) {
					retained = resolver
					return agentslot.ResolveOne(resolver, value)
				}),
			}},
			requirements: []agentslot.Requirement{agentslot.RequireOne(value)},
		},
	} {
		if err := builder.Install(module); err != nil {
			t.Fatalf("install %s: %v", module.ID(), err)
		}
	}
	if _, err := builder.Build(); err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := agentslot.ResolveOne(retained, value); !errors.Is(err, agentslot.ErrResolverClosed) {
		t.Fatalf("ResolveOne() after Build error = %v, want ErrResolverClosed", err)
	}
}

func TestConstructorFailureDoesNotFreezeBuilderOrPublishValues(t *testing.T) {
	dependency := agentslot.One[string]("dependency")
	built := agentslot.One[string]("built")
	wantErr := errors.New("construction failed")
	attempts := 0
	builder := agentslot.NewBuilder()
	if err := builder.Install(&dependentModule{
		testModule: testModule{id: "built", contributions: []agentslot.Contribution{
			agentslot.SetWith(built, func(resolver agentslot.Resolver) (string, error) {
				attempts++
				if _, err := agentslot.ResolveOne(resolver, dependency); err != nil {
					return "", err
				}
				if attempts == 1 {
					return "", wantErr
				}
				return "ready", nil
			}),
		}},
		requirements: []agentslot.Requirement{agentslot.RequireOne(dependency)},
	}); err != nil {
		t.Fatalf("install built: %v", err)
	}
	if err := builder.Install(testModule{id: "dependency", contributions: []agentslot.Contribution{agentslot.Set(dependency, "value")}}); err != nil {
		t.Fatalf("install dependency: %v", err)
	}
	if _, err := builder.Build(); !errors.Is(err, wantErr) {
		t.Fatalf("first Build() error = %v, want construction failure", err)
	}
	if err := builder.Install(testModule{id: "repair.marker"}); err != nil {
		t.Fatalf("install after failed build: %v", err)
	}
	plan, err := builder.Build()
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	if got, ok := agentslot.Get(plan, built); !ok || got != "ready" {
		t.Fatalf("built = %q, %v", got, ok)
	}
	if attempts != 2 {
		t.Fatalf("constructor attempts = %d, want 2", attempts)
	}
}

func TestConstructorRejectsNilComponent(t *testing.T) {
	type component interface{ Value() string }
	slot := agentslot.One[component]("component")
	builder := agentslot.NewBuilder()
	if err := builder.Install(testModule{id: "component", contributions: []agentslot.Contribution{
		agentslot.SetWith(slot, func(agentslot.Resolver) (component, error) { return nil, nil }),
	}}); err != nil {
		t.Fatalf("install: %v", err)
	}
	_, err := builder.Build()
	if !errors.Is(err, agentslot.ErrInvalidContribution) {
		t.Fatalf("Build() error = %v, want ErrInvalidContribution", err)
	}
}

func TestResolvedManyIsACopy(t *testing.T) {
	values := agentslot.Many[string]("values")
	built := agentslot.One[[]agentslot.Named[string]]("built")
	builder := agentslot.NewBuilder()
	consumer := &dependentModule{
		testModule: testModule{id: "consumer", contributions: []agentslot.Contribution{
			agentslot.SetWith(built, func(resolver agentslot.Resolver) ([]agentslot.Named[string], error) {
				return agentslot.ResolveMany(resolver, values)
			}),
		}},
		requirements: []agentslot.Requirement{agentslot.RequireMany(values, 1)},
	}
	for _, module := range []agentslot.Module{
		consumer,
		testModule{id: "values", contributions: []agentslot.Contribution{agentslot.Add(values, "one", "value")}},
	} {
		if err := builder.Install(module); err != nil {
			t.Fatalf("install %s: %v", module.ID(), err)
		}
	}
	plan, err := builder.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got, _ := agentslot.Get(plan, built)
	got[0].Value = "mutated"
	if resolved := agentslot.All(plan, values); !reflect.DeepEqual(resolved, []agentslot.Named[string]{{Key: "one", Value: "value"}}) {
		t.Fatalf("values = %#v", resolved)
	}
}

func TestOptionalDependenciesResolveWithoutRequiringProviders(t *testing.T) {
	optionalOne := agentslot.One[string]("optional.one")
	optionalMany := agentslot.Many[string]("optional.many")
	optionalChain := agentslot.Chain[string]("optional.chain")
	built := agentslot.One[string]("built.optional")
	builder := agentslot.NewBuilder()
	consumer := &dependentModule{
		testModule: testModule{id: "consumer", contributions: []agentslot.Contribution{
			agentslot.SetWith(built, func(resolver agentslot.Resolver) (string, error) {
				_, present, err := agentslot.ResolveOptionalOne(resolver, optionalOne)
				if err != nil {
					return "", err
				}
				many, err := agentslot.ResolveMany(resolver, optionalMany)
				if err != nil {
					return "", err
				}
				chain, err := agentslot.ResolveChain(resolver, optionalChain)
				if err != nil {
					return "", err
				}
				if present || len(many) != 0 || len(chain) != 0 {
					return "unexpected", nil
				}
				return "empty", nil
			}),
		}},
		requirements: []agentslot.Requirement{
			agentslot.OptionalOne(optionalOne),
			agentslot.OptionalMany(optionalMany),
			agentslot.OptionalChain(optionalChain),
		},
	}
	if err := builder.Install(consumer); err != nil {
		t.Fatalf("install: %v", err)
	}
	assembly, err := builder.Build(agentslot.RequireOne(built))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got, ok := agentslot.Get(assembly, built); !ok || got != "empty" {
		t.Fatalf("built = %q, %v; want empty, true", got, ok)
	}
	wantRequirements := []agentslot.RequirementDescription{
		{Slot: "optional.chain", Kind: "chain", Optional: true},
		{Slot: "optional.many", Kind: "many", Optional: true},
		{Slot: "optional.one", Kind: "one", Optional: true},
	}
	if got := assembly.Describe().Modules[0].Requires; !reflect.DeepEqual(got, wantRequirements) {
		t.Fatalf("optional requirements = %#v, want %#v", got, wantRequirements)
	}
}

func TestResolveOneOnAbsentOptionalDependencyReturnsErrorInsteadOfPanicking(t *testing.T) {
	optional := agentslot.One[string]("optional.one")
	built := agentslot.One[string]("built.optional")
	builder := agentslot.NewBuilder()
	if err := builder.Install(&dependentModule{
		testModule: testModule{id: "consumer", contributions: []agentslot.Contribution{
			agentslot.SetWith(built, func(resolver agentslot.Resolver) (string, error) {
				return agentslot.ResolveOne(resolver, optional)
			}),
		}},
		requirements: []agentslot.Requirement{agentslot.OptionalOne(optional)},
	}); err != nil {
		t.Fatalf("install: %v", err)
	}
	_, err := builder.Build()
	if !errors.Is(err, agentslot.ErrRequirementUnsatisfied) {
		t.Fatalf("Build error = %v, want ErrRequirementUnsatisfied", err)
	}
}

func TestInstalledOptionalProviderPrecedesConsumer(t *testing.T) {
	dependency := agentslot.One[string]("optional.dependency")
	built := agentslot.One[string]("optional.consumer")
	builder := agentslot.NewBuilder()
	consumer := &dependentModule{
		testModule: testModule{id: "consumer", contributions: []agentslot.Contribution{
			agentslot.SetWith(built, func(resolver agentslot.Resolver) (string, error) {
				value, present, err := agentslot.ResolveOptionalOne(resolver, dependency)
				if err != nil || !present {
					return "", err
				}
				return value, nil
			}),
		}},
		requirements: []agentslot.Requirement{agentslot.OptionalOne(dependency)},
	}
	for _, module := range []agentslot.Module{
		consumer,
		testModule{id: "provider", contributions: []agentslot.Contribution{agentslot.Set(dependency, "ready")}},
	} {
		if err := builder.Install(module); err != nil {
			t.Fatalf("install %s: %v", module.ID(), err)
		}
	}
	assembly, err := builder.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got, ok := agentslot.Get(assembly, built); !ok || got != "ready" {
		t.Fatalf("built = %q, %v; want ready, true", got, ok)
	}
	modules := assembly.Describe().Modules
	if len(modules) != 2 || modules[0].ID != "provider" || modules[1].ID != "consumer" || !modules[1].Requires[0].Optional {
		t.Fatalf("module order/description = %#v", modules)
	}
}
