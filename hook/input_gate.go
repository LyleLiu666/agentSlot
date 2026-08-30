package hook

import (
	"context"
	"errors"
	"fmt"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/model"
)

// InputGateSlot is the ordered, optional pre-submission decision chain. Each
// contribution owns one durable invocation and cannot mutate the proposed
// user input or obtain Runtime/Store authority.
var InputGateSlot = agentslot.Chain[InputGate]("hook.input_gate")

type InputGate interface {
	Descriptor() ExtensionDescriptor
	Evaluate(context.Context, InputGateView) (InputGateResult, error)
}

type InputOperation string

const (
	InputSend       InputOperation = "send"
	InputSteer      InputOperation = "steer"
	InputEditQueued InputOperation = "edit_queued"
)

func (o InputOperation) Valid() bool {
	return o == InputSend || o == InputSteer || o == InputEditQueued
}

// InputGateView is a detached read-only proposal. PreviousInputDigest is
// present only for edits so Runtime can prove which queued content was viewed
// without exposing a second mutable copy through the result.
type InputGateView struct {
	InvocationID        InvocationID
	Operation           InputOperation
	SessionID           agent.SessionID
	AgentID             agent.AgentID
	WorkspaceID         agent.WorkspaceID
	Revision            agent.Revision
	MessageID           agent.MessageID
	ClientMessageID     agent.ClientMessageID
	Input               agent.MessageInput
	PreviousInputDigest string `json:",omitempty"`
}

func (v InputGateView) Validate() error {
	if !v.InvocationID.Valid() || !v.Operation.Valid() || !v.SessionID.Valid() || !v.AgentID.Valid() ||
		!v.WorkspaceID.Valid() || v.Revision == 0 || !v.MessageID.Valid() || !v.Input.Valid() ||
		(v.ClientMessageID != "" && !v.ClientMessageID.Valid()) {
		return fmt.Errorf("hook: invalid input gate view")
	}
	if v.Operation == InputEditQueued {
		if !ValidDigest(v.PreviousInputDigest) {
			return fmt.Errorf("hook: edit input gate requires the previous input digest")
		}
	} else if v.PreviousInputDigest != "" {
		return fmt.Errorf("hook: only edit input gate may carry a previous input digest")
	}
	return nil
}

// InputGateResult deliberately has no replacement-input field. Context is a
// separate model contribution consumed only if the unchanged proposal is
// later claimed into one Run/Step.
type InputGateResult struct {
	Decision Decision
	Reason   string
	Context  []model.Input
}

// InvocationFailure lets a product adapter preserve a finite safe failure
// code without exposing implementation error text to Runtime branches. Status
// is restricted to unsuccessful terminal invocation states.
type InvocationFailure struct {
	Status InvocationStatus
	Code   agent.ErrorCode
	Reason string
	Cause  error
}

func (e *InvocationFailure) Error() string {
	if e == nil {
		return "<nil>"
	}
	return "hook: extension invocation failed"
}

func (e *InvocationFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *InvocationFailure) Validate() error {
	if e == nil || (e.Status != InvocationFailed && e.Status != InvocationCanceled && e.Status != InvocationOutcomeUnknown) ||
		!validSafeIdentity(string(e.Code), MaxExtensionKeyBytes) {
		return errors.New("hook: invalid invocation failure")
	}
	return ValidateSafeReason(e.Reason)
}

func (r InputGateResult) Validate(sessionID agent.SessionID) error {
	if r.Decision != DecisionAccept && r.Decision != DecisionReject {
		return fmt.Errorf("hook: input gate decision must be accept or reject")
	}
	if err := ValidateSafeReason(r.Reason); err != nil {
		return err
	}
	if r.Decision == DecisionReject {
		if r.Reason == "" || len(r.Context) != 0 {
			return fmt.Errorf("hook: rejected input requires a reason and cannot contribute context")
		}
		return nil
	}
	if len(r.Context) > MaxContextInputs {
		return fmt.Errorf("hook: input gate context exceeds the input limit")
	}
	for _, input := range r.Context {
		wrongMessageSession := input.Message != nil && input.Message.SessionID != sessionID
		wrongCallSession := input.ToolCall != nil && input.ToolCall.SessionID != sessionID
		if !input.Valid() || input.SystemPrompt != nil || wrongMessageSession || wrongCallSession {
			return fmt.Errorf("hook: invalid input gate context")
		}
	}
	if err := model.ValidateInputs(r.Context); err != nil {
		return fmt.Errorf("hook: input gate context violates the model protocol")
	}
	if len(r.Context) > 0 {
		fingerprint, err := FingerprintTypedInput(r.Context)
		if err != nil || fingerprint.Bytes > MaxContextBytes {
			return fmt.Errorf("hook: input gate context exceeds the byte limit")
		}
	}
	return nil
}
