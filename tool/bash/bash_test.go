package bash_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/tool"
	"github.com/LyleLiu666/agentSlot/tool/bash"
)

func TestBashUsesConfiguredDirectoryAndExplicitEnvironmentOnly(t *testing.T) {
	t.Setenv("AGENTSLOT_SECRET", "must-not-leak")
	directory := t.TempDir()
	installed := newBash(t, directory, map[string]string{"VISIBLE": "yes"}, time.Second, 1024)
	result := invoke(installed, `printf '%s|%s|%s' "$PWD" "$VISIBLE" "${AGENTSLOT_SECRET-unset}"`)
	if result.Status != tool.ResultSucceeded {
		t.Fatalf("result = %#v", result)
	}
	output := decode(t, result)
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	if output.Stdout != canonical+"|yes|unset" {
		t.Fatalf("stdout = %q", output.Stdout)
	}
}

func TestBashReturnsNonZeroExitAndSeparatedLimitedOutput(t *testing.T) {
	installed := newBash(t, t.TempDir(), nil, time.Second, 4)
	result := invoke(installed, `printf 'abcdef'; printf 'uvwxyz' >&2; exit 7`)
	if result.Status != tool.ResultSucceeded {
		t.Fatalf("non-zero command result = %#v", result)
	}
	output := decode(t, result)
	if output.ExitCode != 7 || output.Stdout != "abcd" || output.Stderr != "uvwx" || !output.StdoutTruncated || !output.StderrTruncated {
		t.Fatalf("output = %#v", output)
	}
}

func TestBashTimeoutIsStructuredFailure(t *testing.T) {
	directory := t.TempDir()
	installed := newBash(t, directory, nil, 20*time.Millisecond, 1024)
	result := invoke(installed, `(sleep 0.1; printf leaked > child-output) & wait`)
	if result.Status != tool.ResultFailed || result.Error == nil || result.Error.Code != "timeout" {
		t.Fatalf("timeout result = %#v", result)
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("timeout result invalid: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(directory, "child-output")); !os.IsNotExist(err) {
		t.Fatalf("timed-out Bash left a child process running: %v", err)
	}
}

func TestBashRequiresExplicitAbsoluteBoundary(t *testing.T) {
	_, err := bash.New(bash.Config{WorkingDirectory: "relative", Timeout: time.Second, MaxStdoutBytes: 1, MaxStderrBytes: 1})
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative config error = %v", err)
	}
	if _, err := bash.NewModule(bash.Config{WorkingDirectory: filepath.Clean(t.TempDir()), Timeout: time.Second, MaxStdoutBytes: 1, MaxStderrBytes: 1}); err != nil {
		t.Fatalf("explicit module: %v", err)
	}
}

func newBash(t *testing.T, directory string, environment map[string]string, timeout time.Duration, limit int) *bash.Bash {
	t.Helper()
	installed, err := bash.New(bash.Config{
		WorkingDirectory: directory, Environment: environment, Timeout: timeout,
		MaxStdoutBytes: limit, MaxStderrBytes: limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	return installed
}

func invoke(installed *bash.Bash, command string) tool.ToolResult {
	arguments, _ := json.Marshal(map[string]string{"command": command})
	return installed.Invoke(context.Background(), tool.ToolInvocation{
		Call:      tool.Call{ID: agent.ToolCallID("call-1"), Name: bash.Key, Arguments: arguments},
		SessionID: "session-1", RunID: "run-1", StepID: "step-1",
	})
}

func decode(t *testing.T, result tool.ToolResult) bash.Output {
	t.Helper()
	var output bash.Output
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	return output
}
