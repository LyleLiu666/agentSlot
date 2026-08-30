package session_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestFileStoreUpgradesToV2OnlyAfterFirstExtensionEntryAndNeverDowngrades(t *testing.T) {
	directory := t.TempDir()
	store := openFileStore(t, directory)
	created, err := store.Create(context.Background(), session.NewSession{
		Session:     agent.Session{ID: "file-format-upgrade", AgentID: "agent-1", WorkspaceID: "workspace-1"},
		ModelConfig: defaultConfig(), RunState: session.RunIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertSessionFileFormat(t, directory, "agentslot.session-file/v1", false)

	message := agent.Message{
		ID: "ordinary-message", SessionID: created.Session.ID, Role: agent.RoleUser,
		Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "ordinary mutation"}},
	}
	ordinary, err := store.Commit(context.Background(), session.CommitRequest{
		SessionID: created.Session.ID, ExpectedRevision: created.Revision, IdempotencyKey: "ordinary-mutation",
		Changes: []session.Change{{Kind: session.AppendMessage, Message: &message}},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertSessionFileFormat(t, directory, "agentslot.session-file/v1", false)

	current := committedSnapshot(t, store, created.Session.ID, ordinary.Revision)
	prepared := preparedExtensionEntry(t, created.Session.ID, 1, "file-format-invocation")
	upgraded := commitExtension(t, store, current, "first-extension", prepared)
	assertSessionFileFormat(t, directory, "agentslot.session-file/v2", true)

	if err := store.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened := openFileStore(t, directory)
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	loaded := committedSnapshot(t, reopened, created.Session.ID, upgraded.Revision)
	secondMessage := agent.Message{
		ID: "ordinary-after-upgrade", SessionID: created.Session.ID, Role: agent.RoleUser,
		Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "must stay v2"}},
	}
	if _, err := reopened.Commit(context.Background(), session.CommitRequest{
		SessionID: created.Session.ID, ExpectedRevision: loaded.Revision, IdempotencyKey: "ordinary-after-upgrade",
		Changes: []session.Change{{Kind: session.AppendMessage, Message: &secondMessage}},
	}); err != nil {
		t.Fatal(err)
	}
	assertSessionFileFormat(t, directory, "agentslot.session-file/v2", true)
}

func TestFileStoreRejectsV1V2SchemaConfusionUnknownAndTruncatedDocuments(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(t *testing.T, document map[string]json.RawMessage) []byte
	}{
		{
			name: "v1 carrying extension journal",
			mutate: func(t *testing.T, document map[string]json.RawMessage) []byte {
				document["format"] = json.RawMessage(`"agentslot.session-file/v1"`)
				return marshalDocument(t, document)
			},
		},
		{
			name: "v1 carrying explicit empty extension journal",
			mutate: func(t *testing.T, document map[string]json.RawMessage) []byte {
				var snapshot map[string]json.RawMessage
				if err := json.Unmarshal(document["snapshot"], &snapshot); err != nil {
					t.Fatal(err)
				}
				snapshot["ExtensionJournal"] = json.RawMessage(`[]`)
				document["snapshot"] = marshalDocument(t, snapshot)
				document["format"] = json.RawMessage(`"agentslot.session-file/v1"`)
				return marshalDocument(t, document)
			},
		},
		{
			name: "v2 missing extension journal",
			mutate: func(t *testing.T, document map[string]json.RawMessage) []byte {
				var snapshot map[string]json.RawMessage
				if err := json.Unmarshal(document["snapshot"], &snapshot); err != nil {
					t.Fatal(err)
				}
				delete(snapshot, "ExtensionJournal")
				document["snapshot"] = marshalDocument(t, snapshot)
				return marshalDocument(t, document)
			},
		},
		{
			name: "unknown top-level field",
			mutate: func(t *testing.T, document map[string]json.RawMessage) []byte {
				document["unexpected"] = json.RawMessage(`true`)
				return marshalDocument(t, document)
			},
		},
		{
			name: "unknown format",
			mutate: func(t *testing.T, document map[string]json.RawMessage) []byte {
				document["format"] = json.RawMessage(`"agentslot.session-file/v999"`)
				return marshalDocument(t, document)
			},
		},
		{
			name: "truncated",
			mutate: func(t *testing.T, document map[string]json.RawMessage) []byte {
				encoded := marshalDocument(t, document)
				return encoded[:len(encoded)/2]
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			store := openFileStore(t, directory)
			created, err := store.Create(context.Background(), session.NewSession{
				Session:     agent.Session{ID: agent.SessionID("schema-" + filepath.Base(test.name)), AgentID: "agent-1", WorkspaceID: "workspace-1"},
				ModelConfig: defaultConfig(), RunState: session.RunIdle,
			})
			if err != nil {
				t.Fatal(err)
			}
			prepared := preparedExtensionEntry(t, created.Session.ID, 1, "schema-invocation")
			commitExtension(t, store, created, "schema-extension", prepared)
			path := onlySessionFile(t, directory)
			encoded, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &document); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, test.mutate(t, document), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Load(context.Background(), session.SessionRef{SessionID: created.Session.ID}); !agent.IsCode(err, agent.CodeSessionUnrecoverable) {
				t.Fatalf("Load error = %v, code=%q", err, agent.CodeOf(err))
			}
		})
	}
}

func assertSessionFileFormat(t *testing.T, directory, wantFormat string, wantExtensionJournal bool) {
	t.Helper()
	encoded, err := os.ReadFile(onlySessionFile(t, directory))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Format   string                     `json:"format"`
		Snapshot map[string]json.RawMessage `json:"snapshot"`
	}
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	_, hasJournal := document.Snapshot["ExtensionJournal"]
	if document.Format != wantFormat || hasJournal != wantExtensionJournal {
		t.Fatalf("file format = %q journal-field=%t, want %q/%t", document.Format, hasJournal, wantFormat, wantExtensionJournal)
	}
}

func onlySessionFile(t *testing.T, directory string) string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			return filepath.Join(directory, entry.Name())
		}
	}
	t.Fatal("session file not found")
	return ""
}

func marshalDocument(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
