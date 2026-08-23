// Package file provides a crash-safe local immutable ArtifactStore. Each
// artifact is committed as one self-describing file by atomic rename; callers
// receive only a content-derived opaque ID, never the backing path.
package file

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/artifact"
)

const (
	format         = "agentslot.artifact-file/v1"
	fileSuffix     = ".artifact"
	maxHeaderBytes = 64 << 10
)

var ErrCorrupt = errors.New("artifact/file: corrupt artifact")

type Store struct{ root string }

type header struct {
	Format    string `json:"format"`
	ID        string `json:"id"`
	MediaType string `json:"media_type"`
	Name      string `json:"name,omitempty"`
	Size      int64  `json:"size"`
}

func New(root string) (*Store, error) {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, errors.New("artifact/file: root must be absolute and clean")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, errors.New("artifact/file: artifact directory is unavailable")
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, errors.New("artifact/file: artifact directory is unavailable")
	}
	return &Store{root: root}, nil
}

// NewModule constructs the file Store and contributes it under the stable
// artifact.file module identity.
func NewModule(root string) (agentslot.Module, error) {
	store, err := New(root)
	if err != nil {
		return nil, err
	}
	return artifact.NewModule("artifact.file", store)
}

func (s *Store) Write(ctx context.Context, request artifact.WriteRequest) (artifact.Metadata, error) {
	if err := ctx.Err(); err != nil {
		return artifact.Metadata{}, err
	}
	if err := request.Validate(); err != nil {
		return artifact.Metadata{}, err
	}
	if s == nil || s.root == "" {
		return artifact.Metadata{}, errors.New("artifact/file: store is unavailable")
	}
	body, err := os.CreateTemp(s.root, ".artifact-body-*")
	if err != nil {
		return artifact.Metadata{}, errors.New("artifact/file: cannot stage artifact")
	}
	bodyPath := body.Name()
	defer os.Remove(bodyPath)
	digest := sha256.New()
	writeHashPrefix(digest, request.MediaType, request.Name)
	size, copyErr := copyContext(ctx, io.MultiWriter(body, digest), request.Body)
	syncErr := body.Sync()
	closeErr := body.Close()
	if copyErr != nil {
		return artifact.Metadata{}, copyErr
	}
	if syncErr != nil || closeErr != nil {
		return artifact.Metadata{}, errors.New("artifact/file: cannot durably stage artifact")
	}
	id := hex.EncodeToString(digest.Sum(nil))
	metadata := artifact.Metadata{ID: id, MediaType: request.MediaType, Name: request.Name, Size: size}
	if err := metadata.Validate(); err != nil {
		return artifact.Metadata{}, err
	}
	finalPath := filepath.Join(s.root, id+fileSuffix)
	if existing, err := s.openMetadata(id); err == nil {
		if existing != metadata {
			return artifact.Metadata{}, ErrCorrupt
		}
		return metadata, nil
	} else if !errors.Is(err, artifact.ErrNotFound) {
		return artifact.Metadata{}, err
	}
	staged, err := os.CreateTemp(s.root, ".artifact-commit-*")
	if err != nil {
		return artifact.Metadata{}, errors.New("artifact/file: cannot stage artifact commit")
	}
	stagedPath := staged.Name()
	defer os.Remove(stagedPath)
	encodedHeader, err := json.Marshal(header{Format: format, ID: id, MediaType: metadata.MediaType, Name: metadata.Name, Size: metadata.Size})
	if err != nil {
		_ = staged.Close()
		return artifact.Metadata{}, errors.New("artifact/file: cannot encode artifact metadata")
	}
	if _, err := staged.Write(append(encodedHeader, '\n')); err != nil {
		_ = staged.Close()
		return artifact.Metadata{}, errors.New("artifact/file: cannot write artifact metadata")
	}
	input, err := os.Open(bodyPath)
	if err != nil {
		_ = staged.Close()
		return artifact.Metadata{}, errors.New("artifact/file: cannot reopen staged artifact")
	}
	_, copyErr = copyContext(ctx, staged, input)
	_ = input.Close()
	if copyErr != nil {
		_ = staged.Close()
		return artifact.Metadata{}, copyErr
	}
	if err := staged.Chmod(0o600); err != nil {
		_ = staged.Close()
		return artifact.Metadata{}, errors.New("artifact/file: cannot protect artifact")
	}
	if err := staged.Sync(); err != nil {
		_ = staged.Close()
		return artifact.Metadata{}, errors.New("artifact/file: cannot durably write artifact")
	}
	if err := staged.Close(); err != nil {
		return artifact.Metadata{}, errors.New("artifact/file: cannot close artifact")
	}
	if err := os.Rename(stagedPath, finalPath); err != nil {
		return artifact.Metadata{}, errors.New("artifact/file: cannot commit artifact")
	}
	if err := syncDirectory(s.root); err != nil {
		return artifact.Metadata{}, errors.New("artifact/file: cannot commit artifact directory")
	}
	return metadata, nil
}

func (s *Store) Open(ctx context.Context, id string) (artifact.Content, error) {
	if err := ctx.Err(); err != nil {
		return artifact.Content{}, err
	}
	if s == nil || !validID(id) {
		return artifact.Content{}, artifact.ErrNotFound
	}
	file, err := os.Open(filepath.Join(s.root, id+fileSuffix))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return artifact.Content{}, artifact.ErrNotFound
		}
		return artifact.Content{}, errors.New("artifact/file: artifact is unavailable")
	}
	reader := bufio.NewReaderSize(file, maxHeaderBytes)
	line, err := reader.ReadSlice('\n')
	if err != nil || len(line) > maxHeaderBytes {
		_ = file.Close()
		return artifact.Content{}, ErrCorrupt
	}
	var stored header
	if err := json.Unmarshal(line[:len(line)-1], &stored); err != nil || stored.Format != format || stored.ID != id {
		_ = file.Close()
		return artifact.Content{}, ErrCorrupt
	}
	metadata := artifact.Metadata{ID: stored.ID, MediaType: stored.MediaType, Name: stored.Name, Size: stored.Size}
	if err := metadata.Validate(); err != nil {
		_ = file.Close()
		return artifact.Content{}, ErrCorrupt
	}
	info, err := file.Stat()
	if err != nil || info.Size() != int64(len(line))+metadata.Size {
		_ = file.Close()
		return artifact.Content{}, ErrCorrupt
	}
	return artifact.Content{Metadata: metadata, Body: &readerCloser{Reader: reader, closer: file}}, nil
}

func (s *Store) openMetadata(id string) (artifact.Metadata, error) {
	content, err := s.Open(context.Background(), id)
	if err != nil {
		return artifact.Metadata{}, err
	}
	_ = content.Body.Close()
	return content.Metadata, nil
}

type readerCloser struct {
	io.Reader
	closer io.Closer
}

func (r *readerCloser) Close() error { return r.closer.Close() }

func validID(id string) bool {
	if len(id) != sha256.Size*2 || strings.ToLower(id) != id {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func writeHashPrefix(digest hash.Hash, mediaType, name string) {
	_, _ = io.WriteString(digest, mediaType)
	_, _ = digest.Write([]byte{0})
	_, _ = io.WriteString(digest, name)
	_, _ = digest.Write([]byte{0})
}

func copyContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 32<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			written, writeErr := destination.Write(buffer[:read])
			total += int64(written)
			if writeErr != nil {
				return total, errors.New("artifact/file: cannot write artifact")
			}
			if written != read {
				return total, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, errors.New("artifact/file: cannot read artifact input")
		}
	}
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

var _ artifact.ArtifactStore = (*Store)(nil)
