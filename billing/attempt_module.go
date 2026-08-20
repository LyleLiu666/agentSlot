package billing

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/model"
)

// NewAttemptModule connects the independent pricing, quota, and ledger Slots
// to the synchronous physical-attempt boundary. It contains no account or plan
// policy; the caller supplies that through SubjectResolver and components.
func NewAttemptModule(id string, subjects SubjectResolver) (agentslot.Module, error) {
	if id == "" || subjects == nil {
		return nil, errors.New("billing: attempt module requires ID and subject resolver")
	}
	return &attemptModule{id: id, subjects: subjects}, nil
}

type attemptModule struct {
	id       string
	subjects SubjectResolver
}

func (m *attemptModule) ID() string { return m.id }

func (m *attemptModule) RequiredSlots() []agentslot.Requirement {
	return []agentslot.Requirement{
		agentslot.RequireOne(LedgerSlot),
		agentslot.OptionalOne(PriceResolverSlot),
		agentslot.OptionalOne(QuotaGuardSlot),
	}
}

func (m *attemptModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.AppendWith(model.AttemptObserverSlot,
		func(resolver agentslot.Resolver) (model.AttemptObserver, error) {
			ledger, err := agentslot.ResolveOne(resolver, LedgerSlot)
			if err != nil {
				return nil, err
			}
			prices, _, err := agentslot.ResolveOptionalOne(resolver, PriceResolverSlot)
			if err != nil {
				return nil, err
			}
			quota, _, err := agentslot.ResolveOptionalOne(resolver, QuotaGuardSlot)
			if err != nil {
				return nil, err
			}
			return &attemptAccounting{
				subjects: m.subjects, ledger: ledger, prices: prices, quota: quota,
				started: make(map[attemptKey]*attemptAccountingState),
			}, nil
		}))
}

type attemptAccountingState struct {
	mu          sync.Mutex
	identity    model.AttemptIdentity
	subject     Subject
	reservation *QuotaReservation
	finishEvent *model.AttemptFinished
	outcome     *AttemptOutcome
}

type attemptKey struct {
	sessionID string
	runID     string
	stepID    string
	attemptID string
}

func keyForAttempt(identity model.AttemptIdentity) attemptKey {
	return attemptKey{
		sessionID: string(identity.SessionID), runID: string(identity.RunID),
		stepID: string(identity.StepID), attemptID: string(identity.AttemptID),
	}
}

type attemptAccounting struct {
	subjects SubjectResolver
	ledger   BillingLedger
	prices   PriceResolver
	quota    QuotaGuard

	mu      sync.Mutex
	started map[attemptKey]*attemptAccountingState
}

func (a *attemptAccounting) AttemptStarted(ctx context.Context, event model.AttemptStarted) (resultErr error) {
	if err := event.Validate(); err != nil {
		return err
	}
	key := keyForAttempt(event.Identity)
	a.mu.Lock()
	_, duplicate := a.started[key]
	if !duplicate {
		a.started[key] = &attemptAccountingState{identity: event.Identity}
	}
	a.mu.Unlock()
	if duplicate {
		return errors.New("billing: duplicate attempt start")
	}
	defer func() {
		if resultErr == nil {
			return
		}
		a.mu.Lock()
		delete(a.started, key)
		a.mu.Unlock()
	}()
	subject, err := a.subjects.ResolveBillingSubject(ctx, event.Identity)
	if err != nil {
		return fmt.Errorf("billing: resolve subject: %w", err)
	}
	if !subject.Valid() {
		return errors.New("billing: subject resolver returned invalid subject")
	}
	check := QuotaCheck{Subject: subject, Attempt: event.Identity}
	if max := event.Identity.Config.Parameters.MaxTokens; max != nil {
		check.RequestedTokens = int64(*max)
	}
	var reservation *QuotaReservation
	if a.quota != nil {
		decision, checkErr := a.quota.Check(ctx, check)
		if checkErr != nil {
			return fmt.Errorf("billing: check quota: %w", checkErr)
		}
		if err := decision.Validate(); err != nil {
			return err
		}
		if !decision.Allowed {
			return fmt.Errorf("billing: quota denied: %s", decision.Reason)
		}
		reserved, reserveErr := a.quota.Reserve(ctx, check)
		if reserveErr != nil {
			return fmt.Errorf("billing: reserve quota: %w", reserveErr)
		}
		if reserved.Validate() != nil || reserved.Subject != subject || !reflect.DeepEqual(reserved.Attempt, event.Identity) {
			return errors.New("billing: quota guard returned invalid reservation")
		}
		reservation = &reserved
	}
	intent := AttemptIntent{Attempt: event.Identity, Subject: subject, RecordedAt: time.Now().UTC()}
	if err := a.ledger.RecordAttemptIntent(ctx, intent); err != nil {
		if reservation != nil {
			err = errors.Join(err, a.quota.Release(ctx, *reservation, "ledger_intent_failed"))
		}
		return fmt.Errorf("billing: record attempt intent: %w", err)
	}
	a.mu.Lock()
	state := a.started[key]
	a.mu.Unlock()
	state.mu.Lock()
	state.subject = subject
	state.reservation = reservation
	state.mu.Unlock()
	return nil
}

func (a *attemptAccounting) AttemptFinished(ctx context.Context, event model.AttemptFinished) error {
	if err := event.Validate(); err != nil {
		return err
	}
	key := keyForAttempt(event.Identity)
	a.mu.Lock()
	state, ok := a.started[key]
	a.mu.Unlock()
	if !ok {
		return errors.New("billing: attempt finish has no recorded start")
	}
	if !reflect.DeepEqual(state.identity, event.Identity) {
		return errors.New("billing: attempt finish identity does not match start")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.finishEvent != nil && !reflect.DeepEqual(*state.finishEvent, event) {
		return errors.New("billing: conflicting attempt finish replay")
	}
	if state.outcome == nil {
		var price *Price
		if a.prices != nil && event.Outcome == model.AttemptSucceeded && !event.Usage.Estimated {
			resolved, err := a.prices.ResolvePrice(ctx, PriceQuery{
				ProviderKey: event.Identity.Config.ProviderKey, ModelID: event.Identity.Config.ModelID,
				Usage: event.Usage, At: time.Now().UTC(),
			})
			if err != nil {
				return fmt.Errorf("billing: resolve price: %w", err)
			}
			if err := resolved.Validate(); err != nil {
				return err
			}
			price = &resolved
		}
		capturedEvent := event
		capturedOutcome := AttemptOutcome{
			Attempt: event.Identity, Subject: state.subject, Outcome: event.Outcome,
			ProviderRequestID: event.ProviderRequestID, Usage: event.Usage, ErrorCode: event.ErrorCode,
			Price: price, RecordedAt: time.Now().UTC(),
		}
		state.finishEvent = &capturedEvent
		state.outcome = &capturedOutcome
	}
	outcome := *state.outcome
	if err := a.ledger.RecordAttemptOutcome(ctx, outcome); err != nil {
		return fmt.Errorf("billing: record attempt outcome: %w", err)
	}
	if state.reservation != nil {
		var err error
		if event.Outcome == model.AttemptSucceeded {
			err = a.quota.Commit(ctx, QuotaCommit{Reservation: *state.reservation, Usage: event.Usage, Price: outcome.Price})
		} else {
			err = a.quota.Release(ctx, *state.reservation, string(event.Outcome))
		}
		if err != nil {
			return fmt.Errorf("billing: settle quota: %w", err)
		}
	}
	a.mu.Lock()
	delete(a.started, key)
	a.mu.Unlock()
	return nil
}
