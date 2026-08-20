package memory_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/agent"
	agentcontext "github.com/LyleLiu666/agentSlot/context"
	"github.com/LyleLiu666/agentSlot/memory"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/tool"
)

func TestMemoryRuntimeRecallsFromTheActualLatestUserIntent(t *testing.T) {
	store := &memoryStoreProbe{recallResult: memory.RecallResult{Items: []memory.Item{{
		ID: "memory-1", Kind: memory.KindSemantic, Scope: memory.Scope{Kind: memory.ScopeWorkspace, ID: "workspace-1"},
		Summary: "Use append-only history", Score: 1,
	}}}}
	assembly := buildMemoryRuntime(t, store)
	sources := agentslot.Ordered(assembly, agentcontext.SourceSlot)
	if len(sources) != 1 {
		t.Fatalf("context sources = %d", len(sources))
	}
	inputs := []model.Input{
		{Message: &agent.Message{ID: "old", SessionID: "session-1", Role: agent.RoleUser, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "old request"}}}},
		{Message: &agent.Message{ID: "answer", SessionID: "session-1", Role: agent.RoleAssistant, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "answer"}}}},
		{Message: &agent.Message{ID: "current", SessionID: "session-1", Role: agent.RoleUser, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "review the current architecture"}}}},
	}
	contribution, err := sources[0].Contribute(context.Background(), agentcontext.ContextInput{
		SessionID: "session-1", Revision: 7, Inputs: inputs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.recall.Query != "review the current architecture" {
		t.Fatalf("recall query = %q", store.recall.Query)
	}
	if len(contribution) != 1 || contribution[0].Message == nil || contribution[0].Message.ID != "memory.primary-7" ||
		!strings.Contains(contribution[0].Message.Parts[0].Text, "Use append-only history") {
		t.Fatalf("memory contribution = %#v", contribution)
	}
}

func TestMemoryRuntimeDoesNotExposeStoreErrorsToTheModel(t *testing.T) {
	store := &memoryStoreProbe{recallErr: errors.New("postgres password=super-secret")}
	assembly := buildMemoryRuntime(t, store)
	recall, ok := agentslot.Lookup(assembly, tool.ToolSlot, memory.ToolRecall)
	if !ok {
		t.Fatal("memory.recall tool was not assembled")
	}
	result := recall.Invoke(context.Background(), tool.ToolInvocation{
		Call:      tool.Call{ID: "call-1", Name: memory.ToolRecall, Arguments: []byte(`{"query":"what matters","limit":5}`)},
		SessionID: "session-1", RunID: "run-1", StepID: "step-1",
	})
	if result.Status != tool.ResultFailed || result.Error == nil || strings.Contains(result.Error.Message, "super-secret") {
		t.Fatalf("model-facing memory error = %#v", result)
	}
}

func TestMemoryItemRejectsNonFiniteScore(t *testing.T) {
	item := memory.Item{
		ID: "memory-1", Kind: memory.KindSemantic, Scope: memory.Scope{Kind: memory.ScopeUser, ID: "user-1"},
		Summary: "fact", Score: math.NaN(),
	}
	if err := item.Validate(); err == nil {
		t.Fatal("memory item accepted NaN score")
	}
}

func buildMemoryRuntime(t *testing.T, store memory.MemoryStore) *agentslot.Assembly {
	t.Helper()
	runtime, err := memory.NewRuntimeModule("memory.runtime", "primary", memory.ScopeResolverFunc(
		func(context.Context, agent.SessionID) (memory.RuntimeScope, error) {
			return memory.RuntimeScope{
				AgentID: "agent-1", WorkspaceID: "workspace-1",
				Scopes:      []memory.Scope{{Kind: memory.ScopeWorkspace, ID: "workspace-1"}},
				WriteScopes: []memory.ScopeKind{memory.ScopeWorkspace},
			}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	app := agentslot.NewApplication("memory", []agentslot.Module{memoryStoreModule{store: store}, runtime},
		agentslot.RequireChain(agentcontext.SourceSlot, 1), agentslot.RequireMany(tool.ToolSlot, 3))
	assembly, err := app.Build()
	if err != nil {
		t.Fatal(err)
	}
	return assembly
}

type memoryStoreModule struct{ store memory.MemoryStore }

func (memoryStoreModule) ID() string { return "memory.store.test" }
func (m memoryStoreModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Add(memory.StoreSlot, "primary", m.store))
}

type memoryStoreProbe struct {
	recall       memory.RecallRequest
	recallResult memory.RecallResult
	recallErr    error
}

func (s *memoryStoreProbe) Recall(_ context.Context, request memory.RecallRequest) (memory.RecallResult, error) {
	s.recall = request
	return s.recallResult, s.recallErr
}
func (*memoryStoreProbe) Remember(context.Context, memory.RememberRequest) (memory.RememberResult, error) {
	return memory.RememberResult{ItemID: "memory-1"}, nil
}
func (*memoryStoreProbe) Forget(context.Context, memory.ForgetRequest) error { return nil }
