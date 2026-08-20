package standardagent

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestGatewayStreamsTemporaryOutputAndDurableCommitsInOrder(t *testing.T) {
	completed := complete("done")
	completed.AttemptID = "attempt-2"
	executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{
		{Kind: model.EventDelta, AttemptID: "attempt-1", Text: "par"},
		{Kind: model.EventReset, AttemptID: "attempt-1"},
		{Kind: model.EventDelta, AttemptID: "attempt-2", Text: "done"},
		completed,
	}})
	access, stop := startRuntimeTestApplication(t, executor)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	stream, err := access.Subscribe(context.Background(), interaction.SubscribeRequest{
		SessionID: opened.SessionID, AfterRevision: opened.Revision,
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer stream.Close()
	if _, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("hello"),
	}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	view, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var chunks, attempts []string
	var resetAttempt string
	var reachedFinalRevision bool
	for !reachedFinalRevision {
		event, err := stream.Recv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		switch event.Kind {
		case interaction.EventChunk:
			chunks = append(chunks, event.Text)
			attempts = append(attempts, event.AttemptID)
		case interaction.EventReset:
			resetAttempt = event.AttemptID
		case interaction.EventRevision:
			if event.SessionID != opened.SessionID || event.RunID != "" || event.StepID != "" || event.AttemptID != "" || event.Text != "" {
				t.Fatalf("durable notification leaked payload: %#v", event)
			}
			reachedFinalRevision = event.Revision == view.Revision
		}
	}
	if resetAttempt != "attempt-1" || !reflect.DeepEqual(chunks, []string{"par", "done"}) || !reflect.DeepEqual(attempts, []string{"attempt-1", "attempt-2"}) {
		t.Fatalf("temporary events = chunks %#v attempts %#v reset %q", chunks, attempts, resetAttempt)
	}
}

func TestNonStreamingResultContainsOnlyAssistantTextMessages(t *testing.T) {
	runID := agent.RunID("run-1")
	inputID := agent.MessageID("message-input")
	textID := agent.MessageID("message-text")
	result, err := aggregateRunResult(interaction.SessionView{
		SessionID: "session-1", Revision: 9,
		RecentHistory: []session.HistoryFact{
			{Message: &agent.Message{ID: inputID, RunID: runID, Role: agent.RoleUser}},
			// A content-empty assistant message exists only to own ToolCalls.
			{Message: &agent.Message{ID: "message-tool-owner", RunID: runID, Role: agent.RoleAssistant}},
			{Message: &agent.Message{ID: textID, RunID: runID, Role: agent.RoleAssistant, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "answer"}}}},
			{Run: &session.RunFact{RunID: runID, Kind: session.RunCompleted}},
		},
	}, inputID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.AssistantMessages) != 1 || result.AssistantMessages[0].ID != textID {
		t.Fatalf("assistant text messages = %#v", result.AssistantMessages)
	}
}

func TestSlowEventSubscriberDropsTemporaryChunksBeforeDurableRevision(t *testing.T) {
	hub := newEventHub()
	stream, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for index := 0; index <= eventStreamBufferLimit; index++ {
		hub.publish(interaction.Event{Kind: interaction.EventChunk, Text: "x"})
	}
	hub.publish(interaction.Event{Kind: interaction.EventRevision, SessionID: "session-1", Revision: 7})
	for {
		event, err := stream.Recv(context.Background())
		if err != nil {
			t.Fatalf("temporary overflow closed stream before revision: %v", err)
		}
		if event.Kind == interaction.EventRevision {
			if event.Revision != 7 {
				t.Fatalf("revision event = %#v", event)
			}
			break
		}
	}
}

func TestSlowEventSubscriberClosesWhenDurableRevisionsCannotBeDelivered(t *testing.T) {
	hub := newEventHub()
	stream, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for index := 0; index <= eventStreamBufferLimit; index++ {
		hub.publish(interaction.Event{Kind: interaction.EventRevision, SessionID: "session-1", Revision: agent.Revision(index + 1)})
	}
	if _, err := stream.Recv(context.Background()); !errors.Is(err, interaction.ErrEventStreamOverflow) {
		t.Fatalf("durable overflow error = %v", err)
	}
}

func TestEventStreamReleasesDrainedBuffer(t *testing.T) {
	hub := newEventHub()
	contract, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	stream := contract.(*runtimeEventStream)
	defer stream.Close()
	hub.publish(interaction.Event{Kind: interaction.EventRevision, SessionID: "session-1", Revision: 2})
	if _, err := stream.Recv(context.Background()); err != nil {
		t.Fatal(err)
	}
	stream.mu.Lock()
	retained := stream.queue != nil
	stream.mu.Unlock()
	if retained {
		t.Fatal("drained event queue retained its backing message references")
	}
}

func TestGatewayReconnectRequiresViewBeforeSubscribingFromStaleRevision(t *testing.T) {
	executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{complete("done")}})
	access, stop := startRuntimeTestApplication(t, executor)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("hello")}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	if _, err := access.Subscribe(context.Background(), interaction.SubscribeRequest{SessionID: opened.SessionID, AfterRevision: opened.Revision}); err == nil {
		t.Fatal("stale subscriber was accepted without first obtaining a current SessionView")
	}
	snapshot, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := access.Subscribe(context.Background(), interaction.SubscribeRequest{SessionID: opened.SessionID, AfterRevision: snapshot.Revision})
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
}

func TestGatewayNonStreamingAggregationUsesTheSameDurableRun(t *testing.T) {
	executor := model.NewFakeModelExecutor(
		model.FakeExecution{Events: []model.ModelEvent{complete("first")}},
		model.FakeExecution{Events: []model.ModelEvent{complete("second")}},
	)
	h := &recordingHook{proposal: textInput("continue"), observed: make(chan struct{}, 8)}
	access, _, stop := startRound7Application(t, executor, AgentRuntimeConfig{}, hookModule{hook: h})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	result, err := access.SendAndWait(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("hello"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "completed" || !result.RunID.Valid() || len(result.AssistantMessages) != 2 || result.AssistantMessages[0].Parts[0].Text != "first" || result.AssistantMessages[1].Parts[0].Text != "second" {
		t.Fatalf("aggregate result = %#v", result)
	}
	snapshot, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	var durable int
	for _, fact := range snapshot.RecentHistory {
		if fact.Message != nil && fact.Message.Role == "assistant" && fact.Message.RunID == result.RunID {
			durable++
		}
	}
	if durable != len(result.AssistantMessages) {
		t.Fatalf("aggregate assistant count %d differs from History %d", len(result.AssistantMessages), durable)
	}
}

func TestDisconnectingEventStreamDoesNotCancelRun(t *testing.T) {
	blocked := make(chan struct{})
	executor := model.NewFakeModelExecutor(model.FakeExecution{
		Block:  blocked,
		Events: []model.ModelEvent{{Kind: model.EventDelta, Text: "working"}, complete("done")},
	})
	access, stop := startRuntimeTestApplication(t, executor)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	stream, err := access.Subscribe(context.Background(), interaction.SubscribeRequest{
		SessionID: opened.SessionID, AfterRevision: opened.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("hello"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	close(blocked)
	waitRuntimeIdle(t, access, opened.SessionID)
	snapshot, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	var completed bool
	for _, fact := range snapshot.RecentHistory {
		if fact.Run != nil && fact.Run.Kind == "completed" {
			completed = true
		}
	}
	if !completed {
		t.Fatalf("disconnect canceled or lost the Run: %#v", snapshot.RecentHistory)
	}
}

func TestRevisionNotificationAndNonStreamingResultResolveToTheSameDurableFact(t *testing.T) {
	executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{
		{Kind: model.EventDelta, Text: "temporary"}, complete("durable"),
	}})
	access, stop := startRuntimeTestApplication(t, executor)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	stream, err := access.Subscribe(context.Background(), interaction.SubscribeRequest{
		SessionID: opened.SessionID, AfterRevision: opened.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	result, err := access.SendAndWait(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("hello"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.AssistantMessages) != 1 {
		t.Fatalf("non-streaming messages = %#v", result.AssistantMessages)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var notified bool
	for !notified {
		event, err := stream.Recv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if event.Kind == interaction.EventRevision && event.Revision == result.Revision {
			if event.SessionID != opened.SessionID || event.RunID != "" || event.StepID != "" || event.AttemptID != "" || event.Text != "" {
				t.Fatalf("revision notification leaked fact payload: %#v", event)
			}
			notified = true
		}
	}
	snapshot, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range snapshot.RecentHistory {
		if fact.Message != nil && fact.Message.ID == result.AssistantMessages[0].ID {
			if !reflect.DeepEqual(*fact.Message, result.AssistantMessages[0]) {
				t.Fatalf("View fact = %#v, non-streaming fact = %#v", *fact.Message, result.AssistantMessages[0])
			}
			return
		}
	}
	t.Fatal("assistant fact was not recoverable through revision + View")
}

func TestGatewayEventStreamsAreIsolatedBySession(t *testing.T) {
	executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{complete("one")}})
	access, stop := startRuntimeTestApplication(t, executor)
	defer stop()
	first := createRuntimeTestSession(t, access)
	second := createRuntimeTestSession(t, access)
	firstStream, err := access.Subscribe(context.Background(), interaction.SubscribeRequest{SessionID: first.SessionID, AfterRevision: first.Revision})
	if err != nil {
		t.Fatal(err)
	}
	defer firstStream.Close()
	secondStream, err := access.Subscribe(context.Background(), interaction.SubscribeRequest{SessionID: second.SessionID, AfterRevision: second.Revision})
	if err != nil {
		t.Fatal(err)
	}
	defer secondStream.Close()
	if _, err := access.Send(context.Background(), interaction.SendRequest{SessionID: first.SessionID, ExpectedRevision: first.Revision, Input: textInput("hello")}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, first.SessionID)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := firstStream.Recv(ctx); err != nil {
		t.Fatalf("first Session received no events: %v", err)
	}
	short, shortCancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer shortCancel()
	if _, err := secondStream.Recv(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Session received another Session's event: %v", err)
	}
}

func TestClosingSessionWakesEventReceiver(t *testing.T) {
	access, stop := startRuntimeTestApplication(t, model.NewFakeModelExecutor())
	defer stop()
	opened := createRuntimeTestSession(t, access)
	stream, err := access.Subscribe(context.Background(), interaction.SubscribeRequest{SessionID: opened.SessionID, AfterRevision: opened.Revision})
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan error, 1)
	go func() {
		_, err := stream.Recv(context.Background())
		received <- err
	}()
	if err := access.CloseSession(context.Background(), interaction.CloseSessionRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-received:
		if !errors.Is(err, interaction.ErrEventStreamClosed) {
			t.Fatalf("Recv after close = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked Recv was not woken by Session close")
	}
}
