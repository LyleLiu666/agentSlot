package standardagent

import (
	"context"
	"sort"
	"sync"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

// gateway is the fixed, application-scoped user-operation boundary. It owns
// no Session truth: every Session operation is routed through the private
// runtimeAccess selected by runtimeCoordinator.
type gateway struct {
	runtime *applicationRuntime
}

func withCoordinator[T any](ctx context.Context, g *gateway, operation func(context.Context, *runtimeCoordinator) (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if g == nil || g.runtime == nil {
		return zero, notStartedError()
	}
	coordinator, operationCtx, release, err := g.runtime.acquire(ctx)
	if err != nil {
		return zero, err
	}
	defer release()
	return operation(operationCtx, coordinator)
}

func withRuntime[T any](ctx context.Context, g *gateway, id agent.SessionID, operation func(context.Context, runtimeAccess) (T, error)) (T, error) {
	return withCoordinator(ctx, g, func(operationCtx context.Context, coordinator *runtimeCoordinator) (T, error) {
		runtime, err := coordinator.runtime(id)
		if err != nil {
			var zero T
			return zero, err
		}
		if err := runtime.awaitOpen(operationCtx); err != nil {
			var zero T
			return zero, err
		}
		return operation(operationCtx, runtime)
	})
}

func (g *gateway) ListSessions(ctx context.Context, request interaction.ListSessionsRequest) (interaction.SessionList, error) {
	return withCoordinator(ctx, g, func(operationCtx context.Context, coordinator *runtimeCoordinator) (interaction.SessionList, error) {
		if !request.AgentID.Valid() || !request.WorkspaceID.Valid() {
			return interaction.SessionList{}, invalidInput("gateway.list_sessions", "AgentID and WorkspaceID are required")
		}
		listed, err := coordinator.components.store.ListSessions(operationCtx, session.ListRequest{
			AgentID: request.AgentID, WorkspaceID: request.WorkspaceID,
			Limit: request.Limit, Cursor: request.Cursor,
		})
		if err != nil {
			return interaction.SessionList{}, err
		}
		result := interaction.SessionList{
			Sessions:   make([]interaction.SessionSummary, len(listed.Sessions)),
			NextCursor: listed.NextCursor,
		}
		for index, summary := range listed.Sessions {
			result.Sessions[index] = interaction.SessionSummary{
				SessionID: summary.SessionID, AgentID: summary.AgentID, WorkspaceID: summary.WorkspaceID,
				Revision: summary.Revision, UpdatedAt: summary.UpdatedAt,
			}
		}
		return result, nil
	})
}

func (g *gateway) CreateSession(ctx context.Context, request interaction.CreateSessionRequest) (interaction.SessionOpened, error) {
	return withCoordinator(ctx, g, func(operationCtx context.Context, coordinator *runtimeCoordinator) (interaction.SessionOpened, error) {
		return coordinator.create(operationCtx, request)
	})
}

func (g *gateway) ResumeSession(ctx context.Context, request interaction.ResumeSessionRequest) (interaction.SessionOpened, error) {
	return withCoordinator(ctx, g, func(operationCtx context.Context, coordinator *runtimeCoordinator) (interaction.SessionOpened, error) {
		return coordinator.resume(operationCtx, request)
	})
}

func (g *gateway) ForkSession(ctx context.Context, request interaction.ForkSessionRequest) (interaction.SessionOpened, error) {
	return withCoordinator(ctx, g, func(operationCtx context.Context, coordinator *runtimeCoordinator) (interaction.SessionOpened, error) {
		return coordinator.fork(operationCtx, request)
	})
}

func (g *gateway) StartSessionFromSummary(ctx context.Context, request interaction.SummarySessionRequest) (interaction.SessionOpened, error) {
	return withCoordinator(ctx, g, func(operationCtx context.Context, coordinator *runtimeCoordinator) (interaction.SessionOpened, error) {
		return coordinator.summary(operationCtx, request)
	})
}

func (g *gateway) Send(ctx context.Context, request interaction.SendRequest) (interaction.EnqueueReceipt, error) {
	return withRuntime(ctx, g, request.SessionID, func(operationCtx context.Context, runtime runtimeAccess) (interaction.EnqueueReceipt, error) {
		return runtime.send(operationCtx, request)
	})
}

func (g *gateway) SendAndWait(ctx context.Context, request interaction.SendRequest) (interaction.RunResult, error) {
	return withRuntime(ctx, g, request.SessionID, func(operationCtx context.Context, runtime runtimeAccess) (interaction.RunResult, error) {
		receipt, err := runtime.send(operationCtx, request)
		if err != nil {
			return interaction.RunResult{}, err
		}
		if err := runtime.idle(operationCtx, interaction.WhenIdleRequest{SessionID: request.SessionID}); err != nil {
			return interaction.RunResult{}, err
		}
		return runtime.runResult(operationCtx, receipt.MessageID)
	})
}

func aggregateRunResult(snapshot interaction.SessionView, inputID agent.MessageID) (interaction.RunResult, error) {
	result := interaction.RunResult{SessionID: snapshot.SessionID, InputMessageID: inputID, Revision: snapshot.Revision}
	for _, fact := range snapshot.RecentHistory {
		if fact.Message != nil && fact.Message.ID == inputID {
			result.RunID = fact.Message.RunID
			break
		}
	}
	if !result.RunID.Valid() {
		return interaction.RunResult{}, agent.NewCodedError(agent.ErrorConflict, agent.CodeNoPendingWork, "gateway.send_and_wait", "input was accepted but did not start a Run", nil)
	}
	for _, fact := range snapshot.RecentHistory {
		if fact.Message != nil && fact.Message.RunID == result.RunID && fact.Message.Role == agent.RoleAssistant && assistantHasText(*fact.Message) {
			message := cloneRuntimeMessage(*fact.Message)
			result.AssistantMessages = append(result.AssistantMessages, message)
		}
		if fact.Run != nil && fact.Run.RunID == result.RunID && fact.Run.Kind != session.RunStarted {
			result.Outcome = fact.Run.Kind
		}
	}
	if !result.Outcome.Valid() || result.Outcome == session.RunStarted {
		return interaction.RunResult{}, agent.NewError(agent.ErrorInternal, "gateway.send_and_wait", "Run became idle without a terminal History fact", nil)
	}
	return result, nil
}

func assistantHasText(message agent.Message) bool {
	for _, part := range message.Parts {
		if part.Kind == agent.PartText {
			return true
		}
	}
	return false
}

func (g *gateway) Steer(ctx context.Context, request interaction.SteerRequest) (interaction.EnqueueReceipt, error) {
	return withRuntime(ctx, g, request.SessionID, func(operationCtx context.Context, runtime runtimeAccess) (interaction.EnqueueReceipt, error) {
		return runtime.steer(operationCtx, request)
	})
}

func (g *gateway) RunPending(ctx context.Context, request interaction.RunPendingRequest) (interaction.RunReceipt, error) {
	return withRuntime(ctx, g, request.SessionID, func(operationCtx context.Context, runtime runtimeAccess) (interaction.RunReceipt, error) {
		return runtime.pending(operationCtx, request)
	})
}

func (g *gateway) Cancel(ctx context.Context, request interaction.CancelRequest) error {
	_, err := withRuntime(ctx, g, request.SessionID, func(operationCtx context.Context, runtime runtimeAccess) (struct{}, error) {
		return struct{}{}, runtime.cancel(operationCtx, request)
	})
	return err
}

func (g *gateway) WhenIdle(ctx context.Context, request interaction.WhenIdleRequest) error {
	_, err := withRuntime(ctx, g, request.SessionID, func(operationCtx context.Context, runtime runtimeAccess) (struct{}, error) {
		return struct{}{}, runtime.idle(operationCtx, request)
	})
	return err
}

func (g *gateway) EditQueued(ctx context.Context, request interaction.EditQueuedRequest) (interaction.CommitReceipt, error) {
	return withRuntime(ctx, g, request.SessionID, func(operationCtx context.Context, runtime runtimeAccess) (interaction.CommitReceipt, error) {
		return runtime.editQueued(operationCtx, request)
	})
}

func (g *gateway) DeleteQueued(ctx context.Context, request interaction.DeleteQueuedRequest) (interaction.CommitReceipt, error) {
	return withRuntime(ctx, g, request.SessionID, func(operationCtx context.Context, runtime runtimeAccess) (interaction.CommitReceipt, error) {
		return runtime.deleteQueued(operationCtx, request)
	})
}

func (g *gateway) ReclassifyQueued(ctx context.Context, request interaction.ReclassifyQueuedRequest) (interaction.CommitReceipt, error) {
	return withRuntime(ctx, g, request.SessionID, func(operationCtx context.Context, runtime runtimeAccess) (interaction.CommitReceipt, error) {
		return runtime.reclassifyQueued(operationCtx, request)
	})
}

func (g *gateway) ModelConfig(ctx context.Context, request interaction.ModelConfigRequest) (interaction.ModelConfigView, error) {
	return withRuntime(ctx, g, request.SessionID, func(operationCtx context.Context, runtime runtimeAccess) (interaction.ModelConfigView, error) {
		return runtime.modelConfig(operationCtx, request)
	})
}

func (g *gateway) UpdateModelConfig(ctx context.Context, request interaction.UpdateModelConfigRequest) (interaction.CommitReceipt, error) {
	return withRuntime(ctx, g, request.SessionID, func(operationCtx context.Context, runtime runtimeAccess) (interaction.CommitReceipt, error) {
		return runtime.updateModelConfig(operationCtx, request)
	})
}

func (g *gateway) View(ctx context.Context, request interaction.SessionViewRequest) (interaction.SessionView, error) {
	return withRuntime(ctx, g, request.SessionID, func(operationCtx context.Context, runtime runtimeAccess) (interaction.SessionView, error) {
		return runtime.view(operationCtx, request)
	})
}

func (g *gateway) History(ctx context.Context, request interaction.HistoryRequest) (interaction.HistoryPage, error) {
	return withRuntime(ctx, g, request.SessionID, func(operationCtx context.Context, runtime runtimeAccess) (interaction.HistoryPage, error) {
		return runtime.history(operationCtx, request)
	})
}

func (g *gateway) ExtensionDiagnostics(ctx context.Context, request interaction.ExtensionDiagnosticsRequest) (interaction.ExtensionDiagnosticsPage, error) {
	return withRuntime(ctx, g, request.SessionID, func(operationCtx context.Context, runtime runtimeAccess) (interaction.ExtensionDiagnosticsPage, error) {
		return runtime.extensionDiagnostics(operationCtx, request)
	})
}

func (g *gateway) Subscribe(ctx context.Context, request interaction.SubscribeRequest) (interaction.EventStream, error) {
	return withRuntime(ctx, g, request.SessionID, func(operationCtx context.Context, runtime runtimeAccess) (interaction.EventStream, error) {
		return runtime.subscribe(operationCtx, request)
	})
}

func (g *gateway) Commands(ctx context.Context, _ interaction.CommandScope) ([]interaction.CommandDescriptor, error) {
	return withCoordinator(ctx, g, func(context.Context, *runtimeCoordinator) ([]interaction.CommandDescriptor, error) {
		descriptors := make([]interaction.CommandDescriptor, 0, len(g.runtime.dependencies.commandDescriptors))
		for _, descriptor := range g.runtime.dependencies.commandDescriptors {
			descriptors = append(descriptors, cloneCommandDescriptor(descriptor))
		}
		sort.Slice(descriptors, func(i, j int) bool { return descriptors[i].Key < descriptors[j].Key })
		return descriptors, nil
	})
}

func (g *gateway) InvokeCommand(ctx context.Context, invocation interaction.CommandInvocation) (interaction.CommandResult, error) {
	if err := ctx.Err(); err != nil {
		return interaction.CommandResult{}, err
	}
	if g == nil || g.runtime == nil {
		return interaction.CommandResult{}, notStartedError()
	}
	coordinator, operationCtx, release, err := g.runtime.acquire(ctx)
	if err != nil {
		return interaction.CommandResult{}, err
	}
	defer release()
	command, ok := g.runtime.command(invocation.Key)
	if !ok {
		return interaction.CommandResult{}, agent.NewCodedError(agent.ErrorNotFound, agent.CodeCommandNotFound, "gateway.invoke_command", "interaction command is not installed", nil)
	}
	actions := &commandActions{coordinator: coordinator, scope: invocation.Scope, actor: invocation.Actor, open: true}
	defer actions.close()
	return command.Invoke(operationCtx, invocation, actions)
}

func cloneCommandDescriptor(descriptor interaction.CommandDescriptor) interaction.CommandDescriptor {
	cloned := descriptor
	cloned.Fields = append([]interaction.FieldDescriptor(nil), descriptor.Fields...)
	for index := range cloned.Fields {
		cloned.Fields[index].Choices = append([]interaction.Choice(nil), descriptor.Fields[index].Choices...)
	}
	return cloned
}

func (g *gateway) CloseSession(ctx context.Context, request interaction.CloseSessionRequest) (interaction.CloseSessionReceipt, error) {
	return withCoordinator(ctx, g, func(operationCtx context.Context, coordinator *runtimeCoordinator) (interaction.CloseSessionReceipt, error) {
		return coordinator.close(operationCtx, request)
	})
}

type commandActions struct {
	mu          sync.RWMutex
	coordinator *runtimeCoordinator
	scope       interaction.CommandScope
	actor       agent.ActorIdentity
	open        bool
}

func (a *commandActions) close() {
	a.mu.Lock()
	a.open = false
	a.coordinator = nil
	a.scope = interaction.CommandScope{}
	a.actor = agent.ActorIdentity{}
	a.mu.Unlock()
}

func (a *commandActions) Apply(ctx context.Context, request interaction.ActionRequest) (interaction.ActionResult, error) {
	if err := ctx.Err(); err != nil {
		return interaction.ActionResult{}, err
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.open || a.coordinator == nil {
		return interaction.ActionResult{}, closedCommandActionError()
	}
	runtime, err := a.coordinator.runtime(a.scope.SessionID)
	if err != nil {
		return interaction.ActionResult{}, err
	}
	if err := runtime.awaitOpen(ctx); err != nil {
		return interaction.ActionResult{}, err
	}
	switch request.Kind {
	case interaction.ActionSend:
		receipt, err := runtime.send(ctx, interaction.SendRequest{
			SessionID: a.scope.SessionID, ExpectedRevision: request.ExpectedRevision, Actor: a.actor,
			ClientMessageID: request.ClientMessageID, Input: request.Input,
		})
		return interaction.ActionResult{Revision: receipt.Revision, MessageID: receipt.MessageID}, err
	case interaction.ActionSteer:
		receipt, err := runtime.steer(ctx, interaction.SteerRequest{
			SessionID: a.scope.SessionID, ExpectedRevision: request.ExpectedRevision, Actor: a.actor,
			ClientMessageID: request.ClientMessageID, Input: request.Input,
		})
		return interaction.ActionResult{Revision: receipt.Revision, MessageID: receipt.MessageID}, err
	case interaction.ActionRunPending:
		receipt, err := runtime.pending(ctx, interaction.RunPendingRequest{
			SessionID: a.scope.SessionID, ExpectedRevision: request.ExpectedRevision, Actor: a.actor,
		})
		return interaction.ActionResult{Revision: receipt.Revision, RunID: receipt.RunID}, err
	case interaction.ActionCancel:
		err := runtime.cancel(ctx, interaction.CancelRequest{SessionID: a.scope.SessionID, ExpectedRevision: request.ExpectedRevision, Actor: a.actor})
		return interaction.ActionResult{}, err
	case interaction.ActionUpdateModelConfig:
		receipt, err := runtime.updateModelConfig(ctx, interaction.UpdateModelConfigRequest{
			SessionID:               a.scope.SessionID,
			ExpectedRevision:        request.ExpectedRevision,
			Actor:                   a.actor,
			Config:                  request.Config,
			AcceptCompatibilityLoss: request.AcceptCompatibilityLoss,
		})
		return interaction.ActionResult{Revision: receipt.Revision}, err
	default:
		return interaction.ActionResult{}, agent.NewError(agent.ErrorInvalidInput, "gateway.command_action", "unsupported command action", nil)
	}
}

func (a *commandActions) CurrentModelConfig(ctx context.Context) (interaction.ModelConfigView, error) {
	if err := ctx.Err(); err != nil {
		return interaction.ModelConfigView{}, err
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.open || a.coordinator == nil {
		return interaction.ModelConfigView{}, closedCommandActionError()
	}
	runtime, err := a.coordinator.runtime(a.scope.SessionID)
	if err != nil {
		return interaction.ModelConfigView{}, err
	}
	if err := runtime.awaitOpen(ctx); err != nil {
		return interaction.ModelConfigView{}, err
	}
	return runtime.modelConfig(ctx, interaction.ModelConfigRequest{SessionID: a.scope.SessionID})
}

func (a *commandActions) AvailableModels(ctx context.Context) ([]model.Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.open || a.coordinator == nil {
		return nil, closedCommandActionError()
	}
	return a.coordinator.models(ctx)
}

func closedCommandActionError() error {
	return agent.NewCodedError(agent.ErrorConflict, agent.CodeRuntimeClosed, "gateway.command_action", "command action scope is closed", nil)
}

func runtimeUnavailable(op string) error {
	return agent.NewCodedError(agent.ErrorUnavailable, agent.CodeRuntimeUnavailable, op, "Runtime capability is unavailable", nil)
}

var (
	_ interaction.GatewayAccess  = (*gateway)(nil)
	_ interaction.CommandActions = (*commandActions)(nil)
)
