// Package sessionhistory implements the standard read-only session_history
// Tool. It exposes a model-safe projection through the existing tool Slot and
// never receives Session mutation, Runtime, Gateway, or Assembly capabilities.
package sessionhistory

import (
	"context"
	"encoding/json"
	"errors"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/artifact"
	"github.com/LyleLiu666/agentSlot/session"
	"github.com/LyleLiu666/agentSlot/tool"
)

const Key = "session_history"

const schema = `{"type":"object","properties":{"session_id":{"type":"string","minLength":1},"before_sequence":{"type":"integer","minimum":1},"step_limit":{"type":"integer","minimum":1,"maximum":100}},"additionalProperties":false}`

type Scope string

const (
	ScopeCurrentSession Scope = "current_session"
	ScopeSameWorkspace  Scope = "same_workspace"
	ScopeFullAccess     Scope = "full_access"
)

func (s Scope) Valid() bool {
	return s == ScopeCurrentSession || s == ScopeSameWorkspace || s == ScopeFullAccess
}

// HistoryReader is the complete capability held by the Tool. SessionStore
// satisfies it, but the Tool cannot call any mutation method through this
// interface.
type HistoryReader interface {
	HistoryPage(context.Context, session.HistoryPageRequest) (session.HistoryPage, error)
}

type AccessRequest struct {
	Actor              agent.ActorIdentity
	CurrentSessionID   agent.SessionID
	CurrentAgentID     agent.AgentID
	CurrentWorkspaceID agent.WorkspaceID
	TargetSessionID    agent.SessionID
	TargetAgentID      agent.AgentID
	TargetWorkspaceID  agent.WorkspaceID
}

type Authorizer interface {
	AuthorizeHistoryRead(context.Context, AccessRequest) error
}

type AuthorizerFunc func(context.Context, AccessRequest) error

func (f AuthorizerFunc) AuthorizeHistoryRead(ctx context.Context, request AccessRequest) error {
	if f == nil {
		return errors.New("sessionhistory: nil Authorizer")
	}
	return f(ctx, request)
}

type Config struct {
	Scope            Scope
	DefaultStepLimit int
	MaxStepLimit     int
	Authorizer       Authorizer
}

type Tool struct {
	reader           HistoryReader
	scope            Scope
	defaultStepLimit int
	maxStepLimit     int
	authorizer       Authorizer
	definition       tool.Definition
}

func New(reader HistoryReader, config Config) (*Tool, error) {
	if reader == nil {
		return nil, errors.New("sessionhistory: HistoryReader is required")
	}
	scope := config.Scope
	if scope == "" {
		scope = ScopeSameWorkspace
	}
	if !scope.Valid() {
		return nil, errors.New("sessionhistory: invalid read scope")
	}
	maximum := config.MaxStepLimit
	if maximum == 0 {
		maximum = 100
	}
	defaultLimit := config.DefaultStepLimit
	if defaultLimit == 0 {
		defaultLimit = 10
	}
	if maximum < 1 || maximum > 100 || defaultLimit < 1 || defaultLimit > maximum {
		return nil, errors.New("sessionhistory: step limits must satisfy 1 <= default <= maximum <= 100")
	}
	if scope == ScopeFullAccess && config.Authorizer == nil {
		return nil, errors.New("sessionhistory: full_access requires an explicit Authorizer")
	}
	input, err := tool.ParseInputSchema([]byte(schema))
	if err != nil {
		return nil, err
	}
	return &Tool{
		reader: reader, scope: scope, defaultStepLimit: defaultLimit, maxStepLimit: maximum,
		authorizer: config.Authorizer,
		definition: tool.Definition{
			Name:        Key,
			Description: "Read an older page of model-safe Session history without modifying the Session.",
			InputSchema: input,
		},
	}, nil
}

func (t *Tool) Definition() tool.Definition       { return t.definition }
func (*Tool) ParallelSafety() tool.ParallelSafety { return tool.ParallelSafe }

type arguments struct {
	SessionID      string `json:"session_id"`
	BeforeSequence uint64 `json:"before_sequence"`
	StepLimit      int    `json:"step_limit"`
}

func (t *Tool) Invoke(ctx context.Context, invocation tool.ToolInvocation) tool.ToolResult {
	if err := ctx.Err(); err != nil {
		return failure(invocation, "canceled", "Session history read was canceled")
	}
	if !invocation.SessionID.Valid() || !invocation.AgentID.Valid() || !invocation.WorkspaceID.Valid() || !invocation.Actor.Valid() || invocation.MaxInlineOutputBytes <= 0 {
		return failure(invocation, "invalid_invocation", "Trusted Session history identity or output budget is missing")
	}
	if err := t.definition.InputSchema.ValidateArguments(invocation.Call.Arguments); err != nil {
		return failure(invocation, "invalid_arguments", "Session history arguments are invalid")
	}
	var input arguments
	if err := json.Unmarshal(invocation.Call.Arguments, &input); err != nil {
		return failure(invocation, "invalid_arguments", "Session history arguments are invalid")
	}
	target := invocation.SessionID
	if input.SessionID != "" {
		target = agent.SessionID(input.SessionID)
	}
	limit := input.StepLimit
	if limit == 0 {
		limit = t.defaultStepLimit
	}
	if !target.Valid() || limit < 1 || limit > t.maxStepLimit {
		return failure(invocation, "invalid_arguments", "Session history target or step limit is invalid")
	}
	if target != invocation.SessionID && t.scope == ScopeCurrentSession {
		return denied(invocation)
	}
	page, err := t.reader.HistoryPage(ctx, session.HistoryPageRequest{
		SessionID: target, BeforeHistorySequence: session.HistorySequence(input.BeforeSequence), StepLimit: limit,
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return failure(invocation, "canceled", "Session history read was canceled")
		}
		if target != invocation.SessionID {
			return denied(invocation)
		}
		return failure(invocation, "history_unavailable", "Session history is unavailable")
	}
	if !page.AgentID.Valid() || !page.WorkspaceID.Valid() || page.Revision == 0 {
		return failure(invocation, "history_unavailable", "Session history is unavailable")
	}
	if target != invocation.SessionID {
		if t.scope == ScopeSameWorkspace && (page.AgentID != invocation.AgentID || page.WorkspaceID != invocation.WorkspaceID) {
			return denied(invocation)
		}
		if t.authorizer != nil {
			authorization := AccessRequest{
				Actor: invocation.Actor, CurrentSessionID: invocation.SessionID,
				CurrentAgentID: invocation.AgentID, CurrentWorkspaceID: invocation.WorkspaceID,
				TargetSessionID: target, TargetAgentID: page.AgentID, TargetWorkspaceID: page.WorkspaceID,
			}
			if err := t.authorizer.AuthorizeHistoryRead(ctx, authorization); err != nil {
				return denied(invocation)
			}
		}
	}
	output, err := fitResponse(target, page, invocation.MaxInlineOutputBytes)
	if err != nil {
		return failure(invocation, "result_too_large", "A complete Session history step does not fit the output budget")
	}
	return tool.ToolResult{CallID: invocation.Call.ID, Status: tool.ResultSucceeded, Output: output}
}

func denied(invocation tool.ToolInvocation) tool.ToolResult {
	return failure(invocation, "access_denied", "Session history access is not allowed")
}

func failure(invocation tool.ToolInvocation, code, message string) tool.ToolResult {
	return tool.ToolResult{CallID: invocation.Call.ID, Status: tool.ResultFailed, Error: &tool.StructuredError{Code: code, Message: message}}
}

type response struct {
	SessionID      agent.SessionID         `json:"session_id"`
	Revision       agent.Revision          `json:"revision"`
	FirstSequence  session.HistorySequence `json:"first_sequence,omitempty"`
	LastSequence   session.HistorySequence `json:"last_sequence,omitempty"`
	BeforeSequence session.HistorySequence `json:"before_sequence,omitempty"`
	HasMore        bool                    `json:"has_more"`
	Facts          []safeFact              `json:"facts"`
}

type safeFact struct {
	Sequence session.HistorySequence `json:"sequence"`
	Kind     session.HistoryFactKind `json:"kind"`
	Role     agent.Role              `json:"role,omitempty"`
	Parts    []safePart              `json:"parts,omitempty"`
	Tool     *safeTool               `json:"tool,omitempty"`
	Run      *safeRun                `json:"run,omitempty"`
	Model    *safeModelChange        `json:"model,omitempty"`
	Budget   *safeBudget             `json:"budget,omitempty"`
}

type safePart struct {
	Kind      agent.MessagePartKind `json:"kind"`
	Text      string                `json:"text,omitempty"`
	Artifact  string                `json:"artifact_id,omitempty"`
	MediaType string                `json:"media_type,omitempty"`
	Name      string                `json:"name,omitempty"`
}

type safeTool struct {
	Name      string                `json:"name,omitempty"`
	Arguments json.RawMessage       `json:"arguments,omitempty"`
	Status    tool.ResultStatus     `json:"status,omitempty"`
	Output    json.RawMessage       `json:"output,omitempty"`
	Error     *tool.StructuredError `json:"error,omitempty"`
	Artifacts []safeArtifact        `json:"artifacts,omitempty"`
}

type safeArtifact struct {
	ID        string `json:"id"`
	MediaType string `json:"media_type"`
	Name      string `json:"name,omitempty"`
	Size      int64  `json:"size"`
}

type safeSelection struct {
	ProviderKey string `json:"provider_key,omitempty"`
	ModelID     string `json:"model_id"`
	Reasoning   string `json:"reasoning"`
}

type safeRun struct {
	Kind  session.RunFactKind `json:"kind"`
	Model safeSelection       `json:"model"`
}

type safeModelChange struct {
	Previous safeSelection `json:"previous"`
	Current  safeSelection `json:"current"`
}

type safeBudget struct {
	UsedTokens int64 `json:"used_tokens"`
	MaxTokens  int64 `json:"max_tokens"`
}

type factUnit struct {
	runID  agent.RunID
	stepID agent.StepID
	facts  []session.HistoryFact
}

func fitResponse(sessionID agent.SessionID, page session.HistoryPage, budget int) (json.RawMessage, error) {
	units := groupFacts(page.Facts)
	if len(units) == 0 {
		return marshalResponse(sessionID, page.Revision, nil, page.HasMore, budget)
	}
	for start := 0; start < len(units); start++ {
		selected := flattenUnits(units[start:])
		encoded, err := marshalResponse(sessionID, page.Revision, selected, page.HasMore || start > 0, budget)
		if err == nil {
			return encoded, nil
		}
	}
	return nil, errors.New("sessionhistory: complete fact unit exceeds output budget")
}

func marshalResponse(sessionID agent.SessionID, revision agent.Revision, facts []session.HistoryFact, hasMore bool, budget int) (json.RawMessage, error) {
	projected := make([]safeFact, 0, len(facts))
	for _, fact := range facts {
		if safe, ok := projectFact(fact); ok {
			projected = append(projected, safe)
		}
	}
	result := response{SessionID: sessionID, Revision: revision, HasMore: hasMore, Facts: projected}
	if len(facts) > 0 {
		result.FirstSequence = facts[0].Sequence
		result.LastSequence = facts[len(facts)-1].Sequence
		if hasMore {
			result.BeforeSequence = facts[0].Sequence
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > budget {
		return nil, errors.New("sessionhistory: response exceeds output budget")
	}
	return encoded, nil
}

func groupFacts(facts []session.HistoryFact) []factUnit {
	units := make([]factUnit, 0)
	for _, fact := range facts {
		if fact.StepID.Valid() {
			if len(units) == 0 || units[len(units)-1].stepID != fact.StepID {
				units = append(units, factUnit{runID: fact.RunID, stepID: fact.StepID})
			}
			units[len(units)-1].facts = append(units[len(units)-1].facts, fact)
			continue
		}
		if len(units) > 0 && units[len(units)-1].stepID.Valid() {
			units[len(units)-1].facts = append(units[len(units)-1].facts, fact)
			continue
		}
		units = append(units, factUnit{runID: fact.RunID, facts: []session.HistoryFact{fact}})
	}
	return units
}

func flattenUnits(units []factUnit) []session.HistoryFact {
	var facts []session.HistoryFact
	for _, unit := range units {
		facts = append(facts, unit.facts...)
	}
	return facts
}

func projectFact(fact session.HistoryFact) (safeFact, bool) {
	projected := safeFact{Sequence: fact.Sequence, Kind: fact.Kind}
	switch {
	case fact.Message != nil:
		projected.Role = fact.Message.Role
		for _, part := range fact.Message.Parts {
			projected.Parts = append(projected.Parts, safePart{
				Kind: part.Kind, Text: part.Text, Artifact: part.AttachmentID, MediaType: part.MediaType, Name: part.Name,
			})
		}
		return projected, true
	case fact.ToolCall != nil:
		projected.Tool = &safeTool{Name: fact.ToolCall.Name, Arguments: append(json.RawMessage(nil), fact.ToolCall.Arguments...)}
		return projected, true
	case fact.ToolResult != nil:
		projected.Tool = &safeTool{
			Status: fact.ToolResult.Status, Output: append(json.RawMessage(nil), fact.ToolResult.Output...),
			Error: cloneStructuredError(fact.ToolResult.Error), Artifacts: projectArtifacts(fact.ToolResult.Artifacts),
		}
		return projected, true
	case fact.Run != nil:
		projected.Run = &safeRun{Kind: fact.Run.Kind, Model: selection(fact.Run.ModelConfig)}
		return projected, true
	case fact.ModelConfigChanged != nil:
		projected.Model = &safeModelChange{Previous: selection(fact.ModelConfigChanged.Previous), Current: selection(fact.ModelConfigChanged.Current)}
		return projected, true
	case fact.RunBudgetExceeded != nil:
		projected.Budget = &safeBudget{UsedTokens: fact.RunBudgetExceeded.UsedTokens, MaxTokens: fact.RunBudgetExceeded.MaxTokens}
		return projected, true
	default:
		// Attempt identity, continuation bytes, Context contributions, FactID,
		// Actor, timestamps, and internal envelopes are intentionally omitted.
		return safeFact{}, false
	}
}

func projectArtifacts(source []artifact.Metadata) []safeArtifact {
	result := make([]safeArtifact, 0, len(source))
	for _, item := range source {
		result = append(result, safeArtifact{ID: item.ID, MediaType: item.MediaType, Name: item.Name, Size: item.Size})
	}
	return result
}

func selection(config session.SessionModelConfig) safeSelection {
	return safeSelection{ProviderKey: config.ProviderKey, ModelID: config.ModelID, Reasoning: string(config.Reasoning)}
}

func cloneStructuredError(source *tool.StructuredError) *tool.StructuredError {
	if source == nil {
		return nil
	}
	copy := *source
	return &copy
}

type module struct{ config Config }

func NewModule(config Config) agentslot.Module { return module{config: config} }

func (module) ID() string { return "tool.session_history" }
func (module) RequiredSlots() []agentslot.Requirement {
	return []agentslot.Requirement{agentslot.RequireOne(session.StoreSlot)}
}
func (m module) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.AddWith(tool.ToolSlot, Key, func(resolver agentslot.Resolver) (tool.Tool, error) {
		store, err := agentslot.ResolveOne(resolver, session.StoreSlot)
		if err != nil {
			return nil, err
		}
		return New(HistoryReader(store), m.config)
	}))
}

var _ tool.Tool = (*Tool)(nil)
