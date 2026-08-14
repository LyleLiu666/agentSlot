package agentslot_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
)

func TestPlanDescriptionIsDeterministicAndOmitsComponentValues(t *testing.T) {
	loop := agentslot.One[string]("agent.loop")
	tools := agentslot.Many[string]("tool")
	hooks := agentslot.Chain[string]("agent.hook")
	builder := agentslot.NewBuilder()

	consumer := &dependentModule{
		testModule: testModule{id: "consumer"},
		requirements: []agentslot.Requirement{
			agentslot.RequireKey(tools, "shell"),
			agentslot.RequireOne(loop),
		},
	}
	bundle := testModule{
		id: "bundle",
		contributions: []agentslot.Contribution{
			agentslot.Set(loop, "SECRET-LOOP-VALUE"),
			agentslot.Add(tools, "shell", "SECRET-TOOL-VALUE"),
			agentslot.Append(hooks, "SECRET-HOOK-VALUE"),
		},
	}
	for _, module := range []agentslot.Module{consumer, bundle} {
		if err := builder.Install(module); err != nil {
			t.Fatalf("install %s: %v", module.ID(), err)
		}
	}

	plan, err := builder.Build(
		agentslot.RequireMany(tools, 1),
		agentslot.RequireOne(loop),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	want := agentslot.PlanDescription{
		Schema: agentslot.PlanDescriptionSchema,
		Modules: []agentslot.ModuleDescription{
			{ID: "bundle"},
			{ID: "consumer", Requires: []agentslot.RequirementDescription{
				{Slot: "agent.loop", Kind: "one", Minimum: 1},
				{Slot: "tool", Kind: "many", Key: "shell", Minimum: 1},
			}},
		},
		Slots: []agentslot.SlotDescription{
			{ID: "agent.hook", Kind: "chain", ValueType: "string", Contributions: []agentslot.ContributionDescription{{Module: "bundle"}}},
			{ID: "agent.loop", Kind: "one", ValueType: "string", Contributions: []agentslot.ContributionDescription{{Module: "bundle"}}},
			{ID: "tool", Kind: "many", ValueType: "string", Contributions: []agentslot.ContributionDescription{{Module: "bundle", Key: "shell"}}},
		},
		Profile: []agentslot.RequirementDescription{
			{Slot: "agent.loop", Kind: "one", Minimum: 1},
			{Slot: "tool", Kind: "many", Minimum: 1},
		},
	}
	if got := plan.Describe(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Describe() = %#v, want %#v", got, want)
	}

	encoded, err := json.Marshal(plan.Describe())
	if err != nil {
		t.Fatalf("marshal description: %v", err)
	}
	if strings.Contains(string(encoded), "SECRET-") {
		t.Fatalf("description leaked component values: %s", encoded)
	}
}
