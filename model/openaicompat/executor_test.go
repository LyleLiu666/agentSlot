package openaicompat_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/model/openaicompat"
)

func TestExecutorStreamsOpenAICompatibleCompletion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" || request.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("request = %s %s authorization=%q", request.Method, request.URL.Path, request.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		streamOptions, _ := body["stream_options"].(map[string]any)
		if body["model"] != "chat-model" || body["stream"] != true || streamOptions["include_usage"] != true {
			t.Errorf("request body = %#v", body)
		}
		messages, _ := body["messages"].([]any)
		if len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" || messages[1].(map[string]any)["content"] != "hello" {
			t.Errorf("messages = %#v", messages)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		flusher := writer.(http.Flusher)
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n"))
		flusher.Flush()
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2,\"total_tokens\":6}}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	executor := newExecutor(t, server.URL+"/v1", "secret")
	prompt := "be concise"
	stream, err := executor.Execute(context.Background(), model.ModelRequest{
		Config: model.Config{ProviderKey: "openai", ModelID: "chat-model", Reasoning: model.ReasoningDefault},
		Inputs: []model.Input{
			{SystemPrompt: &prompt},
			{Message: &agent.Message{ID: "message-1", SessionID: "session-1", Role: agent.RoleUser, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "hello"}}}},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer stream.Close()
	events := receiveUntilTerminal(t, stream)
	if len(events) != 4 || events[0].Kind != model.EventDelta || events[0].Text != "hel" || events[1].Text != "lo" || events[2].Kind != model.EventUsage || events[3].Kind != model.EventComplete {
		t.Fatalf("events = %#v", events)
	}
	if events[0].AttemptID == "" || events[0].AttemptID != events[1].AttemptID || events[1].AttemptID != events[2].AttemptID || events[2].AttemptID != events[3].AttemptID {
		t.Fatalf("attempt IDs = %#v", events)
	}
	if events[2].Usage == nil || events[2].Usage.InputTokens != 4 || events[2].Usage.OutputTokens != 2 || events[2].Usage.TotalTokens != 6 {
		t.Fatalf("usage = %#v", events[2].Usage)
	}
	if got := events[3].Output.Parts[0].Text; got != "hello" {
		t.Fatalf("completion = %q", got)
	}
}

func TestExecutorAggregatesStreamingToolCallFragments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"provider-call-1\",\"type\":\"function\",\"function\":{\"name\":\"bash\",\"arguments\":\"{\\\"command\\\":\"}}]}}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"pwd\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	executor := newExecutor(t, server.URL, "")
	stream, err := executor.Execute(context.Background(), requestWithUser("run a command"))
	if err != nil {
		t.Fatal(err)
	}
	events := receiveUntilTerminal(t, stream)
	terminal := events[len(events)-1]
	if terminal.Kind != model.EventComplete || len(terminal.Output.ToolCalls) != 1 {
		t.Fatalf("terminal = %#v", terminal)
	}
	call := terminal.Output.ToolCalls[0]
	if call.CorrelationID != "provider-call-1" || call.Name != "bash" || string(call.Arguments) != `{"command":"pwd"}` {
		t.Fatalf("tool call = %#v", call)
	}
}

func TestExecutorResetsPartialOutputBeforeRetry(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempt := requests.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		if attempt == 1 {
			_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
			return
		}
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"complete\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	executor := newExecutorWithAttempts(t, server.URL, 2)
	stream, err := executor.Execute(context.Background(), requestWithUser("retry"))
	if err != nil {
		t.Fatal(err)
	}
	events := receiveUntilTerminal(t, stream)
	if len(events) != 4 || events[0].Kind != model.EventDelta || events[1].Kind != model.EventReset || events[2].Kind != model.EventDelta || events[3].Kind != model.EventComplete {
		t.Fatalf("retry events = %#v", events)
	}
	if events[0].AttemptID != events[1].AttemptID || events[2].AttemptID != events[3].AttemptID || events[0].AttemptID == events[2].AttemptID {
		t.Fatalf("retry attempt IDs = %#v", events)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d", requests.Load())
	}
}

func TestExecutorDoesNotRetryNonRetryableHTTPErrorOrLeakBody(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(writer, "upstream secret details", http.StatusBadRequest)
	}))
	defer server.Close()
	executor := newExecutorWithAttempts(t, server.URL, 3)
	stream, err := executor.Execute(context.Background(), requestWithUser("bad"))
	if err != nil {
		t.Fatal(err)
	}
	events := receiveUntilTerminal(t, stream)
	terminal := events[len(events)-1]
	if terminal.Kind != model.EventFailed || terminal.Err == nil || strings.Contains(terminal.Err.Error(), "upstream secret details") {
		t.Fatalf("terminal error = %#v", terminal)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d", requests.Load())
	}
}

func TestExecutorBoundsAccumulatedProviderOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"output larger than limit\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	executor := newExecutorConfig(t, openaicompat.Config{
		ProviderKey: "openai", BaseURL: server.URL, MaxAttempts: 1, MaxOutputBytes: 8,
		Models: []openaicompat.Model{{ID: "chat-model", Title: "Chat Model", Capabilities: model.ExecutionCapabilities{
			Media:     model.Capabilities{InputModalities: []model.Modality{model.ModalityText}, OutputModalities: []model.Modality{model.ModalityText}},
			Reasoning: []model.Reasoning{model.ReasoningDefault}, ContextWindowTokens: 100, MaxOutputTokens: 20,
		}}},
	})
	stream, err := executor.Execute(context.Background(), requestWithUser("bounded"))
	if err != nil {
		t.Fatal(err)
	}
	events := receiveUntilTerminal(t, stream)
	if terminal := events[len(events)-1]; terminal.Kind != model.EventFailed {
		t.Fatalf("oversized terminal = %#v", terminal)
	}
}

func TestExecutorCatalogInspectAndTokenCountAreDetachedAndDeterministic(t *testing.T) {
	executor := newExecutor(t, "https://example.invalid/v1", "")
	config := model.Config{ProviderKey: "openai", ModelID: "chat-model", Reasoning: model.ReasoningHigh}
	capabilities, err := executor.Inspect(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.ContextWindowTokens != 16_384 {
		t.Fatalf("capabilities = %#v", capabilities)
	}
	models, err := executor.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	models[0].Capabilities.Reasoning[0] = "mutated"
	again, _ := executor.Models(context.Background())
	if again[0].Capabilities.Reasoning[0] != model.ReasoningDefault {
		t.Fatal("catalog returned shared capability slices")
	}
	request := requestWithUser("count this")
	first, err := executor.CountTokens(context.Background(), request)
	if err != nil || first <= 0 {
		t.Fatalf("CountTokens = %d, %v", first, err)
	}
	second, _ := executor.CountTokens(context.Background(), request)
	if first != second {
		t.Fatalf("token estimates differ: %d / %d", first, second)
	}
}

func TestExecutorRejectsInvalidConfigurationAndSelection(t *testing.T) {
	_, err := openaicompat.New(openaicompat.Config{BaseURL: "://bad"})
	if err == nil {
		t.Fatal("invalid base URL was accepted")
	}
	executor := newExecutor(t, "https://example.invalid", "")
	_, err = executor.Inspect(context.Background(), model.Config{ProviderKey: "other", ModelID: "chat-model", Reasoning: model.ReasoningDefault})
	if err == nil {
		t.Fatal("wrong provider was accepted")
	}
	_, err = executor.Inspect(context.Background(), model.Config{ProviderKey: "openai", ModelID: "missing", Reasoning: model.ReasoningDefault})
	if err == nil {
		t.Fatal("unknown model was accepted")
	}
}

func newExecutor(t *testing.T, baseURL, apiKey string) *openaicompat.Executor {
	t.Helper()
	return newExecutorConfig(t, openaicompat.Config{
		ProviderKey: "openai", BaseURL: baseURL, APIKey: apiKey, MaxAttempts: 1,
		Models: []openaicompat.Model{{
			ID: "chat-model", Title: "Chat Model",
			Capabilities: model.ExecutionCapabilities{
				Media:     model.Capabilities{InputModalities: []model.Modality{model.ModalityText}, OutputModalities: []model.Modality{model.ModalityText}, ToolCalling: true},
				Reasoning: []model.Reasoning{model.ReasoningDefault, model.ReasoningHigh}, ContextWindowTokens: 16_384, MaxOutputTokens: 4_096,
			},
		}},
	})
}

func newExecutorWithAttempts(t *testing.T, baseURL string, attempts int) *openaicompat.Executor {
	t.Helper()
	config := openaicompat.Config{
		ProviderKey: "openai", BaseURL: baseURL, MaxAttempts: attempts, RetryBackoff: time.Millisecond,
		Models: []openaicompat.Model{{
			ID: "chat-model", Title: "Chat Model",
			Capabilities: model.ExecutionCapabilities{
				Media:     model.Capabilities{InputModalities: []model.Modality{model.ModalityText}, OutputModalities: []model.Modality{model.ModalityText}},
				Reasoning: []model.Reasoning{model.ReasoningDefault}, ContextWindowTokens: 16_384, MaxOutputTokens: 4_096,
			},
		}},
	}
	return newExecutorConfig(t, config)
}

func newExecutorConfig(t *testing.T, config openaicompat.Config) *openaicompat.Executor {
	t.Helper()
	executor, err := openaicompat.New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return executor
}

func requestWithUser(text string) model.ModelRequest {
	return model.ModelRequest{
		Config: model.Config{ProviderKey: "openai", ModelID: "chat-model", Reasoning: model.ReasoningDefault},
		Inputs: []model.Input{{Message: &agent.Message{
			ID: "message-1", SessionID: "session-1", Role: agent.RoleUser,
			Parts: []agent.MessagePart{{Kind: agent.PartText, Text: text}},
		}}},
	}
}

func receiveUntilTerminal(t *testing.T, stream model.ModelStream) []model.ModelEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var events []model.ModelEvent
	for {
		event, err := stream.Recv(ctx)
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		events = append(events, event)
		if event.Kind == model.EventComplete || event.Kind == model.EventFailed {
			return events
		}
	}
}

var _ model.ModelExecutor = (*openaicompat.Executor)(nil)
var _ model.ModelCatalog = (*openaicompat.Executor)(nil)
