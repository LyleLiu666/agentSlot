package standardagent

import (
	"context"
	"errors"
	"time"

	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

type inputGateBinding struct {
	gate       hook.InputGate
	descriptor hook.ExtensionDescriptor
}

type inputGateOccurrence struct {
	message             agent.Message
	input               agent.MessageInput
	previousInputDigest string
	originalRunID       agent.RunID
	originalDelivery    session.Delivery
	actor               agent.ActorIdentity
	entries             []session.ExtensionJournalEntry
	views               []hook.InputGateView
}

func (r *runtimeInstance) sendThroughInputGates(ctx context.Context, request interaction.SendRequest) (interaction.EnqueueReceipt, error) {
	r.submissionMu.Lock()
	locked := true
	defer func() {
		if locked {
			r.submissionMu.Unlock()
		}
	}()

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
		ID: agent.MessageID(r.nextID("message")), ClientMessageID: request.ClientMessageID, SessionID: r.id(), Role: agent.RoleUser,
		Parts: cloneRuntimeParts(request.Input.Parts), CreatedAt: time.Now().UTC(),
	}
	occurrence, err := r.prepareInputGateLocked(ctx, snapshot, request.ExpectedRevision, request.Actor, hook.InputSend, message, "", "")
	r.mu.Unlock()
	if err != nil {
		return interaction.EnqueueReceipt{}, err
	}
	if err := r.executeInputGateOccurrence(ctx, occurrence); err != nil {
		return interaction.EnqueueReceipt{}, r.finalizeRejectedInput(occurrence, err)
	}
	receipt, run, err := r.finalizeAcceptedSend(ctx, occurrence)
	r.submissionMu.Unlock()
	locked = false
	if err != nil || run == nil {
		return receipt, err
	}
	select {
	case <-run.prepared:
		receipt.Revision = run.prepareRevision
		return receipt, nil
	case <-ctx.Done():
		return interaction.EnqueueReceipt{}, ctx.Err()
	}
}

func (r *runtimeInstance) steerThroughInputGates(ctx context.Context, request interaction.SteerRequest) (interaction.EnqueueReceipt, error) {
	r.submissionMu.Lock()
	defer r.submissionMu.Unlock()

	r.mu.Lock()
	if err := r.ensureOpenLocked("gateway.steer"); err != nil {
		r.mu.Unlock()
		return interaction.EnqueueReceipt{}, err
	}
	if r.state != runtimeRunning || r.active == nil {
		r.mu.Unlock()
		return interaction.EnqueueReceipt{}, agent.NewCodedError(agent.ErrorConflict, agent.CodeNoActiveRun, "gateway.steer", "session has no active Run", nil)
	}
	snapshot, err := r.viewLocked(ctx)
	if err != nil {
		r.mu.Unlock()
		return interaction.EnqueueReceipt{}, err
	}
	message := agent.Message{
		ID: agent.MessageID(r.nextID("message")), ClientMessageID: request.ClientMessageID, SessionID: r.id(), Role: agent.RoleUser,
		Parts: cloneRuntimeParts(request.Input.Parts), CreatedAt: time.Now().UTC(),
	}
	occurrence, err := r.prepareInputGateLocked(ctx, snapshot, request.ExpectedRevision, request.Actor, hook.InputSteer, message, r.active.id, "")
	r.mu.Unlock()
	if err != nil {
		return interaction.EnqueueReceipt{}, err
	}
	if err := r.executeInputGateOccurrence(ctx, occurrence); err != nil {
		return interaction.EnqueueReceipt{}, r.finalizeRejectedInput(occurrence, err)
	}
	return r.finalizeAcceptedSteer(ctx, occurrence)
}

func (r *runtimeInstance) editThroughInputGates(ctx context.Context, request interaction.EditQueuedRequest) (interaction.CommitReceipt, error) {
	r.submissionMu.Lock()
	defer r.submissionMu.Unlock()

	r.mu.Lock()
	if err := r.ensureOpenLocked("gateway.edit_queued"); err != nil {
		r.mu.Unlock()
		return interaction.CommitReceipt{}, err
	}
	snapshot, err := r.viewLocked(ctx)
	if err != nil {
		r.mu.Unlock()
		return interaction.CommitReceipt{}, err
	}
	item, ok := queueByID(snapshot.Queue, request.MessageID)
	if !ok {
		r.mu.Unlock()
		return interaction.CommitReceipt{}, agent.NewCodedError(agent.ErrorNotFound, agent.CodeQueueItemNotFound, "gateway.edit_queued", "queue item was not found", nil)
	}
	previous, err := hook.FingerprintTypedInput(agent.MessageInput{Parts: cloneRuntimeParts(item.Message.Parts)})
	if err != nil {
		r.mu.Unlock()
		return interaction.CommitReceipt{}, agent.NewError(agent.ErrorInternal, "gateway.edit_queued", "queued input could not be fingerprinted", err)
	}
	message := item.Message
	message.Parts = cloneRuntimeParts(request.Input.Parts)
	occurrence, err := r.prepareInputGateLocked(ctx, snapshot, request.ExpectedRevision, request.Actor, hook.InputEditQueued, message, "", previous.Digest)
	if occurrence != nil {
		occurrence.originalDelivery = item.Delivery
	}
	r.mu.Unlock()
	if err != nil {
		return interaction.CommitReceipt{}, err
	}
	if err := r.executeInputGateOccurrence(ctx, occurrence); err != nil {
		return interaction.CommitReceipt{}, r.finalizeRejectedInput(occurrence, err)
	}
	return r.finalizeAcceptedEdit(ctx, occurrence)
}

func (r *runtimeInstance) prepareInputGateLocked(
	ctx context.Context,
	snapshot session.Snapshot,
	expected agent.Revision,
	actor agent.ActorIdentity,
	operation hook.InputOperation,
	message agent.Message,
	originalRunID agent.RunID,
	previousInputDigest string,
) (*inputGateOccurrence, error) {
	occurrence := &inputGateOccurrence{
		message: cloneRuntimeMessage(message),
		input:   agent.MessageInput{Parts: cloneRuntimeParts(message.Parts)}, previousInputDigest: previousInputDigest,
		originalRunID: originalRunID, actor: actor,
		entries: make([]session.ExtensionJournalEntry, len(r.components.inputGates)),
		views:   make([]hook.InputGateView, len(r.components.inputGates)),
	}
	now := time.Now().UTC()
	changes := make([]session.Change, 0, len(r.components.inputGates))
	for index, binding := range r.components.inputGates {
		view := hook.InputGateView{
			InvocationID: hook.InvocationID(r.nextID("extension")), Operation: operation,
			SessionID: r.id(), AgentID: r.agentID, WorkspaceID: r.workspaceID, Revision: expected.Next(),
			MessageID: message.ID, ClientMessageID: message.ClientMessageID,
			Input: agent.MessageInput{Parts: cloneRuntimeParts(message.Parts)}, PreviousInputDigest: previousInputDigest,
		}
		if err := view.Validate(); err != nil {
			return nil, agent.NewError(agent.ErrorInternal, "standardagent.input_gate", "Runtime constructed an invalid input gate view", err)
		}
		fingerprint, err := hook.FingerprintTypedInput(view)
		if err != nil {
			return nil, agent.NewError(agent.ErrorInternal, "standardagent.input_gate", "input gate view could not be fingerprinted", err)
		}
		entry := session.ExtensionJournalEntry{
			InvocationID: view.InvocationID, Sequence: session.ExtensionSequence(len(snapshot.ExtensionJournal) + index + 1),
			Descriptor: binding.descriptor, Boundary: hook.BoundaryInputGate,
			SessionID: r.id(), MessageID: message.ID, InputDigest: fingerprint.Digest, PreparedAt: now,
			Status: hook.InvocationPrepared, EffectDisposition: hook.EffectNone, ContextDisposition: hook.ContextNone,
		}
		occurrence.views[index], occurrence.entries[index] = view, entry
		entryCopy := entry
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
	}
	commit, err := r.commitExternalLocked(ctx, expected, "input-gate-prepare", actor, changes)
	if err != nil {
		return nil, err
	}
	if commit.Revision != expected.Next() {
		return nil, agent.NewError(agent.ErrorInternal, "standardagent.input_gate", "prepared input gate commit returned an unexpected revision", nil)
	}
	r.hasInputGateJournal = true
	return occurrence, nil
}

func (r *runtimeInstance) executeInputGateOccurrence(ctx context.Context, occurrence *inputGateOccurrence) error {
	for index, binding := range r.components.inputGates {
		if err := ctx.Err(); err != nil {
			r.cancelPreparedInputGates(occurrence, index)
			return err
		}
		pending := occurrence.entries[index]
		pending.Status = hook.InvocationPending
		pending.PendingAt = time.Now().UTC()
		if pending.PendingAt.Before(pending.PreparedAt) {
			pending.PendingAt = pending.PreparedAt
		}
		if err := r.commitInputGateEntry(context.WithoutCancel(ctx), occurrence.actor, &pending, "input-gate-pending"); err != nil {
			r.cancelPreparedInputGates(occurrence, index+1)
			return err
		}
		occurrence.entries[index] = pending

		result, failure := evaluateInputGate(ctx, binding.gate, cloneInputGateView(occurrence.views[index]))
		terminal := pending
		terminal.FinishedAt = time.Now().UTC()
		if terminal.FinishedAt.Before(terminal.PendingAt) {
			terminal.FinishedAt = terminal.PendingAt
		}
		terminal.EffectDisposition = hook.EffectPending
		if failure != nil {
			terminal.Status = failure.Status
			terminal.ErrorCode = failure.Code
			terminal.ErrorReason = failure.Reason
		} else {
			terminal.Status = hook.InvocationSucceeded
			terminal.Result = &hook.InvocationResult{Decision: result.Decision, Reason: result.Reason}
			if len(result.Context) > 0 {
				terminal.ContextDisposition = hook.ContextPending
				terminal.ContextInputs = cloneRuntimeInputs(result.Context)
				fingerprint, err := hook.FingerprintTypedInput(terminal.ContextInputs)
				if err != nil {
					return err
				}
				terminal.ContextDigest, terminal.ContextBytes = fingerprint.Digest, fingerprint.Bytes
			}
		}
		if err := r.commitInputGateEntry(context.WithoutCancel(ctx), occurrence.actor, &terminal, "input-gate-terminal"); err != nil {
			r.cancelPreparedInputGates(occurrence, index+1)
			return err
		}
		occurrence.entries[index] = terminal
		if failure != nil {
			r.cancelPreparedInputGates(occurrence, index+1)
			return failure
		}
		if result.Decision == hook.DecisionReject {
			r.cancelPreparedInputGates(occurrence, index+1)
			return agent.NewCodedError(agent.ErrorForbidden, agent.CodeInputRejected, "standardagent.input_gate", "input was rejected", nil)
		}
	}
	return nil
}

func evaluateInputGate(ctx context.Context, gate hook.InputGate, view hook.InputGateView) (result hook.InputGateResult, failure *hook.InvocationFailure) {
	defer func() {
		if recover() != nil {
			result = hook.InputGateResult{}
			failure = &hook.InvocationFailure{
				Status: hook.InvocationFailed, Code: agent.CodeExtensionFailed,
				Reason: "input gate panicked",
			}
		}
	}()
	result, err := gate.Evaluate(ctx, view)
	if ctxErr := ctx.Err(); ctxErr != nil {
		status, code, reason := hook.InvocationCanceled, agent.CodeCanceled, "input gate was canceled"
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			status, code, reason = hook.InvocationFailed, agent.ErrorCode("extension_deadline_exceeded"), "input gate deadline expired"
		}
		return hook.InputGateResult{}, &hook.InvocationFailure{Status: status, Code: code, Reason: reason, Cause: ctxErr}
	}
	if err != nil {
		var declared *hook.InvocationFailure
		if errors.As(err, &declared) && declared.Validate() == nil {
			copy := *declared
			return hook.InputGateResult{}, &copy
		}
		return hook.InputGateResult{}, &hook.InvocationFailure{
			Status: hook.InvocationFailed, Code: agent.CodeExtensionFailed,
			Reason: "input gate component failed", Cause: err,
		}
	}
	if err := result.Validate(view.SessionID); err != nil {
		return hook.InputGateResult{}, &hook.InvocationFailure{
			Status: hook.InvocationFailed, Code: agent.CodeExtensionFailed,
			Reason: "input gate returned an invalid result", Cause: err,
		}
	}
	proposal := agent.Message{
		ID: view.MessageID, ClientMessageID: view.ClientMessageID, SessionID: view.SessionID, Role: agent.RoleUser,
		Parts: cloneRuntimeParts(view.Input.Parts),
	}
	candidate := make([]model.Input, 0, len(result.Context)+1)
	candidate = append(candidate, model.Input{Message: &proposal})
	candidate = append(candidate, cloneRuntimeInputs(result.Context)...)
	if err := model.ValidateInputs(candidate); err != nil {
		return hook.InputGateResult{}, &hook.InvocationFailure{
			Status: hook.InvocationFailed, Code: agent.CodeExtensionFailed,
			Reason: "input gate context conflicts with the proposed input", Cause: err,
		}
	}
	return result, nil
}

func (r *runtimeInstance) cancelPreparedInputGates(occurrence *inputGateOccurrence, start int) {
	type update struct {
		index int
		entry session.ExtensionJournalEntry
	}
	now := time.Now().UTC()
	updates := make([]update, 0, len(occurrence.entries)-start)
	changes := make([]session.Change, 0, len(occurrence.entries)-start)
	for index := start; index < len(occurrence.entries); index++ {
		entry := occurrence.entries[index]
		if entry.Status != hook.InvocationPrepared {
			continue
		}
		entry.Status = hook.InvocationCanceled
		entry.FinishedAt = now
		if entry.FinishedAt.Before(entry.PreparedAt) {
			entry.FinishedAt = entry.PreparedAt
		}
		entry.ErrorCode = agent.CodeCanceled
		entry.ErrorReason = "input gate was not started"
		entry.EffectDisposition = hook.EffectDiscarded
		entryCopy := entry
		updates = append(updates, update{index: index, entry: entry})
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
	}
	if len(changes) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureOpenLocked("input-gate-cancel"); err != nil {
		return
	}
	snapshot, err := r.viewLocked(context.Background())
	if err != nil {
		return
	}
	if _, err := r.commitLockedAs(context.Background(), snapshot.Revision, "input-gate-cancel", occurrence.actor, changes); err != nil {
		return
	}
	for _, update := range updates {
		occurrence.entries[update.index] = update.entry
	}
}

func (r *runtimeInstance) commitInputGateEntry(ctx context.Context, actor agent.ActorIdentity, entry *session.ExtensionJournalEntry, operation string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureOpenLocked(operation); err != nil {
		return err
	}
	snapshot, err := r.viewLocked(ctx)
	if err != nil {
		return err
	}
	_, err = r.commitLockedAs(ctx, snapshot.Revision, operation, actor, []session.Change{{Kind: session.UpdateExtensionJournal, Extension: entry}})
	return err
}

func (r *runtimeInstance) finalizeRejectedInput(occurrence *inputGateOccurrence, executionErr error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot, viewErr := r.viewLocked(context.Background())
	if viewErr != nil {
		return executionErr
	}
	decisive := -1
	for index, entry := range occurrence.entries {
		if entry.Status == hook.InvocationSucceeded && entry.Result != nil && entry.Result.Decision == hook.DecisionReject ||
			entry.Status == hook.InvocationFailed || entry.Status == hook.InvocationOutcomeUnknown ||
			entry.Status == hook.InvocationCanceled && entry.EffectDisposition == hook.EffectPending {
			decisive = index
			break
		}
	}
	changes := make([]session.Change, 0, len(occurrence.entries))
	for index := range occurrence.entries {
		entry := occurrence.entries[index]
		if !entry.Status.Terminal() || entry.EffectDisposition != hook.EffectPending {
			continue
		}
		if index == decisive {
			entry.EffectDisposition = hook.EffectApplied
		} else {
			entry.EffectDisposition = hook.EffectDiscarded
		}
		if entry.ContextDisposition == hook.ContextPending {
			entry.ContextDisposition = hook.ContextDiscarded
			entry.ContextInputs = nil
		}
		occurrence.entries[index] = entry
		entryCopy := entry
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
	}
	if len(changes) > 0 {
		if commit, err := r.commitLockedAs(context.Background(), snapshot.Revision, "input-gate-reject", occurrence.actor, changes); err == nil {
			snapshot.Revision = commit.Revision
		} else {
			return err
		}
	}
	cause := classifyInputGateOperationError(executionErr)
	return r.inputGateError(snapshot.Revision, occurrence, cause)
}

func classifyInputGateOperationError(err error) error {
	if agent.IsCode(err, agent.CodeInputRejected) {
		return err
	}
	var failure *hook.InvocationFailure
	if errors.As(err, &failure) {
		kind := agent.ErrorUnavailable
		message := "input gate failed"
		switch {
		case failure.Status == hook.InvocationCanceled || failure.Code == agent.CodeCanceled:
			kind, message = agent.ErrorCanceled, "input gate was canceled"
		case failure.Code == agent.ErrorCode("extension_deadline_exceeded"):
			kind, message = agent.ErrorDeadline, "input gate deadline expired"
		}
		return agent.NewCodedError(kind, failure.Code, "standardagent.input_gate", message, failure.Cause)
	}
	if errors.Is(err, context.Canceled) {
		return agent.NewCodedError(agent.ErrorCanceled, agent.CodeCanceled, "standardagent.input_gate", "input gate was canceled", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return agent.NewCodedError(agent.ErrorDeadline, agent.ErrorCode("extension_deadline_exceeded"), "standardagent.input_gate", "input gate deadline expired", err)
	}
	return agent.NewCodedError(agent.ErrorUnavailable, agent.CodeExtensionFailed, "standardagent.input_gate", "input gate failed", err)
}

func (r *runtimeInstance) finalizeAcceptedSend(ctx context.Context, occurrence *inputGateOccurrence) (interaction.EnqueueReceipt, *activeRun, error) {
	r.mu.Lock()
	if err := r.ensureOpenLocked("gateway.send"); err != nil {
		discardErr := r.discardAcceptedInputLocked(occurrence, err)
		r.mu.Unlock()
		return interaction.EnqueueReceipt{}, nil, discardErr
	}
	snapshot, err := r.viewLocked(context.WithoutCancel(ctx))
	if err != nil {
		r.mu.Unlock()
		return interaction.EnqueueReceipt{}, nil, err
	}
	item := session.QueueItem{Message: occurrence.message, Delivery: session.DeliveryNormal}
	if r.state == runtimeRunning {
		changes := []session.Change{{Kind: session.EnqueueMessage, QueueItem: &item}}
		changes = append(changes, finalizeAcceptedInputEntries(occurrence)...)
		commit, err := r.commitLockedAs(context.WithoutCancel(ctx), snapshot.Revision, "input-gate-send", occurrence.actor, changes)
		r.mu.Unlock()
		if err != nil {
			return interaction.EnqueueReceipt{}, nil, err
		}
		return interaction.EnqueueReceipt{MessageID: occurrence.message.ID, Revision: commit.Revision}, nil, nil
	}
	run, step, changes := r.startChangesLocked(snapshot, item, true)
	changes = append(changes, consumeAcceptedInputEntries(occurrence, run.id, step)...)
	_, err = r.commitLockedAs(context.WithoutCancel(ctx), snapshot.Revision, "input-gate-send-start", occurrence.actor, changes)
	if err != nil {
		run.cancel()
		r.mu.Unlock()
		return interaction.EnqueueReceipt{}, nil, err
	}
	r.activateLocked(run)
	go r.runLoop(run, step)
	r.mu.Unlock()
	return interaction.EnqueueReceipt{MessageID: occurrence.message.ID}, run, nil
}

func (r *runtimeInstance) finalizeAcceptedSteer(ctx context.Context, occurrence *inputGateOccurrence) (interaction.EnqueueReceipt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureOpenLocked("gateway.steer"); err != nil {
		return interaction.EnqueueReceipt{}, r.discardAcceptedInputLocked(occurrence, err)
	}
	snapshot, err := r.viewLocked(context.WithoutCancel(ctx))
	if err != nil {
		return interaction.EnqueueReceipt{}, err
	}
	if r.state != runtimeRunning || r.active == nil || r.active.id != occurrence.originalRunID {
		cause := agent.NewCodedError(agent.ErrorConflict, agent.CodeNoActiveRun, "gateway.steer", "the active Run changed while input gates executed", nil)
		return interaction.EnqueueReceipt{}, r.discardAcceptedInputLocked(occurrence, cause)
	}
	item := session.QueueItem{Message: occurrence.message, Delivery: session.DeliverySteer}
	changes := []session.Change{{Kind: session.EnqueueMessage, QueueItem: &item}}
	changes = append(changes, finalizeAcceptedInputEntries(occurrence)...)
	commit, err := r.commitLockedAs(context.WithoutCancel(ctx), snapshot.Revision, "input-gate-steer", occurrence.actor, changes)
	if err != nil {
		return interaction.EnqueueReceipt{}, err
	}
	return interaction.EnqueueReceipt{MessageID: occurrence.message.ID, Revision: commit.Revision}, nil
}

func (r *runtimeInstance) finalizeAcceptedEdit(ctx context.Context, occurrence *inputGateOccurrence) (interaction.CommitReceipt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureOpenLocked("gateway.edit_queued"); err != nil {
		return interaction.CommitReceipt{}, r.discardAcceptedInputLocked(occurrence, err)
	}
	snapshot, err := r.viewLocked(context.WithoutCancel(ctx))
	if err != nil {
		return interaction.CommitReceipt{}, err
	}
	item, ok := queueByID(snapshot.Queue, occurrence.message.ID)
	if !ok || item.Claimed() {
		cause := agent.NewCodedError(agent.ErrorConflict, agent.CodeQueueItemClaimed, "gateway.edit_queued", "queued input changed while input gates executed", nil)
		return interaction.CommitReceipt{}, r.discardAcceptedInputLocked(occurrence, cause)
	}
	current, err := hook.FingerprintTypedInput(agent.MessageInput{Parts: cloneRuntimeParts(item.Message.Parts)})
	if err != nil || current.Digest != occurrence.previousInputDigest || item.Delivery != occurrence.originalDelivery {
		cause := agent.NewCodedError(agent.ErrorConflict, agent.CodeRevisionConflict, "gateway.edit_queued", "queued input changed while input gates executed", err)
		return interaction.CommitReceipt{}, r.discardAcceptedInputLocked(occurrence, cause)
	}
	edit := session.QueueEdit{MessageID: occurrence.message.ID, Input: occurrence.input, Delivery: item.Delivery}
	changes := []session.Change{{Kind: session.EditQueue, QueueEdit: &edit}}
	changes = append(changes, discardOlderInputContexts(snapshot, occurrence)...)
	changes = append(changes, finalizeAcceptedInputEntries(occurrence)...)
	commit, err := r.commitLockedAs(context.WithoutCancel(ctx), snapshot.Revision, "input-gate-edit", occurrence.actor, changes)
	if err != nil {
		return interaction.CommitReceipt{}, err
	}
	return interaction.CommitReceipt{SessionID: r.id(), Revision: commit.Revision}, nil
}

func (r *runtimeInstance) discardAcceptedInputLocked(occurrence *inputGateOccurrence, cause error) error {
	snapshot, err := r.viewLocked(context.Background())
	if err != nil {
		return cause
	}
	changes := discardAcceptedInputEntries(occurrence)
	if len(changes) > 0 {
		commit, commitErr := r.commitLockedAs(context.Background(), snapshot.Revision, "input-gate-discard", occurrence.actor, changes)
		if commitErr != nil {
			return commitErr
		}
		snapshot.Revision = commit.Revision
	}
	conflict := &interaction.RevisionConflictError{CurrentRevision: snapshot.Revision, SnapshotRequired: true, Cause: cause}
	return r.inputGateError(snapshot.Revision, occurrence, conflict)
}
