package model_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/LyleLiu666/agentSlot/model"
)

func TestModalityJSONUsesStableSemanticNamesInsteadOfByteEncoding(t *testing.T) {
	encoded, err := json.Marshal([]model.Modality{model.ModalityText, model.ModalityImage, model.ModalityAudio})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `["text","image","audio"]` {
		t.Fatalf("modality JSON = %s", encoded)
	}
	var decoded []model.Modality
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, model.AllModalities()) {
		t.Fatalf("decoded modalities = %#v", decoded)
	}
	for _, invalid := range []string{`"video"`, `1`, `null`} {
		var modality model.Modality
		if err := json.Unmarshal([]byte(invalid), &modality); !errors.Is(err, model.ErrUnknownModality) {
			t.Fatalf("Unmarshal(%s) error = %v, want ErrUnknownModality", invalid, err)
		}
	}
}

func TestCanonicalModalitiesAreClosedAndStable(t *testing.T) {
	want := []struct {
		modality model.Modality
		name     string
	}{
		{modality: model.ModalityText, name: "text"},
		{modality: model.ModalityImage, name: "image"},
		{modality: model.ModalityAudio, name: "audio"},
	}

	all := model.AllModalities()
	if len(all) != len(want) {
		t.Fatalf("AllModalities() returned %d modalities, want %d", len(all), len(want))
	}

	for i, expected := range want {
		if all[i] != expected.modality {
			t.Fatalf("AllModalities()[%d] = %v, want %v", i, all[i], expected.modality)
		}
		if got := expected.modality.String(); got != expected.name {
			t.Errorf("%v.String() = %q, want %q", expected.modality, got, expected.name)
		}
		parsed, err := model.ParseModality(expected.name)
		if err != nil {
			t.Fatalf("ParseModality(%q): %v", expected.name, err)
		}
		if parsed != expected.modality {
			t.Errorf("ParseModality(%q) = %v, want %v", expected.name, parsed, expected.modality)
		}
	}

	if _, err := model.ParseModality("video"); !errors.Is(err, model.ErrUnknownModality) {
		t.Fatalf("ParseModality(video) error = %v, want ErrUnknownModality", err)
	}
	if model.Modality(99).Valid() {
		t.Fatal("unknown modality reported as valid")
	}

	all[0] = model.ModalityAudio
	if reflect.DeepEqual(all, model.AllModalities()) {
		t.Fatal("AllModalities returned mutable shared state")
	}
}

func TestCapabilitiesDeclareInputAndOutputSeparately(t *testing.T) {
	capabilities := model.Capabilities{
		InputModalities:  []model.Modality{model.ModalityText, model.ModalityImage},
		OutputModalities: []model.Modality{model.ModalityText},
		ToolCalling:      true,
	}

	if err := capabilities.Validate(); err != nil {
		t.Fatalf("Validate(): %v", err)
	}
	if !capabilities.SupportsInput(model.ModalityImage) {
		t.Fatal("image input should be supported")
	}
	if capabilities.SupportsInput(model.ModalityAudio) {
		t.Fatal("audio input should not be supported")
	}
	if !capabilities.SupportsOutput(model.ModalityText) {
		t.Fatal("text output should be supported")
	}
	if capabilities.SupportsOutput(model.ModalityImage) {
		t.Fatal("image output should not be supported")
	}
	if !capabilities.ToolCalling {
		t.Fatal("tool calling must remain a separate capability from media modalities")
	}
}

func TestCapabilitiesRejectInvalidDeclarations(t *testing.T) {
	tests := map[string]model.Capabilities{
		"missing input": {
			OutputModalities: []model.Modality{model.ModalityText},
		},
		"missing output": {
			InputModalities: []model.Modality{model.ModalityText},
		},
		"unknown input": {
			InputModalities:  []model.Modality{99},
			OutputModalities: []model.Modality{model.ModalityText},
		},
		"duplicate output": {
			InputModalities:  []model.Modality{model.ModalityText},
			OutputModalities: []model.Modality{model.ModalityText, model.ModalityText},
		},
	}

	for name, capabilities := range tests {
		t.Run(name, func(t *testing.T) {
			if err := capabilities.Validate(); err == nil {
				t.Fatal("Validate() succeeded, want error")
			}
		})
	}
}
