package standardagent

import (
	"context"
	"sync"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/session"
)

// hookObserver serializes AfterCommit observations without executing product
// code while Runtime holds its state mutex. Errors are deliberately isolated:
// a committed Session fact cannot be rolled back by an observer.
type hookObserver struct {
	hooks  []hook.AgentHook
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	changed *sync.Cond
	queue   []hook.CommitView
	stopped bool
}

func newHookObserver(hooks []hook.AgentHook) *hookObserver {
	if len(hooks) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	observer := &hookObserver{hooks: append([]hook.AgentHook(nil), hooks...), ctx: ctx, cancel: cancel}
	observer.changed = sync.NewCond(&observer.mu)
	go observer.run()
	return observer
}

func (o *hookObserver) publish(view hook.CommitView) {
	if o == nil {
		return
	}
	o.mu.Lock()
	if !o.stopped {
		o.queue = append(o.queue, view)
		o.changed.Signal()
	}
	o.mu.Unlock()
}

func (o *hookObserver) stop() {
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

func (o *hookObserver) run() {
	for {
		o.mu.Lock()
		for len(o.queue) == 0 && !o.stopped {
			o.changed.Wait()
		}
		if len(o.queue) == 0 && o.stopped {
			o.mu.Unlock()
			return
		}
		view := o.queue[0]
		o.queue = o.queue[1:]
		o.mu.Unlock()
		for _, candidate := range o.hooks {
			_ = candidate.AfterCommit(o.ctx, view)
		}
	}
}

func runIDForCommit(changes []session.Change) agent.RunID {
	for _, change := range changes {
		switch {
		case change.RunFact != nil:
			return change.RunFact.RunID
		case change.Message != nil && change.Message.RunID.Valid():
			return change.Message.RunID
		case change.ToolCall != nil:
			return change.ToolCall.RunID
		case change.Journal != nil:
			return change.Journal.RunID
		case change.RunState != nil:
			return change.RunState.RunID
		case change.QueueClaim != nil:
			return change.QueueClaim.RunID
		}
	}
	return ""
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
