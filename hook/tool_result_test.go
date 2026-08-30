package hook_test

import (
	"encoding/json"
	"testing"

	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/tool"
)

func TestToolResultScopeIsFiniteAndExcludesUnknownOutcomes(t *testing.T) {
	scope := hook.ToolResultScope{ToolKeys: []string{"shell"}, Statuses: []tool.ResultStatus{tool.ResultSucceeded, tool.ResultFailed}}
	if err := scope.Validate(); err != nil {
		t.Fatal(err)
	}
	if !scope.Matches("shell", tool.ResultSucceeded) || !scope.Matches("shell", tool.ResultFailed) ||
		scope.Matches("shell", tool.ResultUnknown) || scope.Matches("other", tool.ResultSucceeded) {
		t.Fatalf("scope match = %#v", scope)
	}
	for name, invalid := range map[string]hook.ToolResultScope{
		"unknown status": {All: true, Statuses: []tool.ResultStatus{tool.ResultUnknown}},
		"no status":      {All: true},
		"mixed wildcard": {All: true, ToolKeys: []string{"shell"}, Statuses: []tool.ResultStatus{tool.ResultSucceeded}},
	} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("%s scope was accepted", name)
		}
	}
}

func TestToolResultViewAndContextProposalAreDetachedAndBounded(t *testing.T) {
	view := hook.ToolResultView{
		InvocationID: "post-1", SessionID: "session-1", AgentID: "agent-1", WorkspaceID: "workspace-1", Revision: 9,
		RunID: "run-1", StepID: "step-1", NextStepID: "step-2", MessageID: "message-1", ToolCallID: "call-1",
		ToolKey: "shell", Arguments: json.RawMessage(`{"argv":["true"]}`),
		Result: tool.ToolResult{CallID: "call-1", Status: tool.ResultSucceeded, Output: json.RawMessage(`{"ok":true}`)},
	}
	if err := view.Validate(); err != nil {
		t.Fatal(err)
	}
	proposal := hook.ToolResultHookResult{Context: []model.Input{{Message: &agent.Message{
		ID: "context-1", SessionID: "session-1", Role: agent.RoleUser,
		Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "check passed"}},
	}}}}
	if err := proposal.Validate("session-1"); err != nil {
		t.Fatal(err)
	}
	view.Result.CallID = "other"
	if err := view.Validate(); err == nil {
		t.Fatal("view accepted a result for another ToolCall")
	}
}
