package standardagent

import (
	"context"
	"sort"
	"sync"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
)

// gateway is the fixed, application-scoped user-operation boundary. It owns
// no Session truth: every Session operation is routed through the private
// runtimeAccess selected by runtimeCoordinator.
type gateway struct {
	runtime *applicationRuntime
}

func withCoordinator[T any](ctx context.Context, g *gateway, operation func(*runtimeCoordinator) (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if g == nil || g.runtime == nil {
		return zero, notStartedError()
	}
	coordinator, release, err := g.runtime.acquire()
	if err != nil {
		return zero, err
	}
	defer release()
	return operation(coordinator)
}

func withRuntime[T any](ctx context.Context, g *gateway, id agent.SessionID, operation func(runtimeAccess) (T, error)) (T, error) {
	return withCoordinator(ctx, g, func(coordinator *runtimeCoordinator) (T, error) {
		runtime, err := coordinator.runtime(id)
		if err != nil {
			var zero T
			return zero, err
		}
		return operation(runtime)
	})
}

func (g *gateway) ListSessions(ctx context.Context, request interaction.ListSessionsRequest) (interaction.SessionList, error) {
	return withCoordinator(ctx, g, func(*runtimeCoordinator) (interaction.SessionList, error) {
		// The authoritative list is a persisted Session query, not a projection
		// of currently open Runtime entries. Session persistence arrives in the
		// next implementation round, so this skeleton must not return a lie.
		return interaction.SessionList{}, runtimeUnavailable("gateway.list_sessions")
	})
}

func (g *gateway) CreateSession(ctx context.Context, request interaction.CreateSessionRequest) (interaction.SessionOpened, error) {
	return withCoordinator(ctx, g, func(coordinator *runtimeCoordinator) (interaction.SessionOpened, error) {
		return coordinator.create(ctx, request)
	})
}

func (g *gateway) ResumeSession(ctx context.Context, request interaction.ResumeSessionRequest) (interaction.SessionOpened, error) {
	return withCoordinator(ctx, g, func(coordinator *runtimeCoordinator) (interaction.SessionOpened, error) {
		return coordinator.resume(ctx, request)
	})
}

func (g *gateway) ForkSession(ctx context.Context, request interaction.ForkSessionRequest) (interaction.SessionOpened, error) {
	return withCoordinator(ctx, g, func(coordinator *runtimeCoordinator) (interaction.SessionOpened, error) {
		return coordinator.fork(ctx, request)
	})
}

func (g *gateway) StartSessionFromSummary(ctx context.Context, request interaction.SummarySessionRequest) (interaction.SessionOpened, error) {
	return withCoordinator(ctx, g, func(coordinator *runtimeCoordinator) (interaction.SessionOpened, error) {
		return coordinator.summary(ctx, request)
	})
}

func (g *gateway) Send(ctx context.Context, request interaction.SendRequest) (interaction.EnqueueReceipt, error) {
	return withRuntime(ctx, g, request.SessionID, func(runtime runtimeAccess) (interaction.EnqueueReceipt, error) {
		return runtime.send(ctx, request)
	})
}

func (g *gateway) Steer(ctx context.Context, request interaction.SteerRequest) (interaction.EnqueueReceipt, error) {
	return withRuntime(ctx, g, request.SessionID, func(runtime runtimeAccess) (interaction.EnqueueReceipt, error) {
		return runtime.steer(ctx, request)
	})
}

func (g *gateway) RunPending(ctx context.Context, request interaction.RunPendingRequest) (interaction.RunReceipt, error) {
	return withRuntime(ctx, g, request.SessionID, func(runtime runtimeAccess) (interaction.RunReceipt, error) {
		return runtime.pending(ctx, request)
	})
}

func (g *gateway) Cancel(ctx context.Context, request interaction.CancelRequest) error {
	_, err := withRuntime(ctx, g, request.SessionID, func(runtime runtimeAccess) (struct{}, error) {
		return struct{}{}, runtime.cancel(ctx, request)
	})
	return err
}

func (g *gateway) WhenIdle(ctx context.Context, request interaction.WhenIdleRequest) error {
	_, err := withRuntime(ctx, g, request.SessionID, func(runtime runtimeAccess) (struct{}, error) {
		return struct{}{}, runtime.idle(ctx, request)
	})
	return err
}

func (g *gateway) EditQueued(ctx context.Context, request interaction.EditQueuedRequest) (interaction.CommitReceipt, error) {
	return withRuntime(ctx, g, request.SessionID, func(runtime runtimeAccess) (interaction.CommitReceipt, error) {
		return runtime.editQueued(ctx, request)
	})
}

func (g *gateway) DeleteQueued(ctx context.Context, request interaction.DeleteQueuedRequest) (interaction.CommitReceipt, error) {
	return withRuntime(ctx, g, request.SessionID, func(runtime runtimeAccess) (interaction.CommitReceipt, error) {
		return runtime.deleteQueued(ctx, request)
	})
}

func (g *gateway) ReclassifyQueued(ctx context.Context, request interaction.ReclassifyQueuedRequest) (interaction.CommitReceipt, error) {
	return withRuntime(ctx, g, request.SessionID, func(runtime runtimeAccess) (interaction.CommitReceipt, error) {
		return runtime.reclassifyQueued(ctx, request)
	})
}

func (g *gateway) ModelConfig(ctx context.Context, request interaction.ModelConfigRequest) (interaction.ModelConfigView, error) {
	return withRuntime(ctx, g, request.SessionID, func(runtime runtimeAccess) (interaction.ModelConfigView, error) {
		return runtime.modelConfig(ctx, request)
	})
}

func (g *gateway) UpdateModelConfig(ctx context.Context, request interaction.UpdateModelConfigRequest) (interaction.CommitReceipt, error) {
	return withRuntime(ctx, g, request.SessionID, func(runtime runtimeAccess) (interaction.CommitReceipt, error) {
		return runtime.updateModelConfig(ctx, request)
	})
}

func (g *gateway) Snapshot(ctx context.Context, request interaction.SnapshotRequest) (interaction.SessionSnapshot, error) {
	return withRuntime(ctx, g, request.SessionID, func(runtime runtimeAccess) (interaction.SessionSnapshot, error) {
		return runtime.snapshot(ctx, request)
	})
}

func (g *gateway) Subscribe(ctx context.Context, request interaction.SubscribeRequest) (interaction.EventStream, error) {
	return withRuntime(ctx, g, request.SessionID, func(runtime runtimeAccess) (interaction.EventStream, error) {
		return runtime.subscribe(ctx, request)
	})
}

func (g *gateway) Commands(ctx context.Context, _ interaction.CommandScope) ([]interaction.CommandDescriptor, error) {
	return withCoordinator(ctx, g, func(*runtimeCoordinator) ([]interaction.CommandDescriptor, error) {
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
	coordinator, release, err := g.runtime.acquire()
	if err != nil {
		return interaction.CommandResult{}, err
	}
	defer release()
	command, ok := g.runtime.command(invocation.Key)
	if !ok {
		return interaction.CommandResult{}, agent.NewCodedError(agent.ErrorNotFound, agent.CodeCommandNotFound, "gateway.invoke_command", "interaction command is not installed", nil)
	}
	actions := &commandActions{coordinator: coordinator, scope: invocation.Scope, open: true}
	defer actions.close()
	return command.Invoke(ctx, invocation, actions)
}

func cloneCommandDescriptor(descriptor interaction.CommandDescriptor) interaction.CommandDescriptor {
	cloned := descriptor
	cloned.Fields = append([]interaction.FieldDescriptor(nil), descriptor.Fields...)
	for index := range cloned.Fields {
		cloned.Fields[index].Choices = append([]interaction.Choice(nil), descriptor.Fields[index].Choices...)
	}
	return cloned
}

func (g *gateway) CloseSession(ctx context.Context, request interaction.CloseSessionRequest) error {
	_, err := withCoordinator(ctx, g, func(coordinator *runtimeCoordinator) (struct{}, error) {
		return struct{}{}, coordinator.close(ctx, request.SessionID)
	})
	return err
}

type commandActions struct {
	mu          sync.RWMutex
	coordinator *runtimeCoordinator
	scope       interaction.CommandScope
	open        bool
}

func (a *commandActions) close() {
	a.mu.Lock()
	a.open = false
	a.coordinator = nil
	a.scope = interaction.CommandScope{}
	a.mu.Unlock()
}

func (a *commandActions) Apply(ctx context.Context, request interaction.ActionRequest) (interaction.ActionResult, error) {
	if err := ctx.Err(); err != nil {
		return interaction.ActionResult{}, err
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.open || a.coordinator == nil {
		return interaction.ActionResult{}, agent.NewCodedError(agent.ErrorConflict, agent.CodeRuntimeClosed, "gateway.command_action", "command action scope is closed", nil)
	}
	runtime, err := a.coordinator.runtime(a.scope.SessionID)
	if err != nil {
		return interaction.ActionResult{}, err
	}
	switch request.Kind {
	case interaction.ActionSend:
		receipt, err := runtime.send(ctx, interaction.SendRequest{
			SessionID: a.scope.SessionID, ExpectedRevision: request.ExpectedRevision, Input: request.Input,
		})
		return interaction.ActionResult{Revision: receipt.Revision, MessageID: receipt.MessageID}, err
	case interaction.ActionSteer:
		receipt, err := runtime.steer(ctx, interaction.SteerRequest{
			SessionID: a.scope.SessionID, ExpectedRevision: request.ExpectedRevision, Input: request.Input,
		})
		return interaction.ActionResult{Revision: receipt.Revision, MessageID: receipt.MessageID}, err
	case interaction.ActionRunPending:
		receipt, err := runtime.pending(ctx, interaction.RunPendingRequest{SessionID: a.scope.SessionID})
		return interaction.ActionResult{Revision: receipt.Revision, RunID: receipt.RunID}, err
	case interaction.ActionCancel:
		err := runtime.cancel(ctx, interaction.CancelRequest{SessionID: a.scope.SessionID})
		return interaction.ActionResult{}, err
	case interaction.ActionUpdateModelConfig:
		receipt, err := runtime.updateModelConfig(ctx, interaction.UpdateModelConfigRequest{
			SessionID:               a.scope.SessionID,
			ExpectedRevision:        request.ExpectedRevision,
			Config:                  request.Config,
			AcceptCompatibilityLoss: request.AcceptCompatibilityLoss,
		})
		return interaction.ActionResult{Revision: receipt.Revision}, err
	default:
		return interaction.ActionResult{}, agent.NewError(agent.ErrorInvalidInput, "gateway.command_action", "unsupported command action", nil)
	}
}

func runtimeUnavailable(op string) error {
	return agent.NewCodedError(agent.ErrorUnavailable, agent.CodeRuntimeUnavailable, op, "Runtime capability is unavailable", nil)
}

var (
	_ interaction.GatewayAccess  = (*gateway)(nil)
	_ interaction.CommandActions = (*commandActions)(nil)
)
