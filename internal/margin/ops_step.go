package margin

import (
	"context"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// planStep plans a case's next step. Interest is accrued to the instant only
// when the step will act -- sell, apply collateral or declare bankruptcy --
// so the loans it repays or records are exact; a case that only waits for a
// price or a venue, or completes, posts nothing at all, however often it is
// retried.
func (s *Service) planStep(ctx context.Context, tx pgx.Tx, a Account, pool econ.Facility, c Case, now time.Time) (Plan, AccountState, []Lot, error) {
	plan, state, lots, err := s.planAt(ctx, tx, a, c, now)
	if err != nil {
		return Plan{}, AccountState{}, nil, err
	}
	switch plan.Decision {
	case DecisionSell, DecisionSellAll, DecisionApplyCollateral, DecisionBankrupt:
		accrued, err := s.accrue(ctx, tx, a, pool, now, true)
		if err != nil {
			return Plan{}, AccountState{}, nil, err
		}
		if len(accrued) > 0 {
			return s.planAt(ctx, tx, a, c, now)
		}
	}
	return plan, state, lots, nil
}

// planAt loads the account and plans its case's next step against the
// venue's availability for every risky holding.
func (s *Service) planAt(ctx context.Context, tx pgx.Tx, a Account, c Case, now time.Time) (Plan, AccountState, []Lot, error) {
	state, lots, err := s.loadState(ctx, tx, a, now)
	if err != nil {
		return Plan{}, AccountState{}, nil, err
	}
	venueUp := true
	for _, h := range state.Holdings {
		if h.Numeraire || !h.Quantity.IsPositive() {
			continue
		}
		aid, err := assets.ParseAssetID(h.AssetID)
		if err != nil {
			return Plan{}, AccountState{}, nil, errs.Wrap(err, errs.CodeInternal, "margin: holding asset id")
		}
		up, err := s.d.Venue.Available(ctx, aid)
		if err != nil {
			return Plan{}, AccountState{}, nil, err
		}
		venueUp = venueUp && up
	}
	plan, err := PlanLiquidation(s.policy, PlanInput{State: state, StepsTaken: c.StepsExecuted, VenueAvailable: venueUp})
	if err != nil {
		return Plan{}, AccountState{}, nil, err
	}
	return plan, state, lots, nil
}

func (s *Service) loadAfterStep(ctx context.Context, tx pgx.Tx, a Account, now time.Time) (Evaluation, error) {
	state, _, err := s.loadState(ctx, tx, a, now)
	if err != nil {
		return Evaluation{}, err
	}
	return Evaluate(s.policy, state)
}

// executeStep executes the plan's first order in full (the paper venue fills
// at the price the plan computed), posts it, applies the repayment to the
// lots and records the step.
func (s *Service) executeStep(ctx context.Context, tx pgx.Tx, a Account, c *Case, pool, fund econ.Facility, state AccountState,
	lots []Lot, plan Plan, now time.Time) (Step, error) {
	next, done, err := ExecuteFirst(s.policy, state, plan, 10_000)
	if err != nil {
		return Step{}, err
	}
	evNext, err := Evaluate(s.policy, next)
	if err != nil {
		return Step{}, err
	}
	stepNo := c.StepsExecuted + 1
	prefix := "margin:liq:" + c.ID.String() + ":" + strconv.Itoa(stepNo)
	step := Step{
		ID: newStepID(), CaseID: c.ID, StepNo: stepNo, Action: done.Action, Quantity: done.Quantity,
		SlippageBPS: done.SlippageBPS, SlippageCapBPS: s.policy.LiquidationSlippageCapBPS,
		Proceeds: done.Proceeds, Fee: done.Fee, Repaid: done.Repay, Excess: done.Excess,
		EquityBefore: plan.Before.Equity, EquityAfter: evNext.Equity, PlanHash: plan.Hash, ExecutedAt: now,
	}
	req := map[string]string{"case_id": c.ID.String(), "step_no": strconv.Itoa(stepNo), "action": string(done.Action),
		"asset_id": done.AssetID, "quantity": done.Quantity.String(), "proceeds": done.Proceeds.String(), "fee": done.Fee.String(),
		"repay": done.Repay.String(), "excess": done.Excess.String(), "plan_hash": plan.Hash}
	evidence := map[string]any{"plan": plan, "executed": done}
	acct := a.AccountID
	switch done.Action {
	case ActionSell:
		asset, err := assets.ParseAssetID(done.AssetID)
		if err != nil {
			return Step{}, errs.Wrap(err, errs.CodeInternal, "margin: liquidation asset id")
		}
		step.AssetID, step.Mark, step.ExecutionPrice = asset, done.Mark, done.ExecutionPrice
		tm := s.meta("margin_liquidation_step", step.ID.String(), "reduce-only liquidation sale")
		tp, err := LiquidationPosting(tm, acct, fund, asset, done.Quantity, a.NumeraireAssetID, done.Proceeds, done.Fee)
		if err != nil {
			return Step{}, err
		}
		trade, err := s.postOnce(ctx, tx, postSpec{key: prefix + ":trade", kind: "LIQUIDATION", account: acct, request: req, posting: tp, evidence: evidence})
		if err != nil {
			return Step{}, err
		}
		step.TradeJournalID = &trade.Journal
	case ActionApplyCollateral:
		step.AssetID = a.NumeraireAssetID
		rm := s.meta("margin_liquidation_step", step.ID.String(), "numeraire collateral applied to the loans")
		rp, err := CollateralMovePosting(rm, acct, a.NumeraireAssetID, done.Quantity, MoveRelease)
		if err != nil {
			return Step{}, err
		}
		released, err := s.postOnce(ctx, tx, postSpec{key: prefix + ":release", kind: "LIQUIDATION_RELEASE", account: acct, request: req, posting: rp, evidence: evidence})
		if err != nil {
			return Step{}, err
		}
		step.ReleaseJournal = &released.Journal
	default:
		return Step{}, errs.Newf(errs.CodeInternal, "margin: unknown step action %s", done.Action)
	}
	if done.Repay.IsPositive() {
		pm := s.meta("margin_liquidation_step", step.ID.String(), "liquidation proceeds repay the loans")
		pp, err := RepayPosting(pm, acct, pool, a.NumeraireAssetID, done.Repay)
		if err != nil {
			return Step{}, err
		}
		repaid, err := s.postOnce(ctx, tx, postSpec{key: prefix + ":repay", kind: "LIQUIDATION_REPAY", account: acct, request: req, posting: pp, evidence: evidence})
		if err != nil {
			return Step{}, err
		}
		step.RepayJournalID = &repaid.Journal
	}
	if done.Excess.IsPositive() {
		xm := s.meta("margin_liquidation_step", step.ID.String(), "liquidation proceeds above the loans stay as collateral")
		xp, err := CollateralMovePosting(xm, acct, a.NumeraireAssetID, done.Excess, MovePost)
		if err != nil {
			return Step{}, err
		}
		excess, err := s.postOnce(ctx, tx, postSpec{key: prefix + ":excess", kind: "LIQUIDATION_EXCESS", account: acct, request: req, posting: xp, evidence: evidence})
		if err != nil {
			return Step{}, err
		}
		step.ExcessJournalID = &excess.Journal
	}
	if err := s.applyAllocations(ctx, tx, lots, done.Allocations, now); err != nil {
		return Step{}, err
	}
	if err := s.st.insertStep(ctx, tx, step); err != nil {
		return Step{}, err
	}
	if err := s.st.countStep(ctx, tx, c, stepNo); err != nil {
		return Step{}, err
	}
	return step, nil
}

// QueueFailure is an account the queue could not process this time.
type QueueFailure struct {
	AccountID accounts.AccountID
	Err       error
}

// QueueResult is what one queue run did.
type QueueResult struct {
	Results  []LiquidationResult
	Failures []QueueFailure
}

type queued struct {
	acct       accounts.AccountID
	mid        AccountID
	deficiency money.Quantity
}

// ProcessLiquidationQueue runs one liquidation step for each of up to limit
// accounts in MARGIN_CALL or with an open case, most under-margined first
// (maintenance minus equity, descending; ties by margin account id). It is
// bounded twice: at most MaxQueueScan candidates are considered and at most
// limit steps executed, one per account, each in its own savepoint so one
// account's failure does not stop the others. A case in MANUAL_REVIEW is
// not a candidate: a person owns it.
func (s *Service) ProcessLiquidationQueue(ctx context.Context, tx pgx.Tx, limit int) (QueueResult, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	if err := bindingsImmediate(ctx, tx); err != nil {
		return QueueResult{}, err
	}
	rows, err := tx.Query(ctx, `SELECT m.account_id, m.id FROM margin_accounts m
		WHERE m.status IN ('MARGIN_CALL','LIQUIDATING') AND m.policy_version = $1
		  AND NOT EXISTS (SELECT 1 FROM margin_liquidation_cases c WHERE c.margin_account_id = m.id AND c.status = 'MANUAL_REVIEW')
		ORDER BY m.id LIMIT $2`, s.policy.Version, s.cfg.MaxQueueScan)
	if err != nil {
		return QueueResult{}, dbErr("list liquidation candidates", err)
	}
	var cands []queued
	for rows.Next() {
		var q queued
		if err := rows.Scan(&q.acct, &q.mid); err != nil {
			rows.Close()
			return QueueResult{}, dbErr("scan liquidation candidate", err)
		}
		cands = append(cands, q)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return QueueResult{}, dbErr("list liquidation candidates", err)
	}
	var out QueueResult
	now := s.now()
	var ranked []queued
	for _, q := range cands {
		a, err := s.st.accountByID(ctx, tx, q.mid, false)
		if err != nil {
			out.Failures = append(out.Failures, QueueFailure{AccountID: q.acct, Err: err})
			continue
		}
		state, _, err := s.loadState(ctx, tx, a, now)
		if err != nil {
			out.Failures = append(out.Failures, QueueFailure{AccountID: q.acct, Err: err})
			continue
		}
		ev, err := Evaluate(s.policy, state)
		if err != nil {
			out.Failures = append(out.Failures, QueueFailure{AccountID: q.acct, Err: err})
			continue
		}
		q.deficiency = ev.MaintenanceMargin.Sub(ev.Equity)
		ranked = append(ranked, q)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if c := ranked[i].deficiency.Cmp(ranked[j].deficiency); c != 0 {
			return c > 0
		}
		return ranked[i].mid.String() < ranked[j].mid.String()
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	for _, q := range ranked {
		res, err := withSavepoint(ctx, tx, func(sp pgx.Tx) (LiquidationResult, error) {
			return s.Liquidate(ctx, sp, LiquidateRequest{AccountID: q.acct})
		})
		if err != nil {
			out.Failures = append(out.Failures, QueueFailure{AccountID: q.acct, Err: err})
			continue
		}
		out.Results = append(out.Results, res)
	}
	return out, nil
}

// DeficitResult is the outcome of covering a bankrupt case's deficit.
type DeficitResult struct {
	CaseID               CaseID
	Deficit              money.Quantity
	JournalTransactionID ledger.TransactionID
	Replayed             bool
}

// CoverDeficit has the insurance fund cover a bankrupt case's deficit with a
// DEFICIT_COVERAGE posting: the customer's loan becomes a DEFICIT the
// customer owes, the lending pool is repaid in cash, and the insurance fund
// pays that cash and holds the claim (DEFICIT_RECEIVABLE). The deficit is
// the one the case recorded, and it must equal both the loans the books
// carry and what the lots owe; the fund must hold the cash. Exactly once per
// case.
func (s *Service) CoverDeficit(ctx context.Context, tx pgx.Tx, cid CaseID) (DeficitResult, error) {
	if err := bindingsImmediate(ctx, tx); err != nil {
		return DeficitResult{}, err
	}
	c, err := s.st.caseByID(ctx, tx, cid, false)
	if err != nil {
		return DeficitResult{}, err
	}
	a, err := s.st.accountByID(ctx, tx, c.MarginAccountID, true)
	if err != nil {
		return DeficitResult{}, err
	}
	if err := s.requireOwnPolicy(a); err != nil {
		return DeficitResult{}, err
	}
	if c, err = s.st.caseByID(ctx, tx, cid, true); err != nil {
		return DeficitResult{}, err
	}
	if c.Status != CaseBankrupt || c.DeficitUnits == nil {
		return DeficitResult{}, errs.Newf(errs.CodeInvalidStateTransition, "margin: case %s is %s; only a bankrupt case has a deficit to cover", c.ID, c.Status)
	}
	deficit := *c.DeficitUnits
	if c.CoverageJournalID != nil {
		return DeficitResult{CaseID: c.ID, Deficit: deficit, JournalTransactionID: *c.CoverageJournalID, Replayed: true}, nil
	}
	if err := s.authorize(ctx, tx, a.Mode, true, gates.Margin); err != nil {
		return DeficitResult{}, err
	}
	pool, fund, err := s.accountFacilities(ctx, tx, a)
	if err != nil {
		return DeficitResult{}, err
	}
	loan, err := s.d.Ledger.Balance(ctx, tx, ledger.CustomerAccount(a.AccountID, ledger.CodeMarginLoan, a.NumeraireAssetID))
	if err != nil {
		return DeficitResult{}, err
	}
	lots, err := s.st.openLots(ctx, tx, a.ID, true)
	if err != nil {
		return DeficitResult{}, err
	}
	owed := money.Quantity{}
	for _, l := range lots {
		owed = owed.Add(l.Outstanding())
	}
	if !loan.Equal(deficit) || !owed.Equal(deficit) {
		return DeficitResult{}, errs.New(errs.CodeReconciliationRequired, "margin: the recorded deficit disagrees with the books").
			WithField("deficit", deficit.String()).WithField("ledger_loan", loan.String()).WithField("lots", owed.String())
	}
	cash, err := s.d.Ledger.Balance(ctx, tx, fund.Account(ledger.CodeInsuranceFundCash, a.NumeraireAssetID))
	if err != nil {
		return DeficitResult{}, err
	}
	if cash.Cmp(deficit) < 0 {
		return DeficitResult{}, errs.New(errs.CodeInsufficientBuyingPower, "margin: the insurance fund cannot cover this deficit").
			WithField("deficit", deficit.String()).WithField("fund_cash", cash.String())
	}
	now := s.now()
	m := s.meta("margin_liquidation_case", c.ID.String(), "insurance fund covers a liquidation deficit")
	p, err := DeficitCoveragePosting(m, a.AccountID, pool, fund, a.NumeraireAssetID, deficit)
	if err != nil {
		return DeficitResult{}, err
	}
	req := map[string]string{"case_id": c.ID.String(), "deficit": deficit.String(), "asset_id": a.NumeraireAssetID.String(),
		"lending_pool_id": pool.ID.String(), "insurance_fund_id": fund.ID.String()}
	out, err := s.postOnce(ctx, tx, postSpec{key: "margin:deficit:" + c.ID.String(), kind: "DEFICIT_COVERAGE", account: a.AccountID,
		request: req, posting: p, evidence: req})
	if err != nil {
		return DeficitResult{}, err
	}
	for _, l := range lots {
		al := LoanAllocation{LotID: l.ID.String(), Interest: l.OutstandingInterest, Principal: l.OutstandingPrincipal}
		if err := s.st.repayLot(ctx, tx, l.ID, al, CloseDeficitCovered, now); err != nil {
			return DeficitResult{}, err
		}
	}
	if err := s.st.setCoverage(ctx, tx, &c, out.Journal, now); err != nil {
		return DeficitResult{}, err
	}
	return DeficitResult{CaseID: c.ID, Deficit: deficit, JournalTransactionID: out.Journal}, nil
}

// StressRun is a stored stress run.
type StressRun struct {
	ID     StressRunID
	Report StressReport
}

// RunStress runs every stress scenario against the account's current state
// and stores the report (append-only, with its canonical hash). It moves no
// value and changes no status.
func (s *Service) RunStress(ctx context.Context, tx pgx.Tx, acct accounts.AccountID) (StressRun, error) {
	a, err := s.st.accountByCustomer(ctx, tx, acct, false)
	if err != nil {
		return StressRun{}, err
	}
	if err := s.requireOwnPolicy(a); err != nil {
		return StressRun{}, err
	}
	if err := s.authorize(ctx, tx, a.Mode, true, gates.Margin); err != nil {
		return StressRun{}, err
	}
	now := s.now()
	state, _, err := s.loadState(ctx, tx, a, now)
	if err != nil {
		return StressRun{}, err
	}
	rep, err := RunStress(s.policy, state)
	if err != nil {
		return StressRun{}, err
	}
	rid, err := s.st.insertStressRun(ctx, tx, a.ID, rep, now)
	if err != nil {
		return StressRun{}, err
	}
	return StressRun{ID: rid, Report: rep}, nil
}
