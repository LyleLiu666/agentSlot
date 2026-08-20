package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	agentcontext "github.com/LyleLiu666/agentSlot/context"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/tool"
)

const (
	ToolRecall   = "memory.recall"
	ToolRemember = "memory.remember"
	ToolForget   = "memory.forget"
)

type RuntimeScope struct {
	AgentID     agent.AgentID
	WorkspaceID agent.WorkspaceID
	Scopes      []Scope
	WriteScopes []ScopeKind
}

func (s RuntimeScope) Validate() error {
	if !s.AgentID.Valid() || !s.WorkspaceID.Valid() || len(s.Scopes) == 0 {
		return errors.New("memory: invalid runtime scope")
	}
	seenScopes := make(map[ScopeKind]bool, len(s.Scopes))
	for _, scope := range s.Scopes {
		if !scope.Valid() {
			return errors.New("memory: invalid runtime scope entry")
		}
		if seenScopes[scope.Kind] {
			return errors.New("memory: duplicate runtime scope kind")
		}
		seenScopes[scope.Kind] = true
	}
	seenWrites := make(map[ScopeKind]bool, len(s.WriteScopes))
	for _, kind := range s.WriteScopes {
		if !kind.Valid() {
			return errors.New("memory: invalid write scope")
		}
		if seenWrites[kind] || !seenScopes[kind] {
			return errors.New("memory: write scope must name one unique visible scope")
		}
		seenWrites[kind] = true
	}
	return nil
}

type ScopeResolver interface {
	ResolveMemoryScope(context.Context, agent.SessionID) (RuntimeScope, error)
}

type ScopeResolverFunc func(context.Context, agent.SessionID) (RuntimeScope, error)

func (f ScopeResolverFunc) ResolveMemoryScope(ctx context.Context, sessionID agent.SessionID) (RuntimeScope, error) {
	if f == nil {
		return RuntimeScope{}, errors.New("memory: nil scope resolver")
	}
	return f(ctx, sessionID)
}

func ToolKeys() []string { return []string{ToolRecall, ToolRemember, ToolForget} }

// NewRuntimeModule contributes three standard memory tools and one pre-recall
// ContextSource backed by the selected keyed MemoryStore.
func NewRuntimeModule(id, storeKey string, scopes ScopeResolver) (agentslot.Module, error) {
	if id == "" || storeKey == "" || scopes == nil {
		return nil, errors.New("memory: runtime module requires ID, store key, and scope resolver")
	}
	return runtimeModule{id: id, storeKey: storeKey, scopes: scopes}, nil
}

type runtimeModule struct {
	id       string
	storeKey string
	scopes   ScopeResolver
}

func (m runtimeModule) ID() string { return m.id }
func (m runtimeModule) RequiredSlots() []agentslot.Requirement {
	return []agentslot.Requirement{agentslot.RequireKey(StoreSlot, m.storeKey)}
}
func (m runtimeModule) Register(reg agentslot.Registrar) error {
	contributions := []agentslot.Contribution{
		agentslot.AppendWith(agentcontext.SourceSlot, func(resolver agentslot.Resolver) (agentcontext.ContextSource, error) {
			store, err := agentslot.ResolveKey(resolver, StoreSlot, m.storeKey)
			if err != nil {
				return nil, err
			}
			return &memoryContextSource{key: "memory." + m.storeKey, store: store, scopes: m.scopes}, nil
		}),
	}
	for _, key := range ToolKeys() {
		key := key
		contributions = append(contributions, agentslot.AddWith(tool.ToolSlot, key, func(resolver agentslot.Resolver) (tool.Tool, error) {
			store, err := agentslot.ResolveKey(resolver, StoreSlot, m.storeKey)
			if err != nil {
				return nil, err
			}
			return newMemoryTool(key, store, m.scopes)
		}))
	}
	return reg.Contribute(contributions...)
}

type memoryContextSource struct {
	key    string
	store  MemoryStore
	scopes ScopeResolver
}

func (s *memoryContextSource) Key() string { return s.key }
func (s *memoryContextSource) Contribute(ctx context.Context, input agentcontext.ContextInput) ([]model.Input, error) {
	scope, err := s.scopes.ResolveMemoryScope(ctx, input.SessionID)
	if err != nil {
		return nil, err
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	query := latestRecallQuery(input.Inputs)
	if query == "" {
		return nil, nil
	}
	result, err := s.store.Recall(ctx, RecallRequest{
		Query: query, Scopes: scope.Scopes, Limit: 5,
		SessionID: input.SessionID, AgentID: scope.AgentID, WorkspaceID: scope.WorkspaceID,
	})
	if err != nil {
		return nil, err
	}
	if len(result.Items) == 0 {
		return nil, nil
	}
	var text strings.Builder
	text.WriteString("MEMORY_CONTEXT\n")
	for _, item := range result.Items {
		if err := item.Validate(); err != nil {
			return nil, err
		}
		text.WriteString("- ")
		text.WriteString(item.Summary)
		text.WriteByte('\n')
	}
	return []model.Input{{Message: &agent.Message{
		ID: agent.MessageID(fmt.Sprintf("%s-%d", s.key, input.Revision)), SessionID: input.SessionID,
		Role: agent.RoleUser, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: strings.TrimSuffix(text.String(), "\n")}},
	}}}, nil
}

type memoryTool struct {
	definition tool.Definition
	store      MemoryStore
	scopes     ScopeResolver
}

func newMemoryTool(name string, store MemoryStore, scopes ScopeResolver) (*memoryTool, error) {
	raw, description, err := memoryToolSchema(name)
	if err != nil {
		return nil, err
	}
	schema, err := tool.ParseInputSchema([]byte(raw))
	if err != nil {
		return nil, err
	}
	return &memoryTool{definition: tool.Definition{Name: name, Description: description, InputSchema: schema}, store: store, scopes: scopes}, nil
}

func (t *memoryTool) Definition() tool.Definition       { return t.definition }
func (*memoryTool) ParallelSafety() tool.ParallelSafety { return tool.Serial }
func (t *memoryTool) Invoke(ctx context.Context, invocation tool.ToolInvocation) tool.ToolResult {
	scope, err := t.scopes.ResolveMemoryScope(ctx, invocation.SessionID)
	if err == nil {
		err = scope.Validate()
	}
	var value any
	if err == nil {
		switch t.definition.Name {
		case ToolRecall:
			var args struct {
				Query string `json:"query"`
				Limit int    `json:"limit"`
			}
			if err = json.Unmarshal(invocation.Call.Arguments, &args); err == nil {
				value, err = t.store.Recall(ctx, RecallRequest{
					Query: args.Query, Limit: args.Limit, Scopes: scope.Scopes, SessionID: invocation.SessionID,
					RunID: invocation.RunID, AgentID: scope.AgentID, WorkspaceID: scope.WorkspaceID,
				})
			}
		case ToolRemember:
			var args rememberArguments
			if err = json.Unmarshal(invocation.Call.Arguments, &args); err == nil {
				var validFrom time.Time
				var validTo *time.Time
				if Kind(args.Kind) == KindTemporal {
					validFrom, err = time.Parse(time.RFC3339, args.ValidFrom)
					if err == nil && args.ValidTo != "" {
						parsed, parseErr := time.Parse(time.RFC3339, args.ValidTo)
						err = parseErr
						validTo = &parsed
					}
				}
				target, found := scopeByKind(scope.Scopes, ScopeKind(args.ScopeKind))
				if err == nil && (!found || !writeAllowed(scope.WriteScopes, target.Kind)) {
					err = errors.New("memory write scope is not allowed")
				}
				if err == nil {
					value, err = t.store.Remember(ctx, RememberRequest{
						InvocationID: string(invocation.Call.ID), SessionID: invocation.SessionID, RunID: invocation.RunID,
						AgentID: scope.AgentID, Scope: target, Kind: Kind(args.Kind), Title: args.Title, Summary: args.Summary,
						EvidenceText: args.EvidenceText, Subject: args.Subject, Predicate: args.Predicate, Object: args.Object,
						ValidFrom: validFrom, ValidTo: validTo, SourceRef: args.SourceRef,
					})
				}
			}
		case ToolForget:
			var args struct {
				TargetID  string `json:"target_id"`
				ScopeKind string `json:"scope_kind"`
				Mode      string `json:"mode"`
				Reason    string `json:"reason"`
			}
			if err = json.Unmarshal(invocation.Call.Arguments, &args); err == nil {
				target, found := scopeByKind(scope.Scopes, ScopeKind(args.ScopeKind))
				if !found || !writeAllowed(scope.WriteScopes, target.Kind) {
					err = errors.New("memory write scope is not allowed")
				} else {
					err = t.store.Forget(ctx, ForgetRequest{
						SessionID: invocation.SessionID, RunID: invocation.RunID, TargetID: args.TargetID,
						Scope: target, Mode: ForgetMode(args.Mode), Reason: args.Reason,
					})
					value = map[string]any{"target_id": args.TargetID, "applied": err == nil}
				}
			}
		}
	}
	if err != nil {
		return tool.ToolResult{CallID: invocation.Call.ID, Status: tool.ResultFailed,
			Error: &tool.StructuredError{Code: "memory_error", Message: "memory operation failed"}}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return tool.ToolResult{CallID: invocation.Call.ID, Status: tool.ResultFailed,
			Error: &tool.StructuredError{Code: "memory_encoding_failed", Message: "memory result could not be encoded"}}
	}
	return tool.ToolResult{CallID: invocation.Call.ID, Status: tool.ResultSucceeded, Output: encoded}
}

func latestRecallQuery(inputs []model.Input) string {
	for index := len(inputs) - 1; index >= 0; index-- {
		message := inputs[index].Message
		if message == nil || message.Role != agent.RoleUser {
			continue
		}
		var parts []string
		for _, part := range message.Parts {
			if part.Kind == agent.PartText && strings.TrimSpace(part.Text) != "" {
				parts = append(parts, part.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return ""
}

type rememberArguments struct {
	ScopeKind    string `json:"scope_kind"`
	Kind         string `json:"kind"`
	Title        string `json:"title"`
	Summary      string `json:"summary"`
	EvidenceText string `json:"evidence_text"`
	Subject      string `json:"subject"`
	Predicate    string `json:"predicate"`
	Object       string `json:"object"`
	ValidFrom    string `json:"valid_from"`
	ValidTo      string `json:"valid_to"`
	SourceRef    string `json:"source_ref"`
}

func scopeByKind(scopes []Scope, kind ScopeKind) (Scope, bool) {
	for _, scope := range scopes {
		if scope.Kind == kind {
			return scope, true
		}
	}
	return Scope{}, false
}

func writeAllowed(allowed []ScopeKind, kind ScopeKind) bool {
	for _, candidate := range allowed {
		if candidate == kind {
			return true
		}
	}
	return false
}

func memoryToolSchema(name string) (string, string, error) {
	switch name {
	case ToolRecall:
		return `{"type":"object","additionalProperties":false,"properties":{"query":{"type":"string","minLength":1},"limit":{"type":"integer","minimum":1,"maximum":20}},"required":["query","limit"]}`, "Recall long-term memory visible to this Session.", nil
	case ToolRemember:
		return `{"type":"object","additionalProperties":false,"properties":{"scope_kind":{"type":"string","enum":["user","org","workspace","session","agent"]},"kind":{"type":"string","enum":["summary","semantic","evidence","temporal"]},"title":{"type":"string"},"summary":{"type":"string"},"evidence_text":{"type":"string"},"subject":{"type":"string"},"predicate":{"type":"string"},"object":{"type":"string"},"valid_from":{"type":"string","format":"date-time"},"valid_to":{"type":"string","format":"date-time"},"source_ref":{"type":"string"}},"required":["scope_kind","kind"],"allOf":[{"if":{"properties":{"kind":{"const":"temporal"}},"required":["kind"]},"then":{"required":["subject","predicate","object","valid_from"]}}]}`, "Write a governed long-term-memory candidate.", nil
	case ToolForget:
		return `{"type":"object","additionalProperties":false,"properties":{"target_id":{"type":"string","minLength":1},"scope_kind":{"type":"string","enum":["user","org","workspace","session","agent"]},"mode":{"type":"string","enum":["invalidate","delete_candidate"]},"reason":{"type":"string","minLength":1}},"required":["target_id","scope_kind","mode","reason"]}`, "Invalidate formal memory or delete a candidate in an allowed scope.", nil
	default:
		return "", "", fmt.Errorf("memory: unknown tool %q", name)
	}
}
