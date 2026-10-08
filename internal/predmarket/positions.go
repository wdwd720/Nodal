package predmarket

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// PositionRequest asks to issue or redeem complete sets on one account.
type PositionRequest struct {
	MarketID  MarketID
	AccountID accounts.AccountID
	// Quantity is the number of complete sets in base units: a positive
	// multiple of 1,000,000 (one set = one share of every outcome = one
	// collateral unit).
	Quantity       money.Quantity
	IdempotencyKey string
	// ActorType and ActorID say who asked: the account's owner (USER), a
	// service acting for them (SERVICE), or an agent (AGENT). The effect is
	// always applied by this service.
	ActorType security.ActorType
	ActorID   string
	// AgentID, AgentAuthority and Authority describe an AGENT request: the
	// agent, the authority level it acted under, and the delegation facts
	// the caller resolved for it. Risk V2 refuses an agent request without
	// them.
	AgentID        string
	AgentAuthority agentauthority.Level
	Authority      *risk.AuthoritySnapshot
	CorrelationID  string
}

func (r PositionRequest) validate() error {
	var p []string
	if r.MarketID.IsZero() {
		p = append(p, "market_id required")
	}
	if r.AccountID.IsZero() {
		p = append(p, "account_id required")
	}
	switch r.ActorType {
	case security.ActorUser, security.ActorService, security.ActorAgent:
	default:
		p = append(p, "a position is requested by its USER, a SERVICE acting for them, or an AGENT")
	}
	if strings.TrimSpace(r.ActorID) == "" {
		p = append(p, "actor_id required")
	}
	if len(p) > 0 {
		return errs.New(errs.CodeValidationFailed, "predmarket: invalid position request").WithField("problems", p)
	}
	return ValidateSetQuantity(r.Quantity)
}

func (r PositionRequest) intent(m Market, kind econ.IntentKind, verb string) (econ.Intent, error) {
	q := r.Quantity
	sets, err := q.Div(ShareUnit(), money.RoundExact)
	if err != nil {
		return econ.Intent{}, err
	}
	terms, err := json.Marshal(map[string]string{
		"market_id": m.ID.String(), "market_symbol": m.Symbol, "action": verb, "complete_sets": sets.String(),
		"quantity": q.String(),
	})
	if err != nil {
		return econ.Intent{}, errs.Wrap(err, errs.CodeInternal, "predmarket: encode intent terms")
	}
	collateral := m.CollateralAssetID
	return econ.Intent{
		AccountID: r.AccountID, Kind: kind, Mode: m.Mode, Capability: gates.PredictionMarkets,
		ActorType: r.ActorType, ActorID: r.ActorID, AgentID: r.AgentID, InstrumentID: m.InstrumentID.String(),
		AssetID: &collateral, Quantity: &q, Terms: terms, IdempotencyKey: r.IdempotencyKey, CorrelationID: r.CorrelationID,
	}, nil
}

// baseRisk is the part of a Risk V2 snapshot every position action shares.
func (s *Service) baseRisk(ctx context.Context, tx pgx.Tx, it econ.Intent, m Market, facts AccountFacts, riskIncreasing bool,
	notional money.USD, authority *risk.AuthoritySnapshot) (risk.AdvancedInput, error) {
	kills, err := s.killSwitches(ctx, tx)
	if err != nil {
		return risk.AdvancedInput{}, err
	}
	inst, err := s.instr.Get(ctx, tx, m.InstrumentID)
	if err != nil {
		return risk.AdvancedInput{}, dbErr("load market instrument", err)
	}
	now := s.now()
	return risk.AdvancedInput{
		Stage: risk.StageFinal,
		Intent: risk.AdvancedIntent{
			ID: it.ID.String(), Kind: string(it.Kind), AccountID: it.AccountID.String(), ActorType: string(it.ActorType),
			AgentID: it.AgentID, Capability: string(it.Capability), Mode: string(it.Mode), InstrumentID: m.InstrumentID.String(),
			InstrumentKind: "EVENT_OUTCOME", NotionalUSD: notional, RiskIncreasing: riskIncreasing,
		},
		Identity:  &risk.IdentitySnapshot{Verified: facts.Verification.AtLeast(valuedomain.VerificationNodalIdentity)},
		Account:   &risk.AdvancedAccountSnapshot{Status: string(facts.Status)},
		Authority: authority,
		Instrument: &risk.InstrumentSnapshot{
			Status: string(inst.Status), SessionOpen: m.tradingOpen(now), Expired: !now.Before(m.CloseAt),
		},
		KillSwitches: kills,
		Now:          now,
	}, nil
}

// requireTrading refuses unless the market is OPEN and the clock is inside
// its trading window.
func (s *Service) requireTrading(m Market) error {
	if !m.tradingOpen(s.now()) {
		return errs.Newf(errs.CodeInvalidStateTransition,
			"predmarket: market %s is %s; complete sets are issued and redeemed only while it is OPEN and before its close time",
			m.Symbol, m.Status).WithField("market_id", m.ID.String())
	}
	return nil
}

// Issue sells complete sets: q base units of collateral buy q base units of
// every outcome share (PREDICTION_SET_ISSUANCE), the collateral going into
// the market's escrow. It is a CREATE_PREDICTION_POSITION intent, risk
// increasing, evaluated by Risk V2 on the market instrument (status, session,
// expiry), the account, identity, the contractual set price and the
// account's available collateral, and backed by a capital reservation of the
// collateral, so concurrent issuances can never spend the same collateral
// twice. The request is idempotent on (account, idempotency key).
func (s *Service) Issue(ctx context.Context, tx pgx.Tx, req PositionRequest) (ActionResult, error) {
	if err := req.validate(); err != nil {
		return ActionResult{}, err
	}
	m, err := s.lockMarket(ctx, tx, req.MarketID, lockShare)
	if err != nil {
		return ActionResult{}, err
	}
	facts, err := s.facts.Facts(ctx, tx, req.AccountID)
	if err != nil {
		return ActionResult{}, err
	}
	in, err := req.intent(m, econ.KindCreatePredictionPosition, "ISSUE_COMPLETE_SETS")
	if err != nil {
		return ActionResult{}, err
	}
	q := req.Quantity
	notional, err := usd(q, money.RoundUp)
	if err != nil {
		return ActionResult{}, err
	}
	key := func(it econ.Intent) string { return "predmarket:issue:" + it.ID.String() }
	a := action{
		intent: in, facts: facts, mover: serviceActor(), agentAuthority: req.AgentAuthority,
		subject:    settlement.Subject{Type: settlement.SubjectInstrument, ID: m.InstrumentID.String(), AssetID: m.CollateralAssetID.String()},
		effectKey:  key,
		effectKind: "PREDICTION_SET_ISSUANCE",
		riskInput: func(it econ.Intent) (risk.AdvancedInput, error) {
			ri, err := s.baseRisk(ctx, tx, it, m, facts, true, notional, req.Authority)
			if err != nil {
				return risk.AdvancedInput{}, err
			}
			avail, err := s.capital.Availability(ctx, tx, it.AccountID.String(), m.CollateralAssetID, ri.Now)
			if err != nil {
				return risk.AdvancedInput{}, err
			}
			have, err := usd(avail.Available, money.RoundDown)
			if err != nil {
				return risk.AdvancedInput{}, err
			}
			ri.Balance = &risk.BalanceSnapshot{Available: have, Required: notional}
			// The price a complete set is issued at is fixed by the contract,
			// one collateral unit, and read at evaluation time: it cannot be
			// stale and has no second source to diverge from.
			ri.Oracle = &risk.OracleSnapshot{Available: true, ObservedAt: ri.Now, DivergenceBPS: 0}
			return ri, nil
		},
		precheck: func(tx pgx.Tx) error {
			if err := s.requireTrading(m); err != nil {
				return err
			}
			_, err := s.requireReconciled(ctx, tx, m)
			return err
		},
		reserves: []reserveSpec{{Asset: m.CollateralAssetID, Quantity: q, Suffix: "collateral"}},
		request: func(it econ.Intent) any {
			return map[string]string{"market_id": m.ID.String(), "account_id": it.AccountID.String(), "quantity": q.String(), "intent_id": it.ID.String()}
		},
		posting: func(it econ.Intent) (ledger.Posting, error) {
			return IssuancePosting(m, it.AccountID, q, postingMeta{
				Key: key(it), Reference: ledger.FinancialEventReference{Type: "prediction_set_issuance", ID: it.ID.String()},
				At: s.now(), Description: "complete-set issuance on prediction market " + m.Symbol,
			})
		},
		evidence:  positionEvidence(m, "PREDICTION_SET_ISSUANCE", q),
		postcheck: func(tx pgx.Tx) error { _, err := s.requireReconciled(ctx, tx, m); return err },
	}
	return s.run(ctx, tx, a, m.Mode)
}

// Redeem buys back complete sets: q base units of every outcome share return
// q base units of collateral from the escrow (PREDICTION_SET_REDEMPTION). It
// is a RESOLVE_PREDICTION_POSITION intent that reduces risk, allowed only
// while the market is OPEN and before its close time; after close, shares
// leave only through settlement or a void refund. The shares are reserved
// before they are retired, so a redemption can never consume shares an open
// order already holds.
func (s *Service) Redeem(ctx context.Context, tx pgx.Tx, req PositionRequest) (ActionResult, error) {
	if err := req.validate(); err != nil {
		return ActionResult{}, err
	}
	m, err := s.lockMarket(ctx, tx, req.MarketID, lockShare)
	if err != nil {
		return ActionResult{}, err
	}
	facts, err := s.facts.Facts(ctx, tx, req.AccountID)
	if err != nil {
		return ActionResult{}, err
	}
	in, err := req.intent(m, econ.KindResolvePredictionPosition, "REDEEM_COMPLETE_SETS")
	if err != nil {
		return ActionResult{}, err
	}
	q := req.Quantity
	notional, err := usd(q, money.RoundUp)
	if err != nil {
		return ActionResult{}, err
	}
	reserves := make([]reserveSpec, 0, len(m.Outcomes))
	for _, o := range m.Outcomes {
		reserves = append(reserves, reserveSpec{Asset: o.ShareAssetID, Quantity: q, Suffix: "outcome-" + o.Symbol})
	}
	key := func(it econ.Intent) string { return "predmarket:redeem:" + it.ID.String() }
	a := action{
		intent: in, facts: facts, mover: serviceActor(), agentAuthority: req.AgentAuthority,
		subject:    settlement.Subject{Type: settlement.SubjectInstrument, ID: m.InstrumentID.String(), AssetID: m.CollateralAssetID.String()},
		effectKey:  key,
		effectKind: "PREDICTION_SET_REDEMPTION",
		riskInput: func(it econ.Intent) (risk.AdvancedInput, error) {
			ri, err := s.baseRisk(ctx, tx, it, m, facts, false, notional, req.Authority)
			if err != nil {
				return risk.AdvancedInput{}, err
			}
			// The complete sets the account can return: its smallest available
			// holding across the outcomes, each set worth one unit.
			var sets money.Quantity
			for i, o := range m.Outcomes {
				avail, err := s.capital.Availability(ctx, tx, it.AccountID.String(), o.ShareAssetID, ri.Now)
				if err != nil {
					return risk.AdvancedInput{}, err
				}
				if i == 0 || avail.Available.Cmp(sets) < 0 {
					sets = avail.Available
				}
			}
			have, err := usd(sets, money.RoundDown)
			if err != nil {
				return risk.AdvancedInput{}, err
			}
			ri.Balance = &risk.BalanceSnapshot{Available: have, Required: notional}
			return ri, nil
		},
		precheck: func(tx pgx.Tx) error {
			if err := s.requireTrading(m); err != nil {
				return err
			}
			_, err := s.requireReconciled(ctx, tx, m)
			return err
		},
		reserves: reserves,
		request: func(it econ.Intent) any {
			return map[string]string{"market_id": m.ID.String(), "account_id": it.AccountID.String(), "quantity": q.String(), "intent_id": it.ID.String()}
		},
		posting: func(it econ.Intent) (ledger.Posting, error) {
			return RedemptionPosting(m, it.AccountID, q, postingMeta{
				Key: key(it), Reference: ledger.FinancialEventReference{Type: "prediction_set_redemption", ID: it.ID.String()},
				At: s.now(), Description: "complete-set redemption on prediction market " + m.Symbol,
			})
		},
		evidence:  positionEvidence(m, "PREDICTION_SET_REDEMPTION", q),
		postcheck: func(tx pgx.Tx) error { _, err := s.requireReconciled(ctx, tx, m); return err },
	}
	return s.run(ctx, tx, a, m.Mode)
}

func positionEvidence(m Market, kind string, q money.Quantity) func(econ.Intent, risk.AdvancedDecision, settlement.Route, []string, ledger.TransactionID) any {
	return func(it econ.Intent, d risk.AdvancedDecision, r settlement.Route, reservations []string, journal ledger.TransactionID) any {
		outcomes := make([]string, 0, len(m.Outcomes))
		for _, o := range m.Outcomes {
			outcomes = append(outcomes, o.Symbol+"="+o.ShareAssetID.String())
		}
		return map[string]any{
			"kind": kind, "market_id": m.ID.String(), "market_symbol": m.Symbol, "intent_id": it.ID.String(),
			"account_id": it.AccountID.String(), "actor": map[string]any{"type": string(it.ActorType), "id": it.ActorID},
			"mode": string(it.Mode), "quantity": q.String(), "collateral_asset_id": m.CollateralAssetID.String(),
			"outcomes": outcomes, "facility_id": m.FacilityID.String(),
			"route":                  map[string]any{"permitted": r.Permitted, "product": r.Product, "executor": string(r.Executor)},
			"risk_decision_hash":     d.Hash,
			"risk_policy_version":    d.PolicyVersion,
			"reservations":           reservations,
			"journal_transaction_id": journal.String(),
		}
	}
}
