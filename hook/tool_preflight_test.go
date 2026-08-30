package hook_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/hook"
)

func TestToolPreflightContractFreezesStaticScopeAndFiniteDecision(t *testing.T) {
	scope := hook.ToolScope{ToolKeys: []string{"apply_patch", "shell"}}
	if err := scope.Validate(); err != nil || !scope.Matches("apply_patch") || scope.Matches("read_file") {
		t.Fatalf("exact scope = %#v err=%v", scope, err)
	}
	wildcard := hook.ToolScope{All: true}
	if err := wildcard.Validate(); err != nil || !wildcard.Matches("anything") {
		t.Fatalf("wildcard scope = %#v err=%v", wildcard, err)
	}
	view := hook.ToolPreflightView{
		InvocationID: "preflight-1", SessionID: "session-1", AgentID: "agent-1", WorkspaceID: "workspace-1",
		Revision: 7, RunID: "run-1", StepID: "step-1", ToolCallID: "call-1", ToolKey: "apply_patch",
		Arguments: json.RawMessage(`{"path":"README.md"}`),
	}
	if err := view.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, result := range []hook.ToolPreflightResult{
		{Decision: hook.DecisionAllow},
		{Decision: hook.DecisionDeny, Reason: "project policy"},
		{Decision: hook.DecisionRequireApproval, Reason: "external side effect"},
	} {
		if err := result.Validate(); err != nil {
			t.Fatalf("result %#v: %v", result, err)
		}
	}
}

func TestToolPreflightContractRejectsDynamicOrAmbiguousAuthority(t *testing.T) {
	invalidScopes := []hook.ToolScope{
		{},
		{All: true, ToolKeys: []string{"shell"}},
		{ToolKeys: []string{"shell", "shell"}},
		{ToolKeys: []string{" bad"}},
		{ToolKeys: []string{strings.Repeat("x", hook.MaxExtensionKeyBytes+1)}},
	}
	for _, scope := range invalidScopes {
		if err := scope.Validate(); err == nil {
			t.Fatalf("invalid scope accepted: %#v", scope)
		}
	}
	invalidResults := []hook.ToolPreflightResult{
		{},
		{Decision: hook.DecisionDeny},
		{Decision: hook.DecisionRequireApproval},
		{Decision: hook.DecisionAccept},
	}
	for _, result := range invalidResults {
		if err := result.Validate(); err == nil {
			t.Fatalf("invalid result accepted: %#v", result)
		}
	}
	invalidView := hook.ToolPreflightView{
		InvocationID: "preflight-1", SessionID: "session-1", AgentID: "agent-1", WorkspaceID: "workspace-1",
		Revision: 7, RunID: "run-1", StepID: "step-1", ToolCallID: agent.ToolCallID("call-1"), ToolKey: "shell",
		Arguments: json.RawMessage(`{"value":1,"value":2}`),
	}
	if err := invalidView.Validate(); err == nil {
		t.Fatal("ToolPreflight view accepted duplicate JSON object members")
	}
}
