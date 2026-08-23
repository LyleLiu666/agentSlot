package model

import (
	"context"
	"errors"
	"reflect"
	"strings"

	agentslot "github.com/LyleLiu666/agentSlot"
)

// TokenCounterSlot owns planning-time measurement of the complete request
// that will be visible to the selected provider.
var TokenCounterSlot = agentslot.One[TokenCounter]("model.token-counter")

// TokenCounter measures one complete fixed ModelRequest without mutating it.
// Implementations must account for provider-visible framing, tools, schemas,
// attachments, and any other wire projection owned by the selected adapter.
type TokenCounter interface {
	CountTokens(context.Context, ModelRequest) (int, error)
}

// TokenCounterFunc adapts a function to TokenCounter.
type TokenCounterFunc func(context.Context, ModelRequest) (int, error)

func (f TokenCounterFunc) CountTokens(ctx context.Context, request ModelRequest) (int, error) {
	if f == nil {
		return 0, errors.New("model: TokenCounterFunc is nil")
	}
	return f(ctx, request)
}

// NewTokenCounterModule wraps one explicit TokenCounter implementation for
// normal AgentSlot assembly. Provider modules commonly contribute their own
// counter alongside, but independently from, their ModelExecutor.
func NewTokenCounterModule(id string, counter TokenCounter) (agentslot.Module, error) {
	if id == "" || strings.TrimSpace(id) != id {
		return nil, errors.New("model: token counter module ID must be non-empty without surrounding whitespace")
	}
	if nilTokenCounter(counter) {
		return nil, errors.New("model: TokenCounter is required")
	}
	return &tokenCounterModule{id: id, counter: counter}, nil
}

type tokenCounterModule struct {
	id      string
	counter TokenCounter
}

func (m *tokenCounterModule) ID() string { return m.id }

func (m *tokenCounterModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Set(TokenCounterSlot, m.counter))
}

func nilTokenCounter(counter TokenCounter) bool {
	if counter == nil {
		return true
	}
	value := reflect.ValueOf(counter)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
