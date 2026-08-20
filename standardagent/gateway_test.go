package standardagent

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
)

func TestGatewayPublishesAndInvokesOneSharedCommandCatalog(t *testing.T) {
	entry := &captureEntrypoint{}
	application := NewApplication(ApplicationSpec{
		Name: "commands-agent", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: newSeededStore()},
			commandModule{commands: map[string]interaction.InteractionCommand{
				"zeta": fakeCommand{descriptor: interaction.CommandDescriptor{Key: "zeta", Title: "Zeta"}},
				"alpha": fakeCommand{descriptor: interaction.CommandDescriptor{
					Key: "alpha", Title: "Alpha",
					Fields: []interaction.FieldDescriptor{{Key: "mode", Choices: []interaction.Choice{{Value: "one", Title: "One"}}}},
				}, result: json.RawMessage(`{"ok":true}`)},
			}},
			NewEntrypointModule("entrypoint.test", "test", entry),
		},
	})
	runtime, err := application.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Stop(context.Background()) })

	descriptors, err := entry.Access().Commands(context.Background(), interaction.CommandScope{})
	if err != nil {
		t.Fatalf("commands: %v", err)
	}
	wantDescriptors := []interaction.CommandDescriptor{{
		Key: "alpha", Title: "Alpha",
		Fields: []interaction.FieldDescriptor{{Key: "mode", Choices: []interaction.Choice{{Value: "one", Title: "One"}}}},
	}, {Key: "zeta", Title: "Zeta"}}
	if !reflect.DeepEqual(descriptors, wantDescriptors) {
		t.Fatalf("descriptors = %#v, want %#v", descriptors, wantDescriptors)
	}
	descriptors[0].Fields[0].Choices[0].Title = "mutated"
	descriptorsAgain, err := entry.Access().Commands(context.Background(), interaction.CommandScope{})
	if err != nil || descriptorsAgain[0].Fields[0].Choices[0].Title != "One" {
		t.Fatalf("command descriptors were not detached: %#v, %v", descriptorsAgain, err)
	}
	result, err := entry.Access().InvokeCommand(context.Background(), interaction.CommandInvocation{Key: "alpha"})
	if err != nil || string(result.Data) != `{"ok":true}` {
		t.Fatalf("InvokeCommand result = %s, %v", result.Data, err)
	}
	_, err = entry.Access().InvokeCommand(context.Background(), interaction.CommandInvocation{Key: "missing"})
	if !agent.IsCode(err, agent.CodeCommandNotFound) {
		t.Fatalf("missing command error = %v, code=%q", err, agent.CodeOf(err))
	}
}

func TestBuildRejectsCommandWhoseDescriptorDoesNotMatchSlotKey(t *testing.T) {
	application := NewApplication(ApplicationSpec{
		Name: "bad-command-agent", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: newSeededStore()},
			commandModule{commands: map[string]interaction.InteractionCommand{
				"model": fakeCommand{descriptor: interaction.CommandDescriptor{Key: "other"}},
			}},
			NewEntrypointModule("entrypoint.test", "test", &captureEntrypoint{}),
		},
	})
	if _, err := application.Build(); err == nil {
		t.Fatal("Build succeeded with mismatched command key")
	}
}

func TestGatewayRoutesExecutionOnlyToAnOpenRuntime(t *testing.T) {
	entry := &captureEntrypoint{}
	application := NewApplication(ApplicationSpec{
		Name: "routing-agent", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: newSeededStore()},
			NewEntrypointModule("entrypoint.test", "test", entry),
		},
	})
	runtime, err := application.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Stop(context.Background()) })

	_, err = entry.Access().Send(context.Background(), interaction.SendRequest{SessionID: "session-1"})
	if !agent.IsCode(err, agent.CodeSessionNotOpen) {
		t.Fatalf("Send before Resume error = %v, code=%q", err, agent.CodeOf(err))
	}
	if _, err := entry.Access().ResumeSession(context.Background(), interaction.ResumeSessionRequest{SessionID: "session-1"}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	_, err = entry.Access().Send(context.Background(), interaction.SendRequest{
		SessionID: "session-1", ExpectedRevision: 1,
		Input: agent.MessageInput{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "hello"}}},
	})
	if err != nil {
		t.Fatalf("Send through fixed Runtime: %v", err)
	}
}

func TestInteractionCommandCannotRetainActionsAfterInvocation(t *testing.T) {
	entry := &captureEntrypoint{}
	command := &retainingCommand{}
	application := NewApplication(ApplicationSpec{
		Name: "command-scope-agent", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: newSeededStore()},
			commandModule{commands: map[string]interaction.InteractionCommand{"retain": command}},
			NewEntrypointModule("entrypoint.test", "test", entry),
		},
	})
	runtime, err := application.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Stop(context.Background()) })
	if _, err := entry.Access().ResumeSession(context.Background(), interaction.ResumeSessionRequest{SessionID: "session-1"}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, err := entry.Access().InvokeCommand(context.Background(), interaction.CommandInvocation{
		Key: "retain", Scope: interaction.CommandScope{SessionID: "session-1"},
	}); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	_, err = command.actions.Apply(context.Background(), interaction.ActionRequest{
		Kind: interaction.ActionCancel,
	})
	if !agent.IsCode(err, agent.CodeRuntimeClosed) {
		t.Fatalf("retained action error = %v, code=%q", err, agent.CodeOf(err))
	}
	if _, err := command.actions.CurrentModelConfig(context.Background()); !agent.IsCode(err, agent.CodeRuntimeClosed) {
		t.Fatalf("retained model query error = %v, code=%q", err, agent.CodeOf(err))
	}
	if _, err := command.actions.AvailableModels(context.Background()); !agent.IsCode(err, agent.CodeRuntimeClosed) {
		t.Fatalf("retained catalog query error = %v, code=%q", err, agent.CodeOf(err))
	}
}

func TestCommandActionsAreBoundToInvocationSession(t *testing.T) {
	entry := &captureEntrypoint{}
	application := NewApplication(ApplicationSpec{
		Name: "command-target-agent", DefaultModelConfig: testDefaultModel(),
		Modules: []agentslot.Module{
			componentsModule{store: newSeededStore()},
			commandModule{commands: map[string]interaction.InteractionCommand{"cancel": applyingCommand{}}},
			NewEntrypointModule("entrypoint.test", "test", entry),
		},
	})
	runtime, err := application.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Stop(context.Background()) })
	if _, err := entry.Access().ResumeSession(context.Background(), interaction.ResumeSessionRequest{SessionID: "session-1"}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	_, err = entry.Access().InvokeCommand(context.Background(), interaction.CommandInvocation{
		Key: "cancel", Scope: interaction.CommandScope{SessionID: "session-1"},
	})
	if err != nil {
		t.Fatalf("bound cancel action: %v", err)
	}
}

type commandModule struct {
	commands map[string]interaction.InteractionCommand
}

func (commandModule) ID() string { return "test.commands" }

func (m commandModule) Register(reg agentslot.Registrar) error {
	contributions := make([]agentslot.Contribution, 0, len(m.commands))
	for key, command := range m.commands {
		contributions = append(contributions, agentslot.Add(interaction.CommandSlot, key, command))
	}
	return reg.Contribute(contributions...)
}

type fakeCommand struct {
	descriptor interaction.CommandDescriptor
	result     json.RawMessage
	err        error
}

type retainingCommand struct {
	actions interaction.CommandActions
}

type applyingCommand struct{}

func (applyingCommand) Describe() interaction.CommandDescriptor {
	return interaction.CommandDescriptor{Key: "cancel", Title: "Cancel"}
}

func (applyingCommand) Invoke(ctx context.Context, _ interaction.CommandInvocation, actions interaction.CommandActions) (interaction.CommandResult, error) {
	_, err := actions.Apply(ctx, interaction.ActionRequest{Kind: interaction.ActionCancel})
	return interaction.CommandResult{}, err
}

func (*retainingCommand) Describe() interaction.CommandDescriptor {
	return interaction.CommandDescriptor{Key: "retain", Title: "Retain"}
}

func (c *retainingCommand) Invoke(_ context.Context, _ interaction.CommandInvocation, actions interaction.CommandActions) (interaction.CommandResult, error) {
	c.actions = actions
	return interaction.CommandResult{}, nil
}

func (c fakeCommand) Describe() interaction.CommandDescriptor { return c.descriptor }

func (c fakeCommand) Invoke(context.Context, interaction.CommandInvocation, interaction.CommandActions) (interaction.CommandResult, error) {
	if c.err != nil {
		return interaction.CommandResult{}, c.err
	}
	return interaction.CommandResult{Data: append(json.RawMessage(nil), c.result...)}, nil
}
