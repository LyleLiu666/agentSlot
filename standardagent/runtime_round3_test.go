package standardagent

import (
	"context"
	"reflect"
	"testing"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestModelAttemptIsDurableBeforeFakeProviderProducesOutput(t *testing.T) {
	block := make(chan struct{})
	usage := model.TokenUsage{
		InputTokens: 10, OutputTokens: 5, CachedInputTokens: 4, ReasoningTokens: 2, TotalTokens: 15,
	}
	executor := model.NewFakeModelExecutor(model.FakeExecution{
		Block: block, Usage: usage,
		Events: []model.ModelEvent{{Kind: model.EventComplete, AttemptID: "attempt-one", Output: &model.Completion{Parts: textInput("done").Parts}}},
	})
	access, store, stop := startRound7Application(t, executor, AgentRuntimeConfig{})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("hello")}); err != nil {
		t.Fatal(err)
	}
	during := loadRound3Snapshot(t, store, opened.SessionID)
	attempts := attemptFacts(during.History)
	if len(attempts) != 1 || attempts[0].Kind != session.AttemptStarted || attempts[0].AttemptID != "attempt-one" {
		t.Fatalf("attempt facts before output = %#v", attempts)
	}
	close(block)
	waitRuntimeIdle(t, access, opened.SessionID)
	after := loadRound3Snapshot(t, store, opened.SessionID)
	attempts = attemptFacts(after.History)
	if len(attempts) != 2 || attempts[1].Kind != session.AttemptSucceeded || !reflect.DeepEqual(attempts[1].Usage, usage) {
		t.Fatalf("terminal attempt facts = %#v", attempts)
	}
}

func TestFakeExecutorPersistsEveryPhysicalRetryAttempt(t *testing.T) {
	executor := model.NewFakeModelExecutor(model.FakeExecution{
		AttemptUsage: map[agent.AttemptID]model.TokenUsage{
			"attempt-a": {InputTokens: 4, OutputTokens: 1, TotalTokens: 5},
			"attempt-b": {InputTokens: 4, OutputTokens: 2, TotalTokens: 6},
		},
		Events: []model.ModelEvent{
			{Kind: model.EventDelta, AttemptID: "attempt-a", Text: "partial"},
			{Kind: model.EventReset, AttemptID: "attempt-a"},
			{Kind: model.EventComplete, AttemptID: "attempt-b", Output: &model.Completion{Parts: textInput("done").Parts}},
		},
	})
	access, store, stop := startRound7Application(t, executor, AgentRuntimeConfig{})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("retry")}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	attempts := attemptFacts(loadRound3Snapshot(t, store, opened.SessionID).History)
	want := []session.ModelAttemptKind{session.AttemptStarted, session.AttemptFailed, session.AttemptStarted, session.AttemptSucceeded}
	if len(attempts) != len(want) {
		t.Fatalf("attempt facts = %#v", attempts)
	}
	for index, kind := range want {
		if attempts[index].Kind != kind {
			t.Fatalf("attempt %d kind = %q, want %q", index, attempts[index].Kind, kind)
		}
	}
	if attempts[1].ErrorCode != "scripted_failure" || attempts[1].Usage.TotalTokens != 5 || attempts[3].Usage.TotalTokens != 6 {
		t.Fatalf("attempt terminal details = %#v", attempts)
	}
}

func TestContextRetentionStoresCompleteLogicalRequestsAndSourceFacts(t *testing.T) {
	for _, test := range []struct {
		name     string
		mode     ContextRetentionMode
		retained int
	}{
		{name: "latest only", mode: ContextLatestOnly, retained: 0},
		{name: "retain all", mode: ContextRetainAll, retained: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			installed := &countingTool{definition: testToolDefinition(t, "lookup")}
			executor := model.NewFakeModelExecutor(
				model.FakeExecution{Events: []model.ModelEvent{{Kind: model.EventComplete, Output: &model.Completion{ToolCalls: []model.ToolCallRequest{{Name: "lookup", Arguments: []byte(`{"value":"one"}`)}}}}}},
				model.FakeExecution{Events: []model.ModelEvent{{Kind: model.EventComplete, Output: &model.Completion{Parts: textInput("done").Parts}}}},
			)
			access, store, stop := startRound7Application(t, executor, AgentRuntimeConfig{
				SystemPrompt: "fixed prompt", ContextRetentionMode: test.mode, ToolKeys: []string{"lookup"},
			}, toolModule{key: "lookup", value: installed}, contextSourceModule{source: fixedContextSource("source", "contribution")})
			defer stop()
			opened := createRuntimeTestSession(t, access)
			if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("use tool")}); err != nil {
				t.Fatal(err)
			}
			waitRuntimeIdle(t, access, opened.SessionID)
			snapshot := loadRound3Snapshot(t, store, opened.SessionID)
			requests := executor.Requests()
			if len(requests) != 2 || snapshot.Context.Version != 2 || !reflect.DeepEqual(snapshot.Context.Request, requests[1]) {
				t.Fatalf("latest Context/request = %#v / %#v", snapshot.Context, requests)
			}
			if len(snapshot.RetainedContexts) != test.retained {
				t.Fatalf("retained contexts = %#v", snapshot.RetainedContexts)
			}
			if test.mode == ContextRetainAll && !reflect.DeepEqual(snapshot.RetainedContexts[0].Request, requests[0]) {
				t.Fatalf("first logical request was rewritten: retained %#v request %#v", snapshot.RetainedContexts[0].Request, requests[0])
			}
			for _, request := range requests {
				if len(request.Tools) != 1 || len(request.Inputs) < 2 || request.Inputs[0].SystemPrompt == nil || *request.Inputs[0].SystemPrompt != "fixed prompt" {
					t.Fatalf("incomplete logical request = %#v", request)
				}
			}
			contributions := contributionFacts(snapshot.History)
			if len(contributions) != 2 || contributions[0].SourceKey != "source" || contributions[1].SourceKey != "source" {
				t.Fatalf("context contribution facts = %#v", contributions)
			}
		})
	}
}

func TestRunTokenBudgetStopsBeforeAnotherModelStepAndNextInputStartsNewRun(t *testing.T) {
	installed := &countingTool{definition: testToolDefinition(t, "lookup")}
	executor := model.NewFakeModelExecutor(
		model.FakeExecution{
			Usage:  model.TokenUsage{InputTokens: 4, OutputTokens: 1, TotalTokens: 5},
			Events: []model.ModelEvent{{Kind: model.EventComplete, Output: &model.Completion{ToolCalls: []model.ToolCallRequest{{Name: "lookup", Arguments: []byte(`{"value":"one"}`)}}}}},
		},
		model.FakeExecution{
			Usage:  model.TokenUsage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5},
			Events: []model.ModelEvent{{Kind: model.EventComplete, Output: &model.Completion{Parts: textInput("continued").Parts}}},
		},
	)
	access, store, stop := startRound7Application(t, executor, AgentRuntimeConfig{MaxTokensPerRun: 5, ToolKeys: []string{"lookup"}}, toolModule{key: "lookup", value: installed})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("use tool")}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	first := loadRound3Snapshot(t, store, opened.SessionID)
	if len(executor.Requests()) != 1 || len(budgetFacts(first.History)) != 1 || lastRunTerminal(first.History) != session.RunInterrupted {
		t.Fatalf("budget terminal = requests %d facts %#v terminal %q", len(executor.Requests()), budgetFacts(first.History), lastRunTerminal(first.History))
	}
	if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: first.Revision, Input: textInput("continue")}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	second := loadRound3Snapshot(t, store, opened.SessionID)
	if len(executor.Requests()) != 2 || lastRunTerminal(second.History) != session.RunCompleted {
		t.Fatalf("continued Run = requests %d terminal %q", len(executor.Requests()), lastRunTerminal(second.History))
	}
}

func TestFailedAttemptUsageConsumesRunBudgetWithoutDoubleCountingSubsets(t *testing.T) {
	executor := model.NewFakeModelExecutor(model.FakeExecution{
		AttemptUsage: map[agent.AttemptID]model.TokenUsage{
			"attempt-a": {
				InputTokens: 4, OutputTokens: 1, CachedInputTokens: 3,
				ReasoningTokens: 1, TotalTokens: 5,
			},
		},
		Events: []model.ModelEvent{
			{Kind: model.EventDelta, AttemptID: "attempt-a", Text: "partial"},
			{Kind: model.EventReset, AttemptID: "attempt-a"},
			{Kind: model.EventComplete, AttemptID: "attempt-b", Output: &model.Completion{Parts: textInput("must not run").Parts}},
		},
	})
	access, store, stop := startRound7Application(t, executor, AgentRuntimeConfig{MaxTokensPerRun: 5})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("retry until budget")}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	snapshot := loadRound3Snapshot(t, store, opened.SessionID)
	budgets := budgetFacts(snapshot.History)
	if len(budgets) != 1 || budgets[0].UsedTokens != 5 || budgets[0].MaxTokens != 5 {
		t.Fatalf("budget facts = %#v", budgets)
	}
	attempts := attemptFacts(snapshot.History)
	if len(attempts) != 2 || attempts[1].Kind != session.AttemptFailed || attempts[1].Usage.TotalTokens != 5 || attempts[1].Usage.CachedInputTokens != 3 || attempts[1].Usage.ReasoningTokens != 1 {
		t.Fatalf("failed attempt accounting = %#v", attempts)
	}
}

func loadRound3Snapshot(t *testing.T, store session.SessionStore, id agent.SessionID) session.Snapshot {
	t.Helper()
	snapshot, err := store.Load(context.Background(), session.SessionRef{SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func attemptFacts(history []session.HistoryFact) []session.ModelAttemptFact {
	var result []session.ModelAttemptFact
	for _, fact := range history {
		if fact.ModelAttempt != nil {
			result = append(result, *fact.ModelAttempt)
		}
	}
	return result
}

func contributionFacts(history []session.HistoryFact) []session.ContextContributionFact {
	var result []session.ContextContributionFact
	for _, fact := range history {
		if fact.ContextContribution != nil {
			result = append(result, *fact.ContextContribution)
		}
	}
	return result
}

func budgetFacts(history []session.HistoryFact) []session.RunBudgetExceededFact {
	var result []session.RunBudgetExceededFact
	for _, fact := range history {
		if fact.RunBudgetExceeded != nil {
			result = append(result, *fact.RunBudgetExceeded)
		}
	}
	return result
}

func lastRunTerminal(history []session.HistoryFact) session.RunFactKind {
	var result session.RunFactKind
	for _, fact := range history {
		if fact.Run != nil && fact.Run.Kind != session.RunStarted {
			result = fact.Run.Kind
		}
	}
	return result
}
