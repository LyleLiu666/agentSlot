package componentcatalog

import (
	"slices"
	"testing"
)

func TestStandardCatalogPreservesApprovedInventory(t *testing.T) {
	catalog := Standard()
	if err := catalog.Validate(); err != nil {
		t.Fatalf("Standard().Validate() error = %v", err)
	}
	if got, want := len(catalog.Components), 42; got != want {
		t.Fatalf("component count = %d, want %d", got, want)
	}

	wantIDs := []string{
		"agent.hook", "agent.loop", "agent.provider", "approval.service", "artifact.store",
		"audit.sink", "authorization.provider", "billing.ledger", "checkpoint.store",
		"context.compactor", "context.source", "credential.resolver", "execution.environment",
		"gateway.channel", "goal.evaluator", "goal.store", "health.contributor", "hook.input_gate",
		"interaction.command", "job.store", "mailbox", "memory.store", "metric.sink",
		"model.attempt.observer", "model.catalog", "model.executor", "model.middleware",
		"model.provider", "model.selector", "model.token-counter", "policy.guard",
		"price.resolver", "quota.guard", "session.commit.observer", "session.store", "skill",
		"tool", "tool.middleware", "trace.sink", "usage.recorder", "workflow.scheduler",
		"workspace.manager",
	}
	gotIDs := make([]string, 0, len(catalog.Components))
	for _, component := range catalog.Components {
		gotIDs = append(gotIDs, component.ID)
		if len(component.Evidence.KnownGaps) == 0 {
			t.Errorf("component %q does not record its missing evidence", component.ID)
		}
	}
	slices.Sort(gotIDs)
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("component IDs = %q, want %q", gotIDs, wantIDs)
	}

	counts := catalog.Counts()
	if counts.Mapped != 9 || counts.Contracted != 32 || counts.Conformant != 1 || counts.Proven != 0 || counts.Assembled != 0 {
		t.Fatalf("maturity counts = %#v", counts)
	}
	if counts.AtLeastContracted() != 33 {
		t.Fatalf("at-least-contracted count = %d, want 33", counts.AtLeastContracted())
	}
	if component, ok := catalog.Lookup("session.store"); !ok || component.Maturity != MaturityContracted || component.Evidence.ConformanceSuite != "session.store/v1" || len(component.Evidence.KnownGaps) == 0 {
		t.Fatalf("session.store = %#v, %v", component, ok)
	}
	if component, ok := catalog.Lookup("model.executor"); !ok || component.Maturity != MaturityConformant || component.Evidence.ConformanceSuite != "model.executor/v1" {
		t.Fatalf("model.executor = %#v, %v", component, ok)
	}
	if component, ok := catalog.Lookup("credential.resolver"); !ok || component.Maturity != MaturityContracted || !component.Contract.Available || component.Contract.Package != module+"/credential" {
		t.Fatalf("credential.resolver = %#v, %v", component, ok)
	}
	if len(catalog.Implementations) != 13 || len(catalog.Presets) != 2 {
		t.Fatalf("scaffold inventory = %d implementations / %d presets", len(catalog.Implementations), len(catalog.Presets))
	}
	for _, presetID := range []string{"local-coding", "minimal-chat"} {
		preset, ok := catalog.LookupPreset(presetID)
		if !ok || len(preset.Implementations) == 0 {
			t.Fatalf("preset %q = %#v, %v", presetID, preset, ok)
		}
	}
}

func TestStandardCatalogReturnsDetachedValues(t *testing.T) {
	first := Standard()
	first.Components[0].ID = "changed"
	first.Components[0].Profiles[0].Name = "changed"
	first.Components[0].Evidence.IndependentImplementations = append(first.Components[0].Evidence.IndependentImplementations, "changed")
	first.Implementations[0].Dependencies = append(first.Implementations[0].Dependencies, "changed")
	first.Implementations[0].Configuration = append(first.Implementations[0].Configuration, ConfigurationField{Key: "changed", Description: "changed"})
	first.Implementations[0].ToolKeys = append(first.Implementations[0].ToolKeys, "changed")
	first.Presets[0].Implementations[0] = "changed"
	first.Presets[0].ToolKeys[0] = "changed"

	second := Standard()
	if second.Components[0].ID == "changed" || second.Components[0].Profiles[0].Name == "changed" || len(second.Components[0].Evidence.IndependentImplementations) != 0 ||
		len(second.Implementations[0].Dependencies) != 0 || len(second.Implementations[0].Configuration) != 0 || len(second.Implementations[0].ToolKeys) != 0 ||
		second.Presets[0].Implementations[0] == "changed" || second.Presets[0].ToolKeys[0] == "changed" {
		t.Fatalf("Standard returned shared mutable state: %#v / %#v / %#v", second.Components[0], second.Implementations[0], second.Presets[0])
	}
}

func TestImplementationMetadataCoversConfigurationAndToolExposure(t *testing.T) {
	catalog := Standard()
	provider, ok := catalog.LookupImplementation("model.openaicompat.executor")
	if !ok || len(provider.Configuration) != 4 || provider.Configuration[3].Key != "credential-ref" {
		t.Fatalf("provider configuration metadata = %#v, %v", provider.Configuration, ok)
	}
	files, ok := catalog.LookupImplementation("tool.files")
	if !ok || !slices.Equal(files.ToolKeys, []string{"file_read", "file_write", "file_edit"}) ||
		!slices.Contains(files.Dependencies, "policy.guard") {
		t.Fatalf("file tool scaffold metadata = %#v, %v", files, ok)
	}
}

func TestCatalogRejectsUnavailableOrIncompletePresetSelections(t *testing.T) {
	catalog := Standard()
	catalog.Presets[0].Implementations = slices.DeleteFunc(catalog.Presets[0].Implementations, func(id string) bool {
		return id == "workspace.local"
	})
	if err := catalog.Validate(); err == nil {
		t.Fatal("Validate accepted a preset missing an implementation dependency")
	}

	catalog = Standard()
	catalog.Implementations[0].Available = false
	if err := catalog.Validate(); err == nil {
		t.Fatal("Validate accepted a preset selecting an unavailable implementation")
	}

	catalog = Standard()
	catalog.Implementations[0].UnavailableReason = "contradicts available state"
	if err := catalog.Validate(); err == nil {
		t.Fatal("Validate accepted an unavailable reason on an available implementation")
	}

	catalog = Standard()
	catalog.Presets[0].ToolKeys = append(catalog.Presets[0].ToolKeys, "uninstalled_tool")
	if err := catalog.Validate(); err == nil {
		t.Fatal("Validate accepted a Tool key not supplied by the selected implementations")
	}
}

func TestCatalogRejectsInvalidStandardFacts(t *testing.T) {
	base := Standard().Components[0]
	tests := []struct {
		name      string
		component Component
	}{
		{name: "duplicate ID", component: base},
		{name: "invalid kind", component: func() Component { value := base; value.ID = "invalid.kind"; value.Kind = "bag"; return value }()},
		{name: "missing localized responsibility", component: func() Component {
			value := base
			value.ID = "missing.text"
			value.Text.Chinese.Responsibility = ""
			return value
		}()},
		{name: "conformant without suite", component: func() Component {
			value := base
			value.ID = "missing.suite"
			value.Maturity = MaturityConformant
			value.Evidence.ConformanceSuite = ""
			return value
		}()},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			catalog := Catalog{StandardVersion: StandardVersion, Components: []Component{base, tc.component}}
			if err := catalog.Validate(); err == nil {
				t.Fatal("Validate accepted invalid catalog")
			}
		})
	}
}
