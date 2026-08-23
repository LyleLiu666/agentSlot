package standardagent

import (
	"context"
	"testing"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/policy"
	"github.com/LyleLiu666/agentSlot/tool"
	"github.com/LyleLiu666/agentSlot/workspace"
)

func TestToolDispatcherRunsParallelSafeBatchBeforeSerialCall(t *testing.T) {
	schema := dispatcherSchema(t)
	release := make(chan struct{})
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	serialStarted := make(chan struct{})
	dispatcher, err := newToolDispatcher([]agentslot.Named[tool.Tool]{
		{Key: "first", Value: &dispatcherTool{name: "first", safety: tool.ParallelSafe, started: firstStarted, release: release, schema: schema}},
		{Key: "second", Value: &dispatcherTool{name: "second", safety: tool.ParallelSafe, started: secondStarted, release: release, schema: schema}},
		{Key: "serial", Value: &dispatcherTool{name: "serial", safety: tool.Serial, started: serialStarted, schema: schema}},
	}, nil, nil, 1024)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan []tool.ToolResult, 1)
	go func() {
		done <- dispatchForTest(dispatcher, []agent.ToolCall{
			dispatcherCall("call-1", "first"), dispatcherCall("call-2", "second"), dispatcherCall("call-3", "serial"),
		})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, started := range []<-chan struct{}{firstStarted, secondStarted} {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("parallel-safe tools did not start together")
		}
	}
	select {
	case <-serialStarted:
		t.Fatal("serial tool overlapped the parallel-safe batch")
	default:
	}
	close(release)
	select {
	case results := <-done:
		if len(results) != 3 || results[0].CallID != "call-1" || results[1].CallID != "call-2" || results[2].CallID != "call-3" {
			t.Fatalf("ordered results = %#v", results)
		}
	case <-ctx.Done():
		t.Fatal("dispatcher did not finish")
	}
}

func TestToolDispatcherReturnsSafeFailuresForUnknownAndInvalidArguments(t *testing.T) {
	schema := dispatcherSchema(t)
	dispatcher, err := newToolDispatcher([]agentslot.Named[tool.Tool]{{
		Key: "known", Value: &dispatcherTool{name: "known", safety: tool.Serial, started: make(chan struct{}), schema: schema},
	}}, nil, nil, 1024)
	if err != nil {
		t.Fatal(err)
	}
	invalid := dispatcherCall("call-1", "known")
	invalid.Arguments = []byte(`{"unexpected":true}`)
	results := dispatchForTest(dispatcher, []agent.ToolCall{invalid, dispatcherCall("call-2", "missing")})
	if results[0].Error == nil || results[0].Error.Code != "invalid_arguments" || results[1].Error == nil || results[1].Error.Code != "tool_not_found" {
		t.Fatalf("safe failures = %#v", results)
	}
}

func TestToolDispatcherRejectsSlotKeyDefinitionDrift(t *testing.T) {
	_, err := newToolDispatcher([]agentslot.Named[tool.Tool]{{
		Key: "slot-key", Value: &dispatcherTool{name: "different", safety: tool.Serial, started: make(chan struct{}), schema: dispatcherSchema(t)},
	}}, nil, nil, 1024)
	if err == nil {
		t.Fatal("dispatcher accepted Tool key/definition drift")
	}
}

func TestToolDispatcherRequiresApprovalWithoutGivingPolicyMutationAuthority(t *testing.T) {
	arguments := make(chan string, 1)
	guard := policy.GuardFunc(func(_ context.Context, action policy.Action) (policy.Decision, error) {
		action.Tool.Call.Arguments[0] = '['
		return policy.Decision{Effect: policy.RequireApproval, Reason: "external effect"}, nil
	})
	approval := policy.ApprovalFunc(func(_ context.Context, request policy.ApprovalRequest) (policy.ApprovalDecision, error) {
		if string(request.Action.Tool.Call.Arguments) != `{}` || request.Reason != "external effect" {
			t.Errorf("approval request = %#v", request)
		}
		return policy.ApprovalDecision{Approved: true}, nil
	})
	dispatcher, err := newToolDispatcher([]agentslot.Named[tool.Tool]{{
		Key: "effect", Value: &dispatcherTool{
			name: "effect", safety: tool.Serial, started: make(chan struct{}), schema: dispatcherSchema(t), arguments: arguments,
		},
	}}, []policy.PolicyGuard{guard}, approval, 1024)
	if err != nil {
		t.Fatal(err)
	}
	result := dispatchForTest(dispatcher, []agent.ToolCall{dispatcherCall("call-1", "effect")})[0]
	if result.Status != tool.ResultSucceeded {
		t.Fatalf("approved result = %#v", result)
	}
	if got := <-arguments; got != `{}` {
		t.Fatalf("tool received policy-mutated arguments %q", got)
	}
}

func TestToolDispatcherFailsClosedForPolicyAndApprovalFailures(t *testing.T) {
	tests := []struct {
		name     string
		guard    policy.PolicyGuard
		approval policy.ApprovalService
		code     string
	}{
		{name: "denied", guard: policy.GuardFunc(func(context.Context, policy.Action) (policy.Decision, error) {
			return policy.Decision{Effect: policy.Deny, Reason: "blocked"}, nil
		}), code: "policy_denied"},
		{name: "guard error", guard: policy.GuardFunc(func(context.Context, policy.Action) (policy.Decision, error) {
			return policy.Decision{}, context.DeadlineExceeded
		}), code: "policy_error"},
		{name: "missing approval", guard: policy.GuardFunc(func(context.Context, policy.Action) (policy.Decision, error) {
			return policy.Decision{Effect: policy.RequireApproval, Reason: "confirm"}, nil
		}), code: "approval_required"},
		{name: "approval denied", guard: policy.GuardFunc(func(context.Context, policy.Action) (policy.Decision, error) {
			return policy.Decision{Effect: policy.RequireApproval, Reason: "confirm"}, nil
		}), approval: policy.ApprovalFunc(func(context.Context, policy.ApprovalRequest) (policy.ApprovalDecision, error) {
			return policy.ApprovalDecision{Approved: false, Reason: "operator declined"}, nil
		}), code: "approval_denied"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			started := make(chan struct{})
			dispatcher, err := newToolDispatcher([]agentslot.Named[tool.Tool]{{
				Key: "effect", Value: &dispatcherTool{name: "effect", safety: tool.Serial, started: started, schema: dispatcherSchema(t)},
			}}, []policy.PolicyGuard{test.guard}, test.approval, 1024)
			if err != nil {
				t.Fatal(err)
			}
			result := dispatchForTest(dispatcher, []agent.ToolCall{dispatcherCall("call-1", "effect")})[0]
			if result.Status != tool.ResultFailed || result.Error == nil || result.Error.Code != test.code {
				t.Fatalf("result = %#v", result)
			}
			select {
			case <-started:
				t.Fatal("blocked tool was invoked")
			default:
			}
		})
	}
}

type dispatcherTool struct {
	name      string
	safety    tool.ParallelSafety
	started   chan struct{}
	release   <-chan struct{}
	schema    tool.InputSchema
	arguments chan<- string
}

func dispatchForTest(dispatcher *toolDispatcher, calls []agent.ToolCall) []tool.ToolResult {
	return dispatcher.dispatchPrepared(context.Background(), calls, nil, workspace.Scope{AgentID: "test-agent", WorkspaceID: "test-workspace"}, nil).results
}

func (t *dispatcherTool) Definition() tool.Definition {
	return tool.Definition{Name: t.name, InputSchema: t.schema}
}
func (t *dispatcherTool) ParallelSafety() tool.ParallelSafety { return t.safety }
func (t *dispatcherTool) Invoke(_ context.Context, invocation tool.ToolInvocation) tool.ToolResult {
	close(t.started)
	if t.arguments != nil {
		t.arguments <- string(invocation.Call.Arguments)
	}
	if t.release != nil {
		<-t.release
	}
	return tool.ToolResult{CallID: invocation.Call.ID, Status: tool.ResultSucceeded}
}

func dispatcherSchema(t *testing.T) tool.InputSchema {
	t.Helper()
	schema, err := tool.ParseInputSchema([]byte(`{"type":"object","additionalProperties":false}`))
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func dispatcherCall(id agent.ToolCallID, name string) agent.ToolCall {
	return agent.ToolCall{
		ID: id, MessageID: "message-1", SessionID: "session-1", RunID: "run-1", StepID: "step-1",
		Name: name, Arguments: []byte(`{}`),
	}
}
