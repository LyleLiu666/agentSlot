// Package openaicompat implements the OpenAI Chat Completions-compatible wire
// protocol behind AgentSlot's provider-neutral ModelExecutor contract.
package openaicompat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
)

type Model struct {
	ID           string
	Title        string
	Capabilities model.ExecutionCapabilities
}

type Config struct {
	ProviderKey    string
	BaseURL        string
	APIKey         string
	Models         []Model
	HTTPClient     *http.Client
	MaxAttempts    int
	RetryBackoff   time.Duration
	RequestTimeout time.Duration
	MaxEventBytes  int
	MaxOutputBytes int
}

type Executor struct {
	providerKey    string
	endpoint       string
	apiKey         string
	client         *http.Client
	models         map[string]Model
	descriptors    []model.Descriptor
	maxAttempts    int
	retryBackoff   time.Duration
	requestTimeout time.Duration
	maxEventBytes  int
	maxOutputBytes int
	sequence       atomic.Uint64
}

var (
	_ model.ModelExecutor = (*Executor)(nil)
	_ model.ModelCatalog  = (*Executor)(nil)
)

func New(config Config) (*Executor, error) {
	if config.ProviderKey == "" {
		return nil, errors.New("openaicompat: provider key is required")
	}
	parsed, err := url.Parse(config.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("openaicompat: base URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if len(config.Models) == 0 {
		return nil, errors.New("openaicompat: at least one model is required")
	}
	models := make(map[string]Model, len(config.Models))
	descriptors := make([]model.Descriptor, 0, len(config.Models))
	for _, configured := range config.Models {
		if configured.ID == "" || configured.Title == "" {
			return nil, errors.New("openaicompat: model ID and title are required")
		}
		if err := configured.Capabilities.Validate(); err != nil {
			return nil, fmt.Errorf("openaicompat: model %q capabilities: %w", configured.ID, err)
		}
		if _, duplicate := models[configured.ID]; duplicate {
			return nil, fmt.Errorf("openaicompat: duplicate model %q", configured.ID)
		}
		configured.Capabilities = cloneCapabilities(configured.Capabilities)
		models[configured.ID] = configured
		descriptors = append(descriptors, model.Descriptor{
			ProviderKey: config.ProviderKey, ModelID: configured.ID, Title: configured.Title,
			Capabilities: cloneCapabilities(configured.Capabilities),
		})
	}
	sort.Slice(descriptors, func(i, j int) bool { return descriptors[i].ModelID < descriptors[j].ModelID })
	maxAttempts := config.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = 3
	}
	if maxAttempts < 1 || maxAttempts > 10 {
		return nil, errors.New("openaicompat: MaxAttempts must be between 1 and 10")
	}
	if config.RetryBackoff < 0 || config.RequestTimeout < 0 || config.MaxEventBytes < 0 || config.MaxOutputBytes < 0 {
		return nil, errors.New("openaicompat: timeouts and size limits cannot be negative")
	}
	retryBackoff := config.RetryBackoff
	if retryBackoff == 0 {
		retryBackoff = 250 * time.Millisecond
	}
	requestTimeout := config.RequestTimeout
	if requestTimeout == 0 {
		requestTimeout = 2 * time.Minute
	}
	maxEventBytes := config.MaxEventBytes
	if maxEventBytes == 0 {
		maxEventBytes = 4 << 20
	}
	maxOutputBytes := config.MaxOutputBytes
	if maxOutputBytes == 0 {
		maxOutputBytes = 16 << 20
	}
	client := config.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	return &Executor{
		providerKey: config.ProviderKey,
		endpoint:    strings.TrimRight(parsed.String(), "/") + "/chat/completions",
		apiKey:      config.APIKey, client: client, models: models, descriptors: descriptors,
		maxAttempts: maxAttempts, retryBackoff: retryBackoff,
		requestTimeout: requestTimeout, maxEventBytes: maxEventBytes, maxOutputBytes: maxOutputBytes,
	}, nil
}

func (e *Executor) Execute(ctx context.Context, request model.ModelRequest, recorder model.AttemptRecorder) (model.ModelStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := e.Inspect(ctx, request.Config); err != nil {
		return nil, err
	}
	if recorder == nil {
		return nil, agent.NewError(agent.ErrorInvalidInput, "openaicompat.execute", "AttemptRecorder is required", nil)
	}
	payload, err := buildRequest(request)
	if err != nil {
		return nil, agent.NewError(agent.ErrorInvalidInput, "openaicompat.execute", "invalid logical model request", err)
	}
	return newStream(ctx, e, request, payload, recorder), nil
}

func (e *Executor) Inspect(ctx context.Context, config model.Config) (model.ExecutionCapabilities, error) {
	if err := ctx.Err(); err != nil {
		return model.ExecutionCapabilities{}, err
	}
	if err := config.Validate(); err != nil {
		return model.ExecutionCapabilities{}, agent.NewError(agent.ErrorInvalidInput, "openaicompat.inspect", "invalid model config", err)
	}
	if config.ProviderKey != e.providerKey {
		return model.ExecutionCapabilities{}, agent.NewCodedError(agent.ErrorNotFound, agent.CodeModelNotSupported, "openaicompat.inspect", "provider is not configured by this executor", nil)
	}
	configured, ok := e.models[config.ModelID]
	if !ok || !supportsReasoning(configured.Capabilities.Reasoning, config.Reasoning) {
		return model.ExecutionCapabilities{}, agent.NewCodedError(agent.ErrorNotFound, agent.CodeModelNotSupported, "openaicompat.inspect", "model or reasoning mode is not supported", nil)
	}
	if config.Parameters.MaxTokens != nil && configured.Capabilities.MaxOutputTokens > 0 && *config.Parameters.MaxTokens > configured.Capabilities.MaxOutputTokens {
		return model.ExecutionCapabilities{}, agent.NewCodedError(agent.ErrorInvalidInput, agent.CodeModelNotSupported, "openaicompat.inspect", "requested max tokens exceed model capability", nil)
	}
	return cloneCapabilities(configured.Capabilities), nil
}

func (e *Executor) CountTokens(ctx context.Context, request model.ModelRequest) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if _, err := e.Inspect(ctx, request.Config); err != nil {
		return 0, err
	}
	payload, err := buildRequest(request)
	if err != nil {
		return 0, agent.NewError(agent.ErrorInvalidInput, "openaicompat.count_tokens", "invalid logical model request", err)
	}
	// A byte count is a conservative tokenizer-independent upper bound for
	// byte-pair tokenizers. It may compact early, but never understates the
	// request merely because a compatible endpoint uses a different tokenizer.
	return len(payload), nil
}

func (e *Executor) Models(ctx context.Context) ([]model.Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	descriptors := make([]model.Descriptor, len(e.descriptors))
	for index, descriptor := range e.descriptors {
		descriptors[index] = descriptor
		descriptors[index].Capabilities = cloneCapabilities(descriptor.Capabilities)
	}
	return descriptors, nil
}

func cloneCapabilities(source model.ExecutionCapabilities) model.ExecutionCapabilities {
	copy := source
	copy.Media.InputModalities = append([]model.Modality(nil), source.Media.InputModalities...)
	copy.Media.OutputModalities = append([]model.Modality(nil), source.Media.OutputModalities...)
	copy.Reasoning = append([]model.Reasoning(nil), source.Reasoning...)
	return copy
}

func supportsReasoning(supported []model.Reasoning, selected model.Reasoning) bool {
	for _, candidate := range supported {
		if candidate == selected {
			return true
		}
	}
	return false
}

// NewModule contributes one real Executor and its UI-facing catalog. The
// provider wire adapter remains explicit; importing this package changes no
// Assembly.
func NewModule(config Config) (agentslot.Module, error) {
	executor, err := New(config)
	if err != nil {
		return nil, err
	}
	return executorModule{executor: executor}, nil
}

type executorModule struct{ executor *Executor }

func (m executorModule) ID() string { return "model.openaicompat." + m.executor.providerKey }

func (m executorModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(
		agentslot.Set(model.ExecutorSlot, model.ModelExecutor(m.executor)),
		agentslot.Add(model.CatalogSlot, m.executor.providerKey, model.ModelCatalog(m.executor)),
	)
}
