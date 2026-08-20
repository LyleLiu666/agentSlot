package goal_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/goal"
)

func TestMemoryStoreGoalDecisionIsCASProtectedAndIdempotent(t *testing.T) {
	store := goal.NewMemoryStore()
	created, err := store.Set(context.Background(), goal.SetRequest{SessionID: "session-1", Objective: "finish the migration", MaxFollowOns: 3})
	if err != nil {
		t.Fatal(err)
	}
	record := goal.DecisionRecord{
		ID: "decision-1", GoalID: created.ID, SessionID: created.SessionID, RunID: "run-1", StepID: "step-1",
		ExpectedVersion: created.Version, RecordedAt: time.Now(),
		Evaluation: goal.Evaluation{Decision: goal.DecisionContinue, Reason: goal.ReasonProgressPossible,
			NextInstruction: agent.MessageInput{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "continue with tests"}}}},
	}
	updated, err := store.RecordDecision(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	if updated.FollowOns != 1 || updated.Status != goal.StatusActive {
		t.Fatalf("updated goal = %#v", updated)
	}
	if replay, err := store.RecordDecision(context.Background(), record); err != nil || replay != updated {
		t.Fatalf("equal replay = %#v, %v", replay, err)
	}
	stale := record
	stale.ID = "decision-2"
	if _, err := store.RecordDecision(context.Background(), stale); !errors.Is(err, goal.ErrVersionConflict) {
		t.Fatalf("stale decision error = %v", err)
	}
	paused, err := store.ChangeStatus(context.Background(), goal.StateChangeRequest{
		SessionID: updated.SessionID, ExpectedVersion: updated.Version, Status: goal.StatusPaused,
	})
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := store.RecordDecision(context.Background(), record); err != nil || replay != updated || replay == paused {
		t.Fatalf("historical idempotent replay = %#v, want %#v: %v", replay, updated, err)
	}
}

func TestGoalStoreRejectsDecisionsOutsideActiveStateAndTerminalReopening(t *testing.T) {
	store := goal.NewMemoryStore()
	created, err := store.Set(context.Background(), goal.SetRequest{
		SessionID: "session-1", Objective: "finish", MaxFollowOns: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	paused, err := store.ChangeStatus(context.Background(), goal.StateChangeRequest{
		SessionID: created.SessionID, ExpectedVersion: created.Version, Status: goal.StatusPaused,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordDecision(context.Background(), goal.DecisionRecord{
		ID: "decision-paused", GoalID: paused.ID, SessionID: paused.SessionID, RunID: "run-1", StepID: "step-1",
		ExpectedVersion: paused.Version, RecordedAt: time.Now(),
		Evaluation: goal.Evaluation{Decision: goal.DecisionDone, Reason: goal.ReasonObjectiveMet},
	}); err == nil {
		t.Fatal("paused goal accepted an evaluator decision")
	}
	active, err := store.ChangeStatus(context.Background(), goal.StateChangeRequest{
		SessionID: paused.SessionID, ExpectedVersion: paused.Version, Status: goal.StatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := store.ChangeStatus(context.Background(), goal.StateChangeRequest{
		SessionID: active.SessionID, ExpectedVersion: active.Version, Status: goal.StatusCompleted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ChangeStatus(context.Background(), goal.StateChangeRequest{
		SessionID: completed.SessionID, ExpectedVersion: completed.Version, Status: goal.StatusActive,
	}); err == nil {
		t.Fatal("completed goal was reopened")
	}
}
