package standardagent

import (
	"context"
	"errors"
	"sync"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestAttemptObserversSynchronouslyReceiveEveryPhysicalAttempt(t *testing.T) {
	executor := newRound7Executor(nil, nil, model.FakeExecution{Events: []model.ModelEvent{complete("done")}})
	observer := &attemptObserverProbe{}
	access, _, stop := startRound7Application(t, executor, AgentRuntimeConfig{}, attemptObserverModule{observer: observer})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("start"),
	}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	started, finished := observer.snapshot()
	if len(started) != 1 || len(finished) != 1 {
		t.Fatalf("attempt observer events = %d/%d, want 1/1", len(started), len(finished))
	}
	if started[0].Identity != finished[0].Identity || started[0].Identity.ConfigRevision == 0 {
		t.Fatalf("attempt identities = %#v / %#v", started[0].Identity, finished[0].Identity)
	}
}

func TestAttemptObserverFailureStopsBeforeExecutorCanContinue(t *testing.T) {
	executor := newRound7Executor(nil, nil, model.FakeExecution{Events: []model.ModelEvent{complete("must not complete")}})
	observer := &attemptObserverProbe{startErr: errors.New("quota denied")}
	access, _, stop := startRound7Application(t, executor, AgentRuntimeConfig{}, attemptObserverModule{observer: observer})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("start"),
	}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	if requests := executor.fake.Requests(); len(requests) != 1 {
		t.Fatalf("logical requests = %d, want 1", len(requests))
	}
	started, finished := observer.snapshot()
	if len(started) != 1 || len(finished) != 0 {
		t.Fatalf("attempt observer events = %d/%d, want 1/0", len(started), len(finished))
	}
}

func TestAttemptObserverStartFailureCompensatesEarlierObservers(t *testing.T) {
	executor := newRound7Executor(nil, nil, model.FakeExecution{Events: []model.ModelEvent{complete("must not complete")}})
	accepted := &attemptObserverProbe{}
	rejected := &attemptObserverProbe{startErr: errors.New("quota denied")}
	access, _, stop := startRound7Application(t, executor, AgentRuntimeConfig{},
		attemptObserverModule{id: "model.attempt-observer.accepted", observer: accepted},
		attemptObserverModule{id: "model.attempt-observer.rejected", observer: rejected},
	)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("start"),
	}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	started, finished := accepted.snapshot()
	if len(started) != 1 || len(finished) != 1 {
		t.Fatalf("accepted observer events = %d/%d, want 1/1", len(started), len(finished))
	}
	if finished[0].Outcome != model.AttemptCanceled || finished[0].ErrorCode != "attempt_start_rejected" {
		t.Fatalf("compensation = %#v", finished[0])
	}
	_, rejectedFinished := rejected.snapshot()
	if len(rejectedFinished) != 0 {
		t.Fatalf("rejecting observer received compensation: %#v", rejectedFinished)
	}
}

func TestAttemptObserverFinishFailureStillPreservesThePhysicalOutcome(t *testing.T) {
	executor := newRound7Executor(nil, nil, model.FakeExecution{Events: []model.ModelEvent{complete("provider result")}})
	observer := &attemptObserverProbe{finishErr: errors.New("ledger unavailable")}
	access, store, stop := startRound7Application(t, executor, AgentRuntimeConfig{}, attemptObserverModule{observer: observer})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("start"),
	}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	snapshot, err := store.Load(context.Background(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	facts := attemptFacts(snapshot.History)
	if len(facts) != 2 || facts[0].Kind != session.AttemptStarted || facts[1].Kind != session.AttemptSucceeded {
		t.Fatalf("durable attempt facts = %#v", facts)
	}
}

type attemptObserverModule struct {
	id       string
	observer model.AttemptObserver
}

func (m attemptObserverModule) ID() string {
	if m.id != "" {
		return m.id
	}
	return "model.attempt-observer.test"
}
func (m attemptObserverModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Append(model.AttemptObserverSlot, m.observer))
}

type attemptObserverProbe struct {
	mu        sync.Mutex
	startErr  error
	finishErr error
	started   []model.AttemptStarted
	finished  []model.AttemptFinished
}

func (p *attemptObserverProbe) AttemptStarted(_ context.Context, event model.AttemptStarted) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started = append(p.started, event)
	return p.startErr
}

func (p *attemptObserverProbe) AttemptFinished(_ context.Context, event model.AttemptFinished) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.finished = append(p.finished, event)
	return p.finishErr
}

func (p *attemptObserverProbe) snapshot() ([]model.AttemptStarted, []model.AttemptFinished) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]model.AttemptStarted(nil), p.started...), append([]model.AttemptFinished(nil), p.finished...)
}
