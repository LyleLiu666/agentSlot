// Package openaicompat implements the OpenAI Chat Completions-compatible wire
// protocol behind AgentSlot's provider-neutral ModelExecutor contract.
package openaicompat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/artifact"
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
	// MaxAttachmentBytes bounds each artifact opened for a provider request.
	// Zero uses the adapter default. The Store may enforce a lower limit.
	MaxAttachmentBytes int64
	ArtifactStore      artifact.ArtifactStore
}

type Executor struct {
	providerKey        string
	endpoint           string
	apiKey             string
	client             *http.Client
	models             map[string]Model
	descriptors        []model.Descriptor
	maxAttempts        int
	retryBackoff       time.Duration
	requestTimeout     time.Duration
	maxEventBytes      int
	maxOutputBytes     int
	maxAttachmentBytes int64
	artifacts          artifact.ArtifactStore
	sequence           atomic.Uint64
}

var (
	_ model.ModelExecutor = (*Executor)(nil)
	_ model.ModelCatalog  = (*Executor)(nil)
)

func New(config Config) (*Executor, error) {
	return newExecutor(config, false)
}

func newExecutor(config Config, allowMissingArtifactStore bool) (*Executor, error) {
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
	needsArtifactStore := false
	for _, configured := range config.Models {
		if configured.ID == "" || configured.Title == "" {
			return nil, errors.New("openaicompat: model ID and title are required")
		}
		if err := configured.Capabilities.Validate(); err != nil {
			return nil, fmt.Errorf("openaicompat: model %q capabilities: %w", configured.ID, err)
		}
		if configured.Capabilities.Media.SupportsInput(model.ModalityAudio) ||
			configured.Capabilities.Media.SupportsOutput(model.ModalityImage) ||
			configured.Capabilities.Media.SupportsOutput(model.ModalityAudio) {
			return nil, fmt.Errorf("openaicompat: model %q declares media unsupported by the Chat Completions adapter", configured.ID)
		}
		needsArtifactStore = needsArtifactStore || configured.Capabilities.Media.SupportsInput(model.ModalityImage)
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
	if config.RetryBackoff < 0 || config.RequestTimeout < 0 || config.MaxEventBytes < 0 || config.MaxOutputBytes < 0 || config.MaxAttachmentBytes < 0 {
		return nil, errors.New("openaicompat: timeouts and size limits cannot be negative")
	}
	if needsArtifactStore && nilArtifactStore(config.ArtifactStore) && !allowMissingArtifactStore {
		return nil, errors.New("openaicompat: ArtifactStore is required by an image-capable model")
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
	maxAttachmentBytes := config.MaxAttachmentBytes
	if maxAttachmentBytes == 0 {
		maxAttachmentBytes = 20 << 20
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
		maxAttachmentBytes: maxAttachmentBytes, artifacts: config.ArtifactStore,
	}, nil
}

func (e *Executor) Execute(ctx context.Context, request model.ModelRequest, recorder model.AttemptRecorder) (model.ModelStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	capabilities, err := e.Inspect(ctx, request.Config)
	if err != nil {
		return nil, err
	}
	if recorder == nil {
		return nil, agent.NewError(agent.ErrorInvalidInput, "openaicompat.execute", "AttemptRecorder is required", nil)
	}
	payload, err := buildRequest(ctx, request, capabilities, e.artifacts, e.maxAttachmentBytes)
	if err != nil {
		return nil, agent.NewError(agent.ErrorInvalidInput, "openaicompat.execute", "invalid logical model request", err)
	}
	inputTokenEstimate, err := e.CountTokens(ctx, request)
	if err != nil {
		return nil, err
	}
	return newStream(ctx, e, request, payload, inputTokenEstimate, recorder), nil
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
	capabilities, err := e.Inspect(ctx, request.Config)
	if err != nil {
		return 0, err
	}
	estimateRequest, imageTokens, err := requestForTokenEstimate(ctx, request, e.artifacts, e.maxAttachmentBytes)
	if err != nil {
		return 0, agent.NewError(agent.ErrorInvalidInput, "openaicompat.count_tokens", "invalid logical model request", err)
	}
	payload, err := buildRequest(ctx, estimateRequest, capabilities, nil, e.maxAttachmentBytes)
	if err != nil {
		return 0, agent.NewError(agent.ErrorInvalidInput, "openaicompat.count_tokens", "invalid logical model request", err)
	}
	// Text and schemas retain the conservative byte-count estimate used by this
	// tokenizer-independent adapter. Images are estimated from decoded geometry;
	// counting base64 transport bytes would make ordinary images exceed the
	// context window before a provider ever sees them.
	return len(payload) + imageTokens, nil
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
	executor, err := newExecutor(config, true)
	if err != nil {
		return nil, err
	}
	return executorModule{config: config, catalog: executor, needsArtifactStore: executorNeedsArtifactStore(executor)}, nil
}

type executorModule struct {
	config             Config
	catalog            *Executor
	needsArtifactStore bool
}

func (m executorModule) ID() string { return "model.openaicompat." + m.catalog.providerKey }

func (m executorModule) RequiredSlots() []agentslot.Requirement {
	if !m.needsArtifactStore {
		return nil
	}
	return []agentslot.Requirement{agentslot.RequireOne(artifact.StoreSlot)}
}

func (m executorModule) Register(reg agentslot.Registrar) error {
	executorContribution := agentslot.Set(model.ExecutorSlot, model.ModelExecutor(m.catalog))
	if m.needsArtifactStore {
		executorContribution = agentslot.SetWith(model.ExecutorSlot, func(resolver agentslot.Resolver) (model.ModelExecutor, error) {
			store, err := agentslot.ResolveOne(resolver, artifact.StoreSlot)
			if err != nil {
				return nil, err
			}
			config := m.config
			config.ArtifactStore = store
			return New(config)
		})
	}
	return reg.Contribute(
		executorContribution,
		agentslot.Add(model.CatalogSlot, m.catalog.providerKey, model.ModelCatalog(m.catalog)),
	)
}

func executorNeedsArtifactStore(executor *Executor) bool {
	for _, configured := range executor.models {
		if configured.Capabilities.Media.SupportsInput(model.ModalityImage) {
			return true
		}
	}
	return false
}

func nilArtifactStore(store artifact.ArtifactStore) bool {
	if store == nil {
		return true
	}
	value := reflect.ValueOf(store)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
