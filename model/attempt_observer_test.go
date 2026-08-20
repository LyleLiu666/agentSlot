package model_test

import (
	"testing"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
)

func TestAttemptObserverFactsRequireCompleteStableIdentity(t *testing.T) {
	identity := model.AttemptIdentity{
		SessionID: "session-1", RunID: "run-1", StepID: "step-1", AttemptID: "attempt-1",
		ConfigRevision: 1, Config: model.Config{ProviderKey: "provider", ModelID: "model", Reasoning: model.ReasoningDefault},
	}
	if err := (model.AttemptStarted{Identity: identity}).Validate(); err != nil {
		t.Fatal(err)
	}
	finished := model.AttemptFinished{
		Identity: identity, Outcome: model.AttemptSucceeded,
		Usage: model.TokenUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	}
	if err := finished.Validate(); err != nil {
		t.Fatal(err)
	}
	identity.AttemptID = agent.AttemptID("")
	if err := (model.AttemptStarted{Identity: identity}).Validate(); err == nil {
		t.Fatal("invalid identity was accepted")
	}
}
