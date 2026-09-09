package credit

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// FundingState is the lifecycle of a Credit purchase (gola.md PART XI).
//
// The states exist because "the card was captured" and "the money is ours" are
// different facts separated by a dispute window that can run for months. A
// system with one "PAID" state will hand out payout-eligible value on the
// strength of a payment that can still be reversed.
type FundingState string

// Funding states.
const (
	FundingCreated              FundingState = "CREATED"
	FundingAuthorizationPending FundingState = "AUTHORIZATION_PENDING"
	FundingAuthorized           FundingState = "AUTHORIZED"
	FundingCapturePending       FundingState = "CAPTURE_PENDING"
	FundingCaptured             FundingState = "CAPTURED"
	// FundingReversible is where Credits exist and are spendable, and the
	// funder can still take the money back. Most live funding sits here.
	FundingReversible FundingState = "REVERSIBLE"
	FundingSettled    FundingState = "SETTLED"
	FundingReversed   FundingState = "REVERSED"
	FundingRefunded   FundingState = "REFUNDED"
	FundingDisputed   FundingState = "DISPUTED"
	FundingFailed     FundingState = "FAILED"

	// FundingCanceled is a purchase abandoned or cancelled before any money
	// moved. It is deliberately not FAILED: nothing was declined and nothing
	// went wrong, and a support queue that cannot tell the two apart will
	// chase customers whose only crime was closing a tab.
	FundingCanceled FundingState = "CANCELED"

	// FundingManualReview is where a funding goes when the provider said
	// something this binary does not understand.
	//
	// Without this state an unmapped provider status leaves two options, and
	// both are worse: crash, or pick the nearest state and act on it. Picking
	// is how a payment nobody understood becomes Credits somebody spent. A
	// funding sitting here has had no economic effect and is waiting for a
	// person.
	FundingManualReview FundingState = "MANUAL_REVIEW"
)

var allFundingStates = []FundingState{
	FundingCreated, FundingAuthorizationPending, FundingAuthorized, FundingCapturePending,
	FundingCaptured, FundingReversible, FundingSettled, FundingReversed, FundingRefunded,
	FundingDisputed, FundingFailed, FundingCanceled, FundingManualReview,
}

// AllFundingStates returns every declared state in declaration order (a copy).
func AllFundingStates() []FundingState {
	return append([]FundingState(nil), allFundingStates...)
}

// Valid reports whether s is a declared state.
func (s FundingState) Valid() bool {
	for _, x := range allFundingStates {
		if x == s {
			return true
		}
	}
	return false
}

func (s FundingState) String() string { return string(s) }

// Terminal reports whether no further transition is possible.
func (s FundingState) Terminal() bool {
	switch s {
	case FundingReversed, FundingRefunded, FundingFailed, FundingCanceled:
		return true
	}
	return false
}

// Minted reports whether Credits exist for a funding in this state. It is the
// question an operator resolving a MANUAL_REVIEW has to answer before choosing
// a resolution, and the question Reverse asks before deciding whether there is
// anything to claw back.
func (s FundingState) Minted() bool {
	switch s {
	case FundingReversible, FundingSettled, FundingDisputed, FundingReversed, FundingRefunded:
		return true
	}
	return false
}

// fundingTransitions is the explicit legal transition table.
//
// The pre-capture states may skip forward, and that is a change from the
// strictly sequential table this started as. The reason is that the strict
// version could not survive a real acquirer. A Stripe PaymentIntent with
// automatic capture never reports requires_capture at all: it goes
// requires_payment_method -> succeeded, sometimes with processing in between
// and sometimes not. Webhooks are also re-delivered and unordered, so the
// first delivery we ever see for a purchase may be the last one that happened.
// A table that only allows one step at a time turns both of those ordinary
// situations into a permanently jammed funding.
//
// Nothing is given up by allowing the skip. The pre-capture states are
// observational -- they record what the provider has told us so far and have
// no economic effect. The effects are guarded elsewhere and unchanged: minting
// requires CAPTURED and happens on the CAPTURED -> REVERSIBLE edge, the lot id
// on the row makes a second mint a no-op, and no state may move backwards.
var fundingTransitions = map[FundingState][]FundingState{
	FundingCreated: {
		FundingAuthorizationPending, FundingAuthorized, FundingCapturePending, FundingCaptured,
		FundingFailed, FundingCanceled, FundingManualReview,
	},
	FundingAuthorizationPending: {
		FundingAuthorized, FundingCapturePending, FundingCaptured,
		FundingFailed, FundingCanceled, FundingManualReview,
	},
	FundingAuthorized: {
		FundingCapturePending, FundingCaptured,
		FundingFailed, FundingCanceled, FundingManualReview,
	},
	// Once the provider is processing, cancelling is no longer ours to do.
	FundingCapturePending: {FundingCaptured, FundingFailed, FundingManualReview},
	// Credits are minted on CAPTURED -> REVERSIBLE, not on CAPTURED, so that
	// the mint has its own transition and cannot be triggered twice by a
	// duplicated capture webhook.
	FundingCaptured:   {FundingReversible, FundingFailed, FundingManualReview},
	FundingReversible: {FundingSettled, FundingDisputed, FundingReversed, FundingRefunded, FundingManualReview},
	// A card network can dispute a payment a processor already calls settled.
	FundingSettled:  {FundingDisputed, FundingRefunded, FundingManualReview},
	FundingDisputed: {FundingSettled, FundingReversed, FundingManualReview},
	// An operator resolving a review may send the funding anywhere a provider
	// event could legitimately have sent it -- with one exception. There is no
	// resolution to SETTLED. Settlement means the dispute window closed, which
	// is a fact about a clock and a policy; an operator who could assert it by
	// hand could make value payout-eligible by closing a ticket. Resolving to
	// REVERSIBLE puts the funding back on the path that reaches SETTLED
	// honestly.
	FundingManualReview: {
		FundingAuthorizationPending, FundingAuthorized, FundingCapturePending, FundingCaptured,
		FundingReversible, FundingDisputed, FundingReversed, FundingRefunded,
		FundingFailed, FundingCanceled,
	},
	FundingReversed: {},
	FundingRefunded: {},
	FundingFailed:   {},
	FundingCanceled: {},
}

// CanTransitionFunding reports whether from → to is legal.
func CanTransitionFunding(from, to FundingState) bool {
	for _, t := range fundingTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// LotFinalityFor maps a funding state to the finality its Credits should carry.
// Returning false means the state does not imply a finality — the funding has
// not minted anything yet.
func LotFinalityFor(s FundingState) (valuedomain.FundingFinality, bool) {
	switch s {
	case FundingReversible:
		return valuedomain.FinalityReversible, true
	case FundingSettled:
		return valuedomain.FinalitySettled, true
	case FundingDisputed:
		return valuedomain.FinalityDisputed, true
	case FundingReversed, FundingRefunded:
		return valuedomain.FinalityReversed, true
	}
	return "", false
}

// Funding is a credit_fundings row.
type Funding struct {
	ID                FundingID
	AccountID         accounts.AccountID
	Provider          string
	ProviderReference string
	State             FundingState
	CreditQuantity    money.Quantity
	PaidAmount        money.USD
	PaidCurrency      string
	FeeAmount         money.USD
	IdempotencyKey    string
	LotID             *LotID
	SettledAt         *time.Time
	ReversedAt        *time.Time
	FailureReason     string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

const fundingColumns = `id, account_id, provider, coalesce(provider_reference,''), state, credit_quantity::text,
	paid_amount_minor, paid_currency, fee_amount_minor, idempotency_key, lot_id,
	settled_at, reversed_at, coalesce(failure_reason,''), created_at, updated_at`

func scanFunding(row pgx.Row) (Funding, error) {
	var (
		f     Funding
		qty   string
		paid  int64
		fee   int64
		state string
	)
	if err := row.Scan(&f.ID, &f.AccountID, &f.Provider, &f.ProviderReference, &state, &qty,
		&paid, &f.PaidCurrency, &fee, &f.IdempotencyKey, &f.LotID,
		&f.SettledAt, &f.ReversedAt, &f.FailureReason, &f.CreatedAt, &f.UpdatedAt); err != nil {
		return Funding{}, err
	}
	var err error
	if f.CreditQuantity, err = money.ParseQuantity(qty); err != nil {
		return Funding{}, errs.Wrap(err, errs.CodeInternal, "credit: funding quantity is not an integer")
	}
	f.State = FundingState(state)
	f.PaidAmount = money.USDFromMinor(paid)
	f.FeeAmount = money.USDFromMinor(fee)
	f.CreatedAt, f.UpdatedAt = f.CreatedAt.UTC(), f.UpdatedAt.UTC()
	return f, nil
}

// CreateFundingRequest starts a Credit purchase. No Credits exist yet.
type CreateFundingRequest struct {
	AccountID         accounts.AccountID
	Provider          string
	ProviderReference string
	CreditQuantity    money.Quantity
	PaidAmount        money.USD
	PaidCurrency      string
	FeeAmount         money.USD
	IdempotencyKey    string
}

// Validate checks the request without touching the database.
func (r CreateFundingRequest) Validate() error {
	if r.AccountID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "credit: funding requires an account id")
	}
	if strings.TrimSpace(r.Provider) == "" {
		return errs.New(errs.CodeValidationFailed, "credit: funding requires a provider")
	}
	if r.CreditQuantity.Sign() <= 0 {
		return errs.New(errs.CodeValidationFailed, "credit: funding must buy a positive number of Credits")
	}
	if r.PaidAmount.IsNegative() || r.FeeAmount.IsNegative() {
		return errs.New(errs.CodeValidationFailed, "credit: funding amounts cannot be negative")
	}
	if strings.TrimSpace(r.IdempotencyKey) == "" {
		return errs.New(errs.CodeValidationFailed, "credit: funding requires an idempotency key")
	}
	return nil
}

// CreateFunding records an intended purchase in state CREATED.
func (s *Service) CreateFunding(ctx context.Context, tx pgx.Tx, r CreateFundingRequest) (Funding, error) {
	if err := r.Validate(); err != nil {
		return Funding{}, err
	}
	currency := r.PaidCurrency
	if currency == "" {
		currency = "USD"
	}
	var providerRef any
	if r.ProviderReference != "" {
		providerRef = r.ProviderReference
	}
	f, err := scanFunding(tx.QueryRow(ctx,
		`INSERT INTO credit_fundings
		   (id, account_id, provider, provider_reference, state, credit_quantity,
		    paid_amount_minor, paid_currency, fee_amount_minor, idempotency_key)
		 VALUES ($1,$2,$3,$4,'CREATED',$5::numeric,$6,$7,$8,$9)
		 ON CONFLICT (idempotency_key) DO NOTHING
		 RETURNING `+fundingColumns,
		NewFundingID(), r.AccountID, r.Provider, providerRef, r.CreditQuantity.String(),
		r.PaidAmount.Minor(), currency, r.FeeAmount.Minor(), r.IdempotencyKey))
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Funding{}, mapError(err)
	}
	// The key was already used. Returning the existing row makes a retried
	// checkout idempotent rather than a duplicate charge.
	existing, err := scanFunding(tx.QueryRow(ctx,
		`SELECT `+fundingColumns+` FROM credit_fundings WHERE idempotency_key = $1`, r.IdempotencyKey))
	if err != nil {
		return Funding{}, mapError(err)
	}
	if existing.AccountID != r.AccountID || existing.CreditQuantity.Cmp(r.CreditQuantity) != 0 {
		return Funding{}, errs.New(errs.CodeInvalidIdempotencyReuse,
			"this idempotency key was already used for a different Credit purchase").
			WithField("idempotency_key", r.IdempotencyKey)
	}
	return existing, nil
}

// Funding returns one funding row.
func (s *Service) Funding(ctx context.Context, q db.Querier, id FundingID) (Funding, error) {
	f, err := scanFunding(q.QueryRow(ctx, `SELECT `+fundingColumns+` FROM credit_fundings WHERE id = $1`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Funding{}, errs.New(errs.CodeNotFound, "credit funding not found").WithField("funding_id", id.String())
		}
		return Funding{}, mapError(err)
	}
	return f, nil
}

// AdvanceFunding moves a funding to a new state, writing the transition row in
// the same statement pair the 00603 binding requires.
//
// It does not mint, destroy or re-classify Credits: those are separate,
// explicitly named operations (MintFrom, Reverse, SettleLots), because a state
// change and an economic effect that happen silently together are how a
// duplicated webhook mints twice.
func (s *Service) AdvanceFunding(ctx context.Context, tx pgx.Tx, id FundingID, to FundingState, reason, providerEvent string) (Funding, error) {
	if tx == nil {
		return Funding{}, errs.New(errs.CodeInternal, "credit: AdvanceFunding requires a transaction")
	}
	if !to.Valid() {
		return Funding{}, errs.Newf(errs.CodeValidationFailed, "credit: unknown funding state %q", to)
	}
	if strings.TrimSpace(reason) == "" {
		return Funding{}, errs.New(errs.CodeValidationFailed, "credit: a funding state change requires a reason")
	}
	f, err := scanFunding(tx.QueryRow(ctx,
		`SELECT `+fundingColumns+` FROM credit_fundings WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Funding{}, errs.New(errs.CodeNotFound, "credit funding not found").WithField("funding_id", id.String())
		}
		return Funding{}, mapError(err)
	}
	if f.State == to {
		// Provider webhooks are re-delivered and arrive out of order. Landing
		// on the state we are already in is a duplicate, not an error.
		return f, nil
	}
	if !CanTransitionFunding(f.State, to) {
		return Funding{}, errs.Newf(errs.CodeInvalidStateTransition,
			"credit funding cannot go %s -> %s", f.State, to).
			WithField("funding_id", id.String()).
			WithField("from", string(f.State)).
			WithField("to", string(to))
	}
	actorType, actorID := actorFrom(ctx)
	var providerEv any
	if providerEvent != "" {
		providerEv = providerEvent
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO credit_funding_transitions
		   (id, funding_id, from_state, to_state, actor_type, actor_id, reason, provider_event)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		NewFundingTransitionID(), id, string(f.State), string(to), actorType, actorID, reason, providerEv); err != nil {
		return Funding{}, mapError(err)
	}
	set := `state = $2`
	switch to {
	case FundingSettled:
		set += `, settled_at = now()`
	case FundingReversed, FundingRefunded:
		set += `, reversed_at = now()`
	case FundingFailed:
		set += `, failure_reason = $3`
	}
	args := []any{id, string(to)}
	if to == FundingFailed {
		args = append(args, reason)
	}
	updated, err := scanFunding(tx.QueryRow(ctx,
		`UPDATE credit_fundings SET `+set+` WHERE id = $1 RETURNING `+fundingColumns, args...))
	if err != nil {
		return Funding{}, mapError(err)
	}
	return updated, nil
}

// MintFrom issues the Credits a captured funding paid for, exactly once.
//
// The funding must be in CAPTURED. Success moves it to REVERSIBLE and records
// the lot on the funding row, so a repeated call finds lot_id already set and
// returns the same lot instead of minting again.
func (s *Service) MintFrom(ctx context.Context, tx pgx.Tx, id FundingID, effectiveAt time.Time) (Lot, error) {
	f, err := scanFunding(tx.QueryRow(ctx,
		`SELECT `+fundingColumns+` FROM credit_fundings WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Lot{}, errs.New(errs.CodeNotFound, "credit funding not found").WithField("funding_id", id.String())
		}
		return Lot{}, mapError(err)
	}
	if f.LotID != nil {
		return s.Lot(ctx, tx, *f.LotID)
	}
	if f.State != FundingCaptured {
		return Lot{}, errs.Newf(errs.CodeInvalidStateTransition,
			"Credits are minted from a CAPTURED funding; this one is %s", f.State).
			WithField("funding_id", id.String())
	}
	lot, err := s.Issue(ctx, tx, IssueRequest{
		AccountID: f.AccountID,
		Quantity:  f.CreditQuantity,
		Origin:    valuedomain.OriginPurchased,
		// Captured is not settled. The Credits are spendable immediately --
		// that is the product -- and are not payout-eligible until the funding
		// itself settles.
		Finality:         valuedomain.FinalityReversible,
		Reference:        Reference{Type: "credit_funding", ID: f.ID.String()},
		FundingReference: &Reference{Type: "credit_funding", ID: f.ID.String()},
		IdempotencyKey:   "credit_funding:" + f.ID.String() + ":mint",
		Reason:           "Credits purchased",
		EffectiveAt:      effectiveAt,
	})
	if err != nil {
		return Lot{}, err
	}
	if _, err := s.AdvanceFunding(ctx, tx, id, FundingReversible,
		"Credits minted from captured funding", ""); err != nil {
		return Lot{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE credit_fundings SET lot_id = $2 WHERE id = $1`, id, lot.ID); err != nil {
		return Lot{}, mapError(err)
	}
	return lot, nil
}

// ReverseResult describes what a chargeback actually did.
type ReverseResult struct {
	// Destroyed is the value clawed back out of the account's Credit balance.
	Destroyed money.Quantity
	// Deficit is what the account had already spent and therefore owes. It is
	// the uncollateralised hole PART XI is about, made visible and collectable
	// instead of absorbed silently.
	Deficit money.Quantity
	// LotIDs are the provenance lots the reversal touched.
	LotIDs []LotID
}

// Reverse claws back the Credits a funding paid for after the funder took the
// money back.
//
// The economics that matter: a user can buy 10,000 Credits, spend them on the
// internal market, and then charge back. The Credits are gone, the value they
// bought is in someone else's hands, and the platform is out the money. This
// records that honestly — destroying what remains and booking the rest as a
// DEFICIT the account owes — rather than letting the balance go negative or
// quietly unwinding other people's trades.
//
// It deliberately does NOT reverse the trades the Credits funded. Silently
// reversing a market trade because a third party charged back would take value
// from an innocent counterparty; PART LXXXI forbids it.
func (s *Service) Reverse(ctx context.Context, tx pgx.Tx, id FundingID, effectiveAt time.Time, reason string) (ReverseResult, error) {
	if tx == nil {
		return ReverseResult{}, errs.New(errs.CodeInternal, "credit: Reverse requires a transaction")
	}
	f, err := s.Funding(ctx, tx, id)
	if err != nil {
		return ReverseResult{}, err
	}
	if f.LotID == nil {
		// Nothing was ever minted; the state change is the whole reversal.
		if _, err := s.AdvanceFunding(ctx, tx, id, FundingReversed, reason, ""); err != nil {
			return ReverseResult{}, err
		}
		return ReverseResult{}, nil
	}
	assetID, err := s.AssetID(ctx, tx)
	if err != nil {
		return ReverseResult{}, err
	}
	lot, err := s.Lot(ctx, tx, *f.LotID)
	if err != nil {
		return ReverseResult{}, err
	}

	// What the account can actually give back is bounded by its balance, not
	// by what the lot still records: the user may have spent Credits from
	// other lots too.
	balance, err := s.creditBalance(ctx, tx, f.AccountID, assetID)
	if err != nil {
		return ReverseResult{}, err
	}
	amount := lot.Quantity
	covered := balance.Min(amount)
	shortfall := amount.Sub(covered)

	custBalance := ledger.CustomerAccount(f.AccountID, ledger.CodeCreditBalance, assetID)
	custIssuance := ledger.CustomerAccount(f.AccountID, ledger.CodeCreditIssuance, assetID)
	custDeficit := ledger.CustomerAccount(f.AccountID, ledger.CodeDeficit, assetID)
	ref := ledger.FinancialEventReference{Type: "credit_funding", ID: f.ID.String()}

	res := ReverseResult{Destroyed: covered, Deficit: shortfall, LotIDs: []LotID{lot.ID}}

	if covered.IsPositive() {
		post, err := s.poster.Post(ctx, tx, ledger.Posting{
			Kind:           ledger.KindCreditReversed,
			IdempotencyKey: "credit_funding:" + f.ID.String() + ":reversal",
			Reference:      ref,
			EffectiveAt:    effectiveAt,
			Description:    "Credit funding reversed",
			Metadata:       map[string]any{"reason": reason},
			Entries: []ledger.Entry{
				{Account: custIssuance, Side: ledger.Debit, Quantity: covered},
				{Account: custBalance, Side: ledger.Credit, Quantity: covered},
			},
		})
		if err != nil {
			return ReverseResult{}, err
		}
		// Consume against the reversal so the lots record where the destroyed
		// units came from. Finality is not required to be spendable here: a
		// clawback must work on disputed value too.
		if _, err := s.Consume(ctx, tx, ConsumeRequest{
			AccountID:   f.AccountID,
			Quantity:    covered,
			JournalTxID: post.TransactionID,
			Reference:   Reference{Type: "credit_funding_reversal", ID: f.ID.String()},
			Reason:      reason,
		}); err != nil {
			return ReverseResult{}, err
		}
	}
	if shortfall.IsPositive() {
		if _, err := s.poster.Post(ctx, tx, ledger.Posting{
			Kind:           ledger.KindCreditReversed,
			IdempotencyKey: "credit_funding:" + f.ID.String() + ":reversal_deficit",
			Reference:      ref,
			EffectiveAt:    effectiveAt,
			Description:    "Credit funding reversal exceeded the balance; deficit recorded",
			Metadata:       map[string]any{"reason": reason},
			Entries: []ledger.Entry{
				{Account: custIssuance, Side: ledger.Debit, Quantity: shortfall},
				{Account: custDeficit, Side: ledger.Credit, Quantity: shortfall},
			},
		}); err != nil {
			return ReverseResult{}, err
		}
	}

	if err := s.SetFinality(ctx, tx, lot.ID, valuedomain.FinalityReversed,
		Reference{Type: "credit_funding", ID: f.ID.String()}, reason); err != nil {
		return ReverseResult{}, err
	}
	if _, err := s.AdvanceFunding(ctx, tx, id, FundingReversed, reason, ""); err != nil {
		return ReverseResult{}, err
	}
	return res, nil
}

// SettleFunding marks a funding final and promotes its Credits from REVERSIBLE
// to SETTLED, which is what makes them capable of ever being paid out.
func (s *Service) SettleFunding(ctx context.Context, tx pgx.Tx, id FundingID, reason string) error {
	f, err := s.Funding(ctx, tx, id)
	if err != nil {
		return err
	}
	if _, err := s.AdvanceFunding(ctx, tx, id, FundingSettled, reason, ""); err != nil {
		return err
	}
	if f.LotID == nil {
		return nil
	}
	return s.SetFinality(ctx, tx, *f.LotID, valuedomain.FinalitySettled,
		Reference{Type: "credit_funding", ID: f.ID.String()}, reason)
}

// DisputeFunding freezes the Credits a funding produced while a dispute runs.
func (s *Service) DisputeFunding(ctx context.Context, tx pgx.Tx, id FundingID, reason string) error {
	f, err := s.Funding(ctx, tx, id)
	if err != nil {
		return err
	}
	if _, err := s.AdvanceFunding(ctx, tx, id, FundingDisputed, reason, ""); err != nil {
		return err
	}
	if f.LotID == nil {
		return nil
	}
	return s.SetFinality(ctx, tx, *f.LotID, valuedomain.FinalityDisputed,
		Reference{Type: "credit_funding", ID: f.ID.String()}, reason)
}

// creditBalance reads the account's CREDIT_BALANCE from the ledger projection.
func (s *Service) creditBalance(ctx context.Context, q db.Querier, accountID accounts.AccountID, assetID assets.AssetID) (money.Quantity, error) {
	var raw string
	err := q.QueryRow(ctx,
		`SELECT coalesce((SELECT b.balance
		                    FROM ledger_accounts la JOIN ledger_balances b ON b.ledger_account_id = la.id
		                   WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = $1
		                     AND la.code = 'CREDIT_BALANCE' AND la.asset_id = $2), 0)::text`,
		accountID, assetID).Scan(&raw)
	if err != nil {
		return money.Quantity{}, mapError(err)
	}
	q2, err := money.ParseQuantity(raw)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodeInternal, "credit: balance is not an integer")
	}
	return q2, nil
}
