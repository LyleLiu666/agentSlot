package standardagent

import (
	"context"
	"time"

	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/session"
)

// extensionRecoveryPlan classifies the durable journal once when a Runtime is
// opened. Boundary-specific recovery remains responsible for its own state
// transitions; the plan only removes repeated whole-journal discovery passes.
type extensionRecoveryPlan struct {
	lifecycle     []session.ExtensionJournalEntry
	inputGate     []session.ExtensionJournalEntry
	toolPreflight []session.ExtensionJournalEntry
	toolResult    []session.ExtensionJournalEntry
	completion    []session.ExtensionJournalEntry
}

func planExtensionRecovery(snapshot session.Snapshot) extensionRecoveryPlan {
	plan := extensionRecoveryPlan{}
	for _, entry := range snapshot.ExtensionJournal {
		switch entry.Boundary {
		case hook.BoundarySessionLifecycle:
			plan.lifecycle = append(plan.lifecycle, entry)
		case hook.BoundaryInputGate:
			plan.inputGate = append(plan.inputGate, entry)
		case hook.BoundaryToolPreflight:
			plan.toolPreflight = append(plan.toolPreflight, entry)
		case hook.BoundaryToolResult:
			plan.toolResult = append(plan.toolResult, entry)
		case hook.BoundaryCompletion:
			plan.completion = append(plan.completion, entry)
		}
	}
	return plan
}

func extensionEntriesForRun(entries []session.ExtensionJournalEntry, runID agent.RunID) []session.ExtensionJournalEntry {
	result := make([]session.ExtensionJournalEntry, 0)
	for _, entry := range entries {
		if entry.RunID == runID {
			result = append(result, entry)
		}
	}
	return result
}

// recoverExtensionsAfterOpen settles non-replayable work in one durable
// commit. It intentionally does not settle ToolPreflight or Completion work:
// those boundaries can be reconstructed from durable Run evidence and are
// resumed by restorePreparedRun without replaying an unknown command.
func (r *runtimeInstance) recoverExtensionsAfterOpen(ctx context.Context, snapshot session.Snapshot, plan extensionRecoveryPlan) (session.Snapshot, error) {
	now := time.Now().UTC()
	changes := inputGateRecoveryChanges(snapshot, plan.inputGate, now)
	changes = append(changes, toolResultRecoveryChanges(plan.toolResult, now)...)
	if len(changes) == 0 {
		return snapshot, nil
	}
	if _, err := r.commitLocked(ctx, snapshot.Revision, "extension-recovery-after-open", changes); err != nil {
		return session.Snapshot{}, err
	}
	return r.session.View(ctx)
}
