// Package grpcchannel provides the runnable out-of-process gRPC example for
// the complete interaction.GatewayAccess contract. It is one
// gateway.channel implementation, not a second Gateway or transport Slot.
package grpcchannel

import (
	"context"
	"errors"
	"net"
	"reflect"
	"sync"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	"google.golang.org/grpc"
)

const Profile = "gateway.channel/remote-grpc/v1"

type Operation string

const (
	OperationListSessions            Operation = "list_sessions"
	OperationCreateSession           Operation = "create_session"
	OperationResumeSession           Operation = "resume_session"
	OperationForkSession             Operation = "fork_session"
	OperationStartSessionFromSummary Operation = "start_session_from_summary"
	OperationSend                    Operation = "send"
	OperationSendAndWait             Operation = "send_and_wait"
	OperationSteer                   Operation = "steer"
	OperationRunPending              Operation = "run_pending"
	OperationCancel                  Operation = "cancel"
	OperationWhenIdle                Operation = "when_idle"
	OperationEditQueued              Operation = "edit_queued"
	OperationDeleteQueued            Operation = "delete_queued"
	OperationReclassifyQueued        Operation = "reclassify_queued"
	OperationModelConfig             Operation = "model_config"
	OperationUpdateModelConfig       Operation = "update_model_config"
	OperationView                    Operation = "view"
	OperationHistory                 Operation = "history"
	OperationExtensionDiagnostics    Operation = "extension_diagnostics"
	OperationSubscribe               Operation = "subscribe"
	OperationCommands                Operation = "commands"
	OperationInvokeCommand           Operation = "invoke_command"
	OperationCloseSession            Operation = "close_session"
)

type RequestScope struct {
	Operation   Operation
	AgentID     agent.AgentID
	WorkspaceID agent.WorkspaceID
	SessionID   agent.SessionID
}

type Authenticator func(context.Context) (agent.ActorIdentity, error)
type Authorizer func(context.Context, agent.ActorIdentity, RequestScope) error

type Config struct {
	Listener         net.Listener
	Authenticate     Authenticator
	Authorize        Authorizer
	MaxRequestBytes  int
	MaxResponseBytes int
	ServerOptions    []grpc.ServerOption
}

type Channel struct {
	config Config
	server *grpc.Server

	mu        sync.Mutex
	access    interaction.GatewayAccess
	started   bool
	stopped   bool
	lifecycle context.Context
	cancel    context.CancelFunc
	serveDone chan error
}

func New(config Config) (*Channel, error) {
	if config.Listener == nil || config.Authenticate == nil || config.Authorize == nil {
		return nil, errors.New("grpcchannel: listener, authentication, and authorization are required")
	}
	if config.MaxRequestBytes == 0 {
		config.MaxRequestBytes = 8 << 20
	}
	if config.MaxResponseBytes == 0 {
		config.MaxResponseBytes = 32 << 20
	}
	if config.MaxRequestBytes < 1 || config.MaxResponseBytes < 1 {
		return nil, errors.New("grpcchannel: message limits must be positive")
	}
	options := append([]grpc.ServerOption{grpc.MaxRecvMsgSize(config.MaxRequestBytes)}, config.ServerOptions...)
	channel := &Channel{config: config, serveDone: make(chan error, 1)}
	channel.server = grpc.NewServer(options...)
	registerGatewayService(channel.server, channel)
	return channel, nil
}

func (c *Channel) Bind(access interaction.GatewayAccess) error {
	if isNilAccess(access) {
		return errors.New("grpcchannel: GatewayAccess is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started || c.stopped || c.access != nil {
		return errors.New("grpcchannel: channel is already bound or started")
	}
	c.access = access
	return nil
}

func (c *Channel) Start(context.Context) error {
	c.mu.Lock()
	if c.access == nil || c.started || c.stopped {
		c.mu.Unlock()
		return errors.New("grpcchannel: channel must be bound exactly once before Start")
	}
	c.lifecycle, c.cancel = context.WithCancel(context.Background())
	c.started = true
	c.mu.Unlock()
	go func() { c.serveDone <- c.server.Serve(c.config.Listener) }()
	return nil
}

func (c *Channel) Stop(ctx context.Context) error {
	c.mu.Lock()
	if !c.started || c.stopped {
		c.mu.Unlock()
		return nil
	}
	c.stopped = true
	c.cancel()
	c.mu.Unlock()
	done := make(chan struct{})
	go func() {
		c.server.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		c.server.Stop()
		<-done
		return ctx.Err()
	}
	select {
	case err := <-c.serveDone:
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return errors.New("grpcchannel: server stopped unexpectedly")
		}
	default:
	}
	return nil
}

func (c *Channel) operationContext(parent context.Context, detached bool) (context.Context, context.CancelFunc) {
	if !detached {
		return context.WithCancel(parent)
	}
	c.mu.Lock()
	lifecycle := c.lifecycle
	c.mu.Unlock()
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	stop := context.AfterFunc(lifecycle, cancel)
	return ctx, func() { stop(); cancel() }
}

func isNilAccess(value interaction.GatewayAccess) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

var _ interaction.GatewayChannel = (*Channel)(nil)
var _ agentslot.Lifecycle = (*Channel)(nil)
