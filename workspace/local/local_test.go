package local_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/LyleLiu666/agentSlot/workspace"
	"github.com/LyleLiu666/agentSlot/workspace/local"
)

func TestManagerBindsExactScopesWithoutExposingOrFallingBackRoots(t *testing.T) {
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	first := workspace.Scope{AgentID: "agent-1", WorkspaceID: "workspace-1"}
	second := workspace.Scope{AgentID: "agent-1", WorkspaceID: "workspace-2"}
	manager, err := local.NewManager(
		local.Binding{Scope: first, RootDirectory: firstRoot},
		local.Binding{Scope: second, RootDirectory: secondRoot},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []workspace.Scope{first, second} {
		boundary, err := workspace.Resolve(context.Background(), manager, scope)
		if err != nil || boundary.Scope() != scope {
			t.Fatalf("Resolve(%#v) = %#v, %v", scope, boundary, err)
		}
	}
	if _, err := manager.Resolve(context.Background(), workspace.Scope{AgentID: "agent-1", WorkspaceID: "missing"}); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("missing Resolve = %v", err)
	}
	if _, ok := any(manager).(interface{ Root() string }); ok {
		t.Fatal("Manager exposed a process-wide root")
	}
}

func TestManagerRejectsAmbiguousOrUnsafeBindings(t *testing.T) {
	root := t.TempDir()
	scope := workspace.Scope{AgentID: "agent-1", WorkspaceID: "workspace-1"}
	for _, bindings := range [][]local.Binding{
		{{Scope: scope, RootDirectory: "relative"}},
		{{Scope: scope, RootDirectory: filepath.Join(root, "missing")}},
		{{Scope: scope, RootDirectory: root}, {Scope: scope, RootDirectory: root}},
	} {
		if _, err := local.NewManager(bindings...); err == nil {
			t.Fatalf("NewManager accepted %#v", bindings)
		}
	}
}
