package standardagent

import (
	"context"
	"fmt"

	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/session"
)

func finalizeAcceptedInputEntries(occurrence *inputGateOccurrence) []session.Change {
	changes := make([]session.Change, 0, len(occurrence.entries))
	for index := range occurrence.entries {
		entry := occurrence.entries[index]
		entry.EffectDisposition = hook.EffectApplied
		entryCopy := entry
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
		occurrence.entries[index] = entry
	}
	return changes
}

func consumeAcceptedInputEntries(occurrence *inputGateOccurrence, runID agent.RunID, stepID agent.StepID) []session.Change {
	changes := make([]session.Change, 0, len(occurrence.entries)*2)
	for index := range occurrence.entries {
		entry := occurrence.entries[index]
		entry.EffectDisposition = hook.EffectApplied
		if entry.ContextDisposition == hook.ContextPending {
			fact := session.ContextContributionFact{
				RunID: runID, StepID: stepID, SourceKey: inputGateContextSource(entry), Inputs: cloneRuntimeInputs(entry.ContextInputs),
			}
			changes = append(changes, session.Change{Kind: session.AppendContextContribution, ContextContribution: &fact})
			entry.ContextDisposition = hook.ContextConsumed
			entry.ContextInputs = nil
		}
		entryCopy := entry
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
		occurrence.entries[index] = entry
	}
	return changes
}

func consumePendingInputContexts(snapshot session.Snapshot, messageID agent.MessageID, runID agent.RunID, stepID agent.StepID) []session.Change {
	changes := make([]session.Change, 0)
	for _, entry := range snapshot.ExtensionJournal {
		if entry.Boundary != hook.BoundaryInputGate || entry.MessageID != messageID ||
			entry.Status != hook.InvocationSucceeded || entry.Result == nil || entry.Result.Decision != hook.DecisionAccept ||
			entry.EffectDisposition != hook.EffectApplied || entry.ContextDisposition != hook.ContextPending {
			continue
		}
		fact := session.ContextContributionFact{
			RunID: runID, StepID: stepID, SourceKey: inputGateContextSource(entry), Inputs: cloneRuntimeInputs(entry.ContextInputs),
		}
		changes = append(changes, session.Change{Kind: session.AppendContextContribution, ContextContribution: &fact})
		entry.ContextDisposition = hook.ContextConsumed
		entry.ContextInputs = nil
		entryCopy := entry
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
	}
	return changes
}

func discardAcceptedInputEntries(occurrence *inputGateOccurrence) []session.Change {
	changes := make([]session.Change, 0, len(occurrence.entries))
	for index := range occurrence.entries {
		entry := occurrence.entries[index]
		if entry.EffectDisposition != hook.EffectPending {
			continue
		}
		entry.EffectDisposition = hook.EffectDiscarded
		if entry.ContextDisposition == hook.ContextPending {
			entry.ContextDisposition = hook.ContextDiscarded
			entry.ContextInputs = nil
		}
		entryCopy := entry
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
		occurrence.entries[index] = entry
	}
	return changes
}

func discardOlderInputContexts(snapshot session.Snapshot, occurrence *inputGateOccurrence) []session.Change {
	current := make(map[hook.InvocationID]struct{}, len(occurrence.entries))
	for _, entry := range occurrence.entries {
		current[entry.InvocationID] = struct{}{}
	}
	changes := make([]session.Change, 0)
	for _, entry := range snapshot.ExtensionJournal {
		if entry.Boundary != hook.BoundaryInputGate || entry.MessageID != occurrence.message.ID || entry.ContextDisposition != hook.ContextPending {
			continue
		}
		if _, sameOccurrence := current[entry.InvocationID]; sameOccurrence {
			continue
		}
		entry.ContextDisposition = hook.ContextDiscarded
		entry.ContextInputs = nil
		entryCopy := entry
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
	}
	return changes
}

func discardPendingInputContexts(snapshot session.Snapshot, messageID agent.MessageID) []session.Change {
	changes := make([]session.Change, 0)
	for _, entry := range snapshot.ExtensionJournal {
		if entry.Boundary != hook.BoundaryInputGate || entry.MessageID != messageID ||
			entry.EffectDisposition != hook.EffectApplied || entry.ContextDisposition != hook.ContextPending {
			continue
		}
		entry.ContextDisposition = hook.ContextDiscarded
		entry.ContextInputs = nil
		entryCopy := entry
		changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
	}
	return changes
}

func inputGateContextSource(entry session.ExtensionJournalEntry) string {
	return fmt.Sprintf("hook:%s:%s", entry.Descriptor.Key, entry.InvocationID)
}

func cloneInputGateView(source hook.InputGateView) hook.InputGateView {
	copy := source
	copy.Input.Parts = cloneRuntimeParts(source.Input.Parts)
	return copy
}

func (r *runtimeInstance) inputGateError(revision agent.Revision, occurrence *inputGateOccurrence, cause error) error {
	page, _ := r.components.store.ExtensionDiagnostics(context.Background(), session.ExtensionPageRequest{SessionID: r.id(), Limit: session.MaxExtensionPageLimit})
	ids := make(map[hook.InvocationID]struct{}, len(occurrence.entries))
	for _, entry := range occurrence.entries {
		ids[entry.InvocationID] = struct{}{}
	}
	diagnostics := make([]session.ExtensionDiagnostic, 0, len(occurrence.entries))
	for index := len(page.Diagnostics) - 1; index >= 0; index-- {
		if _, ok := ids[page.Diagnostics[index].InvocationID]; ok {
			diagnostics = append(diagnostics, page.Diagnostics[index])
		}
	}
	return &interaction.InputGateError{SessionID: r.id(), CurrentRevision: revision, Diagnostics: diagnostics, Cause: cause}
}
