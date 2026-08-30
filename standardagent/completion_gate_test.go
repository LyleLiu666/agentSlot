package standardagent

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/goal"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestCompletionGateContinuesTheSameRunWithAppendOnlyNextStepContext(t *testing.T) {
	executor := newRound7Executor(nil, nil,
		model.FakeExecution{Events: []model.ModelEvent{complete("first result")}},
		model.FakeExecution{Events: []model.ModelEvent{complete("final result")}},
	)
	gate := newCompletionGateSequence("quality", hook.CompletionContinue, hook.CompletionComplete)
	access, store, stop := startRound7Application(t, executor, AgentRuntimeConfig{}, completionGateModule{gates: []hook.CompletionGate{gate}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	result, err := access.SendAndWait(t.Context(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("begin"),
	})
	if err != nil || result.Outcome != session.RunCompleted {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	requests := executor.fake.Requests()
	if len(requests) != 2 || requests[0].RunID != requests[1].RunID || countInputText(requests[1].Inputs, "completion context 1") != 1 {
		t.Fatalf("completion requests = %#v", requests)
	}
	snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ExtensionJournal) != 2 || snapshot.ExtensionJournal[0].Result.Decision != hook.DecisionContinue ||
		snapshot.ExtensionJournal[0].EffectDisposition != hook.EffectApplied || snapshot.ExtensionJournal[0].ContextDisposition != hook.ContextConsumed ||
		snapshot.ExtensionJournal[1].Result.Decision != hook.DecisionComplete || snapshot.ExtensionJournal[1].EffectDisposition != hook.EffectApplied {
		t.Fatalf("completion journal = %#v", snapshot.ExtensionJournal)
	}
	if !historyHasUnchangedText(snapshot.History, "first result") {
		t.Fatalf("completion gate rewrote assistant History: %#v", snapshot.History)
	}
}

func TestGoalDoneIsNotCommittedUntilCompletionGateAllowsIt(t *testing.T) {
	executor := newRound7Executor(nil, nil,
		model.FakeExecution{Events: []model.ModelEvent{complete("candidate")}},
		model.FakeExecution{Events: []model.ModelEvent{complete("after stop continuation")}},
	)
	goalStore := goal.NewMemoryStore()
	evaluator := &goalEvaluatorSequence{evaluations: []goal.Evaluation{
		{Decision: goal.DecisionDone, Reason: goal.ReasonObjectiveMet},
		{Decision: goal.DecisionDone, Reason: goal.ReasonObjectiveMet},
	}}
	gate := newCompletionGateSequence("goal-stop", hook.CompletionContinue, hook.CompletionComplete)
	var committedPrematurely atomic.Bool
	gate.afterEvaluation = func(call int) {
		if call != 1 {
			return
		}
		current, ok, err := goalStore.Current(context.Background(), gate.sessionID())
		if err != nil || !ok || current.Status != goal.StatusActive {
			committedPrematurely.Store(true)
		}
	}
	access, _, stop := startRound7Application(t, executor, AgentRuntimeConfig{},
		goalStoreModule{store: goalStore}, goalEvaluatorModule{evaluator: evaluator}, completionGateModule{gates: []hook.CompletionGate{gate}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	gate.setSessionID(opened.SessionID)
	if _, err := goalStore.Set(t.Context(), goal.SetRequest{SessionID: opened.SessionID, Objective: "finish only after stop", MaxFollowOns: 3}); err != nil {
		t.Fatal(err)
	}
	result, err := access.SendAndWait(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("begin")})
	if err != nil || result.Outcome != session.RunCompleted {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	current, ok, err := goalStore.Current(t.Context(), opened.SessionID)
	if err != nil || !ok || current.Status != goal.StatusCompleted {
		t.Fatalf("Goal was not completed after Stop allowed it: %#v ok=%t err=%v", current, ok, err)
	}
	if committedPrematurely.Load() {
		t.Fatal("Goal done was committed before Stop allowed it")
	}
}

func TestCompletionGateFailureInterruptsInsteadOfCompletingTheRun(t *testing.T) {
	executor := newRound7Executor(nil, nil, model.FakeExecution{Events: []model.ModelEvent{complete("candidate")}})
	gate := newCompletionGateSequence("failure", hook.CompletionComplete)
	gate.err = &hook.InvocationFailure{Status: hook.InvocationFailed, Code: "completion_failed", Reason: "completion gate failed"}
	access, store, stop := startRound7Application(t, executor, AgentRuntimeConfig{}, completionGateModule{gates: []hook.CompletionGate{gate}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	result, err := access.SendAndWait(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("begin")})
	if err != nil || result.Outcome != session.RunInterrupted {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ExtensionJournal) != 1 || snapshot.ExtensionJournal[0].Status != hook.InvocationFailed ||
		snapshot.ExtensionJournal[0].EffectDisposition != hook.EffectApplied || !lastRunHasTermination(snapshot.History, session.TerminationExtension, "completion_failed") {
		t.Fatalf("completion failure = %#v / %#v", snapshot.ExtensionJournal, snapshot.History)
	}
}

func TestCompletionGateChainIsValidatedAndFrozenAtBuild(t *testing.T) {
	for name, gates := range map[string][]hook.CompletionGate{
		"typed nil": {(*completionGateSequence)(nil)},
		"duplicate": {newCompletionGateSequence("duplicate", hook.CompletionComplete), newCompletionGateSequence("duplicate", hook.CompletionComplete)},
	} {
		t.Run(name, func(t *testing.T) {
			app := NewApplication(ApplicationSpec{
				Name: "invalid-completion", DefaultModelConfig: testDefaultModel(),
				Modules: []agentslot.Module{
					session.NewMemoryModule(), executorModule{executor: model.NewFakeModelExecutor()}, completionGateModule{gates: gates},
					NewGatewayChannelModule("entrypoint.invalid-completion", "invalid-completion", &captureChannel{}),
				},
			})
			if _, err := app.Build(); err == nil {
				t.Fatal("invalid CompletionGate chain was accepted")
			}
		})
	}
}

func TestPreparedCompletionGateReplaysExactlyOnceAfterRecovery(t *testing.T) {
	store, sessionID := seedCompletionRecoverySession(t, hook.InvocationPrepared)
	gate := newCompletionGateSequence("recovery", hook.CompletionComplete)
	access, stop := startToolPreflightApplication(t, store, model.NewFakeModelExecutor(), AgentRuntimeConfig{}, completionGateModule{gates: []hook.CompletionGate{gate}})
	defer stop()
	if _, err := access.ResumeSession(t.Context(), interaction.ResumeSessionRequest{SessionID: sessionID}); err != nil {
		t.Fatal(err)
	}
	if err := access.WhenIdle(t.Context(), interaction.WhenIdleRequest{SessionID: sessionID}); err != nil {
		t.Fatal(err)
	}
	final, err := store.Load(t.Context(), session.SessionRef{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(gate.views) != 1 || final.RunState != session.RunIdle || final.ExtensionJournal[0].Status != hook.InvocationSucceeded ||
		final.ExtensionJournal[0].EffectDisposition != hook.EffectApplied {
		t.Fatalf("prepared completion recovery = calls:%d state:%q journal:%#v", len(gate.views), final.RunState, final.ExtensionJournal)
	}
}

func TestPendingCompletionGateBecomesUnknownAndNeverReplaysAfterRecovery(t *testing.T) {
	store, sessionID := seedCompletionRecoverySession(t, hook.InvocationPending)
	gate := newCompletionGateSequence("recovery", hook.CompletionComplete)
	access, stop := startToolPreflightApplication(t, store, model.NewFakeModelExecutor(), AgentRuntimeConfig{}, completionGateModule{gates: []hook.CompletionGate{gate}})
	defer stop()
	if _, err := access.ResumeSession(t.Context(), interaction.ResumeSessionRequest{SessionID: sessionID}); err != nil {
		t.Fatal(err)
	}
	if err := access.WhenIdle(t.Context(), interaction.WhenIdleRequest{SessionID: sessionID}); err != nil {
		t.Fatal(err)
	}
	final, err := store.Load(t.Context(), session.SessionRef{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	entry := final.ExtensionJournal[0]
	if len(gate.views) != 0 || entry.Status != hook.InvocationOutcomeUnknown || entry.EffectDisposition != hook.EffectApplied ||
		!lastRunHasTermination(final.History, session.TerminationExtension, "hook_outcome_unknown") {
		t.Fatalf("pending completion recovery = calls:%d journal:%#v history:%#v", len(gate.views), entry, final.History)
	}
}

func TestSucceededCompletionContinuationIsAppliedWithoutReplayAfterRecovery(t *testing.T) {
	store, sessionID := seedCompletionRecoverySession(t, hook.InvocationSucceeded)
	gate := newCompletionGateSequence("recovery", hook.CompletionComplete)
	executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{complete("recovered completion")}})
	access, stop := startToolPreflightApplication(t, store, executor, AgentRuntimeConfig{}, completionGateModule{gates: []hook.CompletionGate{gate}})
	defer stop()
	if _, err := access.ResumeSession(t.Context(), interaction.ResumeSessionRequest{SessionID: sessionID}); err != nil {
		t.Fatal(err)
	}
	if err := access.WhenIdle(t.Context(), interaction.WhenIdleRequest{SessionID: sessionID}); err != nil {
		t.Fatal(err)
	}
	requests := executor.Requests()
	final, err := store.Load(t.Context(), session.SessionRef{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || countInputText(requests[0].Inputs, "recovered completion context") != 1 || len(gate.views) != 1 ||
		final.ExtensionJournal[0].ContextDisposition != hook.ContextConsumed || final.ExtensionJournal[0].EffectDisposition != hook.EffectApplied {
		t.Fatalf("succeeded completion recovery = requests:%#v calls:%d journal:%#v", requests, len(gate.views), final.ExtensionJournal)
	}
}

func TestCompletionRecoveryDefinitionMismatchFailsClosedAndSettlesReservations(t *testing.T) {
	store, sessionID := seedCompletionRecoverySession(t, hook.InvocationPrepared)
	gate := newCompletionGateSequence("changed-definition", hook.CompletionComplete)
	access, stop := startToolPreflightApplication(t, store, model.NewFakeModelExecutor(), AgentRuntimeConfig{}, completionGateModule{gates: []hook.CompletionGate{gate}})
	defer stop()
	if _, err := access.ResumeSession(t.Context(), interaction.ResumeSessionRequest{SessionID: sessionID}); err != nil {
		t.Fatal(err)
	}
	if err := access.WhenIdle(t.Context(), interaction.WhenIdleRequest{SessionID: sessionID}); err != nil {
		t.Fatal(err)
	}
	final, err := store.Load(t.Context(), session.SessionRef{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(gate.views) != 0 || final.ExtensionJournal[0].Status != hook.InvocationCanceled ||
		final.ExtensionJournal[0].EffectDisposition != hook.EffectDiscarded ||
		!lastRunHasTermination(final.History, session.TerminationExtension, "completion_gate_definition_mismatch") {
		t.Fatalf("completion definition mismatch = calls:%d journal:%#v history:%#v", len(gate.views), final.ExtensionJournal, final.History)
	}
}

func TestSteerSupersedesInFlightCompletionContextWithoutRewritingHistory(t *testing.T) {
	gate := newBlockingCompletionGate("steer")
	executor := newRound7Executor(nil, nil,
		model.FakeExecution{Events: []model.ModelEvent{complete("candidate history")}},
		model.FakeExecution{Events: []model.ModelEvent{complete("after steer")}},
	)
	access, store, stop := startRound7Application(t, executor, AgentRuntimeConfig{}, completionGateModule{gates: []hook.CompletionGate{gate}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	done := make(chan error, 1)
	go func() {
		result, err := access.SendAndWait(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("begin")})
		if err == nil && result.Outcome != session.RunCompleted {
			err = context.Canceled
		}
		done <- err
	}()
	waitForSignal(t, gate.entered, "CompletionGate")
	blocked, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	view, err := access.View(t.Context(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := access.Steer(t.Context(), interaction.SteerRequest{SessionID: opened.SessionID, ExpectedRevision: view.Revision, Input: textInput("operator steer")}); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	requests := executor.fake.Requests()
	final, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || countInputText(requests[1].Inputs, "operator steer") != 1 || countInputText(requests[1].Inputs, "blocked completion context") != 0 {
		t.Fatalf("steer request context = %#v", requests)
	}
	if !historyHasUnchangedText(blocked.History, "candidate history") || !historyHasUnchangedText(final.History, "candidate history") || historyHasUnchangedText(final.History, "mutated candidate") {
		t.Fatalf("CompletionGate rewrote append-only History: before=%#v after=%#v", blocked.History, final.History)
	}
	if final.ExtensionJournal[0].EffectDisposition != hook.EffectDiscarded || final.ExtensionJournal[0].ContextDisposition != hook.ContextDiscarded {
		t.Fatalf("superseded completion context was not discarded: %#v", final.ExtensionJournal)
	}
}

func TestCancelDiscardsInFlightCompletionAndCancelsTheRun(t *testing.T) {
	gate := newBlockingCompletionGate("cancel")
	executor := newRound7Executor(nil, nil, model.FakeExecution{Events: []model.ModelEvent{complete("candidate")}})
	access, store, stop := startRound7Application(t, executor, AgentRuntimeConfig{}, completionGateModule{gates: []hook.CompletionGate{gate}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	result := make(chan interaction.RunResult, 1)
	errs := make(chan error, 1)
	go func() {
		value, err := access.SendAndWait(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("begin")})
		result <- value
		errs <- err
	}()
	waitForSignal(t, gate.entered, "CompletionGate")
	view, err := access.View(t.Context(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if err := access.Cancel(t.Context(), interaction.CancelRequest{SessionID: opened.SessionID, ExpectedRevision: view.Revision}); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if runResult := <-result; runResult.Outcome != session.RunCanceled {
		t.Fatalf("canceled completion result = %#v", runResult)
	}
	final, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range final.ExtensionJournal {
		if entry.EffectDisposition == hook.EffectPending || entry.ContextDisposition == hook.ContextPending {
			t.Fatalf("cancel left a dangling completion effect: %#v", entry)
		}
	}
}

func TestGoalContinueSkipsStopAndAppliedFollowOnCountIsRestoredFromJournal(t *testing.T) {
	executor := newRound7Executor(nil, nil,
		model.FakeExecution{Events: []model.ModelEvent{complete("goal progress")}},
		model.FakeExecution{Events: []model.ModelEvent{complete("goal candidate")}},
		model.FakeExecution{Events: []model.ModelEvent{complete("stop continuation result")}},
	)
	goalStore := goal.NewMemoryStore()
	evaluator := &goalEvaluatorSequence{evaluations: []goal.Evaluation{
		{Decision: goal.DecisionContinue, Reason: goal.ReasonProgressPossible, NextInstruction: textInput("goal next instruction")},
		{Decision: goal.DecisionDone, Reason: goal.ReasonObjectiveMet},
		{Decision: goal.DecisionDone, Reason: goal.ReasonObjectiveMet},
	}}
	gate := newCompletionGateSequence("goal-order", hook.CompletionContinue, hook.CompletionComplete)
	access, _, stop := startRound7Application(t, executor, AgentRuntimeConfig{}, goalStoreModule{store: goalStore}, goalEvaluatorModule{evaluator: evaluator}, completionGateModule{gates: []hook.CompletionGate{gate}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := goalStore.Set(t.Context(), goal.SetRequest{SessionID: opened.SessionID, Objective: "finish in order", MaxFollowOns: 4}); err != nil {
		t.Fatal(err)
	}
	result, err := access.SendAndWait(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("begin")})
	if err != nil || result.Outcome != session.RunCompleted {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	requests := executor.fake.Requests()
	views := gate.snapshotViews()
	if len(requests) != 3 || countInputText(requests[1].Inputs, "goal next instruction") != 1 || len(views) != 2 {
		t.Fatalf("Goal/Stop order = requests:%#v views:%#v", requests, views)
	}
	if views[0].FollowOns != 0 || views[1].FollowOns != 1 || views[0].RunID != views[1].RunID || views[0].Budget.MaxTokens != views[1].Budget.MaxTokens {
		t.Fatalf("Stop follow-on accounting = %#v", views)
	}
}

func TestNoCompletionGateKeepsTheJournalFreeOfSyntheticEntries(t *testing.T) {
	executor := newRound7Executor(nil, nil, model.FakeExecution{Events: []model.ModelEvent{complete("done")}})
	access, store, stop := startRound7Application(t, executor, AgentRuntimeConfig{})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	result, err := access.SendAndWait(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("begin")})
	if err != nil || result.Outcome != session.RunCompleted {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	final, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(final.ExtensionJournal) != 0 {
		t.Fatalf("no-gate Run created extension state: %#v", final.ExtensionJournal)
	}
}

func TestCompletionGatePersistenceCutPointsFailClosedWithoutDanglingInvocations(t *testing.T) {
	for _, test := range []struct {
		operation string
		wantCalls int
	}{
		{operation: "completion-gate-prepare", wantCalls: 0},
		{operation: "completion-gate-pending", wantCalls: 0},
		{operation: "completion-gate-apply", wantCalls: 1},
	} {
		t.Run(test.operation, func(t *testing.T) {
			store := newTransitionFaultStore(test.operation)
			gate := newCompletionGateSequence("persistence-fault", hook.CompletionContinue)
			executor := newRound7Executor(nil, nil, model.FakeExecution{Events: []model.ModelEvent{complete("candidate")}})
			access, stop := startToolPreflightApplication(t, store, executor, AgentRuntimeConfig{}, completionGateModule{gates: []hook.CompletionGate{gate}})
			defer stop()
			opened := createRuntimeTestSession(t, access)
			result, err := access.SendAndWait(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("fault")})
			if err != nil || (result.Outcome != session.RunFailed && result.Outcome != session.RunInterrupted) {
				t.Fatalf("result=%#v err=%v", result, err)
			}
			if len(gate.snapshotViews()) != test.wantCalls || len(executor.fake.Requests()) != 1 {
				t.Fatalf("calls=%d requests=%d", len(gate.snapshotViews()), len(executor.fake.Requests()))
			}
			final, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range final.ExtensionJournal {
				if !entry.Status.Terminal() || entry.EffectDisposition == hook.EffectPending || entry.ContextDisposition == hook.ContextPending {
					t.Fatalf("dangling entry after %s: %#v", test.operation, entry)
				}
			}
		})
	}
}

func TestGoalCandidateTerminalCommitFaultDoesNotCommitGoalOrLeaveDanglingEffect(t *testing.T) {
	store := newTransitionFaultStore("completion-gate-terminal")
	goalStore := goal.NewMemoryStore()
	evaluator := &goalEvaluatorSequence{evaluations: []goal.Evaluation{{Decision: goal.DecisionDone, Reason: goal.ReasonObjectiveMet}}}
	gate := newCompletionGateSequence("goal-terminal-fault", hook.CompletionContinue)
	executor := newRound7Executor(nil, nil, model.FakeExecution{Events: []model.ModelEvent{complete("candidate")}})
	access, stop := startToolPreflightApplication(t, store, executor, AgentRuntimeConfig{},
		goalStoreModule{store: goalStore}, goalEvaluatorModule{evaluator: evaluator}, completionGateModule{gates: []hook.CompletionGate{gate}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := goalStore.Set(t.Context(), goal.SetRequest{SessionID: opened.SessionID, Objective: "do not commit early", MaxFollowOns: 2}); err != nil {
		t.Fatal(err)
	}
	result, err := access.SendAndWait(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("begin")})
	if err != nil || result.Outcome != session.RunInterrupted {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	current, ok, err := goalStore.Current(t.Context(), opened.SessionID)
	if err != nil || !ok || current.Status != goal.StatusActive {
		t.Fatalf("Goal crossed failed terminal commit: %#v ok=%t err=%v", current, ok, err)
	}
	final, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range final.ExtensionJournal {
		if entry.EffectDisposition == hook.EffectPending || entry.ContextDisposition == hook.ContextPending {
			t.Fatalf("terminal fault left dangling completion state: %#v", entry)
		}
	}
}

func TestCompletionGateChainPipelinesJournalTransitionsWithoutExtraFileRewrites(t *testing.T) {
	store := newPreflightRecordingStore()
	first := newCompletionGateSequence("write-first", hook.CompletionComplete)
	second := newCompletionGateSequence("write-second", hook.CompletionComplete)
	executor := newRound7Executor(nil, nil, model.FakeExecution{Events: []model.ModelEvent{complete("done")}})
	access, stop := startToolPreflightApplication(t, store, executor, AgentRuntimeConfig{}, completionGateModule{gates: []hook.CompletionGate{first, second}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	result, err := access.SendAndWait(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("begin")})
	if err != nil || result.Outcome != session.RunCompleted {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if store.runtimeOperationCount("completion-gate-prepare") != 1 || store.runtimeOperationCount("completion-gate-pending") != 1 ||
		store.runtimeOperationCount("completion-gate-terminal") != 1 || store.runtimeOperationCount("completion-gate-apply") != 1 {
		t.Fatalf("CompletionGate state transitions were not pipelined: operations=%#v", store.operations)
	}
}

func TestCompletionApplyCommitThatBecameDurableBeforeItsErrorIsNotMisreportedAsFailure(t *testing.T) {
	store := &appliedCompletionFaultStore{MemoryStore: session.NewMemoryStore(), operation: "completion-gate-apply"}
	gate := newCompletionGateSequence("durable-apply", hook.CompletionContinue, hook.CompletionComplete)
	executor := newRound7Executor(nil, nil,
		model.FakeExecution{Events: []model.ModelEvent{complete("candidate")}},
		model.FakeExecution{Events: []model.ModelEvent{complete("continued")}},
	)
	access, stop := startToolPreflightApplication(t, store, executor, AgentRuntimeConfig{}, completionGateModule{gates: []hook.CompletionGate{gate}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	result, err := access.SendAndWait(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("begin")})
	if err != nil || result.Outcome != session.RunCompleted {
		t.Fatalf("durable apply result=%#v err=%v", result, err)
	}
	final, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range final.ExtensionJournal {
		if entry.EffectDisposition != hook.EffectApplied {
			t.Fatalf("durable apply was not preserved: %#v", final.ExtensionJournal)
		}
	}
}

func TestGoalDecisionThatBecameDurableBeforeItsErrorIsRetriedIdempotently(t *testing.T) {
	goalStore := &appliedGoalFaultStore{MemoryStore: goal.NewMemoryStore()}
	evaluator := &goalEvaluatorSequence{evaluations: []goal.Evaluation{{Decision: goal.DecisionDone, Reason: goal.ReasonObjectiveMet}}}
	gate := newCompletionGateSequence("durable-goal", hook.CompletionComplete)
	executor := newRound7Executor(nil, nil, model.FakeExecution{Events: []model.ModelEvent{complete("candidate")}})
	access, _, stop := startRound7Application(t, executor, AgentRuntimeConfig{},
		goalStoreModule{store: goalStore}, goalEvaluatorModule{evaluator: evaluator}, completionGateModule{gates: []hook.CompletionGate{gate}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := goalStore.Set(t.Context(), goal.SetRequest{SessionID: opened.SessionID, Objective: "finish once", MaxFollowOns: 2}); err != nil {
		t.Fatal(err)
	}
	result, err := access.SendAndWait(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("begin")})
	if err != nil || result.Outcome != session.RunCompleted {
		t.Fatalf("durable Goal result=%#v err=%v", result, err)
	}
	current, ok, err := goalStore.Current(t.Context(), opened.SessionID)
	if err != nil || !ok || current.Status != goal.StatusCompleted || current.Version != 2 {
		t.Fatalf("Goal decision was not exactly once: %#v ok=%t err=%v", current, ok, err)
	}
}

func TestRecoveryConvergesWhenGoalDecisionWasCommittedBeforeCompletionEffect(t *testing.T) {
	store := session.NewMemoryStore()
	goalStore := goal.NewMemoryStore()
	sessionID := agent.SessionID("session-goal-completion-recovery")
	runID, stepID, nextStep := agent.RunID("run-goal-recovery"), agent.StepID("step-goal-source"), agent.StepID("step-goal-target")
	config := testDefaultModel()
	started := session.RunFact{SessionID: sessionID, RunID: runID, Kind: session.RunStarted, ModelConfig: config, ConfigRevision: 1}
	assistant := agent.Message{
		ID: "message-goal-recovery", SessionID: sessionID, RunID: runID, StepID: stepID, Role: agent.RoleAssistant,
		Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "candidate"}},
	}
	created, err := store.Create(t.Context(), session.NewSession{
		Session: agent.Session{ID: sessionID, AgentID: "agent", WorkspaceID: "workspace"}, History: []session.HistoryFact{{Run: &started}, {Message: &assistant}},
		ModelConfig: config, RunState: session.RunRunning, ActiveRunID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	current, err := goalStore.Set(t.Context(), goal.SetRequest{SessionID: sessionID, Objective: "recover exact done", MaxFollowOns: 2})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	view := hook.CompletionView{
		InvocationID: "goal-completion-recovery-invocation", SessionID: sessionID, AgentID: "agent", WorkspaceID: "workspace",
		Revision: created.Revision.Next(), RunID: runID, StepID: stepID, NextStepID: nextStep, LastAssistantMessage: assistant,
		GoalCandidate: &hook.CompletionGoalCandidate{GoalID: current.ID, Version: current.Version},
	}
	fingerprint, err := hook.FingerprintTypedInput(view)
	if err != nil {
		t.Fatal(err)
	}
	entry := session.ExtensionJournalEntry{
		InvocationID: view.InvocationID, Sequence: 1, Descriptor: preflightDescriptor("completion-recovery"), Boundary: hook.BoundaryCompletion,
		SessionID: sessionID, RunID: runID, StepID: stepID, TargetStepID: nextStep, MessageID: assistant.ID, GoalID: current.ID, GoalVersion: current.Version,
		InputDigest: fingerprint.Digest, PreparedRevision: view.Revision, PreparedAt: now,
		Status: hook.InvocationPrepared, EffectDisposition: hook.EffectNone, ContextDisposition: hook.ContextNone,
	}
	commit, err := store.Commit(t.Context(), session.CommitRequest{
		SessionID: sessionID, ExpectedRevision: created.Revision, IdempotencyKey: "goal-completion-prepare",
		Changes: []session.Change{{Kind: session.UpdateExtensionJournal, Extension: &entry}},
	})
	if err != nil {
		t.Fatal(err)
	}
	entry.Status, entry.PendingAt = hook.InvocationPending, now.Add(time.Millisecond)
	commit, err = store.Commit(t.Context(), session.CommitRequest{
		SessionID: sessionID, ExpectedRevision: commit.Revision, IdempotencyKey: "goal-completion-pending",
		Changes: []session.Change{{Kind: session.UpdateExtensionJournal, Extension: &entry}},
	})
	if err != nil {
		t.Fatal(err)
	}
	entry.Status, entry.FinishedAt = hook.InvocationSucceeded, now.Add(2*time.Millisecond)
	entry.Result, entry.EffectDisposition = &hook.InvocationResult{Decision: hook.DecisionComplete}, hook.EffectPending
	if _, err := store.Commit(t.Context(), session.CommitRequest{
		SessionID: sessionID, ExpectedRevision: commit.Revision, IdempotencyKey: "goal-completion-terminal",
		Changes: []session.Change{{Kind: session.UpdateExtensionJournal, Extension: &entry}},
	}); err != nil {
		t.Fatal(err)
	}
	record := goal.DecisionRecord{
		ID:     "goal-decision-" + string(runID) + "-" + string(stepID) + "-" + strconv.FormatUint(current.Version, 10),
		GoalID: current.ID, SessionID: sessionID, RunID: runID, StepID: stepID, ExpectedVersion: current.Version,
		Evaluation: goal.Evaluation{Decision: goal.DecisionDone, Reason: goal.ReasonObjectiveMet}, RecordedAt: now,
	}
	if _, err := goalStore.RecordDecision(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	gate := newCompletionGateSequence("recovery", hook.CompletionComplete)
	access, stop := startToolPreflightApplication(t, store, model.NewFakeModelExecutor(), AgentRuntimeConfig{},
		goalStoreModule{store: goalStore}, goalEvaluatorModule{evaluator: &goalEvaluatorSequence{}}, completionGateModule{gates: []hook.CompletionGate{gate}})
	defer stop()
	if _, err := access.ResumeSession(t.Context(), interaction.ResumeSessionRequest{SessionID: sessionID}); err != nil {
		t.Fatal(err)
	}
	if err := access.WhenIdle(t.Context(), interaction.WhenIdleRequest{SessionID: sessionID}); err != nil {
		t.Fatal(err)
	}
	final, err := store.Load(t.Context(), session.SessionRef{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(gate.snapshotViews()) != 0 || final.ExtensionJournal[0].EffectDisposition != hook.EffectApplied || final.RunState != session.RunIdle {
		t.Fatalf("Goal/effect recovery did not converge: calls=%d journal=%#v state=%s", len(gate.snapshotViews()), final.ExtensionJournal, final.RunState)
	}
}

func TestGoalDoneAndCompletionEffectConvergeAfterOneApplyCommitFault(t *testing.T) {
	store := newTransitionFaultStore("completion-gate-apply")
	goalStore := goal.NewMemoryStore()
	evaluator := &goalEvaluatorSequence{evaluations: []goal.Evaluation{{Decision: goal.DecisionDone, Reason: goal.ReasonObjectiveMet}}}
	gate := newCompletionGateSequence("goal-apply-retry", hook.CompletionComplete)
	executor := newRound7Executor(nil, nil, model.FakeExecution{Events: []model.ModelEvent{complete("candidate")}})
	access, stop := startToolPreflightApplication(t, store, executor, AgentRuntimeConfig{},
		goalStoreModule{store: goalStore}, goalEvaluatorModule{evaluator: evaluator}, completionGateModule{gates: []hook.CompletionGate{gate}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := goalStore.Set(t.Context(), goal.SetRequest{SessionID: opened.SessionID, Objective: "finish durably", MaxFollowOns: 2}); err != nil {
		t.Fatal(err)
	}
	result, err := access.SendAndWait(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("begin")})
	if err != nil || result.Outcome != session.RunCompleted {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	current, ok, err := goalStore.Current(t.Context(), opened.SessionID)
	if err != nil || !ok || current.Status != goal.StatusCompleted {
		t.Fatalf("Goal after apply retry = %#v ok=%t err=%v", current, ok, err)
	}
	final, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if final.ExtensionJournal[0].EffectDisposition != hook.EffectApplied {
		t.Fatalf("completion effect did not converge: %#v", final.ExtensionJournal)
	}
}

func seedCompletionRecoverySession(t *testing.T, status hook.InvocationStatus) (*session.MemoryStore, agent.SessionID) {
	t.Helper()
	store := session.NewMemoryStore()
	sessionID := agent.SessionID("session-completion-recovery-" + string(status))
	runID, stepID, nextStep := agent.RunID("run-completion-recovery"), agent.StepID("step-completion-source"), agent.StepID("step-completion-target")
	config := testDefaultModel()
	started := session.RunFact{SessionID: sessionID, RunID: runID, Kind: session.RunStarted, ModelConfig: config, ConfigRevision: 1}
	assistant := agent.Message{
		ID: "message-completion-recovery", SessionID: sessionID, RunID: runID, StepID: stepID, Role: agent.RoleAssistant,
		Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "candidate"}},
	}
	created, err := store.Create(t.Context(), session.NewSession{
		Session: agent.Session{ID: sessionID, AgentID: "agent", WorkspaceID: "workspace"}, History: []session.HistoryFact{{Run: &started}, {Message: &assistant}},
		ModelConfig: config, RunState: session.RunRunning, ActiveRunID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := hook.ExtensionDescriptor{Key: "test.completion-recovery", DefinitionDigest: "sha256:" + strings.Repeat("b", 64)}
	view := hook.CompletionView{
		InvocationID: "completion-recovery-invocation", SessionID: sessionID, AgentID: "agent", WorkspaceID: "workspace",
		Revision: created.Revision.Next(), RunID: runID, StepID: stepID, NextStepID: nextStep, LastAssistantMessage: assistant,
		Budget: model.TokenBudget{}, FollowOns: 0,
	}
	fingerprint, err := hook.FingerprintTypedInput(view)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	entry := session.ExtensionJournalEntry{
		InvocationID: view.InvocationID, Sequence: 1, Descriptor: descriptor, Boundary: hook.BoundaryCompletion,
		SessionID: sessionID, RunID: runID, StepID: stepID, TargetStepID: nextStep, MessageID: assistant.ID,
		InputDigest: fingerprint.Digest, PreparedRevision: view.Revision, PreparedAt: now,
		Status: hook.InvocationPrepared, EffectDisposition: hook.EffectNone, ContextDisposition: hook.ContextNone,
	}
	commit, err := store.Commit(t.Context(), session.CommitRequest{
		SessionID: sessionID, ExpectedRevision: created.Revision, IdempotencyKey: "completion-recovery-prepare",
		Changes: []session.Change{{Kind: session.UpdateExtensionJournal, Extension: &entry}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if status == hook.InvocationPrepared {
		return store, sessionID
	}
	entry.Status, entry.PendingAt = hook.InvocationPending, now.Add(time.Millisecond)
	commit, err = store.Commit(t.Context(), session.CommitRequest{
		SessionID: sessionID, ExpectedRevision: commit.Revision, IdempotencyKey: "completion-recovery-pending",
		Changes: []session.Change{{Kind: session.UpdateExtensionJournal, Extension: &entry}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if status == hook.InvocationPending {
		return store, sessionID
	}
	contextMessage := agent.Message{
		ID: "completion-recovery-context", SessionID: sessionID, RunID: runID, StepID: nextStep, Role: agent.RoleUser,
		Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "recovered completion context"}},
	}
	entry.Status, entry.FinishedAt = hook.InvocationSucceeded, now.Add(2*time.Millisecond)
	entry.Result = &hook.InvocationResult{Decision: hook.DecisionContinue}
	entry.EffectDisposition, entry.ContextDisposition = hook.EffectPending, hook.ContextPending
	entry.ContextInputs = []model.Input{{Message: &contextMessage}}
	contextFingerprint, err := hook.FingerprintTypedInput(entry.ContextInputs)
	if err != nil {
		t.Fatal(err)
	}
	entry.ContextDigest, entry.ContextBytes = contextFingerprint.Digest, contextFingerprint.Bytes
	if _, err := store.Commit(t.Context(), session.CommitRequest{
		SessionID: sessionID, ExpectedRevision: commit.Revision, IdempotencyKey: "completion-recovery-terminal",
		Changes: []session.Change{{Kind: session.UpdateExtensionJournal, Extension: &entry}},
	}); err != nil {
		t.Fatal(err)
	}
	return store, sessionID
}

type completionGateSequence struct {
	mu              sync.Mutex
	descriptor      hook.ExtensionDescriptor
	decisions       []hook.CompletionDecision
	views           []hook.CompletionView
	err             error
	afterEvaluation func(int)
	session         agent.SessionID
}

func newCompletionGateSequence(key string, decisions ...hook.CompletionDecision) *completionGateSequence {
	return &completionGateSequence{descriptor: preflightDescriptor("completion-" + key), decisions: decisions}
}

func (g *completionGateSequence) Descriptor() hook.ExtensionDescriptor { return g.descriptor }
func (g *completionGateSequence) Evaluate(_ context.Context, view hook.CompletionView) (hook.CompletionGateResult, error) {
	g.mu.Lock()
	g.views = append(g.views, view)
	call := len(g.views)
	decision := hook.CompletionComplete
	if len(g.decisions) > 0 {
		decision = g.decisions[0]
		g.decisions = g.decisions[1:]
	}
	err := g.err
	after := g.afterEvaluation
	g.mu.Unlock()
	if after != nil {
		after(call)
	}
	if err != nil {
		return hook.CompletionGateResult{}, err
	}
	result := hook.CompletionGateResult{Decision: decision}
	if decision == hook.CompletionContinue {
		result.Context = []model.Input{{Message: &agent.Message{
			ID: agent.MessageID("completion-context-" + string(view.InvocationID)), SessionID: view.SessionID, RunID: view.RunID, StepID: view.NextStepID,
			Role: agent.RoleUser, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "completion context " + strconv.Itoa(call)}},
		}}}
	}
	return result, nil
}
func (g *completionGateSequence) setSessionID(id agent.SessionID) {
	g.mu.Lock()
	g.session = id
	g.mu.Unlock()
}
func (g *completionGateSequence) sessionID() agent.SessionID {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.session
}
func (g *completionGateSequence) snapshotViews() []hook.CompletionView {
	g.mu.Lock()
	defer g.mu.Unlock()
	views := make([]hook.CompletionView, len(g.views))
	for index, view := range g.views {
		views[index] = cloneCompletionView(view)
	}
	return views
}

type completionGateModule struct{ gates []hook.CompletionGate }

func (completionGateModule) ID() string { return "test.completion-gate" }
func (m completionGateModule) Register(reg agentslot.Registrar) error {
	contributions := make([]agentslot.Contribution, 0, len(m.gates))
	for _, gate := range m.gates {
		contributions = append(contributions, agentslot.Append(hook.CompletionGateSlot, gate))
	}
	return reg.Contribute(contributions...)
}

func historyHasUnchangedText(history []session.HistoryFact, text string) bool {
	for _, fact := range history {
		if fact.Message != nil && len(fact.Message.Parts) == 1 && fact.Message.Parts[0].Text == text {
			return true
		}
	}
	return false
}

var _ hook.CompletionGate = (*completionGateSequence)(nil)

type blockingCompletionGate struct {
	mu         sync.Mutex
	descriptor hook.ExtensionDescriptor
	entered    chan struct{}
	release    chan struct{}
	calls      int
}

func newBlockingCompletionGate(key string) *blockingCompletionGate {
	return &blockingCompletionGate{descriptor: preflightDescriptor("completion-blocking-" + key), entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *blockingCompletionGate) Descriptor() hook.ExtensionDescriptor { return g.descriptor }
func (g *blockingCompletionGate) Evaluate(_ context.Context, view hook.CompletionView) (hook.CompletionGateResult, error) {
	g.mu.Lock()
	g.calls++
	call := g.calls
	g.mu.Unlock()
	if call == 1 {
		close(g.entered)
		view.LastAssistantMessage.Parts[0].Text = "mutated candidate"
		<-g.release
		return hook.CompletionGateResult{Decision: hook.CompletionContinue, Context: []model.Input{{Message: &agent.Message{
			ID: "blocking-completion-context", SessionID: view.SessionID, RunID: view.RunID, StepID: view.NextStepID, Role: agent.RoleUser,
			Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "blocked completion context"}},
		}}}}, nil
	}
	return hook.CompletionGateResult{Decision: hook.CompletionComplete}, nil
}

var _ hook.CompletionGate = (*blockingCompletionGate)(nil)

type appliedCompletionFaultStore struct {
	*session.MemoryStore
	operation string
	fired     atomic.Bool
}

type appliedGoalFaultStore struct {
	*goal.MemoryStore
	fired atomic.Bool
}

func (s *appliedGoalFaultStore) RecordDecision(ctx context.Context, record goal.DecisionRecord) (goal.Goal, error) {
	result, err := s.MemoryStore.RecordDecision(ctx, record)
	if err == nil && s.fired.CompareAndSwap(false, true) {
		return goal.Goal{}, errors.New("injected error after durable Goal decision")
	}
	return result, err
}

func (s *appliedCompletionFaultStore) Commit(ctx context.Context, request session.CommitRequest) (session.Commit, error) {
	commit, err := s.MemoryStore.Commit(ctx, request)
	if err != nil {
		return commit, err
	}
	if strings.Contains(request.IdempotencyKey, "runtime-"+s.operation+"-") && s.fired.CompareAndSwap(false, true) {
		return session.Commit{}, errors.New("injected error after durable completion commit")
	}
	return commit, nil
}
