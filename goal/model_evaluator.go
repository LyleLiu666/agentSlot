package goal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
)

const evaluationSystemPrompt = `You are the completion judge for one explicit agent goal. Decide from the supplied objective and evidence whether the objective is done, can continue autonomously, or is blocked. Return exactly one JSON object with keys decision, reason, and next_instruction. decision is continue, blocked, or done. reason is progress_possible, objective_met, needs_input, or external_blocker. next_instruction must be a concrete non-empty instruction only for continue, and an empty string otherwise. Do not use tools and do not add markdown.`

func NewModelEvaluatorModule(id string) (agentslot.Module, error) {
	if id == "" {
		return nil, errors.New("goal: model evaluator module ID is required")
	}
	return modelEvaluatorModule{id: id}, nil
}

func NewModelEvaluator(executor model.ModelExecutor) (*ModelEvaluator, error) {
	if executor == nil {
		return nil, errors.New("goal: ModelExecutor is required")
	}
	return &ModelEvaluator{executor: executor}, nil
}

type modelEvaluatorModule struct{ id string }

func (m modelEvaluatorModule) ID() string { return m.id }
func (m modelEvaluatorModule) RequiredSlots() []agentslot.Requirement {
	return []agentslot.Requirement{agentslot.RequireOne(model.ExecutorSlot)}
}
func (m modelEvaluatorModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.SetWith(EvaluatorSlot, func(resolver agentslot.Resolver) (Evaluator, error) {
		executor, err := agentslot.ResolveOne(resolver, model.ExecutorSlot)
		if err != nil {
			return nil, err
		}
		return NewModelEvaluator(executor)
	}))
}

type ModelEvaluator struct{ executor model.ModelExecutor }

func (e *ModelEvaluator) Evaluate(ctx context.Context, request EvaluationRequest, recorder model.AttemptRecorder) (Evaluation, error) {
	if e == nil || e.executor == nil || recorder == nil {
		return Evaluation{}, errors.New("goal: model evaluator is not configured")
	}
	if err := request.Goal.Validate(); err != nil {
		return Evaluation{}, err
	}
	if !request.RunID.Valid() || !request.StepID.Valid() || request.Revision == 0 || request.ModelConfig.Validate() != nil {
		return Evaluation{}, errors.New("goal: invalid evaluation request")
	}
	inputs := make([]model.Input, 0, len(request.Messages)+2)
	system := evaluationSystemPrompt
	inputs = append(inputs, model.Input{SystemPrompt: &system})
	for index := range request.Messages {
		message := request.Messages[index]
		if !message.Valid() {
			return Evaluation{}, errors.New("goal: invalid evidence message")
		}
		inputs = append(inputs, model.Input{Message: &message})
	}
	prompt := agent.Message{
		ID: "goal-evaluation-input", SessionID: request.Goal.SessionID, RunID: request.RunID, StepID: request.StepID,
		Role: agent.RoleUser, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "GOAL:\n" + request.Goal.Objective + "\nEvaluate completion now."}},
	}
	inputs = append(inputs, model.Input{Message: &prompt})
	stream, err := e.executor.Execute(ctx, model.ModelRequest{
		SessionID: request.Goal.SessionID, RunID: request.RunID, StepID: request.StepID,
		Config: request.ModelConfig, ConfigRevision: request.Revision, Inputs: inputs,
	}, recorder)
	if err != nil {
		return Evaluation{}, err
	}
	if stream == nil {
		return Evaluation{}, errors.New("goal: model evaluator returned a nil stream")
	}
	defer stream.Close()
	streamState := model.StreamState{}
	for {
		event, err := stream.Recv(ctx)
		if err != nil {
			return Evaluation{}, streamState.End(err)
		}
		if err := streamState.Accept(event); err != nil {
			return Evaluation{}, err
		}
		switch event.Kind {
		case model.EventComplete:
			if len(event.Output.ToolCalls) != 0 {
				return Evaluation{}, errors.New("goal: evaluator attempted a tool call")
			}
			text, err := evaluationText(event.Output.Parts)
			if err != nil {
				return Evaluation{}, err
			}
			return parseEvaluation(text)
		case model.EventFailed:
			return Evaluation{}, event.Err
		}
	}
}

type evaluationWire struct {
	Decision        Decision   `json:"decision"`
	Reason          ReasonCode `json:"reason"`
	NextInstruction string     `json:"next_instruction"`
}

func parseEvaluation(raw string) (Evaluation, error) {
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	var wire evaluationWire
	if err := decoder.Decode(&wire); err != nil {
		return Evaluation{}, fmt.Errorf("goal: decode evaluator response: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Evaluation{}, err
	}
	result := Evaluation{Decision: wire.Decision, Reason: wire.Reason}
	if strings.TrimSpace(wire.NextInstruction) != wire.NextInstruction {
		return Evaluation{}, errors.New("goal: next instruction cannot contain surrounding whitespace")
	}
	if wire.NextInstruction != "" {
		result.NextInstruction = agent.MessageInput{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: wire.NextInstruction}}}
	}
	if err := result.Validate(); err != nil {
		return Evaluation{}, err
	}
	return result, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("goal: evaluator response contains multiple JSON values")
		}
		return fmt.Errorf("goal: decode evaluator response suffix: %w", err)
	}
	return nil
}

func evaluationText(parts []agent.MessagePart) (string, error) {
	var text strings.Builder
	for _, part := range parts {
		if part.Kind != agent.PartText {
			return "", errors.New("goal: evaluator response must be text only")
		}
		text.WriteString(part.Text)
	}
	if text.Len() == 0 {
		return "", errors.New("goal: evaluator response is empty")
	}
	return text.String(), nil
}
