package model_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/tool"
)

type executor struct{}

func (executor) Execute(context.Context, model.ModelRequest, model.AttemptRecorder) (model.ModelStream, error) {
	return nil, nil
}
func (executor) Inspect(context.Context, model.Config) (model.ExecutionCapabilities, error) {
	return testCapabilities(), nil
}

type counter struct{}

func (counter) CountTokens(context.Context, model.ModelRequest) (int, error) { return 17, nil }

type attemptRecorder struct {
	started  []model.AttemptStart
	finished []model.AttemptFinish
	budget   model.TokenBudget
}

func (r *attemptRecorder) Started(_ context.Context, value model.AttemptStart) error {
	r.started = append(r.started, value)
	return nil
}
func (r *attemptRecorder) Finished(_ context.Context, value model.AttemptFinish) error {
	r.finished = append(r.finished, value)
	r.budget.UsedTokens += value.Usage.TotalTokens
	return nil
}
func (r *attemptRecorder) Budget() model.TokenBudget { return r.budget }

func testCapabilities() model.ExecutionCapabilities {
	return model.ExecutionCapabilities{
		Media:     model.Capabilities{InputModalities: []model.Modality{model.ModalityText}, OutputModalities: []model.Modality{model.ModalityText}},
		Reasoning: []model.Reasoning{model.ReasoningDefault}, ContextWindowTokens: 1000, MaxOutputTokens: 100,
	}
}

func TestRequiredModelExecutorRejectsMissingProvider(t *testing.T) {
	_, err := agentslot.NewBuilder().Build(agentslot.RequireOne(model.ExecutorSlot))
	if !errors.Is(err, agentslot.ErrRequirementUnsatisfied) {
		t.Fatalf("Build() error = %v, want ErrRequirementUnsatisfied", err)
	}
}

type module struct{}

func (module) ID() string { return "model.contracts" }
func (module) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Set(model.ExecutorSlot, model.ModelExecutor(executor{})))
}

func TestModelExecutorIsOneTypedSlot(t *testing.T) {
	builder := agentslot.NewBuilder()
	if err := builder.Install(module{}); err != nil {
		t.Fatalf("install: %v", err)
	}
	assembly, err := builder.Build(agentslot.RequireOne(model.ExecutorSlot))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, ok := agentslot.Get(assembly, model.ExecutorSlot); !ok {
		t.Fatal("model.executor contribution missing")
	}
}

type counterModule struct{}

func (counterModule) ID() string { return "model.token-counter" }
func (counterModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Set(model.TokenCounterSlot, model.TokenCounter(counter{})))
}

func TestTokenCounterIsIndependentOneTypedSlot(t *testing.T) {
	builder := agentslot.NewBuilder()
	if err := builder.Install(counterModule{}); err != nil {
		t.Fatalf("install: %v", err)
	}
	assembly, err := builder.Build(agentslot.RequireOne(model.TokenCounterSlot))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	resolved, ok := agentslot.Get(assembly, model.TokenCounterSlot)
	if !ok {
		t.Fatal("model.token-counter contribution missing")
	}
	if got, err := resolved.CountTokens(context.Background(), model.ModelRequest{}); err != nil || got != 17 {
		t.Fatalf("CountTokens() = %d, %v; want 17", got, err)
	}
}

func TestNewTokenCounterModuleValidatesAndContributesExplicitCounter(t *testing.T) {
	if _, err := model.NewTokenCounterModule("", counter{}); err == nil {
		t.Fatal("empty module ID accepted")
	}
	if _, err := model.NewTokenCounterModule("counter", nil); err == nil {
		t.Fatal("nil TokenCounter accepted")
	}
	module, err := model.NewTokenCounterModule("counter", counter{})
	if err != nil {
		t.Fatal(err)
	}
	builder := agentslot.NewBuilder()
	if err := builder.Install(module); err != nil {
		t.Fatal(err)
	}
	assembly, err := builder.Build(agentslot.RequireOne(model.TokenCounterSlot))
	if err != nil {
		t.Fatal(err)
	}
	resolved, ok := agentslot.Get(assembly, model.TokenCounterSlot)
	if !ok {
		t.Fatal("TokenCounter missing")
	}
	if got, err := resolved.CountTokens(context.Background(), model.ModelRequest{}); err != nil || got != 17 {
		t.Fatalf("CountTokens() = %d, %v; want 17", got, err)
	}
}

func TestModelEventsSeparateTemporaryAndTerminalFacts(t *testing.T) {
	output := &model.Completion{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "complete"}}}
	valid := []model.ModelEvent{
		{Kind: model.EventDelta, Text: "partial"},
		{Kind: model.EventReset},
		{Kind: model.EventComplete, Output: output},
		{Kind: model.EventFailed, Err: errors.New("provider failed")},
	}
	for _, event := range valid {
		if err := event.Validate(); err != nil {
			t.Fatalf("valid event rejected: %v", err)
		}
	}
	if err := (model.ModelEvent{Kind: model.EventComplete}).Validate(); err == nil {
		t.Fatal("complete event without message accepted")
	}
	if err := (model.ModelEvent{Kind: model.EventFailed}).Validate(); err == nil {
		t.Fatal("failed event without error accepted")
	}
	if err := (model.ModelEvent{Kind: model.EventDelta}).Validate(); err == nil {
		t.Fatal("empty delta accepted")
	}
	if err := (model.ModelEvent{Kind: model.EventReset, Text: "stale"}).Validate(); err == nil {
		t.Fatal("reset event with text accepted")
	}
	if err := (model.AttemptFinish{AttemptID: "attempt-1", Outcome: model.AttemptSucceeded, Usage: model.TokenUsage{InputTokens: 3, OutputTokens: 2, TotalTokens: 4}}).Validate(); err == nil {
		t.Fatal("inconsistent attempt usage accepted")
	}
}

func TestAttemptRecorderVocabularyAndTokenBudget(t *testing.T) {
	start := model.AttemptStart{AttemptID: "attempt-1", ProviderKey: "provider", ModelID: "model"}
	finish := model.AttemptFinish{
		AttemptID: "attempt-1", Outcome: model.AttemptFailed, ErrorCode: "transport",
		Usage: model.TokenUsage{InputTokens: 4, OutputTokens: 1, TotalTokens: 5, Estimated: true, EstimateSource: "adapter"},
	}
	if err := start.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := finish.Validate(); err != nil {
		t.Fatal(err)
	}
	missingCode := finish
	missingCode.ErrorCode = ""
	if err := missingCode.Validate(); err == nil {
		t.Fatal("failed attempt without a safe error code was accepted")
	}
	budget := model.TokenBudget{MaxTokens: 5, UsedTokens: 5}
	if err := budget.Validate(); err != nil || !budget.Exhausted() || budget.RemainingTokens() != 0 {
		t.Fatalf("budget = %#v, err = %v", budget, err)
	}
}

func TestCompletionAllowsIdentityFreeToolOnlyResult(t *testing.T) {
	completion := model.Completion{ToolCalls: []model.ToolCallRequest{{Name: "lookup", Arguments: []byte(`{"q":"x"}`)}}}
	if !completion.Valid() {
		t.Fatal("tool-only completion rejected")
	}
	if (&agent.Message{ID: "message-1", SessionID: "session-1", RunID: "run-1", StepID: "step-1", Role: agent.RoleAssistant}).Valid() != true {
		t.Fatal("tool-call parent assistant message cannot be content-empty")
	}
	if (model.Completion{ToolCalls: []model.ToolCallRequest{{Name: "lookup", Arguments: []byte(`not-json`)}}}).Valid() {
		t.Fatal("tool request with invalid JSON accepted")
	}
}

func TestCompletionCarriesOpaqueModelContinuationWithoutInterpretingIt(t *testing.T) {
	completion := model.Completion{
		ToolCalls:    []model.ToolCallRequest{{Name: "lookup", Arguments: []byte(`{"q":"x"}`)}},
		Continuation: json.RawMessage(`[{"type":"opaque","signature":"provider-owned"}]`),
	}
	if !completion.Valid() {
		t.Fatal("valid opaque continuation was rejected")
	}
	completion.Continuation = json.RawMessage(`not-json`)
	if completion.Valid() {
		t.Fatal("invalid opaque continuation was accepted")
	}
}

func TestFakeModelExecutorScriptsAndDetachesRequests(t *testing.T) {
	output := &model.Completion{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "done"}}}
	fake := model.NewFakeModelExecutor(model.FakeExecution{Usage: model.TokenUsage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3}, Events: []model.ModelEvent{
		{Kind: model.EventDelta, Text: "do"},
		{Kind: model.EventComplete, Output: output},
	}})
	request := model.ModelRequest{
		SessionID: "session-1", RunID: "run-1", StepID: "step-1",
		Config: model.Config{ModelID: "model-1", Reasoning: model.ReasoningDefault},
		Inputs: []model.Input{{Message: &agent.Message{ID: "message-1", SessionID: "session-1", Role: agent.RoleUser, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "hello"}}}}},
	}
	recorder := &attemptRecorder{}
	stream, err := fake.Execute(context.Background(), request, recorder)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if event, err := stream.Recv(context.Background()); err != nil || event.Kind != model.EventDelta {
		t.Fatalf("first Recv = %#v, %v", event, err)
	}
	if event, err := stream.Recv(context.Background()); err != nil || event.Kind != model.EventComplete || event.Output.Parts[0].Text != "done" {
		t.Fatalf("second Recv = %#v, %v", event, err)
	}
	if _, err := stream.Recv(context.Background()); !errors.Is(err, model.ErrStreamClosed) {
		t.Fatalf("terminal Recv error = %v, want ErrStreamClosed", err)
	}
	request.Inputs[0].Message.Parts[0].Text = "mutated"
	requests := fake.Requests()
	if requests[0].Inputs[0].Message.Parts[0].Text != "hello" {
		t.Fatalf("captured request was aliased: %#v", requests[0])
	}
	if len(recorder.started) != 1 || len(recorder.finished) != 1 || recorder.finished[0].Outcome != model.AttemptSucceeded || recorder.finished[0].Usage.TotalTokens != 3 {
		t.Fatalf("attempt records = %#v / %#v", recorder.started, recorder.finished)
	}
}

func TestReasoningVocabularyIsClosed(t *testing.T) {
	for _, reasoning := range []model.Reasoning{
		model.ReasoningDefault,
		model.ReasoningLow,
		model.ReasoningMedium,
		model.ReasoningHigh,
		model.ReasoningXHigh,
		model.ReasoningMax,
	} {
		if !reasoning.Valid() {
			t.Fatalf("portable reasoning %q is invalid", reasoning)
		}
	}
	if model.Reasoning("vendor-secret-mode").Valid() {
		t.Fatal("reasoning vocabulary is not closed")
	}
}

func TestModelConfigValidatesPortableParameters(t *testing.T) {
	valid := model.Config{ModelID: "model-1", Reasoning: model.ReasoningDefault}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid model config rejected: %v", err)
	}
	invalid := valid
	invalid.ModelID = ""
	if err := invalid.Validate(); err == nil {
		t.Fatal("model config without model ID accepted")
	}
	nan := math.NaN()
	invalid = valid
	invalid.Parameters.Temperature = &nan
	if err := invalid.Validate(); err == nil {
		t.Fatal("model config with non-finite temperature accepted")
	}
}

func TestValidateInputsRequiresOneLeadingSystemPromptAndContiguousToolExchange(t *testing.T) {
	system := "system"
	user := &agent.Message{ID: "user-1", SessionID: "session-1", Role: agent.RoleUser, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "hello"}}}
	assistant := &agent.Message{ID: "assistant-1", SessionID: "session-1", RunID: "run-1", StepID: "step-1", Role: agent.RoleAssistant}
	call := &agent.ToolCall{ID: "call-1", MessageID: assistant.ID, SessionID: assistant.SessionID, RunID: assistant.RunID, StepID: assistant.StepID, Name: "lookup", Arguments: []byte(`{}`)}
	result := &tool.ToolResult{CallID: call.ID, Status: tool.ResultSucceeded}
	valid := []model.Input{{SystemPrompt: &system}, {Message: user}, {Message: assistant}, {ToolCall: call}, {ToolResult: result}}
	if err := model.ValidateInputs(valid); err != nil {
		t.Fatalf("valid protocol rejected: %v", err)
	}
	lateSystem := append([]model.Input{{Message: user}}, model.Input{SystemPrompt: &system})
	if err := model.ValidateInputs(lateSystem); err == nil {
		t.Fatal("late SystemPrompt was accepted")
	}
	interrupted := []model.Input{{Message: assistant}, {ToolCall: call}, {Message: user}, {ToolResult: result}}
	if err := model.ValidateInputs(interrupted); err == nil {
		t.Fatal("non-contiguous tool exchange was accepted")
	}
}
