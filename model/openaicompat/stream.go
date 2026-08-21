package openaicompat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
)

type eventStream struct {
	ctx                context.Context
	cancel             context.CancelFunc
	events             chan model.ModelEvent
	done               chan struct{}
	once               sync.Once
	request            model.ModelRequest
	recorder           model.AttemptRecorder
	inputTokenEstimate int
}

func newStream(parent context.Context, executor *Executor, request model.ModelRequest, payload []byte, inputTokenEstimate int, recorder model.AttemptRecorder) *eventStream {
	ctx, cancel := context.WithCancel(parent)
	stream := &eventStream{ctx: ctx, cancel: cancel, events: make(chan model.ModelEvent, 16), done: make(chan struct{}), request: request, recorder: recorder, inputTokenEstimate: inputTokenEstimate}
	go stream.run(executor, append([]byte(nil), payload...))
	return stream
}

func (s *eventStream) Recv(ctx context.Context) (model.ModelEvent, error) {
	select {
	case event, ok := <-s.events:
		if !ok {
			return model.ModelEvent{}, model.ErrStreamClosed
		}
		return event, nil
	case <-ctx.Done():
		return model.ModelEvent{}, ctx.Err()
	case <-s.ctx.Done():
		return model.ModelEvent{}, model.ErrStreamClosed
	}
}

func (s *eventStream) Close() error {
	s.once.Do(s.cancel)
	<-s.done
	return nil
}

func (s *eventStream) run(executor *Executor, payload []byte) {
	defer close(s.done)
	defer close(s.events)
	for attempt := 1; attempt <= executor.maxAttempts; attempt++ {
		if s.recorder.Budget().Exhausted() {
			s.emit(model.ModelEvent{Kind: model.EventFailed, Err: model.ErrTokenBudgetExceeded})
			return
		}
		attemptID := agent.AttemptID(fmt.Sprintf("%s-attempt-%d", executor.providerKey, executor.sequence.Add(1)))
		if err := s.recorder.Started(s.ctx, model.AttemptStart{AttemptID: attemptID, ProviderKey: s.request.Config.ProviderKey, ModelID: s.request.Config.ModelID}); err != nil {
			s.emit(model.ModelEvent{Kind: model.EventFailed, AttemptID: string(attemptID), Err: err})
			return
		}
		result := s.attempt(executor, payload, string(attemptID))
		usage := result.usage
		if usage == (model.TokenUsage{}) {
			outputTokens := int64((result.outputBytes + 3) / 4)
			inputTokens := int64(s.inputTokenEstimate)
			usage = model.TokenUsage{
				InputTokens: inputTokens, OutputTokens: outputTokens,
				TotalTokens: inputTokens + outputTokens,
				Estimated:   true, EstimateSource: "openaicompat.local_semantic_estimate",
			}
		}
		outcome := model.AttemptFailed
		errorCode := result.errorCode
		if result.err == nil {
			outcome, errorCode = model.AttemptSucceeded, ""
		} else if errors.Is(result.err, context.Canceled) || s.ctx.Err() != nil {
			outcome, errorCode = model.AttemptCanceled, "canceled"
		} else if errors.Is(result.err, context.DeadlineExceeded) {
			errorCode = "timeout"
		}
		finish := model.AttemptFinish{
			AttemptID: attemptID, Outcome: outcome, ProviderRequestID: result.providerRequestID,
			Usage: usage, ErrorCode: errorCode,
		}
		if err := s.recorder.Finished(context.WithoutCancel(s.ctx), finish); err != nil {
			s.emit(model.ModelEvent{Kind: model.EventFailed, AttemptID: string(attemptID), Err: err})
			return
		}
		if result.err == nil {
			s.emit(model.ModelEvent{Kind: model.EventComplete, AttemptID: string(attemptID), Output: &result.completion})
			return
		}
		if s.ctx.Err() != nil {
			return
		}
		if !result.retryable || attempt == executor.maxAttempts {
			if result.emitted && !s.emit(model.ModelEvent{Kind: model.EventReset, AttemptID: string(attemptID)}) {
				return
			}
			s.emit(model.ModelEvent{
				Kind: model.EventFailed, AttemptID: string(attemptID),
				Err: agent.NewError(agent.ErrorUnavailable, "openaicompat.stream", "model provider request failed", result.err),
			})
			return
		}
		if result.emitted && !s.emit(model.ModelEvent{Kind: model.EventReset, AttemptID: string(attemptID)}) {
			return
		}
		if s.recorder.Budget().Exhausted() {
			s.emit(model.ModelEvent{Kind: model.EventFailed, AttemptID: string(attemptID), Err: model.ErrTokenBudgetExceeded})
			return
		}
		if !waitRetry(s.ctx, executor.retryBackoff, attempt) {
			return
		}
	}
}

type attemptResult struct {
	completion        model.Completion
	emitted           bool
	retryable         bool
	usage             model.TokenUsage
	providerRequestID string
	outputBytes       int
	errorCode         string
	err               error
}

func (s *eventStream) attempt(executor *Executor, payload []byte, attemptID string) attemptResult {
	ctx := s.ctx
	cancel := func() {}
	if executor.requestTimeout > 0 {
		ctx, cancel = context.WithTimeout(s.ctx, executor.requestTimeout)
	}
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, executor.endpoint, bytes.NewReader(payload))
	if err != nil {
		return attemptResult{errorCode: "request_build", err: err}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("User-Agent", "AgentSlot-OpenAI-Compatible/1")
	if executor.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+executor.apiKey)
	}
	response, err := executor.client.Do(request)
	if err != nil {
		return attemptResult{retryable: true, errorCode: "transport", err: err}
	}
	defer response.Body.Close()
	providerRequestID := response.Header.Get("x-request-id")
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		retryable := response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
		return attemptResult{retryable: retryable, providerRequestID: providerRequestID, errorCode: fmt.Sprintf("http_%d", response.StatusCode), err: fmt.Errorf("provider returned HTTP status %d", response.StatusCode)}
	}
	parser := sseParser{
		attemptID: attemptID, maxEventBytes: executor.maxEventBytes,
		maxOutputBytes: executor.maxOutputBytes, emit: s.emit,
	}
	completion, emitted, err := parser.consume(response.Body)
	result := attemptResult{
		completion: completion, emitted: emitted, retryable: true, usage: parser.usage,
		providerRequestID: providerRequestID, outputBytes: parser.outputBytes, err: err,
	}
	if err != nil {
		result.errorCode = "stream"
	}
	return result
}

func (s *eventStream) emit(event model.ModelEvent) bool {
	select {
	case s.events <- event:
		return true
	case <-s.ctx.Done():
		return false
	}
}

func waitRetry(ctx context.Context, base time.Duration, attempt int) bool {
	delay := base
	for index := 1; index < attempt && delay < 10*time.Second; index++ {
		delay *= 2
	}
	if delay > 10*time.Second {
		delay = 10 * time.Second
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

type sseParser struct {
	attemptID      string
	maxEventBytes  int
	maxOutputBytes int
	emit           func(model.ModelEvent) bool
	text           strings.Builder
	tools          map[int]*toolCallAccumulator
	emitted        bool
	finished       bool
	done           bool
	outputBytes    int
	usage          model.TokenUsage
}

type toolCallAccumulator struct {
	id        strings.Builder
	name      strings.Builder
	arguments strings.Builder
}

type chatChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

func (p *sseParser) consume(reader io.Reader) (model.Completion, bool, error) {
	p.tools = make(map[int]*toolCallAccumulator)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), p.maxEventBytes)
	var data []string
	dataBytes := 0
	process := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		dataBytes = 0
		if payload == "[DONE]" {
			p.done = true
			return nil
		}
		var chunk chatChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return err
		}
		if chunk.Usage != nil {
			usage := model.TokenUsage{
				InputTokens: int64(chunk.Usage.PromptTokens), OutputTokens: int64(chunk.Usage.CompletionTokens), TotalTokens: int64(chunk.Usage.TotalTokens),
			}
			if chunk.Usage.PromptTokensDetails != nil {
				usage.CachedInputTokens = int64(chunk.Usage.PromptTokensDetails.CachedTokens)
			}
			if chunk.Usage.CompletionTokensDetails != nil {
				usage.ReasoningTokens = int64(chunk.Usage.CompletionTokensDetails.ReasoningTokens)
			}
			if err := usage.Validate(); err != nil {
				return errors.New("provider returned invalid usage")
			}
			p.usage = usage
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				if err := p.addOutputBytes(len(choice.Delta.Content)); err != nil {
					return err
				}
				p.text.WriteString(choice.Delta.Content)
				p.emitted = true
				if !p.emit(model.ModelEvent{Kind: model.EventDelta, AttemptID: p.attemptID, Text: choice.Delta.Content}) {
					return context.Canceled
				}
			}
			for _, fragment := range choice.Delta.ToolCalls {
				if err := p.addOutputBytes(len(fragment.ID) + len(fragment.Function.Name) + len(fragment.Function.Arguments)); err != nil {
					return err
				}
				accumulator := p.tools[fragment.Index]
				if accumulator == nil {
					accumulator = &toolCallAccumulator{}
					p.tools[fragment.Index] = accumulator
				}
				accumulator.id.WriteString(fragment.ID)
				accumulator.name.WriteString(fragment.Function.Name)
				accumulator.arguments.WriteString(fragment.Function.Arguments)
			}
			if choice.FinishReason != nil {
				p.finished = true
			}
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := process(); err != nil {
				return model.Completion{}, p.emitted, err
			}
			if p.done {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			dataBytes += len(value)
			if dataBytes > p.maxEventBytes {
				return model.Completion{}, p.emitted, errors.New("provider event exceeds configured size limit")
			}
			data = append(data, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return model.Completion{}, p.emitted, err
	}
	if len(data) > 0 {
		if err := process(); err != nil {
			return model.Completion{}, p.emitted, err
		}
	}
	if !p.done && !p.finished {
		return model.Completion{}, p.emitted, errors.New("provider stream ended before a terminal marker")
	}
	completion := model.Completion{}
	if p.text.Len() > 0 {
		completion.Parts = []agent.MessagePart{{Kind: agent.PartText, Text: p.text.String()}}
	}
	indexes := make([]int, 0, len(p.tools))
	for index := range p.tools {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		accumulator := p.tools[index]
		arguments := accumulator.arguments.String()
		if arguments == "" {
			arguments = "{}"
		}
		call := model.ToolCallRequest{
			CorrelationID: accumulator.id.String(), Name: accumulator.name.String(), Arguments: json.RawMessage(arguments),
		}
		if !call.Valid() {
			return model.Completion{}, p.emitted, errors.New("provider returned an invalid tool call")
		}
		completion.ToolCalls = append(completion.ToolCalls, call)
	}
	if !completion.Valid() {
		return model.Completion{}, p.emitted, errors.New("provider returned an empty completion")
	}
	return completion, p.emitted, nil
}

func (p *sseParser) addOutputBytes(size int) error {
	p.outputBytes += size
	if p.outputBytes > p.maxOutputBytes {
		return errors.New("provider output exceeds configured size limit")
	}
	return nil
}

var _ model.ModelStream = (*eventStream)(nil)
