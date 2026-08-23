package file_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/LyleLiu666/agentSlot/artifact"
	artifactfile "github.com/LyleLiu666/agentSlot/artifact/file"
)

func TestStorePersistsImmutableBinaryArtifactAcrossReopen(t *testing.T) {
	root := filepath.Join(t.TempDir(), "artifacts")
	store, err := artifactfile.New(root)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte{0, 1, 2, 3, 255, 0, 4}
	metadata, err := store.Write(context.Background(), artifact.WriteRequest{MediaType: "application/octet-stream", Name: "sample.bin", Body: bytes.NewReader(body)})
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Size != int64(len(body)) || metadata.MediaType != "application/octet-stream" || metadata.Name != "sample.bin" {
		t.Fatalf("metadata = %#v", metadata)
	}
	reopened, err := artifactfile.New(root)
	if err != nil {
		t.Fatal(err)
	}
	content, err := reopened.Open(context.Background(), metadata.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer content.Body.Close()
	read, err := io.ReadAll(content.Body)
	if err != nil || !bytes.Equal(read, body) || content.Metadata != metadata {
		t.Fatalf("opened = %#v %v bytes=%v", content.Metadata, err, read)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("artifact directory = %#v, %v", entries, err)
	}
}

func TestStoreConcurrentIdenticalWritesConverge(t *testing.T) {
	store, err := artifactfile.New(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	const writers = 12
	ids := make(chan string, writers)
	errs := make(chan error, writers)
	var wait sync.WaitGroup
	for index := 0; index < writers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			metadata, err := store.Write(context.Background(), artifact.WriteRequest{MediaType: "text/plain", Name: "same.txt", Body: bytes.NewBufferString("same content")})
			if err != nil {
				errs <- err
				return
			}
			ids <- metadata.ID
		}()
	}
	wait.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var expected string
	for id := range ids {
		if expected == "" {
			expected = id
		}
		if id != expected {
			t.Fatalf("identical writes returned %q and %q", expected, id)
		}
	}
}

func TestStoreRejectsInvalidIDsAndCorruptFilesWithoutPathDisclosure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "artifacts")
	store, err := artifactfile.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(context.Background(), "../../escape"); err == nil || !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("invalid ID error = %v", err)
	}
	metadata, err := store.Write(context.Background(), artifact.WriteRequest{MediaType: "text/plain", Body: bytes.NewBufferString("content")})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, metadata.ID+".artifact")
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = store.Open(context.Background(), metadata.ID)
	if !errors.Is(err, artifactfile.ErrCorrupt) || bytes.Contains([]byte(err.Error()), []byte(root)) {
		t.Fatalf("corrupt error = %v", err)
	}
}
