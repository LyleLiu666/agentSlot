package model_test

import (
	"context"
	"errors"
	"math"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/tool"
)

type executor struct{}

func (executor) Execute(context.Context, model.ModelRequest) (model.ModelStream, error) {
	return nil, nil
}
func (executor) Inspect(context.Context, model.Config) (model.ExecutionCapabilities, error) {
	return testCapabilities(), nil
}
func (executor) CountTokens(context.Context, model.ModelRequest) (int, error) { return 0, nil }

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

func TestFakeModelExecutorScriptsAndDetachesRequests(t *testing.T) {
	output := &model.Completion{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "done"}}}
	fake := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{
		{Kind: model.EventDelta, Text: "do"},
		{Kind: model.EventComplete, Output: output},
	}})
	request := model.ModelRequest{
		SessionID: "session-1", RunID: "run-1", StepID: "step-1",
		Config: model.Config{ModelID: "model-1", Reasoning: model.ReasoningDefault},
		Inputs: []model.Input{{Message: &agent.Message{ID: "message-1", SessionID: "session-1", Role: agent.RoleUser, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "hello"}}}}},
	}
	stream, err := fake.Execute(context.Background(), request)
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
}

func TestReasoningVocabularyIsClosed(t *testing.T) {
	if !model.ReasoningHigh.Valid() || model.Reasoning("vendor-secret-mode").Valid() {
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
