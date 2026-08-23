// Package acpchannel adapts the stable Agent Client Protocol v1 surface to an
// AgentSlot Gateway. It is an inbound gateway.channel, not an Agent provider.
package acpchannel

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"sync"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	acp "github.com/coder/acp-go-sdk"
)

const Profile = "gateway.channel/inbound-acp/v1"

type Operation string

const (
	OperationInitialize    Operation = "initialize"
	OperationListSessions  Operation = "list_sessions"
	OperationCreateSession Operation = "create_session"
	OperationResumeSession Operation = "resume_session"
	OperationPrompt        Operation = "prompt"
	OperationCancel        Operation = "cancel"
	OperationCloseSession  Operation = "close_session"
)

type RequestScope struct {
	Operation   Operation
	AgentID     agent.AgentID
	WorkspaceID agent.WorkspaceID
	SessionID   agent.SessionID
}

type Authorizer func(context.Context, agent.ActorIdentity, RequestScope) error

// Config describes one already-authenticated ACP transport. Authentication is
// deliberately owned by the product transport boundary; Actor is the trusted
// result, never a value accepted from ACP metadata.
type Config struct {
	Input            io.Reader
	Output           io.Writer
	Close            func() error
	Actor            agent.ActorIdentity
	Authorize        Authorizer
	AgentID          agent.AgentID
	WorkspaceID      agent.WorkspaceID
	WorkingDirectory string
	AgentName        string
	AgentVersion     string
}

type Channel struct {
	config     Config
	mu         sync.Mutex
	access     interaction.GatewayAccess
	started    bool
	stopped    bool
	lifecycle  context.Context
	cancel     context.CancelFunc
	connection *acp.AgentSideConnection
	adapter    *adapter
}

func New(config Config) (*Channel, error) {
	if config.Input == nil || config.Output == nil || config.Close == nil || config.Authorize == nil {
		return nil, errors.New("acpchannel: input, output, close, and authorization are required")
	}
	if !config.Actor.Valid() || config.Actor.Kind == agent.ActorLocalUser {
		return nil, errors.New("acpchannel: a trusted remote, service, or agent identity is required")
	}
	if !config.AgentID.Valid() || !config.WorkspaceID.Valid() {
		return nil, errors.New("acpchannel: fixed AgentID and WorkspaceID are required")
	}
	if config.WorkingDirectory == "" || !filepath.IsAbs(config.WorkingDirectory) {
		return nil, errors.New("acpchannel: WorkingDirectory must be absolute")
	}
	if config.AgentName == "" {
		config.AgentName = "agentslot"
	}
	if config.AgentVersion == "" {
		config.AgentVersion = "dev"
	}
	return &Channel{config: config}, nil
}

func (c *Channel) Bind(access interaction.GatewayAccess) error {
	if nilInterface(access) {
		return errors.New("acpchannel: GatewayAccess is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started || c.stopped || c.access != nil {
		return errors.New("acpchannel: channel is already bound or started")
	}
	c.access = access
	return nil
}

func (c *Channel) Start(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.access == nil || c.started || c.stopped {
		return errors.New("acpchannel: channel must be bound exactly once before Start")
	}
	c.lifecycle, c.cancel = context.WithCancel(context.Background())
	c.adapter = newAdapter(c.config, c.access, c.lifecycle)
	c.connection = acp.NewAgentSideConnection(c.adapter, c.config.Output, c.config.Input)
	c.adapter.setConnection(c.connection)
	c.started = true
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
	connection := c.connection
	closeTransport := c.config.Close
	c.mu.Unlock()

	closeErr := closeTransport()
	select {
	case <-connection.Done():
		return closeErr
	case <-ctx.Done():
		return errors.Join(closeErr, ctx.Err())
	}
}

func nilInterface(value any) bool {
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
