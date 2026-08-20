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
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/interaction/inprocess"
	"github.com/LyleLiu666/agentSlot/session"
	"github.com/LyleLiu666/agentSlot/standardagent"
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
		contextRetentionMode: standardagent.ContextLatestOnly, maxTokensPerRun: 0,
		actor: agent.ActorIdentity{Kind: agent.ActorLocalUser, ID: "reference-test-user"},
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
	stopContext, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	stopErr := running.Stop(stopContext)
	stopCancel()
	if stopErr != nil {
		t.Fatal(stopErr)
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
	sessionID := agentSessionID(fields[1])
	files, err := os.ReadDir(sessionDirectory)
	if err != nil || len(files) != 1 {
		t.Fatalf("persisted sessions = %d, %v", len(files), err)
	}
	assertReferenceSessionFacts(t, sessionDirectory, sessionID)

	var resumed bytes.Buffer
	config.sessionID = sessionID
	config.input = io.NopCloser(strings.NewReader("/quit\n"))
	config.output = &resumed
	config.errorOutput = io.Discard
	probe := inprocess.New()
	restarted, resumedCLI, err := buildReferenceWithChannels(config,
		standardagent.NewGatewayChannelModule("reference.channel.inprocess-test", "in-process test", probe),
	)
	if err != nil {
		t.Fatal(err)
	}
	runningAgain, err := restarted.Start(context.Background())
	if err != nil {
		t.Fatalf("resume persisted Session: %v", err)
	}
	access, err := probe.Access()
	if err != nil {
		t.Fatal(err)
	}
	assertReferenceGatewayPagination(t, access, sessionID)
	waitReferenceCLI(t, resumedCLI.Done())
	resumeStopContext, resumeStopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	resumeStopErr := runningAgain.Stop(resumeStopContext)
	resumeStopCancel()
	if resumeStopErr != nil {
		t.Fatal(resumeStopErr)
	}
	if !strings.Contains(resumed.String(), "session "+string(sessionID)+" revision") {
		t.Fatalf("resume output = %q", resumed.String())
	}
}

func TestReferenceAgentRejectsSessionStorageInsideToolWorkspace(t *testing.T) {
	workspace := t.TempDir()
	_, _, err := buildReference(referenceConfig{
		providerKey: "provider", providerURL: "https://example.invalid", modelID: "model",
		workspace: workspace, sessionDir: filepath.Join(workspace, ".agentslot", "sessions"),
		contextRetentionMode: standardagent.ContextLatestOnly,
		actor:                agent.ActorIdentity{Kind: agent.ActorLocalUser, ID: "reference-test-user"},
		httpHosts:            []string{"example.invalid"}, input: io.NopCloser(strings.NewReader("")),
		output: io.Discard, errorOutput: io.Discard, observationOut: io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("overlapping storage error = %v", err)
	}
}

func TestReferenceAgentRequiresExplicitRuntimeBoundaryConfiguration(t *testing.T) {
	base := referenceConfig{
		providerKey: "provider", providerURL: "https://example.invalid", modelID: "model",
		workspace: t.TempDir(), sessionDir: filepath.Join(t.TempDir(), "sessions"),
		contextRetentionMode: standardagent.ContextLatestOnly,
		actor:                agent.ActorIdentity{Kind: agent.ActorLocalUser, ID: "reference-test-user"},
		httpHosts:            []string{"example.invalid"}, input: io.NopCloser(strings.NewReader("")),
		output: io.Discard, errorOutput: io.Discard, observationOut: io.Discard,
	}
	tests := []struct {
		name   string
		mutate func(*referenceConfig)
	}{
		{name: "context retention", mutate: func(config *referenceConfig) { config.contextRetentionMode = "" }},
		{name: "token budget", mutate: func(config *referenceConfig) { config.maxTokensPerRun = -1 }},
		{name: "actor identity", mutate: func(config *referenceConfig) { config.actor = agent.ActorIdentity{} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := base
			test.mutate(&config)
			if _, _, err := buildReference(config); err == nil {
				t.Fatal("invalid reference Runtime boundary configuration was accepted")
			}
		})
	}
}

func assertReferenceSessionFacts(t *testing.T, directory string, sessionID agent.SessionID) {
	t.Helper()
	store, err := session.NewFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(context.Background()); err != nil {
			t.Errorf("close inspected FileStore: %v", err)
		}
	}()
	snapshot, err := store.Load(context.Background(), session.SessionRef{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ModelConfig.ProviderKey != "test-provider" || snapshot.ModelConfig.ModelID != "test-model" {
		t.Fatalf("persisted model config = %#v", snapshot.ModelConfig)
	}
	request := snapshot.Context.Request
	if snapshot.Context.Version != 2 || len(snapshot.RetainedContexts) != 0 || request.Config != snapshot.ModelConfig || len(request.Tools) != 5 ||
		len(request.Inputs) == 0 || request.Inputs[0].SystemPrompt == nil {
		t.Fatalf("latest complete Context = %#v", snapshot.Context)
	}
	attempts := make(map[agent.AttemptID][]session.ModelAttemptKind)
	var calls, results int
	var userActor agent.ActorIdentity
	for _, fact := range snapshot.History {
		switch {
		case fact.ModelAttempt != nil:
			attempts[fact.ModelAttempt.AttemptID] = append(attempts[fact.ModelAttempt.AttemptID], fact.ModelAttempt.Kind)
		case fact.ToolCall != nil:
			calls++
		case fact.ToolResult != nil:
			results++
		case fact.Message != nil && fact.Message.Role == agent.RoleUser:
			userActor = fact.Actor
		}
	}
	if len(attempts) != 2 || calls != 1 || results != 1 {
		t.Fatalf("History attempt/tool facts = attempts %#v, calls %d, results %d", attempts, calls, results)
	}
	for id, kinds := range attempts {
		if len(kinds) != 2 || kinds[0] != session.AttemptStarted || kinds[1] != session.AttemptSucceeded {
			t.Fatalf("Attempt %s facts = %#v", id, kinds)
		}
	}
	if userActor != (agent.ActorIdentity{Kind: agent.ActorLocalUser, ID: "reference-test-user"}) {
		t.Fatalf("durable user ActorIdentity = %#v", userActor)
	}
}

func assertReferenceGatewayPagination(t *testing.T, access interaction.GatewayAccess, sessionID agent.SessionID) {
	t.Helper()
	view, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if view.SessionID != sessionID || view.Revision == 0 || view.RunState != session.RunIdle || len(view.RecentHistory) == 0 {
		t.Fatalf("resumed SessionView = %#v", view)
	}
	latest, err := access.History(context.Background(), interaction.HistoryRequest{SessionID: sessionID, StepLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(latest.Facts) == 0 || !latest.HasMore {
		t.Fatalf("latest History page = %#v", latest)
	}
	older, err := access.History(context.Background(), interaction.HistoryRequest{
		SessionID: sessionID, BeforeHistorySequence: latest.Facts[0].Sequence, StepLimit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(older.Facts) == 0 || older.Facts[len(older.Facts)-1].Sequence >= latest.Facts[0].Sequence {
		t.Fatalf("older History page = %#v after %#v", older, latest)
	}
	if latest.Revision != view.Revision || older.Revision != view.Revision {
		t.Fatalf("History page revisions = %d, %d; View revision = %d", latest.Revision, older.Revision, view.Revision)
	}
	seen := make(map[session.HistorySequence]bool, len(latest.Facts))
	for _, fact := range latest.Facts {
		seen[fact.Sequence] = true
	}
	for _, fact := range older.Facts {
		if seen[fact.Sequence] {
			t.Fatalf("History sequence %d appeared in both cursor pages", fact.Sequence)
		}
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
