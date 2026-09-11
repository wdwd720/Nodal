package payout

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/terms"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Poster is the part of internal/ledger this package uses.
type Poster interface {
	Post(ctx context.Context, tx pgx.Tx, p ledger.Posting) (ledger.PostResult, error)
}

// Credits is the part of internal/credit this package uses.
type Credits interface {
	Consume(ctx context.Context, tx pgx.Tx, r credit.ConsumeRequest) ([]credit.Allocation, error)
	Restore(ctx context.Context, tx pgx.Tx, r credit.RestoreRequest) error
	EligibleLots(ctx context.Context, q db.Querier, r credit.BalanceRequest) ([]credit.Lot, money.Quantity, error)
	Lots(ctx context.Context, q db.Querier, accountID accountsAccountID) ([]credit.Lot, error)
	AssetID(ctx context.Context, q db.Querier) (assetID assetIDType, err error)
}

// assetIDType is the asset identifier type, aliased so the interface above
// does not force every caller to import internal/assets.
type assetIDType = assetsAssetID

// KillSwitchChecker is the emergency-control guard. *killswitch.Checker
// satisfies it, and internal/withdrawal declares the same interface for the
// same reason: the check reads Postgres inside the transaction that authorizes
// the action, so a switch activated a moment ago is always seen.
type KillSwitchChecker interface {
	Check(ctx context.Context, q db.Querier, a killswitch.Action) error
}

// Accounts reads account status. *accounts.Repository satisfies it.
type Accounts interface {
	Get(ctx context.Context, q db.Querier, accountID accountsAccountID) (accountsAccount, error)
}

// Service creates, reserves, submits and reconciles payouts.
type Service struct {
	poster    Poster
	credits   Credits
	engine    *Engine
	providers *Registry
	clk       clock.Clock
	kills     KillSwitchChecker
	accounts  Accounts
}

// NewService returns a Service. No argument may be nil.
//
// kills and accounts are required rather than optional for the reason F-163
// records: this package creates, reserves and submits every conversion request
// the product has, and it imported no kill switch at all, so WITHDRAWALS_DISABLE,
// GLOBAL_NEW_RISK_KILL and ACCOUNT_FREEZE stopped none of it while
// POLICY_AUTHORITY §2 said all three blocked the WITHDRAW class. A guard a
// caller may leave nil is a guard that is eventually left nil.
func NewService(poster Poster, credits Credits, engine *Engine, providers *Registry, clk clock.Clock, kills KillSwitchChecker, accts Accounts) *Service {
	if poster == nil || credits == nil || engine == nil || providers == nil || clk == nil || kills == nil || accts == nil {
		panic("payout: NewService requires a poster, credits, an engine, a provider registry, a clock, a kill-switch checker and an account reader")
	}
	return &Service{poster: poster, credits: credits, engine: engine, providers: providers, clk: clk, kills: kills, accounts: accts}
}

// guardWithdraw is the WITHDRAW-class check every entry point that moves a
// conversion request forward makes, inside the transaction that authorizes it.
//
// Two controls, in the order an operator would expect them to fire: the kill
// switches (WITHDRAWALS_DISABLE and GLOBAL_NEW_RISK_KILL are global, and
// ACCOUNT_FREEZE names the account), then the account's own status. Both are
// what internal/withdrawal -- the older crypto-address path, which refuses
// everything today -- has always done at the same boundary; the conversion
// request is the withdrawal surface that actually reaches a provider, and it
// made neither check (F-163, D-092).
func (s *Service) guardWithdraw(ctx context.Context, q db.Querier, accountID accountsAccountID) error {
	if err := s.kills.Check(ctx, q, killswitch.Action{Class: killswitch.Withdraw, AccountID: accountID.String()}); err != nil {
		return err
	}
	acct, err := s.accounts.Get(ctx, q, accountID)
	if err != nil {
		return err
	}
	if acct.Status != accountsStatusActive {
		code := errs.CodeForbidden
		if acct.Status == accountsStatusFrozen {
			code = errs.CodeAccountFrozen
		}
		return errs.Newf(code, "account status %s does not allow a conversion request", acct.Status).
			WithField("account_status", string(acct.Status))
	}
	return nil
}

// Provider returns a registered payout provider, so callers can ask what it
// actually supports rather than assuming (PART LXXVI).
func (s *Service) Provider(name string) (Provider, error) { return s.providers.Get(name) }

// ProviderNames lists the configured payout providers, sorted.
//
// A deployment runs one payout slot, so callers use this to find the one there
// is rather than to choose between several. An empty list is the honest state
// of a deployment with no conversion contract, and every surface that reads it
// answers PROVIDER_UNAVAILABLE rather than inventing a provider.
func (s *Service) ProviderNames() []string { return s.providers.Names() }

// RequiredDisclosure is the legal document a person must have accepted before
// value may leave.
//
// It is terms.WithdrawalDisclosure, named through the registry rather than as a
// string, so a rename in the registry is a compile error here rather than a
// control that silently stops applying.
var RequiredDisclosure = terms.WithdrawalDisclosure

// disclosureRefusal is the error both entry points raise.
//
// # Why this is in the domain service and not only at the boundary
//
// §48 puts the withdrawal disclosure at the moment somebody asks to take value
// out, deliberately NOT at signup. A check that lived only in the HTTP handler
// would be a control that applies to one of the ways into this package, and
// this package has several: an operator resolving a stuck payout, a retry, a
// worker. The refusal belongs where the act is, so every caller meets it.
//
// # Why it refuses rather than producing an ineligible Decision
//
// A Decision is about WHICH of an account's units may leave, and an ineligible
// one still creates a payout_requests row. "This person has not agreed to the
// terms under which value leaves" is not a fact about their units, and a
// request row standing against it would be a record of an ask that should never
// have been taken. So it is an error, with a code the surface can act on, and
// nothing is written.
func disclosureRefusal() error {
	return errs.New(errs.CodeTermsAcceptanceRequired,
		"the withdrawal disclosure has not been accepted at the version now served").
		WithField("documents", []string{string(RequiredDisclosure)}).
		WithField("action", "ACCEPT_TERMS")
}

// Create evaluates eligibility and, if the full amount is eligible, reserves
// the exact units in one transaction.
//
// Reserving at creation rather than at submission is deliberate. Between the
// two there is a verification step that can take days, and value that is
// "about to be withdrawn" but still spendable is value that can be spent twice
// — once on the internal market and once out of the system.
func (s *Service) Create(ctx context.Context, tx pgx.Tx, r CreateRequest, in EligibilityInput) (Request, Decision, error) {
	if err := r.Validate(); err != nil {
		return Request{}, Decision{}, err
	}
	if !r.DisclosureAccepted {
		return Request{}, Decision{}, disclosureRefusal()
	}
	if tx == nil {
		return Request{}, Decision{}, errs.New(errs.CodeInternal, "payout: Create requires a transaction")
	}
	// The emergency controls, in the transaction that authorizes the request and
	// before anything is consumed, decided or written: a quote is burned by the
	// branch below, and burning it under an active WITHDRAWALS_DISABLE would be a
	// switch that costs the user something while stopping nothing.
	if err := s.guardWithdraw(ctx, tx, r.AccountID); err != nil {
		return Request{}, Decision{}, err
	}
	if existing, found, err := s.byIdempotencyKey(ctx, tx, r.IdempotencyKey); err != nil {
		return Request{}, Decision{}, err
	} else if found {
		// The key is globally unique on this table, and the HTTP layer's own
		// idempotency record is keyed by actor -- so a DIFFERENT caller reusing
		// a key passes the boundary and arrives here. Returning the row without
		// asking whose it is renders another account's payout to them: its
		// account id, its requested, reserved and settled quantities, its
		// destination and its failure reason. It also silently discards the
		// caller's own request (F-106).
		//
		// The same comparison internal/credit, internal/funding,
		// internal/withdrawal and internal/capital already make.
		if existing.AccountID != r.AccountID {
			return Request{}, Decision{}, errs.New(errs.CodeInvalidIdempotencyReuse,
				"payout: idempotency key belongs to another account").
				WithField("idempotency_key", r.IdempotencyKey)
		}
		return existing, Decision{}, nil
	}

	// The quote is consumed BEFORE eligibility is evaluated, so a quote that
	// has expired or has already funded a payout refuses the request without
	// anything having been decided about the money. It is also what makes a
	// quote fund exactly one payout: the row is locked here and the same
	// transaction carries the reservation.
	//
	// Validate has already refused a request with no quote (D-119), so there is
	// no branch here that a missing one could take -- which was the whole shape
	// of F-224: the minimum and the fee lived inside `if r.QuoteID != nil` and
	// a request that named no quote met neither.
	quote, err := s.consumeQuote(ctx, tx, *r.QuoteID, r.AccountID, r.EffectiveAt)
	if err != nil {
		return Request{}, Decision{}, err
	}
	if r.DestinationID == nil || *r.DestinationID != quote.DestinationID {
		return Request{}, Decision{}, errs.New(errs.CodeValidationFailed,
			"that quote was given for a different destination").
			WithField("field", "destination_id")
	}
	if quote.GrossQuantity.Cmp(r.Quantity) != 0 {
		return Request{}, Decision{}, errs.New(errs.CodeValidationFailed,
			"that quote was given for a different amount").
			WithField("quoted", quote.GrossQuantity.String()).
			WithField("requested", r.Quantity.String())
	}
	// The provider's minimum, judged NET of fees and against what the provider
	// publishes TODAY. consumeQuote has already refused a quote whose own
	// minimum_ok was false; this is the other direction -- a minimum raised
	// between the quote and the commit -- and it is why the terms are an input
	// to this call rather than a number copied onto the quote row.
	if minimum := r.ProviderTerms.MinimumAmount.Minor(); quote.NetAmountMinor < minimum {
		return Request{}, Decision{}, errs.Newf(errs.CodeValidationFailed,
			"this payout would net %d and the provider will not send less than %d",
			quote.NetAmountMinor, minimum).
			WithField("refusal", string(ReasonMinimumNotMet)).
			WithField("net_amount_minor", quote.NetAmountMinor).
			WithField("minimum_amount_minor", minimum).
			WithField("currency", quote.Currency)
	}

	in.AccountID = r.AccountID
	in.Requested = r.Quantity
	decision, err := s.engine.Evaluate(ctx, tx, in)
	if err != nil {
		return Request{}, Decision{}, err
	}

	creditAsset, err := s.credits.AssetID(ctx, tx)
	if err != nil {
		return Request{}, Decision{}, err
	}
	reasons, err := json.Marshal(decision.ReasonStrings())
	if err != nil {
		return Request{}, Decision{}, errs.Wrap(err, errs.CodeInternal, "payout: encode reasons")
	}

	req := Request{
		ID: NewRequestID(), AccountID: r.AccountID, DestinationID: r.DestinationID,
		CreditAssetID: creditAsset, State: StateEligibilityCheck,
		RequestedQuantity: r.Quantity, PolicyVersion: decision.PolicyVersion,
		PolicyHash: decision.PolicyHash, EligibilityReasons: decision.ReasonStrings(),
		VerificationLevel: in.Verified, IdempotencyKey: r.IdempotencyKey,
	}
	req.QuoteID = &quote.ID
	req.QuoteGrossAmountMinor = quote.GrossAmountMinor
	req.QuoteFeeAmountMinor = quote.FeeAmountMinor
	req.QuoteNetAmountMinor = quote.NetAmountMinor
	req.QuoteCurrency = quote.Currency
	err = tx.QueryRow(ctx,
		`INSERT INTO payout_requests
		   (id, account_id, destination_id, credit_asset_id, state, requested_quantity,
		    policy_version, policy_hash, eligibility_reasons, verification_level, idempotency_key, quote_id,
		    quote_gross_amount_minor, quote_fee_amount_minor, quote_net_amount_minor, quote_currency)
		 VALUES ($1,$2,$3,$4,'ELIGIBILITY_CHECK',$5::numeric,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		 RETURNING created_at, updated_at`,
		req.ID, req.AccountID, *r.DestinationID, req.CreditAssetID, req.RequestedQuantity.String(),
		req.PolicyVersion, req.PolicyHash, reasons, string(req.VerificationLevel),
		req.IdempotencyKey, quote.ID,
		quote.GrossAmountMinor, quote.FeeAmountMinor, quote.NetAmountMinor, quote.Currency).
		Scan(&req.CreatedAt, &req.UpdatedAt)
	if err != nil {
		return Request{}, Decision{}, mapError(err)
	}

	if !decision.Sufficient() {
		// Nothing is reserved and nothing has moved. Two different answers are
		// possible here and conflating them would be the product's biggest
		// missed opportunity: "you cannot" and "you have not verified yet".
		if decision.VerificationWouldSuffice {
			pending, terr := s.transition(ctx, tx, req.ID, StateVerificationRequired,
				"eligible once identity verification reaches "+string(decision.RequiredVerification), "")
			if terr != nil {
				return Request{}, Decision{}, terr
			}
			return pending, decision, nil
		}
		rejected, rerr := s.transition(ctx, tx, req.ID, StateRejected,
			"eligibility: "+strings.Join(decision.ReasonStrings(), ", "), "")
		if rerr != nil {
			return Request{}, Decision{}, rerr
		}
		return rejected, decision, nil
	}

	reserved, err := s.reserve(ctx, tx, req, decision, r.EffectiveAt, r.CorrelationID)
	if err != nil {
		return Request{}, Decision{}, err
	}
	final, err := s.transitionWith(ctx, tx, reserved.ID, StateVerified,
		s.reservedChange("eligible and reserved", reserved.ReservedQuantity))
	if err != nil {
		return Request{}, Decision{}, err
	}
	return final, decision, nil
}

// CompleteVerification re-evaluates a payout that was waiting on identity
// verification and, if it now passes, reserves the value.
//
// Re-evaluating rather than trusting the earlier decision matters: between the
// request and the verification the user may have spent the Credits, the policy
// may have changed, or the capability may have been revoked. The verification
// answers one question; it does not settle the others.
func (s *Service) CompleteVerification(ctx context.Context, tx pgx.Tx, requestID RequestID, in EligibilityInput, effectiveAt time.Time) (Request, Decision, error) {
	req, err := s.forUpdate(ctx, tx, requestID)
	if err != nil {
		return Request{}, Decision{}, err
	}
	switch req.State {
	case StateVerificationRequired, StateVerificationPending:
	default:
		return Request{}, Decision{}, errs.Newf(errs.CodeInvalidStateTransition,
			"only a payout awaiting verification can complete it; this one is %s", req.State).
			WithField("payout_id", requestID.String())
	}
	// The same guard as Create, for the same reason it re-evaluates eligibility
	// rather than trusting the earlier decision: days can pass here, and this is
	// the call that reserves the value.
	if err := s.guardWithdraw(ctx, tx, req.AccountID); err != nil {
		return Request{}, Decision{}, err
	}

	in.AccountID = req.AccountID
	in.Requested = req.RequestedQuantity
	decision, err := s.engine.Evaluate(ctx, tx, in)
	if err != nil {
		return Request{}, Decision{}, err
	}
	// The re-decision, recorded whole. The reasons used to be left at whatever
	// Create wrote, so a request that became eligible on verification still
	// rendered the shortfall it no longer had.
	reasons, err := json.Marshal(decision.ReasonStrings())
	if err != nil {
		return Request{}, Decision{}, errs.Wrap(err, errs.CodeInternal, "payout: encode reasons")
	}
	if _, err := tx.Exec(ctx,
		`UPDATE payout_requests
		    SET verification_level = $2, policy_version = $3, policy_hash = $4, eligibility_reasons = $5
		  WHERE id = $1`,
		req.ID, string(in.Verified), decision.PolicyVersion, decision.PolicyHash, reasons); err != nil {
		return Request{}, Decision{}, mapError(err)
	}
	if !decision.Sufficient() {
		rejected, rerr := s.transition(ctx, tx, req.ID, StateRejected,
			"still not eligible after verification: "+strings.Join(decision.ReasonStrings(), ", "), "")
		return rejected, decision, rerr
	}
	reserved, err := s.reserve(ctx, tx, req, decision, effectiveAt, "")
	if err != nil {
		return Request{}, Decision{}, err
	}
	final, err := s.transitionWith(ctx, tx, reserved.ID, StateVerified,
		s.reservedChange("verification complete; value reserved", reserved.ReservedQuantity))
	if err != nil {
		return Request{}, Decision{}, err
	}
	return final, decision, nil
}

// reserve moves the approved units out of the spendable domain.
//
// What is reserved is the GROSS: the quote's fee is taken out of the amount
// that leaves, not added to it, so the units the customer gives up are exactly
// the units they asked to convert and the provider's fee is inside them. A
// reservation of the net would leave the fee spendable and the account short at
// settlement (D-119).
//
// Consumption is restricted to exactly the origins the decision approved, so
// lot selection cannot stray outside it even though the two run a moment apart
// — a promotional grant can never be swept into a payout approved for creator
// earnings.
func (s *Service) reserve(ctx context.Context, tx pgx.Tx, req Request, d Decision, effectiveAt time.Time, correlationID string) (Request, error) {
	conv := valuedomain.ConversionKey{From: valuedomain.InternalCredit, To: valuedomain.PayoutPending}
	post, err := s.poster.Post(ctx, tx, ledger.Posting{
		Kind:           ledger.KindPayoutReserved,
		IdempotencyKey: "payout:" + req.ID.String() + ":reserve",
		Reference:      ledger.FinancialEventReference{Type: "payout_request", ID: req.ID.String()},
		EffectiveAt:    effectiveAt,
		CorrelationID:  correlationID,
		Description:    "Credits reserved against a payout request",
		Conversion:     &conv,
		Entries: []ledger.Entry{
			{
				Account: ledger.CustomerAccount(req.AccountID, ledger.CodeCreditBalance, req.CreditAssetID),
				Side:    ledger.Credit, Quantity: d.Requested,
			},
			{
				Account: ledger.CustomerAccount(req.AccountID, ledger.CodePayoutReserved, req.CreditAssetID),
				Side:    ledger.Debit, Quantity: d.Requested,
			},
		},
		Metadata: map[string]any{"policy_version": d.PolicyVersion, "policy_hash": d.PolicyHash},
	})
	if err != nil {
		return Request{}, err
	}

	allocs, err := s.credits.Consume(ctx, tx, credit.ConsumeRequest{
		AccountID:                req.AccountID,
		Quantity:                 d.Requested,
		JournalTxID:              post.TransactionID,
		Reference:                credit.Reference{Type: "payout_request", ID: req.ID.String()},
		Reason:                   "reserved against a payout request",
		RequireSpendableFinality: true,
		AllowedOrigins:           d.Origins,
	})
	if err != nil {
		return Request{}, err
	}

	// The reservation is NOT written here. 00807 made the quantities on
	// payout_requests the transition row's to write, and the reservation
	// belongs to the VERIFIED step both callers take immediately after this
	// returns; the allocations below are balanced against it by a DEFERRED
	// constraint, so the order inside the transaction does not matter.
	for _, a := range allocs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO payout_allocations (id, request_id, lot_id, origin, quantity)
			 VALUES ($1,$2,$3,$4,$5::numeric)`,
			NewAllocationID(), req.ID, a.LotID, string(a.Origin), a.Quantity.String()); err != nil {
			return Request{}, mapError(err)
		}
	}
	req.ReservedQuantity = d.Requested
	return req, nil
}

// reservedChange is the VERIFIED step a reservation rides on.
func (s *Service) reservedChange(reason string, qty money.Quantity) stateChange {
	at := s.clk.Now().UTC()
	return stateChange{Reason: reason, ReservedQuantity: &qty, ReservedAt: &at}
}

// releasedChange is the step a returned reservation rides on: the quantity goes
// to zero in the same row that records why.
func releasedChange(reason string) stateChange {
	zero := money.Quantity{}
	return stateChange{Reason: reason, ReservedQuantity: &zero}
}

// Submit hands a reserved payout to its provider.
//
// The provider idempotency key is written and COMMITTED before the provider is
// called. That ordering is the whole crash-safety story: if the process dies
// between the write and the call, or between the call and recording the
// answer, the key is on disk and Reconcile can ask the provider what happened
// to it. A key generated at call time would leave nothing to ask about.
func (s *Service) Submit(ctx context.Context, d *db.DB, requestID RequestID, providerName string) (Request, error) {
	if d == nil {
		return Request{}, errs.New(errs.CodeInternal, "payout: Submit requires a database")
	}
	provider, err := s.providers.Get(providerName)
	if err != nil {
		return Request{}, err
	}
	// A payout is submitted to the provider it was claimed for.
	//
	// providerName is a caller argument and was never compared to the persisted
	// r.Provider, and the re-entry branch did not overwrite it either. So
	// Submit(id, "providerB") on a request already claimed for providerA called
	// providerB with providerA's idempotency key: two providers, one key, two
	// disbursements, and neither of them able to dedupe the other (F-115).
	if err := s.providerMatches(ctx, d, requestID, providerName); err != nil {
		return Request{}, err
	}

	// Phase one: claim the request and persist the key, in its own committed
	// transaction.
	//
	// claimed says whether THIS call did the claiming, and it exists because
	// the state does not answer that question. The early return below used to
	// leave `req` in whatever state it found, and phase two then guarded on
	// `req.State != StateSubmitted` -- which sent PROVIDER_PENDING and
	// PAYOUT_STATUS_UNKNOWN home and let SUBMITTED fall straight through to the
	// provider call. SUBMITTED is precisely what a crash between this commit
	// and applyProviderResult leaves behind, so the branch whose comment says
	// "do not resubmit" was the one that resubmitted (F-115).
	//
	// Two concurrent Submits reached it the same way: the second blocks on
	// FOR UPDATE, is released by the first's COMMIT, re-reads SUBMITTED under
	// READ COMMITTED, and returns here -- with the row lock already gone,
	// because phase two is outside any transaction. Nothing serialised the
	// external call.
	var (
		req       Request
		claimed   bool
		submitReq SubmitRequest
	)
	err = d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		r, err := s.forUpdate(ctx, tx, requestID)
		if err != nil {
			return err
		}
		if r.State == StateSubmitted || r.State == StateProviderPending || r.State == StateStatusUnknown {
			// Already claimed by an earlier attempt. Reconcile, do not resubmit.
			req = r
			return nil
		}
		if r.State != StateVerified {
			return errs.Newf(errs.CodeInvalidStateTransition,
				"a payout is submitted from VERIFIED; this one is %s", r.State).
				WithField("payout_id", requestID.String())
		}
		// The last point at which a switch can still stop value leaving. The
		// reservation is already made and the request is about to be claimed and
		// handed to a provider under a committed idempotency key, after which only
		// reconciliation can answer what happened -- so an operator who activates
		// WITHDRAWALS_DISABLE, GLOBAL_NEW_RISK_KILL or ACCOUNT_FREEZE between the
		// request and the sweep gets what they asked for. The request stays in
		// VERIFIED with the value reserved, and the next sweep submits it once the
		// switch is released.
		if err := s.guardWithdraw(ctx, tx, r.AccountID); err != nil {
			return err
		}
		key := "nodal-payout-" + r.ID.String()
		// What the provider is about to be told, assembled in the transaction
		// that claims the request and refused BEFORE the claim if it is not an
		// instruction anybody could act on.
		//
		// It used to be `SubmitRequest{IdempotencyKey: ..., Reference: ...,
		// Amount: money.USD{}}` -- no amount, no destination, no currency, no
		// kind -- and the request still reached SETTLED with the whole reserved
		// quantity posted as having left the system (F-226). Building it here
		// rather than after the claim means a request that cannot be described
		// to a provider stays in VERIFIED with its value reserved, instead of
		// being claimed under a committed key nobody can act on.
		built, berr := s.submitRequestFor(ctx, tx, r, key)
		if berr != nil {
			return berr
		}
		submitReq = built
		if _, err := tx.Exec(ctx,
			`UPDATE payout_requests SET provider = $2, provider_idempotency_key = $3, submitted_at = $4
			  WHERE id = $1`, r.ID, providerName, key, s.clk.Now()); err != nil {
			return mapError(err)
		}
		moved, err := s.transition(ctx, tx, r.ID, StateSubmitted, "submitting to "+providerName, "")
		if err != nil {
			return err
		}
		moved.Provider, moved.ProviderIdempotencyKey = providerName, key
		req, claimed = moved, true
		return nil
	})
	if err != nil {
		return Request{}, err
	}
	if !claimed {
		return req, nil
	}

	// Phase two: the external call. Everything from here is recoverable from
	// the key committed above.
	result, callErr := provider.Submit(ctx, submitReq)
	return s.applyProviderResult(ctx, d, req.ID, providerName, result, callErr)
}

// submitRequestFor builds the instruction a provider is given, from the
// destination and the quote the request was created against.
//
// The amount is the quote's NET, in the quote's currency: the gross is what the
// customer gave up and the fee is inside it, so the net is what the provider is
// asked to send. Both are recorded on the request, so this reads a fact rather
// than recomputing one against a fee schedule that may since have moved.
func (s *Service) submitRequestFor(ctx context.Context, tx pgx.Tx, r Request, key string) (SubmitRequest, error) {
	if r.DestinationID == nil || r.DestinationID.IsZero() {
		return SubmitRequest{}, errs.New(errs.CodeInvalidStateTransition,
			"this payout names no destination, so there is nobody to pay").
			WithField("payout_id", r.ID.String())
	}
	dest, err := s.Destination(ctx, tx, *r.DestinationID)
	if err != nil {
		return SubmitRequest{}, err
	}
	if dest.AccountID != r.AccountID {
		// Not reachable through Create, which refuses a quote for another
		// account's destination. Said here anyway, because this is the last
		// place before value is sent somewhere.
		return SubmitRequest{}, errs.New(errs.CodeForbidden,
			"this payout names a destination that belongs to another account").
			WithField("payout_id", r.ID.String())
	}
	out := SubmitRequest{
		IdempotencyKey:       key,
		Reference:            r.ID.String(),
		DestinationReference: dest.ProviderReference,
		DestinationKind:      dest.Kind,
		Amount:               money.USDFromMinor(r.QuoteNetAmountMinor),
		Currency:             r.QuoteCurrency,
	}
	if err := out.Validate(); err != nil {
		return SubmitRequest{}, err
	}
	return out, nil
}

// applyProviderResult records what a provider said and moves the request.
//
// A settlement that cannot be POSTED is its own scenario and is handled
// explicitly rather than propagated: the provider says the money is gone, and
// Nodal cannot record that -- because a capability was revoked mid-flight, or a
// ledger invariant refused the posting. Returning an error there would leave
// the request in SUBMITTED with the value still reserved and no record of why,
// which is the worst of the three possible outcomes. Instead the request goes
// to MANUAL_REVIEW with the reason attached, the reservation stays, and a human
// resolves it. That is what MANUAL_REVIEW is for.
func (s *Service) applyProviderResult(ctx context.Context, d *db.DB, requestID RequestID, providerName string, result SubmitResult, callErr error) (Request, error) {
	out, err := s.applyProviderResultTx(ctx, d, requestID, providerName, result, callErr)
	if err == nil {
		return out, nil
	}
	if callErr == nil && result.Status == ProviderSettled {
		reviewed, rerr := s.toManualReview(ctx, d, requestID,
			"the provider settled this payout and it could not be recorded: "+err.Error())
		if rerr != nil {
			// Parking it failed too. Return the root cause rather than the
			// symptom, with the second failure attached, because an operator
			// chasing this needs to know both that the settlement did not
			// record and that the request is still sitting in SUBMITTED.
			return Request{}, errs.Newf(errs.CodeInternal,
				"%s (and the request could not be parked for review: %s)", err.Error(), rerr.Error())
		}
		return reviewed, err
	}
	return out, err
}

// toManualReview parks a request for a human, in its own transaction so it
// survives whatever failed before it.
func (s *Service) toManualReview(ctx context.Context, d *db.DB, requestID RequestID, reason string) (Request, error) {
	var out Request
	err := d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		r, err := s.transition(ctx, tx, requestID, StateManualReview, reason, "")
		out = r
		return err
	})
	return out, err
}

func (s *Service) applyProviderResultTx(ctx context.Context, d *db.DB, requestID RequestID, providerName string, result SubmitResult, callErr error) (Request, error) {
	var out Request
	err := d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.recordProviderEvent(ctx, tx, requestID, providerName, "RESPONSE", result, callErr); err != nil {
			return err
		}
		switch {
		case callErr != nil:
			// A transport error says nothing about whether the money moved.
			// PART XXI: do not release the reservation and retry blindly.
			r, err := s.transition(ctx, tx, requestID, StateStatusUnknown,
				"provider call did not return a definite answer: "+callErr.Error(), "")
			out = r
			return err
		case result.Status == ProviderSettled:
			r, err := s.settle(ctx, tx, requestID, result)
			out = r
			return err // handled below when it is a posting failure
		case result.Status == ProviderAccepted:
			r, err := s.transitionWith(ctx, tx, requestID, StateProviderPending, stateChange{
				Reason:            "provider accepted the payout",
				ProviderEvent:     result.RawStatus,
				ProviderReference: result.ProviderReference,
				ProviderStatus:    result.RawStatus,
			})
			out = r
			return err
		case result.Status == ProviderFailed:
			r, err := s.fail(ctx, tx, requestID, result.FailureReason)
			out = r
			return err
		default:
			r, err := s.transition(ctx, tx, requestID, StateStatusUnknown,
				"provider reported an unknown status", result.RawStatus)
			out = r
			return err
		}
	})
	return out, err
}

// settle records that value has irrevocably left.
func (s *Service) settle(ctx context.Context, tx pgx.Tx, requestID RequestID, result SubmitResult) (Request, error) {
	req, err := s.forUpdate(ctx, tx, requestID)
	if err != nil {
		return Request{}, err
	}
	if req.State == StateSettled {
		return req, nil
	}
	conv := valuedomain.ConversionKey{From: valuedomain.PayoutPending, To: valuedomain.ExternalSettled}
	if _, err := s.poster.Post(ctx, tx, ledger.Posting{
		Kind:           ledger.KindPayoutSettled,
		IdempotencyKey: "payout:" + req.ID.String() + ":settle",
		Reference:      ledger.FinancialEventReference{Type: "payout_request", ID: req.ID.String()},
		EffectiveAt:    s.clk.Now(),
		Description:    "payout settled by provider",
		Conversion:     &conv,
		Entries: []ledger.Entry{
			{
				Account: ledger.CustomerAccount(req.AccountID, ledger.CodePayoutReserved, req.CreditAssetID),
				Side:    ledger.Credit, Quantity: req.ReservedQuantity,
			},
			{
				Account: ledger.PlatformAccount(ledger.CodePayoutSettled, req.CreditAssetID),
				Side:    ledger.Debit, Quantity: req.ReservedQuantity,
			},
		},
		Metadata: map[string]any{"provider_reference": result.ProviderReference},
	}); err != nil {
		return Request{}, err
	}
	settledAt := s.clk.Now().UTC()
	settled := req.ReservedQuantity
	return s.transitionWith(ctx, tx, req.ID, StateSettled, stateChange{
		Reason:            "provider settled the payout",
		ProviderEvent:     result.RawStatus,
		SettledQuantity:   &settled,
		SettledAt:         &settledAt,
		ProviderReference: result.ProviderReference,
		ProviderStatus:    result.RawStatus,
	})
}

// fail returns the reserved units to the exact lots they came from.
//
// The return posting is deliberately not capability-gated: if PAYOUT_SETTLE is
// revoked while payouts are in flight, the reserved value must still be able to
// reach the user.
func (s *Service) fail(ctx context.Context, tx pgx.Tx, requestID RequestID, reason string) (Request, error) {
	req, err := s.forUpdate(ctx, tx, requestID)
	if err != nil {
		return Request{}, err
	}
	if req.State.Terminal() {
		return req, nil
	}
	if req.ReservedQuantity.IsPositive() {
		if err := s.returnReservation(ctx, tx, req, reason); err != nil {
			return Request{}, err
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE payout_requests SET failure_reason = $2 WHERE id = $1`, req.ID, reason); err != nil {
		return Request{}, mapError(err)
	}
	return s.transitionWith(ctx, tx, req.ID, StateFailed, releasedChange(reason))
}

// FlagForManualReview sends a payout to a human.
//
// It is the one direction that is always safe to offer: it only ever moves a
// request INTO MANUAL_REVIEW, never out of it, and the transition table still
// decides whether that is legal from where the request is now. A compliance
// operator who sees something wrong, or an automated check that cannot decide,
// uses this; getting back out is ResolveManualReview, which is
// dual-controlled.
func (s *Service) FlagForManualReview(
	ctx context.Context, tx pgx.Tx, requestID RequestID, reason string,
) (Request, error) {
	if strings.TrimSpace(reason) == "" {
		return Request{}, errs.New(errs.CodeValidationFailed,
			"flagging a payout for review requires a reason")
	}
	req, err := s.forUpdate(ctx, tx, requestID)
	if err != nil {
		return Request{}, err
	}
	if req.State == StateManualReview {
		return req, nil
	}
	return s.transition(ctx, tx, req.ID, StateManualReview, reason, "")
}

// ManualResolution is what an operator decided about a payout that landed in
// MANUAL_REVIEW.
//
// Note what is NOT here: there is no "declare it settled". A payout is settled
// when the PROVIDER says it is, and the only ways to learn that are the
// provider's own lookup and reconciliation. An operator who could mark a
// payout SETTLED by hand could close a ticket by asserting money moved, and
// the ledger would then carry a settlement nobody can point at.
type ManualResolution string

// Manual resolutions.
const (
	// ResolveFail concludes the payout did not happen and returns the exact
	// reserved units to the exact lots they came from.
	ResolveFail ManualResolution = "FAIL"
	// ResolveReject concludes the request should not have been accepted and
	// returns the reservation, without asserting anything about a provider.
	ResolveReject ManualResolution = "REJECT"
	// ResolveRetryVerification sends the request back to VERIFIED so it can be
	// submitted again. It is legal ONLY from MANUAL_REVIEW reached before a
	// submission, and Resolve refuses it otherwise: a payout that may already
	// have been sent must never be re-submitted.
	ResolveRetryVerification ManualResolution = "RETRY"
)

// Valid reports whether r is declared.
func (r ManualResolution) Valid() bool {
	switch r {
	case ResolveFail, ResolveReject, ResolveRetryVerification:
		return true
	}
	return false
}

// ResolveManualReview applies an operator's decision to a payout sitting in
// MANUAL_REVIEW. It is the executing half of the dual-controlled
// PAYOUT_MANUAL_REVIEW_RESOLVE administrative action; the approval itself is
// verified by internal/admin before this is reached.
func (s *Service) ResolveManualReview(
	ctx context.Context, tx pgx.Tx, requestID RequestID, resolution ManualResolution, reason string,
) (Request, error) {
	if !resolution.Valid() {
		return Request{}, errs.Newf(errs.CodeValidationFailed,
			"unknown manual payout resolution %q", resolution)
	}
	if strings.TrimSpace(reason) == "" {
		return Request{}, errs.New(errs.CodeValidationFailed,
			"resolving a payout by hand requires a reason; it is the only record of why")
	}
	req, err := s.forUpdate(ctx, tx, requestID)
	if err != nil {
		return Request{}, err
	}
	if req.State != StateManualReview {
		return Request{}, errs.Newf(errs.CodeInvalidStateTransition,
			"this payout is %s, not MANUAL_REVIEW", req.State).
			WithField("payout_id", requestID.String()).
			WithField("state", string(req.State))
	}

	switch resolution {
	case ResolveFail:
		return s.fail(ctx, tx, req.ID, reason)
	case ResolveReject:
		if req.ReservedQuantity.IsPositive() {
			if rerr := s.returnReservation(ctx, tx, req, reason); rerr != nil {
				return Request{}, rerr
			}
		}
		return s.transitionWith(ctx, tx, req.ID, StateRejected, releasedChange(reason))
	default: // ResolveRetryVerification
		// A payout that reached MANUAL_REVIEW from SUBMITTED, PROVIDER_PENDING
		// or PAYOUT_STATUS_UNKNOWN may already exist at the provider. Sending
		// it back to VERIFIED would make a second submission possible, which
		// is precisely how a payout gets paid twice.
		submitted, serr := s.everSubmitted(ctx, tx, req.ID)
		if serr != nil {
			return Request{}, serr
		}
		if submitted {
			return Request{}, errs.New(errs.CodeSubmissionStateUnknown,
				"this payout has already been submitted at least once; it can be failed or reconciled, never retried").
				WithField("payout_id", requestID.String())
		}
		return s.transition(ctx, tx, req.ID, StateVerified, reason, "")
	}
}

// providerMatches refuses a provider that is not the one this payout was
// already claimed for. A request with no provider recorded has not been
// claimed and may go to any registered one.
func (s *Service) providerMatches(ctx context.Context, d *db.DB, id RequestID, providerName string) error {
	var recorded *string
	if err := d.QueryRow(ctx, `SELECT provider FROM payout_requests WHERE id = $1`, id).Scan(&recorded); err != nil {
		return mapError(err)
	}
	if recorded == nil || *recorded == "" || *recorded == providerName {
		return nil
	}
	return errs.Newf(errs.CodeConflict,
		"this payout was claimed for provider %q and cannot be submitted to %q; its idempotency key belongs to the first",
		*recorded, providerName).
		WithField("payout_id", id.String())
}

// everSubmitted reports whether the request has ever been in a state that
// means a provider call may have happened. It reads the transition history
// rather than the current state, because the current state is MANUAL_REVIEW
// either way and the history is what distinguishes the two cases.
func (s *Service) everSubmitted(ctx context.Context, tx pgx.Tx, id RequestID) (bool, error) {
	var n int
	err := tx.QueryRow(ctx,
		`SELECT count(*) FROM payout_request_transitions
		  WHERE request_id = $1
		    AND to_state IN ('SUBMITTED','PROVIDER_PENDING','PAYOUT_STATUS_UNKNOWN','SETTLED')`,
		id).Scan(&n)
	if err != nil {
		return false, mapError(err)
	}
	return n > 0, nil
}

// Cancel returns a payout's value to the user before it has been submitted.
func (s *Service) Cancel(ctx context.Context, tx pgx.Tx, requestID RequestID, reason string) (Request, error) {
	if strings.TrimSpace(reason) == "" {
		return Request{}, errs.New(errs.CodeValidationFailed, "cancelling a payout requires a reason")
	}
	req, err := s.forUpdate(ctx, tx, requestID)
	if err != nil {
		return Request{}, err
	}
	switch req.State {
	case StateSubmitted, StateProviderPending, StateStatusUnknown:
		return Request{}, errs.Newf(errs.CodeConflict,
			"this payout has already been submitted and may have been paid; it can only be resolved by reconciliation, not cancelled").
			WithField("payout_id", requestID.String()).
			WithField("state", string(req.State))
	}
	// The current state is not the whole question, and MANUAL_REVIEW is why.
	//
	// applyProviderResult parks a payout there precisely when the provider was
	// called and the settlement could not be recorded -- a lost response, or a
	// ledger posting that refused. So a payout the provider has PAID can sit in
	// MANUAL_REVIEW, which the switch above does not name, and Cancel would
	// then release the reservation and restore the user's Credits while the
	// money was already gone (F-107).
	//
	// everSubmitted reads the transition history rather than the current state,
	// for exactly this reason. ResolveManualReview already consulted it; the
	// single-user endpoint did not, so the dual-controlled path refused to
	// retry an ever-submitted payout while the account owner could unwind one.
	submitted, err := s.everSubmitted(ctx, tx, requestID)
	if err != nil {
		return Request{}, err
	}
	if submitted {
		return Request{}, errs.Newf(errs.CodeConflict,
			"this payout has been submitted before and may have been paid; it can only be resolved by reconciliation, not cancelled").
			WithField("payout_id", requestID.String()).
			WithField("state", string(req.State))
	}
	if req.State.Terminal() {
		return req, nil
	}
	if req.ReservedQuantity.IsPositive() {
		if err := s.returnReservation(ctx, tx, req, reason); err != nil {
			return Request{}, err
		}
	}
	return s.transitionWith(ctx, tx, req.ID, StateRejected, releasedChange(reason))
}

// returnReservation posts the unwind and restores the exact lot slices.
func (s *Service) returnReservation(ctx context.Context, tx pgx.Tx, req Request, reason string) error {
	allocs, err := s.allocations(ctx, tx, req.ID, false)
	if err != nil {
		return err
	}
	if len(allocs) == 0 {
		return nil
	}
	conv := valuedomain.ConversionKey{From: valuedomain.PayoutPending, To: valuedomain.InternalCredit}
	post, err := s.poster.Post(ctx, tx, ledger.Posting{
		Kind:           ledger.KindPayoutReturned,
		IdempotencyKey: "payout:" + req.ID.String() + ":return",
		Reference:      ledger.FinancialEventReference{Type: "payout_request", ID: req.ID.String()},
		EffectiveAt:    s.clk.Now(),
		Description:    "reserved Credits returned to the customer",
		Conversion:     &conv,
		Entries: []ledger.Entry{
			{
				Account: ledger.CustomerAccount(req.AccountID, ledger.CodePayoutReserved, req.CreditAssetID),
				Side:    ledger.Credit, Quantity: req.ReservedQuantity,
			},
			{
				Account: ledger.CustomerAccount(req.AccountID, ledger.CodeCreditBalance, req.CreditAssetID),
				Side:    ledger.Debit, Quantity: req.ReservedQuantity,
			},
		},
		Metadata: map[string]any{"reason": reason},
	})
	if err != nil {
		return err
	}
	restore := make([]credit.Allocation, 0, len(allocs))
	for _, a := range allocs {
		restore = append(restore, credit.Allocation{LotID: a.LotID, Quantity: a.Quantity, Origin: a.Origin})
	}
	if err := s.credits.Restore(ctx, tx, credit.RestoreRequest{
		Allocations: restore,
		JournalTxID: post.TransactionID,
		Reference:   credit.Reference{Type: "payout_request", ID: req.ID.String()},
		Reason:      reason,
	}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE payout_allocations SET returned = true WHERE request_id = $1 AND NOT returned`, req.ID); err != nil {
		return mapError(err)
	}
	return nil
}

// Reconcile resolves a payout whose outcome is unknown by asking the provider
// what happened to the key Nodal chose.
//
// This is the answer to PART XXXVIII for this provider category: external
// success followed by a local crash is discovered rather than duplicated,
// because the key was persisted before the call and the provider is the
// authority on what it did with it.
func (s *Service) Reconcile(ctx context.Context, d *db.DB, requestID RequestID) (Request, error) {
	var req Request
	if err := d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		r, err := s.forUpdate(ctx, tx, requestID)
		req = r
		return err
	}); err != nil {
		return Request{}, err
	}
	if req.State.Terminal() {
		return s.checkTerminalAgainstProvider(ctx, d, req)
	}
	if req.ProviderIdempotencyKey == "" {
		return Request{}, errs.New(errs.CodeReconciliationRequired,
			"this payout has no provider key, so nothing external can be asked about it").
			WithField("payout_id", requestID.String())
	}
	provider, err := s.providers.Get(req.Provider)
	if err != nil {
		return Request{}, err
	}
	result, lookupErr := provider.Lookup(ctx, req.ProviderIdempotencyKey)
	if lookupErr != nil {
		// Still unknown. The reservation stays; that is the point.
		return req, errs.Wrap(lookupErr, errs.CodeReconciliationRequired,
			"the provider could not say what happened to this payout; the reservation is retained")
	}
	return s.applyProviderResult(ctx, d, requestID, req.Provider, result, nil)
}

// checkTerminalAgainstProvider asks the provider what it now says about a
// payout that is already finished, and refuses to act on a different answer.
//
// PART LXXII item 23 is a compromised provider sending a contradictory status.
// The danger is not the first lie, it is a later one rewriting a settled fact,
// and the state machine already makes that impossible: FAILED and REJECTED
// have no outgoing transitions at all, and SETTLED leads only to REVERSED,
// which is a clawback with its own compensating postings rather than a change
// of story. So nothing here moves a state.
//
// What was wrong before is the opposite failure. Reconcile returned early on a
// terminal request without asking anything, so a provider that changed its
// answer was never heard and left no trace. An operator asking "what does the
// provider say about this settled payout" got silence that looked like
// agreement. The answer is now fetched, recorded as a provider event, and a
// contradiction is returned as an error somebody has to look at.
func (s *Service) checkTerminalAgainstProvider(ctx context.Context, d *db.DB, req Request) (Request, error) {
	if req.ProviderIdempotencyKey == "" || req.State == StateReversed {
		// Nothing external was ever keyed under this request, or the reversal
		// is already the record of a settlement being undone.
		return req, nil
	}
	provider, err := s.providers.Get(req.Provider)
	if err != nil {
		return req, err
	}
	result, lookupErr := provider.Lookup(ctx, req.ProviderIdempotencyKey)
	if lookupErr != nil {
		// A finished payout is not made unsafe by a provider that will not
		// answer; there is no reservation left to hold or release. Say so
		// rather than reporting agreement that was never obtained.
		return req, errs.Wrap(lookupErr, errs.CodeReconciliationRequired,
			"the provider could not say what happened to this finished payout; its recorded outcome is unchanged").
			WithField("payout_id", req.ID.String())
	}
	if err := d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		return s.recordProviderEvent(ctx, tx, req.ID, req.Provider, "RESPONSE", result, nil)
	}); err != nil {
		return req, err
	}
	contradiction := terminalContradiction(req, result)
	if contradiction == "" {
		return req, nil
	}
	observability.LoggerFrom(ctx).ErrorContext(ctx, "payout: provider contradicts a finished payout",
		"payout_id", req.ID.String(), "recorded_state", string(req.State),
		"provider_status", string(result.Status), "provider_raw_status", result.RawStatus)
	return req, errs.New(errs.CodeReconciliationRequired, contradiction).
		WithField("payout_id", req.ID.String()).
		WithField("recorded_state", string(req.State)).
		WithField("provider_status", string(result.Status))
}

// terminalContradiction names the way a provider's current answer disagrees
// with a finished payout, or "" when the two can be reconciled.
//
// Only definite disagreements count. A provider that has gone back to
// ACCEPTED after settling is not treated as contradicting itself, because
// read-after-write lag on the provider's side produces exactly that, and an
// alarm a coincidence can trigger is one operators learn to ignore.
func terminalContradiction(req Request, result SubmitResult) string {
	switch req.State {
	case StateSettled:
		if result.Status == ProviderFailed {
			return "the provider now reports this payout failed, and it is recorded as settled with the postings to match"
		}
		if result.Status == ProviderSettled && req.ProviderReference != "" &&
			result.ProviderReference != "" && req.ProviderReference != result.ProviderReference {
			return "the provider now reports a different reference for this settled payout"
		}
	case StateFailed, StateRejected:
		if result.Status == ProviderSettled {
			return "the provider now reports this payout settled, and it is recorded as " +
				strings.ToLower(string(req.State)) + " with the reservation returned"
		}
	}
	return ""
}

// OpenRequests returns payouts that hold value and are not finished, which is
// what a reconciliation sweep works through.
func (s *Service) OpenRequests(ctx context.Context, q db.Querier, olderThan time.Time, limit int) ([]Request, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.Query(ctx,
		`SELECT `+requestColumns+`
		   FROM payout_requests
		  WHERE state IN ('SUBMITTED','PROVIDER_PENDING','PAYOUT_STATUS_UNKNOWN')
		    AND (submitted_at IS NULL OR submitted_at < $1)
		  ORDER BY submitted_at NULLS FIRST
		  LIMIT $2`, olderThan, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, mapError(err)
		}
		out = append(out, r)
	}
	return out, mapError(rows.Err())
}

// stateChange is everything one move of a conversion request carries.
//
// The money is here rather than in an UPDATE beside the transition row because
// 00807 made the transition row the only thing that can write it. Every place
// this package changed a quantity it was also moving the state -- reserving is
// the VERIFIED step, settling is the SETTLED step, returning a reservation is
// the FAILED or REJECTED step -- so the two were always one event, and writing
// them as one is what stops a quantity being rewritten beside a lawful move
// (F-229).
//
// A nil pointer means "this change says nothing about that number", which is
// what an ordinary state move says; the trigger leaves the column alone.
type stateChange struct {
	Reason        string
	ProviderEvent string

	ReservedQuantity *money.Quantity
	SettledQuantity  *money.Quantity
	ReservedAt       *time.Time
	SettledAt        *time.Time

	ProviderReference string
	ProviderStatus    string
}

// transition moves a request's state and writes the transition row the 00603
// binding requires and the 00807 trigger reads.
func (s *Service) transition(ctx context.Context, tx pgx.Tx, id RequestID, to State, reason, providerEvent string) (Request, error) {
	return s.transitionWith(ctx, tx, id, to, stateChange{Reason: reason, ProviderEvent: providerEvent})
}

func (s *Service) transitionWith(ctx context.Context, tx pgx.Tx, id RequestID, to State, ch stateChange) (Request, error) {
	if !to.Valid() {
		return Request{}, errs.Newf(errs.CodeValidationFailed, "unknown payout state %q", to)
	}
	if strings.TrimSpace(ch.Reason) == "" {
		return Request{}, errs.New(errs.CodeValidationFailed, "a payout state change requires a reason")
	}
	req, err := s.forUpdate(ctx, tx, id)
	if err != nil {
		return Request{}, err
	}
	if req.State == to {
		return req, nil
	}
	if !CanTransition(req.State, to) {
		return Request{}, errs.Newf(errs.CodeInvalidStateTransition,
			"a payout cannot go %s -> %s", req.State, to).
			WithField("payout_id", id.String()).
			WithField("from", string(req.State)).
			WithField("to", string(to))
	}
	actorType, actorID := actorFrom(ctx)
	optional := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	quantity := func(q *money.Quantity) any {
		if q == nil {
			return nil
		}
		return q.String()
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO payout_request_transitions
		   (id, request_id, from_state, to_state, actor_type, actor_id, reason, provider_event,
		    reserved_quantity, settled_quantity, reserved_at, settled_at,
		    provider_reference, provider_status)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::numeric,$10::numeric,$11,$12,$13,$14)`,
		NewTransitionID(), id, string(req.State), string(to), actorType, actorID, ch.Reason,
		optional(ch.ProviderEvent),
		quantity(ch.ReservedQuantity), quantity(ch.SettledQuantity), ch.ReservedAt, ch.SettledAt,
		optional(ch.ProviderReference), optional(ch.ProviderStatus)); err != nil {
		return Request{}, mapError(err)
	}
	// The trigger wrote the row; nothing here does, and cp_app holds no UPDATE
	// on any of the columns it writes.
	updated, err := scanRequest(tx.QueryRow(ctx,
		`SELECT `+requestColumns+` FROM payout_requests WHERE id = $1`, id))
	if err != nil {
		return Request{}, mapError(err)
	}
	return updated, nil
}

// recordProviderEvent stores what a provider said, raw, before it is
// interpreted. A provider that later contradicts itself has to be arguable
// against something.
func (s *Service) recordProviderEvent(ctx context.Context, tx pgx.Tx, id RequestID, provider, direction string, result SubmitResult, callErr error) error {
	payload := map[string]any{
		"status":             string(result.Status),
		"raw_status":         result.RawStatus,
		"provider_reference": result.ProviderReference,
		"failure_reason":     result.FailureReason,
	}
	if callErr != nil {
		payload["transport_error"] = callErr.Error()
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "payout: encode provider event")
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO payout_provider_events (id, request_id, provider, direction, provider_status, payload)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		NewProviderEventID(), id, provider, direction, result.RawStatus, body)
	return mapError(err)
}

func actorFrom(ctx context.Context) (string, string) {
	if p, ok := security.PrincipalFrom(ctx); ok && p.SubjectID != "" {
		return string(p.ActorType), p.SubjectID
	}
	return "SYSTEM", "payout-service"
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	switch db.SQLState(err) {
	case "PO001":
		return errs.Wrap(err, errs.CodeInternal,
			"a payout's reservation does not match the units it allocated")
	case "PO003":
		return errs.Wrap(err, errs.CodeForbidden, "payout allocations are immutable")
	case "AU001":
		return errs.Wrap(err, errs.CodeInternal, "a payout changed state without its transition row")
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return errs.New(errs.CodeNotFound, "payout request not found")
	}
	if mapped := ledger.MapError(err); mapped != nil {
		return mapped
	}
	return errs.Wrap(err, errs.CodeInternal, "payout: database error")
}

// AwaitingSubmission returns reserved payouts that have not been handed to a
// provider yet, oldest first.
//
// VERIFIED is the state Create leaves a fully-reserved request in and the only
// state Submit accepts. A request sitting here holds the customer's Credits in
// PAYOUT_RESERVED: the money has left their spendable balance and has not gone
// anywhere, which is the worst place for it to stop.
//
// `olderThan` bounds it to requests created before that instant, so a sweep
// never races the transaction that is still creating one.
func (s *Service) AwaitingSubmission(ctx context.Context, q db.Querier, olderThan time.Time, limit int) ([]Request, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.Query(ctx,
		`SELECT `+requestColumns+`
		   FROM payout_requests
		  WHERE state = 'VERIFIED' AND reserved_at IS NOT NULL AND created_at < $1
		  ORDER BY created_at
		  LIMIT $2`, olderThan.UTC(), limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, mapError(err)
		}
		out = append(out, r)
	}
	return out, mapError(rows.Err())
}
