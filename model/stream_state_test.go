package model_test

import (
	"errors"
	"testing"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
)

func TestStreamStateAcceptsPortableRetrySequence(t *testing.T) {
	state := model.StreamState{}
	for _, event := range []model.ModelEvent{
		{Kind: model.EventDelta, AttemptID: "attempt-1", Text: "partial"},
		{Kind: model.EventReset, AttemptID: "attempt-1"},
		{Kind: model.EventDelta, AttemptID: "attempt-2", Text: "complete"},
		{Kind: model.EventComplete, AttemptID: "attempt-2", Output: &model.Completion{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "complete"}}}},
	} {
		if err := state.Accept(event); err != nil {
			t.Fatalf("Accept(%#v): %v", event, err)
		}
	}
	if !state.Terminal() {
		t.Fatal("complete stream did not become terminal")
	}
	if err := state.End(model.ErrStreamClosed); err != nil {
		t.Fatalf("terminal close rejected: %v", err)
	}
}

func TestStreamStateRejectsIncompleteAndContradictorySequences(t *testing.T) {
	complete := func(attempt string) model.ModelEvent {
		return model.ModelEvent{Kind: model.EventComplete, AttemptID: attempt, Output: &model.Completion{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "done"}}}}
	}
	for _, test := range []struct {
		name   string
		events []model.ModelEvent
		end    error
	}{
		{name: "missing terminal", events: []model.ModelEvent{{Kind: model.EventDelta, AttemptID: "attempt-1", Text: "partial"}}, end: model.ErrStreamClosed},
		{name: "attempt changes without reset", events: []model.ModelEvent{{Kind: model.EventDelta, AttemptID: "attempt-1", Text: "partial"}, complete("attempt-2")}},
		{name: "reset before output", events: []model.ModelEvent{{Kind: model.EventReset, AttemptID: "attempt-1"}}},
		{name: "partial failure without reset", events: []model.ModelEvent{{Kind: model.EventDelta, AttemptID: "attempt-1", Text: "partial"}, {Kind: model.EventFailed, AttemptID: "attempt-1", Err: errors.New("failed")}}},
		{name: "missing complete attempt", events: []model.ModelEvent{complete("")}},
		{name: "event after terminal", events: []model.ModelEvent{complete("attempt-1"), {Kind: model.EventDelta, AttemptID: "attempt-1", Text: "late"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := model.StreamState{}
			var got error
			for _, event := range test.events {
				if got = state.Accept(event); got != nil {
					break
				}
			}
			if got == nil && test.end != nil {
				got = state.End(test.end)
			}
			if got == nil {
				t.Fatal("invalid stream sequence was accepted")
			}
		})
	}
}

func TestStreamStateAllowsFailureBeforeAnyProviderAttempt(t *testing.T) {
	state := model.StreamState{}
	if err := state.Accept(model.ModelEvent{Kind: model.EventFailed, Err: model.ErrTokenBudgetExceeded}); err != nil {
		t.Fatal(err)
	}
	if !state.Terminal() {
		t.Fatal("failed stream did not become terminal")
	}
}
