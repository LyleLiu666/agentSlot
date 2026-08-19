package hook_test

import (
	"context"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/hook"
)

type testHook struct{}

func (testHook) BeforeRunComplete(context.Context, hook.RunCompleteView) (hook.FollowOnProposal, error) {
	return hook.FollowOnProposal{}, nil
}
func (testHook) AfterCommit(context.Context, hook.CommitView) error { return nil }

type module struct{}

func (module) ID() string { return "hook.contracts" }
func (module) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Append(hook.HookSlot, hook.AgentHook(testHook{})))
}

func TestHookIsAnOrderedOptionalChain(t *testing.T) {
	builder := agentslot.NewBuilder()
	if err := builder.Install(module{}); err != nil {
		t.Fatalf("install: %v", err)
	}
	assembly, err := builder.Build(agentslot.RequireChain(hook.HookSlot, 1))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := agentslot.Ordered(assembly, hook.HookSlot); len(got) != 1 {
		t.Fatalf("hook count = %d, want 1", len(got))
	}
}

func TestHookProposalContainsOnlyAdditionalMessages(t *testing.T) {
	proposal := hook.FollowOnProposal{Messages: []agent.MessageInput{{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "continue"}}}}}
	if len(proposal.Messages) != 1 || !proposal.Messages[0].Valid() {
		t.Fatalf("proposal = %#v", proposal)
	}
}

func TestMissingRequiredHookChainFailsBuild(t *testing.T) {
	_, err := agentslot.NewBuilder().Build(agentslot.RequireChain(hook.HookSlot, 1))
	if err == nil {
		t.Fatal("missing required agent.hook chain was accepted")
	}
}
