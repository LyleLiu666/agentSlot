package session

import (
	"fmt"
	"time"

	agent "github.com/LyleLiu666/agentSlot/agent"
)

const historyPageLimit = 100

func prepareInitialHistory(sessionID agent.SessionID, source []HistoryFact) ([]HistoryFact, error) {
	history := make([]HistoryFact, 0, len(source))
	for _, fact := range source {
		// A derived Session may carry source facts through the fixed Manager.
		// Store-owned identity and order are never trusted across aggregates.
		fact.FactID = ""
		fact.Sequence = 0
		fact.SessionID = ""
		fact.RunID = ""
		fact.StepID = ""
		fact.At = time.Time{}
		fact.Actor = agent.ActorIdentity{}
		fact.Kind = ""
		if err := appendHistoryFact(&history, sessionID, fact, agent.ActorIdentity{}); err != nil {
			return nil, invalid("session.create", err.Error())
		}
	}
	return history, nil
}

func appendHistoryFact(history *[]HistoryFact, sessionID agent.SessionID, source HistoryFact, actor agent.ActorIdentity) error {
	if hasHistoryEnvelope(source) {
		return fmt.Errorf("session: appended history fact contains store-owned metadata")
	}
	if err := source.validatePayload(sessionID); err != nil {
		return err
	}
	fact := cloneHistoryFact(source)
	fact.Sequence = HistorySequence(len(*history) + 1)
	fact.FactID = agent.FactID(fmt.Sprintf("%s:fact:%d", sessionID, fact.Sequence))
	fact.SessionID = sessionID
	fact.At = time.Now().UTC()
	fact.Actor = normalizedActor(actor)
	fact.Kind = fact.payloadKind()
	fact.RunID, fact.StepID = factContainment(*history, fact)
	if err := fact.Validate(sessionID); err != nil {
		return err
	}
	*history = append(*history, fact)
	return nil
}

func hasHistoryEnvelope(f HistoryFact) bool {
	return f.FactID != "" || f.Sequence != 0 || f.SessionID != "" || f.RunID != "" || f.StepID != "" ||
		!f.At.IsZero() || f.Actor.Kind != "" || f.Actor.ID != "" || f.Kind != ""
}

func normalizedActor(actor agent.ActorIdentity) agent.ActorIdentity {
	if actor.Valid() {
		return actor
	}
	return agent.ActorIdentity{Kind: agent.ActorService, ID: "agentslot"}
}

func factContainment(history []HistoryFact, fact HistoryFact) (agent.RunID, agent.StepID) {
	switch {
	case fact.Message != nil:
		return fact.Message.RunID, fact.Message.StepID
	case fact.ToolCall != nil:
		return fact.ToolCall.RunID, fact.ToolCall.StepID
	case fact.ToolResult != nil:
		if call, ok := findToolCall(history, fact.ToolResult.CallID); ok {
			return call.RunID, call.StepID
		}
	case fact.Run != nil:
		return fact.Run.RunID, ""
	case fact.ModelAttempt != nil:
		return fact.ModelAttempt.RunID, fact.ModelAttempt.StepID
	case fact.ContextContribution != nil:
		return fact.ContextContribution.RunID, fact.ContextContribution.StepID
	case fact.RunBudgetExceeded != nil:
		return fact.RunBudgetExceeded.RunID, ""
	}
	return "", ""
}

func historyPage(history []HistoryFact, request HistoryPageRequest) (HistoryPage, error) {
	if !request.SessionID.Valid() {
		return HistoryPage{}, invalid("session.history_page", "session ID is required")
	}
	limit := request.StepLimit
	if limit == 0 {
		limit = historyPageLimit
	}
	if limit < 0 || limit > historyPageLimit {
		return HistoryPage{}, invalid("session.history_page", "step limit must be between 1 and 100")
	}
	end := len(history)
	if request.BeforeHistorySequence != 0 {
		end = 0
		for end < len(history) && history[end].Sequence < request.BeforeHistorySequence {
			end++
		}
		if end > 0 && end < len(history) && history[end].StepID.Valid() && history[end-1].StepID == history[end].StepID {
			stepID := history[end].StepID
			for end > 0 && history[end-1].StepID == stepID {
				end--
			}
		}
	}
	if end == 0 {
		return HistoryPage{}, nil
	}
	start := end
	steps := 0
	seen := make(map[agent.StepID]bool)
	runs := make(map[agent.RunID]bool)
	for index := end - 1; index >= 0; index-- {
		stepID := history[index].StepID
		if stepID.Valid() && !seen[stepID] {
			if steps == limit {
				break
			}
			seen[stepID] = true
			steps++
		}
		if stepID.Valid() && history[index].RunID.Valid() {
			runs[history[index].RunID] = true
		}
		start = index
	}
	// Run lifecycle facts immediately after an omitted Step belong to that
	// older Run. Do not leak them into a page whose logical Steps all belong to
	// newer Runs. Session-level facts without Run containment remain visible at
	// the transition boundary.
	for steps > 0 && start < end && history[start].RunID.Valid() && !runs[history[start].RunID] {
		start++
	}
	// A Session can contain configuration or lifecycle facts before its first
	// executable Step. Keep those histories pageable instead of returning an
	// unbounded page merely because no StepID exists yet.
	if steps == 0 && end > limit {
		start = end - limit
	}
	facts := make([]HistoryFact, end-start)
	for index := range facts {
		facts[index] = cloneHistoryFact(history[start+index])
	}
	return HistoryPage{Facts: facts, HasMore: start > 0}, nil
}
