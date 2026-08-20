package policy_test

import (
	"context"
	"reflect"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/policy"
)

func TestPolicyGuardIsAnOrderedChainAndApprovalIsOne(t *testing.T) {
	builder := agentslot.NewBuilder()
	if err := builder.Install(policyModule{}); err != nil {
		t.Fatal(err)
	}
	assembly, err := builder.Build(
		agentslot.RequireChain(policy.GuardSlot, 2),
		agentslot.RequireOne(policy.ApprovalSlot),
	)
	if err != nil {
		t.Fatal(err)
	}
	guards := agentslot.Ordered(assembly, policy.GuardSlot)
	if len(guards) != 2 || reflect.ValueOf(guards[0]).Pointer() == reflect.ValueOf(guards[1]).Pointer() {
		t.Fatalf("guards = %#v", guards)
	}
	if _, ok := agentslot.Get(assembly, policy.ApprovalSlot); !ok {
		t.Fatal("approval service missing")
	}
}

func TestRuleGuardReturnsConfiguredDecisionWithoutMutatingAction(t *testing.T) {
	guard, err := policy.NewToolRuleGuard(
		policy.Decision{Effect: policy.Allow},
		policy.ToolRule{ToolKey: "bash", Decision: policy.Decision{Effect: policy.RequireApproval, Reason: "shell changes the workspace"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	action := policy.Action{Kind: policy.ActionTool, Tool: &policy.ToolAction{ToolKey: "bash"}}
	decision, err := guard.Evaluate(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Effect != policy.RequireApproval || decision.Reason == "" || action.Tool.ToolKey != "bash" {
		t.Fatalf("decision = %#v action = %#v", decision, action)
	}
	if _, err := policy.NewToolRuleGuard(policy.Decision{Effect: "unknown"}); err == nil {
		t.Fatal("invalid default decision was accepted")
	}
}

type policyModule struct{}

func (policyModule) ID() string { return "policy.contract.test" }

func (policyModule) Register(reg agentslot.Registrar) error {
	first := policy.GuardFunc(func(context.Context, policy.Action) (policy.Decision, error) {
		return policy.Decision{Effect: policy.Allow}, nil
	})
	second := policy.GuardFunc(func(context.Context, policy.Action) (policy.Decision, error) {
		return policy.Decision{Effect: policy.Deny, Reason: "test"}, nil
	})
	approval := policy.ApprovalFunc(func(context.Context, policy.ApprovalRequest) (policy.ApprovalDecision, error) {
		return policy.ApprovalDecision{Approved: true}, nil
	})
	return reg.Contribute(
		agentslot.Append(policy.GuardSlot, policy.PolicyGuard(first)),
		agentslot.Append(policy.GuardSlot, policy.PolicyGuard(second)),
		agentslot.Set(policy.ApprovalSlot, policy.ApprovalService(approval)),
	)
}

var _ policy.PolicyGuard = policy.GuardFunc(nil)
var _ policy.ApprovalService = policy.ApprovalFunc(nil)
