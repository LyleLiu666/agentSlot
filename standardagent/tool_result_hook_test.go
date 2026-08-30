package standardagent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
	"github.com/LyleLiu666/agentSlot/tool"
)

func TestToolResultsAndPostReservationsCommitAtomicallyAndContextReachesOnlyTheNextStep(t *testing.T) {
	store := newPostRecordingStore()
	succeeded := newRecordingToolResultHook("success", hook.ToolResultScope{All: true, Statuses: []tool.ResultStatus{tool.ResultSucceeded}}, "success context")
	failed := newRecordingToolResultHook("failure", hook.ToolResultScope{All: true, Statuses: []tool.ResultStatus{tool.ResultFailed}}, "failure context")
	executor := preflightBatchExecutor(
		model.ToolCallRequest{Name: "good", Arguments: []byte(`{"value":"one"}`)},
		model.ToolCallRequest{Name: "bad", Arguments: []byte(`{"value":"two"}`)},
	)
	access, stop := startToolPreflightApplication(t, store, executor,
		AgentRuntimeConfig{ToolKeys: []string{"good", "bad"}, MaxInlineToolResultBytes: 1024},
		toolModule{key: "good", value: &fixedResultTool{definition: testToolDefinition(t, "good"), status: tool.ResultSucceeded}},
		toolModule{key: "bad", value: &fixedResultTool{definition: testToolDefinition(t, "bad"), status: tool.ResultFailed}},
		toolResultHookModule{hooks: []hook.ToolResultHook{succeeded, failed}},
	)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	result, err := access.SendAndWait(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("post")})
	if err != nil || result.Outcome != session.RunCompleted {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if succeeded.callCount() != 1 || failed.callCount() != 1 {
		t.Fatalf("post calls succeeded/failed = %d/%d", succeeded.callCount(), failed.callCount())
	}
	commits := store.resultCommits()
	if len(commits) != 1 || commits[0].results != 2 || commits[0].terminalJournals != 2 || commits[0].postPrepared != 2 {
		t.Fatalf("result commit shapes = %#v", commits)
	}
	views := append(succeeded.Views(), failed.Views()...)
	for _, view := range views {
		if view.Revision != commits[0].revision || !view.NextStepID.Valid() || view.StepID == view.NextStepID || view.Result.CallID != view.ToolCallID {
			t.Fatalf("post view = %#v commit=%#v", view, commits[0])
		}
	}
	requests := executor.Requests()
	if len(requests) != 2 || countInputText(requests[1].Inputs, "success context") != 1 || countInputText(requests[1].Inputs, "failure context") != 1 {
		t.Fatalf("next request = %#v", requests)
	}
	if inputTextIndex(requests[1].Inputs, "success context") >= inputTextIndex(requests[1].Inputs, "failure context") {
		t.Fatalf("Post context lost ToolCall/profile order: %#v", requests[1].Inputs)
	}
	snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ExtensionJournal) != 2 {
		t.Fatalf("post journal = %#v", snapshot.ExtensionJournal)
	}
	for _, entry := range snapshot.ExtensionJournal {
		if entry.Boundary != hook.BoundaryToolResult || entry.Status != hook.InvocationSucceeded ||
			entry.EffectDisposition != hook.EffectApplied || entry.ContextDisposition != hook.ContextConsumed || len(entry.ContextInputs) != 0 {
			t.Fatalf("post entry = %#v", entry)
		}
	}
}

func TestPostHookRunsOnlyForInvokedCertainToolResults(t *testing.T) {
	post := newRecordingToolResultHook("eligible", hook.ToolResultScope{All: true, Statuses: []tool.ResultStatus{tool.ResultSucceeded, tool.ResultFailed}}, "")
	executor := preflightBatchExecutor(
		model.ToolCallRequest{Name: "good", Arguments: []byte(`{"value":"one"}`)},
		model.ToolCallRequest{Name: "invalid", Arguments: []byte(`{"wrong":true}`)},
	)
	access, store, stop := startRound7Application(t, executor,
		AgentRuntimeConfig{ToolKeys: []string{"good", "invalid"}, MaxInlineToolResultBytes: 1024},
		toolModule{key: "good", value: &fixedResultTool{definition: testToolDefinition(t, "good"), status: tool.ResultSucceeded}},
		toolModule{key: "invalid", value: &fixedResultTool{definition: testToolDefinition(t, "invalid"), status: tool.ResultSucceeded}},
		toolResultHookModule{hooks: []hook.ToolResultHook{post}},
	)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	result, err := access.SendAndWait(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("eligible")})
	if err != nil || result.Outcome != session.RunCompleted {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	views := post.Views()
	if len(views) != 1 || views[0].ToolKey != "good" || views[0].Result.Status != tool.ResultSucceeded {
		t.Fatalf("post views = %#v", views)
	}
	snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ExtensionJournal) != 1 {
		t.Fatalf("post reservations included ineligible results: %#v", snapshot.ExtensionJournal)
	}
}

func TestPostHookFailurePreservesToolResultsDiscardsContextAndInterruptsBeforeNextModel(t *testing.T) {
	first := newRecordingToolResultHook("first", hook.ToolResultScope{All: true, Statuses: []tool.ResultStatus{tool.ResultSucceeded}}, "must be discarded")
	failing := newRecordingToolResultHook("failing", hook.ToolResultScope{All: true, Statuses: []tool.ResultStatus{tool.ResultSucceeded}}, "")
	failing.err = &hook.InvocationFailure{Status: hook.InvocationFailed, Code: "post_command_failed", Reason: "post command failed"}
	unstarted := newRecordingToolResultHook("unstarted", hook.ToolResultScope{All: true, Statuses: []tool.ResultStatus{tool.ResultSucceeded}}, "")
	executor := preflightBatchExecutor(model.ToolCallRequest{Name: "good", Arguments: []byte(`{"value":"one"}`)})
	access, store, stop := startRound7Application(t, executor,
		AgentRuntimeConfig{ToolKeys: []string{"good"}, MaxInlineToolResultBytes: 1024},
		toolModule{key: "good", value: &fixedResultTool{definition: testToolDefinition(t, "good"), status: tool.ResultSucceeded}},
		toolResultHookModule{hooks: []hook.ToolResultHook{first, failing, unstarted}},
	)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	result, err := access.SendAndWait(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("fail post")})
	if err != nil || result.Outcome != session.RunInterrupted {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if first.callCount() != 1 || failing.callCount() != 1 || unstarted.callCount() != 0 || len(executor.Requests()) != 1 {
		t.Fatalf("post/model calls = %d/%d/%d requests=%d", first.callCount(), failing.callCount(), unstarted.callCount(), len(executor.Requests()))
	}
	snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if !hasToolResultStatus(snapshot.History, tool.ResultSucceeded) || !lastRunHasTermination(snapshot.History, session.TerminationExtension, "post_command_failed") {
		t.Fatalf("tool result/termination = %#v", snapshot.History)
	}
	if len(snapshot.ExtensionJournal) != 3 || snapshot.ExtensionJournal[0].ContextDisposition != hook.ContextDiscarded ||
		snapshot.ExtensionJournal[1].Status != hook.InvocationFailed || snapshot.ExtensionJournal[1].EffectDisposition != hook.EffectApplied ||
		snapshot.ExtensionJournal[2].Status != hook.InvocationCanceled {
		t.Fatalf("failed post journal = %#v", snapshot.ExtensionJournal)
	}
}

func TestToolResultHookPersistenceCutPointsFailClosedWithoutDanglingInvocations(t *testing.T) {
	for _, test := range []struct {
		name      string
		operation string
		wantCalls int
	}{
		{name: "result and prepared atomic commit", operation: "tool-results", wantCalls: 0},
		{name: "pending commit", operation: "tool-result-hook-pending", wantCalls: 0},
		{name: "terminal commit", operation: "tool-result-hook-terminal", wantCalls: 1},
		{name: "context consume commit", operation: "tool-result-hook-consume", wantCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newPostFaultStore(test.operation)
			post := newRecordingToolResultHook("fault", hook.ToolResultScope{All: true, Statuses: []tool.ResultStatus{tool.ResultSucceeded}}, "fault context")
			executor := preflightBatchExecutor(model.ToolCallRequest{Name: "good", Arguments: []byte(`{"value":"one"}`)})
			access, stop := startToolPreflightApplication(t, store, executor,
				AgentRuntimeConfig{ToolKeys: []string{"good"}, MaxInlineToolResultBytes: 1024},
				toolModule{key: "good", value: &fixedResultTool{definition: testToolDefinition(t, "good"), status: tool.ResultSucceeded}},
				toolResultHookModule{hooks: []hook.ToolResultHook{post}},
			)
			defer stop()
			opened := createRuntimeTestSession(t, access)
			result, err := access.SendAndWait(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("fault")})
			if err != nil || (result.Outcome != session.RunFailed && result.Outcome != session.RunInterrupted) {
				t.Fatalf("result=%#v err=%v", result, err)
			}
			if post.callCount() != test.wantCalls || len(executor.Requests()) != 1 {
				t.Fatalf("calls=%d requests=%d", post.callCount(), len(executor.Requests()))
			}
			snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range snapshot.ExtensionJournal {
				if !entry.Status.Terminal() || entry.EffectDisposition == hook.EffectPending || entry.ContextDisposition == hook.ContextPending {
					t.Fatalf("dangling entry after %s: %#v", test.operation, entry)
				}
			}
			if test.operation != "tool-results" && !hasToolResultStatus(snapshot.History, tool.ResultSucceeded) {
				t.Fatalf("original ToolResult was rolled back: %#v", snapshot.History)
			}
		})
	}
}

func TestToolResultHookRecoveryNeverReplaysCommandsOrCarriesContextIntoAnAbandonedStep(t *testing.T) {
	for _, status := range []hook.InvocationStatus{hook.InvocationPrepared, hook.InvocationPending, hook.InvocationSucceeded} {
		t.Run(string(status), func(t *testing.T) {
			store, sessionID := seedToolResultHookRecoverySession(t, status)
			post := newRecordingToolResultHook("recovery", hook.ToolResultScope{All: true, Statuses: []tool.ResultStatus{tool.ResultSucceeded}}, "must not replay")
			access, stop := startToolPreflightApplication(t, store, model.NewFakeModelExecutor(), AgentRuntimeConfig{},
				toolResultHookModule{hooks: []hook.ToolResultHook{post}})
			defer stop()
			if _, err := access.ResumeSession(t.Context(), interaction.ResumeSessionRequest{SessionID: sessionID}); err != nil {
				t.Fatal(err)
			}
			if post.callCount() != 0 {
				t.Fatalf("recovery replayed %s command", status)
			}
			final, err := store.Load(t.Context(), session.SessionRef{SessionID: sessionID})
			if err != nil {
				t.Fatal(err)
			}
			entry := final.ExtensionJournal[0]
			if !entry.Status.Terminal() || entry.EffectDisposition == hook.EffectPending || entry.ContextDisposition == hook.ContextPending || len(entry.ContextInputs) != 0 {
				t.Fatalf("recovered %s entry = %#v", status, entry)
			}
		})
	}
}

func TestToolResultHookNeverRewritesCommittedToolHistory(t *testing.T) {
	post := &blockingToolResultHook{
		descriptor: preflightDescriptor("post-append-only"),
		scope:      hook.ToolResultScope{All: true, Statuses: []tool.ResultStatus{tool.ResultSucceeded}},
		entered:    make(chan struct{}), release: make(chan struct{}),
	}
	executor := preflightBatchExecutor(model.ToolCallRequest{Name: "good", Arguments: []byte(`{"value":"one"}`)})
	access, store, stop := startRound7Application(t, executor,
		AgentRuntimeConfig{ToolKeys: []string{"good"}, MaxInlineToolResultBytes: 1024},
		toolModule{key: "good", value: &fixedResultTool{definition: testToolDefinition(t, "good"), status: tool.ResultSucceeded}},
		toolResultHookModule{hooks: []hook.ToolResultHook{post}},
	)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	done := make(chan interaction.RunResult, 1)
	errs := make(chan error, 1)
	go func() {
		result, err := access.SendAndWait(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("append")})
		if err != nil {
			errs <- err
			return
		}
		done <- result
	}()
	waitForSignal(t, post.entered, "ToolResultHook")
	prepared, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	prefix := append([]session.HistoryFact(nil), prepared.History...)
	close(post.release)
	select {
	case err := <-errs:
		t.Fatal(err)
	case result := <-done:
		if result.Outcome != session.RunCompleted {
			t.Fatalf("result=%#v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not finish")
	}
	final, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(final.History) < len(prefix) || !reflect.DeepEqual(final.History[:len(prefix)], prefix) {
		t.Fatalf("Post Hook rewrote committed History: before=%#v after=%#v", prefix, final.History)
	}
}

func TestNoToolResultHooksKeepTheOriginalZeroExtensionFastPath(t *testing.T) {
	executor := preflightBatchExecutor(model.ToolCallRequest{Name: "good", Arguments: []byte(`{"value":"one"}`)})
	access, store, stop := startRound7Application(t, executor,
		AgentRuntimeConfig{ToolKeys: []string{"good"}, MaxInlineToolResultBytes: 1024},
		toolModule{key: "good", value: &fixedResultTool{definition: testToolDefinition(t, "good"), status: tool.ResultSucceeded}},
	)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	result, err := access.SendAndWait(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("no post")})
	if err != nil || result.Outcome != session.RunCompleted {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ExtensionJournal) != 0 {
		t.Fatalf("no-Hook path created extension state: %#v", snapshot.ExtensionJournal)
	}
}

func TestCancelWinsWhileToolResultHookIsRunningAndSettlesEveryReservation(t *testing.T) {
	post := &blockingToolResultHook{
		descriptor: preflightDescriptor("post-cancel"),
		scope:      hook.ToolResultScope{All: true, Statuses: []tool.ResultStatus{tool.ResultSucceeded}},
		entered:    make(chan struct{}), release: make(chan struct{}),
	}
	executor := preflightBatchExecutor(model.ToolCallRequest{Name: "good", Arguments: []byte(`{"value":"one"}`)})
	access, store, stop := startRound7Application(t, executor,
		AgentRuntimeConfig{ToolKeys: []string{"good"}, MaxInlineToolResultBytes: 1024},
		toolModule{key: "good", value: &fixedResultTool{definition: testToolDefinition(t, "good"), status: tool.ResultSucceeded}},
		toolResultHookModule{hooks: []hook.ToolResultHook{post}},
	)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	done := make(chan interaction.RunResult, 1)
	errs := make(chan error, 1)
	go func() {
		result, err := access.SendAndWait(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("cancel post")})
		if err != nil {
			errs <- err
			return
		}
		done <- result
	}()
	waitForSignal(t, post.entered, "ToolResultHook")
	snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if err := access.Cancel(t.Context(), interaction.CancelRequest{SessionID: opened.SessionID, ExpectedRevision: snapshot.Revision}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errs:
		t.Fatal(err)
	case result := <-done:
		if result.Outcome != session.RunCanceled {
			t.Fatalf("result=%#v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled run did not finish")
	}
	final, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range final.ExtensionJournal {
		if !entry.Status.Terminal() || entry.EffectDisposition == hook.EffectPending || entry.ContextDisposition == hook.ContextPending {
			t.Fatalf("cancel left Post invocation dangling: %#v", entry)
		}
	}
}

type recordingToolResultHook struct {
	mu         sync.Mutex
	descriptor hook.ExtensionDescriptor
	scope      hook.ToolResultScope
	context    string
	err        error
	views      []hook.ToolResultView
}

type blockingToolResultHook struct {
	descriptor hook.ExtensionDescriptor
	scope      hook.ToolResultScope
	entered    chan struct{}
	release    chan struct{}
	once       sync.Once
}

func (h *blockingToolResultHook) Descriptor() hook.ExtensionDescriptor { return h.descriptor }
func (h *blockingToolResultHook) Scope() hook.ToolResultScope          { return h.scope }
func (h *blockingToolResultHook) Evaluate(ctx context.Context, _ hook.ToolResultView) (hook.ToolResultHookResult, error) {
	h.once.Do(func() { close(h.entered) })
	select {
	case <-h.release:
		return hook.ToolResultHookResult{}, nil
	case <-ctx.Done():
		return hook.ToolResultHookResult{}, ctx.Err()
	}
}

func newRecordingToolResultHook(key string, scope hook.ToolResultScope, contextText string) *recordingToolResultHook {
	return &recordingToolResultHook{descriptor: preflightDescriptor("post-" + key), scope: scope, context: contextText}
}

func (h *recordingToolResultHook) Descriptor() hook.ExtensionDescriptor { return h.descriptor }
func (h *recordingToolResultHook) Scope() hook.ToolResultScope          { return h.scope }
func (h *recordingToolResultHook) Evaluate(_ context.Context, view hook.ToolResultView) (hook.ToolResultHookResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.views = append(h.views, view)
	if h.err != nil || h.context == "" {
		return hook.ToolResultHookResult{}, h.err
	}
	return hook.ToolResultHookResult{Context: []model.Input{{Message: &agent.Message{
		ID: agent.MessageID("context-" + string(view.InvocationID)), SessionID: view.SessionID, RunID: view.RunID, StepID: view.NextStepID,
		Role: agent.RoleUser, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: h.context}},
	}}}}, nil
}
func (h *recordingToolResultHook) callCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.views)
}
func (h *recordingToolResultHook) Views() []hook.ToolResultView {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hook.ToolResultView(nil), h.views...)
}

type toolResultHookModule struct{ hooks []hook.ToolResultHook }

func (toolResultHookModule) ID() string { return "test.tool-result-hook" }
func (m toolResultHookModule) Register(reg agentslot.Registrar) error {
	contributions := make([]agentslot.Contribution, 0, len(m.hooks))
	for _, candidate := range m.hooks {
		contributions = append(contributions, agentslot.Append(hook.ToolResultHookSlot, candidate))
	}
	return reg.Contribute(contributions...)
}

type fixedResultTool struct {
	definition tool.Definition
	status     tool.ResultStatus
}

func (t *fixedResultTool) Definition() tool.Definition       { return t.definition }
func (*fixedResultTool) ParallelSafety() tool.ParallelSafety { return tool.ParallelSafe }
func (t *fixedResultTool) Invoke(_ context.Context, invocation tool.ToolInvocation) tool.ToolResult {
	result := tool.ToolResult{CallID: invocation.Call.ID, Status: t.status, Output: json.RawMessage(`{"source":"tool"}`)}
	if t.status == tool.ResultFailed {
		result.Error = &tool.StructuredError{Code: "tool_failed", Message: "tool failed safely"}
	}
	return result
}

type postCommitShape struct {
	revision         agent.Revision
	results          int
	terminalJournals int
	postPrepared     int
}

type postRecordingStore struct {
	*session.MemoryStore
	mu      sync.Mutex
	commits []postCommitShape
}

type postFaultStore struct {
	*session.MemoryStore
	operation string
	fired     atomic.Bool
}

func newPostFaultStore(operation string) *postFaultStore {
	return &postFaultStore{MemoryStore: session.NewMemoryStore(), operation: operation}
}

func (s *postFaultStore) Commit(ctx context.Context, request session.CommitRequest) (session.Commit, error) {
	if strings.Contains(request.IdempotencyKey, "runtime-"+s.operation+"-") && s.fired.CompareAndSwap(false, true) {
		return session.Commit{}, errors.New("injected post commit failure")
	}
	return s.MemoryStore.Commit(ctx, request)
}

func newPostRecordingStore() *postRecordingStore {
	return &postRecordingStore{MemoryStore: session.NewMemoryStore()}
}
func (s *postRecordingStore) Commit(ctx context.Context, request session.CommitRequest) (session.Commit, error) {
	shape := postCommitShape{revision: request.ExpectedRevision.Next()}
	for _, change := range request.Changes {
		switch change.Kind {
		case session.AppendToolResult:
			shape.results++
		case session.UpdateRunJournal:
			if change.Journal.Status != session.JournalPrepared && change.Journal.Status != session.JournalPending {
				shape.terminalJournals++
			}
		case session.UpdateExtensionJournal:
			if change.Extension.Boundary == hook.BoundaryToolResult && change.Extension.Status == hook.InvocationPrepared {
				shape.postPrepared++
			}
		}
	}
	commit, err := s.MemoryStore.Commit(ctx, request)
	if err == nil && shape.results > 0 {
		s.mu.Lock()
		s.commits = append(s.commits, shape)
		s.mu.Unlock()
	}
	return commit, err
}
func (s *postRecordingStore) resultCommits() []postCommitShape {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]postCommitShape(nil), s.commits...)
}

func countInputText(inputs []model.Input, text string) int {
	count := 0
	for _, input := range inputs {
		if input.Message == nil {
			continue
		}
		for _, part := range input.Message.Parts {
			if strings.Contains(part.Text, text) {
				count++
			}
		}
	}
	return count
}

func inputTextIndex(inputs []model.Input, text string) int {
	for index, input := range inputs {
		if input.Message == nil {
			continue
		}
		for _, part := range input.Message.Parts {
			if strings.Contains(part.Text, text) {
				return index
			}
		}
	}
	return len(inputs) + 1
}

func hasToolResultStatus(history []session.HistoryFact, status tool.ResultStatus) bool {
	for _, fact := range history {
		if fact.ToolResult != nil && fact.ToolResult.Status == status {
			return true
		}
	}
	return false
}

func seedToolResultHookRecoverySession(t *testing.T, status hook.InvocationStatus) (*session.MemoryStore, agent.SessionID) {
	t.Helper()
	store := session.NewMemoryStore()
	sessionID := agent.SessionID("session-post-recovery")
	created, err := store.Create(t.Context(), session.NewSession{
		Session: agent.Session{ID: sessionID, AgentID: "agent", WorkspaceID: "workspace"}, ModelConfig: testDefaultModel(), RunState: session.RunIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	call := agent.ToolCall{
		ID: "call-post-recovery", MessageID: "message-post-recovery", SessionID: sessionID,
		RunID: "run-post-recovery", StepID: "step-post-source", Name: "effect", Arguments: []byte(`{"value":"one"}`),
	}
	result := tool.ToolResult{CallID: call.ID, Status: tool.ResultSucceeded, Output: json.RawMessage(`{"ok":true}`)}
	view := hook.ToolResultView{
		InvocationID: "post-recovery-invocation", SessionID: sessionID, AgentID: "agent", WorkspaceID: "workspace", Revision: created.Revision.Next(),
		RunID: call.RunID, StepID: call.StepID, NextStepID: "step-post-target", MessageID: call.MessageID, ToolCallID: call.ID,
		ToolKey: call.Name, Arguments: call.Arguments, Result: result,
	}
	fingerprint, err := hook.FingerprintTypedInput(view)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	entry := session.ExtensionJournalEntry{
		InvocationID: view.InvocationID, Sequence: 1, Descriptor: preflightDescriptor("post-recovery"), Boundary: hook.BoundaryToolResult,
		SessionID: sessionID, RunID: call.RunID, StepID: call.StepID, TargetStepID: view.NextStepID, MessageID: call.MessageID, ToolCallID: call.ID,
		InputDigest: fingerprint.Digest, PreparedRevision: view.Revision, PreparedAt: now,
		Status: hook.InvocationPrepared, EffectDisposition: hook.EffectNone, ContextDisposition: hook.ContextNone,
	}
	commit, err := store.Commit(t.Context(), session.CommitRequest{
		SessionID: sessionID, ExpectedRevision: created.Revision, IdempotencyKey: "prepare-post-recovery",
		Changes: []session.Change{{Kind: session.UpdateExtensionJournal, Extension: &entry}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if status == hook.InvocationPrepared {
		return store, sessionID
	}
	entry.Status, entry.PendingAt = hook.InvocationPending, now.Add(time.Millisecond)
	commit, err = store.Commit(t.Context(), session.CommitRequest{
		SessionID: sessionID, ExpectedRevision: commit.Revision, IdempotencyKey: "pending-post-recovery",
		Changes: []session.Change{{Kind: session.UpdateExtensionJournal, Extension: &entry}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if status == hook.InvocationPending {
		return store, sessionID
	}
	contextInputs := []model.Input{{Message: &agent.Message{
		ID: "message-post-context", SessionID: sessionID, RunID: call.RunID, StepID: view.NextStepID,
		Role: agent.RoleUser, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "recovered context"}},
	}}}
	contextFingerprint, err := hook.FingerprintTypedInput(contextInputs)
	if err != nil {
		t.Fatal(err)
	}
	entry.Status, entry.FinishedAt, entry.Result, entry.EffectDisposition = hook.InvocationSucceeded, now.Add(2*time.Millisecond), &hook.InvocationResult{}, hook.EffectPending
	entry.ContextDisposition, entry.ContextInputs = hook.ContextPending, contextInputs
	entry.ContextDigest, entry.ContextBytes = contextFingerprint.Digest, contextFingerprint.Bytes
	if _, err := store.Commit(t.Context(), session.CommitRequest{
		SessionID: sessionID, ExpectedRevision: commit.Revision, IdempotencyKey: "terminal-post-recovery",
		Changes: []session.Change{{Kind: session.UpdateExtensionJournal, Extension: &entry}},
	}); err != nil {
		t.Fatal(err)
	}
	return store, sessionID
}

var _ session.SessionStore = (*postRecordingStore)(nil)
var _ session.SessionStore = (*postFaultStore)(nil)
var _ hook.ToolResultHook = (*recordingToolResultHook)(nil)
var _ hook.ToolResultHook = (*blockingToolResultHook)(nil)
var _ tool.Tool = (*fixedResultTool)(nil)
