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
	ctx    context.Context
	cancel context.CancelFunc
	events chan model.ModelEvent
	once   sync.Once
}

func newStream(parent context.Context, executor *Executor, payload []byte) *eventStream {
	ctx, cancel := context.WithCancel(parent)
	stream := &eventStream{ctx: ctx, cancel: cancel, events: make(chan model.ModelEvent, 16)}
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
	return nil
}

func (s *eventStream) run(executor *Executor, payload []byte) {
	defer close(s.events)
	for attempt := 1; attempt <= executor.maxAttempts; attempt++ {
		attemptID := fmt.Sprintf("%s-attempt-%d", executor.providerKey, executor.sequence.Add(1))
		completion, emitted, retryable, err := s.attempt(executor, payload, attemptID)
		if err == nil {
			s.emit(model.ModelEvent{Kind: model.EventComplete, AttemptID: attemptID, Output: &completion})
			return
		}
		if s.ctx.Err() != nil {
			return
		}
		if !retryable || attempt == executor.maxAttempts {
			if emitted && !s.emit(model.ModelEvent{Kind: model.EventReset, AttemptID: attemptID}) {
				return
			}
			s.emit(model.ModelEvent{
				Kind: model.EventFailed, AttemptID: attemptID,
				Err: agent.NewError(agent.ErrorUnavailable, "openaicompat.stream", "model provider request failed", err),
			})
			return
		}
		if emitted && !s.emit(model.ModelEvent{Kind: model.EventReset, AttemptID: attemptID}) {
			return
		}
		if !waitRetry(s.ctx, executor.retryBackoff, attempt) {
			return
		}
	}
}

func (s *eventStream) attempt(executor *Executor, payload []byte, attemptID string) (model.Completion, bool, bool, error) {
	ctx := s.ctx
	cancel := func() {}
	if executor.requestTimeout > 0 {
		ctx, cancel = context.WithTimeout(s.ctx, executor.requestTimeout)
	}
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, executor.endpoint, bytes.NewReader(payload))
	if err != nil {
		return model.Completion{}, false, false, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("User-Agent", "AgentSlot-OpenAI-Compatible/1")
	if executor.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+executor.apiKey)
	}
	response, err := executor.client.Do(request)
	if err != nil {
		return model.Completion{}, false, true, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		retryable := response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
		return model.Completion{}, false, retryable, fmt.Errorf("provider returned HTTP status %d", response.StatusCode)
	}
	parser := sseParser{
		attemptID: attemptID, maxEventBytes: executor.maxEventBytes,
		maxOutputBytes: executor.maxOutputBytes, emit: s.emit,
	}
	completion, emitted, err := parser.consume(response.Body)
	return completion, emitted, true, err
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
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
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
			usage := model.Usage{
				InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens, TotalTokens: chunk.Usage.TotalTokens,
			}
			if !usage.Valid() {
				return errors.New("provider returned invalid usage")
			}
			if !p.emit(model.ModelEvent{Kind: model.EventUsage, AttemptID: p.attemptID, Usage: &usage}) {
				return context.Canceled
			}
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
