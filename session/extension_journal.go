package session

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/model"
)

// ExtensionSequence is the immutable order of an invocation inside one
// Session aggregate. Status updates retain the original sequence.
type ExtensionSequence uint64

// ExtensionJournalEntry is the durable recovery record for one invocation.
// It stores only typed decisions and bounded diagnostics. Command definitions,
// process environment, and raw protocol streams do not belong here.
type ExtensionJournalEntry struct {
	InvocationID hook.InvocationID
	Sequence     ExtensionSequence
	Descriptor   hook.ExtensionDescriptor
	Boundary     hook.BoundaryKind

	SessionID         agent.SessionID
	RunID             agent.RunID
	StepID            agent.StepID
	TargetStepID      agent.StepID `json:",omitempty"`
	MessageID         agent.MessageID
	ToolCallID        agent.ToolCallID
	GoalID            string              `json:",omitempty"`
	GoalVersion       uint64              `json:",omitempty"`
	LifecyclePhase    hook.LifecyclePhase `json:",omitempty"`
	LifecycleOpenKind hook.OpenKind       `json:",omitempty"`

	InputDigest      string
	PreparedRevision agent.Revision `json:",omitempty"`
	PreparedAt       time.Time
	PendingAt        time.Time `json:",omitempty"`
	FinishedAt       time.Time `json:",omitempty"`

	Status             hook.InvocationStatus
	Result             *hook.InvocationResult `json:",omitempty"`
	ErrorCode          agent.ErrorCode        `json:",omitempty"`
	ErrorReason        string                 `json:",omitempty"`
	EffectDisposition  hook.EffectDisposition
	ContextDisposition hook.ContextDisposition
	ContextInputs      []model.Input `json:",omitempty"`
	ContextDigest      string        `json:",omitempty"`
	ContextBytes       int           `json:",omitempty"`
}

func (e ExtensionJournalEntry) Validate(sessionID agent.SessionID) error {
	if !e.InvocationID.Valid() || e.Sequence == 0 || e.SessionID != sessionID || !sessionID.Valid() {
		return fmt.Errorf("session: extension journal identity is invalid")
	}
	if err := e.Descriptor.Validate(); err != nil {
		return err
	}
	if !e.Boundary.Valid() || !e.Status.Valid() || !e.EffectDisposition.Valid() || !e.ContextDisposition.Valid() || !hook.ValidDigest(e.InputDigest) {
		return fmt.Errorf("session: extension journal vocabulary is invalid")
	}
	if err := e.validateSubject(); err != nil {
		return err
	}
	if (e.Boundary == hook.BoundaryToolPreflight || e.Boundary == hook.BoundaryToolResult || e.Boundary == hook.BoundaryCompletion || e.Boundary == hook.BoundarySessionLifecycle) && e.PreparedRevision == 0 {
		return fmt.Errorf("session: execution-boundary extension requires its prepared revision")
	}
	if e.PreparedAt.IsZero() {
		return fmt.Errorf("session: extension journal requires prepared time")
	}
	if !e.PendingAt.IsZero() && e.PendingAt.Before(e.PreparedAt) {
		return fmt.Errorf("session: extension pending time precedes prepared time")
	}
	if !e.FinishedAt.IsZero() && (e.FinishedAt.Before(e.PreparedAt) || (!e.PendingAt.IsZero() && e.FinishedAt.Before(e.PendingAt))) {
		return fmt.Errorf("session: extension finished time is inconsistent")
	}
	if err := hook.ValidateSafeReason(e.ErrorReason); err != nil {
		return err
	}
	if err := e.validateStatusShape(); err != nil {
		return err
	}
	return e.validateContext(sessionID)
}

func (e ExtensionJournalEntry) validateSubject() error {
	if (e.RunID != "" && !e.RunID.Valid()) || (e.StepID != "" && !e.StepID.Valid()) ||
		(e.TargetStepID != "" && !e.TargetStepID.Valid()) ||
		(e.MessageID != "" && !e.MessageID.Valid()) || (e.ToolCallID != "" && !e.ToolCallID.Valid()) {
		return fmt.Errorf("session: extension subject identity is invalid")
	}
	switch e.Boundary {
	case hook.BoundaryInputGate:
		if !e.MessageID.Valid() || e.RunID != "" || e.StepID != "" || e.TargetStepID != "" || e.ToolCallID != "" || e.GoalID != "" || e.GoalVersion != 0 {
			return fmt.Errorf("session: input gate subject is invalid")
		}
	case hook.BoundaryToolPreflight:
		if !e.RunID.Valid() || !e.StepID.Valid() || !e.ToolCallID.Valid() || e.GoalID != "" || e.GoalVersion != 0 {
			return fmt.Errorf("session: tool extension subject is incomplete")
		}
		if e.TargetStepID != "" {
			return fmt.Errorf("session: tool preflight cannot target a later step")
		}
	case hook.BoundaryToolResult:
		if !e.RunID.Valid() || !e.StepID.Valid() || !e.TargetStepID.Valid() || !e.ToolCallID.Valid() || e.TargetStepID == e.StepID || e.GoalID != "" || e.GoalVersion != 0 {
			return fmt.Errorf("session: tool result extension subject is incomplete")
		}
	case hook.BoundaryCompletion:
		if !e.RunID.Valid() || !e.StepID.Valid() || !e.TargetStepID.Valid() || e.TargetStepID == e.StepID || !e.MessageID.Valid() || e.ToolCallID != "" ||
			((e.GoalID == "") != (e.GoalVersion == 0)) ||
			(e.GoalID != "" && (hook.CompletionGoalCandidate{GoalID: e.GoalID, Version: e.GoalVersion}).Validate() != nil) {
			return fmt.Errorf("session: completion subject is invalid")
		}
	case hook.BoundarySessionLifecycle:
		if e.RunID != "" || e.StepID != "" || e.TargetStepID != "" || e.MessageID != "" || e.ToolCallID != "" || e.GoalID != "" || e.GoalVersion != 0 ||
			!e.LifecyclePhase.Valid() ||
			(e.LifecyclePhase == hook.LifecycleOpen && !e.LifecycleOpenKind.Valid()) ||
			(e.LifecyclePhase == hook.LifecycleClose && e.LifecycleOpenKind != "") {
			return fmt.Errorf("session: lifecycle subject must be Session-scoped")
		}
	default:
		if e.LifecyclePhase != "" || e.LifecycleOpenKind != "" {
			return fmt.Errorf("session: non-lifecycle extension carries lifecycle identity")
		}
	}
	return nil
}

func (e ExtensionJournalEntry) validateStatusShape() error {
	switch e.Status {
	case hook.InvocationPrepared:
		if !e.PendingAt.IsZero() || !e.FinishedAt.IsZero() || e.Result != nil || e.ErrorCode != "" || e.ErrorReason != "" || e.EffectDisposition != hook.EffectNone {
			return fmt.Errorf("session: prepared extension carries execution outcome")
		}
	case hook.InvocationPending:
		if e.PendingAt.IsZero() || !e.FinishedAt.IsZero() || e.Result != nil || e.ErrorCode != "" || e.ErrorReason != "" || e.EffectDisposition != hook.EffectNone {
			return fmt.Errorf("session: pending extension shape is invalid")
		}
	default:
		if e.FinishedAt.IsZero() || e.EffectDisposition == hook.EffectNone {
			return fmt.Errorf("session: terminal extension requires finished time and effect disposition")
		}
		if e.Status == hook.InvocationSucceeded {
			if e.Result == nil || e.ErrorCode != "" || e.ErrorReason != "" {
				return fmt.Errorf("session: succeeded extension result is invalid")
			}
			if err := e.Result.Validate(); err != nil {
				return err
			}
			if err := e.validateBoundaryResult(); err != nil {
				return err
			}
		} else {
			if e.Result != nil || !validExtensionErrorCode(string(e.ErrorCode)) {
				return fmt.Errorf("session: unsuccessful extension requires a safe error code and no result")
			}
		}
	}
	return nil
}

func (e ExtensionJournalEntry) validateBoundaryResult() error {
	hasContext := e.ContextDisposition != hook.ContextNone
	switch e.Boundary {
	case hook.BoundaryInputGate:
		if e.Result.Decision != hook.DecisionAccept && e.Result.Decision != hook.DecisionReject {
			return fmt.Errorf("session: input gate result has an invalid decision")
		}
		if e.Result.Decision == hook.DecisionReject && hasContext {
			return fmt.Errorf("session: rejected input cannot contribute context")
		}
	case hook.BoundaryToolPreflight:
		if e.Result.Decision != hook.DecisionAllow && e.Result.Decision != hook.DecisionDeny && e.Result.Decision != hook.DecisionRequireApproval {
			return fmt.Errorf("session: tool preflight result has an invalid decision")
		}
		if hasContext {
			return fmt.Errorf("session: tool preflight cannot contribute context")
		}
	case hook.BoundaryToolResult, hook.BoundarySessionLifecycle:
		if e.Result.Decision != hook.DecisionNone {
			return fmt.Errorf("session: context-only extension cannot persist a decision")
		}
	case hook.BoundaryCompletion:
		if e.Result.Decision != hook.DecisionComplete && e.Result.Decision != hook.DecisionContinue {
			return fmt.Errorf("session: completion result has an invalid decision")
		}
		if (e.Result.Decision == hook.DecisionContinue) != hasContext {
			return fmt.Errorf("session: completion continuation and context must agree")
		}
	}
	return nil
}

func (e ExtensionJournalEntry) validateContext(sessionID agent.SessionID) error {
	switch e.ContextDisposition {
	case hook.ContextNone:
		if len(e.ContextInputs) != 0 || e.ContextDigest != "" || e.ContextBytes != 0 {
			return fmt.Errorf("session: context-free extension carries context metadata")
		}
	case hook.ContextPending:
		if e.Status != hook.InvocationSucceeded ||
			(e.EffectDisposition != hook.EffectPending && e.EffectDisposition != hook.EffectApplied) ||
			len(e.ContextInputs) == 0 || len(e.ContextInputs) > hook.MaxContextInputs {
			return fmt.Errorf("session: pending extension context is not attached to a successful effect")
		}
		if err := validateExtensionContextInputs(e.ContextInputs, sessionID); err != nil {
			return err
		}
		if e.Boundary == hook.BoundaryToolResult || e.Boundary == hook.BoundaryCompletion {
			for _, input := range e.ContextInputs {
				if input.Message == nil || input.SystemPrompt != nil || input.ToolCall != nil || input.ToolResult != nil ||
					input.Message.RunID != e.RunID || input.Message.StepID != e.TargetStepID || input.Message.Role != agent.RoleUser {
					return fmt.Errorf("session: extension context is not bound to its exact target step")
				}
			}
		}
		fingerprint, err := hook.FingerprintTypedInput(e.ContextInputs)
		if err != nil || fingerprint.Digest != e.ContextDigest || fingerprint.Bytes != e.ContextBytes || fingerprint.Bytes > hook.MaxContextBytes {
			return fmt.Errorf("session: extension context fingerprint is invalid")
		}
	case hook.ContextConsumed, hook.ContextDiscarded:
		if len(e.ContextInputs) != 0 || !hook.ValidDigest(e.ContextDigest) || e.ContextBytes <= 0 {
			return fmt.Errorf("session: terminal context disposition must retain only its fingerprint")
		}
		if e.ContextDisposition == hook.ContextConsumed && e.EffectDisposition != hook.EffectApplied {
			return fmt.Errorf("session: consumed context requires applied effect")
		}
		if e.ContextDisposition == hook.ContextDiscarded && e.EffectDisposition != hook.EffectApplied && e.EffectDisposition != hook.EffectDiscarded {
			return fmt.Errorf("session: discarded context requires a finalized effect")
		}
	}
	return nil
}

func validateExtensionContextInputs(inputs []model.Input, sessionID agent.SessionID) error {
	for _, input := range inputs {
		wrongMessageSession := input.Message != nil && input.Message.SessionID != sessionID
		wrongCallSession := input.ToolCall != nil && input.ToolCall.SessionID != sessionID
		if !input.Valid() || input.SystemPrompt != nil || wrongMessageSession || wrongCallSession {
			return fmt.Errorf("session: invalid extension context input")
		}
	}
	if err := model.ValidateInputs(inputs); err != nil {
		return fmt.Errorf("session: extension context violates the model protocol")
	}
	return nil
}

func validExtensionErrorCode(value string) bool {
	if value == "" || len(value) > 128 || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) || unicode.In(character, unicode.Cf, unicode.Zl, unicode.Zp) {
			return false
		}
	}
	return true
}

func applyExtensionJournal(snapshot *Snapshot, next ExtensionJournalEntry) error {
	if err := next.Validate(snapshot.Session.ID); err != nil {
		return invalid("session.commit", err.Error())
	}
	index := int(next.Sequence) - 1
	if index == len(snapshot.ExtensionJournal) {
		if next.Status != hook.InvocationPrepared || next.Sequence != ExtensionSequence(len(snapshot.ExtensionJournal)+1) {
			return extensionConflict("new extension invocation must append the next prepared sequence")
		}
		for _, existing := range snapshot.ExtensionJournal {
			if existing.InvocationID == next.InvocationID {
				return extensionConflict("extension invocation ID is already assigned")
			}
		}
		snapshot.ExtensionJournal = append(snapshot.ExtensionJournal, cloneExtensionJournalEntry(next))
		return nil
	}
	if index < 0 || index >= len(snapshot.ExtensionJournal) {
		return extensionConflict("extension sequence is not contiguous")
	}

	current := snapshot.ExtensionJournal[index]
	if !sameExtensionIdentity(current, next) {
		return extensionConflict("extension update changed immutable invocation identity")
	}
	if err := validateExtensionTransition(current, next); err != nil {
		return err
	}
	snapshot.ExtensionJournal[index] = cloneExtensionJournalEntry(next)
	return nil
}

func validateExtensionTransition(current, next ExtensionJournalEntry) error {
	switch current.Status {
	case hook.InvocationPrepared:
		if next.Status != hook.InvocationPending && next.Status != hook.InvocationFailed && next.Status != hook.InvocationCanceled {
			return extensionConflict("prepared extension may only become pending, failed, or canceled")
		}
		if !next.PendingAt.IsZero() && next.Status != hook.InvocationPending {
			return extensionConflict("direct terminal extension cannot invent a pending boundary")
		}
		if next.Status == hook.InvocationFailed && next.EffectDisposition != hook.EffectPending {
			return extensionConflict("failed extension outcome must await effect application")
		}
		if next.Status == hook.InvocationCanceled && next.EffectDisposition != hook.EffectDiscarded {
			return extensionConflict("administratively canceled extension must discard its effect")
		}
	case hook.InvocationPending:
		if !next.Status.Terminal() {
			return extensionConflict("pending extension requires a terminal outcome")
		}
		if !next.PendingAt.Equal(current.PendingAt) || next.EffectDisposition != hook.EffectPending {
			return extensionConflict("pending identity is immutable and terminal effect must await application")
		}
	default:
		if next.Status != current.Status || !sameExtensionTerminalOutcome(current, next) {
			return extensionConflict("terminal extension outcome is immutable")
		}
		effectProgressed := current.EffectDisposition != next.EffectDisposition
		switch current.EffectDisposition {
		case hook.EffectPending:
			if next.EffectDisposition != hook.EffectPending && next.EffectDisposition != hook.EffectApplied && next.EffectDisposition != hook.EffectDiscarded {
				return extensionConflict("pending extension effect has an invalid transition")
			}
		case hook.EffectApplied, hook.EffectDiscarded:
			if next.EffectDisposition != current.EffectDisposition {
				return extensionConflict("finalized extension effect is immutable")
			}
		default:
			return extensionConflict("terminal extension effect is invalid")
		}
		contextProgressed := current.ContextDisposition != next.ContextDisposition
		switch current.ContextDisposition {
		case hook.ContextNone:
			if next.ContextDisposition != hook.ContextNone {
				return extensionConflict("context-free extension cannot acquire context")
			}
		case hook.ContextPending:
			if next.ContextDisposition != hook.ContextPending && next.ContextDisposition != hook.ContextConsumed && next.ContextDisposition != hook.ContextDiscarded {
				return extensionConflict("pending extension context has an invalid transition")
			}
			if next.ContextDisposition == hook.ContextPending && !reflect.DeepEqual(current.ContextInputs, next.ContextInputs) {
				return extensionConflict("pending extension context inputs are immutable")
			}
		case hook.ContextConsumed, hook.ContextDiscarded:
			if next.ContextDisposition != current.ContextDisposition {
				return extensionConflict("finalized extension context is immutable")
			}
		}
		if !effectProgressed && !contextProgressed {
			return extensionConflict("terminal extension disposition did not advance")
		}
	}
	return nil
}

func sameExtensionIdentity(left, right ExtensionJournalEntry) bool {
	return left.InvocationID == right.InvocationID && left.Sequence == right.Sequence &&
		left.Descriptor == right.Descriptor && left.Boundary == right.Boundary &&
		left.SessionID == right.SessionID && left.RunID == right.RunID && left.StepID == right.StepID && left.TargetStepID == right.TargetStepID &&
		left.MessageID == right.MessageID && left.ToolCallID == right.ToolCallID && left.GoalID == right.GoalID && left.GoalVersion == right.GoalVersion &&
		left.LifecyclePhase == right.LifecyclePhase && left.LifecycleOpenKind == right.LifecycleOpenKind &&
		left.InputDigest == right.InputDigest && left.PreparedRevision == right.PreparedRevision && left.PreparedAt.Equal(right.PreparedAt)
}

func sameExtensionTerminalOutcome(left, right ExtensionJournalEntry) bool {
	return left.PendingAt.Equal(right.PendingAt) && left.FinishedAt.Equal(right.FinishedAt) &&
		reflect.DeepEqual(left.Result, right.Result) && left.ErrorCode == right.ErrorCode && left.ErrorReason == right.ErrorReason &&
		left.ContextDigest == right.ContextDigest && left.ContextBytes == right.ContextBytes
}

func extensionConflict(message string) error {
	return agent.NewCodedError(agent.ErrorConflict, agent.CodeJournalInvariant, "session.commit", message, nil)
}

const (
	DefaultExtensionPageLimit = 50
	MaxExtensionPageLimit     = 100
)

type ExtensionPageRequest struct {
	SessionID               agent.SessionID
	BeforeExtensionSequence ExtensionSequence
	Limit                   int
}

type ExtensionPage struct {
	AgentID     agent.AgentID
	WorkspaceID agent.WorkspaceID
	Revision    agent.Revision
	Diagnostics []ExtensionDiagnostic
	HasMore     bool
}

// ExtensionDiagnostic is the safe detached projection shared by Store and
// Gateway. It intentionally omits ContextInputs and all process protocol data.
type ExtensionDiagnostic struct {
	InvocationID      hook.InvocationID
	Sequence          ExtensionSequence
	Descriptor        hook.ExtensionDescriptor
	Boundary          hook.BoundaryKind
	SessionID         agent.SessionID
	RunID             agent.RunID
	StepID            agent.StepID
	TargetStepID      agent.StepID
	MessageID         agent.MessageID
	ToolCallID        agent.ToolCallID
	LifecyclePhase    hook.LifecyclePhase
	LifecycleOpenKind hook.OpenKind
	InputDigest       string
	PreparedRevision  agent.Revision
	PreparedAt        time.Time
	PendingAt         time.Time
	FinishedAt        time.Time
	Status            hook.InvocationStatus
	Decision          hook.Decision
	Reason            string
	ErrorCode         agent.ErrorCode
	ErrorReason       string
	Effect            hook.EffectDisposition
	Context           hook.ContextDisposition
	ContextDigest     string
	ContextBytes      int
}

func extensionPage(entries []ExtensionJournalEntry, request ExtensionPageRequest) (ExtensionPage, error) {
	if !request.SessionID.Valid() {
		return ExtensionPage{}, invalid("session.extension_diagnostics", "session ID is required")
	}
	limit := request.Limit
	if limit == 0 {
		limit = DefaultExtensionPageLimit
	}
	if limit < 1 || limit > MaxExtensionPageLimit {
		return ExtensionPage{}, invalid("session.extension_diagnostics", fmt.Sprintf("limit must be between 1 and %d", MaxExtensionPageLimit))
	}
	start := len(entries)
	if request.BeforeExtensionSequence != 0 {
		start = sort.Search(len(entries), func(index int) bool {
			return entries[index].Sequence >= request.BeforeExtensionSequence
		})
	}
	page := ExtensionPage{Diagnostics: make([]ExtensionDiagnostic, 0, min(limit, start))}
	for index := start - 1; index >= 0 && len(page.Diagnostics) < limit; index-- {
		page.Diagnostics = append(page.Diagnostics, extensionDiagnostic(entries[index]))
	}
	page.HasMore = start-len(page.Diagnostics) > 0
	return page, nil
}

func extensionDiagnostic(entry ExtensionJournalEntry) ExtensionDiagnostic {
	view := ExtensionDiagnostic{
		InvocationID: entry.InvocationID, Sequence: entry.Sequence, Descriptor: entry.Descriptor, Boundary: entry.Boundary,
		SessionID: entry.SessionID, RunID: entry.RunID, StepID: entry.StepID, TargetStepID: entry.TargetStepID, MessageID: entry.MessageID, ToolCallID: entry.ToolCallID,
		LifecyclePhase: entry.LifecyclePhase, LifecycleOpenKind: entry.LifecycleOpenKind,
		InputDigest: entry.InputDigest, PreparedRevision: entry.PreparedRevision,
		PreparedAt: entry.PreparedAt, PendingAt: entry.PendingAt, FinishedAt: entry.FinishedAt,
		Status: entry.Status, ErrorCode: entry.ErrorCode, ErrorReason: entry.ErrorReason,
		Effect: entry.EffectDisposition, Context: entry.ContextDisposition, ContextDigest: entry.ContextDigest, ContextBytes: entry.ContextBytes,
	}
	if entry.Result != nil {
		view.Decision = entry.Result.Decision
		view.Reason = entry.Result.Reason
	}
	return view
}
