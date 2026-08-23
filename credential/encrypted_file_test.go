package credential_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LyleLiu666/agentSlot/credential"
)

func TestEncryptedFileResolverDecryptsAtEachResolveBoundary(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	path := filepath.Join(t.TempDir(), "credentials.enc")
	writeCredentialFile(t, path, key, credential.Record{
		Ref: credential.Ref{ID: "provider"}, Identity: credential.Identity{Fingerprint: "provider-v1"},
		Material: credential.Material{Kind: credential.KindBearer, Token: []byte("first-secret")},
	})
	var keyCalls atomic.Int64
	resolver, err := credential.NewEncryptedFileResolver(path, func(context.Context) ([]byte, error) {
		keyCalls.Add(1)
		return append([]byte(nil), key...), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	resolveBearer := func() (credential.Identity, string, error) {
		var value string
		identity, err := resolver.Resolve(context.Background(), credential.Request{
			Ref: credential.Ref{ID: "provider"}, Kind: credential.KindBearer,
		}, func(material credential.Material) error {
			value = string(material.Token)
			return nil
		})
		return identity, value, err
	}
	identity, value, err := resolveBearer()
	if err != nil || identity.Fingerprint != "provider-v1" || value != "first-secret" {
		t.Fatalf("first resolve = %#v, %q, %v", identity, value, err)
	}
	writeCredentialFile(t, path, key, credential.Record{
		Ref: credential.Ref{ID: "provider"}, Identity: credential.Identity{Fingerprint: "provider-v2"},
		Material: credential.Material{Kind: credential.KindBearer, Token: []byte("rotated-secret")},
	})
	identity, value, err = resolveBearer()
	if err != nil || identity.Fingerprint != "provider-v2" || value != "rotated-secret" || keyCalls.Load() != 2 {
		t.Fatalf("rotated resolve = %#v, %q, %v; key calls=%d", identity, value, err, keyCalls.Load())
	}
}

func TestEncryptedFileResolverFailsClosedWithoutExposingFileOrSecret(t *testing.T) {
	goodKey := []byte("0123456789abcdef0123456789abcdef")
	path := filepath.Join(t.TempDir(), "credentials.enc")
	writeCredentialFile(t, path, goodKey, credential.Record{
		Ref: credential.Ref{ID: "provider"}, Identity: credential.Identity{Fingerprint: "provider-v1"},
		Material: credential.Material{Kind: credential.KindBasic, Username: []byte("private-user"), Password: []byte("private-password")},
	})
	badKey := []byte("abcdef0123456789abcdef0123456789")
	resolver, err := credential.NewEncryptedFileResolver(path, func(context.Context) ([]byte, error) {
		return append([]byte(nil), badKey...), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	_, err = resolver.Resolve(context.Background(), credential.Request{
		Ref: credential.Ref{ID: "provider"}, Kind: credential.KindBasic,
	}, func(credential.Material) error {
		called = true
		return nil
	})
	if !errors.Is(err, credential.ErrUnavailable) || called {
		t.Fatalf("wrong-key Resolve called=%v err=%v", called, err)
	}
	for _, forbidden := range []string{path, "private-user", "private-password"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("error exposed %q: %v", forbidden, err)
		}
	}
}

func TestEncryptedFileResolverRejectsGroupReadableSecretFile(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	path := filepath.Join(t.TempDir(), "credentials.enc")
	writeCredentialFile(t, path, key, credential.Record{
		Ref: credential.Ref{ID: "provider"}, Identity: credential.Identity{Fingerprint: "provider-v1"},
		Material: credential.Material{Kind: credential.KindBearer, Token: []byte("secret")},
	})
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	resolver, err := credential.NewEncryptedFileResolver(path, func(context.Context) ([]byte, error) {
		return append([]byte(nil), key...), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolver.Resolve(context.Background(), credential.Request{
		Ref: credential.Ref{ID: "provider"}, Kind: credential.KindBearer,
	}, func(credential.Material) error { return nil })
	if !errors.Is(err, credential.ErrUnavailable) {
		t.Fatalf("insecure file error = %v", err)
	}
}

func writeCredentialFile(t *testing.T, path string, key []byte, records ...credential.Record) {
	t.Helper()
	sealed, err := credential.Seal(records, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, sealed, 0o600); err != nil {
		t.Fatal(err)
	}
}
