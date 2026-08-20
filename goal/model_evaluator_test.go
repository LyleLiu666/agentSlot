package goal_test

import (
	"context"
	"testing"
	"time"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/goal"
	"github.com/LyleLiu666/agentSlot/model"
)

func TestModelEvaluatorUsesStrictStructuredDecisionWithoutTools(t *testing.T) {
	executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{{
		Kind: model.EventComplete, Output: &model.Completion{Parts: []agent.MessagePart{{
			Kind: agent.PartText, Text: `{"decision":"continue","reason":"progress_possible","next_instruction":"run the remaining tests"}`,
		}}},
	}}})
	evaluator := newGoalModelEvaluatorForTest(t, executor)
	result, err := evaluator.Evaluate(context.Background(), goalEvaluationRequest(), &goalRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != goal.DecisionContinue || result.NextInstruction.Parts[0].Text != "run the remaining tests" {
		t.Fatalf("evaluation = %#v", result)
	}
	requests := executor.Requests()
	if len(requests) != 1 || len(requests[0].Tools) != 0 {
		t.Fatalf("goal model request = %#v", requests)
	}
}

func TestModelEvaluatorRejectsFreeTextAndUnknownJSONFields(t *testing.T) {
	for _, response := range []string{
		"probably done",
		`{"decision":"done","reason":"objective_met","next_instruction":"","confidence":1}`,
	} {
		executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{{
			Kind: model.EventComplete, Output: &model.Completion{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: response}}},
		}}})
		evaluator := newGoalModelEvaluatorForTest(t, executor)
		if _, err := evaluator.Evaluate(context.Background(), goalEvaluationRequest(), &goalRecorder{}); err == nil {
			t.Fatalf("response %q was accepted", response)
		}
	}
}

func newGoalModelEvaluatorForTest(t *testing.T, executor model.ModelExecutor) goal.Evaluator {
	t.Helper()
	evaluator, err := goal.NewModelEvaluator(executor)
	if err != nil {
		t.Fatal(err)
	}
	return evaluator
}

func goalEvaluationRequest() goal.EvaluationRequest {
	return goal.EvaluationRequest{
		Goal:  goal.Goal{ID: "goal-1", SessionID: "session-1", Objective: "finish", Status: goal.StatusActive, Version: 1, MaxFollowOns: 3, UpdatedAt: time.Now()},
		RunID: "run-1", StepID: "step-1", Revision: 1,
		ModelConfig: model.Config{ProviderKey: "fake", ModelID: "fake", Reasoning: model.ReasoningDefault},
	}
}

type goalRecorder struct{}

func (*goalRecorder) Started(context.Context, model.AttemptStart) error   { return nil }
func (*goalRecorder) Finished(context.Context, model.AttemptFinish) error { return nil }
func (*goalRecorder) Budget() model.TokenBudget                           { return model.TokenBudget{} }
