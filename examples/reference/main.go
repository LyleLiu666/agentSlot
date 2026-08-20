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
)

type referenceConfig struct {
	providerKey    string
	providerURL    string
	apiKey         string
	modelID        string
	workspace      string
	sessionDir     string
	httpHosts      []string
	approveEffects bool
	sessionID      agent.SessionID
	input          io.ReadCloser
	output         io.Writer
	errorOutput    io.Writer
	observationOut io.Writer
}

func buildReference(config referenceConfig) (*agentslot.Application, *cli.Entrypoint, error) {
	if config.providerKey == "" || config.providerURL == "" || config.modelID == "" {
		return nil, nil, errors.New("reference: provider key, URL, and model ID are required")
	}
	if pathWithin(config.sessionDir, config.workspace) {
		return nil, nil, errors.New("reference: Session storage must be outside workspace tool access")
	}
	defaultModel := model.Config{ProviderKey: config.providerKey, ModelID: config.modelID, Reasoning: model.ReasoningDefault}
	sessions, err := session.NewFileModule(config.sessionDir)
	if err != nil {
		return nil, nil, err
	}
	provider, err := openaicompat.NewModule(openaicompat.Config{
		ProviderKey: config.providerKey, BaseURL: config.providerURL, APIKey: config.apiKey,
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
	})
	if err != nil {
		return nil, nil, err
	}
	bashTool, err := bash.NewModule(bash.Config{
		WorkingDirectory: config.workspace, Environment: map[string]string{"PATH": "/usr/bin:/bin", "LANG": "C"},
		Timeout: 30 * time.Second, MaxStdoutBytes: 1 << 20, MaxStderrBytes: 1 << 20,
	})
	if err != nil {
		return nil, nil, err
	}
	fileTools, err := files.NewModule(files.Config{RootDirectory: config.workspace, MaxReadBytes: 4 << 20, MaxWriteBytes: 4 << 20})
	if err != nil {
		return nil, nil, err
	}
	httpTool, err := httptool.NewModule(httptool.Config{
		AllowedHosts: config.httpHosts, AllowedMethods: []string{http.MethodGet, http.MethodHead},
		Timeout: 30 * time.Second, MaxRequestBytes: 64 << 10, MaxResponseBytes: 4 << 20,
	})
	if err != nil {
		return nil, nil, err
	}
	guard, err := policy.NewToolRuleGuard(
		policy.Decision{Effect: policy.RequireApproval, Reason: "tool can change workspace or external state"},
		policy.ToolRule{ToolKey: files.ReadKey, Decision: policy.Decision{Effect: policy.Allow}},
	)
	if err != nil {
		return nil, nil, err
	}
	policyModule := referencePolicyModule{
		guard: guard,
		approval: policy.ApprovalFunc(func(context.Context, policy.ApprovalRequest) (policy.ApprovalDecision, error) {
			return policy.ApprovalDecision{Approved: config.approveEffects, Reason: "configured reference-agent decision"}, nil
		}),
	}
	observations, err := jsonlines.NewModule("reference.observations", config.observationOut)
	if err != nil {
		return nil, nil, err
	}
	cliConfig := cli.Config{
		AgentID: "reference-agent", WorkspaceID: "local-workspace", SessionID: config.sessionID,
		Input: config.input, Output: config.output, ErrorOutput: config.errorOutput,
	}
	if config.sessionID.Valid() {
		cliConfig.AgentID, cliConfig.WorkspaceID = "", ""
	}
	entrypoint, err := cli.New(cliConfig)
	if err != nil {
		return nil, nil, err
	}
	application := standardagent.NewApplication(standardagent.ApplicationSpec{
		Name: "reference-agent", DefaultModelConfig: defaultModel,
		RuntimeConfig: standardagent.AgentRuntimeConfig{
			SystemPrompt: "You are a careful coding agent. Use installed tools only when they materially help the user.",
			ToolKeys:     []string{bash.Key, files.ReadKey, files.WriteKey, files.EditKey, httptool.Key},
		},
		Modules: []agentslot.Module{
			sessions, provider, bashTool, fileTools, httpTool, policyModule, observations,
			interaction.NewModelCommandModule(),
			standardagent.NewEntrypointModule("reference.entrypoint.cli", "cli", entrypoint),
		},
	})
	return application, entrypoint, nil
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
		providerKey: "openai-compatible",
		providerURL: environment("AGENTSLOT_PROVIDER_URL", "https://api.openai.com/v1"),
		apiKey:      os.Getenv("AGENTSLOT_API_KEY"), modelID: environment("AGENTSLOT_MODEL", "gpt-4.1-mini"),
		workspace: workspace, sessionDir: environment("AGENTSLOT_SESSION_DIR", filepath.Join(configDirectory, "agentslot", "reference", "sessions")),
		httpHosts: hosts, approveEffects: environmentBoolean("AGENTSLOT_APPROVE_EFFECTS"),
		input: os.Stdin, output: os.Stdout, errorOutput: os.Stderr, observationOut: os.Stderr,
	}
	application, entrypoint, err := buildReference(config)
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
	case <-entrypoint.Done():
	}
	stopContext, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	if err := running.Stop(stopContext); err != nil {
		log.Fatal(err)
	}
	if err := entrypoint.Err(); err != nil {
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
