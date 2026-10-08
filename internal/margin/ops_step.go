package margin

import (
	"context"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// planStep plans a case's next step. Interest is accrued to the instant only
// when the step will act -- sell, apply collateral, sweep the wallet, declare
// bankruptcy -- or the case is about to go to a person (from then on its
// interest is waived), so the loans it repays or records are exact; a case
// that only waits for a price or a venue, or completes, posts nothing at
// all, however often it is retried.
func (s *Service) planStep(ctx context.Context, tx pgx.Tx, a Account, pool econ.Facility, c Case, now time.Time) (Plan, AccountState, []Lot, error) {
	plan, state, lots, err := s.planAt(ctx, tx, a, c, now)
	if err != nil {
		return Plan{}, AccountState{}, nil, err
	}
	switch plan.Decision {
	case DecisionSell, DecisionSellAll, DecisionApplyCollateral, DecisionSweepWallet, DecisionBankrupt, DecisionManualReview:
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
// venue's availability for every risky holding, the trading steps of the
// case's budget window and the customer's free numeraire cash.
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
	trading, err := s.st.tradingSteps(ctx, tx, c)
	if err != nil {
		return Plan{}, AccountState{}, nil, err
	}
	free, err := s.freeNumeraire(ctx, tx, a)
	if err != nil {
		return Plan{}, AccountState{}, nil, err
	}
	plan, err := PlanLiquidation(s.policy, PlanInput{State: state, StepsTaken: trading, VenueAvailable: venueUp, FreeNumeraire: free})
	if err != nil {
		return Plan{}, AccountState{}, nil, err
	}
	return plan, state, lots, nil
}

// freeNumeraire is the customer's numeraire wallet balance that no
// reservation or withdrawal hold has claimed: what a sweep may take. It is
// zero when the customer account cannot reserve value at all (FROZEN or
// CLOSED): margin moves a wallet only through a capital reservation.
func (s *Service) freeNumeraire(ctx context.Context, q db.Querier, a Account) (money.Quantity, error) {
	cust, err := s.accounts.Get(ctx, q, a.AccountID)
	if err != nil {
		return money.Quantity{}, err
	}
	if !cust.Status.AllowsRiskReduction() {
		return money.Quantity{}, nil
	}
	av, err := s.d.Capital.Availability(ctx, q, a.AccountID.String(), a.NumeraireAssetID, s.d.Clock.Now())
	if err != nil {
		return money.Quantity{}, err
	}
	return av.Available, nil
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
func (s *Service) executeStep(ctx context.Context, tx pgx.Tx, a Account, c *Case, pool econ.Facility, state AccountState,
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
		// The fee is disclosed to the customer in the fee ledger before it
		// is charged, and booked against the journal that charges it.
		var fee *feeledger.StoredQuote
		if done.Fee.IsPositive() {
			q, err := s.discloseLiquidationFee(ctx, tx, a, state.NumeraireDecimals, prefix+":fee", done.Fee, done.Proceeds, now)
			if err != nil {
				return Step{}, err
			}
			fee = &q
		}
		tm := s.meta(refLiquidationStep, step.ID.String(), "reduce-only liquidation sale")
		tp, err := LiquidationPosting(tm, acct, asset, done.Quantity, a.NumeraireAssetID, done.Proceeds, done.Fee)
		if err != nil {
			return Step{}, err
		}
		trade, err := s.postOnce(ctx, tx, postSpec{key: prefix + ":trade", kind: "LIQUIDATION", account: acct, request: req, posting: tp, evidence: evidence})
		if err != nil {
			return Step{}, err
		}
		step.TradeJournalID = &trade.Journal
		if fee != nil {
			if err := s.bookLiquidationFee(ctx, tx, a, *fee, trade.Journal, trade.Effect.ID); err != nil {
				return Step{}, err
			}
		}
	case ActionApplyCollateral:
		step.AssetID = a.NumeraireAssetID
		rm := s.meta(refLiquidationStep, step.ID.String(), "numeraire collateral applied to the loans")
		rp, err := CollateralMovePosting(rm, acct, a.NumeraireAssetID, done.Quantity, MoveRelease)
		if err != nil {
			return Step{}, err
		}
		released, err := s.postOnce(ctx, tx, postSpec{key: prefix + ":release", kind: "LIQUIDATION_RELEASE", account: acct, request: req, posting: rp, evidence: evidence})
		if err != nil {
			return Step{}, err
		}
		step.ReleaseJournal = &released.Journal
	case ActionSweepWallet:
		// The customer's free cash repays the loans before any bankruptcy. It
		// is taken only through a capital reservation, so cash another
		// operation has reserved (or a withdrawal hold covers) is never swept.
		step.AssetID = a.NumeraireAssetID
		res, err := s.d.Capital.Reserve(ctx, tx, capital.ReserveRequest{
			AccountID: acct.String(), AssetID: a.NumeraireAssetID, Quantity: done.Quantity,
			ActorType: s.actor.Type, ActorID: s.actor.ID, IdempotencyKey: prefix + ":sweep-reserve", TTL: reservationTTL,
			Reason: "margin liquidation sweeps the free wallet balance into the loans",
		})
		if err != nil {
			return Step{}, err
		}
		sm := s.meta(refLiquidationStep, step.ID.String(), "the free wallet balance repays the loans before bankruptcy")
		sp, err := RepayPosting(sm, acct, pool, a.NumeraireAssetID, done.Quantity)
		if err != nil {
			return Step{}, err
		}
		swept, err := s.postOnce(ctx, tx, postSpec{key: prefix + ":sweep", kind: "LIQUIDATION_SWEEP", account: acct, request: req,
			posting: sp, evidence: evidence, reserve: res.ID.String()})
		if err != nil {
			return Step{}, err
		}
		if _, err := s.d.Capital.ConsumeFinal(ctx, tx, res.ID, done.Quantity, 0, ""); err != nil {
			return Step{}, err
		}
		step.RepayJournalID = &swept.Journal
	default:
		return Step{}, errs.Newf(errs.CodeInternal, "margin: unknown step action %s", done.Action)
	}
	if done.Repay.IsPositive() && done.Action != ActionSweepWallet {
		pm := s.meta(refLiquidationStep, step.ID.String(), "liquidation proceeds repay the loans")
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
		xm := s.meta(refLiquidationStep, step.ID.String(), "liquidation proceeds above the loans stay as collateral")
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

// Transactor runs a function in a database transaction of its own and
// commits it; *db.DB satisfies it.
type Transactor interface {
	InTx(ctx context.Context, opts db.TxOptions, fn func(ctx context.Context, tx pgx.Tx) error) error
}

// queueRetries is how often one account's step is re-run after a retryable
// failure (a deadlock or serialization failure): Liquidate is wholly inside
// its transaction, so re-running it is safe.
const queueRetries = 2

// ProcessLiquidationQueue runs one liquidation step for each of up to limit
// accounts in MARGIN_CALL or with an open case, most under-margined first
// (maintenance minus equity, descending; ties by margin account id). It is
// bounded twice: at most MaxQueueScan candidates are considered and at most
// limit steps executed, one per account.
//
// It is a driver, not an operation: it owns its transactions. The ranking is
// read in one read-only transaction, and every account's step then runs and
// COMMITS in a transaction of its own, so one account's failure -- including
// a refusal the database makes only at COMMIT -- rolls back that account's
// step and nothing else. Every operation it runs keeps the (ctx, tx) shape. A
// case in MANUAL_REVIEW is not a candidate: a person owns it.
func (s *Service) ProcessLiquidationQueue(ctx context.Context, txr Transactor, limit int) (QueueResult, error) {
	if txr == nil {
		return QueueResult{}, errs.New(errs.CodeInternal, "margin: the liquidation queue needs a transactor")
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	var out QueueResult
	var ranked []queued
	if err := txr.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, ReadOnly: true}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ranked, out.Failures, err = s.rankQueue(ctx, tx, limit)
		return err
	}); err != nil {
		return QueueResult{}, err
	}
	for _, q := range ranked {
		var res LiquidationResult
		err := txr.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: queueRetries}, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			res, err = s.Liquidate(ctx, tx, LiquidateRequest{AccountID: q.acct})
			return err
		})
		if err != nil {
			out.Failures = append(out.Failures, QueueFailure{AccountID: q.acct, Err: err})
			continue
		}
		out.Results = append(out.Results, res)
	}
	return out, nil
}

// rankQueue lists the candidates and orders them by deficiency.
func (s *Service) rankQueue(ctx context.Context, q db.Querier, limit int) ([]queued, []QueueFailure, error) {
	rows, err := q.Query(ctx, `SELECT m.account_id, m.id FROM margin_accounts m
		WHERE m.status IN ('MARGIN_CALL','LIQUIDATING') AND m.policy_version = $1
		  AND NOT EXISTS (SELECT 1 FROM margin_liquidation_cases c WHERE c.margin_account_id = m.id AND c.status = 'MANUAL_REVIEW')
		ORDER BY m.id LIMIT $2`, s.policy.Version, s.cfg.MaxQueueScan)
	if err != nil {
		return nil, nil, dbErr("list liquidation candidates", err)
	}
	var cands []queued
	for rows.Next() {
		var c queued
		if err := rows.Scan(&c.acct, &c.mid); err != nil {
			rows.Close()
			return nil, nil, dbErr("scan liquidation candidate", err)
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, dbErr("list liquidation candidates", err)
	}
	var failures []QueueFailure
	now := s.now()
	var ranked []queued
	for _, c := range cands {
		a, err := s.st.accountByID(ctx, q, c.mid, false)
		if err != nil {
			failures = append(failures, QueueFailure{AccountID: c.acct, Err: err})
			continue
		}
		state, _, err := s.loadState(ctx, q, a, now)
		if err != nil {
			failures = append(failures, QueueFailure{AccountID: c.acct, Err: err})
			continue
		}
		ev, err := Evaluate(s.policy, state)
		if err != nil {
			failures = append(failures, QueueFailure{AccountID: c.acct, Err: err})
			continue
		}
		c.deficiency = ev.MaintenanceMargin.Sub(ev.Equity)
		ranked = append(ranked, c)
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
	return ranked, failures, nil
}

// DeficitResult is the outcome of covering a bankrupt case's deficit.
type DeficitResult struct {
	CaseID               CaseID
	Deficit              money.Quantity
	JournalTransactionID ledger.TransactionID
	// CustomerRestricted is true when covering the deficit moved the
	// customer account from ACTIVE to RESTRICTED.
	CustomerRestricted bool
	Replayed           bool
}

// CoverDeficit has the insurance fund cover a bankrupt case's deficit with a
// DEFICIT_COVERAGE posting: the customer's loan becomes a DEFICIT the
// customer owes, the lending pool is repaid in cash, and the insurance fund
// pays that cash and holds the claim (DEFICIT_RECEIVABLE). The deficit is
// the one the case recorded, and it must equal both the loans the books
// carry and what the lots owe; the fund must hold the cash. Exactly once per
// case.
//
// In the same transaction the customer account, which now owes the fund, is
// RESTRICTED (accounts.Repository.Transition, ACTIVE -> RESTRICTED): the
// status every new-risk path refuses (Risk Kernel V2 for risk-increasing
// intents, the settlement planner, funding, the faucet, margin itself), that
// withdrawals and customer transfers refuse (they need ACTIVE), and that
// buying power V2 blocks (no trade buying power, nothing withdrawable),
// while risk reduction stays open -- including RepayDeficit, which pays the
// fund back through a capital reservation like any other repayment. An
// account already RESTRICTED, FROZEN or CLOSED keeps its status: margin only
// ever tightens one. Lifting the restriction once the deficit is repaid is a
// person's decision (an accounts status change), not margin's.
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
	if err := s.lockFacilities(ctx, tx, a, poolBooks|fundBooks); err != nil {
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
			WithField("deficit", deficit.String()).WithField("fund_balance", cash.String())
	}
	now := s.now()
	m := s.meta(refLiquidationCase, c.ID.String(), "insurance fund covers a liquidation deficit")
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
	restricted, err := s.restrictCustomer(ctx, tx, a, c, deficit, now)
	if err != nil {
		return DeficitResult{}, err
	}
	return DeficitResult{CaseID: c.ID, Deficit: deficit, JournalTransactionID: out.Journal, CustomerRestricted: restricted}, nil
}

// restrictCustomer moves a customer account that owes a covered deficit from
// ACTIVE to RESTRICTED, with its transition row and an audit event. It never
// loosens a status: a RESTRICTED, FROZEN or CLOSED account is left alone.
func (s *Service) restrictCustomer(ctx context.Context, tx pgx.Tx, a Account, c Case, deficit money.Quantity, at time.Time) (bool, error) {
	cust, err := s.accounts.Get(ctx, tx, a.AccountID)
	if err != nil {
		return false, err
	}
	if cust.Status != accounts.StatusActive {
		return false, nil
	}
	reason := "margin case " + c.ID.String() + " ended with a deficit of " + deficit.String() +
		" base units that the insurance fund covered and the account owes"
	if _, err := s.accounts.Transition(ctx, tx, a.AccountID, accounts.StatusChange{To: accounts.StatusRestricted,
		ActorType: string(s.actor.Type), ActorID: s.actor.ID, Reason: reason}, at); err != nil {
		return false, err
	}
	if err := s.st.auditTransition(ctx, tx, a.AccountID, s.actor, "customer_account", a.AccountID.String(),
		string(accounts.StatusActive), string(accounts.StatusRestricted), reason, "", at); err != nil {
		return false, err
	}
	return true, nil
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
