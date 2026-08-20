package model_test

import (
	"context"
	"testing"

	"github.com/LyleLiu666/agentSlot/model"
)

func TestStaticCatalogReturnsDetachedValidatedModels(t *testing.T) {
	descriptor := model.Descriptor{ProviderKey: "provider", ModelID: "model", Title: "Model", Capabilities: testCapabilities()}
	catalog, err := model.NewStaticCatalog(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	models, err := catalog.Models(context.Background())
	if err != nil || len(models) != 1 {
		t.Fatalf("Models = %#v, %v", models, err)
	}
	models[0].Capabilities.Reasoning[0] = model.ReasoningHigh
	again, _ := catalog.Models(context.Background())
	if again[0].Capabilities.Reasoning[0] != model.ReasoningDefault {
		t.Fatal("catalog result aliases stored capabilities")
	}
}

func TestStaticCatalogRejectsDuplicateModel(t *testing.T) {
	descriptor := model.Descriptor{ProviderKey: "provider", ModelID: "model", Title: "Model", Capabilities: testCapabilities()}
	if _, err := model.NewStaticCatalog(descriptor, descriptor); err == nil {
		t.Fatal("duplicate catalog model accepted")
	}
}
