package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

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
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Refusal reason codes (the intent's rejection_code and the error's
// "reason" field).
const (
	RefusalRouteNotActive    = "BRIDGE_ROUTE_NOT_ACTIVE"
	RefusalRouteDenied       = "BRIDGE_ROUTE_DENIED"
	RefusalAccountIneligible = "BRIDGE_ACCOUNT_INELIGIBLE"
	RefusalChainBalance      = "BRIDGE_CHAIN_BALANCE_INSUFFICIENT"
	RefusalInsufficientFunds = "BRIDGE_INSUFFICIENT_FUNDS"
	RefusalRiskRejected      = "RISK_REJECTED"
)

func refusalCode(reason string) errs.Code {
	switch reason {
	case RefusalRouteNotActive:
		return errs.CodeAssetRestricted
	case RefusalRouteDenied:
		return errs.CodeForbidden
	case RefusalAccountIneligible:
		return errs.CodeAccountFrozen
	case RefusalChainBalance, RefusalInsufficientFunds:
		return errs.CodeInsufficientBuyingPower
	case RefusalRiskRejected:
		return errs.CodeRiskRejected
	}
	return errs.CodeConflict
}

type refusal struct {
	code string
	err  error
}

func refuse(reason, detail string) refusal {
	return refusal{code: reason, err: errs.New(refusalCode(reason), "bridge: "+detail).WithField("reason", reason)}
}

// AccountFacts are what the HTTP layer resolved about the account.
type AccountFacts struct {
	Jurisdiction     string
	Verification     valuedomain.VerificationLevel
	IdentityVerified bool
}

// BridgeRequest asks to move Quantity base units of a route's source asset
// from the account's sandbox wallet on the source chain to its wallet on the
// destination chain.
type BridgeRequest struct {
	RouteID        RouteID
	AccountID      accounts.AccountID
	Quantity       money.Quantity
	IdempotencyKey string
	// Mode is SANDBOX (the service produces blocks and drives the leg) or
	// DEVNET (blocks come from the chains' own producer).
	Mode          gates.Mode
	Actor         econ.Actor
	Facts         AccountFacts
	CorrelationID string
}

func (r BridgeRequest) validate() error {
	var problems []string
	if r.RouteID.IsZero() || r.AccountID.IsZero() {
		problems = append(problems, "route and account are required")
	}
	if !r.Quantity.IsPositive() {
		problems = append(problems, "quantity must be positive")
	} else if _, err := devnet.AmountOf(r.Quantity); err != nil {
		problems = append(problems, "quantity must fit a devnet amount (u64)")
	}
	if k := r.IdempotencyKey; strings.TrimSpace(k) != k || k == "" || len(k) > 200 {
		problems = append(problems, "idempotency key must be 1..200 bytes without surrounding whitespace")
	}
	if r.Mode != gates.ModeSandbox && r.Mode != gates.ModeDevnet {
		problems = append(problems, "mode must be SANDBOX or DEVNET")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "bridge: invalid request").WithField("problems", problems)
	}
	return nil
}

// LegTicket is what PrepareLeg recorded; a Refusal is already recorded in
// the transaction (intent REJECTED, decision persisted) and the caller
// commits it.
type LegTicket struct {
	Leg     Leg
	Replay  bool
	Refusal error
}

// LegResult is a leg's state when a workflow call returns.
type LegResult struct {
	Leg     Leg
	Replay  bool
	Settled bool
}

// deriveNonce gives each leg its own lock nonce from its id.
func deriveNonce(lid LegID) uint64 {
	sum := sha256.Sum256([]byte("nodal-sandbox/bridge/nonce/v1\x00" + lid.String()))
	return binary.BigEndian.Uint64(sum[:8])
}

type legTerms struct {
	RouteID string `json:"route_id"`
}

type legEffectRequest struct {
	LegID     string `json:"leg_id"`
	RouteID   string `json:"route_id"`
	AccountID string `json:"account_id"`
	Quantity  string `json:"quantity"`
	Phase     string `json:"phase"`
}

// Bridge runs a leg: admission, reservation, the plan and the holder's
// signed lock in one transaction; the lock submitted exactly once between
// transactions; the outcome recorded; then (SANDBOX) blocks produced and the
// leg driven through source finality, relay, destination finality and
// reconciliation. An unknown lock submission returns SUBMISSION_STATE_UNKNOWN
// and is reconciled by tx id, never resubmitted.
func (s *Service) Bridge(ctx context.Context, req BridgeRequest) (LegResult, error) {
	if err := req.validate(); err != nil {
		return LegResult{}, err
	}
	r, err := s.GetRoute(ctx, s.db, req.RouteID)
	if err != nil {
		return LegResult{}, err
	}
	holder := s.holder.HolderAddress(req.AccountID)
	if err := s.host.EnsureNativeBalance(ctx, r.SourceChain, holder, s.params.GasMinimum, s.params.GasTopUp); err != nil {
		return LegResult{}, err
	}
	quote, err := s.provider.Quote(ctx, r, req.Quantity, s.clk.Now())
	if err != nil {
		return LegResult{}, err
	}
	var ticket LegTicket
	if err := s.db.InTx(ctx, txOptions, func(ctx context.Context, tx pgx.Tx) error {
		t, err := s.PrepareLeg(ctx, tx, req, quote)
		ticket = t
		return err
	}); err != nil {
		return LegResult{}, err
	}
	if ticket.Refusal != nil {
		return LegResult{Replay: ticket.Replay}, ticket.Refusal
	}
	if ticket.Replay {
		// The same request again: drive the leg it made (never resubmitting).
		out, err := s.drive(ctx, ticket.Leg.ID, ticket.Leg.Mode == gates.ModeSandbox)
		out.Replay = true
		return out, err
	}
	exec := s.provider.Execute(ctx, r.SourceChain, ticket.Leg.LockTx)
	leg, err := s.recordLockSubmission(ctx, ticket.Leg.ID, exec)
	if err != nil {
		return LegResult{Leg: leg}, err
	}
	if leg.Status == StateFailed {
		return LegResult{Leg: leg, Settled: true}, nil
	}
	if exec.Outcome == ExecUnknown {
		return LegResult{Leg: leg}, submissionUnknown(leg.LockTxID, exec.Err)
	}
	return s.drive(ctx, leg.ID, req.Mode == gates.ModeSandbox)
}

func submissionUnknown(txID devnet.TxID, cause error) error {
	e := errs.New(errs.CodeSubmissionStateUnknown,
		"bridge: the submission's outcome is unknown; it will be reconciled by transaction id, never resubmitted").
		WithField("tx_id", string(txID))
	if cause != nil {
		e = e.WithField("cause", cause.Error())
	}
	return e
}

// PrepareLeg is a leg's first transaction: authenticate, authorize, lock the
// route, record the intent (idempotent), route it, check eligibility, the
// holder's balance on the source chain (net of its own pending locks) and
// Risk V2, reserve the units, claim the source effect, plan the lock, have
// the holder's custodian sign it after inspection, and record the leg in
// BRIDGE_REQUESTED with the signed lock. Nothing is submitted.
func (s *Service) PrepareLeg(ctx context.Context, tx pgx.Tx, req BridgeRequest, quote BridgeQuote) (LegTicket, error) {
	if err := req.validate(); err != nil {
		return LegTicket{}, err
	}
	if err := s.authenticate(ctx, req.Actor, req.AccountID); err != nil {
		return LegTicket{}, err
	}
	if err := immediateBindings(ctx, tx); err != nil {
		return LegTicket{}, err
	}
	if err := s.authorizer.Require(ctx, tx, gates.CrossChainBuyingPower, req.Mode); err != nil {
		return LegTicket{}, err
	}
	r, err := s.getRoute(ctx, tx, req.RouteID, true)
	if err != nil {
		return LegTicket{}, err
	}
	now := s.clk.Now().UTC()
	if quote.RouteID != r.ID || !quote.AmountIn.Equal(req.Quantity) || now.After(quote.ExpiresAt) {
		return LegTicket{}, errs.New(errs.CodeQuoteExpired, "bridge: the quote is expired or does not describe this request")
	}
	holder := s.holder.HolderAddress(req.AccountID)
	// The holder's units on the source chain, net of this account's own locks
	// that are signed but not yet in the chain state (a landed lock already
	// left the wallet). Read under the route's row lock, which serializes the
	// route's admissions, through this transaction.
	pending, err := listLegs(ctx, tx, `route_id = $1 AND account_id = $2 AND status = 'BRIDGE_REQUESTED' ORDER BY id`, r.ID, req.AccountID)
	if err != nil {
		return LegTicket{}, err
	}
	var spendable money.Quantity
	chainErr := s.host.ViewIn(ctx, tx, func(v devnethost.View) error {
		if _, err := v.Tip(r.DestinationChain); err != nil {
			return err
		}
		held, err := v.TokenBalance(r.SourceChain, holder, r.SourceMint)
		if err != nil {
			return err
		}
		spendable = devnet.QuantityOf(held)
		for _, l := range pending {
			st, err := v.Status(r.SourceChain, l.LockTxID)
			if err != nil {
				return err
			}
			if !landed(st) {
				spendable = spendable.Sub(l.Quantity)
			}
		}
		return nil
	})
	terms, err := json.Marshal(legTerms{RouteID: r.ID.String()})
	if err != nil {
		return LegTicket{}, errs.Wrap(err, errs.CodeInternal, "bridge: encode terms")
	}
	q := req.Quantity
	in, existing, err := s.intents.Submit(ctx, tx, econ.Intent{
		AccountID: req.AccountID, Kind: econ.KindBridgeValue, Mode: req.Mode, Capability: gates.CrossChainBuyingPower,
		ActorType: req.Actor.Type, ActorID: req.Actor.ID, AssetID: &r.SourceAssetID, Quantity: &q, Terms: terms,
		IdempotencyKey: req.IdempotencyKey, CorrelationID: req.CorrelationID,
	})
	if err != nil {
		return LegTicket{}, err
	}
	if existing {
		return s.replayLeg(ctx, tx, in)
	}
	refused := func(rf refusal) (LegTicket, error) {
		if _, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusRejected, req.Actor, rf.err.Error(), rf.code); err != nil {
			return LegTicket{}, err
		}
		return LegTicket{Refusal: rf.err}, nil
	}

	rt, err := econ.Route(econ.RouteRequest{
		Intent:       in,
		Subject:      settlement.Subject{Type: settlement.SubjectFacility, ID: r.FacilityID.String(), AssetID: r.SourceAssetID.String()},
		Jurisdiction: req.Facts.Jurisdiction, Verification: req.Facts.Verification,
	}, s.router, now)
	if err != nil {
		return LegTicket{}, err
	}
	if !rt.MayExecuteNow() {
		rf := refuse(RefusalRouteDenied, "the settlement compiler refused the intent")
		if e, ok := errs.As(rf.err); ok {
			rf.err = e.WithField("route_reasons", rt.Reasons)
		}
		return refused(rf)
	}
	if _, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusRouted, req.Actor, "routed through the settlement compiler", ""); err != nil {
		return LegTicket{}, err
	}
	acct, err := s.accounts.Get(ctx, tx, req.AccountID)
	if err != nil {
		return LegTicket{}, err
	}
	if acct.Kind != accounts.KindCustomer || !acct.Status.AllowsNewRisk() {
		return refused(refuse(RefusalAccountIneligible, "the account may not bridge"))
	}
	if r.Status != RouteActive {
		return refused(refuse(RefusalRouteNotActive, "the route is "+string(r.Status)))
	}
	if chainErr == nil && spendable.Cmp(q) < 0 {
		return refused(refuse(RefusalChainBalance, "the holder's wallet on the source chain does not hold this many units"))
	}
	avail, err := s.capital.Availability(ctx, tx, req.AccountID.String(), r.SourceAssetID, now)
	if err != nil {
		return LegTicket{}, err
	}
	if rf, ok, err := s.evaluateRisk(ctx, tx, in, r, acct, req.Facts, q, avail.Available, chainErr == nil); err != nil {
		return LegTicket{}, err
	} else if !ok {
		return refused(rf)
	}
	if _, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusRiskApproved, req.Actor, "risk kernel allowed the bridge", ""); err != nil {
		return LegTicket{}, err
	}
	res, err := s.capital.Reserve(ctx, tx, capital.ReserveRequest{
		AccountID: req.AccountID.String(), AssetID: r.SourceAssetID, Quantity: q, ActorType: req.Actor.Type, ActorID: req.Actor.ID,
		IdempotencyKey: "bridge:lock:" + in.ID.String(), TTL: s.params.ReservationTTL, Reason: "bridge: units to lock",
	})
	if err != nil {
		switch errs.CodeOf(err) {
		case errs.CodeInsufficientBuyingPower, errs.CodeAccountFrozen, errs.CodeAssetRestricted:
			return refused(refusal{code: RefusalInsufficientFunds, err: err})
		}
		return LegTicket{}, err
	}
	if _, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusReserved, req.Actor, "units reserved", ""); err != nil {
		return LegTicket{}, err
	}

	lid := id.New[legKind]()
	leg := Leg{ID: lid, RouteID: r.ID, AccountID: req.AccountID, IntentID: in.ID, Mode: req.Mode, Quantity: q,
		SourceOwner: holder, DestinationOwner: holder, Nonce: deriveNonce(lid), Status: StateAvailable}
	prepared, err := s.provider.Plan(ctx, tx, PlanRequest{Kind: PlanLock, Route: r, Leg: leg.ref(), Quote: quote})
	if err != nil {
		return LegTicket{}, err
	}
	leg.Plan, leg.PlanHash = *prepared.Plan, prepared.PlanHash
	if err := insertLeg(ctx, tx, leg, req.Actor, now); err != nil {
		return LegTicket{}, err
	}
	effect, replayed, err := s.effects.Begin(ctx, tx, econ.BeginRequest{
		Key: "bridge:source:" + lid.String(), Subsystem: econ.SubsystemBridge, Kind: effectKindSource, IntentID: &in.ID,
		AccountID: req.AccountID.String(), PostsLedger: true, Actor: req.Actor, CorrelationID: req.CorrelationID,
		Request: legEffectRequest{LegID: lid.String(), RouteID: r.ID.String(), AccountID: req.AccountID.String(), Quantity: q.String(), Phase: "SOURCE"},
	})
	if err != nil {
		return LegTicket{}, err
	}
	if replayed {
		return LegTicket{}, errs.New(errs.CodeInternal, "bridge: a fresh leg found its source effect already claimed")
	}
	leg, err = s.transitionLeg(ctx, tx, leg, StateReserved, req.Actor, "units reserved in WALLET(source)",
		legSet{reservation: &res.ID, sourceEffect: &effect.ID})
	if err != nil {
		return LegTicket{}, err
	}
	txID, raw, err := s.holder.SignHolderLock(req.AccountID, prepared.PlanHash, prepared.Lock, prepared.Message)
	if err != nil {
		return LegTicket{}, err
	}
	bh, height := prepared.RecentBlockhash, prepared.BlockhashHeight
	leg, err = s.transitionLeg(ctx, tx, leg, StateBridgeRequested, req.Actor, "lock signed by the holder; submitting",
		legSet{lockTxID: txID, lockTx: raw, lockBlockhash: &bh, lockBlockhashHeight: &height})
	if err != nil {
		return LegTicket{}, err
	}
	if err := s.capital.LockForOrder(ctx, tx, res.ID, lid.String()); err != nil {
		return LegTicket{}, err
	}
	if _, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusExecuting, req.Actor, "bridge lock signed; submitting", ""); err != nil {
		return LegTicket{}, err
	}
	return LegTicket{Leg: leg}, nil
}

func (s *Service) replayLeg(ctx context.Context, tx pgx.Tx, in econ.Intent) (LegTicket, error) {
	leg, found, err := legByIntent(ctx, tx, in.ID)
	if err != nil {
		return LegTicket{}, err
	}
	if found {
		return LegTicket{Leg: leg, Replay: true}, nil
	}
	if in.Status == econ.StatusRejected {
		return LegTicket{Replay: true, Refusal: errs.New(refusalCode(in.RejectionCode), "bridge: this request was refused").
			WithField("reason", in.RejectionCode)}, nil
	}
	return LegTicket{}, errs.New(errs.CodeInternal, "bridge: an intent without its leg").WithField("intent_id", in.ID.String())
}

// evaluateRisk runs Risk V2 with the sections BRIDGE_VALUE requires
// (account, identity, balance, chain), valued at the route's unit price.
func (s *Service) evaluateRisk(ctx context.Context, tx pgx.Tx, in econ.Intent, r Route, acct accounts.Account, facts AccountFacts,
	q, available money.Quantity, chainHealthy bool) (refusal, bool, error) {
	// The notional (for limits) rounds up. The balance section compares the
	// two quantities' values rounded the same way, so the comparison keeps
	// the order of the units; the exact unit check is the capital
	// reservation that follows.
	notional, err := usd(q, r, money.RoundUp)
	if err != nil {
		return refusal{}, false, err
	}
	required, err := usd(q, r, money.RoundDown)
	if err != nil {
		return refusal{}, false, err
	}
	avail, err := usd(available, r, money.RoundDown)
	if err != nil {
		return refusal{}, false, err
	}
	snap, err := s.kills.Snapshot(ctx, tx)
	if err != nil {
		return refusal{}, false, err
	}
	kills := make([]risk.KillSwitch, 0, len(snap.Switches))
	for _, sw := range snap.Switches {
		kills = append(kills, risk.KillSwitch{Kind: string(sw.Kind), Scope: sw.ScopeID})
	}
	breakers, err := s.openBreakers(ctx, tx, r)
	if err != nil {
		return refusal{}, false, err
	}
	input := risk.AdvancedInput{
		Stage: risk.StagePreTrade,
		Intent: risk.AdvancedIntent{ID: in.ID.String(), Kind: string(in.Kind), AccountID: in.AccountID.String(),
			ActorType: string(in.ActorType), Capability: string(in.Capability), Mode: string(in.Mode), NotionalUSD: notional,
			RiskIncreasing: true},
		Identity:        &risk.IdentitySnapshot{Verified: facts.IdentityVerified},
		Account:         &risk.AdvancedAccountSnapshot{Status: string(acct.Status)},
		Balance:         &risk.BalanceSnapshot{Available: avail, Required: required},
		Chain:           &risk.ChainSnapshot{Healthy: chainHealthy},
		CircuitBreakers: breakers,
		KillSwitches:    kills,
		Now:             s.clk.Now().UTC(),
	}
	decision := risk.EvaluateAdvanced(s.policy, input)
	if _, err := s.risk.RecordAdvancedDecision(ctx, tx, input, decision, in.CorrelationID); err != nil {
		return refusal{}, false, err
	}
	if rerr := decision.RejectionError(); rerr != nil {
		return refusal{code: RefusalRiskRejected, err: rerr}, false, nil
	}
	return refusal{}, true, nil
}

// usd values q base units at the route's unit price.
func usd(q money.Quantity, r Route, mode money.RoundingMode) (money.USD, error) {
	cents, err := money.Notional(q, r.Decimals, r.UnitPrice, 2, mode)
	if err != nil {
		return money.USD{}, err
	}
	n, err := cents.Int64()
	if err != nil {
		return money.USD{}, errs.New(errs.CodeOverflow, "bridge: value exceeds the USD range")
	}
	return money.USDFromMinor(n), nil
}

func (s *Service) openBreakers(ctx context.Context, tx pgx.Tx, r Route) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT id::text FROM reconciliation_records
		WHERE kind = $1 AND scope_type = $2 AND scope_id = $3 AND blocks_new_risk
		  AND status IN ('OPEN','MISMATCH','INVESTIGATING','ESCALATED') ORDER BY id`,
		string(reconciliation.KindBridge), ScopeBridgeRoute, r.ID.String())
	if err != nil {
		return nil, dbErr("read circuit breakers", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var rid string
		if err := rows.Scan(&rid); err != nil {
			return nil, dbErr("scan circuit breaker", err)
		}
		out = append(out, "BRIDGE_MISMATCH:"+rid)
	}
	return out, rows.Err()
}

// ScopeBridgeRoute is the reconciliation scope type of a route.
const ScopeBridgeRoute = "bridge_route"

func externalRef(chain devnet.ChainID, txID devnet.TxID) string {
	return "devnet:" + string(chain) + ":" + string(txID)
}

// recordLockSubmission is the second transaction: what the lock submission
// did.
func (s *Service) recordLockSubmission(ctx context.Context, lid LegID, exec BridgeExecution) (Leg, error) {
	var out Leg
	err := s.db.InTx(ctx, txOptions, func(ctx context.Context, tx pgx.Tx) error {
		leg, err := getLeg(ctx, tx, lid, true)
		if err != nil {
			return err
		}
		out = leg
		if leg.Status != StateBridgeRequested {
			return nil
		}
		switch exec.Outcome {
		case ExecSubmitted:
			return nil
		case ExecUnknown:
			reason := "the lock submission's outcome is unknown"
			if exec.Err != nil {
				reason += ": " + exec.Err.Error()
			}
			if _, err := s.effects.MarkUnknown(ctx, tx, *leg.SourceEffectID, externalRef(exec.Chain, leg.LockTxID), systemActor, reason); err != nil {
				return err
			}
			return s.intentUnknown(ctx, tx, leg.IntentID, reason)
		default:
			out, err = s.failSource(ctx, tx, leg, "SUBMISSION_REFUSED:"+string(devnet.ReasonOf(exec.Err)), "the chain refused the lock")
			return err
		}
	})
	return out, err
}

func (s *Service) intentUnknown(ctx context.Context, tx pgx.Tx, iid econ.IntentID, reason string) error {
	in, err := s.intents.Get(ctx, tx, iid)
	if err != nil || in.Status != econ.StatusExecuting {
		return err
	}
	_, err = s.intents.Transition(ctx, tx, iid, econ.StatusUnknown, systemActor, reason, "")
	return err
}

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

// failSource ends a leg whose lock never finalized: the source effect fails,
// the reservation is released, nothing is posted.
func (s *Service) failSource(ctx context.Context, tx pgx.Tx, leg Leg, code, reason string) (Leg, error) {
	if leg.SourceEffectID != nil {
		if _, err := s.effects.Fail(ctx, tx, *leg.SourceEffectID, code, systemActor, reason); err != nil {
			return Leg{}, err
		}
	}
	if leg.ReservationID != nil {
		if _, err := s.capital.Release(ctx, tx, *leg.ReservationID, reason); err != nil {
			return Leg{}, err
		}
	}
	out, err := s.transitionLeg(ctx, tx, leg, StateFailed, systemActor, reason, legSet{failureCode: code})
	if err != nil {
		return Leg{}, err
	}
	return out, s.finishIntent(ctx, tx, leg.IntentID, false, reason)
}

type legEvidence struct {
	Disclaimer           string            `json:"disclaimer"`
	LegID                string            `json:"leg_id"`
	Phase                string            `json:"phase"`
	Plan                 BridgePlan        `json:"plan"`
	PlanHash             string            `json:"plan_hash"`
	Observation          BridgeObservation `json:"observation"`
	Release              *releaseEvidence  `json:"release,omitempty"`
	JournalTransactionID string            `json:"journal_transaction_id"`
}

// releaseEvidence is the release a destination posting records; every
// amount and nonce is a string, as canonical evidence requires.
type releaseEvidence struct {
	Chain       string `json:"chain"`
	Mint        string `json:"mint"`
	ToOwner     string `json:"to_owner"`
	Amount      string `json:"amount"`
	SourceChain string `json:"source_chain"`
	SourceTxID  string `json:"source_tx_id"`
	Nonce       string `json:"nonce"`
	TxID        string `json:"tx_id"`
}

const evidenceDisclaimer = "simulated sandbox value on simulated chains; not money"

// SettleSource acts on the source chain's final word for a leg's lock. It
// observes the lock itself, through the transaction: FINALIZED posts
// BRIDGE_SOURCE_FINALIZED (the units move to BRIDGE_IN_FLIGHT); FAILED
// (final) or NOT_FOUND expired below finality fails the leg and releases the
// reservation; anything else -- pending, confirmed, or confirmed and then
// reorged away before finality -- leaves it BRIDGE_REQUESTED.
func (s *Service) SettleSource(ctx context.Context, tx pgx.Tx, lid LegID) (Leg, error) {
	leg, err := getLeg(ctx, tx, lid, true)
	if err != nil || leg.Status != StateBridgeRequested {
		return leg, err
	}
	r, err := s.lockRoute(ctx, tx, leg.RouteID)
	if err != nil {
		return Leg{}, err
	}
	obs, err := s.provider.Observe(ctx, tx, r.SourceChain, leg.LockTxID, leg.LockBlockhash, leg.LockBlockhashHeight)
	if err != nil {
		return Leg{}, err
	}
	switch {
	case obs.Finalized():
		now := s.clk.Now().UTC()
		res, err := s.ledger.Post(ctx, tx, sourceFinalizedPosting(r, leg, now))
		if err != nil {
			return Leg{}, err
		}
		if _, err := s.capital.Consume(ctx, tx, *leg.ReservationID, leg.Quantity, 0, leg.ID.String()); err != nil {
			return Leg{}, err
		}
		ev, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "bridge/source/" + leg.ID.String(), Document: legEvidence{
			Disclaimer: evidenceDisclaimer, LegID: leg.ID.String(), Phase: "SOURCE_FINALIZED", Plan: leg.Plan,
			PlanHash: hex.EncodeToString(leg.PlanHash[:]), Observation: obs, JournalTransactionID: res.TransactionID.String(),
		}})
		if err != nil {
			return Leg{}, err
		}
		if _, err := s.effects.Apply(ctx, tx, *leg.SourceEffectID, econ.ApplyRequest{
			JournalTransactionID: &res.TransactionID, ReservationID: leg.ReservationID.String(),
			ExternalRef: externalRef(r.SourceChain, leg.LockTxID), Evidence: &ev, Actor: systemActor, Reason: "lock finalized on the source chain",
		}); err != nil {
			return Leg{}, err
		}
		return s.transitionLeg(ctx, tx, leg, StateSourceFinalized, systemActor, "lock finalized; units in flight",
			legSet{sourceJournal: &res.TransactionID})
	case obs.FailedFinal():
		return s.failSource(ctx, tx, leg, "CHAIN_FAILED:"+string(obs.Status.Err), "the lock failed on the source chain")
	case obs.Expired:
		return s.failSource(ctx, tx, leg, "SUBMISSION_EXPIRED",
			"the source chain does not hold the lock and its blockhash expired below finality; it can never land")
	}
	return leg, nil
}

// PrepareRelease relays a source-finalized leg: if no earlier release
// attempt can still land, it claims the destination effect (once), has the
// provider build the release from the FINALIZED lock and its attester sign
// it, and records the attempt before anything is submitted. A release the
// destination refused as already released, or running out of attempts,
// sends the leg to manual review. It returns nil when there is nothing to
// relay now.
func (s *Service) PrepareRelease(ctx context.Context, tx pgx.Tx, lid LegID) (*Attempt, error) {
	leg, err := getLeg(ctx, tx, lid, true)
	if err != nil {
		return nil, err
	}
	if leg.Status != StateSourceFinalized && leg.Status != StateMessageInFlight {
		return nil, nil
	}
	r, err := s.lockRoute(ctx, tx, leg.RouteID)
	if err != nil {
		return nil, err
	}
	atts, err := attemptsOf(ctx, tx, lid, AttemptRelease)
	if err != nil {
		return nil, err
	}
	for _, a := range atts {
		obs, err := s.provider.Observe(ctx, tx, a.Chain, a.TxID, a.RecentBlockhash, a.BlockhashHeight)
		if err != nil {
			return nil, err
		}
		if obs.FailedFinal() && obs.Status.Err == devnet.CodeBridgeAlreadyReleased {
			_, err := s.transitionLeg(ctx, tx, leg, StateManualReview, systemActor,
				"the destination says this lock was already released by another transaction", legSet{})
			return nil, err
		}
		if !obs.Dead() {
			return nil, nil // a release that can still land: wait for it, never race it
		}
	}
	round, err := s.attemptsThisRound(ctx, tx, lid)
	if err != nil {
		return nil, err
	}
	if round >= r.MaxReleaseAttempts {
		_, err := s.transitionLeg(ctx, tx, leg, StateManualReview, systemActor, "release attempts exhausted", legSet{})
		return nil, err
	}
	set := legSet{}
	if leg.DestinationEffectID == nil {
		effect, _, err := s.effects.Begin(ctx, tx, econ.BeginRequest{
			Key: "bridge:destination:" + lid.String(), Subsystem: econ.SubsystemBridge, Kind: effectKindDestination,
			IntentID: &leg.IntentID, AccountID: leg.AccountID.String(), PostsLedger: true, Actor: systemActor,
			Request: legEffectRequest{LegID: lid.String(), RouteID: r.ID.String(), AccountID: leg.AccountID.String(),
				Quantity: leg.Quantity.String(), Phase: "DESTINATION"},
		})
		if err != nil {
			return nil, err
		}
		set.destinationEffect = &effect.ID
	}
	prepared, err := s.provider.Plan(ctx, tx, PlanRequest{Kind: PlanRelease, Route: r, Leg: leg.ref()})
	if err != nil {
		return nil, err
	}
	all, err := countAllAttempts(ctx, tx, lid)
	if err != nil {
		return nil, err
	}
	a := Attempt{LegID: lid, Number: all + 1, Kind: AttemptRelease, Chain: prepared.Chain, TxID: prepared.TxID, Raw: prepared.Raw,
		RecentBlockhash: prepared.RecentBlockhash, BlockhashHeight: prepared.BlockhashHeight}
	if err := insertAttempt(ctx, tx, a); err != nil {
		return nil, err
	}
	if leg.Status == StateSourceFinalized {
		if _, err := s.transitionLeg(ctx, tx, leg, StateMessageInFlight, systemActor, "release signed; relaying", set); err != nil {
			return nil, err
		}
	} else if set.destinationEffect != nil {
		if err := s.updateLeg(ctx, tx, leg, "", set); err != nil {
			return nil, err
		}
	}
	return &a, nil
}

// attemptsThisRound counts the release attempts made since the leg last left
// manual review (an operator's resume grants a fresh round).
func (s *Service) attemptsThisRound(ctx context.Context, tx pgx.Tx, lid LegID) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM bridge_release_attempts a
		WHERE a.leg_id = $1 AND a.kind = 'RELEASE' AND a.created_at > coalesce((
			SELECT max(occurred_at) FROM bridge_leg_transitions
			 WHERE leg_id = $1 AND from_status = 'MANUAL_REVIEW' AND to_status = 'MESSAGE_IN_FLIGHT'), '-infinity')`, lid).Scan(&n)
	if err != nil {
		return 0, dbErr("count release attempts", err)
	}
	return n, nil
}

// recordReleaseSubmission records an ambiguous release submission: the
// destination effect becomes UNKNOWN. A definite outcome needs nothing: the
// attempt row already says what was sent, and the chain says what happened.
func (s *Service) recordReleaseSubmission(ctx context.Context, lid LegID, a Attempt, exec BridgeExecution) error {
	if exec.Outcome != ExecUnknown {
		return nil
	}
	return s.db.InTx(ctx, txOptions, func(ctx context.Context, tx pgx.Tx) error {
		leg, err := getLeg(ctx, tx, lid, true)
		if err != nil || leg.DestinationEffectID == nil {
			return err
		}
		effect, err := s.effects.Get(ctx, tx, *leg.DestinationEffectID)
		if err != nil || effect.Status != econ.EffectPending {
			return err
		}
		reason := "the release submission's outcome is unknown"
		if exec.Err != nil {
			reason += ": " + exec.Err.Error()
		}
		if _, err := s.effects.MarkUnknown(ctx, tx, effect.ID, externalRef(a.Chain, a.TxID), systemActor, reason); err != nil {
			return err
		}
		return s.intentUnknown(ctx, tx, leg.IntentID, reason)
	})
}

// SettleDestination acts on the destination chain's word for the leg's
// latest release: FINALIZED posts BRIDGE_DESTINATION_FINALIZED; CONFIRMED
// marks DESTINATION_OBSERVED; a release that left the canonical chain before
// finality (a reorg) or died returns the leg to MESSAGE_IN_FLIGHT, from which
// a new attempt is relayed only once nothing can still land.
func (s *Service) SettleDestination(ctx context.Context, tx pgx.Tx, lid LegID) (Leg, error) {
	leg, err := getLeg(ctx, tx, lid, true)
	if err != nil || (leg.Status != StateMessageInFlight && leg.Status != StateDestinationObserved) {
		return leg, err
	}
	r, err := s.lockRoute(ctx, tx, leg.RouteID)
	if err != nil {
		return Leg{}, err
	}
	atts, err := attemptsOf(ctx, tx, lid, AttemptRelease)
	if err != nil || len(atts) == 0 {
		return leg, err
	}
	a := atts[len(atts)-1]
	obs, err := s.provider.Observe(ctx, tx, a.Chain, a.TxID, a.RecentBlockhash, a.BlockhashHeight)
	if err != nil {
		return Leg{}, err
	}
	switch {
	case obs.Finalized():
		now := s.clk.Now().UTC()
		res, err := s.ledger.Post(ctx, tx, destinationFinalizedPosting(r, leg, string(a.TxID), now))
		if err != nil {
			return Leg{}, err
		}
		release := releaseEvidence{Chain: string(r.DestinationChain), Mint: r.DestinationMint.String(),
			ToOwner: leg.DestinationOwner.String(), Amount: leg.Quantity.String(), SourceChain: string(r.SourceChain),
			SourceTxID: string(leg.LockTxID), Nonce: strconv.FormatUint(leg.Nonce, 10), TxID: string(a.TxID)}
		ev, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "bridge/destination/" + leg.ID.String(), Document: legEvidence{
			Disclaimer: evidenceDisclaimer, LegID: leg.ID.String(), Phase: "DESTINATION_FINALIZED", Plan: leg.Plan,
			PlanHash: hex.EncodeToString(leg.PlanHash[:]), Observation: obs, Release: &release, JournalTransactionID: res.TransactionID.String(),
		}})
		if err != nil {
			return Leg{}, err
		}
		if _, err := s.effects.Apply(ctx, tx, *leg.DestinationEffectID, econ.ApplyRequest{
			JournalTransactionID: &res.TransactionID, ExternalRef: externalRef(a.Chain, a.TxID), Evidence: &ev,
			Actor: systemActor, Reason: "release finalized on the destination chain",
		}); err != nil {
			return Leg{}, err
		}
		out, err := s.transitionLeg(ctx, tx, leg, StateDestinationFinalized, systemActor, "release finalized; value on the destination",
			legSet{destinationJournal: &res.TransactionID})
		if err != nil {
			return Leg{}, err
		}
		return out, s.finishIntent(ctx, tx, leg.IntentID, true, "bridged")
	case obs.Landed():
		if leg.Status == StateMessageInFlight {
			return s.transitionLeg(ctx, tx, leg, StateDestinationObserved, systemActor, "release confirmed on the destination; awaiting finality", legSet{})
		}
	default:
		if leg.Status == StateDestinationObserved {
			return s.transitionLeg(ctx, tx, leg, StateMessageInFlight, systemActor,
				"the observed release left the canonical chain before finality", legSet{})
		}
	}
	return leg, nil
}

// ReconcileLeg checks a destination-finalized leg against both chains: the
// lock on the source is FINALIZED and is exactly the leg's, exactly one
// release of it is FINALIZED on the destination and it is exactly the
// release the leg expects, and both postings exist. A match is RECONCILED;
// anything else goes to manual review. The route reconciliation runs as
// well.
func (s *Service) ReconcileLeg(ctx context.Context, tx pgx.Tx, lid LegID) (Leg, error) {
	leg, err := getLeg(ctx, tx, lid, true)
	if err != nil || leg.Status != StateDestinationFinalized {
		return leg, err
	}
	r, err := s.lockRoute(ctx, tx, leg.RouteID)
	if err != nil {
		return Leg{}, err
	}
	problem, err := s.checkLeg(ctx, tx, leg, r)
	if err != nil {
		return Leg{}, err
	}
	if problem != "" {
		return s.transitionLeg(ctx, tx, leg, StateManualReview, systemActor, "leg reconciliation failed: "+problem, legSet{})
	}
	out, err := s.transitionLeg(ctx, tx, leg, StateReconciled, systemActor, "lock, release and postings agree", legSet{})
	if err != nil {
		return Leg{}, err
	}
	if _, err := s.reconcileRouteLocked(ctx, tx, r, reconciliation.ModeEventDriven, systemActor); err != nil {
		return Leg{}, err
	}
	return out, nil
}

// ResolveReview is an operator's re-check of a released leg in manual review:
// if the lock, the release and both postings now agree, the leg is
// RECONCILED; otherwise it stays in review and the problem is returned.
func (s *Service) ResolveReview(ctx context.Context, tx pgx.Tx, lid LegID, actor econ.Actor, reason string) (Leg, error) {
	if err := requireActor(actor); err != nil {
		return Leg{}, err
	}
	if actor.Type != security.ActorOperator || strings.TrimSpace(reason) == "" {
		return Leg{}, errs.New(errs.CodeForbidden, "bridge: resolving a review is an operator's decision with a reason")
	}
	leg, err := getLeg(ctx, tx, lid, true)
	if err != nil {
		return Leg{}, err
	}
	if leg.Status != StateManualReview || leg.DestinationJournal == nil {
		return Leg{}, errs.New(errs.CodeInvalidStateTransition, "bridge: only a released leg in manual review can be resolved as reconciled")
	}
	r, err := s.getRoute(ctx, tx, leg.RouteID, false)
	if err != nil {
		return Leg{}, err
	}
	problem, err := s.checkLeg(ctx, tx, leg, r)
	if err != nil {
		return Leg{}, err
	}
	if problem != "" {
		return leg, errs.New(errs.CodeReconciliationRequired, "bridge: the leg still does not reconcile").WithField("problem", problem)
	}
	return s.transitionLeg(ctx, tx, leg, StateReconciled, actor, reason, legSet{})
}

// checkLeg returns what disagrees between a released leg and the chains, or
// "".
func (s *Service) checkLeg(ctx context.Context, tx pgx.Tx, leg Leg, r Route) (string, error) {
	if leg.SourceJournal == nil || leg.DestinationJournal == nil {
		return "a posting is missing", nil
	}
	atts, err := attemptsOf(ctx, tx, leg.ID, AttemptRelease)
	if err != nil {
		return "", err
	}
	amount, err := devnet.AmountOf(leg.Quantity)
	if err != nil {
		return "", err
	}
	problem := ""
	err = s.host.ViewIn(ctx, tx, func(v devnethost.View) error {
		lt, err := v.LockedTransfer(r.SourceChain, leg.LockTxID)
		if err != nil {
			problem = "the lock is not on the source chain"
			return nil
		}
		want := devnet.BridgeLock{Mint: r.SourceMint, Owner: leg.SourceOwner, Amount: amount, DestChain: r.DestinationChain,
			DestOwner: leg.DestinationOwner, Nonce: leg.Nonce}
		if lt.Status.State != devnet.TxFinalized || lt.Lock != want {
			problem = "the source lock is not the leg's finalized lock"
			return nil
		}
		var final *Attempt
		for i := range atts {
			st, err := v.Status(r.DestinationChain, atts[i].TxID)
			if err != nil {
				return err
			}
			if st.State != devnet.TxFinalized {
				continue
			}
			if final != nil {
				problem = "more than one release of the lock is finalized"
				return nil
			}
			final = &atts[i]
		}
		if final == nil {
			problem = "no release of the lock is finalized on the destination"
			return nil
		}
		exp := ReleaseExpectation{Chain: r.DestinationChain, Mint: r.DestinationMint, ToOwner: leg.DestinationOwner, Amount: amount,
			SourceChain: r.SourceChain, SourceTxID: leg.LockTxID, Nonce: leg.Nonce}
		if _, err := InspectRelease(r.Attester, exp, final.Raw); err != nil {
			problem = "the finalized release is not the leg's release"
		}
		return nil
	})
	return problem, err
}

// ResumeRelay is an operator's decision to relay a leg in manual review
// again (for instance once a frozen destination mint is unfrozen). It grants
// a fresh round of release attempts.
func (s *Service) ResumeRelay(ctx context.Context, tx pgx.Tx, lid LegID, actor econ.Actor, reason string) (Leg, error) {
	if err := requireActor(actor); err != nil {
		return Leg{}, err
	}
	if actor.Type != security.ActorOperator {
		return Leg{}, errs.New(errs.CodeForbidden, "bridge: only an operator resumes a leg in manual review")
	}
	leg, err := getLeg(ctx, tx, lid, true)
	if err != nil {
		return Leg{}, err
	}
	if leg.Status != StateManualReview || leg.SourceJournal == nil || leg.DestinationJournal != nil || leg.RefundEffectID != nil {
		return Leg{}, errs.New(errs.CodeInvalidStateTransition,
			"bridge: only a source-final, unreleased leg in manual review whose refund was never requested can resume relaying")
	}
	return s.transitionLeg(ctx, tx, leg, StateMessageInFlight, actor, reason, legSet{})
}

// Advance drives a leg as far as it can go now, never resubmitting a lock
// and never racing a release that can still land. A SANDBOX leg is driven by
// producing blocks; a DEVNET leg is only observed.
func (s *Service) Advance(ctx context.Context, lid LegID) (LegResult, error) {
	leg, err := getLeg(ctx, s.db, lid, false)
	if err != nil {
		return LegResult{}, err
	}
	return s.drive(ctx, lid, leg.Mode == gates.ModeSandbox)
}

// Observe advances a leg by observation only: no blocks are produced.
func (s *Service) Observe(ctx context.Context, lid LegID) (LegResult, error) {
	return s.drive(ctx, lid, false)
}

func (s *Service) inTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return s.db.InTx(ctx, txOptions, fn)
}

// maxDriveSteps bounds one drive: every step either changes the leg's status,
// records a release attempt (bounded by the route's MaxReleaseAttempts before
// manual review) or stops.
const maxDriveSteps = 64

func (s *Service) drive(ctx context.Context, lid LegID, produce bool) (LegResult, error) {
	for step := 0; step < maxDriveSteps; step++ {
		leg, err := getLeg(ctx, s.db, lid, false)
		if err != nil {
			return LegResult{}, err
		}
		r, err := s.GetRoute(ctx, s.db, leg.RouteID)
		if err != nil {
			return LegResult{Leg: leg}, err
		}
		attemptsBefore, err := countAllAttempts(ctx, s.db, lid)
		if err != nil {
			return LegResult{Leg: leg}, err
		}
		again := false
		switch leg.Status {
		case StateBridgeRequested:
			if _, err := s.awaitFinal(ctx, r.SourceChain, leg.LockTxID, leg.LockBlockhash, leg.LockBlockhashHeight, produce); err != nil {
				return LegResult{Leg: leg}, err
			}
			err = s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error { _, err := s.SettleSource(ctx, tx, lid); return err })
		case StateSourceFinalized, StateMessageInFlight, StateDestinationObserved:
			again, err = s.relayAndObserve(ctx, leg, r, produce)
		case StateDestinationFinalized:
			err = s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error { _, err := s.ReconcileLeg(ctx, tx, lid); return err })
		default:
			return LegResult{Leg: leg, Settled: leg.Status.Terminal()}, nil
		}
		if err != nil {
			return LegResult{Leg: leg}, err
		}
		after, err := getLeg(ctx, s.db, lid, false)
		if err != nil {
			return LegResult{}, err
		}
		attemptsAfter, err := countAllAttempts(ctx, s.db, lid)
		if err != nil {
			return LegResult{}, err
		}
		if after.Status == leg.Status && attemptsAfter == attemptsBefore && !again {
			return LegResult{Leg: after, Settled: after.Status.Terminal()}, nil
		}
	}
	leg, err := getLeg(ctx, s.db, lid, false)
	return LegResult{Leg: leg, Settled: leg.Status.Terminal()}, err
}

// relayAndObserve relays a new release if one is due, submits it exactly
// once, drives (SANDBOX) or observes (DEVNET) the latest release, and settles
// the destination. It reports whether the latest release is dead while the
// leg still waits for one, so the caller relays again at once.
func (s *Service) relayAndObserve(ctx context.Context, leg Leg, r Route, produce bool) (bool, error) {
	var fresh *Attempt
	if leg.Status != StateDestinationObserved {
		if err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			a, err := s.PrepareRelease(ctx, tx, leg.ID)
			fresh = a
			return err
		}); err != nil {
			return false, err
		}
	}
	if fresh != nil {
		if err := s.host.EnsureNativeBalance(ctx, fresh.Chain, s.attester.PublicKey(), s.params.GasMinimum, s.params.GasTopUp); err != nil {
			return false, err
		}
		exec := s.provider.Execute(ctx, fresh.Chain, fresh.Raw)
		if err := s.recordReleaseSubmission(ctx, leg.ID, *fresh, exec); err != nil {
			return false, err
		}
	}
	atts, err := attemptsOf(ctx, s.db, leg.ID, AttemptRelease)
	if err != nil || len(atts) == 0 {
		return false, err
	}
	last := atts[len(atts)-1]
	obs, err := s.awaitFinal(ctx, last.Chain, last.TxID, last.RecentBlockhash, last.BlockhashHeight, produce)
	if err != nil {
		return false, err
	}
	var after Leg
	if err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		l, err := s.SettleDestination(ctx, tx, leg.ID)
		after = l
		return err
	}); err != nil {
		return false, err
	}
	return obs.Dead() && (after.Status == StateMessageInFlight || after.Status == StateSourceFinalized), nil
}

// awaitFinal observes a transaction and, when produce is set, produces blocks
// until its outcome is final. It never resubmits.
func (s *Service) awaitFinal(ctx context.Context, chain devnet.ChainID, txID devnet.TxID, bh devnet.Hash, bhHeight uint64, produce bool) (BridgeObservation, error) {
	params, ok := s.host.Config().Params(chain)
	if !ok {
		return BridgeObservation{}, errs.New(errs.CodeValidationFailed, "bridge: the host does not run chain "+string(chain))
	}
	budget := params.BlockhashWindow + params.FinalityDepth + 2
	for round := uint64(0); ; round++ {
		obs, err := s.provider.Observe(ctx, nil, chain, txID, bh, bhHeight)
		if err != nil || obs.Final() || !produce || round >= budget {
			return obs, err
		}
		n := uint64(1)
		switch obs.Status.State {
		case devnet.TxConfirmed, devnet.TxFailed:
			tip, err := s.host.Tip(ctx, chain)
			if err != nil {
				return obs, err
			}
			if target := obs.Status.Height + params.FinalityDepth; tip.Height < target {
				n = target - tip.Height
			}
		case devnet.TxNotFound:
			n = max(1, params.BlockhashWindow/4)
		}
		if _, err := s.host.ProduceBlocks(ctx, chain, min(n, devnethost.MaxBlocksPerCall)); err != nil {
			return obs, err
		}
	}
}

// Refund returns a leg's units on the source chain after its destination
// never released: an operator's decision for a leg in manual review whose
// source is final, whose every release attempt failed or expired below
// finality, and whose source asset no other subsystem reconciles supply for
// (the refund mints replacement units on the source; the escrowed originals
// stay retired in the escrow address). The refund is a release on the
// source chain, recorded before it is submitted once; BRIDGE_REFUND posts
// only when it is FINALIZED.
func (s *Service) Refund(ctx context.Context, lid LegID, actor econ.Actor, reason string) (LegResult, error) {
	if err := requireActor(actor); err != nil {
		return LegResult{}, err
	}
	if actor.Type != security.ActorOperator || strings.TrimSpace(reason) == "" {
		return LegResult{}, errs.New(errs.CodeForbidden, "bridge: a refund is an operator's decision with a reason")
	}
	var fresh *Attempt
	var leg Leg
	if err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		a, l, err := s.prepareRefund(ctx, tx, lid, actor, reason)
		fresh, leg = a, l
		return err
	}); err != nil {
		return LegResult{}, err
	}
	if fresh != nil {
		if err := s.host.EnsureNativeBalance(ctx, fresh.Chain, s.attester.PublicKey(), s.params.GasMinimum, s.params.GasTopUp); err != nil {
			return LegResult{Leg: leg}, err
		}
		exec := s.provider.Execute(ctx, fresh.Chain, fresh.Raw)
		if exec.Outcome == ExecUnknown {
			if err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				l, err := getLeg(ctx, tx, lid, true)
				if err != nil || l.RefundEffectID == nil {
					return err
				}
				effect, err := s.effects.Get(ctx, tx, *l.RefundEffectID)
				if err != nil || effect.Status != econ.EffectPending {
					return err
				}
				_, err = s.effects.MarkUnknown(ctx, tx, effect.ID, externalRef(fresh.Chain, fresh.TxID), systemActor,
					"the refund submission's outcome is unknown")
				return err
			}); err != nil {
				return LegResult{Leg: leg}, err
			}
		}
	}
	atts, err := attemptsOf(ctx, s.db, lid, AttemptRefund)
	if err != nil || len(atts) == 0 {
		return LegResult{Leg: leg}, err
	}
	last := atts[len(atts)-1]
	if _, err := s.awaitFinal(ctx, last.Chain, last.TxID, last.RecentBlockhash, last.BlockhashHeight, leg.Mode == gates.ModeSandbox); err != nil {
		return LegResult{Leg: leg}, err
	}
	var out Leg
	err = s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		l, err := s.SettleRefund(ctx, tx, lid, actor)
		out = l
		return err
	})
	return LegResult{Leg: out, Settled: out.Status.Terminal()}, err
}

func (s *Service) prepareRefund(ctx context.Context, tx pgx.Tx, lid LegID, actor econ.Actor, reason string) (*Attempt, Leg, error) {
	leg, err := getLeg(ctx, tx, lid, true)
	if err != nil {
		return nil, Leg{}, err
	}
	if leg.Status == StateFailed && leg.RefundJournal != nil {
		return nil, leg, nil // already refunded: the same decision again changes nothing
	}
	if leg.Status != StateManualReview || leg.SourceJournal == nil || leg.DestinationJournal != nil || leg.RefundJournal != nil {
		return nil, leg, errs.New(errs.CodeInvalidStateTransition, "bridge: only a source-final, unreleased leg in manual review can be refunded")
	}
	r, err := s.getRoute(ctx, tx, leg.RouteID, false)
	if err != nil {
		return nil, leg, err
	}
	var reconciledElsewhere bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM stable_assets WHERE asset_id = $1)`, r.SourceAssetID).Scan(&reconciledElsewhere); err != nil {
		return nil, leg, dbErr("check source asset", err)
	}
	if reconciledElsewhere {
		return nil, leg, errs.New(errs.CodeForbidden,
			"bridge: a refund would mint replacement units of an asset whose supply the stable asset controller reconciles; complete the release instead")
	}
	for _, kind := range []AttemptKind{AttemptRelease, AttemptRefund} {
		atts, err := attemptsOf(ctx, tx, lid, kind)
		if err != nil {
			return nil, leg, err
		}
		for _, a := range atts {
			obs, err := s.provider.Observe(ctx, tx, a.Chain, a.TxID, a.RecentBlockhash, a.BlockhashHeight)
			if err != nil {
				return nil, leg, err
			}
			if kind == AttemptRelease && !obs.Dead() {
				return nil, leg, errs.New(errs.CodeConflict, "bridge: a release of this leg can still land or has landed; it cannot be refunded")
			}
			if kind == AttemptRefund && !obs.Dead() {
				return nil, leg, nil // a refund is already on its way: observe it, never send another
			}
		}
	}
	set := legSet{}
	if leg.RefundEffectID == nil {
		effect, _, err := s.effects.Begin(ctx, tx, econ.BeginRequest{
			Key: "bridge:refund:" + lid.String(), Subsystem: econ.SubsystemBridge, Kind: effectKindRefund,
			IntentID: &leg.IntentID, AccountID: leg.AccountID.String(), PostsLedger: true, Actor: actor,
			Request: legEffectRequest{LegID: lid.String(), RouteID: r.ID.String(), AccountID: leg.AccountID.String(),
				Quantity: leg.Quantity.String(), Phase: "REFUND"},
		})
		if err != nil {
			return nil, leg, err
		}
		set.refundEffect = &effect.ID
		if err := s.updateLeg(ctx, tx, leg, "", set); err != nil {
			return nil, leg, err
		}
	}
	prepared, err := s.provider.Plan(ctx, tx, PlanRequest{Kind: PlanRefund, Route: r, Leg: leg.ref()})
	if err != nil {
		return nil, leg, err
	}
	all, err := countAllAttempts(ctx, tx, lid)
	if err != nil {
		return nil, leg, err
	}
	a := Attempt{LegID: lid, Number: all + 1, Kind: AttemptRefund, Chain: prepared.Chain, TxID: prepared.TxID, Raw: prepared.Raw,
		RecentBlockhash: prepared.RecentBlockhash, BlockhashHeight: prepared.BlockhashHeight}
	if err := insertAttempt(ctx, tx, a); err != nil {
		return nil, leg, err
	}
	if err := s.auditEvent(ctx, tx, actor, "bridge.leg.refund_requested", "bridge_leg", lid.String(), leg.AccountID.String(), reason,
		map[string]string{"refund_tx_id": string(a.TxID)}); err != nil {
		return nil, leg, err
	}
	return &a, leg, nil
}

// SettleRefund posts BRIDGE_REFUND once the refund is FINALIZED on the
// source chain: the units return to WALLET(source), the destination effect
// fails, and the leg ends FAILED with code REFUNDED.
func (s *Service) SettleRefund(ctx context.Context, tx pgx.Tx, lid LegID, actor econ.Actor) (Leg, error) {
	leg, err := getLeg(ctx, tx, lid, true)
	if err != nil || leg.Status != StateManualReview || leg.RefundEffectID == nil {
		return leg, err
	}
	r, err := s.lockRoute(ctx, tx, leg.RouteID)
	if err != nil {
		return Leg{}, err
	}
	atts, err := attemptsOf(ctx, tx, lid, AttemptRefund)
	if err != nil || len(atts) == 0 {
		return leg, err
	}
	a := atts[len(atts)-1]
	obs, err := s.provider.Observe(ctx, tx, a.Chain, a.TxID, a.RecentBlockhash, a.BlockhashHeight)
	if err != nil || !obs.Finalized() {
		return leg, err
	}
	res, err := s.ledger.Post(ctx, tx, refundPosting(r, leg, string(a.TxID), s.clk.Now().UTC()))
	if err != nil {
		return Leg{}, err
	}
	if _, err := s.effects.Apply(ctx, tx, *leg.RefundEffectID, econ.ApplyRequest{
		JournalTransactionID: &res.TransactionID, ExternalRef: externalRef(a.Chain, a.TxID), Actor: actor, Reason: "refund finalized on the source chain",
	}); err != nil {
		return Leg{}, err
	}
	// The lock's books are reversed by the refund journal: the source effect
	// stays APPLIED in history and is marked COMPENSATED by that journal.
	if _, err := s.effects.Compensate(ctx, tx, *leg.SourceEffectID, &res.TransactionID, actor,
		"the destination never released; the refund journal reverses the lock's books"); err != nil {
		return Leg{}, err
	}
	if leg.DestinationEffectID != nil {
		effect, err := s.effects.Get(ctx, tx, *leg.DestinationEffectID)
		if err != nil {
			return Leg{}, err
		}
		if effect.Status == econ.EffectPending || effect.Status == econ.EffectUnknown {
			if _, err := s.effects.Fail(ctx, tx, effect.ID, "REFUNDED", actor, "the destination never released; refunded on the source"); err != nil {
				return Leg{}, err
			}
		}
	}
	out, err := s.transitionLeg(ctx, tx, leg, StateFailed, actor, "refunded on the source chain",
		legSet{refundJournal: &res.TransactionID, failureCode: "REFUNDED"})
	if err != nil {
		return Leg{}, err
	}
	return out, s.finishIntent(ctx, tx, leg.IntentID, false, "refunded")
}

// ObservePending advances every open leg by observation only (DEVNET legs,
// and any leg a process stopped driving): no blocks are produced, nothing is
// resubmitted. It is also the reconciliation of unknown lock and release
// submissions: their outcome is read from the chains by tx id.
func (s *Service) ObservePending(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	legs, err := listLegs(ctx, s.db, `status IN ('BRIDGE_REQUESTED','SOURCE_FINALIZED','MESSAGE_IN_FLIGHT','DESTINATION_OBSERVED','DESTINATION_FINALIZED')
		ORDER BY created_at, id LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	for _, l := range legs {
		if _, err := s.drive(ctx, l.ID, false); err != nil {
			return 0, err
		}
	}
	return len(legs), nil
}
