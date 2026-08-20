package standardagent

import (
	"context"
	"time"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/observe"
	"github.com/LyleLiu666/agentSlot/session"
)

func (r *runtimeInstance) send(ctx context.Context, request interaction.SendRequest) (interaction.EnqueueReceipt, error) {
	if request.SessionID != r.id() || !request.Input.Valid() {
		return interaction.EnqueueReceipt{}, invalidInput("gateway.send", "SessionID and message input are required")
	}
	r.mu.Lock()
	if err := r.ensureOpenLocked("gateway.send"); err != nil {
		r.mu.Unlock()
		return interaction.EnqueueReceipt{}, err
	}
	snapshot, err := r.viewLocked(ctx)
	if err != nil {
		r.mu.Unlock()
		return interaction.EnqueueReceipt{}, err
	}
	message := agent.Message{
		ID: agent.MessageID(r.nextID("message")), SessionID: r.id(), Role: agent.RoleUser,
		Parts: cloneRuntimeParts(request.Input.Parts), CreatedAt: time.Now().UTC(),
	}
	item := session.QueueItem{Message: message, Delivery: session.DeliveryNormal}
	if r.state == runtimeRunning {
		commit, err := r.commitLocked(ctx, request.ExpectedRevision, "send", []session.Change{{Kind: session.EnqueueMessage, QueueItem: &item}})
		r.mu.Unlock()
		if err != nil {
			return interaction.EnqueueReceipt{}, err
		}
		return interaction.EnqueueReceipt{MessageID: message.ID, Revision: commit.Revision}, nil
	}
	run, step, changes := r.startChangesLocked(snapshot, item, true)
	_, err = r.commitLocked(ctx, request.ExpectedRevision, "send-start", changes)
	if err != nil {
		run.cancel()
		r.mu.Unlock()
		return interaction.EnqueueReceipt{}, err
	}
	r.activateLocked(run)
	go r.runLoop(run, step)
	r.mu.Unlock()
	select {
	case <-run.prepared:
		return interaction.EnqueueReceipt{MessageID: message.ID, Revision: run.prepareRevision}, nil
	case <-ctx.Done():
		return interaction.EnqueueReceipt{}, ctx.Err()
	}
}

func (r *runtimeInstance) steer(ctx context.Context, request interaction.SteerRequest) (interaction.EnqueueReceipt, error) {
	if request.SessionID != r.id() || !request.Input.Valid() {
		return interaction.EnqueueReceipt{}, invalidInput("gateway.steer", "SessionID and message input are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureOpenLocked("gateway.steer"); err != nil {
		return interaction.EnqueueReceipt{}, err
	}
	if r.state != runtimeRunning || r.active == nil {
		return interaction.EnqueueReceipt{}, agent.NewCodedError(agent.ErrorConflict, agent.CodeNoActiveRun, "gateway.steer", "session has no active Run", nil)
	}
	message := agent.Message{
		ID: agent.MessageID(r.nextID("message")), SessionID: r.id(), Role: agent.RoleUser,
		Parts: cloneRuntimeParts(request.Input.Parts), CreatedAt: time.Now().UTC(),
	}
	item := session.QueueItem{Message: message, Delivery: session.DeliverySteer}
	commit, err := r.commitLocked(ctx, request.ExpectedRevision, "steer", []session.Change{{Kind: session.EnqueueMessage, QueueItem: &item}})
	if err != nil {
		return interaction.EnqueueReceipt{}, err
	}
	return interaction.EnqueueReceipt{MessageID: message.ID, Revision: commit.Revision}, nil
}

func (r *runtimeInstance) pending(ctx context.Context, request interaction.RunPendingRequest) (interaction.RunReceipt, error) {
	if request.SessionID != r.id() {
		return interaction.RunReceipt{}, invalidInput("gateway.run_pending", "SessionID is required")
	}
	r.mu.Lock()
	if err := r.ensureOpenLocked("gateway.run_pending"); err != nil {
		r.mu.Unlock()
		return interaction.RunReceipt{}, err
	}
	if r.state == runtimeRunning {
		r.mu.Unlock()
		return interaction.RunReceipt{}, agent.NewCodedError(agent.ErrorConflict, agent.CodeActiveRun, "gateway.run_pending", "session already has an active Run", nil)
	}
	snapshot, err := r.viewLocked(ctx)
	if err != nil {
		r.mu.Unlock()
		return interaction.RunReceipt{}, err
	}
	item, ok := firstPending(snapshot.Queue, session.DeliveryNormal)
	if !ok {
		r.mu.Unlock()
		return interaction.RunReceipt{}, agent.NewCodedError(agent.ErrorConflict, agent.CodeNoPendingWork, "gateway.run_pending", "session has no pending normal input", nil)
	}
	run, step, changes := r.startChangesLocked(snapshot, item, false)
	_, err = r.commitLocked(ctx, request.ExpectedRevision, "run-pending", changes)
	if err != nil {
		run.cancel()
		r.mu.Unlock()
		return interaction.RunReceipt{}, err
	}
	r.activateLocked(run)
	go r.runLoop(run, step)
	r.mu.Unlock()
	select {
	case <-run.prepared:
		return interaction.RunReceipt{SessionID: r.id(), RunID: run.id, Revision: run.prepareRevision}, nil
	case <-ctx.Done():
		return interaction.RunReceipt{}, ctx.Err()
	}
}

func (r *runtimeInstance) cancel(_ context.Context, request interaction.CancelRequest) error {
	if request.SessionID != r.id() {
		return invalidInput("gateway.cancel", "SessionID is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureOpenLocked("gateway.cancel"); err != nil {
		return err
	}
	if r.state == runtimeIdle || r.active == nil {
		return nil
	}
	r.active.cancelRequested = true
	r.active.cancel()
	return nil
}

func (r *runtimeInstance) idle(ctx context.Context, request interaction.WhenIdleRequest) error {
	if request.SessionID != r.id() {
		return invalidInput("gateway.when_idle", "SessionID is required")
	}
	for {
		r.mu.Lock()
		if r.state == runtimeClosed {
			r.mu.Unlock()
			return nil
		}
		if r.state == runtimeIdle {
			r.mu.Unlock()
			return nil
		}
		signal := r.idleSignal
		if r.closing {
			signal = r.closeDone
		}
		r.mu.Unlock()
		select {
		case <-signal:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (r *runtimeInstance) close(ctx context.Context) error {
	r.mu.Lock()
	if r.state == runtimeClosed {
		done := r.closeDone
		r.mu.Unlock()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if !r.closing {
		r.closing = true
		var runDone <-chan struct{}
		if r.active != nil {
			r.active.cancelRequested = true
			r.active.cancel()
			runDone = r.active.done
		}
		go r.finishClose(runDone)
	}
	done := r.closeDone
	r.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *runtimeInstance) finishClose(runDone <-chan struct{}) {
	if runDone != nil {
		<-runDone
	}
	r.observer.stop()
	r.events.close()
	r.components.observations.publishTrace(observe.TraceRecord{
		Kind: observe.TraceRuntimeClosed, At: time.Now().UTC(), Identity: observe.Identity{SessionID: r.id()},
	})
	r.mu.Lock()
	if r.state != runtimeClosed {
		r.state = runtimeClosed
		r.active = nil
		close(r.closeDone)
	}
	r.mu.Unlock()
}

func (r *runtimeInstance) modelConfig(ctx context.Context, request interaction.ModelConfigRequest) (interaction.ModelConfigView, error) {
	if request.SessionID != r.id() {
		return interaction.ModelConfigView{}, invalidInput("gateway.model_config", "SessionID is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureOpenLocked("gateway.model_config"); err != nil {
		return interaction.ModelConfigView{}, err
	}
	snapshot, err := r.viewLocked(ctx)
	if err != nil {
		return interaction.ModelConfigView{}, err
	}
	return interaction.ModelConfigView{SessionID: r.id(), Revision: snapshot.Revision, Config: cloneRuntimeConfig(snapshot.ModelConfig)}, nil
}

func (r *runtimeInstance) updateModelConfig(ctx context.Context, request interaction.UpdateModelConfigRequest) (interaction.CommitReceipt, error) {
	if request.SessionID != r.id() {
		return interaction.CommitReceipt{}, invalidInput("gateway.update_model_config", "SessionID is required")
	}
	if err := request.Config.Validate(); err != nil {
		return interaction.CommitReceipt{}, invalidInput("gateway.update_model_config", err.Error())
	}
	r.mu.Lock()
	if err := r.ensureOpenLocked("gateway.update_model_config"); err != nil {
		r.mu.Unlock()
		return interaction.CommitReceipt{}, err
	}
	if r.state == runtimeRunning {
		r.mu.Unlock()
		return interaction.CommitReceipt{}, agent.NewCodedError(agent.ErrorConflict, agent.CodeActiveRun, "gateway.update_model_config", "model config cannot change while a Run is active", nil)
	}
	r.mu.Unlock()
	// Inspect is a replaceable component call. It must not run under the
	// Runtime mutex or gain the ability to block unrelated control commands.
	capabilities, err := r.inspectModel(ctx, request.Config)
	if err != nil {
		return interaction.CommitReceipt{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureOpenLocked("gateway.update_model_config"); err != nil {
		return interaction.CommitReceipt{}, err
	}
	if r.state == runtimeRunning {
		return interaction.CommitReceipt{}, agent.NewCodedError(agent.ErrorConflict, agent.CodeActiveRun, "gateway.update_model_config", "model config cannot change while a Run is active", nil)
	}
	snapshot, err := r.viewLocked(ctx)
	if err != nil {
		return interaction.CommitReceipt{}, err
	}
	warnings := modelCompatibilityWarnings(snapshot.History, capabilities.Media)
	if len(warnings) > 0 && !request.AcceptCompatibilityLoss {
		compatibility := &model.CompatibilityError{Warnings: warnings}
		return interaction.CommitReceipt{}, agent.NewCodedError(agent.ErrorConflict, agent.CodeCompatibilityConfirmationRequired, "gateway.update_model_config", "target model cannot directly consume all persisted attachments", compatibility)
	}
	config := cloneRuntimeConfig(request.Config)
	change := session.ModelConfigChange{Previous: cloneRuntimeConfig(snapshot.ModelConfig), Current: cloneRuntimeConfig(config)}
	event := session.SessionEvent{Kind: session.EventModelConfigChanged, ModelConfigChanged: &change}
	commit, err := r.commitLocked(ctx, request.ExpectedRevision, "model-config", []session.Change{
		{Kind: session.SetModelConfig, ModelConfig: &config},
		{Kind: session.AppendSessionEvent, SessionEvent: &event},
	})
	if err != nil {
		return interaction.CommitReceipt{}, err
	}
	return interaction.CommitReceipt{SessionID: r.id(), Revision: commit.Revision}, nil
}

func modelCompatibilityWarnings(history []session.HistoryFact, capabilities model.Capabilities) []model.CompatibilityWarning {
	counts := make(map[model.Modality]int)
	for _, fact := range history {
		if fact.Message == nil {
			continue
		}
		for _, part := range fact.Message.Parts {
			if part.Kind != agent.PartAttachment {
				continue
			}
			modality := attachmentModality(part.MediaType)
			if modality.Valid() && !capabilities.SupportsInput(modality) {
				counts[modality]++
			}
		}
	}
	warnings := make([]model.CompatibilityWarning, 0, len(counts))
	for _, modality := range model.AllModalities() {
		if count := counts[modality]; count > 0 {
			warnings = append(warnings, model.CompatibilityWarning{Modality: modality, Count: count})
		}
	}
	return warnings
}

func (r *runtimeInstance) editQueued(ctx context.Context, request interaction.EditQueuedRequest) (interaction.CommitReceipt, error) {
	if request.SessionID != r.id() || !request.MessageID.Valid() || !request.Input.Valid() {
		return interaction.CommitReceipt{}, invalidInput("gateway.edit_queued", "SessionID, MessageID, and input are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureOpenLocked("gateway.edit_queued"); err != nil {
		return interaction.CommitReceipt{}, err
	}
	snapshot, err := r.viewLocked(ctx)
	if err != nil {
		return interaction.CommitReceipt{}, err
	}
	item, ok := queueByID(snapshot.Queue, request.MessageID)
	if !ok {
		return interaction.CommitReceipt{}, agent.NewCodedError(agent.ErrorNotFound, agent.CodeQueueItemNotFound, "gateway.edit_queued", "queue item was not found", nil)
	}
	edit := session.QueueEdit{MessageID: request.MessageID, Input: request.Input, Delivery: item.Delivery}
	commit, err := r.commitLocked(ctx, request.ExpectedRevision, "queue-edit", []session.Change{{Kind: session.EditQueue, QueueEdit: &edit}})
	if err != nil {
		return interaction.CommitReceipt{}, err
	}
	return interaction.CommitReceipt{SessionID: r.id(), Revision: commit.Revision}, nil
}

func (r *runtimeInstance) deleteQueued(ctx context.Context, request interaction.DeleteQueuedRequest) (interaction.CommitReceipt, error) {
	if request.SessionID != r.id() || !request.MessageID.Valid() {
		return interaction.CommitReceipt{}, invalidInput("gateway.delete_queued", "SessionID and MessageID are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureOpenLocked("gateway.delete_queued"); err != nil {
		return interaction.CommitReceipt{}, err
	}
	change := session.QueueDelete{MessageID: request.MessageID}
	commit, err := r.commitLocked(ctx, request.ExpectedRevision, "queue-delete", []session.Change{{Kind: session.DeleteQueue, QueueDelete: &change}})
	if err != nil {
		return interaction.CommitReceipt{}, err
	}
	return interaction.CommitReceipt{SessionID: r.id(), Revision: commit.Revision}, nil
}

func (r *runtimeInstance) reclassifyQueued(ctx context.Context, request interaction.ReclassifyQueuedRequest) (interaction.CommitReceipt, error) {
	if request.SessionID != r.id() || !request.MessageID.Valid() || !request.Delivery.Valid() {
		return interaction.CommitReceipt{}, invalidInput("gateway.reclassify_queued", "SessionID, MessageID, and delivery are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureOpenLocked("gateway.reclassify_queued"); err != nil {
		return interaction.CommitReceipt{}, err
	}
	change := session.QueueReclassify{MessageID: request.MessageID, Delivery: request.Delivery}
	commit, err := r.commitLocked(ctx, request.ExpectedRevision, "queue-reclassify", []session.Change{{Kind: session.ReclassifyQueue, QueueReclassification: &change}})
	if err != nil {
		return interaction.CommitReceipt{}, err
	}
	return interaction.CommitReceipt{SessionID: r.id(), Revision: commit.Revision}, nil
}
