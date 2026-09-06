package reconciliation

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/positions"
)

// Disposition is how a recovery pass decided the fate of one attempt.
type Disposition string

// Dispositions.
const (
	// DispositionAdopted: an external execution was found and adopted. Its
	// fill, ledger entries and position change are now persisted exactly once.
	DispositionAdopted Disposition = "ADOPTED"
	// DispositionFailedOnChain: the transaction landed and failed. Nothing
	// filled; the attempt is FAILED.
	DispositionFailedOnChain Disposition = "FAILED_ON_CHAIN"
	// DispositionProvenAbsent: both observers agree the signature is unknown
	// and the chain has moved past the last valid block height by the policy
	// margin. The attempt EXPIRED and the same semantic context may be
	// retried by the executor.
	DispositionProvenAbsent Disposition = "PROVEN_ABSENT"
	// DispositionUncertain: the observers disagree, only one answered, or the
	// transaction may still land. The reservation is kept, no duplicate is
	// sent, and the record blocks new risk until it is resolved.
	DispositionUncertain Disposition = "UNCERTAIN"
	// DispositionAlreadySettled: the attempt's fate was already known when
	// the pass started; nothing to recover.
	DispositionAlreadySettled Disposition = "ALREADY_SETTLED"
)

// RecoveryOutcome is the result of one PART 48 recovery pass.
type RecoveryOutcome struct {
	Record      Record
	AttemptID   execution.AttemptID
	OrderID     execution.OrderID
	Disposition Disposition
	Signature   string
	// Resolution is the two-observer verdict when a signature was queried.
	Resolution chain.Resolution
	// ProviderState is what the venue said, when an adapter was available.
	ProviderState execution.ExternalState
	// Fill is the adopted fill; nil unless Disposition is ADOPTED.
	Fill *execution.Fill
	// RetryAllowed reports whether the executor may build a new attempt under
	// the same semantic context (PART 48 "if proven absent: retry").
	RetryAllowed bool
	// ReservationKept reports whether the capital reservation is still held.
	ReservationKept bool
}

// RecoverAttempt runs the PART 48 recovery for one attempt.
//
// The order of operations is the goal document's, and none of it may be
// reordered or skipped:
//
//	keep the reservation → do not submit a duplicate → query the provider →
//	query the chain → search by transaction signature → inspect wallet
//	activity → inspect fills → only then resolve.
//
// There is no submit path in this file. The reservation is released only when
// the order's fate is decided; while uncertainty remains it is kept, exactly
// as PART 48 requires.
func (e *Engine) RecoverAttempt(ctx context.Context, attemptID execution.AttemptID) (RecoveryOutcome, error) {
	if e.attempts == nil || e.orders == nil {
		return RecoveryOutcome{}, errs.New(errs.CodeInternal, "reconciliation: execution repositories are not configured")
	}
	att, err := e.attempts.Get(ctx, e.db, attemptID)
	if err != nil {
		return RecoveryOutcome{}, err
	}
	ord, err := e.orders.Get(ctx, e.db, att.OrderID)
	if err != nil {
		return RecoveryOutcome{}, err
	}
	out := RecoveryOutcome{AttemptID: att.ID, OrderID: ord.ID, Signature: att.TxSignature, ReservationKept: true}

	// Step 0. A submission that was interrupted mid-flight is uncertainty,
	// not failure: move it to SUBMISSION_UNKNOWN before anything else so a
	// crashed process leaves a state a restart can reason about (PART 48,
	// PART 49 step 6).
	att, ord, err = e.claimUnknown(ctx, att, ord)
	if err != nil {
		return out, err
	}
	if !att.Status.Recoverable() {
		// The attempt's fate is already known. Report what was adopted so a
		// replay is not just harmless but informative: the caller sees the
		// same fill, the same record and the same reservation state as the
		// pass that decided it.
		out.Disposition = DispositionAlreadySettled
		ev := evidence{Signature: att.TxSignature, Detail: "attempt is " + string(att.Status)}
		if att.TxSignature != "" {
			if fills, ferr := e.orders.ListFills(ctx, e.db, ord.ID); ferr == nil {
				for i := range fills {
					if fills[i].TxSignature == att.TxSignature {
						ev.Existing = &fills[i]
						fill := fills[i]
						out.Fill = &fill
					}
				}
			}
		}
		out.ReservationKept = e.reservationHeld(ctx, ord)
		rec, err := e.recordOutcome(ctx, att, ord, out, ev)
		out.Record = rec
		return out, err
	}
	if att.Status == execution.AttemptSubmissionUnknown {
		e.metrics.unknownSubmission(ctx, att.ID.String())
	}

	// Steps 1-6. Gather external truth. All network I/O happens here, outside
	// any database transaction.
	ev := e.gather(ctx, att, ord)
	out.ProviderState = ev.ProviderState
	out.Resolution = ev.Resolution
	if ev.Signature != "" {
		out.Signature = ev.Signature
	}

	// Step 7. Only now decide.
	switch {
	case ev.Adopted != nil:
		out.Disposition = DispositionAdopted
	case ev.LandedFailed:
		out.Disposition = DispositionFailedOnChain
	case ev.ProvenAbsent:
		out.Disposition = DispositionProvenAbsent
		out.RetryAllowed = true
	default:
		out.Disposition = DispositionUncertain
	}

	err = e.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var applyErr error
		out, applyErr = e.apply(ctx, tx, att, ord, out, ev)
		return applyErr
	})
	return out, err
}

// claimUnknown moves an attempt (and its order) that was interrupted while
// submitting into SUBMISSION_UNKNOWN. It is idempotent: an attempt already in
// SUBMISSION_UNKNOWN is returned untouched.
func (e *Engine) claimUnknown(ctx context.Context, att execution.Attempt, ord execution.Order) (execution.Attempt, execution.Order, error) {
	if att.Status != execution.AttemptSubmitting {
		return att, ord, nil
	}
	err := e.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		status := execution.AttemptSubmissionUnknown
		updated, err := e.attempts.Update(ctx, tx, att.ID, execution.AttemptPatch{
			Status: &status,
			Reason: "PART 48: transport outcome unknown after interruption",
		})
		if err != nil {
			return err
		}
		att = updated
		if ord.Status == execution.OrderSubmitting {
			o, err := e.orders.Transition(ctx, tx, ord.ID, execution.OrderSubmissionUnknown, execution.TransitionEvidence{
				ActorType: string(SystemActor().Type), ActorID: ActorName,
				Reason: "PART 48: transport outcome unknown after interruption", CausationID: att.ID.String(),
			})
			if err != nil {
				return err
			}
			ord = o
		}
		return nil
	})
	return att, ord, err
}

// evidence is everything the gather phase learned.
type evidence struct {
	Signature     string
	ProviderState execution.ExternalState
	ProviderRaw   string
	ProviderErr   string
	Resolution    chain.Resolution
	Observation   *chain.TxObservation
	// Adopted is the external execution event to persist, when one was found
	// and it belongs to this order.
	Adopted      *execution.ExternalExecutionEvent
	LandedFailed bool
	ProvenAbsent bool
	BlockHeight  uint64
	WalletAddr   string
	// Candidates are wallet transactions inspected while searching for an
	// execution without a known signature.
	Candidates []string
	// UnknownActivity lists signatures touching the wallet that match no
	// attempt: PART 48 "inspect wallet activity" turning up something the
	// platform did not initiate.
	UnknownActivity []string
	Detail          string
	// Existing is a fill already persisted for the signature.
	Existing *execution.Fill
	// OutsideExpectations is set when the observed economics fall outside the
	// order's signed bounds.
	OutsideExpectations string
}

// gather performs PART 48 steps 3-6: query the provider, query the chain,
// search by signature, inspect wallet activity, inspect fills.
func (e *Engine) gather(ctx context.Context, att execution.Attempt, ord execution.Order) evidence {
	ev := evidence{Signature: att.TxSignature}

	wallet, err := e.wallets.Get(ctx, e.db, att.WalletID)
	if err == nil {
		ev.WalletAddr = wallet.Address
	} else {
		ev.Detail = "wallet lookup failed: " + err.Error()
	}

	// Step: inspect fills first. A fill already recorded for this signature
	// means a previous pass adopted the execution; nothing external needs to
	// change and no duplicate work happens.
	if fills, err := e.orders.ListFills(ctx, e.db, ord.ID); err == nil {
		for i := range fills {
			if att.TxSignature != "" && fills[i].TxSignature == att.TxSignature {
				ev.Existing = &fills[i]
			}
		}
	}

	// Step: query the provider.
	if ad, ok := e.adapters[att.Provider]; ok {
		st, err := ad.Status(ctx, execution.ExternalReference{
			Venue: att.Provider, TxSignature: att.TxSignature,
			ProviderRequestID: att.ProviderRequestID, WalletAddress: ev.WalletAddr,
		})
		if err != nil {
			ev.ProviderErr = err.Error()
			ev.ProviderState = execution.ExternalNotFound
		} else {
			ev.ProviderState = st.State
			ev.ProviderRaw = st.RawRef
			if ev.Signature == "" && len(st.Fills) > 0 {
				ev.Signature = st.Fills[0].TxSignature
			}
		}
	}

	// Step: query the chain, by signature when one is available.
	if ev.Signature != "" {
		ev.Resolution = e.resolveSignature(ctx, ev.Signature)
		if ev.Resolution.Observation != nil {
			obs := *ev.Resolution.Observation
			ev.Observation = &obs
		}
	}

	// Step: inspect wallet activity when no signature is known, or when the
	// signature is unknown to both observers.
	if ev.Observation == nil && ev.WalletAddr != "" && e.observers.Primary != nil {
		since := ord.CreatedAt.Add(-time.Hour)
		if att.SubmittedAt != nil {
			since = att.SubmittedAt.Add(-5 * time.Minute)
		}
		acts, err := e.observers.Primary.SearchWalletActivity(ctx, ev.WalletAddr, since, 50)
		if err != nil {
			ev.Detail = appendDetail(ev.Detail, "wallet activity search failed: "+err.Error())
		}
		for _, a := range acts {
			ev.Candidates = append(ev.Candidates, a.Signature)
			if !a.Succeeded() {
				continue
			}
			if e.matchesOrder(ctx, a, ord, ev.WalletAddr) {
				res := e.resolveSignature(ctx, a.Signature)
				if res.State == chain.Agreed || res.State == chain.PrimaryOnly || res.State == chain.SecondaryOnly {
					ev.Signature = a.Signature
					ev.Resolution = res
					obs := *res.Observation
					ev.Observation = &obs
					break
				}
			}
			if !e.attributed(ctx, a.Signature) {
				ev.UnknownActivity = append(ev.UnknownActivity, a.Signature)
			}
		}
	}

	// Step: interpret the observation.
	switch {
	case ev.Observation == nil:
		ev.ProvenAbsent = e.provenAbsent(ctx, att, ev.Resolution)
	case !ev.Observation.Succeeded():
		ev.LandedFailed = true
		ev.Detail = appendDetail(ev.Detail, "transaction landed with error: "+ev.Observation.Err)
	default:
		event, detail, err := e.deriveFill(ctx, *ev.Observation, ord, ev.WalletAddr, att.Provider)
		if err != nil {
			ev.Detail = appendDetail(ev.Detail, "fill derivation failed: "+err.Error())
			break
		}
		ev.Adopted = &event
		ev.OutsideExpectations = detail
	}
	return ev
}

func appendDetail(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}

// resolveSignature asks both observers about a signature and applies the
// agreement policy. It never picks the optimistic answer (PART 196).
func (e *Engine) resolveSignature(ctx context.Context, sig string) chain.Resolution {
	if e.observers.Primary == nil {
		return chain.Resolution{State: chain.NotFound, Finality: chain.FinalitySubmitted, BlockDependent: true, Detail: "no chain observer configured"}
	}
	required := chain.FinalityConfirmed
	primary, perr := e.observers.Primary.GetTransaction(ctx, sig)
	if e.observers.Secondary == nil {
		if perr != nil {
			return chain.Resolution{State: chain.NotFound, Finality: chain.FinalitySubmitted, BlockDependent: true, Detail: "primary observer failed: " + perr.Error()}
		}
		return e.observers.Policy.ResolveSingle(primary, chain.SidePrimary, required, "secondary observer not configured")
	}
	secondary, serr := e.observers.Secondary.GetTransaction(ctx, sig)
	switch {
	case perr != nil && serr != nil:
		return chain.Resolution{State: chain.NotFound, Finality: chain.FinalitySubmitted, BlockDependent: true, Detail: "no observer answered"}
	case perr != nil:
		return e.observers.Policy.ResolveSingle(secondary, chain.SideSecondary, required, "primary unavailable: "+perr.Error())
	case serr != nil:
		return e.observers.Policy.ResolveSingle(primary, chain.SidePrimary, required, "secondary unavailable: "+serr.Error())
	}
	return e.observers.Policy.Resolve(primary, secondary, required)
}

// provenAbsent reports whether absence may be treated as proven: both
// observers had to answer NOT_FOUND, and the chain must have advanced past the
// attempt's last valid block height by the policy margin. A single observer
// can never prove absence (chain.AgreementPolicy.ResolveSingle says so, and
// this repeats the rule at the decision point).
func (e *Engine) provenAbsent(ctx context.Context, att execution.Attempt, res chain.Resolution) bool {
	if res.State != chain.NotFound || res.Degraded {
		return false
	}
	if att.LastValidBlockHeight == nil || e.observers.Primary == nil {
		return false
	}
	height, err := e.observers.Primary.GetBlockHeight(ctx)
	if err != nil {
		return false
	}
	if e.observers.Secondary != nil {
		if h2, err := e.observers.Secondary.GetBlockHeight(ctx); err == nil && h2 < height {
			height = h2 // never the optimistic answer
		}
	}
	if *att.LastValidBlockHeight < 0 {
		return false
	}
	lastValid := uint64(*att.LastValidBlockHeight)
	return height > lastValid+e.policy.ProvenAbsentMargin
}

// attributed reports whether a signature is already attributed to one of the
// platform's attempts.
func (e *Engine) attributed(ctx context.Context, sig string) bool {
	if _, err := e.attempts.FindBySignature(ctx, e.db, sig); err == nil {
		return true
	}
	return false
}

// matchesOrder reports whether a wallet transaction moves the order's assets
// in the order's direction. It is a candidate filter, not a decision: the
// economics are compared against the order's bounds by deriveFill.
func (e *Engine) matchesOrder(ctx context.Context, obs chain.TxObservation, ord execution.Order, wallet string) bool {
	in, err := e.asset(ctx, ord.InputAssetID)
	if err != nil {
		return false
	}
	out, err := e.asset(ctx, ord.OutputAssetID)
	if err != nil {
		return false
	}
	var sawIn, sawOut bool
	for _, d := range obs.DeltasFor(wallet) {
		switch d.Mint {
		case in.MintAddress:
			sawIn = d.Delta().IsNegative()
		case out.MintAddress:
			sawOut = d.Delta().IsPositive()
		}
	}
	return sawIn && sawOut
}

// deriveFill turns an agreed chain observation into the external execution
// event to persist. It uses the wallet's exact token deltas — never a
// provider's summary — so the fill records what actually moved.
//
// The returned string is non-empty when the economics fall outside the
// order's signed bounds; the caller turns that into a MISMATCH and a SEV1
// unauthorized-signing candidate.
func (e *Engine) deriveFill(ctx context.Context, obs chain.TxObservation, ord execution.Order, wallet, venue string) (execution.ExternalExecutionEvent, string, error) {
	if wallet == "" {
		return execution.ExternalExecutionEvent{}, "", errs.New(errs.CodeInternal, "reconciliation: wallet address unknown")
	}
	in, err := e.asset(ctx, ord.InputAssetID)
	if err != nil {
		return execution.ExternalExecutionEvent{}, "", err
	}
	out, err := e.asset(ctx, ord.OutputAssetID)
	if err != nil {
		return execution.ExternalExecutionEvent{}, "", err
	}
	inDelta := money.QuantityFromInt64(0)
	outDelta := money.QuantityFromInt64(0)
	for _, d := range obs.DeltasFor(wallet) {
		switch d.Mint {
		case in.MintAddress:
			inDelta = inDelta.Add(d.Delta())
		case out.MintAddress:
			outDelta = outDelta.Add(d.Delta())
		}
	}
	spent := inDelta.Neg()
	if !spent.IsPositive() {
		return execution.ExternalExecutionEvent{}, "", errs.New(errs.CodeReconciliationRequired,
			"reconciliation: transaction does not spend the order's input asset").
			WithField("input_delta", inDelta.String())
	}
	if outDelta.IsNegative() {
		return execution.ExternalExecutionEvent{}, "", errs.New(errs.CodeReconciliationRequired,
			"reconciliation: transaction reduces the order's output asset").
			WithField("output_delta", outDelta.String())
	}
	var problems []string
	remaining := ord.Remaining()
	if spent.Cmp(remaining) > 0 {
		problems = append(problems, fmt.Sprintf("input %s exceeds the order's remaining %s", spent, remaining))
	}
	if outDelta.Cmp(ord.MinOutputQuantity) < 0 && ord.MinOutputQuantity.IsPositive() {
		problems = append(problems, fmt.Sprintf("output %s is below the signed minimum %s", outDelta, ord.MinOutputQuantity))
	}
	ev := execution.ExternalExecutionEvent{
		Venue:          venue,
		ExternalFillID: obs.Signature,
		TxSignature:    obs.Signature,
		Slot:           obs.Slot,
		InputAsset:     ord.InputAssetID,
		OutputAsset:    ord.OutputAssetID,
		InputQty:       spent,
		OutputQty:      outDelta,
		NetworkFee:     obs.Fee,
		ObservedAt:     obs.ObservedAt,
		RawRef:         obs.RawRef,
	}
	if !e.nativeAsset.IsZero() {
		ev.NetworkFeeAsset = e.nativeAsset
	} else {
		ev.NetworkFee = money.QuantityFromInt64(0)
	}
	detail := ""
	if len(problems) > 0 {
		detail = joinProblems(problems)
	}
	return ev, detail, nil
}

func joinProblems(p []string) string {
	s := ""
	for i, x := range p {
		if i > 0 {
			s += "; "
		}
		s += x
	}
	return s
}

// apply persists the outcome of a recovery pass inside one transaction:
// the fill (idempotently), its ledger posting, its position change, the
// attempt and order transitions, the reservation decision and the
// reconciliation record.
func (e *Engine) apply(ctx context.Context, tx pgx.Tx, att execution.Attempt, ord execution.Order, out RecoveryOutcome, ev evidence) (RecoveryOutcome, error) {
	switch out.Disposition {
	case DispositionAdopted:
		fill, err := e.adopt(ctx, tx, att, ord, *ev.Adopted, ev)
		if err != nil {
			return out, err
		}
		out.Fill = &fill
		out.ReservationKept = false
	case DispositionFailedOnChain:
		if err := e.markAttempt(ctx, tx, att, execution.AttemptFailed, "transaction landed with error"); err != nil {
			return out, err
		}
	case DispositionProvenAbsent:
		if err := e.markAttempt(ctx, tx, att, execution.AttemptExpired, "proven absent: both observers report unknown past the last valid block height"); err != nil {
			return out, err
		}
	case DispositionUncertain, DispositionAlreadySettled:
		// PART 48: keep the reservation, change nothing, escalate through
		// the record.
	}
	rec, err := e.recordOutcomeTx(ctx, tx, att, ord, out, ev)
	if err != nil {
		return out, err
	}
	out.Record = rec
	return out, nil
}

// markAttempt transitions an attempt, tolerating a status that already moved.
func (e *Engine) markAttempt(ctx context.Context, tx pgx.Tx, att execution.Attempt, to execution.AttemptStatus, reason string) error {
	if att.Status == to {
		return nil
	}
	if !execution.CanTransitionAttempt(att.Status, to) {
		return nil
	}
	_, err := e.attempts.Update(ctx, tx, att.ID, execution.AttemptPatch{Status: &to, Reason: reason})
	return err
}

// adopt persists an external execution exactly once: one fill, one ledger
// transaction, one position change, and the reservation finalized.
//
// Every step is guarded by a marker the database owns, so a crash between any
// two of them is repaired by simply running adopt again:
//
//	fills UNIQUE (venue, external_fill_id)      one fill
//	journal idempotency key "fill:<fill id>"    one set of ledger entries
//	fills.journal_transaction_id (set-once)     one link
//	fills.position_applied_at (set-once)        one position change
func (e *Engine) adopt(ctx context.Context, tx pgx.Tx, att execution.Attempt, ord execution.Order, xe execution.ExternalExecutionEvent, ev evidence) (execution.Fill, error) {
	inAsset, err := e.asset(ctx, ord.InputAssetID)
	if err != nil {
		return execution.Fill{}, err
	}
	outAsset, err := e.asset(ctx, ord.OutputAssetID)
	if err != nil {
		return execution.Fill{}, err
	}
	mantissa, scale, err := execution.EffectivePrice(xe.OutputQty, outAsset.Decimals, xe.InputQty, inAsset.Decimals)
	if err != nil {
		return execution.Fill{}, err
	}
	finality := chainFinality(ev.Resolution.Finality)
	var slotPtrValue *int64
	if xe.Slot > 0 && xe.Slot <= math.MaxInt64 {
		slot := int64(xe.Slot)
		slotPtrValue = &slot
	}
	fill := execution.Fill{
		ID: execution.NewFillID(), OrderID: ord.ID, AttemptID: att.ID, AccountID: ord.AccountID,
		Venue: xe.Venue, ExternalFillID: xe.ExternalFillID, TxSignature: xe.TxSignature, Slot: slotPtrValue,
		InputAssetID: ord.InputAssetID, InputQuantity: xe.InputQty,
		OutputAssetID: ord.OutputAssetID, OutputQuantity: xe.OutputQty,
		NetworkFeeQuantity: xe.NetworkFee, NetworkFeeAssetID: xe.NetworkFeeAsset,
		VenueFeeQuantity: money.QuantityFromInt64(0), PlatformFeeQuantity: money.QuantityFromInt64(0),
		EffectivePriceMantissa: mantissa, EffectivePriceScale: scale,
		Source: execution.FillFromReconciliation, Finality: finality,
		ObservedAt: xe.ObservedAt, RawRef: xe.RawRef,
	}
	if fill.ObservedAt.IsZero() {
		fill.ObservedAt = e.clk.Now()
	}
	stored, err := e.orders.RecordFill(ctx, tx, fill)
	if err != nil {
		return execution.Fill{}, err
	}

	if !stored.Posted() && e.ledger != nil {
		posting, err := e.fillPosting(ctx, ord, stored, inAsset)
		if err != nil {
			return execution.Fill{}, err
		}
		res, err := e.ledger.Post(ctx, tx, posting)
		if err != nil {
			return execution.Fill{}, err
		}
		if err := e.orders.MarkFillPosted(ctx, tx, stored.ID, res.TransactionID.String()); err != nil {
			return execution.Fill{}, err
		}
		stored.JournalTransactionID = res.TransactionID.String()
	}

	if !stored.PositionApplied() && e.positions != nil {
		if err := e.applyPositions(ctx, tx, ord, stored, inAsset); err != nil {
			return execution.Fill{}, err
		}
		if err := e.orders.MarkPositionApplied(ctx, tx, stored.ID, e.clk.Now()); err != nil {
			return execution.Fill{}, err
		}
	}

	if err := e.markAttempt(ctx, tx, att, execution.AttemptAdopted, "PART 48: external execution adopted"); err != nil {
		return execution.Fill{}, err
	}
	if err := e.settleReservation(ctx, tx, ord); err != nil {
		return execution.Fill{}, err
	}
	return stored, nil
}

// chainFinality maps a chain finality level onto the execution one, never
// upgrading: an unresolved level becomes OBSERVED, the weakest a fill may
// carry.
func chainFinality(f chain.Finality) execution.FinalityLevel {
	switch f {
	case chain.FinalityFinalized:
		return execution.FinalityFinalized
	case chain.FinalityConfirmed:
		return execution.FinalityConfirmed
	default:
		return execution.FinalityObserved
	}
}

// fillPosting builds the canonical TRADE_FILL posting for an adopted fill.
func (e *Engine) fillPosting(ctx context.Context, ord execution.Order, f execution.Fill, inAsset assets.Asset) (ledger.Posting, error) {
	in := ledger.SwapInputs{
		AccountID: ord.AccountID, FillID: f.ID.String(),
		OutAsset: ord.InputAssetID, OutQuantity: f.InputQuantity,
		InAsset: ord.OutputAssetID, InQuantity: f.OutputQuantity,
		NetworkFeeAsset: f.NetworkFeeAssetID, NetworkFeeQuantity: f.NetworkFeeQuantity,
		PlatformFeeQuantity: money.QuantityFromInt64(0),
		EffectiveAt:         f.ObservedAt, CorrelationID: ord.CorrelationID,
		Description: "reconciliation-adopted fill " + f.ExternalFillID,
	}
	if usd := e.valueUSD(ctx, inAsset, f.InputQuantity, f.ObservedAt); usd != nil {
		in.OutUSD = usd
		in.InUSD = usd
	}
	return ledger.SwapPosting(in)
}

// applyPositions records the lot changes of an adopted fill. It works from
// the fill's *net* wallet delta per asset — output minus a fee paid in the
// output asset, input plus a fee paid in the input asset — so Σ open lots
// keeps matching the WALLET ledger balance the same posting produced.
// Positions are only ever acquired or disposed; nothing is overwritten
// (PART 195).
func (e *Engine) applyPositions(ctx context.Context, tx pgx.Tx, ord execution.Order, f execution.Fill, inAsset assets.Asset) error {
	notional := e.valueUSD(ctx, inAsset, f.InputQuantity, f.ObservedAt)
	usd := money.USDFromMinor(0)
	source := "reconciliation:unvalued"
	if notional != nil {
		abs, err := notional.Abs()
		if err != nil {
			return errs.Wrap(err, errs.CodeOverflow, "reconciliation: fill valuation")
		}
		usd = abs
		source = "reconciliation:settlement_asset"
	}
	ref := positions.Ref{Type: "fill", ID: f.ID.String()}
	journal := untypedID(f.JournalTransactionID)
	for _, d := range walletDeltas(f) {
		switch {
		case d.qty.IsPositive():
			if _, err := e.positions.Acquire(ctx, tx, positions.AcquireLot{
				AccountID: ord.AccountID, AssetID: d.asset, Quantity: d.qty, AcquiredAt: f.ObservedAt,
				Cost: usd, BasisSource: "fill", ValuationSource: source, AcquisitionRef: ref, JournalTxID: journal,
			}); err != nil {
				return err
			}
		case d.qty.IsNegative():
			if _, _, err := e.positions.Dispose(ctx, tx, positions.Disposal{
				AccountID: ord.AccountID, AssetID: d.asset, Quantity: d.qty.Neg(), DisposedAt: f.ObservedAt,
				Proceeds: usd, ValuationSource: source, DispositionRef: ref, JournalTxID: journal,
			}); err != nil {
				return err
			}
		}
		// Only the acquisition carries the notional as basis; a second
		// acquisition in the same fill would double-count it.
		usd = money.USDFromMinor(0)
	}
	return nil
}

// assetDelta is one asset's net wallet movement inside a fill.
type assetDelta struct {
	asset assets.AssetID
	qty   money.Quantity
}

// walletDeltas returns the net WALLET movement per asset that a fill causes,
// in a deterministic order: the disposals first (so a lot is freed before the
// acquisition that may reuse the same asset), then the acquisitions.
//
// It mirrors ledger.SwapPosting exactly: the input asset leaves the wallet,
// the output asset enters it, and every fee leaves it in the asset it is
// denominated in.
func walletDeltas(f execution.Fill) []assetDelta {
	sum := map[assets.AssetID]money.Quantity{}
	add := func(a assets.AssetID, q money.Quantity) {
		if a.IsZero() || q.IsZero() {
			return
		}
		cur, ok := sum[a]
		if !ok {
			cur = money.QuantityFromInt64(0)
		}
		sum[a] = cur.Add(q)
	}
	add(f.InputAssetID, f.InputQuantity.Neg())
	add(f.OutputAssetID, f.OutputQuantity)
	add(f.NetworkFeeAssetID, f.NetworkFeeQuantity.Neg())
	add(f.VenueFeeAssetID, f.VenueFeeQuantity.Neg())
	add(f.PlatformFeeAssetID, f.PlatformFeeQuantity.Neg())

	out := make([]assetDelta, 0, len(sum))
	for a, qty := range sum {
		if qty.IsNegative() {
			out = append(out, assetDelta{asset: a, qty: qty})
		}
	}
	sort.Slice(out, func(i, j int) bool { return id.Compare(out[i].asset, out[j].asset) < 0 })
	acquired := make([]assetDelta, 0, len(sum))
	for a, qty := range sum {
		if qty.IsPositive() {
			acquired = append(acquired, assetDelta{asset: a, qty: qty})
		}
	}
	sort.Slice(acquired, func(i, j int) bool { return id.Compare(acquired[i].asset, acquired[j].asset) < 0 })
	return append(out, acquired...)
}

// untypedID parses a uuid text into an untyped id, returning the zero id when
// the text is empty or malformed (the column is nullable by design).
func untypedID(s string) id.ID[id.Any] {
	if s == "" {
		return id.ID[id.Any]{}
	}
	v, err := id.ParseAny(s)
	if err != nil {
		return id.ID[id.Any]{}
	}
	return v
}

// settleReservation finalizes the order's capital reservation once the order
// is fully filled: the consumed part stays deployed and the unused remainder
// returns to the account's available quantity (PART 49 step 12). While the
// order is still open the reservation is kept untouched.
func (e *Engine) settleReservation(ctx context.Context, tx pgx.Tx, ord execution.Order) error {
	if e.reservations == nil || ord.ReservationID == "" {
		return nil
	}
	fresh, err := e.orders.Get(ctx, tx, ord.ID)
	if err != nil {
		return err
	}
	if fresh.Status != execution.OrderFilled && !fresh.Status.Terminal() {
		return nil
	}
	rid, err := capital.ParseReservationID(ord.ReservationID)
	if err != nil {
		return errs.Wrap(err, errs.CodeValidationFailed, "reconciliation: order reservation id")
	}
	res, err := e.reservations.Get(ctx, tx, rid)
	if err != nil {
		if errs.HasCode(err, errs.CodeNotFound) {
			return nil
		}
		return err
	}
	if res.Status != capital.ReservationActive {
		return nil
	}
	consume := fresh.FilledInputQuantity.Sub(res.ConsumedQuantity)
	if consume.IsNegative() {
		consume = money.QuantityFromInt64(0)
	}
	if consume.Cmp(res.Remaining()) > 0 {
		consume = res.Remaining()
	}
	usdMinor := int64(0)
	if inAsset, err := e.asset(ctx, fresh.InputAssetID); err == nil {
		if usd := e.valueUSD(ctx, inAsset, consume, e.clk.Now()); usd != nil {
			usdMinor = usd.Minor()
			if remaining, rerr := res.RemainingUSD(); rerr == nil && usdMinor > remaining.Minor() {
				usdMinor = remaining.Minor()
			}
		}
	}
	if consume.IsZero() && usdMinor == 0 {
		_, err = e.reservations.Release(ctx, tx, rid, "reconciliation: order settled with nothing consumed")
		return err
	}
	_, err = e.reservations.ConsumeFinal(ctx, tx, rid, consume, usdMinor, fresh.ID.String())
	return err
}

// reservationHeld reports whether the order's capital reservation is still
// ACTIVE. It never fails the pass: an unreadable reservation is reported as
// still held, which is the conservative answer.
func (e *Engine) reservationHeld(ctx context.Context, ord execution.Order) bool {
	if e.reservations == nil || ord.ReservationID == "" {
		return false
	}
	rid, err := capital.ParseReservationID(ord.ReservationID)
	if err != nil {
		return true
	}
	res, err := e.reservations.Get(ctx, e.db, rid)
	if err != nil {
		return true
	}
	return res.Status == capital.ReservationActive
}

// ReleaseReservation releases an order's reservation once its fate is decided
// and no execution was adopted. It is separate from the recovery pass because
// PART 48 forbids releasing while uncertainty remains: only the executor or an
// operator, having decided the order is over, may call it.
func (e *Engine) ReleaseReservation(ctx context.Context, tx pgx.Tx, ord execution.Order, reason string) error {
	if e.reservations == nil || ord.ReservationID == "" {
		return nil
	}
	if !ord.Status.Terminal() {
		return errs.Newf(errs.CodeInvalidStateTransition,
			"reconciliation: order is %s; a reservation is kept until the order is terminal", ord.Status).
			WithField("order_id", ord.ID.String())
	}
	rid, err := capital.ParseReservationID(ord.ReservationID)
	if err != nil {
		return errs.Wrap(err, errs.CodeValidationFailed, "reconciliation: order reservation id")
	}
	res, err := e.reservations.Get(ctx, tx, rid)
	if err != nil {
		if errs.HasCode(err, errs.CodeNotFound) {
			return nil
		}
		return err
	}
	if res.Status != capital.ReservationActive {
		return nil
	}
	_, err = e.reservations.Release(ctx, tx, rid, reason)
	return err
}
