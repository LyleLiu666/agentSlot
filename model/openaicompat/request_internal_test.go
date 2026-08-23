package openaicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LyleLiu666/agentSlot/artifact"
	"github.com/LyleLiu666/agentSlot/tool"
)

func TestToolResultProjectionExposesStableArtifactMetadataWithoutStorageLocation(t *testing.T) {
	projection := toolResultProjectionFor(tool.ToolResult{
		Output:    json.RawMessage(`{"preview":"bounded"}`),
		Artifacts: []artifact.Metadata{{ID: "artifact-1", MediaType: "text/plain", Name: "full.txt", Size: 4096}},
	})
	encoded, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, expected := range []string{`"id":"artifact-1"`, `"media_type":"text/plain"`, `"name":"full.txt"`, `"size":4096`, `"preview":"bounded"`} {
		if !strings.Contains(text, expected) {
			t.Fatalf("projection %s lacks %s", text, expected)
		}
	}
	if strings.Contains(text, "path") || strings.Contains(text, "url") || strings.Contains(text, "key") {
		t.Fatalf("projection exposed storage location: %s", text)
	}
}
