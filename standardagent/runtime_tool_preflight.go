package standardagent

import (
	"context"
	"errors"
	"time"

	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/session"
	"github.com/LyleLiu666/agentSlot/tool"
)

type toolPreflightBinding struct {
	preflight  hook.ToolPreflight
	descriptor hook.ExtensionDescriptor
	scope      hook.ToolScope
}

func cloneToolScope(source hook.ToolScope) hook.ToolScope {
	return hook.ToolScope{All: source.All, ToolKeys: append([]string(nil), source.ToolKeys...)}
}

func (r *runtimeInstance) appendToolPreflightReservations(call agent.ToolCall, revision agent.Revision, nextSequence *session.ExtensionSequence, changes []session.Change) ([]session.Change, error) {
	if len(r.components.toolPreflights) == 0 {
		return changes, nil
	}
	if nextSequence == nil || *nextSequence == 0 {
		return nil, agent.NewError(agent.ErrorInternal, "standardagent.tool_preflight", "extension sequence allocator is invalid", nil)
	}
	now := time.Now().UTC()
	for _, binding := range r.components.toolPreflights {
		if !binding.scope.Matches(call.Name) {
			continue
		}
		view := hook.ToolPreflightView{
			InvocationID: hook.InvocationID(r.nextID("preflight")), SessionID: r.id(), AgentID: r.agentID, WorkspaceID: r.workspaceID,
			Revision: revision, RunID: call.RunID, StepID: call.StepID, MessageID: call.MessageID, ToolCallID: call.ID,
			ToolKey: call.Name, Arguments: append([]byte(nil), call.Arguments...),
		}
		fingerprint, err := hook.FingerprintTypedInput(view)
		if err != nil {
			return nil, err
		}
		entry := session.ExtensionJournalEntry{
			InvocationID: view.InvocationID, Sequence: *nextSequence,
			Descriptor: binding.descriptor, Boundary: hook.BoundaryToolPreflight,
			SessionID: r.id(), RunID: call.RunID, StepID: call.StepID, MessageID: call.MessageID, ToolCallID: call.ID,
			InputDigest: fingerprint.Digest, PreparedRevision: revision, PreparedAt: now,
			Status: hook.InvocationPrepared, EffectDisposition: hook.EffectNone, ContextDisposition: hook.ContextNone,
		}
		entryCopy := entry
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
		*nextSequence++
	}
	return changes, nil
}

type toolPreflightBatchFailure struct {
	code   agent.ErrorCode
	reason string
}

func (r *runtimeInstance) evaluateToolPreflights(run *activeRun, calls []agent.ToolCall) ([]toolPreflightAuthorization, *toolPreflightBatchFailure, error) {
	if len(r.components.toolPreflights) == 0 {
		return nil, nil, nil
	}
	snapshot, err := r.toolPreflightSnapshot(run)
	if err != nil {
		return nil, nil, err
	}
	entriesByCall := make(map[agent.ToolCallID][]session.ExtensionJournalEntry, len(calls))
	journal := snapshot.ExtensionJournal
	if run.recoveredToolPreflights != nil {
		journal = run.recoveredToolPreflights
		run.recoveredToolPreflights = nil
	}
	for _, entry := range journal {
		if entry.Boundary == hook.BoundaryToolPreflight && entry.RunID == run.id {
			entriesByCall[entry.ToolCallID] = append(entriesByCall[entry.ToolCallID], entry)
		}
	}
	authorizations := make([]toolPreflightAuthorization, len(calls))
	succeeded := make([]session.ExtensionJournalEntry, 0)
	pipelinedPending := make(map[hook.InvocationID]struct{})
	for callIndex, call := range calls {
		expected := matchingToolPreflights(r.components.toolPreflights, call.Name)
		entries := entriesByCall[call.ID]
		if len(entries) == 0 && len(expected) > 0 {
			snapshot, entries, err = r.supplementToolPreflightReservations(run, snapshot, call, expected)
			if err != nil {
				return nil, nil, err
			}
			entriesByCall[call.ID] = entries
		}
		if err := validateToolPreflightReservations(r, call, expected, entries); err != nil {
			unsettled := append([]session.ExtensionJournalEntry(nil), succeeded...)
			unsettled = append(unsettled, remainingToolPreflightEntries(calls[callIndex:], entriesByCall)...)
			if settleErr := r.settleToolPreflightDefinitionMismatch(run, unsettled); settleErr != nil {
				return r.toolPreflightPersistenceFailure(run, settleErr)
			}
			return nil, &toolPreflightBatchFailure{
				code: "tool_preflight_definition_mismatch", reason: "prepared tool preflight definitions do not match the current application",
			}, nil
		}
		if validation := r.components.dispatcher.validateCall(call); validation != nil {
			if err := r.cancelToolPreflightEntries(run, entries, "tool_preflight_not_applicable"); err != nil {
				return r.toolPreflightPersistenceFailure(run, err)
			}
			continue
		}
		for entryIndex := range entries {
			entry := entries[entryIndex]
			binding := expected[entryIndex]
			switch {
			case entry.Status == hook.InvocationSucceeded && entry.EffectDisposition == hook.EffectApplied:
				authorizations[callIndex].merge(entry.Result)
				if entry.Result.Decision == hook.DecisionDeny {
					if err := r.cancelToolPreflightEntries(run, entries[entryIndex+1:], "tool_preflight_short_circuited"); err != nil {
						return r.toolPreflightPersistenceFailure(run, err)
					}
					break
				}
			case entry.Status == hook.InvocationSucceeded && entry.EffectDisposition == hook.EffectPending:
				succeeded = append(succeeded, entry)
				authorizations[callIndex].merge(entry.Result)
				if entry.Result.Decision == hook.DecisionDeny {
					if err := r.cancelToolPreflightEntries(run, entries[entryIndex+1:], "tool_preflight_short_circuited"); err != nil {
						return r.toolPreflightPersistenceFailure(run, err)
					}
				}
			case entry.Status == hook.InvocationPrepared || pipelinedToolPreflightPending(entry, pipelinedPending):
				pending := entry
				if entry.Status == hook.InvocationPrepared {
					pending.Status = hook.InvocationPending
					pending.PendingAt = time.Now().UTC()
					if err := r.commitToolPreflightEntries(run, "tool-preflight-pending", pending); err != nil {
						return r.toolPreflightPersistenceFailure(run, err)
					}
					entries[entryIndex] = pending
				}
				view := toolPreflightView(r, pending, call)
				result, invokeErr := invokeToolPreflight(run.ctx, binding.preflight, view)
				if invokeErr != nil || result.Validate() != nil {
					failure := classifyToolPreflightFailure(invokeErr)
					failed := pending
					failed.Status, failed.FinishedAt = failure.status, time.Now().UTC()
					failed.ErrorCode, failed.ErrorReason = failure.code, failure.reason
					failed.EffectDisposition = hook.EffectPending
					if err := r.failToolPreflightBatch(run, failed, succeeded, entries[entryIndex+1:], calls[callIndex+1:], entriesByCall); err != nil {
						return r.toolPreflightPersistenceFailure(run, err)
					}
					return nil, &toolPreflightBatchFailure{code: failure.code, reason: failure.reason}, nil
				}
				finished := pending
				finished.Status, finished.FinishedAt = hook.InvocationSucceeded, time.Now().UTC()
				finished.Result = &hook.InvocationResult{Decision: result.Decision, Reason: result.Reason}
				finished.EffectDisposition = hook.EffectPending
				finishedTransitions := []session.ExtensionJournalEntry{finished}
				if result.Decision != hook.DecisionDeny && entryIndex+1 < len(entries) {
					next := entries[entryIndex+1]
					next.Status, next.PendingAt = hook.InvocationPending, time.Now().UTC()
					if next.PendingAt.Before(next.PreparedAt) {
						next.PendingAt = next.PreparedAt
					}
					finishedTransitions = append(finishedTransitions, next)
					entries[entryIndex+1] = next
					pipelinedPending[next.InvocationID] = struct{}{}
				}
				if err := r.commitToolPreflightEntries(run, "tool-preflight-finished", finishedTransitions...); err != nil {
					return r.toolPreflightPersistenceFailure(run, err)
				}
				entries[entryIndex] = finished
				succeeded = append(succeeded, finished)
				authorizations[callIndex].merge(finished.Result)
				if result.Decision == hook.DecisionDeny {
					if err := r.cancelToolPreflightEntries(run, entries[entryIndex+1:], "tool_preflight_short_circuited"); err != nil {
						return r.toolPreflightPersistenceFailure(run, err)
					}
					entryIndex = len(entries)
				}
			default:
				failure := classifiedToolPreflightFailure{status: entry.Status, code: entry.ErrorCode, reason: entry.ErrorReason}
				if !entry.Status.Terminal() || failure.code == "" {
					failure = classifyToolPreflightFailure(&hook.InvocationFailure{
						Status: hook.InvocationOutcomeUnknown, Code: "hook_preflight_outcome_unknown", Reason: "tool preflight outcome is unknown",
					})
				}
				if entry.Status.Terminal() && entry.EffectDisposition == hook.EffectPending {
					entry.EffectDisposition = hook.EffectApplied
					if err := r.commitToolPreflightEntries(run, "tool-preflight-unknown-applied", entry); err != nil {
						return r.toolPreflightPersistenceFailure(run, err)
					}
				}
				if err := r.failToolPreflightBatch(run, session.ExtensionJournalEntry{}, succeeded, entries[entryIndex+1:], calls[callIndex+1:], entriesByCall); err != nil {
					return r.toolPreflightPersistenceFailure(run, err)
				}
				return nil, &toolPreflightBatchFailure{code: failure.code, reason: failure.reason}, nil
			}
			if authorizations[callIndex].denied {
				break
			}
		}
	}
	if err := r.finalizeToolPreflightEffects(run, succeeded, hook.EffectApplied); err != nil {
		return r.toolPreflightPersistenceFailure(run, err)
	}
	return authorizations, nil, nil
}

func pipelinedToolPreflightPending(entry session.ExtensionJournalEntry, pending map[hook.InvocationID]struct{}) bool {
	if entry.Status != hook.InvocationPending {
		return false
	}
	_, ok := pending[entry.InvocationID]
	return ok
}

func (r *runtimeInstance) toolPreflightPersistenceFailure(run *activeRun, cause error) ([]toolPreflightAuthorization, *toolPreflightBatchFailure, error) {
	if settleErr := r.settleToolPreflightsAfterPersistenceError(run, "tool_preflight_persistence_unknown"); settleErr != nil {
		return nil, nil, errors.Join(cause, settleErr)
	}
	return nil, &toolPreflightBatchFailure{code: "hook_outcome_unknown", reason: "tool preflight persistence outcome is unknown"}, nil
}

func (r *runtimeInstance) settleToolPreflightsAfterPersistenceError(run *activeRun, code agent.ErrorCode) error {
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
		if existing.Boundary != hook.BoundaryToolPreflight || existing.RunID != run.id || existing.Status != hook.InvocationPending {
			continue
		}
		entry := existing
		entry.Status, entry.FinishedAt = hook.InvocationOutcomeUnknown, now
		if entry.FinishedAt.Before(entry.PendingAt) {
			entry.FinishedAt = entry.PendingAt
		}
		entry.ErrorCode, entry.ErrorReason = code, "tool preflight persistence outcome is unknown"
		entry.EffectDisposition = hook.EffectPending
		entryCopy := entry
		unknownChanges = append(unknownChanges, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
	}
	if len(unknownChanges) > 0 {
		if _, err := r.commitLocked(context.Background(), snapshot.Revision, "tool-preflight-persistence-unknown", unknownChanges); err != nil {
			return err
		}
		snapshot, err = r.viewLocked(context.Background())
		if err != nil {
			return err
		}
	}
	finalChanges := make([]session.Change, 0)
	for _, existing := range snapshot.ExtensionJournal {
		if existing.Boundary != hook.BoundaryToolPreflight || existing.RunID != run.id {
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
		default:
			continue
		}
		entryCopy := entry
		finalChanges = append(finalChanges, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
	}
	if len(finalChanges) == 0 {
		return nil
	}
	_, err = r.commitLocked(context.Background(), snapshot.Revision, "tool-preflight-persistence-settle", finalChanges)
	return err
}

func remainingToolPreflightEntries(calls []agent.ToolCall, entriesByCall map[agent.ToolCallID][]session.ExtensionJournalEntry) []session.ExtensionJournalEntry {
	var entries []session.ExtensionJournalEntry
	for _, call := range calls {
		entries = append(entries, entriesByCall[call.ID]...)
	}
	return entries
}

func (r *runtimeInstance) settleToolPreflightDefinitionMismatch(run *activeRun, entries []session.ExtensionJournalEntry) error {
	updates := make([]session.ExtensionJournalEntry, 0, len(entries))
	seen := make(map[hook.InvocationID]struct{}, len(entries))
	for _, entry := range entries {
		if _, duplicate := seen[entry.InvocationID]; duplicate {
			continue
		}
		seen[entry.InvocationID] = struct{}{}
		switch {
		case entry.Status == hook.InvocationPrepared:
			entry.Status, entry.FinishedAt = hook.InvocationCanceled, time.Now().UTC()
			entry.ErrorCode, entry.EffectDisposition = "tool_preflight_definition_mismatch", hook.EffectDiscarded
			updates = append(updates, entry)
		case entry.Status.Terminal() && entry.EffectDisposition == hook.EffectPending:
			if entry.Status == hook.InvocationSucceeded {
				entry.EffectDisposition = hook.EffectDiscarded
			} else {
				entry.EffectDisposition = hook.EffectApplied
			}
			updates = append(updates, entry)
		}
	}
	return r.commitToolPreflightEntries(run, "tool-preflight-definition-mismatch", updates...)
}

func matchingToolPreflights(bindings []toolPreflightBinding, key string) []toolPreflightBinding {
	result := make([]toolPreflightBinding, 0, len(bindings))
	for _, binding := range bindings {
		if binding.scope.Matches(key) {
			result = append(result, binding)
		}
	}
	return result
}

func validateToolPreflightReservations(r *runtimeInstance, call agent.ToolCall, expected []toolPreflightBinding, entries []session.ExtensionJournalEntry) error {
	if len(expected) != len(entries) {
		return agent.NewError(agent.ErrorConflict, "standardagent.tool_preflight", "prepared ToolPreflight set does not match the assembled chain", nil)
	}
	for index, entry := range entries {
		if entry.Descriptor != expected[index].descriptor || entry.ToolCallID != call.ID || entry.RunID != call.RunID || entry.StepID != call.StepID {
			return agent.NewError(agent.ErrorConflict, "standardagent.tool_preflight", "prepared ToolPreflight identity does not match the assembled chain", nil)
		}
		view := toolPreflightView(r, entry, call)
		fingerprint, err := hook.FingerprintTypedInput(view)
		if err != nil || fingerprint.Digest != entry.InputDigest {
			return agent.NewError(agent.ErrorConflict, "standardagent.tool_preflight", "prepared ToolPreflight input changed", err)
		}
	}
	return nil
}

func toolPreflightView(r *runtimeInstance, entry session.ExtensionJournalEntry, call agent.ToolCall) hook.ToolPreflightView {
	return hook.ToolPreflightView{
		InvocationID: entry.InvocationID, SessionID: r.id(), AgentID: r.agentID, WorkspaceID: r.workspaceID,
		Revision: entry.PreparedRevision, RunID: call.RunID, StepID: call.StepID, MessageID: call.MessageID, ToolCallID: call.ID,
		ToolKey: call.Name, Arguments: append([]byte(nil), call.Arguments...),
	}
}

func (r *runtimeInstance) toolPreflightSnapshot(run *activeRun) (session.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != run || run.cancelRequested || r.closing {
		return session.Snapshot{}, context.Canceled
	}
	return r.viewLocked(context.Background())
}

func (r *runtimeInstance) supplementToolPreflightReservations(run *activeRun, snapshot session.Snapshot, call agent.ToolCall, expected []toolPreflightBinding) (session.Snapshot, []session.ExtensionJournalEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != run || run.cancelRequested || r.closing {
		return snapshot, nil, context.Canceled
	}
	latest, err := r.viewLocked(context.Background())
	if err != nil {
		return snapshot, nil, err
	}
	for _, entry := range latest.ExtensionJournal {
		if entry.Boundary == hook.BoundaryToolPreflight && entry.ToolCallID == call.ID {
			return snapshot, nil, agent.NewError(agent.ErrorConflict, "standardagent.tool_preflight", "partial ToolPreflight reservation set cannot be supplemented", nil)
		}
	}
	nextSequence := session.ExtensionSequence(len(latest.ExtensionJournal) + 1)
	changes, err := r.appendToolPreflightReservations(call, latest.Revision.Next(), &nextSequence, nil)
	if err != nil {
		return snapshot, nil, err
	}
	if len(changes) != len(expected) {
		return snapshot, nil, agent.NewError(agent.ErrorInternal, "standardagent.tool_preflight", "ToolPreflight scope changed after assembly", nil)
	}
	if _, err := r.commitLocked(context.Background(), latest.Revision, "tool-preflight-supplement", changes); err != nil {
		return snapshot, nil, err
	}
	updated, err := r.viewLocked(context.Background())
	if err != nil {
		return snapshot, nil, err
	}
	entries := make([]session.ExtensionJournalEntry, 0, len(expected))
	for _, entry := range updated.ExtensionJournal {
		if entry.Boundary == hook.BoundaryToolPreflight && entry.ToolCallID == call.ID {
			entries = append(entries, entry)
		}
	}
	return updated, entries, nil
}

func (r *runtimeInstance) commitToolPreflightEntries(run *activeRun, operation string, entries ...session.ExtensionJournalEntry) error {
	if len(entries) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != run || run.cancelRequested || r.closing {
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

func (r *runtimeInstance) cancelToolPreflightEntries(run *activeRun, entries []session.ExtensionJournalEntry, code agent.ErrorCode) error {
	updates := make([]session.ExtensionJournalEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Status != hook.InvocationPrepared {
			continue
		}
		entry.Status, entry.FinishedAt = hook.InvocationCanceled, time.Now().UTC()
		entry.ErrorCode, entry.EffectDisposition = code, hook.EffectDiscarded
		updates = append(updates, entry)
	}
	return r.commitToolPreflightEntries(run, "tool-preflight-canceled", updates...)
}

func (r *runtimeInstance) finalizeToolPreflightEffects(run *activeRun, entries []session.ExtensionJournalEntry, disposition hook.EffectDisposition) error {
	updates := make([]session.ExtensionJournalEntry, 0, len(entries))
	seen := make(map[hook.InvocationID]struct{}, len(entries))
	for _, entry := range entries {
		if _, duplicate := seen[entry.InvocationID]; duplicate || entry.Status != hook.InvocationSucceeded || entry.EffectDisposition != hook.EffectPending {
			continue
		}
		seen[entry.InvocationID] = struct{}{}
		entry.EffectDisposition = disposition
		updates = append(updates, entry)
	}
	return r.commitToolPreflightEntries(run, "tool-preflight-effect", updates...)
}

func (r *runtimeInstance) failToolPreflightBatch(run *activeRun, failed session.ExtensionJournalEntry, succeeded []session.ExtensionJournalEntry, currentRemaining []session.ExtensionJournalEntry, futureCalls []agent.ToolCall, byCall map[agent.ToolCallID][]session.ExtensionJournalEntry) error {
	if err := r.finalizeToolPreflightEffects(run, succeeded, hook.EffectDiscarded); err != nil {
		return err
	}
	if failed.InvocationID.Valid() {
		if err := r.commitToolPreflightEntries(run, "tool-preflight-failed", failed); err != nil {
			return err
		}
		failed.EffectDisposition = hook.EffectApplied
		if err := r.commitToolPreflightEntries(run, "tool-preflight-failure-applied", failed); err != nil {
			return err
		}
	}
	remaining := append([]session.ExtensionJournalEntry(nil), currentRemaining...)
	for _, call := range futureCalls {
		remaining = append(remaining, byCall[call.ID]...)
	}
	return r.cancelToolPreflightEntries(run, remaining, "tool_preflight_batch_aborted")
}

func invokeToolPreflight(ctx context.Context, preflight hook.ToolPreflight, view hook.ToolPreflightView) (result hook.ToolPreflightResult, err error) {
	defer func() {
		if recover() != nil {
			result = hook.ToolPreflightResult{}
			err = errors.New("tool preflight panicked")
		}
	}()
	return preflight.Evaluate(ctx, view)
}

type classifiedToolPreflightFailure struct {
	status hook.InvocationStatus
	code   agent.ErrorCode
	reason string
}

func classifyToolPreflightFailure(err error) classifiedToolPreflightFailure {
	failure := classifiedToolPreflightFailure{status: hook.InvocationFailed, code: "hook_preflight_failed", reason: "tool preflight failed"}
	var declared *hook.InvocationFailure
	if errors.As(err, &declared) && declared.Validate() == nil {
		failure.status, failure.code, failure.reason = declared.Status, declared.Code, declared.Reason
	}
	return failure
}

func preflightFailedResults(calls []agent.ToolCall) []tool.ToolResult {
	results := make([]tool.ToolResult, len(calls))
	for index, call := range calls {
		results[index] = failedToolResult(call.ID, "tool_preflight_failed", "tool batch stopped because authorization advice failed")
	}
	return results
}

func toolPreflightTermination(failure *toolPreflightBatchFailure) *session.RunTermination {
	if failure == nil {
		return nil
	}
	termination := &session.RunTermination{Source: session.TerminationExtension, Kind: agent.ErrorUnavailable, Code: failure.code, SafeMessage: failure.reason}
	if termination.Validate() != nil {
		termination.SafeMessage = ""
	}
	return termination
}
