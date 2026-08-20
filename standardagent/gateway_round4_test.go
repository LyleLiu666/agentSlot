package standardagent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestTwoGatewayChannelsRaceFromOneRevisionAndOnlyOneWriteCommits(t *testing.T) {
	firstChannel := &captureChannel{}
	secondChannel := &captureChannel{}
	executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{complete("done")}})
	application := NewApplication(ApplicationSpec{
		Name: "two-channel-cas", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: session.NewMemoryStore(), executor: executor},
			NewGatewayChannelModule("channel.first", "first", firstChannel),
			NewGatewayChannelModule("channel.second", "second", secondChannel),
		},
	})
	running, err := application.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer running.Stop(context.Background())
	opened, err := firstChannel.Access().CreateSession(context.Background(), interaction.CreateSessionRequest{AgentID: "agent-1", WorkspaceID: "workspace-1"})
	if err != nil {
		t.Fatal(err)
	}

	actors := []agent.ActorIdentity{
		{Kind: agent.ActorLocalUser, ID: "local-a"},
		{Kind: agent.ActorRemoteUser, ID: "remote-b"},
	}
	accesses := []interaction.GatewayAccess{firstChannel.Access(), secondChannel.Access()}
	start := make(chan struct{})
	type result struct {
		index   int
		receipt interaction.EnqueueReceipt
		err     error
	}
	results := make(chan result, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for index := range accesses {
		go func(index int) {
			ready.Done()
			<-start
			receipt, err := accesses[index].Send(context.Background(), interaction.SendRequest{
				SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Actor: actors[index], Input: textInput(fmt.Sprintf("from-%d", index)),
			})
			results <- result{index: index, receipt: receipt, err: err}
		}(index)
	}
	ready.Wait()
	close(start)
	first := <-results
	second := <-results
	var winner result
	var conflict *interaction.RevisionConflictError
	for _, candidate := range []result{first, second} {
		if candidate.err == nil {
			if winner.receipt.MessageID.Valid() {
				t.Fatalf("both Channel writes succeeded: %#v / %#v", first, second)
			}
			winner = candidate
			continue
		}
		if !errors.As(candidate.err, &conflict) || !agent.IsCode(candidate.err, agent.CodeRevisionConflict) {
			t.Fatalf("losing Channel error = %T %v", candidate.err, candidate.err)
		}
	}
	if !winner.receipt.MessageID.Valid() || conflict == nil || !conflict.SnapshotRequired || conflict.CurrentRevision <= opened.Revision {
		t.Fatalf("winner/conflict = %#v / %#v", winner, conflict)
	}
	waitRuntimeIdle(t, firstChannel.Access(), opened.SessionID)
	view, err := secondChannel.Access().View(context.Background(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range view.RecentHistory {
		if fact.Message != nil && fact.Message.ID == winner.receipt.MessageID {
			if fact.Actor != actors[winner.index] {
				t.Fatalf("durable actor = %#v, want %#v", fact.Actor, actors[winner.index])
			}
			return
		}
	}
	t.Fatal("winning message was not durable")
}

func TestCancelAndCloseRejectStaleRevisionWithCurrentViewHint(t *testing.T) {
	block := make(chan struct{})
	executor := model.NewFakeModelExecutor(model.FakeExecution{Block: block, Events: []model.ModelEvent{complete("late")}})
	access, stop := startRuntimeTestApplication(t, executor)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	receipt, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("wait")})
	if err != nil {
		t.Fatal(err)
	}
	err = access.Cancel(context.Background(), interaction.CancelRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision})
	assertRevisionConflict(t, err, receipt.Revision)
	if err := access.Cancel(context.Background(), interaction.CancelRequest{SessionID: opened.SessionID, ExpectedRevision: receipt.Revision}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	view, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	err = access.CloseSession(context.Background(), interaction.CloseSessionRequest{SessionID: opened.SessionID, ExpectedRevision: view.Revision - 1})
	assertRevisionConflict(t, err, view.Revision)
	if _, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: opened.SessionID}); err != nil {
		t.Fatalf("stale Close removed Runtime: %v", err)
	}
	if err := access.CloseSession(context.Background(), interaction.CloseSessionRequest{SessionID: opened.SessionID, ExpectedRevision: view.Revision}); err != nil {
		t.Fatal(err)
	}
}

func TestSessionViewAndCursorHistoryCoverLogicalStepsWithoutDrift(t *testing.T) {
	const sessionID = agent.SessionID("gateway-history")
	store := session.NewMemoryStore()
	history := make([]session.HistoryFact, 0, 303)
	for index := 1; index <= 101; index++ {
		runID := agent.RunID(fmt.Sprintf("run-%03d", index))
		stepID := agent.StepID(fmt.Sprintf("step-%03d", index))
		started := session.RunFact{SessionID: sessionID, RunID: runID, Kind: session.RunStarted, ModelConfig: testDefaultModel(), ConfigRevision: 1}
		message := agent.Message{
			ID: agent.MessageID(fmt.Sprintf("message-%03d", index)), SessionID: sessionID, RunID: runID, StepID: stepID,
			Role: agent.RoleUser, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: string(stepID)}},
		}
		completed := started
		completed.Kind = session.RunCompleted
		history = append(history, session.HistoryFact{Run: &started}, session.HistoryFact{Message: &message}, session.HistoryFact{Run: &completed})
	}
	if _, err := store.Create(context.Background(), session.NewSession{
		Session: agent.Session{ID: sessionID, AgentID: "agent-1", WorkspaceID: "workspace-1"},
		History: history, ModelConfig: testDefaultModel(), RunState: session.RunIdle,
	}); err != nil {
		t.Fatal(err)
	}
	executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{complete("new")}})
	entry := &captureChannel{}
	application := NewApplication(ApplicationSpec{
		Name: "history-view", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: store, executor: executor},
			NewGatewayChannelModule("channel.history", "history", entry),
		},
	})
	running, err := application.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer running.Stop(context.Background())
	opened, err := entry.Access().ResumeSession(context.Background(), interaction.ResumeSessionRequest{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	view, err := entry.Access().View(context.Background(), interaction.SessionViewRequest{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if !view.HasMoreHistory || countStepIDs(view.RecentHistory) != 100 || len(view.RecentHistory) == 0 {
		t.Fatalf("recent View = steps %d facts %d more=%v", countStepIDs(view.RecentHistory), len(view.RecentHistory), view.HasMoreHistory)
	}
	if view.ModelConfig != testDefaultModel() {
		t.Fatalf("View model config = %#v", view.ModelConfig)
	}
	cursor := view.RecentHistory[0].Sequence
	if _, err := entry.Access().Send(context.Background(), interaction.SendRequest{SessionID: sessionID, ExpectedRevision: opened.Revision, Input: textInput("append concurrently")}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, entry.Access(), sessionID)
	older, err := entry.Access().History(context.Background(), interaction.HistoryRequest{SessionID: sessionID, BeforeHistorySequence: cursor, StepLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if countStepIDs(older.Facts) != 1 || older.HasMore {
		t.Fatalf("older page = steps %d facts %d more=%v", countStepIDs(older.Facts), len(older.Facts), older.HasMore)
	}
	seen := make(map[session.HistorySequence]bool)
	for _, fact := range append(append([]session.HistoryFact(nil), older.Facts...), view.RecentHistory...) {
		if seen[fact.Sequence] {
			t.Fatalf("HistorySequence %d appeared in both pages", fact.Sequence)
		}
		seen[fact.Sequence] = true
	}
}

func assertRevisionConflict(t *testing.T, err error, minimum agent.Revision) {
	t.Helper()
	var conflict *interaction.RevisionConflictError
	if !errors.As(err, &conflict) || !agent.IsCode(err, agent.CodeRevisionConflict) || !conflict.SnapshotRequired || conflict.CurrentRevision < minimum {
		t.Fatalf("revision conflict = %T %#v, minimum %d", err, err, minimum)
	}
}

func countStepIDs(history []session.HistoryFact) int {
	seen := make(map[agent.StepID]bool)
	for _, fact := range history {
		if fact.StepID.Valid() {
			seen[fact.StepID] = true
		}
	}
	return len(seen)
}
