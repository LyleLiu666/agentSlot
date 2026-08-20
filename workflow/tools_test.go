package workflow_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/tool"
	"github.com/LyleLiu666/agentSlot/workflow"
)

func TestWorkflowToolModuleContributesTheClosedStandardToolPack(t *testing.T) {
	toolModule, err := workflow.NewToolModule("workflow.tools.test")
	if err != nil {
		t.Fatal(err)
	}
	app := agentslot.NewApplication("workflow-tools", []agentslot.Module{
		workflowComponentModule{scheduler: failingScheduler{}, mailbox: workflow.NewMemoryMailbox()}, toolModule,
	}, agentslot.RequireMany(tool.ToolSlot, len(workflow.ToolKeys())))
	assembly, err := app.Build()
	if err != nil {
		t.Fatal(err)
	}
	tools := agentslot.All(assembly, tool.ToolSlot)
	if len(tools) != len(workflow.ToolKeys()) {
		t.Fatalf("workflow tools = %#v", tools)
	}
	for index, expected := range workflow.ToolKeys() {
		if tools[index].Key != expected || tools[index].Value.Definition().Name != expected {
			t.Fatalf("workflow tool %d = %#v", index, tools[index])
		}
		if err := tools[index].Value.Definition().Validate(); err != nil {
			t.Fatalf("workflow tool %q: %v", expected, err)
		}
	}
}

func TestWorkflowToolDoesNotExposeProviderErrorsToTheModel(t *testing.T) {
	toolModule, err := workflow.NewToolModule("workflow.tools.test")
	if err != nil {
		t.Fatal(err)
	}
	app := agentslot.NewApplication("workflow-tools", []agentslot.Module{
		workflowComponentModule{scheduler: failingScheduler{}, mailbox: workflow.NewMemoryMailbox()}, toolModule,
	}, agentslot.RequireMany(tool.ToolSlot, 1))
	assembly, err := app.Build()
	if err != nil {
		t.Fatal(err)
	}
	status, ok := agentslot.Lookup(assembly, tool.ToolSlot, workflow.ToolStatus)
	if !ok {
		t.Fatal("agent.status was not assembled")
	}
	result := status.Invoke(context.Background(), tool.ToolInvocation{
		Call:      tool.Call{ID: "call-1", Name: workflow.ToolStatus, Arguments: []byte(`{"job_id":"job-1"}`)},
		SessionID: "session-1", RunID: "run-1", StepID: "step-1",
	})
	if result.Status != tool.ResultFailed || result.Error == nil || strings.Contains(result.Error.Message, "database-password") {
		t.Fatalf("model-facing workflow error = %#v", result)
	}
}

type workflowComponentModule struct {
	scheduler workflow.Scheduler
	mailbox   workflow.Mailbox
}

func (workflowComponentModule) ID() string { return "workflow.components.test" }
func (m workflowComponentModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(
		agentslot.Set(workflow.SchedulerSlot, m.scheduler),
		agentslot.Set(workflow.MailboxSlot, m.mailbox),
	)
}

type failingScheduler struct{}

func (failingScheduler) Spawn(context.Context, workflow.SpawnRequest) (workflow.Job, error) {
	return workflow.Job{}, errors.New("database-password=secret")
}
func (failingScheduler) Get(context.Context, string) (workflow.Job, bool, error) {
	return workflow.Job{}, false, errors.New("database-password=secret")
}
func (failingScheduler) List(context.Context, workflow.JobQuery) ([]workflow.Job, error) {
	return nil, errors.New("database-password=secret")
}
func (failingScheduler) Wait(context.Context, string, uint64) (workflow.Job, error) {
	return workflow.Job{}, errors.New("database-password=secret")
}
func (failingScheduler) Send(context.Context, workflow.SendRequest) (workflow.Message, error) {
	return workflow.Message{}, errors.New("database-password=secret")
}
func (failingScheduler) Close(context.Context, workflow.CloseRequest) (workflow.Job, error) {
	return workflow.Job{}, errors.New("database-password=secret")
}
