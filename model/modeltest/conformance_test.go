package modeltest_test

import (
	"context"
	"errors"
	"testing"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/model/modeltest"
)

func TestRunChecksFakeExecutorSuccessRetryAndFailure(t *testing.T) {
	request := model.ModelRequest{
		Config: model.Config{ProviderKey: "fake", ModelID: "fake", Reasoning: model.ReasoningDefault},
		Inputs: []model.Input{{Message: &agent.Message{
			ID: "message-1", SessionID: "session-1", Role: agent.RoleUser,
			Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "work"}},
		}}},
	}
	for _, test := range []struct {
		name         string
		execution    model.FakeExecution
		wantEvents   int
		wantAttempts int
	}{
		{name: "success", execution: model.FakeExecution{Events: []model.ModelEvent{{
			Kind: model.EventComplete, Output: &model.Completion{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "done"}}},
		}}}, wantEvents: 1, wantAttempts: 1},
		{name: "retry", execution: model.FakeExecution{Events: []model.ModelEvent{
			{Kind: model.EventDelta, AttemptID: "attempt-1", Text: "partial"},
			{Kind: model.EventReset, AttemptID: "attempt-1"},
			{Kind: model.EventComplete, AttemptID: "attempt-2", Output: &model.Completion{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "done"}}}},
		}}, wantEvents: 3, wantAttempts: 2},
		{name: "failure", execution: model.FakeExecution{Events: []model.ModelEvent{{Kind: model.EventFailed, Err: errors.New("scripted")}}}, wantEvents: 1, wantAttempts: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			report := modeltest.Run(t, context.Background(), model.NewFakeModelExecutor(test.execution), request, model.TokenBudget{})
			if len(report.Events) != test.wantEvents || len(report.Starts) != test.wantAttempts || len(report.Finishes) != test.wantAttempts {
				t.Fatalf("conformance report = %#v", report)
			}
			for index := range report.Starts {
				if report.Starts[index].AttemptID != report.Finishes[index].AttemptID {
					t.Fatalf("Attempt %d pair = %#v / %#v", index, report.Starts[index], report.Finishes[index])
				}
			}
		})
	}
}
