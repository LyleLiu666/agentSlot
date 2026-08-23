package acpchannel

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/session"
	acp "github.com/coder/acp-go-sdk"
)

func TestNewRequiresTrustedBoundaryAndFixedScope(t *testing.T) {
	valid := Config{
		Input: netPipe(t), Output: netPipe(t), Close: func() error { return nil },
		Actor:     agent.ActorIdentity{Kind: agent.ActorRemoteUser, ID: "user-1"},
		Authorize: func(context.Context, agent.ActorIdentity, RequestScope) error { return nil },
		AgentID:   "agent-1", WorkspaceID: "workspace-1", WorkingDirectory: t.TempDir(),
	}
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{"input", func(c *Config) { c.Input = nil }},
		{"output", func(c *Config) { c.Output = nil }},
		{"close", func(c *Config) { c.Close = nil }},
		{"authorize", func(c *Config) { c.Authorize = nil }},
		{"actor", func(c *Config) { c.Actor = agent.ActorIdentity{} }},
		{"local actor", func(c *Config) { c.Actor.Kind = agent.ActorLocalUser }},
		{"agent id", func(c *Config) { c.AgentID = "" }},
		{"workspace id", func(c *Config) { c.WorkspaceID = "" }},
		{"relative cwd", func(c *Config) { c.WorkingDirectory = "." }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := valid
			tt.edit(&config)
			if _, err := New(config); err == nil {
				t.Fatal("invalid ACP channel configuration was accepted")
			}
		})
	}
}

func TestACPWireNegotiatesOnlyImplementedStableCapabilities(t *testing.T) {
	gateway := &gatewayProbe{}
	client, updates, stop := startPair(t, gateway, nil)
	defer stop()

	response, err := client.Initialize(context.Background(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	if err != nil {
		t.Fatal(err)
	}
	capabilities := response.AgentCapabilities
	if response.ProtocolVersion != acp.ProtocolVersionNumber || capabilities.LoadSession || capabilities.PromptCapabilities.Image || capabilities.PromptCapabilities.Audio || capabilities.PromptCapabilities.EmbeddedContext {
		t.Fatalf("unexpected negotiated capabilities: %#v", response)
	}
	if capabilities.SessionCapabilities.List == nil || capabilities.SessionCapabilities.Resume == nil || capabilities.SessionCapabilities.Close == nil {
		t.Fatalf("stable session capabilities missing: %#v", capabilities.SessionCapabilities)
	}
	if len(response.AuthMethods) != 0 || updates == nil {
		t.Fatalf("transport-authenticated profile advertised ACP auth: %#v", response.AuthMethods)
	}
}

func TestACPWireProjectsSessionAndPromptThroughGateway(t *testing.T) {
	var authorized []RequestScope
	gateway := &gatewayProbe{
		opened: interaction.SessionOpened{SessionID: "session-1", Revision: 3},
		view:   interaction.SessionView{SessionID: "session-1", Revision: 3},
		result: interaction.RunResult{
			SessionID: "session-1", Outcome: session.RunCompleted,
			AssistantMessages: []agent.Message{{Role: agent.RoleAssistant, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "done"}}}},
		},
	}
	client, updates, stop := startPair(t, gateway, func(_ context.Context, actor agent.ActorIdentity, scope RequestScope) error {
		if actor.ID != "remote-1" {
			t.Fatalf("untrusted actor: %#v", actor)
		}
		authorized = append(authorized, scope)
		return nil
	})
	defer stop()

	opened, err := client.NewSession(context.Background(), acp.NewSessionRequest{Cwd: gatewayCWD, McpServers: []acp.McpServer{}})
	if err != nil || opened.SessionId != "session-1" {
		t.Fatalf("new session: %#v, %v", opened, err)
	}
	description := "design"
	response, err := client.Prompt(context.Background(), acp.PromptRequest{
		SessionId: opened.SessionId,
		Prompt: []acp.ContentBlock{
			{Text: &acp.ContentBlockText{Type: "text", Text: "review"}},
			{ResourceLink: &acp.ContentBlockResourceLink{Type: "resource_link", Name: "spec", Uri: "file:///workspace/spec.md", Description: &description}},
		},
	})
	if err != nil || response.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("prompt: %#v, %v", response, err)
	}
	if gateway.created.AgentID != "agent-1" || gateway.created.WorkspaceID != "workspace-1" {
		t.Fatalf("client escaped fixed scope: %#v", gateway.created)
	}
	if gateway.sent.Actor.Kind != agent.ActorRemoteUser || gateway.sent.Actor.ID != "remote-1" || gateway.sent.ExpectedRevision != 3 {
		t.Fatalf("prompt identity/revision lost: %#v", gateway.sent)
	}
	if len(gateway.sent.Input.Parts) != 2 || gateway.sent.Input.Parts[0].Text != "review" || gateway.sent.Input.Parts[1].Text != "[spec](file:///workspace/spec.md) — design" {
		t.Fatalf("ACP content projection mismatch: %#v", gateway.sent.Input.Parts)
	}
	gotUpdate := <-updates.updates
	if gotUpdate.Update.AgentMessageChunk == nil || gotUpdate.Update.AgentMessageChunk.Content.Text == nil || gotUpdate.Update.AgentMessageChunk.Content.Text.Text != "done" {
		t.Fatalf("durable assistant response not projected: %#v", gotUpdate)
	}
	if len(authorized) < 2 || authorized[0].Operation != OperationCreateSession || authorized[1].Operation != OperationPrompt {
		t.Fatalf("authorization was not applied per operation: %#v", authorized)
	}
}

func TestACPRejectsUnadvertisedContentAndScopeChanges(t *testing.T) {
	gateway := &gatewayProbe{view: interaction.SessionView{SessionID: "session-1", Revision: 1}}
	client, _, stop := startPair(t, gateway, nil)
	defer stop()

	_, err := client.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/other", McpServers: []acp.McpServer{}})
	if err == nil {
		t.Fatal("different working directory was accepted")
	}
	_, err = client.Prompt(context.Background(), acp.PromptRequest{SessionId: "session-1", Prompt: []acp.ContentBlock{{Image: &acp.ContentBlockImage{Type: "image", Data: "AA==", MimeType: "image/png"}}}})
	if err == nil || gateway.sent.SessionID != "" {
		t.Fatal("unadvertised image content reached Gateway")
	}
}

func TestACPWireMapsListResumeAndClose(t *testing.T) {
	updated := time.Date(2026, 8, 23, 9, 10, 11, 12, time.UTC)
	gateway := &gatewayProbe{
		listed: interaction.SessionList{Sessions: []interaction.SessionSummary{{SessionID: "session-1", AgentID: "agent-1", WorkspaceID: "workspace-1", UpdatedAt: updated}}, NextCursor: "next"},
		opened: interaction.SessionOpened{SessionID: "session-1", Revision: 4},
		view:   interaction.SessionView{SessionID: "session-1", Revision: 4},
	}
	client, _, stop := startPair(t, gateway, nil)
	defer stop()

	cursor := "cursor"
	listed, err := client.ListSessions(context.Background(), acp.ListSessionsRequest{Cursor: &cursor})
	if err != nil || len(listed.Sessions) != 1 || listed.Sessions[0].Cwd != gatewayCWD || listed.Sessions[0].UpdatedAt == nil || *listed.Sessions[0].UpdatedAt != updated.Format(time.RFC3339Nano) || listed.NextCursor == nil || *listed.NextCursor != "next" {
		t.Fatalf("list projection: %#v, %v", listed, err)
	}
	if gateway.listedRequest.Cursor != cursor || gateway.listedRequest.AgentID != "agent-1" || gateway.listedRequest.WorkspaceID != "workspace-1" {
		t.Fatalf("list scope/cursor mismatch: %#v", gateway.listedRequest)
	}
	if _, err := client.ResumeSession(context.Background(), acp.ResumeSessionRequest{SessionId: "session-1", Cwd: gatewayCWD}); err != nil {
		t.Fatal(err)
	}
	if gateway.resumed.SessionID != "session-1" {
		t.Fatalf("resume mismatch: %#v", gateway.resumed)
	}
	if _, err := client.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: "session-1"}); err != nil {
		t.Fatal(err)
	}
	if gateway.closed.SessionID != "session-1" || gateway.closed.ExpectedRevision != 4 || gateway.closed.Actor.ID != "remote-1" {
		t.Fatalf("close mismatch: %#v", gateway.closed)
	}
}

func TestPromptDisconnectDoesNotCancelDurableRunButExplicitCancelDoes(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	gateway := &gatewayProbe{view: interaction.SessionView{SessionID: "session-1", Revision: 7}, sendStarted: started, sendRelease: release}
	server, clientConn, _, client := startRawPair(t, gateway, nil)

	done := make(chan error, 1)
	go func() {
		_, err := client.Prompt(context.Background(), acp.PromptRequest{SessionId: "session-1", Prompt: []acp.ContentBlock{{Text: &acp.ContentBlockText{Type: "text", Text: "run"}}}})
		done <- err
	}()
	<-started
	_ = clientConn.Close()
	select {
	case <-gateway.sendCanceled:
		t.Fatal("peer disconnect canceled durable SendAndWait")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	_ = <-done
	_ = server.Stop(context.Background())

	gateway2 := &gatewayProbe{view: interaction.SessionView{SessionID: "session-2", Revision: 9}, cancelDone: make(chan struct{})}
	client2, _, stop := startPair(t, gateway2, nil)
	defer stop()
	if err := client2.Cancel(context.Background(), acp.CancelNotification{SessionId: "session-2"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gateway2.cancelDone:
	case <-time.After(time.Second):
		t.Fatal("ACP cancel notification was not handled")
	}
	if gateway2.canceled.SessionID != "session-2" || gateway2.canceled.ExpectedRevision != 9 || gateway2.canceled.Actor.ID != "remote-1" {
		t.Fatalf("explicit cancel not mapped: %#v", gateway2.canceled)
	}
}

const gatewayCWD = "/workspace"

type gatewayProbe struct {
	interaction.GatewayAccess
	mu            sync.Mutex
	opened        interaction.SessionOpened
	created       interaction.CreateSessionRequest
	listed        interaction.SessionList
	listedRequest interaction.ListSessionsRequest
	resumed       interaction.ResumeSessionRequest
	view          interaction.SessionView
	sent          interaction.SendRequest
	result        interaction.RunResult
	canceled      interaction.CancelRequest
	closed        interaction.CloseSessionRequest
	cancelDone    chan struct{}
	sendStarted   chan struct{}
	sendRelease   chan struct{}
	sendCanceled  chan struct{}
}

func (g *gatewayProbe) CreateSession(_ context.Context, request interaction.CreateSessionRequest) (interaction.SessionOpened, error) {
	g.created = request
	return g.opened, nil
}
func (g *gatewayProbe) ListSessions(_ context.Context, request interaction.ListSessionsRequest) (interaction.SessionList, error) {
	g.listedRequest = request
	return g.listed, nil
}
func (g *gatewayProbe) ResumeSession(_ context.Context, request interaction.ResumeSessionRequest) (interaction.SessionOpened, error) {
	g.resumed = request
	return g.opened, nil
}
func (g *gatewayProbe) View(_ context.Context, _ interaction.SessionViewRequest) (interaction.SessionView, error) {
	return g.view, nil
}
func (g *gatewayProbe) SendAndWait(ctx context.Context, request interaction.SendRequest) (interaction.RunResult, error) {
	g.sent = request
	if g.sendStarted != nil {
		if g.sendCanceled == nil {
			g.sendCanceled = make(chan struct{})
		}
		close(g.sendStarted)
		select {
		case <-ctx.Done():
			close(g.sendCanceled)
			return interaction.RunResult{}, ctx.Err()
		case <-g.sendRelease:
		}
	}
	return g.result, nil
}
func (g *gatewayProbe) Cancel(_ context.Context, request interaction.CancelRequest) error {
	g.canceled = request
	if g.cancelDone != nil {
		close(g.cancelDone)
	}
	return nil
}
func (g *gatewayProbe) CloseSession(_ context.Context, request interaction.CloseSessionRequest) error {
	g.closed = request
	return nil
}

type clientProbe struct{ updates chan acp.SessionNotification }

func (c *clientProbe) SessionUpdate(_ context.Context, update acp.SessionNotification) error {
	c.updates <- update
	return nil
}
func (*clientProbe) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, errors.New("unsupported")
}
func (*clientProbe) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, errors.New("unsupported")
}
func (*clientProbe) RequestPermission(context.Context, acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	return acp.RequestPermissionResponse{}, errors.New("unsupported")
}
func (*clientProbe) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errors.New("unsupported")
}
func (*clientProbe) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, errors.New("unsupported")
}
func (*clientProbe) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, errors.New("unsupported")
}
func (*clientProbe) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, errors.New("unsupported")
}
func (*clientProbe) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, errors.New("unsupported")
}

func startPair(t *testing.T, gateway interaction.GatewayAccess, authorize Authorizer) (*acp.ClientSideConnection, *clientProbe, func()) {
	server, clientConn, probe, client := startRawPair(t, gateway, authorize)
	return client, probe, func() { _ = clientConn.Close(); _ = server.Stop(context.Background()) }
}

func startRawPair(t *testing.T, gateway interaction.GatewayAccess, authorize Authorizer) (*Channel, net.Conn, *clientProbe, *acp.ClientSideConnection) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	if authorize == nil {
		authorize = func(context.Context, agent.ActorIdentity, RequestScope) error { return nil }
	}
	channel, err := New(Config{Input: serverConn, Output: serverConn, Close: serverConn.Close, Actor: agent.ActorIdentity{Kind: agent.ActorRemoteUser, ID: "remote-1"}, Authorize: authorize, AgentID: "agent-1", WorkspaceID: "workspace-1", WorkingDirectory: gatewayCWD, AgentName: "test-agent", AgentVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := channel.Bind(gateway); err != nil {
		t.Fatal(err)
	}
	if err := channel.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	probe := &clientProbe{updates: make(chan acp.SessionNotification, 8)}
	client := acp.NewClientSideConnection(probe, clientConn, clientConn)
	return channel, clientConn, probe, client
}

func netPipe(t *testing.T) net.Conn {
	t.Helper()
	one, two := net.Pipe()
	t.Cleanup(func() { _ = one.Close(); _ = two.Close() })
	return one
}
