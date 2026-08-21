// Package artifact defines stable storage contracts for binary or textual
// content referenced by durable Agent messages.
//
// History stores only an artifact ID and safe metadata. Provider adapters open
// that ID through Store when they need the bytes for a model request. This
// keeps filesystem paths, object-store details, and encoded binary data out of
// the provider-neutral message contract.
package artifact

import (
	"context"
	"errors"
	"io"
	"mime"
	"reflect"
	"strings"

	agentslot "github.com/LyleLiu666/agentSlot"
)

// StoreSlot is the standard immutable artifact storage ecosystem.
var StoreSlot = agentslot.One[ArtifactStore]("artifact.store")

// ErrNotFound reports that an artifact ID is unknown or no longer retained.
// Store implementations should wrap this value so callers can use errors.Is.
var ErrNotFound = errors.New("artifact: not found")

// Metadata is the durable, provider-neutral description of stored content.
// ID is opaque to consumers. MediaType is a canonical MIME type without
// parameters. Size is the exact number of stored bytes.
type Metadata struct {
	ID        string
	MediaType string
	Name      string
	Size      int64
}

// Validate checks the portable artifact metadata invariants.
func (m Metadata) Validate() error {
	if m.ID == "" || strings.TrimSpace(m.ID) != m.ID {
		return errors.New("artifact: ID must be non-empty without surrounding whitespace")
	}
	if err := validateMediaType(m.MediaType); err != nil {
		return err
	}
	if m.Size < 0 {
		return errors.New("artifact: size cannot be negative")
	}
	return nil
}

// WriteRequest supplies content for one immutable artifact. Write must consume
// Body before it returns and must not retain the reader. Name is display-only
// metadata and may be empty.
type WriteRequest struct {
	MediaType string
	Name      string
	Body      io.Reader
}

// Validate checks the caller-owned portion of an artifact write.
func (r WriteRequest) Validate() error {
	if err := validateMediaType(r.MediaType); err != nil {
		return err
	}
	if nilInterface(r.Body) {
		return errors.New("artifact: body is required")
	}
	return nil
}

// Content is one opened immutable artifact. The caller must close Body. A
// successful Store.Open returns metadata whose ID equals the requested ID and
// whose Size equals the readable content length.
type Content struct {
	Metadata Metadata
	Body     io.ReadCloser
}

// Validate checks the metadata and readable body returned by a Store.
func (c Content) Validate() error {
	if err := c.Metadata.Validate(); err != nil {
		return err
	}
	if nilInterface(c.Body) {
		return errors.New("artifact: opened body is required")
	}
	return nil
}

// ArtifactStore persists immutable content and resolves stable IDs. Write returns only
// after the content is durable for this Store's documented retention scope.
// Open never exposes a local path or provider credential.
type ArtifactStore interface {
	Write(context.Context, WriteRequest) (Metadata, error)
	Open(context.Context, string) (Content, error)
}

// NewModule wraps one explicit Store implementation for normal AgentSlot
// assembly. Implementations that own lifecycle resources can instead provide a
// custom Module with Start and Stop.
func NewModule(id string, store ArtifactStore) (agentslot.Module, error) {
	if id == "" || strings.TrimSpace(id) != id {
		return nil, errors.New("artifact: module ID must be non-empty without surrounding whitespace")
	}
	if nilInterface(store) {
		return nil, errors.New("artifact: Store is required")
	}
	return &module{id: id, store: store}, nil
}

type module struct {
	id    string
	store ArtifactStore
}

func (m *module) ID() string { return m.id }

func (m *module) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Set(StoreSlot, m.store))
}

func validateMediaType(value string) error {
	parsed, parameters, err := mime.ParseMediaType(value)
	if err != nil || value == "" || value != strings.ToLower(value) || parsed != value || len(parameters) != 0 {
		return errors.New("artifact: media type must be a canonical lowercase MIME type without parameters")
	}
	return nil
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
