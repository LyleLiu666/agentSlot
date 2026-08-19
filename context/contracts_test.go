package context_test

import (
	stdcontext "context"
	"errors"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	agentcontext "github.com/LyleLiu666/agentSlot/context"
)

type source struct{}

func (source) Contribute(stdcontext.Context, agentcontext.ContextInput) ([]agent.Message, error) {
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
	output := agentcontext.CompactionOutput{Revision: 7, Messages: []agent.Message{{ID: "message-1"}}}
	if output.Revision != 7 || len(output.Messages) != 1 {
		t.Fatalf("compactor output = %#v", output)
	}
}
