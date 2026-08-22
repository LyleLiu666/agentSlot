package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

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
	AgentID          agent.AgentID
	WorkspaceID      agent.WorkspaceID
	AgentRole        string
	ParentRunID      agent.RunID
	RootRunID        agent.RunID
	JobID            string
	Scopes           []Scope
	RecallVisibility []Visibility
	WritePolicies    []WritePolicy
}

func (s RuntimeScope) Validate() error {
	if !s.AgentID.Valid() || !s.WorkspaceID.Valid() || !validOptionalText(s.AgentRole) ||
		!validOptionalRunID(s.ParentRunID) || !validOptionalRunID(s.RootRunID) ||
		!validOptionalText(s.JobID) || len(s.Scopes) == 0 || len(s.RecallVisibility) == 0 {
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
	seenVisibility := make(map[Visibility]bool, len(s.RecallVisibility))
	for _, visibility := range s.RecallVisibility {
		if !visibility.Valid() || seenVisibility[visibility] {
			return errors.New("memory: invalid recall visibility")
		}
		seenVisibility[visibility] = true
	}
	seenWrites := make(map[ScopeKind]bool, len(s.WritePolicies))
	for _, policy := range s.WritePolicies {
		if err := policy.Validate(); err != nil {
			return err
		}
		if seenWrites[policy.ScopeKind] || !seenScopes[policy.ScopeKind] {
			return errors.New("memory: write scope must name one unique visible scope")
		}
		seenWrites[policy.ScopeKind] = true
	}
	return nil
}

func (s RuntimeScope) operation(sessionID agent.SessionID, runID agent.RunID, stepID agent.StepID) OperationContext {
	return OperationContext{
		SessionID: sessionID, RunID: runID, StepID: stepID, AgentID: s.AgentID, WorkspaceID: s.WorkspaceID,
		AgentRole: s.AgentRole, ParentRunID: s.ParentRunID, RootRunID: s.RootRunID, JobID: s.JobID,
	}
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
	return newRuntimeModule(id, storeKey, scopes, false)
}

// NewDefaultRuntimeModule always appends memory recall context, while each
// standard memory Tool is contributed only when that Tool key has not been
// installed explicitly. This lets products replace one Tool without copying
// or disabling the rest of the memory runtime.
func NewDefaultRuntimeModule(id, storeKey string, scopes ScopeResolver) (agentslot.Module, error) {
	return newRuntimeModule(id, storeKey, scopes, true)
}

func newRuntimeModule(id, storeKey string, scopes ScopeResolver, conditionalTools bool) (agentslot.Module, error) {
	if id == "" || storeKey == "" || scopes == nil {
		return nil, errors.New("memory: runtime module requires ID, store key, and scope resolver")
	}
	return runtimeModule{id: id, storeKey: storeKey, scopes: scopes, conditionalTools: conditionalTools}, nil
}

type runtimeModule struct {
	id               string
	storeKey         string
	scopes           ScopeResolver
	conditionalTools bool
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
		constructor := func(resolver agentslot.Resolver) (tool.Tool, error) {
			store, err := agentslot.ResolveKey(resolver, StoreSlot, m.storeKey)
			if err != nil {
				return nil, err
			}
			return newMemoryTool(key, store, m.scopes)
		}
		if m.conditionalTools {
			contributions = append(contributions, agentslot.AddDefaultWith(tool.ToolSlot, key, constructor))
		} else {
			contributions = append(contributions, agentslot.AddWith(tool.ToolSlot, key, constructor))
		}
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
	request := RecallRequest{
		Operation: scope.operation(input.SessionID, "", ""), Query: query,
		Intent: RecallTaskContinuity, IncludeEvidence: false,
		Scopes: cloneScopes(scope.Scopes), VisibilityFilter: cloneVisibility(scope.RecallVisibility), Limit: 5,
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	result, err := s.store.Recall(ctx, cloneRecallRequest(request))
	if err != nil {
		return nil, err
	}
	if err := result.Validate(request); err != nil {
		return nil, err
	}
	if len(result.Items) == 0 {
		return nil, nil
	}
	var text strings.Builder
	text.WriteString("MEMORY_CONTEXT\n")
	for _, item := range result.Items {
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
	err := t.definition.InputSchema.ValidateArguments(invocation.Call.Arguments)
	var scope RuntimeScope
	if err == nil {
		scope, err = t.scopes.ResolveMemoryScope(ctx, invocation.SessionID)
	}
	if err == nil {
		err = scope.Validate()
	}
	var value any
	if err == nil {
		switch t.definition.Name {
		case ToolRecall:
			var args recallArguments
			if err = decodeStrict(invocation.Call.Arguments, &args); err == nil {
				request := RecallRequest{
					Operation: scope.operation(invocation.SessionID, invocation.RunID, invocation.StepID),
					Query:     args.Query, Intent: args.Intent, IncludeEvidence: args.IncludeEvidence,
					Scopes: cloneScopes(scope.Scopes), VisibilityFilter: cloneVisibility(scope.RecallVisibility), Limit: args.Limit,
				}
				if err = request.Validate(); err == nil {
					var result RecallResult
					result, err = t.store.Recall(ctx, cloneRecallRequest(request))
					if err == nil {
						err = result.Validate(request)
						value = result
					}
				}
			}
		case ToolRemember:
			var args rememberArguments
			if err = decodeStrict(invocation.Call.Arguments, &args); err == nil {
				target, found := scopeByKind(scope.Scopes, ScopeKind(args.ScopeKind))
				policy, allowed := writePolicyFor(scope.WritePolicies, target.Kind)
				if !found || !allowed {
					err = errors.New("memory write scope is not allowed")
				}
				if err == nil {
					var payload CandidatePayload
					payload, err = decodeCandidatePayload(args.CandidateType, args.Payload)
					request := RememberRequest{
						Operation:    scope.operation(invocation.SessionID, invocation.RunID, invocation.StepID),
						InvocationID: invocation.Call.ID, Scope: target, SourceKind: args.SourceKind,
						SourceRef: args.SourceRef, Confidence: args.Confidence,
						Visibility: policy.Visibility, WritebackMode: policy.WritebackMode, Payload: payload,
					}
					if err == nil {
						err = request.Validate()
					}
					if err == nil {
						var result RememberResult
						result, err = t.store.Remember(ctx, request)
						if err == nil {
							err = result.Validate()
							value = result
						}
					}
				}
			}
		case ToolForget:
			var args forgetArguments
			if err = decodeStrict(invocation.Call.Arguments, &args); err == nil {
				target, found := scopeByKind(scope.Scopes, ScopeKind(args.ScopeKind))
				_, allowed := writePolicyFor(scope.WritePolicies, target.Kind)
				if !found || !allowed {
					err = errors.New("memory write scope is not allowed")
				} else {
					request := ForgetRequest{
						Operation: scope.operation(invocation.SessionID, invocation.RunID, invocation.StepID),
						TargetID:  args.TargetID, Scope: target, Mode: args.Mode, Reason: args.Reason,
					}
					if err = request.Validate(); err == nil {
						err = t.store.Forget(ctx, request)
					}
					if err == nil {
						value = map[string]any{"target_id": args.TargetID, "applied": true}
					}
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

type recallArguments struct {
	Query           string       `json:"query"`
	Intent          RecallIntent `json:"intent"`
	IncludeEvidence bool         `json:"include_evidence"`
	Limit           int          `json:"limit"`
}

type rememberArguments struct {
	ScopeKind     ScopeKind       `json:"scope_kind"`
	CandidateType Kind            `json:"candidate_type"`
	SourceKind    SourceKind      `json:"source_kind"`
	SourceRef     string          `json:"source_ref"`
	Confidence    float64         `json:"confidence"`
	Payload       json.RawMessage `json:"payload"`
}

type forgetArguments struct {
	TargetID  string     `json:"target_id"`
	ScopeKind ScopeKind  `json:"scope_kind"`
	Mode      ForgetMode `json:"mode"`
	Reason    string     `json:"reason"`
}

func scopeByKind(scopes []Scope, kind ScopeKind) (Scope, bool) {
	for _, scope := range scopes {
		if scope.Kind == kind {
			return scope, true
		}
	}
	return Scope{}, false
}

func cloneScopes(scopes []Scope) []Scope { return append([]Scope(nil), scopes...) }

func cloneVisibility(visibility []Visibility) []Visibility {
	return append([]Visibility(nil), visibility...)
}

func cloneRecallRequest(request RecallRequest) RecallRequest {
	request.Scopes = cloneScopes(request.Scopes)
	request.VisibilityFilter = cloneVisibility(request.VisibilityFilter)
	return request
}

func writePolicyFor(policies []WritePolicy, kind ScopeKind) (WritePolicy, bool) {
	for _, policy := range policies {
		if policy.ScopeKind == kind {
			return policy, true
		}
	}
	return WritePolicy{}, false
}

func decodeCandidatePayload(kind Kind, raw json.RawMessage) (CandidatePayload, error) {
	var payload CandidatePayload
	switch kind {
	case KindSessionSummary:
		payload = &SessionSummaryPayload{}
	case KindSemantic:
		payload = &SemanticPayload{}
	case KindEvidence:
		payload = &EvidencePayload{}
	case KindTemporal:
		payload = &TemporalPayload{}
	default:
		return nil, errors.New("memory: unsupported candidate type")
	}
	if err := decodeStrict(raw, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func decodeStrict(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("memory: arguments contain trailing JSON")
	}
	return nil
}

func memoryToolSchema(name string) (string, string, error) {
	switch name {
	case ToolRecall:
		return `{"type":"object","additionalProperties":false,"properties":{"query":{"type":"string","minLength":1},"intent":{"type":"string","enum":["task_continuity","semantic_lookup","evidence_lookup","temporal_lookup","general"]},"include_evidence":{"type":"boolean"},"limit":{"type":"integer","minimum":1,"maximum":20}},"required":["query","intent","include_evidence","limit"]}`, "Recall long-term memory visible to this Session.", nil
	case ToolRemember:
		return rememberToolSchema, "Write a governed long-term-memory candidate.", nil
	case ToolForget:
		return `{"type":"object","additionalProperties":false,"properties":{"target_id":{"type":"string","minLength":1},"scope_kind":{"type":"string","enum":["user","org","workspace","session","agent"]},"mode":{"type":"string","enum":["invalidate","delete_candidate"]},"reason":{"type":"string","minLength":1}},"required":["target_id","scope_kind","mode","reason"]}`, "Invalidate formal memory or delete a candidate in an allowed scope.", nil
	default:
		return "", "", fmt.Errorf("memory: unknown tool %q", name)
	}
}

const rememberToolSchema = `{
  "type":"object",
  "additionalProperties":false,
  "properties":{
    "scope_kind":{"type":"string","enum":["user","org","workspace","session","agent"]},
    "candidate_type":{"type":"string","enum":["session_summary","semantic","evidence","temporal_fact"]},
    "source_kind":{"type":"string","enum":["user_message","assistant_message","tool_result","runlog","host_import"]},
    "source_ref":{"type":"string","minLength":1},
    "confidence":{"type":"number","minimum":0,"maximum":1},
    "payload":{"type":"object"}
  },
  "required":["scope_kind","candidate_type","source_kind","source_ref","confidence","payload"],
  "allOf":[
    {"if":{"properties":{"candidate_type":{"const":"session_summary"}},"required":["candidate_type"]},"then":{"properties":{"scope_kind":{"const":"session"},"payload":{"type":"object","additionalProperties":false,"properties":{"current_state":{"type":"string","minLength":1},"validated_findings":{"type":"array","minItems":1,"items":{"type":"string","minLength":1}},"next_actions":{"type":"array","minItems":1,"items":{"type":"string","minLength":1}},"blockers":{"type":"array","items":{"type":"string","minLength":1}},"key_refs":{"type":"array","items":{"type":"string","minLength":1}}},"required":["current_state","validated_findings","next_actions"]}}}},
    {"if":{"properties":{"candidate_type":{"const":"semantic"}},"required":["candidate_type"]},"then":{"properties":{"payload":{"type":"object","additionalProperties":false,"properties":{"title":{"type":"string","minLength":1},"summary":{"type":"string","minLength":1},"topic_keys":{"type":"array","minItems":1,"items":{"type":"string","minLength":1}},"evidence_refs":{"type":"array","items":{"type":"string","minLength":1}}},"required":["title","summary","topic_keys"]}}}},
    {"if":{"properties":{"candidate_type":{"const":"evidence"}},"required":["candidate_type"]},"then":{"properties":{"payload":{"type":"object","additionalProperties":false,"properties":{"evidence_kind":{"type":"string","enum":["conversation_chunk","tool_output","document_chunk","log_chunk"]},"body_text":{"type":"string","minLength":1},"mime_type":{"type":"string","minLength":1},"redaction_state":{"type":"string","enum":["clean","redacted"]}},"required":["evidence_kind","body_text","mime_type","redaction_state"]}}}},
    {"if":{"properties":{"candidate_type":{"const":"temporal_fact"}},"required":["candidate_type"]},"then":{"properties":{"payload":{"type":"object","additionalProperties":false,"properties":{"subject":{"type":"string","minLength":1},"predicate":{"type":"string","minLength":1},"object":{"type":"string","minLength":1},"valid_from":{"type":"string","format":"date-time"},"valid_to":{"type":"string","format":"date-time"},"evidence_refs":{"type":"array","items":{"type":"string","minLength":1}}},"required":["subject","predicate","object","valid_from"]}}}}
  ]
}`
