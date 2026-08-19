package standardagent

import (
	"context"
	"sync"

	"github.com/LyleLiu666/agentSlot/interaction"
)

// gatewayBinding is the stable capability attached to Entrypoints during
// Build. Start atomically binds the live Gateway and Stop removes it, so an
// adapter cannot accidentally operate on a half-started application.
type gatewayBinding struct {
	mu     sync.RWMutex
	target interaction.GatewayAccess
}

func (b *gatewayBinding) bind(target interaction.GatewayAccess) {
	b.mu.Lock()
	b.target = target
	b.mu.Unlock()
}

func (b *gatewayBinding) unbind() {
	b.mu.Lock()
	b.target = nil
	b.mu.Unlock()
}

func (b *gatewayBinding) access() (interaction.GatewayAccess, error) {
	b.mu.RLock()
	target := b.target
	b.mu.RUnlock()
	if target == nil {
		return nil, notStartedError()
	}
	return target, nil
}

func (b *gatewayBinding) ListSessions(ctx context.Context, request interaction.ListSessionsRequest) (interaction.SessionList, error) {
	target, err := b.access()
	if err != nil {
		return interaction.SessionList{}, err
	}
	return target.ListSessions(ctx, request)
}

func (b *gatewayBinding) CreateSession(ctx context.Context, request interaction.CreateSessionRequest) (interaction.SessionOpened, error) {
	target, err := b.access()
	if err != nil {
		return interaction.SessionOpened{}, err
	}
	return target.CreateSession(ctx, request)
}

func (b *gatewayBinding) ResumeSession(ctx context.Context, request interaction.ResumeSessionRequest) (interaction.SessionOpened, error) {
	target, err := b.access()
	if err != nil {
		return interaction.SessionOpened{}, err
	}
	return target.ResumeSession(ctx, request)
}

func (b *gatewayBinding) ForkSession(ctx context.Context, request interaction.ForkSessionRequest) (interaction.SessionOpened, error) {
	target, err := b.access()
	if err != nil {
		return interaction.SessionOpened{}, err
	}
	return target.ForkSession(ctx, request)
}

func (b *gatewayBinding) StartSessionFromSummary(ctx context.Context, request interaction.SummarySessionRequest) (interaction.SessionOpened, error) {
	target, err := b.access()
	if err != nil {
		return interaction.SessionOpened{}, err
	}
	return target.StartSessionFromSummary(ctx, request)
}

func (b *gatewayBinding) Send(ctx context.Context, request interaction.SendRequest) (interaction.EnqueueReceipt, error) {
	target, err := b.access()
	if err != nil {
		return interaction.EnqueueReceipt{}, err
	}
	return target.Send(ctx, request)
}

func (b *gatewayBinding) Steer(ctx context.Context, request interaction.SteerRequest) (interaction.EnqueueReceipt, error) {
	target, err := b.access()
	if err != nil {
		return interaction.EnqueueReceipt{}, err
	}
	return target.Steer(ctx, request)
}

func (b *gatewayBinding) RunPending(ctx context.Context, request interaction.RunPendingRequest) (interaction.RunReceipt, error) {
	target, err := b.access()
	if err != nil {
		return interaction.RunReceipt{}, err
	}
	return target.RunPending(ctx, request)
}

func (b *gatewayBinding) Cancel(ctx context.Context, request interaction.CancelRequest) error {
	target, err := b.access()
	if err != nil {
		return err
	}
	return target.Cancel(ctx, request)
}

func (b *gatewayBinding) WhenIdle(ctx context.Context, request interaction.WhenIdleRequest) error {
	target, err := b.access()
	if err != nil {
		return err
	}
	return target.WhenIdle(ctx, request)
}

func (b *gatewayBinding) EditQueued(ctx context.Context, request interaction.EditQueuedRequest) (interaction.CommitReceipt, error) {
	target, err := b.access()
	if err != nil {
		return interaction.CommitReceipt{}, err
	}
	return target.EditQueued(ctx, request)
}

func (b *gatewayBinding) DeleteQueued(ctx context.Context, request interaction.DeleteQueuedRequest) (interaction.CommitReceipt, error) {
	target, err := b.access()
	if err != nil {
		return interaction.CommitReceipt{}, err
	}
	return target.DeleteQueued(ctx, request)
}

func (b *gatewayBinding) ReclassifyQueued(ctx context.Context, request interaction.ReclassifyQueuedRequest) (interaction.CommitReceipt, error) {
	target, err := b.access()
	if err != nil {
		return interaction.CommitReceipt{}, err
	}
	return target.ReclassifyQueued(ctx, request)
}

func (b *gatewayBinding) ModelConfig(ctx context.Context, request interaction.ModelConfigRequest) (interaction.ModelConfigView, error) {
	target, err := b.access()
	if err != nil {
		return interaction.ModelConfigView{}, err
	}
	return target.ModelConfig(ctx, request)
}

func (b *gatewayBinding) UpdateModelConfig(ctx context.Context, request interaction.UpdateModelConfigRequest) (interaction.CommitReceipt, error) {
	target, err := b.access()
	if err != nil {
		return interaction.CommitReceipt{}, err
	}
	return target.UpdateModelConfig(ctx, request)
}

func (b *gatewayBinding) Snapshot(ctx context.Context, request interaction.SnapshotRequest) (interaction.SessionSnapshot, error) {
	target, err := b.access()
	if err != nil {
		return interaction.SessionSnapshot{}, err
	}
	return target.Snapshot(ctx, request)
}

func (b *gatewayBinding) Subscribe(ctx context.Context, request interaction.SubscribeRequest) (interaction.EventStream, error) {
	target, err := b.access()
	if err != nil {
		return nil, err
	}
	return target.Subscribe(ctx, request)
}

func (b *gatewayBinding) Commands(ctx context.Context, scope interaction.CommandScope) ([]interaction.CommandDescriptor, error) {
	target, err := b.access()
	if err != nil {
		return nil, err
	}
	return target.Commands(ctx, scope)
}

func (b *gatewayBinding) InvokeCommand(ctx context.Context, invocation interaction.CommandInvocation) (interaction.CommandResult, error) {
	target, err := b.access()
	if err != nil {
		return interaction.CommandResult{}, err
	}
	return target.InvokeCommand(ctx, invocation)
}

func (b *gatewayBinding) CloseSession(ctx context.Context, request interaction.CloseSessionRequest) error {
	target, err := b.access()
	if err != nil {
		return err
	}
	return target.CloseSession(ctx, request)
}

var _ interaction.GatewayAccess = (*gatewayBinding)(nil)
