// Package billing defines portable pricing, quota, and ledger component
// boundaries. It does not infer account ownership, plans, or product policy.
package billing

import (
	"context"
	"errors"
	"strings"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/model"
)

var (
	PriceResolverSlot = agentslot.One[PriceResolver]("price.resolver")
	QuotaGuardSlot    = agentslot.One[QuotaGuard]("quota.guard")
	LedgerSlot        = agentslot.One[BillingLedger]("billing.ledger")
)

type Subject struct {
	Kind string
	ID   string
}

func (s Subject) Valid() bool {
	return strings.TrimSpace(s.Kind) != "" && strings.TrimSpace(s.Kind) == s.Kind &&
		strings.TrimSpace(s.ID) != "" && strings.TrimSpace(s.ID) == s.ID
}

type PriceQuery struct {
	ProviderKey string
	ModelID     string
	Usage       model.TokenUsage
	At          time.Time
}

type Price struct {
	Currency     string
	AmountMicros int64
	Version      string
}

func (p Price) Validate() error {
	if p.Currency == "" || p.AmountMicros < 0 || p.Version == "" {
		return errors.New("billing: invalid price")
	}
	return nil
}

type PriceResolver interface {
	ResolvePrice(context.Context, PriceQuery) (Price, error)
}

type QuotaCheck struct {
	Subject         Subject
	Attempt         model.AttemptIdentity
	RequestedTokens int64
}

type QuotaDecision struct {
	Allowed bool
	Reason  string
}

func (d QuotaDecision) Validate() error {
	if (d.Allowed && d.Reason != "") || (!d.Allowed && strings.TrimSpace(d.Reason) == "") {
		return errors.New("billing: invalid quota decision")
	}
	return nil
}

type QuotaReservation struct {
	ID      string
	Subject Subject
	Attempt model.AttemptIdentity
}

func (r QuotaReservation) Validate() error {
	if strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.ID) != r.ID || !r.Subject.Valid() {
		return errors.New("billing: invalid quota reservation")
	}
	return r.Attempt.Validate()
}

type QuotaCommit struct {
	Reservation QuotaReservation
	Usage       model.TokenUsage
	Price       *Price
}

type QuotaGuard interface {
	Check(context.Context, QuotaCheck) (QuotaDecision, error)
	Reserve(context.Context, QuotaCheck) (QuotaReservation, error)
	Commit(context.Context, QuotaCommit) error
	Release(context.Context, QuotaReservation, string) error
}

type AttemptIntent struct {
	Attempt    model.AttemptIdentity
	Subject    Subject
	RecordedAt time.Time
}

func (i AttemptIntent) Validate() error {
	if err := i.Attempt.Validate(); err != nil {
		return err
	}
	if !i.Subject.Valid() || i.RecordedAt.IsZero() {
		return errors.New("billing: invalid attempt intent")
	}
	return nil
}

type AttemptOutcome struct {
	Attempt           model.AttemptIdentity
	Subject           Subject
	Outcome           model.AttemptOutcome
	ProviderRequestID string
	Usage             model.TokenUsage
	ErrorCode         string
	Price             *Price
	RecordedAt        time.Time
}

func (o AttemptOutcome) Validate() error {
	if err := o.Attempt.Validate(); err != nil {
		return err
	}
	if !o.Subject.Valid() || o.RecordedAt.IsZero() {
		return errors.New("billing: invalid attempt outcome")
	}
	if err := (model.AttemptFinish{
		AttemptID: o.Attempt.AttemptID, Outcome: o.Outcome, ProviderRequestID: o.ProviderRequestID,
		Usage: o.Usage, ErrorCode: o.ErrorCode,
	}).Validate(); err != nil {
		return err
	}
	if o.Price != nil {
		return o.Price.Validate()
	}
	return nil
}

type BillingLedger interface {
	RecordAttemptIntent(context.Context, AttemptIntent) error
	RecordAttemptOutcome(context.Context, AttemptOutcome) error
}

type SubjectResolver interface {
	ResolveBillingSubject(context.Context, model.AttemptIdentity) (Subject, error)
}

type SubjectResolverFunc func(context.Context, model.AttemptIdentity) (Subject, error)

func (f SubjectResolverFunc) ResolveBillingSubject(ctx context.Context, attempt model.AttemptIdentity) (Subject, error) {
	if f == nil {
		return Subject{}, errors.New("billing: nil subject resolver")
	}
	return f(ctx, attempt)
}
