package standardagent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/interaction/inprocess"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestSlashMenuAndStructuredClientsShareOneGatewayCommandBackend(t *testing.T) {
	defaultModel := model.Config{
		ProviderKey: "provider-a", ModelID: "model-a", Reasoning: model.ReasoningDefault,
	}
	memory := session.NewMemoryModule()
	catalog, err := model.NewStaticCatalog(model.Descriptor{
		ProviderKey: "provider-b", ModelID: "model-b", Title: "Model B",
		Capabilities: textCapabilities(128_000),
	})
	if err != nil {
		t.Fatal(err)
	}
	functionEntry := inprocess.New()
	menuEntry := inprocess.New()
	application := NewApplication(ApplicationSpec{
		Name: "round8-interaction", DefaultModelConfig: defaultModel,
		Modules: []agentslot.Module{
			memory,
			executorModule{executor: model.NewFakeModelExecutor()},
			modelCatalogModule{key: "provider-b", catalog: catalog},
			interaction.NewModelCommandModule(),
			NewEntrypointModule("entrypoint.function", "function", functionEntry),
			NewEntrypointModule("entrypoint.menu", "menu", menuEntry),
		},
	})
	running, err := application.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := running.Stop(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	functionAccess, err := functionEntry.Access()
	if err != nil {
		t.Fatal(err)
	}
	menuAccess, err := menuEntry.Access()
	if err != nil {
		t.Fatal(err)
	}
	if functionAccess != menuAccess {
		t.Fatal("Entrypoints did not receive the same framework-owned Gateway binding")
	}
	functionCommands, err := functionAccess.Commands(context.Background(), interaction.CommandScope{})
	if err != nil {
		t.Fatal(err)
	}
	menuCommands, err := menuAccess.Commands(context.Background(), interaction.CommandScope{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(functionCommands, menuCommands) || len(functionCommands) != 1 || functionCommands[0].Key != interaction.ModelCommandKey {
		t.Fatalf("shared command catalogs = %#v / %#v", functionCommands, menuCommands)
	}

	opened, err := functionAccess.CreateSession(context.Background(), interaction.CreateSessionRequest{
		AgentID: "agent-1", WorkspaceID: "workspace-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := interaction.CommandScope{AgentID: "agent-1", WorkspaceID: "workspace-1", SessionID: opened.SessionID}
	slashResult, err := invokeExactSlash(context.Background(), functionAccess, scope, "/model", opened.Revision, nil)
	if err != nil {
		t.Fatalf("slash query: %v", err)
	}
	menuResult, err := invokeMenuCommand(context.Background(), menuAccess, scope, interaction.ModelCommandKey, opened.Revision, nil)
	if err != nil {
		t.Fatalf("menu query: %v", err)
	}
	structuredResult, err := functionAccess.InvokeCommand(context.Background(), interaction.CommandInvocation{
		Scope: scope, Key: interaction.ModelCommandKey, ExpectedRevision: opened.Revision,
	})
	if err != nil {
		t.Fatalf("structured query: %v", err)
	}
	if string(slashResult.Data) != string(menuResult.Data) || string(slashResult.Data) != string(structuredResult.Data) {
		t.Fatalf("renderers observed different command data:\nslash=%s\nmenu=%s\nstructured=%s", slashResult.Data, menuResult.Data, structuredResult.Data)
	}

	arguments, err := json.Marshal(interaction.ModelCommandArguments{
		ProviderKey: "provider-b", ModelID: "model-b", Reasoning: model.ReasoningHigh,
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := invokeMenuCommand(context.Background(), menuAccess, scope, interaction.ModelCommandKey, opened.Revision, arguments)
	if err != nil {
		t.Fatalf("menu update: %v", err)
	}
	current, err := functionAccess.ModelConfig(context.Background(), interaction.ModelConfigRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != current.Revision || current.Config.ProviderKey != "provider-b" || current.Config.ModelID != "model-b" || current.Config.Reasoning != model.ReasoningHigh {
		t.Fatalf("shared backend model config = %#v, command revision %d", current, updated.Revision)
	}
}

// These helpers represent renderers, not alternate Agent backends. Slash is
// an explicit user protocol, so deterministic parsing is intentional here.
func invokeExactSlash(ctx context.Context, access interaction.GatewayAccess, scope interaction.CommandScope, slash string, revision agent.Revision, arguments json.RawMessage) (interaction.CommandResult, error) {
	key, ok := strings.CutPrefix(slash, "/")
	if !ok || key == "" || strings.ContainsAny(key, " \t\r\n") {
		return interaction.CommandResult{}, invalidInput("test.slash", "invalid explicit slash command")
	}
	return access.InvokeCommand(ctx, interaction.CommandInvocation{Scope: scope, Key: key, ExpectedRevision: revision, Arguments: arguments})
}

func invokeMenuCommand(ctx context.Context, access interaction.GatewayAccess, scope interaction.CommandScope, key string, revision agent.Revision, arguments json.RawMessage) (interaction.CommandResult, error) {
	return access.InvokeCommand(ctx, interaction.CommandInvocation{Scope: scope, Key: key, ExpectedRevision: revision, Arguments: arguments})
}

type modelCatalogModule struct {
	key     string
	catalog model.ModelCatalog
}

func (m modelCatalogModule) ID() string { return "test.model-catalog." + m.key }

func (m modelCatalogModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Add(model.CatalogSlot, m.key, m.catalog))
}
