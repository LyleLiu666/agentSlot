package standardagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/goal"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/interaction"
	agentloop "github.com/LyleLiu666/agentSlot/loop"
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
	controlActor    agent.ActorIdentity
}

func (r *activeRun) signalPrepared(revision agent.Revision) {
	r.prepareOnce.Do(func() {
		r.prepareRevision = revision
		close(r.prepared)
	})
}

// restorePreparedRun reconstructs only the safe, pre-execution portion of an
// interrupted Run. SessionStore recovery has already converted every call
// that may have crossed the execution boundary to outcome_unknown.
func (r *runtimeInstance) restorePreparedRun(snapshot session.Snapshot) error {
	if snapshot.RunState != session.RunRunning {
		return nil
	}
	calls := make([]agent.ToolCall, 0)
	var step agent.StepID
	for _, entry := range snapshot.RunJournal {
		if entry.Status != session.JournalPrepared || entry.RunID != snapshot.ActiveRunID || entry.ToolCall == nil {
			continue
		}
		if step.Valid() && step != entry.StepID {
			return agent.NewError(agent.ErrorInternal, "standardagent.restore", "prepared ToolCalls span multiple steps", nil)
		}
		step = entry.StepID
		calls = append(calls, *entry.ToolCall)
	}
	if len(calls) == 0 || !step.Valid() {
		return agent.NewError(agent.ErrorInternal, "standardagent.restore", "running Session has no resumable prepared ToolCall", nil)
	}
	var started *session.RunFact
	var usedTokens int64
	for _, fact := range snapshot.History {
		if fact.Run != nil && fact.Run.RunID == snapshot.ActiveRunID && fact.Run.Kind == session.RunStarted {
			copy := *fact.Run
			started = &copy
		}
		if fact.ModelAttempt != nil && fact.ModelAttempt.RunID == snapshot.ActiveRunID && fact.ModelAttempt.Kind != session.AttemptStarted {
			usedTokens += fact.ModelAttempt.Usage.TotalTokens
		}
	}
	if started == nil {
		return agent.NewError(agent.ErrorInternal, "standardagent.restore", "prepared Run has no RunStarted fact", nil)
	}
	runContext, cancel := context.WithCancel(context.Background())
	run := &activeRun{
		id: snapshot.ActiveRunID, config: cloneRuntimeConfig(started.ModelConfig), configRevision: started.ConfigRevision,
		ctx: runContext, cancel: cancel, done: make(chan struct{}), prepared: make(chan struct{}), usedTokens: usedTokens,
	}
	r.activateLocked(run)
	run.signalPrepared(snapshot.Revision)
	go r.runLoopPrepared(run, step, calls)
	return nil
}

type stepOutcome uint8

const (
	stepNatural stepOutcome = iota
	stepContinue
	stepFailed
	stepCanceled
	stepBudgetExceeded
	stepWaiting
	stepToolsReady
)

func (r *runtimeInstance) nextID(kind string) string {
	return fmt.Sprintf("%s-%s-%d", kind, r.prefix, r.sequence.Add(1))
}
func (r *runtimeInstance) runLoop(run *activeRun, step agent.StepID) {
	r.runLoopWithPrepared(run, step, nil)
}

func (r *runtimeInstance) runLoopPrepared(run *activeRun, step agent.StepID, calls []agent.ToolCall) {
	r.runLoopWithPrepared(run, step, calls)
}

func (r *runtimeInstance) runLoopWithPrepared(run *activeRun, step agent.StepID, prepared []agent.ToolCall) {
	for {
		driver := &runtimeLoopRun{runtime: r, run: run, step: step}
		if len(prepared) > 0 {
			driver.state = agentloop.StateToolsReady
			driver.pendingTools = append([]agent.ToolCall(nil), prepared...)
		}
		outcome, err := invokeAgentLoop(r.components.agentLoop, run.ctx, driver)
		escapedAction := driver.closeActions()
		prepared = nil
		run.signalPrepared(r.revision())
		if err != nil {
			if errors.Is(err, context.Canceled) || run.ctx.Err() != nil {
				outcome = agentloop.OutcomeCanceled
			} else {
				outcome = agentloop.OutcomeFailed
			}
		}
		terminal, committedOutcome := driver.terminalOutcome()
		if escapedAction || !outcome.Terminal() || !terminal || committedOutcome != outcome {
			outcome = agentloop.OutcomeFailed
		}
		nextRun, firstStep := r.finishRun(run, stepOutcomeFromLoop(outcome))
		if nextRun == nil {
			return
		}
		run, step = nextRun, firstStep
	}
}

type runtimeLoopRun struct {
	runtime        *runtimeInstance
	run            *activeRun
	step           agent.StepID
	lifecycleMu    sync.Mutex
	closed         bool
	active         bool
	activeDone     chan struct{}
	stateMu        sync.Mutex
	terminal       bool
	outcome        agentloop.Outcome
	state          agentloop.State
	nextStep       agent.StepID
	pendingTools   []agent.ToolCall
	legacyStepping atomic.Bool
}

func (r *runtimeLoopRun) SessionID() agent.SessionID { return r.runtime.id() }
func (r *runtimeLoopRun) RunID() agent.RunID         { return r.run.id }
func (r *runtimeLoopRun) State() agentloop.State {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if r.state == "" {
		return agentloop.StateReadyForModel
	}
	return r.state
}

func (r *runtimeLoopRun) Step(ctx context.Context) (agentloop.Outcome, error) {
	if !r.legacyStepping.CompareAndSwap(false, true) {
		return agentloop.OutcomeFailed, errors.New("standardagent: AgentLoop called Run.Step concurrently")
	}
	defer r.legacyStepping.Store(false)
	state := r.State()
	if state == agentloop.StateContinueReady {
		var err error
		state, err = r.Act(ctx, agentloop.Action{Kind: agentloop.ActionContinue})
		if err != nil {
			return agentloop.OutcomeFailed, err
		}
	}
	if state == agentloop.StateReadyForModel {
		var err error
		state, err = r.Act(ctx, agentloop.Action{Kind: agentloop.ActionRequestModel})
		if err != nil {
			return agentloop.OutcomeFailed, err
		}
	}
	if state == agentloop.StateToolsReady {
		var err error
		state, err = r.Act(ctx, agentloop.Action{Kind: agentloop.ActionExecuteTools})
		if err != nil {
			return agentloop.OutcomeFailed, err
		}
	}
	if state == agentloop.StateContinueReady {
		return agentloop.OutcomeContinue, nil
	}
	outcome := loopOutcomeFromState(state)
	if !outcome.Terminal() {
		return agentloop.OutcomeFailed, errors.New("standardagent: Runtime returned an unknown Loop state")
	}
	if outcome != agentloop.OutcomeWaiting {
		if _, err := r.Act(ctx, agentloop.Action{Kind: agentloop.ActionFinish, Outcome: outcome}); err != nil {
			return agentloop.OutcomeFailed, err
		}
	}
	return outcome, nil
}

func (r *runtimeLoopRun) Act(ctx context.Context, action agentloop.Action) (agentloop.State, error) {
	if err := ctx.Err(); err != nil {
		return agentloop.StateCanceled, err
	}
	if err := r.run.ctx.Err(); err != nil {
		return agentloop.StateCanceled, err
	}
	if err := action.Validate(); err != nil {
		return agentloop.StateFailed, err
	}
	if err := r.beginAction(); err != nil {
		return agentloop.StateFailed, err
	}
	defer r.endAction()
	r.stateMu.Lock()
	if r.terminal {
		r.stateMu.Unlock()
		return agentloop.StateFailed, errors.New("standardagent: AgentLoop submitted an action after terminal state")
	}
	if r.state == "" {
		r.state = agentloop.StateReadyForModel
	}
	r.stateMu.Unlock()
	switch action.Kind {
	case agentloop.ActionRequestModel:
		r.stateMu.Lock()
		if r.state != agentloop.StateReadyForModel {
			r.stateMu.Unlock()
			return agentloop.StateFailed, errors.New("standardagent: model action is not valid in the current Run state")
		}
		r.stateMu.Unlock()
		outcome, next, calls := r.runtime.requestModel(r.run, r.step)
		r.stateMu.Lock()
		r.nextStep = next
		r.pendingTools = calls
		r.state = loopStateFromStep(outcome)
		state := r.state
		r.stateMu.Unlock()
		return state, nil
	case agentloop.ActionContinue:
		r.stateMu.Lock()
		defer r.stateMu.Unlock()
		if r.state != agentloop.StateContinueReady || !r.nextStep.Valid() {
			return agentloop.StateFailed, errors.New("standardagent: continue action is not valid in the current Run state")
		}
		r.step, r.nextStep, r.state = r.nextStep, "", agentloop.StateReadyForModel
		return r.state, nil
	case agentloop.ActionExecuteTools:
		r.stateMu.Lock()
		if r.state != agentloop.StateToolsReady || len(r.pendingTools) == 0 {
			r.stateMu.Unlock()
			return agentloop.StateFailed, errors.New("standardagent: no prepared Tool batch is available")
		}
		calls := append([]agent.ToolCall(nil), r.pendingTools...)
		r.stateMu.Unlock()
		dispatched := r.runtime.components.dispatcher.dispatchPrepared(r.run.ctx, calls, func(call agent.ToolCall) error {
			return r.runtime.markToolExecuting(r.run, call)
		}, r.runtime.workspaceScope(), r.runtime.workspaceBoundary)
		next, canceled, err := r.runtime.commitToolResults(r.run, calls, dispatched.results)
		r.stateMu.Lock()
		r.pendingTools = nil
		if err != nil || dispatched.contractViolation {
			r.state = agentloop.StateFailed
		} else if canceled {
			r.state = agentloop.StateCanceled
		} else {
			r.nextStep, r.state = next, agentloop.StateContinueReady
		}
		state := r.state
		r.stateMu.Unlock()
		return state, err
	case agentloop.ActionWait:
		r.stateMu.Lock()
		defer r.stateMu.Unlock()
		if r.state != agentloop.StateReadyForModel && r.state != agentloop.StateContinueReady {
			return agentloop.StateFailed, errors.New("standardagent: wait action is not valid in the current Run state")
		}
		r.state, r.terminal, r.outcome = agentloop.StateWaiting, true, agentloop.OutcomeWaiting
		return r.state, nil
	case agentloop.ActionFinish:
		r.stateMu.Lock()
		defer r.stateMu.Unlock()
		if !finishAllowed(r.state, action.Outcome) {
			return agentloop.StateFailed, errors.New("standardagent: finish outcome does not match Runtime state")
		}
		r.terminal, r.outcome = true, action.Outcome
		return r.state, nil
	default:
		return agentloop.StateFailed, errors.New("standardagent: unsupported Run action")
	}
}

func (r *runtimeLoopRun) beginAction() error {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if r.closed {
		return errors.New("standardagent: AgentLoop submitted an action after Run returned")
	}
	if r.active {
		return errors.New("standardagent: AgentLoop submitted concurrent actions")
	}
	r.active = true
	r.activeDone = make(chan struct{})
	return nil
}

func (r *runtimeLoopRun) endAction() {
	r.lifecycleMu.Lock()
	r.active = false
	close(r.activeDone)
	r.activeDone = nil
	r.lifecycleMu.Unlock()
}

func (r *runtimeLoopRun) closeActions() bool {
	r.lifecycleMu.Lock()
	r.closed = true
	done := r.activeDone
	r.lifecycleMu.Unlock()
	if done == nil {
		return false
	}
	r.run.cancel()
	<-done
	return true
}

func (r *runtimeLoopRun) terminalOutcome() (bool, agentloop.Outcome) {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	return r.terminal, r.outcome
}

func finishAllowed(state agentloop.State, outcome agentloop.Outcome) bool {
	if state == agentloop.StateReadyForModel || state == agentloop.StateContinueReady {
		return outcome == agentloop.OutcomeCompleted || outcome == agentloop.OutcomeFailed
	}
	return loopOutcomeFromState(state) == outcome
}

func invokeAgentLoop(component agentloop.AgentLoop, ctx context.Context, run agentloop.Run) (outcome agentloop.Outcome, err error) {
	defer func() {
		if recover() != nil {
			outcome = agentloop.OutcomeFailed
			err = errors.New("standardagent: AgentLoop panicked")
		}
	}()
	return component.Run(ctx, run)
}

func loopStateFromStep(outcome stepOutcome) agentloop.State {
	switch outcome {
	case stepContinue:
		return agentloop.StateContinueReady
	case stepNatural:
		return agentloop.StateCompleted
	case stepCanceled:
		return agentloop.StateCanceled
	case stepBudgetExceeded:
		return agentloop.StateBudgetExceeded
	case stepWaiting:
		return agentloop.StateWaiting
	case stepToolsReady:
		return agentloop.StateToolsReady
	default:
		return agentloop.StateFailed
	}
}

func stepOutcomeFromLoop(outcome agentloop.Outcome) stepOutcome {
	switch outcome {
	case agentloop.OutcomeCompleted:
		return stepNatural
	case agentloop.OutcomeCanceled:
		return stepCanceled
	case agentloop.OutcomeBudgetExceeded:
		return stepBudgetExceeded
	case agentloop.OutcomeWaiting:
		return stepWaiting
	default:
		return stepFailed
	}
}

func (r *runtimeInstance) requestModel(run *activeRun, step agent.StepID) (stepOutcome, agent.StepID, []agent.ToolCall) {
	defer func() { run.signalPrepared(r.revision()) }()
	if r.runBudget(run).Exhausted() {
		if err := r.recordBudgetExceeded(run); err != nil {
			return stepFailed, "", nil
		}
		return stepBudgetExceeded, "", nil
	}
	request, err := r.prepareModelRequest(run, step)
	if err != nil {
		run.signalPrepared(r.revision())
		if errors.Is(err, context.Canceled) {
			return stepCanceled, "", nil
		}
		return stepFailed, "", nil
	}
	recorder := &runtimeAttemptRecorder{runtime: r, run: run, step: step}
	stream, err := r.components.executor.Execute(run.ctx, request, recorder)
	if err != nil {
		run.signalPrepared(r.revision())
		if errors.Is(err, model.ErrTokenBudgetExceeded) {
			if recordErr := r.recordBudgetExceeded(run); recordErr != nil {
				return stepFailed, "", nil
			}
			return stepBudgetExceeded, "", nil
		}
		if errors.Is(err, context.Canceled) {
			return stepCanceled, "", nil
		}
		return stepFailed, "", nil
	}
	if stream == nil {
		run.signalPrepared(r.revision())
		return stepFailed, "", nil
	}
	defer stream.Close()
	assistantMessageID := agent.MessageID(r.nextID("message"))
	for {
		event, err := stream.Recv(run.ctx)
		if err != nil {
			if errors.Is(err, model.ErrTokenBudgetExceeded) {
				if recordErr := r.recordBudgetExceeded(run); recordErr != nil {
					return stepFailed, "", nil
				}
				return stepBudgetExceeded, "", nil
			}
			if errors.Is(err, context.Canceled) {
				return stepCanceled, "", nil
			}
			return stepFailed, "", nil
		}
		if err := event.Validate(); err != nil {
			return stepFailed, "", nil
		}
		switch event.Kind {
		case model.EventDelta:
			r.events.publish(interaction.Event{Kind: interaction.EventChunk, SessionID: r.id(), RunID: run.id, StepID: step, MessageID: assistantMessageID, AttemptID: event.AttemptID, Text: event.Text})
		case model.EventReset:
			r.observeModelAttempt(observe.TraceModelAttemptReset, run, step, agent.AttemptID(event.AttemptID))
			r.events.publish(interaction.Event{Kind: interaction.EventReset, SessionID: r.id(), RunID: run.id, StepID: step, MessageID: assistantMessageID, AttemptID: event.AttemptID})
		case model.EventFailed:
			if errors.Is(event.Err, model.ErrTokenBudgetExceeded) {
				if recordErr := r.recordBudgetExceeded(run); recordErr != nil {
					return stepFailed, "", nil
				}
				return stepBudgetExceeded, "", nil
			}
			return stepFailed, "", nil
		case model.EventComplete:
			calls, canceled, err := r.commitCompletion(run, step, assistantMessageID, *event.Output)
			if canceled {
				return stepCanceled, "", nil
			}
			if err != nil {
				return stepFailed, "", nil
			}
			if len(calls) > 0 {
				return stepToolsReady, "", calls
			}
			next, continued, canceled, err := r.continueAfterCompletion(run)
			if canceled {
				return stepCanceled, "", nil
			}
			if err != nil {
				return stepFailed, "", nil
			}
			if continued {
				return stepContinue, next, nil
			}
			return stepNatural, "", nil
		}
	}
}

// markToolExecuting is the durable point of no automatic return. Before this
// commit a published ToolCall may be authorized again after recovery; after
// it, recovery must report outcome_unknown instead of risking a duplicate
// side effect.
func (r *runtimeInstance) markToolExecuting(run *activeRun, call agent.ToolCall) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != run || run.cancelRequested || r.closing {
		return context.Canceled
	}
	snapshot, err := r.viewLocked(context.Background())
	if err != nil {
		return err
	}
	pending := session.JournalEntry{
		RunID: call.RunID, StepID: call.StepID, ToolCall: &call, Status: session.JournalPending,
	}
	_, err = r.commitLocked(context.Background(), snapshot.Revision, "tool-executing", []session.Change{{Kind: session.UpdateRunJournal, Journal: &pending}})
	return err
}

type runtimeAttemptRecorder struct {
	runtime *runtimeInstance
	run     *activeRun
	step    agent.StepID
}

func (a *runtimeAttemptRecorder) Started(ctx context.Context, started model.AttemptStart) error {
	if err := started.Validate(); err != nil {
		return err
	}
	r := a.runtime
	r.mu.Lock()
	if r.active != a.run || r.closing {
		r.mu.Unlock()
		return context.Canceled
	}
	if started.ProviderKey != a.run.config.ProviderKey || started.ModelID != a.run.config.ModelID {
		r.mu.Unlock()
		return agent.NewError(agent.ErrorInvalidInput, "standardagent.model_attempt", "Executor attempt does not match the frozen Run model", nil)
	}
	identity := model.AttemptIdentity{
		SessionID: r.id(), RunID: a.run.id, StepID: a.step, AttemptID: started.AttemptID,
		ConfigRevision: a.run.configRevision, Config: cloneRuntimeConfig(a.run.config),
	}
	r.mu.Unlock()
	event := model.AttemptStarted{Identity: identity}
	accepted := make([]model.AttemptObserver, 0, len(r.components.attemptObservers))
	for _, observer := range r.components.attemptObservers {
		if err := observer.AttemptStarted(ctx, event); err != nil {
			return compensateAttemptStart(ctx, identity, accepted,
				fmt.Errorf("standardagent: record model attempt start: %w", err))
		}
		accepted = append(accepted, observer)
	}

	r.mu.Lock()
	if r.active != a.run || r.closing || a.run.cancelRequested {
		r.mu.Unlock()
		return compensateAttemptStart(ctx, identity, accepted, context.Canceled)
	}
	snapshot, err := r.viewLocked(context.Background())
	if err != nil {
		r.mu.Unlock()
		return compensateAttemptStart(ctx, identity, accepted, err)
	}
	fact := session.ModelAttemptFact{
		AttemptID: started.AttemptID, RunID: a.run.id, StepID: a.step, Kind: session.AttemptStarted,
		ProviderKey: started.ProviderKey, ModelID: started.ModelID,
	}
	if _, err := r.commitLocked(context.Background(), snapshot.Revision, "attempt-start", []session.Change{{Kind: session.AppendModelAttempt, ModelAttempt: &fact}}); err != nil {
		r.mu.Unlock()
		return compensateAttemptStart(ctx, identity, accepted, err)
	}
	a.run.signalPrepared(r.revision())
	r.observeModelAttempt(observe.TraceModelAttemptStarted, a.run, a.step, started.AttemptID)
	r.components.observations.publishMetric(observe.MetricRecord{
		Name: observe.MetricModelAttemptTotal, Kind: observe.MetricCounter, Value: 1, At: time.Now().UTC(),
		Identity: observe.Identity{
			SessionID: r.id(), RunID: a.run.id, StepID: a.step, AttemptID: started.AttemptID,
			Actor: serviceObservationActor("model-executor"),
		},
		Attributes: map[string]string{"provider": a.run.config.ProviderKey, "model": a.run.config.ModelID},
	})
	r.mu.Unlock()
	return nil
}

func compensateAttemptStart(ctx context.Context, identity model.AttemptIdentity, accepted []model.AttemptObserver, cause error) error {
	compensation := model.AttemptFinished{
		Identity: identity, Outcome: model.AttemptCanceled, ErrorCode: "attempt_start_rejected",
	}
	for index := len(accepted) - 1; index >= 0; index-- {
		if err := accepted[index].AttemptFinished(context.WithoutCancel(ctx), compensation); err != nil {
			cause = errors.Join(cause, fmt.Errorf("standardagent: compensate accepted attempt observer: %w", err))
		}
	}
	return cause
}

func (a *runtimeAttemptRecorder) Finished(ctx context.Context, finished model.AttemptFinish) error {
	if err := finished.Validate(); err != nil {
		return err
	}
	r := a.runtime
	r.mu.Lock()
	if r.active != a.run {
		r.mu.Unlock()
		return context.Canceled
	}
	identity := model.AttemptIdentity{
		SessionID: r.id(), RunID: a.run.id, StepID: a.step, AttemptID: finished.AttemptID,
		ConfigRevision: a.run.configRevision, Config: cloneRuntimeConfig(a.run.config),
	}
	r.mu.Unlock()
	event := model.AttemptFinished{
		Identity: identity, Outcome: finished.Outcome, ProviderRequestID: finished.ProviderRequestID,
		Usage: finished.Usage, ErrorCode: finished.ErrorCode,
	}
	var observerErr error
	for _, observer := range r.components.attemptObservers {
		if err := observer.AttemptFinished(ctx, event); err != nil {
			observerErr = errors.Join(observerErr, fmt.Errorf("standardagent: record model attempt finish: %w", err))
		}
	}

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
		return errors.Join(observerErr, err)
	}
	fact := session.ModelAttemptFact{
		AttemptID: finished.AttemptID, RunID: a.run.id, StepID: a.step, Kind: kind,
		ProviderKey: a.run.config.ProviderKey, ModelID: a.run.config.ModelID,
		ProviderRequestID: finished.ProviderRequestID, Usage: finished.Usage, ErrorCode: finished.ErrorCode,
	}
	if _, err := r.commitLocked(context.Background(), snapshot.Revision, "attempt-finish", []session.Change{{Kind: session.AppendModelAttempt, ModelAttempt: &fact}}); err != nil {
		return errors.Join(observerErr, err)
	}
	a.run.usedTokens += finished.Usage.TotalTokens
	r.observeModelAttempt(trace, a.run, a.step, finished.AttemptID)
	r.components.observations.publishUsage(observe.UsageRecord{
		Kind: observe.UsageModel, At: time.Now().UTC(),
		Identity: observe.Identity{
			SessionID: r.id(), RunID: a.run.id, StepID: a.step, AttemptID: finished.AttemptID,
			Actor: serviceObservationActor("model-executor"),
		},
		ProviderKey: a.run.config.ProviderKey, ModelID: a.run.config.ModelID,
		InputTokens: finished.Usage.InputTokens, OutputTokens: finished.Usage.OutputTokens,
		CachedInputTokens: finished.Usage.CachedInputTokens, CacheWriteTokens: finished.Usage.CacheWriteTokens,
		ReasoningTokens: finished.Usage.ReasoningTokens, TotalTokens: finished.Usage.TotalTokens,
		Estimated: finished.Usage.Estimated, EstimateSource: finished.Usage.EstimateSource,
	})
	return observerErr
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

func (r *runtimeInstance) observeModelAttempt(kind observe.TraceKind, run *activeRun, step agent.StepID, attemptID agent.AttemptID) {
	r.components.observations.publishTrace(observe.TraceRecord{
		Kind: kind, At: time.Now().UTC(),
		Identity: observe.Identity{
			SessionID: r.id(), RunID: run.id, StepID: step, AttemptID: attemptID,
			Actor: serviceObservationActor("model-executor"),
		},
	})
}

func (r *runtimeInstance) commitCompletion(run *activeRun, step agent.StepID, messageID agent.MessageID, output model.Completion) ([]agent.ToolCall, bool, error) {
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
		ID: messageID, SessionID: r.id(), RunID: run.id, StepID: step,
		Role: agent.RoleAssistant, Parts: cloneRuntimeParts(output.Parts), CreatedAt: time.Now().UTC(),
	}
	if len(output.Continuation) > 0 {
		assistant.ModelContinuation = &agent.ModelContinuation{
			ProviderKey: run.config.ProviderKey, ModelID: run.config.ModelID,
			State: append(json.RawMessage(nil), output.Continuation...),
		}
	}
	changes := []session.Change{{Kind: session.AppendMessage, Message: &assistant}}
	calls := make([]agent.ToolCall, 0, len(output.ToolCalls))
	for _, requested := range output.ToolCalls {
		call := agent.ToolCall{
			ID: agent.ToolCallID(r.nextID("call")), MessageID: assistant.ID, SessionID: r.id(),
			RunID: run.id, StepID: step, CorrelationID: requested.CorrelationID,
			Name: requested.Name, Arguments: append([]byte(nil), requested.Arguments...),
		}
		prepared := session.JournalEntry{RunID: run.id, StepID: step, ToolCall: &call, Status: session.JournalPrepared}
		changes = append(changes,
			session.Change{Kind: session.AppendToolCall, ToolCall: &call},
			session.Change{Kind: session.UpdateRunJournal, Journal: &prepared},
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
	steers := pendingByDelivery(snapshot.Queue, session.DeliverySteer)
	if len(steers) == 0 {
		goalProposals, goalHandled, goalErr := r.evaluateGoalCompletion(run, snapshot)
		if goalErr != nil {
			return "", false, false, goalErr
		}
		proposals = append(proposals, goalProposals...)
		if !goalHandled {
			view := hook.RunCompleteView{
				SessionID: r.id(), RunID: run.id, Revision: snapshot.Revision,
				ModelConfig: cloneRuntimeConfig(run.config), Messages: cloneHookMessages(snapshot.History),
			}
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
	steers = pendingByDelivery(snapshot.Queue, session.DeliverySteer)
	if len(steers) > 0 {
		// A user steer arriving during Goal/Hook evaluation takes precedence over
		// autonomous continuation proposed from an older Session revision.
		proposals = nil
	}
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

func (r *runtimeInstance) evaluateGoalCompletion(run *activeRun, snapshot session.Snapshot) ([]agent.MessageInput, bool, error) {
	if r.components.goalStore == nil || r.components.goalEvaluator == nil {
		return nil, false, nil
	}
	current, ok, err := r.components.goalStore.Current(run.ctx, r.id())
	if err != nil {
		return nil, true, fmt.Errorf("standardagent: load current goal: %w", err)
	}
	if !ok || current.Status != goal.StatusActive {
		return nil, false, nil
	}
	evaluationStep := goalEvaluationStep(snapshot, run.id)
	record := goal.DecisionRecord{
		ID:     fmt.Sprintf("goal-decision-%s-%s-%d", run.id, evaluationStep, current.Version),
		GoalID: current.ID, SessionID: r.id(), RunID: run.id,
		StepID: evaluationStep, ExpectedVersion: current.Version, RecordedAt: time.Now().UTC(),
	}
	if current.FollowOns >= current.MaxFollowOns {
		record.Evaluation = goal.Evaluation{Decision: goal.DecisionBlocked, Reason: goal.ReasonFollowOnLimit}
	} else {
		recorder := &runtimeAttemptRecorder{runtime: r, run: run, step: record.StepID}
		record.Evaluation, err = r.components.goalEvaluator.Evaluate(run.ctx, goal.EvaluationRequest{
			Goal: current, RunID: run.id, StepID: record.StepID, Revision: snapshot.Revision,
			ModelConfig: cloneRuntimeConfig(run.config), Messages: cloneHookMessages(snapshot.History),
		}, recorder)
		if err != nil || record.Evaluation.Validate() != nil {
			record.Evaluation = goal.Evaluation{Decision: goal.DecisionBlocked, Reason: goal.ReasonEvaluatorFailure}
		}
	}
	latest, err := r.session.View(run.ctx)
	if err != nil {
		return nil, true, fmt.Errorf("standardagent: verify goal decision revision: %w", err)
	}
	if len(pendingByDelivery(latest.Queue, session.DeliverySteer)) > 0 {
		// This View is the linearization point for completion evaluation. A user
		// steer already accepted while the Evaluator was running invalidates the
		// stale decision; the caller will consume that steer as the next step.
		return nil, true, nil
	}
	if _, err := r.components.goalStore.RecordDecision(context.WithoutCancel(run.ctx), record); err != nil {
		if errors.Is(err, goal.ErrVersionConflict) {
			return nil, true, nil
		}
		return nil, true, fmt.Errorf("standardagent: record goal decision: %w", err)
	}
	if record.Evaluation.Decision != goal.DecisionContinue {
		return nil, true, nil
	}
	return []agent.MessageInput{{Parts: cloneRuntimeParts(record.Evaluation.NextInstruction.Parts)}}, true, nil
}

func goalEvaluationStep(snapshot session.Snapshot, runID agent.RunID) agent.StepID {
	for index := len(snapshot.History) - 1; index >= 0; index-- {
		fact := snapshot.History[index]
		if fact.Message != nil && fact.Message.RunID == runID && fact.Message.StepID.Valid() {
			return fact.Message.StepID
		}
	}
	return agent.StepID("goal-evaluation")
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
	case stepBudgetExceeded, stepWaiting:
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
	actor := agent.ActorIdentity{}
	if terminalKind == session.RunCanceled && run.controlActor.Valid() {
		actor = run.controlActor
	} else if terminalKind == session.RunCompleted {
		actor = agent.ActorIdentity{Kind: agent.ActorAgent, ID: string(snapshot.Session.AgentID)}
	}
	if _, err := r.commitLockedAs(context.Background(), snapshot.Revision, "run-finish", actor, changes); err != nil {
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
		r.commitObserver.stop()
		r.events.close()
		r.components.observations.publishTrace(observe.TraceRecord{
			Kind: observe.TraceRuntimeClosed, At: time.Now().UTC(),
			Identity: observe.Identity{SessionID: r.id(), Actor: serviceObservationActor("agent-runtime")},
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
	return r.commitLockedAs(ctx, expected, operation, agent.ActorIdentity{}, changes)
}

func (r *runtimeInstance) commitExternalLocked(ctx context.Context, expected agent.Revision, operation string, actor agent.ActorIdentity, changes []session.Change) (session.Commit, error) {
	commit, err := r.commitLockedAs(ctx, expected, operation, actor, changes)
	if err == nil || !agent.IsCode(err, agent.CodeRevisionConflict) {
		return commit, err
	}
	current := r.revision()
	if snapshot, viewErr := r.session.View(ctx); viewErr == nil {
		current = snapshot.Revision
		r.revisionValue.Store(uint64(current))
	}
	return session.Commit{}, &interaction.RevisionConflictError{CurrentRevision: current, SnapshotRequired: true, Cause: err}
}

func (r *runtimeInstance) commitLockedAs(ctx context.Context, expected agent.Revision, operation string, actor agent.ActorIdentity, changes []session.Change) (session.Commit, error) {
	commit, err := r.components.store.Commit(ctx, session.CommitRequest{
		SessionID: r.id(), ExpectedRevision: expected,
		IdempotencyKey: fmt.Sprintf("runtime-%s-%s", operation, r.nextID("commit")), Actor: actor, Changes: changes,
	})
	if err != nil {
		return session.Commit{}, err
	}
	r.revisionValue.Store(uint64(commit.Revision))
	if commit.Applied {
		r.commitObserver.publish(session.CommitNotice{
			SessionID: r.id(), Revision: commit.Revision,
			FirstHistorySequence: commit.FirstHistorySequence, LastHistorySequence: commit.LastHistorySequence,
		})
		r.publishCommitEvent(commit.Revision)
		r.publishCommitObservations(changes, actor)
	}
	return commit, nil
}

func (r *runtimeInstance) publishCommitObservations(changes []session.Change, actor agent.ActorIdentity) {
	now := time.Now().UTC()
	actor = normalizedObservationActor(actor)
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
			identity := observe.Identity{SessionID: r.id(), RunID: change.RunFact.RunID, Actor: actor}
			r.components.observations.publishTrace(observe.TraceRecord{Kind: kind, At: now, Identity: identity})
			if change.RunFact.Kind != session.RunStarted {
				r.components.observations.publishMetric(observe.MetricRecord{
					Name: observe.MetricRunTotal, Kind: observe.MetricCounter, Value: 1, At: now,
					Identity:   identity,
					Attributes: map[string]string{"outcome": outcome},
				})
			}
		}
		if change.SessionEvent != nil && change.SessionEvent.Kind == session.EventModelConfigChanged {
			current := change.SessionEvent.ModelConfigChanged.Current
			r.components.observations.publishAudit(observe.AuditRecord{
				Kind: observe.AuditModelConfigChanged, At: now,
				Identity: observe.Identity{SessionID: r.id(), Actor: actor},
				Action:   current.ProviderKey + "/" + current.ModelID, Decision: "committed",
			})
		}
	}
}

func (r *runtimeInstance) publishCommitEvent(revision agent.Revision) {
	r.events.publish(interaction.Event{Kind: interaction.EventRevision, SessionID: r.id(), Revision: revision})
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
			message := cloneRuntimeMessage(*fact.Message)
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

func cloneRuntimeMessage(source agent.Message) agent.Message {
	copy := source
	copy.Parts = cloneRuntimeParts(source.Parts)
	if source.ModelContinuation != nil {
		continuation := *source.ModelContinuation
		continuation.State = append(json.RawMessage(nil), source.ModelContinuation.State...)
		copy.ModelContinuation = &continuation
	}
	return copy
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
	copy.Artifacts = append(source.Artifacts[:0:0], source.Artifacts...)
	if source.Error != nil {
		errorCopy := *source.Error
		copy.Error = &errorCopy
	}
	return copy
}
