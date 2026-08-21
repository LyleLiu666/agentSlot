package loop

import (
	"errors"
	"reflect"
	"strings"

	agentslot "github.com/LyleLiu666/agentSlot"
)

// NewModule wraps one explicit AgentLoop implementation for normal AgentSlot
// assembly. Implementations that own lifecycle resources can instead provide a
// custom Module with Start and Stop.
func NewModule(id string, component AgentLoop) (agentslot.Module, error) {
	if id == "" || strings.TrimSpace(id) != id {
		return nil, errors.New("loop: module ID must be non-empty without surrounding whitespace")
	}
	if nilAgentLoop(component) {
		return nil, errors.New("loop: AgentLoop is required")
	}
	return &module{id: id, component: component}, nil
}

type module struct {
	id        string
	component AgentLoop
}

func (m *module) ID() string { return m.id }

func (m *module) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Set(AgentLoopSlot, m.component))
}

func nilAgentLoop(component AgentLoop) bool {
	if component == nil {
		return true
	}
	value := reflect.ValueOf(component)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
