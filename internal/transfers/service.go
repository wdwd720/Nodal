package transfers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/settlement"
)

// ReservationTTL bounds the sender's reservation. It is consumed in the same
// transaction that creates it; the TTL only matters if that transaction is
// abandoned, and then the reservation disappears with it.
const ReservationTTL = 5 * time.Minute

// effectKind is the economic effect kind of a transfer.
const effectKind = "P2P_TRANSFER"

// serviceActor is who moves a transfer through its lifecycle: a deterministic
// service, never the customer and never an agent.
var serviceActor = econ.SystemActor("transfers")

// Reserver is the part of capital.Service a transfer uses. *capital.Service
// satisfies it.
type Reserver interface {
	Reserve(ctx context.Context, tx pgx.Tx, r capital.ReserveRequest) (capital.Reservation, error)
	Consume(ctx context.Context, tx pgx.Tx, id capital.ReservationID, qty money.Quantity, usdMinor int64, orderID string) (capital.Reservation, error)
	Availability(ctx context.Context, q db.Querier, accountID string, asset assets.AssetID, now time.Time) (capital.Availability, error)
}

// Deps are the Service's collaborators. Every one is required: a transfer
// service missing its screen, its notifier or its kill-switch reader would
// move value past a control it was supposed to pass.
type Deps struct {
	Clock        clock.Clock
	Ledger       *ledger.Service
	Capital      Reserver
	Intents      *econ.IntentStore
	Effects      *econ.EffectStore
	Authorizer   *econ.Authorizer
	KillSwitches econ.KillSwitchReader
	Router       *legalrouter.Router
	Risk         *risk.Store
	RiskPolicy   risk.AdvancedPolicy
	Policy       Policy
	Valuer       Valuer
	Screen       ComplianceScreen
	Notifier     Notifier
	Audit        audit.Writer
}

// Service sends, reverses, reads and reconciles internal transfers. It holds
// no state beyond its dependencies; every mutating method runs in the
// caller's transaction.
type Service struct {
	clk        clock.Clock
	ledger     *ledger.Service
	capital    Reserver
	intents    *econ.IntentStore
	effects    *econ.EffectStore
	authz      *econ.Authorizer
	kills      econ.KillSwitchReader
	router     *legalrouter.Router
	risk       *risk.Store
	riskPolicy risk.AdvancedPolicy
	policy     Policy
	policyHash string
	valuer     Valuer
	screen     ComplianceScreen
	notifier   Notifier
	audit      audit.Writer
	accounts   *accounts.Repository
	assets     *assets.Repository
	profiles   *compliance.Repository
}

// NewService wires a Service.
func NewService(d Deps) (*Service, error) {
	var missing []string
	for _, dep := range []struct {
		name string
		nil  bool
	}{
		{"clock", d.Clock == nil}, {"ledger", d.Ledger == nil}, {"capital", d.Capital == nil},
		{"intents", d.Intents == nil}, {"effects", d.Effects == nil}, {"authorizer", d.Authorizer == nil},
		{"kill switches", d.KillSwitches == nil}, {"router", d.Router == nil}, {"risk store", d.Risk == nil},
		{"valuer", d.Valuer == nil}, {"screen", d.Screen == nil}, {"notifier", d.Notifier == nil}, {"audit", d.Audit == nil},
	} {
		if dep.nil {
			missing = append(missing, dep.name)
		}
	}
	if len(missing) > 0 {
		return nil, errs.New(errs.CodeValidationFailed, "transfers: missing dependencies").WithField("missing", missing)
	}
	if err := d.Policy.Validate(); err != nil {
		return nil, err
	}
	if d.RiskPolicy.Missing() {
		return nil, errs.New(errs.CodeValidationFailed, "transfers: the risk policy is missing; a transfer is never evaluated without one")
	}
	hash, err := d.Policy.Hash()
	if err != nil {
		return nil, err
	}
	return &Service{
		clk: d.Clock, ledger: d.Ledger, capital: d.Capital, intents: d.Intents, effects: d.Effects, authz: d.Authorizer,
		kills: d.KillSwitches, router: d.Router, risk: d.Risk, riskPolicy: d.RiskPolicy, policy: d.Policy, policyHash: hash,
		valuer: d.Valuer, screen: d.Screen, notifier: d.Notifier, audit: d.Audit,
		accounts: accounts.NewRepository(), assets: assets.NewRepository(), profiles: compliance.NewRepository(d.Audit),
	}, nil
}

// Policy returns the transfer policy in force and its hash.
func (s *Service) Policy() (Policy, string) { return s.policy, s.policyHash }

// SendRequest asks to move Quantity base units of a SIMULATED asset from the
// sender to exactly one recipient, named by account id or by handle.
type SendRequest struct {
	SenderAccountID    accounts.AccountID
	RecipientAccountID accounts.AccountID
	RecipientHandle    string
	AssetID            assets.AssetID
	Quantity           money.Quantity
	Mode               gates.Mode
	Memo               string
	IdempotencyKey     string
	CorrelationID      string
}

// canonicalSend is what the idempotency hash covers: everything the sender
// stated, as stated.
type canonicalSend struct {
	Sender          string `json:"sender_account_id"`
	RecipientID     string `json:"recipient_account_id"`
	RecipientHandle string `json:"recipient_handle"`
	Asset           string `json:"asset_id"`
	Quantity        string `json:"quantity"`
	Mode            string `json:"mode"`
	MemoSHA256      string `json:"memo_sha256"`
}

func (r SendRequest) hash() ([]byte, error) {
	c := canonicalSend{
		Sender: r.SenderAccountID.String(), RecipientHandle: r.RecipientHandle, Asset: r.AssetID.String(),
		Quantity: r.Quantity.String(), Mode: string(r.Mode), MemoSHA256: MemoDigest(r.Memo),
	}
	if !r.RecipientAccountID.IsZero() {
		c.RecipientID = r.RecipientAccountID.String()
	}
	return econ.RequestHash(c)
}

func (s *Service) validateSend(r *SendRequest) error {
	var problems []string
	if r.SenderAccountID.IsZero() {
		problems = append(problems, "sender_account_id required")
	}
	if r.RecipientHandle != "" {
		h, err := NormalizeHandle(r.RecipientHandle)
		if err != nil {
			problems = append(problems, "recipient_handle is not a valid handle")
		}
		r.RecipientHandle = h
	}
	if r.RecipientAccountID.IsZero() == (r.RecipientHandle == "") {
		problems = append(problems, "name the recipient by exactly one of recipient_account_id or recipient_handle")
	}
	if r.AssetID.IsZero() {
		problems = append(problems, "asset_id required")
	}
	if !r.Quantity.IsPositive() {
		problems = append(problems, "quantity must be a positive number of base units")
	}
	if !r.Mode.Valid() {
		problems = append(problems, "mode must be a declared mode")
	}
	if err := ValidateMemo(r.Memo, s.policy.MemoMaxRunes); err != nil {
		problems = append(problems, "memo: "+err.Error())
	}
	if !validIdempotencyKey(r.IdempotencyKey) {
		problems = append(problems, "idempotency key must be 1..200 bytes of UTF-8 without surrounding whitespace")
	}
	if len(r.CorrelationID) > 200 {
		problems = append(problems, "correlation id is at most 200 bytes")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "invalid transfer request").WithField("problems", problems)
	}
	return nil
}

// Result is the outcome of Send.
type Result struct {
	Transfer Transfer
	// Replayed is true when the idempotency key had already been used for the
	// same request: nothing new was written.
	Replayed bool
}

// sendState is everything one Send learned, kept for the decision evidence.
type sendState struct {
	t         Transfer
	intent    econ.Intent
	userID    accounts.UserID
	sender    accounts.Account
	recipient accounts.Account
	asset     assets.Asset
	quote     Quote
	stepUp    bool
	route     *settlement.Route
	policy    *PolicyDecision
	decision  *risk.AdvancedDecision
	riskID    *risk.DecisionID
	screen    *ScreenResult
	screenErr string
	rsv       *capital.Reservation
	journal   *ledger.TransactionID
	now       time.Time
}

// Send moves value from the sender to the recipient: authenticate, resolve
// the recipient, check eligibility, authorize the mode, value the amount,
// demand step-up above the threshold, record the intent, route it through
// the settlement compiler, apply the transfer policy and Risk Kernel V2,
// screen it, reserve the sender's funds, claim the effect, post the
// P2P_TRANSFER journal, consume the reservation, archive the evidence, apply
// the effect, complete the transfer and the intent and notify the recipient
// -- all in tx, so it all happens or none of it does.
//
// A refusal that is a decision (route, recipient rules, limits, velocity,
// risk, compliance, funds) is recorded: the transfer and its intent are
// REJECTED with a stable code, the risk decision and the evidence are
// written, and the returned error carries the same code. Nothing is posted.
// Such a refusal is recognisable by Result.Transfer.Status == REJECTED: commit
// tx to keep the record (the transaction is never left aborted by a refusal).
// Any other error means tx must be rolled back. A refusal before the request
// is accepted for evaluation
// (authentication, validation, an unknown or inactive recipient, a
// non-simulated asset, a capability that is not enabled, a missing price,
// step-up) writes nothing.
//
// Send is idempotent on (sender account, idempotency key): the same request
// returns the original transfer (and, for a refusal, the original error) with
// Replayed set; a different request under the same key is
// INVALID_IDEMPOTENCY_REUSE.
func (s *Service) Send(ctx context.Context, tx pgx.Tx, p security.Principal, req SendRequest) (Result, error) {
	if tx == nil {
		return Result{}, errs.New(errs.CodeInternal, "transfers: Send requires a transaction")
	}
	now := s.clk.Now().UTC()

	// 1. Authenticate. Agents never move value between customers.
	if p.IsAgent() {
		return Result{}, s.refuseAgent(p, req, now)
	}
	if err := p.Validate(); err != nil {
		return Result{}, errs.Wrap(err, errs.CodeForbidden, "invalid principal")
	}
	if p.ActorType != security.ActorUser {
		return Result{}, errs.New(errs.CodeForbidden, "a transfer is sent by the customer who owns the sender account").
			WithField("actor_type", string(p.ActorType))
	}
	userID, err := accounts.ParseUserID(p.SubjectID)
	if err != nil {
		return Result{}, errs.New(errs.CodeForbidden, "the principal does not name a user")
	}
	ctx = security.WithPrincipal(ctx, p)
	if err := security.RequireAccountOwner(ctx, req.SenderAccountID.String()); err != nil {
		return Result{}, securityErr(err)
	}
	if err := s.validateSend(&req); err != nil {
		return Result{}, err
	}
	reqHash, err := req.hash()
	if err != nil {
		return Result{}, err
	}

	// 2. One decision at a time for this sender, then replay.
	if err := lockAccount(ctx, tx, req.SenderAccountID); err != nil {
		return Result{}, err
	}
	if existing, found, err := transferByKey(ctx, tx, req.SenderAccountID, req.IdempotencyKey); err != nil {
		return Result{}, err
	} else if found {
		if !bytes.Equal(existing.RequestHash, reqHash) {
			return Result{}, errs.New(errs.CodeInvalidIdempotencyReuse,
				"this idempotency key was already used for a different transfer").WithField("transfer_id", existing.ID.String())
		}
		return Result{Transfer: existing, Replayed: true}, existing.Err()
	}

	// 3. Eligibility: both parties, the asset, the mode.
	st := &sendState{userID: userID, now: now}
	if st.sender, err = s.accounts.Get(ctx, tx, req.SenderAccountID); err != nil {
		return Result{}, notFound(err, "sender account not found")
	}
	if st.sender.OwnerUserID != userID {
		return Result{}, errs.New(errs.CodeForbidden, "the sender account belongs to someone else")
	}
	if err := eligibleParty("sender", st.sender); err != nil {
		return Result{}, err
	}
	if st.recipient, err = s.resolveRecipient(ctx, tx, req); err != nil {
		return Result{}, err
	}
	if st.recipient.ID == st.sender.ID {
		return Result{}, errs.New(errs.CodeValidationFailed, "a transfer moves value between two different accounts")
	}
	if err := eligibleParty("recipient", st.recipient); err != nil {
		return Result{}, err
	}
	if st.asset, err = s.assets.Get(ctx, tx, req.AssetID); err != nil {
		return Result{}, notFound(err, "asset not found")
	}
	if st.asset.Kind != assets.KindSimulated {
		return Result{}, errs.New(errs.CodeForbidden, "only SIMULATED value moves between customers in this build").
			WithField("asset_id", st.asset.ID.String()).WithField("asset_kind", string(st.asset.Kind))
	}
	if st.asset.Status != assets.StatusActive {
		return Result{}, errs.Newf(errs.CodeAssetRestricted, "asset is %s", st.asset.Status).WithField("asset_id", st.asset.ID.String())
	}
	if err := s.authz.Require(ctx, tx, gates.P2PTransfers, req.Mode); err != nil {
		return Result{}, err
	}
	if !supportedMode(req.Mode) {
		return Result{}, errs.Newf(errs.CodeValidationFailed, "transfers run in %v", SupportedModes())
	}

	// 4. Value it (fail closed), then step-up above the threshold.
	if st.quote, err = s.valuer.Quote(ctx, tx, st.asset, now); err != nil {
		return Result{}, err
	}
	notional, err := st.quote.USD(req.Quantity, money.RoundUp)
	if err != nil {
		return Result{}, err
	}
	st.stepUp = s.policy.StepUpRequired(notional)
	if st.stepUp {
		if err := security.RequireStepUp(ctx, s.policy.StepUpMaxAge(), s.clk.Now); err != nil {
			return Result{}, securityErr(err).
				WithField("step_up_above", s.policy.StepUpAbove.String()).
				WithField("notional_usd", notional.String())
		}
	}

	// 5. The intent, and the transfer register row, both born now.
	terms, err := json.Marshal(map[string]string{
		"recipient_account_id": st.recipient.ID.String(), "recipient_handle": req.RecipientHandle,
		"memo_sha256": MemoDigest(req.Memo),
	})
	if err != nil {
		return Result{}, errs.Wrap(err, errs.CodeInternal, "transfers: encode intent terms")
	}
	qty := req.Quantity
	assetID := st.asset.ID
	intent, existed, err := s.intents.Submit(ctx, tx, econ.Intent{
		AccountID: st.sender.ID, Kind: econ.KindTransferValue, Mode: req.Mode, Capability: gates.P2PTransfers,
		ActorType: security.ActorUser, ActorID: p.SubjectID, AssetID: &assetID, Quantity: &qty, Terms: terms,
		IdempotencyKey: "p2p:" + req.IdempotencyKey, CorrelationID: req.CorrelationID,
	})
	if err != nil {
		return Result{}, err
	}
	if existed {
		return Result{}, errs.New(errs.CodeConflict, "an intent exists for this key without its transfer").
			WithField("intent_id", intent.ID.String())
	}
	st.intent = intent
	st.t, err = insertTransfer(ctx, tx, Transfer{
		ID: NewTransferID(), IntentID: intent.ID, SenderAccountID: st.sender.ID, SenderUserID: userID,
		RecipientAccountID: st.recipient.ID, RecipientHandle: req.RecipientHandle, AssetID: st.asset.ID,
		Quantity: req.Quantity, NotionalUSD: notional, Mode: req.Mode, Memo: req.Memo,
		IdempotencyKey: req.IdempotencyKey, RequestHash: reqHash, PolicyVersion: s.policy.Version,
		PolicyHash: s.policyHash, CorrelationID: req.CorrelationID, CreatedAt: now,
	})
	if err != nil {
		return Result{}, err
	}

	// 6. Route through the settlement compiler: agent ladder, legal router,
	// simulated domain.
	route, err := econ.Route(econ.RouteRequest{
		Intent:  intent,
		Subject: settlement.Subject{Type: settlement.SubjectAccount, ID: st.recipient.ID.String(), AssetID: st.asset.ID.String()},
	}, s.router, now)
	if err != nil {
		return Result{}, err
	}
	st.route = &route
	if !route.MayExecuteNow() {
		return s.reject(ctx, tx, st, append([]string{RejectRouteRefused}, route.Reasons...))
	}
	if err := s.advance(ctx, tx, st, econ.StatusRouted, "routed: "+route.Product); err != nil {
		return Result{}, err
	}

	// 7. The transfer policy and Risk Kernel V2, both always evaluated so the
	// evidence names everything that would have to change.
	codes, err := s.evaluate(ctx, tx, st, req)
	if err != nil {
		return Result{}, err
	}
	if len(codes) > 0 {
		return s.reject(ctx, tx, st, codes)
	}
	if err := s.advance(ctx, tx, st, econ.StatusRiskApproved, "policy and risk approved"); err != nil {
		return Result{}, err
	}

	// 8. Compliance screen. An error is not a pass.
	res, err := s.screen.Screen(ctx, ScreenRequest{
		TransferID: st.t.ID.String(), SenderAccountID: st.sender.ID, SenderUserID: userID,
		RecipientAccountID: st.recipient.ID, RecipientUserID: st.recipient.OwnerUserID, AssetID: st.asset.ID,
		Quantity: req.Quantity, NotionalUSD: notional, MemoSHA256: MemoDigest(req.Memo),
	})
	if err != nil {
		st.screenErr = err.Error()
		return s.reject(ctx, tx, st, []string{RejectComplianceUnavailable})
	}
	st.screen = &res
	if res.Decision != ScreenClear {
		codes := []string{RejectComplianceBlocked}
		if validReasonCode(res.Code) {
			codes = append(codes, res.Code)
		}
		return s.reject(ctx, tx, st, codes)
	}

	// 9. Reserve the sender's funds: the authoritative no-overspend check,
	// under the (account, asset) totals lock every spender of this wallet
	// takes.
	rsv, err := s.capital.Reserve(ctx, tx, capital.ReserveRequest{
		// The reservation names the economic intent it serves (00733); the
		// transfer row and the effect name the reservation in turn.
		EconomicIntentID: st.t.IntentID.String(),
		AccountID:        st.sender.ID.String(), AssetID: st.asset.ID, Quantity: req.Quantity,
		ActorType: security.ActorUser, ActorID: p.SubjectID, IdempotencyKey: "p2p:" + st.t.ID.String(),
		TTL: ReservationTTL, Reason: "simulated transfer " + st.t.ID.String(),
	})
	if err != nil {
		switch errs.CodeOf(err) {
		case errs.CodeInsufficientBuyingPower:
			return s.reject(ctx, tx, st, []string{RejectInsufficientFunds})
		case errs.CodeAccountFrozen, errs.CodeAssetRestricted:
			return s.reject(ctx, tx, st, []string{string(errs.CodeOf(err))})
		}
		return Result{}, err
	}
	st.rsv = &rsv
	if err := s.advance(ctx, tx, st, econ.StatusReserved, "sender funds reserved"); err != nil {
		return Result{}, err
	}

	// 10. Claim the effect, post, consume, evidence, apply.
	eff, replay, err := s.effects.Begin(ctx, tx, econ.BeginRequest{
		Key: "p2p:" + st.t.ID.String(), Subsystem: econ.SubsystemTransfers, Kind: effectKind, IntentID: &intent.ID,
		AccountID: st.sender.ID.String(), Request: effectRequest(st.t), PostsLedger: true, Actor: serviceActor,
		CorrelationID: req.CorrelationID,
	})
	if err != nil {
		return Result{}, err
	}
	if replay {
		return Result{}, errs.New(errs.CodeConflict, "the effect of a new transfer already exists").WithField("effect_id", eff.ID.String())
	}
	if err := s.advance(ctx, tx, st, econ.StatusExecuting, "posting"); err != nil {
		return Result{}, err
	}
	post, err := s.ledger.Post(ctx, tx, transferPosting(st.t, now))
	if err != nil {
		return Result{}, err
	}
	st.journal = &post.TransactionID
	if _, err := s.capital.Consume(ctx, tx, rsv.ID, req.Quantity, 0, ""); err != nil {
		return Result{}, err
	}
	ref, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "p2p/" + st.t.ID.String(), Document: s.evidence(st, StatusCompleted, nil)})
	if err != nil {
		return Result{}, err
	}
	if _, err := s.effects.Apply(ctx, tx, eff.ID, econ.ApplyRequest{
		JournalTransactionID: &post.TransactionID, ReservationID: rsv.ID.String(), Evidence: &ref,
		Actor: serviceActor, Reason: "simulated transfer posted",
	}); err != nil {
		return Result{}, err
	}
	effID := eff.ID
	done, err := s.decide(ctx, tx, st.t, StatusCompleted, outcome{
		RiskDecisionID: st.riskID, EffectID: &effID, JournalTransactionID: &post.TransactionID,
		ReservationID: rsv.ID.String(), Evidence: ref,
	}, serviceActor, "posted", now)
	if err != nil {
		return Result{}, err
	}
	if err := s.advance(ctx, tx, st, econ.StatusCompleted, "posted"); err != nil {
		return Result{}, err
	}

	// 11. Tell the recipient, in the same transaction.
	if err := s.notifier.TransferReceived(ctx, tx, Notice{
		TransferID: done.ID, RecipientAccountID: st.recipient.ID, RecipientUserID: st.recipient.OwnerUserID,
		AssetID: st.asset.ID, AssetSymbol: st.asset.Symbol, AssetDecimals: st.asset.Decimals,
		Quantity: done.Quantity, CorrelationID: req.CorrelationID,
	}); err != nil {
		return Result{}, err
	}
	return Result{Transfer: done}, nil
}

// evaluate gathers the snapshots, applies the transfer policy and Risk Kernel
// V2, persists the risk decision and returns every violated code: policy
// codes in precedence order, then risk reason codes.
func (s *Service) evaluate(ctx context.Context, tx pgx.Tx, st *sendState, req SendRequest) ([]string, error) {
	now := st.now
	daily, err := usageSince(ctx, tx, st.sender.ID, now.Add(-s.policy.DailyWindow()))
	if err != nil {
		return nil, err
	}
	velocity, err := usageSince(ctx, tx, st.sender.ID, now.Add(-s.policy.VelocityWindow()))
	if err != nil {
		return nil, err
	}
	rule, ruleAt, err := ruleInForce(ctx, tx, st.sender.ID, st.recipient.ID)
	if err != nil {
		return nil, err
	}
	mode, err := modeInForce(ctx, tx, st.sender.ID)
	if err != nil {
		return nil, err
	}
	pd := s.policy.Evaluate(PolicyInput{
		Notional: st.t.NotionalUSD, Daily: daily, Velocity: velocity, Rule: rule, SenderMode: mode,
	})
	st.policy = &pd

	avail, err := s.capital.Availability(ctx, tx, st.sender.ID.String(), st.asset.ID, now)
	if err != nil {
		return nil, err
	}
	// Balance compares like with like: both sides valued with the same
	// rounding, so a sufficient balance is never refused by a rounding
	// artefact. The exact check, in base units, is the reservation.
	availableUSD, err := st.quote.USD(avail.Available, money.RoundDown)
	if err != nil {
		return nil, err
	}
	requiredUSD, err := st.quote.USD(req.Quantity, money.RoundDown)
	if err != nil {
		return nil, err
	}
	// The advanced capabilities' one identity rule (D-055): NODAL_IDENTITY
	// or a verified, unexpired compliance profile, read in this transaction.
	verified, err := compliance.NewAccountVerification(s.clk).IdentityVerified(ctx, tx, st.sender.ID)
	if err != nil {
		return nil, err
	}
	snap, err := s.kills.Snapshot(ctx, tx)
	if err != nil {
		return nil, err
	}
	addedAt := st.recipient.CreatedAt.UTC()
	if ruleAt != nil && rule == RuleAllow {
		addedAt = *ruleAt
	}
	in := risk.AdvancedInput{
		Stage: risk.StageFinal,
		Intent: risk.AdvancedIntent{
			ID: st.intent.ID.String(), Kind: string(econ.KindTransferValue), AccountID: st.sender.ID.String(),
			ActorType: string(security.ActorUser), Capability: string(gates.P2PTransfers), Mode: string(st.t.Mode),
			NotionalUSD: st.t.NotionalUSD,
			// Value leaving the sender: every kill switch that stops new risk
			// stops a transfer.
			RiskIncreasing: true,
		},
		Identity:     &risk.IdentitySnapshot{Verified: verified},
		Account:      &risk.AdvancedAccountSnapshot{Status: string(st.sender.Status)},
		Balance:      &risk.BalanceSnapshot{Available: availableUSD, Required: requiredUSD},
		Velocity:     &risk.VelocitySnapshot{CountInWindow: velocity.Count, NotionalInWindow: velocity.Notional},
		Destination:  &risk.DestinationSnapshot{Allowlisted: pd.Allowlisted, AddedAt: addedAt},
		KillSwitches: riskKillSwitches(snap),
		Now:          now,
	}
	d := risk.EvaluateAdvanced(s.riskPolicy, in)
	rid, err := s.risk.RecordAdvancedDecision(ctx, tx, in, d, req.CorrelationID)
	if err != nil {
		return nil, dbErr("record risk decision", err)
	}
	st.decision, st.riskID = &d, &rid
	codes := append([]string(nil), pd.Codes...)
	return append(codes, d.ReasonCodes...), nil
}

// reject records a refusal: evidence, the intent REJECTED, the transfer
// REJECTED with its primary code and every code, and returns the error.
func (s *Service) reject(ctx context.Context, tx pgx.Tx, st *sendState, codes []string) (Result, error) {
	codes = dedupeKeepOrder(codes)
	ref, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "p2p/" + st.t.ID.String(), Document: s.evidence(st, StatusRejected, codes)})
	if err != nil {
		return Result{}, err
	}
	if _, err := s.intents.Transition(ctx, tx, st.intent.ID, econ.StatusRejected, serviceActor, "transfer refused: "+codes[0], codes[0]); err != nil {
		return Result{}, err
	}
	t, err := s.decide(ctx, tx, st.t, StatusRejected, outcome{
		RejectionCode: codes[0], ReasonCodes: codes, RiskDecisionID: st.riskID, Evidence: ref,
	}, serviceActor, "refused: "+codes[0], st.now)
	if err != nil {
		return Result{}, err
	}
	return Result{Transfer: t}, t.Err()
}

func (s *Service) advance(ctx context.Context, tx pgx.Tx, st *sendState, to econ.IntentStatus, reason string) error {
	in, err := s.intents.Transition(ctx, tx, st.intent.ID, to, serviceActor, reason, "")
	if err != nil {
		return err
	}
	st.intent = in
	return nil
}

// refuseAgent routes the would-be intent through the settlement compiler at
// the highest authority an agent can hold, so the refusal an agent gets is the
// compiler's own (AGENT_ACTION_PERMANENTLY_FORBIDDEN), and refuses whatever
// the compiler says: nothing about an agent principal can make this a
// transfer. Nothing is written.
func (s *Service) refuseAgent(p security.Principal, req SendRequest, now time.Time) error {
	qty := req.Quantity
	route, err := econ.Route(econ.RouteRequest{
		Intent: econ.Intent{
			ID: econ.NewIntentID(), AccountID: req.SenderAccountID, Kind: econ.KindTransferValue, Mode: req.Mode,
			Capability: gates.P2PTransfers, ActorType: security.ActorAgent, ActorID: p.SubjectID, AgentID: p.SubjectID,
			Quantity: &qty, IdempotencyKey: "p2p:agent-refusal",
		},
		Subject:        settlement.Subject{Type: settlement.SubjectAccount, ID: req.RecipientAccountID.String(), AssetID: req.AssetID.String()},
		AgentAuthority: agentauthority.LevelAutonomousPortfolio,
	}, s.router, now)
	e := errs.New(errs.CodeForbidden, "an agent never moves value between customers; a person sends a transfer").
		WithField("actor_type", string(p.ActorType))
	if err == nil {
		e = e.WithField("route_permitted", route.Permitted).WithField("route_reasons", route.Reasons)
	}
	return e
}

func (s *Service) resolveRecipient(ctx context.Context, q db.Querier, req SendRequest) (accounts.Account, error) {
	rid := req.RecipientAccountID
	if req.RecipientHandle != "" {
		var err error
		if rid, err = s.ResolveHandle(ctx, q, req.RecipientHandle); err != nil {
			return accounts.Account{}, err
		}
	}
	a, err := s.accounts.Get(ctx, q, rid)
	if err != nil {
		return accounts.Account{}, notFound(err, "recipient not found")
	}
	return a, nil
}

// eligibleParty refuses anything but an ACTIVE customer account.
func eligibleParty(role string, a accounts.Account) error {
	if a.Kind != accounts.KindCustomer {
		return errs.Newf(errs.CodeForbidden, "the %s is not a customer account", role).WithField(role+"_account_id", a.ID.String())
	}
	switch a.Status {
	case accounts.StatusActive:
		return nil
	case accounts.StatusFrozen:
		return errs.Newf(errs.CodeAccountFrozen, "the %s account is frozen", role).WithField(role+"_account_id", a.ID.String())
	}
	return errs.Newf(errs.CodeForbidden, "the %s account is %s; transfers need ACTIVE accounts", role, a.Status).
		WithField(role+"_account_id", a.ID.String()).WithField("account_status", string(a.Status))
}

// Err is the refusal a REJECTED transfer stands for, or nil. A replay of a
// refused request returns the same error as the original.
func (t Transfer) Err() error {
	if t.Status != StatusRejected {
		return nil
	}
	return errs.Newf(errorCodeFor(t.RejectionCode), "transfer refused: %s", t.RejectionCode).
		WithField("transfer_id", t.ID.String()).
		WithField("rejection_code", t.RejectionCode).
		WithField("reason_codes", t.ReasonCodes)
}

// errorCodeFor maps a primary rejection code onto the stable API code.
func errorCodeFor(code string) errs.Code {
	switch code {
	case RejectRouteRefused, RejectRecipientBlocked, RejectRecipientNotAllowed, RejectComplianceBlocked:
		return errs.CodeForbidden
	case RejectComplianceUnavailable:
		return errs.CodeProviderUnavailable
	case RejectInsufficientFunds, risk.ReasonInsufficientBalance:
		return errs.CodeInsufficientBuyingPower
	case string(errs.CodeAccountFrozen):
		return errs.CodeAccountFrozen
	case string(errs.CodeAssetRestricted):
		return errs.CodeAssetRestricted
	}
	return errs.CodeRiskRejected
}

func riskKillSwitches(snap killswitch.Snapshot) []risk.KillSwitch {
	out := []risk.KillSwitch{}
	for _, sw := range snap.Switches {
		if sw.Active {
			out = append(out, risk.KillSwitch{Kind: string(sw.Kind), Scope: sw.ScopeID})
		}
	}
	return out
}

// effectRequest is the canonical request an effect key is claimed for.
func effectRequest(t Transfer) map[string]string {
	return map[string]string{
		"transfer_id": t.ID.String(), "sender_account_id": t.SenderAccountID.String(),
		"recipient_account_id": t.RecipientAccountID.String(), "asset_id": t.AssetID.String(),
		"quantity": t.Quantity.String(), "mode": string(t.Mode), "policy_hash": t.PolicyHash,
	}
}

func dedupeKeepOrder(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, c := range in {
		if c != "" && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

func securityErr(err error) *errs.Error {
	switch {
	case errors.Is(err, security.ErrUnauthenticated):
		return errs.Wrap(err, errs.CodeUnauthenticated, "authentication required")
	case errors.Is(err, security.ErrStepUpRequired):
		return errs.Wrap(err, errs.CodeStepUpRequired, "a recent strong authentication is required for this transfer")
	}
	return errs.Wrap(err, errs.CodeForbidden, "not permitted")
}

func notFound(err error, detail string) error {
	if errs.CodeOf(err) == errs.CodeNotFound {
		return errs.New(errs.CodeNotFound, detail)
	}
	return dbErr("load", err)
}

// ReverseRequest asks an operator to reverse a COMPLETED transfer.
type ReverseRequest struct {
	TransferID TransferID
	// ReasonCode is a stable code (e.g. FRAUD_CONFIRMED); it becomes the
	// compensation journal's reason_code.
	ReasonCode    string
	Reason        string
	CorrelationID string
}

// Reverse returns a COMPLETED transfer's value to its sender with a
// COMPENSATION journal (reversal_of = the original journal, the reason code
// in its metadata): sender Dr WALLET / Cr TRANSFER_OUT, recipient
// Dr TRANSFER_IN / Cr WALLET. History is never edited. Only an OPERATOR
// holding ledger:post_correction may reverse.
//
// It fails closed: if the recipient no longer holds the funds (they are
// spent, reserved or held) it is refused with CONFLICT and nothing changes;
// a balance never goes negative to make a reversal fit. Reversing a REVERSED
// transfer returns it unchanged. A kill switch stops new transfers, never
// the correction of an old one, so Reverse does not consult the capability
// gate.
func (s *Service) Reverse(ctx context.Context, tx pgx.Tx, p security.Principal, req ReverseRequest) (Transfer, error) {
	if tx == nil {
		return Transfer{}, errs.New(errs.CodeInternal, "transfers: Reverse requires a transaction")
	}
	if p.IsAgent() || p.ActorType != security.ActorOperator {
		return Transfer{}, errs.New(errs.CodeForbidden, "only an operator reverses a transfer").WithField("actor_type", string(p.ActorType))
	}
	ctx = security.WithPrincipal(ctx, p)
	if err := security.RequireAt(ctx, security.PermLedgerPostCorrection, s.clk.Now); err != nil {
		return Transfer{}, securityErr(err)
	}
	if !validReasonCode(req.ReasonCode) {
		return Transfer{}, errs.New(errs.CodeValidationFailed, "a reversal needs a stable reason code (^[A-Z][A-Z0-9_]{2,63}$)")
	}
	if r := strings.TrimSpace(req.Reason); r == "" || len(r) > 500 {
		return Transfer{}, errs.New(errs.CodeValidationFailed, "a reversal needs a reason of 1..500 bytes")
	}
	now := s.clk.Now().UTC()
	actor := econ.Actor{Type: security.ActorOperator, ID: p.SubjectID}

	peek, err := getTransfer(ctx, tx, req.TransferID, false)
	if err != nil {
		return Transfer{}, err
	}
	// The reversal debits the recipient, so it takes the recipient's transfer
	// lock: it cannot interleave with a transfer the recipient is sending.
	if err := lockAccount(ctx, tx, peek.RecipientAccountID); err != nil {
		return Transfer{}, err
	}
	t, err := getTransfer(ctx, tx, req.TransferID, true)
	if err != nil {
		return Transfer{}, err
	}
	switch t.Status {
	case StatusReversed:
		return t, nil
	case StatusCompleted:
	default:
		return Transfer{}, errs.Newf(errs.CodeInvalidStateTransition, "a %s transfer cannot be reversed", t.Status).
			WithField("transfer_id", t.ID.String())
	}
	if t.JournalTransactionID == nil || t.EffectID == nil {
		return Transfer{}, errs.New(errs.CodeInternal, "a COMPLETED transfer without its journal or effect").WithField("transfer_id", t.ID.String())
	}
	// Recorded first, which also takes the sender's audit stream before any
	// ledger row: every transaction touching this transfer takes them in the
	// same order.
	if err := s.auditTransfer(ctx, tx, t, "transfers.transfer.reversal_requested", actor, req.Reason, "",
		map[string]string{"transfer_id": t.ID.String(), "reason_code": req.ReasonCode}, now); err != nil {
		return Transfer{}, err
	}
	avail, err := s.capital.Availability(ctx, tx, t.RecipientAccountID.String(), t.AssetID, now)
	if err != nil {
		return Transfer{}, err
	}
	rsv, err := s.capital.Reserve(ctx, tx, capital.ReserveRequest{
		AccountID: t.RecipientAccountID.String(), AssetID: t.AssetID, Quantity: t.Quantity,
		ActorType: security.ActorOperator, ActorID: p.SubjectID, IdempotencyKey: "p2p-reversal:" + t.ID.String(),
		TTL: ReservationTTL, Reason: "reversal of simulated transfer " + t.ID.String(),
	})
	if err != nil {
		if errs.CodeOf(err) == errs.CodeInsufficientBuyingPower {
			return Transfer{}, errs.Wrap(err, errs.CodeConflict,
				"the recipient no longer holds the transferred funds; the reversal is refused and nothing changed").
				WithField("transfer_id", t.ID.String()).
				WithField("rejection_code", "TRANSFER_REVERSAL_FUNDS_UNAVAILABLE").
				WithField("recipient_available", avail.Available.String()).
				WithField("required", t.Quantity.String())
		}
		return Transfer{}, err
	}
	post, err := s.ledger.Post(ctx, tx, reversalPosting(t, req.ReasonCode, req.CorrelationID, now))
	if err != nil {
		return Transfer{}, err
	}
	if _, err := s.capital.Consume(ctx, tx, rsv.ID, t.Quantity, 0, ""); err != nil {
		return Transfer{}, err
	}
	ref, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "p2p/" + t.ID.String() + "/reversal", Document: reversalEvidence{
		Schema: "nodal.transfers.reversal/v1", TransferID: t.ID.String(), IntentID: t.IntentID.String(),
		OriginalJournal: t.JournalTransactionID.String(), ReversalJournal: post.TransactionID.String(),
		EffectID: t.EffectID.String(), ReasonCode: req.ReasonCode, Reason: req.Reason, Operator: p.SubjectID,
		Sender: t.SenderAccountID.String(), Recipient: t.RecipientAccountID.String(), Asset: t.AssetID.String(),
		Quantity: t.Quantity, RecipientAvailableBefore: avail.Available, ReservationID: rsv.ID.String(), ReversedAt: now,
	}})
	if err != nil {
		return Transfer{}, err
	}
	if _, err := s.effects.Compensate(ctx, tx, *t.EffectID, &post.TransactionID, actor, req.ReasonCode+": "+req.Reason); err != nil {
		return Transfer{}, err
	}
	out, err := s.markReversed(ctx, tx, t, post.TransactionID, req.ReasonCode, actor, req.Reason, ref, now)
	if err != nil {
		return Transfer{}, err
	}
	recipient, err := s.accounts.Get(ctx, tx, t.RecipientAccountID)
	if err != nil {
		return Transfer{}, dbErr("load recipient", err)
	}
	asset, err := s.assets.Get(ctx, tx, t.AssetID)
	if err != nil {
		return Transfer{}, dbErr("load asset", err)
	}
	if err := s.notifier.TransferReversed(ctx, tx, Notice{
		TransferID: t.ID, RecipientAccountID: recipient.ID, RecipientUserID: recipient.OwnerUserID,
		AssetID: asset.ID, AssetSymbol: asset.Symbol, AssetDecimals: asset.Decimals, Quantity: t.Quantity,
		CorrelationID: req.CorrelationID,
	}); err != nil {
		return Transfer{}, err
	}
	return out, nil
}

// Get returns one transfer to one of its parties (or an operator who may read
// any account). Anyone else is told it does not exist.
func (s *Service) Get(ctx context.Context, q db.Querier, p security.Principal, tid TransferID) (Transfer, error) {
	t, err := getTransfer(ctx, q, tid, false)
	if err != nil {
		return Transfer{}, err
	}
	if !s.mayRead(p, t.SenderAccountID, t.RecipientAccountID) {
		return Transfer{}, errs.New(errs.CodeNotFound, "transfer not found").WithField("transfer_id", tid.String())
	}
	return t, nil
}

// ListForAccount pages the transfers an account sent or received, newest
// first. The cursor is the id of the last transfer of the previous page.
func (s *Service) ListForAccount(ctx context.Context, q db.Querier, p security.Principal, account accounts.AccountID, cursor string, limit int) ([]Transfer, string, error) {
	if !s.mayRead(p, account, account) {
		return nil, "", errs.New(errs.CodeForbidden, "not permitted to read this account's transfers")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var rows pgx.Rows
	var err error
	if cursor == "" {
		rows, err = q.Query(ctx, `SELECT `+transferColumns+` FROM internal_transfers
			WHERE sender_account_id = $1 OR recipient_account_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2`, account, limit+1)
	} else {
		cid, perr := ParseTransferID(cursor)
		if perr != nil {
			return nil, "", errs.New(errs.CodeValidationFailed, "invalid cursor")
		}
		rows, err = q.Query(ctx, `SELECT `+transferColumns+` FROM internal_transfers
			WHERE (sender_account_id = $1 OR recipient_account_id = $1)
			  AND (created_at, id) < (SELECT created_at, id FROM internal_transfers WHERE id = $2)
			ORDER BY created_at DESC, id DESC LIMIT $3`, account, cid, limit+1)
	}
	if err != nil {
		return nil, "", dbErr("list transfers", err)
	}
	defer rows.Close()
	var out []Transfer
	for rows.Next() {
		t, err := scanTransfer(rows)
		if err != nil {
			return nil, "", dbErr("scan transfer", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, "", dbErr("list transfers", err)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = out[len(out)-1].ID.String()
	}
	return out, next, nil
}

func (s *Service) mayRead(p security.Principal, a, b accounts.AccountID) bool {
	if p.Validate() != nil {
		return false
	}
	if p.OwnsAccount(a.String()) || p.OwnsAccount(b.String()) {
		return true
	}
	return !p.IsAgent() && p.Has(security.PermAccountReadAny, s.clk.Now())
}

// Handle is a registered recipient handle.
type Handle struct {
	ID        string
	AccountID accounts.AccountID
	Handle    string
	CreatedAt time.Time
}

// RegisterHandle gives an account the handle other customers can pay it by.
// Only the account's owner registers it; an account has at most one handle
// and a handle names at most one account, for good (handles are never
// reassigned, so a payment to a handle can never reach a later holder).
func (s *Service) RegisterHandle(ctx context.Context, tx pgx.Tx, p security.Principal, account accounts.AccountID, handle string) (Handle, error) {
	userID, err := s.requireOwner(ctx, p, account)
	if err != nil {
		return Handle{}, err
	}
	h, err := NormalizeHandle(handle)
	if err != nil {
		return Handle{}, err
	}
	out := Handle{AccountID: account, Handle: h}
	err = tx.QueryRow(ctx, `INSERT INTO transfer_handles (id, account_id, handle, registered_by_user_id)
		VALUES ($1,$2,$3,$4) RETURNING id::text, created_at`, NewTransferID(), account, h, userID).Scan(&out.ID, &out.CreatedAt)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return Handle{}, errs.Wrap(err, errs.CodeConflict, "that handle is taken, or this account already has one")
		}
		return Handle{}, dbErr("register handle", err)
	}
	out.CreatedAt = out.CreatedAt.UTC()
	return out, nil
}

// ResolveHandle returns the account a handle names.
func (s *Service) ResolveHandle(ctx context.Context, q db.Querier, handle string) (accounts.AccountID, error) {
	h, err := NormalizeHandle(handle)
	if err != nil {
		return accounts.AccountID{}, err
	}
	var account accounts.AccountID
	err = q.QueryRow(ctx, `SELECT account_id FROM transfer_handles WHERE handle = $1`, h).Scan(&account)
	if errors.Is(err, pgx.ErrNoRows) {
		return accounts.AccountID{}, errs.New(errs.CodeNotFound, "no account has that handle")
	}
	if err != nil {
		return accounts.AccountID{}, dbErr("resolve handle", err)
	}
	return account, nil
}

// SetRecipientRule records the sender's rule about one recipient: ALLOW puts
// it on the sender's allowlist, BLOCK refuses every transfer to it, CLEAR
// withdraws either. Rules are append-only; the latest is in force.
func (s *Service) SetRecipientRule(ctx context.Context, tx pgx.Tx, p security.Principal, sender, recipient accounts.AccountID, rule RuleKind, reason string) error {
	userID, err := s.requireOwner(ctx, p, sender)
	if err != nil {
		return err
	}
	if !rule.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "unknown rule %q", rule)
	}
	if sender == recipient {
		return errs.New(errs.CodeValidationFailed, "a rule names another account")
	}
	if len(reason) > 500 {
		return errs.New(errs.CodeValidationFailed, "reason is at most 500 bytes")
	}
	if _, err := s.accounts.Get(ctx, tx, recipient); err != nil {
		return notFound(err, "recipient not found")
	}
	_, err = tx.Exec(ctx, `INSERT INTO transfer_recipient_rules
		(id, sender_account_id, recipient_account_id, rule, set_by_user_id, reason, effective_at)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7)`, NewTransferID(), sender, recipient, rule, userID, reason, s.clk.Now().UTC())
	if err != nil {
		return dbErr("record recipient rule", err)
	}
	return nil
}

// SetSenderMode records whether the sender may pay anyone (OPEN) or only
// allowlisted recipients (ALLOWLIST_ONLY).
func (s *Service) SetSenderMode(ctx context.Context, tx pgx.Tx, p security.Principal, account accounts.AccountID, mode SenderMode) error {
	userID, err := s.requireOwner(ctx, p, account)
	if err != nil {
		return err
	}
	if !mode.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "unknown sender mode %q", mode)
	}
	_, err = tx.Exec(ctx, `INSERT INTO transfer_sender_modes (id, account_id, mode, set_by_user_id, effective_at)
		VALUES ($1,$2,$3,$4,$5)`, NewTransferID(), account, mode, userID, s.clk.Now().UTC())
	if err != nil {
		return dbErr("record sender mode", err)
	}
	return nil
}

func (s *Service) requireOwner(ctx context.Context, p security.Principal, account accounts.AccountID) (accounts.UserID, error) {
	if p.IsAgent() || p.ActorType != security.ActorUser {
		return accounts.UserID{}, errs.New(errs.CodeForbidden, "only the customer who owns the account configures its transfers")
	}
	userID, err := accounts.ParseUserID(p.SubjectID)
	if err != nil {
		return accounts.UserID{}, errs.New(errs.CodeForbidden, "the principal does not name a user")
	}
	if err := security.RequireAccountOwner(security.WithPrincipal(ctx, p), account.String()); err != nil {
		return accounts.UserID{}, securityErr(err)
	}
	return userID, nil
}
