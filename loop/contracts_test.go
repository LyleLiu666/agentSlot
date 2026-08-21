package loop_test

import (
	"context"
	"errors"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/loop"
)

func TestAgentLoopContractUsesStableSlotAndFiniteOutcomes(t *testing.T) {
	if loop.AgentLoopSlot.ID() != "agent.loop" {
		t.Fatalf("slot ID = %q", loop.AgentLoopSlot.ID())
	}
	for _, outcome := range []loop.Outcome{
		loop.OutcomeContinue,
		loop.OutcomeCompleted,
		loop.OutcomeFailed,
		loop.OutcomeCanceled,
		loop.OutcomeBudgetExceeded,
	} {
		if !outcome.Valid() {
			t.Fatalf("outcome %q is invalid", outcome)
		}
	}
	if loop.Outcome("invented").Valid() {
		t.Fatal("invented outcome is valid")
	}
}

func TestNewModuleRejectsInvalidIdentityAndTypedNilLoop(t *testing.T) {
	var typedNil *testLoop
	for _, test := range []struct {
		id        string
		component loop.AgentLoop
	}{
		{id: ""},
		{id: " invalid ", component: testLoop{}},
		{id: "typed-nil", component: typedNil},
	} {
		if _, err := loop.NewModule(test.id, test.component); err == nil {
			t.Fatalf("NewModule(%q, %T) accepted invalid input", test.id, test.component)
		}
	}
}

type testLoop struct{}

func (testLoop) Run(context.Context, loop.Run) (loop.Outcome, error) {
	return loop.OutcomeCompleted, nil
}

func TestGenericApplicationDoesNotSelectAnAgentLoop(t *testing.T) {
	application := agentslot.NewApplication("missing-loop", nil, agentslot.RequireOne(loop.AgentLoopSlot))
	if _, err := application.Build(); !errors.Is(err, agentslot.ErrRequirementUnsatisfied) {
		t.Fatalf("Build error = %v, want missing AgentLoop", err)
	}
}
