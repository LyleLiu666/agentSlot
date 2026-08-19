package interaction_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/interaction"
)

type entrypoint struct{}

func (entrypoint) Attach(interaction.GatewayAccess) error { return nil }

type command struct{}

func (command) Describe() interaction.CommandDescriptor {
	return interaction.CommandDescriptor{Key: "example"}
}

func TestEntrypointRejectsDuplicateAdapterKey(t *testing.T) {
	builder := agentslot.NewBuilder()
	if err := builder.Install(module{}); err != nil {
		t.Fatalf("install first: %v", err)
	}
	err := builder.Install(duplicateEntrypointModule{})
	if !errors.Is(err, agentslot.ErrDuplicateKey) {
		t.Fatalf("duplicate entrypoint error = %v, want ErrDuplicateKey", err)
	}
}

type duplicateEntrypointModule struct{}

func (duplicateEntrypointModule) ID() string { return "interaction.duplicate" }
func (duplicateEntrypointModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Add(interaction.EntrypointSlot, "test", interaction.Entrypoint(entrypoint{})))
}
func (command) Invoke(context.Context, interaction.CommandInvocation, interaction.CommandActions) (interaction.CommandResult, error) {
	return interaction.CommandResult{}, nil
}

var _ interaction.Entrypoint = entrypoint{}
var _ interaction.InteractionCommand = command{}

type module struct{}

func (module) ID() string { return "interaction.contracts" }
func (module) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Add(interaction.EntrypointSlot, "test", interaction.Entrypoint(entrypoint{})))
}

type commandModule struct{}

func (commandModule) ID() string { return "interaction.command.contracts" }
func (commandModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Add(interaction.CommandSlot, "example", interaction.InteractionCommand(command{})))
}

func TestEntrypointIsKeyedManySlot(t *testing.T) {
	builder := agentslot.NewBuilder()
	if err := builder.Install(module{}); err != nil {
		t.Fatalf("install: %v", err)
	}
	assembly, err := builder.Build(agentslot.RequireMany(interaction.EntrypointSlot, 1))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, ok := agentslot.Lookup(assembly, interaction.EntrypointSlot, "test"); !ok {
		t.Fatal("interaction.entrypoint contribution missing")
	}
}

func TestInteractionCommandIsAKeyedManySlot(t *testing.T) {
	builder := agentslot.NewBuilder()
	if err := builder.Install(commandModule{}); err != nil {
		t.Fatalf("install: %v", err)
	}
	assembly, err := builder.Build(agentslot.RequireKey(interaction.CommandSlot, "example"))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, ok := agentslot.Lookup(assembly, interaction.CommandSlot, "example"); !ok {
		t.Fatal("interaction.command contribution missing")
	}
}

func TestInteractionCommandRejectsDuplicateKey(t *testing.T) {
	builder := agentslot.NewBuilder()
	if err := builder.Install(commandModule{}); err != nil {
		t.Fatalf("install first: %v", err)
	}
	err := builder.Install(secondCommandModule{})
	if !errors.Is(err, agentslot.ErrDuplicateKey) {
		t.Fatalf("duplicate command error = %v, want ErrDuplicateKey", err)
	}
}

type secondCommandModule struct{}

func (secondCommandModule) ID() string { return "interaction.command.contracts.second" }
func (secondCommandModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Add(interaction.CommandSlot, "example", interaction.InteractionCommand(command{})))
}

func TestCommandInvocationUsesStructuredArguments(t *testing.T) {
	invocation := interaction.CommandInvocation{Key: "model", Arguments: json.RawMessage(`{"model":"example"}`)}
	if string(invocation.Arguments) != `{"model":"example"}` {
		t.Fatalf("arguments = %s", invocation.Arguments)
	}
}

func TestGatewayAccessDoesNotExposeRuntimeOrStoreTypes(t *testing.T) {
	access := reflect.TypeOf((*interaction.GatewayAccess)(nil)).Elem()
	for index := 0; index < access.NumMethod(); index++ {
		method := access.Method(index)
		signature := method.Type.String()
		for _, forbidden := range []string{"AgentRuntime", "RuntimeAccess", "SessionStore"} {
			if strings.Contains(signature, forbidden) {
				t.Fatalf("GatewayAccess.%s exposes %s: %s", method.Name, forbidden, signature)
			}
		}
	}
}
