package openaicompat_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/artifact"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/model/openaicompat"
)

func TestExecutorProjectsImageAttachmentsIntoOpenAIContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		messages := body["messages"].([]any)
		content := messages[0].(map[string]any)["content"].([]any)
		if len(content) != 2 || content[0].(map[string]any)["text"] != "what is this?" {
			t.Errorf("content = %#v", content)
		}
		image := content[1].(map[string]any)["image_url"].(map[string]any)
		if image["url"] != "data:image/png;base64,iVBORw==" {
			t.Errorf("image URL = %#v", image["url"])
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"image\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	executor := newExecutorConfig(t, openaicompat.Config{
		ProviderKey: "openai", BaseURL: server.URL, MaxAttempts: 1,
		ArtifactStore: &fixtureArtifactStore{entries: map[string]fixtureArtifact{
			"artifact-image": {metadata: artifact.Metadata{ID: "artifact-image", MediaType: "image/png", Name: "image.png", Size: 4}, body: []byte{0x89, 'P', 'N', 'G'}},
		}},
		Models: []openaicompat.Model{{ID: "vision", Title: "Vision", Capabilities: model.ExecutionCapabilities{
			Media: model.Capabilities{
				InputModalities:  []model.Modality{model.ModalityText, model.ModalityImage},
				OutputModalities: []model.Modality{model.ModalityText},
			},
			Reasoning: []model.Reasoning{model.ReasoningDefault}, ContextWindowTokens: 100, MaxOutputTokens: 20,
		}}},
	})
	request := requestWithUser("what is this?")
	request.Config.ModelID = "vision"
	request.Inputs[0].Message.Parts = append(request.Inputs[0].Message.Parts, agent.MessagePart{
		Kind: agent.PartAttachment, AttachmentID: "artifact-image", MediaType: "image/png", Name: "image.png",
	})
	stream, err := executor.Execute(context.Background(), request, &attemptRecorder{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	events := receiveUntilTerminal(t, stream)
	if events[len(events)-1].Kind != model.EventComplete {
		t.Fatalf("events = %#v", events)
	}
}

func TestExecutorCountsImageSemanticsInsteadOfBase64PayloadBytes(t *testing.T) {
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Y9ZlN8AAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	png = append(png, bytes.Repeat([]byte{0}, 100_000)...)
	executor := newExecutorConfig(t, openaicompat.Config{
		ProviderKey: "openai", BaseURL: "https://example.invalid", MaxAttempts: 1,
		ArtifactStore: &fixtureArtifactStore{entries: map[string]fixtureArtifact{
			"artifact-image": {metadata: artifact.Metadata{ID: "artifact-image", MediaType: "image/png", Name: "image.png", Size: int64(len(png))}, body: png},
		}},
		Models: []openaicompat.Model{{ID: "vision", Title: "Vision", Capabilities: model.ExecutionCapabilities{
			Media:     model.Capabilities{InputModalities: []model.Modality{model.ModalityText, model.ModalityImage}, OutputModalities: []model.Modality{model.ModalityText}},
			Reasoning: []model.Reasoning{model.ReasoningDefault}, ContextWindowTokens: 4_096, MaxOutputTokens: 512,
		}}},
	})
	request := requestWithUser("describe")
	request.Config.ModelID = "vision"
	request.Inputs[0].Message.Parts = append(request.Inputs[0].Message.Parts, agent.MessagePart{
		Kind: agent.PartAttachment, AttachmentID: "artifact-image", MediaType: "image/png", Name: "image.png",
	})
	tokens, err := executor.CountTokens(context.Background(), request)
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if tokens <= 0 || tokens >= 4_096 {
		t.Fatalf("image token estimate = %d", tokens)
	}
}

func TestExecutorFallbackUsageDoesNotBillBase64TransportAsTokens(t *testing.T) {
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Y9ZlN8AAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	png = append(png, bytes.Repeat([]byte{0}, 100_000)...)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	executor := newExecutorConfig(t, openaicompat.Config{
		ProviderKey: "openai", BaseURL: server.URL, MaxAttempts: 1,
		ArtifactStore: &fixtureArtifactStore{entries: map[string]fixtureArtifact{
			"artifact-image": {metadata: artifact.Metadata{ID: "artifact-image", MediaType: "image/png", Name: "image.png", Size: int64(len(png))}, body: png},
		}},
		Models: []openaicompat.Model{{ID: "vision", Title: "Vision", Capabilities: model.ExecutionCapabilities{
			Media:     model.Capabilities{InputModalities: []model.Modality{model.ModalityText, model.ModalityImage}, OutputModalities: []model.Modality{model.ModalityText}},
			Reasoning: []model.Reasoning{model.ReasoningDefault}, ContextWindowTokens: 4_096, MaxOutputTokens: 512,
		}}},
	})
	request := requestWithUser("describe")
	request.Config.ModelID = "vision"
	request.Inputs[0].Message.Parts = append(request.Inputs[0].Message.Parts, agent.MessagePart{
		Kind: agent.PartAttachment, AttachmentID: "artifact-image", MediaType: "image/png", Name: "image.png",
	})
	recorder := &attemptRecorder{}
	stream, err := executor.Execute(context.Background(), request, recorder)
	if err != nil {
		t.Fatal(err)
	}
	events := receiveUntilTerminal(t, stream)
	if events[len(events)-1].Kind != model.EventComplete {
		t.Fatalf("events = %#v", events)
	}
	_, finishes := recorder.records()
	if len(finishes) != 1 || !finishes[0].Usage.Estimated || finishes[0].Usage.InputTokens >= 4_096 {
		t.Fatalf("fallback usage = %#v", finishes)
	}
}

func TestImageCapableModuleRequiresArtifactStoreAtBuildTime(t *testing.T) {
	config := openaicompat.Config{
		ProviderKey: "openai", BaseURL: "https://example.invalid", Models: []openaicompat.Model{{
			ID: "vision", Title: "Vision", Capabilities: model.ExecutionCapabilities{
				Media:     model.Capabilities{InputModalities: []model.Modality{model.ModalityText, model.ModalityImage}, OutputModalities: []model.Modality{model.ModalityText}},
				Reasoning: []model.Reasoning{model.ReasoningDefault}, ContextWindowTokens: 100, MaxOutputTokens: 20,
			},
		}},
	}
	provider, err := openaicompat.NewModule(config)
	if err != nil {
		t.Fatalf("NewModule: %v", err)
	}
	builder := agentslot.NewBuilder()
	if err := builder.Install(provider); err != nil {
		t.Fatalf("Install provider: %v", err)
	}
	if _, err := builder.Build(agentslot.RequireOne(model.ExecutorSlot)); !errors.Is(err, agentslot.ErrRequirementUnsatisfied) {
		t.Fatalf("Build without artifact store = %v", err)
	}

	provider, err = openaicompat.NewModule(config)
	if err != nil {
		t.Fatalf("NewModule retry: %v", err)
	}
	storeModule, err := artifact.NewModule("artifact.fixture", &fixtureArtifactStore{})
	if err != nil {
		t.Fatalf("artifact module: %v", err)
	}
	builder = agentslot.NewBuilder()
	if err := builder.Install(storeModule); err != nil {
		t.Fatalf("Install store: %v", err)
	}
	if err := builder.Install(provider); err != nil {
		t.Fatalf("Install provider: %v", err)
	}
	if _, err := builder.Build(agentslot.RequireOne(model.ExecutorSlot)); err != nil {
		t.Fatalf("Build with artifact store: %v", err)
	}
}

type fixtureArtifact struct {
	metadata artifact.Metadata
	body     []byte
}

type fixtureArtifactStore struct {
	entries map[string]fixtureArtifact
}

func (*fixtureArtifactStore) Write(context.Context, artifact.WriteRequest) (artifact.Metadata, error) {
	return artifact.Metadata{}, errors.New("not implemented")
}

func (s *fixtureArtifactStore) Open(_ context.Context, id string) (artifact.Content, error) {
	entry, ok := s.entries[id]
	if !ok {
		return artifact.Content{}, artifact.ErrNotFound
	}
	return artifact.Content{Metadata: entry.metadata, Body: io.NopCloser(bytes.NewReader(entry.body))}, nil
}

func TestExecutorStreamsOpenAICompatibleCompletion(t *testing.T) {
	recorder := &attemptRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		starts, _ := recorder.records()
		if len(starts) != 1 {
			t.Errorf("provider request began before Attempt Started was durable: %#v", starts)
		}
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
		writer.Header().Set("x-request-id", "provider-request-1")
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
	}, recorder)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer stream.Close()
	events := receiveUntilTerminal(t, stream)
	if len(events) != 3 || events[0].Kind != model.EventDelta || events[0].Text != "hel" || events[1].Text != "lo" || events[2].Kind != model.EventComplete {
		t.Fatalf("events = %#v", events)
	}
	if events[0].AttemptID == "" || events[0].AttemptID != events[1].AttemptID || events[1].AttemptID != events[2].AttemptID {
		t.Fatalf("attempt IDs = %#v", events)
	}
	starts, finishes := recorder.records()
	if len(starts) != 1 || len(finishes) != 1 || finishes[0].Usage.InputTokens != 4 || finishes[0].Usage.OutputTokens != 2 || finishes[0].Usage.TotalTokens != 6 || finishes[0].ProviderRequestID != "provider-request-1" {
		t.Fatalf("attempt records = %#v / %#v", starts, finishes)
	}
	if got := events[2].Output.Parts[0].Text; got != "hello" {
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
	stream, err := executor.Execute(context.Background(), requestWithUser("run a command"), &attemptRecorder{})
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
	recorder := &attemptRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempt := requests.Add(1)
		starts, finishes := recorder.records()
		if len(starts) != int(attempt) {
			t.Errorf("request %d began without its Started record: %#v", attempt, starts)
		}
		if attempt == 2 && len(finishes) != 1 {
			t.Errorf("retry began before prior terminal record: %#v", finishes)
		}
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
	stream, err := executor.Execute(context.Background(), requestWithUser("retry"), recorder)
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
	starts, finishes := recorder.records()
	if len(starts) != 2 || len(finishes) != 2 || finishes[0].Outcome != model.AttemptFailed || !finishes[0].Usage.Estimated || finishes[1].Outcome != model.AttemptSucceeded {
		t.Fatalf("retry attempt records = %#v / %#v", starts, finishes)
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
	recorder := &attemptRecorder{}
	stream, err := executor.Execute(context.Background(), requestWithUser("bad"), recorder)
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
	_, finishes := recorder.records()
	if len(finishes) != 1 || finishes[0].ErrorCode != "http_400" || !finishes[0].Usage.Estimated {
		t.Fatalf("failed attempt = %#v", finishes)
	}
}

func TestExecutorRecordsProviderTimeoutAsFailureNotUserCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		select {
		case <-request.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	}))
	defer server.Close()
	executor := newExecutorConfig(t, openaicompat.Config{
		ProviderKey: "openai", BaseURL: server.URL, MaxAttempts: 1, RequestTimeout: 10 * time.Millisecond,
		Models: []openaicompat.Model{{ID: "chat-model", Title: "Chat Model", Capabilities: model.ExecutionCapabilities{
			Media:     model.Capabilities{InputModalities: []model.Modality{model.ModalityText}, OutputModalities: []model.Modality{model.ModalityText}},
			Reasoning: []model.Reasoning{model.ReasoningDefault}, ContextWindowTokens: 100, MaxOutputTokens: 20,
		}}},
	})
	recorder := &attemptRecorder{}
	stream, err := executor.Execute(context.Background(), requestWithUser("timeout"), recorder)
	if err != nil {
		t.Fatal(err)
	}
	events := receiveUntilTerminal(t, stream)
	if terminal := events[len(events)-1]; terminal.Kind != model.EventFailed {
		t.Fatalf("timeout terminal = %#v", terminal)
	}
	_, finishes := recorder.records()
	if len(finishes) != 1 || finishes[0].Outcome != model.AttemptFailed || finishes[0].ErrorCode != "timeout" {
		t.Fatalf("timeout attempt = %#v", finishes)
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
	stream, err := executor.Execute(context.Background(), requestWithUser("bounded"), &attemptRecorder{})
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

type attemptRecorder struct {
	mu       sync.Mutex
	started  []model.AttemptStart
	finished []model.AttemptFinish
	used     int64
}

func (r *attemptRecorder) Started(_ context.Context, value model.AttemptStart) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = append(r.started, value)
	return nil
}

func (r *attemptRecorder) Finished(_ context.Context, value model.AttemptFinish) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished = append(r.finished, value)
	r.used += value.Usage.TotalTokens
	return nil
}

func (r *attemptRecorder) Budget() model.TokenBudget {
	r.mu.Lock()
	defer r.mu.Unlock()
	return model.TokenBudget{UsedTokens: r.used}
}

func (r *attemptRecorder) records() ([]model.AttemptStart, []model.AttemptFinish) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]model.AttemptStart(nil), r.started...), append([]model.AttemptFinish(nil), r.finished...)
}

var _ model.ModelExecutor = (*openaicompat.Executor)(nil)
var _ model.ModelCatalog = (*openaicompat.Executor)(nil)
