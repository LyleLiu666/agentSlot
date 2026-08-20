package standardagent

import (
	"context"
	"fmt"
	"strings"

	agent "github.com/LyleLiu666/agentSlot/agent"
	agentcontext "github.com/LyleLiu666/agentSlot/context"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

func (r *runtimeInstance) prepareModelRequest(run *activeRun, step agent.StepID) (model.ModelRequest, error) {
	r.mu.Lock()
	if r.active != run || run.cancelRequested || r.closing {
		r.mu.Unlock()
		return model.ModelRequest{}, context.Canceled
	}
	snapshot, err := r.viewLocked(run.ctx)
	r.mu.Unlock()
	if err != nil {
		return model.ModelRequest{}, err
	}
	snapshot, dynamic, err := r.persistContextContributions(run, step, snapshot)
	if err != nil {
		return model.ModelRequest{}, err
	}

	for {
		request, _, tokens, err := r.buildContextCandidate(run, step, snapshot, dynamic)
		if err != nil {
			return model.ModelRequest{}, err
		}

		// Replaceable components run outside the Runtime mutex. If any command
		// advanced the aggregate while they worked, recompute from that exact
		// revision instead of publishing a falsely labelled projection.
		r.mu.Lock()
		if r.active != run || run.cancelRequested || r.closing {
			r.mu.Unlock()
			return model.ModelRequest{}, context.Canceled
		}
		latest, err := r.viewLocked(run.ctx)
		if err != nil {
			r.mu.Unlock()
			return model.ModelRequest{}, err
		}
		if latest.Revision != snapshot.Revision {
			snapshot = latest
			r.mu.Unlock()
			continue
		}
		contextView := session.ContextView{
			Version: snapshot.Context.Version + 1, SourceRevision: snapshot.Revision,
			SourceHistorySequence: latestHistorySequence(snapshot.History),
			TokenCount:            tokens, Request: cloneRuntimeModelRequest(request),
		}
		retain := r.components.config.ContextRetentionMode == ContextRetainAll
		_, err = r.commitLocked(run.ctx, snapshot.Revision, "context", []session.Change{{Kind: session.SetContext, Context: &contextView, RetainPreviousContext: retain}})
		r.mu.Unlock()
		if err != nil {
			return model.ModelRequest{}, err
		}
		return request, nil
	}
}

func (r *runtimeInstance) persistContextContributions(run *activeRun, step agent.StepID, snapshot session.Snapshot) (session.Snapshot, []model.Input, error) {
	dynamic := historyInputs(snapshot.History)
	if err := validateDynamicInputs(r.id(), dynamic); err != nil {
		return session.Snapshot{}, nil, err
	}
	for _, source := range r.components.sources {
		for {
			contribution, err := source.Contribute(run.ctx, agentcontext.ContextInput{
				SessionID: r.id(), Revision: snapshot.Revision, Inputs: cloneRuntimeInputs(dynamic), Config: cloneRuntimeConfig(run.config),
			})
			if err != nil {
				return session.Snapshot{}, nil, err
			}
			candidate := append(cloneRuntimeInputs(dynamic), cloneRuntimeInputs(contribution)...)
			if err := validateDynamicInputs(r.id(), candidate); err != nil {
				return session.Snapshot{}, nil, err
			}
			r.mu.Lock()
			if r.active != run || run.cancelRequested || r.closing {
				r.mu.Unlock()
				return session.Snapshot{}, nil, context.Canceled
			}
			latest, err := r.viewLocked(run.ctx)
			if err != nil {
				r.mu.Unlock()
				return session.Snapshot{}, nil, err
			}
			if latest.Revision != snapshot.Revision {
				snapshot = latest
				r.mu.Unlock()
				continue
			}
			fact := session.ContextContributionFact{
				RunID: run.id, StepID: step, SourceKey: source.Key(), Inputs: cloneRuntimeInputs(contribution),
			}
			_, err = r.commitLocked(run.ctx, snapshot.Revision, "context-source", []session.Change{{Kind: session.AppendContextContribution, ContextContribution: &fact}})
			r.mu.Unlock()
			if err != nil {
				return session.Snapshot{}, nil, err
			}
			snapshot, err = r.session.View(run.ctx)
			if err != nil {
				return session.Snapshot{}, nil, err
			}
			dynamic = candidate
			break
		}
	}
	return snapshot, dynamic, nil
}

func (r *runtimeInstance) buildContextCandidate(run *activeRun, step agent.StepID, snapshot session.Snapshot, sourceDynamic []model.Input) (model.ModelRequest, []model.Input, int, error) {
	capabilities, err := r.inspectModel(run.ctx, run.config)
	if err != nil {
		return model.ModelRequest{}, nil, 0, err
	}
	if len(r.components.dispatcher.definitions()) > 0 && !capabilities.Media.ToolCalling {
		return model.ModelRequest{}, nil, 0, modelNotSupported("selected model does not support tool calling", nil)
	}

	dynamic := cloneRuntimeInputs(sourceDynamic)
	if err := validateDynamicInputs(r.id(), dynamic); err != nil {
		return model.ModelRequest{}, nil, 0, err
	}
	dynamic = projectUnsupportedAttachments(dynamic, capabilities.Media)
	if err := validateDynamicInputs(r.id(), dynamic); err != nil {
		return model.ModelRequest{}, nil, 0, err
	}

	request := r.assembleModelRequest(run, step, dynamic)
	tokens, err := r.components.executor.CountTokens(run.ctx, cloneRuntimeModelRequest(request))
	if err != nil {
		return model.ModelRequest{}, nil, 0, err
	}
	if tokens < 0 {
		return model.ModelRequest{}, nil, 0, agent.NewError(agent.ErrorInternal, "standardagent.context", "ModelExecutor returned a negative token count", nil)
	}
	limit := capabilities.ContextWindowTokens
	if configured := r.components.config.Context.HardTokenLimit; configured > 0 && configured < limit {
		limit = configured
	}
	if tokens <= limit {
		return request, dynamic, tokens, nil
	}
	if r.components.compactor == nil {
		return model.ModelRequest{}, nil, 0, contextLimitError(tokens, limit)
	}
	compacted, err := r.components.compactor.Compact(run.ctx, agentcontext.CompactionInput{
		SessionID: r.id(), Revision: snapshot.Revision, Inputs: cloneRuntimeInputs(dynamic), Config: cloneRuntimeConfig(run.config),
	})
	if err != nil {
		return model.ModelRequest{}, nil, 0, err
	}
	if compacted.SourceRevision != snapshot.Revision {
		return model.ModelRequest{}, nil, 0, agent.NewCodedError(agent.ErrorConflict, agent.CodeRevisionConflict, "standardagent.context.compact", "compactor output was based on a different Session revision", nil)
	}
	dynamic = cloneRuntimeInputs(compacted.Inputs)
	if err := validateDynamicInputs(r.id(), dynamic); err != nil {
		return model.ModelRequest{}, nil, 0, err
	}
	dynamic = projectUnsupportedAttachments(dynamic, capabilities.Media)
	request = r.assembleModelRequest(run, step, dynamic)
	tokens, err = r.components.executor.CountTokens(run.ctx, cloneRuntimeModelRequest(request))
	if err != nil {
		return model.ModelRequest{}, nil, 0, err
	}
	if tokens < 0 {
		return model.ModelRequest{}, nil, 0, agent.NewError(agent.ErrorInternal, "standardagent.context", "ModelExecutor returned a negative token count", nil)
	}
	if tokens > limit {
		return model.ModelRequest{}, nil, 0, contextLimitError(tokens, limit)
	}
	return request, dynamic, tokens, nil
}

func latestHistorySequence(history []session.HistoryFact) session.HistorySequence {
	if len(history) == 0 {
		return 0
	}
	return history[len(history)-1].Sequence
}

func (r *runtimeInstance) assembleModelRequest(run *activeRun, step agent.StepID, dynamic []model.Input) model.ModelRequest {
	inputs := make([]model.Input, 0, len(dynamic)+1)
	if r.components.config.SystemPrompt != "" {
		prompt := r.components.config.SystemPrompt
		inputs = append(inputs, model.Input{SystemPrompt: &prompt})
	}
	inputs = append(inputs, cloneRuntimeInputs(dynamic)...)
	return model.ModelRequest{
		SessionID: r.id(), RunID: run.id, StepID: step,
		Config: cloneRuntimeConfig(run.config), ConfigRevision: run.configRevision,
		Inputs: inputs, Tools: r.components.dispatcher.definitions(),
	}
}

func (r *runtimeInstance) inspectModel(ctx context.Context, config model.Config) (model.ExecutionCapabilities, error) {
	capabilities, err := r.components.executor.Inspect(ctx, cloneRuntimeConfig(config))
	if err != nil {
		return model.ExecutionCapabilities{}, modelNotSupported("model configuration is not supported", err)
	}
	if err := capabilities.Validate(); err != nil {
		return model.ExecutionCapabilities{}, modelNotSupported("ModelExecutor returned invalid capabilities", err)
	}
	foundReasoning := false
	for _, supported := range capabilities.Reasoning {
		if supported == config.Reasoning {
			foundReasoning = true
			break
		}
	}
	if !foundReasoning {
		return model.ExecutionCapabilities{}, modelNotSupported("selected reasoning mode is not supported", nil)
	}
	if config.Parameters.MaxTokens != nil && capabilities.MaxOutputTokens > 0 && *config.Parameters.MaxTokens > capabilities.MaxOutputTokens {
		return model.ExecutionCapabilities{}, modelNotSupported("requested output token limit exceeds model capability", nil)
	}
	return capabilities, nil
}

func validateDynamicInputs(sessionID agent.SessionID, inputs []model.Input) error {
	for _, input := range inputs {
		if input.SystemPrompt != nil {
			return agent.NewError(agent.ErrorInvalidInput, "standardagent.context", "Context components cannot supply SystemPrompt", nil)
		}
		if input.Message != nil && input.Message.SessionID != sessionID {
			return agent.NewError(agent.ErrorInvalidInput, "standardagent.context", "Context message belongs to another Session", nil)
		}
		if input.ToolCall != nil && input.ToolCall.SessionID != sessionID {
			return agent.NewError(agent.ErrorInvalidInput, "standardagent.context", "Context tool call belongs to another Session", nil)
		}
	}
	if err := model.ValidateInputs(inputs); err != nil {
		return agent.NewError(agent.ErrorInvalidInput, "standardagent.context", "Context violates the model protocol", err)
	}
	return nil
}

func projectUnsupportedAttachments(inputs []model.Input, capabilities model.Capabilities) []model.Input {
	projected := cloneRuntimeInputs(inputs)
	for index := range projected {
		message := projected[index].Message
		if message == nil {
			continue
		}
		parts := make([]agent.MessagePart, 0, len(message.Parts))
		for _, part := range message.Parts {
			if part.Kind != agent.PartAttachment || capabilities.SupportsInput(attachmentModality(part.MediaType)) {
				parts = append(parts, part)
				continue
			}
			name := ""
			if part.Name != "" {
				name = fmt.Sprintf(" name=%q", part.Name)
			}
			parts = append(parts, agent.MessagePart{Kind: agent.PartText, Text: fmt.Sprintf("[attachment id=%q media_type=%q%s omitted: selected model does not support this modality]", part.AttachmentID, part.MediaType, name)})
		}
		message.Parts = parts
	}
	return projected
}

func attachmentModality(mediaType string) model.Modality {
	switch {
	case strings.HasPrefix(strings.ToLower(mediaType), "image/"):
		return model.ModalityImage
	case strings.HasPrefix(strings.ToLower(mediaType), "audio/"):
		return model.ModalityAudio
	default:
		// AgentSlot currently has no generic binary modality. Keeping it as an
		// unsupported reference avoids inventing provider behavior.
		return 0
	}
}

func cloneRuntimeInputs(source []model.Input) []model.Input {
	result := make([]model.Input, len(source))
	for index, input := range source {
		result[index] = input
		if input.SystemPrompt != nil {
			value := *input.SystemPrompt
			result[index].SystemPrompt = &value
		}
		if input.Message != nil {
			value := *input.Message
			value.Parts = cloneRuntimeParts(input.Message.Parts)
			result[index].Message = &value
		}
		if input.ToolCall != nil {
			value := *input.ToolCall
			value.Arguments = append([]byte(nil), input.ToolCall.Arguments...)
			result[index].ToolCall = &value
		}
		if input.ToolResult != nil {
			value := cloneRuntimeToolResult(*input.ToolResult)
			result[index].ToolResult = &value
		}
	}
	return result
}

func cloneRuntimeModelRequest(source model.ModelRequest) model.ModelRequest {
	cloned := source
	cloned.Config = cloneRuntimeConfig(source.Config)
	cloned.Inputs = cloneRuntimeInputs(source.Inputs)
	cloned.Tools = append(cloned.Tools[:0:0], source.Tools...)
	return cloned
}

func contextLimitError(tokens, limit int) error {
	return agent.NewCodedError(agent.ErrorInvalidInput, agent.CodeContextLimitExceeded, "standardagent.context", fmt.Sprintf("model request uses %d tokens and exceeds the hard limit %d", tokens, limit), nil)
}

func modelNotSupported(message string, cause error) error {
	return agent.NewCodedError(agent.ErrorInvalidInput, agent.CodeModelNotSupported, "standardagent.model", message, cause)
}
