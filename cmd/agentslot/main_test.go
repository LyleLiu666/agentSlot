package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitGeneratesCatalogPresetWithoutSecrets(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "agent")
	var stdout, stderr bytes.Buffer
	err := run([]string{
		"init", "--preset", "minimal-chat", "--module", "example.com/my-agent",
		"--agentslot-version", "v0.0.8", "--provider-url", "https://example.invalid/v1",
		"--credential-file", filepath.Join(root, "private", "credentials.enc"),
		"--session-dir", filepath.Join(root, "state", "sessions"), target,
	}, strings.NewReader(""), &stdout, &stderr, runtimeEnvironment{workingDirectory: root, configDirectory: filepath.Join(root, "config")})
	if err != nil {
		t.Fatalf("run init: %v\nstderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "minimal-chat") || !strings.Contains(stdout.String(), target) {
		t.Fatalf("unexpected success output: %q", stdout.String())
	}
	mainSource, err := os.ReadFile(filepath.Join(target, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mainSource), "APIKey") || strings.Contains(string(mainSource), "secret-value") {
		t.Fatal("generated source contains credential material")
	}
}

func TestInitDefaultsToLocalCodingAndReleasedBuildVersion(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "agent")
	var stdout, stderr bytes.Buffer
	err := run([]string{"init", "--workspace", workspace, target}, strings.NewReader(""), &stdout, &stderr, runtimeEnvironment{
		workingDirectory: workspace, configDirectory: filepath.Join(root, "config"), buildVersion: "v0.0.9",
	})
	if err != nil {
		t.Fatalf("run init: %v\nstderr=%s", err, stderr.String())
	}
	goMod, err := os.ReadFile(filepath.Join(target, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(goMod), "github.com/LyleLiu666/agentSlot v0.0.9") {
		t.Fatalf("build version was not pinned:\n%s", goMod)
	}
	mainSource, err := os.ReadFile(filepath.Join(target, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mainSource), "tool/bash") {
		t.Fatal("default preset is not local-coding")
	}
}

func TestInitDevelopmentBuildRequiresExplicitVersion(t *testing.T) {
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	err := run([]string{"init", "--preset", "minimal-chat", filepath.Join(root, "agent")}, strings.NewReader(""), &stdout, &stderr, runtimeEnvironment{
		workingDirectory: root, configDirectory: filepath.Join(root, "config"), buildVersion: "(devel)",
	})
	if err == nil || !strings.Contains(err.Error(), "--agentslot-version") {
		t.Fatalf("development version error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "agent")); !os.IsNotExist(statErr) {
		t.Fatalf("failed command changed target: %v", statErr)
	}
}

func TestInitRejectsUnexpectedArguments(t *testing.T) {
	root := t.TempDir()
	for _, arguments := range [][]string{{}, {"unknown"}, {"init"}, {"init", "a", "b"}} {
		var stdout, stderr bytes.Buffer
		if err := run(arguments, strings.NewReader(""), &stdout, &stderr, runtimeEnvironment{workingDirectory: root, configDirectory: filepath.Join(root, "config"), buildVersion: "v0.0.9"}); err == nil {
			t.Fatalf("arguments %#v unexpectedly succeeded", arguments)
		}
	}
}

func TestInteractiveInitCollectsOnlyNonSecretChoices(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "agent")
	answers := strings.Join([]string{
		"2",
		"",
		"private-provider",
		"https://model.invalid/v1",
		"private-model",
		"credentials/model",
		"",
		"",
	}, "\n") + "\n"
	var stdout, stderr bytes.Buffer
	err := run([]string{"init", "--agentslot-version", "v0.0.8", target}, strings.NewReader(answers), &stdout, &stderr, runtimeEnvironment{
		workingDirectory: root, configDirectory: filepath.Join(root, "config"), interactive: true,
	})
	if err != nil {
		t.Fatalf("interactive init: %v\nstderr=%s", err, stderr.String())
	}
	mainSource, err := os.ReadFile(filepath.Join(target, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"private-provider", "private-model", "credentials/model"} {
		if !strings.Contains(string(mainSource), expected) {
			t.Errorf("interactive value %q was not generated", expected)
		}
	}
	if strings.Contains(stdout.String(), "API key") {
		t.Fatal("wizard asked for credential material")
	}
}
