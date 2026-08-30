package standardagent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestInputGateRejectAdvancesRevisionWithoutMutatingTheProposedInput(t *testing.T) {
	gate := &recordingInputGate{
		descriptor: inputGateDescriptor("reject"),
		result:     hook.InputGateResult{Decision: hook.DecisionReject, Reason: "project policy rejected the input"},
	}
	access, store, stop := startRound7Application(t, model.NewFakeModelExecutor(), AgentRuntimeConfig{}, inputGateModule{gates: []hook.InputGate{gate}})
	defer stop()
	opened := createRuntimeTestSession(t, access)

	_, err := access.Send(t.Context(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, ClientMessageID: "client-repeatable", Input: textInput("do not persist"),
	})
	var gateErr *interaction.InputGateError
	if !errors.As(err, &gateErr) || gateErr.CurrentRevision <= opened.Revision || len(gateErr.Diagnostics) != 1 {
		t.Fatalf("Send rejection = %v / %#v", err, gateErr)
	}
	view, err := access.View(t.Context(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if view.Revision != gateErr.CurrentRevision || view.RunState != session.RunIdle || len(view.Queue) != 0 || len(view.RecentHistory) != 0 {
		t.Fatalf("rejected input mutated session: %#v", view)
	}
	snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ExtensionJournal) != 1 || snapshot.ExtensionJournal[0].Status != hook.InvocationSucceeded ||
		snapshot.ExtensionJournal[0].Result == nil || snapshot.ExtensionJournal[0].Result.Decision != hook.DecisionReject ||
		snapshot.ExtensionJournal[0].EffectDisposition != hook.EffectApplied {
		t.Fatalf("rejection journal = %#v", snapshot.ExtensionJournal)
	}
	if observed := gate.lastView(); observed.Revision != opened.Revision.Next() {
		t.Fatalf("InputGate view revision = %d, want prepared revision %d", observed.Revision, opened.Revision.Next())
	}
}

func TestInputGateContextIsSeparateFromTheUserMessageAndConsumedOnce(t *testing.T) {
	gate := &recordingInputGate{descriptor: inputGateDescriptor("context")}
	gate.evaluate = func(view hook.InputGateView) hook.InputGateResult {
		message := agent.Message{
			ID: agent.MessageID("gate-context-" + view.MessageID), SessionID: view.SessionID, Role: agent.RoleUser,
			Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "[extension context] use the project formatter"}},
		}
		return hook.InputGateResult{Decision: hook.DecisionAccept, Context: []model.Input{{Message: &message}}}
	}
	executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{complete("done")}})
	access, store, stop := startRound7Application(t, executor, AgentRuntimeConfig{}, inputGateModule{gates: []hook.InputGate{gate}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	receipt, err := access.Send(t.Context(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("original user text"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := access.WhenIdle(t.Context(), interaction.WhenIdleRequest{SessionID: opened.SessionID}); err != nil {
		t.Fatal(err)
	}
	requests := executor.Requests()
	if len(requests) != 1 || len(requests[0].Inputs) != 2 || requests[0].Inputs[0].Message == nil || requests[0].Inputs[1].Message == nil ||
		requests[0].Inputs[0].Message.ID != receipt.MessageID || requests[0].Inputs[0].Message.Parts[0].Text != "original user text" ||
		!strings.Contains(requests[0].Inputs[1].Message.Parts[0].Text, "extension context") {
		t.Fatalf("model request did not preserve input/context order: %#v", requests)
	}
	snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ExtensionJournal) != 1 || snapshot.ExtensionJournal[0].EffectDisposition != hook.EffectApplied ||
		snapshot.ExtensionJournal[0].ContextDisposition != hook.ContextConsumed || len(snapshot.ExtensionJournal[0].ContextInputs) != 0 {
		t.Fatalf("consumed journal = %#v", snapshot.ExtensionJournal)
	}
	contributions := contributionFacts(snapshot.History)
	if len(contributions) != 1 || contributions[0].RunID == "" || contributions[0].StepID == "" || len(contributions[0].Inputs) != 1 {
		t.Fatalf("context contribution facts = %#v", contributions)
	}
	messages := historyMessageFacts(snapshot.History)
	if len(messages) != 2 || messages[0].ID != receipt.MessageID || messages[0].Parts[0].Text != "original user text" {
		t.Fatalf("context was presented as a durable user message: %#v", messages)
	}
}

func TestInputGateRejectsContextThatCollidesWithTheProposedMessageIdentity(t *testing.T) {
	gate := &recordingInputGate{descriptor: inputGateDescriptor("context-identity")}
	gate.evaluate = func(view hook.InputGateView) hook.InputGateResult {
		colliding := agent.Message{
			ID: view.MessageID, SessionID: view.SessionID, Role: agent.RoleUser,
			Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "must not reuse proposed identity"}},
		}
		return hook.InputGateResult{Decision: hook.DecisionAccept, Context: []model.Input{{Message: &colliding}}}
	}
	access, store, stop := startRound7Application(t, model.NewFakeModelExecutor(), AgentRuntimeConfig{}, inputGateModule{gates: []hook.InputGate{gate}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	_, err := access.Send(t.Context(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("proposed input"),
	})
	var gateErr *interaction.InputGateError
	if !errors.As(err, &gateErr) || agent.CodeOf(err) != agent.CodeExtensionFailed {
		t.Fatalf("colliding context error = %v / %#v", err, gateErr)
	}
	snapshot, loadErr := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(snapshot.History) != 0 || len(snapshot.Queue) != 0 || len(snapshot.ExtensionJournal) != 1 ||
		snapshot.ExtensionJournal[0].Status != hook.InvocationFailed {
		t.Fatalf("colliding context mutated Session input: %#v", snapshot)
	}
}

func TestQueuedInputGateContextIsConsumedByItsClaimedStepOnly(t *testing.T) {
	gate := &recordingInputGate{descriptor: inputGateDescriptor("queued-context")}
	gate.evaluate = func(view hook.InputGateView) hook.InputGateResult {
		message := agent.Message{
			ID: agent.MessageID("context-" + view.MessageID), SessionID: view.SessionID, Role: agent.RoleUser,
			Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "context for " + view.Input.Parts[0].Text}},
		}
		return hook.InputGateResult{Decision: hook.DecisionAccept, Context: []model.Input{{Message: &message}}}
	}
	firstRelease := make(chan struct{})
	executor := model.NewFakeModelExecutor(
		model.FakeExecution{Block: firstRelease, Events: []model.ModelEvent{complete("first done")}},
		model.FakeExecution{Events: []model.ModelEvent{complete("second done")}},
	)
	access, store, stop := startRound7Application(t, executor, AgentRuntimeConfig{}, inputGateModule{gates: []hook.InputGate{gate}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("first")}); err != nil {
		t.Fatal(err)
	}
	if err := executor.WaitForRequests(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	view, err := access.View(t.Context(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := access.Send(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: view.Revision, Input: textInput("second")})
	if err != nil {
		t.Fatal(err)
	}
	beforeClaim, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if entry := inputJournalForMessage(t, beforeClaim, queued.MessageID); entry.EffectDisposition != hook.EffectApplied || entry.ContextDisposition != hook.ContextPending {
		t.Fatalf("queued context finalized too early: %#v", entry)
	}
	close(firstRelease)
	if err := access.WhenIdle(t.Context(), interaction.WhenIdleRequest{SessionID: opened.SessionID}); err != nil {
		t.Fatal(err)
	}
	requests := executor.Requests()
	if len(requests) != 2 {
		t.Fatalf("model requests = %d", len(requests))
	}
	firstContext, secondContext := 0, 0
	for _, input := range requests[1].Inputs {
		if input.Message == nil || len(input.Message.Parts) == 0 {
			continue
		}
		if strings.Contains(input.Message.Parts[0].Text, "context for first") {
			firstContext++
		}
		if strings.Contains(input.Message.Parts[0].Text, "context for second") {
			secondContext++
		}
	}
	if firstContext != 0 || secondContext != 1 {
		t.Fatalf("second Step context counts = first:%d second:%d, request=%#v", firstContext, secondContext, requests[1])
	}
	afterClaim, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if entry := inputJournalForMessage(t, afterClaim, queued.MessageID); entry.ContextDisposition != hook.ContextConsumed {
		t.Fatalf("queued context was not consumed: %#v", entry)
	}
}

func TestDeleteCanWinWhileEditInputGateRunsAndDiscardsBothContexts(t *testing.T) {
	editEntered := make(chan struct{})
	editRelease := make(chan struct{})
	gate := &recordingInputGate{descriptor: inputGateDescriptor("edit-race")}
	gate.evaluate = func(view hook.InputGateView) hook.InputGateResult {
		if view.Operation == hook.InputEditQueued {
			close(editEntered)
			<-editRelease
		}
		message := agent.Message{
			ID: agent.MessageID("context-" + view.InvocationID), SessionID: view.SessionID, Role: agent.RoleUser,
			Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "context for " + string(view.Operation)}},
		}
		return hook.InputGateResult{Decision: hook.DecisionAccept, Context: []model.Input{{Message: &message}}}
	}
	modelRelease := make(chan struct{})
	executor := model.NewFakeModelExecutor(model.FakeExecution{Block: modelRelease, Events: []model.ModelEvent{complete("done")}})
	access, store, stop := startRound7Application(t, executor, AgentRuntimeConfig{}, inputGateModule{gates: []hook.InputGate{gate}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("active")}); err != nil {
		t.Fatal(err)
	}
	if err := executor.WaitForRequests(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	view, _ := access.View(t.Context(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	queued, err := access.Send(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: view.Revision, Input: textInput("queued")})
	if err != nil {
		t.Fatal(err)
	}

	view, _ = access.View(t.Context(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	editDone := make(chan error, 1)
	go func(revision agent.Revision) {
		_, err := access.EditQueued(context.Background(), interaction.EditQueuedRequest{
			SessionID: opened.SessionID, MessageID: queued.MessageID, ExpectedRevision: revision, Input: textInput("edited"),
		})
		editDone <- err
	}(view.Revision)
	select {
	case <-editEntered:
	case <-time.After(time.Second):
		t.Fatal("edit InputGate did not start")
	}
	view, _ = access.View(t.Context(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if _, err := access.DeleteQueued(t.Context(), interaction.DeleteQueuedRequest{
		SessionID: opened.SessionID, MessageID: queued.MessageID, ExpectedRevision: view.Revision,
	}); err != nil {
		t.Fatal(err)
	}
	close(editRelease)
	var editErr error
	select {
	case editErr = <-editDone:
	case <-time.After(time.Second):
		t.Fatal("edit did not finish after Delete")
	}
	var gateErr *interaction.InputGateError
	if !errors.As(editErr, &gateErr) {
		t.Fatalf("EditQueued race error = %v", editErr)
	}
	snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := queueByID(snapshot.Queue, queued.MessageID); ok {
		t.Fatal("deleted queued input was restored by stale edit")
	}
	for _, entry := range snapshot.ExtensionJournal {
		if entry.MessageID == queued.MessageID && entry.ContextDisposition == hook.ContextPending {
			t.Fatalf("stale context survived Delete/Edit race: %#v", entry)
		}
	}
	close(modelRelease)
}

func TestApplicationRejectsDuplicateInputGateDescriptorKeys(t *testing.T) {
	first := &recordingInputGate{descriptor: inputGateDescriptor("duplicate"), result: hook.InputGateResult{Decision: hook.DecisionAccept}}
	second := &recordingInputGate{descriptor: first.descriptor, result: hook.InputGateResult{Decision: hook.DecisionAccept}}
	entry := &captureChannel{}
	application := NewApplication(ApplicationSpec{
		Name: "duplicate-input-gate", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			session.NewMemoryModule(), executorModule{executor: model.NewFakeModelExecutor()},
			inputGateModule{gates: []hook.InputGate{first, second}},
			NewGatewayChannelModule("entrypoint.duplicate-input-gate", "test", entry),
		},
	})
	if _, err := application.Build(); err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("duplicate InputGate build error = %v", err)
	}
}

func TestApplicationRejectsTypedNilInputGateAndFreezesDescriptor(t *testing.T) {
	var typedNil *recordingInputGate
	entry := &captureChannel{}
	application := NewApplication(ApplicationSpec{
		Name: "nil-input-gate", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			session.NewMemoryModule(), executorModule{executor: model.NewFakeModelExecutor()},
			inputGateModule{gates: []hook.InputGate{typedNil}},
			NewGatewayChannelModule("entrypoint.nil-input-gate", "test", entry),
		},
	})
	if _, err := application.Build(); err == nil || !strings.Contains(err.Error(), "nil") {
		t.Fatalf("typed nil InputGate build error = %v", err)
	}

	original := inputGateDescriptor("frozen")
	gate := &recordingInputGate{descriptor: original, result: hook.InputGateResult{Decision: hook.DecisionReject, Reason: "frozen descriptor"}}
	access, store, stop := startRound7Application(t, model.NewFakeModelExecutor(), AgentRuntimeConfig{}, inputGateModule{gates: []hook.InputGate{gate}})
	defer stop()
	gate.descriptor = inputGateDescriptor("mutated-after-build")
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(t.Context(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("descriptor must be frozen"),
	}); err == nil {
		t.Fatal("rejecting InputGate unexpectedly accepted input")
	}
	snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ExtensionJournal) != 1 || snapshot.ExtensionJournal[0].Descriptor != original {
		t.Fatalf("Runtime did not use build-time descriptor: %#v", snapshot.ExtensionJournal)
	}
}

func TestDeleteDiscardsOnlyContextWhoseInputEffectWasAlreadyApplied(t *testing.T) {
	messageID := agent.MessageID("delete-race-message")
	pendingEffect := session.ExtensionJournalEntry{
		Boundary: hook.BoundaryInputGate, MessageID: messageID,
		EffectDisposition: hook.EffectPending, ContextDisposition: hook.ContextPending,
	}
	appliedEffect := pendingEffect
	appliedEffect.EffectDisposition = hook.EffectApplied
	snapshot := session.Snapshot{ExtensionJournal: []session.ExtensionJournalEntry{pendingEffect, appliedEffect}}
	changes := discardPendingInputContexts(snapshot, messageID)
	if len(changes) != 1 || changes[0].Extension == nil || changes[0].Extension.EffectDisposition != hook.EffectApplied ||
		changes[0].Extension.ContextDisposition != hook.ContextDiscarded {
		t.Fatalf("Delete context finalization = %#v", changes)
	}
}

func TestResumeDiscardsPreparedInputGateWithoutReplayingTheComponent(t *testing.T) {
	store := session.NewMemoryStore()
	created, err := store.Create(t.Context(), session.NewSession{
		Session:     agent.Session{ID: "prepared-input-session", AgentID: "agent", WorkspaceID: "workspace"},
		ModelConfig: testDefaultModel(), RunState: session.RunIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := hook.FingerprintTypedInput(struct {
		Operation string `json:"operation"`
		MessageID string `json:"message_id"`
	}{Operation: "send", MessageID: "prepared-message"})
	if err != nil {
		t.Fatal(err)
	}
	prepared := session.ExtensionJournalEntry{
		InvocationID: "prepared-invocation", Sequence: 1, Descriptor: inputGateDescriptor("recovery"),
		Boundary: hook.BoundaryInputGate, SessionID: created.Session.ID, MessageID: "prepared-message",
		InputDigest: fingerprint.Digest, PreparedAt: time.Now().UTC(), Status: hook.InvocationPrepared,
		EffectDisposition: hook.EffectNone, ContextDisposition: hook.ContextNone,
	}
	if _, err := store.Commit(t.Context(), session.CommitRequest{
		SessionID: created.Session.ID, ExpectedRevision: created.Revision, IdempotencyKey: "prepare-input",
		Changes: []session.Change{{Kind: session.UpdateExtensionJournal, Extension: &prepared}},
	}); err != nil {
		t.Fatal(err)
	}
	gate := &recordingInputGate{descriptor: prepared.Descriptor, result: hook.InputGateResult{Decision: hook.DecisionAccept}}
	entry := &captureChannel{}
	running, err := NewApplication(ApplicationSpec{
		Name: "prepared-input-recovery", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			sessionPairModule{store: store}, executorModule{executor: model.NewFakeModelExecutor()},
			inputGateModule{gates: []hook.InputGate{gate}},
			NewGatewayChannelModule("entrypoint.prepared-input-recovery", "test", entry),
		},
	}).Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = running.Stop(context.Background()) }()
	if _, err := entry.Access().ResumeSession(t.Context(), interaction.ResumeSessionRequest{SessionID: created.Session.ID}); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.Load(t.Context(), session.SessionRef{SessionID: created.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	journal := recovered.ExtensionJournal[0]
	if journal.Status != hook.InvocationFailed || journal.EffectDisposition != hook.EffectDiscarded || journal.ErrorCode != "extension_input_unavailable" {
		t.Fatalf("recovered prepared InputGate = %#v", journal)
	}
	if gate.viewCount() != 0 {
		t.Fatal("resume replayed a prepared InputGate without its original input")
	}
}

func TestResumeRetainsAppliedQueuedContextUntilRunPendingClaimsIt(t *testing.T) {
	store := session.NewMemoryStore()
	created, err := store.Create(t.Context(), session.NewSession{
		Session:     agent.Session{ID: "queued-context-recovery", AgentID: "agent", WorkspaceID: "workspace"},
		ModelConfig: testDefaultModel(), RunState: session.RunIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	message := agent.Message{
		ID: "queued-recovery-message", SessionID: created.Session.ID, Role: agent.RoleUser,
		Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "queued after restart"}},
	}
	contextMessage := agent.Message{
		ID: "queued-recovery-context", SessionID: created.Session.ID, Role: agent.RoleUser,
		Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "recovered gate context"}},
	}
	contextInputs := []model.Input{{Message: &contextMessage}}
	contextFingerprint, err := hook.FingerprintTypedInput(contextInputs)
	if err != nil {
		t.Fatal(err)
	}
	inputFingerprint, err := hook.FingerprintTypedInput(agent.MessageInput{Parts: message.Parts})
	if err != nil {
		t.Fatal(err)
	}
	prepared := session.ExtensionJournalEntry{
		InvocationID: "queued-recovery-invocation", Sequence: 1, Descriptor: inputGateDescriptor("queued-recovery"),
		Boundary: hook.BoundaryInputGate, SessionID: created.Session.ID, MessageID: message.ID,
		InputDigest: inputFingerprint.Digest, PreparedAt: time.Now().UTC(), Status: hook.InvocationPrepared,
		EffectDisposition: hook.EffectNone, ContextDisposition: hook.ContextNone,
	}
	commitInputJournalChange(t, store, created.Session.ID, created.Revision, "prepare", prepared, nil)
	pending := prepared
	pending.Status, pending.PendingAt = hook.InvocationPending, prepared.PreparedAt.Add(time.Millisecond)
	commitInputJournalChange(t, store, created.Session.ID, created.Revision.Next(), "pending", pending, nil)
	succeeded := pending
	succeeded.Status, succeeded.FinishedAt = hook.InvocationSucceeded, pending.PendingAt.Add(time.Millisecond)
	succeeded.Result = &hook.InvocationResult{Decision: hook.DecisionAccept}
	succeeded.EffectDisposition, succeeded.ContextDisposition = hook.EffectPending, hook.ContextPending
	succeeded.ContextInputs = contextInputs
	succeeded.ContextDigest, succeeded.ContextBytes = contextFingerprint.Digest, contextFingerprint.Bytes
	commitInputJournalChange(t, store, created.Session.ID, created.Revision.Next().Next(), "succeeded", succeeded, nil)
	applied := succeeded
	applied.EffectDisposition = hook.EffectApplied
	item := session.QueueItem{Message: message, Delivery: session.DeliveryNormal}
	commitInputJournalChange(t, store, created.Session.ID, created.Revision.Next().Next().Next(), "enqueue", applied,
		[]session.Change{{Kind: session.EnqueueMessage, QueueItem: &item}})

	gate := &recordingInputGate{descriptor: prepared.Descriptor, result: hook.InputGateResult{Decision: hook.DecisionAccept}}
	executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{complete("done")}})
	entry := &captureChannel{}
	running, err := NewApplication(ApplicationSpec{
		Name: "queued-context-recovery", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			sessionPairModule{store: store}, executorModule{executor: executor}, inputGateModule{gates: []hook.InputGate{gate}},
			NewGatewayChannelModule("entrypoint.queued-context-recovery", "test", entry),
		},
	}).Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = running.Stop(context.Background()) }()
	opened, err := entry.Access().ResumeSession(t.Context(), interaction.ResumeSessionRequest{SessionID: created.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if gate.viewCount() != 0 {
		t.Fatal("resume replayed an already applied queued InputGate")
	}
	if _, err := entry.Access().RunPending(t.Context(), interaction.RunPendingRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision,
	}); err != nil {
		t.Fatal(err)
	}
	if err := entry.Access().WhenIdle(t.Context(), interaction.WhenIdleRequest{SessionID: opened.SessionID}); err != nil {
		t.Fatal(err)
	}
	requests := executor.Requests()
	if len(requests) != 1 || len(requests[0].Inputs) != 2 || requests[0].Inputs[0].Message.ID != message.ID ||
		requests[0].Inputs[1].Message == nil || requests[0].Inputs[1].Message.Parts[0].Text != "recovered gate context" {
		t.Fatalf("recovered queued request = %#v", requests)
	}
	final, err := store.Load(t.Context(), session.SessionRef{SessionID: created.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if final.ExtensionJournal[0].ContextDisposition != hook.ContextConsumed {
		t.Fatalf("recovered context disposition = %#v", final.ExtensionJournal[0])
	}
}

func TestResumeDiscardsUnappliedEditContextEvenWhenTheOriginalQueueItemExists(t *testing.T) {
	store := session.NewMemoryStore()
	created, err := store.Create(t.Context(), session.NewSession{
		Session:     agent.Session{ID: "unapplied-edit-recovery", AgentID: "agent", WorkspaceID: "workspace"},
		ModelConfig: testDefaultModel(), RunState: session.RunIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	original := agent.Message{ID: "unapplied-edit-message", SessionID: created.Session.ID, Role: agent.RoleUser,
		Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "original queued content"}}}
	item := session.QueueItem{Message: original, Delivery: session.DeliveryNormal}
	queued, err := store.Commit(t.Context(), session.CommitRequest{
		SessionID: created.Session.ID, ExpectedRevision: created.Revision, IdempotencyKey: "seed-edit-subject",
		Changes: []session.Change{{Kind: session.EnqueueMessage, QueueItem: &item}},
	})
	if err != nil {
		t.Fatal(err)
	}
	contextMessage := agent.Message{ID: "unapplied-edit-context", SessionID: created.Session.ID, Role: agent.RoleUser,
		Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "context for proposed edit"}}}
	contextInputs := []model.Input{{Message: &contextMessage}}
	contextFingerprint, err := hook.FingerprintTypedInput(contextInputs)
	if err != nil {
		t.Fatal(err)
	}
	inputFingerprint, err := hook.FingerprintTypedInput(agent.MessageInput{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "proposed edit"}}})
	if err != nil {
		t.Fatal(err)
	}
	prepared := session.ExtensionJournalEntry{
		InvocationID: "unapplied-edit-invocation", Sequence: 1, Descriptor: inputGateDescriptor("unapplied-edit"),
		Boundary: hook.BoundaryInputGate, SessionID: created.Session.ID, MessageID: original.ID,
		InputDigest: inputFingerprint.Digest, PreparedAt: time.Now().UTC(), Status: hook.InvocationPrepared,
		EffectDisposition: hook.EffectNone, ContextDisposition: hook.ContextNone,
	}
	commitInputJournalChange(t, store, created.Session.ID, queued.Revision, "unapplied-edit-prepare", prepared, nil)
	pending := prepared
	pending.Status, pending.PendingAt = hook.InvocationPending, prepared.PreparedAt.Add(time.Millisecond)
	commitInputJournalChange(t, store, created.Session.ID, queued.Revision.Next(), "unapplied-edit-pending", pending, nil)
	succeeded := pending
	succeeded.Status, succeeded.FinishedAt = hook.InvocationSucceeded, pending.PendingAt.Add(time.Millisecond)
	succeeded.Result = &hook.InvocationResult{Decision: hook.DecisionAccept}
	succeeded.EffectDisposition, succeeded.ContextDisposition = hook.EffectPending, hook.ContextPending
	succeeded.ContextInputs, succeeded.ContextDigest, succeeded.ContextBytes = contextInputs, contextFingerprint.Digest, contextFingerprint.Bytes
	commitInputJournalChange(t, store, created.Session.ID, queued.Revision.Next().Next(), "unapplied-edit-succeeded", succeeded, nil)

	gate := &recordingInputGate{descriptor: prepared.Descriptor, result: hook.InputGateResult{Decision: hook.DecisionAccept}}
	entry := &captureChannel{}
	running, err := NewApplication(ApplicationSpec{
		Name: "unapplied-edit-recovery", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			sessionPairModule{store: store}, executorModule{executor: model.NewFakeModelExecutor()}, inputGateModule{gates: []hook.InputGate{gate}},
			NewGatewayChannelModule("entrypoint.unapplied-edit-recovery", "test", entry),
		},
	}).Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = running.Stop(context.Background()) }()
	if _, err := entry.Access().ResumeSession(t.Context(), interaction.ResumeSessionRequest{SessionID: created.Session.ID}); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.Load(t.Context(), session.SessionRef{SessionID: created.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	queuedItem, ok := queueByID(recovered.Queue, original.ID)
	if !ok || queuedItem.Message.Parts[0].Text != "original queued content" {
		t.Fatalf("recovery changed the original QueueItem: %#v", recovered.Queue)
	}
	journal := recovered.ExtensionJournal[0]
	if journal.EffectDisposition != hook.EffectDiscarded || journal.ContextDisposition != hook.ContextDiscarded || len(journal.ContextInputs) != 0 {
		t.Fatalf("unapplied edit recovery = %#v", journal)
	}
	if gate.viewCount() != 0 {
		t.Fatal("resume replayed a terminal unapplied edit")
	}
}

func commitInputJournalChange(t *testing.T, store *session.MemoryStore, sessionID agent.SessionID, revision agent.Revision, key string, entry session.ExtensionJournalEntry, prefix []session.Change) {
	t.Helper()
	entryCopy := entry
	changes := append([]session.Change(nil), prefix...)
	changes = append(changes, session.Change{Kind: session.UpdateExtensionJournal, Extension: &entryCopy})
	if _, err := store.Commit(t.Context(), session.CommitRequest{
		SessionID: sessionID, ExpectedRevision: revision, IdempotencyKey: "input-journal-" + key, Changes: changes,
	}); err != nil {
		t.Fatal(err)
	}
}

func inputJournalForMessage(t *testing.T, snapshot session.Snapshot, messageID agent.MessageID) session.ExtensionJournalEntry {
	t.Helper()
	for _, entry := range snapshot.ExtensionJournal {
		if entry.Boundary == hook.BoundaryInputGate && entry.MessageID == messageID {
			return entry
		}
	}
	t.Fatalf("input journal for %q was not found", messageID)
	return session.ExtensionJournalEntry{}
}

type inputGateModule struct{ gates []hook.InputGate }

func (inputGateModule) ID() string { return "test.input-gates" }

func (m inputGateModule) Register(reg agentslot.Registrar) error {
	contributions := make([]agentslot.Contribution, 0, len(m.gates))
	for _, gate := range m.gates {
		contributions = append(contributions, agentslot.Append(hook.InputGateSlot, gate))
	}
	return reg.Contribute(contributions...)
}

type recordingInputGate struct {
	mu         sync.Mutex
	descriptor hook.ExtensionDescriptor
	result     hook.InputGateResult
	evaluate   func(hook.InputGateView) hook.InputGateResult
	views      []hook.InputGateView
}

func (g *recordingInputGate) Descriptor() hook.ExtensionDescriptor { return g.descriptor }

func (g *recordingInputGate) Evaluate(_ context.Context, view hook.InputGateView) (hook.InputGateResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.views = append(g.views, view)
	if g.evaluate != nil {
		return g.evaluate(view), nil
	}
	return g.result, nil
}

func (g *recordingInputGate) viewCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.views)
}

func (g *recordingInputGate) lastView() hook.InputGateView {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.views) == 0 {
		return hook.InputGateView{}
	}
	return cloneInputGateView(g.views[len(g.views)-1])
}

func inputGateDescriptor(key string) hook.ExtensionDescriptor {
	return hook.ExtensionDescriptor{Key: "test." + key, DefinitionDigest: "sha256:" + strings.Repeat("a", 64)}
}
