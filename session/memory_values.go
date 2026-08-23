package session

import (
	"encoding/json"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/tool"
)

func cloneSnapshot(source Snapshot) Snapshot {
	copy := source
	copy.History = make([]HistoryFact, len(source.History))
	for index, fact := range source.History {
		copy.History[index] = cloneHistoryFact(fact)
	}
	copy.Context = cloneContext(source.Context)
	copy.RetainedContexts = make([]ContextView, len(source.RetainedContexts))
	for index, contextView := range source.RetainedContexts {
		copy.RetainedContexts[index] = cloneContext(contextView)
	}
	copy.Queue = make([]QueueItem, len(source.Queue))
	for index, item := range source.Queue {
		copy.Queue[index] = cloneQueueItem(item)
	}
	copy.RunJournal = make([]JournalEntry, len(source.RunJournal))
	for index, entry := range source.RunJournal {
		copy.RunJournal[index] = cloneJournalEntry(entry)
	}
	copy.Events = make([]SessionEvent, len(source.Events))
	for index, event := range source.Events {
		copy.Events[index] = cloneSessionEvent(event)
	}
	copy.ModelConfig = cloneModelConfig(source.ModelConfig)
	if source.Fork != nil {
		fork := *source.Fork
		copy.Fork = &fork
	}
	return copy
}

func cloneSessionEvent(source SessionEvent) SessionEvent {
	copy := source
	if source.ModelConfigChanged != nil {
		change := *source.ModelConfigChanged
		change.Previous = cloneModelConfig(change.Previous)
		change.Current = cloneModelConfig(change.Current)
		copy.ModelConfigChanged = &change
	}
	return copy
}

func cloneHistoryFact(source HistoryFact) HistoryFact {
	copy := source
	if source.Message != nil {
		message := cloneMessage(*source.Message)
		copy.Message = &message
	}
	if source.ToolCall != nil {
		call := cloneToolCall(*source.ToolCall)
		copy.ToolCall = &call
	}
	if source.ToolResult != nil {
		result := cloneToolResult(*source.ToolResult)
		copy.ToolResult = &result
	}
	if source.Run != nil {
		run := *source.Run
		run.ModelConfig = cloneModelConfig(source.Run.ModelConfig)
		copy.Run = &run
	}
	if source.ModelAttempt != nil {
		attempt := *source.ModelAttempt
		copy.ModelAttempt = &attempt
	}
	if source.ModelConfigChanged != nil {
		change := *source.ModelConfigChanged
		change.Previous = cloneModelConfig(change.Previous)
		change.Current = cloneModelConfig(change.Current)
		copy.ModelConfigChanged = &change
	}
	if source.ContextContribution != nil {
		contribution := *source.ContextContribution
		contribution.Inputs = make([]model.Input, len(source.ContextContribution.Inputs))
		for index, input := range source.ContextContribution.Inputs {
			contribution.Inputs[index] = cloneModelInput(input)
		}
		copy.ContextContribution = &contribution
	}
	if source.RunBudgetExceeded != nil {
		budget := *source.RunBudgetExceeded
		copy.RunBudgetExceeded = &budget
	}
	return copy
}

func cloneContext(source ContextView) ContextView {
	copy := source
	copy.Request = cloneSessionModelRequest(source.Request)
	return copy
}

func cloneSessionModelRequest(source model.ModelRequest) model.ModelRequest {
	copy := source
	copy.Config = cloneModelConfig(source.Config)
	copy.Inputs = make([]model.Input, len(source.Inputs))
	for index, input := range source.Inputs {
		copy.Inputs[index] = cloneModelInput(input)
	}
	copy.Tools = append(copy.Tools[:0:0], source.Tools...)
	return copy
}

func cloneModelInput(source model.Input) model.Input {
	copy := source
	if source.Message != nil {
		message := cloneMessage(*source.Message)
		copy.Message = &message
	}
	if source.ToolCall != nil {
		call := cloneToolCall(*source.ToolCall)
		copy.ToolCall = &call
	}
	if source.ToolResult != nil {
		result := cloneToolResult(*source.ToolResult)
		copy.ToolResult = &result
	}
	if source.SystemPrompt != nil {
		prompt := *source.SystemPrompt
		copy.SystemPrompt = &prompt
	}
	return copy
}

func cloneQueueItem(source QueueItem) QueueItem {
	copy := source
	copy.Message = cloneMessage(source.Message)
	return copy
}

func cloneJournalEntry(source JournalEntry) JournalEntry {
	copy := source
	if source.ToolCall != nil {
		call := cloneToolCall(*source.ToolCall)
		copy.ToolCall = &call
	}
	copy.ToolResult = cloneToolResultPtr(source.ToolResult)
	return copy
}

func cloneMessage(source agent.Message) agent.Message {
	copy := source
	copy.Parts = cloneParts(source.Parts)
	if source.ModelContinuation != nil {
		continuation := *source.ModelContinuation
		continuation.State = append(json.RawMessage(nil), source.ModelContinuation.State...)
		copy.ModelContinuation = &continuation
	}
	return copy
}

func cloneParts(source []agent.MessagePart) []agent.MessagePart {
	return append([]agent.MessagePart(nil), source...)
}

func cloneToolCall(source agent.ToolCall) agent.ToolCall {
	copy := source
	copy.Arguments = append(json.RawMessage(nil), source.Arguments...)
	return copy
}

func cloneToolResult(source tool.ToolResult) tool.ToolResult {
	copy := source
	copy.Output = append(json.RawMessage(nil), source.Output...)
	copy.Artifacts = append(source.Artifacts[:0:0], source.Artifacts...)
	if source.Error != nil {
		errorCopy := *source.Error
		copy.Error = &errorCopy
	}
	return copy
}

func cloneToolResultPtr(source *tool.ToolResult) *tool.ToolResult {
	if source == nil {
		return nil
	}
	copy := cloneToolResult(*source)
	return &copy
}

func cloneModelConfig(source model.Config) model.Config {
	copy := source
	if source.Parameters.Temperature != nil {
		value := *source.Parameters.Temperature
		copy.Parameters.Temperature = &value
	}
	if source.Parameters.MaxTokens != nil {
		value := *source.Parameters.MaxTokens
		copy.Parameters.MaxTokens = &value
	}
	return copy
}
