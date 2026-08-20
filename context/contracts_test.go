package context_test

import (
	stdcontext "context"
	"errors"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	agentcontext "github.com/LyleLiu666/agentSlot/context"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/tool"
)

type source struct{}

func (source) Key() string { return "test" }

func (source) Contribute(stdcontext.Context, agentcontext.ContextInput) ([]model.Input, error) {
	return nil, nil
}

func TestContextCompactorRejectsASecondProvider(t *testing.T) {
	builder := agentslot.NewBuilder()
	first := testModule{id: "compactor.first"}
	second := testModule{id: "compactor.second"}
	for _, candidate := range []testModule{first, second} {
		if err := builder.Install(candidate); err != nil {
			if candidate.id == second.id && errors.Is(err, agentslot.ErrSlotOccupied) {
				return
			}
			t.Fatalf("install %s: %v", candidate.id, err)
		}
	}
	t.Fatal("second context.compactor provider was accepted")
}

type testModule struct{ id string }

func (m testModule) ID() string { return m.id }
func (m testModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Set(agentcontext.CompactorSlot, agentcontext.ContextCompactor(compactor{})))
}

type compactor struct{}

func (compactor) Compact(stdcontext.Context, agentcontext.CompactionInput) (agentcontext.CompactionOutput, error) {
	return agentcontext.CompactionOutput{}, nil
}

type module struct{}

func (module) ID() string { return "context.contracts" }
func (module) Register(reg agentslot.Registrar) error {
	return reg.Contribute(
		agentslot.Append(agentcontext.SourceSlot, agentcontext.ContextSource(source{})),
		agentslot.Set(agentcontext.CompactorSlot, agentcontext.ContextCompactor(compactor{})),
	)
}

func TestContextContractsUseOrderedSourceAndReplaceableCompactor(t *testing.T) {
	builder := agentslot.NewBuilder()
	if err := builder.Install(module{}); err != nil {
		t.Fatalf("install: %v", err)
	}
	assembly, err := builder.Build(
		agentslot.RequireChain(agentcontext.SourceSlot, 1),
		agentslot.RequireOne(agentcontext.CompactorSlot),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := agentslot.Ordered(assembly, agentcontext.SourceSlot); len(got) != 1 {
		t.Fatalf("context source count = %d, want 1", len(got))
	}
	if _, ok := agentslot.Get(assembly, agentcontext.CompactorSlot); !ok {
		t.Fatal("context.compactor contribution missing")
	}
}

func TestCompactorOutputKeepsSourceRevision(t *testing.T) {
	message := &agent.Message{ID: "message-1", SessionID: "session-1", Role: agent.RoleUser, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "hello"}}}
	output := agentcontext.CompactionOutput{SourceRevision: 7, Inputs: []model.Input{{Message: message}}}
	if output.SourceRevision != 7 || len(output.Inputs) != 1 {
		t.Fatalf("compactor output = %#v", output)
	}
}

func TestTailCompactorKeepsToolProtocolGroupAndDoesNotMutateInput(t *testing.T) {
	compactor, err := agentcontext.NewTailCompactor(2)
	if err != nil {
		t.Fatal(err)
	}
	user := &agent.Message{ID: "user-1", SessionID: "session-1", RunID: "run-1", StepID: "step-1", Role: agent.RoleUser, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "old"}}}
	assistant := &agent.Message{ID: "assistant-1", SessionID: "session-1", RunID: "run-1", StepID: "step-2", Role: agent.RoleAssistant}
	call := &agent.ToolCall{ID: "call-1", MessageID: assistant.ID, SessionID: "session-1", RunID: "run-1", StepID: "step-2", Name: "lookup", Arguments: []byte(`{}`)}
	result := &tool.ToolResult{CallID: call.ID, Status: tool.ResultSucceeded}
	inputs := []model.Input{{Message: user}, {Message: assistant}, {ToolCall: call}, {ToolResult: result}}
	output, err := compactor.Compact(stdcontext.Background(), agentcontext.CompactionInput{SessionID: "session-1", Revision: 9, Inputs: inputs})
	if err != nil {
		t.Fatal(err)
	}
	if output.SourceRevision != 9 || len(output.Inputs) != 3 || output.Inputs[0].Message.ID != assistant.ID || output.Inputs[2].ToolResult.CallID != call.ID {
		t.Fatalf("compacted output = %#v", output)
	}
	output.Inputs[0].Message.Role = agent.RoleUser
	if assistant.Role != agent.RoleAssistant {
		t.Fatal("compactor output aliases input")
	}
}
