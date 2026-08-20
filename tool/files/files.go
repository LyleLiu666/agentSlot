// Package files provides explicitly installed, workspace-confined file tools.
package files

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/tool"
)

const (
	ReadKey  = "file_read"
	WriteKey = "file_write"
	EditKey  = "file_edit"
	moduleID = "tool.builtin.files"
)

// Config fixes the filesystem boundary and payload limits for all three tools.
type Config struct {
	RootDirectory string
	MaxReadBytes  int
	MaxWriteBytes int
}

// ReadOutput is the versioned text returned by file_read.
type ReadOutput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	SHA256  string `json:"sha256"`
	Bytes   int    `json:"bytes"`
}

// MutationOutput identifies the durable version produced by file_write or
// file_edit.
type MutationOutput struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Bytes   int    `json:"bytes"`
	Created bool   `json:"created,omitempty"`
}

// NewModule contributes file_read, file_write, and file_edit as one
// lifecycle-owned capability. Importing this package never installs them.
func NewModule(config Config) (agentslot.Module, error) {
	if config.RootDirectory == "" || !filepath.IsAbs(config.RootDirectory) {
		return nil, errors.New("files: root directory must be an absolute path")
	}
	if config.MaxReadBytes <= 0 || config.MaxWriteBytes <= 0 {
		return nil, errors.New("files: read and write limits must be positive")
	}
	filesystem := &fileSystem{config: config}
	read, err := newFileTool(ReadKey, "Read one UTF-8 text file within the configured workspace", filesystem)
	if err != nil {
		return nil, err
	}
	write, err := newFileTool(WriteKey, "Create or version-safely replace one UTF-8 text file", filesystem)
	if err != nil {
		return nil, err
	}
	edit, err := newFileTool(EditKey, "Replace one exact text occurrence in a versioned file", filesystem)
	if err != nil {
		return nil, err
	}
	return &module{filesystem: filesystem, read: read, write: write, edit: edit}, nil
}

type module struct {
	filesystem *fileSystem
	read       *fileTool
	write      *fileTool
	edit       *fileTool
}

func (*module) ID() string { return moduleID }

func (m *module) Register(reg agentslot.Registrar) error {
	return reg.Contribute(
		agentslot.Add(tool.ToolSlot, ReadKey, tool.Tool(m.read)),
		agentslot.Add(tool.ToolSlot, WriteKey, tool.Tool(m.write)),
		agentslot.Add(tool.ToolSlot, EditKey, tool.Tool(m.edit)),
	)
}

func (m *module) Start(context.Context) error { return m.filesystem.open() }
func (m *module) Stop(context.Context) error  { return m.filesystem.close() }

type fileSystem struct {
	mu     sync.RWMutex
	config Config
	root   *os.Root
}

func (f *fileSystem) open() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.root != nil {
		return nil
	}
	root, err := os.OpenRoot(f.config.RootDirectory)
	if err != nil {
		return fmt.Errorf("files: open root: %w", err)
	}
	f.root = root
	return nil
}

func (f *fileSystem) close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.root == nil {
		return nil
	}
	err := f.root.Close()
	f.root = nil
	return err
}

type fileTool struct {
	key        string
	definition tool.Definition
	filesystem *fileSystem
}

var _ tool.Tool = (*fileTool)(nil)

func newFileTool(key, description string, filesystem *fileSystem) (*fileTool, error) {
	var schema string
	switch key {
	case ReadKey:
		schema = `{"type":"object","properties":{"path":{"type":"string","minLength":1}},"required":["path"],"additionalProperties":false}`
	case WriteKey:
		schema = `{"type":"object","properties":{"path":{"type":"string","minLength":1},"content":{"type":"string"},"expected_sha256":{"type":"string"}},"required":["path","content"],"additionalProperties":false}`
	case EditKey:
		schema = `{"type":"object","properties":{"path":{"type":"string","minLength":1},"old_text":{"type":"string","minLength":1},"new_text":{"type":"string"},"expected_sha256":{"type":"string"}},"required":["path","old_text","new_text"],"additionalProperties":false}`
	default:
		return nil, fmt.Errorf("files: unknown tool key %q", key)
	}
	parsed, err := tool.ParseInputSchema([]byte(schema))
	if err != nil {
		return nil, err
	}
	return &fileTool{key: key, definition: tool.Definition{Name: key, Description: description, InputSchema: parsed}, filesystem: filesystem}, nil
}

func (t *fileTool) Definition() tool.Definition { return t.definition }

func (t *fileTool) ParallelSafety() tool.ParallelSafety {
	if t.key == ReadKey {
		return tool.ParallelSafe
	}
	return tool.Serial
}

func (t *fileTool) Invoke(ctx context.Context, invocation tool.ToolInvocation) tool.ToolResult {
	if err := t.definition.InputSchema.ValidateArguments(invocation.Call.Arguments); err != nil {
		return failure(invocation, "invalid_arguments", "file arguments do not match the declared schema")
	}
	switch t.key {
	case ReadKey:
		var input readArguments
		if json.Unmarshal(invocation.Call.Arguments, &input) != nil {
			return failure(invocation, "invalid_arguments", "file arguments are invalid")
		}
		return t.filesystem.read(ctx, invocation, input)
	case WriteKey:
		var input writeArguments
		if json.Unmarshal(invocation.Call.Arguments, &input) != nil {
			return failure(invocation, "invalid_arguments", "file arguments are invalid")
		}
		return t.filesystem.write(ctx, invocation, input)
	case EditKey:
		var input editArguments
		if json.Unmarshal(invocation.Call.Arguments, &input) != nil {
			return failure(invocation, "invalid_arguments", "file arguments are invalid")
		}
		return t.filesystem.edit(ctx, invocation, input)
	default:
		return failure(invocation, "internal_error", "file tool is not configured")
	}
}

type readArguments struct {
	Path string `json:"path"`
}

type writeArguments struct {
	Path           string `json:"path"`
	Content        string `json:"content"`
	ExpectedSHA256 string `json:"expected_sha256"`
}

type editArguments struct {
	Path           string `json:"path"`
	OldText        string `json:"old_text"`
	NewText        string `json:"new_text"`
	ExpectedSHA256 string `json:"expected_sha256"`
}

func (f *fileSystem) read(ctx context.Context, invocation tool.ToolInvocation, input readArguments) tool.ToolResult {
	if !validPath(input.Path) {
		return failure(invocation, "invalid_path", "path must be a local normalized workspace path")
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.root == nil {
		return failure(invocation, "not_started", "file tools are not started")
	}
	content, code := readText(ctx, f.root, input.Path, f.config.MaxReadBytes)
	if code != "" {
		return failure(invocation, code, readFailureMessage(code))
	}
	output, err := json.Marshal(ReadOutput{Path: input.Path, Content: string(content), SHA256: digest(content), Bytes: len(content)})
	if err != nil {
		return failure(invocation, "encoding_failed", "file result could not be encoded")
	}
	return tool.ToolResult{CallID: invocation.Call.ID, Status: tool.ResultSucceeded, Output: output}
}

func (f *fileSystem) write(ctx context.Context, invocation tool.ToolInvocation, input writeArguments) tool.ToolResult {
	if !validPath(input.Path) {
		return failure(invocation, "invalid_path", "path must be a local normalized workspace path")
	}
	content := []byte(input.Content)
	if !utf8.Valid(content) {
		return failure(invocation, "unsupported_content", "file content must be UTF-8 text")
	}
	if len(content) > f.config.MaxWriteBytes {
		return failure(invocation, "content_too_large", "file content exceeds the configured limit")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.root == nil {
		return failure(invocation, "not_started", "file tools are not started")
	}
	created, code := checkVersion(ctx, f.root, input.Path, input.ExpectedSHA256, f.config.MaxWriteBytes)
	if code != "" {
		return failure(invocation, code, mutationFailureMessage(code))
	}
	if code := atomicWrite(ctx, f.root, input.Path, content); code != "" {
		return failure(invocation, code, mutationFailureMessage(code))
	}
	return mutationSuccess(invocation, input.Path, content, created)
}

func (f *fileSystem) edit(ctx context.Context, invocation tool.ToolInvocation, input editArguments) tool.ToolResult {
	if !validPath(input.Path) {
		return failure(invocation, "invalid_path", "path must be a local normalized workspace path")
	}
	if input.ExpectedSHA256 == "" {
		return failure(invocation, "version_conflict", "file version is missing or no longer current")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.root == nil {
		return failure(invocation, "not_started", "file tools are not started")
	}
	content, code := readText(ctx, f.root, input.Path, f.config.MaxWriteBytes)
	if code != "" {
		return failure(invocation, code, mutationFailureMessage(code))
	}
	if digest(content) != input.ExpectedSHA256 {
		return failure(invocation, "version_conflict", "file version is missing or no longer current")
	}
	occurrences := strings.Count(string(content), input.OldText)
	if occurrences == 0 {
		return failure(invocation, "text_not_found", "old_text was not found")
	}
	if occurrences != 1 {
		return failure(invocation, "ambiguous_edit", "old_text must occur exactly once")
	}
	replaced := []byte(strings.Replace(string(content), input.OldText, input.NewText, 1))
	if len(replaced) > f.config.MaxWriteBytes {
		return failure(invocation, "content_too_large", "edited content exceeds the configured limit")
	}
	if code := atomicWrite(ctx, f.root, input.Path, replaced); code != "" {
		return failure(invocation, code, mutationFailureMessage(code))
	}
	return mutationSuccess(invocation, input.Path, replaced, false)
}

func checkVersion(ctx context.Context, root *os.Root, name, expected string, limit int) (created bool, code string) {
	if err := ctx.Err(); err != nil {
		return false, "canceled"
	}
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		if expected != "" {
			return false, "version_conflict"
		}
		return true, ""
	}
	if err != nil {
		return false, "access_failed"
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, "unsupported_file"
	}
	if expected == "" {
		return false, "version_conflict"
	}
	content, code := readText(ctx, root, name, limit)
	if code != "" {
		return false, code
	}
	if digest(content) != expected {
		return false, "version_conflict"
	}
	return false, ""
}

func readText(ctx context.Context, root *os.Root, name string, limit int) ([]byte, string) {
	if err := ctx.Err(); err != nil {
		return nil, "canceled"
	}
	file, err := root.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "not_found"
	}
	if err != nil {
		return nil, "access_failed"
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, "access_failed"
	}
	if !info.Mode().IsRegular() {
		return nil, "unsupported_file"
	}
	if info.Size() > int64(limit) {
		return nil, "content_too_large"
	}
	content, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, "access_failed"
	}
	if len(content) > limit {
		return nil, "content_too_large"
	}
	if err := ctx.Err(); err != nil {
		return nil, "canceled"
	}
	if !utf8.Valid(content) {
		return nil, "unsupported_content"
	}
	return content, ""
}

func atomicWrite(ctx context.Context, root *os.Root, name string, content []byte) string {
	if err := ctx.Err(); err != nil {
		return "canceled"
	}
	directory := filepath.Dir(name)
	if directory != "." {
		if err := root.MkdirAll(directory, 0o700); err != nil {
			return "write_failed"
		}
	}
	temporary, file, err := createTemporary(root, directory)
	if err != nil {
		return "write_failed"
	}
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = root.Remove(temporary)
		}
	}()
	if _, err := file.Write(content); err != nil {
		return "write_failed"
	}
	if err := file.Sync(); err != nil {
		return "write_failed"
	}
	if err := file.Close(); err != nil {
		return "write_failed"
	}
	if err := ctx.Err(); err != nil {
		return "canceled"
	}
	if err := root.Rename(temporary, name); err != nil {
		return "write_failed"
	}
	keep = true
	if parent, err := root.Open(directory); err == nil {
		_ = parent.Sync()
		_ = parent.Close()
	}
	return ""
}

func createTemporary(root *os.Root, directory string) (string, *os.File, error) {
	for attempts := 0; attempts < 10; attempts++ {
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, err
		}
		name := ".agentslot-" + hex.EncodeToString(random[:]) + ".tmp"
		if directory != "." {
			name = filepath.Join(directory, name)
		}
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return name, file, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", nil, err
		}
	}
	return "", nil, errors.New("files: could not allocate temporary file")
}

func validPath(name string) bool {
	return filepath.IsLocal(name) && filepath.Clean(name) == name && name != "." && !strings.ContainsRune(name, '\x00')
}

func digest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func mutationSuccess(invocation tool.ToolInvocation, path string, content []byte, created bool) tool.ToolResult {
	output, err := json.Marshal(MutationOutput{Path: path, SHA256: digest(content), Bytes: len(content), Created: created})
	if err != nil {
		return failure(invocation, "encoding_failed", "file result could not be encoded")
	}
	return tool.ToolResult{CallID: invocation.Call.ID, Status: tool.ResultSucceeded, Output: output}
}

func failure(invocation tool.ToolInvocation, code, message string) tool.ToolResult {
	return tool.ToolResult{CallID: invocation.Call.ID, Status: tool.ResultFailed, Error: &tool.StructuredError{Code: code, Message: message}}
}

func readFailureMessage(code string) string {
	switch code {
	case "not_found":
		return "file was not found"
	case "content_too_large":
		return "file exceeds the configured read limit"
	case "unsupported_content":
		return "file is not UTF-8 text"
	case "unsupported_file":
		return "path is not a regular file"
	case "canceled":
		return "file operation was canceled"
	default:
		return "file could not be read within the configured workspace"
	}
}

func mutationFailureMessage(code string) string {
	switch code {
	case "not_found":
		return "file was not found"
	case "version_conflict":
		return "file version is missing or no longer current"
	case "content_too_large":
		return "file content exceeds the configured limit"
	case "unsupported_content":
		return "file is not UTF-8 text"
	case "unsupported_file":
		return "path is not a regular file"
	case "canceled":
		return "file operation was canceled"
	default:
		return "file could not be changed within the configured workspace"
	}
}
