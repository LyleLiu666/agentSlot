// Package credential defines late-bound access to outbound credentials.
// Product configuration stores only Ref values. Raw material is exposed only
// to the callback of one physical outbound operation and is never an Assembly,
// Session, observation, usage, billing, or audit value.
package credential

import (
	"context"
	"errors"
	"reflect"
	"strings"

	agentslot "github.com/LyleLiu666/agentSlot"
)

var ResolverSlot = agentslot.One[Resolver]("credential.resolver")

var (
	ErrNotFound     = errors.New("credential: not found")
	ErrUnavailable  = errors.New("credential: unavailable")
	ErrKindMismatch = errors.New("credential: kind mismatch")
)

// Ref is a product-issued, non-secret reference. Natural-language model output
// and Tool arguments must not choose or search these references.
type Ref struct {
	ID string
}

func (r Ref) Validate() error {
	if r.ID == "" || strings.TrimSpace(r.ID) != r.ID || len(r.ID) > 256 {
		return errors.New("credential: Ref ID must contain 1 to 256 bytes without surrounding whitespace")
	}
	return nil
}

type Kind string

const (
	KindBearer Kind = "bearer"
	KindBasic  Kind = "basic"
)

func (k Kind) Valid() bool { return k == KindBearer || k == KindBasic }

// Identity is the only credential identity safe to copy into billing or audit
// facts. Fingerprint must be opaque and non-reversible; implementations must
// not derive it by directly hashing low-entropy secret material.
type Identity struct {
	Fingerprint string
}

func (i Identity) Validate() error {
	if i.Fingerprint == "" || strings.TrimSpace(i.Fingerprint) != i.Fingerprint || len(i.Fingerprint) > 256 {
		return errors.New("credential: fingerprint must contain 1 to 256 bytes without surrounding whitespace")
	}
	return nil
}

// Material is a short-lived tagged credential value. Exactly the fields for
// Kind are populated. Resolver implementations clear the callback-owned byte
// slices after the callback returns, but callers can still make copies; the Go
// runtime cannot provide a universal zeroization guarantee.
type Material struct {
	Kind     Kind
	Token    []byte
	Username []byte
	Password []byte
}

func (m Material) Validate() error {
	if !m.Kind.Valid() {
		return errors.New("credential: invalid material kind")
	}
	switch m.Kind {
	case KindBearer:
		if len(m.Token) == 0 || len(m.Username) != 0 || len(m.Password) != 0 {
			return errors.New("credential: bearer material requires only a token")
		}
	case KindBasic:
		if len(m.Username) == 0 || len(m.Password) == 0 || len(m.Token) != 0 {
			return errors.New("credential: basic material requires only username and password")
		}
	}
	return nil
}

func (m *Material) Clear() {
	clear(m.Token)
	clear(m.Username)
	clear(m.Password)
	*m = Material{}
}

func cloneMaterial(source Material) Material {
	return Material{
		Kind: source.Kind, Token: append([]byte(nil), source.Token...),
		Username: append([]byte(nil), source.Username...), Password: append([]byte(nil), source.Password...),
	}
}

type Request struct {
	Ref  Ref
	Kind Kind
}

func (r Request) Validate() error {
	if err := r.Ref.Validate(); err != nil {
		return err
	}
	if !r.Kind.Valid() {
		return errors.New("credential: requested kind is invalid")
	}
	return nil
}

type Consumer func(Material) error

// Resolver exposes material only during consume. Resolution happens at the
// physical outbound-operation boundary, not while building an Assembly or
// preparing durable Session state.
type Resolver interface {
	Resolve(context.Context, Request, Consumer) (Identity, error)
}

func NewModule(id string, resolver Resolver) (agentslot.Module, error) {
	if id == "" || strings.TrimSpace(id) != id {
		return nil, errors.New("credential: module ID is required without surrounding whitespace")
	}
	if nilResolver(resolver) {
		return nil, errors.New("credential: Resolver is required")
	}
	return module{id: id, resolver: resolver}, nil
}

type module struct {
	id       string
	resolver Resolver
}

func (m module) ID() string { return m.id }
func (m module) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Set(ResolverSlot, m.resolver))
}

func nilResolver(value Resolver) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	return reflected.Kind() == reflect.Pointer && reflected.IsNil()
}
