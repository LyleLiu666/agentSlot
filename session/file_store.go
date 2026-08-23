package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
)

const (
	fileStoreFormat           = "agentslot.session-file/v1"
	maxFileStoreDocumentBytes = 256 << 20
)

// FileStore is a crash-safe, single-process SessionStore backed by one private
// JSON document per Session. Each commit writes and fsyncs a complete new
// document before atomically renaming it over the previous revision. The
// standard architecture still permits only one process to execute a Session;
// FileStore does not pretend to provide a distributed execution lease.
type FileStore struct {
	mu        sync.Mutex
	directory string
	opened    bool
	list      sessionListIndex
}

var _ SessionStore = (*FileStore)(nil)

// NewFileStore validates and fixes the storage path without touching the
// filesystem. Call Open explicitly, or install NewFileModule so lifecycle
// ordering opens the Store before the standard Runtime starts.
func NewFileStore(directory string) (*FileStore, error) {
	if directory == "" {
		return nil, invalid("session.file_store", "directory is required")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, agent.NewError(agent.ErrorInvalidInput, "session.file_store", "cannot resolve storage directory", err)
	}
	return &FileStore{directory: absolute}, nil
}

func (s *FileStore) Open(ctx context.Context) error {
	if err := contextErr(ctx, "session.file_store.open"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.opened {
		return agent.NewError(agent.ErrorConflict, "session.file_store.open", "FileStore is already open", nil)
	}
	if err := os.MkdirAll(s.directory, 0o700); err != nil {
		return agent.NewError(agent.ErrorUnavailable, "session.file_store.open", "cannot create storage directory", err)
	}
	if err := os.Chmod(s.directory, 0o700); err != nil {
		return agent.NewError(agent.ErrorUnavailable, "session.file_store.open", "cannot secure storage directory", err)
	}
	info, err := os.Stat(s.directory)
	if err != nil || !info.IsDir() {
		return agent.NewError(agent.ErrorInvalidInput, "session.file_store.open", "storage path is not a directory", err)
	}
	s.list.reset()
	s.opened = true
	return nil
}

func (s *FileStore) Close(ctx context.Context) error {
	if err := contextErr(ctx, "session.file_store.close"); err != nil {
		return err
	}
	s.mu.Lock()
	s.opened = false
	s.mu.Unlock()
	return nil
}

func (s *FileStore) Create(ctx context.Context, initial NewSession) (Snapshot, error) {
	if err := contextErr(ctx, "session.file_store.create"); err != nil {
		return Snapshot{}, err
	}
	if err := validateNewSession(initial); err != nil {
		return Snapshot{}, err
	}
	if initial.RunState == "" {
		initial.RunState = RunIdle
	}
	history, err := prepareInitialHistory(initial.Session.ID, initial.History)
	if err != nil {
		return Snapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked("session.file_store.create"); err != nil {
		return Snapshot{}, err
	}
	path := s.path(initial.Session.ID)
	if _, err := os.Stat(path); err == nil {
		return Snapshot{}, agent.NewCodedError(agent.ErrorConflict, agent.CodeSessionAlreadyExists, "session.file_store.create", "session already exists", nil)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, agent.NewError(agent.ErrorUnavailable, "session.file_store.create", "cannot inspect session file", err)
	}
	snapshot := cloneSnapshot(Snapshot{
		Session: initial.Session, Revision: 1, History: history,
		Context: initial.Context, Queue: initial.Queue, RunJournal: initial.RunJournal,
		ModelConfig: initial.ModelConfig, RunState: initial.RunState, ActiveRunID: initial.ActiveRunID,
		Fork: initial.Fork,
	})
	snapshot.Session.Revision = snapshot.Revision
	document := fileStoreDocument{Format: fileStoreFormat, Snapshot: snapshot, UpdatedAt: s.list.recordCreate(snapshot.Session.ID), Idempotency: make(map[string]fileStoreCommit)}
	if err := s.persistLocked(ctx, path, document); err != nil {
		return Snapshot{}, err
	}
	return cloneSnapshot(snapshot), nil
}

func (s *FileStore) HistoryPage(ctx context.Context, request HistoryPageRequest) (HistoryPage, error) {
	if err := contextErr(ctx, "session.file_store.history_page"); err != nil {
		return HistoryPage{}, err
	}
	if !request.SessionID.Valid() {
		return HistoryPage{}, invalid("session.file_store.history_page", "session ID is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked("session.file_store.history_page"); err != nil {
		return HistoryPage{}, err
	}
	document, err := s.readLocked(request.SessionID)
	if err != nil {
		return HistoryPage{}, err
	}
	page, err := historyPage(document.Snapshot.History, request)
	if err != nil {
		return HistoryPage{}, err
	}
	page.AgentID = document.Snapshot.Session.AgentID
	page.WorkspaceID = document.Snapshot.Session.WorkspaceID
	page.Revision = document.Snapshot.Revision
	return page, nil
}

func (s *FileStore) ListSessions(ctx context.Context, request ListRequest) (ListResult, error) {
	if err := contextErr(ctx, "session.file_store.list"); err != nil {
		return ListResult{}, err
	}
	if err := request.Validate(); err != nil {
		return ListResult{}, invalid("session.file_store.list", err.Error())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked("session.file_store.list"); err != nil {
		return ListResult{}, err
	}
	entries, err := os.ReadDir(s.directory)
	if err != nil {
		return ListResult{}, agent.NewError(agent.ErrorUnavailable, "session.file_store.list", "cannot read storage directory", err)
	}
	listed := make([]listedSession, 0)
	for _, entry := range entries {
		if err := contextErr(ctx, "session.file_store.list"); err != nil {
			return ListResult{}, err
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(s.directory, entry.Name())
		document, err := s.readDocumentLocked(path, "")
		if err != nil {
			return ListResult{}, err
		}
		current := document.Snapshot
		s.list.observeUpdatedAt(document.UpdatedAt)
		if current.Session.AgentID != request.AgentID || current.Session.WorkspaceID != request.WorkspaceID {
			continue
		}
		listed = append(listed, listedSession{
			summary: SessionSummary{
				SessionID: current.Session.ID, AgentID: current.Session.AgentID, WorkspaceID: current.Session.WorkspaceID,
				Revision: current.Revision, UpdatedAt: document.UpdatedAt,
			},
			generation: s.list.generationFor(current.Session.ID),
		})
	}
	result, err := s.list.paginate(request, listed)
	if err != nil {
		return ListResult{}, invalid("session.file_store.list", err.Error())
	}
	return result, nil
}

func (s *FileStore) Load(ctx context.Context, ref SessionRef) (Snapshot, error) {
	if err := contextErr(ctx, "session.file_store.load"); err != nil {
		return Snapshot{}, err
	}
	if !ref.SessionID.Valid() {
		return Snapshot{}, invalid("session.file_store.load", "session ID is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked("session.file_store.load"); err != nil {
		return Snapshot{}, err
	}
	document, err := s.readLocked(ref.SessionID)
	if err != nil {
		return Snapshot{}, err
	}
	return cloneSnapshot(document.Snapshot), nil
}

func (s *FileStore) Recover(ctx context.Context, ref SessionRef) (Snapshot, error) {
	if err := contextErr(ctx, "session.file_store.recover"); err != nil {
		return Snapshot{}, err
	}
	if !ref.SessionID.Valid() {
		return Snapshot{}, invalid("session.file_store.recover", "session ID is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked("session.file_store.recover"); err != nil {
		return Snapshot{}, err
	}
	document, err := s.readLocked(ref.SessionID)
	if err != nil {
		return Snapshot{}, err
	}
	changed, err := recoverAggregate(&document.Snapshot)
	if err != nil {
		return Snapshot{}, err
	}
	if changed {
		document.Snapshot.Revision++
		document.Snapshot.Session.Revision = document.Snapshot.Revision
		document.UpdatedAt = s.list.nextUpdatedAt(document.UpdatedAt)
		if err := s.persistLocked(ctx, s.path(ref.SessionID), document); err != nil {
			return Snapshot{}, err
		}
	}
	return cloneSnapshot(document.Snapshot), nil
}

func (s *FileStore) Commit(ctx context.Context, request CommitRequest) (Commit, error) {
	if err := contextErr(ctx, "session.file_store.commit"); err != nil {
		return Commit{}, err
	}
	if err := request.Validate(); err != nil {
		return Commit{}, invalid("session.file_store.commit", err.Error())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked("session.file_store.commit"); err != nil {
		return Commit{}, err
	}
	document, err := s.readLocked(request.SessionID)
	if err != nil {
		return Commit{}, err
	}
	fingerprint, err := requestFingerprint(request)
	if err != nil {
		return Commit{}, agent.NewError(agent.ErrorInternal, "session.file_store.commit", "cannot fingerprint idempotent request", err)
	}
	if previous, exists := document.Idempotency[request.IdempotencyKey]; exists {
		if previous.Fingerprint != fingerprint {
			return Commit{}, agent.NewCodedError(agent.ErrorConflict, agent.CodeRevisionConflict, "session.file_store.commit", "idempotency key was already used for another request", nil)
		}
		result := previous.Commit
		result.Applied = false
		return result, nil
	}
	if request.ExpectedRevision != document.Snapshot.Revision {
		return Commit{}, agent.NewCodedError(agent.ErrorConflict, agent.CodeRevisionConflict, "session.file_store.commit", "expected revision does not match", nil)
	}
	working := cloneSnapshot(document.Snapshot)
	previousHistoryLength := len(working.History)
	if err := applyChanges(&working, request); err != nil {
		return Commit{}, err
	}
	working.Revision++
	working.Session.Revision = working.Revision
	first, last := appendedHistoryRange(previousHistoryLength, len(working.History))
	result := Commit{
		SessionID: request.SessionID, Revision: working.Revision,
		FirstHistorySequence: first, LastHistorySequence: last, Applied: true,
	}
	document.Snapshot = working
	document.UpdatedAt = s.list.nextUpdatedAt(document.UpdatedAt)
	if document.Idempotency == nil {
		document.Idempotency = make(map[string]fileStoreCommit)
	}
	document.Idempotency[request.IdempotencyKey] = fileStoreCommit{Fingerprint: fingerprint, Commit: result}
	if err := s.persistLocked(ctx, s.path(request.SessionID), document); err != nil {
		return Commit{}, err
	}
	return result, nil
}

type fileStoreDocument struct {
	Format      string                     `json:"format"`
	Snapshot    Snapshot                   `json:"snapshot"`
	UpdatedAt   time.Time                  `json:"updated_at,omitempty"`
	Idempotency map[string]fileStoreCommit `json:"idempotency"`
}

type fileStoreCommit struct {
	Fingerprint string `json:"fingerprint"`
	Commit      Commit `json:"commit"`
}

func (s *FileStore) ensureOpenLocked(operation string) error {
	if !s.opened {
		return agent.NewCodedError(agent.ErrorUnavailable, agent.CodeApplicationNotStarted, operation, "FileStore is not open", nil)
	}
	return nil
}

func (s *FileStore) path(id agent.SessionID) string {
	hash := sha256.Sum256([]byte(id))
	return filepath.Join(s.directory, hex.EncodeToString(hash[:])+".json")
}

func (s *FileStore) readLocked(id agent.SessionID) (fileStoreDocument, error) {
	return s.readDocumentLocked(s.path(id), id)
}

func (s *FileStore) readDocumentLocked(path string, expectedID agent.SessionID) (fileStoreDocument, error) {
	id := expectedID
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileStoreDocument{}, agent.NewCodedError(agent.ErrorNotFound, agent.CodeSessionNotFound, "session.file_store.load", "session not found", nil)
	}
	if err != nil {
		return fileStoreDocument{}, agent.NewError(agent.ErrorUnavailable, "session.file_store.load", "cannot open session file", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fileStoreDocument{}, agent.NewError(agent.ErrorUnavailable, "session.file_store.load", "cannot inspect session file", err)
	}
	if info.Size() > maxFileStoreDocumentBytes {
		return fileStoreDocument{}, unrecoverableFile(id, "session file exceeds the configured safety limit", nil)
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxFileStoreDocumentBytes+1))
	decoder.DisallowUnknownFields()
	var document fileStoreDocument
	if err := decoder.Decode(&document); err != nil {
		return fileStoreDocument{}, unrecoverableFile(id, "cannot decode session file", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fileStoreDocument{}, unrecoverableFile(id, "session file contains trailing data", err)
	}
	if info, err = file.Stat(); err != nil || info.Size() > maxFileStoreDocumentBytes {
		return fileStoreDocument{}, unrecoverableFile(id, "session file changed or exceeded the safety limit while reading", err)
	}
	if !id.Valid() {
		id = document.Snapshot.Session.ID
	}
	// Documents written before Session listing existed have no logical update
	// timestamp. Their file modification time is a one-time migration fallback;
	// all new writes persist the timestamp inside the crash-safe document.
	if document.UpdatedAt.IsZero() {
		document.UpdatedAt = info.ModTime().UTC()
	}
	if err := validateFileDocument(id, document); err != nil {
		return fileStoreDocument{}, unrecoverableFile(id, "session file violates aggregate invariants", err)
	}
	return document, nil
}

func (s *FileStore) persistLocked(ctx context.Context, path string, document fileStoreDocument) error {
	if err := contextErr(ctx, "session.file_store.persist"); err != nil {
		return err
	}
	data, err := json.Marshal(document)
	if err != nil {
		return agent.NewError(agent.ErrorInternal, "session.file_store.persist", "cannot encode session aggregate", err)
	}
	temporary, err := os.CreateTemp(s.directory, ".agentslot-session-*.tmp")
	if err != nil {
		return agent.NewError(agent.ErrorUnavailable, "session.file_store.persist", "cannot create temporary session file", err)
	}
	temporaryPath := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return agent.NewError(agent.ErrorUnavailable, "session.file_store.persist", "cannot secure temporary session file", err)
	}
	if _, err := temporary.Write(data); err != nil {
		return agent.NewError(agent.ErrorUnavailable, "session.file_store.persist", "cannot write temporary session file", err)
	}
	if err := temporary.Sync(); err != nil {
		return agent.NewError(agent.ErrorUnavailable, "session.file_store.persist", "cannot sync temporary session file", err)
	}
	if err := temporary.Close(); err != nil {
		return agent.NewError(agent.ErrorUnavailable, "session.file_store.persist", "cannot close temporary session file", err)
	}
	if err := contextErr(ctx, "session.file_store.persist"); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return agent.NewError(agent.ErrorUnavailable, "session.file_store.persist", "cannot atomically replace session file", err)
	}
	keep = true
	directory, err := os.Open(s.directory)
	if err != nil {
		return agent.NewError(agent.ErrorUnavailable, "session.file_store.persist", "cannot open storage directory for sync", err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil || closeErr != nil {
		return agent.NewError(agent.ErrorUnavailable, "session.file_store.persist", "cannot sync storage directory", errors.Join(syncErr, closeErr))
	}
	return nil
}

func validateFileDocument(id agent.SessionID, document fileStoreDocument) error {
	if document.Format != fileStoreFormat {
		return fmt.Errorf("unsupported format %q", document.Format)
	}
	snapshot := document.Snapshot
	if snapshot.Session.ID != id || snapshot.Revision == 0 || snapshot.Session.Revision != snapshot.Revision {
		return errors.New("session identity or revision is inconsistent")
	}
	if !snapshot.Session.AgentID.Valid() || !snapshot.Session.WorkspaceID.Valid() {
		return errors.New("session scope is incomplete")
	}
	if snapshot.Fork != nil && (snapshot.Fork.ParentSessionID != snapshot.Session.ParentSessionID || !snapshot.Fork.ParentSessionID.Valid()) {
		return errors.New("fork provenance is inconsistent")
	}
	if snapshot.Fork != nil {
		if snapshot.Fork.CutoffSequence > HistorySequence(len(snapshot.History)) {
			return errors.New("fork cutoff exceeds the durable History prefix")
		}
		for _, fact := range snapshot.History {
			if fact.Sequence <= snapshot.Fork.CutoffSequence {
				if !fact.OriginFactID.Valid() {
					return errors.New("forked History prefix fact has no source identity")
				}
			} else if fact.OriginFactID != "" {
				return errors.New("child History fact unexpectedly carries source identity")
			}
		}
	}
	if err := snapshot.ModelConfig.Validate(); err != nil {
		return err
	}
	if !snapshot.RunState.Valid() || (snapshot.RunState == RunRunning) != snapshot.ActiveRunID.Valid() {
		return errors.New("run state and active run are inconsistent")
	}
	for index, fact := range snapshot.History {
		if err := fact.Validate(id); err != nil {
			return err
		}
		if fact.Sequence != HistorySequence(index+1) {
			return errors.New("history sequence is not contiguous")
		}
	}
	for _, item := range snapshot.Queue {
		if !validQueueItem(item, id) {
			return errors.New("invalid queue item")
		}
		if item.Claimed() && (snapshot.RunState != RunRunning || item.ClaimedBy != snapshot.ActiveRunID) {
			return errors.New("claimed queue item does not belong to the active run")
		}
	}
	if duplicateQueueMessage(snapshot.Queue) {
		return errors.New("duplicate queue message")
	}
	for _, entry := range snapshot.RunJournal {
		if err := entry.Validate(id); err != nil {
			return err
		}
		if unfinishedJournal(entry.Status) && (snapshot.RunState != RunRunning || entry.RunID != snapshot.ActiveRunID) {
			return errors.New("unfinished journal does not belong to the active run")
		}
	}
	if err := validateContext(snapshot.Context, id); err != nil {
		return err
	}
	if err := validateContextRun(snapshot.History, snapshot.Context); err != nil {
		return err
	}
	previousVersion := ContextVersion(0)
	for _, retained := range snapshot.RetainedContexts {
		if err := validateContext(retained, id); err != nil {
			return err
		}
		if err := validateContextRun(snapshot.History, retained); err != nil {
			return err
		}
		if (previousVersion != 0 && retained.Version <= previousVersion) || retained.Version >= snapshot.Context.Version {
			return errors.New("retained context versions are inconsistent")
		}
		previousVersion = retained.Version
	}
	if err := validateHistoryConsistency(id, snapshot.History, snapshot.RunJournal); err != nil {
		return err
	}
	for _, event := range snapshot.Events {
		if err := event.Validate(); err != nil || event.Revision == 0 || event.Revision > snapshot.Revision {
			return errors.New("invalid session event")
		}
	}
	for key, record := range document.Idempotency {
		if key == "" || record.Fingerprint == "" || record.Commit.SessionID != id || record.Commit.Revision == 0 || record.Commit.Revision > snapshot.Revision {
			return errors.New("invalid idempotency record")
		}
	}
	return nil
}

func unrecoverableFile(id agent.SessionID, message string, cause error) error {
	if cause != nil {
		message = fmt.Sprintf("%s: %v", message, cause)
	}
	return agent.NewCodedError(agent.ErrorInternal, agent.CodeSessionUnrecoverable, "session.file_store", fmt.Sprintf("%s: %s", message, id), cause)
}

// NewFileModule explicitly installs a lifecycle-owned FileStore.
func NewFileModule(directory string) (agentslot.Module, error) {
	store, err := NewFileStore(directory)
	if err != nil {
		return nil, err
	}
	return &fileModule{store: store}, nil
}

type fileModule struct {
	store *FileStore
}

func (*fileModule) ID() string { return "session.file" }

func (m *fileModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Set(StoreSlot, SessionStore(m.store)))
}

func (m *fileModule) Start(ctx context.Context) error { return m.store.Open(ctx) }
func (m *fileModule) Stop(ctx context.Context) error  { return m.store.Close(ctx) }
