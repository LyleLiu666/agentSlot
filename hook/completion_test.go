package hook_test

import (
	"encoding/json"
	"testing"

	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/model"
)

func TestCompletionViewAndResultKeepCompletionAuthorityFinite(t *testing.T) {
	view := hook.CompletionView{
		InvocationID: "completion-1", SessionID: "session-1", AgentID: "agent-1", WorkspaceID: "workspace-1",
		Revision: 7, RunID: "run-1", StepID: "step-1", NextStepID: "step-2",
		LastAssistantMessage: agent.Message{
			ID: "assistant-1", SessionID: "session-1", RunID: "run-1", StepID: "step-1", Role: agent.RoleAssistant,
			Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "done"}},
		},
		Budget: model.TokenBudget{MaxTokens: 100, UsedTokens: 40}, FollowOns: 2,
		GoalCandidate: &hook.CompletionGoalCandidate{GoalID: "goal-1", Version: 3},
	}
	if err := view.Validate(); err != nil {
		t.Fatal(err)
	}
	context := []model.Input{{Message: &agent.Message{
		ID: "completion-context-1", SessionID: view.SessionID, RunID: view.RunID, StepID: view.NextStepID,
		Role: agent.RoleUser, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "run the final check"}},
	}}}
	if err := (hook.CompletionGateResult{Decision: hook.CompletionContinue, Context: context}).Validate(view); err != nil {
		t.Fatal(err)
	}
	for name, result := range map[string]hook.CompletionGateResult{
		"complete with context":    {Decision: hook.CompletionComplete, Context: context},
		"continue without context": {Decision: hook.CompletionContinue},
		"unknown decision":         {Decision: "guess"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := result.Validate(view); err == nil {
				t.Fatal("invalid CompletionGate result was accepted")
			}
		})
	}
	view.LastAssistantMessage.ModelContinuation = &agent.ModelContinuation{ProviderKey: "private", ModelID: "model", State: json.RawMessage(`{"secret":"opaque"}`)}
	if err := view.Validate(); err == nil {
		t.Fatal("completion view exposed provider-owned continuation state")
	}
}
