// Package memory defines the portable long-term-memory component boundary.
// Memory is deliberately separate from authoritative Session history.
package memory

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
)

// StoreSlot is keyed because one Agent may use separate stores for personal,
// project, organizational, or specialized memory domains.
var StoreSlot = agentslot.Many[MemoryStore]("memory.store")

type ScopeKind string

const (
	ScopeUser      ScopeKind = "user"
	ScopeOrg       ScopeKind = "org"
	ScopeWorkspace ScopeKind = "workspace"
	ScopeSession   ScopeKind = "session"
	ScopeAgent     ScopeKind = "agent"
)

func (k ScopeKind) Valid() bool {
	switch k {
	case ScopeUser, ScopeOrg, ScopeWorkspace, ScopeSession, ScopeAgent:
		return true
	default:
		return false
	}
}

type Scope struct {
	Kind ScopeKind `json:"kind"`
	ID   string    `json:"id"`
}

func (s Scope) Valid() bool { return s.Kind.Valid() && validText(s.ID) }

// OperationContext is authoritative execution provenance supplied by the
// Runtime or product binding. Model-callable tools never accept these fields
// from their JSON arguments.
type OperationContext struct {
	SessionID   agent.SessionID
	RunID       agent.RunID
	StepID      agent.StepID
	AgentID     agent.AgentID
	WorkspaceID agent.WorkspaceID
	AgentRole   string
	ParentRunID agent.RunID
	RootRunID   agent.RunID
	JobID       string
}

// Validate checks portable identity. Writes require a Run and Step; pre-recall
// context projection may operate before either identity exists.
func (c OperationContext) Validate(requireRunStep bool) error {
	if !c.SessionID.Valid() || !c.AgentID.Valid() || !c.WorkspaceID.Valid() ||
		!validOptionalText(c.AgentRole) || !validOptionalText(c.JobID) ||
		!validOptionalRunID(c.ParentRunID) || !validOptionalRunID(c.RootRunID) {
		return errors.New("memory: invalid operation context")
	}
	if requireRunStep {
		if !c.RunID.Valid() || !c.StepID.Valid() {
			return errors.New("memory: write operation requires RunID and StepID")
		}
		return nil
	}
	if (c.RunID == "") != (c.StepID == "") {
		return errors.New("memory: RunID and StepID must be provided together")
	}
	if c.RunID != "" && (!c.RunID.Valid() || !c.StepID.Valid()) {
		return errors.New("memory: invalid RunID or StepID")
	}
	return nil
}

type Visibility string

const (
	VisibilityTask      Visibility = "task"
	VisibilityAgent     Visibility = "agent"
	VisibilityParent    Visibility = "parent"
	VisibilitySession   Visibility = "session"
	VisibilityWorkspace Visibility = "workspace"
	VisibilityUser      Visibility = "user"
	VisibilityOrg       Visibility = "org"
)

func (v Visibility) Valid() bool {
	switch v {
	case VisibilityTask, VisibilityAgent, VisibilityParent, VisibilitySession,
		VisibilityWorkspace, VisibilityUser, VisibilityOrg:
		return true
	default:
		return false
	}
}

type WritebackMode string

const (
	WritebackNone          WritebackMode = "none"
	WritebackSummaryOnly   WritebackMode = "summary_only"
	WritebackArtifactsOnly WritebackMode = "artifacts_only"
	WritebackFull          WritebackMode = "full"
)

func (m WritebackMode) Valid() bool {
	switch m {
	case WritebackNone, WritebackSummaryOnly, WritebackArtifactsOnly, WritebackFull:
		return true
	default:
		return false
	}
}

// WritePolicy binds one visible Scope kind to explicit governance. It is
// resolved by the host and never guessed by a MemoryStore adapter.
type WritePolicy struct {
	ScopeKind     ScopeKind
	Visibility    Visibility
	WritebackMode WritebackMode
}

func (p WritePolicy) Validate() error {
	if !p.ScopeKind.Valid() || !p.Visibility.Valid() || !p.WritebackMode.Valid() ||
		!visibilityAllowedForScope(p.ScopeKind, p.Visibility) {
		return errors.New("memory: invalid write policy")
	}
	return nil
}

func visibilityAllowedForScope(scope ScopeKind, visibility Visibility) bool {
	switch scope {
	case ScopeAgent:
		return visibility == VisibilityTask || visibility == VisibilityAgent || visibility == VisibilityParent
	case ScopeSession:
		return visibility == VisibilityTask || visibility == VisibilityParent || visibility == VisibilitySession
	case ScopeWorkspace:
		return visibility == VisibilityParent || visibility == VisibilitySession || visibility == VisibilityWorkspace
	case ScopeUser:
		return visibility == VisibilityUser
	case ScopeOrg:
		return visibility == VisibilityOrg
	default:
		return false
	}
}

type RecallIntent string

const (
	RecallTaskContinuity RecallIntent = "task_continuity"
	RecallSemanticLookup RecallIntent = "semantic_lookup"
	RecallEvidenceLookup RecallIntent = "evidence_lookup"
	RecallTemporalLookup RecallIntent = "temporal_lookup"
	RecallGeneral        RecallIntent = "general"
)

func (i RecallIntent) Valid() bool {
	switch i {
	case RecallTaskContinuity, RecallSemanticLookup, RecallEvidenceLookup, RecallTemporalLookup, RecallGeneral:
		return true
	default:
		return false
	}
}

type RecallRequest struct {
	Operation        OperationContext
	Query            string
	Intent           RecallIntent
	IncludeEvidence  bool
	Scopes           []Scope
	VisibilityFilter []Visibility
	Limit            int
}

func (r RecallRequest) Validate() error {
	if err := r.Operation.Validate(false); err != nil {
		return err
	}
	if !nonBlankText(r.Query) || !r.Intent.Valid() || r.Limit <= 0 || r.Limit > 20 || len(r.Scopes) == 0 || len(r.VisibilityFilter) == 0 {
		return errors.New("memory: invalid recall request")
	}
	seenScopes := make(map[ScopeKind]bool, len(r.Scopes))
	for _, scope := range r.Scopes {
		if !scope.Valid() || seenScopes[scope.Kind] {
			return errors.New("memory: invalid or duplicate recall scope")
		}
		seenScopes[scope.Kind] = true
	}
	seenVisibility := make(map[Visibility]bool, len(r.VisibilityFilter))
	for _, visibility := range r.VisibilityFilter {
		if !visibility.Valid() || seenVisibility[visibility] {
			return errors.New("memory: invalid or duplicate recall visibility")
		}
		seenVisibility[visibility] = true
	}
	return nil
}

type Kind string

const (
	KindSessionSummary Kind = "session_summary"
	KindSemantic       Kind = "semantic"
	KindEvidence       Kind = "evidence"
	KindTemporal       Kind = "temporal_fact"
)

func (k Kind) Valid() bool {
	return k == KindSessionSummary || k == KindSemantic || k == KindEvidence || k == KindTemporal
}

type SourceKind string

const (
	SourceUserMessage         SourceKind = "user_message"
	SourceAssistantMessage    SourceKind = "assistant_message"
	SourceToolResult          SourceKind = "tool_result"
	SourceRunlog              SourceKind = "runlog"
	SourceHostImport          SourceKind = "host_import"
	SourceWorkerConsolidation SourceKind = "worker_consolidation"
)

func (k SourceKind) Valid() bool {
	switch k {
	case SourceUserMessage, SourceAssistantMessage, SourceToolResult, SourceRunlog,
		SourceHostImport, SourceWorkerConsolidation:
		return true
	default:
		return false
	}
}

type EvidenceKind string

const (
	EvidenceConversationChunk EvidenceKind = "conversation_chunk"
	EvidenceToolOutput        EvidenceKind = "tool_output"
	EvidenceDocumentChunk     EvidenceKind = "document_chunk"
	EvidenceLogChunk          EvidenceKind = "log_chunk"
)

func (k EvidenceKind) Valid() bool {
	switch k {
	case EvidenceConversationChunk, EvidenceToolOutput, EvidenceDocumentChunk, EvidenceLogChunk:
		return true
	default:
		return false
	}
}

type RedactionState string

const (
	RedactionClean    RedactionState = "clean"
	RedactionRedacted RedactionState = "redacted"
)

func (s RedactionState) Valid() bool { return s == RedactionClean || s == RedactionRedacted }

// CandidatePayload is a closed portable union. Implementations receive one of
// the four public payloads below; provider- or product-private payloads do not
// cross the memory.store boundary.
type CandidatePayload interface {
	Kind() Kind
	memoryCandidatePayload()
}

type SessionSummaryPayload struct {
	CurrentState      string   `json:"current_state"`
	ValidatedFindings []string `json:"validated_findings"`
	NextActions       []string `json:"next_actions"`
	Blockers          []string `json:"blockers,omitempty"`
	KeyRefs           []string `json:"key_refs,omitempty"`
}

func (*SessionSummaryPayload) Kind() Kind              { return KindSessionSummary }
func (*SessionSummaryPayload) memoryCandidatePayload() {}

type SemanticPayload struct {
	Title        string   `json:"title"`
	Summary      string   `json:"summary"`
	TopicKeys    []string `json:"topic_keys"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
}

func (*SemanticPayload) Kind() Kind              { return KindSemantic }
func (*SemanticPayload) memoryCandidatePayload() {}

type EvidencePayload struct {
	EvidenceKind   EvidenceKind   `json:"evidence_kind"`
	BodyText       string         `json:"body_text"`
	MIMEType       string         `json:"mime_type"`
	RedactionState RedactionState `json:"redaction_state"`
}

func (*EvidencePayload) Kind() Kind              { return KindEvidence }
func (*EvidencePayload) memoryCandidatePayload() {}

type TemporalPayload struct {
	Subject      string     `json:"subject"`
	Predicate    string     `json:"predicate"`
	Object       string     `json:"object"`
	ValidFrom    time.Time  `json:"valid_from"`
	ValidTo      *time.Time `json:"valid_to,omitempty"`
	EvidenceRefs []string   `json:"evidence_refs,omitempty"`
}

func (*TemporalPayload) Kind() Kind              { return KindTemporal }
func (*TemporalPayload) memoryCandidatePayload() {}

type RememberRequest struct {
	Operation     OperationContext
	InvocationID  agent.ToolCallID
	Scope         Scope
	SourceKind    SourceKind
	SourceRef     string
	Confidence    float64
	Visibility    Visibility
	WritebackMode WritebackMode
	Payload       CandidatePayload
}

func (r RememberRequest) Validate() error {
	if err := r.Operation.Validate(true); err != nil {
		return err
	}
	if !r.InvocationID.Valid() || !r.Scope.Valid() || !r.SourceKind.Valid() || !validText(r.SourceRef) ||
		math.IsNaN(r.Confidence) || math.IsInf(r.Confidence, 0) || r.Confidence < 0 || r.Confidence > 1 ||
		!r.Visibility.Valid() || !r.WritebackMode.Valid() || !visibilityAllowedForScope(r.Scope.Kind, r.Visibility) {
		return errors.New("memory: invalid remember request")
	}
	switch payload := r.Payload.(type) {
	case *SessionSummaryPayload:
		if payload == nil || r.Scope.Kind != ScopeSession || !nonBlankText(payload.CurrentState) ||
			!nonBlankTextSlice(payload.ValidatedFindings, true) || !nonBlankTextSlice(payload.NextActions, true) ||
			!nonBlankTextSlice(payload.Blockers, false) || !validTextSlice(payload.KeyRefs, false) {
			return errors.New("memory: invalid session summary payload")
		}
	case *SemanticPayload:
		if payload == nil || !nonBlankText(payload.Title) || !nonBlankText(payload.Summary) ||
			!validTextSlice(payload.TopicKeys, true) || !validTextSlice(payload.EvidenceRefs, false) {
			return errors.New("memory: invalid semantic payload")
		}
	case *EvidencePayload:
		if payload == nil || !payload.EvidenceKind.Valid() || !nonBlankText(payload.BodyText) ||
			!validText(payload.MIMEType) || !payload.RedactionState.Valid() {
			return errors.New("memory: invalid evidence payload")
		}
	case *TemporalPayload:
		if payload == nil || !nonBlankText(payload.Subject) || !validText(payload.Predicate) || !nonBlankText(payload.Object) ||
			payload.ValidFrom.IsZero() || (payload.ValidTo != nil && !payload.ValidTo.After(payload.ValidFrom)) ||
			!validTextSlice(payload.EvidenceRefs, false) {
			return errors.New("memory: invalid temporal payload")
		}
	default:
		return errors.New("memory: invalid or unsupported candidate payload")
	}
	return nil
}

type RememberResult struct {
	ItemID      string `json:"item_id"`
	DuplicateOf string `json:"duplicate_of,omitempty"`
}

func (r RememberResult) Validate() error {
	if !validText(r.ItemID) || !validOptionalText(r.DuplicateOf) || (r.DuplicateOf != "" && r.DuplicateOf != r.ItemID) {
		return errors.New("memory: invalid remember result")
	}
	return nil
}

type ValidityState string

const (
	ValidityActive      ValidityState = "active"
	ValidityInvalidated ValidityState = "invalidated"
	ValiditySuperseded  ValidityState = "superseded"
)

func (s ValidityState) Valid() bool {
	return s == ValidityActive || s == ValidityInvalidated || s == ValiditySuperseded
}

type Item struct {
	ID            string        `json:"id"`
	Kind          Kind          `json:"kind"`
	Scope         Scope         `json:"scope"`
	Summary       string        `json:"summary"`
	SourceRef     string        `json:"source_ref"`
	Score         float64       `json:"score"`
	ValidityState ValidityState `json:"validity_state"`
	AgentID       agent.AgentID `json:"agent_id,omitempty"`
	AgentRole     string        `json:"agent_role,omitempty"`
	ParentRunID   agent.RunID   `json:"parent_run_id,omitempty"`
	RootRunID     agent.RunID   `json:"root_run_id,omitempty"`
	JobID         string        `json:"job_id,omitempty"`
	Visibility    Visibility    `json:"visibility"`
}

func (i Item) Validate() error {
	if !validText(i.ID) || !i.Kind.Valid() || !i.Scope.Valid() || !validText(i.Summary) || !validText(i.SourceRef) ||
		math.IsNaN(i.Score) || math.IsInf(i.Score, 0) || i.Score < 0 || i.Score > 1 || !i.ValidityState.Valid() ||
		!validOptionalAgentID(i.AgentID) || !validOptionalText(i.AgentRole) ||
		!validOptionalRunID(i.ParentRunID) || !validOptionalRunID(i.RootRunID) || !validOptionalText(i.JobID) ||
		!i.Visibility.Valid() || !visibilityAllowedForScope(i.Scope.Kind, i.Visibility) ||
		(i.Kind == KindSessionSummary && i.Scope.Kind != ScopeSession) {
		return errors.New("memory: invalid item")
	}
	return nil
}

type RecallResult struct {
	Items    []Item   `json:"items"`
	Degraded bool     `json:"degraded"`
	Warnings []string `json:"warnings,omitempty"`
}

// Validate rejects unbounded or inactive recall results before a standard Tool
// or ContextSource exposes them to a model.
func (r RecallResult) Validate(request RecallRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if len(r.Items) > request.Limit {
		return errors.New("memory: invalid recall result size")
	}
	allowedScopes := make(map[Scope]bool, len(request.Scopes))
	for _, scope := range request.Scopes {
		allowedScopes[scope] = true
	}
	allowedVisibility := make(map[Visibility]bool, len(request.VisibilityFilter))
	for _, visibility := range request.VisibilityFilter {
		allowedVisibility[visibility] = true
	}
	for _, item := range r.Items {
		if err := item.Validate(); err != nil || item.ValidityState != ValidityActive ||
			!allowedScopes[item.Scope] || !allowedVisibility[item.Visibility] {
			return errors.New("memory: invalid recall result item")
		}
	}
	for _, warning := range r.Warnings {
		if !validText(warning) {
			return errors.New("memory: invalid recall warning")
		}
	}
	return nil
}

type ForgetMode string

const (
	ForgetInvalidate ForgetMode = "invalidate"
	ForgetDelete     ForgetMode = "delete_candidate"
)

func (m ForgetMode) Valid() bool { return m == ForgetInvalidate || m == ForgetDelete }

type ForgetRequest struct {
	Operation OperationContext
	TargetID  string
	Scope     Scope
	Mode      ForgetMode
	Reason    string
}

func (r ForgetRequest) Validate() error {
	if err := r.Operation.Validate(true); err != nil {
		return err
	}
	if !validText(r.TargetID) || !r.Scope.Valid() || !r.Mode.Valid() || !nonBlankText(r.Reason) {
		return errors.New("memory: invalid forget request")
	}
	return nil
}

type MemoryStore interface {
	Recall(context.Context, RecallRequest) (RecallResult, error)
	Remember(context.Context, RememberRequest) (RememberResult, error)
	Forget(context.Context, ForgetRequest) error
}

func validText(value string) bool { return value != "" && strings.TrimSpace(value) == value }

func nonBlankText(value string) bool { return strings.TrimSpace(value) != "" }

func validOptionalText(value string) bool { return value == "" || validText(value) }

func validOptionalRunID(value agent.RunID) bool { return value == "" || value.Valid() }

func validOptionalAgentID(value agent.AgentID) bool { return value == "" || value.Valid() }

func validTextSlice(values []string, required bool) bool {
	if required && len(values) == 0 {
		return false
	}
	for _, value := range values {
		if !validText(value) {
			return false
		}
	}
	return true
}

func nonBlankTextSlice(values []string, required bool) bool {
	if required && len(values) == 0 {
		return false
	}
	for _, value := range values {
		if !nonBlankText(value) {
			return false
		}
	}
	return true
}
