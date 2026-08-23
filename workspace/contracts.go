// Package workspace defines trusted, named resource boundaries without
// assuming that a Workspace is a local filesystem directory.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
)

// ManagerSlot is the optional Workspace resource-boundary ecosystem.
var ManagerSlot = agentslot.One[Manager]("workspace.manager")

var (
	// ErrNotFound reports that no Workspace boundary exists for the exact
	// requested Agent and Workspace identity.
	ErrNotFound = errors.New("workspace: boundary not found")
	// ErrUnavailable reports that a known Workspace boundary cannot currently
	// be used. Implementations must not fall back to process-global resources.
	ErrUnavailable = errors.New("workspace: boundary unavailable")
)

// Scope is the complete portable identity of one Workspace boundary. Both
// fields are required because Workspace IDs need only be unique within the
// owning Agent.
type Scope struct {
	AgentID     agent.AgentID
	WorkspaceID agent.WorkspaceID
}

// Validate checks the complete portable Workspace identity.
func (s Scope) Validate() error {
	if !s.AgentID.Valid() || !s.WorkspaceID.Valid() {
		return errors.New("workspace: AgentID and WorkspaceID are required")
	}
	return nil
}

// Boundary is an opaque, manager-owned binding for one exact Scope. Its public
// surface intentionally exposes no path, remote address, credential, resource
// handle, or filesystem operation. Product components may coordinate private
// resource access behind implementations while standard Tools remain separate
// capabilities.
//
// A Boundary remains valid for the lifecycle documented by its Manager's
// owning Module. Manager implementations must resolve the same Scope to the
// same logical resource boundary and isolate different Scopes.
type Boundary interface {
	Scope() Scope
}

// Manager resolves a trusted Workspace identity to its resource boundary.
// Missing and unavailable resources fail explicitly; implementations must
// never fall back to a current directory, default tenant, or global handle.
type Manager interface {
	Resolve(context.Context, Scope) (Boundary, error)
}

// ManagerFunc adapts a function to Manager.
type ManagerFunc func(context.Context, Scope) (Boundary, error)

func (f ManagerFunc) Resolve(ctx context.Context, scope Scope) (Boundary, error) {
	if f == nil {
		return nil, errors.New("workspace: ManagerFunc is nil")
	}
	return f(ctx, scope)
}

// Resolve validates both sides of the Manager contract and rejects a manager
// that substitutes a different Agent or Workspace boundary.
func Resolve(ctx context.Context, manager Manager, scope Scope) (Boundary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if nilInterface(manager) {
		return nil, errors.New("workspace: Manager is required")
	}
	boundary, err := manager.Resolve(ctx, scope)
	if err != nil {
		return nil, err
	}
	if nilInterface(boundary) {
		return nil, errors.New("workspace: Manager returned a nil Boundary")
	}
	if got := boundary.Scope(); got != scope {
		return nil, fmt.Errorf("workspace: Manager returned boundary scope %q/%q for %q/%q", got.AgentID, got.WorkspaceID, scope.AgentID, scope.WorkspaceID)
	}
	return boundary, nil
}

// NewModule wraps one explicit Manager implementation for normal AgentSlot
// assembly. Managers that own lifecycle resources can provide a custom Module
// with Start and Stop instead.
func NewModule(id string, manager Manager) (agentslot.Module, error) {
	if id == "" || strings.TrimSpace(id) != id {
		return nil, errors.New("workspace: module ID must be non-empty without surrounding whitespace")
	}
	if nilInterface(manager) {
		return nil, errors.New("workspace: Manager is required")
	}
	return &module{id: id, manager: manager}, nil
}

type module struct {
	id      string
	manager Manager
}

func (m *module) ID() string { return m.id }

func (m *module) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Set(ManagerSlot, m.manager))
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
