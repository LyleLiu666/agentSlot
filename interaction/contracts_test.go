package interaction_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
)

type channel struct{}

func (channel) Bind(interaction.GatewayAccess) error { return nil }

type command struct{}

func (command) Describe() interaction.CommandDescriptor {
	return interaction.CommandDescriptor{Key: "example"}
}

func TestGatewayChannelRejectsDuplicateAdapterKey(t *testing.T) {
	builder := agentslot.NewBuilder()
	if err := builder.Install(module{}); err != nil {
		t.Fatalf("install first: %v", err)
	}
	err := builder.Install(duplicateChannelModule{})
	if !errors.Is(err, agentslot.ErrDuplicateKey) {
		t.Fatalf("duplicate channel error = %v, want ErrDuplicateKey", err)
	}
}

type duplicateChannelModule struct{}

func (duplicateChannelModule) ID() string { return "interaction.duplicate" }
func (duplicateChannelModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Add(interaction.ChannelSlot, "test", interaction.GatewayChannel(channel{})))
}
func (command) Invoke(context.Context, interaction.CommandInvocation, interaction.CommandActions) (interaction.CommandResult, error) {
	return interaction.CommandResult{}, nil
}

var _ interaction.GatewayChannel = channel{}
var _ interaction.InteractionCommand = command{}

type module struct{}

func (module) ID() string { return "interaction.contracts" }
func (module) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Add(interaction.ChannelSlot, "test", interaction.GatewayChannel(channel{})))
}

type commandModule struct{}

func (commandModule) ID() string { return "interaction.command.contracts" }
func (commandModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Add(interaction.CommandSlot, "example", interaction.InteractionCommand(command{})))
}

func TestGatewayChannelIsKeyedManySlot(t *testing.T) {
	builder := agentslot.NewBuilder()
	if err := builder.Install(module{}); err != nil {
		t.Fatalf("install: %v", err)
	}
	assembly, err := builder.Build(agentslot.RequireMany(interaction.ChannelSlot, 1))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, ok := agentslot.Lookup(assembly, interaction.ChannelSlot, "test"); !ok {
		t.Fatal("gateway.channel contribution missing")
	}
}

func TestRevisionConflictCarriesRefreshMetadataAndStableClassification(t *testing.T) {
	cause := agent.NewCodedError(agent.ErrorConflict, agent.CodeRevisionConflict, "session.commit", "stale", nil)
	err := &interaction.RevisionConflictError{CurrentRevision: 9, SnapshotRequired: true, Cause: cause}
	if !agent.IsCode(err, agent.CodeRevisionConflict) || err.CurrentRevision != 9 || !err.SnapshotRequired {
		t.Fatalf("revision conflict = %#v, code=%q", err, agent.CodeOf(err))
	}
}

func TestEveryGatewayWriteCarriesRevisionAndActor(t *testing.T) {
	requests := []any{
		interaction.SendRequest{},
		interaction.SteerRequest{},
		interaction.RunPendingRequest{},
		interaction.CancelRequest{},
		interaction.EditQueuedRequest{},
		interaction.DeleteQueuedRequest{},
		interaction.ReclassifyQueuedRequest{},
		interaction.UpdateModelConfigRequest{},
		interaction.CloseSessionRequest{},
		interaction.CommandInvocation{},
	}
	wantRevision := reflect.TypeOf(agent.Revision(0))
	wantActor := reflect.TypeOf(agent.ActorIdentity{})
	for _, request := range requests {
		typeOf := reflect.TypeOf(request)
		revision, ok := typeOf.FieldByName("ExpectedRevision")
		if !ok || revision.Type != wantRevision {
			t.Errorf("%s must carry ExpectedRevision of type agent.Revision", typeOf.Name())
		}
		actor, ok := typeOf.FieldByName("Actor")
		if !ok || actor.Type != wantActor {
			t.Errorf("%s must carry Actor of type agent.ActorIdentity", typeOf.Name())
		}
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
