package billing_test

import (
	"testing"
	"time"

	"github.com/LyleLiu666/agentSlot/billing"
	"github.com/LyleLiu666/agentSlot/model"
)

func TestBillingFactsUsePhysicalAttemptIdentity(t *testing.T) {
	attempt := model.AttemptIdentity{
		SessionID: "session-1", RunID: "run-1", StepID: "step-1", AttemptID: "attempt-1", ConfigRevision: 1,
		Config: model.Config{ProviderKey: "provider", ModelID: "model", Reasoning: model.ReasoningDefault},
	}
	subject := billing.Subject{Kind: "account", ID: "account-1"}
	if err := (billing.AttemptIntent{Attempt: attempt, Subject: subject, RecordedAt: time.Now()}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (billing.AttemptOutcome{
		Attempt: attempt, Subject: subject, Outcome: model.AttemptFailed, ErrorCode: "timeout", RecordedAt: time.Now(),
	}).Validate(); err != nil {
		t.Fatal(err)
	}
}
