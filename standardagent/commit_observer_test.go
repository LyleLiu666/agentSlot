package standardagent

import (
	"context"
	"errors"
	"testing"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestSessionCommitObserverPreservesRevisionOrderAndIsolatesFailures(t *testing.T) {
	recorded := make(chan session.CommitNotice, 2)
	observer := newSessionCommitObserver([]session.SessionCommitObserver{
		session.CommitObserverFunc(func(context.Context, session.CommitNotice) error { panic("broken observer") }),
		session.CommitObserverFunc(func(context.Context, session.CommitNotice) error { return errors.New("ignored observer error") }),
		session.CommitObserverFunc(func(_ context.Context, notice session.CommitNotice) error {
			recorded <- notice
			return nil
		}),
	})
	defer observer.stop()
	observer.publish(session.CommitNotice{SessionID: "session-1", Revision: 2, FirstHistorySequence: 1, LastHistorySequence: 2})
	observer.publish(session.CommitNotice{SessionID: "session-1", Revision: 3})
	first := receiveCommitNotice(t, recorded)
	second := receiveCommitNotice(t, recorded)
	if first.Revision != 2 || second.Revision != 3 {
		t.Fatalf("observer revisions = %d, %d", first.Revision, second.Revision)
	}
}

func TestDifferentSessionCommitObserversCanRunConcurrently(t *testing.T) {
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondObserved := make(chan struct{})
	shared := session.CommitObserverFunc(func(_ context.Context, notice session.CommitNotice) error {
		switch notice.SessionID {
		case "session-1":
			close(firstEntered)
			<-releaseFirst
		case "session-2":
			close(secondObserved)
		}
		return nil
	})
	first := newSessionCommitObserver([]session.SessionCommitObserver{shared})
	second := newSessionCommitObserver([]session.SessionCommitObserver{shared})
	defer first.stop()
	defer second.stop()
	first.publish(session.CommitNotice{SessionID: "session-1", Revision: 2})
	waitSignal(t, firstEntered, "first Session observer")
	second.publish(session.CommitNotice{SessionID: "session-2", Revision: 2})
	waitSignal(t, secondObserved, "second Session observer while first was blocked")
	close(releaseFirst)
}

func TestRuntimeCommitObservationIsAsynchronousAndCarriesFactRange(t *testing.T) {
	entered := make(chan session.CommitNotice, 1)
	release := make(chan struct{})
	observer := session.CommitObserverFunc(func(_ context.Context, notice session.CommitNotice) error {
		select {
		case entered <- notice:
		default:
		}
		<-release
		return nil
	})
	executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{complete("done")}})
	access, stop := startRuntimeTestApplication(t, executor, commitObserverModule{observers: []session.SessionCommitObserver{observer}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	sent := make(chan error, 1)
	go func() {
		_, err := access.Send(context.Background(), interaction.SendRequest{
			SessionID: opened.SessionID, ExpectedRevision: opened.Revision,
			Actor: agent.ActorIdentity{Kind: agent.ActorLocalUser, ID: "user-1"}, Input: textInput("hello"),
		})
		sent <- err
	}()
	notice := receiveCommitNotice(t, entered)
	if notice.Revision <= opened.Revision || notice.FirstHistorySequence == 0 || notice.LastHistorySequence < notice.FirstHistorySequence {
		t.Fatalf("first Runtime commit notice = %#v", notice)
	}
	select {
	case err := <-sent:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Send waited for asynchronous SessionCommitObserver")
	}
	close(release)
	waitRuntimeIdle(t, access, opened.SessionID)
}

type commitObserverModule struct {
	observers []session.SessionCommitObserver
}

func (commitObserverModule) ID() string { return "test.session-commit-observers" }

func (m commitObserverModule) Register(reg agentslot.Registrar) error {
	contributions := make([]agentslot.Contribution, 0, len(m.observers))
	for _, observer := range m.observers {
		contributions = append(contributions, agentslot.Append(session.CommitObserverSlot, observer))
	}
	return reg.Contribute(contributions...)
}

func receiveCommitNotice(t *testing.T, notices <-chan session.CommitNotice) session.CommitNotice {
	t.Helper()
	select {
	case notice := <-notices:
		return notice
	case <-time.After(time.Second):
		t.Fatal("Session commit notice was not observed")
		return session.CommitNotice{}
	}
}

func waitSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}
