package stableasset

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
)

// systemActor is the actor of every step after admission: the service, never
// the requester and never an agent.
var systemActor = econ.SystemActor("stableasset")

// MintRequest asks to issue Quantity base units of a stable asset to the
// account's sandbox wallet, paid in ReserveAssetID (an eligible cash
// equivalent) at the target peg.
type MintRequest struct {
	StableAssetID  StableAssetID
	AccountID      accounts.AccountID
	Quantity       money.Quantity
	ReserveAssetID assets.AssetID
	IdempotencyKey string
	// Mode is SANDBOX (the service produces blocks and drives the request to
	// a final state) or DEVNET (the service observes; blocks come from the
	// chain's own producer).
	Mode          gates.Mode
	Actor         econ.Actor
	Facts         AccountFacts
	CorrelationID string
}

func (r MintRequest) validate() error {
	var problems []string
	if r.StableAssetID.IsZero() || r.AccountID.IsZero() || r.ReserveAssetID.IsZero() {
		problems = append(problems, "stable asset, account and reserve asset are required")
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
		return errs.New(errs.CodeValidationFailed, "stableasset: invalid mint request").WithField("problems", problems)
	}
	return nil
}

// MintTicket is what PrepareMint recorded.
type MintTicket struct {
	// Request is the recorded request (zero when refused).
	Request MintRecord
	// Replay is true when the idempotency key named an earlier request.
	Replay bool
	// Refusal is set when the request was refused. The refusal is already
	// recorded in the transaction (intent REJECTED, risk decision persisted),
	// so the caller commits and then returns Refusal.
	Refusal error
}

// MintResult is a mint request's state when Mint returns.
type MintResult struct {
	Request MintRecord
	Replay  bool
	// Settled reports that the request is terminal (ISSUED or FAILED).
	Settled bool
}

type mintTerms struct {
	StableAssetID  string `json:"stable_asset_id"`
	ReserveAssetID string `json:"reserve_asset_id"`
}

type mintEffectRequest struct {
	IntentID       string `json:"intent_id"`
	StableAssetID  string `json:"stable_asset_id"`
	AccountID      string `json:"account_id"`
	Quantity       string `json:"quantity"`
	ReserveAssetID string `json:"reserve_asset_id"`
}

// Mint runs the whole issuance workflow (goal §5): admission, reservation,
// plan, inspection and signing in one transaction; the submission between
// transactions, exactly once; the outcome in a second transaction; and, in
// SANDBOX mode, block production until the transaction is final and the
// posting in a third. A submission whose outcome is unknown returns
// SUBMISSION_STATE_UNKNOWN and is left to ReconcileUnknown. A replayed
// idempotency key returns the existing request and never submits again.
func (s *Service) Mint(ctx context.Context, req MintRequest) (MintResult, error) {
	if err := req.validate(); err != nil {
		return MintResult{}, err
	}
	a, err := s.Get(ctx, s.db, req.StableAssetID)
	if err != nil {
		return MintResult{}, err
	}
	if err := s.host.EnsureNativeBalance(ctx, a.Chain, s.issuer.PublicKey(), s.params.GasMinimum, s.params.GasTopUp); err != nil {
		return MintResult{}, err
	}
	var ticket MintTicket
	if err := s.db.InTx(ctx, txOptions, func(ctx context.Context, tx pgx.Tx) error {
		t, err := s.PrepareMint(ctx, tx, req)
		ticket = t
		return err
	}); err != nil {
		return MintResult{}, err
	}
	if ticket.Refusal != nil {
		return MintResult{Replay: ticket.Replay}, ticket.Refusal
	}
	if ticket.Replay {
		return MintResult{Request: ticket.Request, Replay: true, Settled: ticket.Request.Status.Terminal()}, nil
	}
	return s.driveMint(ctx, ticket.Request)
}

// PrepareMint is the first transaction of a mint: authenticate, authorize the
// mode, record the intent (idempotent), route, check eligibility, verify the
// reserve payment's price, apply the fully reserved admission rule under the
// asset's row lock, evaluate Risk V2, reserve the reserve payment, claim the
// effect, build and hash the plan, inspect and sign the transaction, and
// record the request SUBMITTING with its signed bytes. Nothing is submitted.
func (s *Service) PrepareMint(ctx context.Context, tx pgx.Tx, req MintRequest) (MintTicket, error) {
	if err := req.validate(); err != nil {
		return MintTicket{}, err
	}
	if err := s.authenticate(ctx, req.Actor, req.AccountID); err != nil {
		return MintTicket{}, err
	}
	if err := s.authorizer.Require(ctx, tx, gates.StableAssetMint, req.Mode); err != nil {
		return MintTicket{}, err
	}
	a, err := s.get(ctx, tx, req.StableAssetID, true)
	if err != nil {
		return MintTicket{}, err
	}
	if err := s.onHost(a); err != nil {
		return MintTicket{}, err
	}
	// The chain is read through this transaction: the tip is the blockhash
	// the plan will reference, and a failed read is an unhealthy chain.
	tip, tipErr := s.chainTip(ctx, tx, a.Chain)
	terms, err := json.Marshal(mintTerms{StableAssetID: a.ID.String(), ReserveAssetID: req.ReserveAssetID.String()})
	if err != nil {
		return MintTicket{}, errs.Wrap(err, errs.CodeInternal, "stableasset: encode terms")
	}
	q := req.Quantity
	in, existing, err := s.intents.Submit(ctx, tx, econ.Intent{
		AccountID: req.AccountID, Kind: econ.KindMintStableAsset, Mode: req.Mode, Capability: gates.StableAssetMint,
		ActorType: req.Actor.Type, ActorID: req.Actor.ID, AssetID: &a.AssetID, Quantity: &q, Terms: terms,
		IdempotencyKey: req.IdempotencyKey, CorrelationID: req.CorrelationID,
	})
	if err != nil {
		return MintTicket{}, err
	}
	if existing {
		return s.replayMint(ctx, tx, in)
	}
	refused := func(r refusal) (MintTicket, error) {
		if err := s.reject(ctx, tx, in.ID, req.Actor, r); err != nil {
			return MintTicket{}, err
		}
		return MintTicket{Refusal: r.err}, nil
	}

	if r, ok, err := s.route(in, a, req.Facts); err != nil {
		return MintTicket{}, err
	} else if !ok {
		return refused(r)
	}
	if _, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusRouted, req.Actor, "routed through the settlement compiler", ""); err != nil {
		return MintTicket{}, err
	}
	acct, err := s.accounts.Get(ctx, tx, req.AccountID)
	if err != nil {
		return MintTicket{}, err
	}
	if acct.Kind != accounts.KindCustomer || !acct.Status.AllowsNewRisk() {
		return refused(refuse(RefusalAccountIneligible, "the account may not take on a new stable asset position"))
	}
	if !a.Status.AllowsMint() {
		return refused(refuse(RefusalAssetNotActive, "the stable asset is "+string(a.Status)+" and is not issuing"))
	}
	reserve, ok := a.Reserve(req.ReserveAssetID)
	if !ok || !reserve.AcceptedForMint {
		return refused(refuse(RefusalReserveAssetIneligible, "the reserve asset is not an eligible mint payment"))
	}
	now := s.clk.Now().UTC()
	prices, err := s.pricing(ctx, tx, a, now)
	if err != nil {
		return MintTicket{}, err
	}
	price := prices.prices[reserve.AssetID]
	if price == nil {
		return refused(refuse(RefusalReservePriceUnavailable, "the reserve payment has no fresh price from the oracle source"))
	}
	rq, err := requiredReserve(q, a.Decimals, a.Peg, reserve.Decimals, *price, reserve.FactorBPS)
	if err != nil {
		return MintTicket{}, err
	}
	rl, err := s.reserveLedgerAt(ctx, tx, a, prices, now)
	if err != nil {
		return MintTicket{}, err
	}
	if reason := admitMint(rl, a, q, prices.holding(reserve, rq)); reason != "" {
		return refused(refuse(reason, "the fully reserved admission rule refused the mint"))
	}

	avail, err := s.capital.Availability(ctx, tx, req.AccountID.String(), reserve.AssetID, now)
	if err != nil {
		return MintTicket{}, err
	}
	sec, err := mintRiskSections(a, reserve, *price, q, rq, avail.Available)
	if err != nil {
		return MintTicket{}, err
	}
	sec.chainHealthy = tipErr == nil
	if r, ok, err := s.evaluateRisk(ctx, tx, in, a, acct, req.Facts, sec); err != nil {
		return MintTicket{}, err
	} else if !ok {
		return refused(r)
	}
	if _, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusRiskApproved, req.Actor, "risk kernel allowed the mint", ""); err != nil {
		return MintTicket{}, err
	}

	res, err := s.capital.Reserve(ctx, tx, capital.ReserveRequest{
		AccountID: req.AccountID.String(), AssetID: reserve.AssetID, Quantity: rq, ActorType: req.Actor.Type, ActorID: req.Actor.ID,
		EconomicIntentID: in.ID.String(),
		IdempotencyKey:   "stableasset:mint:" + in.ID.String(), TTL: s.params.ReservationTTL, Reason: "stable asset mint: reserve payment",
	})
	if err != nil {
		if isBusinessRefusal(err) {
			return refused(refusal{code: RefusalInsufficientFunds, err: err})
		}
		return MintTicket{}, err
	}
	if _, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusReserved, req.Actor, "reserve payment reserved", ""); err != nil {
		return MintTicket{}, err
	}
	effect, replayed, err := s.effects.Begin(ctx, tx, econ.BeginRequest{
		Key: "stableasset:mint:" + in.ID.String(), Subsystem: econ.SubsystemStableAsset, Kind: effectKindMint,
		IntentID: &in.ID, AccountID: req.AccountID.String(), PostsLedger: true, Actor: req.Actor, CorrelationID: req.CorrelationID,
		Request: mintEffectRequest{IntentID: in.ID.String(), StableAssetID: a.ID.String(), AccountID: req.AccountID.String(),
			Quantity: q.String(), ReserveAssetID: reserve.AssetID.String()},
	})
	if err != nil {
		return MintTicket{}, err
	}
	if replayed {
		return MintTicket{}, errs.New(errs.CodeInternal, "stableasset: a fresh intent found its effect already claimed")
	}

	if tipErr != nil {
		return MintTicket{}, tipErr
	}
	blockhash := tip.Hash
	rid := id.New[mintRequestKind]()
	plan := MintPlan{
		Version: mintPlanVersion, RequestID: rid.String(), StableAssetID: a.ID.String(), AccountID: req.AccountID.String(),
		Chain: string(a.Chain), Mint: a.Mint.String(), Recipient: s.custody.HolderAddress(req.AccountID).String(),
		Amount: q.String(), MintAuthority: a.MintAuthority.String(), FeePayer: a.MintAuthority.String(),
		RecentBlockhash: blockhash.String(), BlockhashHeight: strconv.FormatUint(tip.Height, 10),
		ReserveAssetID: reserve.AssetID.String(), ReserveQuantity: rq.String(),
		ReserveFactor: strconv.FormatInt(int64(reserve.FactorBPS), 10), ReservePrice: priceDoc(*price), Peg: priceDoc(a.Peg),
	}
	planHash, _, err := plan.Hash()
	if err != nil {
		return MintTicket{}, err
	}
	signing, err := plan.SigningPlan()
	if err != nil {
		return MintTicket{}, err
	}
	msg, err := plan.Message()
	if err != nil {
		return MintTicket{}, err
	}
	signed, err := s.issuer.SignMint(plan, msg)
	if err != nil {
		return MintTicket{}, err
	}
	rec := MintRecord{
		ID: rid, StableAssetID: a.ID, AccountID: req.AccountID, IntentID: in.ID, EffectID: effect.ID, Mode: req.Mode,
		Quantity: q, ReserveAssetID: reserve.AssetID, ReserveQuantity: rq, ReservationID: res.ID,
		Recipient: s.custody.HolderAddress(req.AccountID), Plan: plan, PlanHash: planHash, SigningPlan: signing,
		Chain: a.Chain, TxID: signed.TxID, SignedTx: signed.Raw, RecentBlockhash: blockhash, Status: MintSubmitting,
		CreatedAt: now,
	}
	if err := insertMint(ctx, tx, rec, req.Actor, now); err != nil {
		return MintTicket{}, err
	}
	// The reservation is bound to the request, so expiry leaves it alone
	// for as long as the chain outcome is unknown.
	if err := s.capital.LockForOrder(ctx, tx, res.ID, rid.String()); err != nil {
		return MintTicket{}, err
	}
	if _, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusExecuting, req.Actor, "mint plan signed; submitting", ""); err != nil {
		return MintTicket{}, err
	}
	stored, err := getMint(ctx, tx, rid, false)
	if err != nil {
		return MintTicket{}, err
	}
	return MintTicket{Request: stored}, nil
}

// mintRiskSections values the mint for the risk kernel: the notional is the
// issued value at the peg (rounded up), the balance section compares the
// reserve asset available to the account with the payment required, both
// at the verified reserve price.
func mintRiskSections(a PlatformStableAsset, reserve EligibleReserve, price money.Price, q, rq, available money.Quantity) (riskSections, error) {
	notional, err := unitsValue(q, a.Decimals).times(a.Peg).ToUSD(money.RoundUp)
	if err != nil {
		return riskSections{}, err
	}
	avail, err := unitsValue(available, reserve.Decimals).times(price).ToUSD(money.RoundDown)
	if err != nil {
		return riskSections{}, err
	}
	required, err := unitsValue(rq, reserve.Decimals).times(price).ToUSD(money.RoundUp)
	if err != nil {
		return riskSections{}, err
	}
	return riskSections{notional: notional, available: avail, required: required, riskIncreasing: true}, nil
}

// replayMint answers a replayed idempotency key from what the first request
// recorded. It never submits anything.
func (s *Service) replayMint(ctx context.Context, tx pgx.Tx, in econ.Intent) (MintTicket, error) {
	rec, found, err := mintByIntent(ctx, tx, in.ID)
	if err != nil {
		return MintTicket{}, err
	}
	if found {
		return MintTicket{Request: rec, Replay: true}, nil
	}
	if in.Status == econ.StatusRejected {
		return MintTicket{Replay: true, Refusal: errs.New(refusalCode(in.RejectionCode),
			"stableasset: this request was refused").WithField("reason", in.RejectionCode)}, nil
	}
	return MintTicket{}, errs.New(errs.CodeInternal, "stableasset: an intent without its mint request").
		WithField("intent_id", in.ID.String())
}

// driveMint submits a freshly prepared request exactly once, records the
// outcome, and drives a SANDBOX request to a final state.
func (s *Service) driveMint(ctx context.Context, rec MintRecord) (MintResult, error) {
	outcome, subErr := s.submit(ctx, rec.Chain, rec.SignedTx)
	rec, err := s.recordMintSubmission(ctx, rec.ID, outcome, subErr)
	if err != nil {
		return MintResult{Request: rec}, err
	}
	switch rec.Status {
	case MintFailed:
		return MintResult{Request: rec, Settled: true}, nil
	case MintSubmissionUnknown:
		return MintResult{Request: rec}, submissionUnknown(rec.TxID, subErr)
	}
	if _, err := s.awaitFinal(ctx, rec.Chain, rec.TxID, rec.RecentBlockhash, parseHeight(rec.Plan.BlockhashHeight), rec.Mode == gates.ModeSandbox); err != nil {
		return MintResult{Request: rec}, err
	}
	return s.settleMintTx(ctx, rec.ID)
}

func (s *Service) settleMintTx(ctx context.Context, rid MintRequestID) (MintResult, error) {
	var rec MintRecord
	err := s.db.InTx(ctx, txOptions, func(ctx context.Context, tx pgx.Tx) error {
		r, err := s.SettleMint(ctx, tx, rid)
		rec = r
		return err
	})
	return MintResult{Request: rec, Settled: rec.Status.Terminal()}, err
}

// recordMintSubmission is the second transaction: what the submission did.
func (s *Service) recordMintSubmission(ctx context.Context, rid MintRequestID, outcome submission, subErr error) (MintRecord, error) {
	var out MintRecord
	err := s.db.InTx(ctx, txOptions, func(ctx context.Context, tx pgx.Tx) error {
		a, rec, err := s.lockMint(ctx, tx, rid)
		if err != nil {
			return err
		}
		out = rec
		if rec.Status != MintSubmitting {
			return nil
		}
		switch outcome {
		case submitted:
			out, err = s.transitionMint(ctx, tx, rec, MintSubmitted, systemActor, "the chain accepted the submission", "", nil, nil)
		case submitUnknown:
			out, err = s.markMintUnknown(ctx, tx, rec, subErr)
		default:
			out, err = s.failMint(ctx, tx, a, rec, "SUBMISSION_REFUSED:"+string(devnet.ReasonOf(subErr)), "the chain refused the transaction")
		}
		return err
	})
	return out, err
}

func (s *Service) markMintUnknown(ctx context.Context, tx pgx.Tx, rec MintRecord, cause error) (MintRecord, error) {
	reason := "the submission's outcome is unknown"
	if cause != nil {
		reason += ": " + cause.Error()
	}
	if _, err := s.effects.MarkUnknown(ctx, tx, rec.EffectID, externalRef(rec.Chain, rec.TxID), systemActor, reason); err != nil {
		return MintRecord{}, err
	}
	if _, err := s.intents.Transition(ctx, tx, rec.IntentID, econ.StatusUnknown, systemActor, reason, ""); err != nil {
		return MintRecord{}, err
	}
	return s.transitionMint(ctx, tx, rec, MintSubmissionUnknown, systemActor, reason, "", nil, nil)
}

func externalRef(chain devnet.ChainID, txID devnet.TxID) string {
	return "devnet:" + string(chain) + ":" + string(txID)
}

// lockMint takes the asset row lock, then the request row lock: every step
// that changes a request takes them in this order.
func (s *Service) lockMint(ctx context.Context, tx pgx.Tx, rid MintRequestID) (PlatformStableAsset, MintRecord, error) {
	peek, err := getMint(ctx, tx, rid, false)
	if err != nil {
		return PlatformStableAsset{}, MintRecord{}, err
	}
	a, err := s.get(ctx, tx, peek.StableAssetID, true)
	if err != nil {
		return PlatformStableAsset{}, MintRecord{}, err
	}
	if err := s.onHost(a); err != nil {
		return PlatformStableAsset{}, MintRecord{}, err
	}
	rec, err := getMint(ctx, tx, rid, true)
	return a, rec, err
}

// SettleMint is the transaction that acts on the chain's final word. It
// observes the transaction itself -- it never takes a caller's word for the
// outcome -- and: FINALIZED posts the issuance and applies the effect;
// FAILED (final) or NOT_FOUND past its blockhash fails the request and
// releases the reserve payment; anything else changes nothing. Settling a
// terminal request is a no-op.
func (s *Service) SettleMint(ctx context.Context, tx pgx.Tx, rid MintRequestID) (MintRecord, error) {
	a, rec, err := s.lockMint(ctx, tx, rid)
	if err != nil || rec.Status.Terminal() {
		return rec, err
	}
	obs, err := s.observe(ctx, tx, rec.Chain, rec.TxID, rec.RecentBlockhash, parseHeight(rec.Plan.BlockhashHeight))
	if err != nil {
		return rec, err
	}
	switch {
	case obs.Finalized():
		return s.issue(ctx, tx, a, rec, obs)
	case obs.FailedFinal():
		return s.failMint(ctx, tx, a, rec, "CHAIN_FAILED:"+string(obs.Status.Err), "the mint failed on chain")
	case obs.Expired:
		return s.failMint(ctx, tx, a, rec, "SUBMISSION_EXPIRED",
			"the chain does not hold the transaction and its blockhash expired below finality; it can never land")
	}
	return rec, nil
}

// mintEvidence is the archived record of one issuance.
type mintEvidence struct {
	Disclaimer           string           `json:"disclaimer"`
	RequestID            string           `json:"request_id"`
	IntentID             string           `json:"intent_id"`
	EffectID             string           `json:"effect_id"`
	Plan                 MintPlan         `json:"plan"`
	PlanHash             string           `json:"plan_hash"`
	SigningPlan          SigningPlan      `json:"signing_plan"`
	Observation          ChainObservation `json:"observation"`
	JournalTransactionID string           `json:"journal_transaction_id"`
	ReserveAfter         ReserveLedger    `json:"reserve_after"`
}

const evidenceDisclaimer = "simulated sandbox value on a simulated chain; not money, not an attestation"

// issue posts STABLE_ISSUANCE for a FINALIZED mint. It re-checks the cap and
// the issuance's own full backing under the asset row lock first (the
// database checks both again at COMMIT), then posts, consumes the
// reservation, archives the evidence, applies the effect, and verifies the
// post-issuance reserve position. If aggregate coverage fell between
// admission and finality because a reserve price moved, the issuance -- final
// on chain -- is still recorded, and the circuit breaker pauses the asset.
func (s *Service) issue(ctx context.Context, tx pgx.Tx, a PlatformStableAsset, rec MintRecord, obs ChainObservation) (MintRecord, error) {
	now := s.clk.Now().UTC()
	before, err := s.reserveLedger(ctx, tx, a, now)
	if err != nil {
		return MintRecord{}, err
	}
	if before.IssuedSupply.Add(rec.Quantity).Cmp(a.IssuanceCap) > 0 {
		return MintRecord{}, errs.New(errs.CodeInternal, "stableasset: posting this issuance would exceed the cap; it stays unposted for review").
			WithField("request_id", rec.ID.String())
	}
	if err := checkPlanBacking(a, rec); err != nil {
		return MintRecord{}, err
	}
	res, err := s.ledger.Post(ctx, tx, issuancePosting(a, rec, now))
	if err != nil {
		return MintRecord{}, err
	}
	if _, err := s.capital.Consume(ctx, tx, rec.ReservationID, rec.ReserveQuantity, 0, rec.ID.String()); err != nil {
		return MintRecord{}, err
	}
	after, err := s.reserveLedger(ctx, tx, a, now)
	if err != nil {
		return MintRecord{}, err
	}
	ev, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "stableasset/mint/" + rec.ID.String(), Document: mintEvidence{
		Disclaimer: evidenceDisclaimer, RequestID: rec.ID.String(), IntentID: rec.IntentID.String(), EffectID: rec.EffectID.String(),
		Plan: rec.Plan, PlanHash: hex.EncodeToString(rec.PlanHash[:]), SigningPlan: rec.SigningPlan, Observation: obs,
		JournalTransactionID: res.TransactionID.String(), ReserveAfter: after,
	}})
	if err != nil {
		return MintRecord{}, err
	}
	if _, err := s.effects.Apply(ctx, tx, rec.EffectID, econ.ApplyRequest{
		JournalTransactionID: &res.TransactionID, ReservationID: rec.ReservationID.String(),
		ExternalRef: externalRef(rec.Chain, rec.TxID), Evidence: &ev, Actor: systemActor, Reason: "mint finalized on chain and posted",
	}); err != nil {
		return MintRecord{}, err
	}
	slot := int64(obs.Status.Slot) // #nosec G115 -- a devnet slot counts blocks and stays far below 2^63
	out, err := s.transitionMint(ctx, tx, rec, MintIssued, systemActor, "finalized on chain and posted", "", &res.TransactionID, &slot)
	if err != nil {
		return MintRecord{}, err
	}
	if err := s.finishIntent(ctx, tx, rec.IntentID, true, "mint issued"); err != nil {
		return MintRecord{}, err
	}
	// Reserve and supply after the posting, under the same lock: a shortfall
	// or a difference with the chain pauses the asset in this transaction.
	if _, err := s.reconcileLocked(ctx, tx, &a, reconciliation.ModeEventDriven, systemActor); err != nil {
		return MintRecord{}, err
	}
	return out, nil
}

// checkPlanBacking re-derives the reserve the issuance needed at the price
// and factor its plan recorded, and refuses to post if the request carries
// less: the plan is the contract the customer's payment was taken under.
func checkPlanBacking(a PlatformStableAsset, rec MintRecord) error {
	reserve, ok := a.Reserve(rec.ReserveAssetID)
	if !ok {
		return errs.New(errs.CodeInternal, "stableasset: the mint's reserve asset left the policy")
	}
	mantissa, err := money.ParseQuantity(rec.Plan.ReservePrice.Mantissa)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "stableasset: the plan's reserve price does not parse")
	}
	factor, err := strconv.ParseInt(rec.Plan.ReserveFactor, 10, 64)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "stableasset: the plan's reserve factor does not parse")
	}
	price := money.Price{Mantissa: mantissa, Scale: rec.Plan.ReservePrice.Scale, QuoteAsset: rec.Plan.ReservePrice.Quote,
		Source: rec.Plan.ReservePrice.Source, At: rec.Plan.ReservePrice.At}
	need, err := requiredReserve(rec.Quantity, a.Decimals, a.Peg, reserve.Decimals, price, money.BPS(factor))
	if err != nil {
		return err
	}
	if need.Cmp(rec.ReserveQuantity) > 0 {
		return errs.New(errs.CodeInternal, "stableasset: the mint request carries less reserve than its plan requires").
			WithField("request_id", rec.ID.String())
	}
	return nil
}

// failMint records a mint that did not and will not happen: the effect
// fails, the reserve payment's reservation is released, the request and the
// intent fail. Nothing is posted.
func (s *Service) failMint(ctx context.Context, tx pgx.Tx, _ PlatformStableAsset, rec MintRecord, code, reason string) (MintRecord, error) {
	if _, err := s.effects.Fail(ctx, tx, rec.EffectID, code, systemActor, reason); err != nil {
		return MintRecord{}, err
	}
	if _, err := s.capital.Release(ctx, tx, rec.ReservationID, reason); err != nil {
		return MintRecord{}, err
	}
	out, err := s.transitionMint(ctx, tx, rec, MintFailed, systemActor, reason, code, nil, nil)
	if err != nil {
		return MintRecord{}, err
	}
	return out, s.finishIntent(ctx, tx, rec.IntentID, false, reason)
}

// finishIntent ends an executing (or unknown) intent: COMPLETED, RECONCILED
// when an unknown outcome was resolved, or FAILED.
func (s *Service) finishIntent(ctx context.Context, tx pgx.Tx, iid econ.IntentID, success bool, reason string) error {
	in, err := s.intents.Get(ctx, tx, iid)
	if err != nil {
		return err
	}
	to := econ.StatusFailed
	switch {
	case success && in.Status == econ.StatusUnknown:
		to = econ.StatusReconciled
	case success:
		to = econ.StatusCompleted
	}
	if in.Status == to || in.Status.Terminal() {
		return nil
	}
	_, err = s.intents.Transition(ctx, tx, iid, to, systemActor, reason, "")
	return err
}

// stamp is used where an insert must carry the service clock.
func stamp(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }
