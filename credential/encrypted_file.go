package credential

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const (
	encryptedFileHeader = "agentslot.credentials/v1\n"
	maxEncryptedBytes   = 8 << 20
)

// KeyProvider returns key material immediately before one file decrypt. The
// resolver clones and clears its clone; it does not retain or mutate the
// provider-owned buffer. AES-256 keys are exactly 32 bytes.
type KeyProvider func(context.Context) ([]byte, error)

// EncryptedFileResolver is a local reference implementation. It reads and
// decrypts the file on every Resolve, so credential rotation does not require
// rebuilding the Assembly. File paths and decrypt details are not returned in
// errors.
type EncryptedFileResolver struct {
	path string
	key  KeyProvider
}

func NewEncryptedFileResolver(path string, key KeyProvider) (*EncryptedFileResolver, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("credential: encrypted file path must be absolute and clean")
	}
	if key == nil {
		return nil, errors.New("credential: KeyProvider is required")
	}
	return &EncryptedFileResolver{path: path, key: key}, nil
}

func (r *EncryptedFileResolver) Resolve(ctx context.Context, request Request, consume Consumer) (Identity, error) {
	if err := ctx.Err(); err != nil {
		return Identity{}, err
	}
	if err := request.Validate(); err != nil {
		return Identity{}, err
	}
	if consume == nil {
		return Identity{}, errors.New("credential: Consumer is required")
	}
	if r == nil || r.key == nil {
		return Identity{}, ErrUnavailable
	}
	key, err := r.key(ctx)
	if err != nil {
		return Identity{}, unavailable()
	}
	keyCopy := append([]byte(nil), key...)
	defer clear(keyCopy)
	if len(keyCopy) != 32 {
		return Identity{}, unavailable()
	}
	sealed, err := readSecureFile(r.path)
	if err != nil {
		return Identity{}, unavailable()
	}
	defer clear(sealed)
	records, err := openRecords(sealed, keyCopy)
	if err != nil {
		return Identity{}, unavailable()
	}
	defer clearRecords(records)
	for _, record := range records {
		if record.Ref.ID != request.Ref.ID {
			continue
		}
		if record.Material.Kind != request.Kind {
			return Identity{}, ErrKindMismatch
		}
		material := cloneMaterial(record.Material)
		defer material.Clear()
		if err := consume(material); err != nil {
			return record.Identity, err
		}
		return record.Identity, nil
	}
	return Identity{}, ErrNotFound
}

func unavailable() error {
	return errors.Join(ErrUnavailable, errors.New("credential: encrypted credential source cannot be opened"))
}

func readSecureFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() <= int64(len(encryptedFileHeader)+12) || info.Size() > maxEncryptedBytes {
		return nil, errors.New("credential: encrypted file is not a secure regular file")
	}
	content, err := io.ReadAll(io.LimitReader(file, maxEncryptedBytes+1))
	if err != nil || len(content) > maxEncryptedBytes {
		clear(content)
		return nil, errors.New("credential: encrypted file cannot be read")
	}
	return content, nil
}

// Seal produces the versioned encrypted-file bytes used by
// EncryptedFileResolver. The returned bytes contain ciphertext only. Products
// remain responsible for an atomic 0600 write and for obtaining the AES-256
// key from a source independent of the encrypted file.
func Seal(records []Record, key []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("credential: Seal requires a 32-byte AES-256 key")
	}
	seen := make(map[string]struct{}, len(records))
	detached := make([]Record, len(records))
	for index, record := range records {
		if err := record.validate(); err != nil {
			clearRecords(detached)
			return nil, errors.New("credential: Seal received an invalid record")
		}
		if _, duplicate := seen[record.Ref.ID]; duplicate {
			clearRecords(detached)
			return nil, errors.New("credential: Seal received duplicate references")
		}
		seen[record.Ref.ID] = struct{}{}
		detached[index] = record
		detached[index].Material = cloneMaterial(record.Material)
	}
	defer clearRecords(detached)
	plain, err := json.Marshal(detached)
	if err != nil {
		return nil, errors.New("credential: encode encrypted records")
	}
	defer clear(plain)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("credential: initialize AES-256")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("credential: initialize authenticated encryption")
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, errors.New("credential: generate encryption nonce")
	}
	output := append([]byte(encryptedFileHeader), nonce...)
	output = aead.Seal(output, nonce, plain, []byte(encryptedFileHeader))
	return output, nil
}

func openRecords(sealed, key []byte) ([]Record, error) {
	if len(sealed) < len(encryptedFileHeader) || string(sealed[:len(encryptedFileHeader)]) != encryptedFileHeader {
		return nil, errors.New("credential: unsupported encrypted file")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	remaining := sealed[len(encryptedFileHeader):]
	if len(remaining) <= aead.NonceSize() {
		return nil, errors.New("credential: truncated encrypted file")
	}
	nonce := remaining[:aead.NonceSize()]
	plain, err := aead.Open(nil, nonce, remaining[aead.NonceSize():], []byte(encryptedFileHeader))
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	var records []Record
	if err := json.Unmarshal(plain, &records); err != nil {
		clearRecords(records)
		return nil, err
	}
	seen := make(map[string]struct{}, len(records))
	for _, record := range records {
		if err := record.validate(); err != nil {
			clearRecords(records)
			return nil, err
		}
		if _, duplicate := seen[record.Ref.ID]; duplicate {
			clearRecords(records)
			return nil, errors.New("credential: duplicate encrypted record")
		}
		seen[record.Ref.ID] = struct{}{}
	}
	return records, nil
}

func clearRecords(records []Record) {
	for index := range records {
		records[index].Material.Clear()
	}
}

var _ Resolver = (*EncryptedFileResolver)(nil)
