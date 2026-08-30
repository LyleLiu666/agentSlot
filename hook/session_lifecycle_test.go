package hook_test

import (
	"testing"

	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/model"
)

func TestSessionLifecycleContractKeepsPhaseScopeAndContextFinite(t *testing.T) {
	openScope := hook.LifecycleScope{Phases: []hook.LifecyclePhase{hook.LifecycleOpen}}
	if err := openScope.Validate(); err != nil || !openScope.Matches(hook.LifecycleOpen) || openScope.Matches(hook.LifecycleClose) {
		t.Fatalf("open scope = %#v err=%v", openScope, err)
	}
	for name, scope := range map[string]hook.LifecycleScope{
		"empty":     {},
		"duplicate": {Phases: []hook.LifecyclePhase{hook.LifecycleOpen, hook.LifecycleOpen}},
		"unknown":   {Phases: []hook.LifecyclePhase{"unknown"}},
	} {
		t.Run(name, func(t *testing.T) {
			if scope.Validate() == nil {
				t.Fatal("invalid lifecycle scope was accepted")
			}
		})
	}

	open := hook.SessionLifecycleView{
		InvocationID: "lifecycle-1", SessionID: "session-1", AgentID: "agent-1", WorkspaceID: "workspace-1",
		Revision: 2, Phase: hook.LifecycleOpen, OpenKind: hook.OpenCreate,
	}
	if err := open.Validate(); err != nil {
		t.Fatal(err)
	}
	context := []model.Input{{Message: &agent.Message{
		ID: "lifecycle-context-1", SessionID: open.SessionID, Role: agent.RoleUser,
		Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "project context"}},
	}}}
	if err := (hook.SessionLifecycleResult{Context: context}).Validate(open); err != nil {
		t.Fatal(err)
	}
	closeView := open
	closeView.Phase, closeView.OpenKind = hook.LifecycleClose, ""
	if err := closeView.Validate(); err != nil {
		t.Fatal(err)
	}
	invalidOpen := open
	invalidOpen.OpenKind = ""
	if invalidOpen.Validate() == nil {
		t.Fatal("open lifecycle view accepted an empty open kind")
	}
	invalidClose := closeView
	invalidClose.OpenKind = hook.OpenResume
	if invalidClose.Validate() == nil {
		t.Fatal("close lifecycle view accepted an open kind")
	}
	if err := (hook.SessionLifecycleResult{Context: context}).Validate(closeView); err == nil {
		t.Fatal("close lifecycle result accepted context")
	}
	for _, kind := range []hook.OpenKind{hook.OpenCreate, hook.OpenResume, hook.OpenFork, hook.OpenSummary} {
		candidate := open
		candidate.OpenKind = kind
		if err := candidate.Validate(); err != nil {
			t.Fatalf("open kind %q: %v", kind, err)
		}
	}
}
