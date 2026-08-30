package standardagent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/policy"
	"github.com/LyleLiu666/agentSlot/session"
	"github.com/LyleLiu666/agentSlot/tool"
)

func TestToolCallAndAllPreflightReservationsArePreparedInOneCommit(t *testing.T) {
	store := newPreflightRecordingStore()
	first := &recordingToolPreflight{descriptor: preflightDescriptor("first"), scope: hook.ToolScope{All: true}, result: hook.ToolPreflightResult{Decision: hook.DecisionAllow}}
	second := &recordingToolPreflight{descriptor: preflightDescriptor("second"), scope: hook.ToolScope{ToolKeys: []string{"echo"}}, result: hook.ToolPreflightResult{Decision: hook.DecisionAllow}}
	installed := &countingTool{definition: testToolDefinition(t, "echo")}
	executor := preflightExecutor("echo", `{"value":"one"}`)
	access, stop := startToolPreflightApplication(t, store, executor, AgentRuntimeConfig{ToolKeys: []string{"echo"}, MaxInlineToolResultBytes: 1024},
		toolModule{key: "echo", value: installed}, toolPreflightModule{gates: []hook.ToolPreflight{first, second}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	result, err := access.SendAndWait(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("run")})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != session.RunCompleted || installed.calls.Load() != 1 || first.callCount() != 1 || second.callCount() != 1 {
		t.Fatalf("result=%#v tool=%d preflight=%d/%d", result, installed.calls.Load(), first.callCount(), second.callCount())
	}
	preparedCommits := store.toolPreparedCommits()
	if len(preparedCommits) != 1 || preparedCommits[0].toolCalls != 1 || preparedCommits[0].runPrepared != 1 || preparedCommits[0].preflightPrepared != 2 {
		t.Fatalf("Tool preparation commits = %#v", preparedCommits)
	}
	if pending, finished, applied := store.runtimeOperationCount("tool-preflight-pending"), store.runtimeOperationCount("tool-preflight-finished"), store.runtimeOperationCount("tool-preflight-effect"); pending != 1 || finished != 2 || applied != 1 {
		t.Fatalf("preflight transition commits pending/finished/effect = %d/%d/%d, want 1/2/1", pending, finished, applied)
	}
	for _, view := range append(first.Views(), second.Views()...) {
		if view.Revision != preparedCommits[0].revision || view.ToolKey != "echo" || view.ToolCallID == "" || view.RunID == "" || view.StepID == "" {
			t.Fatalf("preflight view = %#v, prepared revision=%d", view, preparedCommits[0].revision)
		}
	}
}

func TestToolPreflightDenyDoesNotAuthorizeOrAbortOtherBatchCalls(t *testing.T) {
	deny := &recordingToolPreflight{
		descriptor: preflightDescriptor("deny-first"), scope: hook.ToolScope{ToolKeys: []string{"denied"}},
		result: hook.ToolPreflightResult{Decision: hook.DecisionDeny, Reason: "blocked by project policy"},
	}
	allow := &recordingToolPreflight{
		descriptor: preflightDescriptor("allow-all"), scope: hook.ToolScope{All: true},
		result: hook.ToolPreflightResult{Decision: hook.DecisionAllow},
	}
	deniedTool := &countingTool{definition: testToolDefinition(t, "denied")}
	allowedTool := &countingTool{definition: testToolDefinition(t, "allowed")}
	executor := preflightBatchExecutor(
		model.ToolCallRequest{Name: "denied", Arguments: []byte(`{"value":"one"}`)},
		model.ToolCallRequest{Name: "allowed", Arguments: []byte(`{"value":"two"}`)},
	)
	access, store, stop := startRound7Application(t, executor, AgentRuntimeConfig{ToolKeys: []string{"denied", "allowed"}, MaxInlineToolResultBytes: 1024},
		toolModule{key: "denied", value: deniedTool}, toolModule{key: "allowed", value: allowedTool},
		toolPreflightModule{gates: []hook.ToolPreflight{deny, allow}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	result, err := access.SendAndWait(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("batch")})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != session.RunCompleted || deniedTool.calls.Load() != 0 || allowedTool.calls.Load() != 1 || deny.callCount() != 1 || allow.callCount() != 1 {
		t.Fatalf("result=%#v denied=%d allowed=%d gates=%d/%d", result, deniedTool.calls.Load(), allowedTool.calls.Load(), deny.callCount(), allow.callCount())
	}
	snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if !hasToolResultCode(snapshot.History, "preflight_denied") {
		t.Fatalf("denied ToolResult is missing: %#v", snapshot.History)
	}
	var canceled int
	for _, entry := range snapshot.ExtensionJournal {
		if entry.Descriptor.Key == "test.allow-all" && entry.ToolCallID != "" && entry.Status == hook.InvocationCanceled {
			canceled++
		}
	}
	if canceled != 1 {
		t.Fatalf("denied call did not cancel its remaining preflight: %#v", snapshot.ExtensionJournal)
	}
}

func TestToolPreflightRequireApprovalMergesWithGuardWithoutBypassingEither(t *testing.T) {
	preflight := &recordingToolPreflight{
		descriptor: preflightDescriptor("approval"), scope: hook.ToolScope{All: true},
		result: hook.ToolPreflightResult{Decision: hook.DecisionRequireApproval, Reason: "Hook requires review"},
	}
	guard := policy.GuardFunc(func(context.Context, policy.Action) (policy.Decision, error) {
		return policy.Decision{Effect: policy.RequireApproval, Reason: "Guard requires review"}, nil
	})
	var approvalReason string
	approval := policy.ApprovalFunc(func(_ context.Context, request policy.ApprovalRequest) (policy.ApprovalDecision, error) {
		approvalReason = request.Reason
		return policy.ApprovalDecision{Approved: true}, nil
	})
	installed := &countingTool{definition: testToolDefinition(t, "effect")}
	access, _, stop := startRound7Application(t, preflightExecutor("effect", `{"value":"one"}`), AgentRuntimeConfig{ToolKeys: []string{"effect"}, MaxInlineToolResultBytes: 1024},
		toolModule{key: "effect", value: installed}, toolPreflightModule{gates: []hook.ToolPreflight{preflight}},
		preflightPolicyModule{guard: guard, approval: approval})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	result, err := access.SendAndWait(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("approve")})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != session.RunCompleted || installed.calls.Load() != 1 ||
		!strings.Contains(approvalReason, "Hook requires review") || !strings.Contains(approvalReason, "Guard requires review") {
		t.Fatalf("result=%#v calls=%d approval reason=%q", result, installed.calls.Load(), approvalReason)
	}
}

func TestToolPreflightInfrastructureFailureInterruptsTheRunBeforeAnyTool(t *testing.T) {
	failing := &recordingToolPreflight{
		descriptor: preflightDescriptor("failed"), scope: hook.ToolScope{All: true},
		err: &hook.InvocationFailure{Status: hook.InvocationFailed, Code: agent.ErrorCode("hook_pre_tool_failed"), Reason: "preflight command failed"},
	}
	unstarted := &recordingToolPreflight{descriptor: preflightDescriptor("unstarted"), scope: hook.ToolScope{All: true}, result: hook.ToolPreflightResult{Decision: hook.DecisionAllow}}
	firstTool := &countingTool{definition: testToolDefinition(t, "first")}
	secondTool := &countingTool{definition: testToolDefinition(t, "second")}
	access, store, stop := startRound7Application(t, preflightBatchExecutor(
		model.ToolCallRequest{Name: "first", Arguments: []byte(`{"value":"one"}`)},
		model.ToolCallRequest{Name: "second", Arguments: []byte(`{"value":"two"}`)},
	), AgentRuntimeConfig{ToolKeys: []string{"first", "second"}, MaxInlineToolResultBytes: 1024},
		toolModule{key: "first", value: firstTool}, toolModule{key: "second", value: secondTool},
		toolPreflightModule{gates: []hook.ToolPreflight{failing, unstarted}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	result, err := access.SendAndWait(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("fail closed")})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != session.RunInterrupted || firstTool.calls.Load() != 0 || secondTool.calls.Load() != 0 || unstarted.callCount() != 0 {
		t.Fatalf("result=%#v tools=%d/%d unstarted=%d", result, firstTool.calls.Load(), secondTool.calls.Load(), unstarted.callCount())
	}
	snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if !lastRunHasTermination(snapshot.History, session.TerminationExtension, "hook_pre_tool_failed") ||
		countExtensionStatus(snapshot.ExtensionJournal, hook.InvocationCanceled) != 3 {
		t.Fatalf("preflight failure state = %#v / %#v", snapshot.History, snapshot.ExtensionJournal)
	}
}

func TestPipelinedToolPreflightCommitFailureNeverInvokesTheNextComponentOrTool(t *testing.T) {
	for _, test := range []struct {
		operation       string
		wantFirstCalls  int
		wantSecondCalls int
	}{
		{operation: "tool-preflight-pending", wantFirstCalls: 0, wantSecondCalls: 0},
		{operation: "tool-preflight-finished", wantFirstCalls: 1, wantSecondCalls: 0},
		{operation: "tool-preflight-effect", wantFirstCalls: 1, wantSecondCalls: 1},
	} {
		t.Run(test.operation, func(t *testing.T) {
			store := newTransitionFaultStore(test.operation)
			first := &recordingToolPreflight{
				descriptor: preflightDescriptor("first-pipelined"), scope: hook.ToolScope{All: true},
				result: hook.ToolPreflightResult{Decision: hook.DecisionAllow},
			}
			second := &recordingToolPreflight{
				descriptor: preflightDescriptor("second-pipelined"), scope: hook.ToolScope{All: true},
				result: hook.ToolPreflightResult{Decision: hook.DecisionAllow},
			}
			installed := &countingTool{definition: testToolDefinition(t, "effect")}
			access, stop := startToolPreflightApplication(t, store, preflightExecutor("effect", `{"value":"one"}`),
				AgentRuntimeConfig{ToolKeys: []string{"effect"}, MaxInlineToolResultBytes: 1024},
				toolModule{key: "effect", value: installed}, toolPreflightModule{gates: []hook.ToolPreflight{first, second}})
			defer stop()
			opened := createRuntimeTestSession(t, access)
			result, err := access.SendAndWait(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("pipeline failure")})
			if err != nil && !agent.IsCode(err, agent.CodeRuntimeClosed) {
				t.Fatalf("result=%#v err=%v", result, err)
			}
			if err == nil && result.Outcome != session.RunFailed && result.Outcome != session.RunInterrupted {
				t.Fatalf("result=%#v err=%v", result, err)
			}
			if first.callCount() != test.wantFirstCalls || second.callCount() != test.wantSecondCalls || installed.calls.Load() != 0 {
				t.Fatalf("pipelined failure calls first/second/tool = %d/%d/%d", first.callCount(), second.callCount(), installed.calls.Load())
			}
			snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range snapshot.ExtensionJournal {
				if !entry.Status.Terminal() || entry.EffectDisposition == hook.EffectPending || entry.ContextDisposition == hook.ContextPending {
					t.Fatalf("pipelined failure left dangling entry: %#v", entry)
				}
			}
		})
	}
}

func TestToolPreflightFailureSettlementCommitErrorsStillLeaveNoPendingEffect(t *testing.T) {
	for _, operation := range []string{"tool-preflight-failed", "tool-preflight-failure-applied", "tool-preflight-canceled"} {
		t.Run(operation, func(t *testing.T) {
			store := newTransitionFaultStore(operation)
			failing := &recordingToolPreflight{
				descriptor: preflightDescriptor("failing-settlement"), scope: hook.ToolScope{All: true},
				err: &hook.InvocationFailure{Status: hook.InvocationFailed, Code: "hook_preflight_failed", Reason: "expected test failure"},
			}
			unstarted := &recordingToolPreflight{
				descriptor: preflightDescriptor("unstarted-settlement"), scope: hook.ToolScope{All: true},
				result: hook.ToolPreflightResult{Decision: hook.DecisionAllow},
			}
			installed := &countingTool{definition: testToolDefinition(t, "effect")}
			access, stop := startToolPreflightApplication(t, store, preflightExecutor("effect", `{"value":"one"}`),
				AgentRuntimeConfig{ToolKeys: []string{"effect"}, MaxInlineToolResultBytes: 1024},
				toolModule{key: "effect", value: installed}, toolPreflightModule{gates: []hook.ToolPreflight{failing, unstarted}})
			defer stop()
			opened := createRuntimeTestSession(t, access)
			result, err := access.SendAndWait(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("settlement failure")})
			if err != nil || result.Outcome != session.RunInterrupted {
				t.Fatalf("result=%#v err=%v", result, err)
			}
			if failing.callCount() != 1 || unstarted.callCount() != 0 || installed.calls.Load() != 0 {
				t.Fatalf("settlement failure calls failing/unstarted/tool = %d/%d/%d", failing.callCount(), unstarted.callCount(), installed.calls.Load())
			}
			snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range snapshot.ExtensionJournal {
				if !entry.Status.Terminal() || entry.EffectDisposition == hook.EffectPending || entry.ContextDisposition == hook.ContextPending {
					t.Fatalf("%s left dangling entry: %#v", operation, entry)
				}
			}
		})
	}
}

func TestInvalidToolArgumentsCancelReservationsWithoutCallingPreflight(t *testing.T) {
	gate := &recordingToolPreflight{descriptor: preflightDescriptor("must-not-run"), scope: hook.ToolScope{All: true}, result: hook.ToolPreflightResult{Decision: hook.DecisionAllow}}
	installed := &countingTool{definition: testToolDefinition(t, "strict")}
	access, store, stop := startRound7Application(t, preflightExecutor("strict", `{"wrong":true}`), AgentRuntimeConfig{ToolKeys: []string{"strict"}, MaxInlineToolResultBytes: 1024},
		toolModule{key: "strict", value: installed}, toolPreflightModule{gates: []hook.ToolPreflight{gate}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	result, err := access.SendAndWait(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("invalid")})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != session.RunCompleted || gate.callCount() != 0 || installed.calls.Load() != 0 {
		t.Fatalf("result=%#v gate=%d tool=%d", result, gate.callCount(), installed.calls.Load())
	}
	snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ExtensionJournal) != 1 || snapshot.ExtensionJournal[0].Status != hook.InvocationCanceled ||
		snapshot.ExtensionJournal[0].EffectDisposition != hook.EffectDiscarded || !hasToolResultCode(snapshot.History, "invalid_arguments") {
		t.Fatalf("invalid argument state = %#v / %#v", snapshot.ExtensionJournal, snapshot.History)
	}
}

func TestToolPreflightNeverRewritesPreparedSessionHistory(t *testing.T) {
	gate := &blockingToolPreflight{
		descriptor: preflightDescriptor("append-only"), scope: hook.ToolScope{All: true},
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	installed := &countingTool{definition: testToolDefinition(t, "effect")}
	access, store, stop := startRound7Application(t, preflightExecutor("effect", `{"value":"one"}`), AgentRuntimeConfig{ToolKeys: []string{"effect"}, MaxInlineToolResultBytes: 1024},
		toolModule{key: "effect", value: installed}, toolPreflightModule{gates: []hook.ToolPreflight{gate}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	done := make(chan interaction.RunResult, 1)
	errors := make(chan error, 1)
	go func() {
		result, err := access.SendAndWait(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("append only")})
		if err != nil {
			errors <- err
			return
		}
		done <- result
	}()
	waitForSignal(t, gate.entered, "ToolPreflight")
	prepared, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	original := append([]session.HistoryFact(nil), prepared.History...)
	close(gate.release)
	select {
	case err := <-errors:
		t.Fatal(err)
	case result := <-done:
		if result.Outcome != session.RunCompleted {
			t.Fatalf("result = %#v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not complete")
	}
	final, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(final.History) < len(original) || !reflect.DeepEqual(final.History[:len(original)], original) {
		t.Fatalf("ToolPreflight rewrote durable History:\nbefore=%#v\nafter=%#v", original, final.History)
	}
}

func TestNoToolPreflightKeepsParallelToolsAndCreatesNoExtensionState(t *testing.T) {
	release := make(chan struct{})
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	first := &preflightParallelTool{definition: testToolDefinition(t, "first"), started: firstStarted, release: release}
	second := &preflightParallelTool{definition: testToolDefinition(t, "second"), started: secondStarted, release: release}
	access, store, stop := startRound7Application(t, preflightBatchExecutor(
		model.ToolCallRequest{Name: "first", Arguments: []byte(`{"value":"one"}`)},
		model.ToolCallRequest{Name: "second", Arguments: []byte(`{"value":"two"}`)},
	), AgentRuntimeConfig{ToolKeys: []string{"first", "second"}, MaxInlineToolResultBytes: 1024},
		toolModule{key: "first", value: first}, toolModule{key: "second", value: second})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	done := make(chan error, 1)
	go func() {
		result, err := access.SendAndWait(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("parallel")})
		if err == nil && result.Outcome != session.RunCompleted {
			err = errors.New("run did not complete")
		}
		done <- err
	}()
	waitForSignal(t, firstStarted, "first parallel Tool")
	waitForSignal(t, secondStarted, "second parallel Tool")
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ExtensionJournal) != 0 {
		t.Fatalf("no-ToolPreflight path created extension state: %#v", snapshot.ExtensionJournal)
	}
}

func TestCancelWinsWhileToolPreflightIsRunningAndSettlesEveryReservation(t *testing.T) {
	gate := &blockingToolPreflight{
		descriptor: preflightDescriptor("cancel"), scope: hook.ToolScope{All: true},
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	installed := &countingTool{definition: testToolDefinition(t, "effect")}
	access, store, stop := startRound7Application(t, preflightExecutor("effect", `{"value":"one"}`),
		AgentRuntimeConfig{ToolKeys: []string{"effect"}, MaxInlineToolResultBytes: 1024},
		toolModule{key: "effect", value: installed}, toolPreflightModule{gates: []hook.ToolPreflight{gate}},
	)
	defer stop()
	opened := createRuntimeTestSession(t, access)
	done := make(chan interaction.RunResult, 1)
	errs := make(chan error, 1)
	go func() {
		result, err := access.SendAndWait(context.Background(), interaction.SendRequest{
			SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("cancel preflight"),
		})
		if err != nil {
			errs <- err
			return
		}
		done <- result
	}()
	waitForSignal(t, gate.entered, "ToolPreflight")
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
			t.Fatalf("cancel result = %#v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled preflight Run did not finish")
	}
	if installed.calls.Load() != 0 {
		t.Fatalf("Tool ran %d times after cancellation", installed.calls.Load())
	}
	final, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range final.ExtensionJournal {
		if entry.Boundary == hook.BoundaryToolPreflight &&
			(!entry.Status.Terminal() || entry.EffectDisposition == hook.EffectPending) {
			t.Fatalf("cancel left Preflight invocation unsettled: %#v", entry)
		}
	}
}

func TestPreparedToolPreflightReplaysExactlyOnceAfterRecovery(t *testing.T) {
	store, call, descriptor := seedToolPreflightRecoverySession(t, hook.InvocationPrepared)
	gate := &recordingToolPreflight{
		descriptor: descriptor, scope: hook.ToolScope{All: true},
		result: hook.ToolPreflightResult{Decision: hook.DecisionAllow},
	}
	installed := &countingTool{definition: testToolDefinition(t, call.Name)}
	executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{complete("continued")}})
	access, stop := startToolPreflightApplication(t, store, executor, AgentRuntimeConfig{ToolKeys: []string{call.Name}, MaxInlineToolResultBytes: 1024},
		toolModule{key: call.Name, value: installed}, toolPreflightModule{gates: []hook.ToolPreflight{gate}})
	defer stop()
	if _, err := access.ResumeSession(t.Context(), interaction.ResumeSessionRequest{SessionID: call.SessionID}); err != nil {
		t.Fatal(err)
	}
	if err := access.WhenIdle(t.Context(), interaction.WhenIdleRequest{SessionID: call.SessionID}); err != nil {
		t.Fatal(err)
	}
	if gate.callCount() != 1 || installed.calls.Load() != 1 {
		t.Fatalf("recovered prepared calls = preflight:%d tool:%d", gate.callCount(), installed.calls.Load())
	}
	final, err := store.Load(t.Context(), session.SessionRef{SessionID: call.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if final.ExtensionJournal[0].Status != hook.InvocationSucceeded || final.ExtensionJournal[0].EffectDisposition != hook.EffectApplied {
		t.Fatalf("recovered prepared journal = %#v", final.ExtensionJournal[0])
	}
}

func TestPendingToolPreflightBecomesUnknownAndNeverReplaysAfterRecovery(t *testing.T) {
	store, call, descriptor := seedToolPreflightRecoverySession(t, hook.InvocationPending)
	gate := &recordingToolPreflight{
		descriptor: descriptor, scope: hook.ToolScope{All: true},
		result: hook.ToolPreflightResult{Decision: hook.DecisionAllow},
	}
	installed := &countingTool{definition: testToolDefinition(t, call.Name)}
	access, stop := startToolPreflightApplication(t, store, model.NewFakeModelExecutor(), AgentRuntimeConfig{ToolKeys: []string{call.Name}, MaxInlineToolResultBytes: 1024},
		toolModule{key: call.Name, value: installed}, toolPreflightModule{gates: []hook.ToolPreflight{gate}})
	defer stop()
	if _, err := access.ResumeSession(t.Context(), interaction.ResumeSessionRequest{SessionID: call.SessionID}); err != nil {
		t.Fatal(err)
	}
	if err := access.WhenIdle(t.Context(), interaction.WhenIdleRequest{SessionID: call.SessionID}); err != nil {
		t.Fatal(err)
	}
	if gate.callCount() != 0 || installed.calls.Load() != 0 {
		t.Fatalf("unknown recovery replayed work = preflight:%d tool:%d", gate.callCount(), installed.calls.Load())
	}
	final, err := store.Load(t.Context(), session.SessionRef{SessionID: call.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	journal := final.ExtensionJournal[0]
	if journal.Status != hook.InvocationOutcomeUnknown || journal.EffectDisposition != hook.EffectApplied ||
		!lastRunHasTermination(final.History, session.TerminationExtension, "hook_outcome_unknown") {
		t.Fatalf("unknown recovery state = %#v / %#v", journal, final.History)
	}
}

func TestLegacyPreparedToolCallGetsOneCompletePreflightSetAfterRecovery(t *testing.T) {
	store, call, descriptor := seedToolPreflightRecoverySession(t, "")
	gate := &recordingToolPreflight{descriptor: descriptor, scope: hook.ToolScope{All: true}, result: hook.ToolPreflightResult{Decision: hook.DecisionAllow}}
	installed := &countingTool{definition: testToolDefinition(t, call.Name)}
	access, stop := startToolPreflightApplication(t, store, model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{complete("continued")}}),
		AgentRuntimeConfig{ToolKeys: []string{call.Name}, MaxInlineToolResultBytes: 1024},
		toolModule{key: call.Name, value: installed}, toolPreflightModule{gates: []hook.ToolPreflight{gate}})
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
	if gate.callCount() != 1 || installed.calls.Load() != 1 || len(final.ExtensionJournal) != 1 ||
		final.ExtensionJournal[0].Status != hook.InvocationSucceeded {
		t.Fatalf("legacy recovery = gate:%d tool:%d journal:%#v", gate.callCount(), installed.calls.Load(), final.ExtensionJournal)
	}
}

func TestPartialToolPreflightRecoveryFailsClosedAndSettlesReservations(t *testing.T) {
	store, call, descriptor := seedToolPreflightRecoverySession(t, hook.InvocationPrepared)
	first := &recordingToolPreflight{descriptor: descriptor, scope: hook.ToolScope{All: true}, result: hook.ToolPreflightResult{Decision: hook.DecisionAllow}}
	second := &recordingToolPreflight{descriptor: preflightDescriptor("new-definition"), scope: hook.ToolScope{All: true}, result: hook.ToolPreflightResult{Decision: hook.DecisionAllow}}
	installed := &countingTool{definition: testToolDefinition(t, call.Name)}
	access, stop := startToolPreflightApplication(t, store, model.NewFakeModelExecutor(),
		AgentRuntimeConfig{ToolKeys: []string{call.Name}, MaxInlineToolResultBytes: 1024},
		toolModule{key: call.Name, value: installed}, toolPreflightModule{gates: []hook.ToolPreflight{first, second}})
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
	if first.callCount() != 0 || second.callCount() != 0 || installed.calls.Load() != 0 || len(final.ExtensionJournal) != 1 ||
		final.ExtensionJournal[0].Status != hook.InvocationCanceled || !lastRunHasTermination(final.History, session.TerminationExtension, "tool_preflight_definition_mismatch") {
		t.Fatalf("partial recovery = gates:%d/%d tool:%d journal:%#v history:%#v", first.callCount(), second.callCount(), installed.calls.Load(), final.ExtensionJournal, final.History)
	}
}

func TestApplicationRejectsInvalidToolPreflightChainAndFreezesMetadata(t *testing.T) {
	for name, gates := range map[string][]hook.ToolPreflight{
		"typed nil": {(*recordingToolPreflight)(nil)},
		"duplicate": {
			&recordingToolPreflight{descriptor: preflightDescriptor("duplicate"), scope: hook.ToolScope{All: true}},
			&recordingToolPreflight{descriptor: preflightDescriptor("duplicate"), scope: hook.ToolScope{All: true}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			application := NewApplication(ApplicationSpec{
				Name: "invalid-tool-preflight", DefaultModelConfig: testDefaultModel(),
				Modules: []agentslot.Module{
					session.NewMemoryModule(), executorModule{executor: model.NewFakeModelExecutor()}, toolPreflightModule{gates: gates},
					NewGatewayChannelModule("entrypoint.invalid-tool-preflight", "test", &captureChannel{}),
				},
			})
			if _, err := application.Build(); err == nil {
				t.Fatal("invalid ToolPreflight chain was accepted")
			}
		})
	}

	originalDescriptor := preflightDescriptor("frozen-metadata")
	gate := &recordingToolPreflight{
		descriptor: originalDescriptor, scope: hook.ToolScope{ToolKeys: []string{"effect"}},
		result: hook.ToolPreflightResult{Decision: hook.DecisionDeny, Reason: "frozen deny"},
	}
	installed := &countingTool{definition: testToolDefinition(t, "effect")}
	access, store, stop := startRound7Application(t, preflightExecutor("effect", `{"value":"one"}`),
		AgentRuntimeConfig{ToolKeys: []string{"effect"}, MaxInlineToolResultBytes: 1024},
		toolModule{key: "effect", value: installed}, toolPreflightModule{gates: []hook.ToolPreflight{gate}})
	defer stop()
	gate.descriptor = preflightDescriptor("mutated")
	gate.scope.ToolKeys[0] = "different"
	opened := createRuntimeTestSession(t, access)
	if _, err := access.SendAndWait(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("frozen")}); err != nil {
		t.Fatal(err)
	}
	final, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if gate.callCount() != 1 || installed.calls.Load() != 0 || len(final.ExtensionJournal) != 1 || final.ExtensionJournal[0].Descriptor != originalDescriptor {
		t.Fatalf("frozen metadata = gate:%d tool:%d journal:%#v", gate.callCount(), installed.calls.Load(), final.ExtensionJournal)
	}
}

func preflightExecutor(toolKey string, arguments string) *model.FakeModelExecutor {
	return preflightBatchExecutor(model.ToolCallRequest{Name: toolKey, Arguments: []byte(arguments)})
}

func seedToolPreflightRecoverySession(t *testing.T, status hook.InvocationStatus) (*session.MemoryStore, agent.ToolCall, hook.ExtensionDescriptor) {
	t.Helper()
	store := session.NewMemoryStore()
	call, descriptor := seedToolPreflightRecoverySessionInStore(t, store, status)
	return store, call, descriptor
}

func seedToolPreflightRecoverySessionInStore(t *testing.T, store session.SessionStore, status hook.InvocationStatus) (agent.ToolCall, hook.ExtensionDescriptor) {
	t.Helper()
	config := testDefaultModel()
	assistant := agent.Message{
		ID: "message-preflight-recovery", SessionID: "session-preflight-recovery", RunID: "run-preflight-recovery", StepID: "step-preflight-recovery",
		Role: agent.RoleAssistant,
	}
	call := agent.ToolCall{
		ID: "call-preflight-recovery", MessageID: assistant.ID, SessionID: assistant.SessionID,
		RunID: assistant.RunID, StepID: assistant.StepID, Name: "effect", Arguments: []byte(`{"value":"original"}`),
	}
	descriptor := preflightDescriptor("recovery")
	started := session.RunFact{
		SessionID: call.SessionID, RunID: call.RunID, Kind: session.RunStarted,
		ModelConfig: config, ConfigRevision: 1,
	}
	preparedCall := session.JournalEntry{RunID: call.RunID, StepID: call.StepID, ToolCall: &call, Status: session.JournalPrepared}
	created, err := store.Create(t.Context(), session.NewSession{
		Session:     agent.Session{ID: call.SessionID, AgentID: "agent", WorkspaceID: "workspace"},
		History:     []session.HistoryFact{{Run: &started}, {Message: &assistant}, {ToolCall: &call}},
		RunJournal:  []session.JournalEntry{preparedCall},
		ModelConfig: config, RunState: session.RunRunning, ActiveRunID: call.RunID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if status == "" {
		return call, descriptor
	}
	view := hook.ToolPreflightView{
		InvocationID: "preflight-recovery-invocation", SessionID: call.SessionID, AgentID: "agent", WorkspaceID: "workspace",
		Revision: created.Revision.Next(), RunID: call.RunID, StepID: call.StepID, MessageID: call.MessageID, ToolCallID: call.ID, ToolKey: call.Name, Arguments: call.Arguments,
	}
	fingerprint, err := hook.FingerprintTypedInput(view)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	extension := session.ExtensionJournalEntry{
		InvocationID: view.InvocationID, Sequence: 1, Descriptor: descriptor, Boundary: hook.BoundaryToolPreflight,
		SessionID: call.SessionID, RunID: call.RunID, StepID: call.StepID, ToolCallID: call.ID,
		InputDigest: fingerprint.Digest, PreparedRevision: view.Revision, PreparedAt: now,
		Status: hook.InvocationPrepared, EffectDisposition: hook.EffectNone, ContextDisposition: hook.ContextNone,
	}
	preparedCommit, err := store.Commit(t.Context(), session.CommitRequest{
		SessionID: call.SessionID, ExpectedRevision: created.Revision, IdempotencyKey: "prepare-tool-preflight-recovery",
		Changes: []session.Change{{Kind: session.UpdateExtensionJournal, Extension: &extension}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if status == hook.InvocationPending {
		extension.Status, extension.PendingAt = hook.InvocationPending, now.Add(time.Millisecond)
		if _, err := store.Commit(t.Context(), session.CommitRequest{
			SessionID: call.SessionID, ExpectedRevision: preparedCommit.Revision, IdempotencyKey: "pend-tool-preflight-recovery",
			Changes: []session.Change{{Kind: session.UpdateExtensionJournal, Extension: &extension}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return call, descriptor
}

func preflightBatchExecutor(calls ...model.ToolCallRequest) *model.FakeModelExecutor {
	return model.NewFakeModelExecutor(
		model.FakeExecution{Events: []model.ModelEvent{{Kind: model.EventComplete, Output: &model.Completion{ToolCalls: calls}}}},
		model.FakeExecution{Events: []model.ModelEvent{complete("done")}},
	)
}

func preflightDescriptor(key string) hook.ExtensionDescriptor {
	return hook.ExtensionDescriptor{Key: "test." + key, DefinitionDigest: "sha256:" + strings.Repeat("b", 64)}
}

type recordingToolPreflight struct {
	mu         sync.Mutex
	descriptor hook.ExtensionDescriptor
	scope      hook.ToolScope
	result     hook.ToolPreflightResult
	err        error
	views      []hook.ToolPreflightView
}

type blockingToolPreflight struct {
	descriptor hook.ExtensionDescriptor
	scope      hook.ToolScope
	entered    chan struct{}
	release    chan struct{}
	once       sync.Once
}

type preflightParallelTool struct {
	definition tool.Definition
	started    chan struct{}
	release    <-chan struct{}
}

func (t *preflightParallelTool) Definition() tool.Definition       { return t.definition }
func (*preflightParallelTool) ParallelSafety() tool.ParallelSafety { return tool.ParallelSafe }
func (t *preflightParallelTool) Invoke(_ context.Context, invocation tool.ToolInvocation) tool.ToolResult {
	close(t.started)
	<-t.release
	return tool.ToolResult{CallID: invocation.Call.ID, Status: tool.ResultSucceeded}
}

func (g *blockingToolPreflight) Descriptor() hook.ExtensionDescriptor { return g.descriptor }
func (g *blockingToolPreflight) Scope() hook.ToolScope                { return g.scope }
func (g *blockingToolPreflight) Evaluate(ctx context.Context, _ hook.ToolPreflightView) (hook.ToolPreflightResult, error) {
	g.once.Do(func() { close(g.entered) })
	select {
	case <-g.release:
		return hook.ToolPreflightResult{Decision: hook.DecisionAllow}, nil
	case <-ctx.Done():
		return hook.ToolPreflightResult{}, ctx.Err()
	}
}

func (g *recordingToolPreflight) Descriptor() hook.ExtensionDescriptor { return g.descriptor }
func (g *recordingToolPreflight) Scope() hook.ToolScope                { return g.scope }
func (g *recordingToolPreflight) Evaluate(_ context.Context, view hook.ToolPreflightView) (hook.ToolPreflightResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.views = append(g.views, view)
	return g.result, g.err
}
func (g *recordingToolPreflight) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.views)
}
func (g *recordingToolPreflight) Views() []hook.ToolPreflightView {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]hook.ToolPreflightView(nil), g.views...)
}

type toolPreflightModule struct{ gates []hook.ToolPreflight }

func (toolPreflightModule) ID() string { return "test.tool-preflight" }
func (m toolPreflightModule) Register(reg agentslot.Registrar) error {
	contributions := make([]agentslot.Contribution, 0, len(m.gates))
	for _, gate := range m.gates {
		contributions = append(contributions, agentslot.Append(hook.ToolPreflightSlot, gate))
	}
	return reg.Contribute(contributions...)
}

type preflightPolicyModule struct {
	guard    policy.PolicyGuard
	approval policy.ApprovalService
}

func (preflightPolicyModule) ID() string { return "test.preflight-policy" }
func (m preflightPolicyModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Append(policy.GuardSlot, m.guard), agentslot.Set(policy.ApprovalSlot, m.approval))
}

type preflightCommitShape struct {
	revision          agent.Revision
	toolCalls         int
	runPrepared       int
	preflightPrepared int
}

type preflightRecordingStore struct {
	*session.MemoryStore
	mu         sync.Mutex
	commits    []preflightCommitShape
	operations []string
}

func newPreflightRecordingStore() *preflightRecordingStore {
	return &preflightRecordingStore{MemoryStore: session.NewMemoryStore()}
}

func (s *preflightRecordingStore) Commit(ctx context.Context, request session.CommitRequest) (session.Commit, error) {
	shape := preflightCommitShape{revision: request.ExpectedRevision.Next()}
	for _, change := range request.Changes {
		switch change.Kind {
		case session.AppendToolCall:
			shape.toolCalls++
		case session.UpdateRunJournal:
			if change.Journal.Status == session.JournalPrepared {
				shape.runPrepared++
			}
		case session.UpdateExtensionJournal:
			if change.Extension.Boundary == hook.BoundaryToolPreflight && change.Extension.Status == hook.InvocationPrepared {
				shape.preflightPrepared++
			}
		}
	}
	commit, err := s.MemoryStore.Commit(ctx, request)
	if err == nil {
		s.mu.Lock()
		s.operations = append(s.operations, request.IdempotencyKey)
		if shape.toolCalls > 0 {
			s.commits = append(s.commits, shape)
		}
		s.mu.Unlock()
	}
	return commit, err
}

func (s *preflightRecordingStore) toolPreparedCommits() []preflightCommitShape {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]preflightCommitShape(nil), s.commits...)
}

func (s *preflightRecordingStore) runtimeOperationCount(operation string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, key := range s.operations {
		if strings.Contains(key, "runtime-"+operation+"-") {
			count++
		}
	}
	return count
}

func startToolPreflightApplication(t *testing.T, store session.SessionStore, executor model.ModelExecutor, config AgentRuntimeConfig, extras ...agentslot.Module) (interaction.GatewayAccess, func()) {
	t.Helper()
	entry := &captureChannel{}
	modules := []agentslot.Module{componentsModule{store: store, executor: executor}}
	modules = append(modules, extras...)
	modules = append(modules, NewGatewayChannelModule("entrypoint.preflight", "preflight", entry))
	running, err := NewApplication(ApplicationSpec{
		Name: "preflight", Modules: modules, RuntimeConfig: config,
		DefaultModelConfig: model.Config{ModelID: "default", Reasoning: model.ReasoningDefault},
	}).Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return entry.Access(), func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := running.Stop(ctx); err != nil {
			t.Errorf("stop: %v", err)
		}
	}
}

func hasToolResultCode(history []session.HistoryFact, code string) bool {
	for _, fact := range history {
		if fact.ToolResult != nil && fact.ToolResult.Error != nil && fact.ToolResult.Error.Code == code {
			return true
		}
	}
	return false
}

func countExtensionStatus(entries []session.ExtensionJournalEntry, status hook.InvocationStatus) int {
	count := 0
	for _, entry := range entries {
		if entry.Status == status {
			count++
		}
	}
	return count
}

func lastRunHasTermination(history []session.HistoryFact, source session.TerminationSource, code string) bool {
	for index := len(history) - 1; index >= 0; index-- {
		fact := history[index]
		if fact.Run != nil && fact.Run.Kind != session.RunStarted {
			return fact.Run.Termination != nil && fact.Run.Termination.Source == source && string(fact.Run.Termination.Code) == code
		}
	}
	return false
}

var _ session.SessionStore = (*preflightRecordingStore)(nil)
