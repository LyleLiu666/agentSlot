package session

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/model"
)

func TestFileStoreFaultInjectionDoesNotHalfPublishV2Upgrade(t *testing.T) {
	store, created, path, before := newFaultInjectionStore(t, "v2-upgrade")
	store.persistence.rename = func(string, string) error { return errors.New("injected rename failure") }
	fingerprint, err := hook.FingerprintTypedInput(struct {
		SessionID agent.SessionID `json:"session_id"`
	}{SessionID: created.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	entry := ExtensionJournalEntry{
		InvocationID: "fault-upgrade-invocation", Sequence: 1,
		Descriptor: extensionDescriptorFixture(), Boundary: hook.BoundarySessionLifecycle,
		SessionID: created.Session.ID, InputDigest: fingerprint.Digest,
		PreparedAt: time.Date(2026, time.August, 30, 10, 0, 0, 0, time.UTC),
		Status:     hook.InvocationPrepared, EffectDisposition: hook.EffectNone, ContextDisposition: hook.ContextNone,
	}
	_, err = store.Commit(context.Background(), CommitRequest{
		SessionID: created.Session.ID, ExpectedRevision: created.Revision, IdempotencyKey: "upgrade-v2",
		Changes: []Change{{Kind: UpdateExtensionJournal, Extension: &entry}},
	})
	if !agent.IsKind(err, agent.ErrorUnavailable) {
		t.Fatalf("v2 upgrade error = %v", err)
	}
	assertFaultDidNotPublish(t, store, path, before, created.Session.ID, created.Revision)
}

func extensionDescriptorFixture() hook.ExtensionDescriptor {
	return hook.ExtensionDescriptor{Key: "fault.fixture", DefinitionDigest: "sha256:" + strings.Repeat("a", 64)}
}

func TestFileStoreFaultInjectionRejectsShortWriteWithoutPublishing(t *testing.T) {
	store, created, path, before := newFaultInjectionStore(t, "short-write")
	store.persistence.write = func(file *os.File, data []byte) (int, error) {
		return file.Write(data[:len(data)/2])
	}

	if _, err := store.Commit(context.Background(), faultCommit(created)); !agent.IsKind(err, agent.ErrorUnavailable) || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short-write Commit error = %v", err)
	}
	assertFaultDidNotPublish(t, store, path, before, created.Session.ID, created.Revision)
}

func TestFileStoreFaultInjectionBeforeRenamePreservesPreviousRevision(t *testing.T) {
	tests := []struct {
		name   string
		inject func(*FileStore, context.CancelFunc)
	}{
		{name: "temporary sync", inject: func(store *FileStore, _ context.CancelFunc) {
			store.persistence.syncFile = func(*os.File) error { return errors.New("injected sync failure") }
		}},
		{name: "temporary close", inject: func(store *FileStore, _ context.CancelFunc) {
			closeFile := store.persistence.closeFile
			store.persistence.closeFile = func(file *os.File) error {
				_ = closeFile(file)
				return errors.New("injected close failure")
			}
		}},
		{name: "rename", inject: func(store *FileStore, _ context.CancelFunc) {
			store.persistence.rename = func(string, string) error { return errors.New("injected rename failure") }
		}},
		{name: "cancellation before rename", inject: func(store *FileStore, cancel context.CancelFunc) {
			closeFile := store.persistence.closeFile
			store.persistence.closeFile = func(file *os.File) error {
				err := closeFile(file)
				cancel()
				return err
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, created, path, before := newFaultInjectionStore(t, test.name)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			test.inject(store, cancel)
			if _, err := store.Commit(ctx, faultCommit(created)); err == nil {
				t.Fatal("fault-injected Commit succeeded")
			}
			assertFaultDidNotPublish(t, store, path, before, created.Session.ID, created.Revision)
		})
	}
}

func TestFileStoreFaultInjectionAfterRenameSupportsIdempotentObservation(t *testing.T) {
	store, created, _, _ := newFaultInjectionStore(t, "directory-sync")
	request := faultCommit(created)
	store.persistence.syncDirectory = func(*os.File) error { return errors.New("injected directory sync failure") }
	if _, err := store.Commit(context.Background(), request); !agent.IsKind(err, agent.ErrorUnavailable) {
		t.Fatalf("directory-sync Commit error = %v", err)
	}

	loaded, err := store.Load(context.Background(), SessionRef{SessionID: created.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != created.Revision+1 || len(loaded.History) != 1 {
		t.Fatalf("renamed revision = %#v", loaded)
	}
	replayed, err := store.Commit(context.Background(), request)
	if err != nil {
		t.Fatalf("idempotent replay after ambiguous durability: %v", err)
	}
	if replayed.Applied || replayed.Revision != loaded.Revision {
		t.Fatalf("idempotent replay = %#v", replayed)
	}
}

func newFaultInjectionStore(t *testing.T, suffix string) (*FileStore, Snapshot, string, []byte) {
	t.Helper()
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	created, err := store.Create(context.Background(), NewSession{
		Session:     agent.Session{ID: agent.SessionID("fault-" + suffix), AgentID: "agent", WorkspaceID: "workspace"},
		ModelConfig: model.Config{ProviderKey: "provider", ModelID: "model", Reasoning: model.ReasoningDefault},
		RunState:    RunIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := store.path(created.Session.ID)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return store, created, path, before
}

func faultCommit(created Snapshot) CommitRequest {
	message := agent.Message{
		ID: "message", SessionID: created.Session.ID, Role: agent.RoleUser,
		Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "must be atomic"}},
	}
	return CommitRequest{
		SessionID: created.Session.ID, ExpectedRevision: created.Revision, IdempotencyKey: "fault-commit",
		Changes: []Change{{Kind: AppendMessage, Message: &message}},
	}
}

func assertFaultDidNotPublish(t *testing.T, store *FileStore, path string, before []byte, sessionID agent.SessionID, revision agent.Revision) {
	t.Helper()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("pre-rename failure changed the durable session document")
	}
	entries, err := os.ReadDir(store.directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("temporary persistence files leaked: %v", entries)
	}
	loaded, err := store.Load(context.Background(), SessionRef{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != revision || len(loaded.History) != 0 {
		t.Fatalf("durable snapshot changed = %#v", loaded)
	}
}
