// Package tool defines provider-neutral tool vocabulary shared by AgentSlot
// tool contracts and adapters.
package tool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	agent "github.com/LyleLiu666/agentSlot/agent"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	// InputSchemaDialect is the fixed schema vocabulary for model-facing tool
	// arguments. JSON Schema 2020-12 schemas are usable as OpenAPI 3.1 Schema
	// Objects without making every tool an HTTP API.
	InputSchemaDialect = "https://json-schema.org/draft/2020-12/schema"
)

var (
	// ErrInvalidInputSchema reports a tool input schema outside AgentSlot's
	// self-contained portable envelope.
	ErrInvalidInputSchema = errors.New("invalid tool input schema")
	// ErrInvalidArguments reports call arguments that are not valid JSON or do
	// not conform to their tool's InputSchema.
	ErrInvalidArguments = errors.New("invalid tool arguments")
)

// InputSchema is a detached, canonical JSON representation of a tool's named
// arguments. The root is always a closed object schema. Provider adapters may
// impose smaller keyword or size subsets but may not reinterpret the schema.
type InputSchema struct {
	json     []byte
	compiled *jsonschema.Schema
}

// ParseInputSchema validates the portable AgentSlot envelope and returns a
// detached canonical representation. Full instance validation remains the
// responsibility of the tool invocation boundary.
func ParseInputSchema(raw []byte) (InputSchema, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return InputSchema{}, fmt.Errorf("%w: %v", ErrInvalidInputSchema, err)
	}
	if document == nil {
		return InputSchema{}, fmt.Errorf("%w: schema must be a JSON object", ErrInvalidInputSchema)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return InputSchema{}, fmt.Errorf("%w: schema contains trailing data", ErrInvalidInputSchema)
	}
	if document["type"] != "object" {
		return InputSchema{}, fmt.Errorf("%w: top-level type must be object", ErrInvalidInputSchema)
	}
	if additional, exists := document["additionalProperties"]; !exists || additional != false {
		return InputSchema{}, fmt.Errorf("%w: top-level additionalProperties must be false", ErrInvalidInputSchema)
	}
	if dialect, exists := document["$schema"]; exists && dialect != InputSchemaDialect {
		return InputSchema{}, fmt.Errorf("%w: unsupported $schema %q", ErrInvalidInputSchema, dialect)
	}
	if hasExternalReference(document) {
		return InputSchema{}, fmt.Errorf("%w: external schema references are forbidden", ErrInvalidInputSchema)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	const resource = "urn:agentslot:tool-input-schema"
	if err := compiler.AddResource(resource, document); err != nil {
		return InputSchema{}, fmt.Errorf("%w: %v", ErrInvalidInputSchema, err)
	}
	compiled, err := compiler.Compile(resource)
	if err != nil {
		return InputSchema{}, fmt.Errorf("%w: %v", ErrInvalidInputSchema, err)
	}
	canonical, err := json.Marshal(document)
	if err != nil {
		return InputSchema{}, fmt.Errorf("%w: %v", ErrInvalidInputSchema, err)
	}
	return InputSchema{json: canonical, compiled: compiled}, nil
}

// JSON returns a detached copy of the canonical schema document.
func (s InputSchema) JSON() json.RawMessage {
	return append(json.RawMessage(nil), s.json...)
}

// ValidateArguments verifies one JSON argument value against the compiled
// input schema. Provider adapters and invocation runtimes use the same method
// so schema enforcement cannot drift between model protocols.
func (s InputSchema) ValidateArguments(raw []byte) error {
	if s.compiled == nil {
		return fmt.Errorf("%w: uninitialized schema", ErrInvalidInputSchema)
	}
	arguments, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArguments, err)
	}
	if err := s.compiled.Validate(arguments); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArguments, err)
	}
	return nil
}

// Definition is the provider-neutral, model-facing description of one tool.
type Definition struct {
	Name        string
	Description string
	InputSchema InputSchema
}

// Call is one model-requested tool invocation. Arguments are JSON instance
// values that must conform to the Definition's InputSchema; they are not a
// schema document.
type Call struct {
	ID        agent.ToolCallID
	Name      string
	Arguments json.RawMessage
}

func hasExternalReference(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "$ref" || key == "$dynamicRef" {
				reference, ok := child.(string)
				if !ok || !strings.HasPrefix(reference, "#") {
					return true
				}
			}
			if hasExternalReference(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if hasExternalReference(child) {
				return true
			}
		}
	}
	return false
}
