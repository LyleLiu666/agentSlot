package agentslot_test

import (
	"os/exec"
	"strings"
	"testing"
)

func TestReliabilityGateDeclaresEveryDeterministicStage(t *testing.T) {
	output, err := exec.Command("sh", "scripts/reliability-gate.sh", "--list").CombinedOutput()
	if err != nil {
		t.Fatalf("list reliability gate: %v\n%s", err, output)
	}
	for _, stage := range []string{
		"agentslot:format",
		"agentslot:fault-injection",
		"agentslot:race",
		"agentslot:vet",
		"agentslot:build",
	} {
		if !strings.Contains(string(output), stage) {
			t.Fatalf("gate plan = %q, missing %q", output, stage)
		}
	}
}
