package hook

import (
	"context"
	"fmt"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
)

// SessionLifecycleSlot is the ordered, optional Session-instance open/close
// chain. The fixed Runtime owns registration, recovery, close, and Session
// commits; a contribution can only return bounded open context or a failure.
var SessionLifecycleSlot = agentslot.Chain[SessionLifecycle]("hook.session_lifecycle")

type SessionLifecycle interface {
	Descriptor() ExtensionDescriptor
	Scope() LifecycleScope
	Evaluate(context.Context, SessionLifecycleView) (SessionLifecycleResult, error)
}

type LifecyclePhase string

const (
	LifecycleOpen  LifecyclePhase = "open"
	LifecycleClose LifecyclePhase = "close"
)

func (p LifecyclePhase) Valid() bool { return p == LifecycleOpen || p == LifecycleClose }

type OpenKind string

const (
	OpenCreate  OpenKind = "create"
	OpenResume  OpenKind = "resume"
	OpenFork    OpenKind = "fork"
	OpenSummary OpenKind = "summary"
)

func (k OpenKind) Valid() bool {
	return k == OpenCreate || k == OpenResume || k == OpenFork || k == OpenSummary
}

// LifecycleScope is immutable build-time metadata. A contribution must name
// at least one phase and cannot make runtime-dependent matching decisions.
type LifecycleScope struct {
	Phases []LifecyclePhase
}

func (s LifecycleScope) Validate() error {
	if len(s.Phases) == 0 || len(s.Phases) > 2 {
		return fmt.Errorf("hook: lifecycle scope requires a bounded phase set")
	}
	seen := make(map[LifecyclePhase]struct{}, len(s.Phases))
	for _, phase := range s.Phases {
		if !phase.Valid() {
			return fmt.Errorf("hook: lifecycle scope phase is invalid")
		}
		if _, duplicate := seen[phase]; duplicate {
			return fmt.Errorf("hook: lifecycle scope phase %q is duplicated", phase)
		}
		seen[phase] = struct{}{}
	}
	return nil
}

func (s LifecycleScope) Matches(phase LifecyclePhase) bool {
	for _, candidate := range s.Phases {
		if candidate == phase {
			return true
		}
	}
	return false
}

// SessionLifecycleView is detached from Session state. OpenKind is present
// only for open; close is an explicit Gateway operation and has no open kind.
type SessionLifecycleView struct {
	InvocationID InvocationID
	SessionID    agent.SessionID
	AgentID      agent.AgentID
	WorkspaceID  agent.WorkspaceID
	Revision     agent.Revision
	Phase        LifecyclePhase
	OpenKind     OpenKind `json:",omitempty"`
}

func (v SessionLifecycleView) Validate() error {
	if !v.InvocationID.Valid() || !v.SessionID.Valid() || !v.AgentID.Valid() || !v.WorkspaceID.Valid() ||
		v.Revision == 0 || !v.Phase.Valid() {
		return fmt.Errorf("hook: invalid session lifecycle view")
	}
	if v.Phase == LifecycleOpen {
		if !v.OpenKind.Valid() {
			return fmt.Errorf("hook: open lifecycle view requires an open kind")
		}
	} else if v.OpenKind != "" {
		return fmt.Errorf("hook: close lifecycle view cannot carry an open kind")
	}
	return nil
}

type SessionLifecycleResult struct {
	Context []model.Input
}

func (r SessionLifecycleResult) Validate(view SessionLifecycleView) error {
	if err := view.Validate(); err != nil {
		return err
	}
	if view.Phase == LifecycleClose && len(r.Context) != 0 {
		return fmt.Errorf("hook: close lifecycle result cannot contribute context")
	}
	if err := validateContextProposal(r.Context, view.SessionID); err != nil {
		return fmt.Errorf("hook: invalid session lifecycle context: %w", err)
	}
	return nil
}
