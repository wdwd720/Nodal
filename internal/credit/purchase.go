package credit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/capacity"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/webhook"
)

// PurchaseService turns money into Credits, and provider events into ledger
// effects.
//
// It is the only thing that calls a PurchaseProvider, and the only thing that
// drives a Funding through its lifecycle. Everything it does is inside the
// caller's transaction, so a purchase that half-applied does not exist.
type PurchaseService struct {
	credits  *Service
	provider PurchaseProvider
	pricing  PricingPolicy
	gates    GateChecker
	capacity CapacityGuard
	clk      clock.Clock
	env      string
}

// GateChecker is the capability-gate guard. *gates.Checker satisfies it.
//
// It is an interface for the same reason internal/funding's is: activating a
// real gate runs the whole dual-control path with evidence, and a test that
// wanted to prove a refund is handled correctly should not first have to
// simulate two approvers.
type GateChecker interface {
	RequireActive(ctx context.Context, q db.Querier, cap gates.Capability) error
}

// CapacityGuard refuses an action before the infrastructure it needs runs out.
// *capacity.Guard satisfies it.
//
// It is separate from GateChecker because the two answer different questions
// and have different answers. A gate says whether this deployment is APPROVED
// to sell Credits, which people decide under dual control. This says whether
// there is ROOM to do it safely right now, which is measured. A deployment
// that conflated them would report a full launch tier as a revoked approval,
// and raising a ceiling would look like granting an approval.
//
// It is optional. A deployment with no ceilings passes nil, and the only thing
// that changes is that no ceiling is enforced -- which is the honest outcome
// rather than a silent default budget nobody chose.
type CapacityGuard interface {
	AdmitAmount(ctx context.Context, q db.Querier, a capacity.Action, amountMinor int64) (capacity.Reading, error)
}

// PurchaseServiceConfig wires a PurchaseService.
type PurchaseServiceConfig struct {
	Credits  *Service
	Provider PurchaseProvider
	Pricing  PricingPolicy
	Gates    GateChecker
	// Capacity is optional; nil enforces no ceilings.
	Capacity CapacityGuard
	Clock    clock.Clock
	// Environment is stamped onto every provider object and compared against
	// every inbound event.
	Environment string
}

// NewPurchaseService validates the configuration.
func NewPurchaseService(cfg PurchaseServiceConfig) (*PurchaseService, error) {
	if cfg.Credits == nil {
		return nil, errs.New(errs.CodeValidationFailed, "credit: a purchase service needs the credit service")
	}
	if cfg.Provider == nil {
		return nil, errs.New(errs.CodeValidationFailed, "credit: a purchase service needs a provider")
	}
	if cfg.Gates == nil {
		return nil, errs.New(errs.CodeValidationFailed,
			"credit: a purchase service needs a capability checker; without one nothing would gate selling Credits")
	}
	if err := cfg.Pricing.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.Environment) == "" {
		return nil, errs.New(errs.CodeValidationFailed, "credit: a purchase service needs an environment")
	}
	clk := cfg.Clock
	if clk == nil {
		clk = clock.System()
	}
	return &PurchaseService{
		credits: cfg.Credits, provider: cfg.Provider, pricing: cfg.Pricing,
		gates: cfg.Gates, capacity: cfg.Capacity, clk: clk, env: cfg.Environment,
	}, nil
}

// Pricing returns the policy in force. It is exposed so a funding page can
// show the rate without a second source of truth existing.
func (s *PurchaseService) Pricing() PricingPolicy { return s.pricing }

// StartPurchaseRequest is what a user's "buy Credits" request carries.
//
// Note the absence. There is no Credits field. The client says how much money
// it intends to pay; the server says what that buys.
type StartPurchaseRequest struct {
	AccountID accounts.AccountID
	// Amount is the money the customer will pay, in minor units.
	Amount   money.USD
	Currency string
	// IdempotencyKey makes a double-clicked buy button one purchase.
	IdempotencyKey string
	Description    string
}

// StartedPurchase is what the caller returns to the browser.
type StartedPurchase struct {
	Funding Funding
	// ClientSecret drives the provider's payment UI. It is returned to this
	// customer once and is not persisted.
	ClientSecret string
	// CreditQuantity is what the server decided the amount buys, so the UI can
	// show it without recomputing anything.
	CreditQuantity money.Quantity
	PricingVersion string
}

// Transactor is the database a purchase needs: one that can run a function in
// a transaction. StartPurchase needs three of them, so it cannot be handed a
// single transaction by its caller.
type Transactor interface {
	InTx(ctx context.Context, opts db.TxOptions, fn func(ctx context.Context, tx pgx.Tx) error) error
}

// StartPurchase opens a Credit purchase.
//
// # The order of operations, and why it is three transactions
//
// The funding row and its idempotency key are persisted — committed —
// BEFORE the provider is called, so a lost response leaves a record to
// reconcile against rather than a charge nobody knows about. The provider call
// uses that same key, so a retry is the same payment.
//
// That was the intent and it was not what happened. All of it ran inside one
// transaction the caller opened, with the provider call between the INSERT and
// the COMMIT, so the row was WRITTEN before the call and not PERSISTED before
// it. A rollback erased the funding row and left the Stripe object, and the
// retry then created a second funding row that the first object's metadata
// still named — a permanent disagreement that every webhook delivery
// reproduced as a 500 until Stripe gave up (F-96).
//
// It also held a database connection across a 20-second network call, out of a
// pool of eight, which is the pool starvation cmd/reconciliation-worker
// documents itself avoiding for exactly this reason.
//
// So:
//
//  1. commit the gate check, the ceiling, the pricing and the funding row;
//  2. call the provider with no transaction open;
//  3. commit the provider reference and any state the provider reported.
//
// Step 1 is also where the capacity ceiling becomes authoritative: the guard
// takes a transaction-scoped advisory lock, which is only sound because this
// transaction no longer contains a network call.
//
// A crash between 1 and 2, or between 2 and 3, leaves a funding row with no
// provider reference. That is the state the replay path below already handles:
// the same idempotency key returns the same funding, and the provider returns
// the same object for the same key.
func (s *PurchaseService) StartPurchase(ctx context.Context, database Transactor, r StartPurchaseRequest) (StartedPurchase, error) {
	if r.AccountID.IsZero() {
		return StartedPurchase{}, errs.New(errs.CodeValidationFailed, "credit: a purchase needs an account")
	}
	if strings.TrimSpace(r.IdempotencyKey) == "" {
		return StartedPurchase{}, errs.New(errs.CodeValidationFailed, "credit: a purchase needs an idempotency key")
	}
	currency := strings.ToUpper(strings.TrimSpace(r.Currency))
	if currency == "" {
		currency = s.pricing.Currency
	}
	if !strings.EqualFold(currency, s.pricing.Currency) {
		return StartedPurchase{}, errs.Newf(errs.CodeUnsupported,
			"credit: Credits are priced in %s, not %s", s.pricing.Currency, currency)
	}

	var (
		f    Funding
		qty  money.Quantity
		hash string
	)
	// Phase 1. Everything that decides whether this purchase may happen, and
	// the row that records that it did, in one short transaction with no
	// network call in it.
	if err := database.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 2},
		func(ctx context.Context, tx pgx.Tx) error {
			return s.openFunding(ctx, tx, r, currency, &f, &qty, &hash)
		}); err != nil {
		return StartedPurchase{}, err
	}

	// A replayed request that found the existing funding must not open a
	// second payment. It has a provider reference already; hand back what is
	// there. The client secret is not re-derivable, which is correct: a second
	// browser cannot resume somebody else's payment form.
	if f.ProviderReference != "" {
		return StartedPurchase{
			Funding: f, CreditQuantity: f.CreditQuantity, PricingVersion: s.pricing.Version,
		}, nil
	}

	// Phase 2. The provider, with nothing held.
	sess, err := s.provider.CreatePurchase(ctx, CreatePurchaseRequest{
		IdempotencyKey: r.IdempotencyKey,
		FundingID:      f.ID,
		AccountID:      r.AccountID,
		Amount:         r.Amount,
		Currency:       currency,
		CreditQuantity: qty,
		PricingVersion: s.pricing.Version,
		PricingHash:    hash,
		Environment:    s.env,
		Description:    r.Description,
	})
	if err != nil {
		return StartedPurchase{}, err
	}

	// Phase 3. Record what the provider said.
	if err := database.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 2},
		func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx,
				`UPDATE credit_fundings SET provider_reference = $2 WHERE id = $1 AND provider_reference IS NULL`,
				f.ID, sess.ProviderReference); err != nil {
				return mapError(err)
			}
			f.ProviderReference = sess.ProviderReference
			if to, ok := FundingStateFor(sess.Status); ok && to != f.State {
				var aerr error
				f, aerr = s.credits.AdvanceFunding(ctx, tx, f.ID, to,
					"provider opened the payment", sess.RawStatus)
				return aerr
			}
			return nil
		}); err != nil {
		return StartedPurchase{}, err
	}

	return StartedPurchase{
		Funding: f, ClientSecret: sess.ClientSecret,
		CreditQuantity: qty, PricingVersion: s.pricing.Version,
	}, nil
}

// openFunding is phase 1: the gate, the ceiling, the price and the row.
func (s *PurchaseService) openFunding(
	ctx context.Context, tx pgx.Tx, r StartPurchaseRequest, currency string,
	f *Funding, qty *money.Quantity, hash *string,
) error {
	// The gate first. Nothing below this line should run in a deployment that
	// has not been approved to sell Credits, and a provider call is the most
	// expensive thing to have to undo.
	if err := s.gates.RequireActive(ctx, tx, gates.CreditPurchase); err != nil {
		return err
	}

	// Then whether there is room. After the gate, because an unapproved
	// deployment should hear that rather than a capacity number; before
	// pricing and before the provider, because the provider call is the
	// expensive thing to undo and because a ceiling reached is not a reason to
	// have taken somebody's money first.
	//
	// The guard takes an advisory lock for the duration of this transaction,
	// so the measurement and the row that changes it are one step. That is
	// only sound because this transaction contains no network call -- see
	// StartPurchase's doc for what used to be in here.
	if s.capacity != nil {
		if _, err := s.capacity.AdmitAmount(ctx, tx, capacity.ActionCreditPurchase, r.Amount.Minor()); err != nil {
			return err
		}
	}

	// The server decides the Credits. This is the whole defence against a
	// client asking for nine million.
	q, err := s.pricing.CreditsFor(r.Amount)
	if err != nil {
		return err
	}
	h, err := s.pricing.Hash()
	if err != nil {
		return err
	}
	*qty, *hash = q, h

	out, err := s.credits.CreateFunding(ctx, tx, CreateFundingRequest{
		AccountID:      r.AccountID,
		Provider:       s.provider.Name(),
		CreditQuantity: q,
		PaidAmount:     r.Amount,
		PaidCurrency:   currency,
		IdempotencyKey: r.IdempotencyKey,
	})
	if err != nil {
		return err
	}
	*f = out
	return nil
}

// FundingByProviderReference finds the funding a provider object belongs to.
//
// The provider reference is the link, not the metadata. Metadata can be edited
// in a provider dashboard by anyone with access; this column is under a unique
// constraint in a database only this application writes to.
func (s *PurchaseService) FundingByProviderReference(ctx context.Context, q db.Querier, ref string) (Funding, error) {
	f, err := scanFunding(q.QueryRow(ctx,
		`SELECT `+fundingColumns+` FROM credit_fundings WHERE provider = $1 AND provider_reference = $2`,
		s.provider.Name(), ref))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Funding{}, errs.New(errs.CodeNotFound, "no credit funding has that provider reference").
				WithField("provider_reference", ref)
		}
		return Funding{}, mapError(err)
	}
	return f, nil
}

// Dispatch applies one verified provider event. It implements
// webhook.Dispatcher[PurchaseEvent].
//
// The pipeline above has already verified the signature, deduplicated the
// event id and opened the transaction. What is left is deciding whether the
// event is ours, whether it says what our records say, and what it means.
func (s *PurchaseService) Dispatch(ctx context.Context, tx pgx.Tx, ev PurchaseEvent) (webhook.Disposition, error) {
	log := observability.LoggerFrom(ctx)

	if ev.Foreign {
		// Normal on a shared provider account. Recorded, not acted on.
		log.InfoContext(ctx, "credit: ignoring a provider event that is not ours",
			"event_id", ev.Identity.EventID, "event_type", ev.Identity.EventType, "reason", ev.ForeignReason)
		return webhook.Ignored, nil
	}
	if !ev.Recognized {
		return webhook.Ignored, nil
	}
	ref := ev.Snapshot.ProviderReference
	if ref == "" {
		return webhook.Ignored, nil
	}

	f, err := s.FundingByProviderReference(ctx, tx, ref)
	if err != nil {
		if errs.CodeOf(err) == errs.CodeNotFound {
			// Signed, carries our marker or names a payment intent, and
			// matches nothing we have. On a shared account the ordinary cause
			// is a charge or dispute belonging to the other product. It is
			// foreignness discovered one step later, and it is recorded rather
			// than retried forever.
			log.InfoContext(ctx, "credit: provider event names no known funding",
				"event_id", ev.Identity.EventID, "event_type", ev.Identity.EventType, "provider_reference", ref)
			return webhook.Ignored, nil
		}
		return "", err
	}

	// Two identities that disagree is the shape of a mis-copied or tampered
	// provider object. It is never resolved by preferring one of them.
	if !ev.FundingID.IsZero() && ev.FundingID != f.ID {
		return "", errs.Newf(errs.CodeValidationFailed,
			"credit: provider object %s names funding %s in metadata but belongs to funding %s",
			ref, ev.FundingID, f.ID)
	}

	// A funding parked for a person waits for that person.
	//
	// fundingTransitions gives MANUAL_REVIEW ten destinations, because an
	// operator resolving a review may send the funding anywhere a provider
	// event could legitimately have sent it. Dispatch reads the same table, so
	// every destination written for a human was also one the next provider
	// event could take -- and MANUAL_REVIEW -> CAPTURED is the edge that mints.
	//
	// The sequence that costs money: a refund arrives before the capture, is
	// not a legal transition from CAPTURE_PENDING, and parks the funding. It is
	// reported Applied, so the inbox marks it PROCESSED and it is never
	// redelivered. The later payment_intent.succeeded then finds a parked
	// funding, MANUAL_REVIEW -> CAPTURED is legal, and Credits are minted for
	// money that was returned. Observed against a real database: balance 10000
	// where it must be 0 (F-100).
	//
	// So events for a parked funding are recorded and not acted on. Applied
	// rather than an error for the same reason review() uses it: an error rolls
	// back and the provider redelivers forever, and the review is already the
	// durable statement that something needs a human. The identity check above
	// stays above this, because two identities that disagree is tampering and
	// deserves an error whatever state the funding is in.
	if f.State == FundingManualReview {
		log.WarnContext(ctx, "credit: provider event arrived for a funding parked for review",
			"event_id", ev.Identity.EventID, "event_type", ev.Identity.EventType,
			"funding_id", f.ID.String(), "event_would_set", string(stateOrUnmapped(ev.Snapshot.Status)))
		return webhook.Applied, nil
	}

	// The provider is authoritative about what it charged. If that disagrees
	// with what we recorded, minting Credits against it would issue value for
	// a payment we do not understand.
	if ev.Snapshot.Amount.Minor() != 0 && ev.Snapshot.Amount.Minor() != f.PaidAmount.Minor() {
		return s.review(ctx, tx, f, fmt.Sprintf(
			"provider reports %s for this payment and the funding records %s",
			ev.Snapshot.Amount, f.PaidAmount,
		), ev.Identity.EventID)
	}

	to, ok := FundingStateFor(ev.Snapshot.Status)
	if !ok {
		return s.review(ctx, tx, f,
			"provider status "+ev.Snapshot.RawStatus+" has no mapping in this binary", ev.Identity.EventID)
	}
	// Money the provider says it returned is not money to mint against. A
	// succeeded PaymentIntent carrying an already-refunded charge maps to
	// CAPTURED, and CAPTURED is the edge that mints (F-113).
	if to == FundingCaptured {
		if reason := refundedReason(ev.Snapshot); reason != "" {
			return s.review(ctx, tx, f, reason+", so it is not a capture to mint against", ev.Identity.EventID)
		}
	}

	// A state we are already in, or one behind where we are, is a re-delivery.
	// Providers redeliver for days and do not order their deliveries.
	if to == f.State {
		return webhook.Applied, nil
	}
	if !CanTransitionFunding(f.State, to) {
		if f.State.Terminal() || isBackwards(f.State, to) {
			log.InfoContext(ctx, "credit: ignoring a stale provider event",
				"event_id", ev.Identity.EventID, "funding_id", f.ID.String(),
				"funding_state", string(f.State), "event_would_set", string(to))
			return webhook.Applied, nil
		}
		return s.review(ctx, tx, f,
			fmt.Sprintf("provider event would move the funding %s -> %s, which is not a legal transition", f.State, to),
			ev.Identity.EventID)
	}

	return s.apply(ctx, tx, f, to, "provider event "+ev.Identity.EventType, ev.Identity.EventID)
}

// apply moves a funding to a state AND performs the economic effect that
// state implies, in one place.
//
// Doing the state change separately from the effect is the bug this function
// exists to prevent. Three of these states carry a change to the finality of
// the Credits the funding minted, and the service already has a helper for
// each that does both halves. Calling AdvanceFunding and stopping would leave
// a disputed funding's Credits sitting at REVERSIBLE -- still spendable, and
// on their way to payout-eligible -- while every row said DISPUTED.
func (s *PurchaseService) apply(ctx context.Context, tx pgx.Tx, f Funding, to FundingState, reason, eventID string) (webhook.Disposition, error) {
	switch to {
	case FundingCaptured:
		// The money arrived. Advance, then mint. MintFrom is idempotent
		// through the lot id on the funding row, so a duplicated capture
		// event mints once -- which is acceptance criterion PAY-003.
		if _, err := s.credits.AdvanceFunding(ctx, tx, f.ID, to, reason, eventID); err != nil {
			return "", err
		}
		if _, err := s.credits.MintFrom(ctx, tx, f.ID, s.clk.Now()); err != nil {
			return "", err
		}

	case FundingSettled:
		// Advances AND promotes the lot from REVERSIBLE to SETTLED, which is
		// what makes purchased value capable of ever being paid out.
		if err := s.credits.SettleFunding(ctx, tx, f.ID, reason); err != nil {
			return "", err
		}

	case FundingDisputed:
		// Advances AND freezes the lot. Freezing is the half that matters:
		// value under dispute must stop being spendable immediately, not when
		// somebody notices.
		if err := s.credits.DisputeFunding(ctx, tx, f.ID, reason); err != nil {
			return "", err
		}

	case FundingReversed:
		// Advances AND claws back. Reverse destroys what remains and books
		// the rest as a recorded DEFICIT; it deliberately does not unwind the
		// trades the Credits funded, because reversing a third party's trade
		// takes value from someone who did nothing wrong.
		if _, err := s.credits.Reverse(ctx, tx, f.ID, s.clk.Now(), reason); err != nil {
			return "", err
		}

	case FundingRefunded:
		// The same accounting, a different meaning. A refund is our decision
		// and carries no risk signal about the account; collapsing it into
		// REVERSED would make every goodwill refund look like a chargeback in
		// the fraud model.
		if _, err := s.credits.Refund(ctx, tx, f.ID, s.clk.Now(), reason); err != nil {
			return "", err
		}

	default:
		if _, err := s.credits.AdvanceFunding(ctx, tx, f.ID, to, reason, eventID); err != nil {
			return "", err
		}
	}
	return webhook.Applied, nil
}

// review parks a funding for a person and reports the event as applied.
//
// Applied rather than an error, deliberately. An error would roll back and the
// provider would redeliver, and on the next delivery the same thing would be
// not understood again -- forever, with no record that anything was wrong. A
// funding in MANUAL_REVIEW is a durable, findable statement that something
// needs a human.
func (s *PurchaseService) review(ctx context.Context, tx pgx.Tx, f Funding, reason, eventID string) (webhook.Disposition, error) {
	observability.LoggerFrom(ctx).WarnContext(ctx, "credit: funding parked for manual review",
		"funding_id", f.ID.String(), "funding_state", string(f.State), "reason", reason, "event_id", eventID)
	if f.State == FundingManualReview {
		return webhook.Applied, nil
	}
	if !CanTransitionFunding(f.State, FundingManualReview) {
		// A terminal funding cannot be parked. There is nothing left to
		// decide, so the event is recorded and ignored.
		return webhook.Ignored, nil
	}
	if _, err := s.credits.AdvanceFunding(ctx, tx, f.ID, FundingManualReview, reason, eventID); err != nil {
		return "", err
	}
	return webhook.Applied, nil
}

// refundedReason describes a provider snapshot that reports money returned, or
// "" when it reports none.
//
// AmountRefundedMinor is populated by the Stripe adapter in two places -- from
// a charge's amount_refunded on the webhook path and from the payment intent's
// charges on the lookup path -- and until F-113 it was read NOWHERE. Neither
// Dispatch nor Reconcile consulted it.
//
// That matters because a PaymentIntent's status stays `succeeded` and its
// amount is unchanged after a refund. So the amount check passes, the status
// maps to CAPTURED, and Credits are minted for money that has been given back:
// on the webhook path when a succeeded delivery arrives carrying an already
// refunded charge, and on the reconciliation path when a sweep looks up a
// funding whose refund webhook was lost.
//
// The provider is authoritative about what it charged. It is equally
// authoritative about what it returned, and this binary was listening to only
// half of that.
func refundedReason(snap PurchaseSnapshot) string {
	if snap.AmountRefundedMinor <= 0 {
		return ""
	}
	if snap.Amount.Minor() > 0 && snap.AmountRefundedMinor >= snap.Amount.Minor() {
		return fmt.Sprintf("the provider reports this payment fully refunded (%d of %d minor units)",
			snap.AmountRefundedMinor, snap.Amount.Minor())
	}
	return fmt.Sprintf("the provider reports %d minor units of this payment refunded",
		snap.AmountRefundedMinor)
}

// stateOrUnmapped names what an event would have set, for a log line that has
// to say something useful even when the status has no mapping in this binary.
func stateOrUnmapped(s PurchaseStatus) FundingState {
	if to, ok := FundingStateFor(s); ok {
		return to
	}
	return FundingState("UNMAPPED:" + s)
}

// ManualResolution is an operator's decision about a funding parked in
// MANUAL_REVIEW.
//
// Two things are deliberately absent from this vocabulary.
//
// There is no resolution to SETTLED. Settlement means the reversibility window
// closed, which is a fact about a clock and a policy; an operator who could
// assert it by hand could make value payout-eligible by closing a ticket.
//
// There is no resolution that returns a funding to the provider-driven path
// either. Resuming would hand it back to Dispatch, and Dispatch parked it
// because this binary could not decide what the provider was saying -- being
// told to look again does not make it able to. An operator who believes the
// payment succeeded says CAPTURED; one who believes it did not says FAILED.
//
// So every resolution is an assertion about what actually happened, and each
// carries the same economic effect the equivalent provider event would have.
type ManualResolution string

const (
	// ResolutionCaptured: the provider shows the money arrived. Mints.
	ResolutionCaptured ManualResolution = "CAPTURED"
	// ResolutionRefunded: the money was returned. Claws back.
	ResolutionRefunded ManualResolution = "REFUNDED"
	// ResolutionReversed: a dispute was lost. Claws back and books the
	// remainder as a recorded deficit.
	ResolutionReversed ManualResolution = "REVERSED"
	// ResolutionDisputed: a dispute is open. Freezes the Credits so they stop
	// being spendable while it runs.
	ResolutionDisputed ManualResolution = "DISPUTED"
	// ResolutionFailed: the payment never completed.
	ResolutionFailed ManualResolution = "FAILED"
	// ResolutionCanceled: the payment was abandoned before it completed.
	ResolutionCanceled ManualResolution = "CANCELED"
)

var allManualResolutions = []ManualResolution{
	ResolutionCaptured, ResolutionRefunded, ResolutionReversed,
	ResolutionDisputed, ResolutionFailed, ResolutionCanceled,
}

// AllManualResolutions returns every declared resolution.
func AllManualResolutions() []ManualResolution {
	return append([]ManualResolution(nil), allManualResolutions...)
}

// Valid reports whether r is a declared resolution.
func (r ManualResolution) Valid() bool {
	for _, x := range allManualResolutions {
		if x == r {
			return true
		}
	}
	return false
}

func (r ManualResolution) String() string { return string(r) }

// fundingStateFor maps a resolution to the state it asserts. Every one of them
// is in fundingTransitions[FundingManualReview]; the pairing is checked by
// TestManualResolutionsAreAllLegalFromReview rather than assumed.
func (r ManualResolution) fundingState() FundingState {
	switch r {
	case ResolutionCaptured:
		return FundingCaptured
	case ResolutionRefunded:
		return FundingRefunded
	case ResolutionReversed:
		return FundingReversed
	case ResolutionDisputed:
		return FundingDisputed
	case ResolutionFailed:
		return FundingFailed
	case ResolutionCanceled:
		return FundingCanceled
	}
	return ""
}

// ResolveManualReview applies an operator's decision to a parked funding.
//
// It is the only exit from MANUAL_REVIEW. Dispatch stopped being one in F-100,
// and without this a parked funding would stay parked forever -- with its money
// counted against the at-risk ceiling for the life of the deployment, which is
// F-90's failure returning through a different door.
//
// The effect runs through apply, the same function a provider event uses, so a
// hand resolution mints, freezes or claws back by exactly the audited path an
// automatic one would. approvalID is recorded where an event id would be, so
// the funding's transition row names the approval that authorised it.
func (s *PurchaseService) ResolveManualReview(
	ctx context.Context, tx pgx.Tx, id FundingID, r ManualResolution, reason, approvalID string,
) (Funding, error) {
	if !r.Valid() {
		return Funding{}, errs.Newf(errs.CodeValidationFailed,
			"unknown credit funding resolution %q; a funding is never declared settled by hand", r)
	}
	f, err := s.credits.Funding(ctx, tx, id)
	if err != nil {
		return Funding{}, err
	}
	if f.State != FundingManualReview {
		// Not a race to retry: the funding is not the thing the approval was
		// written about any more, and applying the decision anyway would move
		// it from a state nobody reviewed.
		return Funding{}, errs.Newf(errs.CodeConflict,
			"this credit funding is %s, not MANUAL_REVIEW; there is no review to resolve", f.State)
	}
	to := r.fundingState()
	if !CanTransitionFunding(f.State, to) {
		return Funding{}, errs.Newf(errs.CodeConflict,
			"resolving to %s would move the funding %s -> %s, which is not a legal transition", r, f.State, to)
	}
	if _, err := s.apply(ctx, tx, f, to, "operator resolution "+string(r)+": "+reason, approvalID); err != nil {
		return Funding{}, err
	}
	return s.credits.Funding(ctx, tx, id)
}

// isBackwards reports whether to is behind from on the pre-capture path.
//
// It exists so that a re-delivered early event -- "requires_payment_method"
// arriving after "succeeded" -- is ignored rather than parked for a human. A
// provider that redelivers is not a provider that is wrong.
func isBackwards(from, to FundingState) bool {
	rank := map[FundingState]int{
		FundingCreated:              0,
		FundingAuthorizationPending: 1,
		FundingAuthorized:           2,
		FundingCapturePending:       3,
		FundingCaptured:             4,
		FundingReversible:           5,
		FundingSettled:              6,
	}
	f, okFrom := rank[from]
	t, okTo := rank[to]
	return okFrom && okTo && t < f
}

// Reconcile compares one funding against the provider's own record.
//
// It is what closes the gap a lost response opens: the funding exists, the
// provider reference may or may not, and only asking settles it.
func (s *PurchaseService) Reconcile(ctx context.Context, tx pgx.Tx, id FundingID) (Funding, error) {
	f, err := s.credits.Funding(ctx, tx, id)
	if err != nil {
		return Funding{}, err
	}
	if f.ProviderReference == "" {
		return f, nil
	}
	snap, err := s.provider.GetPurchase(ctx, f.ProviderReference)
	if err != nil {
		return Funding{}, err
	}
	if snap.Amount.Minor() != f.PaidAmount.Minor() {
		if _, err := s.review(ctx, tx, f, fmt.Sprintf(
			"reconciliation found the provider charging %s where the funding records %s",
			snap.Amount, f.PaidAmount,
		), "reconcile"); err != nil {
			return Funding{}, err
		}
		return s.credits.Funding(ctx, tx, id)
	}
	// The sweep applies LESS scrutiny than the webhook path by construction --
	// it never sees metadata or an event type -- so the one thing it must not
	// skip is whether the money is still there. This is the path where a lost
	// refund webhook turns into minted Credits fifteen minutes later (F-113).
	if reason := refundedReason(snap); reason != "" {
		if _, rerr := s.review(ctx, tx, f, reason, "reconcile"); rerr != nil {
			return Funding{}, rerr
		}
		return s.credits.Funding(ctx, tx, id)
	}
	to, ok := FundingStateFor(snap.Status)
	if !ok || to == f.State || !CanTransitionFunding(f.State, to) {
		return f, nil
	}
	f, err = s.credits.AdvanceFunding(ctx, tx, id, to, "reconciliation with the provider", snap.RawStatus)
	if err != nil {
		return Funding{}, err
	}
	if to == FundingCaptured {
		if _, err := s.credits.MintFrom(ctx, tx, id, s.clk.Now()); err != nil {
			return Funding{}, err
		}
		return s.credits.Funding(ctx, tx, id)
	}
	return f, nil
}

// SettleDue promotes fundings whose reversibility window has closed.
//
// The window is policy, not a constant. The goal document is explicit that
// there is no arbitrary seven days unless a policy says seven days, so the
// duration is an argument and the caller reads it from configuration.
func (s *PurchaseService) SettleDue(ctx context.Context, tx pgx.Tx, window time.Duration, limit int) (int, error) {
	if window <= 0 {
		return 0, errs.New(errs.CodeValidationFailed,
			"credit: a settlement window must be positive; a zero window would settle a payment the moment it captured")
	}
	if limit <= 0 {
		limit = 100
	}
	// The cutoff is computed in SQL, from the database's own clock, and not
	// from s.clk.
	//
	// updated_at is set by a database trigger. Comparing it against a time
	// this process computed means comparing two clocks, and the answer is
	// wrong by however far apart they are -- which in a test with an injected
	// clock is months, and in production is however much skew the fleet has.
	// One clock decides, and it is the one that wrote the column.
	//
	// reversible_at, not updated_at. updated_at is maintained by a trigger and
	// means "when was this row last touched", so any unrelated write would
	// silently restart a dispute window. reversible_at is stamped once, by
	// AdvanceFunding, on the edge that actually matters.
	rows, err := tx.Query(ctx,
		`SELECT id FROM credit_fundings
		  WHERE state = 'REVERSIBLE'
		    AND reversible_at IS NOT NULL
		    AND reversible_at < now() - make_interval(secs => $1)
		  ORDER BY reversible_at
		  LIMIT $2
		  FOR UPDATE SKIP LOCKED`, window.Seconds(), limit)
	if err != nil {
		return 0, mapError(err)
	}
	var ids []FundingID
	for rows.Next() {
		var id FundingID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, mapError(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, mapError(err)
	}
	for _, id := range ids {
		// SettleFunding, not AdvanceFunding: it also promotes the lot from
		// REVERSIBLE to SETTLED, and the promotion is the whole point. A
		// funding row that says SETTLED over a lot that still says REVERSIBLE
		// would leave the value permanently ineligible for payout while every
		// operator screen said it had settled.
		if err := s.credits.SettleFunding(ctx, tx, id, "the reversibility window closed"); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}
