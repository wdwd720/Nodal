package stableasset

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
)

// BurnRequest asks to burn Quantity base units of a stable asset from the
// account's sandbox wallet and redeem them for the policy's payout reserve
// asset at the target peg (par, rounded down).
type BurnRequest struct {
	StableAssetID  StableAssetID
	AccountID      accounts.AccountID
	Quantity       money.Quantity
	IdempotencyKey string
	Mode           gates.Mode
	Actor          econ.Actor
	Facts          AccountFacts
	CorrelationID  string
}

func (r BurnRequest) validate() error {
	var problems []string
	if r.StableAssetID.IsZero() || r.AccountID.IsZero() {
		problems = append(problems, "stable asset and account are required")
	}
	if !r.Quantity.IsPositive() {
		problems = append(problems, "quantity must be positive")
	} else if _, err := devnet.AmountOf(r.Quantity); err != nil {
		problems = append(problems, "quantity must fit a devnet amount (u64)")
	}
	if !trimmed(r.IdempotencyKey) || len(r.IdempotencyKey) > 200 {
		problems = append(problems, "idempotency key must be 1..200 bytes without surrounding whitespace")
	}
	if r.Mode != gates.ModeSandbox && r.Mode != gates.ModeDevnet {
		problems = append(problems, "mode must be SANDBOX or DEVNET")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "stableasset: invalid burn request").WithField("problems", problems)
	}
	return nil
}

// BurnTicket is what PrepareBurn recorded (see MintTicket).
type BurnTicket struct {
	Request BurnRecord
	Replay  bool
	Refusal error
}

// BurnResult is a burn request's state when Burn returns.
type BurnResult struct {
	Request BurnRecord
	Replay  bool
	// Settled reports that the request is terminal (REDEEMED or FAILED).
	Settled bool
}

type burnEffectRequest struct {
	IntentID      string `json:"intent_id"`
	StableAssetID string `json:"stable_asset_id"`
	AccountID     string `json:"account_id"`
	Quantity      string `json:"quantity"`
}

// Burn runs the burn and redemption workflow, the exact inverse of Mint with
// the same exactly-once structure: the holder's ledger units are reserved,
// the holder's custodied devnet key signs TOKEN_BURN after inspection, the
// burn is submitted once, and the redemption posts only after the burn is
// FINALIZED (or is queued, in redemption mode QUEUED).
func (s *Service) Burn(ctx context.Context, req BurnRequest) (BurnResult, error) {
	if err := req.validate(); err != nil {
		return BurnResult{}, err
	}
	a, err := s.Get(ctx, s.db, req.StableAssetID)
	if err != nil {
		return BurnResult{}, err
	}
	if err := s.onHost(a); err != nil {
		return BurnResult{}, err
	}
	holder := s.custody.HolderAddress(req.AccountID)
	if err := s.host.EnsureNativeBalance(ctx, a.Chain, holder, s.params.GasMinimum, s.params.GasTopUp); err != nil {
		return BurnResult{}, err
	}
	var ticket BurnTicket
	if err := s.db.InTx(ctx, txOptions, func(ctx context.Context, tx pgx.Tx) error {
		t, err := s.PrepareBurn(ctx, tx, req)
		ticket = t
		return err
	}); err != nil {
		return BurnResult{}, err
	}
	if ticket.Refusal != nil {
		return BurnResult{Replay: ticket.Replay}, ticket.Refusal
	}
	if ticket.Replay {
		return BurnResult{Request: ticket.Request, Replay: true, Settled: ticket.Request.Status.Terminal()}, nil
	}
	return s.driveBurn(ctx, ticket.Request)
}

// PrepareBurn is the first transaction of a burn (see PrepareMint).
func (s *Service) PrepareBurn(ctx context.Context, tx pgx.Tx, req BurnRequest) (BurnTicket, error) {
	if err := req.validate(); err != nil {
		return BurnTicket{}, err
	}
	if err := s.authenticate(ctx, req.Actor, req.AccountID); err != nil {
		return BurnTicket{}, err
	}
	if err := s.authorizer.Require(ctx, tx, gates.StableAssetMint, req.Mode); err != nil {
		return BurnTicket{}, err
	}
	a, err := s.get(ctx, tx, req.StableAssetID, true)
	if err != nil {
		return BurnTicket{}, err
	}
	if err := s.onHost(a); err != nil {
		return BurnTicket{}, err
	}
	holder := s.custody.HolderAddress(req.AccountID)
	// One read of the chain through this transaction: the tip the plan will
	// reference and the holder's balance, at the same instant.
	var tip devnet.BlockInfo
	var onChain uint64
	chainErr := s.host.ViewIn(ctx, tx, func(v devnethost.View) error {
		var err error
		if tip, err = v.Tip(a.Chain); err != nil {
			return err
		}
		onChain, err = v.TokenBalance(a.Chain, holder, a.Mint)
		return err
	})
	terms, err := json.Marshal(map[string]string{"stable_asset_id": a.ID.String()})
	if err != nil {
		return BurnTicket{}, errs.Wrap(err, errs.CodeInternal, "stableasset: encode terms")
	}
	q := req.Quantity
	in, existing, err := s.intents.Submit(ctx, tx, econ.Intent{
		AccountID: req.AccountID, Kind: econ.KindBurnStableAsset, Mode: req.Mode, Capability: gates.StableAssetMint,
		ActorType: req.Actor.Type, ActorID: req.Actor.ID, AssetID: &a.AssetID, Quantity: &q, Terms: terms,
		IdempotencyKey: req.IdempotencyKey, CorrelationID: req.CorrelationID,
	})
	if err != nil {
		return BurnTicket{}, err
	}
	if existing {
		return s.replayBurn(ctx, tx, in)
	}
	refused := func(r refusal) (BurnTicket, error) {
		if err := s.reject(ctx, tx, in.ID, req.Actor, r); err != nil {
			return BurnTicket{}, err
		}
		return BurnTicket{Refusal: r.err}, nil
	}

	if r, ok, err := s.route(in, a, req.Facts); err != nil {
		return BurnTicket{}, err
	} else if !ok {
		return refused(r)
	}
	if _, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusRouted, req.Actor, "routed through the settlement compiler", ""); err != nil {
		return BurnTicket{}, err
	}
	acct, err := s.accounts.Get(ctx, tx, req.AccountID)
	if err != nil {
		return BurnTicket{}, err
	}
	if acct.Kind != accounts.KindCustomer || !acct.Status.AllowsRiskReduction() {
		return refused(refuse(RefusalAccountIneligible, "the account may not redeem"))
	}
	if !a.Status.AllowsBurn() {
		return refused(refuse(RefusalAssetNotActive, "the stable asset is "+string(a.Status)+" and is not redeeming"))
	}
	payout, ok := a.payoutReserve()
	if !ok {
		return BurnTicket{}, errs.New(errs.CodeInternal, "stableasset: the reserve policy has no payout asset")
	}
	pq, err := parPayout(q, a.Decimals, a.Peg, payout.Decimals)
	if err != nil {
		return BurnTicket{}, err
	}
	if !pq.IsPositive() {
		return refused(refuse(RefusalRedemptionBelowMinimum, "the burn is worth less than one base unit of the payout asset"))
	}
	now := s.clk.Now().UTC()
	rl, err := s.reserveLedger(ctx, tx, a, now)
	if err != nil {
		return BurnTicket{}, err
	}
	if rl.cashHolding(payout.AssetID).Sub(sumQuantity(rl.PendingRedemption, payout.AssetID)).Cmp(pq) < 0 {
		return refused(refuse(RefusalReserveInsufficient, "the cash reserve cannot pay this redemption"))
	}
	if chainErr != nil {
		return BurnTicket{}, chainErr
	}
	pending, err := s.pendingHolderBurns(ctx, tx, a.ID, req.AccountID)
	if err != nil {
		return BurnTicket{}, err
	}
	if devnet.QuantityOf(onChain).Sub(pending).Cmp(q) < 0 {
		return refused(refuse(RefusalChainBalanceInsufficient, "the holder's wallet on chain does not hold this many units"))
	}

	avail, err := s.capital.Availability(ctx, tx, req.AccountID.String(), a.AssetID, now)
	if err != nil {
		return BurnTicket{}, err
	}
	notional, err := unitsValue(q, a.Decimals).times(a.Peg).ToUSD(money.RoundUp)
	if err != nil {
		return BurnTicket{}, err
	}
	available, err := unitsValue(avail.Available, a.Decimals).times(a.Peg).ToUSD(money.RoundDown)
	if err != nil {
		return BurnTicket{}, err
	}
	if r, ok, err := s.evaluateRisk(ctx, tx, in, a, acct, req.Facts,
		riskSections{notional: notional, available: available, required: notional, riskIncreasing: false, chainHealthy: chainErr == nil}); err != nil {
		return BurnTicket{}, err
	} else if !ok {
		return refused(r)
	}
	if _, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusRiskApproved, req.Actor, "risk kernel allowed the burn", ""); err != nil {
		return BurnTicket{}, err
	}
	res, err := s.capital.Reserve(ctx, tx, capital.ReserveRequest{
		AccountID: req.AccountID.String(), AssetID: a.AssetID, Quantity: q, ActorType: req.Actor.Type, ActorID: req.Actor.ID,
		EconomicIntentID: in.ID.String(),
		IdempotencyKey:   "stableasset:burn:" + in.ID.String(), TTL: s.params.ReservationTTL, Reason: "stable asset burn",
	})
	if err != nil {
		if isBusinessRefusal(err) {
			return refused(refusal{code: RefusalInsufficientFunds, err: err})
		}
		return BurnTicket{}, err
	}
	if _, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusReserved, req.Actor, "units to burn reserved", ""); err != nil {
		return BurnTicket{}, err
	}
	effect, replayed, err := s.effects.Begin(ctx, tx, econ.BeginRequest{
		Key: "stableasset:burn:" + in.ID.String(), Subsystem: econ.SubsystemStableAsset, Kind: effectKindBurn,
		IntentID: &in.ID, AccountID: req.AccountID.String(), PostsLedger: true, Actor: req.Actor, CorrelationID: req.CorrelationID,
		Request: burnEffectRequest{IntentID: in.ID.String(), StableAssetID: a.ID.String(), AccountID: req.AccountID.String(), Quantity: q.String()},
	})
	if err != nil {
		return BurnTicket{}, err
	}
	if replayed {
		return BurnTicket{}, errs.New(errs.CodeInternal, "stableasset: a fresh intent found its effect already claimed")
	}

	blockhash := tip.Hash
	rid := id.New[burnRequestKind]()
	plan := BurnPlan{
		Version: burnPlanVersion, RequestID: rid.String(), StableAssetID: a.ID.String(), AccountID: req.AccountID.String(),
		Chain: string(a.Chain), Mint: a.Mint.String(), Holder: holder.String(), Amount: q.String(), FeePayer: holder.String(),
		RecentBlockhash: blockhash.String(), BlockhashHeight: strconv.FormatUint(tip.Height, 10),
		PayoutAssetID: payout.AssetID.String(), PayoutQuantity: pq.String(),
		RedemptionMode: string(a.RedemptionMode), Peg: priceDoc(a.Peg),
	}
	planHash, _, err := plan.Hash()
	if err != nil {
		return BurnTicket{}, err
	}
	signing, err := plan.SigningPlan()
	if err != nil {
		return BurnTicket{}, err
	}
	msg, err := plan.Message()
	if err != nil {
		return BurnTicket{}, err
	}
	signed, err := s.custody.SignHolderBurn(req.AccountID, plan, msg)
	if err != nil {
		return BurnTicket{}, err
	}
	rec := BurnRecord{
		ID: rid, StableAssetID: a.ID, AccountID: req.AccountID, IntentID: in.ID, EffectID: effect.ID, Mode: req.Mode,
		Quantity: q, PayoutAssetID: payout.AssetID, PayoutQuantity: pq, ReservationID: res.ID, Holder: holder,
		Plan: plan, PlanHash: planHash, SigningPlan: signing, Chain: a.Chain, TxID: signed.TxID, SignedTx: signed.Raw,
		RecentBlockhash: blockhash, Status: BurnSubmitting,
	}
	if err := insertBurn(ctx, tx, rec, req.Actor, now); err != nil {
		return BurnTicket{}, err
	}
	if err := s.capital.LockForOrder(ctx, tx, res.ID, rid.String()); err != nil {
		return BurnTicket{}, err
	}
	if _, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusExecuting, req.Actor, "burn plan signed; submitting", ""); err != nil {
		return BurnTicket{}, err
	}
	stored, err := getBurn(ctx, tx, rid, false)
	if err != nil {
		return BurnTicket{}, err
	}
	return BurnTicket{Request: stored}, nil
}

// pendingHolderBurns is what the holder's open burns will still take from its
// wallet on chain: burns not yet landed or known failed.
func (s *Service) pendingHolderBurns(ctx context.Context, tx pgx.Tx, sid StableAssetID, account accounts.AccountID) (money.Quantity, error) {
	var text string
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(quantity), 0)::text FROM stable_burn_requests
		WHERE stable_asset_id = $1 AND account_id = $2 AND status IN ('SUBMITTING','SUBMITTED','SUBMISSION_UNKNOWN')`,
		sid, account).Scan(&text); err != nil {
		return money.Quantity{}, dbErr("sum pending burns", err)
	}
	return money.ScanQuantity(text)
}

func (s *Service) replayBurn(ctx context.Context, tx pgx.Tx, in econ.Intent) (BurnTicket, error) {
	rec, found, err := burnByIntent(ctx, tx, in.ID)
	if err != nil {
		return BurnTicket{}, err
	}
	if found {
		return BurnTicket{Request: rec, Replay: true}, nil
	}
	if in.Status == econ.StatusRejected {
		return BurnTicket{Replay: true, Refusal: errs.New(refusalCode(in.RejectionCode),
			"stableasset: this request was refused").WithField("reason", in.RejectionCode)}, nil
	}
	return BurnTicket{}, errs.New(errs.CodeInternal, "stableasset: an intent without its burn request").
		WithField("intent_id", in.ID.String())
}

func (s *Service) driveBurn(ctx context.Context, rec BurnRecord) (BurnResult, error) {
	outcome, subErr := s.submit(ctx, rec.Chain, rec.SignedTx)
	rec, err := s.recordBurnSubmission(ctx, rec.ID, outcome, subErr)
	if err != nil {
		return BurnResult{Request: rec}, err
	}
	switch rec.Status {
	case BurnFailed:
		return BurnResult{Request: rec, Settled: true}, nil
	case BurnSubmissionUnknown:
		return BurnResult{Request: rec}, submissionUnknown(rec.TxID, subErr)
	}
	if _, err := s.awaitFinal(ctx, rec.Chain, rec.TxID, rec.RecentBlockhash, parseHeight(rec.Plan.BlockhashHeight), rec.Mode == gates.ModeSandbox); err != nil {
		return BurnResult{Request: rec}, err
	}
	return s.settleBurnTx(ctx, rec.ID)
}

func (s *Service) settleBurnTx(ctx context.Context, rid BurnRequestID) (BurnResult, error) {
	var rec BurnRecord
	err := s.db.InTx(ctx, txOptions, func(ctx context.Context, tx pgx.Tx) error {
		r, err := s.SettleBurn(ctx, tx, rid)
		rec = r
		return err
	})
	return BurnResult{Request: rec, Settled: rec.Status.Terminal()}, err
}

func (s *Service) recordBurnSubmission(ctx context.Context, rid BurnRequestID, outcome submission, subErr error) (BurnRecord, error) {
	var out BurnRecord
	err := s.db.InTx(ctx, txOptions, func(ctx context.Context, tx pgx.Tx) error {
		a, rec, err := s.lockBurn(ctx, tx, rid)
		if err != nil {
			return err
		}
		out = rec
		if rec.Status != BurnSubmitting {
			return nil
		}
		switch outcome {
		case submitted:
			out, err = s.transitionBurn(ctx, tx, rec, BurnSubmitted, systemActor, "the chain accepted the submission", "", nil, nil)
		case submitUnknown:
			reason := "the submission's outcome is unknown"
			if subErr != nil {
				reason += ": " + subErr.Error()
			}
			if _, err = s.effects.MarkUnknown(ctx, tx, rec.EffectID, externalRef(rec.Chain, rec.TxID), systemActor, reason); err != nil {
				return err
			}
			if _, err = s.intents.Transition(ctx, tx, rec.IntentID, econ.StatusUnknown, systemActor, reason, ""); err != nil {
				return err
			}
			out, err = s.transitionBurn(ctx, tx, rec, BurnSubmissionUnknown, systemActor, reason, "", nil, nil)
		default:
			out, err = s.failBurn(ctx, tx, a, rec, "SUBMISSION_REFUSED:"+string(devnet.ReasonOf(subErr)), "the chain refused the transaction")
		}
		return err
	})
	return out, err
}

func (s *Service) lockBurn(ctx context.Context, tx pgx.Tx, rid BurnRequestID) (PlatformStableAsset, BurnRecord, error) {
	peek, err := getBurn(ctx, tx, rid, false)
	if err != nil {
		return PlatformStableAsset{}, BurnRecord{}, err
	}
	a, err := s.get(ctx, tx, peek.StableAssetID, true)
	if err != nil {
		return PlatformStableAsset{}, BurnRecord{}, err
	}
	if err := s.onHost(a); err != nil {
		return PlatformStableAsset{}, BurnRecord{}, err
	}
	rec, err := getBurn(ctx, tx, rid, true)
	return a, rec, err
}

// SettleBurn acts on the chain's final word for a burn (see SettleMint). A
// FINALIZED burn is redeemed at once in redemption mode INSTANT and queued
// in mode QUEUED; a queued burn is paid by ProcessRedemptions.
func (s *Service) SettleBurn(ctx context.Context, tx pgx.Tx, rid BurnRequestID) (BurnRecord, error) {
	a, rec, err := s.lockBurn(ctx, tx, rid)
	if err != nil || rec.Status.Terminal() || rec.Status == BurnRedemptionQueued {
		return rec, err
	}
	obs, err := s.observe(ctx, tx, rec.Chain, rec.TxID, rec.RecentBlockhash, parseHeight(rec.Plan.BlockhashHeight))
	if err != nil {
		return rec, err
	}
	switch {
	case obs.Finalized():
		if a.RedemptionMode == RedemptionQueued {
			slot := int64(obs.Status.Slot) // #nosec G115 -- a devnet slot counts blocks and stays far below 2^63
			return s.transitionBurn(ctx, tx, rec, BurnRedemptionQueued, systemActor, "burn finalized on chain; redemption queued", "", nil, &slot)
		}
		return s.redeem(ctx, tx, a, rec, &obs)
	case obs.FailedFinal():
		return s.failBurn(ctx, tx, a, rec, "CHAIN_FAILED:"+string(obs.Status.Err), "the burn failed on chain")
	case obs.Expired:
		return s.failBurn(ctx, tx, a, rec, "SUBMISSION_EXPIRED",
			"the chain does not hold the transaction and its blockhash expired below finality; it can never land")
	}
	return rec, nil
}

type burnEvidence struct {
	Disclaimer           string            `json:"disclaimer"`
	RequestID            string            `json:"request_id"`
	IntentID             string            `json:"intent_id"`
	EffectID             string            `json:"effect_id"`
	Plan                 BurnPlan          `json:"plan"`
	PlanHash             string            `json:"plan_hash"`
	SigningPlan          SigningPlan       `json:"signing_plan"`
	Observation          *ChainObservation `json:"observation,omitempty"`
	JournalTransactionID string            `json:"journal_transaction_id"`
	ReserveAfter         ReserveLedger     `json:"reserve_after"`
}

// redeem posts STABLE_REDEMPTION for a burn that is final on chain: the
// payout must still be covered by the cash reserve, and the burn is the
// holder's obligation to honour whatever the asset's status, because the
// units no longer exist on chain.
func (s *Service) redeem(ctx context.Context, tx pgx.Tx, a PlatformStableAsset, rec BurnRecord, obs *ChainObservation) (BurnRecord, error) {
	now := s.clk.Now().UTC()
	before, err := s.reserveLedger(ctx, tx, a, now)
	if err != nil {
		return BurnRecord{}, err
	}
	if before.cashHolding(rec.PayoutAssetID).Cmp(rec.PayoutQuantity) < 0 {
		return BurnRecord{}, errs.New(errs.CodeInternal, "stableasset: the cash reserve cannot pay a final burn; it stays unposted for review").
			WithField("request_id", rec.ID.String())
	}
	res, err := s.ledger.Post(ctx, tx, redemptionPosting(a, rec, now))
	if err != nil {
		return BurnRecord{}, err
	}
	if _, err := s.capital.Consume(ctx, tx, rec.ReservationID, rec.Quantity, 0, rec.ID.String()); err != nil {
		return BurnRecord{}, err
	}
	after, err := s.reserveLedger(ctx, tx, a, now)
	if err != nil {
		return BurnRecord{}, err
	}
	ev, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "stableasset/burn/" + rec.ID.String(), Document: burnEvidence{
		Disclaimer: evidenceDisclaimer, RequestID: rec.ID.String(), IntentID: rec.IntentID.String(), EffectID: rec.EffectID.String(),
		Plan: rec.Plan, PlanHash: hex.EncodeToString(rec.PlanHash[:]), SigningPlan: rec.SigningPlan, Observation: obs,
		JournalTransactionID: res.TransactionID.String(), ReserveAfter: after,
	}})
	if err != nil {
		return BurnRecord{}, err
	}
	if _, err := s.effects.Apply(ctx, tx, rec.EffectID, econ.ApplyRequest{
		JournalTransactionID: &res.TransactionID, ReservationID: rec.ReservationID.String(),
		ExternalRef: externalRef(rec.Chain, rec.TxID), Evidence: &ev, Actor: systemActor, Reason: "burn finalized on chain; redemption posted",
	}); err != nil {
		return BurnRecord{}, err
	}
	var slot *int64
	if obs != nil {
		v := int64(obs.Status.Slot) // #nosec G115 -- a devnet slot counts blocks and stays far below 2^63
		slot = &v
	}
	out, err := s.transitionBurn(ctx, tx, rec, BurnRedeemed, systemActor, "redemption posted", "", &res.TransactionID, slot)
	if err != nil {
		return BurnRecord{}, err
	}
	if err := s.finishIntent(ctx, tx, rec.IntentID, true, "burn redeemed"); err != nil {
		return BurnRecord{}, err
	}
	// Reserve and supply after the posting, under the same lock: a shortfall
	// or a difference with the chain pauses the asset in this transaction.
	if _, err := s.reconcileLocked(ctx, tx, &a, reconciliation.ModeEventDriven, systemActor); err != nil {
		return BurnRecord{}, err
	}
	return out, nil
}

func (s *Service) failBurn(ctx context.Context, tx pgx.Tx, _ PlatformStableAsset, rec BurnRecord, code, reason string) (BurnRecord, error) {
	if _, err := s.effects.Fail(ctx, tx, rec.EffectID, code, systemActor, reason); err != nil {
		return BurnRecord{}, err
	}
	if _, err := s.capital.Release(ctx, tx, rec.ReservationID, reason); err != nil {
		return BurnRecord{}, err
	}
	out, err := s.transitionBurn(ctx, tx, rec, BurnFailed, systemActor, reason, code, nil, nil)
	if err != nil {
		return BurnRecord{}, err
	}
	return out, s.finishIntent(ctx, tx, rec.IntentID, false, reason)
}

// PayRedemption posts the redemption of one queued burn.
func (s *Service) PayRedemption(ctx context.Context, tx pgx.Tx, rid BurnRequestID) (BurnRecord, error) {
	a, rec, err := s.lockBurn(ctx, tx, rid)
	if err != nil {
		return BurnRecord{}, err
	}
	if rec.Status != BurnRedemptionQueued {
		return rec, nil
	}
	return s.redeem(ctx, tx, a, rec, nil)
}
