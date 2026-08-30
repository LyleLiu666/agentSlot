package hook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/LyleLiu666/agentSlot/internal/jsonvalue"
)

const (
	MaxExtensionKeyBytes = 128
	MaxInvocationIDBytes = 128
	MaxSafeReasonBytes   = 1024
	MaxContextInputs     = 16
	MaxContextBytes      = 256 << 10
)

// InvocationID identifies one durable occurrence of one extension
// contribution. Products may choose the ID, but it must remain opaque to the
// framework and stable for the lifetime of the journal entry.
type InvocationID string

func (id InvocationID) Valid() bool {
	return validSafeIdentity(string(id), MaxInvocationIDBytes)
}

// ExtensionDescriptor is the immutable build-time identity of one Chain
// contribution. DefinitionDigest changes whenever the selected definition's
// behaviorally relevant configuration changes.
type ExtensionDescriptor struct {
	Key              string
	DefinitionDigest string
}

func (d ExtensionDescriptor) Validate() error {
	if !validSafeIdentity(d.Key, MaxExtensionKeyBytes) {
		return fmt.Errorf("hook: extension key must be bounded, trimmed, valid UTF-8, and control-free")
	}
	if !ValidDigest(d.DefinitionDigest) {
		return fmt.Errorf("hook: definition digest must be lowercase sha256")
	}
	return nil
}

// BoundaryKind is the finite provider-neutral transaction seam that owns an
// invocation. Product event names are deliberately excluded.
type BoundaryKind string

const (
	BoundaryInputGate        BoundaryKind = "input_gate"
	BoundaryToolPreflight    BoundaryKind = "tool_preflight"
	BoundaryToolResult       BoundaryKind = "tool_result"
	BoundaryCompletion       BoundaryKind = "completion"
	BoundarySessionLifecycle BoundaryKind = "session_lifecycle"
)

func (k BoundaryKind) Valid() bool {
	switch k {
	case BoundaryInputGate, BoundaryToolPreflight, BoundaryToolResult, BoundaryCompletion, BoundarySessionLifecycle:
		return true
	default:
		return false
	}
}

type InvocationStatus string

const (
	InvocationPrepared       InvocationStatus = "prepared"
	InvocationPending        InvocationStatus = "pending"
	InvocationSucceeded      InvocationStatus = "succeeded"
	InvocationFailed         InvocationStatus = "failed"
	InvocationCanceled       InvocationStatus = "canceled"
	InvocationOutcomeUnknown InvocationStatus = "outcome_unknown"
)

func (s InvocationStatus) Valid() bool {
	switch s {
	case InvocationPrepared, InvocationPending, InvocationSucceeded, InvocationFailed, InvocationCanceled, InvocationOutcomeUnknown:
		return true
	default:
		return false
	}
}

func (s InvocationStatus) Terminal() bool {
	return s == InvocationSucceeded || s == InvocationFailed || s == InvocationCanceled || s == InvocationOutcomeUnknown
}

type EffectDisposition string

const (
	EffectNone      EffectDisposition = "none"
	EffectPending   EffectDisposition = "pending"
	EffectApplied   EffectDisposition = "applied"
	EffectDiscarded EffectDisposition = "discarded"
)

func (d EffectDisposition) Valid() bool {
	return d == EffectNone || d == EffectPending || d == EffectApplied || d == EffectDiscarded
}

type ContextDisposition string

const (
	ContextNone      ContextDisposition = "none"
	ContextPending   ContextDisposition = "pending"
	ContextConsumed  ContextDisposition = "consumed"
	ContextDiscarded ContextDisposition = "discarded"
)

func (d ContextDisposition) Valid() bool {
	return d == ContextNone || d == ContextPending || d == ContextConsumed || d == ContextDiscarded
}

// Decision is the complete cross-boundary vocabulary that may be persisted in
// the generic journal. A zero decision is valid for context-only and lifecycle
// results.
type Decision string

const (
	DecisionNone            Decision = ""
	DecisionAccept          Decision = "accept"
	DecisionReject          Decision = "reject"
	DecisionAllow           Decision = "allow"
	DecisionDeny            Decision = "deny"
	DecisionRequireApproval Decision = "require_approval"
	DecisionComplete        Decision = "complete"
	DecisionContinue        Decision = "continue"
)

func (d Decision) Valid() bool {
	switch d {
	case DecisionNone, DecisionAccept, DecisionReject, DecisionAllow, DecisionDeny, DecisionRequireApproval, DecisionComplete, DecisionContinue:
		return true
	default:
		return false
	}
}

type InvocationResult struct {
	Decision Decision
	Reason   string
}

func (r InvocationResult) Validate() error {
	if !r.Decision.Valid() {
		return fmt.Errorf("hook: invalid invocation decision %q", r.Decision)
	}
	if err := ValidateSafeReason(r.Reason); err != nil {
		return err
	}
	return nil
}

// TypedInputFingerprint is the representation-independent identity and
// canonical byte size of one typed value.
type TypedInputFingerprint struct {
	Digest string
	Bytes  int
}

// FingerprintTypedInput marshals a typed provider-neutral value, rejects
// ambiguous JSON, and hashes its canonical data-model representation.
func FingerprintTypedInput(value any) (TypedInputFingerprint, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return TypedInputFingerprint{}, fmt.Errorf("hook: typed input cannot be encoded: %w", err)
	}
	canonical, err := jsonvalue.Canonical(raw)
	if err != nil {
		return TypedInputFingerprint{}, fmt.Errorf("hook: typed input is not unambiguous JSON: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return TypedInputFingerprint{Digest: "sha256:" + hex.EncodeToString(digest[:]), Bytes: len(canonical)}, nil
}

func ValidDigest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func ValidateSafeReason(value string) error {
	if value == "" {
		return nil
	}
	if len(value) > MaxSafeReasonBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return fmt.Errorf("hook: reason must be bounded, trimmed, valid UTF-8")
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.In(character, unicode.Cf, unicode.Zl, unicode.Zp) {
			return fmt.Errorf("hook: reason must be one safe display line")
		}
	}
	return nil
}

func validSafeIdentity(value string, limit int) bool {
	if value == "" || len(value) > limit || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.In(character, unicode.Cf, unicode.Zl, unicode.Zp) {
			return false
		}
	}
	return true
}
