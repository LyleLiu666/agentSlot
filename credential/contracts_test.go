package credential_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/credential"
)

func TestResolverContractSupportsDistinctCredentialShapes(t *testing.T) {
	resolver, err := credential.NewMemoryResolver(
		credential.Record{
			Ref: credential.Ref{ID: "model-primary"}, Identity: credential.Identity{Fingerprint: "credential-model-v1"},
			Material: credential.Material{Kind: credential.KindBearer, Token: []byte("bearer-secret")},
		},
		credential.Record{
			Ref: credential.Ref{ID: "git-basic"}, Identity: credential.Identity{Fingerprint: "credential-git-v1"},
			Material: credential.Material{Kind: credential.KindBasic, Username: []byte("alice"), Password: []byte("password-secret")},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	var exposed []byte
	identity, err := resolver.Resolve(context.Background(), credential.Request{
		Ref: credential.Ref{ID: "model-primary"}, Kind: credential.KindBearer,
	}, func(material credential.Material) error {
		exposed = material.Token
		if string(material.Token) != "bearer-secret" || len(material.Username) != 0 || len(material.Password) != 0 {
			t.Fatalf("bearer material = %#v", material)
		}
		return nil
	})
	if err != nil || identity.Fingerprint != "credential-model-v1" {
		t.Fatalf("bearer resolve = %#v, %v", identity, err)
	}
	for _, value := range exposed {
		if value != 0 {
			t.Fatal("callback material remained readable after Resolve returned")
		}
	}

	identity, err = resolver.Resolve(context.Background(), credential.Request{
		Ref: credential.Ref{ID: "git-basic"}, Kind: credential.KindBasic,
	}, func(material credential.Material) error {
		if string(material.Username) != "alice" || string(material.Password) != "password-secret" || len(material.Token) != 0 {
			t.Fatalf("basic material = %#v", material)
		}
		return nil
	})
	if err != nil || identity.Fingerprint != "credential-git-v1" {
		t.Fatalf("basic resolve = %#v, %v", identity, err)
	}
}

func TestResolverRejectsWrongShapeWithoutExposingMaterial(t *testing.T) {
	resolver, err := credential.NewMemoryResolver(credential.Record{
		Ref: credential.Ref{ID: "model"}, Identity: credential.Identity{Fingerprint: "model-v1"},
		Material: credential.Material{Kind: credential.KindBearer, Token: []byte("private-value")},
	})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	_, err = resolver.Resolve(context.Background(), credential.Request{
		Ref: credential.Ref{ID: "model"}, Kind: credential.KindBasic,
	}, func(credential.Material) error {
		called = true
		return nil
	})
	if !errors.Is(err, credential.ErrKindMismatch) || called || strings.Contains(err.Error(), "private-value") {
		t.Fatalf("wrong-shape result called=%v err=%v", called, err)
	}
}

func TestResolverModuleDescriptionContainsNoCredentialValues(t *testing.T) {
	resolver, err := credential.NewMemoryResolver(credential.Record{
		Ref: credential.Ref{ID: "safe-reference"}, Identity: credential.Identity{Fingerprint: "safe-fingerprint"},
		Material: credential.Material{Kind: credential.KindBearer, Token: []byte("must-not-appear")},
	})
	if err != nil {
		t.Fatal(err)
	}
	module, err := credential.NewModule("credential.test", resolver)
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := agentslot.NewApplication("credential-contract", []agentslot.Module{module}, agentslot.RequireOne(credential.ResolverSlot)).Build()
	if err != nil {
		t.Fatal(err)
	}
	description, err := json.Marshal(assembly.Describe())
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"must-not-appear", "safe-reference", "safe-fingerprint"} {
		if strings.Contains(string(description), forbidden) {
			t.Fatalf("Assembly description exposed %q: %s", forbidden, description)
		}
	}
}

func TestResolverHonorsCanceledContextBeforeExposure(t *testing.T) {
	resolver, err := credential.NewMemoryResolver(credential.Record{
		Ref: credential.Ref{ID: "model"}, Identity: credential.Identity{Fingerprint: "model-v1"},
		Material: credential.Material{Kind: credential.KindBearer, Token: []byte("private")},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	_, err = resolver.Resolve(ctx, credential.Request{Ref: credential.Ref{ID: "model"}, Kind: credential.KindBearer}, func(credential.Material) error {
		called = true
		return nil
	})
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("canceled Resolve called=%v err=%v", called, err)
	}
}
