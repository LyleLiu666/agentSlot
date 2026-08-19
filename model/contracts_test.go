package model_test

import (
	"context"
	"errors"
	"math"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
)

type executor struct{}

func (executor) Execute(context.Context, model.ModelRequest) (model.ModelStream, error) {
	return nil, nil
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
	message := &agent.Message{
		ID: "message-1", SessionID: "session-1", RunID: "run-1", StepID: "step-1", Role: agent.RoleAssistant,
		Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "complete"}},
	}
	valid := []model.ModelEvent{
		{Kind: model.EventDelta, Text: "partial"},
		{Kind: model.EventReset},
		{Kind: model.EventComplete, Message: message},
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
