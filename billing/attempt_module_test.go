package billing_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/billing"
	"github.com/LyleLiu666/agentSlot/model"
)

func TestAttemptModuleCoordinatesQuotaIntentPriceAndOutcome(t *testing.T) {
	ledger := &ledgerProbe{}
	quota := &quotaProbe{}
	price := &priceProbe{}
	observer := buildAttemptObserver(t, ledger, quota, price)
	identity := billingAttemptIdentity("session-1", "attempt-1")
	if err := observer.AttemptStarted(context.Background(), model.AttemptStarted{Identity: identity}); err != nil {
		t.Fatal(err)
	}
	usage := model.TokenUsage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5}
	if err := observer.AttemptFinished(context.Background(), model.AttemptFinished{
		Identity: identity, Outcome: model.AttemptSucceeded, ProviderRequestID: "request-1", Usage: usage,
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"quota.check", "quota.reserve", "ledger.intent", "price.resolve", "ledger.outcome", "quota.commit"}
	got := append(append([]string(nil), quota.events...), ledger.events...)
	// Each probe owns its local ordering, so assert the cross-boundary facts and
	// the required ordering within each independent component.
	if !reflect.DeepEqual(quota.events, []string{"quota.check", "quota.reserve", "quota.commit"}) ||
		!reflect.DeepEqual(ledger.events, []string{"ledger.intent", "ledger.outcome"}) ||
		!reflect.DeepEqual(price.events, []string{"price.resolve"}) {
		t.Fatalf("accounting events quota=%v ledger=%v price=%v (full contract %v, observed %v)", quota.events, ledger.events, price.events, want, got)
	}
	if quota.commit.Usage.TotalTokens != 5 || ledger.outcome.Price == nil || ledger.outcome.Price.AmountMicros != 25 {
		t.Fatalf("settlement facts quota=%#v ledger=%#v", quota.commit, ledger.outcome)
	}
}

func TestAttemptModuleReleasesReservationWhenDurableIntentFails(t *testing.T) {
	ledger := &ledgerProbe{intentErr: errors.New("outbox unavailable")}
	quota := &quotaProbe{}
	observer := buildAttemptObserver(t, ledger, quota, nil)
	err := observer.AttemptStarted(context.Background(), model.AttemptStarted{Identity: billingAttemptIdentity("session-1", "attempt-1")})
	if err == nil {
		t.Fatal("AttemptStarted succeeded without a durable intent")
	}
	if !reflect.DeepEqual(quota.events, []string{"quota.check", "quota.reserve", "quota.release"}) || quota.releaseReason != "ledger_intent_failed" {
		t.Fatalf("quota rollback = %v, %q", quota.events, quota.releaseReason)
	}
}

func TestAttemptAccountingKeysIncludeTheWholeRunIdentity(t *testing.T) {
	observer := buildAttemptObserver(t, &ledgerProbe{}, nil, nil)
	first := billingAttemptIdentity("session-1", "attempt-1")
	second := billingAttemptIdentity("session-2", "attempt-1")
	if err := observer.AttemptStarted(context.Background(), model.AttemptStarted{Identity: first}); err != nil {
		t.Fatal(err)
	}
	if err := observer.AttemptStarted(context.Background(), model.AttemptStarted{Identity: second}); err != nil {
		t.Fatalf("same local AttemptID in another Session conflicted: %v", err)
	}
}

func TestAttemptModuleUsesExplicitProductDefaultWhenModelOmitsMaxTokens(t *testing.T) {
	ledger := &ledgerProbe{}
	quota := &quotaProbe{}
	module, err := billing.NewAttemptModuleWithOptions("billing.attempt", billing.AttemptModuleOptions{
		Subjects: billing.SubjectResolverFunc(func(context.Context, model.AttemptIdentity) (billing.Subject, error) {
			return billing.Subject{Kind: "account", ID: "account-1"}, nil
		}),
		DefaultRequestedTokens: 77,
	})
	if err != nil {
		t.Fatal(err)
	}
	observer := buildAttemptObserverWithModule(t, ledger, quota, nil, module)
	identity := billingAttemptIdentity("session-1", "attempt-1")
	identity.Config.Parameters.MaxTokens = nil
	if err := observer.AttemptStarted(context.Background(), model.AttemptStarted{Identity: identity}); err != nil {
		t.Fatal(err)
	}
	if quota.lastCheck.RequestedTokens != 77 {
		t.Fatalf("requested tokens = %d, want explicit product default", quota.lastCheck.RequestedTokens)
	}
}

func TestAttemptModuleRejectsNegativeProductDefault(t *testing.T) {
	if _, err := billing.NewAttemptModuleWithOptions("billing.attempt", billing.AttemptModuleOptions{
		Subjects: billing.SubjectResolverFunc(func(context.Context, model.AttemptIdentity) (billing.Subject, error) {
			return billing.Subject{Kind: "account", ID: "account-1"}, nil
		}),
		DefaultRequestedTokens: -1,
	}); err == nil {
		t.Fatal("negative default requested tokens were accepted")
	}
}

func TestAttemptFinishRetryReusesTheExactLedgerFact(t *testing.T) {
	ledger := &ledgerProbe{}
	quota := &quotaProbe{commitFailures: 1}
	observer := buildAttemptObserver(t, ledger, quota, &priceProbe{})
	identity := billingAttemptIdentity("session-1", "attempt-1")
	if err := observer.AttemptStarted(context.Background(), model.AttemptStarted{Identity: identity}); err != nil {
		t.Fatal(err)
	}
	event := model.AttemptFinished{
		Identity: identity, Outcome: model.AttemptSucceeded,
		Usage: model.TokenUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	}
	if err := observer.AttemptFinished(context.Background(), event); err == nil {
		t.Fatal("first finish unexpectedly ignored quota settlement failure")
	}
	if err := observer.AttemptFinished(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if len(ledger.outcomes) != 2 || !reflect.DeepEqual(ledger.outcomes[0], ledger.outcomes[1]) {
		t.Fatalf("retried ledger outcomes drifted: %#v", ledger.outcomes)
	}
}

func buildAttemptObserver(t *testing.T, ledger billing.BillingLedger, quota billing.QuotaGuard, price billing.PriceResolver) model.AttemptObserver {
	t.Helper()
	module, err := billing.NewAttemptModule("billing.attempt", billing.SubjectResolverFunc(
		func(context.Context, model.AttemptIdentity) (billing.Subject, error) {
			return billing.Subject{Kind: "account", ID: "account-1"}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	return buildAttemptObserverWithModule(t, ledger, quota, price, module)
}

func buildAttemptObserverWithModule(t *testing.T, ledger billing.BillingLedger, quota billing.QuotaGuard, price billing.PriceResolver, module agentslot.Module) model.AttemptObserver {
	t.Helper()
	modules := []agentslot.Module{oneModule[billing.BillingLedger]{id: "ledger", slot: billing.LedgerSlot, value: ledger}}
	if quota != nil {
		modules = append(modules, oneModule[billing.QuotaGuard]{id: "quota", slot: billing.QuotaGuardSlot, value: quota})
	}
	if price != nil {
		modules = append(modules, oneModule[billing.PriceResolver]{id: "price", slot: billing.PriceResolverSlot, value: price})
	}
	modules = append(modules, module)
	app := agentslot.NewApplication("billing", modules, agentslot.RequireChain(model.AttemptObserverSlot, 1))
	assembly, err := app.Build()
	if err != nil {
		t.Fatal(err)
	}
	values := agentslot.Ordered(assembly, model.AttemptObserverSlot)
	if len(values) != 1 {
		t.Fatalf("attempt observers = %d", len(values))
	}
	return values[0]
}

type oneModule[T any] struct {
	id    string
	slot  agentslot.OneSlot[T]
	value T
}

func (m oneModule[T]) ID() string { return m.id }
func (m oneModule[T]) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Set(m.slot, m.value))
}

func billingAttemptIdentity(sessionID, attemptID string) model.AttemptIdentity {
	maxTokens := 10
	return model.AttemptIdentity{
		SessionID: agent.SessionID(sessionID), RunID: "run-1", StepID: "step-1", AttemptID: agent.AttemptID(attemptID),
		ConfigRevision: 1,
		Config: model.Config{ProviderKey: "provider", ModelID: "model", Reasoning: model.ReasoningDefault,
			Parameters: model.Parameters{MaxTokens: &maxTokens}},
	}
}

type ledgerProbe struct {
	events    []string
	intentErr error
	outcome   billing.AttemptOutcome
	outcomes  []billing.AttemptOutcome
}

func (p *ledgerProbe) RecordAttemptIntent(context.Context, billing.AttemptIntent) error {
	p.events = append(p.events, "ledger.intent")
	return p.intentErr
}
func (p *ledgerProbe) RecordAttemptOutcome(_ context.Context, outcome billing.AttemptOutcome) error {
	p.events = append(p.events, "ledger.outcome")
	p.outcome = outcome
	p.outcomes = append(p.outcomes, outcome)
	return nil
}

type quotaProbe struct {
	events         []string
	commit         billing.QuotaCommit
	releaseReason  string
	commitFailures int
	lastCheck      billing.QuotaCheck
}

func (p *quotaProbe) Check(_ context.Context, check billing.QuotaCheck) (billing.QuotaDecision, error) {
	p.events = append(p.events, "quota.check")
	p.lastCheck = check
	return billing.QuotaDecision{Allowed: true}, nil
}
func (p *quotaProbe) Reserve(_ context.Context, check billing.QuotaCheck) (billing.QuotaReservation, error) {
	p.events = append(p.events, "quota.reserve")
	return billing.QuotaReservation{ID: "reservation-1", Subject: check.Subject, Attempt: check.Attempt}, nil
}
func (p *quotaProbe) Commit(_ context.Context, commit billing.QuotaCommit) error {
	p.events = append(p.events, "quota.commit")
	p.commit = commit
	if p.commitFailures > 0 {
		p.commitFailures--
		return errors.New("quota ledger unavailable")
	}
	return nil
}
func (p *quotaProbe) Release(_ context.Context, _ billing.QuotaReservation, reason string) error {
	p.events = append(p.events, "quota.release")
	p.releaseReason = reason
	return nil
}

type priceProbe struct{ events []string }

func (p *priceProbe) ResolvePrice(context.Context, billing.PriceQuery) (billing.Price, error) {
	p.events = append(p.events, "price.resolve")
	return billing.Price{Currency: "USD", AmountMicros: 25, Version: "test-v1"}, nil
}
