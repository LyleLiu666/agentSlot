package interaction

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
)

const ModelCommandKey = "model"

// ModelCommandArguments is the UI-neutral input to the optional built-in
// model command. An absent or empty object queries state; a non-empty value is
// a complete target selection, never a partial patch.
type ModelCommandArguments struct {
	ProviderKey             string          `json:"provider_key,omitempty"`
	ModelID                 string          `json:"model_id,omitempty"`
	Reasoning               model.Reasoning `json:"reasoning,omitempty"`
	Temperature             *float64        `json:"temperature,omitempty"`
	MaxTokens               *int            `json:"max_tokens,omitempty"`
	AcceptCompatibilityLoss bool            `json:"accept_compatibility_loss,omitempty"`
}

// ModelCommandSelection is the current durable Session selection returned to
// every UI renderer through the same Gateway command backend.
type ModelCommandSelection struct {
	ProviderKey string          `json:"provider_key"`
	ModelID     string          `json:"model_id"`
	Reasoning   model.Reasoning `json:"reasoning"`
	Temperature *float64        `json:"temperature,omitempty"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
}

// ModelCommandOption is one detached catalog entry. It contains portable UI
// facts only; credentials and provider client configuration never cross the
// Gateway boundary.
type ModelCommandOption struct {
	ProviderKey         string            `json:"provider_key"`
	ModelID             string            `json:"model_id"`
	Title               string            `json:"title"`
	InputModalities     []model.Modality  `json:"input_modalities"`
	OutputModalities    []model.Modality  `json:"output_modalities"`
	Reasoning           []model.Reasoning `json:"reasoning"`
	ContextWindowTokens int               `json:"context_window_tokens"`
	MaxOutputTokens     int               `json:"max_output_tokens"`
}

// ModelCommandData is the renderer-independent state used by slash, menu,
// command-palette, and direct structured clients.
type ModelCommandData struct {
	Current ModelCommandSelection `json:"current"`
	Models  []ModelCommandOption  `json:"models"`
}

// NewModelCommandModule explicitly installs the optional built-in model
// command. Applications may omit it or install another command under the same
// stable key, but duplicate keys are rejected by Assembly Build.
func NewModelCommandModule() agentslot.Module { return modelCommandModule{} }

type modelCommandModule struct{}

func (modelCommandModule) ID() string { return "interaction.command.model" }

func (modelCommandModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Add(CommandSlot, ModelCommandKey, InteractionCommand(modelCommand{})))
}

type modelCommand struct{}

func (modelCommand) Describe() CommandDescriptor {
	return CommandDescriptor{
		Key: ModelCommandKey, Title: "Model", Description: "View or change the current Session model",
		Fields: []FieldDescriptor{
			{Key: "provider_key", Title: "Provider", Type: FieldText, Required: true},
			{Key: "model_id", Title: "Model", Type: FieldText, Required: true},
			{
				Key: "reasoning", Title: "Reasoning", Type: FieldText, Required: true,
				Description: "Use only a reasoning value advertised by the selected model",
			},
			{Key: "temperature", Title: "Temperature", Type: FieldText},
			{Key: "max_tokens", Title: "Maximum output tokens", Type: FieldText},
			{Key: "accept_compatibility_loss", Title: "Accept compatibility loss", Type: FieldBoolean},
		},
	}
}

func (modelCommand) Invoke(ctx context.Context, invocation CommandInvocation, actions CommandActions) (CommandResult, error) {
	arguments, update, err := decodeModelCommandArguments(invocation.Arguments)
	if err != nil {
		return CommandResult{}, err
	}
	current, err := actions.CurrentModelConfig(ctx)
	if err != nil {
		return CommandResult{}, err
	}
	models, err := actions.AvailableModels(ctx)
	if err != nil {
		return CommandResult{}, err
	}
	if update {
		config := model.Config{
			ProviderKey: arguments.ProviderKey,
			ModelID:     arguments.ModelID,
			Reasoning:   arguments.Reasoning,
			Parameters: model.Parameters{
				Temperature: cloneFloat(arguments.Temperature),
				MaxTokens:   cloneInt(arguments.MaxTokens),
			},
		}
		result, err := actions.Apply(ctx, ActionRequest{
			Kind:                    ActionUpdateModelConfig,
			ExpectedRevision:        invocation.ExpectedRevision,
			Config:                  config,
			AcceptCompatibilityLoss: arguments.AcceptCompatibilityLoss,
		})
		if err != nil {
			return CommandResult{}, err
		}
		current.Revision = result.Revision
		current.Config = config
	}
	data, err := json.Marshal(ModelCommandData{
		Current: modelCommandSelection(current.Config),
		Models:  modelCommandOptions(models),
	})
	if err != nil {
		return CommandResult{}, agent.NewError(agent.ErrorInternal, "interaction.model_command", "cannot encode command result", err)
	}
	return CommandResult{Revision: current.Revision, Data: data}, nil
}

func decodeModelCommandArguments(raw json.RawMessage) (ModelCommandArguments, bool, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ModelCommandArguments{}, false, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var arguments ModelCommandArguments
	if err := decoder.Decode(&arguments); err != nil {
		return ModelCommandArguments{}, false, invalidModelCommandArguments(err)
	}
	if err := ensureJSONEnd(decoder); err != nil {
		return ModelCommandArguments{}, false, invalidModelCommandArguments(err)
	}
	update := arguments.ProviderKey != "" || arguments.ModelID != "" || arguments.Reasoning != "" || arguments.Temperature != nil || arguments.MaxTokens != nil
	if !update {
		if arguments.AcceptCompatibilityLoss {
			return ModelCommandArguments{}, false, invalidModelCommandArguments(fmt.Errorf("compatibility confirmation requires a target model"))
		}
		return arguments, false, nil
	}
	if arguments.ProviderKey == "" {
		return ModelCommandArguments{}, false, invalidModelCommandArguments(fmt.Errorf("provider_key is required"))
	}
	config := model.Config{
		ProviderKey: arguments.ProviderKey,
		ModelID:     arguments.ModelID,
		Reasoning:   arguments.Reasoning,
		Parameters: model.Parameters{
			Temperature: arguments.Temperature,
			MaxTokens:   arguments.MaxTokens,
		},
	}
	if err := config.Validate(); err != nil {
		return ModelCommandArguments{}, false, invalidModelCommandArguments(err)
	}
	return arguments, true, nil
}

func ensureJSONEnd(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func invalidModelCommandArguments(cause error) error {
	return agent.NewError(agent.ErrorInvalidInput, "interaction.model_command", "invalid model command arguments", cause)
}

func modelCommandSelection(config model.Config) ModelCommandSelection {
	return ModelCommandSelection{
		ProviderKey: config.ProviderKey,
		ModelID:     config.ModelID,
		Reasoning:   config.Reasoning,
		Temperature: cloneFloat(config.Parameters.Temperature),
		MaxTokens:   cloneInt(config.Parameters.MaxTokens),
	}
}

func modelCommandOptions(descriptors []model.Descriptor) []ModelCommandOption {
	options := make([]ModelCommandOption, len(descriptors))
	for index, descriptor := range descriptors {
		options[index] = ModelCommandOption{
			ProviderKey:         descriptor.ProviderKey,
			ModelID:             descriptor.ModelID,
			Title:               descriptor.Title,
			InputModalities:     append([]model.Modality(nil), descriptor.Capabilities.Media.InputModalities...),
			OutputModalities:    append([]model.Modality(nil), descriptor.Capabilities.Media.OutputModalities...),
			Reasoning:           append([]model.Reasoning(nil), descriptor.Capabilities.Reasoning...),
			ContextWindowTokens: descriptor.Capabilities.ContextWindowTokens,
			MaxOutputTokens:     descriptor.Capabilities.MaxOutputTokens,
		}
	}
	return options
}

func cloneFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

var _ InteractionCommand = modelCommand{}
