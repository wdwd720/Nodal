package predmarket

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Held payouts.
//
// A settlement or void never waits for one holder. When the route or Risk V2
// refuses a holder's own payout -- a FROZEN or CLOSED account, an unverified
// identity, a jurisdiction the router denies -- the holder's shares are
// retired with everyone else's and the amount is HELD:
//
//   - the ledger: the holder's share legs are posted (customer Cr WALLET / Dr
//     TRADING_OUTFLOW, facility Dr PREDICTION_SHARES_OUTSTANDING / Cr
//     PREDICTION_SHARES_ISSUED), and no collateral moves: the amount stays in
//     the market's PREDICTION_ESCROW (and PREDICTION_COLLATERAL), the
//     market's own segregated book;
//   - the record: a prediction_held_payouts row names the holder, the amount,
//     the refused intent and its reasons, and the settlement whose total_held
//     it is part of.
//
// Conservation holds exactly at every step: escrow before = Σ delivered + Σ
// held + residual (the settlement record states it, the database checks the
// held rows sum to total_held), and afterwards escrow = collateral = residual
// + Σ unclaimed held. The holder claims the payout with ClaimHeldPayout once
// eligible; the claim is an economic intent of its own, evaluated by Risk V2
// with the holder's facts as they are then.

// ClaimRequest claims the payout a market holds for an account.
type ClaimRequest struct {
	MarketID  MarketID
	AccountID accounts.AccountID
	// IdempotencyKey makes the claim idempotent on (account, key).
	IdempotencyKey string
	// ActorType and ActorID say who asked: the holder (USER) or a service
	// acting for them (SERVICE). Never an agent or an operator.
	ActorType     security.ActorType
	ActorID       string
	CorrelationID string
}

func (r ClaimRequest) validate() error {
	var p []string
	if r.MarketID.IsZero() {
		p = append(p, "market_id required")
	}
	if r.AccountID.IsZero() {
		p = append(p, "account_id required")
	}
	allowed := false
	for _, t := range ClaimActorTypes() {
		allowed = allowed || t == r.ActorType
	}
	if !allowed {
		p = append(p, "a held payout is claimed by its USER or a SERVICE acting for them")
	}
	if strings.TrimSpace(r.ActorID) == "" {
		p = append(p, "actor_id required")
	}
	if k := r.IdempotencyKey; k == "" || strings.TrimSpace(k) != k || len(k) > 256 || !utf8.ValidString(k) {
		p = append(p, "idempotency key must be 1..256 bytes of UTF-8 without surrounding whitespace")
	}
	if len(p) > 0 {
		return errs.New(errs.CodeValidationFailed, "predmarket: invalid claim").WithField("problems", p)
	}
	return nil
}

// ClaimResult is a held payout after a claim, and the economic action that
// claimed it.
type ClaimResult struct {
	HeldPayout HeldPayout
	Action     ActionResult
}

// ClaimHeldPayout delivers the payout a settled (or voided) market holds for
// an account: a RESOLVE_PREDICTION_POSITION intent, routed, authorized and
// evaluated by Risk V2 with the holder's facts as they are now -- an account
// that is still FROZEN, or an identity still unverified, is refused and the
// payout stays held -- then the held amount moves from the market's escrow to
// the holder's wallet, exactly once (the effect key is the held payout's),
// and the claim is recorded. The claim of the market's last held payout halts
// its instrument. A replay of the same request returns its first outcome.
func (s *Service) ClaimHeldPayout(ctx context.Context, tx pgx.Tx, req ClaimRequest) (ClaimResult, error) {
	if err := req.validate(); err != nil {
		return ClaimResult{}, err
	}
	// The market row serializes the claims of one market (and the halt that
	// follows the last one).
	m, err := s.lockMarket(ctx, tx, req.MarketID, lockUpdate)
	if err != nil {
		return ClaimResult{}, err
	}
	h, found, err := s.HeldPayoutOf(ctx, tx, m.ID, req.AccountID)
	if err != nil {
		return ClaimResult{}, err
	}
	if !found {
		return ClaimResult{}, errs.New(errs.CodeNotFound, "predmarket: this market holds no payout for this account").
			WithField("market_id", m.ID.String())
	}
	if h.Claimed() {
		// Only the request that claimed it may ask again (a replay, answered
		// from its intent); any other request is a second claim.
		first, err := s.intents.Get(ctx, tx, h.Claim.IntentID)
		if err != nil {
			return ClaimResult{}, err
		}
		if first.IdempotencyKey != req.IdempotencyKey {
			return ClaimResult{}, errs.New(errs.CodeConflict, "predmarket: this held payout has already been claimed").
				WithField("held_payout_id", h.ID.String()).WithField("claim_id", h.Claim.ID.String())
		}
	}
	facts, err := s.facts.Facts(ctx, tx, req.AccountID)
	if err != nil {
		return ClaimResult{}, err
	}
	inst, err := s.instr.Get(ctx, tx, m.InstrumentID)
	if err != nil {
		return ClaimResult{}, dbErr("load market instrument", err)
	}
	a, err := s.claimAction(ctx, tx, m, h, req, facts, inst.Status)
	if err != nil {
		return ClaimResult{}, err
	}
	r, err := s.run(ctx, tx, a, m.Mode)
	if err != nil {
		return ClaimResult{Action: r}, err
	}
	stored, _, err := s.HeldPayoutOf(ctx, tx, m.ID, req.AccountID)
	if err != nil {
		return ClaimResult{Action: r}, err
	}
	return ClaimResult{HeldPayout: stored, Action: r}, nil
}

func (s *Service) claimAction(ctx context.Context, tx pgx.Tx, m Market, h HeldPayout, req ClaimRequest, facts AccountFacts,
	instrumentStatus assets.Status) (action, error) {
	effectKind := "PREDICTION_PAYOUT_CLAIM"
	if h.Kind == SettlementRefund {
		effectKind = "PREDICTION_REFUND_CLAIM"
	}
	key := "predmarket:claim:" + h.ID.String()
	amount := h.Amount
	collateral := m.CollateralAssetID
	notional, err := usd(amount, money.RoundUp)
	if err != nil {
		return action{}, err
	}
	body, err := json.Marshal(map[string]string{
		"market_id": m.ID.String(), "market_symbol": m.Symbol, "action": "CLAIM_HELD_" + string(h.Kind),
		"held_payout_id": h.ID.String(), "settlement_id": h.SettlementID.String(), "amount": amount.String(),
	})
	if err != nil {
		return action{}, errs.Wrap(err, errs.CodeInternal, "predmarket: encode claim terms")
	}
	in := econ.Intent{
		AccountID: req.AccountID, Kind: econ.KindResolvePredictionPosition, Mode: m.Mode, Capability: gates.PredictionMarkets,
		ActorType: req.ActorType, ActorID: req.ActorID, InstrumentID: m.InstrumentID.String(), AssetID: &collateral,
		Quantity: &amount, Terms: body, IdempotencyKey: req.IdempotencyKey, CorrelationID: req.CorrelationID,
	}
	return action{
		intent: in, facts: facts, mover: serviceActor(),
		subject:    settlement.Subject{Type: settlement.SubjectInstrument, ID: m.InstrumentID.String(), AssetID: collateral.String()},
		effectKey:  func(econ.Intent) string { return key },
		effectKind: effectKind,
		riskInput: func(it econ.Intent) (risk.AdvancedInput, error) {
			kills, err := s.killSwitches(ctx, tx)
			if err != nil {
				return risk.AdvancedInput{}, err
			}
			// The escrow holds exactly this amount for this holder.
			have, err := usd(amount, money.RoundDown)
			if err != nil {
				return risk.AdvancedInput{}, err
			}
			now := s.now()
			return risk.AdvancedInput{
				Stage: risk.StageFinal,
				Intent: risk.AdvancedIntent{
					ID: it.ID.String(), Kind: string(it.Kind), AccountID: it.AccountID.String(), ActorType: string(it.ActorType),
					Capability: string(it.Capability), Mode: string(it.Mode), InstrumentID: m.InstrumentID.String(),
					InstrumentKind: "EVENT_OUTCOME", NotionalUSD: notional, RiskIncreasing: false,
				},
				Identity: &risk.IdentitySnapshot{Verified: facts.Verification.AtLeast(valuedomain.VerificationNodalIdentity)},
				Account:  &risk.AdvancedAccountSnapshot{Status: string(facts.Status)},
				// The market instrument stays CLOSE_ONLY while a held payout
				// awaits its claim: the claim window is that session.
				Instrument:   &risk.InstrumentSnapshot{Status: string(instrumentStatus), SessionOpen: true, Expired: !now.Before(m.CloseAt)},
				Balance:      &risk.BalanceSnapshot{Available: have, Required: have},
				KillSwitches: kills,
				Now:          now,
			}, nil
		},
		precheck: func(tx pgx.Tx) error {
			if m.Status != StatusSettled && m.Status != StatusVoided {
				return errs.Newf(errs.CodeInvalidStateTransition, "predmarket: market %s is %s; a held payout is claimed after settlement", m.Symbol, m.Status)
			}
			cur, found, err := s.HeldPayoutOf(ctx, tx, m.ID, req.AccountID)
			if err != nil {
				return err
			}
			if !found || cur.ID != h.ID {
				return errs.New(errs.CodeInternal, "predmarket: the held payout changed under its claim")
			}
			if cur.Claimed() {
				return errs.New(errs.CodeConflict, "predmarket: this held payout has already been claimed").
					WithField("held_payout_id", h.ID.String()).WithField("claim_id", cur.Claim.ID.String())
			}
			_, err = s.requireReconciled(ctx, tx, m)
			return err
		},
		request: func(it econ.Intent) any {
			return map[string]string{
				"market_id": m.ID.String(), "account_id": it.AccountID.String(), "held_payout_id": h.ID.String(),
				"amount": amount.String(), "intent_id": it.ID.String(),
			}
		},
		posting: func(it econ.Intent) (ledger.Posting, error) {
			return ClaimPosting(m, h.Kind, it.AccountID, amount, postingMeta{
				Key: key, Reference: ledger.FinancialEventReference{Type: "prediction_held_claim", ID: it.ID.String()},
				At: s.now(), Description: "prediction market " + m.Symbol + " held " + strings.ToLower(string(h.Kind)) + " claimed",
			})
		},
		evidence: func(it econ.Intent, d risk.AdvancedDecision, r settlement.Route, _ []string, journal ledger.TransactionID) any {
			return map[string]any{
				"kind": effectKind, "market_id": m.ID.String(), "market_symbol": m.Symbol, "intent_id": it.ID.String(),
				"account_id": it.AccountID.String(), "held_payout_id": h.ID.String(), "settlement_id": h.SettlementID.String(),
				"amount": amount.String(), "collateral_asset_id": collateral.String(), "held_reason_codes": h.ReasonCodes,
				"actor":                  map[string]any{"type": string(it.ActorType), "id": it.ActorID},
				"route":                  map[string]any{"permitted": r.Permitted, "product": r.Product, "executor": string(r.Executor)},
				"risk_decision_hash":     d.Hash,
				"journal_transaction_id": journal.String(),
			}
		},
		afterApply: func(tx pgx.Tx, res ActionResult) error {
			c := HeldPayoutClaim{
				ID: id.New[heldClaimKind](), HeldPayoutID: h.ID, MarketID: m.ID, AccountID: h.AccountID, Amount: amount,
				IntentID: res.Intent.ID, EffectID: res.Effect.ID, JournalTransactionID: res.JournalTransactionID,
				ClaimedBy:   econ.Actor{Type: res.Intent.ActorType, ID: res.Intent.ActorID},
				EvidenceRef: res.Effect.EvidenceRef, EvidenceSHA256: res.Effect.EvidenceSHA256, ClaimedAt: s.now(),
			}
			if err := insertHeldClaim(ctx, tx, c); err != nil {
				return err
			}
			left, err := unclaimedHeld(ctx, tx, m.ID)
			if err != nil {
				return err
			}
			if left == 0 {
				return s.setInstrumentStatus(ctx, tx, m.InstrumentID, assets.StatusHalted, serviceActor(),
					"prediction market "+m.Symbol+": the last held payout was claimed")
			}
			return nil
		},
		postcheck: func(tx pgx.Tx) error { _, err := s.requireReconciled(ctx, tx, m); return err },
	}, nil
}
