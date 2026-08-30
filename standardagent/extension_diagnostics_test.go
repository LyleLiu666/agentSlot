package standardagent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestGatewayProjectsBoundedExtensionDiagnosticsWithoutPayload(t *testing.T) {
	access, store, stop := startRound7Application(t, model.NewFakeModelExecutor(), AgentRuntimeConfig{})
	defer stop()
	opened := createRuntimeTestSession(t, access)
	fingerprint, err := hook.FingerprintTypedInput(struct {
		SessionID string `json:"session_id"`
	}{SessionID: string(opened.SessionID)})
	if err != nil {
		t.Fatal(err)
	}
	entry := session.ExtensionJournalEntry{
		InvocationID: "gateway-diagnostic", Sequence: 1,
		Descriptor: hook.ExtensionDescriptor{Key: "diagnostic.fixture", DefinitionDigest: "sha256:" + strings.Repeat("a", 64)},
		Boundary:   hook.BoundarySessionLifecycle, SessionID: opened.SessionID, InputDigest: fingerprint.Digest,
		PreparedAt: time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC), Status: hook.InvocationPrepared,
		EffectDisposition: hook.EffectNone, ContextDisposition: hook.ContextNone,
	}
	committed, err := store.Commit(context.Background(), session.CommitRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, IdempotencyKey: "gateway-diagnostic",
		Changes: []session.Change{{Kind: session.UpdateExtensionJournal, Extension: &entry}},
	})
	if err != nil {
		t.Fatal(err)
	}

	page, err := access.ExtensionDiagnostics(context.Background(), interaction.ExtensionDiagnosticsRequest{SessionID: opened.SessionID, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if page.SessionID != opened.SessionID || page.Revision != committed.Revision || len(page.Diagnostics) != 1 || page.Diagnostics[0].InvocationID != entry.InvocationID || page.HasMore {
		t.Fatalf("Gateway diagnostics = %#v", page)
	}
}
