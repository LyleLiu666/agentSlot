package interaction_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
)

func TestModelCommandQueriesCurrentSelectionAndAvailableModels(t *testing.T) {
	command := installedModelCommand(t)
	actions := &modelCommandActions{
		current: interaction.ModelConfigView{
			SessionID: "session-1",
			Revision:  7,
			Config: model.Config{
				ProviderKey: "provider-a",
				ModelID:     "model-a",
				Reasoning:   model.ReasoningMedium,
			},
		},
		models: []model.Descriptor{{
			ProviderKey: "provider-b",
			ModelID:     "model-b",
			Title:       "Model B",
			Capabilities: model.ExecutionCapabilities{
				Media: model.Capabilities{
					InputModalities:  []model.Modality{model.ModalityText, model.ModalityImage},
					OutputModalities: []model.Modality{model.ModalityText},
				},
				Reasoning:           []model.Reasoning{model.ReasoningDefault, model.ReasoningHigh},
				ContextWindowTokens: 128_000,
				MaxOutputTokens:     8_192,
			},
		}},
	}

	result, err := command.Invoke(context.Background(), interaction.CommandInvocation{
		Key:   interaction.ModelCommandKey,
		Scope: interaction.CommandScope{SessionID: "session-1"},
	}, actions)
	if err != nil {
		t.Fatalf("Invoke query: %v", err)
	}
	var data interaction.ModelCommandData
	if err := json.Unmarshal(result.Data, &data); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.Revision != 7 || data.Current.ProviderKey != "provider-a" || data.Current.ModelID != "model-a" || data.Current.Reasoning != model.ReasoningMedium {
		t.Fatalf("current selection = %#v, result revision %d", data.Current, result.Revision)
	}
	if len(data.Models) != 1 || data.Models[0].ProviderKey != "provider-b" || data.Models[0].ModelID != "model-b" || data.Models[0].ContextWindowTokens != 128_000 {
		t.Fatalf("available models = %#v", data.Models)
	}
	if !strings.Contains(string(result.Data), `"input_modalities":["text","image"]`) ||
		!strings.Contains(string(result.Data), `"output_modalities":["text"]`) {
		t.Fatalf("model command modality JSON = %s", result.Data)
	}
	if len(actions.applied) != 0 {
		t.Fatalf("query unexpectedly applied actions: %#v", actions.applied)
	}
}

func TestModelCommandDescribesTheCompletePortableReasoningVocabulary(t *testing.T) {
	command := installedModelCommand(t)
	descriptor := command.Describe()
	for _, field := range descriptor.Fields {
		if field.Key != "reasoning" {
			continue
		}
		values := make([]string, 0, len(field.Choices))
		for _, choice := range field.Choices {
			values = append(values, choice.Value)
		}
		if !reflect.DeepEqual(values, []string{"default", "low", "medium", "high", "xhigh", "max"}) {
			t.Fatalf("reasoning choices = %#v", values)
		}
		return
	}
	t.Fatal("reasoning field is missing")
}

func TestModelCommandUpdatesThroughBoundActionAndReturnsCommittedState(t *testing.T) {
	command := installedModelCommand(t)
	actions := &modelCommandActions{current: interaction.ModelConfigView{
		SessionID: "session-1",
		Revision:  3,
		Config:    model.Config{ProviderKey: "provider-a", ModelID: "model-a", Reasoning: model.ReasoningDefault},
	}}
	temperature := 0.4
	maxTokens := 2048
	arguments, err := json.Marshal(interaction.ModelCommandArguments{
		ProviderKey:             "provider-b",
		ModelID:                 "model-b",
		Reasoning:               model.ReasoningHigh,
		Temperature:             &temperature,
		MaxTokens:               &maxTokens,
		AcceptCompatibilityLoss: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := command.Invoke(context.Background(), interaction.CommandInvocation{
		Key:              interaction.ModelCommandKey,
		Scope:            interaction.CommandScope{SessionID: "session-1"},
		ExpectedRevision: 3,
		Arguments:        arguments,
	}, actions)
	if err != nil {
		t.Fatalf("Invoke update: %v", err)
	}
	if len(actions.applied) != 1 {
		t.Fatalf("actions = %#v", actions.applied)
	}
	wantAction := interaction.ActionRequest{
		Kind:             interaction.ActionUpdateModelConfig,
		ExpectedRevision: 3,
		Config: model.Config{
			ProviderKey: "provider-b", ModelID: "model-b", Reasoning: model.ReasoningHigh,
			Parameters: model.Parameters{Temperature: &temperature, MaxTokens: &maxTokens},
		},
		AcceptCompatibilityLoss: true,
	}
	if !reflect.DeepEqual(actions.applied[0], wantAction) {
		t.Fatalf("action = %#v, want %#v", actions.applied[0], wantAction)
	}
	var data interaction.ModelCommandData
	if err := json.Unmarshal(result.Data, &data); err != nil {
		t.Fatal(err)
	}
	if result.Revision != 4 || data.Current.ProviderKey != "provider-b" || data.Current.ModelID != "model-b" {
		t.Fatalf("committed result = %#v, revision %d", data, result.Revision)
	}
}

func TestModelCommandRejectsUnknownOrIncompleteArguments(t *testing.T) {
	command := installedModelCommand(t)
	actions := &modelCommandActions{current: interaction.ModelConfigView{Revision: 1}}
	for _, arguments := range []json.RawMessage{
		json.RawMessage(`{"unknown":true}`),
		json.RawMessage(`{"provider_key":"provider"}`),
		json.RawMessage(`{"provider_key":"provider","model_id":"model","reasoning":"not-real"}`),
		json.RawMessage(`{} {}`),
	} {
		if _, err := command.Invoke(context.Background(), interaction.CommandInvocation{
			Key: interaction.ModelCommandKey, Arguments: arguments,
		}, actions); err == nil {
			t.Fatalf("arguments %s were accepted", arguments)
		}
	}
	if len(actions.applied) != 0 {
		t.Fatalf("invalid arguments applied actions: %#v", actions.applied)
	}
}

func installedModelCommand(t *testing.T) interaction.InteractionCommand {
	t.Helper()
	builder := agentslot.NewBuilder()
	if err := builder.Install(interaction.NewModelCommandModule()); err != nil {
		t.Fatalf("install model command: %v", err)
	}
	assembly, err := builder.Build(agentslot.RequireKey(interaction.CommandSlot, interaction.ModelCommandKey))
	if err != nil {
		t.Fatalf("build model command: %v", err)
	}
	command, ok := agentslot.Lookup(assembly, interaction.CommandSlot, interaction.ModelCommandKey)
	if !ok {
		t.Fatal("model command missing")
	}
	return command
}

type modelCommandActions struct {
	current interaction.ModelConfigView
	models  []model.Descriptor
	applied []interaction.ActionRequest
	err     error
}

func (a *modelCommandActions) CurrentModelConfig(context.Context) (interaction.ModelConfigView, error) {
	if a.err != nil {
		return interaction.ModelConfigView{}, a.err
	}
	return a.current, nil
}

func (a *modelCommandActions) AvailableModels(context.Context) ([]model.Descriptor, error) {
	if a.err != nil {
		return nil, a.err
	}
	return append([]model.Descriptor(nil), a.models...), nil
}

func (a *modelCommandActions) Apply(_ context.Context, request interaction.ActionRequest) (interaction.ActionResult, error) {
	if a.err != nil {
		return interaction.ActionResult{}, a.err
	}
	a.applied = append(a.applied, request)
	if request.Kind == interaction.ActionUpdateModelConfig {
		a.current.Config = request.Config
		a.current.Revision++
	}
	return interaction.ActionResult{Revision: a.current.Revision}, nil
}

var _ interaction.CommandActions = (*modelCommandActions)(nil)
