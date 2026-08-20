package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agent "github.com/LyleLiu666/agentSlot/agent"
)

func TestReferenceAgentRunsRealProviderBashSessionGatewayAndRuntimeChain(t *testing.T) {
	var requests atomic.Int32
	var providerFailure atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		attempt := requests.Add(1)
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			providerFailure.Store("decode provider request: " + err.Error())
			http.Error(response, "bad request", http.StatusBadRequest)
			return
		}
		response.Header().Set("Content-Type", "text/event-stream")
		if attempt == 1 {
			_, _ = response.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"provider-call-1\",\"function\":{\"name\":\"bash\",\"arguments\":\"{\\\"command\\\":\\\"printf full-chain\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"))
			_, _ = response.Write([]byte("data: [DONE]\n\n"))
			return
		}
		messages, _ := body["messages"].([]any)
		foundToolResult := false
		for _, candidate := range messages {
			message, _ := candidate.(map[string]any)
			if message["role"] == "tool" && strings.Contains(message["content"].(string), "full-chain") {
				foundToolResult = true
			}
		}
		if !foundToolResult {
			providerFailure.Store("second provider request did not contain Bash result")
		}
		_, _ = response.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"reference complete\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = response.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n"))
		_, _ = response.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	workspace := t.TempDir()
	sessionDirectory := filepath.Join(t.TempDir(), "sessions")
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	var output, errorOutput bytes.Buffer
	config := referenceConfig{
		providerKey: "test-provider", providerURL: server.URL, modelID: "test-model",
		workspace: workspace, sessionDir: sessionDirectory, httpHosts: []string{parsed.Host}, approveEffects: true,
		input: io.NopCloser(strings.NewReader("run the check\n/quit\n")), output: &output, errorOutput: &errorOutput, observationOut: io.Discard,
	}
	application, channel, err := buildReference(config)
	if err != nil {
		t.Fatal(err)
	}
	running, err := application.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waitReferenceCLI(t, channel.Done())
	stopContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := running.Stop(stopContext); err != nil {
		t.Fatal(err)
	}
	if channel.Err() != nil || errorOutput.Len() != 0 {
		t.Fatalf("CLI error=%v stderr=%q", channel.Err(), errorOutput.String())
	}
	if requests.Load() != 2 {
		t.Fatalf("provider requests = %d", requests.Load())
	}
	if failure := providerFailure.Load(); failure != nil {
		t.Fatal(failure)
	}
	if !strings.Contains(output.String(), "reference complete") {
		t.Fatalf("CLI output = %q", output.String())
	}
	fields := strings.Fields(strings.SplitN(output.String(), "\n", 2)[0])
	if len(fields) < 2 {
		t.Fatalf("session header = %q", output.String())
	}
	sessionID := fields[1]
	files, err := os.ReadDir(sessionDirectory)
	if err != nil || len(files) != 1 {
		t.Fatalf("persisted sessions = %d, %v", len(files), err)
	}

	var resumed bytes.Buffer
	config.sessionID = agentSessionID(sessionID)
	config.input = io.NopCloser(strings.NewReader("/quit\n"))
	config.output = &resumed
	config.errorOutput = io.Discard
	restarted, resumedCLI, err := buildReference(config)
	if err != nil {
		t.Fatal(err)
	}
	runningAgain, err := restarted.Start(context.Background())
	if err != nil {
		t.Fatalf("resume persisted Session: %v", err)
	}
	waitReferenceCLI(t, resumedCLI.Done())
	if err := runningAgain.Stop(stopContext); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resumed.String(), "session "+sessionID+" revision") {
		t.Fatalf("resume output = %q", resumed.String())
	}
}

func TestReferenceAgentRejectsSessionStorageInsideToolWorkspace(t *testing.T) {
	workspace := t.TempDir()
	_, _, err := buildReference(referenceConfig{
		providerKey: "provider", providerURL: "https://example.invalid", modelID: "model",
		workspace: workspace, sessionDir: filepath.Join(workspace, ".agentslot", "sessions"),
		httpHosts: []string{"example.invalid"}, input: io.NopCloser(strings.NewReader("")),
		output: io.Discard, errorOutput: io.Discard, observationOut: io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("overlapping storage error = %v", err)
	}
}

func waitReferenceCLI(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reference CLI did not finish")
	}
}

func agentSessionID(value string) agent.SessionID { return agent.SessionID(value) }
