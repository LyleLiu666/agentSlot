package standardagent

import (
	"context"
	"sync"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/session"
)

// sessionCommitObserver serializes post-commit notices for one Session without
// executing product code while Runtime holds its state mutex. Each Runtime has
// its own worker, so different Sessions remain independent and may be observed
// concurrently. Errors and panics cannot roll back an already committed fact.
type sessionCommitObserver struct {
	observers []session.SessionCommitObserver
	ctx       context.Context
	cancel    context.CancelFunc

	mu      sync.Mutex
	changed *sync.Cond
	queue   []session.CommitNotice
	stopped bool
}

func newSessionCommitObserver(observers []session.SessionCommitObserver) *sessionCommitObserver {
	if len(observers) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	observer := &sessionCommitObserver{
		observers: append([]session.SessionCommitObserver(nil), observers...), ctx: ctx, cancel: cancel,
	}
	observer.changed = sync.NewCond(&observer.mu)
	go observer.run()
	return observer
}

func (o *sessionCommitObserver) publish(notice session.CommitNotice) {
	if o == nil {
		return
	}
	o.mu.Lock()
	if !o.stopped && notice.Validate() == nil {
		o.queue = append(o.queue, notice)
		o.changed.Signal()
	}
	o.mu.Unlock()
}

func (o *sessionCommitObserver) stop() {
	if o == nil {
		return
	}
	o.mu.Lock()
	if !o.stopped {
		o.stopped = true
		o.cancel()
		o.queue = nil
		o.changed.Broadcast()
	}
	o.mu.Unlock()
}

func (o *sessionCommitObserver) run() {
	for {
		o.mu.Lock()
		for len(o.queue) == 0 && !o.stopped {
			o.changed.Wait()
		}
		if len(o.queue) == 0 && o.stopped {
			o.mu.Unlock()
			return
		}
		notice := o.queue[0]
		o.queue[0] = session.CommitNotice{}
		o.queue = o.queue[1:]
		o.mu.Unlock()
		for _, candidate := range o.observers {
			callSessionCommitObserver(o.ctx, candidate, notice)
		}
	}
}

func callSessionCommitObserver(ctx context.Context, observer session.SessionCommitObserver, notice session.CommitNotice) {
	defer func() { _ = recover() }()
	_ = observer.ObserveSessionCommit(ctx, notice)
}

func cloneHookMessages(history []session.HistoryFact) []agent.Message {
	messages := make([]agent.Message, 0)
	for _, fact := range history {
		if fact.Message == nil {
			continue
		}
		message := *fact.Message
		message.Parts = cloneRuntimeParts(message.Parts)
		messages = append(messages, message)
	}
	return messages
}

func cloneAgentMessages(source []agent.Message) []agent.Message {
	messages := make([]agent.Message, len(source))
	for index, message := range source {
		messages[index] = message
		messages[index].Parts = cloneRuntimeParts(message.Parts)
	}
	return messages
}
