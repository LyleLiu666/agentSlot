package workspace_test

import (
	"context"
	"errors"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/workspace"
)

type localBoundary struct {
	scope workspace.Scope
	root  string
}

func (b *localBoundary) Scope() workspace.Scope { return b.scope }

type localManager struct {
	roots map[workspace.Scope]string
}

func (m *localManager) Resolve(_ context.Context, scope workspace.Scope) (workspace.Boundary, error) {
	root, ok := m.roots[scope]
	if !ok {
		return nil, workspace.ErrNotFound
	}
	return &localBoundary{scope: scope, root: root}, nil
}

type notesBoundary struct {
	scope workspace.Scope
	notes map[string]string
}

func (b *notesBoundary) Scope() workspace.Scope { return b.scope }

type notesManager struct {
	collections map[workspace.Scope]map[string]string
}

func (m *notesManager) Resolve(_ context.Context, scope workspace.Scope) (workspace.Boundary, error) {
	notes, ok := m.collections[scope]
	if !ok {
		return nil, workspace.ErrNotFound
	}
	return &notesBoundary{scope: scope, notes: notes}, nil
}

func TestManagerContractFitsLocalAndNonFilesystemBoundaries(t *testing.T) {
	first := workspace.Scope{AgentID: "agent-1", WorkspaceID: "workspace-1"}
	second := workspace.Scope{AgentID: "agent-1", WorkspaceID: "workspace-2"}
	fixtures := []struct {
		name    string
		manager workspace.Manager
	}{
		{name: "local directories", manager: &localManager{roots: map[workspace.Scope]string{first: "/private/first", second: "/private/second"}}},
		{name: "remote notes", manager: &notesManager{collections: map[workspace.Scope]map[string]string{first: {"one": "first"}, second: {"two": "second"}}}},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			one, err := workspace.Resolve(context.Background(), fixture.manager, first)
			if err != nil || one.Scope() != first {
				t.Fatalf("Resolve(first) = %#v, %v", one, err)
			}
			two, err := workspace.Resolve(context.Background(), fixture.manager, second)
			if err != nil || two.Scope() != second || two.Scope() == one.Scope() {
				t.Fatalf("Resolve(second) = %#v, %v", two, err)
			}
			_, err = workspace.Resolve(context.Background(), fixture.manager, workspace.Scope{AgentID: "agent-1", WorkspaceID: "missing"})
			if !errors.Is(err, workspace.ErrNotFound) {
				t.Fatalf("Resolve(missing) error = %v, want ErrNotFound", err)
			}
		})
	}
}

type boundary struct{ scope workspace.Scope }

func (b boundary) Scope() workspace.Scope { return b.scope }

func TestManagerIsOptionalOneTypedSlot(t *testing.T) {
	manager := workspace.ManagerFunc(func(_ context.Context, scope workspace.Scope) (workspace.Boundary, error) {
		return boundary{scope: scope}, nil
	})
	module, err := workspace.NewModule("workspace.test", manager)
	if err != nil {
		t.Fatal(err)
	}
	builder := agentslot.NewBuilder()
	if err := builder.Install(module); err != nil {
		t.Fatal(err)
	}
	assembly, err := builder.Build(agentslot.OptionalOne(workspace.ManagerSlot))
	if err != nil {
		t.Fatal(err)
	}
	resolved, ok := agentslot.Get(assembly, workspace.ManagerSlot)
	if !ok {
		t.Fatal("workspace.manager contribution missing")
	}
	scope := workspace.Scope{AgentID: agent.AgentID("agent-1"), WorkspaceID: agent.WorkspaceID("workspace-1")}
	got, err := resolved.Resolve(context.Background(), scope)
	if err != nil || got.Scope() != scope {
		t.Fatalf("Resolve() = %#v, %v", got, err)
	}
}

func TestResolveRejectsInvalidOrSubstitutedScope(t *testing.T) {
	scope := workspace.Scope{AgentID: "agent-1", WorkspaceID: "workspace-1"}
	wrong := workspace.ManagerFunc(func(context.Context, workspace.Scope) (workspace.Boundary, error) {
		return boundary{scope: workspace.Scope{AgentID: "agent-1", WorkspaceID: "workspace-other"}}, nil
	})
	if _, err := workspace.Resolve(context.Background(), wrong, scope); err == nil {
		t.Fatal("Manager substituted a different Workspace scope")
	}
	if _, err := workspace.Resolve(context.Background(), wrong, workspace.Scope{}); err == nil {
		t.Fatal("invalid requested scope accepted")
	}
	nilBoundary := workspace.ManagerFunc(func(context.Context, workspace.Scope) (workspace.Boundary, error) {
		return nil, nil
	})
	if _, err := workspace.Resolve(context.Background(), nilBoundary, scope); err == nil {
		t.Fatal("nil Boundary accepted")
	}
}
