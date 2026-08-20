package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/interaction/cli"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestCLIUsesGatewayForTextAndExplicitStructuredCommands(t *testing.T) {
	input := io.NopCloser(strings.NewReader("please /cancel this\n/model {\"target\":\"fast\"}\n/cancel\n/pending\n/quit\n"))
	var output, errors bytes.Buffer
	entry, err := cli.New(cli.Config{
		AgentID: "agent-1", WorkspaceID: "workspace-1", Input: input, Output: &output, ErrorOutput: &errors,
	})
	if err != nil {
		t.Fatal(err)
	}
	gateway := &gatewayProbe{revision: 1}
	if err := entry.Attach(gateway); err != nil {
		t.Fatal(err)
	}
	if err := entry.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDone(t, entry)
	if err := entry.Err(); err != nil {
		t.Fatalf("CLI error = %v, stderr=%s", err, errors.String())
	}
	if len(gateway.sent) != 1 || gateway.sent[0] != "please /cancel this" {
		t.Fatalf("normal text routing = %#v", gateway.sent)
	}
	if len(gateway.commands) != 1 || gateway.commands[0].Key != "model" || string(gateway.commands[0].Arguments) != `{"target":"fast"}` {
		t.Fatalf("structured commands = %#v", gateway.commands)
	}
	if gateway.cancels != 1 || gateway.pending != 1 {
		t.Fatalf("cancel=%d pending=%d", gateway.cancels, gateway.pending)
	}
	if !strings.Contains(output.String(), "assistant reply") || !strings.Contains(output.String(), "pending reply") || !strings.Contains(output.String(), `{"selected":"fast"}`) {
		t.Fatalf("stdout = %q", output.String())
	}
	if err := entry.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCLIResumesConfiguredSessionAndRendersGenericHelp(t *testing.T) {
	input := io.NopCloser(strings.NewReader("/help\n/quit\n"))
	var output bytes.Buffer
	entry, err := cli.New(cli.Config{SessionID: "session-existing", Input: input, Output: &output, ErrorOutput: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	gateway := &gatewayProbe{revision: 7}
	if err := entry.Attach(gateway); err != nil {
		t.Fatal(err)
	}
	if err := entry.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDone(t, entry)
	if gateway.resumed != "session-existing" || gateway.created {
		t.Fatalf("resume=%q created=%v", gateway.resumed, gateway.created)
	}
	if !strings.Contains(output.String(), "/model") || !strings.Contains(output.String(), "/cancel") {
		t.Fatalf("help output = %q", output.String())
	}
}

func TestCLIStopUnblocksAWaitingInput(t *testing.T) {
	reader, writer := io.Pipe()
	entry, err := cli.New(cli.Config{
		AgentID: "agent-1", WorkspaceID: "workspace-1", Input: reader, Output: io.Discard, ErrorOutput: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := entry.Attach(&gatewayProbe{revision: 1}); err != nil {
		t.Fatal(err)
	}
	if err := entry.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := entry.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	waitDone(t, entry)
}

func TestCLIConcurrentStartOpensOnlyOneSession(t *testing.T) {
	reader, writer := io.Pipe()
	entry, err := cli.New(cli.Config{
		AgentID: "agent-1", WorkspaceID: "workspace-1", Input: reader, Output: io.Discard, ErrorOutput: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	gateway := &gatewayProbe{revision: 1}
	if err := entry.Attach(gateway); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- entry.Start(context.Background()) }()
	}
	succeeded := 0
	for range 2 {
		if err := <-results; err == nil {
			succeeded++
		}
	}
	if succeeded != 1 || gateway.createCount.Load() != 1 {
		t.Fatalf("successful starts=%d created sessions=%d", succeeded, gateway.createCount.Load())
	}
	_ = entry.Stop(context.Background())
	_ = writer.Close()
}

func TestCLIValidatesItsTransportBoundary(t *testing.T) {
	if _, err := cli.New(cli.Config{}); err == nil {
		t.Fatal("CLI accepted missing streams and Session scope")
	}
	entry, err := cli.New(cli.Config{
		AgentID: "agent-1", WorkspaceID: "workspace-1", Input: io.NopCloser(strings.NewReader("")), Output: io.Discard, ErrorOutput: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := entry.Attach(nil); err == nil {
		t.Fatal("CLI accepted nil Gateway")
	}
}

func TestCLIStartReportsSessionHeaderWriteFailure(t *testing.T) {
	entry, err := cli.New(cli.Config{
		AgentID: "agent-1", WorkspaceID: "workspace-1", Input: io.NopCloser(strings.NewReader("")),
		Output: failingWriter{}, ErrorOutput: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := entry.Attach(&gatewayProbe{revision: 1}); err != nil {
		t.Fatal(err)
	}
	if err := entry.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "write Session header") {
		t.Fatalf("Start output error = %v", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func waitDone(t *testing.T, entry *cli.Entrypoint) {
	t.Helper()
	select {
	case <-entry.Done():
	case <-time.After(time.Second):
		t.Fatal("CLI did not finish")
	}
}

type gatewayProbe struct {
	interaction.GatewayAccess
	mu          sync.Mutex
	revision    agent.Revision
	created     bool
	resumed     agent.SessionID
	sent        []string
	commands    []interaction.CommandInvocation
	cancels     int
	pending     int
	createCount atomic.Int32
}

func (g *gatewayProbe) CreateSession(_ context.Context, _ interaction.CreateSessionRequest) (interaction.SessionOpened, error) {
	g.created = true
	g.createCount.Add(1)
	return interaction.SessionOpened{SessionID: "session-1", Revision: g.revision}, nil
}

func (g *gatewayProbe) ResumeSession(_ context.Context, request interaction.ResumeSessionRequest) (interaction.SessionOpened, error) {
	g.resumed = request.SessionID
	return interaction.SessionOpened{SessionID: request.SessionID, Revision: g.revision}, nil
}

func (g *gatewayProbe) SendAndWait(_ context.Context, request interaction.SendRequest) (interaction.RunResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.revision++
	g.sent = append(g.sent, request.Input.Parts[0].Text)
	return interaction.RunResult{
		SessionID: request.SessionID, RunID: "run-1", Revision: g.revision, Outcome: session.RunCompleted,
		AssistantMessages: []agent.Message{{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "assistant reply"}}}},
	}, nil
}

func (g *gatewayProbe) Commands(context.Context, interaction.CommandScope) ([]interaction.CommandDescriptor, error) {
	return []interaction.CommandDescriptor{{Key: "model", Title: "Model", Description: "Choose model"}}, nil
}

func (g *gatewayProbe) InvokeCommand(_ context.Context, invocation interaction.CommandInvocation) (interaction.CommandResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.revision++
	invocation.Arguments = append(json.RawMessage(nil), invocation.Arguments...)
	g.commands = append(g.commands, invocation)
	return interaction.CommandResult{Revision: g.revision, Data: json.RawMessage(`{"selected":"fast"}`)}, nil
}

func (g *gatewayProbe) Cancel(context.Context, interaction.CancelRequest) error {
	g.cancels++
	return nil
}

func (g *gatewayProbe) WhenIdle(context.Context, interaction.WhenIdleRequest) error { return nil }

func (g *gatewayProbe) RunPending(_ context.Context, request interaction.RunPendingRequest) (interaction.RunReceipt, error) {
	g.pending++
	g.revision++
	return interaction.RunReceipt{SessionID: request.SessionID, RunID: "run-pending", Revision: g.revision}, nil
}

func (g *gatewayProbe) Snapshot(_ context.Context, request interaction.SnapshotRequest) (interaction.SessionSnapshot, error) {
	return interaction.SessionSnapshot{
		SessionID: request.SessionID, Revision: g.revision, RunState: session.RunIdle,
		History: []session.HistoryFact{{Message: &agent.Message{
			ID: "message-pending", SessionID: request.SessionID, RunID: "run-pending", StepID: "step-pending",
			Role: agent.RoleAssistant, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "pending reply"}},
		}}},
	}, nil
}
