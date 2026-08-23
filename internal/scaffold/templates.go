package scaffold

const mainTemplate = `package main

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/signal"
{{- if .Approval }}
	"strconv"
{{- end }}
	"syscall"
{{- if .Shell }}
	"time"
{{- end }}

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/credential"
	"github.com/LyleLiu666/agentSlot/interaction/cli"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/model/openaicompat"
	"github.com/LyleLiu666/agentSlot/session"
	"github.com/LyleLiu666/agentSlot/standardagent"
{{- if .Artifact }}
	artifactfile "github.com/LyleLiu666/agentSlot/artifact/file"
{{- end }}
{{- if or .Policy .Approval }}
	"github.com/LyleLiu666/agentSlot/policy"
{{- end }}
{{- if .Shell }}
	"github.com/LyleLiu666/agentSlot/tool/bash"
{{- end }}
{{- if .Files }}
	"github.com/LyleLiu666/agentSlot/tool/files"
{{- end }}
{{- if .History }}
	"github.com/LyleLiu666/agentSlot/tool/sessionhistory"
{{- end }}
{{- if .Workspace }}
	"github.com/LyleLiu666/agentSlot/workspace"
	localworkspace "github.com/LyleLiu666/agentSlot/workspace/local"
{{- end }}
)

type config struct {
	providerKey, providerURL, modelID string
	credentialRef, credentialFile string
	sessionDirectory string
{{- if .Workspace }}
	workspace string
{{- end }}
{{- if .Artifact }}
	artifactDirectory string
{{- end }}
	input io.ReadCloser
	output, errorOutput io.Writer
}

func defaultConfig() config {
	return config{
		providerKey: {{printf "%q" .Options.ProviderKey}}, providerURL: {{printf "%q" .Options.ProviderURL}}, modelID: {{printf "%q" .Options.ModelID}},
		credentialRef: {{printf "%q" .Options.CredentialRef}}, credentialFile: {{printf "%q" .Options.CredentialFile}},
		sessionDirectory: {{printf "%q" .Options.SessionDirectory}},
{{- if .Workspace }}
		workspace: {{printf "%q" .Options.Workspace}},
{{- end }}
{{- if .Artifact }}
		artifactDirectory: {{printf "%q" .Options.ArtifactDirectory}},
{{- end }}
		input: os.Stdin, output: os.Stdout, errorOutput: os.Stderr,
	}
}

func buildApplication(cfg config) (*agentslot.Application, *cli.Channel, error) {
	channel, err := cli.New(cli.Config{
		AgentID: "generated-agent", WorkspaceID: "default", Actor: agent.ActorIdentity{Kind: agent.ActorLocalUser, ID: "generated-cli"},
		Input: cfg.input, Output: cfg.output, ErrorOutput: cfg.errorOutput,
	})
	if err != nil { return nil, nil, err }
	sessions, err := session.NewFileModule(cfg.sessionDirectory)
	if err != nil { return nil, nil, err }
	resolver, err := credential.NewEncryptedFileResolver(cfg.credentialFile, func(ctx context.Context) ([]byte, error) {
		if err := ctx.Err(); err != nil { return nil, err }
		encoded := os.Getenv({{printf "%q" .Options.CredentialKeyEnvironment}})
		key, err := hex.DecodeString(encoded)
		if err != nil || len(key) != 32 { return nil, errors.New("AGENTSLOT_CREDENTIAL_KEY_HEX must contain one 32-byte key") }
		return key, nil
	})
	if err != nil { return nil, nil, err }
	credentialModule, err := credential.NewModule("generated.credential", resolver)
	if err != nil { return nil, nil, err }
	provider, err := openaicompat.NewModule(openaicompat.Config{
		ProviderKey: cfg.providerKey, BaseURL: cfg.providerURL, CredentialRef: credential.Ref{ID: cfg.credentialRef},
		Models: []openaicompat.Model{ {ID: cfg.modelID, Title: cfg.modelID, Capabilities: model.ExecutionCapabilities{
			Media: model.Capabilities{InputModalities: []model.Modality{model.ModalityText}, OutputModalities: []model.Modality{model.ModalityText}, ToolCalling: {{if or .Files .Shell .History}}true{{else}}false{{end}}},
			Reasoning: []model.Reasoning{model.ReasoningDefault}, ContextWindowTokens: 128000, MaxOutputTokens: 16384,
		} }},
	})
	if err != nil { return nil, nil, err }
	modules := []agentslot.Module{
		sessions, credentialModule, provider,
		standardagent.NewGatewayChannelModule("generated.channel.cli", "cli", channel),
	}
{{- if .Workspace }}
	workspaceModule, err := localworkspace.NewModule(localworkspace.Binding{Scope: workspace.Scope{AgentID: "generated-agent", WorkspaceID: "default"}, RootDirectory: cfg.workspace})
	if err != nil { return nil, nil, err }
	modules = append(modules, workspaceModule)
{{- end }}
{{- if .Artifact }}
	artifactModule, err := artifactfile.NewModule(cfg.artifactDirectory)
	if err != nil { return nil, nil, err }
	modules = append(modules, artifactModule)
{{- end }}
{{- if .Files }}
	fileTools, err := files.NewModule(files.Config{RootDirectory: cfg.workspace, MaxReadBytes: 4 << 20, MaxWriteBytes: 4 << 20})
	if err != nil { return nil, nil, err }
	modules = append(modules, fileTools)
{{- end }}
{{- if .Shell }}
	shellTool, err := bash.NewModule(bash.Config{
		WorkingDirectory: cfg.workspace, Environment: map[string]string{"PATH": "/usr/bin:/bin", "LANG": "C"},
		Timeout: 2 * time.Minute, MaxStdoutBytes: 4 << 20, MaxStderrBytes: 4 << 20,
	})
	if err != nil { return nil, nil, err }
	modules = append(modules, shellTool)
{{- end }}
{{- if .History }}
	modules = append(modules, sessionhistory.NewModule(sessionhistory.Config{Scope: sessionhistory.ScopeSameWorkspace}))
{{- end }}
{{- if .Policy }}
	guard, err := policy.NewToolRuleGuard(
		policy.Decision{Effect: policy.RequireApproval, Reason: "tool may change Workspace or external state"},
{{- if .Files }}
		policy.ToolRule{ToolKey: files.ReadKey, Decision: policy.Decision{Effect: policy.Allow}},
{{- end }}
{{- if .History }}
		policy.ToolRule{ToolKey: sessionhistory.Key, Decision: policy.Decision{Effect: policy.Allow}},
{{- end }}
	)
	if err != nil { return nil, nil, err }
	modules = append(modules, generatedGuardModule{guard: guard})
{{- end }}
{{- if .Approval }}
	modules = append(modules, generatedApprovalModule{approval: policy.ApprovalFunc(func(context.Context, policy.ApprovalRequest) (policy.ApprovalDecision, error) {
		approved, _ := strconv.ParseBool(os.Getenv({{printf "%q" .Options.ApprovalEnvironment}}))
		return policy.ApprovalDecision{Approved: approved, Reason: {{printf "%q" .Options.ApprovalEnvironment}}}, nil
	})})
{{- end }}
	application := standardagent.NewApplication(standardagent.ApplicationSpec{
		Name: "generated-agent", DefaultModelConfig: model.Config{ProviderKey: cfg.providerKey, ModelID: cfg.modelID, Reasoning: model.ReasoningDefault},
		RuntimeConfig: standardagent.AgentRuntimeConfig{SystemPrompt: "You are a careful assistant.", ToolKeys: []string{ {{.ToolKeys}} }, ContextRetentionMode: standardagent.ContextLatestOnly, {{if or .Files .Shell .History}}MaxInlineToolResultBytes: 64 << 10,{{end}}},
		Modules: modules,
	})
	return application, channel, nil
}

{{- if .Policy }}
type generatedGuardModule struct { guard policy.PolicyGuard }
func (generatedGuardModule) ID() string { return "generated.policy.guard" }
func (m generatedGuardModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Append(policy.GuardSlot, m.guard))
}
{{- end }}
{{- if .Approval }}
type generatedApprovalModule struct { approval policy.ApprovalService }
func (generatedApprovalModule) ID() string { return "generated.approval" }
func (m generatedApprovalModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Set(policy.ApprovalSlot, m.approval))
}
{{- end }}

func main() {
	application, channel, err := buildApplication(defaultConfig())
	if err != nil { panic(err) }
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	running, err := application.Start(ctx)
	if err != nil { panic(err) }
	select { case <-ctx.Done(): case <-channel.Done(): }
	if err := running.Stop(context.Background()); err != nil { panic(err) }
}
`

const testTemplate = `package main

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneratedAssemblyBuilds(t *testing.T) {
	cfg := defaultConfig()
	root := t.TempDir()
	cfg.sessionDirectory = filepath.Join(root, "sessions")
	cfg.credentialFile = filepath.Join(root, "credentials.enc")
{{- if .Workspace }}
	cfg.workspace = t.TempDir()
{{- end }}
{{- if .Artifact }}
	cfg.artifactDirectory = filepath.Join(root, "artifacts")
{{- end }}
	cfg.input = io.NopCloser(strings.NewReader("/quit\n"))
	cfg.output = &bytes.Buffer{}
	cfg.errorOutput = &bytes.Buffer{}
	application, _, err := buildApplication(cfg)
	if err != nil { t.Fatal(err) }
	if _, err := application.Build(); err != nil { t.Fatal(err) }
}
`

const readmeTemplate = `# {{.Preset.Title}}

Generated by ` + "`agentslot init`" + ` from ComponentCatalog preset ` + "`{{.Preset.ID}}`" + `.

- AgentSlot version: ` + "`{{.Options.AgentSlotVersion}}`" + `
- Selected implementations: {{.ImplementationIDs}}
- Standard profile requirements: {{.ProfileRequirements}}
- Provider/model and ` + "`CredentialRef`" + ` are explicit in ` + "`main.go`" + `.
- No credential value was read or written by the generator.
{{- if or .Files .Shell }}
- Read-only file and Session-history tools are allowed; writes and shell commands require approval.
{{- end }}
{{- if .Workspace }}
- Session, Artifact, and encrypted credential storage defaults are outside the Workspace boundary.
{{- end }}

Before running, provision the encrypted credential file and set ` + "`{{.Options.CredentialKeyEnvironment}}`" + `.
{{- if .Approval }}
Set ` + "`{{.Options.ApprovalEnvironment}}=true`" + ` only when external side effects should be approved.
{{- end }}
`
