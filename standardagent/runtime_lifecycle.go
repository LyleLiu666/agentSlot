package standardagent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/observe"
	"github.com/LyleLiu666/agentSlot/session"
)

type sessionLifecycleBinding struct {
	lifecycle  hook.SessionLifecycle
	descriptor hook.ExtensionDescriptor
	scope      hook.LifecycleScope
}

type sessionLifecycleOccurrence struct {
	phase   hook.LifecyclePhase
	kind    hook.OpenKind
	actor   agent.ActorIdentity
	entries []session.ExtensionJournalEntry
	views   []hook.SessionLifecycleView
}

func cloneLifecycleScope(source hook.LifecycleScope) hook.LifecycleScope {
	return hook.LifecycleScope{Phases: append([]hook.LifecyclePhase(nil), source.Phases...)}
}

func matchingSessionLifecycles(bindings []sessionLifecycleBinding, phase hook.LifecyclePhase) []sessionLifecycleBinding {
	result := make([]sessionLifecycleBinding, 0, len(bindings))
	for _, binding := range bindings {
		if binding.scope.Matches(phase) {
			result = append(result, binding)
		}
	}
	return result
}

func (r *runtimeInstance) open(ctx context.Context, kind hook.OpenKind) error {
	r.openOnce.Do(func() {
		r.openErr = r.finishOpen(ctx, kind)
		if r.openErr == nil && r.openOccurrence != nil {
			r.openDiagnosticView, r.openErr = r.lifecycleDiagnostics(r.openOccurrence.entries)
		}
		if r.openErr == nil {
			r.components.observations.publishTrace(observe.TraceRecord{
				Kind: observe.TraceRuntimeOpened, At: time.Now().UTC(),
				Identity: observe.Identity{SessionID: r.id(), Actor: serviceObservationActor("agent-runtime")},
			})
		}
		close(r.openDone)
	})
	return r.awaitOpen(ctx)
}

func (r *runtimeInstance) abortOpen(cause error) {
	r.openOnce.Do(func() {
		r.openErr = cause
		close(r.openDone)
	})
}

func (r *runtimeInstance) awaitOpen(ctx context.Context) error {
	select {
	case <-r.openDone:
		return r.openErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *runtimeInstance) finishOpen(ctx context.Context, kind hook.OpenKind) error {
	if !kind.Valid() {
		return agent.NewError(agent.ErrorInternal, "standardagent.session_lifecycle", "Runtime received an invalid open kind", nil)
	}
	snapshot, err := r.session.View(context.Background())
	if err != nil {
		return err
	}
	snapshot, err = r.recoverSessionLifecycleEntries(snapshot)
	if err != nil {
		return err
	}
	occurrence, err := r.prepareSessionLifecycle(snapshot, hook.LifecycleOpen, kind, agent.ActorIdentity{})
	if err != nil {
		return err
	}
	r.openOccurrence = occurrence
	if occurrence != nil {
		if err := r.executeSessionLifecycle(ctx, occurrence); err != nil {
			var componentFailure lifecycleFailure
			if !errors.As(err, &componentFailure) {
				return err
			}
		}
	}
	snapshot, err = r.session.View(context.Background())
	if err != nil {
		return err
	}
	snapshot, err = r.recoverInputGateEntries(context.Background(), snapshot)
	if err != nil {
		return err
	}
	snapshot, err = r.recoverToolResultHookEntries(context.Background(), snapshot)
	if err != nil {
		return err
	}
	if err := r.restorePreparedRun(snapshot); err != nil {
		return err
	}
	r.mu.Lock()
	if r.state == runtimeOpening {
		r.state = runtimeIdle
	}
	r.mu.Unlock()
	return nil
}

func (r *runtimeInstance) openDiagnostics() []session.ExtensionDiagnostic {
	return append([]session.ExtensionDiagnostic(nil), r.openDiagnosticView...)
}

// beginSynchronousExtensionLocked registers Prompt-bound work before the
// Runtime mutex is released. Explicit close therefore cannot overtake an
// already prepared invocation: it cancels the derived context, waits for the
// invocation to settle its journal entry, and only then evaluates SessionEnd.
func (r *runtimeInstance) beginSynchronousExtensionLocked(parent context.Context) (context.Context, func()) {
	r.extensionWG.Add(1)
	ctx, cancel := context.WithCancel(parent)
	stopLifetimeCancellation := context.AfterFunc(r.extensionCtx, cancel)
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			stopLifetimeCancellation()
			cancel()
			r.extensionWG.Done()
		})
	}
}

func (r *runtimeInstance) recoverSessionLifecycleEntries(snapshot session.Snapshot) (session.Snapshot, error) {
	changes := make([]session.Change, 0)
	now := time.Now().UTC()
	for _, current := range snapshot.ExtensionJournal {
		if current.Boundary != hook.BoundarySessionLifecycle {
			continue
		}
		entry := current
		switch entry.Status {
		case hook.InvocationPrepared:
			entry.Status, entry.FinishedAt = hook.InvocationCanceled, now
			if entry.FinishedAt.Before(entry.PreparedAt) {
				entry.FinishedAt = entry.PreparedAt
			}
			entry.ErrorCode, entry.ErrorReason = "session_lifecycle_recovery_canceled", "abandoned lifecycle invocation was canceled during recovery"
			entry.EffectDisposition = hook.EffectDiscarded
		case hook.InvocationPending:
			entry.Status, entry.FinishedAt = hook.InvocationOutcomeUnknown, now
			if entry.FinishedAt.Before(entry.PendingAt) {
				entry.FinishedAt = entry.PendingAt
			}
			entry.ErrorCode, entry.ErrorReason = "hook_outcome_unknown", "lifecycle outcome is unknown after process recovery"
			entry.EffectDisposition = hook.EffectDiscarded
		default:
			if !entry.Status.Terminal() || entry.EffectDisposition != hook.EffectPending {
				continue
			}
			// A terminal pending effect belongs to an occurrence that never
			// reached its aggregate apply commit. Publishing one member's
			// context would expose a partial chain after recovery.
			entry.EffectDisposition = hook.EffectDiscarded
		}
		if entry.ContextDisposition == hook.ContextPending && entry.EffectDisposition != hook.EffectApplied {
			entry.ContextDisposition, entry.ContextInputs = hook.ContextDiscarded, nil
		}
		copy := entry
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &copy})
	}
	if len(changes) == 0 {
		return snapshot, nil
	}
	if _, err := r.commitLocked(context.Background(), snapshot.Revision, "session-lifecycle-recovery", changes); err != nil {
		return session.Snapshot{}, err
	}
	return r.session.View(context.Background())
}

func (r *runtimeInstance) prepareSessionLifecycle(snapshot session.Snapshot, phase hook.LifecyclePhase, kind hook.OpenKind, actor agent.ActorIdentity) (*sessionLifecycleOccurrence, error) {
	bindings := matchingSessionLifecycles(r.components.sessionLifecycles, phase)
	if len(bindings) == 0 {
		return nil, nil
	}
	preparedRevision := snapshot.Revision.Next()
	now := time.Now().UTC()
	occurrence := &sessionLifecycleOccurrence{
		phase: phase, kind: kind, actor: actor,
		entries: make([]session.ExtensionJournalEntry, len(bindings)), views: make([]hook.SessionLifecycleView, len(bindings)),
	}
	changes := make([]session.Change, 0, len(bindings))
	for index, binding := range bindings {
		view := hook.SessionLifecycleView{
			InvocationID: hook.InvocationID(r.nextID("lifecycle")), SessionID: r.id(), AgentID: r.agentID, WorkspaceID: r.workspaceID,
			Revision: preparedRevision, Phase: phase, OpenKind: kind,
		}
		if err := view.Validate(); err != nil {
			return nil, agent.NewError(agent.ErrorInternal, "standardagent.session_lifecycle", "Runtime constructed an invalid lifecycle view", err)
		}
		fingerprint, err := hook.FingerprintTypedInput(view)
		if err != nil {
			return nil, err
		}
		entry := session.ExtensionJournalEntry{
			InvocationID: view.InvocationID, Sequence: session.ExtensionSequence(len(snapshot.ExtensionJournal) + index + 1),
			Descriptor: binding.descriptor, Boundary: hook.BoundarySessionLifecycle,
			SessionID: r.id(), LifecyclePhase: phase, LifecycleOpenKind: kind,
			InputDigest: fingerprint.Digest, PreparedRevision: preparedRevision, PreparedAt: now,
			Status: hook.InvocationPrepared, EffectDisposition: hook.EffectNone, ContextDisposition: hook.ContextNone,
		}
		occurrence.entries[index], occurrence.views[index] = entry, view
		copy := entry
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &copy})
	}
	if _, err := r.commitLockedAs(context.Background(), snapshot.Revision, "session-lifecycle-prepare", actor, changes); err != nil {
		return nil, err
	}
	return occurrence, nil
}

func (r *runtimeInstance) executeSessionLifecycle(ctx context.Context, occurrence *sessionLifecycleOccurrence) error {
	bindings := matchingSessionLifecycles(r.components.sessionLifecycles, occurrence.phase)
	if len(bindings) != len(occurrence.entries) {
		entry := occurrence.entries[0]
		entry.Status, entry.FinishedAt, entry.EffectDisposition = hook.InvocationFailed, time.Now().UTC(), hook.EffectPending
		if entry.FinishedAt.Before(entry.PreparedAt) {
			entry.FinishedAt = entry.PreparedAt
		}
		entry.ErrorCode, entry.ErrorReason = "session_lifecycle_definition_mismatch", "lifecycle definition changed before execution"
		occurrence.entries[0] = entry
		return r.finalizeSessionLifecycle(occurrence, nil, 0, lifecycleFailure{
			status: hook.InvocationFailed, code: "session_lifecycle_definition_mismatch", reason: "lifecycle definition changed before execution",
		})
	}
	succeeded := make([]session.ExtensionJournalEntry, 0, len(bindings))
	for index, binding := range bindings {
		entry := occurrence.entries[index]
		if index == 0 {
			entry.Status, entry.PendingAt = hook.InvocationPending, time.Now().UTC()
			if entry.PendingAt.Before(entry.PreparedAt) {
				entry.PendingAt = entry.PreparedAt
			}
			if err := r.commitLifecycleEntries(occurrence.actor, "session-lifecycle-pending", entry); err != nil {
				return err
			}
			occurrence.entries[index] = entry
		}
		view := occurrence.views[index]
		fingerprint, fingerprintErr := hook.FingerprintTypedInput(view)
		if fingerprintErr != nil || fingerprint.Digest != entry.InputDigest || binding.descriptor != entry.Descriptor {
			failure := lifecycleFailure{status: hook.InvocationFailed, code: "session_lifecycle_definition_mismatch", reason: "prepared lifecycle invocation does not match the current application", cause: fingerprintErr}
			return r.finalizeSessionLifecycle(occurrence, succeeded, index, failure)
		}
		result, invokeErr := invokeSessionLifecycle(ctx, binding.lifecycle, view)
		failure := classifyLifecycleFailure(invokeErr)
		if invokeErr == nil {
			if validateErr := result.Validate(view); validateErr != nil {
				failure = lifecycleFailure{status: hook.InvocationFailed, code: agent.CodeExtensionFailed, reason: "lifecycle component returned an invalid result", cause: validateErr}
			}
		}
		terminal := entry
		terminal.FinishedAt, terminal.EffectDisposition = time.Now().UTC(), hook.EffectPending
		if terminal.FinishedAt.Before(terminal.PendingAt) {
			terminal.FinishedAt = terminal.PendingAt
		}
		if failure.code != "" {
			terminal.Status, terminal.ErrorCode, terminal.ErrorReason = failure.status, failure.code, failure.reason
			occurrence.entries[index] = terminal
			return r.finalizeSessionLifecycle(occurrence, succeeded, index, failure)
		}
		terminal.Status, terminal.Result = hook.InvocationSucceeded, &hook.InvocationResult{Decision: hook.DecisionNone}
		if len(result.Context) > 0 {
			terminal.ContextInputs = cloneRuntimeInputs(result.Context)
			contextFingerprint, err := hook.FingerprintTypedInput(terminal.ContextInputs)
			if err != nil {
				return err
			}
			terminal.ContextDisposition, terminal.ContextDigest, terminal.ContextBytes = hook.ContextPending, contextFingerprint.Digest, contextFingerprint.Bytes
		}
		occurrence.entries[index] = terminal
		succeeded = append(succeeded, terminal)
		if index+1 == len(bindings) {
			return r.finalizeSessionLifecycle(occurrence, succeeded, -1, lifecycleFailure{})
		}
		next := occurrence.entries[index+1]
		next.Status, next.PendingAt = hook.InvocationPending, time.Now().UTC()
		if next.PendingAt.Before(next.PreparedAt) {
			next.PendingAt = next.PreparedAt
		}
		if err := r.commitLifecycleEntries(occurrence.actor, "session-lifecycle-terminal", terminal, next); err != nil {
			return err
		}
		occurrence.entries[index+1] = next
	}
	return nil
}

func (r *runtimeInstance) finalizeSessionLifecycle(occurrence *sessionLifecycleOccurrence, succeeded []session.ExtensionJournalEntry, failedIndex int, failure lifecycleFailure) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot, err := r.viewLocked(context.Background())
	if err != nil {
		return err
	}
	changes := make([]session.Change, 0, len(occurrence.entries)*2)
	successfulOccurrence := failure.code == ""
	if successfulOccurrence && occurrence.phase == hook.LifecycleOpen {
		changes = append(changes, discardSupersededLifecycleContexts(snapshot, occurrence, succeeded)...)
	}
	for index := range occurrence.entries {
		entry := occurrence.entries[index]
		switch {
		case index < len(succeeded):
			current, ok := extensionEntryByInvocation(snapshot.ExtensionJournal, entry.InvocationID)
			if ok && current.Status == hook.InvocationPending {
				terminal := entry
				changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &terminal})
			}
			if successfulOccurrence {
				entry.EffectDisposition = hook.EffectApplied
			} else {
				entry.EffectDisposition = hook.EffectDiscarded
				if entry.ContextDisposition == hook.ContextPending {
					entry.ContextDisposition, entry.ContextInputs = hook.ContextDiscarded, nil
				}
			}
			applied := entry
			changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &applied})
		case failure.code != "" && index == failedIndex:
			terminal := entry
			changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &terminal})
			entry.EffectDisposition = hook.EffectApplied
			applied := entry
			changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &applied})
		default:
			entry.Status, entry.FinishedAt = hook.InvocationCanceled, time.Now().UTC()
			if entry.FinishedAt.Before(entry.PreparedAt) {
				entry.FinishedAt = entry.PreparedAt
			}
			entry.ErrorCode, entry.ErrorReason, entry.EffectDisposition = "session_lifecycle_chain_stopped", "earlier lifecycle component stopped the chain", hook.EffectDiscarded
			canceled := entry
			changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &canceled})
		}
	}
	if len(changes) > 0 {
		if _, err := r.commitLockedAs(context.Background(), snapshot.Revision, "session-lifecycle-apply", occurrence.actor, changes); err != nil {
			return err
		}
	}
	return failure
}

func discardSupersededLifecycleContexts(snapshot session.Snapshot, occurrence *sessionLifecycleOccurrence, succeeded []session.ExtensionJournalEntry) []session.Change {
	replacements := make(map[string]struct{})
	current := make(map[hook.InvocationID]struct{}, len(occurrence.entries))
	for _, entry := range occurrence.entries {
		current[entry.InvocationID] = struct{}{}
	}
	for _, entry := range succeeded {
		if entry.ContextDisposition == hook.ContextPending {
			replacements[entry.Descriptor.Key] = struct{}{}
		}
	}
	if len(replacements) == 0 {
		return nil
	}
	changes := make([]session.Change, 0)
	for _, existing := range snapshot.ExtensionJournal {
		if existing.Boundary != hook.BoundarySessionLifecycle || existing.LifecyclePhase != hook.LifecycleOpen || existing.ContextDisposition != hook.ContextPending {
			continue
		}
		if _, isCurrent := current[existing.InvocationID]; isCurrent {
			continue
		}
		if _, superseded := replacements[existing.Descriptor.Key]; !superseded {
			continue
		}
		entry := existing
		entry.ContextDisposition, entry.ContextInputs = hook.ContextDiscarded, nil
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entry})
	}
	return changes
}

func (r *runtimeInstance) commitLifecycleEntries(actor agent.ActorIdentity, operation string, entries ...session.ExtensionJournalEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot, err := r.viewLocked(context.Background())
	if err != nil {
		return err
	}
	changes := make([]session.Change, 0, len(entries))
	for index := range entries {
		copy := entries[index]
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &copy})
	}
	_, err = r.commitLockedAs(context.Background(), snapshot.Revision, operation, actor, changes)
	return err
}

type lifecycleFailure struct {
	status hook.InvocationStatus
	code   agent.ErrorCode
	reason string
	cause  error
}

func (e lifecycleFailure) Error() string { return "standardagent: session lifecycle component failed" }
func (e lifecycleFailure) Unwrap() error { return e.cause }

func classifyLifecycleFailure(err error) lifecycleFailure {
	if err == nil {
		return lifecycleFailure{}
	}
	failure := lifecycleFailure{status: hook.InvocationFailed, code: agent.CodeExtensionFailed, reason: "session lifecycle component failed", cause: err}
	var declared *hook.InvocationFailure
	if errors.As(err, &declared) && declared.Validate() == nil {
		failure.status, failure.code, failure.reason = declared.Status, declared.Code, declared.Reason
	} else if errors.Is(err, context.Canceled) {
		failure.status, failure.code, failure.reason = hook.InvocationCanceled, agent.CodeCanceled, "session lifecycle component was canceled"
	}
	return failure
}

func invokeSessionLifecycle(ctx context.Context, lifecycle hook.SessionLifecycle, view hook.SessionLifecycleView) (result hook.SessionLifecycleResult, err error) {
	defer func() {
		if recover() != nil {
			result, err = hook.SessionLifecycleResult{}, errors.New("session lifecycle component panicked")
		}
	}()
	return lifecycle.Evaluate(ctx, view)
}

func (r *runtimeInstance) lifecycleDiagnostics(entries []session.ExtensionJournalEntry) ([]session.ExtensionDiagnostic, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	page, err := r.components.store.ExtensionDiagnostics(context.Background(), session.ExtensionPageRequest{
		SessionID: r.id(), Limit: session.MaxExtensionPageLimit,
	})
	if err != nil {
		return nil, err
	}
	ids := make(map[hook.InvocationID]struct{}, len(entries))
	for _, entry := range entries {
		ids[entry.InvocationID] = struct{}{}
	}
	result := make([]session.ExtensionDiagnostic, 0, len(entries))
	for _, diagnostic := range page.Diagnostics {
		if _, ok := ids[diagnostic.InvocationID]; ok {
			result = append(result, diagnostic)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Sequence < result[j].Sequence })
	return result, nil
}

func bindLifecycleContext(inputs []model.Input, runID agent.RunID, stepID agent.StepID) []model.Input {
	result := cloneRuntimeInputs(inputs)
	for index := range result {
		if result[index].Message != nil {
			result[index].Message.RunID = runID
			result[index].Message.StepID = stepID
		}
	}
	return result
}

func consumePendingLifecycleContexts(snapshot session.Snapshot, runID agent.RunID, stepID agent.StepID) []session.Change {
	changes := make([]session.Change, 0)
	for _, current := range snapshot.ExtensionJournal {
		if current.Boundary != hook.BoundarySessionLifecycle || current.LifecyclePhase != hook.LifecycleOpen ||
			current.Status != hook.InvocationSucceeded || current.EffectDisposition != hook.EffectApplied || current.ContextDisposition != hook.ContextPending {
			continue
		}
		entry := current
		fact := session.ContextContributionFact{
			RunID: runID, StepID: stepID, SourceKey: fmt.Sprintf("hook:%s:%s", entry.Descriptor.Key, entry.InvocationID),
			Inputs: bindLifecycleContext(entry.ContextInputs, runID, stepID),
		}
		changes = append(changes, session.Change{Kind: session.AppendContextContribution, ContextContribution: &fact})
		entry.ContextDisposition, entry.ContextInputs = hook.ContextConsumed, nil
		copy := entry
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &copy})
	}
	return changes
}
