package tool_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/LyleLiu666/agentSlot/tool"
)

func TestInputSchemaUsesPortableJSONSchemaDialect(t *testing.T) {
	if tool.InputSchemaDialect != "https://json-schema.org/draft/2020-12/schema" {
		t.Fatalf("InputSchemaDialect = %q", tool.InputSchemaDialect)
	}

	raw := []byte(`{
		"type":"object",
		"additionalProperties":false,
		"required":["path"],
		"properties":{"path":{"type":"string"}}
	}`)
	schema, err := tool.ParseInputSchema(raw)
	if err != nil {
		t.Fatalf("ParseInputSchema(): %v", err)
	}
	if len(schema.JSON()) == 0 {
		t.Fatal("parsed schema has no JSON representation")
	}

	copyOfJSON := schema.JSON()
	copyOfJSON[0] = '['
	if schema.JSON()[0] != '{' {
		t.Fatal("InputSchema.JSON returned mutable shared state")
	}
}

func TestInputSchemaRoundTripsThroughDurableJSON(t *testing.T) {
	original, err := tool.ParseInputSchema([]byte(`{"type":"object","additionalProperties":false,"required":["path"],"properties":{"path":{"type":"string"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var restored tool.InputSchema
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if err := restored.ValidateArguments([]byte(`{"path":"README.md"}`)); err != nil {
		t.Fatalf("restored schema rejected valid arguments: %v", err)
	}
	if err := restored.ValidateArguments([]byte(`{"unknown":true}`)); !errors.Is(err, tool.ErrInvalidArguments) {
		t.Fatalf("restored schema accepted invalid arguments: %v", err)
	}
}

func TestInputSchemaRejectsNonPortableShapes(t *testing.T) {
	tests := map[string]string{
		"invalid JSON":          `{`,
		"trailing garbage":      `{"type":"object","additionalProperties":false} trailing`,
		"non-object document":   `[]`,
		"missing object type":   `{"additionalProperties":false,"properties":{}}`,
		"open arguments":        `{"type":"object","properties":{}}`,
		"invalid properties":    `{"type":"object","additionalProperties":false,"properties":"path"}`,
		"external reference":    `{"type":"object","additionalProperties":false,"properties":{"item":{"$ref":"https://example.com/item.schema.json"}}}`,
		"unsupported dialect":   `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","additionalProperties":false,"properties":{}}`,
		"non-string schema ref": `{"type":"object","additionalProperties":false,"properties":{"item":{"$ref":12}}}`,
	}

	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := tool.ParseInputSchema([]byte(raw)); !errors.Is(err, tool.ErrInvalidInputSchema) {
				t.Fatalf("ParseInputSchema() error = %v, want ErrInvalidInputSchema", err)
			}
		})
	}
}

func TestToolCallSeparatesSchemaFromArgumentValues(t *testing.T) {
	schema, err := tool.ParseInputSchema([]byte(`{
		"type":"object",
		"additionalProperties":false,
		"required":["path"],
		"properties":{"path":{"type":"string"}}
	}`))
	if err != nil {
		t.Fatalf("ParseInputSchema(): %v", err)
	}

	definition := tool.Definition{
		Name:        "read_file",
		Description: "Read one file from the workspace.",
		InputSchema: schema,
	}
	call := tool.Call{
		ID:        "call-1",
		Name:      definition.Name,
		Arguments: []byte(`{"path":"README.md"}`),
	}

	if call.Name != definition.Name || string(call.Arguments) != `{"path":"README.md"}` {
		t.Fatalf("unexpected tool call: %#v", call)
	}
	if err := definition.InputSchema.ValidateArguments(call.Arguments); err != nil {
		t.Fatalf("ValidateArguments(valid): %v", err)
	}
	if err := definition.InputSchema.ValidateArguments([]byte(`{}`)); !errors.Is(err, tool.ErrInvalidArguments) {
		t.Fatalf("ValidateArguments(missing path) error = %v, want ErrInvalidArguments", err)
	}
	if err := definition.InputSchema.ValidateArguments([]byte(`not-json`)); !errors.Is(err, tool.ErrInvalidArguments) {
		t.Fatalf("ValidateArguments(invalid JSON) error = %v, want ErrInvalidArguments", err)
	}
}

func TestZeroInputSchemaCannotValidateArguments(t *testing.T) {
	var schema tool.InputSchema
	if err := schema.ValidateArguments([]byte(`{}`)); !errors.Is(err, tool.ErrInvalidInputSchema) {
		t.Fatalf("ValidateArguments() error = %v, want ErrInvalidInputSchema", err)
	}
}
