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
	if got, want := len(catalog.Components), 41; got != want {
		t.Fatalf("component count = %d, want %d", got, want)
	}

	wantIDs := []string{
		"agent.hook", "agent.loop", "agent.provider", "approval.service", "artifact.store",
		"audit.sink", "authorization.provider", "billing.ledger", "checkpoint.store",
		"context.compactor", "context.source", "credential.resolver", "execution.environment",
		"gateway.channel", "goal.evaluator", "goal.store", "health.contributor",
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
	if counts.Mapped != 10 || counts.Contracted != 30 || counts.Conformant != 1 || counts.Proven != 0 || counts.Assembled != 0 {
		t.Fatalf("maturity counts = %#v", counts)
	}
	if counts.AtLeastContracted() != 31 {
		t.Fatalf("at-least-contracted count = %d, want 31", counts.AtLeastContracted())
	}
	if component, ok := catalog.Lookup("session.store"); !ok || component.Maturity != MaturityConformant {
		t.Fatalf("session.store = %#v, %v", component, ok)
	}
}

func TestStandardCatalogReturnsDetachedValues(t *testing.T) {
	first := Standard()
	first.Components[0].ID = "changed"
	first.Components[0].Profiles[0].Name = "changed"
	first.Components[0].Evidence.IndependentImplementations = append(first.Components[0].Evidence.IndependentImplementations, "changed")

	second := Standard()
	if second.Components[0].ID == "changed" || second.Components[0].Profiles[0].Name == "changed" || len(second.Components[0].Evidence.IndependentImplementations) != 0 {
		t.Fatalf("Standard returned shared mutable state: %#v", second.Components[0])
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
