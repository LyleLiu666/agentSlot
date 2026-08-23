package scaffold

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestRenderPresetsAreDeterministicAndPinned(t *testing.T) {
	cases := []struct {
		name   string
		preset string
		add    []string
	}{
		{name: "local-coding", preset: "local-coding"},
		{name: "minimal-chat", preset: "minimal-chat"},
		{name: "customized-chat", preset: "minimal-chat", add: []string{"tool.files"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			options := testOptions(t, testCase.preset)
			options.AddImplementations = testCase.add
			first, err := Render(options)
			if err != nil {
				t.Fatal(err)
			}
			second, err := Render(options)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(first, second) {
				t.Fatal("same catalog preset and options produced different files")
			}
			goMod := fileData(t, first, "go.mod")
			if !bytes.Contains(goMod, []byte("github.com/LyleLiu666/agentSlot v0.0.8")) || bytes.Contains(goMod, []byte("replace ")) {
				t.Fatalf("generated go.mod is not reproducibly pinned:\n%s", goMod)
			}
			for _, implementation := range first.Implementations {
				if !implementation.Available {
					t.Fatalf("preset selected unavailable implementation %q", implementation.ID)
				}
			}
		})
	}
}

func TestRenderSeparatesCodingAndChatBoundaries(t *testing.T) {
	coding, err := Render(testOptions(t, "local-coding"))
	if err != nil {
		t.Fatal(err)
	}
	codingMain := string(fileData(t, coding, "main.go"))
	for _, expected := range []string{
		`"bash", "file_read", "file_write", "file_edit", "session_history"`,
		"policy.RequireApproval", "MaxInlineToolResultBytes", "artifact/file", "workspace/local",
	} {
		if !strings.Contains(codingMain, expected) {
			t.Errorf("coding preset is missing %q", expected)
		}
	}

	chat, err := Render(testOptions(t, "minimal-chat"))
	if err != nil {
		t.Fatal(err)
	}
	chatMain := string(fileData(t, chat, "main.go"))
	for _, forbidden := range []string{"tool/files", "tool/bash", "tool/sessionhistory", "artifact/file", "workspace/local", "MaxInlineToolResultBytes", "strconv"} {
		if strings.Contains(chatMain, forbidden) {
			t.Errorf("minimal chat unexpectedly contains %q", forbidden)
		}
	}
}

func TestRenderOptionalToolFiltersAreComplete(t *testing.T) {
	options := testOptions(t, "local-coding")
	options.WithoutFiles = true
	options.WithoutShell = true
	options.WithoutSessionHistory = true
	result, err := Render(options)
	if err != nil {
		t.Fatal(err)
	}
	mainSource := string(fileData(t, result, "main.go"))
	for _, forbidden := range []string{"tool/files", "tool/bash", "tool/sessionhistory", `"file_read"`, `"file_write"`, `"file_edit"`, `"bash"`, `"session_history"`} {
		if strings.Contains(mainSource, forbidden) {
			t.Errorf("disabled tool remains in generated source: %q", forbidden)
		}
	}
}

func TestRenderAddsImplementationDependenciesWithReviewableReasons(t *testing.T) {
	options := testOptions(t, "minimal-chat")
	options.AddImplementations = []string{"tool.files"}
	result, err := Render(options)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"tool.files", "workspace.local", "policy.tool-rules", "approval.configured"} {
		if !slices.Contains(result.Preset.Implementations, expected) {
			t.Errorf("custom selection omitted %q: %q", expected, result.Preset.Implementations)
		}
	}
	if len(result.Adjustments) != 3 {
		t.Fatalf("dependency adjustments = %q", result.Adjustments)
	}
	mainSource := string(fileData(t, result, "main.go"))
	if !strings.Contains(mainSource, "generatedGuardModule") || !strings.Contains(mainSource, `"file_read", "file_write", "file_edit"`) {
		t.Fatalf("custom implementation was not rendered safely:\n%s", mainSource)
	}
}

func TestRenderDoesNotReAddAnExplicitlyRemovedDependency(t *testing.T) {
	options := testOptions(t, "local-coding")
	options.RemoveImplementations = []string{"workspace.local"}
	if _, err := Render(options); err == nil || !strings.Contains(err.Error(), "workspace.manager") {
		t.Fatalf("removed required dependency error = %v", err)
	}
}

func TestGenerateRejectsUnsafeOrExistingTargetsWithoutPartialOutput(t *testing.T) {
	options := testOptions(t, "local-coding")
	options.SessionDirectory = filepath.Join(options.Workspace, "sessions")
	if _, err := Generate(options); err == nil {
		t.Fatal("expected storage inside Workspace to be rejected")
	}
	if _, err := os.Stat(options.TargetDirectory); !os.IsNotExist(err) {
		t.Fatalf("invalid generation changed target: %v", err)
	}

	options = testOptions(t, "minimal-chat")
	if err := os.MkdirAll(options.TargetDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(options.TargetDirectory, "owned")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(options); err == nil {
		t.Fatal("expected existing target to be rejected")
	}
	if value, err := os.ReadFile(marker); err != nil || string(value) != "keep" {
		t.Fatalf("existing target was modified: %q, %v", value, err)
	}
}

func TestGeneratedPresetsCompileAndBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test invokes Go toolchain")
	}
	repository := repositoryRoot(t)
	for _, preset := range []string{"local-coding", "minimal-chat"} {
		t.Run(preset, func(t *testing.T) {
			options := testOptions(t, preset)
			if err := os.MkdirAll(options.Workspace, 0o755); err != nil {
				t.Fatal(err)
			}
			if _, err := Generate(options); err != nil {
				t.Fatal(err)
			}
			canonicalRepository, err := filepath.EvalSymlinks(repository)
			if err != nil {
				t.Fatal(err)
			}
			canonicalTarget, err := filepath.EvalSymlinks(options.TargetDirectory)
			if err != nil {
				t.Fatal(err)
			}
			workDirectory := t.TempDir()
			work := filepath.Join(workDirectory, "go.work")
			command := exec.Command("go", "work", "init", canonicalRepository, canonicalTarget)
			command.Dir = workDirectory
			command.Env = append(os.Environ(), "GOWORK=off")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("go work init: %v\n%s", err, output)
			}
			command = exec.Command("go", "test", "-race", "./...")
			command.Dir = canonicalTarget
			command.Env = append(os.Environ(), "GOWORK="+work)
			if output, err := command.CombinedOutput(); err != nil {
				workspace, _ := os.ReadFile(work)
				t.Fatalf("generated project does not compile: %v\ntarget=%s\nwork=%s\n%s", err, options.TargetDirectory, workspace, output)
			}
			command = exec.Command("go", "vet", "./...")
			command.Dir = canonicalTarget
			command.Env = append(os.Environ(), "GOWORK="+work)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("generated project does not pass vet: %v\n%s", err, output)
			}
		})
	}
}

func testOptions(t *testing.T, preset string) Options {
	t.Helper()
	root := t.TempDir()
	return Options{
		TargetDirectory: filepath.Join(root, "generated"), ModulePath: "example.com/generated",
		AgentSlotVersion: "v0.0.8", PresetID: preset,
		ProviderKey: "compatible", ProviderURL: "https://example.invalid/v1", ModelID: "example-model",
		CredentialRef: "providers/compatible", CredentialFile: filepath.Join(root, "private", "credentials.enc"),
		CredentialKeyEnvironment: "AGENTSLOT_CREDENTIAL_KEY_HEX", ApprovalEnvironment: "AGENTSLOT_APPROVE_EFFECTS",
		Workspace: filepath.Join(root, "workspace"), SessionDirectory: filepath.Join(root, "state", "sessions"),
		ArtifactDirectory: filepath.Join(root, "state", "artifacts"),
	}
}

func fileData(t *testing.T, result Result, name string) []byte {
	t.Helper()
	for _, file := range result.Files {
		if file.Name == name {
			return file.Data
		}
	}
	t.Fatalf("generated file %q not found", name)
	return nil
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
}
