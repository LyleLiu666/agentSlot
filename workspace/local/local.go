// Package local provides a local-directory Workspace Manager. The directory
// mapping remains private to the implementation; standard Boundary values
// expose only their trusted Agent/Workspace Scope.
package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/workspace"
)

type Binding struct {
	Scope         workspace.Scope
	RootDirectory string
}

type Manager struct {
	roots map[workspace.Scope]string
}

func NewManager(bindings ...Binding) (*Manager, error) {
	manager := &Manager{roots: make(map[workspace.Scope]string, len(bindings))}
	for _, binding := range bindings {
		if err := binding.Scope.Validate(); err != nil {
			return nil, errors.New("workspace/local: invalid Scope")
		}
		if binding.RootDirectory == "" || !filepath.IsAbs(binding.RootDirectory) || filepath.Clean(binding.RootDirectory) != binding.RootDirectory {
			return nil, errors.New("workspace/local: root directory must be absolute and clean")
		}
		info, err := os.Stat(binding.RootDirectory)
		if err != nil || !info.IsDir() {
			return nil, errors.New("workspace/local: root directory is unavailable")
		}
		if _, duplicate := manager.roots[binding.Scope]; duplicate {
			return nil, errors.New("workspace/local: duplicate Scope")
		}
		manager.roots[binding.Scope] = binding.RootDirectory
	}
	if len(manager.roots) == 0 {
		return nil, errors.New("workspace/local: at least one binding is required")
	}
	return manager, nil
}

// NewModule constructs the Manager and contributes it under the stable
// workspace.local module identity.
func NewModule(bindings ...Binding) (agentslot.Module, error) {
	manager, err := NewManager(bindings...)
	if err != nil {
		return nil, err
	}
	return workspace.NewModule("workspace.local", manager)
}

func (m *Manager) Resolve(ctx context.Context, scope workspace.Scope) (workspace.Boundary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, workspace.ErrUnavailable
	}
	root, ok := m.roots[scope]
	if !ok {
		return nil, workspace.ErrNotFound
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, workspace.ErrUnavailable
	}
	return boundary{scope: scope}, nil
}

type boundary struct{ scope workspace.Scope }

func (b boundary) Scope() workspace.Scope { return b.scope }

var _ workspace.Manager = (*Manager)(nil)
