package workflow_test

import (
	"context"
	"sync"
	"testing"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/workflow"
)

func TestSchedulerModuleStopWaitsForRunningProviders(t *testing.T) {
	provider := &blockingProvider{started: make(chan struct{}), stopped: make(chan struct{})}
	module, err := workflow.NewSchedulerModule("workflow.scheduler.test")
	if err != nil {
		t.Fatal(err)
	}
	app := agentslot.NewApplication("workflow-stop", []agentslot.Module{
		workflow.NewMemoryJobStoreModule(), workflow.NewMemoryMailboxModule(),
		providerModule{key: "worker", provider: provider}, module,
	}, agentslot.RequireOne(workflow.SchedulerSlot))
	runtime, err := app.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	scheduler, ok := agentslot.Get(runtime.Assembly(), workflow.SchedulerSlot)
	if !ok {
		t.Fatal("scheduler was not assembled")
	}
	if _, err := scheduler.Spawn(context.Background(), workflow.SpawnRequest{
		ProviderKey: "worker", Parent: testParent(), Instruction: "work",
	}); err != nil {
		t.Fatal(err)
	}
	<-provider.started
	if err := runtime.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-provider.stopped:
	default:
		t.Fatal("Stop returned before provider observed cancellation")
	}
}

func TestMemoryMailboxWaitRejectsInvalidAddress(t *testing.T) {
	_, err := workflow.NewMemoryMailbox().Wait(context.Background(), workflow.Address{}, 0)
	if err == nil {
		t.Fatal("Wait accepted an invalid address")
	}
}

func TestMemoryMailboxIsAppendOnlyAddressedAndIdempotent(t *testing.T) {
	mailbox := workflow.NewMemoryMailbox()
	first, err := mailbox.Publish(context.Background(), workflow.Message{
		ID: "message-1", From: workflow.Address{Kind: workflow.AddressJob, ID: "job-1"},
		To: workflow.Address{Kind: workflow.AddressSession, ID: "session-1"}, Body: "done",
	})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := mailbox.Publish(context.Background(), workflow.Message{
		ID: "message-1", From: workflow.Address{Kind: workflow.AddressJob, ID: "job-1"},
		To: workflow.Address{Kind: workflow.AddressSession, ID: "session-1"}, Body: "done",
	})
	if err != nil || replay != first {
		t.Fatalf("mailbox replay = %#v, %v", replay, err)
	}
	if _, err := mailbox.Publish(context.Background(), workflow.Message{
		ID: "message-1", From: workflow.Address{Kind: workflow.AddressJob, ID: "job-1"},
		To: workflow.Address{Kind: workflow.AddressSession, ID: "session-1"}, Body: "changed",
	}); err == nil {
		t.Fatal("mailbox accepted a changed replay")
	}
	messages, err := mailbox.List(context.Background(), workflow.Address{Kind: workflow.AddressSession, ID: "session-1"}, 0)
	if err != nil || len(messages) != 1 || messages[0] != first {
		t.Fatalf("mailbox facts = %#v, %v", messages, err)
	}
}

func TestSchedulerPersistsTheExplicitCancellationReason(t *testing.T) {
	store := workflow.NewMemoryJobStore()
	mailbox := workflow.NewMemoryMailbox()
	provider := &blockingProvider{started: make(chan struct{}), stopped: make(chan struct{})}
	module, err := workflow.NewSchedulerModule("workflow.scheduler.cancel-test")
	if err != nil {
		t.Fatal(err)
	}
	app := agentslot.NewApplication("workflow-cancel", []agentslot.Module{
		jobStoreModule{store}, mailboxModule{mailbox}, providerModule{"worker", provider}, module,
	}, agentslot.RequireOne(workflow.SchedulerSlot))
	runtime, err := app.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop(context.Background())
	scheduler, ok := agentslot.Get(runtime.Assembly(), workflow.SchedulerSlot)
	if !ok {
		t.Fatal("scheduler was not assembled")
	}
	job, err := scheduler.Spawn(context.Background(), workflow.SpawnRequest{
		ProviderKey: "worker", Parent: testParent(), Instruction: "work",
	})
	if err != nil {
		t.Fatal(err)
	}
	<-provider.started
	terminal, err := scheduler.Close(context.Background(), workflow.CloseRequest{JobID: job.ID, Reason: "parent changed direction"})
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != workflow.JobCanceled || terminal.TerminalReason != "parent changed direction" {
		t.Fatalf("canceled job = %#v", terminal)
	}
}

func TestSchedulerRunsProviderAndPublishesDurableParentMailboxResult(t *testing.T) {
	store := workflow.NewMemoryJobStore()
	mailbox := workflow.NewMemoryMailbox()
	provider := providerFunc(func(_ context.Context, task workflow.Task, _ workflow.Inbox) (workflow.Result, error) {
		return workflow.Result{Summary: "completed: " + task.Instruction}, nil
	})
	schedulerModule, err := workflow.NewSchedulerModule("scheduler.reference")
	if err != nil {
		t.Fatal(err)
	}
	app := agentslot.NewApplication("workflow", []agentslot.Module{
		jobStoreModule{store}, mailboxModule{mailbox}, providerModule{"worker", provider}, schedulerModule,
	}, agentslot.RequireOne(workflow.SchedulerSlot))
	runtime, err := app.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop(context.Background())
	scheduler, ok := agentslot.Get(runtime.Assembly(), workflow.SchedulerSlot)
	if !ok {
		t.Fatal("scheduler was not assembled")
	}
	job, err := scheduler.Spawn(context.Background(), workflow.SpawnRequest{
		ProviderKey: "worker", Parent: workflow.Parent{SessionID: "session-1", RunID: "run-1", StepID: "step-1"}, Instruction: "review code",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	terminal, err := workflow.WaitTerminal(ctx, scheduler, job.ID, job.Version)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != workflow.JobCompleted || terminal.Result.Summary != "completed: review code" {
		t.Fatalf("terminal job = %#v", terminal)
	}
	messages, err := mailbox.Wait(ctx, workflow.Address{Kind: workflow.AddressSession, ID: "session-1"}, 0)
	if err != nil || len(messages) != 1 || messages[0].Body != terminal.Result.Summary {
		t.Fatalf("parent mailbox = %#v, %v", messages, err)
	}
}

func TestSchedulerContainsProviderPanicAsAFailedJob(t *testing.T) {
	store := workflow.NewMemoryJobStore()
	mailbox := workflow.NewMemoryMailbox()
	provider := providerFunc(func(context.Context, workflow.Task, workflow.Inbox) (workflow.Result, error) {
		panic("provider secret")
	})
	schedulerModule, err := workflow.NewSchedulerModule("scheduler.reference")
	if err != nil {
		t.Fatal(err)
	}
	app := agentslot.NewApplication("workflow", []agentslot.Module{
		jobStoreModule{store}, mailboxModule{mailbox}, providerModule{"worker", provider}, schedulerModule,
	}, agentslot.RequireOne(workflow.SchedulerSlot))
	runtime, err := app.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop(context.Background())
	scheduler, _ := agentslot.Get(runtime.Assembly(), workflow.SchedulerSlot)
	job, err := scheduler.Spawn(context.Background(), workflow.SpawnRequest{
		ProviderKey: "worker", Parent: testParent(), Instruction: "review",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	terminal, err := workflow.WaitTerminal(ctx, scheduler, job.ID, job.Version)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != workflow.JobFailed || terminal.ErrorCode != "agent_provider_failed" {
		t.Fatalf("panicked provider job = %#v", terminal)
	}
}

type providerFunc func(context.Context, workflow.Task, workflow.Inbox) (workflow.Result, error)

func (f providerFunc) Execute(ctx context.Context, task workflow.Task, inbox workflow.Inbox) (workflow.Result, error) {
	return f(ctx, task, inbox)
}

type blockingProvider struct {
	once    sync.Once
	started chan struct{}
	stopped chan struct{}
}

func (p *blockingProvider) Execute(ctx context.Context, _ workflow.Task, _ workflow.Inbox) (workflow.Result, error) {
	p.once.Do(func() { close(p.started) })
	<-ctx.Done()
	close(p.stopped)
	return workflow.Result{}, ctx.Err()
}

func testParent() workflow.Parent {
	return workflow.Parent{SessionID: "session-1", RunID: "run-1", StepID: "step-1"}
}

type jobStoreModule struct{ store workflow.JobStore }

func (jobStoreModule) ID() string { return "job.store.test" }
func (m jobStoreModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Set(workflow.JobStoreSlot, m.store))
}

type mailboxModule struct{ mailbox workflow.Mailbox }

func (mailboxModule) ID() string { return "mailbox.test" }
func (m mailboxModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Set(workflow.MailboxSlot, m.mailbox))
}

type providerModule struct {
	key      string
	provider workflow.AgentProvider
}

func (m providerModule) ID() string { return "agent.provider." + m.key }
func (m providerModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Add(workflow.AgentProviderSlot, m.key, m.provider))
}
