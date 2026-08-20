package standardagent

import (
	"context"
	"testing"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/tool"
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
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan []tool.ToolResult, 1)
	go func() {
		done <- dispatcher.dispatch(context.Background(), []agent.ToolCall{
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
	}})
	if err != nil {
		t.Fatal(err)
	}
	invalid := dispatcherCall("call-1", "known")
	invalid.Arguments = []byte(`{"unexpected":true}`)
	results := dispatcher.dispatch(context.Background(), []agent.ToolCall{invalid, dispatcherCall("call-2", "missing")})
	if results[0].Error == nil || results[0].Error.Code != "invalid_arguments" || results[1].Error == nil || results[1].Error.Code != "tool_not_found" {
		t.Fatalf("safe failures = %#v", results)
	}
}

func TestToolDispatcherRejectsSlotKeyDefinitionDrift(t *testing.T) {
	_, err := newToolDispatcher([]agentslot.Named[tool.Tool]{{
		Key: "slot-key", Value: &dispatcherTool{name: "different", safety: tool.Serial, started: make(chan struct{}), schema: dispatcherSchema(t)},
	}})
	if err == nil {
		t.Fatal("dispatcher accepted Tool key/definition drift")
	}
}

type dispatcherTool struct {
	name    string
	safety  tool.ParallelSafety
	started chan struct{}
	release <-chan struct{}
	schema  tool.InputSchema
}

func (t *dispatcherTool) Definition() tool.Definition {
	return tool.Definition{Name: t.name, InputSchema: t.schema}
}
func (t *dispatcherTool) ParallelSafety() tool.ParallelSafety { return t.safety }
func (t *dispatcherTool) Invoke(_ context.Context, invocation tool.ToolInvocation) tool.ToolResult {
	close(t.started)
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
