// Package model defines provider-neutral model vocabulary shared by AgentSlot
// model contracts and adapters.
package model

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrUnknownModality reports a media modality outside AgentSlot's closed
// standard vocabulary.
var ErrUnknownModality = errors.New("unknown model modality")

// Modality identifies one semantic media type accepted or produced by a
// model. Tool calls are actions, not media modalities.
type Modality uint8

const (
	ModalityText Modality = iota + 1
	ModalityImage
	ModalityAudio
)

var allModalities = [...]Modality{
	ModalityText,
	ModalityImage,
	ModalityAudio,
}

// AllModalities returns AgentSlot's complete standard modality vocabulary in
// stable order.
func AllModalities() []Modality {
	return append([]Modality(nil), allModalities[:]...)
}

// Valid reports whether the modality belongs to the standard vocabulary.
func (m Modality) Valid() bool {
	switch m {
	case ModalityText, ModalityImage, ModalityAudio:
		return true
	default:
		return false
	}
}

// String returns the stable wire name of the modality.
func (m Modality) String() string {
	switch m {
	case ModalityText:
		return "text"
	case ModalityImage:
		return "image"
	case ModalityAudio:
		return "audio"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(m))
	}
}

// MarshalJSON exposes the stable semantic name. This method is also required
// to prevent slices of the uint8-backed Modality type from being mistaken for
// opaque bytes and encoded as base64 by encoding/json.
func (m Modality) MarshalJSON() ([]byte, error) {
	if !m.Valid() {
		return nil, fmt.Errorf("%w %q", ErrUnknownModality, m)
	}
	return json.Marshal(m.String())
}

// UnmarshalJSON accepts only a stable semantic name from the closed standard
// vocabulary. Numeric values and null are not portable wire representations.
func (m *Modality) UnmarshalJSON(data []byte) error {
	if m == nil {
		return fmt.Errorf("%w: nil target", ErrUnknownModality)
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("%w: %v", ErrUnknownModality, err)
	}
	parsed, err := ParseModality(value)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}

// ParseModality parses one stable modality wire name.
func ParseModality(value string) (Modality, error) {
	switch value {
	case "text":
		return ModalityText, nil
	case "image":
		return ModalityImage, nil
	case "audio":
		return ModalityAudio, nil
	default:
		return 0, fmt.Errorf("%w %q", ErrUnknownModality, value)
	}
}

// Capabilities declares a selected model's semantic input and output media.
// ToolCalling is separate because a tool call is an action with structured
// arguments, not another media modality.
type Capabilities struct {
	InputModalities  []Modality
	OutputModalities []Modality
	ToolCalling      bool
}

// Validate rejects empty, unknown, or duplicate modality declarations.
func (c Capabilities) Validate() error {
	if err := validateModalities("input", c.InputModalities); err != nil {
		return err
	}
	if err := validateModalities("output", c.OutputModalities); err != nil {
		return err
	}
	return nil
}

// SupportsInput reports whether the selected model accepts the modality.
func (c Capabilities) SupportsInput(modality Modality) bool {
	return contains(c.InputModalities, modality)
}

// SupportsOutput reports whether the selected model can produce the modality.
func (c Capabilities) SupportsOutput(modality Modality) bool {
	return contains(c.OutputModalities, modality)
}

func validateModalities(direction string, modalities []Modality) error {
	if len(modalities) == 0 {
		return fmt.Errorf("model: at least one %s modality is required", direction)
	}
	seen := make(map[Modality]struct{}, len(modalities))
	for _, modality := range modalities {
		if !modality.Valid() {
			return fmt.Errorf("model: %s: %w %q", direction, ErrUnknownModality, modality)
		}
		if _, exists := seen[modality]; exists {
			return fmt.Errorf("model: duplicate %s modality %q", direction, modality)
		}
		seen[modality] = struct{}{}
	}
	return nil
}

func contains(modalities []Modality, candidate Modality) bool {
	if !candidate.Valid() {
		return false
	}
	for _, modality := range modalities {
		if modality == candidate {
			return true
		}
	}
	return false
}
