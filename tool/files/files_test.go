package files_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/tool"
	"github.com/LyleLiu666/agentSlot/tool/files"
)

func TestFileToolsReadCreateAndCASWrite(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "existing.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	installed, stop := startTools(t, root, 1024)
	defer stop()

	read := invoke(installed[files.ReadKey], map[string]any{"path": "existing.txt"})
	if read.Status != tool.ResultSucceeded {
		t.Fatalf("read = %#v", read)
	}
	var readOutput files.ReadOutput
	decode(t, read.Output, &readOutput)
	if readOutput.Content != "before" || readOutput.SHA256 != digest("before") || readOutput.Bytes != 6 {
		t.Fatalf("read output = %#v", readOutput)
	}

	conflict := invoke(installed[files.WriteKey], map[string]any{"path": "existing.txt", "content": "after"})
	if conflict.Error == nil || conflict.Error.Code != "version_conflict" {
		t.Fatalf("unguarded overwrite = %#v", conflict)
	}
	updated := invoke(installed[files.WriteKey], map[string]any{
		"path": "existing.txt", "content": "after", "expected_sha256": readOutput.SHA256,
	})
	if updated.Status != tool.ResultSucceeded {
		t.Fatalf("CAS write = %#v", updated)
	}
	created := invoke(installed[files.WriteKey], map[string]any{"path": "nested/new.txt", "content": "new"})
	if created.Status != tool.ResultSucceeded {
		t.Fatalf("create = %#v", created)
	}
	if value, err := os.ReadFile(filepath.Join(root, "nested", "new.txt")); err != nil || string(value) != "new" {
		t.Fatalf("created file = %q, %v", value, err)
	}
}

func TestFileEditRequiresVersionAndOneExactOccurrence(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "note.txt")
	if err := os.WriteFile(path, []byte("one two one"), 0o600); err != nil {
		t.Fatal(err)
	}
	installed, stop := startTools(t, root, 1024)
	defer stop()

	ambiguous := invoke(installed[files.EditKey], map[string]any{
		"path": "note.txt", "old_text": "one", "new_text": "three", "expected_sha256": digest("one two one"),
	})
	if ambiguous.Error == nil || ambiguous.Error.Code != "ambiguous_edit" {
		t.Fatalf("ambiguous edit = %#v", ambiguous)
	}
	missingVersion := invoke(installed[files.EditKey], map[string]any{
		"path": "note.txt", "old_text": "two", "new_text": "three",
	})
	if missingVersion.Error == nil || missingVersion.Error.Code != "version_conflict" {
		t.Fatalf("unguarded edit = %#v", missingVersion)
	}
	edited := invoke(installed[files.EditKey], map[string]any{
		"path": "note.txt", "old_text": "two", "new_text": "three", "expected_sha256": digest("one two one"),
	})
	if edited.Status != tool.ResultSucceeded {
		t.Fatalf("edit = %#v", edited)
	}
	if value, err := os.ReadFile(path); err != nil || string(value) != "one three one" {
		t.Fatalf("edited file = %q, %v", value, err)
	}
}

func TestFileToolsConfinePathsAndRejectNonTextOrOversizedContent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "binary"), []byte{0xff, 0xfe}, 0o600); err != nil {
		t.Fatal(err)
	}
	installed, stop := startTools(t, root, 4)
	defer stop()

	for name, result := range map[string]tool.ToolResult{
		"parent traversal": invoke(installed[files.ReadKey], map[string]any{"path": "../secret.txt"}),
		"symlink escape":   invoke(installed[files.ReadKey], map[string]any{"path": "escape/secret.txt"}),
		"large read":       invoke(installed[files.ReadKey], map[string]any{"path": "large.txt"}),
		"binary read":      invoke(installed[files.ReadKey], map[string]any{"path": "binary"}),
		"large write":      invoke(installed[files.WriteKey], map[string]any{"path": "new.txt", "content": "12345"}),
	} {
		if result.Status != tool.ResultFailed || result.Error == nil {
			t.Fatalf("%s escaped boundary: %#v", name, result)
		}
	}
	if value, err := os.ReadFile(filepath.Join(outside, "secret.txt")); err != nil || string(value) != "secret" {
		t.Fatalf("outside file changed = %q, %v", value, err)
	}
}

func TestFileWriteCASIsAtomicAcrossConcurrentInvocations(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "shared.txt"), []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	installed, stop := startTools(t, root, 1024)
	defer stop()

	var wait sync.WaitGroup
	results := make(chan tool.ToolResult, 2)
	for _, content := range []string{"first", "second"} {
		wait.Add(1)
		go func(content string) {
			defer wait.Done()
			results <- invoke(installed[files.WriteKey], map[string]any{
				"path": "shared.txt", "content": content, "expected_sha256": digest("base"),
			})
		}(content)
	}
	wait.Wait()
	close(results)
	succeeded, conflicted := 0, 0
	for result := range results {
		if result.Status == tool.ResultSucceeded {
			succeeded++
		} else if result.Error != nil && result.Error.Code == "version_conflict" {
			conflicted++
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("succeeded=%d conflicted=%d", succeeded, conflicted)
	}
}

func TestFileModuleOwnsRootLifecycleAndContributesThreeTools(t *testing.T) {
	module, err := files.NewModule(files.Config{RootDirectory: t.TempDir(), MaxReadBytes: 64, MaxWriteBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	application := agentslot.NewApplication("files-lifecycle", []agentslot.Module{module},
		agentslot.RequireKey(tool.ToolSlot, files.ReadKey),
		agentslot.RequireKey(tool.ToolSlot, files.WriteKey),
		agentslot.RequireKey(tool.ToolSlot, files.EditKey),
	)
	assembly, err := application.Build()
	if err != nil {
		t.Fatal(err)
	}
	read, ok := agentslot.Lookup(assembly, tool.ToolSlot, files.ReadKey)
	if !ok {
		t.Fatal("read tool was not contributed")
	}
	beforeStart := invoke(read, map[string]any{"path": "missing"})
	if beforeStart.Error == nil || beforeStart.Error.Code != "not_started" {
		t.Fatalf("before start = %#v", beforeStart)
	}
	runtime, err := application.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	afterStop := invoke(read, map[string]any{"path": "missing"})
	if afterStop.Error == nil || afterStop.Error.Code != "not_started" {
		t.Fatalf("after stop = %#v", afterStop)
	}
}

func startTools(t *testing.T, root string, limit int) (map[string]tool.Tool, func()) {
	t.Helper()
	module, err := files.NewModule(files.Config{RootDirectory: root, MaxReadBytes: limit, MaxWriteBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	application := agentslot.NewApplication("files-test", []agentslot.Module{module})
	assembly, err := application.Build()
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := application.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	installed := make(map[string]tool.Tool, 3)
	for _, key := range []string{files.ReadKey, files.WriteKey, files.EditKey} {
		value, ok := agentslot.Lookup(assembly, tool.ToolSlot, key)
		if !ok {
			t.Fatalf("missing tool %q", key)
		}
		installed[key] = value
	}
	return installed, func() {
		if err := runtime.Stop(context.Background()); err != nil {
			t.Errorf("stop file tools: %v", err)
		}
	}
}

func invoke(installed tool.Tool, arguments map[string]any) tool.ToolResult {
	encoded, _ := json.Marshal(arguments)
	return installed.Invoke(context.Background(), tool.ToolInvocation{
		Call:      tool.Call{ID: agent.ToolCallID("call-1"), Name: installed.Definition().Name, Arguments: encoded},
		SessionID: "session-1", RunID: "run-1", StepID: "step-1",
	})
}

func decode(t *testing.T, data []byte, destination any) {
	t.Helper()
	if err := json.Unmarshal(data, destination); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
