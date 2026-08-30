package standardagent

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/observe"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestExtensionDiagnosticsUseOneDetachedProjectionAcrossViewRunPageAndAudit(t *testing.T) {
	records := &observationRecords{changed: make(chan struct{}, 64)}
	lifecycle := &recordingSessionLifecycle{key: "observable-open", phases: []hook.LifecyclePhase{hook.LifecycleOpen}}
	completion := newCompletionGateSequence("observable-completion", hook.CompletionComplete)
	access, stop := startObservedApplication(t,
		model.NewFakeModelExecutor(model.FakeExecution{Events: []model.ModelEvent{complete("done")}}),
		records, AgentRuntimeConfig{},
		sessionLifecycleModule{lifecycles: []hook.SessionLifecycle{lifecycle}},
		completionGateModule{gates: []hook.CompletionGate{completion}},
	)
	opened := createRuntimeTestSession(t, access)
	result, err := access.SendAndWait(t.Context(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision,
		Actor: agent.ActorIdentity{Kind: agent.ActorLocalUser, ID: "user-1"}, Input: textInput("run"),
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := access.View(t.Context(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	page, err := access.ExtensionDiagnostics(t.Context(), interaction.ExtensionDiagnosticsRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	stop()

	if len(view.RecentExtensionDiagnostics) != 2 || view.HasMoreExtensionDiagnostics {
		t.Fatalf("SessionView diagnostics = %#v more=%t", view.RecentExtensionDiagnostics, view.HasMoreExtensionDiagnostics)
	}
	if !reflect.DeepEqual(view.RecentExtensionDiagnostics, page.Diagnostics) {
		t.Fatalf("View/page diagnostics drifted:\nview=%#v\npage=%#v", view.RecentExtensionDiagnostics, page.Diagnostics)
	}
	if len(result.ExtensionDiagnostics) != 1 || result.HasMoreExtensionDiagnostics ||
		result.ExtensionDiagnostics[0].RunID != result.RunID || result.ExtensionDiagnostics[0].Boundary != hook.BoundaryCompletion {
		t.Fatalf("RunResult diagnostics = %#v more=%t", result.ExtensionDiagnostics, result.HasMoreExtensionDiagnostics)
	}

	audits := records.auditsCopy()
	for _, expected := range page.Diagnostics {
		var latest *session.ExtensionDiagnostic
		for index := range audits {
			audit := audits[index]
			if audit.Kind == observe.AuditExtensionTransition && audit.Extension != nil && audit.Extension.InvocationID == expected.InvocationID {
				copy := *audit.Extension
				latest = &copy
			}
		}
		if latest == nil || !reflect.DeepEqual(*latest, expected) {
			t.Fatalf("latest Audit projection for %s = %#v, want %#v; audits=%#v", expected.InvocationID, latest, expected, audits)
		}
	}
	metrics := records.metricsCopy()
	assertLatestExtensionGauge(t, metrics, observe.MetricExtensionJournalEntries, 2)
	bytes := latestMetricValue(metrics, observe.MetricExtensionJournalBytes)
	if bytes <= 0 {
		t.Fatalf("extension journal byte gauge = %v; metrics=%#v", bytes, metrics)
	}
}

func assertLatestExtensionGauge(t *testing.T, records []observe.MetricRecord, name string, want float64) {
	t.Helper()
	if got := latestMetricValue(records, name); got != want {
		t.Fatalf("metric %s = %v, want %v; records=%#v", name, got, want, records)
	}
}

func latestMetricValue(records []observe.MetricRecord, name string) float64 {
	for index := len(records) - 1; index >= 0; index-- {
		if records[index].Name == name {
			return records[index].Value
		}
	}
	return -1
}

func TestSessionViewCapsRecentExtensionDiagnosticsAtThirtyTwo(t *testing.T) {
	access, store, stop := startRound7Application(t, model.NewFakeModelExecutor(), AgentRuntimeConfig{})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	snapshot, err := store.Load(context.Background(), session.SessionRef{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 40; index++ {
		entry := recentPreparedExtension(t, opened.SessionID, snapshot.Revision.Next(), session.ExtensionSequence(index+1), hook.InvocationID(fmt.Sprintf("recent-%d", index+1)))
		commit, err := store.Commit(t.Context(), session.CommitRequest{
			SessionID: opened.SessionID, ExpectedRevision: snapshot.Revision, IdempotencyKey: fmt.Sprintf("recent-diagnostic-%d", index+1),
			Changes: []session.Change{{Kind: session.UpdateExtensionJournal, Extension: &entry}},
		})
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Revision = commit.Revision
	}
	view, err := access.View(t.Context(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.RecentExtensionDiagnostics) != 32 || !view.HasMoreExtensionDiagnostics ||
		view.RecentExtensionDiagnostics[0].Sequence != 40 || view.RecentExtensionDiagnostics[31].Sequence != 9 {
		t.Fatalf("bounded SessionView diagnostics = %#v more=%t", view.RecentExtensionDiagnostics, view.HasMoreExtensionDiagnostics)
	}
}

func TestRunDiagnosticsAreNewestFirstBoundedAndExcludeOtherRuns(t *testing.T) {
	entries := make([]session.ExtensionJournalEntry, 0, interaction.MaxRunExtensionDiagnostics+8)
	for index := 1; index <= interaction.MaxRunExtensionDiagnostics+7; index++ {
		entries = append(entries, session.ExtensionJournalEntry{
			InvocationID: hook.InvocationID(fmt.Sprintf("run-entry-%d", index)), Sequence: session.ExtensionSequence(index),
			RunID: "run-target", Status: hook.InvocationSucceeded,
		})
		if index == 50 {
			entries = append(entries, session.ExtensionJournalEntry{
				InvocationID: "other-run-entry", Sequence: 1000, RunID: "run-other", Status: hook.InvocationFailed,
			})
		}
	}
	diagnostics, hasMore := diagnosticsForRun(entries, "run-target", interaction.MaxRunExtensionDiagnostics)
	if len(diagnostics) != interaction.MaxRunExtensionDiagnostics || !hasMore ||
		diagnostics[0].InvocationID != "run-entry-107" || diagnostics[len(diagnostics)-1].InvocationID != "run-entry-8" {
		t.Fatalf("bounded Run diagnostics = %#v more=%t", diagnostics, hasMore)
	}
	for _, diagnostic := range diagnostics {
		if diagnostic.RunID != "run-target" {
			t.Fatalf("Run diagnostics included another Run: %#v", diagnostic)
		}
	}
}

func recentPreparedExtension(t *testing.T, sessionID agent.SessionID, revision agent.Revision, sequence session.ExtensionSequence, invocationID hook.InvocationID) session.ExtensionJournalEntry {
	t.Helper()
	view := hook.SessionLifecycleView{
		InvocationID: invocationID, SessionID: sessionID, AgentID: "agent-1", WorkspaceID: "workspace-1",
		Revision: revision, Phase: hook.LifecycleOpen, OpenKind: hook.OpenResume,
	}
	fingerprint, err := hook.FingerprintTypedInput(view)
	if err != nil {
		t.Fatal(err)
	}
	return session.ExtensionJournalEntry{
		InvocationID: invocationID, Sequence: sequence,
		Descriptor: hook.ExtensionDescriptor{Key: "recent.fixture", DefinitionDigest: "sha256:" + strings.Repeat("a", 64)},
		Boundary:   hook.BoundarySessionLifecycle, SessionID: sessionID, LifecyclePhase: hook.LifecycleOpen, LifecycleOpenKind: hook.OpenResume,
		InputDigest: fingerprint.Digest, PreparedRevision: revision, PreparedAt: time.Now().UTC(), Status: hook.InvocationPrepared,
		EffectDisposition: hook.EffectNone, ContextDisposition: hook.ContextNone,
	}
}
