package predmarket

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// SettleRequest settles a RESOLVED market.
type SettleRequest struct {
	MarketID MarketID
	Actor    econ.Actor
}

// VoidRequest voids a market that has not settled and refunds its shares.
type VoidRequest struct {
	MarketID MarketID
	Actor    econ.Actor
	// Reason says why, in at least 8 characters.
	Reason string
	// Evidence is archived with the refund record when present.
	Evidence any
}

// SettleResult is a market after its settlement or refund.
type SettleResult struct {
	Market     Market
	Settlement Settlement
	// Plan and Lines describe what this call paid, one line per holder in
	// account order. Both are empty on a replay.
	Plan  SettlementPlan
	Lines []ActionResult
	// Replay is true when the market had already settled (or been voided):
	// nothing was paid and the stored record is returned.
	Replay bool
}

// Settle pays a RESOLVED market out of its escrow: every share of the
// winning outcome pays exactly one collateral unit, every share of every
// outcome is retired, Σ payouts equals the escrow exactly, and the escrow and
// every outcome's outstanding shares are zero afterwards -- checked from the
// books before the market is allowed to become SETTLED.
//
// It refuses a market that is not RESOLVED (no settlement before resolution),
// that is DISPUTED, whose dispute window is still open, whose books do not
// reconcile, or whose shares are held by an open reservation or hold. It is
// all or nothing: one holder's payout refused (by Risk V2, say) refuses the
// settlement and nothing is paid. Settling a SETTLED market returns its
// record and pays nothing, which is how concurrent attempts settle once: the
// market row is locked, and every attempt after the first finds it SETTLED.
func (s *Service) Settle(ctx context.Context, tx pgx.Tx, req SettleRequest) (SettleResult, error) {
	by, err := canonicalOperator(req.Actor)
	if err != nil {
		return SettleResult{}, err
	}
	var out SettleResult
	err = atomically(ctx, tx, func(tx pgx.Tx) error {
		if err := bindEconTransitionsImmediately(ctx, tx); err != nil {
			return err
		}
		m, err := s.lockMarket(ctx, tx, req.MarketID, lockUpdate)
		if err != nil {
			return err
		}
		switch m.Status {
		case StatusSettled:
			st, found, err := s.SettlementOf(ctx, tx, m.ID)
			if err != nil {
				return err
			}
			if !found {
				return errs.New(errs.CodeInternal, "predmarket: a SETTLED market has no settlement record")
			}
			out = SettleResult{Market: m, Settlement: st, Replay: true}
			return nil
		case StatusResolved:
		case StatusDisputed:
			return errs.Newf(errs.CodeInvalidStateTransition,
				"predmarket: market %s is DISPUTED; it settles only once the dispute is closed", m.Symbol).WithField("market_id", m.ID.String())
		default:
			return errs.Newf(errs.CodeInvalidStateTransition,
				"predmarket: market %s is %s; there is no settlement before resolution", m.Symbol, m.Status).WithField("market_id", m.ID.String())
		}
		if err := s.authorize(ctx, tx, m.Mode); err != nil {
			return err
		}
		res, err := s.Resolution(ctx, tx, *m.CurrentResolutionID)
		if err != nil {
			return err
		}
		now := s.now()
		if until := res.AcceptedAt.Add(m.DisputeWindow); now.Before(until) {
			return errs.Newf(errs.CodeInvalidStateTransition,
				"predmarket: market %s is in its dispute window until %s", m.Symbol, until.Format(time.RFC3339)).
				WithField("market_id", m.ID.String())
		}
		books, err := s.requireReconciled(ctx, tx, m)
		if err != nil {
			return err
		}
		holdings, err := s.holdings(ctx, tx, m)
		if err != nil {
			return err
		}
		if err := s.requireUnencumbered(ctx, tx, m, holdings); err != nil {
			return err
		}
		plan, err := PlanPayout(books.Escrow, books.SharesOutstanding, holdings, res.OutcomeIndex, m.UnitQuantity)
		if err != nil {
			return err
		}
		inst, err := s.instr.Get(ctx, tx, m.InstrumentID)
		if err != nil {
			return dbErr("load market instrument", err)
		}
		attempt := id.New[settlementKind]()
		lines, err := s.pay(ctx, tx, m, plan, by, inst.Status, &res, attempt)
		if err != nil {
			return err
		}
		st, err := s.recordSettlement(ctx, tx, attempt, m, plan, &res, lines, by, "", nil)
		if err != nil {
			return err
		}
		if err := s.setInstrumentStatus(ctx, tx, m.InstrumentID, assets.StatusHalted, by, "prediction market settled"); err != nil {
			return err
		}
		next, err := s.marketTransition(ctx, tx, m, StatusSettled, by, "settled: "+m.Outcomes[plan.WinningOutcome].Symbol+" won",
			st.EvidenceRef, nil, map[string]any{
				"settlement_id": st.ID.String(), "resolution_id": res.ID.String(),
				"escrow_before": plan.EscrowBefore.String(), "total_paid": plan.TotalPaid.String(), "holders": len(plan.Lines),
			})
		if err != nil {
			return err
		}
		if err := s.requireEmptied(ctx, tx, next, money.Quantity{}); err != nil {
			return err
		}
		out = SettleResult{Market: next, Settlement: st, Plan: plan, Lines: lines}
		return nil
	})
	return out, err
}

// Void voids a market in any state but SETTLED and refunds every share under
// RoundingFloorPerHolder: each whole share of any outcome refunds
// floor(unit / N) collateral base units, a holder's refund is rounded down
// once, and what is not paid stays on the facility's books as the residual
// the REFUND record discloses -- escrow before = Σ refunds + residual,
// checked from the books before the market may become VOIDED. Trading halts
// on every outcome instrument first. Neither the market's creator nor, for a
// DISPUTED market, the operator who raised the dispute may void it. Voiding a
// VOIDED market returns its record and pays nothing.
func (s *Service) Void(ctx context.Context, tx pgx.Tx, req VoidRequest) (SettleResult, error) {
	by, err := canonicalOperator(req.Actor)
	if err != nil {
		return SettleResult{}, err
	}
	reason := strings.TrimSpace(req.Reason)
	if len(reason) < 8 || len(reason) > 2000 {
		return SettleResult{}, errs.New(errs.CodeValidationFailed, "predmarket: a void states its reason in 8..2000 characters")
	}
	var out SettleResult
	err = atomically(ctx, tx, func(tx pgx.Tx) error {
		if err := bindEconTransitionsImmediately(ctx, tx); err != nil {
			return err
		}
		m, err := s.lockMarket(ctx, tx, req.MarketID, lockUpdate)
		if err != nil {
			return err
		}
		switch m.Status {
		case StatusVoided:
			st, found, err := s.SettlementOf(ctx, tx, m.ID)
			if err != nil {
				return err
			}
			if !found {
				return errs.New(errs.CodeInternal, "predmarket: a VOIDED market has no refund record")
			}
			out = SettleResult{Market: m, Settlement: st, Replay: true}
			return nil
		case StatusSettled:
			return errs.Newf(errs.CodeInvalidStateTransition, "predmarket: market %s has settled and cannot be voided", m.Symbol)
		}
		if err := requireOtherThan(by, m.CreatedBy, "the creator of a market cannot void it"); err != nil {
			return err
		}
		if m.Status == StatusDisputed {
			_, dispute, err := s.openDispute(ctx, tx, m)
			if err != nil {
				return err
			}
			if err := requireOtherThan(by, dispute.RaisedBy, "the operator who raised a dispute cannot rule on it"); err != nil {
				return err
			}
		}
		if err := s.authorize(ctx, tx, m.Mode); err != nil {
			return err
		}
		books, err := s.requireReconciled(ctx, tx, m)
		if err != nil {
			return err
		}
		holdings, err := s.holdings(ctx, tx, m)
		if err != nil {
			return err
		}
		if err := s.requireUnencumbered(ctx, tx, m, holdings); err != nil {
			return err
		}
		plan, err := PlanRefund(books.Escrow, books.SharesOutstanding, holdings, m.UnitQuantity)
		if err != nil {
			return err
		}
		// Trading halts on every outcome first. The market instrument -- the
		// contract, which no book can trade -- keeps its status while the
		// refunds are evaluated (ACTIVE for a market voided while OPEN,
		// CLOSE_ONLY after close) and goes HALTED once, at the end: no
		// instrument changes status twice in one transaction.
		for _, o := range m.Outcomes {
			if err := s.setInstrumentStatus(ctx, tx, o.InstrumentID, assets.StatusHalted, by, "prediction market voided: no trading"); err != nil {
				return err
			}
		}
		inst, err := s.instr.Get(ctx, tx, m.InstrumentID)
		if err != nil {
			return dbErr("load market instrument", err)
		}
		attempt := id.New[settlementKind]()
		lines, err := s.pay(ctx, tx, m, plan, by, inst.Status, nil, attempt)
		if err != nil {
			return err
		}
		st, err := s.recordSettlement(ctx, tx, attempt, m, plan, nil, lines, by, reason, req.Evidence)
		if err != nil {
			return err
		}
		if err := s.setInstrumentStatus(ctx, tx, m.InstrumentID, assets.StatusHalted, by, "prediction market voided"); err != nil {
			return err
		}
		next, err := s.marketTransition(ctx, tx, m, StatusVoided, by, reason, st.EvidenceRef, nil, map[string]any{
			"settlement_id": st.ID.String(), "escrow_before": plan.EscrowBefore.String(), "total_refunded": plan.TotalPaid.String(),
			"residual": plan.Residual.String(), "refund_rate_per_share": plan.RefundRate.String(), "holders": len(plan.Lines),
		})
		if err != nil {
			return err
		}
		if err := s.requireEmptied(ctx, tx, next, plan.Residual); err != nil {
			return err
		}
		out = SettleResult{Market: next, Settlement: st, Plan: plan, Lines: lines}
		return nil
	})
	return out, err
}

// requireUnencumbered refuses to settle shares that an open reservation or
// hold still backs: an order the book has not cancelled would otherwise be
// left reserving shares that no longer exist.
func (s *Service) requireUnencumbered(ctx context.Context, tx pgx.Tx, m Market, holdings []Holding) error {
	now := s.now()
	for _, h := range holdings {
		for i, sh := range h.Shares {
			if !sh.IsPositive() {
				continue
			}
			av, err := s.capital.Availability(ctx, tx, h.AccountID.String(), m.Outcomes[i].ShareAssetID, now)
			if err != nil {
				return err
			}
			if av.Reserved.IsPositive() || av.Held.IsPositive() {
				return errs.New(errs.CodeReconciliationRequired,
					"predmarket: outcome shares are still reserved or held; cancel the orders holding them first").
					WithField("account_id", h.AccountID.String()).WithField("outcome", m.Outcomes[i].Symbol)
			}
		}
	}
	return nil
}

// requireEmptied is the post-condition of a settlement or refund, read from
// the books: no outcome share is outstanding or held, and the escrow holds
// exactly the disclosed residual (zero after a payout).
func (s *Service) requireEmptied(ctx context.Context, tx pgx.Tx, m Market, residual money.Quantity) error {
	b, err := s.Books(ctx, tx, m)
	if err != nil {
		return err
	}
	violations := b.Violations(residual)
	if b.Escrow.Cmp(residual) != 0 {
		violations = append(violations, "the escrow does not hold exactly the residual")
	}
	for i := range b.SharesOutstanding {
		if !b.SharesOutstanding[i].IsZero() || !b.HeldByCustomers[i].IsZero() {
			violations = append(violations, "outcome "+m.Outcomes[i].Symbol+" still has shares outstanding")
		}
	}
	if len(violations) > 0 {
		return errs.New(errs.CodeReconciliationRequired, "predmarket: the settlement did not empty the market's books").
			WithField("market_id", m.ID.String()).WithField("violations", violations)
	}
	return nil
}

// pay runs one RESOLVE_PREDICTION_POSITION action per plan line, in account
// order, tracking the escrow each payout draws on. attempt is the id the
// settlement record will carry.
func (s *Service) pay(ctx context.Context, tx pgx.Tx, m Market, plan SettlementPlan, by econ.Actor, instrumentStatus assets.Status,
	res *Resolution, attempt SettlementID) ([]ActionResult, error) {
	remaining := plan.EscrowBefore
	out := make([]ActionResult, 0, len(plan.Lines))
	for _, line := range plan.Lines {
		a, err := s.settlementAction(ctx, tx, m, plan, line, by, instrumentStatus, remaining, res, attempt)
		if err != nil {
			return nil, err
		}
		r, err := s.run(ctx, tx, a, m.Mode)
		if err != nil {
			return nil, err
		}
		if r.Replay {
			return nil, errs.New(errs.CodeReconciliationRequired, "predmarket: a holder was already paid by an earlier settlement attempt").
				WithField("account_id", line.AccountID.String())
		}
		remaining = remaining.Sub(line.Amount)
		out = append(out, r)
	}
	return out, nil
}

// settlementAction is one holder's payout or refund as an economic action.
//
// Exactly once is the effect's business: its key is deterministic per market
// and holder, so no market pays a holder twice. The intent's idempotency key
// is scoped to the settlement attempt instead, because economic intents are
// unique per (account, key) across every subsystem: a deterministic key a
// customer could claim first, through any service that accepts client keys,
// would block the whole settlement.
func (s *Service) settlementAction(ctx context.Context, tx pgx.Tx, m Market, plan SettlementPlan, line PlanLine, by econ.Actor,
	instrumentStatus assets.Status, escrowRemaining money.Quantity, res *Resolution, attempt SettlementID) (action, error) {
	verb := "payout"
	effectKind := "PREDICTION_PAYOUT"
	if plan.Kind == SettlementRefund {
		verb, effectKind = "refund", "PREDICTION_REFUND"
	}
	facts, err := s.facts.Facts(ctx, tx, line.AccountID)
	if err != nil {
		return action{}, err
	}
	shares := make(map[string]string, len(m.Outcomes))
	for i, o := range m.Outcomes {
		shares[o.Symbol] = line.Shares[i].String()
	}
	terms := map[string]any{
		"market_id": m.ID.String(), "market_symbol": m.Symbol, "settlement": string(plan.Kind), "shares": shares,
		"amount": line.Amount.String(), "rounding_policy": string(plan.Policy), "settlement_id": attempt.String(),
	}
	if plan.Kind == SettlementPayout {
		terms["winning_outcome"] = m.Outcomes[plan.WinningOutcome].Symbol
		if res != nil {
			terms["resolution_id"] = res.ID.String()
		}
	} else {
		terms["refund_rate_per_share"] = plan.RefundRate.String()
	}
	body, err := json.Marshal(terms)
	if err != nil {
		return action{}, errs.Wrap(err, errs.CodeInternal, "predmarket: encode settlement terms")
	}
	var qty *money.Quantity
	if line.Amount.IsPositive() {
		amt := line.Amount
		qty = &amt
	}
	collateral := m.CollateralAssetID
	key := "predmarket:" + verb + ":" + m.ID.String() + ":" + line.AccountID.String()
	intentKey := "predmarket:" + verb + ":" + attempt.String() + ":" + line.AccountID.String()
	notional, err := usd(line.Amount, money.RoundUp)
	if err != nil {
		return action{}, err
	}
	in := econ.Intent{
		AccountID: line.AccountID, Kind: econ.KindResolvePredictionPosition, Mode: m.Mode, Capability: gates.PredictionMarkets,
		ActorType: by.Type, ActorID: by.ID, InstrumentID: m.InstrumentID.String(), AssetID: &collateral, Quantity: qty,
		Terms: body, IdempotencyKey: intentKey, CorrelationID: "predmarket:" + m.ID.String(),
	}
	return action{
		intent: in, facts: facts, mover: by,
		subject:    settlement.Subject{Type: settlement.SubjectInstrument, ID: m.InstrumentID.String(), AssetID: collateral.String()},
		effectKey:  func(econ.Intent) string { return key },
		effectKind: effectKind,
		riskInput: func(it econ.Intent) (risk.AdvancedInput, error) {
			kills, err := s.killSwitches(ctx, tx)
			if err != nil {
				return risk.AdvancedInput{}, err
			}
			// Both sides are converted the same way, so the comparison never
			// refuses what the exact amounts allow; the ledger refuses an
			// escrow that would go negative.
			have, err := usd(escrowRemaining, money.RoundDown)
			if err != nil {
				return risk.AdvancedInput{}, err
			}
			need, err := usd(line.Amount, money.RoundDown)
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
				// The session of a settlement intent is the settlement window,
				// which the caller has just established is open: the market is
				// RESOLVED with no dispute pending and its dispute window has
				// passed, or a void has opened its refund window.
				Instrument:   &risk.InstrumentSnapshot{Status: string(instrumentStatus), SessionOpen: true, Expired: !now.Before(m.CloseAt)},
				Balance:      &risk.BalanceSnapshot{Available: have, Required: need},
				KillSwitches: kills,
				Now:          now,
			}, nil
		},
		request: func(it econ.Intent) any {
			return map[string]any{
				"market_id": m.ID.String(), "account_id": it.AccountID.String(), "settlement": string(plan.Kind),
				"shares": shares, "amount": line.Amount.String(),
			}
		},
		posting: func(it econ.Intent) (ledger.Posting, error) {
			return SettlementPosting(m, plan.Kind, line, postingMeta{
				Key: key, Reference: ledger.FinancialEventReference{Type: "prediction_" + verb, ID: it.ID.String()},
				At: s.now(), Description: "prediction market " + m.Symbol + " " + verb,
			})
		},
		evidence: func(it econ.Intent, d risk.AdvancedDecision, r settlement.Route, _ []string, journal ledger.TransactionID) any {
			doc := map[string]any{
				"kind": effectKind, "market_id": m.ID.String(), "market_symbol": m.Symbol, "intent_id": it.ID.String(),
				"account_id": it.AccountID.String(), "shares_retired": shares, "amount": line.Amount.String(),
				"collateral_asset_id": collateral.String(), "rounding_policy": string(plan.Policy),
				"settled_by":             map[string]any{"type": string(by.Type), "id": by.ID},
				"route":                  map[string]any{"permitted": r.Permitted, "product": r.Product, "executor": string(r.Executor)},
				"risk_decision_hash":     d.Hash,
				"journal_transaction_id": journal.String(),
			}
			if plan.Kind == SettlementPayout {
				doc["winning_outcome"] = m.Outcomes[plan.WinningOutcome].Symbol
			} else {
				doc["refund_rate_per_share"] = plan.RefundRate.String()
			}
			if res != nil {
				doc["resolution_id"] = res.ID.String()
				doc["report_hash"] = hex.EncodeToString(res.ReportHash)
			}
			return doc
		},
	}, nil
}

// recordSettlement archives the settlement's evidence and writes its record.
func (s *Service) recordSettlement(ctx context.Context, tx pgx.Tx, attempt SettlementID, m Market, plan SettlementPlan, res *Resolution,
	lines []ActionResult, by econ.Actor, reason string, evidence any) (Settlement, error) {
	st := Settlement{
		ID: attempt, MarketID: m.ID, Kind: plan.Kind, OutcomeCount: plan.OutcomeCount,
		UnitQuantity: plan.Unit, RoundingPolicy: plan.Policy, EscrowBefore: plan.EscrowBefore, TotalPaid: plan.TotalPaid,
		Residual: plan.Residual, HolderCount: len(plan.Lines), SettledBy: by, SettledAt: s.now(),
	}
	if plan.Kind == SettlementPayout {
		if res == nil {
			return Settlement{}, errs.New(errs.CodeInternal, "predmarket: a payout needs its resolution")
		}
		w := plan.WinningOutcome
		st.ResolutionID, st.WinningOutcomeIndex = &res.ID, &w
	} else {
		rate := plan.RefundRate
		st.RefundRatePerShare = &rate
	}
	rows := make([]map[string]any, 0, len(plan.Lines))
	for i, line := range plan.Lines {
		shares := make(map[string]string, len(m.Outcomes))
		for j, o := range m.Outcomes {
			shares[o.Symbol] = line.Shares[j].String()
		}
		row := map[string]any{"account_id": line.AccountID.String(), "shares": shares, "amount": line.Amount.String()}
		if i < len(lines) {
			row["intent_id"] = lines[i].Intent.ID.String()
			row["journal_transaction_id"] = lines[i].JournalTransactionID.String()
		}
		rows = append(rows, row)
	}
	doc := map[string]any{
		"kind": "PREDICTION_SETTLEMENT", "settlement_kind": string(plan.Kind), "settlement_id": st.ID.String(),
		"market_id": m.ID.String(), "market_symbol": m.Symbol, "outcome_count": plan.OutcomeCount,
		"unit_quantity": plan.Unit.String(), "rounding_policy": string(plan.Policy),
		"escrow_before": plan.EscrowBefore.String(), "total_paid": plan.TotalPaid.String(), "residual": plan.Residual.String(),
		"conservation": "escrow_before = total_paid + residual", "holders": rows,
		"settled_by": map[string]any{"type": string(by.Type), "id": by.ID}, "settled_at": st.SettledAt,
	}
	if res != nil {
		doc["resolution_id"] = res.ID.String()
		doc["winning_outcome"] = m.Outcomes[plan.WinningOutcome].Symbol
	} else {
		doc["refund_rate_per_share"] = plan.RefundRate.String()
		doc["residual_per_complete_set"] = plan.Unit.Sub(plan.RefundRate.Mul(money.QuantityFromInt64(int64(plan.OutcomeCount)))).String()
	}
	if reason != "" {
		doc["reason"] = reason
	}
	if evidence != nil {
		doc["evidence"] = evidence
	}
	ref, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "predmarket/settlement/" + m.ID.String(), Document: doc})
	if err != nil {
		return Settlement{}, err
	}
	st.EvidenceRef, st.EvidenceSHA256 = ref.URI, ref.SHA256
	if err := insertSettlement(ctx, tx, st); err != nil {
		return Settlement{}, err
	}
	stored, found, err := s.SettlementOf(ctx, tx, m.ID)
	if err != nil {
		return Settlement{}, err
	}
	if !found {
		return Settlement{}, errs.New(errs.CodeInternal, "predmarket: the settlement record vanished")
	}
	return stored, nil
}
