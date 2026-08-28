package session

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	agent "github.com/LyleLiu666/agentSlot/agent"
)

const knownFailureEnvironment = "AGENTSLOT_RUN_KNOWN_FAILURES"

func TestReliabilityToolArgumentFixturesDescribeTheSameJSONValue(t *testing.T) {
	pretty := readReliabilityFixture(t, "tool-arguments-pretty.json")
	restored := readReliabilityFixture(t, "tool-arguments-restored.json")
	if bytes.Equal(pretty, restored) {
		t.Fatal("fixture must preserve distinct JSON representations")
	}
	var left, right any
	if err := json.Unmarshal(pretty, &left); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(restored, &right); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(left, right) {
		t.Fatalf("fixture JSON values differ: left=%#v right=%#v", left, right)
	}
}

func TestKnownFailureToolCallIdentitySurvivesEquivalentJSONRepresentation(t *testing.T) {
	requireKnownFailures(t)
	left := agent.ToolCall{
		ID: "tool-1", CorrelationID: "provider-tool-1", MessageID: "message-1",
		SessionID: "session-1", RunID: "run-1", StepID: "step-1", Name: "example",
		Arguments: readReliabilityFixture(t, "tool-arguments-pretty.json"),
	}
	right := left
	right.Arguments = readReliabilityFixture(t, "tool-arguments-restored.json")
	if !sameToolCall(left, right) {
		t.Fatal("equivalent JSON representation changed the prepared ToolCall identity")
	}
}

func requireKnownFailures(t *testing.T) {
	t.Helper()
	if os.Getenv(knownFailureEnvironment) != "1" {
		t.Skip("set AGENTSLOT_RUN_KNOWN_FAILURES=1 to execute documented red regressions")
	}
}

func readReliabilityFixture(t *testing.T, name string) json.RawMessage {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("testdata", "reliability", name))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(contents) {
		t.Fatalf("fixture %q is not valid JSON", name)
	}
	return json.RawMessage(contents)
}
