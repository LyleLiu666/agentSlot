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

// Entrypoint retains the GatewayAccess attached by standardagent Build.
// Multiple callers may obtain and use the returned capability concurrently.
type Entrypoint struct {
	mu       sync.RWMutex
	access   interaction.GatewayAccess
	attached bool
}

func New() *Entrypoint { return &Entrypoint{} }

func (e *Entrypoint) Attach(access interaction.GatewayAccess) error {
	if nilGateway(access) {
		return agent.NewError(agent.ErrorInvalidInput, "interaction.inprocess.attach", "GatewayAccess is required", nil)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.attached {
		return agent.NewError(agent.ErrorConflict, "interaction.inprocess.attach", "Entrypoint is already attached", nil)
	}
	e.access = access
	e.attached = true
	return nil
}

// Access returns the public Gateway capability. The capability itself remains
// safe across application Start/Stop through the framework-owned binding.
func (e *Entrypoint) Access() (interaction.GatewayAccess, error) {
	e.mu.RLock()
	access := e.access
	attached := e.attached
	e.mu.RUnlock()
	if !attached || nilGateway(access) {
		return nil, agent.NewCodedError(agent.ErrorUnavailable, agent.CodeApplicationNotStarted, "interaction.inprocess.access", "Entrypoint is not attached", nil)
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

var _ interaction.Entrypoint = (*Entrypoint)(nil)
