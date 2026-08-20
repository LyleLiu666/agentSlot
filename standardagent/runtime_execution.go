package standardagent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/observe"
	"github.com/LyleLiu666/agentSlot/session"
	"github.com/LyleLiu666/agentSlot/tool"
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
	prepared        chan struct{}
	prepareOnce     sync.Once
	prepareRevision agent.Revision
	usedTokens      int64
}

func (r *activeRun) signalPrepared(revision agent.Revision) {
	r.prepareOnce.Do(func() {
		r.prepareRevision = revision
		close(r.prepared)
	})
}

type stepOutcome uint8

const (
	stepNatural stepOutcome = iota
	stepContinue
	stepFailed
	stepCanceled
	stepBudgetExceeded
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
	defer func() { run.signalPrepared(r.revision()) }()
	if r.runBudget(run).Exhausted() {
		if err := r.recordBudgetExceeded(run); err != nil {
			return stepFailed, ""
		}
		return stepBudgetExceeded, ""
	}
	request, err := r.prepareModelRequest(run, step)
	if err != nil {
		run.signalPrepared(r.revision())
		if errors.Is(err, context.Canceled) {
			return stepCanceled, ""
		}
		return stepFailed, ""
	}
	recorder := &runtimeAttemptRecorder{runtime: r, run: run, step: step}
	stream, err := r.components.executor.Execute(run.ctx, request, recorder)
	if err != nil {
		run.signalPrepared(r.revision())
		if errors.Is(err, model.ErrTokenBudgetExceeded) {
			if recordErr := r.recordBudgetExceeded(run); recordErr != nil {
				return stepFailed, ""
			}
			return stepBudgetExceeded, ""
		}
		if errors.Is(err, context.Canceled) {
			return stepCanceled, ""
		}
		return stepFailed, ""
	}
	if stream == nil {
		run.signalPrepared(r.revision())
		return stepFailed, ""
	}
	defer stream.Close()
	for {
		event, err := stream.Recv(run.ctx)
		if err != nil {
			if errors.Is(err, model.ErrTokenBudgetExceeded) {
				if recordErr := r.recordBudgetExceeded(run); recordErr != nil {
					return stepFailed, ""
				}
				return stepBudgetExceeded, ""
			}
			if errors.Is(err, context.Canceled) {
				return stepCanceled, ""
			}
			return stepFailed, ""
		}
		if err := event.Validate(); err != nil {
			return stepFailed, ""
		}
		switch event.Kind {
		case model.EventDelta:
			r.events.publish(interaction.Event{Kind: interaction.EventChunk, SessionID: r.id(), RunID: run.id, StepID: step, AttemptID: event.AttemptID, Text: event.Text})
		case model.EventReset:
			r.observeModelAttempt(observe.TraceModelAttemptReset, run, step, event.AttemptID)
			r.events.publish(interaction.Event{Kind: interaction.EventReset, SessionID: r.id(), RunID: run.id, StepID: step, AttemptID: event.AttemptID})
		case model.EventFailed:
			if errors.Is(event.Err, model.ErrTokenBudgetExceeded) {
				if recordErr := r.recordBudgetExceeded(run); recordErr != nil {
					return stepFailed, ""
				}
				return stepBudgetExceeded, ""
			}
			return stepFailed, ""
		case model.EventComplete:
			calls, canceled, err := r.commitCompletion(run, step, *event.Output)
			if canceled {
				return stepCanceled, ""
			}
			if err != nil {
				return stepFailed, ""
			}
			if len(calls) > 0 {
				results := r.components.dispatcher.dispatch(run.ctx, calls)
				next, canceled, err := r.commitToolResults(run, calls, results)
				if err != nil {
					return stepFailed, ""
				}
				if canceled {
					return stepCanceled, ""
				}
				return stepContinue, next
			}
			next, continued, canceled, err := r.continueAfterCompletion(run)
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

type runtimeAttemptRecorder struct {
	runtime *runtimeInstance
	run     *activeRun
	step    agent.StepID
}

func (a *runtimeAttemptRecorder) Started(_ context.Context, started model.AttemptStart) error {
	if err := started.Validate(); err != nil {
		return err
	}
	r := a.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != a.run || r.closing {
		return context.Canceled
	}
	if started.ProviderKey != a.run.config.ProviderKey || started.ModelID != a.run.config.ModelID {
		return agent.NewError(agent.ErrorInvalidInput, "standardagent.model_attempt", "Executor attempt does not match the frozen Run model", nil)
	}
	snapshot, err := r.viewLocked(context.Background())
	if err != nil {
		return err
	}
	fact := session.ModelAttemptFact{
		AttemptID: started.AttemptID, RunID: a.run.id, StepID: a.step, Kind: session.AttemptStarted,
		ProviderKey: started.ProviderKey, ModelID: started.ModelID,
	}
	if _, err := r.commitLocked(context.Background(), snapshot.Revision, "attempt-start", []session.Change{{Kind: session.AppendModelAttempt, ModelAttempt: &fact}}); err != nil {
		return err
	}
	a.run.signalPrepared(r.revision())
	r.observeModelAttempt(observe.TraceModelAttemptStarted, a.run, a.step, string(started.AttemptID))
	r.components.observations.publishMetric(observe.MetricRecord{
		Name: observe.MetricModelAttemptTotal, Kind: observe.MetricCounter, Value: 1, At: time.Now().UTC(),
		Attributes: map[string]string{"provider": a.run.config.ProviderKey, "model": a.run.config.ModelID},
	})
	return nil
}

func (a *runtimeAttemptRecorder) Finished(_ context.Context, finished model.AttemptFinish) error {
	if err := finished.Validate(); err != nil {
		return err
	}
	r := a.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != a.run {
		return context.Canceled
	}
	kind := session.AttemptFailed
	trace := observe.TraceModelAttemptFailed
	switch finished.Outcome {
	case model.AttemptSucceeded:
		kind, trace = session.AttemptSucceeded, observe.TraceModelAttemptDone
	case model.AttemptCanceled:
		kind = session.AttemptCanceled
	}
	snapshot, err := r.viewLocked(context.Background())
	if err != nil {
		return err
	}
	fact := session.ModelAttemptFact{
		AttemptID: finished.AttemptID, RunID: a.run.id, StepID: a.step, Kind: kind,
		ProviderKey: a.run.config.ProviderKey, ModelID: a.run.config.ModelID,
		ProviderRequestID: finished.ProviderRequestID, Usage: finished.Usage, ErrorCode: finished.ErrorCode,
	}
	if _, err := r.commitLocked(context.Background(), snapshot.Revision, "attempt-finish", []session.Change{{Kind: session.AppendModelAttempt, ModelAttempt: &fact}}); err != nil {
		return err
	}
	a.run.usedTokens += finished.Usage.TotalTokens
	r.observeModelAttempt(trace, a.run, a.step, string(finished.AttemptID))
	r.components.observations.publishUsage(observe.UsageRecord{
		Kind: observe.UsageModel, At: time.Now().UTC(),
		Identity:    observe.Identity{SessionID: r.id(), RunID: a.run.id, StepID: a.step},
		ProviderKey: a.run.config.ProviderKey, ModelID: a.run.config.ModelID, AttemptID: string(finished.AttemptID),
		InputTokens: int(finished.Usage.InputTokens), OutputTokens: int(finished.Usage.OutputTokens), TotalTokens: int(finished.Usage.TotalTokens),
	})
	return nil
}

func (a *runtimeAttemptRecorder) Budget() model.TokenBudget { return a.runtime.runBudget(a.run) }

func (r *runtimeInstance) runBudget(run *activeRun) model.TokenBudget {
	r.mu.Lock()
	defer r.mu.Unlock()
	return model.TokenBudget{MaxTokens: r.components.config.MaxTokensPerRun, UsedTokens: run.usedTokens}
}

func (r *runtimeInstance) recordBudgetExceeded(run *activeRun) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != run {
		return context.Canceled
	}
	if r.components.config.MaxTokensPerRun <= 0 || run.usedTokens < r.components.config.MaxTokensPerRun {
		return agent.NewError(agent.ErrorInternal, "standardagent.run_budget", "Executor reported an unexhausted Run budget", nil)
	}
	snapshot, err := r.viewLocked(context.Background())
	if err != nil {
		return err
	}
	fact := session.RunBudgetExceededFact{RunID: run.id, UsedTokens: run.usedTokens, MaxTokens: r.components.config.MaxTokensPerRun}
	_, err = r.commitLocked(context.Background(), snapshot.Revision, "run-budget", []session.Change{{Kind: session.AppendRunBudgetExceeded, RunBudgetExceeded: &fact}})
	return err
}

func (r *runtimeInstance) observeModelAttempt(kind observe.TraceKind, run *activeRun, step agent.StepID, attemptID string) {
	r.components.observations.publishTrace(observe.TraceRecord{
		Kind: kind, At: time.Now().UTC(), AttemptID: attemptID,
		Identity: observe.Identity{SessionID: r.id(), RunID: run.id, StepID: step},
	})
}

func (r *runtimeInstance) commitCompletion(run *activeRun, step agent.StepID, output model.Completion) ([]agent.ToolCall, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != run || run.cancelRequested || r.closing {
		return nil, true, nil
	}
	snapshot, err := r.viewLocked(context.Background())
	if err != nil {
		return nil, false, err
	}
	assistant := agent.Message{
		ID: agent.MessageID(r.nextID("message")), SessionID: r.id(), RunID: run.id, StepID: step,
		Role: agent.RoleAssistant, Parts: cloneRuntimeParts(output.Parts), CreatedAt: time.Now().UTC(),
	}
	changes := []session.Change{{Kind: session.AppendMessage, Message: &assistant}}
	calls := make([]agent.ToolCall, 0, len(output.ToolCalls))
	for _, requested := range output.ToolCalls {
		call := agent.ToolCall{
			ID: agent.ToolCallID(r.nextID("call")), MessageID: assistant.ID, SessionID: r.id(),
			RunID: run.id, StepID: step, CorrelationID: requested.CorrelationID,
			Name: requested.Name, Arguments: append([]byte(nil), requested.Arguments...),
		}
		pending := session.JournalEntry{RunID: run.id, StepID: step, ToolCall: &call, Status: session.JournalPending}
		changes = append(changes,
			session.Change{Kind: session.AppendToolCall, ToolCall: &call},
			session.Change{Kind: session.UpdateRunJournal, Journal: &pending},
		)
		calls = append(calls, call)
	}
	_, err = r.commitLocked(context.Background(), snapshot.Revision, "model-complete", changes)
	return calls, false, err
}

func (r *runtimeInstance) continueAfterCompletion(run *activeRun) (agent.StepID, bool, bool, error) {
	snapshot, err := r.session.View(run.ctx)
	if err != nil {
		return "", false, errors.Is(err, context.Canceled), err
	}
	proposals := make([]agent.MessageInput, 0)
	view := hook.RunCompleteView{SessionID: r.id(), RunID: run.id, Revision: snapshot.Revision, Messages: cloneHookMessages(snapshot.History)}
	for _, candidate := range r.components.hooks {
		candidateView := view
		candidateView.Messages = cloneAgentMessages(view.Messages)
		proposal, hookErr := candidate.BeforeRunComplete(run.ctx, candidateView)
		if hookErr != nil {
			continue
		}
		for _, input := range proposal.Messages {
			if input.Valid() {
				proposals = append(proposals, agent.MessageInput{Parts: cloneRuntimeParts(input.Parts)})
			}
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != run || run.cancelRequested || r.closing {
		return "", false, true, nil
	}
	snapshot, err = r.viewLocked(context.Background())
	if err != nil {
		return "", false, false, err
	}
	steers := pendingByDelivery(snapshot.Queue, session.DeliverySteer)
	if len(steers) == 0 && len(proposals) == 0 {
		return "", false, false, nil
	}
	nextStep := agent.StepID(r.nextID("step"))
	changes := make([]session.Change, 0, len(steers)*3+len(proposals)*4)
	for _, item := range steers {
		changes = appendInputConsumption(changes, item, run.id, nextStep)
	}
	for _, proposal := range proposals {
		message := agent.Message{
			ID: agent.MessageID(r.nextID("message")), SessionID: r.id(), Role: agent.RoleUser,
			Parts: cloneRuntimeParts(proposal.Parts), CreatedAt: time.Now().UTC(),
		}
		item := session.QueueItem{Message: message, Delivery: session.DeliverySteer}
		changes = append(changes, session.Change{Kind: session.EnqueueMessage, QueueItem: &item})
		changes = appendInputConsumption(changes, item, run.id, nextStep)
	}
	_, err = r.commitLocked(context.Background(), snapshot.Revision, "follow-on", changes)
	return nextStep, err == nil, false, err
}

func appendInputConsumption(changes []session.Change, item session.QueueItem, runID agent.RunID, stepID agent.StepID) []session.Change {
	claim := session.QueueClaim{MessageID: item.Message.ID, RunID: runID}
	consume := session.QueueConsume{MessageID: item.Message.ID, RunID: runID}
	message := item.Message
	message.RunID = runID
	message.StepID = stepID
	return append(changes,
		session.Change{Kind: session.ClaimQueue, QueueClaim: &claim},
		session.Change{Kind: session.ConsumeQueue, QueueConsume: &consume},
		session.Change{Kind: session.AppendMessage, Message: &message},
	)
}

func (r *runtimeInstance) commitToolResults(run *activeRun, calls []agent.ToolCall, results []tool.ToolResult) (agent.StepID, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != run {
		return "", true, nil
	}
	snapshot, err := r.viewLocked(context.Background())
	if err != nil {
		return "", false, err
	}
	changes := make([]session.Change, 0, len(results)*2+8)
	for index, result := range results {
		resultCopy := cloneRuntimeToolResult(result)
		status := session.JournalFailed
		switch result.Status {
		case tool.ResultSucceeded:
			status = session.JournalSucceeded
		case tool.ResultUnknown:
			status = session.JournalOutcomeUnknown
		}
		call := calls[index]
		journal := session.JournalEntry{RunID: call.RunID, StepID: call.StepID, ToolCall: &call, ToolResult: &resultCopy, Status: status}
		changes = append(changes,
			session.Change{Kind: session.AppendToolResult, ToolResult: &resultCopy},
			session.Change{Kind: session.UpdateRunJournal, Journal: &journal},
		)
	}
	nextStep := agent.StepID(r.nextID("step"))
	for _, item := range pendingByDelivery(snapshot.Queue, session.DeliverySteer) {
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
	_, err = r.commitLocked(context.Background(), snapshot.Revision, "tool-results", changes)
	return nextStep, run.cancelRequested || r.closing, err
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
	case stepBudgetExceeded:
		terminalKind = session.RunInterrupted
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
		r.observer.stop()
		r.events.close()
		r.components.observations.publishTrace(observe.TraceRecord{
			Kind: observe.TraceRuntimeClosed, At: time.Now().UTC(), Identity: observe.Identity{SessionID: r.id()},
		})
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
		ctx: runContext, cancel: cancel, done: make(chan struct{}), prepared: make(chan struct{}),
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
	r.observer.publish(hook.CommitView{SessionID: r.id(), RunID: runIDForCommit(changes), Revision: commit.Revision})
	r.publishCommitEvents(commit.Revision, changes)
	r.publishCommitObservations(changes)
	return commit, nil
}

func (r *runtimeInstance) publishCommitObservations(changes []session.Change) {
	now := time.Now().UTC()
	for _, change := range changes {
		if change.RunFact != nil {
			kind := observe.TraceRunStarted
			outcome := "started"
			switch change.RunFact.Kind {
			case session.RunCompleted:
				kind, outcome = observe.TraceRunCompleted, "completed"
			case session.RunCanceled:
				kind, outcome = observe.TraceRunCanceled, "canceled"
			case session.RunFailed, session.RunInterrupted:
				kind, outcome = observe.TraceRunFailed, string(change.RunFact.Kind)
			}
			identity := observe.Identity{SessionID: r.id(), RunID: change.RunFact.RunID}
			r.components.observations.publishTrace(observe.TraceRecord{Kind: kind, At: now, Identity: identity})
			if change.RunFact.Kind != session.RunStarted {
				r.components.observations.publishMetric(observe.MetricRecord{
					Name: observe.MetricRunTotal, Kind: observe.MetricCounter, Value: 1, At: now,
					Attributes: map[string]string{"outcome": outcome},
				})
			}
		}
		if change.SessionEvent != nil && change.SessionEvent.Kind == session.EventModelConfigChanged {
			current := change.SessionEvent.ModelConfigChanged.Current
			r.components.observations.publishAudit(observe.AuditRecord{
				Kind: observe.AuditModelConfigChanged, At: now, Identity: observe.Identity{SessionID: r.id()},
				Action: current.ProviderKey + "/" + current.ModelID, Decision: "committed",
			})
		}
	}
}

func (r *runtimeInstance) publishCommitEvents(revision agent.Revision, changes []session.Change) {
	runID := runIDForCommit(changes)
	published := false
	for _, change := range changes {
		if change.Message == nil {
			continue
		}
		message := *change.Message
		message.Parts = cloneRuntimeParts(message.Parts)
		r.events.publish(interaction.Event{Kind: interaction.EventCommitted, SessionID: r.id(), RunID: message.RunID, StepID: message.StepID, Revision: revision, Message: &message})
		published = true
	}
	kind := interaction.EventCommitted
	for _, change := range changes {
		if change.RunState != nil {
			kind = interaction.EventState
			break
		}
	}
	if !published || kind == interaction.EventState {
		r.events.publish(interaction.Event{Kind: kind, SessionID: r.id(), RunID: runID, Revision: revision})
	}
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

func historyInputs(history []session.HistoryFact) []model.Input {
	inputs := make([]model.Input, 0, len(history))
	for _, fact := range history {
		input := model.Input{}
		switch {
		case fact.Message != nil:
			message := *fact.Message
			message.Parts = cloneRuntimeParts(message.Parts)
			input.Message = &message
		case fact.ToolCall != nil:
			call := *fact.ToolCall
			call.Arguments = append([]byte(nil), fact.ToolCall.Arguments...)
			input.ToolCall = &call
		case fact.ToolResult != nil:
			result := cloneRuntimeToolResult(*fact.ToolResult)
			input.ToolResult = &result
		default:
			continue
		}
		inputs = append(inputs, input)
	}
	return inputs
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

func cloneRuntimeToolResult(source tool.ToolResult) tool.ToolResult {
	copy := source
	copy.Output = append([]byte(nil), source.Output...)
	if source.Error != nil {
		errorCopy := *source.Error
		copy.Error = &errorCopy
	}
	return copy
}
