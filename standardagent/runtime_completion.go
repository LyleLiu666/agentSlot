package standardagent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/goal"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

type completionGateBinding struct {
	gate       hook.CompletionGate
	descriptor hook.ExtensionDescriptor
}

type completionOccurrence struct {
	step            agent.StepID
	nextStep        agent.StepID
	goal            *goal.DecisionRecord
	entries         []session.ExtensionJournalEntry
	views           []hook.CompletionView
	continued       bool
	recoveryFailure *completionGateRunError
}

func unsettledCompletionEntries(entries []session.ExtensionJournalEntry, runID agent.RunID) []session.ExtensionJournalEntry {
	result := make([]session.ExtensionJournalEntry, 0)
	for _, entry := range entries {
		if entry.Boundary == hook.BoundaryCompletion && entry.RunID == runID &&
			(entry.Status == hook.InvocationPrepared || (entry.Status.Terminal() && entry.EffectDisposition == hook.EffectPending)) {
			result = append(result, entry)
		}
	}
	return result
}

func (r *runtimeInstance) restoreCompletionOccurrence(snapshot session.Snapshot, run *activeRun, entries []session.ExtensionJournalEntry) (*completionOccurrence, error) {
	if len(entries) == 0 || len(entries) != len(r.components.completionGates) {
		return nil, agent.NewError(agent.ErrorInternal, "standardagent.restore_completion", "prepared CompletionGate chain does not match the current application", nil)
	}
	first := entries[0]
	last, ok := messageByID(snapshot.History, first.MessageID)
	if !ok || last.Role != agent.RoleAssistant || last.RunID != run.id || last.StepID != first.StepID {
		return nil, agent.NewError(agent.ErrorInternal, "standardagent.restore_completion", "prepared CompletionGate assistant input is unavailable", nil)
	}
	occurrence := &completionOccurrence{
		step: first.StepID, nextStep: first.TargetStepID,
		entries: append([]session.ExtensionJournalEntry(nil), entries...), views: make([]hook.CompletionView, len(entries)),
	}
	if first.GoalID != "" {
		occurrence.goal = &goal.DecisionRecord{
			ID: fmt.Sprintf("goal-decision-%s-%s-%d", run.id, first.StepID, first.GoalVersion), GoalID: first.GoalID,
			SessionID: r.id(), RunID: run.id, StepID: first.StepID, ExpectedVersion: first.GoalVersion,
			Evaluation: goal.Evaluation{Decision: goal.DecisionDone, Reason: goal.ReasonObjectiveMet}, RecordedAt: first.PreparedAt,
		}
	}
	followOns := completionFollowOns(snapshot.ExtensionJournal, run.id)
	for index, entry := range entries {
		if entry.StepID != first.StepID || entry.TargetStepID != first.TargetStepID || entry.MessageID != first.MessageID ||
			entry.GoalID != first.GoalID || entry.GoalVersion != first.GoalVersion || entry.Descriptor != r.components.completionGates[index].descriptor {
			return nil, agent.NewError(agent.ErrorInternal, "standardagent.restore_completion", "prepared CompletionGate occurrence is inconsistent", nil)
		}
		view := hook.CompletionView{
			InvocationID: entry.InvocationID, SessionID: r.id(), AgentID: r.agentID, WorkspaceID: r.workspaceID,
			Revision: entry.PreparedRevision, RunID: run.id, StepID: entry.StepID, NextStepID: entry.TargetStepID,
			LastAssistantMessage: completionAssistantViewMessage(last), Budget: model.TokenBudget{MaxTokens: r.components.config.MaxTokensPerRun, UsedTokens: run.usedTokens}, FollowOns: followOns,
		}
		if occurrence.goal != nil {
			view.GoalCandidate = &hook.CompletionGoalCandidate{GoalID: first.GoalID, Version: first.GoalVersion}
		}
		fingerprint, err := hook.FingerprintTypedInput(view)
		if err != nil || fingerprint.Digest != entry.InputDigest {
			return nil, agent.NewError(agent.ErrorInternal, "standardagent.restore_completion", "prepared CompletionGate input does not match durable evidence", err)
		}
		occurrence.views[index] = view
		if entry.Status == hook.InvocationSucceeded && entry.Result != nil && entry.Result.Decision == hook.DecisionContinue {
			occurrence.continued = true
		}
	}
	return occurrence, nil
}

func messageByID(history []session.HistoryFact, id agent.MessageID) (agent.Message, bool) {
	for _, fact := range history {
		if fact.Message != nil && fact.Message.ID == id {
			return cloneRuntimeMessage(*fact.Message), true
		}
	}
	return agent.Message{}, false
}

func (r *runtimeInstance) runCompletionRecovery(run *activeRun, occurrence *completionOccurrence) {
	if occurrence.recoveryFailure != nil {
		settleErr := r.settleCompletionRecoveryEntries(run, occurrence.entries, occurrence.recoveryFailure.code)
		failure := &completionGateRunError{code: occurrence.recoveryFailure.code, reason: occurrence.recoveryFailure.reason, cause: errors.Join(occurrence.recoveryFailure.cause, settleErr)}
		nextRun, firstStep := r.finishRun(run, stepFailed, completionGateTermination(failure))
		if nextRun != nil {
			r.runLoop(nextRun, firstStep)
		}
		return
	}
	continued, superseded, retryGoal, err := r.executeCompletionOccurrence(run, occurrence)
	var next agent.StepID
	if err == nil && (superseded || retryGoal) {
		var canceled bool
		next, continued, canceled, err = r.continueAfterCompletion(run)
		if canceled {
			err = context.Canceled
		}
	} else if continued {
		next = occurrence.nextStep
	}
	if err == nil && continued {
		r.runLoop(run, next)
		return
	}
	outcome := stepNatural
	var termination *session.RunTermination
	if err != nil {
		if errors.Is(err, context.Canceled) {
			outcome, termination = stepCanceled, canceledRunTermination()
		} else if extensionTermination := completionGateTermination(err); extensionTermination != nil {
			outcome, termination = stepFailed, extensionTermination
		} else {
			outcome, termination = stepFailed, terminationFromError(session.TerminationRuntime, agent.CodeRuntimeFailed, err)
		}
	}
	nextRun, firstStep := r.finishRun(run, outcome, termination)
	if nextRun != nil {
		r.runLoop(nextRun, firstStep)
	}
}

func (r *runtimeInstance) settleCompletionRecoveryEntries(run *activeRun, entries []session.ExtensionJournalEntry, code agent.ErrorCode) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != run {
		return context.Canceled
	}
	snapshot, err := r.viewLocked(context.Background())
	if err != nil {
		return err
	}
	selected := make(map[hook.InvocationID]struct{}, len(entries))
	for _, entry := range entries {
		selected[entry.InvocationID] = struct{}{}
	}
	now := time.Now().UTC()
	changes := make([]session.Change, 0, len(entries))
	for _, current := range snapshot.ExtensionJournal {
		if _, ok := selected[current.InvocationID]; !ok {
			continue
		}
		entry := current
		switch {
		case entry.Status == hook.InvocationPrepared:
			entry.Status, entry.FinishedAt = hook.InvocationCanceled, now
			entry.ErrorCode, entry.EffectDisposition = code, hook.EffectDiscarded
		case entry.Status.Terminal() && entry.EffectDisposition == hook.EffectPending:
			if entry.Status == hook.InvocationSucceeded {
				entry.EffectDisposition = hook.EffectDiscarded
			} else {
				entry.EffectDisposition = hook.EffectApplied
			}
			if entry.ContextDisposition == hook.ContextPending {
				entry.ContextDisposition, entry.ContextInputs = hook.ContextDiscarded, nil
			}
		default:
			continue
		}
		if entry.FinishedAt.Before(entry.PreparedAt) {
			entry.FinishedAt = entry.PreparedAt
		}
		entryCopy := entry
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
	}
	if len(changes) == 0 {
		return nil
	}
	_, err = r.commitLocked(context.Background(), snapshot.Revision, "completion-gate-recovery-settle", changes)
	return err
}

type completionGateRunError struct {
	code   agent.ErrorCode
	reason string
	cause  error
}

func (e *completionGateRunError) Error() string { return "standardagent: completion gate failed" }
func (e *completionGateRunError) Unwrap() error { return e.cause }

func completionGateTermination(err error) *session.RunTermination {
	var failure *completionGateRunError
	if !errors.As(err, &failure) {
		return nil
	}
	termination := &session.RunTermination{Source: session.TerminationExtension, Kind: agent.ErrorUnavailable, Code: failure.code, SafeMessage: failure.reason}
	if termination.Validate() != nil {
		termination.SafeMessage = ""
	}
	return termination
}

func (r *runtimeInstance) prepareCompletionOccurrence(run *activeRun, candidate *goal.DecisionRecord) (*completionOccurrence, bool, error) {
	if len(r.components.completionGates) == 0 {
		return nil, false, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != run || run.cancelRequested || r.closing {
		return nil, false, context.Canceled
	}
	snapshot, err := r.viewLocked(context.Background())
	if err != nil {
		return nil, false, err
	}
	if len(pendingByDelivery(snapshot.Queue, session.DeliverySteer)) > 0 {
		return nil, true, nil
	}
	last, ok := lastAssistantForRun(snapshot.History, run.id)
	if !ok {
		return nil, false, agent.NewError(agent.ErrorInternal, "standardagent.completion_gate", "completed Run has no assistant message", nil)
	}
	nextStep := agent.StepID(r.nextID("step"))
	preparedRevision := snapshot.Revision.Next()
	followOns := completionFollowOns(snapshot.ExtensionJournal, run.id)
	now := time.Now().UTC()
	if candidate != nil {
		candidateCopy := *candidate
		// PreparedAt is also the durable idempotency timestamp for the gated
		// Goal decision. Recovery can therefore reconstruct the exact record
		// after an uncertain GoalStore response.
		candidateCopy.RecordedAt = now
		candidate = &candidateCopy
	}
	occurrence := &completionOccurrence{
		step: last.StepID, nextStep: nextStep, goal: candidate,
		entries: make([]session.ExtensionJournalEntry, len(r.components.completionGates)),
		views:   make([]hook.CompletionView, len(r.components.completionGates)),
	}
	changes := make([]session.Change, 0, len(r.components.completionGates))
	for index, binding := range r.components.completionGates {
		view := hook.CompletionView{
			InvocationID: hook.InvocationID(r.nextID("completion")), SessionID: r.id(), AgentID: r.agentID, WorkspaceID: r.workspaceID,
			Revision: preparedRevision, RunID: run.id, StepID: last.StepID, NextStepID: nextStep,
			LastAssistantMessage: completionAssistantViewMessage(last), Budget: model.TokenBudget{MaxTokens: r.components.config.MaxTokensPerRun, UsedTokens: run.usedTokens},
			FollowOns: followOns,
		}
		if candidate != nil {
			view.GoalCandidate = &hook.CompletionGoalCandidate{GoalID: candidate.GoalID, Version: candidate.ExpectedVersion}
		}
		if err := view.Validate(); err != nil {
			return nil, false, agent.NewError(agent.ErrorInternal, "standardagent.completion_gate", "Runtime constructed an invalid completion view", err)
		}
		fingerprint, err := hook.FingerprintTypedInput(view)
		if err != nil {
			return nil, false, err
		}
		entry := session.ExtensionJournalEntry{
			InvocationID: view.InvocationID, Sequence: session.ExtensionSequence(len(snapshot.ExtensionJournal) + index + 1),
			Descriptor: binding.descriptor, Boundary: hook.BoundaryCompletion,
			SessionID: r.id(), RunID: run.id, StepID: last.StepID, TargetStepID: nextStep, MessageID: last.ID,
			InputDigest: fingerprint.Digest, PreparedRevision: preparedRevision, PreparedAt: now,
			Status: hook.InvocationPrepared, EffectDisposition: hook.EffectNone, ContextDisposition: hook.ContextNone,
		}
		if candidate != nil {
			entry.GoalID, entry.GoalVersion = candidate.GoalID, candidate.ExpectedVersion
		}
		occurrence.views[index], occurrence.entries[index] = view, entry
		entryCopy := entry
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
	}
	commit, err := r.commitLocked(context.Background(), snapshot.Revision, "completion-gate-prepare", changes)
	if err != nil {
		settleErr := r.settleCompletionPreparationLocked(run, occurrence.entries)
		return nil, false, errors.Join(err, settleErr)
	}
	if commit.Revision != preparedRevision {
		return nil, false, agent.NewError(agent.ErrorInternal, "standardagent.completion_gate", "prepared completion commit returned an unexpected revision", nil)
	}
	return occurrence, false, nil
}

func (r *runtimeInstance) settleCompletionPreparationLocked(run *activeRun, entries []session.ExtensionJournalEntry) error {
	if r.active != run {
		return context.Canceled
	}
	snapshot, err := r.viewLocked(context.Background())
	if err != nil {
		return err
	}
	selected := make(map[hook.InvocationID]struct{}, len(entries))
	for _, entry := range entries {
		selected[entry.InvocationID] = struct{}{}
	}
	changes := make([]session.Change, 0, len(entries))
	now := time.Now().UTC()
	for _, current := range snapshot.ExtensionJournal {
		if _, ok := selected[current.InvocationID]; !ok || current.Status != hook.InvocationPrepared {
			continue
		}
		entry := current
		entry.Status, entry.FinishedAt = hook.InvocationCanceled, now
		if entry.FinishedAt.Before(entry.PreparedAt) {
			entry.FinishedAt = entry.PreparedAt
		}
		entry.ErrorCode, entry.EffectDisposition = "completion_gate_prepare_failed", hook.EffectDiscarded
		entryCopy := entry
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
	}
	if len(changes) == 0 {
		return nil
	}
	_, err = r.commitLocked(context.Background(), snapshot.Revision, "completion-gate-prepare-settle", changes)
	return err
}

func lastAssistantForRun(history []session.HistoryFact, runID agent.RunID) (agent.Message, bool) {
	for index := len(history) - 1; index >= 0; index-- {
		message := history[index].Message
		if message != nil && message.RunID == runID && message.Role == agent.RoleAssistant {
			return cloneRuntimeMessage(*message), true
		}
	}
	return agent.Message{}, false
}

func completionAssistantViewMessage(source agent.Message) agent.Message {
	message := cloneRuntimeMessage(source)
	message.ClientMessageID = ""
	message.ModelContinuation = nil
	message.CreatedAt = time.Time{}
	return message
}

func completionFollowOns(entries []session.ExtensionJournalEntry, runID agent.RunID) int {
	steps := make(map[agent.StepID]struct{})
	for _, entry := range entries {
		if entry.Boundary == hook.BoundaryCompletion && entry.RunID == runID && entry.Status == hook.InvocationSucceeded &&
			entry.Result != nil && entry.Result.Decision == hook.DecisionContinue && entry.EffectDisposition == hook.EffectApplied {
			steps[entry.TargetStepID] = struct{}{}
		}
	}
	return len(steps)
}

func (r *runtimeInstance) executeCompletionOccurrence(run *activeRun, occurrence *completionOccurrence) (continued, superseded, retryGoal bool, err error) {
	if occurrence == nil {
		return false, false, false, nil
	}
	succeeded := make([]session.ExtensionJournalEntry, 0, len(occurrence.entries))
	pipelined := make(map[hook.InvocationID]struct{})
	for index, binding := range r.components.completionGates {
		entry := occurrence.entries[index]
		if run.ctx.Err() != nil {
			return false, false, false, errors.Join(context.Canceled, r.settleCompletionAbort(run, succeeded, occurrence.entries[index:], "completion_gate_canceled"))
		}
		steered, checkErr := r.completionSteerPending(run)
		if checkErr != nil {
			return false, false, false, checkErr
		}
		if steered {
			return false, true, false, r.settleCompletionAbort(run, succeeded, occurrence.entries[index:], "completion_gate_superseded")
		}
		if entry.Status == hook.InvocationSucceeded && entry.EffectDisposition == hook.EffectPending {
			view := cloneCompletionView(occurrence.views[index])
			fingerprint, fingerprintErr := hook.FingerprintTypedInput(view)
			if fingerprintErr != nil || fingerprint.Digest != entry.InputDigest || binding.descriptor != entry.Descriptor {
				return false, false, false, r.completionFailure(run, succeeded, entry, occurrence.entries[index+1:], "completion_gate_definition_mismatch", "prepared completion gate does not match the current application", fingerprintErr)
			}
			succeeded = append(succeeded, entry)
			continue
		}
		if entry.Status.Terminal() && entry.EffectDisposition == hook.EffectPending {
			failure := classifiedCompletionFailure{status: entry.Status, code: entry.ErrorCode, reason: entry.ErrorReason}
			if failure.code == "" {
				failure.code, failure.reason = "completion_gate_outcome_unknown", "completion gate outcome is unknown"
			}
			settleErr := r.settleCompletionFailure(run, succeeded, entry, occurrence.entries[index+1:])
			return false, false, false, &completionGateRunError{code: failure.code, reason: failure.reason, cause: settleErr}
		}
		if entry.Status == hook.InvocationPrepared {
			entry.Status, entry.PendingAt = hook.InvocationPending, time.Now().UTC()
			if entry.PendingAt.Before(entry.PreparedAt) {
				entry.PendingAt = entry.PreparedAt
			}
			if err := r.commitCompletionEntries(run, "completion-gate-pending", entry); err != nil {
				return false, false, false, r.completionPersistenceFailure(run, err)
			}
			occurrence.entries[index] = entry
		} else if _, ok := pipelined[entry.InvocationID]; !ok || entry.Status != hook.InvocationPending {
			return false, false, false, r.completionFailure(run, succeeded, entry, occurrence.entries[index+1:], "completion_gate_state_invalid", "completion gate state is not executable", nil)
		}

		view := cloneCompletionView(occurrence.views[index])
		fingerprint, fingerprintErr := hook.FingerprintTypedInput(view)
		if fingerprintErr != nil || fingerprint.Digest != entry.InputDigest || binding.descriptor != entry.Descriptor {
			return false, false, false, r.completionFailure(run, succeeded, entry, occurrence.entries[index+1:], "completion_gate_definition_mismatch", "prepared completion gate does not match the current application", fingerprintErr)
		}
		result, invokeErr := invokeCompletionGate(run.ctx, binding.gate, view)
		failure := classifyCompletionGateFailure(invokeErr)
		if invokeErr == nil {
			if validateErr := result.Validate(view); validateErr != nil {
				failure = classifiedCompletionFailure{status: hook.InvocationFailed, code: agent.CodeExtensionFailed, reason: "completion gate returned an invalid result", cause: validateErr}
			}
		}
		terminal := entry
		terminal.FinishedAt, terminal.EffectDisposition = time.Now().UTC(), hook.EffectPending
		if terminal.FinishedAt.Before(terminal.PendingAt) {
			terminal.FinishedAt = terminal.PendingAt
		}
		if failure.code != "" {
			terminal.Status, terminal.ErrorCode, terminal.ErrorReason = failure.status, failure.code, failure.reason
		} else {
			terminal.Status = hook.InvocationSucceeded
			decision := hook.DecisionComplete
			if result.Decision == hook.CompletionContinue {
				decision = hook.DecisionContinue
				occurrence.continued = true
			}
			terminal.Result = &hook.InvocationResult{Decision: decision, Reason: result.Reason}
			if len(result.Context) > 0 {
				terminal.ContextInputs = cloneRuntimeInputs(result.Context)
				contextFingerprint, fingerprintErr := hook.FingerprintTypedInput(terminal.ContextInputs)
				if fingerprintErr != nil {
					return false, false, false, fingerprintErr
				}
				terminal.ContextDisposition, terminal.ContextDigest, terminal.ContextBytes = hook.ContextPending, contextFingerprint.Digest, contextFingerprint.Bytes
			}
		}
		updates := []session.ExtensionJournalEntry{terminal}
		if failure.code == "" && index+1 < len(occurrence.entries) {
			next := occurrence.entries[index+1]
			next.Status, next.PendingAt = hook.InvocationPending, time.Now().UTC()
			if next.PendingAt.Before(next.PreparedAt) {
				next.PendingAt = next.PreparedAt
			}
			updates = append(updates, next)
			occurrence.entries[index+1] = next
			pipelined[next.InvocationID] = struct{}{}
		}
		if failure.code == "" && index+1 == len(occurrence.entries) && occurrence.goal == nil {
			// There is no cross-store Goal mutation to order after this result.
			// Keep the terminal transition in memory and atomically persist it
			// together with effect application/context consumption below. This
			// removes one full FileStore rewrite without creating a recovery gap.
			occurrence.entries[index] = terminal
			succeeded = append(succeeded, terminal)
			return r.finalizeCompletionOccurrence(run, occurrence, succeeded)
		}
		if err := r.commitCompletionEntries(run, "completion-gate-terminal", updates...); err != nil {
			return false, false, false, r.completionPersistenceFailure(run, err)
		}
		occurrence.entries[index] = terminal
		if failure.code != "" {
			settleErr := r.settleCompletionFailure(run, succeeded, terminal, occurrence.entries[index+1:])
			return false, false, false, &completionGateRunError{code: failure.code, reason: failure.reason, cause: errors.Join(failure.cause, settleErr)}
		}
		succeeded = append(succeeded, terminal)
	}
	return r.finalizeCompletionOccurrence(run, occurrence, succeeded)
}

func cloneCompletionView(source hook.CompletionView) hook.CompletionView {
	copy := source
	copy.LastAssistantMessage = cloneRuntimeMessage(source.LastAssistantMessage)
	if source.GoalCandidate != nil {
		candidate := *source.GoalCandidate
		copy.GoalCandidate = &candidate
	}
	return copy
}

func invokeCompletionGate(ctx context.Context, gate hook.CompletionGate, view hook.CompletionView) (result hook.CompletionGateResult, err error) {
	defer func() {
		if recover() != nil {
			result = hook.CompletionGateResult{}
			err = errors.New("completion gate panicked")
		}
	}()
	return gate.Evaluate(ctx, view)
}

type classifiedCompletionFailure struct {
	status hook.InvocationStatus
	code   agent.ErrorCode
	reason string
	cause  error
}

func classifyCompletionGateFailure(err error) classifiedCompletionFailure {
	if err == nil {
		return classifiedCompletionFailure{}
	}
	failure := classifiedCompletionFailure{status: hook.InvocationFailed, code: agent.CodeExtensionFailed, reason: "completion gate failed", cause: err}
	var declared *hook.InvocationFailure
	if errors.As(err, &declared) && declared.Validate() == nil {
		failure.status, failure.code, failure.reason = declared.Status, declared.Code, declared.Reason
	} else if errors.Is(err, context.Canceled) {
		failure.status, failure.code, failure.reason = hook.InvocationCanceled, agent.CodeCanceled, "completion gate was canceled"
	}
	return failure
}

func (r *runtimeInstance) completionSteerPending(run *activeRun) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != run || run.cancelRequested || r.closing {
		return false, context.Canceled
	}
	snapshot, err := r.viewLocked(context.Background())
	if err != nil {
		return false, err
	}
	return len(pendingByDelivery(snapshot.Queue, session.DeliverySteer)) > 0, nil
}

func (r *runtimeInstance) finalizeCompletionOccurrence(run *activeRun, occurrence *completionOccurrence, succeeded []session.ExtensionJournalEntry) (continued, superseded, retryGoal bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != run || run.cancelRequested || r.closing {
		return false, false, false, errors.Join(context.Canceled, r.settleCompletionAbortLocked(run, succeeded, nil, "completion_gate_canceled"))
	}
	snapshot, err := r.viewLocked(context.Background())
	if err != nil {
		return false, false, false, err
	}
	if len(pendingByDelivery(snapshot.Queue, session.DeliverySteer)) > 0 {
		return false, true, false, r.settleCompletionAbortLocked(run, succeeded, nil, "completion_gate_superseded")
	}
	if !occurrence.continued && occurrence.goal != nil {
		if recordErr := recordGoalDecisionIdempotently(context.Background(), r.components.goalStore, *occurrence.goal); recordErr != nil {
			if errors.Is(recordErr, goal.ErrVersionConflict) {
				return false, false, true, r.settleCompletionAbortLocked(run, succeeded, nil, "completion_goal_version_changed")
			} else {
				settleErr := r.settleCompletionAbortLocked(run, succeeded, nil, "completion_goal_commit_failed")
				return false, false, false, &completionGateRunError{
					code: "completion_goal_commit_failed", reason: "completion Goal decision could not be committed", cause: errors.Join(recordErr, settleErr),
				}
			}
		}
	}
	changes := make([]session.Change, 0, len(succeeded)*2)
	for _, entry := range succeeded {
		current, ok := extensionEntryByInvocation(snapshot.ExtensionJournal, entry.InvocationID)
		if !ok {
			return false, false, false, r.completionPersistenceFailureLocked(run, errors.New("completion gate journal entry disappeared"))
		}
		if current.Status == hook.InvocationPending {
			terminalCopy := entry
			changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &terminalCopy})
		} else if !current.Status.Terminal() || current.EffectDisposition != hook.EffectPending {
			return false, false, false, r.completionPersistenceFailureLocked(run, errors.New("completion gate journal state changed before application"))
		}
		entry.EffectDisposition = hook.EffectApplied
		if entry.ContextDisposition == hook.ContextPending {
			fact := session.ContextContributionFact{
				RunID: run.id, StepID: occurrence.nextStep, SourceKey: fmt.Sprintf("hook:%s:%s", entry.Descriptor.Key, entry.InvocationID),
				Inputs: cloneRuntimeInputs(entry.ContextInputs),
			}
			changes = append(changes, session.Change{Kind: session.AppendContextContribution, ContextContribution: &fact})
			entry.ContextDisposition, entry.ContextInputs = hook.ContextConsumed, nil
		}
		entryCopy := entry
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
	}
	if len(changes) > 0 {
		if _, err := r.commitLocked(context.Background(), snapshot.Revision, "completion-gate-apply", changes); err != nil {
			if occurrence.goal != nil && !occurrence.continued {
				if retryErr := r.retryCompletionApplyAfterGoalLocked(run, occurrence, succeeded); retryErr == nil {
					return false, false, false, nil
				} else {
					return false, false, false, retryErr
				}
			}
			if durable, verifyErr := r.completionApplyAlreadyDurableLocked(run, succeeded); durable && verifyErr == nil {
				return occurrence.continued, false, false, nil
			} else if verifyErr != nil {
				err = errors.Join(err, verifyErr)
			}
			return false, false, false, r.completionPersistenceFailureLocked(run, err)
		}
	}
	return occurrence.continued, false, false, nil
}

func (r *runtimeInstance) completionApplyAlreadyDurableLocked(run *activeRun, entries []session.ExtensionJournalEntry) (bool, error) {
	if r.active != run {
		return false, context.Canceled
	}
	snapshot, err := r.viewLocked(context.Background())
	if err != nil {
		return false, err
	}
	for _, expected := range entries {
		current, ok := extensionEntryByInvocation(snapshot.ExtensionJournal, expected.InvocationID)
		if !ok || current.Status != hook.InvocationSucceeded || current.EffectDisposition != hook.EffectApplied ||
			current.ContextDisposition == hook.ContextPending {
			return false, nil
		}
	}
	return true, nil
}

func extensionEntryByInvocation(entries []session.ExtensionJournalEntry, invocationID hook.InvocationID) (session.ExtensionJournalEntry, bool) {
	for _, entry := range entries {
		if entry.InvocationID == invocationID {
			return entry, true
		}
	}
	return session.ExtensionJournalEntry{}, false
}

func (r *runtimeInstance) retryCompletionApplyAfterGoalLocked(run *activeRun, occurrence *completionOccurrence, entries []session.ExtensionJournalEntry) error {
	if r.active != run {
		return context.Canceled
	}
	snapshot, err := r.viewLocked(context.Background())
	if err != nil {
		return err
	}
	selected := make(map[hook.InvocationID]struct{}, len(entries))
	for _, entry := range entries {
		selected[entry.InvocationID] = struct{}{}
	}
	changes := make([]session.Change, 0, len(entries)*2)
	for _, current := range snapshot.ExtensionJournal {
		if _, ok := selected[current.InvocationID]; !ok || current.Status != hook.InvocationSucceeded {
			continue
		}
		if current.EffectDisposition == hook.EffectApplied {
			continue
		}
		if current.EffectDisposition != hook.EffectPending {
			return &completionGateRunError{code: "completion_goal_application_failed", reason: "completion Goal was committed but its gate effect is inconsistent"}
		}
		entry := current
		entry.EffectDisposition = hook.EffectApplied
		if entry.ContextDisposition == hook.ContextPending {
			fact := session.ContextContributionFact{
				RunID: run.id, StepID: occurrence.nextStep, SourceKey: fmt.Sprintf("hook:%s:%s", entry.Descriptor.Key, entry.InvocationID),
				Inputs: cloneRuntimeInputs(entry.ContextInputs),
			}
			changes = append(changes, session.Change{Kind: session.AppendContextContribution, ContextContribution: &fact})
			entry.ContextDisposition, entry.ContextInputs = hook.ContextConsumed, nil
		}
		entryCopy := entry
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
	}
	if len(changes) == 0 {
		return nil
	}
	if _, err := r.commitLocked(context.Background(), snapshot.Revision, "completion-gate-apply-retry", changes); err != nil {
		return &completionGateRunError{
			code: "completion_goal_application_failed", reason: "completion Goal was committed but its gate effect could not be finalized", cause: err,
		}
	}
	return nil
}

func (r *runtimeInstance) commitCompletionEntries(run *activeRun, operation string, entries ...session.ExtensionJournalEntry) error {
	if len(entries) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != run {
		return context.Canceled
	}
	snapshot, err := r.viewLocked(context.Background())
	if err != nil {
		return err
	}
	changes := make([]session.Change, 0, len(entries))
	for index := range entries {
		entry := entries[index]
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entry})
	}
	_, err = r.commitLocked(context.Background(), snapshot.Revision, operation, changes)
	return err
}

func (r *runtimeInstance) settleCompletionAbort(run *activeRun, succeeded, remaining []session.ExtensionJournalEntry, code agent.ErrorCode) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settleCompletionAbortLocked(run, succeeded, remaining, code)
}

func (r *runtimeInstance) settleCompletionAbortLocked(run *activeRun, succeeded, remaining []session.ExtensionJournalEntry, code agent.ErrorCode) error {
	if r.active != run {
		return context.Canceled
	}
	snapshot, err := r.viewLocked(context.Background())
	if err != nil {
		return err
	}
	changes := finalizeCompletionEntriesFromSnapshot(snapshot, succeeded, hook.EffectDiscarded)
	changes = append(changes, cancelCompletionEntries(remaining, code)...)
	if len(changes) == 0 {
		return nil
	}
	_, err = r.commitLocked(context.Background(), snapshot.Revision, "completion-gate-abort", changes)
	return err
}

func (r *runtimeInstance) settleCompletionFailure(run *activeRun, succeeded []session.ExtensionJournalEntry, failed session.ExtensionJournalEntry, remaining []session.ExtensionJournalEntry) error {
	changes := finalizeCompletionEntries(succeeded, hook.EffectDiscarded)
	failed.EffectDisposition = hook.EffectApplied
	failedCopy := failed
	changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &failedCopy})
	changes = append(changes, cancelCompletionEntries(remaining, "completion_gate_batch_aborted")...)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != run {
		return context.Canceled
	}
	snapshot, err := r.viewLocked(context.Background())
	if err != nil {
		return err
	}
	_, err = r.commitLocked(context.Background(), snapshot.Revision, "completion-gate-failure", changes)
	return err
}

func (r *runtimeInstance) completionFailure(run *activeRun, succeeded []session.ExtensionJournalEntry, failed session.ExtensionJournalEntry, remaining []session.ExtensionJournalEntry, code agent.ErrorCode, reason string, cause error) error {
	if failed.Status == hook.InvocationPending {
		failed.Status, failed.FinishedAt = hook.InvocationFailed, time.Now().UTC()
		failed.ErrorCode, failed.ErrorReason, failed.EffectDisposition = code, reason, hook.EffectPending
		if err := r.commitCompletionEntries(run, "completion-gate-invalid", failed); err != nil {
			cause = errors.Join(cause, err)
		}
	}
	settleErr := r.settleCompletionFailure(run, succeeded, failed, remaining)
	return &completionGateRunError{code: code, reason: reason, cause: errors.Join(cause, settleErr)}
}

func finalizeCompletionEntries(entries []session.ExtensionJournalEntry, disposition hook.EffectDisposition) []session.Change {
	changes := make([]session.Change, 0, len(entries))
	for _, entry := range entries {
		if !entry.Status.Terminal() || entry.EffectDisposition != hook.EffectPending {
			continue
		}
		entry.EffectDisposition = disposition
		if entry.ContextDisposition == hook.ContextPending {
			entry.ContextDisposition, entry.ContextInputs = hook.ContextDiscarded, nil
		}
		entryCopy := entry
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
	}
	return changes
}

func finalizeCompletionEntriesFromSnapshot(snapshot session.Snapshot, entries []session.ExtensionJournalEntry, disposition hook.EffectDisposition) []session.Change {
	changes := make([]session.Change, 0, len(entries)*2)
	for _, entry := range entries {
		current, ok := extensionEntryByInvocation(snapshot.ExtensionJournal, entry.InvocationID)
		if !ok {
			continue
		}
		if current.Status == hook.InvocationPending && entry.Status.Terminal() && entry.EffectDisposition == hook.EffectPending {
			terminalCopy := entry
			changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &terminalCopy})
			current = entry
		}
		if !current.Status.Terminal() || current.EffectDisposition != hook.EffectPending {
			continue
		}
		entry = current
		entry.EffectDisposition = disposition
		if entry.ContextDisposition == hook.ContextPending {
			entry.ContextDisposition, entry.ContextInputs = hook.ContextDiscarded, nil
		}
		entryCopy := entry
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
	}
	return changes
}

func cancelCompletionEntries(entries []session.ExtensionJournalEntry, code agent.ErrorCode) []session.Change {
	changes := make([]session.Change, 0, len(entries))
	for _, entry := range entries {
		if entry.Status != hook.InvocationPrepared {
			continue
		}
		entry.Status, entry.FinishedAt = hook.InvocationCanceled, time.Now().UTC()
		if entry.FinishedAt.Before(entry.PreparedAt) {
			entry.FinishedAt = entry.PreparedAt
		}
		entry.ErrorCode, entry.EffectDisposition = code, hook.EffectDiscarded
		entryCopy := entry
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
	}
	return changes
}

func (r *runtimeInstance) completionPersistenceFailure(run *activeRun, cause error) error {
	// Reload authoritative state before settling. A failed FileStore commit may
	// have become durable, so in-memory assumptions are not recovery evidence.
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.completionPersistenceFailureLocked(run, cause)
}

func (r *runtimeInstance) completionPersistenceFailureLocked(run *activeRun, cause error) error {
	if r.active != run {
		return cause
	}
	snapshot, err := r.viewLocked(context.Background())
	if err != nil {
		return errors.Join(cause, err)
	}
	now := time.Now().UTC()
	unknownChanges := make([]session.Change, 0)
	for _, current := range snapshot.ExtensionJournal {
		if current.Boundary != hook.BoundaryCompletion || current.RunID != run.id || current.Status != hook.InvocationPending {
			continue
		}
		entry := current
		entry.Status, entry.FinishedAt = hook.InvocationOutcomeUnknown, now
		entry.ErrorCode, entry.ErrorReason, entry.EffectDisposition = "completion_gate_persistence_unknown", "completion gate persistence outcome is unknown", hook.EffectPending
		if entry.FinishedAt.Before(entry.PreparedAt) {
			entry.FinishedAt = entry.PreparedAt
		}
		entryCopy := entry
		unknownChanges = append(unknownChanges, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
	}
	if len(unknownChanges) > 0 {
		if _, err = r.commitLocked(context.Background(), snapshot.Revision, "completion-gate-persistence-unknown", unknownChanges); err != nil {
			return &completionGateRunError{code: "completion_gate_persistence_unknown", reason: "completion gate persistence outcome is unknown", cause: errors.Join(cause, err)}
		}
		snapshot, err = r.viewLocked(context.Background())
		if err != nil {
			return &completionGateRunError{code: "completion_gate_persistence_unknown", reason: "completion gate persistence outcome is unknown", cause: errors.Join(cause, err)}
		}
	}
	finalChanges := make([]session.Change, 0)
	for _, current := range snapshot.ExtensionJournal {
		if current.Boundary != hook.BoundaryCompletion || current.RunID != run.id {
			continue
		}
		entry := current
		switch {
		case entry.Status == hook.InvocationPrepared:
			entry.Status, entry.FinishedAt = hook.InvocationCanceled, now
			entry.ErrorCode, entry.EffectDisposition = "completion_gate_persistence_unknown", hook.EffectDiscarded
		case entry.Status.Terminal() && entry.EffectDisposition == hook.EffectPending:
			if entry.Status == hook.InvocationSucceeded {
				entry.EffectDisposition = hook.EffectDiscarded
			} else {
				entry.EffectDisposition = hook.EffectApplied
			}
			if entry.ContextDisposition == hook.ContextPending {
				entry.ContextDisposition, entry.ContextInputs = hook.ContextDiscarded, nil
			}
		default:
			continue
		}
		if entry.FinishedAt.Before(entry.PreparedAt) {
			entry.FinishedAt = entry.PreparedAt
		}
		entryCopy := entry
		finalChanges = append(finalChanges, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
	}
	if len(finalChanges) > 0 {
		_, err = r.commitLocked(context.Background(), snapshot.Revision, "completion-gate-persistence-settle", finalChanges)
	}
	return &completionGateRunError{code: "completion_gate_persistence_unknown", reason: "completion gate persistence outcome is unknown", cause: errors.Join(cause, err)}
}
