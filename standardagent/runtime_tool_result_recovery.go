package standardagent

import (
	"time"

	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/session"
)

// toolResultRecoveryChanges settles abandoned Post invocations only after
// SessionStore recovery has interrupted the owning Run. External commands are
// never replayed: prepared work is canceled, pending work remains unknown, and
// terminal context for the abandoned next Step is discarded.
func toolResultRecoveryChanges(entries []session.ExtensionJournalEntry, now time.Time) []session.Change {
	changes := make([]session.Change, 0)
	for _, existing := range entries {
		entry := existing
		switch entry.Status {
		case hook.InvocationPrepared:
			entry.Status, entry.FinishedAt = hook.InvocationCanceled, now
			if entry.FinishedAt.Before(entry.PreparedAt) {
				entry.FinishedAt = entry.PreparedAt
			}
			entry.ErrorCode, entry.EffectDisposition = "tool_result_hook_recovery_canceled", hook.EffectDiscarded
		case hook.InvocationPending:
			// Memory/File Recover normally changes pending to outcome_unknown
			// before Runtime construction. Keep this branch for custom Stores
			// implementing the same SessionStore contract.
			entry.Status, entry.FinishedAt = hook.InvocationOutcomeUnknown, now
			if entry.FinishedAt.Before(entry.PendingAt) {
				entry.FinishedAt = entry.PendingAt
			}
			entry.ErrorCode, entry.ErrorReason = "hook_outcome_unknown", "tool result hook outcome is unknown after recovery"
			entry.EffectDisposition = hook.EffectApplied
		default:
			if !entry.Status.Terminal() || entry.EffectDisposition != hook.EffectPending {
				continue
			}
			if entry.Status == hook.InvocationSucceeded {
				entry.EffectDisposition = hook.EffectDiscarded
			} else {
				entry.EffectDisposition = hook.EffectApplied
			}
		}
		if entry.ContextDisposition == hook.ContextPending {
			entry.ContextDisposition, entry.ContextInputs = hook.ContextDiscarded, nil
		}
		entryCopy := entry
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
	}
	return changes
}
