package hook_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LyleLiu666/agentSlot/hook"
)

func TestTypedInputFingerprintUsesJSONValueSemantics(t *testing.T) {
	type input struct {
		Name    string          `json:"name"`
		Payload json.RawMessage `json:"payload"`
	}

	left, err := hook.FingerprintTypedInput(input{Name: "read", Payload: json.RawMessage(`{"path":"a/b","count":1e0}`)})
	if err != nil {
		t.Fatal(err)
	}
	right, err := hook.FingerprintTypedInput(input{Name: "read", Payload: json.RawMessage(`{ "count": 1.0, "path": "a\u002fb" }`)})
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatalf("equivalent typed input fingerprints differ: left=%#v right=%#v", left, right)
	}
	if !strings.HasPrefix(left.Digest, "sha256:") || len(left.Digest) != len("sha256:")+64 || left.Bytes == 0 {
		t.Fatalf("fingerprint = %#v", left)
	}

	if _, err := hook.FingerprintTypedInput(input{Name: "read", Payload: json.RawMessage(`{"path":"a","path":"b"}`)}); err == nil {
		t.Fatal("ambiguous duplicate JSON object members were accepted")
	}
	if _, err := hook.FingerprintTypedInput(input{Name: "read", Payload: json.RawMessage(`{"path":`)}); err == nil {
		t.Fatal("invalid JSON was accepted")
	}
}

func TestExtensionDescriptorAndInvocationVocabularyAreFinite(t *testing.T) {
	valid := hook.ExtensionDescriptor{
		Key:              "profile.pre-tool.read",
		DefinitionDigest: "sha256:" + strings.Repeat("a", 64),
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, descriptor := range []hook.ExtensionDescriptor{
		{},
		{Key: " leading", DefinitionDigest: valid.DefinitionDigest},
		{Key: "line\nbreak", DefinitionDigest: valid.DefinitionDigest},
		{Key: strings.Repeat("x", hook.MaxExtensionKeyBytes+1), DefinitionDigest: valid.DefinitionDigest},
		{Key: valid.Key, DefinitionDigest: "sha256:" + strings.Repeat("A", 64)},
		{Key: valid.Key, DefinitionDigest: strings.Repeat("a", 64)},
	} {
		if err := descriptor.Validate(); err == nil {
			t.Fatalf("invalid descriptor accepted: %#v", descriptor)
		}
	}

	for _, boundary := range []hook.BoundaryKind{
		hook.BoundaryInputGate,
		hook.BoundaryToolPreflight,
		hook.BoundaryToolResult,
		hook.BoundaryCompletion,
		hook.BoundarySessionLifecycle,
	} {
		if !boundary.Valid() {
			t.Fatalf("boundary %q is not valid", boundary)
		}
	}
	if hook.BoundaryKind("product_event").Valid() {
		t.Fatal("arbitrary product boundary was accepted")
	}

	for _, decision := range []hook.Decision{
		hook.DecisionNone,
		hook.DecisionAccept,
		hook.DecisionReject,
		hook.DecisionAllow,
		hook.DecisionDeny,
		hook.DecisionRequireApproval,
		hook.DecisionComplete,
		hook.DecisionContinue,
	} {
		if !decision.Valid() {
			t.Fatalf("decision %q is not valid", decision)
		}
	}
	if hook.Decision("replace_input").Valid() {
		t.Fatal("unbounded extension decision was accepted")
	}
}
