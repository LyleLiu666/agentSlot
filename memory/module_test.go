package memory_test

import (
	"context"
	"encoding/json"
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
		Summary: "Use append-only history", SourceRef: "message-1", Score: 1,
		ValidityState: memory.ValidityActive, Visibility: memory.VisibilityWorkspace,
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
		Call:      tool.Call{ID: "call-1", Name: memory.ToolRecall, Arguments: []byte(`{"query":"what matters","intent":"general","include_evidence":false,"limit":5}`)},
		SessionID: "session-1", RunID: "run-1", StepID: "step-1",
	})
	if result.Status != tool.ResultFailed || result.Error == nil || strings.Contains(result.Error.Message, "super-secret") {
		t.Fatalf("model-facing memory error = %#v", result)
	}
}

func TestMemoryRuntimeForwardsRecallIntentEvidenceAndAuthoritativeScope(t *testing.T) {
	store := &memoryStoreProbe{}
	assembly := buildMemoryRuntime(t, store)
	recall, ok := agentslot.Lookup(assembly, tool.ToolSlot, memory.ToolRecall)
	if !ok {
		t.Fatal("memory.recall tool was not assembled")
	}
	arguments := []byte(`{"query":"show the source","intent":"evidence_lookup","include_evidence":true,"limit":3}`)
	if err := recall.Definition().InputSchema.ValidateArguments(arguments); err != nil {
		t.Fatal(err)
	}
	result := recall.Invoke(context.Background(), tool.ToolInvocation{
		Call:      tool.Call{ID: "call-1", Name: memory.ToolRecall, Arguments: arguments},
		SessionID: "session-1", RunID: "run-1", StepID: "step-1",
	})
	if result.Status != tool.ResultSucceeded {
		t.Fatalf("recall result = %#v", result)
	}
	request := store.recall
	if request.Intent != memory.RecallEvidenceLookup || !request.IncludeEvidence || request.Limit != 3 ||
		request.Operation.StepID != "step-1" || request.Operation.AgentRole != "primary" ||
		len(request.VisibilityFilter) != 1 || request.VisibilityFilter[0] != memory.VisibilityWorkspace {
		t.Fatalf("recall request = %#v", request)
	}
}

func TestMemoryRuntimeForwardsTypedCandidateAndInjectsGovernance(t *testing.T) {
	store := &memoryStoreProbe{}
	assembly := buildMemoryRuntime(t, store)
	remember, ok := agentslot.Lookup(assembly, tool.ToolSlot, memory.ToolRemember)
	if !ok {
		t.Fatal("memory.remember tool was not assembled")
	}
	arguments := []byte(`{
		"scope_kind":"workspace",
		"candidate_type":"semantic",
		"source_kind":"assistant_message",
		"source_ref":"message-7",
		"confidence":0.75,
		"payload":{"title":"architecture","summary":"keep history canonical","topic_keys":["history"],"evidence_refs":["message-7"]}
	}`)
	if err := remember.Definition().InputSchema.ValidateArguments(arguments); err != nil {
		t.Fatal(err)
	}
	result := remember.Invoke(context.Background(), tool.ToolInvocation{
		Call:      tool.Call{ID: "call-7", Name: memory.ToolRemember, Arguments: arguments},
		SessionID: "session-1", RunID: "run-1", StepID: "step-1",
	})
	if result.Status != tool.ResultSucceeded {
		t.Fatalf("remember result = %#v", result)
	}
	payload, ok := store.remember.Payload.(*memory.SemanticPayload)
	if !ok || payload.Title != "architecture" || len(payload.EvidenceRefs) != 1 ||
		store.remember.SourceKind != memory.SourceAssistantMessage || store.remember.SourceRef != "message-7" ||
		store.remember.Confidence != 0.75 || store.remember.Visibility != memory.VisibilityWorkspace ||
		store.remember.WritebackMode != memory.WritebackFull || store.remember.Operation.JobID != "job-1" {
		t.Fatalf("remember request = %#v, payload = %#v", store.remember, payload)
	}
	var output map[string]any
	if err := json.Unmarshal(result.Output, &output); err != nil || output["item_id"] != "memory-1" {
		t.Fatalf("remember output = %s, err = %v", result.Output, err)
	}
}

func TestMemoryRuntimeForwardsForgetWithFullOperationIdentity(t *testing.T) {
	store := &memoryStoreProbe{}
	assembly := buildMemoryRuntime(t, store)
	forget, ok := agentslot.Lookup(assembly, tool.ToolSlot, memory.ToolForget)
	if !ok {
		t.Fatal("memory.forget tool was not assembled")
	}
	arguments := []byte(`{"target_id":"memory-1","scope_kind":"workspace","mode":"invalidate","reason":"obsolete"}`)
	if err := forget.Definition().InputSchema.ValidateArguments(arguments); err != nil {
		t.Fatal(err)
	}
	result := forget.Invoke(context.Background(), tool.ToolInvocation{
		Call:      tool.Call{ID: "call-8", Name: memory.ToolForget, Arguments: arguments},
		SessionID: "session-1", RunID: "run-1", StepID: "step-1",
	})
	if result.Status != tool.ResultSucceeded || store.forget.Operation.StepID != "step-1" ||
		store.forget.Scope.Kind != memory.ScopeWorkspace || store.forget.Mode != memory.ForgetInvalidate {
		t.Fatalf("forget result = %#v, request = %#v", result, store.forget)
	}
}

func TestMemoryRuntimeRejectsUntrustedGovernanceAndWorkerSourceArguments(t *testing.T) {
	store := &memoryStoreProbe{}
	assembly := buildMemoryRuntime(t, store)
	remember, ok := agentslot.Lookup(assembly, tool.ToolSlot, memory.ToolRemember)
	if !ok {
		t.Fatal("memory.remember tool was not assembled")
	}
	for name, arguments := range map[string][]byte{
		"worker source":    []byte(`{"scope_kind":"workspace","candidate_type":"semantic","source_kind":"worker_consolidation","source_ref":"worker-1","confidence":0.5,"payload":{"title":"x","summary":"y","topic_keys":["z"]}}`),
		"model governance": []byte(`{"scope_kind":"workspace","candidate_type":"semantic","source_kind":"assistant_message","source_ref":"message-1","confidence":0.5,"visibility":"user","payload":{"title":"x","summary":"y","topic_keys":["z"]}}`),
	} {
		t.Run(name, func(t *testing.T) {
			if err := remember.Definition().InputSchema.ValidateArguments(arguments); err == nil {
				t.Fatal("untrusted argument was accepted by the advertised schema")
			}
			result := remember.Invoke(context.Background(), tool.ToolInvocation{
				Call:      tool.Call{ID: "call-untrusted", Name: memory.ToolRemember, Arguments: arguments},
				SessionID: "session-1", RunID: "run-1", StepID: "step-1",
			})
			if result.Status != tool.ResultFailed || store.remember.InvocationID != "" {
				t.Fatalf("untrusted argument reached store: result=%#v request=%#v", result, store.remember)
			}
		})
	}
}

func TestMemoryRuntimeRejectsInvalidStoreResults(t *testing.T) {
	t.Run("recall", func(t *testing.T) {
		store := &memoryStoreProbe{recallResult: memory.RecallResult{Items: []memory.Item{{
			ID: "memory-1", Kind: memory.KindSemantic,
			Scope:   memory.Scope{Kind: memory.ScopeWorkspace, ID: "workspace-1"},
			Summary: "stale", SourceRef: "message-1", Score: 1,
			ValidityState: memory.ValidityInvalidated, Visibility: memory.VisibilityWorkspace,
		}}}}
		assembly := buildMemoryRuntime(t, store)
		recall, _ := agentslot.Lookup(assembly, tool.ToolSlot, memory.ToolRecall)
		result := recall.Invoke(context.Background(), tool.ToolInvocation{
			Call:      tool.Call{ID: "call-recall", Name: memory.ToolRecall, Arguments: []byte(`{"query":"fact","intent":"general","include_evidence":false,"limit":1}`)},
			SessionID: "session-1", RunID: "run-1", StepID: "step-1",
		})
		if result.Status != tool.ResultFailed {
			t.Fatalf("invalid recall result escaped store boundary: %#v", result)
		}
	})

	t.Run("remember", func(t *testing.T) {
		store := &memoryStoreProbe{rememberResult: memory.RememberResult{ItemID: "memory-new", DuplicateOf: "memory-other"}}
		assembly := buildMemoryRuntime(t, store)
		remember, _ := agentslot.Lookup(assembly, tool.ToolSlot, memory.ToolRemember)
		result := remember.Invoke(context.Background(), tool.ToolInvocation{
			Call:      tool.Call{ID: "call-remember", Name: memory.ToolRemember, Arguments: []byte(`{"scope_kind":"workspace","candidate_type":"semantic","source_kind":"assistant_message","source_ref":"message-1","confidence":0.8,"payload":{"title":"fact","summary":"stable","topic_keys":["architecture"]}}`)},
			SessionID: "session-1", RunID: "run-1", StepID: "step-1",
		})
		if result.Status != tool.ResultFailed {
			t.Fatalf("invalid remember result escaped store boundary: %#v", result)
		}
	})
}

func TestDefaultMemoryRuntimeAllowsPerToolReplacement(t *testing.T) {
	store := &memoryStoreProbe{}
	runtime, err := memory.NewDefaultRuntimeModule("memory.runtime.default", "primary", testScopeResolver())
	if err != nil {
		t.Fatal(err)
	}
	replacement := &memoryToolProbe{definition: tool.Definition{
		Name: memory.ToolRecall, Description: "replacement recall", InputSchema: mustInputSchema(t, `{"type":"object","additionalProperties":false}`),
	}}
	app := agentslot.NewApplication("memory-default-replacement", []agentslot.Module{
		memoryStoreModule{store: store}, runtime, memoryToolModule{key: memory.ToolRecall, value: replacement},
	}, agentslot.RequireMany(tool.ToolSlot, 3), agentslot.RequireChain(agentcontext.SourceSlot, 1))
	assembly, err := app.Build()
	if err != nil {
		t.Fatal(err)
	}
	actual, ok := agentslot.Lookup(assembly, tool.ToolSlot, memory.ToolRecall)
	if !ok || actual != replacement {
		t.Fatal("explicit memory.recall Tool did not replace the default")
	}
	for _, key := range []string{memory.ToolRemember, memory.ToolForget} {
		if _, ok := agentslot.Lookup(assembly, tool.ToolSlot, key); !ok {
			t.Fatalf("default memory Tool %q was not assembled", key)
		}
	}
	if sources := agentslot.Ordered(assembly, agentcontext.SourceSlot); len(sources) != 1 {
		t.Fatalf("memory ContextSource count = %d", len(sources))
	}
}

func TestMemoryItemRejectsNonFiniteScore(t *testing.T) {
	item := memory.Item{
		ID: "memory-1", Kind: memory.KindSemantic, Scope: memory.Scope{Kind: memory.ScopeUser, ID: "user-1"},
		Summary: "fact", SourceRef: "message-1", Score: math.NaN(), ValidityState: memory.ValidityActive,
		Visibility: memory.VisibilityUser,
	}
	if err := item.Validate(); err == nil {
		t.Fatal("memory item accepted NaN score")
	}
}

func buildMemoryRuntime(t *testing.T, store memory.MemoryStore) *agentslot.Assembly {
	t.Helper()
	runtime, err := memory.NewRuntimeModule("memory.runtime", "primary", testScopeResolver())
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

func testScopeResolver() memory.ScopeResolver {
	return memory.ScopeResolverFunc(func(context.Context, agent.SessionID) (memory.RuntimeScope, error) {
		return memory.RuntimeScope{
			AgentID: "agent-1", WorkspaceID: "workspace-1",
			AgentRole: "primary", ParentRunID: "run-parent", RootRunID: "run-root", JobID: "job-1",
			Scopes:           []memory.Scope{{Kind: memory.ScopeWorkspace, ID: "workspace-1"}},
			RecallVisibility: []memory.Visibility{memory.VisibilityWorkspace},
			WritePolicies: []memory.WritePolicy{{
				ScopeKind: memory.ScopeWorkspace, Visibility: memory.VisibilityWorkspace, WritebackMode: memory.WritebackFull,
			}},
		}, nil
	})
}

func mustInputSchema(t *testing.T, raw string) tool.InputSchema {
	t.Helper()
	schema, err := tool.ParseInputSchema([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

type memoryToolModule struct {
	key   string
	value tool.Tool
}

func (memoryToolModule) ID() string { return "memory.tool.explicit" }
func (m memoryToolModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Add(tool.ToolSlot, m.key, m.value))
}

type memoryToolProbe struct{ definition tool.Definition }

func (t *memoryToolProbe) Definition() tool.Definition       { return t.definition }
func (*memoryToolProbe) ParallelSafety() tool.ParallelSafety { return tool.Serial }
func (*memoryToolProbe) Invoke(context.Context, tool.ToolInvocation) tool.ToolResult {
	return tool.ToolResult{Status: tool.ResultSucceeded}
}

type memoryStoreModule struct{ store memory.MemoryStore }

func (memoryStoreModule) ID() string { return "memory.store.test" }
func (m memoryStoreModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Add(memory.StoreSlot, "primary", m.store))
}

type memoryStoreProbe struct {
	recall         memory.RecallRequest
	recallResult   memory.RecallResult
	recallErr      error
	remember       memory.RememberRequest
	rememberResult memory.RememberResult
	rememberErr    error
	forget         memory.ForgetRequest
	forgetErr      error
}

func (s *memoryStoreProbe) Recall(_ context.Context, request memory.RecallRequest) (memory.RecallResult, error) {
	s.recall = request
	return s.recallResult, s.recallErr
}
func (s *memoryStoreProbe) Remember(_ context.Context, request memory.RememberRequest) (memory.RememberResult, error) {
	s.remember = request
	if s.rememberResult.ItemID == "" {
		s.rememberResult = memory.RememberResult{ItemID: "memory-1"}
	}
	return s.rememberResult, s.rememberErr
}
func (s *memoryStoreProbe) Forget(_ context.Context, request memory.ForgetRequest) error {
	s.forget = request
	return s.forgetErr
}
