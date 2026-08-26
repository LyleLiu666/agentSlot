package openaicompat

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/LyleLiu666/agentSlot/model"
)

func TestProviderErrorMessageNormalizesControlCharactersAndBoundsUTF8(t *testing.T) {
	message := "  first\n\x1b[31msecond  " + strings.Repeat("界", model.MaxErrorMessageBytes)
	body := `{"error":{"message":` + quoteJSON(t, message) + `}}`
	got := providerErrorMessage(strings.NewReader(body))
	if !strings.HasPrefix(got, "first [31msecond ") || strings.ContainsAny(got, "\n\r\t\x1b") {
		t.Fatalf("normalized message = %q", got)
	}
	if len(got) > model.MaxErrorMessageBytes || !utf8.ValidString(got) || model.ValidateErrorMessage(got) != nil {
		t.Fatalf("bounded message bytes=%d valid=%t", len(got), utf8.ValidString(got))
	}
}

func TestProviderErrorMessageRejectsOversizedAndUnrecognizedBodies(t *testing.T) {
	if got := providerErrorMessage(strings.NewReader(strings.Repeat("x", maxProviderErrorBodyBytes+1))); got != "" {
		t.Fatalf("oversized body produced message %q", got)
	}
	if got := providerErrorMessage(strings.NewReader(`{"internal":"private"}`)); got != "" {
		t.Fatalf("unrecognized body produced message %q", got)
	}
}

func quoteJSON(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
