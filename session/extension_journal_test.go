package session_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestExtensionJournalTransitionsKeepIdentityAndDispositionsIndependent(t *testing.T) {
	store, created := newExtensionSession(t, session.NewMemoryStore(), "extension-transitions")
	prepared := preparedExtensionEntry(t, created.Session.ID, 1, "invocation-1")
	committed := commitExtension(t, store, created, "prepare", prepared)

	for name, invalidEntry := range map[string]session.ExtensionJournalEntry{
		"skip pending before success": extensionSucceeded(prepared),
		"change identity":             extensionPending(withExtensionKey(prepared, "changed-key")),
		"change lifecycle phase": func() session.ExtensionJournalEntry {
			entry := extensionPending(prepared)
			entry.LifecyclePhase, entry.LifecycleOpenKind = hook.LifecycleClose, ""
			return entry
		}(),
		"change open kind": func() session.ExtensionJournalEntry {
			entry := extensionPending(prepared)
			entry.LifecycleOpenKind = hook.OpenFork
			return entry
		}(),
		"skip sequence":  preparedExtensionEntry(t, created.Session.ID, 3, "invocation-3"),
		"reuse sequence": preparedExtensionEntry(t, created.Session.ID, 1, "invocation-2"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := store.Commit(context.Background(), session.CommitRequest{
				SessionID: created.Session.ID, ExpectedRevision: committed.Revision,
				IdempotencyKey: "invalid-" + name,
				Changes:        []session.Change{{Kind: session.UpdateExtensionJournal, Extension: &invalidEntry}},
			})
			if !agent.IsKind(err, agent.ErrorConflict) {
				t.Fatalf("invalid transition error = %v, kind=%q", err, agent.KindOf(err))
			}
		})
	}

	pending := extensionPending(prepared)
	committed = commitExtension(t, store, committedSnapshot(t, store, created.Session.ID, committed.Revision), "pending", pending)
	for name, invalidEntry := range map[string]session.ExtensionJournalEntry{
		"change pending timestamp": func() session.ExtensionJournalEntry {
			entry := extensionSucceeded(pending)
			entry.PendingAt = entry.PendingAt.Add(time.Second)
			return entry
		}(),
		"apply effect with terminal outcome": func() session.ExtensionJournalEntry {
			entry := extensionSucceeded(pending)
			entry.EffectDisposition = hook.EffectApplied
			return entry
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := store.Commit(context.Background(), session.CommitRequest{
				SessionID: created.Session.ID, ExpectedRevision: committed.Revision,
				IdempotencyKey: "invalid-terminal-" + name,
				Changes:        []session.Change{{Kind: session.UpdateExtensionJournal, Extension: &invalidEntry}},
			})
			if !agent.IsKind(err, agent.ErrorConflict) {
				t.Fatalf("invalid terminal transition error = %v, kind=%q", err, agent.KindOf(err))
			}
		})
	}
	succeeded := extensionSucceeded(pending)
	committed = commitExtension(t, store, committedSnapshot(t, store, created.Session.ID, committed.Revision), "succeeded", succeeded)

	loaded, err := store.Load(context.Background(), session.SessionRef{SessionID: created.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	entry := loaded.ExtensionJournal[0]
	if entry.Status != hook.InvocationSucceeded || entry.EffectDisposition != hook.EffectPending || entry.ContextDisposition != hook.ContextNone {
		t.Fatalf("terminal entry = %#v", entry)
	}

	applied := entry
	applied.EffectDisposition = hook.EffectApplied
	commitExtension(t, store, loaded, "apply-effect", applied)
	if _, err := store.Commit(context.Background(), session.CommitRequest{
		SessionID: created.Session.ID, ExpectedRevision: loaded.Revision.Next(), IdempotencyKey: "apply-effect-twice",
		Changes: []session.Change{{Kind: session.UpdateExtensionJournal, Extension: &applied}},
	}); !agent.IsKind(err, agent.ErrorConflict) {
		t.Fatalf("duplicate effect application error = %v, kind=%q", err, agent.KindOf(err))
	}
}

func TestToolResultExtensionPersistsAndProjectsItsDistinctContextTargetStep(t *testing.T) {
	store, created := newExtensionSession(t, session.NewMemoryStore(), "tool-result-target")
	entry := preparedExtensionEntry(t, created.Session.ID, 1, "post-invocation")
	entry.Boundary = hook.BoundaryToolResult
	entry.RunID, entry.StepID, entry.TargetStepID = "run-1", "step-source", "step-target"
	entry.MessageID, entry.ToolCallID, entry.PreparedRevision = "message-1", "call-1", created.Revision.Next()
	commitExtension(t, store, created, "prepare-post", entry)
	page, err := store.ExtensionDiagnostics(t.Context(), session.ExtensionPageRequest{SessionID: created.Session.ID, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Diagnostics) != 1 || page.Diagnostics[0].StepID != "step-source" || page.Diagnostics[0].TargetStepID != "step-target" {
		t.Fatalf("tool result diagnostic = %#v", page.Diagnostics)
	}
	entry.InvocationID, entry.Sequence, entry.TargetStepID = "post-invalid", 2, entry.StepID
	_, err = store.Commit(t.Context(), session.CommitRequest{
		SessionID: created.Session.ID, ExpectedRevision: page.Revision, IdempotencyKey: "invalid-post-target",
		Changes: []session.Change{{Kind: session.UpdateExtensionJournal, Extension: &entry}},
	})
	if err == nil {
		t.Fatal("tool result extension accepted its source Step as context target")
	}
}

func TestExtensionJournalAcceptsOnlyTheDeclaredLegalStatusPaths(t *testing.T) {
	paths := []struct {
		name        string
		pending     bool
		terminal    hook.InvocationStatus
		firstEffect hook.EffectDisposition
	}{
		{name: "prepared to failed", terminal: hook.InvocationFailed, firstEffect: hook.EffectPending},
		{name: "prepared to canceled", terminal: hook.InvocationCanceled, firstEffect: hook.EffectDiscarded},
		{name: "pending to succeeded", pending: true, terminal: hook.InvocationSucceeded, firstEffect: hook.EffectPending},
		{name: "pending to failed", pending: true, terminal: hook.InvocationFailed, firstEffect: hook.EffectPending},
		{name: "pending to canceled", pending: true, terminal: hook.InvocationCanceled, firstEffect: hook.EffectPending},
		{name: "pending to outcome unknown", pending: true, terminal: hook.InvocationOutcomeUnknown, firstEffect: hook.EffectPending},
	}
	for index, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			store, created := newExtensionSession(t, session.NewMemoryStore(), agent.SessionID("legal-path-"+time.Unix(int64(index+1), 0).UTC().Format("150405")))
			prepared := preparedExtensionEntry(t, created.Session.ID, 1, hook.InvocationID("legal-invocation-"+time.Unix(int64(index+1), 0).UTC().Format("150405")))
			commit := commitExtension(t, store, created, "legal-prepare", prepared)
			current := prepared
			if path.pending {
				current = extensionPending(prepared)
				commit = commitExtension(t, store, committedSnapshot(t, store, created.Session.ID, commit.Revision), "legal-pending", current)
			}
			terminal := current
			terminal.Status = path.terminal
			terminal.FinishedAt = current.PreparedAt.Add(3 * time.Second)
			terminal.EffectDisposition = path.firstEffect
			if path.terminal == hook.InvocationSucceeded {
				terminal.Result = &hook.InvocationResult{Decision: hook.DecisionNone}
			} else {
				terminal.ErrorCode = "hook_canceled"
				terminal.ErrorReason = "extension did not produce a usable result"
			}
			commit = commitExtension(t, store, committedSnapshot(t, store, created.Session.ID, commit.Revision), "legal-terminal", terminal)
			if path.firstEffect == hook.EffectPending {
				terminal.EffectDisposition = hook.EffectApplied
				commitExtension(t, store, committedSnapshot(t, store, created.Session.ID, commit.Revision), "legal-effect", terminal)
			}
		})
	}
}

func TestExtensionJournalRejectsCrossBoundaryDecisionsAndOversizedContext(t *testing.T) {
	base := preparedExtensionEntry(t, "extension-result-validation", 1, "result-validation")
	base = extensionPending(base)
	base = extensionSucceeded(base)
	base.Result.Decision = hook.DecisionAllow
	if err := base.Validate(base.SessionID); err == nil {
		t.Fatal("lifecycle entry accepted a ToolPreflight decision")
	}

	base.Result.Decision = hook.DecisionNone
	inputs := make([]model.Input, hook.MaxContextInputs+1)
	for index := range inputs {
		message := agent.Message{
			ID:        agent.MessageID("context-message-" + time.Unix(int64(index+1), 0).UTC().Format("150405")),
			SessionID: base.SessionID, Role: agent.RoleUser,
			Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "bounded"}},
		}
		inputs[index] = model.Input{Message: &message}
	}
	fingerprint, err := hook.FingerprintTypedInput(inputs)
	if err != nil {
		t.Fatal(err)
	}
	base.ContextDisposition = hook.ContextPending
	base.ContextInputs = inputs
	base.ContextDigest = fingerprint.Digest
	base.ContextBytes = fingerprint.Bytes
	if err := base.Validate(base.SessionID); err == nil {
		t.Fatal("extension context exceeded the framework input-count limit")
	}

	largeMessage := agent.Message{
		ID: "large-context-message", SessionID: base.SessionID, Role: agent.RoleUser,
		Parts: []agent.MessagePart{{Kind: agent.PartText, Text: strings.Repeat("x", hook.MaxContextBytes)}},
	}
	base.ContextInputs = []model.Input{{Message: &largeMessage}}
	fingerprint, err = hook.FingerprintTypedInput(base.ContextInputs)
	if err != nil {
		t.Fatal(err)
	}
	base.ContextDigest = fingerprint.Digest
	base.ContextBytes = fingerprint.Bytes
	if err := base.Validate(base.SessionID); err == nil {
		t.Fatal("extension context exceeded the framework byte limit")
	}
}

func TestExtensionJournalContextIsClearedAfterConsumptionButKeepsAuditDigest(t *testing.T) {
	store, created := newExtensionSession(t, session.NewMemoryStore(), "extension-context")
	prepared := preparedExtensionEntry(t, created.Session.ID, 1, "invocation-context")
	commit := commitExtension(t, store, created, "prepare", prepared)
	pending := extensionPending(prepared)
	commit = commitExtension(t, store, committedSnapshot(t, store, created.Session.ID, commit.Revision), "pending", pending)

	message := agent.Message{
		ID: "extension-context-message", SessionID: created.Session.ID, Role: agent.RoleUser,
		Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "additional context"}},
	}
	inputs := []model.Input{{Message: &message}}
	fingerprint, err := hook.FingerprintTypedInput(inputs)
	if err != nil {
		t.Fatal(err)
	}
	succeeded := extensionSucceeded(pending)
	succeeded.ContextDisposition = hook.ContextPending
	succeeded.ContextInputs = inputs
	succeeded.ContextDigest = fingerprint.Digest
	succeeded.ContextBytes = fingerprint.Bytes
	commit = commitExtension(t, store, committedSnapshot(t, store, created.Session.ID, commit.Revision), "succeeded", succeeded)

	loaded := committedSnapshot(t, store, created.Session.ID, commit.Revision)
	consumed := loaded.ExtensionJournal[0]
	consumed.EffectDisposition = hook.EffectApplied
	consumed.ContextDisposition = hook.ContextConsumed
	consumed.ContextInputs = nil
	commitExtension(t, store, loaded, "consume-context", consumed)

	final, err := store.Load(context.Background(), session.SessionRef{SessionID: created.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	entry := final.ExtensionJournal[0]
	if len(entry.ContextInputs) != 0 || entry.ContextDigest != fingerprint.Digest || entry.ContextBytes != fingerprint.Bytes || entry.ContextDisposition != hook.ContextConsumed {
		t.Fatalf("consumed entry = %#v", entry)
	}
}

func TestExtensionJournalCanApplyAnEffectBeforeItsContextIsClaimed(t *testing.T) {
	store, created := newExtensionSession(t, session.NewMemoryStore(), "extension-independent-context")
	prepared := preparedExtensionEntry(t, created.Session.ID, 1, "invocation-independent-context")
	commit := commitExtension(t, store, created, "prepare", prepared)
	pending := extensionPending(prepared)
	commit = commitExtension(t, store, committedSnapshot(t, store, created.Session.ID, commit.Revision), "pending", pending)

	message := agent.Message{
		ID: "independent-context-message", SessionID: created.Session.ID, Role: agent.RoleUser,
		Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "consume only after queue claim"}},
	}
	succeeded := extensionSucceeded(pending)
	succeeded.ContextDisposition = hook.ContextPending
	succeeded.ContextInputs = []model.Input{{Message: &message}}
	fingerprint, err := hook.FingerprintTypedInput(succeeded.ContextInputs)
	if err != nil {
		t.Fatal(err)
	}
	succeeded.ContextDigest, succeeded.ContextBytes = fingerprint.Digest, fingerprint.Bytes
	commit = commitExtension(t, store, committedSnapshot(t, store, created.Session.ID, commit.Revision), "succeeded", succeeded)

	applied := succeeded
	applied.EffectDisposition = hook.EffectApplied
	commit = commitExtension(t, store, committedSnapshot(t, store, created.Session.ID, commit.Revision), "apply-input", applied)

	consumed := applied
	consumed.ContextDisposition = hook.ContextConsumed
	consumed.ContextInputs = nil
	commitExtension(t, store, committedSnapshot(t, store, created.Session.ID, commit.Revision), "consume-context", consumed)

	final, err := store.Load(context.Background(), session.SessionRef{SessionID: created.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	entry := final.ExtensionJournal[0]
	if entry.EffectDisposition != hook.EffectApplied || entry.ContextDisposition != hook.ContextConsumed || len(entry.ContextInputs) != 0 {
		t.Fatalf("independently finalized entry = %#v", entry)
	}
}

func TestExtensionJournalNeverRewritesHistoryOrTurnsContextIntoMessages(t *testing.T) {
	store := session.NewMemoryStore()
	message := agent.Message{
		ID: "existing-user-message", SessionID: "extension-append-only", Role: agent.RoleUser,
		Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "immutable original"}},
	}
	created, err := store.Create(context.Background(), session.NewSession{
		Session: agent.Session{ID: message.SessionID, AgentID: "agent-1", WorkspaceID: "workspace-1"},
		History: []session.HistoryFact{{Message: &message}}, ModelConfig: defaultConfig(), RunState: session.RunIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	originalHistory := created.History
	prepared := preparedExtensionEntry(t, created.Session.ID, 1, "invocation-append-only")
	commit := commitExtension(t, store, created, "append-only-prepare", prepared)
	pending := extensionPending(prepared)
	commit = commitExtension(t, store, committedSnapshot(t, store, created.Session.ID, commit.Revision), "append-only-pending", pending)
	succeeded := extensionSucceeded(pending)
	contextMessage := agent.Message{
		ID: "pending-context-only", SessionID: created.Session.ID, Role: agent.RoleUser,
		Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "must not become History"}},
	}
	succeeded.ContextInputs = []model.Input{{Message: &contextMessage}}
	fingerprint, err := hook.FingerprintTypedInput(succeeded.ContextInputs)
	if err != nil {
		t.Fatal(err)
	}
	succeeded.ContextDisposition = hook.ContextPending
	succeeded.ContextDigest = fingerprint.Digest
	succeeded.ContextBytes = fingerprint.Bytes
	commit = commitExtension(t, store, committedSnapshot(t, store, created.Session.ID, commit.Revision), "append-only-succeeded", succeeded)

	loaded := committedSnapshot(t, store, created.Session.ID, commit.Revision)
	if !reflect.DeepEqual(loaded.History, originalHistory) {
		t.Fatalf("ExtensionJournal rewrote History:\nbefore=%#v\nafter=%#v", originalHistory, loaded.History)
	}
	if loaded.Context.Version != 0 || len(loaded.Context.Request.Inputs) != 0 {
		t.Fatalf("pending extension context escaped into model Context: %#v", loaded.Context)
	}
}

func TestExtensionJournalTerminalAndEffectCanShareOneAtomicCommit(t *testing.T) {
	store, created := newExtensionSession(t, session.NewMemoryStore(), "extension-atomic-effect")
	prepared := preparedExtensionEntry(t, created.Session.ID, 1, "invocation-atomic-effect")
	commit := commitExtension(t, store, created, "atomic-prepare", prepared)
	pending := extensionPending(prepared)
	commit = commitExtension(t, store, committedSnapshot(t, store, created.Session.ID, commit.Revision), "atomic-pending", pending)

	succeeded := extensionSucceeded(pending)
	applied := succeeded
	applied.EffectDisposition = hook.EffectApplied
	before := committedSnapshot(t, store, created.Session.ID, commit.Revision)
	terminalCommit, err := store.Commit(context.Background(), session.CommitRequest{
		SessionID: before.Session.ID, ExpectedRevision: before.Revision, IdempotencyKey: "atomic-terminal-effect",
		Changes: []session.Change{
			{Kind: session.UpdateExtensionJournal, Extension: &succeeded},
			{Kind: session.UpdateExtensionJournal, Extension: &applied},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	after := committedSnapshot(t, store, created.Session.ID, terminalCommit.Revision)
	if terminalCommit.Revision != before.Revision.Next() || after.ExtensionJournal[0].EffectDisposition != hook.EffectApplied {
		t.Fatalf("atomic terminal commit = %#v, entry = %#v", terminalCommit, after.ExtensionJournal[0])
	}
	if !reflect.DeepEqual(after.History, before.History) {
		t.Fatalf("atomic effect commit rewrote History: before=%#v after=%#v", before.History, after.History)
	}
}

func TestExtensionJournalMemoryAndFileStoresHaveDetachedParity(t *testing.T) {
	memory := session.NewMemoryStore()
	directory := t.TempDir()
	file := openFileStore(t, directory)
	t.Cleanup(func() { _ = file.Close(context.Background()) })

	var snapshots []session.Snapshot
	for index, store := range []session.SessionStore{memory, file} {
		created, err := store.Create(context.Background(), session.NewSession{
			Session:     agent.Session{ID: "extension-parity", AgentID: "agent-1", WorkspaceID: "workspace-1"},
			ModelConfig: defaultConfig(), RunState: session.RunIdle,
		})
		if err != nil {
			t.Fatalf("store %d create: %v", index, err)
		}
		prepared := preparedExtensionEntry(t, created.Session.ID, 1, "invocation-parity")
		commit := commitExtension(t, store, created, "prepare", prepared)
		pending := extensionPending(prepared)
		commit = commitExtension(t, store, committedSnapshot(t, store, created.Session.ID, commit.Revision), "pending", pending)
		failed := pending
		failed.Status = hook.InvocationFailed
		failed.FinishedAt = pending.PendingAt.Add(time.Second)
		failed.ErrorCode = "hook_exit_nonzero"
		failed.ErrorReason = "hook process exited unsuccessfully"
		failed.EffectDisposition = hook.EffectPending
		commit = commitExtension(t, store, committedSnapshot(t, store, created.Session.ID, commit.Revision), "failed", failed)
		loaded := committedSnapshot(t, store, created.Session.ID, commit.Revision)
		snapshots = append(snapshots, loaded)

		loaded.ExtensionJournal[0].Descriptor.Key = "mutated-by-caller"
		again, err := store.Load(context.Background(), session.SessionRef{SessionID: created.Session.ID})
		if err != nil {
			t.Fatal(err)
		}
		if again.ExtensionJournal[0].Descriptor.Key == "mutated-by-caller" {
			t.Fatal("caller mutation contaminated Store state")
		}
	}
	if !reflect.DeepEqual(snapshots[0].ExtensionJournal, snapshots[1].ExtensionJournal) {
		t.Fatalf("memory/file journal mismatch:\nmemory=%#v\nfile=%#v", snapshots[0].ExtensionJournal, snapshots[1].ExtensionJournal)
	}
}

func TestExtensionRecoveryNeverReplaysPendingAndIsIdempotent(t *testing.T) {
	file := openFileStore(t, t.TempDir())
	t.Cleanup(func() { _ = file.Close(context.Background()) })
	for index, store := range []session.SessionStore{session.NewMemoryStore(), file} {
		created, err := store.Create(context.Background(), session.NewSession{
			Session:     agent.Session{ID: agent.SessionID("extension-recovery-" + time.Unix(int64(index+1), 0).UTC().Format("150405")), AgentID: "agent-1", WorkspaceID: "workspace-1"},
			ModelConfig: defaultConfig(), RunState: session.RunIdle,
		})
		if err != nil {
			t.Fatal(err)
		}
		prepared := preparedExtensionEntry(t, created.Session.ID, 1, "recovery-pending")
		commit := commitExtension(t, store, created, "recovery-prepare", prepared)
		pending := extensionPending(prepared)
		commit = commitExtension(t, store, committedSnapshot(t, store, created.Session.ID, commit.Revision), "recovery-pending", pending)

		recovered, err := store.Recover(context.Background(), session.SessionRef{SessionID: created.Session.ID})
		if err != nil {
			t.Fatal(err)
		}
		entry := recovered.ExtensionJournal[0]
		if recovered.Revision != commit.Revision.Next() || entry.Status != hook.InvocationOutcomeUnknown || entry.EffectDisposition != hook.EffectPending || entry.ErrorCode != "hook_outcome_unknown" {
			t.Fatalf("store %d recovered entry = %#v at revision %d", index, entry, recovered.Revision)
		}
		again, err := store.Recover(context.Background(), session.SessionRef{SessionID: created.Session.ID})
		if err != nil {
			t.Fatal(err)
		}
		if again.Revision != recovered.Revision || !reflect.DeepEqual(again.ExtensionJournal, recovered.ExtensionJournal) {
			t.Fatalf("store %d repeated recovery changed terminal entry", index)
		}
	}
}

func TestCompletionRecoveryKeepsTheActiveRunUntilRuntimeSettlesTheGate(t *testing.T) {
	file := openFileStore(t, t.TempDir())
	t.Cleanup(func() { _ = file.Close(context.Background()) })
	for storeIndex, store := range []session.SessionStore{session.NewMemoryStore(), file} {
		for _, status := range []hook.InvocationStatus{hook.InvocationPrepared, hook.InvocationPending} {
			sessionID := agent.SessionID(fmt.Sprintf("completion-store-recovery-%d-%s", storeIndex, status))
			runID, stepID, targetStep := agent.RunID("run-completion"), agent.StepID("step-source"), agent.StepID("step-target")
			started := session.RunFact{SessionID: sessionID, RunID: runID, Kind: session.RunStarted, ModelConfig: defaultConfig(), ConfigRevision: 1}
			assistant := agent.Message{
				ID: "message-completion", SessionID: sessionID, RunID: runID, StepID: stepID, Role: agent.RoleAssistant,
				Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "candidate"}},
			}
			created, err := store.Create(context.Background(), session.NewSession{
				Session: agent.Session{ID: sessionID, AgentID: "agent-1", WorkspaceID: "workspace-1"},
				History: []session.HistoryFact{{Run: &started}, {Message: &assistant}}, ModelConfig: defaultConfig(),
				RunState: session.RunRunning, ActiveRunID: runID,
			})
			if err != nil {
				t.Fatal(err)
			}
			view := hook.CompletionView{
				InvocationID: "completion-invocation", SessionID: sessionID, AgentID: "agent-1", WorkspaceID: "workspace-1",
				Revision: created.Revision.Next(), RunID: runID, StepID: stepID, NextStepID: targetStep, LastAssistantMessage: assistant,
			}
			fingerprint, err := hook.FingerprintTypedInput(view)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			entry := session.ExtensionJournalEntry{
				InvocationID: view.InvocationID, Sequence: 1, Descriptor: hook.ExtensionDescriptor{Key: "completion.fixture", DefinitionDigest: "sha256:" + strings.Repeat("a", 64)},
				Boundary: hook.BoundaryCompletion, SessionID: sessionID, RunID: runID, StepID: stepID, TargetStepID: targetStep, MessageID: assistant.ID,
				InputDigest: fingerprint.Digest, PreparedRevision: view.Revision, PreparedAt: now,
				Status: hook.InvocationPrepared, EffectDisposition: hook.EffectNone, ContextDisposition: hook.ContextNone,
			}
			commit := commitExtension(t, store, created, "completion-prepare-"+string(status), entry)
			if status == hook.InvocationPending {
				entry.Status, entry.PendingAt = hook.InvocationPending, now.Add(time.Millisecond)
				commit = commitExtension(t, store, committedSnapshot(t, store, sessionID, commit.Revision), "completion-pending", entry)
			}
			recovered, err := store.Recover(context.Background(), session.SessionRef{SessionID: sessionID})
			if err != nil {
				t.Fatal(err)
			}
			wantStatus := status
			if status == hook.InvocationPending {
				wantStatus = hook.InvocationOutcomeUnknown
			}
			if recovered.RunState != session.RunRunning || recovered.ActiveRunID != runID || recovered.ExtensionJournal[0].Status != wantStatus {
				t.Fatalf("store %d status %s recovery = state:%s/%s journal:%#v", storeIndex, status, recovered.RunState, recovered.ActiveRunID, recovered.ExtensionJournal)
			}
		}
	}
}

func TestExtensionDiagnosticsAreBoundedNewestFirstAndPayloadFree(t *testing.T) {
	store, created := newExtensionSession(t, session.NewMemoryStore(), "extension-diagnostics")
	current := created
	for sequence := 1; sequence <= 105; sequence++ {
		entry := preparedExtensionEntry(t, created.Session.ID, session.ExtensionSequence(sequence), hook.InvocationID("invocation-"+strings.Repeat("x", sequence%3)+time.Unix(int64(sequence), 0).UTC().Format("150405")))
		commit := commitExtension(t, store, current, "prepare-diagnostic-"+time.Unix(int64(sequence), 0).UTC().Format("150405"), entry)
		current = committedSnapshot(t, store, created.Session.ID, commit.Revision)
	}

	page, err := store.ExtensionDiagnostics(context.Background(), session.ExtensionPageRequest{SessionID: created.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Diagnostics) != session.DefaultExtensionPageLimit || !page.HasMore || page.Diagnostics[0].Sequence != 105 || page.Diagnostics[len(page.Diagnostics)-1].Sequence != 56 {
		t.Fatalf("first page = len %d, first %d, last %d, more %t", len(page.Diagnostics), page.Diagnostics[0].Sequence, page.Diagnostics[len(page.Diagnostics)-1].Sequence, page.HasMore)
	}
	older, err := store.ExtensionDiagnostics(context.Background(), session.ExtensionPageRequest{
		SessionID: created.Session.ID, BeforeExtensionSequence: page.Diagnostics[len(page.Diagnostics)-1].Sequence, Limit: session.MaxExtensionPageLimit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(older.Diagnostics) != 55 || older.HasMore || older.Diagnostics[0].Sequence != 55 || older.Diagnostics[len(older.Diagnostics)-1].Sequence != 1 {
		t.Fatalf("older page = %#v", older)
	}
	if _, err := store.ExtensionDiagnostics(context.Background(), session.ExtensionPageRequest{SessionID: created.Session.ID, Limit: session.MaxExtensionPageLimit + 1}); !agent.IsKind(err, agent.ErrorInvalidInput) {
		t.Fatalf("oversized page error = %v", err)
	}

	typeOfDiagnostic := reflect.TypeOf(session.ExtensionDiagnostic{})
	for _, forbidden := range []string{"ContextInputs", "Payload", "Environment", "Argv", "Stdin", "Stdout", "Stderr"} {
		if _, exists := typeOfDiagnostic.FieldByName(forbidden); exists {
			t.Fatalf("diagnostic leaks forbidden field %q", forbidden)
		}
	}
}

func newExtensionSession(t *testing.T, store session.SessionStore, id agent.SessionID) (session.SessionStore, session.Snapshot) {
	t.Helper()
	created, err := store.Create(context.Background(), session.NewSession{
		Session:     agent.Session{ID: id, AgentID: "agent-1", WorkspaceID: "workspace-1"},
		ModelConfig: defaultConfig(), RunState: session.RunIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, created
}

func preparedExtensionEntry(t *testing.T, sessionID agent.SessionID, sequence session.ExtensionSequence, invocationID hook.InvocationID) session.ExtensionJournalEntry {
	t.Helper()
	fingerprint, err := hook.FingerprintTypedInput(struct {
		SessionID agent.SessionID `json:"session_id"`
		Value     string          `json:"value"`
	}{SessionID: sessionID, Value: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	return session.ExtensionJournalEntry{
		InvocationID: invocationID,
		Sequence:     sequence,
		Descriptor: hook.ExtensionDescriptor{
			Key: "profile.fixture", DefinitionDigest: "sha256:" + strings.Repeat("a", 64),
		},
		Boundary:           hook.BoundarySessionLifecycle,
		SessionID:          sessionID,
		LifecyclePhase:     hook.LifecycleOpen,
		LifecycleOpenKind:  hook.OpenResume,
		InputDigest:        fingerprint.Digest,
		PreparedRevision:   agent.Revision(sequence + 1),
		PreparedAt:         time.Date(2026, time.August, 30, 10, 0, int(sequence), 0, time.UTC),
		Status:             hook.InvocationPrepared,
		EffectDisposition:  hook.EffectNone,
		ContextDisposition: hook.ContextNone,
	}
}

func extensionPending(entry session.ExtensionJournalEntry) session.ExtensionJournalEntry {
	entry.Status = hook.InvocationPending
	entry.PendingAt = entry.PreparedAt.Add(time.Second)
	return entry
}

func extensionSucceeded(entry session.ExtensionJournalEntry) session.ExtensionJournalEntry {
	entry.Status = hook.InvocationSucceeded
	entry.FinishedAt = entry.PreparedAt.Add(2 * time.Second)
	entry.Result = &hook.InvocationResult{Decision: hook.DecisionNone, Reason: "fixture completed"}
	entry.EffectDisposition = hook.EffectPending
	return entry
}

func withExtensionKey(entry session.ExtensionJournalEntry, key string) session.ExtensionJournalEntry {
	entry.Descriptor.Key = key
	return entry
}

func commitExtension(t *testing.T, store session.SessionStore, current session.Snapshot, key string, entry session.ExtensionJournalEntry) session.Commit {
	t.Helper()
	commit, err := store.Commit(context.Background(), session.CommitRequest{
		SessionID: current.Session.ID, ExpectedRevision: current.Revision, IdempotencyKey: key,
		Changes: []session.Change{{Kind: session.UpdateExtensionJournal, Extension: &entry}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return commit
}

func committedSnapshot(t *testing.T, store session.SessionStore, id agent.SessionID, wantRevision agent.Revision) session.Snapshot {
	t.Helper()
	loaded, err := store.Load(context.Background(), session.SessionRef{SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != wantRevision {
		t.Fatalf("loaded revision = %d, want %d", loaded.Revision, wantRevision)
	}
	return loaded
}
