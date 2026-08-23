package standardagent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/policy"
	"github.com/LyleLiu666/agentSlot/session"
	"github.com/LyleLiu666/agentSlot/tool"
	"github.com/LyleLiu666/agentSlot/workspace"
)

type runtimeWorkspaceBoundary struct{ scope workspace.Scope }

func (b runtimeWorkspaceBoundary) Scope() workspace.Scope { return b.scope }

func runtimeWorkspaceModule(t *testing.T, scopes ...workspace.Scope) agentslot.Module {
	t.Helper()
	available := make(map[workspace.Scope]bool, len(scopes))
	for _, scope := range scopes {
		available[scope] = true
	}
	module, err := workspace.NewModule("test.workspace", workspace.ManagerFunc(func(_ context.Context, scope workspace.Scope) (workspace.Boundary, error) {
		if !available[scope] {
			return nil, workspace.ErrNotFound
		}
		return runtimeWorkspaceBoundary{scope: scope}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return module
}

type scopeCapturingTool struct {
	definition tool.Definition
	invoked    chan tool.ToolInvocation
}

type workspaceGuardModule struct{ guard policy.PolicyGuard }

func (workspaceGuardModule) ID() string { return "test.workspace-guard" }
func (m workspaceGuardModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Append(policy.GuardSlot, m.guard))
}

func (t *scopeCapturingTool) Definition() tool.Definition       { return t.definition }
func (*scopeCapturingTool) ParallelSafety() tool.ParallelSafety { return tool.Serial }
func (t *scopeCapturingTool) Invoke(_ context.Context, invocation tool.ToolInvocation) tool.ToolResult {
	t.invoked <- invocation
	return tool.ToolResult{CallID: invocation.Call.ID, Status: tool.ResultSucceeded, Output: json.RawMessage(`{"ok":true}`)}
}

func TestRuntimeSuppliesTrustedWorkspaceScopeInsteadOfToolArguments(t *testing.T) {
	schema, err := tool.ParseInputSchema([]byte(`{"type":"object","additionalProperties":false,"properties":{"agent_id":{"type":"string"},"workspace_id":{"type":"string"}},"required":["agent_id","workspace_id"]}`))
	if err != nil {
		t.Fatal(err)
	}
	installed := &scopeCapturingTool{
		definition: tool.Definition{Name: "scope", Description: "capture trusted scope", InputSchema: schema},
		invoked:    make(chan tool.ToolInvocation, 1),
	}
	actions := make(chan policy.Action, 1)
	guard := policy.GuardFunc(func(_ context.Context, action policy.Action) (policy.Decision, error) {
		actions <- action
		return policy.Decision{Effect: policy.Allow}, nil
	})
	fake := model.NewFakeModelExecutor(
		model.FakeExecution{Events: []model.ModelEvent{{Kind: model.EventComplete, Output: &model.Completion{ToolCalls: []model.ToolCallRequest{{
			CorrelationID: "provider-call", Name: "scope", Arguments: []byte(`{"agent_id":"forged-agent","workspace_id":"forged-workspace"}`),
		}}}}}},
		model.FakeExecution{Events: []model.ModelEvent{complete("done")}},
	)
	scope := workspace.Scope{AgentID: "agent-1", WorkspaceID: "workspace-1"}
	access, stop := startRuntimeTestApplicationWithConfig(t, fake, AgentRuntimeConfig{ToolKeys: []string{"scope"}},
		runtimeWorkspaceModule(t, scope), toolModule{key: "scope", value: installed}, workspaceGuardModule{guard: guard})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("run"),
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	select {
	case invocation := <-installed.invoked:
		if invocation.AgentID != scope.AgentID || invocation.WorkspaceID != scope.WorkspaceID {
			t.Fatalf("trusted invocation scope = %q/%q, want %q/%q", invocation.AgentID, invocation.WorkspaceID, scope.AgentID, scope.WorkspaceID)
		}
		if invocation.Actor.Kind != agent.ActorAgent || invocation.Actor.ID != string(scope.AgentID) {
			t.Fatalf("trusted invocation actor = %#v", invocation.Actor)
		}
		if invocation.WorkspaceBoundary == nil || invocation.WorkspaceBoundary.Scope() != scope {
			t.Fatalf("WorkspaceBoundary = %#v, want exact trusted scope", invocation.WorkspaceBoundary)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case action := <-actions:
		if action.Tool.AgentID != scope.AgentID || action.Tool.WorkspaceID != scope.WorkspaceID {
			t.Fatalf("trusted policy scope = %q/%q, want %q/%q", action.Tool.AgentID, action.Tool.WorkspaceID, scope.AgentID, scope.WorkspaceID)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestWorkspaceManagerRejectsMissingBoundaryBeforeSessionCreation(t *testing.T) {
	entry := &captureChannel{}
	store := session.NewMemoryStore()
	application := NewApplication(ApplicationSpec{
		Name: "workspace-missing", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: store},
			runtimeWorkspaceModule(t, workspace.Scope{AgentID: "agent-1", WorkspaceID: "workspace-known"}),
			NewGatewayChannelModule("entrypoint.workspace-missing", "test", entry),
		},
	})
	running, err := application.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = running.Stop(context.Background()) })
	_, err = entry.Access().CreateSession(context.Background(), interaction.CreateSessionRequest{AgentID: "agent-1", WorkspaceID: "workspace-missing"})
	if !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("CreateSession() error = %v, want ErrNotFound", err)
	}
	listed, err := store.ListSessions(context.Background(), session.ListRequest{AgentID: agent.AgentID("agent-1"), WorkspaceID: "workspace-missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Sessions) != 0 {
		t.Fatalf("missing Workspace left %d persisted Sessions", len(listed.Sessions))
	}
}

func TestWorkspaceManagerRejectsMissingBoundaryBeforeSessionRecovery(t *testing.T) {
	entry := &captureChannel{}
	store := newSeededStore()
	application := NewApplication(ApplicationSpec{
		Name: "workspace-resume-missing", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: store},
			runtimeWorkspaceModule(t, workspace.Scope{AgentID: "agent-1", WorkspaceID: "workspace-known"}),
			NewGatewayChannelModule("entrypoint.workspace-resume-missing", "test", entry),
		},
	})
	running, err := application.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = running.Stop(context.Background()) })
	_, err = entry.Access().ResumeSession(context.Background(), interaction.ResumeSessionRequest{SessionID: "session-1"})
	if !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("ResumeSession() error = %v, want ErrNotFound", err)
	}
	if store.RecoverCalls() != 0 {
		t.Fatalf("missing Workspace triggered %d recovery calls", store.RecoverCalls())
	}
}
