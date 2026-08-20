// Package bash provides the explicitly installed built-in Bash Tool.
package bash

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/tool"
)

const (
	Key      = "bash"
	moduleID = "tool.builtin.bash"
)

// Config fixes the execution boundary for every invocation. Environment is
// explicit and never merged with the Agent process environment.
type Config struct {
	WorkingDirectory string
	Environment      map[string]string
	Timeout          time.Duration
	MaxStdoutBytes   int
	MaxStderrBytes   int
	Executable       string
}

// Output is the stable structured Bash result returned to the model.
type Output struct {
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	ExitCode        int    `json:"exit_code"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
}

type arguments struct {
	Command string `json:"command"`
}

// Bash executes one command with fixed directory, environment, timeout, and
// independent stdout/stderr limits.
type Bash struct {
	config     Config
	definition tool.Definition
}

var _ tool.Tool = (*Bash)(nil)

// New validates a Bash boundary once at assembly time.
func New(config Config) (*Bash, error) {
	if err := platformSupported(); err != nil {
		return nil, err
	}
	if config.WorkingDirectory == "" || !filepath.IsAbs(config.WorkingDirectory) {
		return nil, errors.New("bash: working directory must be an absolute path")
	}
	if config.Timeout <= 0 || config.MaxStdoutBytes <= 0 || config.MaxStderrBytes <= 0 {
		return nil, errors.New("bash: timeout and output limits must be positive")
	}
	if config.Executable == "" {
		config.Executable = "/bin/bash"
	}
	if !filepath.IsAbs(config.Executable) {
		return nil, errors.New("bash: executable must be an absolute path")
	}
	for key, value := range config.Environment {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("bash: invalid environment entry %q", key)
		}
	}
	config.Environment = cloneEnvironment(config.Environment)
	schema, err := tool.ParseInputSchema([]byte(`{"type":"object","properties":{"command":{"type":"string","minLength":1}},"required":["command"],"additionalProperties":false}`))
	if err != nil {
		return nil, err
	}
	return &Bash{config: config, definition: tool.Definition{
		Name: Key, Description: "Execute a Bash command in the configured workspace", InputSchema: schema,
	}}, nil
}

// NewModule returns an explicit Slot contribution. Standard applications do
// not install Bash merely because this package was imported.
func NewModule(config Config) (agentslot.Module, error) {
	installed, err := New(config)
	if err != nil {
		return nil, err
	}
	return module{tool: installed}, nil
}

type module struct{ tool *Bash }

func (module) ID() string { return moduleID }
func (m module) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Add(tool.ToolSlot, Key, tool.Tool(m.tool)))
}

func (b *Bash) Definition() tool.Definition       { return b.definition }
func (*Bash) ParallelSafety() tool.ParallelSafety { return tool.Serial }

func (b *Bash) Invoke(ctx context.Context, invocation tool.ToolInvocation) tool.ToolResult {
	if err := b.definition.InputSchema.ValidateArguments(invocation.Call.Arguments); err != nil {
		return failure(invocation, "invalid_arguments", "command must match the Bash input schema")
	}
	var input arguments
	if err := json.Unmarshal(invocation.Call.Arguments, &input); err != nil {
		return failure(invocation, "invalid_arguments", "command arguments are invalid")
	}
	commandContext, cancel := context.WithTimeout(ctx, b.config.Timeout)
	defer cancel()
	// Never load user or system profile scripts: they can reintroduce process
	// secrets and mutate output before the requested command runs.
	command := exec.CommandContext(commandContext, b.config.Executable, "--noprofile", "--norc", "-c", input.Command)
	configureProcessGroup(command)
	command.Dir = b.config.WorkingDirectory
	command.Env = environmentList(b.config.Environment)
	stdout := &limitedBuffer{limit: b.config.MaxStdoutBytes}
	stderr := &limitedBuffer{limit: b.config.MaxStderrBytes}
	command.Stdout, command.Stderr = stdout, stderr
	err := command.Run()
	exitCode := 0
	if err != nil {
		exitCode = -1
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			exitCode = exitError.ExitCode()
		}
	}
	output, marshalErr := json.Marshal(Output{
		Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exitCode,
		StdoutTruncated: stdout.truncated, StderrTruncated: stderr.truncated,
	})
	if marshalErr != nil {
		return failure(invocation, "encoding_failed", "Bash result could not be encoded")
	}
	if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
		return tool.ToolResult{CallID: invocation.Call.ID, Status: tool.ResultFailed, Output: output, Error: &tool.StructuredError{Code: "timeout", Message: "Bash command exceeded its configured timeout"}}
	}
	if errors.Is(commandContext.Err(), context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return tool.ToolResult{CallID: invocation.Call.ID, Status: tool.ResultFailed, Output: output, Error: &tool.StructuredError{Code: "canceled", Message: "Bash command was canceled"}}
	}
	if err != nil && exitCode == -1 {
		return tool.ToolResult{CallID: invocation.Call.ID, Status: tool.ResultFailed, Output: output, Error: &tool.StructuredError{Code: "execution_failed", Message: "Bash process could not be executed"}}
	}
	// A non-zero process exit is an observed command outcome, not a dispatcher
	// failure. The model receives the exit code and stderr and decides next.
	return tool.ToolResult{CallID: invocation.Call.ID, Status: tool.ResultSucceeded, Output: output}
}

func failure(invocation tool.ToolInvocation, code, message string) tool.ToolResult {
	return tool.ToolResult{CallID: invocation.Call.ID, Status: tool.ResultFailed, Error: &tool.StructuredError{Code: code, Message: message}}
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (w *limitedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := w.limit - w.buffer.Len()
	if remaining < len(value) {
		w.truncated = true
		if remaining < 0 {
			remaining = 0
		}
		value = value[:remaining]
	}
	_, _ = w.buffer.Write(value)
	return original, nil
}

func (w *limitedBuffer) String() string { return w.buffer.String() }

func environmentList(environment map[string]string) []string {
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		values = append(values, key+"="+environment[key])
	}
	return values
}

func cloneEnvironment(source map[string]string) map[string]string {
	copy := make(map[string]string, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}
