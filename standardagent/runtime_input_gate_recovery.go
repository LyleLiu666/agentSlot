package standardagent

import (
	"context"
	"time"

	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/session"
)

func (r *runtimeInstance) recoverInputGateEntries(ctx context.Context, snapshot session.Snapshot) (session.Snapshot, error) {
	changes := make([]session.Change, 0)
	now := time.Now().UTC()
	for _, existing := range snapshot.ExtensionJournal {
		if existing.Boundary != hook.BoundaryInputGate {
			continue
		}
		entry := existing
		switch entry.Status {
		case hook.InvocationPrepared:
			entry.Status = hook.InvocationFailed
			entry.FinishedAt = now
			if entry.FinishedAt.Before(entry.PreparedAt) {
				entry.FinishedAt = entry.PreparedAt
			}
			entry.ErrorCode = "extension_input_unavailable"
			entry.ErrorReason = "prepared input gate cannot be replayed without the original proposed input"
			entry.EffectDisposition = hook.EffectPending
			failed := entry
			changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &failed})
			entry.EffectDisposition = hook.EffectDiscarded
			discarded := entry
			changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &discarded})
		case hook.InvocationPending:
			entry.Status = hook.InvocationOutcomeUnknown
			entry.FinishedAt = now
			if entry.FinishedAt.Before(entry.PendingAt) {
				entry.FinishedAt = entry.PendingAt
			}
			entry.ErrorCode = "extension_outcome_unknown"
			entry.ErrorReason = "pending input gate outcome is unknown after recovery"
			entry.EffectDisposition = hook.EffectPending
			unknown := entry
			changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &unknown})
			entry.EffectDisposition = hook.EffectDiscarded
			discarded := entry
			changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &discarded})
		default:
			if !entry.Status.Terminal() {
				continue
			}
			changed := false
			if entry.EffectDisposition == hook.EffectPending {
				entry.EffectDisposition = hook.EffectDiscarded
				changed = true
				// A pending effect means the proposed Send/Steer/Edit never became
				// authoritative. Even if an Edit subject still exists in Queue, its
				// proposed context belongs to the unavailable replacement input.
				if entry.ContextDisposition == hook.ContextPending {
					entry.ContextDisposition = hook.ContextDiscarded
					entry.ContextInputs = nil
				}
			} else if entry.ContextDisposition == hook.ContextPending && !unclaimedQueueMessage(snapshot.Queue, entry.MessageID) {
				entry.ContextDisposition = hook.ContextDiscarded
				entry.ContextInputs = nil
				changed = true
			}
			if changed {
				updated := entry
				changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &updated})
			}
		}
	}
	if len(changes) == 0 {
		return snapshot, nil
	}
	if _, err := r.commitLockedAs(ctx, snapshot.Revision, "input-gate-recovery", agent.ActorIdentity{}, changes); err != nil {
		return session.Snapshot{}, err
	}
	return r.session.View(ctx)
}

func unclaimedQueueMessage(queue []session.QueueItem, messageID agent.MessageID) bool {
	for _, item := range queue {
		if item.Message.ID == messageID {
			return !item.Claimed()
		}
	}
	return false
}

func hasInputGateJournal(entries []session.ExtensionJournalEntry) bool {
	for _, entry := range entries {
		if entry.Boundary == hook.BoundaryInputGate {
			return true
		}
	}
	return false
}
