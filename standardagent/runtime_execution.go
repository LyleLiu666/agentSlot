package standardagent

import (
	"context"
	"errors"
	"fmt"
	"time"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

type runtimeLifecycle string

const (
	runtimeIdle    runtimeLifecycle = "idle"
	runtimeRunning runtimeLifecycle = "running"
	runtimeClosed  runtimeLifecycle = "closed"
)

type activeRun struct {
	id              agent.RunID
	config          model.Config
	configRevision  agent.Revision
	ctx             context.Context
	cancel          context.CancelFunc
	done            chan struct{}
	cancelRequested bool
}

type stepOutcome uint8

const (
	stepNatural stepOutcome = iota
	stepContinue
	stepFailed
	stepCanceled
)

func (r *runtimeInstance) nextID(kind string) string {
	return fmt.Sprintf("%s-%s-%d", kind, r.prefix, r.sequence.Add(1))
}
func (r *runtimeInstance) runLoop(run *activeRun, step agent.StepID) {
	for {
		outcome, nextStep := r.executeStep(run, step)
		if outcome == stepContinue {
			step = nextStep
			continue
		}
		nextRun, firstStep := r.finishRun(run, outcome)
		if nextRun == nil {
			return
		}
		run, step = nextRun, firstStep
	}
}

func (r *runtimeInstance) executeStep(run *activeRun, step agent.StepID) (stepOutcome, agent.StepID) {
	snapshot, err := r.session.View(run.ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return stepCanceled, ""
		}
		return stepFailed, ""
	}
	r.revisionValue.Store(uint64(snapshot.Revision))
	request := model.ModelRequest{
		SessionID: r.id(), RunID: run.id, StepID: step, Config: cloneRuntimeConfig(run.config),
		ConfigRevision: run.configRevision,
		Messages:       historyMessages(snapshot.History),
	}
	stream, err := r.components.executor.Execute(run.ctx, request)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return stepCanceled, ""
		}
		return stepFailed, ""
	}
	if stream == nil {
		return stepFailed, ""
	}
	defer stream.Close()
	for {
		event, err := stream.Recv(run.ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return stepCanceled, ""
			}
			return stepFailed, ""
		}
		if err := event.Validate(); err != nil {
			return stepFailed, ""
		}
		switch event.Kind {
		case model.EventDelta, model.EventReset:
			// Temporary output is intentionally not a Session fact. Gateway
			// publication is added in its dedicated streaming round.
		case model.EventFailed:
			return stepFailed, ""
		case model.EventComplete:
			next, continued, canceled, err := r.commitCompletion(run, step, *event.Output)
			if canceled {
				return stepCanceled, ""
			}
			if err != nil {
				return stepFailed, ""
			}
			if continued {
				return stepContinue, next
			}
			return stepNatural, ""
		}
	}
}

func (r *runtimeInstance) commitCompletion(run *activeRun, step agent.StepID, output model.Completion) (agent.StepID, bool, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != run || run.cancelRequested || r.closing {
		return "", false, true, nil
	}
	snapshot, err := r.viewLocked(context.Background())
	if err != nil {
		return "", false, false, err
	}
	assistant := agent.Message{
		ID: agent.MessageID(r.nextID("message")), SessionID: r.id(), RunID: run.id, StepID: step,
		Role: agent.RoleAssistant, Parts: cloneRuntimeParts(output.Parts), CreatedAt: time.Now().UTC(),
	}
	changes := []session.Change{{Kind: session.AppendMessage, Message: &assistant}}
	steers := pendingByDelivery(snapshot.Queue, session.DeliverySteer)
	var nextStep agent.StepID
	if len(steers) > 0 {
		nextStep = agent.StepID(r.nextID("step"))
		for _, item := range steers {
			claim := session.QueueClaim{MessageID: item.Message.ID, RunID: run.id}
			consume := session.QueueConsume{MessageID: item.Message.ID, RunID: run.id}
			message := item.Message
			message.RunID = run.id
			message.StepID = nextStep
			changes = append(changes,
				session.Change{Kind: session.ClaimQueue, QueueClaim: &claim},
				session.Change{Kind: session.ConsumeQueue, QueueConsume: &consume},
				session.Change{Kind: session.AppendMessage, Message: &message},
			)
		}
	}
	_, err = r.commitLocked(context.Background(), snapshot.Revision, "model-complete", changes)
	return nextStep, len(steers) > 0, false, err
}

func (r *runtimeInstance) finishRun(run *activeRun, outcome stepOutcome) (*activeRun, agent.StepID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != run {
		return nil, ""
	}
	if run.cancelRequested || r.closing {
		outcome = stepCanceled
	}
	natural := outcome == stepNatural
	snapshot, err := r.viewLocked(context.Background())
	if err != nil {
		r.recoverAfterRunFailureLocked(run)
		return nil, ""
	}
	changes := make([]session.Change, 0, 8)
	if !natural {
		for _, item := range pendingByDelivery(snapshot.Queue, session.DeliverySteer) {
			reclassification := session.QueueReclassify{MessageID: item.Message.ID, Delivery: session.DeliveryHeld}
			changes = append(changes, session.Change{Kind: session.ReclassifyQueue, QueueReclassification: &reclassification})
		}
	}
	terminalKind := session.RunFailed
	switch outcome {
	case stepNatural:
		terminalKind = session.RunCompleted
	case stepCanceled:
		terminalKind = session.RunCanceled
	}
	terminal := session.RunFact{
		SessionID: r.id(), RunID: run.id, Kind: terminalKind,
		ModelConfig: cloneRuntimeConfig(run.config), ConfigRevision: run.configRevision,
	}
	changes = append(changes, session.Change{Kind: session.AppendRunFact, RunFact: &terminal})
	idle := session.RunStateChange{RunID: run.id, State: session.RunIdle}
	changes = append(changes, session.Change{Kind: session.SetRunState, RunState: &idle})
	var nextRun *activeRun
	var nextStep agent.StepID
	if natural && !r.closing {
		if item, ok := firstPending(snapshot.Queue, session.DeliveryNormal); ok {
			nextRun, nextStep, changes = r.appendNextRunChanges(snapshot, item, changes)
		}
	}
	if _, err := r.commitLocked(context.Background(), snapshot.Revision, "run-finish", changes); err != nil {
		if nextRun != nil {
			nextRun.cancel()
		}
		r.recoverAfterRunFailureLocked(run)
		return nil, ""
	}
	run.cancel()
	close(run.done)
	if nextRun != nil {
		r.active = nextRun
		r.state = runtimeRunning
		return nextRun, nextStep
	}
	r.active = nil
	r.state = runtimeIdle
	close(r.idleSignal)
	return nil, ""
}

func (r *runtimeInstance) recoverAfterRunFailureLocked(run *activeRun) {
	snapshot, err := r.components.store.Recover(context.Background(), session.SessionRef{SessionID: r.id()})
	run.cancel()
	close(run.done)
	r.active = nil
	close(r.idleSignal)
	if err != nil {
		// Continuing from an unverified durable state could create a second
		// Run or overwrite a still-running aggregate. Fail this Runtime closed;
		// an explicit Close/Resume obtains a fresh recovery attempt.
		r.state = runtimeClosed
		r.closing = true
		close(r.closeDone)
		return
	}
	r.revisionValue.Store(uint64(snapshot.Revision))
	r.state = runtimeIdle
}

func (r *runtimeInstance) startChangesLocked(snapshot session.Snapshot, item session.QueueItem, enqueue bool) (*activeRun, agent.StepID, []session.Change) {
	runID := agent.RunID(r.nextID("run"))
	stepID := agent.StepID(r.nextID("step"))
	runContext, cancel := context.WithCancel(context.Background())
	run := &activeRun{
		id: runID, config: cloneRuntimeConfig(snapshot.ModelConfig), configRevision: snapshot.Revision,
		ctx: runContext, cancel: cancel, done: make(chan struct{}),
	}
	running := session.RunStateChange{RunID: runID, State: session.RunRunning}
	started := session.RunFact{
		SessionID: r.id(), RunID: runID, Kind: session.RunStarted,
		ModelConfig: cloneRuntimeConfig(snapshot.ModelConfig), ConfigRevision: snapshot.Revision,
	}
	claim := session.QueueClaim{MessageID: item.Message.ID, RunID: runID}
	consume := session.QueueConsume{MessageID: item.Message.ID, RunID: runID}
	message := item.Message
	message.RunID = runID
	message.StepID = stepID
	changes := make([]session.Change, 0, 5)
	if enqueue {
		changes = append(changes, session.Change{Kind: session.EnqueueMessage, QueueItem: &item})
	}
	changes = append(changes,
		session.Change{Kind: session.SetRunState, RunState: &running},
		session.Change{Kind: session.AppendRunFact, RunFact: &started},
		session.Change{Kind: session.ClaimQueue, QueueClaim: &claim},
		session.Change{Kind: session.ConsumeQueue, QueueConsume: &consume},
		session.Change{Kind: session.AppendMessage, Message: &message},
	)
	return run, stepID, changes
}

func (r *runtimeInstance) appendNextRunChanges(snapshot session.Snapshot, item session.QueueItem, changes []session.Change) (*activeRun, agent.StepID, []session.Change) {
	run, step, start := r.startChangesLocked(snapshot, item, false)
	return run, step, append(changes, start...)
}

func (r *runtimeInstance) activateLocked(run *activeRun) {
	r.state = runtimeRunning
	r.active = run
	r.idleSignal = make(chan struct{})
}

func (r *runtimeInstance) viewLocked(ctx context.Context) (session.Snapshot, error) {
	snapshot, err := r.session.View(ctx)
	if err != nil {
		return session.Snapshot{}, err
	}
	r.revisionValue.Store(uint64(snapshot.Revision))
	return snapshot, nil
}

func (r *runtimeInstance) commitLocked(ctx context.Context, expected agent.Revision, operation string, changes []session.Change) (session.Commit, error) {
	commit, err := r.components.store.Commit(ctx, session.CommitRequest{
		SessionID: r.id(), ExpectedRevision: expected,
		IdempotencyKey: fmt.Sprintf("runtime-%s-%s", operation, r.nextID("commit")), Changes: changes,
	})
	if err != nil {
		return session.Commit{}, err
	}
	r.revisionValue.Store(uint64(commit.Revision))
	return commit, nil
}

func (r *runtimeInstance) ensureOpenLocked(operation string) error {
	if r.state == runtimeClosed || r.closing {
		return runtimeClosedError(operation)
	}
	return nil
}

func runtimeClosedError(operation string) error {
	return agent.NewCodedError(agent.ErrorConflict, agent.CodeRuntimeClosed, operation, "Session Runtime is closed", nil)
}

func firstPending(queue []session.QueueItem, delivery session.Delivery) (session.QueueItem, bool) {
	for _, item := range queue {
		if !item.Claimed() && item.Delivery == delivery {
			return item, true
		}
	}
	return session.QueueItem{}, false
}

func pendingByDelivery(queue []session.QueueItem, delivery session.Delivery) []session.QueueItem {
	items := make([]session.QueueItem, 0)
	for _, item := range queue {
		if !item.Claimed() && item.Delivery == delivery {
			items = append(items, item)
		}
	}
	return items
}

func queueByID(queue []session.QueueItem, id agent.MessageID) (session.QueueItem, bool) {
	for _, item := range queue {
		if item.Message.ID == id {
			return item, true
		}
	}
	return session.QueueItem{}, false
}

func historyMessages(history []session.HistoryFact) []agent.Message {
	messages := make([]agent.Message, 0, len(history))
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

func cloneRuntimeParts(source []agent.MessagePart) []agent.MessagePart {
	return append([]agent.MessagePart(nil), source...)
}

func cloneRuntimeConfig(source model.Config) model.Config {
	copy := source
	if source.Parameters.Temperature != nil {
		value := *source.Parameters.Temperature
		copy.Parameters.Temperature = &value
	}
	if source.Parameters.MaxTokens != nil {
		value := *source.Parameters.MaxTokens
		copy.Parameters.MaxTokens = &value
	}
	return copy
}
