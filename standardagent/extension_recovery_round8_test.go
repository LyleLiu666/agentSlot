package standardagent

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
	"github.com/LyleLiu666/agentSlot/tool"
)

func TestResumeConvergesLifecyclePreparedRunAndPostContextWithoutRewritingHistory(t *testing.T) {
	store := newPreflightRecordingStore()
	call, preflightDefinition := seedToolPreflightRecoverySessionInStore(t, store, hook.InvocationPrepared)
	snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: call.SessionID})
	if err != nil {
		t.Fatal(err)
	}

	lifecycle := &recordingSessionLifecycle{key: "combined-recovery", phases: []hook.LifecyclePhase{hook.LifecycleOpen}}
	lifecycleEntry := preparedLifecycleRecoveryEntry(t, snapshot, lifecycle.Descriptor(), 2)
	postEntry := preparedPostRecoveryEntry(t, snapshot, call, 3)
	commit, err := store.Commit(t.Context(), session.CommitRequest{
		SessionID: call.SessionID, ExpectedRevision: snapshot.Revision, IdempotencyKey: "combined-recovery-prepare",
		Changes: []session.Change{
			{Kind: session.UpdateExtensionJournal, Extension: &lifecycleEntry},
			{Kind: session.UpdateExtensionJournal, Extension: &postEntry},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	lifecycleEntry.Status, lifecycleEntry.PendingAt = hook.InvocationPending, lifecycleEntry.PreparedAt.Add(time.Millisecond)
	postEntry.Status, postEntry.PendingAt = hook.InvocationPending, postEntry.PreparedAt.Add(time.Millisecond)
	commit, err = store.Commit(t.Context(), session.CommitRequest{
		SessionID: call.SessionID, ExpectedRevision: commit.Revision, IdempotencyKey: "combined-recovery-pending",
		Changes: []session.Change{
			{Kind: session.UpdateExtensionJournal, Extension: &lifecycleEntry},
			{Kind: session.UpdateExtensionJournal, Extension: &postEntry},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	postContext := []model.Input{{Message: &agent.Message{
		ID: "abandoned-post-context", SessionID: call.SessionID, RunID: call.RunID, StepID: postEntry.TargetStepID,
		Role: agent.RoleUser, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "abandoned context must not reach the model"}},
	}}}
	contextFingerprint, err := hook.FingerprintTypedInput(postContext)
	if err != nil {
		t.Fatal(err)
	}
	postEntry.Status, postEntry.FinishedAt = hook.InvocationSucceeded, postEntry.PendingAt.Add(time.Millisecond)
	postEntry.Result, postEntry.EffectDisposition = &hook.InvocationResult{}, hook.EffectPending
	postEntry.ContextDisposition, postEntry.ContextInputs = hook.ContextPending, postContext
	postEntry.ContextDigest, postEntry.ContextBytes = contextFingerprint.Digest, contextFingerprint.Bytes
	if _, err := store.Commit(t.Context(), session.CommitRequest{
		SessionID: call.SessionID, ExpectedRevision: commit.Revision, IdempotencyKey: "combined-recovery-post-terminal",
		Changes: []session.Change{{Kind: session.UpdateExtensionJournal, Extension: &postEntry}},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := store.Load(t.Context(), session.SessionRef{SessionID: call.SessionID})
	if err != nil {
		t.Fatal(err)
	}

	preflight := &recordingToolPreflight{
		descriptor: preflightDefinition, scope: hook.ToolScope{All: true},
		result: hook.ToolPreflightResult{Decision: hook.DecisionAllow},
	}
	post := newRecordingToolResultHook("combined-recovery", hook.ToolResultScope{
		ToolKeys: []string{call.Name}, Statuses: []tool.ResultStatus{tool.ResultSucceeded},
	}, "new post context")
	post.descriptor = postEntry.Descriptor
	installed := &countingTool{definition: testToolDefinition(t, call.Name)}
	executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{complete("recovered run completed")}})
	access, stop := startToolPreflightApplication(t, store, executor,
		AgentRuntimeConfig{ToolKeys: []string{call.Name}, MaxInlineToolResultBytes: 1024},
		toolModule{key: call.Name, value: installed},
		toolPreflightModule{gates: []hook.ToolPreflight{preflight}},
		toolResultHookModule{hooks: []hook.ToolResultHook{post}},
		sessionLifecycleModule{lifecycles: []hook.SessionLifecycle{lifecycle}},
	)
	defer stop()
	if _, err := access.ResumeSession(t.Context(), interaction.ResumeSessionRequest{SessionID: call.SessionID}); err != nil {
		t.Fatal(err)
	}
	if err := access.WhenIdle(t.Context(), interaction.WhenIdleRequest{SessionID: call.SessionID}); err != nil {
		t.Fatal(err)
	}
	final, err := store.Load(t.Context(), session.SessionRef{SessionID: call.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(final.History) < len(before.History) || !reflect.DeepEqual(final.History[:len(before.History)], before.History) {
		t.Fatalf("recovery rewrote append-only History:\nbefore=%#v\nafter=%#v", before.History, final.History)
	}
	if final.RunState != session.RunIdle || preflight.callCount() != 1 || installed.calls.Load() != 1 || lifecycle.calls() != 1 {
		t.Fatalf("combined recovery did not finish exactly once: state=%s pre=%d tool=%d lifecycle=%d", final.RunState, preflight.callCount(), installed.calls.Load(), lifecycle.calls())
	}
	if post.callCount() != 1 || post.Views()[0].InvocationID == postEntry.InvocationID {
		t.Fatalf("old Post command was replayed or new Post was skipped: views=%#v", post.Views())
	}
	for _, request := range executor.Requests() {
		if countInputText(request.Inputs, "abandoned context must not reach the model") != 0 {
			t.Fatalf("abandoned Post context reached model request: %#v", request.Inputs)
		}
	}
	oldLifecycle, ok := extensionEntryByInvocation(final.ExtensionJournal, lifecycleEntry.InvocationID)
	if !ok || oldLifecycle.Status != hook.InvocationOutcomeUnknown || oldLifecycle.EffectDisposition != hook.EffectDiscarded {
		t.Fatalf("old lifecycle did not settle safely: %#v", oldLifecycle)
	}
	oldPost, ok := extensionEntryByInvocation(final.ExtensionJournal, postEntry.InvocationID)
	if !ok || oldPost.Status != hook.InvocationSucceeded || oldPost.EffectDisposition != hook.EffectDiscarded || oldPost.ContextDisposition != hook.ContextDiscarded {
		t.Fatalf("old Post did not settle safely: %#v", oldPost)
	}
	if store.runtimeOperationCount("extension-recovery-after-open") != 1 {
		t.Fatalf("post-open recovery commit count = %d; operations=%#v", store.runtimeOperationCount("extension-recovery-after-open"), store.operations)
	}
}

func preparedLifecycleRecoveryEntry(t *testing.T, snapshot session.Snapshot, descriptor hook.ExtensionDescriptor, sequence session.ExtensionSequence) session.ExtensionJournalEntry {
	t.Helper()
	view := hook.SessionLifecycleView{
		InvocationID: "combined-old-lifecycle", SessionID: snapshot.Session.ID, AgentID: snapshot.Session.AgentID,
		WorkspaceID: snapshot.Session.WorkspaceID, Revision: snapshot.Revision.Next(), Phase: hook.LifecycleOpen, OpenKind: hook.OpenResume,
	}
	fingerprint, err := hook.FingerprintTypedInput(view)
	if err != nil {
		t.Fatal(err)
	}
	return session.ExtensionJournalEntry{
		InvocationID: view.InvocationID, Sequence: sequence, Descriptor: descriptor,
		Boundary: hook.BoundarySessionLifecycle, SessionID: view.SessionID,
		LifecyclePhase: view.Phase, LifecycleOpenKind: view.OpenKind,
		InputDigest: fingerprint.Digest, PreparedRevision: view.Revision, PreparedAt: time.Now().UTC(),
		Status: hook.InvocationPrepared, EffectDisposition: hook.EffectNone, ContextDisposition: hook.ContextNone,
	}
}

func preparedPostRecoveryEntry(t *testing.T, snapshot session.Snapshot, call agent.ToolCall, sequence session.ExtensionSequence) session.ExtensionJournalEntry {
	t.Helper()
	result := tool.ToolResult{CallID: call.ID, Status: tool.ResultSucceeded, Output: json.RawMessage(`{"old":true}`)}
	view := hook.ToolResultView{
		InvocationID: "combined-old-post", SessionID: call.SessionID, AgentID: snapshot.Session.AgentID,
		WorkspaceID: snapshot.Session.WorkspaceID, Revision: snapshot.Revision.Next(), RunID: call.RunID, StepID: call.StepID,
		NextStepID: "combined-old-post-next", MessageID: call.MessageID, ToolCallID: call.ID,
		ToolKey: call.Name, Arguments: call.Arguments, Result: result,
	}
	fingerprint, err := hook.FingerprintTypedInput(view)
	if err != nil {
		t.Fatal(err)
	}
	return session.ExtensionJournalEntry{
		InvocationID: view.InvocationID, Sequence: sequence, Descriptor: preflightDescriptor("post-combined-recovery"),
		Boundary: hook.BoundaryToolResult, SessionID: call.SessionID, RunID: call.RunID, StepID: call.StepID,
		TargetStepID: view.NextStepID, MessageID: call.MessageID, ToolCallID: call.ID,
		InputDigest: fingerprint.Digest, PreparedRevision: view.Revision, PreparedAt: time.Now().UTC(),
		Status: hook.InvocationPrepared, EffectDisposition: hook.EffectNone, ContextDisposition: hook.ContextNone,
	}
}

func TestResumeUsesOnePlannedPostOpenRecoveryCommitAcrossExtensionBoundaries(t *testing.T) {
	store := newPreflightRecordingStore()
	sessionID := agent.SessionID("session-unified-extension-recovery")
	created, err := store.Create(t.Context(), session.NewSession{
		Session:     agent.Session{ID: sessionID, AgentID: "agent", WorkspaceID: "workspace"},
		ModelConfig: testDefaultModel(), RunState: session.RunIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	input, post := mixedRecoveryEntries(t, sessionID, created.Revision.Next())
	commit, err := store.Commit(t.Context(), session.CommitRequest{
		SessionID: sessionID, ExpectedRevision: created.Revision, IdempotencyKey: "mixed-recovery-prepare",
		Changes: []session.Change{
			{Kind: session.UpdateExtensionJournal, Extension: &input},
			{Kind: session.UpdateExtensionJournal, Extension: &post},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	post.Status, post.PendingAt = hook.InvocationPending, post.PreparedAt.Add(time.Millisecond)
	commit, err = store.Commit(t.Context(), session.CommitRequest{
		SessionID: sessionID, ExpectedRevision: commit.Revision, IdempotencyKey: "mixed-recovery-post-pending",
		Changes: []session.Change{{Kind: session.UpdateExtensionJournal, Extension: &post}},
	})
	if err != nil {
		t.Fatal(err)
	}
	contextInputs := []model.Input{{Message: &agent.Message{
		ID: "mixed-post-context", SessionID: sessionID, RunID: post.RunID, StepID: post.TargetStepID,
		Role: agent.RoleUser, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "must be discarded"}},
	}}}
	contextFingerprint, err := hook.FingerprintTypedInput(contextInputs)
	if err != nil {
		t.Fatal(err)
	}
	post.Status, post.FinishedAt, post.Result = hook.InvocationSucceeded, post.PendingAt.Add(time.Millisecond), &hook.InvocationResult{}
	post.EffectDisposition, post.ContextDisposition, post.ContextInputs = hook.EffectPending, hook.ContextPending, contextInputs
	post.ContextDigest, post.ContextBytes = contextFingerprint.Digest, contextFingerprint.Bytes
	if _, err := store.Commit(t.Context(), session.CommitRequest{
		SessionID: sessionID, ExpectedRevision: commit.Revision, IdempotencyKey: "mixed-recovery-post-terminal",
		Changes: []session.Change{{Kind: session.UpdateExtensionJournal, Extension: &post}},
	}); err != nil {
		t.Fatal(err)
	}

	inputGate := &recordingInputGate{descriptor: input.Descriptor, result: hook.InputGateResult{Decision: hook.DecisionAccept}}
	postHook := newRecordingToolResultHook("mixed-recovery", hook.ToolResultScope{ToolKeys: []string{"effect"}, Statuses: []tool.ResultStatus{tool.ResultSucceeded}}, "replayed")
	postHook.descriptor = post.Descriptor
	access, stop := startToolPreflightApplication(t, store, model.NewFakeModelExecutor(), AgentRuntimeConfig{},
		inputGateModule{gates: []hook.InputGate{inputGate}}, toolResultHookModule{hooks: []hook.ToolResultHook{postHook}},
	)
	defer stop()
	if _, err := access.ResumeSession(t.Context(), interaction.ResumeSessionRequest{SessionID: sessionID}); err != nil {
		t.Fatal(err)
	}
	if inputGate.viewCount() != 0 || postHook.callCount() != 0 {
		t.Fatalf("recovery replayed commands: input=%d post=%d", inputGate.viewCount(), postHook.callCount())
	}
	if store.runtimeOperationCount("extension-recovery-after-open") != 1 ||
		store.runtimeOperationCount("input-gate-recovery") != 0 || store.runtimeOperationCount("tool-result-hook-recovery") != 0 {
		t.Fatalf("recovery commits were not unified: %#v", store.operations)
	}
	final, err := store.Load(t.Context(), session.SessionRef{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(final.ExtensionJournal) != 2 || final.ExtensionJournal[0].Status != hook.InvocationFailed ||
		final.ExtensionJournal[0].EffectDisposition != hook.EffectDiscarded ||
		final.ExtensionJournal[1].Status != hook.InvocationSucceeded ||
		final.ExtensionJournal[1].EffectDisposition != hook.EffectDiscarded ||
		final.ExtensionJournal[1].ContextDisposition != hook.ContextDiscarded || len(final.ExtensionJournal[1].ContextInputs) != 0 {
		t.Fatalf("unified recovery result = %#v", final.ExtensionJournal)
	}
}

func mixedRecoveryEntries(t *testing.T, sessionID agent.SessionID, revision agent.Revision) (session.ExtensionJournalEntry, session.ExtensionJournalEntry) {
	t.Helper()
	now := time.Now().UTC()
	inputView := hook.InputGateView{
		InvocationID: "mixed-input", SessionID: sessionID, AgentID: "agent", WorkspaceID: "workspace", Revision: revision,
		Operation: hook.InputSend, MessageID: "mixed-message", Input: agent.MessageInput{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "input"}}},
	}
	inputFingerprint, err := hook.FingerprintTypedInput(inputView)
	if err != nil {
		t.Fatal(err)
	}
	input := session.ExtensionJournalEntry{
		InvocationID: inputView.InvocationID, Sequence: 1, Descriptor: inputGateDescriptor("mixed-recovery"),
		Boundary: hook.BoundaryInputGate, SessionID: sessionID, MessageID: inputView.MessageID,
		InputDigest: inputFingerprint.Digest, PreparedRevision: revision, PreparedAt: now,
		Status: hook.InvocationPrepared, EffectDisposition: hook.EffectNone, ContextDisposition: hook.ContextNone,
	}
	result := tool.ToolResult{CallID: "mixed-call", Status: tool.ResultSucceeded, Output: json.RawMessage(`{"ok":true}`)}
	postView := hook.ToolResultView{
		InvocationID: "mixed-post", SessionID: sessionID, AgentID: "agent", WorkspaceID: "workspace", Revision: revision,
		RunID: "mixed-run", StepID: "mixed-step", NextStepID: "mixed-next-step", MessageID: "mixed-assistant", ToolCallID: result.CallID,
		ToolKey: "effect", Arguments: json.RawMessage(`{"value":"one"}`), Result: result,
	}
	postFingerprint, err := hook.FingerprintTypedInput(postView)
	if err != nil {
		t.Fatal(err)
	}
	post := session.ExtensionJournalEntry{
		InvocationID: postView.InvocationID, Sequence: 2, Descriptor: preflightDescriptor("post-mixed-recovery"),
		Boundary: hook.BoundaryToolResult, SessionID: sessionID, RunID: postView.RunID, StepID: postView.StepID, TargetStepID: postView.NextStepID,
		MessageID: postView.MessageID, ToolCallID: postView.ToolCallID,
		InputDigest: postFingerprint.Digest, PreparedRevision: revision, PreparedAt: now,
		Status: hook.InvocationPrepared, EffectDisposition: hook.EffectNone, ContextDisposition: hook.ContextNone,
	}
	return input, post
}
