package artifact_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/artifact"
)

func TestArtifactContractUsesStableSlotAndValidatesValues(t *testing.T) {
	if artifact.StoreSlot.ID() != "artifact.store" {
		t.Fatalf("slot ID = %q", artifact.StoreSlot.ID())
	}
	valid := artifact.Metadata{ID: "artifact-1", MediaType: "image/png", Name: "diagram.png", Size: 3}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid metadata: %v", err)
	}
	for _, invalid := range []artifact.Metadata{
		{},
		{ID: " artifact-1 ", MediaType: "image/png"},
		{ID: "artifact-1", MediaType: "IMAGE/PNG"},
		{ID: "artifact-1", MediaType: "image/png; charset=binary"},
		{ID: "artifact-1", MediaType: "image/png", Size: -1},
	} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("invalid metadata accepted: %#v", invalid)
		}
	}
	if err := (artifact.WriteRequest{MediaType: "image/png", Name: "diagram.png", Body: bytes.NewReader([]byte("png"))}).Validate(); err != nil {
		t.Fatalf("valid write request: %v", err)
	}
	if err := (artifact.WriteRequest{MediaType: "image/png"}).Validate(); err == nil {
		t.Fatal("write request without body accepted")
	}
}

func TestArtifactModuleContributesOneTypedStore(t *testing.T) {
	store := &memoryStore{}
	module, err := artifact.NewModule("artifact.memory", store)
	if err != nil {
		t.Fatalf("NewModule: %v", err)
	}
	builder := agentslot.NewBuilder()
	if err := builder.Install(module); err != nil {
		t.Fatalf("Install: %v", err)
	}
	assembly, err := builder.Build(agentslot.RequireOne(artifact.StoreSlot))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	resolved, ok := agentslot.Get(assembly, artifact.StoreSlot)
	if !ok || resolved != store {
		t.Fatalf("resolved store = %#v, %v", resolved, ok)
	}
}

func TestArtifactModuleRejectsInvalidIdentityAndTypedNilStore(t *testing.T) {
	var typedNil *memoryStore
	for _, test := range []struct {
		id    string
		store artifact.ArtifactStore
	}{
		{id: "", store: &memoryStore{}},
		{id: " invalid ", store: &memoryStore{}},
		{id: "artifact.nil", store: typedNil},
	} {
		if _, err := artifact.NewModule(test.id, test.store); err == nil {
			t.Fatalf("NewModule(%q, %T) accepted invalid input", test.id, test.store)
		}
	}
}

type memoryStore struct{}

func (*memoryStore) Write(context.Context, artifact.WriteRequest) (artifact.Metadata, error) {
	return artifact.Metadata{}, errors.New("not implemented")
}

func (*memoryStore) Open(context.Context, string) (artifact.Content, error) {
	return artifact.Content{}, errors.New("not implemented")
}

var _ artifact.ArtifactStore = (*memoryStore)(nil)
