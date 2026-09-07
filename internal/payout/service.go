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
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
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

// Service creates, reserves, submits and reconciles payouts.
type Service struct {
	poster    Poster
	credits   Credits
	engine    *Engine
	providers *Registry
	clk       clock.Clock
}

// NewService returns a Service. No argument may be nil.
func NewService(poster Poster, credits Credits, engine *Engine, providers *Registry, clk clock.Clock) *Service {
	if poster == nil || credits == nil || engine == nil || providers == nil || clk == nil {
		panic("payout: NewService requires a poster, credits, an engine, a provider registry and a clock")
	}
	return &Service{poster: poster, credits: credits, engine: engine, providers: providers, clk: clk}
}

// Provider returns a registered payout provider, so callers can ask what it
// actually supports rather than assuming (PART LXXVI).
func (s *Service) Provider(name string) (Provider, error) { return s.providers.Get(name) }

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
	if tx == nil {
		return Request{}, Decision{}, errs.New(errs.CodeInternal, "payout: Create requires a transaction")
	}
	if existing, found, err := s.byIdempotencyKey(ctx, tx, r.IdempotencyKey); err != nil {
		return Request{}, Decision{}, err
	} else if found {
		return existing, Decision{}, nil
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
	var destID any
	if r.DestinationID != nil {
		destID = *r.DestinationID
	}
	err = tx.QueryRow(ctx,
		`INSERT INTO payout_requests
		   (id, account_id, destination_id, credit_asset_id, state, requested_quantity,
		    policy_version, policy_hash, eligibility_reasons, verification_level, idempotency_key)
		 VALUES ($1,$2,$3,$4,'ELIGIBILITY_CHECK',$5::numeric,$6,$7,$8,$9,$10)
		 RETURNING created_at, updated_at`,
		req.ID, req.AccountID, destID, req.CreditAssetID, req.RequestedQuantity.String(),
		req.PolicyVersion, req.PolicyHash, reasons, string(req.VerificationLevel),
		req.IdempotencyKey).Scan(&req.CreatedAt, &req.UpdatedAt)
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
	final, err := s.transition(ctx, tx, reserved.ID, StateVerified, "eligible and reserved", "")
	if err != nil {
		return Request{}, Decision{}, err
	}
	final.ReservedQuantity = reserved.ReservedQuantity
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

	in.AccountID = req.AccountID
	in.Requested = req.RequestedQuantity
	decision, err := s.engine.Evaluate(ctx, tx, in)
	if err != nil {
		return Request{}, Decision{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE payout_requests SET verification_level = $2, policy_version = $3, policy_hash = $4 WHERE id = $1`,
		req.ID, string(in.Verified), decision.PolicyVersion, decision.PolicyHash); err != nil {
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
	final, err := s.transition(ctx, tx, reserved.ID, StateVerified, "verification complete; value reserved", "")
	if err != nil {
		return Request{}, Decision{}, err
	}
	final.ReservedQuantity = reserved.ReservedQuantity
	return final, decision, nil
}

// reserve moves the approved units out of the spendable domain.
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
			{Account: ledger.CustomerAccount(req.AccountID, ledger.CodeCreditBalance, req.CreditAssetID),
				Side: ledger.Credit, Quantity: d.Requested},
			{Account: ledger.CustomerAccount(req.AccountID, ledger.CodePayoutReserved, req.CreditAssetID),
				Side: ledger.Debit, Quantity: d.Requested},
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

	if _, err := tx.Exec(ctx,
		`UPDATE payout_requests SET reserved_quantity = $2::numeric, reserved_at = $3 WHERE id = $1`,
		req.ID, d.Requested.String(), s.clk.Now()); err != nil {
		return Request{}, mapError(err)
	}
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

	// Phase one: claim the request and persist the key, in its own committed
	// transaction.
	var req Request
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
		key := "nodal-payout-" + r.ID.String()
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
		req = moved
		return nil
	})
	if err != nil {
		return Request{}, err
	}
	if req.State != StateSubmitted {
		return req, nil
	}

	// Phase two: the external call. Everything from here is recoverable from
	// the key committed above.
	result, callErr := provider.Submit(ctx, SubmitRequest{
		IdempotencyKey: req.ProviderIdempotencyKey,
		Reference:      req.ID.String(),
		Amount:         money.USD{}, // conversion to external value happens above this layer
	})
	return s.applyProviderResult(ctx, d, req.ID, providerName, result, callErr)
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
			if _, err := tx.Exec(ctx,
				`UPDATE payout_requests SET provider_reference = $2, provider_status = $3 WHERE id = $1`,
				requestID, result.ProviderReference, result.RawStatus); err != nil {
				return mapError(err)
			}
			r, err := s.transition(ctx, tx, requestID, StateProviderPending, "provider accepted the payout", result.RawStatus)
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
			{Account: ledger.CustomerAccount(req.AccountID, ledger.CodePayoutReserved, req.CreditAssetID),
				Side: ledger.Credit, Quantity: req.ReservedQuantity},
			{Account: ledger.PlatformAccount(ledger.CodePayoutSettled, req.CreditAssetID),
				Side: ledger.Debit, Quantity: req.ReservedQuantity},
		},
		Metadata: map[string]any{"provider_reference": result.ProviderReference},
	}); err != nil {
		return Request{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE payout_requests
		    SET settled_quantity = reserved_quantity, settled_at = $2,
		        provider_reference = $3, provider_status = $4
		  WHERE id = $1`,
		req.ID, s.clk.Now(), result.ProviderReference, result.RawStatus); err != nil {
		return Request{}, mapError(err)
	}
	return s.transition(ctx, tx, req.ID, StateSettled, "provider settled the payout", result.RawStatus)
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
		`UPDATE payout_requests SET failure_reason = $2, reserved_quantity = 0 WHERE id = $1`,
		req.ID, reason); err != nil {
		return Request{}, mapError(err)
	}
	return s.transition(ctx, tx, req.ID, StateFailed, reason, "")
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
	if req.State.Terminal() {
		return req, nil
	}
	if req.ReservedQuantity.IsPositive() {
		if err := s.returnReservation(ctx, tx, req, reason); err != nil {
			return Request{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE payout_requests SET reserved_quantity = 0 WHERE id = $1`, req.ID); err != nil {
			return Request{}, mapError(err)
		}
	}
	return s.transition(ctx, tx, req.ID, StateRejected, reason, "")
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
			{Account: ledger.CustomerAccount(req.AccountID, ledger.CodePayoutReserved, req.CreditAssetID),
				Side: ledger.Credit, Quantity: req.ReservedQuantity},
			{Account: ledger.CustomerAccount(req.AccountID, ledger.CodeCreditBalance, req.CreditAssetID),
				Side: ledger.Debit, Quantity: req.ReservedQuantity},
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
		return req, nil
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

// transition moves a request's state and writes the transition row the 00603
// binding requires.
func (s *Service) transition(ctx context.Context, tx pgx.Tx, id RequestID, to State, reason, providerEvent string) (Request, error) {
	if !to.Valid() {
		return Request{}, errs.Newf(errs.CodeValidationFailed, "unknown payout state %q", to)
	}
	if strings.TrimSpace(reason) == "" {
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
	var event any
	if providerEvent != "" {
		event = providerEvent
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO payout_request_transitions
		   (id, request_id, from_state, to_state, actor_type, actor_id, reason, provider_event)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		NewTransitionID(), id, string(req.State), string(to), actorType, actorID, reason, event); err != nil {
		return Request{}, mapError(err)
	}
	updated, err := scanRequest(tx.QueryRow(ctx,
		`UPDATE payout_requests SET state = $2 WHERE id = $1 RETURNING `+requestColumns, id, string(to)))
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
