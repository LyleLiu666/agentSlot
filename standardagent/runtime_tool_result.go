package standardagent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
	"github.com/LyleLiu666/agentSlot/tool"
)

type toolResultHookBinding struct {
	hook       hook.ToolResultHook
	descriptor hook.ExtensionDescriptor
	scope      hook.ToolResultScope
}

type toolResultHookOccurrence struct {
	call     agent.ToolCall
	result   tool.ToolResult
	nextStep agent.StepID
	bindings []toolResultHookBinding
	entries  []session.ExtensionJournalEntry
}

type toolResultHookFailure struct {
	code   agent.ErrorCode
	reason string
}

func cloneToolResultScope(source hook.ToolResultScope) hook.ToolResultScope {
	return hook.ToolResultScope{
		All: source.All, ToolKeys: append([]string(nil), source.ToolKeys...), Statuses: append([]tool.ResultStatus(nil), source.Statuses...),
	}
}

func matchingToolResultHooks(bindings []toolResultHookBinding, key string, status tool.ResultStatus) []toolResultHookBinding {
	result := make([]toolResultHookBinding, 0, len(bindings))
	for _, binding := range bindings {
		if binding.scope.Matches(key, status) {
			result = append(result, binding)
		}
	}
	return result
}

func (r *runtimeInstance) appendToolResultReservations(snapshot session.Snapshot, calls []agent.ToolCall, results []tool.ToolResult, nextStep agent.StepID, preparedRevision agent.Revision, changes []session.Change) ([]toolResultHookOccurrence, []session.Change, error) {
	if len(r.components.toolResultHooks) == 0 {
		return nil, changes, nil
	}
	if len(calls) != len(results) || !nextStep.Valid() || preparedRevision == 0 {
		return nil, nil, agent.NewError(agent.ErrorInternal, "standardagent.tool_result_hook", "tool result reservation input is invalid", nil)
	}
	pending := make(map[agent.ToolCallID]bool, len(snapshot.RunJournal))
	for _, entry := range snapshot.RunJournal {
		if entry.Status == session.JournalPending && entry.ToolCall != nil {
			pending[entry.ToolCall.ID] = true
		}
	}
	nextSequence := session.ExtensionSequence(len(snapshot.ExtensionJournal) + 1)
	now := time.Now().UTC()
	occurrences := make([]toolResultHookOccurrence, 0, len(calls))
	for index, call := range calls {
		result := cloneRuntimeToolResult(results[index])
		if !pending[call.ID] || result.CallID != call.ID || (result.Status != tool.ResultSucceeded && result.Status != tool.ResultFailed) {
			continue
		}
		bindings := matchingToolResultHooks(r.components.toolResultHooks, call.Name, result.Status)
		if len(bindings) == 0 {
			continue
		}
		occurrence := toolResultHookOccurrence{call: call, result: result, nextStep: nextStep, bindings: bindings, entries: make([]session.ExtensionJournalEntry, 0, len(bindings))}
		for _, binding := range bindings {
			invocationID := hook.InvocationID(r.nextID("post-result"))
			view := toolResultView(r, invocationID, preparedRevision, call, result, nextStep)
			fingerprint, err := hook.FingerprintTypedInput(view)
			if err != nil {
				return nil, nil, err
			}
			entry := session.ExtensionJournalEntry{
				InvocationID: invocationID, Sequence: nextSequence, Descriptor: binding.descriptor, Boundary: hook.BoundaryToolResult,
				SessionID: r.id(), RunID: call.RunID, StepID: call.StepID, TargetStepID: nextStep, MessageID: call.MessageID, ToolCallID: call.ID,
				InputDigest: fingerprint.Digest, PreparedRevision: preparedRevision, PreparedAt: now,
				Status: hook.InvocationPrepared, EffectDisposition: hook.EffectNone, ContextDisposition: hook.ContextNone,
			}
			entryCopy := entry
			changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
			occurrence.entries = append(occurrence.entries, entry)
			nextSequence++
		}
		occurrences = append(occurrences, occurrence)
	}
	return occurrences, changes, nil
}

func toolResultView(r *runtimeInstance, invocationID hook.InvocationID, revision agent.Revision, call agent.ToolCall, result tool.ToolResult, nextStep agent.StepID) hook.ToolResultView {
	return hook.ToolResultView{
		InvocationID: invocationID, SessionID: r.id(), AgentID: r.agentID, WorkspaceID: r.workspaceID, Revision: revision,
		RunID: call.RunID, StepID: call.StepID, NextStepID: nextStep, MessageID: call.MessageID, ToolCallID: call.ID,
		ToolKey: call.Name, Arguments: append([]byte(nil), call.Arguments...), Result: cloneRuntimeToolResult(result),
	}
}

func (r *runtimeInstance) evaluateToolResultHooks(run *activeRun, occurrences []toolResultHookOccurrence) (*toolResultHookFailure, bool, error) {
	if len(occurrences) == 0 {
		return nil, false, nil
	}
	succeeded := make([]session.ExtensionJournalEntry, 0)
	for occurrenceIndex := range occurrences {
		occurrence := &occurrences[occurrenceIndex]
		for entryIndex := range occurrence.entries {
			if run.ctx.Err() != nil {
				if err := r.settleToolResultHookAbort(run, succeeded, remainingToolResultEntries(occurrences, occurrenceIndex, entryIndex), "tool_result_hook_canceled"); err != nil {
					settleErr := r.settleToolResultHooksAfterPersistenceError(run, allToolResultEntryIDs(occurrences), "tool_result_hook_canceled")
					return nil, true, errors.Join(err, settleErr)
				}
				return nil, true, nil
			}
			entry := occurrence.entries[entryIndex]
			binding := occurrence.bindings[entryIndex]
			view := toolResultView(r, entry.InvocationID, entry.PreparedRevision, occurrence.call, occurrence.result, occurrence.nextStep)
			fingerprint, err := hook.FingerprintTypedInput(view)
			if err != nil || fingerprint.Digest != entry.InputDigest || binding.descriptor != entry.Descriptor {
				if settleErr := r.settleToolResultHookAbort(run, succeeded, remainingToolResultEntries(occurrences, occurrenceIndex, entryIndex), "tool_result_hook_definition_mismatch"); settleErr != nil {
					return nil, false, settleErr
				}
				return &toolResultHookFailure{code: "tool_result_hook_definition_mismatch", reason: "prepared tool result hooks do not match the current application"}, false, nil
			}
			pending := entry
			pending.Status, pending.PendingAt = hook.InvocationPending, time.Now().UTC()
			if pending.PendingAt.Before(pending.PreparedAt) {
				pending.PendingAt = pending.PreparedAt
			}
			if err := r.commitToolResultHookEntries(run, "tool-result-hook-pending", pending); err != nil {
				settleErr := r.settleToolResultHooksAfterPersistenceError(run, allToolResultEntryIDs(occurrences), "tool_result_hook_persistence_failed")
				return nil, false, errors.Join(err, settleErr)
			}
			result, invocationErr := invokeToolResultHook(run.ctx, binding.hook, view)
			failure := classifyToolResultHookFailure(invocationErr)
			if invocationErr == nil {
				if validateErr := result.Validate(r.id()); validateErr != nil || validateToolResultContextTarget(result.Context, r.id(), run.id, occurrence.nextStep) != nil {
					failure = classifiedToolResultHookFailure{status: hook.InvocationFailed, code: agent.CodeExtensionFailed, reason: "tool result hook returned invalid context"}
				}
			}
			terminal := pending
			terminal.FinishedAt, terminal.EffectDisposition = time.Now().UTC(), hook.EffectPending
			if terminal.FinishedAt.Before(terminal.PendingAt) {
				terminal.FinishedAt = terminal.PendingAt
			}
			if failure.code != "" {
				terminal.Status, terminal.ErrorCode, terminal.ErrorReason = failure.status, failure.code, failure.reason
			} else {
				terminal.Status = hook.InvocationSucceeded
				terminal.Result = &hook.InvocationResult{Decision: hook.DecisionNone}
				if len(result.Context) > 0 {
					terminal.ContextInputs = cloneRuntimeInputs(result.Context)
					contextFingerprint, err := hook.FingerprintTypedInput(terminal.ContextInputs)
					if err != nil {
						return nil, false, err
					}
					terminal.ContextDisposition, terminal.ContextDigest, terminal.ContextBytes = hook.ContextPending, contextFingerprint.Digest, contextFingerprint.Bytes
				}
			}
			if err := r.commitToolResultHookEntries(run, "tool-result-hook-terminal", terminal); err != nil {
				settleErr := r.settleToolResultHooksAfterPersistenceError(run, allToolResultEntryIDs(occurrences), "tool_result_hook_persistence_unknown")
				return nil, false, errors.Join(err, settleErr)
			}
			occurrence.entries[entryIndex] = terminal
			if failure.code != "" {
				remaining := remainingToolResultEntries(occurrences, occurrenceIndex, entryIndex+1)
				if err := r.settleToolResultHookFailure(run, succeeded, terminal, remaining); err != nil {
					settleErr := r.settleToolResultHooksAfterPersistenceError(run, allToolResultEntryIDs(occurrences), "tool_result_hook_failure_commit_failed")
					return nil, false, errors.Join(err, settleErr)
				}
				if run.ctx.Err() != nil {
					return nil, true, nil
				}
				return &toolResultHookFailure{code: failure.code, reason: failure.reason}, false, nil
			}
			succeeded = append(succeeded, terminal)
		}
	}
	if run.ctx.Err() != nil {
		if err := r.settleToolResultHookAbort(run, succeeded, nil, "tool_result_hook_canceled"); err != nil {
			settleErr := r.settleToolResultHooksAfterPersistenceError(run, allToolResultEntryIDs(occurrences), "tool_result_hook_canceled")
			return nil, true, errors.Join(err, settleErr)
		}
		return nil, true, nil
	}
	if err := r.consumeToolResultHookContexts(run, succeeded); err != nil {
		settleErr := r.settleToolResultHooksAfterPersistenceError(run, allToolResultEntryIDs(occurrences), "tool_result_hook_context_commit_failed")
		return nil, false, errors.Join(err, settleErr)
	}
	return nil, false, nil
}

func validateToolResultContextTarget(inputs []model.Input, sessionID agent.SessionID, runID agent.RunID, stepID agent.StepID) error {
	for _, input := range inputs {
		if input.Message == nil || input.ToolCall != nil || input.ToolResult != nil || input.Message.SessionID != sessionID ||
			input.Message.RunID != runID || input.Message.StepID != stepID || input.Message.Role != agent.RoleUser {
			return fmt.Errorf("tool result hook context is not bound to the allocated next step")
		}
	}
	return nil
}

func remainingToolResultEntries(occurrences []toolResultHookOccurrence, occurrenceIndex, entryIndex int) []session.ExtensionJournalEntry {
	remaining := make([]session.ExtensionJournalEntry, 0)
	for index := occurrenceIndex; index < len(occurrences); index++ {
		start := 0
		if index == occurrenceIndex {
			start = entryIndex
		}
		if start < len(occurrences[index].entries) {
			remaining = append(remaining, occurrences[index].entries[start:]...)
		}
	}
	return remaining
}

func allToolResultEntryIDs(occurrences []toolResultHookOccurrence) map[hook.InvocationID]struct{} {
	ids := make(map[hook.InvocationID]struct{})
	for _, occurrence := range occurrences {
		for _, entry := range occurrence.entries {
			ids[entry.InvocationID] = struct{}{}
		}
	}
	return ids
}

func (r *runtimeInstance) commitToolResultHookEntries(run *activeRun, operation string, entries ...session.ExtensionJournalEntry) error {
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

func (r *runtimeInstance) consumeToolResultHookContexts(run *activeRun, entries []session.ExtensionJournalEntry) error {
	changes := make([]session.Change, 0, len(entries)*2)
	for _, entry := range entries {
		if entry.Status != hook.InvocationSucceeded || entry.EffectDisposition != hook.EffectPending {
			continue
		}
		entry.EffectDisposition = hook.EffectApplied
		if entry.ContextDisposition == hook.ContextPending {
			fact := session.ContextContributionFact{
				RunID: entry.RunID, StepID: entry.TargetStepID, SourceKey: fmt.Sprintf("hook:%s:%s", entry.Descriptor.Key, entry.InvocationID),
				Inputs: cloneRuntimeInputs(entry.ContextInputs),
			}
			changes = append(changes, session.Change{Kind: session.AppendContextContribution, ContextContribution: &fact})
			entry.ContextDisposition, entry.ContextInputs = hook.ContextConsumed, nil
		}
		entryCopy := entry
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
	}
	return r.commitToolResultHookChanges(run, "tool-result-hook-consume", changes)
}

func (r *runtimeInstance) settleToolResultHookFailure(run *activeRun, succeeded []session.ExtensionJournalEntry, failed session.ExtensionJournalEntry, remaining []session.ExtensionJournalEntry) error {
	changes := finalizeToolResultHookEntries(succeeded, hook.EffectDiscarded)
	failed.EffectDisposition = hook.EffectApplied
	if failed.ContextDisposition == hook.ContextPending {
		failed.ContextDisposition, failed.ContextInputs = hook.ContextDiscarded, nil
	}
	failedCopy := failed
	changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &failedCopy})
	changes = append(changes, cancelToolResultHookEntries(remaining, "tool_result_hook_batch_aborted")...)
	return r.commitToolResultHookChanges(run, "tool-result-hook-failure", changes)
}

func (r *runtimeInstance) settleToolResultHookAbort(run *activeRun, succeeded, remaining []session.ExtensionJournalEntry, code agent.ErrorCode) error {
	changes := finalizeToolResultHookEntries(succeeded, hook.EffectDiscarded)
	changes = append(changes, cancelToolResultHookEntries(remaining, code)...)
	return r.commitToolResultHookChanges(run, "tool-result-hook-abort", changes)
}

func finalizeToolResultHookEntries(entries []session.ExtensionJournalEntry, disposition hook.EffectDisposition) []session.Change {
	changes := make([]session.Change, 0, len(entries))
	for _, entry := range entries {
		if entry.Status != hook.InvocationSucceeded || entry.EffectDisposition != hook.EffectPending {
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

func cancelToolResultHookEntries(entries []session.ExtensionJournalEntry, code agent.ErrorCode) []session.Change {
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

func (r *runtimeInstance) commitToolResultHookChanges(run *activeRun, operation string, changes []session.Change) error {
	if len(changes) == 0 {
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
	_, err = r.commitLocked(context.Background(), snapshot.Revision, operation, changes)
	return err
}

func (r *runtimeInstance) settleToolResultHooksAfterPersistenceError(run *activeRun, ids map[hook.InvocationID]struct{}, code agent.ErrorCode) error {
	if len(ids) == 0 {
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
	now := time.Now().UTC()
	unknownChanges := make([]session.Change, 0)
	for _, existing := range snapshot.ExtensionJournal {
		if _, selected := ids[existing.InvocationID]; !selected || existing.Status != hook.InvocationPending {
			continue
		}
		entry := existing
		entry.Status, entry.FinishedAt = hook.InvocationOutcomeUnknown, now
		if entry.FinishedAt.Before(entry.PendingAt) {
			entry.FinishedAt = entry.PendingAt
		}
		entry.ErrorCode, entry.ErrorReason = code, "tool result hook persistence outcome is unknown"
		entry.EffectDisposition = hook.EffectPending
		entryCopy := entry
		unknownChanges = append(unknownChanges, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
	}
	if len(unknownChanges) > 0 {
		if _, err := r.commitLocked(context.Background(), snapshot.Revision, "tool-result-hook-persistence-unknown", unknownChanges); err != nil {
			return err
		}
		snapshot, err = r.viewLocked(context.Background())
		if err != nil {
			return err
		}
	}
	finalChanges := make([]session.Change, 0)
	for _, existing := range snapshot.ExtensionJournal {
		if _, selected := ids[existing.InvocationID]; !selected {
			continue
		}
		entry := existing
		switch {
		case entry.Status == hook.InvocationPrepared:
			entry.Status, entry.FinishedAt = hook.InvocationCanceled, now
			if entry.FinishedAt.Before(entry.PreparedAt) {
				entry.FinishedAt = entry.PreparedAt
			}
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
		entryCopy := entry
		finalChanges = append(finalChanges, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
	}
	if len(finalChanges) == 0 {
		return nil
	}
	_, err = r.commitLocked(context.Background(), snapshot.Revision, "tool-result-hook-persistence-settle", finalChanges)
	return err
}

func invokeToolResultHook(ctx context.Context, resultHook hook.ToolResultHook, view hook.ToolResultView) (result hook.ToolResultHookResult, err error) {
	defer func() {
		if recover() != nil {
			result = hook.ToolResultHookResult{}
			err = errors.New("tool result hook panicked")
		}
	}()
	return resultHook.Evaluate(ctx, view)
}

type classifiedToolResultHookFailure struct {
	status hook.InvocationStatus
	code   agent.ErrorCode
	reason string
}

func classifyToolResultHookFailure(err error) classifiedToolResultHookFailure {
	if err == nil {
		return classifiedToolResultHookFailure{}
	}
	failure := classifiedToolResultHookFailure{status: hook.InvocationFailed, code: agent.CodeExtensionFailed, reason: "tool result hook failed"}
	var declared *hook.InvocationFailure
	if errors.As(err, &declared) && declared.Validate() == nil {
		failure.status, failure.code, failure.reason = declared.Status, declared.Code, declared.Reason
	} else if errors.Is(err, context.Canceled) {
		failure.status, failure.code, failure.reason = hook.InvocationCanceled, agent.CodeCanceled, "tool result hook was canceled"
	}
	return failure
}

func toolResultHookTermination(failure *toolResultHookFailure) *session.RunTermination {
	if failure == nil {
		return nil
	}
	termination := &session.RunTermination{Source: session.TerminationExtension, Kind: agent.ErrorUnavailable, Code: failure.code, SafeMessage: failure.reason}
	if termination.Validate() != nil {
		termination.SafeMessage = ""
	}
	return termination
}
