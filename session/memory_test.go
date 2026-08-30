package session_test

import (
	"context"
	"sync"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
	"github.com/LyleLiu666/agentSlot/tool"
)

func TestMemoryModuleExplicitlyContributesOnlySessionStore(t *testing.T) {
	module := session.NewMemoryModule()
	builder := agentslot.NewBuilder()
	if err := builder.Install(module); err != nil {
		t.Fatalf("install memory module: %v", err)
	}
	assembly, err := builder.Build(agentslot.RequireOne(session.StoreSlot))
	if err != nil {
		t.Fatalf("build memory slots: %v", err)
	}
	if _, ok := agentslot.Get(assembly, session.StoreSlot); !ok {
		t.Fatal("memory session.store missing")
	}
}

func TestMemoryStoreMaintainsOrderedAppendOnlyHistoryAndToolJournal(t *testing.T) {
	store, snapshot := newStoredSession(t, "history-session")
	message := message("assistant-1", snapshot.Session.ID, agent.RoleAssistant, "calling tool")
	message.RunID, message.StepID = "run-1", "step-1"
	commit := commitChanges(t, store, snapshot.Session.ID, snapshot.Revision, "message", session.Change{
		Kind: session.AppendMessage, Message: &message,
	})
	call := agent.ToolCall{
		ID: "call-1", MessageID: message.ID, SessionID: snapshot.Session.ID,
		RunID: "run-1", StepID: "step-1", Name: "lookup", Arguments: []byte(`{"q":"x"}`),
	}
	pending := session.JournalEntry{
		RunID: call.RunID, StepID: call.StepID, ToolCall: &call, Status: session.JournalPending,
	}
	commit = commitChanges(t, store, snapshot.Session.ID, commit.Revision, "call",
		session.Change{Kind: session.SetRunState, RunState: &session.RunStateChange{RunID: call.RunID, State: session.RunRunning}},
		session.Change{Kind: session.AppendToolCall, ToolCall: &call},
		session.Change{Kind: session.UpdateRunJournal, Journal: &pending},
	)
	_, err := store.Commit(context.Background(), session.CommitRequest{
		SessionID: snapshot.Session.ID, ExpectedRevision: commit.Revision, IdempotencyKey: "finish-too-early",
		Changes: []session.Change{{Kind: session.SetRunState, RunState: &session.RunStateChange{RunID: call.RunID, State: session.RunIdle}}},
	})
	if !agent.IsCode(err, agent.CodeJournalInvariant) {
		t.Fatalf("finish with pending tool error = %v, want journal_invariant", err)
	}
	result := tool.ToolResult{CallID: call.ID, Status: tool.ResultSucceeded, Output: []byte(`{"ok":true}`)}
	terminal := pending
	terminal.Status = session.JournalSucceeded
	terminal.ToolResult = &result
	commit = commitChanges(t, store, snapshot.Session.ID, commit.Revision, "result",
		session.Change{Kind: session.AppendToolResult, ToolResult: &result},
		session.Change{Kind: session.UpdateRunJournal, Journal: &terminal},
		session.Change{Kind: session.SetRunState, RunState: &session.RunStateChange{RunID: call.RunID, State: session.RunIdle}},
	)

	view := load(t, store, snapshot.Session.ID)
	if len(view.History) != 3 || view.History[0].Message == nil || view.History[1].ToolCall == nil || view.History[2].ToolResult == nil {
		t.Fatalf("history order = %#v, want message/call/result", view.History)
	}
	if len(view.RunJournal) != 1 || view.RunJournal[0].Status != session.JournalSucceeded {
		t.Fatalf("journal = %#v, want one succeeded entry", view.RunJournal)
	}

	orphan := agent.ToolCall{
		ID: "call-orphan", MessageID: message.ID, SessionID: snapshot.Session.ID,
		RunID: "run-1", StepID: "step-2", Name: "lookup", Arguments: []byte(`{}`),
	}
	_, err = store.Commit(context.Background(), session.CommitRequest{
		SessionID: snapshot.Session.ID, ExpectedRevision: commit.Revision, IdempotencyKey: "orphan",
		Changes: []session.Change{{Kind: session.AppendToolCall, ToolCall: &orphan}},
	})
	if !agent.IsCode(err, agent.CodeJournalInvariant) {
		t.Fatalf("tool call without pending journal error = %v, want journal_invariant", err)
	}
	if got := load(t, store, snapshot.Session.ID); got.Revision != commit.Revision || len(got.History) != 3 {
		t.Fatalf("failed transaction changed aggregate: revision=%d history=%d", got.Revision, len(got.History))
	}
}

func TestMemoryStoreClassifiesDuplicateMissingAndCanceledOperations(t *testing.T) {
	store, snapshot := newStoredSession(t, "classified-session")
	_, err := store.Create(context.Background(), session.NewSession{
		Session:     agent.Session{ID: snapshot.Session.ID, AgentID: snapshot.Session.AgentID, WorkspaceID: snapshot.Session.WorkspaceID},
		ModelConfig: defaultConfig(), RunState: session.RunIdle,
	})
	if !agent.IsCode(err, agent.CodeSessionAlreadyExists) {
		t.Fatalf("duplicate create error = %v, want session_already_exists", err)
	}
	if _, err = store.Load(context.Background(), session.SessionRef{SessionID: "missing-session"}); !agent.IsCode(err, agent.CodeSessionNotFound) {
		t.Fatalf("missing load error = %v, want session_not_found", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = store.Load(canceled, session.SessionRef{SessionID: snapshot.Session.ID}); !agent.IsKind(err, agent.ErrorCanceled) {
		t.Fatalf("canceled load error = %v, want canceled", err)
	}
}

func TestMemoryStoreQueueCASAndClaimRules(t *testing.T) {
	store, snapshot := newStoredSession(t, "queue-session")
	queued := message("queued-1", snapshot.Session.ID, agent.RoleUser, "first")
	request := session.CommitRequest{
		SessionID: snapshot.Session.ID, ExpectedRevision: snapshot.Revision, IdempotencyKey: "enqueue",
		Changes: []session.Change{{Kind: session.EnqueueMessage, QueueItem: &session.QueueItem{Message: queued, Delivery: session.DeliveryNormal}}},
	}
	first, err := store.Commit(context.Background(), request)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	retry, err := store.Commit(context.Background(), request)
	if err != nil || retry.Applied || retry.Revision != first.Revision {
		t.Fatalf("idempotent retry = %#v, %v", retry, err)
	}
	changedRequest := request
	changedRequest.Changes = []session.Change{{Kind: session.DeleteQueue, QueueDelete: &session.QueueDelete{MessageID: queued.ID}}}
	if _, err = store.Commit(context.Background(), changedRequest); !agent.IsCode(err, agent.CodeRevisionConflict) {
		t.Fatalf("reused idempotency key error = %v, want revision_conflict", err)
	}

	edited := message(queued.ID, snapshot.Session.ID, agent.RoleUser, "edited")
	second := commitChanges(t, store, snapshot.Session.ID, first.Revision, "edit", session.Change{
		Kind: session.EditQueue, QueueEdit: &session.QueueEdit{MessageID: edited.ID, Input: agent.MessageInput{Parts: edited.Parts}, Delivery: session.DeliverySteer},
	})
	third := commitChanges(t, store, snapshot.Session.ID, second.Revision, "move", session.Change{
		Kind: session.ReclassifyQueue, QueueReclassification: &session.QueueReclassify{MessageID: queued.ID, Delivery: session.DeliveryHeld},
	})
	fourth := commitChanges(t, store, snapshot.Session.ID, third.Revision, "claim",
		session.Change{Kind: session.SetRunState, RunState: &session.RunStateChange{RunID: "run-1", State: session.RunRunning}},
		session.Change{Kind: session.ClaimQueue, QueueClaim: &session.QueueClaim{MessageID: edited.ID, RunID: "run-1"}},
	)

	_, err = store.Commit(context.Background(), session.CommitRequest{
		SessionID: snapshot.Session.ID, ExpectedRevision: fourth.Revision, IdempotencyKey: "delete-claimed",
		Changes: []session.Change{{Kind: session.DeleteQueue, QueueDelete: &session.QueueDelete{MessageID: queued.ID}}},
	})
	if !agent.IsCode(err, agent.CodeQueueItemClaimed) {
		t.Fatalf("delete claimed error = %v, want queue_item_claimed", err)
	}
	_, err = store.Commit(context.Background(), session.CommitRequest{
		SessionID: snapshot.Session.ID, ExpectedRevision: snapshot.Revision, IdempotencyKey: "stale",
		Changes: []session.Change{{Kind: session.DeleteQueue, QueueDelete: &session.QueueDelete{MessageID: queued.ID}}},
	})
	if !agent.IsCode(err, agent.CodeRevisionConflict) {
		t.Fatalf("stale CAS error = %v, want revision_conflict", err)
	}
	view := load(t, store, snapshot.Session.ID)
	if len(view.Queue) != 1 || view.Queue[0].Message.Parts[0].Text != "edited" || view.Queue[0].Delivery != session.DeliveryHeld || !view.Queue[0].Claimed() {
		t.Fatalf("queue = %#v, want edited held claimed item", view.Queue)
	}
	_, err = store.Commit(context.Background(), session.CommitRequest{
		SessionID: snapshot.Session.ID, ExpectedRevision: view.Revision, IdempotencyKey: "finish-before-consume",
		Changes: []session.Change{{Kind: session.SetRunState, RunState: &session.RunStateChange{RunID: "run-1", State: session.RunIdle}}},
	})
	if !agent.IsCode(err, agent.CodeQueueItemClaimed) {
		t.Fatalf("finish with claimed queue error = %v, want queue_item_claimed", err)
	}
	consumed := commitChanges(t, store, snapshot.Session.ID, view.Revision, "consume",
		session.Change{Kind: session.ConsumeQueue, QueueConsume: &session.QueueConsume{MessageID: queued.ID, RunID: "run-1"}},
		session.Change{Kind: session.SetRunState, RunState: &session.RunStateChange{RunID: "run-1", State: session.RunIdle}},
	)
	if queue := load(t, store, snapshot.Session.ID).Queue; len(queue) != 0 {
		t.Fatalf("queue after consume at revision %d = %#v", consumed.Revision, queue)
	}
}

func TestMemoryStoreQueueDeleteBeforeClaim(t *testing.T) {
	store, snapshot := newStoredSession(t, "delete-session")
	queued := message("queued-1", snapshot.Session.ID, agent.RoleUser, "delete me")
	first := commitChanges(t, store, snapshot.Session.ID, snapshot.Revision, "enqueue", session.Change{
		Kind: session.EnqueueMessage, QueueItem: &session.QueueItem{Message: queued, Delivery: session.DeliveryNormal},
	})
	commitChanges(t, store, snapshot.Session.ID, first.Revision, "delete", session.Change{
		Kind: session.DeleteQueue, QueueDelete: &session.QueueDelete{MessageID: queued.ID},
	})
	if queue := load(t, store, snapshot.Session.ID).Queue; len(queue) != 0 {
		t.Fatalf("queue after delete = %#v", queue)
	}
}

func TestMemoryStorePersistsSingleActiveRunUnderConcurrency(t *testing.T) {
	store, snapshot := newStoredSession(t, "run-session")
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, runID := range []agent.RunID{"run-a", "run-b"} {
		wait.Add(1)
		go func(runID agent.RunID) {
			defer wait.Done()
			<-start
			_, err := store.Commit(context.Background(), session.CommitRequest{
				SessionID: snapshot.Session.ID, ExpectedRevision: snapshot.Revision,
				IdempotencyKey: "start-" + string(runID),
				Changes:        []session.Change{{Kind: session.SetRunState, RunState: &session.RunStateChange{RunID: runID, State: session.RunRunning}}},
			})
			results <- err
		}(runID)
	}
	close(start)
	wait.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !agent.IsCode(err, agent.CodeRevisionConflict) {
			t.Fatalf("losing concurrent start error = %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent starts = %d, want 1", successes)
	}
	view := load(t, store, snapshot.Session.ID)
	if view.RunState != session.RunRunning || !view.ActiveRunID.Valid() {
		t.Fatalf("run state = %q/%q", view.RunState, view.ActiveRunID)
	}
	_, err := store.Commit(context.Background(), session.CommitRequest{
		SessionID: snapshot.Session.ID, ExpectedRevision: view.Revision, IdempotencyKey: "second-active",
		Changes: []session.Change{{Kind: session.SetRunState, RunState: &session.RunStateChange{RunID: "run-c", State: session.RunRunning}}},
	})
	if !agent.IsCode(err, agent.CodeActiveRun) {
		t.Fatalf("second active run error = %v, want active_run", err)
	}
	config := model.Config{ProviderKey: "provider-2", ModelID: "model-2", Reasoning: model.ReasoningDefault}
	configEvent := session.SessionEvent{Kind: session.EventModelConfigChanged, ModelConfigChanged: &session.ModelConfigChange{Previous: view.ModelConfig, Current: config}}
	_, err = store.Commit(context.Background(), session.CommitRequest{
		SessionID: snapshot.Session.ID, ExpectedRevision: view.Revision, IdempotencyKey: "config-while-running",
		Changes: []session.Change{
			{Kind: session.SetModelConfig, ModelConfig: &config},
			{Kind: session.AppendSessionEvent, SessionEvent: &configEvent},
		},
	})
	if !agent.IsCode(err, agent.CodeActiveRun) {
		t.Fatalf("model update during run error = %v, want active_run", err)
	}
}

func TestMemoryStoreRecoveryPairsUnknownOutcomeAndEndsRunOnce(t *testing.T) {
	store := session.NewMemoryStore()
	assistant := message("assistant-1", "recover-session", agent.RoleAssistant, "calling")
	assistant.RunID, assistant.StepID = "run-1", "step-1"
	call := agent.ToolCall{
		ID: "call-1", MessageID: assistant.ID, SessionID: assistant.SessionID,
		RunID: assistant.RunID, StepID: assistant.StepID, Name: "side-effect", Arguments: []byte(`{}`),
	}
	pending := session.JournalEntry{RunID: call.RunID, StepID: call.StepID, ToolCall: &call, Status: session.JournalPending}
	steer := message("steer-1", assistant.SessionID, agent.RoleUser, "interrupt")
	unclaimedSteer := message("steer-2", assistant.SessionID, agent.RoleUser, "queued interrupt")
	created, err := store.Create(context.Background(), session.NewSession{
		Session: agent.Session{ID: assistant.SessionID, AgentID: "agent-1", WorkspaceID: "workspace-1"},
		History: []session.HistoryFact{{Message: &assistant}, {ToolCall: &call}}, RunJournal: []session.JournalEntry{pending},
		Queue: []session.QueueItem{
			{Message: steer, Delivery: session.DeliverySteer, ClaimedBy: call.RunID},
			{Message: unclaimedSteer, Delivery: session.DeliverySteer},
		},
		ModelConfig: defaultConfig(), RunState: session.RunRunning, ActiveRunID: call.RunID,
	})
	if err != nil {
		t.Fatalf("create recoverable aggregate: %v", err)
	}
	ordinary := load(t, store, created.Session.ID)
	if ordinary.RunState != session.RunRunning || len(ordinary.History) != 2 {
		t.Fatalf("ordinary Load performed recovery: %#v", ordinary)
	}
	manager, err := session.NewManager(store, defaultConfig())
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	resumed, err := manager.Resume(context.Background(), session.ResumeRequest{SessionID: created.Session.ID})
	if err != nil {
		t.Fatalf("resume with recovery: %v", err)
	}
	recovered := view(t, resumed)
	if recovered.Revision != created.Revision.Next() || recovered.RunState != session.RunIdle || recovered.ActiveRunID != "" {
		t.Fatalf("recovered state = revision %d, %q/%q", recovered.Revision, recovered.RunState, recovered.ActiveRunID)
	}
	if len(recovered.History) != 3 || recovered.History[2].ToolResult == nil || recovered.History[2].ToolResult.Status != tool.ResultUnknown {
		t.Fatalf("recovered history = %#v", recovered.History)
	}
	if recovered.RunJournal[0].Status != session.JournalOutcomeUnknown {
		t.Fatalf("recovered journal = %#v", recovered.RunJournal)
	}
	if len(recovered.Queue) != 2 || recovered.Queue[0].Claimed() || recovered.Queue[0].Delivery != session.DeliveryHeld || recovered.Queue[1].Claimed() || recovered.Queue[1].Delivery != session.DeliveryHeld {
		t.Fatalf("recovered steer queue = %#v, want unclaimed held", recovered.Queue)
	}
	again, err := store.Recover(context.Background(), session.SessionRef{SessionID: created.Session.ID})
	if err != nil || again.Revision != recovered.Revision || len(again.History) != len(recovered.History) {
		t.Fatalf("second recovery = revision %d history %d err %v", again.Revision, len(again.History), err)
	}
}

func TestMemoryStoreRecoveryPreservesPreparedToolCallForSafeResume(t *testing.T) {
	store := session.NewMemoryStore()
	assistant := message("assistant-prepared", "prepared-session", agent.RoleAssistant, "calling")
	assistant.RunID, assistant.StepID = "run-prepared", "step-prepared"
	call := agent.ToolCall{
		ID: "call-prepared", MessageID: assistant.ID, SessionID: assistant.SessionID,
		RunID: assistant.RunID, StepID: assistant.StepID, Name: "effect", Arguments: []byte(`{}`),
	}
	started := session.RunFact{
		SessionID: assistant.SessionID, RunID: call.RunID, Kind: session.RunStarted,
		ModelConfig: defaultConfig(), ConfigRevision: 1,
	}
	prepared := session.JournalEntry{RunID: call.RunID, StepID: call.StepID, ToolCall: &call, Status: session.JournalPrepared}
	created, err := store.Create(context.Background(), session.NewSession{
		Session:    agent.Session{ID: assistant.SessionID, AgentID: "agent-1", WorkspaceID: "workspace-1"},
		History:    []session.HistoryFact{{Run: &started}, {Message: &assistant}, {ToolCall: &call}},
		RunJournal: []session.JournalEntry{prepared}, ModelConfig: defaultConfig(),
		RunState: session.RunRunning, ActiveRunID: call.RunID,
	})
	if err != nil {
		t.Fatalf("create prepared aggregate: %v", err)
	}
	recovered, err := store.Recover(context.Background(), session.SessionRef{SessionID: created.Session.ID})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered.Revision != created.Revision || recovered.RunState != session.RunRunning || recovered.ActiveRunID != call.RunID {
		t.Fatalf("recovered state = revision %d, %q/%q", recovered.Revision, recovered.RunState, recovered.ActiveRunID)
	}
	if len(recovered.History) != 3 || recovered.RunJournal[0].Status != session.JournalPrepared {
		t.Fatalf("prepared call was rewritten during recovery: %#v %#v", recovered.History, recovered.RunJournal)
	}
}

func TestMemoryStoreRecoveryAppendsInterruptedRunFactWithFrozenConfig(t *testing.T) {
	store, snapshot := newStoredSession(t, "interrupted-run-session")
	run := session.RunFact{
		SessionID: snapshot.Session.ID, RunID: "run-1", Kind: session.RunStarted,
		ModelConfig: defaultConfig(), ConfigRevision: snapshot.Revision,
	}
	started := commitChanges(t, store, snapshot.Session.ID, snapshot.Revision, "start-run-fact",
		session.Change{Kind: session.SetRunState, RunState: &session.RunStateChange{RunID: run.RunID, State: session.RunRunning}},
		session.Change{Kind: session.AppendRunFact, RunFact: &run},
	)
	mismatched := run
	mismatched.Kind = session.RunCompleted
	mismatched.ModelConfig.ModelID = "different-model"
	_, err := store.Commit(context.Background(), session.CommitRequest{
		SessionID: snapshot.Session.ID, ExpectedRevision: started.Revision, IdempotencyKey: "mismatched-run-terminal",
		Changes: []session.Change{
			{Kind: session.AppendRunFact, RunFact: &mismatched},
			{Kind: session.SetRunState, RunState: &session.RunStateChange{RunID: run.RunID, State: session.RunIdle}},
		},
	})
	if !agent.IsCode(err, agent.CodeHistoryInvariant) {
		t.Fatalf("mismatched terminal error = %v, code=%q", err, agent.CodeOf(err))
	}
	recovered, err := store.Recover(context.Background(), session.SessionRef{SessionID: snapshot.Session.ID})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if recovered.Revision != started.Revision.Next() || recovered.RunState != session.RunIdle {
		t.Fatalf("recovered state = %#v", recovered)
	}
	runs := make([]session.RunFact, 0, 2)
	for _, fact := range recovered.History {
		if fact.Run != nil {
			runs = append(runs, *fact.Run)
		}
	}
	if len(runs) != 2 || runs[0].Kind != session.RunStarted || runs[1].Kind != session.RunInterrupted || runs[1].ConfigRevision != run.ConfigRevision || runs[1].ModelConfig.ModelID != run.ModelConfig.ModelID {
		t.Fatalf("recovered run facts = %#v", runs)
	}
}

func TestFixedManagerCreateResumeForkAndSummaryPreserveModelRules(t *testing.T) {
	store := session.NewMemoryStore()
	manager, err := session.NewManager(store, defaultConfig())
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	created, err := manager.Create(context.Background(), session.CreateRequest{AgentID: "agent-1", WorkspaceID: "workspace-1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	source := view(t, created)
	if created.ID() != source.Session.ID || created.Revision() != source.Revision {
		t.Fatalf("session handle identity/revision = %q/%d, snapshot = %q/%d", created.ID(), created.Revision(), source.Session.ID, source.Revision)
	}
	explicitCreateConfig := model.Config{ProviderKey: "provider-create", ModelID: "model-create", Reasoning: model.ReasoningMedium}
	explicitlyConfigured, err := manager.Create(context.Background(), session.CreateRequest{
		AgentID: "agent-1", WorkspaceID: "workspace-explicit", ModelConfig: &explicitCreateConfig,
	})
	if err != nil {
		t.Fatalf("create with model override: %v", err)
	}
	if got := view(t, explicitlyConfigured).ModelConfig.ModelID; got != explicitCreateConfig.ModelID {
		t.Fatalf("create model override = %q, want %q", got, explicitCreateConfig.ModelID)
	}
	selected := model.Config{ProviderKey: "provider-2", ModelID: "model-2", Reasoning: model.ReasoningHigh}
	modelEvent := session.SessionEvent{Kind: session.EventModelConfigChanged, ModelConfigChanged: &session.ModelConfigChange{Previous: source.ModelConfig, Current: selected}}
	user := message("source-message", source.Session.ID, agent.RoleUser, "source history")
	queue := message("queued-source", source.Session.ID, agent.RoleUser, "not inherited")
	updated := commitChanges(t, store, source.Session.ID, source.Revision, "source-state",
		session.Change{Kind: session.AppendMessage, Message: &user},
		session.Change{Kind: session.EnqueueMessage, QueueItem: &session.QueueItem{Message: queue, Delivery: session.DeliveryNormal}},
		session.Change{Kind: session.SetModelConfig, ModelConfig: &selected},
		session.Change{Kind: session.AppendSessionEvent, SessionEvent: &modelEvent},
	)
	resumed, err := manager.Resume(context.Background(), session.ResumeRequest{SessionID: source.Session.ID})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	resumedView := view(t, resumed)
	if resumedView.Revision != updated.Revision || resumedView.ModelConfig.ModelID != selected.ModelID {
		t.Fatalf("resumed view = revision %d config %#v", resumedView.Revision, resumedView.ModelConfig)
	}

	forked, err := manager.Fork(context.Background(), session.ForkRequest{
		SourceSessionID: source.Session.ID, Mode: session.ForkFullHistory, AgentID: "agent-1", WorkspaceID: "workspace-2",
	})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	forkView := view(t, forked)
	if forkView.Session.ParentSessionID != source.Session.ID || forkView.Session.ParentRevision != updated.Revision || forkView.ModelConfig.ModelID != selected.ModelID {
		t.Fatalf("fork provenance/config = %#v / %#v", forkView.Session, forkView.ModelConfig)
	}
	if len(forkView.History) != 2 || forkView.History[0].Message == nil || forkView.History[0].Message.ID == user.ID || forkView.History[0].Message.SessionID != forkView.Session.ID || forkView.History[1].ModelConfigChanged == nil {
		t.Fatalf("fork history identities = %#v", forkView.History)
	}
	if forkView.Context.Request.SessionID != "" || len(forkView.RetainedContexts) != 0 || forkView.Context.Version != 0 || forkView.Context.SourceRevision != 0 {
		t.Fatalf("fork reused source model projection: %#v", forkView.Context)
	}
	if len(forkView.Queue) != 0 || len(forkView.RunJournal) != 0 {
		t.Fatalf("fork copied source execution state: queue=%#v journal=%#v", forkView.Queue, forkView.RunJournal)
	}
	override := model.Config{ProviderKey: "provider-3", ModelID: "model-3", Reasoning: model.ReasoningLow}
	overriddenFork, err := manager.Fork(context.Background(), session.ForkRequest{
		SourceSessionID: source.Session.ID, Mode: session.ForkFullHistory, AgentID: "agent-1", WorkspaceID: "workspace-4",
		ModelConfig: &override,
	})
	if err != nil {
		t.Fatalf("fork with model override: %v", err)
	}
	if got := view(t, overriddenFork).ModelConfig.ModelID; got != override.ModelID {
		t.Fatalf("fork model override = %q, want %q", got, override.ModelID)
	}

	summary, err := manager.StartFromSummary(context.Background(), session.SummaryRequest{
		SourceSessionID: source.Session.ID, AgentID: "agent-1", WorkspaceID: "workspace-3",
		Messages: []agent.MessageInput{{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "summary"}}}},
	})
	if err != nil {
		t.Fatalf("summary start: %v", err)
	}
	summaryView := view(t, summary)
	if summaryView.Session.ParentSessionID != source.Session.ID || summaryView.Session.ParentRevision != updated.Revision || summaryView.ModelConfig.ModelID != selected.ModelID {
		t.Fatalf("summary provenance/config = %#v / %#v", summaryView.Session, summaryView.ModelConfig)
	}
	if len(summaryView.History) != 1 || summaryView.History[0].Message.Role != agent.RoleUser || summaryView.History[0].Message.Parts[0].Text != "summary" {
		t.Fatalf("summary history = %#v", summaryView.History)
	}
}

func TestFixedManagerCompleteForkRewritesToolFactIdentity(t *testing.T) {
	store := session.NewMemoryStore()
	manager, err := session.NewManager(store, defaultConfig())
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	assistant := message("assistant-source", "source-with-tool", agent.RoleAssistant, "calling")
	assistant.RunID, assistant.StepID = "run-source", "step-source"
	call := agent.ToolCall{
		ID: "call-source", MessageID: assistant.ID, SessionID: assistant.SessionID,
		RunID: assistant.RunID, StepID: assistant.StepID, Name: "lookup", Arguments: []byte(`{}`),
	}
	result := tool.ToolResult{CallID: call.ID, Status: tool.ResultSucceeded, Output: []byte(`{"value":1}`)}
	_, err = store.Create(context.Background(), session.NewSession{
		Session:     agent.Session{ID: assistant.SessionID, AgentID: "agent-1", WorkspaceID: "workspace-1"},
		History:     []session.HistoryFact{{Message: &assistant}, {ToolCall: &call}, {ToolResult: &result}},
		ModelConfig: defaultConfig(), RunState: session.RunIdle,
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	forked, err := manager.Fork(context.Background(), session.ForkRequest{
		SourceSessionID: assistant.SessionID, Mode: session.ForkFullHistory, AgentID: "agent-1", WorkspaceID: "workspace-2",
	})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	history := view(t, forked).History
	if len(history) != 3 || history[0].Message == nil || history[1].ToolCall == nil || history[2].ToolResult == nil {
		t.Fatalf("forked tool history = %#v", history)
	}
	if history[0].Message.ID == assistant.ID || history[1].ToolCall.ID == call.ID {
		t.Fatalf("fork retained source identity: %#v", history)
	}
	if history[1].ToolCall.MessageID != history[0].Message.ID || history[1].ToolCall.RunID != history[0].Message.RunID || history[1].ToolCall.StepID != history[0].Message.StepID || history[2].ToolResult.CallID != history[1].ToolCall.ID {
		t.Fatalf("fork broke tool fact references: %#v", history)
	}
}

func TestFixedManagerDistinguishesFullForkFromEmptyPrefix(t *testing.T) {
	store := session.NewMemoryStore()
	manager, err := session.NewManager(store, defaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	active := message("message-active", "fork-active-source", agent.RoleUser, "first request")
	active.RunID, active.StepID = "run-active", "step-active"
	source, err := store.Create(context.Background(), session.NewSession{
		Session:     agent.Session{ID: active.SessionID, AgentID: "agent-1", WorkspaceID: "workspace-1"},
		History:     []session.HistoryFact{{Message: &active}},
		ModelConfig: defaultConfig(),
		RunState:    session.RunRunning,
		ActiveRunID: active.RunID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Fork(context.Background(), session.ForkRequest{
		SourceSessionID: source.Session.ID,
		AgentID:         "agent-1",
		WorkspaceID:     "workspace-2",
	}); !agent.IsKind(err, agent.ErrorInvalidInput) {
		t.Fatalf("fork without mode error = %v, want invalid input", err)
	}
	if _, err := manager.Fork(context.Background(), session.ForkRequest{
		SourceSessionID: source.Session.ID,
		Mode:            session.ForkFullHistory,
		CutoffSequence:  1,
		AgentID:         "agent-1",
		WorkspaceID:     "workspace-2",
	}); !agent.IsKind(err, agent.ErrorInvalidInput) {
		t.Fatalf("full fork with cutoff error = %v, want invalid input", err)
	}
	if _, err := manager.Fork(context.Background(), session.ForkRequest{
		SourceSessionID: source.Session.ID,
		Mode:            session.ForkHistoryPrefix,
		CutoffSequence:  1,
		AgentID:         "agent-1",
		WorkspaceID:     "workspace-2",
	}); !agent.IsKind(err, agent.ErrorInvalidInput) {
		t.Fatalf("active Step prefix error = %v, want invalid input", err)
	}

	if _, err := manager.Fork(context.Background(), session.ForkRequest{
		SourceSessionID: source.Session.ID,
		Mode:            session.ForkFullHistory,
		AgentID:         "agent-1",
		WorkspaceID:     "workspace-2",
	}); !agent.IsKind(err, agent.ErrorConflict) || !agent.IsCode(err, agent.CodeActiveRun) {
		t.Fatalf("full fork while active error = %v, want active-run conflict", err)
	}

	forked, err := manager.Fork(context.Background(), session.ForkRequest{
		SourceSessionID: source.Session.ID,
		Mode:            session.ForkHistoryPrefix,
		CutoffSequence:  0,
		AgentID:         "agent-1",
		WorkspaceID:     "workspace-2",
	})
	if err != nil {
		t.Fatalf("empty-prefix fork while active: %v", err)
	}
	child := view(t, forked)
	if len(child.History) != 0 || child.RunState != session.RunIdle || child.ActiveRunID.Valid() {
		t.Fatalf("empty-prefix child = history %d state %q active %q", len(child.History), child.RunState, child.ActiveRunID)
	}
	if child.Fork == nil || child.Fork.ParentSessionID != source.Session.ID || child.Fork.CutoffSequence != 0 {
		t.Fatalf("empty-prefix provenance = %#v", child.Fork)
	}
	parent, err := store.Load(context.Background(), session.SessionRef{SessionID: source.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(parent.History) != 1 || parent.RunState != session.RunRunning || parent.ActiveRunID != active.RunID {
		t.Fatalf("parent changed by fork = history %d state %q active %q", len(parent.History), parent.RunState, parent.ActiveRunID)
	}
}

func TestFixedManagerForksStablePrefixWhileParentContinues(t *testing.T) {
	store := session.NewMemoryStore()
	manager, err := session.NewManager(store, defaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	completed := message("message-completed", "fork-running-source", agent.RoleAssistant, "completed")
	completed.RunID, completed.StepID = "run-active", "step-completed"
	active := message("message-active", completed.SessionID, agent.RoleUser, "running")
	active.RunID, active.StepID = completed.RunID, "step-active"
	source, err := store.Create(context.Background(), session.NewSession{
		Session:     agent.Session{ID: completed.SessionID, AgentID: "agent-1", WorkspaceID: "workspace-1"},
		History:     []session.HistoryFact{{Message: &completed}, {Message: &active}},
		ModelConfig: defaultConfig(),
		RunState:    session.RunRunning,
		ActiveRunID: active.RunID,
	})
	if err != nil {
		t.Fatal(err)
	}
	forked, err := manager.Fork(context.Background(), session.ForkRequest{
		SourceSessionID: source.Session.ID,
		Mode:            session.ForkHistoryPrefix,
		CutoffSequence:  1,
		AgentID:         "agent-1",
		WorkspaceID:     "workspace-2",
	})
	if err != nil {
		t.Fatalf("stable-prefix fork while active: %v", err)
	}
	continued := message("message-continued", source.Session.ID, agent.RoleAssistant, "continued")
	continued.RunID, continued.StepID = active.RunID, active.StepID
	if _, err := store.Commit(context.Background(), session.CommitRequest{
		SessionID:        source.Session.ID,
		ExpectedRevision: source.Revision,
		IdempotencyKey:   "parent-continues",
		Changes:          []session.Change{{Kind: session.AppendMessage, Message: &continued}},
	}); err != nil {
		t.Fatalf("continue parent after fork: %v", err)
	}
	child := view(t, forked)
	if len(child.History) != 1 || child.History[0].Message == nil || child.History[0].Message.Parts[0].Text != "completed" {
		t.Fatalf("child changed after parent continued: %#v", child.History)
	}
	parent, err := store.Load(context.Background(), session.SessionRef{SessionID: source.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(parent.History) != 3 {
		t.Fatalf("parent history length = %d, want 3", len(parent.History))
	}
}

func TestFixedManagerForksAtCompletedHistoryCheckpoint(t *testing.T) {
	store := session.NewMemoryStore()
	manager, err := session.NewManager(store, defaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	assistant := message("assistant-1", "checkpoint-source", agent.RoleAssistant, "calling")
	assistant.RunID, assistant.StepID = "run-1", "step-1"
	call := agent.ToolCall{
		ID: "call-1", MessageID: assistant.ID, SessionID: assistant.SessionID,
		RunID: assistant.RunID, StepID: assistant.StepID, Name: "lookup", Arguments: []byte(`{}`),
	}
	result := tool.ToolResult{CallID: call.ID, Status: tool.ResultSucceeded, Output: []byte(`{"ok":true}`)}
	later := message("message-2", assistant.SessionID, agent.RoleUser, "later")
	later.RunID, later.StepID = "run-2", "step-2"
	journal := session.JournalEntry{RunID: call.RunID, StepID: call.StepID, ToolCall: &call, ToolResult: &result, Status: session.JournalSucceeded}
	created, err := store.Create(context.Background(), session.NewSession{
		Session:    agent.Session{ID: assistant.SessionID, AgentID: "agent-1", WorkspaceID: "workspace-1"},
		History:    []session.HistoryFact{{Message: &assistant}, {ToolCall: &call}, {ToolResult: &result}, {Message: &later}},
		RunJournal: []session.JournalEntry{journal}, ModelConfig: defaultConfig(), RunState: session.RunIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Fork(context.Background(), session.ForkRequest{
		SourceSessionID: created.Session.ID, Mode: session.ForkHistoryPrefix, CutoffSequence: 2, AgentID: "agent-1", WorkspaceID: "workspace-2",
	}); err == nil {
		t.Fatal("fork accepted a cutoff inside an unpaired tool exchange")
	}
	forked, err := manager.Fork(context.Background(), session.ForkRequest{
		SourceSessionID: created.Session.ID, Mode: session.ForkHistoryPrefix, CutoffSequence: 3, AgentID: "agent-1", WorkspaceID: "workspace-2",
	})
	if err != nil {
		t.Fatalf("checkpoint fork: %v", err)
	}
	view := view(t, forked)
	if view.Fork == nil || view.Fork.ParentSessionID != created.Session.ID || view.Fork.CutoffSequence != 3 {
		t.Fatalf("fork provenance = %#v", view.Fork)
	}
	if len(view.History) != 3 || len(view.Queue) != 0 || len(view.RunJournal) != 0 || view.RunState != session.RunIdle {
		t.Fatalf("fork state = history %d queue %d journal %d state %q", len(view.History), len(view.Queue), len(view.RunJournal), view.RunState)
	}
	for index, fact := range view.History {
		if fact.OriginFactID != created.History[index].FactID || fact.FactID == fact.OriginFactID {
			t.Fatalf("fact %d lineage = fact %q origin %q source %q", index, fact.FactID, fact.OriginFactID, created.History[index].FactID)
		}
	}
}

func TestMemoryStoreReturnsDetachedSnapshots(t *testing.T) {
	store := session.NewMemoryStore()
	temperature := 0.5
	config := defaultConfig()
	config.Parameters.Temperature = &temperature
	message := message("message-1", "detached-session", agent.RoleUser, "original")
	created, err := store.Create(context.Background(), session.NewSession{
		Session: agent.Session{ID: message.SessionID, AgentID: "agent-1", WorkspaceID: "workspace-1"},
		History: []session.HistoryFact{{Message: &message}}, ModelConfig: config, RunState: session.RunIdle,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	created.History[0].Message.Parts[0].Text = "mutated"
	*created.ModelConfig.Parameters.Temperature = 9
	loaded := load(t, store, message.SessionID)
	if loaded.History[0].Message.Parts[0].Text != "original" || *loaded.ModelConfig.Parameters.Temperature != 0.5 {
		t.Fatalf("stored state was mutated through returned snapshot: %#v", loaded)
	}
}

func newStoredSession(t *testing.T, id agent.SessionID) (*session.MemoryStore, session.Snapshot) {
	t.Helper()
	store := session.NewMemoryStore()
	snapshot, err := store.Create(context.Background(), session.NewSession{
		Session:     agent.Session{ID: id, AgentID: "agent-1", WorkspaceID: "workspace-1"},
		ModelConfig: defaultConfig(), RunState: session.RunIdle,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return store, snapshot
}

func commitChanges(t *testing.T, store session.SessionStore, id agent.SessionID, revision agent.Revision, key string, changes ...session.Change) session.Commit {
	t.Helper()
	commit, err := store.Commit(context.Background(), session.CommitRequest{
		SessionID: id, ExpectedRevision: revision, IdempotencyKey: key, Changes: changes,
	})
	if err != nil {
		t.Fatalf("commit %q: %v", key, err)
	}
	return commit
}

func load(t *testing.T, store session.SessionStore, id agent.SessionID) session.Snapshot {
	t.Helper()
	snapshot, err := store.Load(context.Background(), session.SessionRef{SessionID: id})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return snapshot
}

func view(t *testing.T, value session.Session) session.Snapshot {
	t.Helper()
	snapshot, err := value.View(context.Background())
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	return snapshot
}

func defaultConfig() model.Config {
	return model.Config{ProviderKey: "provider-1", ModelID: "model-1", Reasoning: model.ReasoningDefault}
}

func message(id agent.MessageID, sessionID agent.SessionID, role agent.Role, text string) agent.Message {
	return agent.Message{
		ID: id, SessionID: sessionID, Role: role,
		Parts: []agent.MessagePart{{Kind: agent.PartText, Text: text}},
	}
}
