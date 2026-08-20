// Package inprocess provides the smallest caller-facing adapter for embedding
// a standard Agent application in the same Go process. It exposes only the
// fixed Gateway boundary; it does not provide Runtime or Store access.
package inprocess

import (
	"reflect"
	"sync"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
)

// Channel retains the GatewayAccess bound by standardagent Build.
// Multiple callers may obtain and use the returned capability concurrently.
type Channel struct {
	mu     sync.RWMutex
	access interaction.GatewayAccess
	bound  bool
}

func New() *Channel { return &Channel{} }

func (e *Channel) Bind(access interaction.GatewayAccess) error {
	if nilGateway(access) {
		return agent.NewError(agent.ErrorInvalidInput, "interaction.inprocess.bind", "GatewayAccess is required", nil)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.bound {
		return agent.NewError(agent.ErrorConflict, "interaction.inprocess.bind", "Channel is already bound", nil)
	}
	e.access = access
	e.bound = true
	return nil
}

// Access returns the public Gateway capability. The capability itself remains
// safe across application Start/Stop through the framework-owned binding.
func (e *Channel) Access() (interaction.GatewayAccess, error) {
	e.mu.RLock()
	access := e.access
	bound := e.bound
	e.mu.RUnlock()
	if !bound || nilGateway(access) {
		return nil, agent.NewCodedError(agent.ErrorUnavailable, agent.CodeApplicationNotStarted, "interaction.inprocess.access", "Channel is not bound", nil)
	}
	return access, nil
}

func nilGateway(value interaction.GatewayAccess) bool {
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
