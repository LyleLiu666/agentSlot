package tool_test

import (
	"context"
	"errors"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/tool"
)

type fakeTool struct{ schema tool.InputSchema }

func (f fakeTool) Definition() tool.Definition {
	return tool.Definition{Name: "example", InputSchema: f.schema}
}
func (fakeTool) ParallelSafety() tool.ParallelSafety { return tool.ParallelSafe }
func (fakeTool) Invoke(context.Context, tool.ToolInvocation) tool.ToolResult {
	return tool.ToolResult{CallID: "call-1", Status: tool.ResultSucceeded}
}

func TestToolRejectsDuplicateKey(t *testing.T) {
	builder := agentslot.NewBuilder()
	if err := builder.Install(module{}); err != nil {
		t.Fatalf("install first: %v", err)
	}
	err := builder.Install(secondModule{})
	if !errors.Is(err, agentslot.ErrDuplicateKey) {
		t.Fatalf("duplicate tool error = %v, want ErrDuplicateKey", err)
	}
}

type secondModule struct{}

func (secondModule) ID() string { return "tool.contracts.second" }
func (secondModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Add(tool.ToolSlot, "example", tool.Tool(fakeTool{schema: emptySchema()})))
}

type module struct{}

func (module) ID() string { return "tool.contracts" }
func (module) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Add(tool.ToolSlot, "example", tool.Tool(fakeTool{schema: emptySchema()})))
}

func emptySchema() tool.InputSchema {
	schema, err := tool.ParseInputSchema([]byte(`{"type":"object","additionalProperties":false}`))
	if err != nil {
		panic(err)
	}
	return schema
}

func TestToolIsKeyedManySlot(t *testing.T) {
	builder := agentslot.NewBuilder()
	if err := builder.Install(module{}); err != nil {
		t.Fatalf("install: %v", err)
	}
	assembly, err := builder.Build(agentslot.RequireKey(tool.ToolSlot, "example"))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, ok := agentslot.Lookup(assembly, tool.ToolSlot, "example"); !ok {
		t.Fatal("tool contribution missing")
	}
}

func TestToolResultHasOneStructuredTerminalStatus(t *testing.T) {
	valid := tool.ToolResult{CallID: "call-1", Status: tool.ResultSucceeded}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid result rejected: %v", err)
	}
	if err := (tool.ToolResult{CallID: "call-1", Status: tool.ResultFailed}).Validate(); err == nil {
		t.Fatal("failed result without structured error accepted")
	}
	failed := tool.ToolResult{
		CallID: "call-1", Status: tool.ResultFailed,
		Error: &tool.StructuredError{Code: "invalid_arguments", Message: "path is required"},
	}
	if err := failed.Validate(); err != nil {
		t.Fatalf("structured failure rejected: %v", err)
	}
	if err := (tool.ToolResult{CallID: "call-1", Status: tool.ResultUnknown}).Validate(); err != nil {
		t.Fatalf("outcome_unknown rejected: %v", err)
	}
	if err := (tool.ToolResult{CallID: "call-1", Status: tool.ResultSucceeded, Output: []byte("not-json")}).Validate(); err == nil {
		t.Fatal("non-JSON structured output accepted")
	}
}

func TestToolInvocationCarriesSessionIdentity(t *testing.T) {
	invocation := tool.ToolInvocation{SessionID: agent.SessionID("session-1"), AgentID: "agent-1", WorkspaceID: "workspace-1", RunID: "run-1", StepID: "step-1"}
	if invocation.SessionID != "session-1" || invocation.AgentID != "agent-1" || invocation.WorkspaceID != "workspace-1" || invocation.RunID != "run-1" || invocation.StepID != "step-1" {
		t.Fatalf("invocation identity lost: %#v", invocation)
	}
}
