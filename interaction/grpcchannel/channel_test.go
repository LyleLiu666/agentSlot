package grpcchannel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/session"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func TestRemoteChannelUsesAuthenticatedActorAndPreservesRevisionConflict(t *testing.T) {
	authenticated := agent.ActorIdentity{Kind: agent.ActorRemoteUser, ID: "principal-1"}
	var captured interaction.SendRequest
	backend := &gatewayStub{}
	backend.create = func(context.Context, interaction.CreateSessionRequest) (interaction.SessionOpened, error) {
		return interaction.SessionOpened{SessionID: "session-1", Revision: 7}, nil
	}
	backend.send = func(_ context.Context, request interaction.SendRequest) (interaction.EnqueueReceipt, error) {
		captured = request
		return interaction.EnqueueReceipt{}, &interaction.RevisionConflictError{CurrentRevision: 9, SnapshotRequired: true,
			Cause: agent.NewCodedError(agent.ErrorConflict, agent.CodeRevisionConflict, "send", "stale revision", nil)}
	}
	client, stop := startTestChannel(t, backend, authenticated)
	defer stop()

	opened, err := client.CreateSession(context.Background(), interaction.CreateSessionRequest{AgentID: "agent-1", WorkspaceID: "workspace-1"})
	if err != nil || opened.Revision != 7 {
		t.Fatalf("CreateSession = %#v, %v", opened, err)
	}
	_, err = client.Send(context.Background(), interaction.SendRequest{
		SessionID: "session-1", ExpectedRevision: 7,
		Actor: agent.ActorIdentity{Kind: agent.ActorLocalUser, ID: "forged"},
		Input: agent.MessageInput{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "hello"}}},
	})
	var conflict *interaction.RevisionConflictError
	if !errors.As(err, &conflict) || conflict.CurrentRevision != 9 || !conflict.SnapshotRequired || agent.CodeOf(err) != agent.CodeRevisionConflict {
		t.Fatalf("remote conflict = %#v / %v", conflict, err)
	}
	if captured.Actor != authenticated {
		t.Fatalf("Gateway received wire Actor %#v instead of authenticated Actor %#v", captured.Actor, authenticated)
	}
}

func TestRemoteChannelPreservesInputGateErrorRevisionAndDiagnostics(t *testing.T) {
	diagnostic := session.ExtensionDiagnostic{
		InvocationID: "invocation-1", Sequence: 1,
		Descriptor: hook.ExtensionDescriptor{Key: "input-check", DefinitionDigest: "sha256:" + strings.Repeat("a", 64)},
		Boundary:   hook.BoundaryInputGate, SessionID: "session-1", MessageID: "message-1",
		Status: hook.InvocationSucceeded, Decision: hook.DecisionReject, Reason: "rejected",
		Effect: hook.EffectApplied, Context: hook.ContextNone,
	}
	backend := &gatewayStub{send: func(context.Context, interaction.SendRequest) (interaction.EnqueueReceipt, error) {
		return interaction.EnqueueReceipt{}, &interaction.InputGateError{
			SessionID: "session-1", CurrentRevision: 12, Diagnostics: []session.ExtensionDiagnostic{diagnostic},
			Cause: agent.NewCodedError(agent.ErrorForbidden, agent.CodeInputRejected, "send", "rejected", nil),
		}
	}}
	client, stop := startTestChannel(t, backend, agent.ActorIdentity{Kind: agent.ActorRemoteUser, ID: "user-1"})
	defer stop()
	_, err := client.Send(context.Background(), interaction.SendRequest{
		SessionID: "session-1", ExpectedRevision: 7,
		Input: agent.MessageInput{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "hello"}}},
	})
	var gateErr *interaction.InputGateError
	if !errors.As(err, &gateErr) || gateErr.SessionID != "session-1" || gateErr.CurrentRevision != 12 || len(gateErr.Diagnostics) != 1 ||
		gateErr.Diagnostics[0].InvocationID != diagnostic.InvocationID || agent.CodeOf(err) != agent.CodeInputRejected {
		t.Fatalf("remote InputGate error = %#v / %v", gateErr, err)
	}
}

func TestRemoteSubscriptionClosesOnlyTheConnectionProjection(t *testing.T) {
	stream := &eventStream{events: []interaction.Event{{Kind: interaction.EventRevision, SessionID: "session-1", Revision: 8}}}
	backend := &gatewayStub{subscribe: func(context.Context, interaction.SubscribeRequest) (interaction.EventStream, error) {
		return stream, nil
	}}
	client, stop := startTestChannel(t, backend, agent.ActorIdentity{Kind: agent.ActorService, ID: "service-1"})
	defer stop()

	remote, err := client.Subscribe(context.Background(), interaction.SubscribeRequest{SessionID: "session-1", AfterRevision: 7})
	if err != nil {
		t.Fatal(err)
	}
	event, err := remote.Recv(context.Background())
	if err != nil || event.Revision != 8 {
		t.Fatalf("Recv = %#v, %v", event, err)
	}
	if err := remote.Close(); err != nil {
		t.Fatal(err)
	}
	if !stream.closed {
		t.Fatal("closing remote subscription did not release backend projection")
	}
}

func TestRemoteSubscriptionPreservesOverflowClassification(t *testing.T) {
	backend := &gatewayStub{subscribe: func(context.Context, interaction.SubscribeRequest) (interaction.EventStream, error) {
		return overflowStream{}, nil
	}}
	client, stop := startTestChannel(t, backend, agent.ActorIdentity{Kind: agent.ActorService, ID: "service-1"})
	defer stop()
	remote, err := client.Subscribe(context.Background(), interaction.SubscribeRequest{SessionID: "session-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Recv(context.Background()); !errors.Is(err, interaction.ErrEventStreamOverflow) {
		t.Fatalf("Recv error = %v, want ErrEventStreamOverflow", err)
	}
}

func TestSendAndWaitSurvivesClientCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	backend := &gatewayStub{sendAndWait: func(ctx context.Context, request interaction.SendRequest) (interaction.RunResult, error) {
		close(started)
		select {
		case <-release:
			close(finished)
			return interaction.RunResult{SessionID: request.SessionID, Revision: request.ExpectedRevision + 1}, nil
		case <-ctx.Done():
			return interaction.RunResult{}, ctx.Err()
		}
	}}
	client, stop := startTestChannel(t, backend, agent.ActorIdentity{Kind: agent.ActorRemoteUser, ID: "user-1"})
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.SendAndWait(ctx, interaction.SendRequest{SessionID: "session-1", ExpectedRevision: 2, Input: agent.MessageInput{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "run"}}}})
		done <- err
	}()
	<-started
	cancel()
	<-done
	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("client cancellation canceled the authoritative Run")
	}
}

func TestChannelRequiresAuthenticationAndAuthorization(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	if _, err := New(Config{Listener: listener}); err == nil {
		t.Fatal("New accepted a remote listener without authentication and authorization")
	}
}

func TestRemoteProfileDispatchesEveryGatewayOperation(t *testing.T) {
	backend := &completeGatewayStub{
		events:       &eventStream{events: []interaction.Event{{Kind: interaction.EventRevision, SessionID: "session-1", Revision: 2}}},
		closeReceipt: interaction.CloseSessionReceipt{SessionID: "session-1", Revision: 9, Diagnostics: []session.ExtensionDiagnostic{{InvocationID: "end-1", Status: hook.InvocationFailed}}},
	}
	client, stop := startTestChannel(t, backend, agent.ActorIdentity{Kind: agent.ActorAgent, ID: "agent-caller"})
	defer stop()
	ctx := context.Background()
	input := agent.MessageInput{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "x"}}}
	calls := []func() error{
		func() error { _, err := client.ListSessions(ctx, interaction.ListSessionsRequest{}); return err },
		func() error { _, err := client.CreateSession(ctx, interaction.CreateSessionRequest{}); return err },
		func() error { _, err := client.ResumeSession(ctx, interaction.ResumeSessionRequest{}); return err },
		func() error { _, err := client.ForkSession(ctx, interaction.ForkSessionRequest{}); return err },
		func() error {
			_, err := client.StartSessionFromSummary(ctx, interaction.SummarySessionRequest{})
			return err
		},
		func() error { _, err := client.Send(ctx, interaction.SendRequest{Input: input}); return err },
		func() error { _, err := client.SendAndWait(ctx, interaction.SendRequest{Input: input}); return err },
		func() error { _, err := client.Steer(ctx, interaction.SteerRequest{Input: input}); return err },
		func() error { _, err := client.RunPending(ctx, interaction.RunPendingRequest{}); return err },
		func() error { return client.Cancel(ctx, interaction.CancelRequest{}) },
		func() error { return client.WhenIdle(ctx, interaction.WhenIdleRequest{}) },
		func() error {
			_, err := client.EditQueued(ctx, interaction.EditQueuedRequest{Input: input})
			return err
		},
		func() error { _, err := client.DeleteQueued(ctx, interaction.DeleteQueuedRequest{}); return err },
		func() error {
			_, err := client.ReclassifyQueued(ctx, interaction.ReclassifyQueuedRequest{})
			return err
		},
		func() error { _, err := client.ModelConfig(ctx, interaction.ModelConfigRequest{}); return err },
		func() error {
			_, err := client.UpdateModelConfig(ctx, interaction.UpdateModelConfigRequest{})
			return err
		},
		func() error { _, err := client.View(ctx, interaction.SessionViewRequest{}); return err },
		func() error { _, err := client.History(ctx, interaction.HistoryRequest{}); return err },
		func() error {
			_, err := client.ExtensionDiagnostics(ctx, interaction.ExtensionDiagnosticsRequest{})
			return err
		},
		func() error { _, err := client.Commands(ctx, interaction.CommandScope{}); return err },
		func() error { _, err := client.InvokeCommand(ctx, interaction.CommandInvocation{}); return err },
		func() error {
			receipt, err := client.CloseSession(ctx, interaction.CloseSessionRequest{})
			if err == nil && (receipt.SessionID != "session-1" || receipt.Revision != 9 || len(receipt.Diagnostics) != 1) {
				return fmt.Errorf("close receipt lost across transport: %#v", receipt)
			}
			return err
		},
	}
	for index, call := range calls {
		if err := call(); err != nil {
			t.Fatalf("Gateway operation %d: %v", index, err)
		}
	}
	stream, err := client.Subscribe(ctx, interaction.SubscribeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(ctx); err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	if got, want := backend.count(), len(calls)+1; got != want {
		t.Fatalf("dispatched operations = %d, want %d", got, want)
	}
}

func startTestChannel(t *testing.T, backend interaction.GatewayAccess, actor agent.ActorIdentity) (*Client, func()) {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	channel, err := New(Config{
		Listener:     listener,
		Authenticate: func(context.Context) (agent.ActorIdentity, error) { return actor, nil },
		Authorize:    func(context.Context, agent.ActorIdentity, RequestScope) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := channel.Bind(backend); err != nil {
		t.Fatal(err)
	}
	if err := channel.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	connection, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}))
	if err != nil {
		t.Fatal(err)
	}
	return NewClient(connection), func() {
		_ = connection.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = channel.Stop(ctx)
	}
}

type gatewayStub struct {
	interaction.GatewayAccess
	create      func(context.Context, interaction.CreateSessionRequest) (interaction.SessionOpened, error)
	send        func(context.Context, interaction.SendRequest) (interaction.EnqueueReceipt, error)
	sendAndWait func(context.Context, interaction.SendRequest) (interaction.RunResult, error)
	subscribe   func(context.Context, interaction.SubscribeRequest) (interaction.EventStream, error)
}

func (g *gatewayStub) CreateSession(ctx context.Context, request interaction.CreateSessionRequest) (interaction.SessionOpened, error) {
	return g.create(ctx, request)
}
func (g *gatewayStub) Send(ctx context.Context, request interaction.SendRequest) (interaction.EnqueueReceipt, error) {
	return g.send(ctx, request)
}
func (g *gatewayStub) SendAndWait(ctx context.Context, request interaction.SendRequest) (interaction.RunResult, error) {
	return g.sendAndWait(ctx, request)
}
func (g *gatewayStub) Subscribe(ctx context.Context, request interaction.SubscribeRequest) (interaction.EventStream, error) {
	return g.subscribe(ctx, request)
}

type eventStream struct {
	mu     sync.Mutex
	events []interaction.Event
	closed bool
}

type overflowStream struct{}

func (overflowStream) Recv(context.Context) (interaction.Event, error) {
	return interaction.Event{}, interaction.ErrEventStreamOverflow
}
func (overflowStream) Close() error { return nil }

type completeGatewayStub struct {
	mu           sync.Mutex
	calls        int
	events       interaction.EventStream
	closeReceipt interaction.CloseSessionReceipt
}

func (g *completeGatewayStub) record()    { g.mu.Lock(); g.calls++; g.mu.Unlock() }
func (g *completeGatewayStub) count() int { g.mu.Lock(); defer g.mu.Unlock(); return g.calls }
func (g *completeGatewayStub) ListSessions(context.Context, interaction.ListSessionsRequest) (interaction.SessionList, error) {
	g.record()
	return interaction.SessionList{}, nil
}
func (g *completeGatewayStub) CreateSession(context.Context, interaction.CreateSessionRequest) (interaction.SessionOpened, error) {
	g.record()
	return interaction.SessionOpened{}, nil
}
func (g *completeGatewayStub) ResumeSession(context.Context, interaction.ResumeSessionRequest) (interaction.SessionOpened, error) {
	g.record()
	return interaction.SessionOpened{}, nil
}
func (g *completeGatewayStub) ForkSession(context.Context, interaction.ForkSessionRequest) (interaction.SessionOpened, error) {
	g.record()
	return interaction.SessionOpened{}, nil
}
func (g *completeGatewayStub) StartSessionFromSummary(context.Context, interaction.SummarySessionRequest) (interaction.SessionOpened, error) {
	g.record()
	return interaction.SessionOpened{}, nil
}
func (g *completeGatewayStub) Send(context.Context, interaction.SendRequest) (interaction.EnqueueReceipt, error) {
	g.record()
	return interaction.EnqueueReceipt{}, nil
}
func (g *completeGatewayStub) SendAndWait(context.Context, interaction.SendRequest) (interaction.RunResult, error) {
	g.record()
	return interaction.RunResult{}, nil
}
func (g *completeGatewayStub) Steer(context.Context, interaction.SteerRequest) (interaction.EnqueueReceipt, error) {
	g.record()
	return interaction.EnqueueReceipt{}, nil
}
func (g *completeGatewayStub) RunPending(context.Context, interaction.RunPendingRequest) (interaction.RunReceipt, error) {
	g.record()
	return interaction.RunReceipt{}, nil
}
func (g *completeGatewayStub) Cancel(context.Context, interaction.CancelRequest) error {
	g.record()
	return nil
}
func (g *completeGatewayStub) WhenIdle(context.Context, interaction.WhenIdleRequest) error {
	g.record()
	return nil
}
func (g *completeGatewayStub) EditQueued(context.Context, interaction.EditQueuedRequest) (interaction.CommitReceipt, error) {
	g.record()
	return interaction.CommitReceipt{}, nil
}
func (g *completeGatewayStub) DeleteQueued(context.Context, interaction.DeleteQueuedRequest) (interaction.CommitReceipt, error) {
	g.record()
	return interaction.CommitReceipt{}, nil
}
func (g *completeGatewayStub) ReclassifyQueued(context.Context, interaction.ReclassifyQueuedRequest) (interaction.CommitReceipt, error) {
	g.record()
	return interaction.CommitReceipt{}, nil
}
func (g *completeGatewayStub) ModelConfig(context.Context, interaction.ModelConfigRequest) (interaction.ModelConfigView, error) {
	g.record()
	return interaction.ModelConfigView{}, nil
}
func (g *completeGatewayStub) UpdateModelConfig(context.Context, interaction.UpdateModelConfigRequest) (interaction.CommitReceipt, error) {
	g.record()
	return interaction.CommitReceipt{}, nil
}
func (g *completeGatewayStub) View(context.Context, interaction.SessionViewRequest) (interaction.SessionView, error) {
	g.record()
	return interaction.SessionView{}, nil
}
func (g *completeGatewayStub) History(context.Context, interaction.HistoryRequest) (interaction.HistoryPage, error) {
	g.record()
	return interaction.HistoryPage{}, nil
}
func (g *completeGatewayStub) ExtensionDiagnostics(context.Context, interaction.ExtensionDiagnosticsRequest) (interaction.ExtensionDiagnosticsPage, error) {
	g.record()
	return interaction.ExtensionDiagnosticsPage{}, nil
}
func (g *completeGatewayStub) Subscribe(context.Context, interaction.SubscribeRequest) (interaction.EventStream, error) {
	g.record()
	return g.events, nil
}
func (g *completeGatewayStub) Commands(context.Context, interaction.CommandScope) ([]interaction.CommandDescriptor, error) {
	g.record()
	return []interaction.CommandDescriptor{}, nil
}
func (g *completeGatewayStub) InvokeCommand(context.Context, interaction.CommandInvocation) (interaction.CommandResult, error) {
	g.record()
	return interaction.CommandResult{}, nil
}
func (g *completeGatewayStub) CloseSession(context.Context, interaction.CloseSessionRequest) (interaction.CloseSessionReceipt, error) {
	g.record()
	return g.closeReceipt, nil
}

func (s *eventStream) Recv(context.Context) (interaction.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) == 0 {
		return interaction.Event{}, interaction.ErrEventStreamClosed
	}
	event := s.events[0]
	s.events = s.events[1:]
	return event, nil
}
func (s *eventStream) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}
