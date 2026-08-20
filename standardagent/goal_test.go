package standardagent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/goal"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestGoalEvaluatorContinuesSameRunUntilStructuredDoneDecision(t *testing.T) {
	executor := newRound7Executor(nil, nil,
		model.FakeExecution{Events: []model.ModelEvent{complete("first result")}},
		model.FakeExecution{Events: []model.ModelEvent{complete("final result")}},
	)
	store := goal.NewMemoryStore()
	evaluator := &goalEvaluatorSequence{evaluations: []goal.Evaluation{
		{Decision: goal.DecisionContinue, Reason: goal.ReasonProgressPossible,
			NextInstruction: textInput("continue toward the goal")},
		{Decision: goal.DecisionDone, Reason: goal.ReasonObjectiveMet},
	}}
	access, _, stop := startRound7Application(t, executor, AgentRuntimeConfig{},
		goalStoreModule{store: store}, goalEvaluatorModule{evaluator: evaluator})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := store.Set(context.Background(), goal.SetRequest{
		SessionID: opened.SessionID, Objective: "produce the complete result", MaxFollowOns: 3,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("begin"),
	}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	requests := executor.fake.Requests()
	if len(requests) != 2 || requests[0].RunID != requests[1].RunID {
		t.Fatalf("goal continuation requests = %#v", requests)
	}
	last := requests[1].Inputs[len(requests[1].Inputs)-1]
	if last.Message == nil || last.Message.Parts[0].Text != "continue toward the goal" {
		t.Fatalf("goal follow-on input = %#v", last)
	}
	current, ok, err := store.Current(context.Background(), opened.SessionID)
	if err != nil || !ok || current.Status != goal.StatusCompleted || current.FollowOns != 1 {
		t.Fatalf("goal state = %#v, %v, %v", current, ok, err)
	}
}

func TestGoalComponentsMustBeInstalledAsAPair(t *testing.T) {
	store := goal.NewMemoryStore()
	app := NewApplication(ApplicationSpec{
		Name: "goal-pair", Modules: []agentslot.Module{
			executorModule{executor: newRound7Executor(nil, nil)}, session.NewMemoryModule(), goalStoreModule{store: store},
			NewGatewayChannelModule("channel.goal-pair", "goal-pair", &captureChannel{}),
		},
		DefaultModelConfig: model.Config{ModelID: "default", Reasoning: model.ReasoningDefault},
	})
	if _, err := app.Build(); err == nil {
		t.Fatal("Build accepted goal.store without goal.evaluator")
	}
}

func TestUserSteerDuringGoalEvaluationDiscardsTheStaleDecision(t *testing.T) {
	executor := newRound7Executor(nil, nil,
		model.FakeExecution{Events: []model.ModelEvent{complete("first result")}},
		model.FakeExecution{Events: []model.ModelEvent{complete("steered result")}},
	)
	store := goal.NewMemoryStore()
	evaluator := newBlockingGoalEvaluator()
	access, _, stop := startRound7Application(t, executor, AgentRuntimeConfig{},
		goalStoreModule{store: store}, goalEvaluatorModule{evaluator: evaluator})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	initial, err := store.Set(context.Background(), goal.SetRequest{
		SessionID: opened.SessionID, Objective: "finish after considering user steer", MaxFollowOns: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("begin"),
	}); err != nil {
		t.Fatal(err)
	}
	evaluator.waitStarted(t, 0)
	view, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := access.Steer(context.Background(), interaction.SteerRequest{
		SessionID: opened.SessionID, ExpectedRevision: view.Revision, Input: textInput("use this correction"),
	}); err != nil {
		t.Fatal(err)
	}
	evaluator.release(0)
	evaluator.waitStarted(t, 1)
	current, ok, err := store.Current(context.Background(), opened.SessionID)
	if err != nil || !ok {
		t.Fatalf("current goal = %#v, %v, %v", current, ok, err)
	}
	if current.Status != goal.StatusActive || current.Version != initial.Version {
		t.Fatalf("stale evaluation changed goal before steer completed: %#v", current)
	}
	evaluator.release(1)
	waitRuntimeIdle(t, access, opened.SessionID)
}

func TestGoalEvaluatorFailurePausesInsteadOfGuessingCompletion(t *testing.T) {
	executor := newRound7Executor(nil, nil, model.FakeExecution{Events: []model.ModelEvent{complete("partial result")}})
	store := goal.NewMemoryStore()
	evaluator := goalEvaluatorFunc(func(context.Context, goal.EvaluationRequest, model.AttemptRecorder) (goal.Evaluation, error) {
		return goal.Evaluation{}, errors.New("provider unavailable")
	})
	access, _, stop := startRound7Application(t, executor, AgentRuntimeConfig{},
		goalStoreModule{store: store}, goalEvaluatorModule{evaluator: evaluator})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := store.Set(context.Background(), goal.SetRequest{
		SessionID: opened.SessionID, Objective: "complete safely", MaxFollowOns: 3,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("begin"),
	}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	current, ok, err := store.Current(context.Background(), opened.SessionID)
	if err != nil || !ok || current.Status != goal.StatusPaused {
		t.Fatalf("goal after evaluator failure = %#v, %v, %v", current, ok, err)
	}
}

type goalStoreModule struct{ store goal.Store }

func (goalStoreModule) ID() string { return "goal.store.test" }
func (m goalStoreModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Set(goal.StoreSlot, m.store))
}

type goalEvaluatorModule struct{ evaluator goal.Evaluator }

func (goalEvaluatorModule) ID() string { return "goal.evaluator.test" }
func (m goalEvaluatorModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Set(goal.EvaluatorSlot, m.evaluator))
}

type goalEvaluatorSequence struct {
	mu          sync.Mutex
	evaluations []goal.Evaluation
}

type goalEvaluatorFunc func(context.Context, goal.EvaluationRequest, model.AttemptRecorder) (goal.Evaluation, error)

func (f goalEvaluatorFunc) Evaluate(ctx context.Context, request goal.EvaluationRequest, recorder model.AttemptRecorder) (goal.Evaluation, error) {
	return f(ctx, request, recorder)
}

type blockingGoalEvaluator struct {
	mu      sync.Mutex
	calls   int
	started [2]chan struct{}
	proceed [2]chan struct{}
}

func newBlockingGoalEvaluator() *blockingGoalEvaluator {
	e := &blockingGoalEvaluator{}
	for index := range e.started {
		e.started[index] = make(chan struct{})
		e.proceed[index] = make(chan struct{})
	}
	return e
}

func (e *blockingGoalEvaluator) Evaluate(context.Context, goal.EvaluationRequest, model.AttemptRecorder) (goal.Evaluation, error) {
	e.mu.Lock()
	index := e.calls
	e.calls++
	e.mu.Unlock()
	if index >= len(e.started) {
		return goal.Evaluation{Decision: goal.DecisionDone, Reason: goal.ReasonObjectiveMet}, nil
	}
	close(e.started[index])
	<-e.proceed[index]
	return goal.Evaluation{Decision: goal.DecisionDone, Reason: goal.ReasonObjectiveMet}, nil
}

func (e *blockingGoalEvaluator) waitStarted(t *testing.T, index int) {
	t.Helper()
	select {
	case <-e.started[index]:
	case <-time.After(time.Second):
		t.Fatalf("goal evaluation %d did not start", index+1)
	}
}

func (e *blockingGoalEvaluator) release(index int) { close(e.proceed[index]) }

func (e *goalEvaluatorSequence) Evaluate(_ context.Context, _ goal.EvaluationRequest, _ model.AttemptRecorder) (goal.Evaluation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.evaluations) == 0 {
		return goal.Evaluation{Decision: goal.DecisionDone, Reason: goal.ReasonObjectiveMet}, nil
	}
	result := e.evaluations[0]
	e.evaluations = e.evaluations[1:]
	return result, nil
}

var _ goal.Evaluator = (*goalEvaluatorSequence)(nil)
var _ goal.Store = (*goal.MemoryStore)(nil)
