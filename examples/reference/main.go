package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/credential"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/interaction/cli"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/model/openaicompat"
	"github.com/LyleLiu666/agentSlot/observe/jsonlines"
	"github.com/LyleLiu666/agentSlot/policy"
	"github.com/LyleLiu666/agentSlot/session"
	"github.com/LyleLiu666/agentSlot/standardagent"
	"github.com/LyleLiu666/agentSlot/tool/bash"
	"github.com/LyleLiu666/agentSlot/tool/files"
	httptool "github.com/LyleLiu666/agentSlot/tool/http"
	"github.com/LyleLiu666/agentSlot/tool/sessionhistory"
)

type referenceConfig struct {
	providerKey          string
	providerURL          string
	credentialToken      []byte
	modelID              string
	workspace            string
	sessionDir           string
	httpHosts            []string
	approveEffects       bool
	sessionID            agent.SessionID
	contextRetentionMode standardagent.ContextRetentionMode
	maxTokensPerRun      int64
	actor                agent.ActorIdentity
	input                io.ReadCloser
	output               io.Writer
	errorOutput          io.Writer
	observationOut       io.Writer
}

func buildReference(config referenceConfig) (*agentslot.Application, *cli.Channel, error) {
	return buildReferenceWithChannels(config)
}

func buildReferenceWithChannels(config referenceConfig, additionalChannels ...agentslot.Module) (*agentslot.Application, *cli.Channel, error) {
	agentID := agent.AgentID("reference-agent")
	workspaceID := agent.WorkspaceID("local-workspace")
	if config.sessionID.Valid() {
		agentID, workspaceID = "", ""
	}
	cliConfig := cli.Config{
		AgentID: agentID, WorkspaceID: workspaceID, SessionID: config.sessionID,
		Actor: config.actor, Input: config.input, Output: config.output, ErrorOutput: config.errorOutput,
	}
	channel, err := cli.New(cliConfig)
	if err != nil {
		return nil, nil, err
	}
	channels := []agentslot.Module{standardagent.NewGatewayChannelModule("reference.channel.cli", "cli", channel)}
	channels = append(channels, additionalChannels...)
	application, err := buildReferenceApplication(config, channels...)
	if err != nil {
		return nil, nil, err
	}
	return application, channel, nil
}

func buildReferenceApplication(config referenceConfig, channels ...agentslot.Module) (*agentslot.Application, error) {
	if config.providerKey == "" || config.providerURL == "" || config.modelID == "" {
		return nil, errors.New("reference: provider key, URL, and model ID are required")
	}
	if pathWithin(config.sessionDir, config.workspace) {
		return nil, errors.New("reference: Session storage must be outside workspace tool access")
	}
	if !config.contextRetentionMode.Valid() {
		return nil, errors.New("reference: ContextRetentionMode must be explicit and valid")
	}
	if config.maxTokensPerRun < 0 {
		return nil, errors.New("reference: MaxTokensPerRun cannot be negative")
	}
	if !config.actor.Valid() {
		return nil, errors.New("reference: ActorIdentity must be explicit and valid")
	}
	if len(channels) == 0 {
		return nil, errors.New("reference: at least one GatewayChannel module is required")
	}
	defaultModel := model.Config{ProviderKey: config.providerKey, ModelID: config.modelID, Reasoning: model.ReasoningDefault}
	sessions, err := session.NewFileModule(config.sessionDir)
	if err != nil {
		return nil, err
	}
	providerConfig := openaicompat.Config{
		ProviderKey: config.providerKey, BaseURL: config.providerURL,
		Models: []openaicompat.Model{{
			ID: config.modelID, Title: config.modelID,
			Capabilities: model.ExecutionCapabilities{
				Media: model.Capabilities{
					InputModalities: []model.Modality{model.ModalityText}, OutputModalities: []model.Modality{model.ModalityText}, ToolCalling: true,
				},
				Reasoning:           []model.Reasoning{model.ReasoningDefault},
				ContextWindowTokens: 128_000, MaxOutputTokens: 16_384,
			},
		}},
		MaxAttempts: 3, RetryBackoff: 250 * time.Millisecond, RequestTimeout: 2 * time.Minute,
	}
	var credentialModule agentslot.Module
	if len(config.credentialToken) > 0 {
		ref := credential.Ref{ID: "reference-provider"}
		resolver, err := credential.NewMemoryResolver(credential.Record{
			Ref: ref, Identity: credential.Identity{Fingerprint: "reference-provider-configured"},
			Material: credential.Material{Kind: credential.KindBearer, Token: config.credentialToken},
		})
		if err != nil {
			return nil, err
		}
		credentialModule, err = credential.NewModule("reference.credential", resolver)
		if err != nil {
			return nil, err
		}
		providerConfig.CredentialRef = ref
	}
	provider, err := openaicompat.NewModule(providerConfig)
	if err != nil {
		return nil, err
	}
	bashTool, err := bash.NewModule(bash.Config{
		WorkingDirectory: config.workspace, Environment: map[string]string{"PATH": "/usr/bin:/bin", "LANG": "C"},
		Timeout: 30 * time.Second, MaxStdoutBytes: 1 << 20, MaxStderrBytes: 1 << 20,
	})
	if err != nil {
		return nil, err
	}
	fileTools, err := files.NewModule(files.Config{RootDirectory: config.workspace, MaxReadBytes: 4 << 20, MaxWriteBytes: 4 << 20})
	if err != nil {
		return nil, err
	}
	httpTool, err := httptool.NewModule(httptool.Config{
		AllowedHosts: config.httpHosts, AllowedMethods: []string{http.MethodGet, http.MethodHead},
		Timeout: 30 * time.Second, MaxRequestBytes: 64 << 10, MaxResponseBytes: 4 << 20,
	})
	if err != nil {
		return nil, err
	}
	guard, err := policy.NewToolRuleGuard(
		policy.Decision{Effect: policy.RequireApproval, Reason: "tool can change workspace or external state"},
		policy.ToolRule{ToolKey: files.ReadKey, Decision: policy.Decision{Effect: policy.Allow}},
	)
	if err != nil {
		return nil, err
	}
	policyModule := referencePolicyModule{
		guard: guard,
		approval: policy.ApprovalFunc(func(context.Context, policy.ApprovalRequest) (policy.ApprovalDecision, error) {
			return policy.ApprovalDecision{Approved: config.approveEffects, Reason: "configured reference-agent decision"}, nil
		}),
	}
	observations, err := jsonlines.NewModule("reference.observations", config.observationOut)
	if err != nil {
		return nil, err
	}
	modules := []agentslot.Module{
		sessions, provider, bashTool, fileTools, httpTool, sessionhistory.NewModule(sessionhistory.Config{Scope: sessionhistory.ScopeSameWorkspace}), policyModule, observations,
		interaction.NewModelCommandModule(),
	}
	if credentialModule != nil {
		modules = append([]agentslot.Module{credentialModule}, modules...)
	}
	modules = append(modules, channels...)
	application := standardagent.NewApplication(standardagent.ApplicationSpec{
		Name: "reference-agent", DefaultModelConfig: defaultModel,
		RuntimeConfig: standardagent.AgentRuntimeConfig{
			SystemPrompt:             "You are a careful coding agent. Use installed tools only when they materially help the user.",
			ToolKeys:                 []string{bash.Key, files.ReadKey, files.WriteKey, files.EditKey, httptool.Key, sessionhistory.Key},
			ContextRetentionMode:     config.contextRetentionMode,
			MaxTokensPerRun:          config.maxTokensPerRun,
			MaxInlineToolResultBytes: 64 << 10,
		},
		Modules: modules,
	})
	return application, nil
}

type referencePolicyModule struct {
	guard    policy.PolicyGuard
	approval policy.ApprovalService
}

func (referencePolicyModule) ID() string { return "reference.policy" }
func (m referencePolicyModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(
		agentslot.Append(policy.GuardSlot, m.guard),
		agentslot.Set(policy.ApprovalSlot, m.approval),
	)
}

func main() {
	workspace, err := filepath.Abs(environment("AGENTSLOT_WORKSPACE", "."))
	if err != nil {
		log.Fatal(err)
	}
	configDirectory, err := os.UserConfigDir()
	if err != nil {
		log.Fatal(err)
	}
	hosts := splitNonEmpty(environment("AGENTSLOT_HTTP_HOSTS", "api.github.com"))
	config := referenceConfig{
		providerKey:     "openai-compatible",
		providerURL:     environment("AGENTSLOT_PROVIDER_URL", "https://api.openai.com/v1"),
		credentialToken: []byte(os.Getenv("AGENTSLOT_API_KEY")), modelID: environment("AGENTSLOT_MODEL", "gpt-4.1-mini"),
		workspace: workspace, sessionDir: environment("AGENTSLOT_SESSION_DIR", filepath.Join(configDirectory, "agentslot", "reference", "sessions")),
		httpHosts: hosts, approveEffects: environmentBoolean("AGENTSLOT_APPROVE_EFFECTS"),
		contextRetentionMode: standardagent.ContextLatestOnly, maxTokensPerRun: 0,
		actor: agent.ActorIdentity{Kind: agent.ActorLocalUser, ID: "reference-cli"},
		input: os.Stdin, output: os.Stdout, errorOutput: os.Stderr, observationOut: os.Stderr,
	}
	defer clear(config.credentialToken)
	application, channel, err := buildReference(config)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	running, err := application.Start(ctx)
	if err != nil {
		log.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-channel.Done():
	}
	stopContext, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	if err := running.Stop(stopContext); err != nil {
		log.Fatal(err)
	}
	if err := channel.Err(); err != nil {
		log.Fatal(err)
	}
}

func pathWithin(candidate, root string) bool {
	if candidate == "" || root == "" {
		return false
	}
	candidate, candidateErr := filepath.Abs(candidate)
	root, rootErr := filepath.Abs(root)
	if candidateErr != nil || rootErr != nil {
		return false
	}
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func environment(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func environmentBoolean(key string) bool {
	value, err := strconv.ParseBool(os.Getenv(key))
	return err == nil && value
}

func splitNonEmpty(value string) []string {
	var values []string
	for _, candidate := range strings.Split(value, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate != "" {
			values = append(values, candidate)
		}
	}
	return values
}
