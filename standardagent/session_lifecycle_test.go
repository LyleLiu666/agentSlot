package standardagent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestSessionLifecycleOpenContextIsAppendOnlyAndConsumedExactlyOnce(t *testing.T) {
	executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{complete("done")}})
	lifecycle := &recordingSessionLifecycle{key: "context", phases: []hook.LifecyclePhase{hook.LifecycleOpen}}
	lifecycle.evaluate = func(view hook.SessionLifecycleView) (hook.SessionLifecycleResult, error) {
		message := &agent.Message{
			ID: "session-start-context", SessionID: view.SessionID, Role: agent.RoleUser,
			Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "session start context"}},
		}
		return hook.SessionLifecycleResult{Context: []model.Input{{Message: message}}}, nil
	}
	access, store, stop := startRound7Application(t, executor, AgentRuntimeConfig{}, sessionLifecycleModule{lifecycles: []hook.SessionLifecycle{lifecycle}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	before, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(before.History) != 0 || len(before.ExtensionJournal) != 1 || before.ExtensionJournal[0].ContextDisposition != hook.ContextPending {
		t.Fatalf("open snapshot = history:%#v journal:%#v", before.History, before.ExtensionJournal)
	}
	result, err := access.SendAndWait(t.Context(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("user request"),
	})
	if err != nil || result.Outcome != session.RunCompleted {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	requests := executor.Requests()
	if len(requests) != 1 || countInputText(requests[0].Inputs, "session start context") != 1 || countInputText(requests[0].Inputs, "user request") != 1 {
		t.Fatalf("model requests = %#v", requests)
	}
	after, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if !historyPrefixEqual(before.History, after.History[:len(before.History)]) {
		t.Fatalf("SessionLifecycle rewrote append-only History: before=%#v after=%#v", before.History, after.History)
	}
	if got := lifecycleContextFacts(after.History); len(got) != 1 || got[0].RunID != result.RunID || !got[0].StepID.Valid() {
		t.Fatalf("lifecycle context facts = %#v", got)
	}
	if after.ExtensionJournal[0].ContextDisposition != hook.ContextConsumed || len(after.ExtensionJournal[0].ContextInputs) != 0 {
		t.Fatalf("lifecycle context was not consumed exactly once: %#v", after.ExtensionJournal[0])
	}
}

func TestSessionLifecycleOpenUsesNPlusTwoCommitsAndNoHookKeepsFastPath(t *testing.T) {
	t.Run("no hook", func(t *testing.T) {
		access, store, stop := startRound7Application(t, model.NewFakeModelExecutor(), AgentRuntimeConfig{})
		defer stop()
		opened := createRuntimeTestSession(t, access)
		snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
		if err != nil {
			t.Fatal(err)
		}
		if opened.Revision != 1 || snapshot.Revision != opened.Revision || len(snapshot.ExtensionJournal) != 0 {
			t.Fatalf("no-hook open added persistence work: opened=%#v snapshot=%#v", opened, snapshot)
		}
	})

	t.Run("two hooks", func(t *testing.T) {
		first := &recordingSessionLifecycle{key: "first", phases: []hook.LifecyclePhase{hook.LifecycleOpen}}
		second := &recordingSessionLifecycle{key: "second", phases: []hook.LifecyclePhase{hook.LifecycleOpen}}
		access, store, stop := startRound7Application(t, model.NewFakeModelExecutor(), AgentRuntimeConfig{}, sessionLifecycleModule{lifecycles: []hook.SessionLifecycle{first, second}})
		defer stop()
		opened := createRuntimeTestSession(t, access)
		snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := opened.Revision, agent.Revision(1+2+2); got != want { // initial revision + (N+2)
			t.Fatalf("open revision = %d, want %d", got, want)
		}
		if snapshot.Revision != opened.Revision || len(snapshot.ExtensionJournal) != 2 || first.calls() != 1 || second.calls() != 1 {
			t.Fatalf("open chain snapshot=%#v calls=%d/%d", snapshot, first.calls(), second.calls())
		}
	})
}

func TestSessionLifecycleContextDoesNotRepeatAcrossLaterSteps(t *testing.T) {
	executor := newRound7Executor(nil, nil,
		model.FakeExecution{Events: []model.ModelEvent{complete("first")}},
		model.FakeExecution{Events: []model.ModelEvent{complete("second")}},
	)
	lifecycle := &recordingSessionLifecycle{key: "one-step", phases: []hook.LifecyclePhase{hook.LifecycleOpen}}
	lifecycle.evaluate = func(view hook.SessionLifecycleView) (hook.SessionLifecycleResult, error) {
		message := &agent.Message{ID: "one-step-context", SessionID: view.SessionID, Role: agent.RoleUser, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "start only once"}}}
		return hook.SessionLifecycleResult{Context: []model.Input{{Message: message}}}, nil
	}
	completion := newCompletionGateSequence("lifecycle-follow-on", hook.CompletionContinue, hook.CompletionComplete)
	access, _, stop := startRound7Application(t, executor, AgentRuntimeConfig{},
		sessionLifecycleModule{lifecycles: []hook.SessionLifecycle{lifecycle}}, completionGateModule{gates: []hook.CompletionGate{completion}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.SendAndWait(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("run")}); err != nil {
		t.Fatal(err)
	}
	requests := executor.fake.Requests()
	if len(requests) != 2 || countInputText(requests[0].Inputs, "start only once") != 1 || countInputText(requests[1].Inputs, "start only once") != 0 {
		t.Fatalf("SessionStart context leaked across Steps: %#v", requests)
	}
}

func TestSessionLifecycleOpeningBarrierPrecedesEveryGatewayOperation(t *testing.T) {
	entered := make(chan hook.SessionLifecycleView, 1)
	release := make(chan struct{})
	lifecycle := &recordingSessionLifecycle{key: "barrier", phases: []hook.LifecyclePhase{hook.LifecycleOpen}}
	lifecycle.evaluate = func(view hook.SessionLifecycleView) (hook.SessionLifecycleResult, error) {
		entered <- view
		<-release
		return hook.SessionLifecycleResult{}, nil
	}
	access, _, stop := startRound7Application(t, model.NewFakeModelExecutor(), AgentRuntimeConfig{}, sessionLifecycleModule{lifecycles: []hook.SessionLifecycle{lifecycle}})
	defer stop()
	openedDone := make(chan interaction.SessionOpened, 1)
	errDone := make(chan error, 1)
	go func() {
		opened, err := access.CreateSession(context.Background(), interaction.CreateSessionRequest{AgentID: "agent-1", WorkspaceID: "workspace-1"})
		openedDone <- opened
		errDone <- err
	}()
	view := <-entered
	viewDone := make(chan error, 1)
	go func() {
		_, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: view.SessionID})
		viewDone <- err
	}()
	select {
	case err := <-viewDone:
		t.Fatalf("View crossed opening barrier: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-errDone; err != nil {
		t.Fatal(err)
	}
	opened := <-openedDone
	if opened.SessionID != view.SessionID {
		t.Fatalf("opened=%#v view=%#v", opened, view)
	}
	if err := <-viewDone; err != nil {
		t.Fatal(err)
	}
}

func TestSessionLifecycleReceiptsSurfaceProjectionStoreFailures(t *testing.T) {
	t.Run("open", func(t *testing.T) {
		projectionErr := errors.New("open diagnostics unavailable")
		store := extensionDiagnosticsFailureStore{SessionStore: session.NewMemoryStore(), err: projectionErr}
		lifecycle := &recordingSessionLifecycle{key: "open-projection", phases: []hook.LifecyclePhase{hook.LifecycleOpen}}
		access, stop := startToolPreflightApplication(t, store, model.NewFakeModelExecutor(), AgentRuntimeConfig{}, sessionLifecycleModule{lifecycles: []hook.SessionLifecycle{lifecycle}})
		defer stop()

		_, err := access.CreateSession(t.Context(), interaction.CreateSessionRequest{AgentID: "agent-1", WorkspaceID: "workspace-1"})
		if !errors.Is(err, projectionErr) {
			t.Fatalf("CreateSession error = %v, want diagnostics Store failure", err)
		}
	})

	t.Run("close", func(t *testing.T) {
		projectionErr := errors.New("close diagnostics unavailable")
		store := extensionDiagnosticsFailureStore{SessionStore: session.NewMemoryStore(), err: projectionErr}
		lifecycle := &recordingSessionLifecycle{key: "close-projection", phases: []hook.LifecyclePhase{hook.LifecycleClose}}
		access, stop := startToolPreflightApplication(t, store, model.NewFakeModelExecutor(), AgentRuntimeConfig{}, sessionLifecycleModule{lifecycles: []hook.SessionLifecycle{lifecycle}})
		defer stop()
		opened := createRuntimeTestSession(t, access)

		_, err := access.CloseSession(t.Context(), interaction.CloseSessionRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision})
		if !errors.Is(err, projectionErr) {
			t.Fatalf("CloseSession error = %v, want diagnostics Store failure", err)
		}
	})
}

func TestSessionLifecycleExplicitCloseReturnsReceiptAndShutdownDoesNotForgeEnd(t *testing.T) {
	lifecycle := &recordingSessionLifecycle{key: "end", phases: []hook.LifecyclePhase{hook.LifecycleOpen, hook.LifecycleClose}}
	lifecycle.evaluate = func(view hook.SessionLifecycleView) (hook.SessionLifecycleResult, error) {
		if view.Phase == hook.LifecycleClose {
			return hook.SessionLifecycleResult{}, &hook.InvocationFailure{Status: hook.InvocationFailed, Code: "end_failed", Reason: "end component failed"}
		}
		return hook.SessionLifecycleResult{}, nil
	}
	access, _, stop := startRound7Application(t, model.NewFakeModelExecutor(), AgentRuntimeConfig{}, sessionLifecycleModule{lifecycles: []hook.SessionLifecycle{lifecycle}})
	opened := createRuntimeTestSession(t, access)
	receipt, err := access.CloseSession(t.Context(), interaction.CloseSessionRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision})
	if err != nil {
		t.Fatalf("safe close was disguised as failure: %v", err)
	}
	if receipt.SessionID != opened.SessionID || receipt.Revision <= opened.Revision || len(receipt.Diagnostics) != 1 || receipt.Diagnostics[0].Status != hook.InvocationFailed {
		t.Fatalf("close receipt = %#v", receipt)
	}
	if got := lifecycle.phaseCalls(hook.LifecycleClose); got != 1 {
		t.Fatalf("explicit close calls = %d", got)
	}
	stop()

	shutdownLifecycle := &recordingSessionLifecycle{key: "shutdown", phases: []hook.LifecyclePhase{hook.LifecycleOpen, hook.LifecycleClose}}
	access, _, shutdown := startRound7Application(t, model.NewFakeModelExecutor(), AgentRuntimeConfig{}, sessionLifecycleModule{lifecycles: []hook.SessionLifecycle{shutdownLifecycle}})
	_ = createRuntimeTestSession(t, access)
	shutdown()
	if got := shutdownLifecycle.phaseCalls(hook.LifecycleClose); got != 0 {
		t.Fatalf("application shutdown forged %d SessionEnd calls", got)
	}
}

func TestSessionLifecycleOpenFailureIsAuditableButDoesNotBlockSafeOpen(t *testing.T) {
	lifecycle := &recordingSessionLifecycle{key: "failed-open", phases: []hook.LifecyclePhase{hook.LifecycleOpen}}
	lifecycle.evaluate = func(hook.SessionLifecycleView) (hook.SessionLifecycleResult, error) {
		return hook.SessionLifecycleResult{}, errors.New("private component detail")
	}
	access, _, stop := startRound7Application(t, model.NewFakeModelExecutor(), AgentRuntimeConfig{}, sessionLifecycleModule{lifecycles: []hook.SessionLifecycle{lifecycle}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if len(opened.Diagnostics) != 1 || opened.Diagnostics[0].Status != hook.InvocationFailed || opened.Diagnostics[0].ErrorCode != agent.CodeExtensionFailed || strings.Contains(opened.Diagnostics[0].ErrorReason, "private") {
		t.Fatalf("open diagnostics = %#v", opened.Diagnostics)
	}
}

func TestSessionLifecycleChainIsValidatedAndFrozenAtBuild(t *testing.T) {
	invalidScope := &recordingSessionLifecycle{key: "invalid-scope", phases: nil}
	for name, lifecycles := range map[string][]hook.SessionLifecycle{
		"typed nil":   {(*recordingSessionLifecycle)(nil)},
		"duplicate":   {&recordingSessionLifecycle{key: "duplicate", phases: []hook.LifecyclePhase{hook.LifecycleOpen}}, &recordingSessionLifecycle{key: "duplicate", phases: []hook.LifecyclePhase{hook.LifecycleClose}}},
		"empty scope": {invalidScope},
	} {
		t.Run(name, func(t *testing.T) {
			app := NewApplication(ApplicationSpec{
				Name: "invalid-session-lifecycle", DefaultModelConfig: testDefaultModel(),
				Modules: []agentslot.Module{
					session.NewMemoryModule(), executorModule{executor: model.NewFakeModelExecutor()}, sessionLifecycleModule{lifecycles: lifecycles},
					NewGatewayChannelModule("entrypoint.invalid-session-lifecycle", "invalid-session-lifecycle", &captureChannel{}),
				},
			})
			if _, err := app.Build(); err == nil {
				t.Fatal("invalid SessionLifecycle chain was accepted")
			}
		})
	}
}

func TestSessionLifecycleDistinguishesOpenKindsAndForkDoesNotCopyParentJournal(t *testing.T) {
	lifecycle := &recordingSessionLifecycle{key: "kinds", phases: []hook.LifecyclePhase{hook.LifecycleOpen}}
	access, store, stop := startRound7Application(t, model.NewFakeModelExecutor(), AgentRuntimeConfig{}, sessionLifecycleModule{lifecycles: []hook.SessionLifecycle{lifecycle}})
	defer stop()
	created := createRuntimeTestSession(t, access)
	forked, err := access.ForkSession(t.Context(), interaction.ForkSessionRequest{
		SourceSessionID: created.SessionID, Mode: session.ForkFullHistory,
		AgentID: "agent-1", WorkspaceID: "workspace-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	summarized, err := access.StartSessionFromSummary(t.Context(), interaction.SummarySessionRequest{
		SourceSessionID: created.SessionID, AgentID: "agent-1", WorkspaceID: "workspace-1", Messages: []agent.MessageInput{textInput("summary")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := access.CloseSession(t.Context(), interaction.CloseSessionRequest{SessionID: created.SessionID, ExpectedRevision: created.Revision}); err != nil {
		t.Fatal(err)
	}
	resumed, err := access.ResumeSession(t.Context(), interaction.ResumeSessionRequest{SessionID: created.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	views := lifecycle.snapshotViews()
	wantKinds := []hook.OpenKind{hook.OpenCreate, hook.OpenFork, hook.OpenSummary, hook.OpenResume}
	if len(views) != len(wantKinds) {
		t.Fatalf("lifecycle views = %#v", views)
	}
	for index, want := range wantKinds {
		if views[index].OpenKind != want || views[index].Phase != hook.LifecycleOpen {
			t.Fatalf("view %d = %#v, want kind %q", index, views[index], want)
		}
	}
	for _, opened := range []interaction.SessionOpened{forked, summarized} {
		snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.ExtensionJournal) != 1 || snapshot.ExtensionJournal[0].SessionID != opened.SessionID {
			t.Fatalf("child copied parent ExtensionJournal: %#v", snapshot.ExtensionJournal)
		}
	}
	if resumed.Revision <= created.Revision {
		t.Fatalf("resume did not durably record its own open: create=%d resume=%d", created.Revision, resumed.Revision)
	}
}

func TestSessionLifecycleCloseCancelsAndSettlesPromptHooksBeforeEnd(t *testing.T) {
	input := &cancelBlockingInputGate{entered: make(chan struct{}), exited: make(chan struct{})}
	endEntered := make(chan struct{})
	lifecycle := &recordingSessionLifecycle{key: "ordered-end", phases: []hook.LifecyclePhase{hook.LifecycleClose}}
	lifecycle.evaluate = func(hook.SessionLifecycleView) (hook.SessionLifecycleResult, error) {
		select {
		case <-input.exited:
		default:
			return hook.SessionLifecycleResult{}, errors.New("SessionEnd overtook Prompt hook settlement")
		}
		close(endEntered)
		return hook.SessionLifecycleResult{}, nil
	}
	access, store, stop := startRound7Application(t, model.NewFakeModelExecutor(), AgentRuntimeConfig{},
		inputGateModule{gates: []hook.InputGate{input}}, sessionLifecycleModule{lifecycles: []hook.SessionLifecycle{lifecycle}})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	sendDone := make(chan error, 1)
	go func() {
		_, err := access.Send(context.Background(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("blocked")})
		sendDone <- err
	}()
	<-input.entered
	snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := access.CloseSession(t.Context(), interaction.CloseSessionRequest{SessionID: opened.SessionID, ExpectedRevision: snapshot.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-sendDone; err == nil {
		t.Fatal("canceled Prompt hook unexpectedly accepted input")
	}
	select {
	case <-endEntered:
	default:
		t.Fatal("SessionEnd was not evaluated")
	}
	final, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if final.Revision != receipt.Revision {
		t.Fatalf("receipt revision=%d final=%d", receipt.Revision, final.Revision)
	}
	for _, entry := range final.ExtensionJournal {
		if entry.Status == hook.InvocationPrepared || entry.Status == hook.InvocationPending || entry.EffectDisposition == hook.EffectPending {
			t.Fatalf("close left unsettled extension entry: %#v", entry)
		}
	}
}

func TestSessionLifecycleCloseSettlesActiveRunBeforeEnd(t *testing.T) {
	blocked := make(chan struct{})
	executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{complete("unused")}, Block: blocked})
	var store *session.MemoryStore
	lifecycle := &recordingSessionLifecycle{key: "run-before-end", phases: []hook.LifecyclePhase{hook.LifecycleClose}}
	lifecycle.evaluate = func(view hook.SessionLifecycleView) (hook.SessionLifecycleResult, error) {
		snapshot, err := store.Load(context.Background(), session.SessionRef{SessionID: view.SessionID})
		if err != nil {
			return hook.SessionLifecycleResult{}, err
		}
		if snapshot.RunState != session.RunIdle || !hasTerminalRunFact(snapshot.History) {
			return hook.SessionLifecycleResult{}, errors.New("SessionEnd overtook active Run settlement")
		}
		return hook.SessionLifecycleResult{}, nil
	}
	access, runtimeStore, stop := startRound7Application(t, executor, AgentRuntimeConfig{}, sessionLifecycleModule{lifecycles: []hook.SessionLifecycle{lifecycle}})
	store = runtimeStore
	defer stop()
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(t.Context(), interaction.SendRequest{SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Input: textInput("run")}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil || snapshot.RunState != session.RunRunning {
		t.Fatalf("active snapshot=%#v err=%v", snapshot, err)
	}
	receipt, err := access.CloseSession(t.Context(), interaction.CloseSessionRequest{SessionID: opened.SessionID, ExpectedRevision: snapshot.Revision})
	if err != nil || len(receipt.Diagnostics) != 1 || receipt.Diagnostics[0].Status != hook.InvocationSucceeded {
		t.Fatalf("close receipt=%#v err=%v", receipt, err)
	}
}

func hasTerminalRunFact(history []session.HistoryFact) bool {
	for _, fact := range history {
		if fact.Run != nil && fact.Run.Kind != session.RunStarted {
			return true
		}
	}
	return false
}

func TestSessionLifecycleResumeSupersedesUnconsumedContextFromSameComponent(t *testing.T) {
	store := session.NewMemoryStore()
	lifecycle := &recordingSessionLifecycle{key: "bounded-resume", phases: []hook.LifecyclePhase{hook.LifecycleOpen}}
	lifecycle.evaluate = func(view hook.SessionLifecycleView) (hook.SessionLifecycleResult, error) {
		text := "create context"
		if view.OpenKind == hook.OpenResume {
			text = "resume context"
		}
		message := &agent.Message{ID: agent.MessageID("context-" + string(view.OpenKind)), SessionID: view.SessionID, Role: agent.RoleUser, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: text}}}
		return hook.SessionLifecycleResult{Context: []model.Input{{Message: message}}}, nil
	}
	first, stopFirst := startToolPreflightApplication(t, store, model.NewFakeModelExecutor(), AgentRuntimeConfig{}, sessionLifecycleModule{lifecycles: []hook.SessionLifecycle{lifecycle}})
	created := createRuntimeTestSession(t, first)
	stopFirst()

	executor := model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{complete("done")}})
	second, stopSecond := startToolPreflightApplication(t, store, executor, AgentRuntimeConfig{}, sessionLifecycleModule{lifecycles: []hook.SessionLifecycle{lifecycle}})
	defer stopSecond()
	resumed, err := second.ResumeSession(t.Context(), interaction.ResumeSessionRequest{SessionID: created.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.SendAndWait(t.Context(), interaction.SendRequest{SessionID: resumed.SessionID, ExpectedRevision: resumed.Revision, Input: textInput("run")}); err != nil {
		t.Fatal(err)
	}
	requests := executor.Requests()
	if len(requests) != 1 || countInputText(requests[0].Inputs, "resume context") != 1 || countInputText(requests[0].Inputs, "create context") != 0 {
		t.Fatalf("resume contexts = %#v", requests)
	}
	snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: created.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ExtensionJournal) != 2 || snapshot.ExtensionJournal[0].ContextDisposition != hook.ContextDiscarded || snapshot.ExtensionJournal[1].ContextDisposition != hook.ContextConsumed {
		t.Fatalf("superseded lifecycle context = %#v", snapshot.ExtensionJournal)
	}
}

func TestSessionLifecycleRecoveryDiscardsPartialChainWithoutReplay(t *testing.T) {
	store := session.NewMemoryStore()
	firstAccess, stopFirst := startToolPreflightApplication(t, store, model.NewFakeModelExecutor(), AgentRuntimeConfig{})
	opened := createRuntimeTestSession(t, firstAccess)
	stopFirst()
	snapshot, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	lifecycles := []hook.SessionLifecycle{
		&recordingSessionLifecycle{key: "recover-first", phases: []hook.LifecyclePhase{hook.LifecycleOpen}},
		&recordingSessionLifecycle{key: "recover-second", phases: []hook.LifecyclePhase{hook.LifecycleOpen}},
		&recordingSessionLifecycle{key: "recover-third", phases: []hook.LifecyclePhase{hook.LifecycleOpen}},
	}
	prepared := make([]session.ExtensionJournalEntry, len(lifecycles))
	changes := make([]session.Change, len(lifecycles))
	preparedRevision := snapshot.Revision.Next()
	for index, lifecycle := range lifecycles {
		view := hook.SessionLifecycleView{
			InvocationID: hook.InvocationID("old-lifecycle-" + lifecycle.Descriptor().Key), SessionID: opened.SessionID,
			AgentID: "agent-1", WorkspaceID: "workspace-1", Revision: preparedRevision, Phase: hook.LifecycleOpen, OpenKind: hook.OpenResume,
		}
		fingerprint, err := hook.FingerprintTypedInput(view)
		if err != nil {
			t.Fatal(err)
		}
		prepared[index] = session.ExtensionJournalEntry{
			InvocationID: view.InvocationID, Sequence: session.ExtensionSequence(index + 1), Descriptor: lifecycle.Descriptor(),
			Boundary: hook.BoundarySessionLifecycle, SessionID: opened.SessionID, LifecyclePhase: hook.LifecycleOpen, LifecycleOpenKind: hook.OpenResume,
			InputDigest: fingerprint.Digest, PreparedRevision: preparedRevision, PreparedAt: time.Now().UTC(),
			Status: hook.InvocationPrepared, EffectDisposition: hook.EffectNone, ContextDisposition: hook.ContextNone,
		}
		entry := prepared[index]
		changes[index] = session.Change{Kind: session.UpdateExtensionJournal, Extension: &entry}
	}
	commit, err := store.Commit(t.Context(), session.CommitRequest{SessionID: opened.SessionID, ExpectedRevision: snapshot.Revision, IdempotencyKey: "old-lifecycle-prepare", Changes: changes})
	if err != nil {
		t.Fatal(err)
	}
	prepared[0].Status, prepared[0].PendingAt = hook.InvocationPending, prepared[0].PreparedAt.Add(time.Second)
	commit, err = store.Commit(t.Context(), session.CommitRequest{SessionID: opened.SessionID, ExpectedRevision: commit.Revision, IdempotencyKey: "old-lifecycle-first-pending", Changes: []session.Change{{Kind: session.UpdateExtensionJournal, Extension: &prepared[0]}}})
	if err != nil {
		t.Fatal(err)
	}
	contextInput := []model.Input{{Message: &agent.Message{ID: "old-lifecycle-context", SessionID: opened.SessionID, Role: agent.RoleUser, Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "must not leak"}}}}}
	contextFingerprint, err := hook.FingerprintTypedInput(contextInput)
	if err != nil {
		t.Fatal(err)
	}
	prepared[0].Status, prepared[0].FinishedAt, prepared[0].EffectDisposition = hook.InvocationSucceeded, prepared[0].PendingAt.Add(time.Second), hook.EffectPending
	prepared[0].Result = &hook.InvocationResult{Decision: hook.DecisionNone}
	prepared[0].ContextDisposition, prepared[0].ContextInputs = hook.ContextPending, contextInput
	prepared[0].ContextDigest, prepared[0].ContextBytes = contextFingerprint.Digest, contextFingerprint.Bytes
	prepared[1].Status, prepared[1].PendingAt = hook.InvocationPending, prepared[1].PreparedAt.Add(2*time.Second)
	_, err = store.Commit(t.Context(), session.CommitRequest{
		SessionID: opened.SessionID, ExpectedRevision: commit.Revision, IdempotencyKey: "old-lifecycle-pipeline",
		Changes: []session.Change{{Kind: session.UpdateExtensionJournal, Extension: &prepared[0]}, {Kind: session.UpdateExtensionJournal, Extension: &prepared[1]}},
	})
	if err != nil {
		t.Fatal(err)
	}

	secondAccess, stopSecond := startToolPreflightApplication(t, store, model.NewFakeModelExecutor(), AgentRuntimeConfig{}, sessionLifecycleModule{lifecycles: lifecycles})
	defer stopSecond()
	if _, err := secondAccess.ResumeSession(t.Context(), interaction.ResumeSessionRequest{SessionID: opened.SessionID}); err != nil {
		t.Fatal(err)
	}
	final, err := store.Load(t.Context(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(final.ExtensionJournal) != 6 {
		t.Fatalf("recovery journal = %#v", final.ExtensionJournal)
	}
	old := final.ExtensionJournal[:3]
	if old[0].Status != hook.InvocationSucceeded || old[0].EffectDisposition != hook.EffectDiscarded || old[0].ContextDisposition != hook.ContextDiscarded ||
		old[1].Status != hook.InvocationOutcomeUnknown || old[1].EffectDisposition != hook.EffectDiscarded ||
		old[2].Status != hook.InvocationCanceled || old[2].EffectDisposition != hook.EffectDiscarded {
		t.Fatalf("partial lifecycle recovery = %#v", old)
	}
	for _, lifecycle := range lifecycles {
		if lifecycle.(*recordingSessionLifecycle).calls() != 1 {
			t.Fatalf("recovery replayed old invocation for %s", lifecycle.Descriptor().Key)
		}
	}
}

func lifecycleContextFacts(history []session.HistoryFact) []session.ContextContributionFact {
	var result []session.ContextContributionFact
	for _, fact := range history {
		if fact.ContextContribution != nil && strings.HasPrefix(fact.ContextContribution.SourceKey, "hook:test.context:") {
			result = append(result, *fact.ContextContribution)
		}
	}
	return result
}

func historyPrefixEqual(left, right []session.HistoryFact) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Sequence != right[index].Sequence || left[index].Kind != right[index].Kind {
			return false
		}
	}
	return true
}

type sessionLifecycleModule struct{ lifecycles []hook.SessionLifecycle }

func (sessionLifecycleModule) ID() string { return "test.session-lifecycle" }
func (m sessionLifecycleModule) Register(reg agentslot.Registrar) error {
	contributions := make([]agentslot.Contribution, 0, len(m.lifecycles))
	for _, lifecycle := range m.lifecycles {
		contributions = append(contributions, agentslot.Append(hook.SessionLifecycleSlot, lifecycle))
	}
	return reg.Contribute(contributions...)
}

type recordingSessionLifecycle struct {
	mu       sync.Mutex
	key      string
	phases   []hook.LifecyclePhase
	evaluate func(hook.SessionLifecycleView) (hook.SessionLifecycleResult, error)
	views    []hook.SessionLifecycleView
}

type cancelBlockingInputGate struct {
	entered chan struct{}
	exited  chan struct{}
}

func (*cancelBlockingInputGate) Descriptor() hook.ExtensionDescriptor {
	return hook.ExtensionDescriptor{Key: "test.cancel-blocking-input", DefinitionDigest: "sha256:" + strings.Repeat("b", 64)}
}
func (g *cancelBlockingInputGate) Evaluate(ctx context.Context, _ hook.InputGateView) (hook.InputGateResult, error) {
	close(g.entered)
	<-ctx.Done()
	close(g.exited)
	return hook.InputGateResult{}, ctx.Err()
}

func (l *recordingSessionLifecycle) Descriptor() hook.ExtensionDescriptor {
	return hook.ExtensionDescriptor{Key: "test." + l.key, DefinitionDigest: "sha256:" + strings.Repeat("a", 64)}
}
func (l *recordingSessionLifecycle) Scope() hook.LifecycleScope {
	return hook.LifecycleScope{Phases: append([]hook.LifecyclePhase(nil), l.phases...)}
}
func (l *recordingSessionLifecycle) Evaluate(_ context.Context, view hook.SessionLifecycleView) (hook.SessionLifecycleResult, error) {
	l.mu.Lock()
	l.views = append(l.views, view)
	evaluate := l.evaluate
	l.mu.Unlock()
	if evaluate != nil {
		return evaluate(view)
	}
	return hook.SessionLifecycleResult{}, nil
}
func (l *recordingSessionLifecycle) calls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.views)
}
func (l *recordingSessionLifecycle) phaseCalls(phase hook.LifecyclePhase) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	for _, view := range l.views {
		if view.Phase == phase {
			count++
		}
	}
	return count
}

func (l *recordingSessionLifecycle) snapshotViews() []hook.SessionLifecycleView {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]hook.SessionLifecycleView(nil), l.views...)
}
