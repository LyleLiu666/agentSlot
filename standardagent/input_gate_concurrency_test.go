package standardagent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/agent"
	agentcontext "github.com/LyleLiu666/agentSlot/context"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestInputGateSubmissionMutexIsPerSession(t *testing.T) {
	firstEntered := make(chan struct{})
	firstRelease := make(chan struct{})
	otherEntered := make(chan struct{})
	otherRelease := make(chan struct{})
	gate := &concurrentInputGate{
		descriptor: inputGateDescriptor("per-session"),
		evaluate: func(ctx context.Context, view hook.InputGateView) (hook.InputGateResult, error) {
			switch view.Input.Parts[0].Text {
			case "first in session":
				close(firstEntered)
				return waitForInputGateRelease(ctx, firstRelease)
			case "other session":
				close(otherEntered)
				return waitForInputGateRelease(ctx, otherRelease)
			default:
				return hook.InputGateResult{Decision: hook.DecisionAccept}, nil
			}
		},
	}
	access, _, stop := startRound7Application(t, model.NewFakeModelExecutor(
		model.FakeExecution{Events: []model.ModelEvent{complete("other done")}},
		model.FakeExecution{Events: []model.ModelEvent{complete("first done")}},
	), AgentRuntimeConfig{}, inputGateModule{gates: []hook.InputGate{gate}})
	defer stop()
	first := createRuntimeTestSession(t, access)
	other := createRuntimeTestSession(t, access)

	firstDone := make(chan error, 1)
	go func() {
		_, err := access.Send(context.Background(), interaction.SendRequest{
			SessionID: first.SessionID, ExpectedRevision: first.Revision, Input: textInput("first in session"),
		})
		firstDone <- err
	}()
	waitForSignal(t, firstEntered, "first Session InputGate")

	secondDone := make(chan error, 1)
	go func() {
		_, err := access.Send(context.Background(), interaction.SendRequest{
			SessionID: first.SessionID, ExpectedRevision: first.Revision, Input: textInput("second in session"),
		})
		secondDone <- err
	}()
	otherDone := make(chan error, 1)
	go func() {
		_, err := access.Send(context.Background(), interaction.SendRequest{
			SessionID: other.SessionID, ExpectedRevision: other.Revision, Input: textInput("other session"),
		})
		otherDone <- err
	}()
	waitForSignal(t, otherEntered, "other Session InputGate")
	if gate.callCount("second in session") != 0 {
		t.Fatal("a second submission entered the same Session while its first occurrence was pending")
	}

	close(otherRelease)
	if err := waitForError(t, otherDone, "other Session send"); err != nil {
		t.Fatal(err)
	}
	close(firstRelease)
	if err := waitForError(t, firstDone, "first Session send"); err != nil {
		t.Fatal(err)
	}
	if err := waitForError(t, secondDone, "stale same-Session send"); !agent.IsCode(err, agent.CodeRevisionConflict) {
		t.Fatalf("stale serialized Send error = %v", err)
	}
	if gate.callCount("second in session") != 0 {
		t.Fatal("stale same-Session Send invoked a component after CAS had already lost")
	}
}

func TestPendingInputGateDoesNotBlockTheActiveRun(t *testing.T) {
	hookEntered := make(chan struct{})
	hookRelease := make(chan struct{})
	gate := &concurrentInputGate{
		descriptor: inputGateDescriptor("run-concurrency"),
		evaluate: func(ctx context.Context, view hook.InputGateView) (hook.InputGateResult, error) {
			if view.Input.Parts[0].Text == "wait in hook" {
				close(hookEntered)
				return waitForInputGateRelease(ctx, hookRelease)
			}
			return hook.InputGateResult{Decision: hook.DecisionAccept}, nil
		},
	}
	modelRelease := make(chan struct{})
	executor := model.NewFakeModelExecutor(
		model.FakeExecution{Block: modelRelease, Events: []model.ModelEvent{complete("first done")}},
		model.FakeExecution{Events: []model.ModelEvent{complete("second done")}},
	)
	access, _, stop := startRound7Application(t, executor, AgentRuntimeConfig{}, inputGateModule{gates: []hook.InputGate{gate}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(t.Context(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("start run"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := executor.WaitForRequests(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	view, err := access.View(t.Context(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	sendDone := make(chan error, 1)
	go func() {
		_, err := access.Send(context.Background(), interaction.SendRequest{
			SessionID: opened.SessionID, ExpectedRevision: view.Revision, Input: textInput("wait in hook"),
		})
		sendDone <- err
	}()
	waitForSignal(t, hookEntered, "pending InputGate")

	close(modelRelease)
	idleCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := access.WhenIdle(idleCtx, interaction.WhenIdleRequest{SessionID: opened.SessionID}); err != nil {
		t.Fatalf("active Run was blocked by the submission mutex: %v", err)
	}
	close(hookRelease)
	if err := waitForError(t, sendDone, "pending-gate Send"); err != nil {
		t.Fatal(err)
	}
	if err := access.WhenIdle(t.Context(), interaction.WhenIdleRequest{SessionID: opened.SessionID}); err != nil {
		t.Fatal(err)
	}
}

func TestSubmissionMutexReleasesAfterTheInputMutationBeforeModelPreparation(t *testing.T) {
	gate := &concurrentInputGate{descriptor: inputGateDescriptor("release-before-model"), evaluate: func(_ context.Context, _ hook.InputGateView) (hook.InputGateResult, error) {
		return hook.InputGateResult{Decision: hook.DecisionAccept}, nil
	}}
	source := &blockingInputGateContextSource{entered: make(chan struct{}), release: make(chan struct{})}
	access, _, stop := startRound7Application(t,
		model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{complete("done")}}),
		AgentRuntimeConfig{}, inputGateModule{gates: []hook.InputGate{gate}}, contextSourceModule{id: "context.source.input-gate-block", source: source},
	)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	firstDone := make(chan error, 1)
	go func() {
		_, err := access.Send(context.Background(), interaction.SendRequest{
			SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("first preparing model"),
		})
		firstDone <- err
	}()
	waitForSignal(t, source.entered, "blocked ContextSource")
	view, err := access.View(t.Context(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	secondDone := make(chan error, 1)
	go func() {
		_, err := access.Send(context.Background(), interaction.SendRequest{
			SessionID: opened.SessionID, ExpectedRevision: view.Revision, Input: textInput("second while first prepares"),
		})
		secondDone <- err
	}()
	deadline := time.After(time.Second)
	for gate.callCount("second while first prepares") == 0 {
		select {
		case <-deadline:
			t.Fatal("submission mutex remained held during model preparation")
		case <-time.After(time.Millisecond):
		}
	}
	if err := waitForError(t, secondDone, "second Send during model preparation"); err != nil {
		t.Fatal(err)
	}
	close(source.release)
	if err := waitForError(t, firstDone, "first Send model preparation"); err != nil {
		t.Fatal(err)
	}
	if err := access.WhenIdle(t.Context(), interaction.WhenIdleRequest{SessionID: opened.SessionID}); err != nil {
		t.Fatal(err)
	}
}

func TestQueueClaimCanWinWhileEditInputGateRunsWithoutRewritingHistory(t *testing.T) {
	editEntered := make(chan struct{})
	editRelease := make(chan struct{})
	gate := &concurrentInputGate{
		descriptor: inputGateDescriptor("claim-edit-race"),
		evaluate: func(ctx context.Context, view hook.InputGateView) (hook.InputGateResult, error) {
			if view.Operation == hook.InputEditQueued {
				close(editEntered)
				if _, err := waitForInputGateRelease(ctx, editRelease); err != nil {
					return hook.InputGateResult{}, err
				}
			}
			contextMessage := agent.Message{
				ID: agent.MessageID("claim-race-context-" + view.InvocationID), SessionID: view.SessionID, Role: agent.RoleUser,
				Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "context for " + view.Input.Parts[0].Text}},
			}
			return hook.InputGateResult{Decision: hook.DecisionAccept, Context: []model.Input{{Message: &contextMessage}}}, nil
		},
	}
	firstRelease := make(chan struct{})
	secondRelease := make(chan struct{})
	executor := model.NewFakeModelExecutor(
		model.FakeExecution{Block: firstRelease, Events: []model.ModelEvent{complete("first done")}},
		model.FakeExecution{Block: secondRelease, Events: []model.ModelEvent{complete("second done")}},
	)
	access, store, stop := startRound7Application(t, executor, AgentRuntimeConfig{}, inputGateModule{gates: []hook.InputGate{gate}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(t.Context(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("active"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := executor.WaitForRequests(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	view, _ := access.View(t.Context(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	queued, err := access.Send(t.Context(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: view.Revision, Input: textInput("original queued"),
	})
	if err != nil {
		t.Fatal(err)
	}
	view, _ = access.View(t.Context(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	editDone := make(chan error, 1)
	go func(revision agent.Revision) {
		_, err := access.EditQueued(context.Background(), interaction.EditQueuedRequest{
			SessionID: opened.SessionID, MessageID: queued.MessageID, ExpectedRevision: revision, Input: textInput("stale edit"),
		})
		editDone <- err
	}(view.Revision)
	waitForSignal(t, editEntered, "EditQueued InputGate")
	close(firstRelease)
	if err := executor.WaitForRequests(t.Context(), 2); err != nil {
		t.Fatal(err)
	}
	close(editRelease)
	var gateErr *interaction.InputGateError
	if err := waitForError(t, editDone, "claimed EditQueued"); !errors.As(err, &gateErr) || !agent.IsCode(err, agent.CodeQueueItemClaimed) {
		t.Fatalf("claimed EditQueued error = %v / %#v", err, gateErr)
	}
	requests := executor.Requests()
	original, staleInput, originalContext, staleContext := 0, 0, 0, 0
	for _, input := range requests[1].Inputs {
		if input.Message == nil || len(input.Message.Parts) == 0 {
			continue
		}
		switch input.Message.Parts[0].Text {
		case "original queued":
			original++
		case "stale edit":
			staleInput++
		case "context for original queued":
			originalContext++
		case "context for stale edit":
			staleContext++
		}
	}
	if original != 1 || originalContext != 1 || staleInput != 0 || staleContext != 0 {
		t.Fatalf("claimed model request rewrote input/context: %#v", requests[1])
	}
	snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range snapshot.ExtensionJournal {
		if entry.MessageID == queued.MessageID && entry.ContextDisposition == hook.ContextPending {
			t.Fatalf("claim/edit race left pending stale context: %#v", entry)
		}
	}
	close(secondRelease)
}

func TestInputGateTriggersOnlyForSubmissionOperationsAndClientMessageIDIsCorrelation(t *testing.T) {
	gate := &concurrentInputGate{
		descriptor: inputGateDescriptor("operations"),
		evaluate: func(_ context.Context, _ hook.InputGateView) (hook.InputGateResult, error) {
			return hook.InputGateResult{Decision: hook.DecisionAccept}, nil
		},
	}
	modelRelease := make(chan struct{})
	executor := model.NewFakeModelExecutor(model.FakeExecution{Block: modelRelease, Events: []model.ModelEvent{complete("done")}})
	access, store, stop := startRound7Application(t, executor, AgentRuntimeConfig{}, inputGateModule{gates: []hook.InputGate{gate}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	first, err := access.Send(t.Context(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, ClientMessageID: "same-client-id", Input: textInput("active"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.WaitForRequests(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	view, _ := access.View(t.Context(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	steer, err := access.Steer(t.Context(), interaction.SteerRequest{
		SessionID: opened.SessionID, ExpectedRevision: view.Revision, ClientMessageID: "same-client-id", Input: textInput("steer"),
	})
	if err != nil {
		t.Fatal(err)
	}
	view, _ = access.View(t.Context(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	queued, err := access.Send(t.Context(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: view.Revision, ClientMessageID: "same-client-id", Input: textInput("queued"),
	})
	if err != nil {
		t.Fatal(err)
	}
	view, _ = access.View(t.Context(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if _, err := access.EditQueued(t.Context(), interaction.EditQueuedRequest{
		SessionID: opened.SessionID, MessageID: queued.MessageID, ExpectedRevision: view.Revision, Input: textInput("edited"),
	}); err != nil {
		t.Fatal(err)
	}
	if first.MessageID == steer.MessageID || first.MessageID == queued.MessageID || steer.MessageID == queued.MessageID {
		t.Fatal("reused ClientMessageID was treated as an exactly-once message identity")
	}
	beforeAdministrativeOperations := gate.callCount("")
	view, _ = access.View(t.Context(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if _, err := access.ReclassifyQueued(t.Context(), interaction.ReclassifyQueuedRequest{
		SessionID: opened.SessionID, MessageID: queued.MessageID, ExpectedRevision: view.Revision, Delivery: session.DeliveryHeld,
	}); err != nil {
		t.Fatal(err)
	}
	view, _ = access.View(t.Context(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if _, err := access.DeleteQueued(t.Context(), interaction.DeleteQueuedRequest{
		SessionID: opened.SessionID, MessageID: queued.MessageID, ExpectedRevision: view.Revision,
	}); err != nil {
		t.Fatal(err)
	}
	if gate.callCount("") != beforeAdministrativeOperations {
		t.Fatal("ReclassifyQueued or DeleteQueued incorrectly triggered InputGate")
	}
	snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ExtensionJournal) != 4 {
		t.Fatalf("InputGate invocation count = %d, want Send+Steer+Send+Edit", len(snapshot.ExtensionJournal))
	}
	close(modelRelease)
}

func TestRunPendingDoesNotTriggerInputGate(t *testing.T) {
	store := session.NewMemoryStore()
	created, err := store.Create(t.Context(), session.NewSession{
		Session:     agent.Session{ID: "run-pending-no-gate", AgentID: "agent", WorkspaceID: "workspace"},
		ModelConfig: testDefaultModel(), RunState: session.RunIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	message := agent.Message{ID: "pending-message", SessionID: created.Session.ID, Role: agent.RoleUser,
		Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "already accepted"}}}
	item := session.QueueItem{Message: message, Delivery: session.DeliveryNormal}
	committed, err := store.Commit(t.Context(), session.CommitRequest{
		SessionID: created.Session.ID, ExpectedRevision: created.Revision, IdempotencyKey: "seed-pending",
		Changes: []session.Change{{Kind: session.EnqueueMessage, QueueItem: &item}},
	})
	if err != nil {
		t.Fatal(err)
	}
	gate := &concurrentInputGate{descriptor: inputGateDescriptor("run-pending"), evaluate: func(_ context.Context, _ hook.InputGateView) (hook.InputGateResult, error) {
		return hook.InputGateResult{Decision: hook.DecisionAccept}, nil
	}}
	entry := &captureChannel{}
	running, err := NewApplication(ApplicationSpec{
		Name: "run-pending-no-gate", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			sessionPairModule{store: store}, executorModule{executor: model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{complete("done")}})},
			inputGateModule{gates: []hook.InputGate{gate}}, NewGatewayChannelModule("entrypoint.run-pending-no-gate", "test", entry),
		},
	}).Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = running.Stop(context.Background()) }()
	if _, err := entry.Access().ResumeSession(t.Context(), interaction.ResumeSessionRequest{SessionID: created.Session.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Access().RunPending(t.Context(), interaction.RunPendingRequest{
		SessionID: created.Session.ID, ExpectedRevision: committed.Revision,
	}); err != nil {
		t.Fatal(err)
	}
	if err := entry.Access().WhenIdle(t.Context(), interaction.WhenIdleRequest{SessionID: created.Session.ID}); err != nil {
		t.Fatal(err)
	}
	if gate.callCount("") != 0 {
		t.Fatal("RunPending incorrectly invoked InputGate for an already accepted QueueItem")
	}
}

func TestNoInputGateRunningSendKeepsTheSingleCommitFastPath(t *testing.T) {
	modelRelease := make(chan struct{})
	executor := model.NewFakeModelExecutor(model.FakeExecution{Block: modelRelease, Events: []model.ModelEvent{complete("done")}})
	access, store, stop := startRound7Application(t, executor, AgentRuntimeConfig{})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(t.Context(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("active"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := executor.WaitForRequests(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	view, err := access.View(t.Context(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := access.Send(t.Context(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: view.Revision, Input: textInput("queued without gates"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Revision != view.Revision.Next() {
		t.Fatalf("no-gate running Send revision = %d, want one commit at %d", receipt.Revision, view.Revision.Next())
	}
	snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ExtensionJournal) != 0 {
		t.Fatalf("no-gate fast path created extension state: %#v", snapshot.ExtensionJournal)
	}
	close(modelRelease)
}

func TestInputGateFailureCancelsUnstartedComponentsAndPreservesHistory(t *testing.T) {
	first := &concurrentInputGate{descriptor: inputGateDescriptor("failure"), evaluate: func(context.Context, hook.InputGateView) (hook.InputGateResult, error) {
		return hook.InputGateResult{}, errors.New("private adapter detail")
	}}
	second := &concurrentInputGate{descriptor: inputGateDescriptor("unstarted"), evaluate: func(context.Context, hook.InputGateView) (hook.InputGateResult, error) {
		return hook.InputGateResult{Decision: hook.DecisionAccept}, nil
	}}
	third := &concurrentInputGate{descriptor: inputGateDescriptor("also-unstarted"), evaluate: func(context.Context, hook.InputGateView) (hook.InputGateResult, error) {
		return hook.InputGateResult{Decision: hook.DecisionAccept}, nil
	}}
	access, store, stop := startRound7Application(t, model.NewFakeModelExecutor(), AgentRuntimeConfig{}, inputGateModule{gates: []hook.InputGate{first, second, third}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	_, err := access.Send(t.Context(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("must not append"),
	})
	var gateErr *interaction.InputGateError
	wantRevision := opened.Revision.Next().Next().Next().Next().Next()
	if !errors.As(err, &gateErr) || agent.CodeOf(err) != agent.CodeExtensionFailed || gateErr.CurrentRevision != wantRevision {
		t.Fatalf("InputGate failure = %v / %#v", err, gateErr)
	}
	snapshot, loadErr := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(snapshot.History) != 0 || len(snapshot.Queue) != 0 || len(snapshot.ExtensionJournal) != 3 {
		t.Fatalf("failed InputGate mutated input facts: %#v", snapshot)
	}
	if snapshot.ExtensionJournal[0].Status != hook.InvocationFailed || snapshot.ExtensionJournal[0].EffectDisposition != hook.EffectApplied ||
		snapshot.ExtensionJournal[1].Status != hook.InvocationCanceled || snapshot.ExtensionJournal[1].EffectDisposition != hook.EffectDiscarded ||
		snapshot.ExtensionJournal[2].Status != hook.InvocationCanceled || snapshot.ExtensionJournal[2].EffectDisposition != hook.EffectDiscarded ||
		second.callCount("") != 0 || third.callCount("") != 0 {
		t.Fatalf("failed InputGate journal = %#v", snapshot.ExtensionJournal)
	}
}

func TestCanceledInputGatePreservesCancellationClassification(t *testing.T) {
	entered := make(chan struct{})
	gate := &concurrentInputGate{descriptor: inputGateDescriptor("canceled"), evaluate: func(ctx context.Context, _ hook.InputGateView) (hook.InputGateResult, error) {
		close(entered)
		<-ctx.Done()
		return hook.InputGateResult{}, ctx.Err()
	}}
	access, store, stop := startRound7Application(t, model.NewFakeModelExecutor(), AgentRuntimeConfig{}, inputGateModule{gates: []hook.InputGate{gate}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := access.Send(ctx, interaction.SendRequest{
			SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("cancel this occurrence"),
		})
		done <- err
	}()
	waitForSignal(t, entered, "cancelable InputGate")
	cancel()
	var gateErr *interaction.InputGateError
	if err := waitForError(t, done, "canceled InputGate"); !errors.As(err, &gateErr) ||
		agent.KindOf(err) != agent.ErrorCanceled || agent.CodeOf(err) != agent.CodeCanceled {
		t.Fatalf("canceled InputGate error = %v / %#v", err, gateErr)
	}
	snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.History) != 0 || len(snapshot.Queue) != 0 || len(snapshot.ExtensionJournal) != 1 ||
		snapshot.ExtensionJournal[0].Status != hook.InvocationCanceled || snapshot.ExtensionJournal[0].EffectDisposition != hook.EffectApplied {
		t.Fatalf("canceled InputGate state = %#v", snapshot)
	}
}

func TestCanceledInputGatePreservesDeclaredOutcomeUnknown(t *testing.T) {
	entered := make(chan struct{})
	gate := &concurrentInputGate{descriptor: inputGateDescriptor("outcome-unknown"), evaluate: func(ctx context.Context, _ hook.InputGateView) (hook.InputGateResult, error) {
		close(entered)
		<-ctx.Done()
		return hook.InputGateResult{}, &hook.InvocationFailure{
			Status: hook.InvocationOutcomeUnknown, Code: agent.ErrorCode("hook_outcome_unknown"),
			Reason: "command termination was not confirmed", Cause: ctx.Err(),
		}
	}}
	access, store, stop := startRound7Application(t, model.NewFakeModelExecutor(), AgentRuntimeConfig{}, inputGateModule{gates: []hook.InputGate{gate}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := access.Send(ctx, interaction.SendRequest{
			SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("unknown side effect"),
		})
		done <- err
	}()
	waitForSignal(t, entered, "outcome-unknown InputGate")
	cancel()
	var gateErr *interaction.InputGateError
	if err := waitForError(t, done, "outcome-unknown InputGate"); !errors.As(err, &gateErr) ||
		agent.KindOf(err) != agent.ErrorUnavailable || agent.CodeOf(err) != agent.ErrorCode("hook_outcome_unknown") {
		t.Fatalf("outcome-unknown InputGate error = %v / %#v", err, gateErr)
	}
	snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.History) != 0 || len(snapshot.Queue) != 0 || len(snapshot.ExtensionJournal) != 1 ||
		snapshot.ExtensionJournal[0].Status != hook.InvocationOutcomeUnknown ||
		snapshot.ExtensionJournal[0].ErrorCode != agent.ErrorCode("hook_outcome_unknown") ||
		snapshot.ExtensionJournal[0].EffectDisposition != hook.EffectApplied {
		t.Fatalf("outcome-unknown InputGate state = %#v", snapshot)
	}
}

func TestPanickingInputGateIsDurablyFailedWithoutAppendingInput(t *testing.T) {
	gate := &concurrentInputGate{descriptor: inputGateDescriptor("panic"), evaluate: func(context.Context, hook.InputGateView) (hook.InputGateResult, error) {
		panic("private panic payload")
	}}
	access, store, stop := startRound7Application(t, model.NewFakeModelExecutor(), AgentRuntimeConfig{}, inputGateModule{gates: []hook.InputGate{gate}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	_, err := access.Send(t.Context(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("must survive panic"),
	})
	var gateErr *interaction.InputGateError
	if !errors.As(err, &gateErr) || agent.KindOf(err) != agent.ErrorUnavailable || agent.CodeOf(err) != agent.CodeExtensionFailed {
		t.Fatalf("panicking InputGate error = %v / %#v", err, gateErr)
	}
	snapshot, loadErr := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(snapshot.History) != 0 || len(snapshot.Queue) != 0 || len(snapshot.ExtensionJournal) != 1 ||
		snapshot.ExtensionJournal[0].Status != hook.InvocationFailed || snapshot.ExtensionJournal[0].ErrorReason != "input gate panicked" {
		t.Fatalf("panicking InputGate state = %#v", snapshot)
	}
}

type concurrentInputGate struct {
	descriptor hook.ExtensionDescriptor
	evaluate   func(context.Context, hook.InputGateView) (hook.InputGateResult, error)
	mu         sync.Mutex
	views      []hook.InputGateView
}

type blockingInputGateContextSource struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (*blockingInputGateContextSource) Key() string { return "input-gate-block" }

func (s *blockingInputGateContextSource) Contribute(ctx context.Context, _ agentcontext.ContextInput) ([]model.Input, error) {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (g *concurrentInputGate) Descriptor() hook.ExtensionDescriptor { return g.descriptor }

func (g *concurrentInputGate) Evaluate(ctx context.Context, view hook.InputGateView) (hook.InputGateResult, error) {
	g.mu.Lock()
	g.views = append(g.views, cloneInputGateView(view))
	g.mu.Unlock()
	return g.evaluate(ctx, view)
}

func (g *concurrentInputGate) callCount(input string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if input == "" {
		return len(g.views)
	}
	count := 0
	for _, view := range g.views {
		if len(view.Input.Parts) > 0 && view.Input.Parts[0].Text == input {
			count++
		}
	}
	return count
}

func waitForInputGateRelease(ctx context.Context, release <-chan struct{}) (hook.InputGateResult, error) {
	select {
	case <-release:
		return hook.InputGateResult{Decision: hook.DecisionAccept}, nil
	case <-ctx.Done():
		return hook.InputGateResult{}, ctx.Err()
	}
}

func waitForSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitForError(t *testing.T, result <-chan error, name string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
		return nil
	}
}
