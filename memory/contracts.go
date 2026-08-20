// Package memory defines the portable long-term-memory component boundary.
// Memory is deliberately separate from authoritative Session history.
package memory

import (
	"context"
	"errors"
	"math"
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
	Kind ScopeKind
	ID   string
}

func (s Scope) Valid() bool { return s.Kind.Valid() && s.ID != "" }

type Kind string

const (
	KindSummary  Kind = "summary"
	KindSemantic Kind = "semantic"
	KindEvidence Kind = "evidence"
	KindTemporal Kind = "temporal"
)

func (k Kind) Valid() bool {
	return k == KindSummary || k == KindSemantic || k == KindEvidence || k == KindTemporal
}

type RecallRequest struct {
	Query       string
	Scopes      []Scope
	Limit       int
	SessionID   agent.SessionID
	RunID       agent.RunID
	AgentID     agent.AgentID
	WorkspaceID agent.WorkspaceID
}

func (r RecallRequest) Validate() error {
	if r.Query == "" || r.Limit <= 0 || !r.SessionID.Valid() || !r.AgentID.Valid() || !r.WorkspaceID.Valid() || len(r.Scopes) == 0 {
		return errors.New("memory: invalid recall request")
	}
	for _, scope := range r.Scopes {
		if !scope.Valid() {
			return errors.New("memory: invalid recall scope")
		}
	}
	return nil
}

type Item struct {
	ID        string
	Kind      Kind
	Scope     Scope
	Summary   string
	SourceRef string
	Score     float64
}

func (i Item) Validate() error {
	if i.ID == "" || !i.Kind.Valid() || !i.Scope.Valid() || i.Summary == "" ||
		math.IsNaN(i.Score) || math.IsInf(i.Score, 0) || i.Score < 0 || i.Score > 1 {
		return errors.New("memory: invalid item")
	}
	return nil
}

type RecallResult struct {
	Items    []Item
	Degraded bool
	Warnings []string
}

type RememberRequest struct {
	InvocationID string
	SessionID    agent.SessionID
	RunID        agent.RunID
	AgentID      agent.AgentID
	Scope        Scope
	Kind         Kind
	Title        string
	Summary      string
	EvidenceText string
	Subject      string
	Predicate    string
	Object       string
	ValidFrom    time.Time
	ValidTo      *time.Time
	SourceRef    string
}

func (r RememberRequest) Validate() error {
	if r.InvocationID == "" || !r.SessionID.Valid() || !r.RunID.Valid() || !r.AgentID.Valid() || !r.Scope.Valid() || !r.Kind.Valid() {
		return errors.New("memory: invalid remember request")
	}
	switch r.Kind {
	case KindSummary, KindSemantic:
		if r.Summary == "" {
			return errors.New("memory: summary memory requires Summary")
		}
	case KindEvidence:
		if r.EvidenceText == "" {
			return errors.New("memory: evidence memory requires EvidenceText")
		}
	case KindTemporal:
		if r.Subject == "" || r.Predicate == "" || r.Object == "" || r.ValidFrom.IsZero() {
			return errors.New("memory: temporal memory requires Subject, Predicate, Object, and ValidFrom")
		}
		if r.ValidTo != nil && !r.ValidTo.After(r.ValidFrom) {
			return errors.New("memory: temporal ValidTo must be after ValidFrom")
		}
	}
	return nil
}

type RememberResult struct {
	ItemID      string
	DuplicateOf string
}

type ForgetMode string

const (
	ForgetInvalidate ForgetMode = "invalidate"
	ForgetDelete     ForgetMode = "delete_candidate"
)

func (m ForgetMode) Valid() bool { return m == ForgetInvalidate || m == ForgetDelete }

type ForgetRequest struct {
	SessionID agent.SessionID
	RunID     agent.RunID
	TargetID  string
	Scope     Scope
	Mode      ForgetMode
	Reason    string
}

func (r ForgetRequest) Validate() error {
	if !r.SessionID.Valid() || !r.RunID.Valid() || r.TargetID == "" || !r.Scope.Valid() || !r.Mode.Valid() || r.Reason == "" {
		return errors.New("memory: invalid forget request")
	}
	return nil
}

type MemoryStore interface {
	Recall(context.Context, RecallRequest) (RecallResult, error)
	Remember(context.Context, RememberRequest) (RememberResult, error)
	Forget(context.Context, ForgetRequest) error
}
