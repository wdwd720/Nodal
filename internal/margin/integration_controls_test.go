//go:build integration

package margin

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
)

func reasonCodes(err error) []string {
	ee, ok := errs.As(err)
	if !ok {
		return nil
	}
	rc, _ := ee.Fields["reason_codes"].([]string)
	return rc
}

func (f *fixture) cashAccount(cash money.Quantity) accounts.AccountID {
	f.t.Helper()
	acct := f.account()
	f.fund(acct, f.usd, cash)
	f.open(acct, ModeCross)
	f.post(acct, f.usd, cash)
	return acct
}

// TestIntegration_ConcurrentBorrowsCannotSpendTheSameMarginTwice: sixteen
// borrows of 150 race against 1,000 of cash collateral, each in its own
// transaction. Every margin operation locks the account first, so each
// borrow sees the ones before it: exactly six succeed (900), the rest are
// refused by Risk Kernel V2, and the books owe exactly what was lent.
func TestIntegration_ConcurrentBorrowsCannotSpendTheSameMarginTwice(t *testing.T) {
	f := newFixture(t)
	facs := f.seed(usd(1_000_000), usd(100_000))
	acct := f.cashAccount(usd(1_000))
	f.mark("100")

	const n = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	errsCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := f.borrow(acct, usd(150), "race-"+uuid.NewString())
			errsCh <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errsCh)
	ok := 0
	for err := range errsCh {
		if err == nil {
			ok++
			continue
		}
		require.Equal(t, errs.CodeRiskRejected, errs.CodeOf(err), "%v", err)
		assert.Contains(t, reasonCodes(err), risk.ReasonMarginInitial)
	}
	assert.Equal(t, 6, ok, "1,000 of collateral covers six borrows of 150 and no more")
	requireQ(t, usd(900), f.customer(acct, ledger.CodeMarginLoan, f.usd), "the books owe what was lent")
	requireQ(t, usd(900), f.balance(facs.Pool.Account(ledger.CodeLoanReceivable, f.usd)), "the pool lent it")
	a, err := f.svc.Account(f.ctx, testDB, acct)
	require.NoError(t, err)
	var lots int
	var principal string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT count(*), coalesce(sum(principal),0)::text FROM margin_borrow_lots WHERE margin_account_id = $1`, a.ID).Scan(&lots, &principal))
	assert.Equal(t, ok, lots)
	assert.Equal(t, usd(900).String(), principal)
	r := f.evaluate(acct)
	assert.False(t, r.Evaluation.Equity.IsNegative(), "no borrow spent margin another had already used")
}

// TestIntegration_ConcurrentLeveragedBuysCannotSpendTheSameMarginTwice:
// sixteen leveraged buys race against one account; whatever succeeds, the
// account still covers its initial margin and stays within its leverage cap,
// and the books hold exactly the successful buys.
func TestIntegration_ConcurrentLeveragedBuysCannotSpendTheSameMarginTwice(t *testing.T) {
	f := newFixture(t)
	f.seed(usd(1_000_000), usd(100_000))
	acct := f.cashAccount(usd(1_000))
	f.mark("100")

	const n = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := f.spot(acct, sol(2), "spot-race-"+uuid.NewString())
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		if err == nil {
			ok++
			continue
		}
		require.Equal(t, errs.CodeRiskRejected, errs.CodeOf(err), "%v", err)
	}
	require.Positive(t, ok)
	require.Less(t, ok, n, "the account cannot carry every buy")
	requireQ(t, sol(int64(2*ok)), f.customer(acct, ledger.CodeMarginCollateral, f.sol), "positions")
	requireQ(t, units(t, "200.2").Mul(money.QuantityFromInt64(int64(ok))), f.customer(acct, ledger.CodeMarginLoan, f.usd), "loans")
	r := f.evaluate(acct)
	assert.True(t, r.Evaluation.Equity.Cmp(r.Evaluation.InitialMargin) >= 0, "equity %s below initial %s", r.Evaluation.Equity, r.Evaluation.InitialMargin)
	assert.LessOrEqual(t, r.Evaluation.EffectiveLeverageBPS, r.Evaluation.LeverageCapBPS)
	var held string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT coalesce(sum(quantity),0)::text FROM asset_reservations WHERE account_id = $1 AND status = 'ACTIVE'`, acct).Scan(&held))
	assert.Equal(t, "0", held, "every reservation was consumed")
}

// TestIntegration_InterestAccrualReplayIsIdempotent: interest is charged once
// per lot and UTC day, exactly, and a replay charges nothing; the database
// refuses a second accrual of a period and one that leaves a gap.
func TestIntegration_InterestAccrualReplayIsIdempotent(t *testing.T) {
	f := newFixture(t)
	facs := f.seed(usd(1_000_000), usd(100_000))
	acct := f.cashAccount(usd(5_000))
	f.mark("100")
	_, err := f.borrow(acct, usd(1_000), "accrue-"+uuid.NewString())
	require.NoError(t, err)
	a, err := f.svc.Account(f.ctx, testDB, acct)
	require.NoError(t, err)
	var lot Lot
	lots, err := f.svc.st.openLots(f.ctx, testDB, a.ID, false)
	require.NoError(t, err)
	require.Len(t, lots, 1)
	lot = lots[0]

	f.clk.Set(time.Date(2026, 10, 10, 6, 0, 0, 0, time.UTC))
	accrue := func() []Accrual {
		var out []Accrual
		require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = f.svc.AccrueInterest(ctx, tx, acct)
			return err
		}))
		return out
	}
	first := accrue()
	require.Len(t, first, 3, "the opening day and two full days; the running day is not complete")
	total := money.Quantity{}
	prev := lot.OpenedAt
	for _, acc := range first {
		assert.Equal(t, prev, acc.Period.Start, "contiguous")
		want, err := InterestFor(usd(1_000), 800, acc.Period.Start, acc.Period.End)
		require.NoError(t, err)
		requireQ(t, want, acc.Amount, "exact ACT/365 interest")
		total = total.Add(acc.Amount)
		prev = acc.Period.End
	}
	assert.Equal(t, time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), prev)
	requireQ(t, usd(1_000).Add(total), f.customer(acct, ledger.CodeMarginLoan, f.usd), "interest is capitalized")
	requireQ(t, total, f.balance(facs.Pool.Account(ledger.CodeInterestIncome, f.usd)), "the pool earned it")
	journals := f.countJournals(acct, ledger.KindMarginInterest)
	assert.Equal(t, 3, journals)

	assert.Empty(t, accrue(), "a replay charges nothing")
	assert.Equal(t, journals, f.countJournals(acct, ledger.KindMarginInterest))
	requireQ(t, usd(1_000).Add(total), f.customer(acct, ledger.CodeMarginLoan, f.usd), "unchanged")

	insert := func(start, end time.Time) error {
		_, err := testDB.Exec(f.ctx, `INSERT INTO margin_interest_accruals (id, lot_id, margin_account_id, period_start, period_end,
			principal_basis, rate_bps, amount) VALUES ($1,$2,$3,$4,$5,$6::numeric,800,0)`,
			uuid.New(), lot.ID, a.ID, start, end, usd(1_000).String())
		return err
	}
	err = insert(first[1].Period.Start, first[1].Period.End)
	require.Error(t, err, "a period is charged once")
	assert.Equal(t, "MG005", db.SQLState(err))
	err = insert(time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC))
	require.Error(t, err, "no gaps: an accrual starts where the last one ended")
	assert.Equal(t, "MG005", db.SQLState(err))
}

// TestIntegration_RepaymentPaysInterestFirstAndClosesTheLot.
func TestIntegration_RepaymentPaysInterestFirstAndClosesTheLot(t *testing.T) {
	f := newFixture(t)
	facs := f.seed(usd(1_000_000), usd(100_000))
	acct := f.cashAccount(usd(5_000))
	f.mark("100")
	_, err := f.borrow(acct, usd(1_000), "repay-borrow-"+uuid.NewString())
	require.NoError(t, err)
	f.clk.Advance(48 * time.Hour)
	repay := func(amount money.Quantity, key string) (LoanResult, error) {
		var r LoanResult
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			r, err = f.svc.Repay(ctx, tx, LoanRequest{AccountID: acct, Amount: amount, IdempotencyKey: key, Requester: userRequester})
			return err
		})
		return r, err
	}
	key := "repay-" + uuid.NewString()
	part, err := repay(usd(100), key)
	require.NoError(t, err)
	interest := f.customer(acct, ledger.CodeInterestExpense, f.usd)
	require.True(t, interest.IsPositive(), "two days accrued to the instant before the repayment")
	require.Len(t, part.Allocations, 1)
	requireQ(t, interest, part.Allocations[0].Interest, "interest first")
	requireQ(t, usd(100).Sub(interest), part.Allocations[0].Principal, "then principal")
	requireQ(t, usd(1_000).Sub(usd(100).Sub(interest)), f.customer(acct, ledger.CodeMarginLoan, f.usd), "remaining loan")

	again, err := repay(usd(100), key)
	require.NoError(t, err)
	assert.True(t, again.Replayed, "a repayment is exactly once")
	assert.Equal(t, part.JournalTransactionID, again.JournalTransactionID)
	_, err = repay(usd(101), key)
	require.Error(t, err)
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, errs.CodeOf(err))

	_, err = repay(usd(10_000), "too-much-"+uuid.NewString())
	require.Error(t, err, "repaying more than is owed is refused")

	f.fund(acct, f.usd, usd(1_000))
	rest := f.customer(acct, ledger.CodeMarginLoan, f.usd)
	_, err = repay(rest, "rest-"+uuid.NewString())
	require.NoError(t, err)
	requireQ(t, money.Quantity{}, f.customer(acct, ledger.CodeMarginLoan, f.usd), "nothing owed")
	a, err := f.svc.Account(f.ctx, testDB, acct)
	require.NoError(t, err)
	var reason string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT close_reason FROM margin_borrow_lots WHERE margin_account_id = $1`, a.ID).Scan(&reason))
	assert.Equal(t, string(CloseRepaid), reason)
	requireQ(t, money.Quantity{}, f.balance(facs.Pool.Account(ledger.CodeLoanReceivable, f.usd)), "the pool is repaid")
	requirePartiesBalance(t, f, acct.String(), facs.Pool.ID.String())
}

// TestIntegration_BorrowAndLeveragedBuyAreExactlyOnce: a replayed borrow or
// buy returns the first outcome and posts nothing; the same key with a
// different request is refused.
func TestIntegration_BorrowAndLeveragedBuyAreExactlyOnce(t *testing.T) {
	f := newFixture(t)
	f.seed(usd(1_000_000), usd(100_000))
	acct := f.cashAccount(usd(1_000))
	f.mark("100")
	key := "once-" + uuid.NewString()
	first, err := f.borrow(acct, usd(100), key)
	require.NoError(t, err)
	second, err := f.borrow(acct, usd(100), key)
	require.NoError(t, err)
	assert.True(t, second.Replayed)
	assert.Equal(t, first.JournalTransactionID, second.JournalTransactionID)
	assert.Equal(t, *first.LotID, *second.LotID)
	assert.Equal(t, 1, f.countJournals(acct, ledger.KindMarginBorrow))
	_, err = f.borrow(acct, usd(200), key)
	require.Error(t, err)
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, errs.CodeOf(err))

	skey := "spot-once-" + uuid.NewString()
	buy, err := f.spot(acct, sol(1), skey)
	require.NoError(t, err)
	trades := f.countJournals(acct, ledger.KindSimTrade)
	replay, err := f.spot(acct, sol(1), skey)
	require.NoError(t, err)
	assert.True(t, replay.Replayed)
	assert.Equal(t, buy.TradeJournal, replay.TradeJournal)
	assert.Equal(t, buy.BorrowJournal, replay.BorrowJournal)
	requireQ(t, buy.Cost, replay.Cost, "cost")
	assert.Equal(t, trades, f.countJournals(acct, ledger.KindSimTrade))
	requireQ(t, sol(1), f.customer(acct, ledger.CodeMarginCollateral, f.sol), "one position, not two")
}

// TestIntegration_DatabaseRefusesInvalidMarginStates goes around Go: the
// database itself refuses a case beyond its step budget, a step count that
// moves without a step, a step out of sequence or above the slippage cap, a
// status change without its transition row, an illegal transition, a
// margin account born in the wrong status or wired to the wrong facility,
// an identity change, and interest that appears without an accrual.
func TestIntegration_DatabaseRefusesInvalidMarginStates(t *testing.T) {
	f := newFixture(t)
	facs := f.seed(usd(1_000_000), usd(100_000))
	acct, a := f.fixedAccountOnBooks()
	f.mark("75")
	f.clk.Advance(2 * time.Minute) // stale: the case waits with no step
	res, err := f.liquidate(acct, 0)
	require.NoError(t, err)
	require.Equal(t, CaseAwaitingPrice, res.Case.Status)
	cid := res.Case.ID

	exec := func(sql string, args ...any) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, args...)
			return err
		})
	}
	// The step budget.
	err = exec(`UPDATE margin_liquidation_cases SET steps_executed = max_steps + 1 WHERE id = $1`, cid)
	require.Error(t, err)
	assert.Equal(t, "MG004", db.SQLState(err), "%v", err)
	err = exec(`UPDATE margin_liquidation_cases SET steps_executed = 1 WHERE id = $1`, cid)
	require.Error(t, err, "a step count moves only over a recorded step")
	assert.Equal(t, "MG004", db.SQLState(err))
	step := func(no int, slip int64) error {
		return exec(`INSERT INTO margin_liquidation_steps (id, case_id, step_no, action, asset_id, quantity, mark_mantissa, mark_scale,
			mark_observed_at, execution_mantissa, execution_scale, slippage_bps, slippage_cap_bps, proceeds, fee, repaid, excess,
			trade_journal_transaction_id, equity_before, equity_after, plan_hash, executed_at)
			VALUES ($1,$2,$3,'SELL',$4,1,75,0,now(),75 * (10000 - $5),4,$5,500,1,1,0,0,
			        (SELECT borrow_journal_transaction_id FROM margin_borrow_lots WHERE margin_account_id = $6 LIMIT 1),0,0,'h',now())`,
			uuid.New(), cid, no, f.sol, slip, a.ID)
	}
	err = step(2, 25)
	require.Error(t, err, "step 2 cannot come first")
	assert.Equal(t, "MG004", db.SQLState(err))
	err = step(1, 600)
	require.Error(t, err, "no step trades beyond the slippage cap")
	assert.Contains(t, err.Error(), "margin_liquidation_steps_slippage_capped")
	err = exec(`UPDATE margin_liquidation_cases SET max_steps = 50 WHERE id = $1`, cid)
	require.Error(t, err, "the budget is frozen")
	// A recorded step the case does not count is refused at COMMIT.
	err = step(1, 25)
	require.Error(t, err)
	assert.Equal(t, "MG004", db.SQLState(err))

	// Only a terminal case carries closed_at, and every terminal case does
	// (margin_liquidation_cases_closed): an open case cannot be marked closed.
	err = exec(`UPDATE margin_liquidation_cases SET closed_at = now() WHERE id = $1`, cid)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "margin_liquidation_cases_closed", "%v", err)

	// A status change without a transition row is refused at COMMIT.
	err = exec(`UPDATE margin_liquidation_cases SET status = 'IN_PROGRESS' WHERE id = $1`, cid)
	require.Error(t, err)
	assert.Equal(t, "AU001", db.SQLState(err))
	err = exec(`UPDATE margin_accounts SET status = 'NORMAL' WHERE id = $1`, a.ID)
	require.Error(t, err)
	assert.Equal(t, "AU001", db.SQLState(err))
	// Checking the bindings immediately (as every margin operation does)
	// makes them no weaker: the bare change is refused at the statement.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if err := bindingsImmediate(ctx, tx); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE margin_accounts SET status = 'NORMAL' WHERE id = $1`, a.ID)
		require.Error(t, err, "refused before COMMIT")
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "AU001", db.SQLState(err))
	// An illegal edge is refused even with a row describing it.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO margin_account_transitions (id, margin_account_id, from_status, to_status, reason, actor_type, actor_id, occurred_at)
			VALUES ($1,$2,'LIQUIDATING','CLOSED','bypass','SYSTEM','x',now())`, uuid.New(), a.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE margin_accounts SET status = 'CLOSED' WHERE id = $1`, a.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "MG003", db.SQLState(err))

	// Born in the wrong status, or wired to the wrong facility.
	other := f.account()
	insertAccount := func(status string, pool, fund any) error {
		return exec(`INSERT INTO margin_accounts (id, account_id, margin_mode, mode, status, policy_version, numeraire_asset_id,
			lending_pool_facility_id, insurance_fund_facility_id, opened_by_actor_type, opened_by_actor_id, opened_at)
			VALUES ($1,$2,'CROSS','PAPER',$3,$4,$5,$6,$7,'SYSTEM','x',now())`,
			uuid.New(), other, status, f.cfg.Policy.Version, f.usd, pool, fund)
	}
	err = insertAccount("LIQUIDATING", facs.Pool.ID, facs.Fund.ID)
	require.Error(t, err)
	assert.Equal(t, "MG001", db.SQLState(err))
	err = insertAccount("NORMAL", facs.Fund.ID, facs.Fund.ID)
	require.Error(t, err, "a margin account borrows only from a lending pool")
	assert.Equal(t, "MG006", db.SQLState(err))
	err = exec(`INSERT INTO margin_accounts (id, account_id, margin_mode, mode, status, policy_version, numeraire_asset_id,
		lending_pool_facility_id, insurance_fund_facility_id, opened_by_actor_type, opened_by_actor_id, opened_at)
		VALUES ($1,$2,'CROSS','LIVE','NORMAL',$3,$4,$5,$6,'SYSTEM','x',now())`, uuid.New(), other, f.cfg.Policy.Version, f.usd, facs.Pool.ID, facs.Fund.ID)
	require.Error(t, err, "a margin account is never LIVE")
	assert.Contains(t, err.Error(), "margin_accounts_mode_check")

	// Identity is frozen: the application role cannot name the column, and
	// the migration owner is stopped by the trigger.
	err = exec(`UPDATE margin_accounts SET margin_mode = 'CASH' WHERE id = $1`, a.ID)
	require.Error(t, err)
	assert.Equal(t, "42501", db.SQLState(err))
	owner, err := pgx.Connect(f.ctx, testMigrateURL)
	require.NoError(t, err)
	defer owner.Close(f.ctx)
	_, err = owner.Exec(f.ctx, `UPDATE margin_accounts SET margin_mode = 'CASH' WHERE id = $1`, a.ID)
	require.Error(t, err)
	assert.Equal(t, "MG002", db.SQLState(err))

	// Interest never appears without an accrual, and principal is never re-lent.
	err = exec(`UPDATE margin_borrow_lots SET outstanding_interest = outstanding_interest + 1 WHERE margin_account_id = $1`, a.ID)
	require.Error(t, err)
	assert.Equal(t, "MG005", db.SQLState(err))
	var lid string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT id FROM margin_borrow_lots WHERE margin_account_id = $1 LIMIT 1`, a.ID).Scan(&lid))
	_, err = owner.Exec(f.ctx, `UPDATE margin_borrow_lots SET principal = principal + 1 WHERE id = $1`, lid)
	require.Error(t, err)
	assert.Equal(t, "MG002", db.SQLState(err))

	// History is history.
	err = exec(`DELETE FROM margin_account_transitions WHERE margin_account_id = $1`, a.ID)
	require.Error(t, err)
}

// TestIntegration_ReleaseThatWouldBreachMaintenanceIsRefused: the fixed
// account (equity 500, initial 400, maintenance 200) may release 50 of cash
// but not 200 (below initial) and never 400 (below maintenance:
// RISK_WITHDRAW_BREACHES_MARGIN).
func TestIntegration_ReleaseThatWouldBreachMaintenanceIsRefused(t *testing.T) {
	f := newFixture(t)
	f.seed(usd(1_000_000), usd(100_000))
	acct, _ := f.fixedAccountOnBooks()
	release := func(asset assets.AssetID, qty money.Quantity, req Requester) (CollateralResult, error) {
		var r CollateralResult
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			r, err = f.svc.ReleaseCollateral(ctx, tx, CollateralRequest{AccountID: acct, AssetID: asset, Quantity: qty,
				IdempotencyKey: "release-" + uuid.NewString(), Requester: req})
			return err
		})
		return r, err
	}
	_, err := release(f.usd, usd(400), userRequester)
	require.Error(t, err)
	assert.Equal(t, errs.CodeRiskRejected, errs.CodeOf(err))
	assert.Contains(t, reasonCodes(err), risk.ReasonWithdrawBreachesMargin)

	_, err = release(f.usd, usd(200), userRequester)
	require.Error(t, err)
	assert.Contains(t, reasonCodes(err), risk.ReasonMarginInitial)
	assert.NotContains(t, reasonCodes(err), risk.ReasonWithdrawBreachesMargin, "equity 300 is still above maintenance")

	before := f.customer(acct, ledger.CodeWallet, f.usd)
	r, err := release(f.usd, usd(50), userRequester)
	require.NoError(t, err)
	assert.Equal(t, StatusNormal, r.Status)
	requireQ(t, before.Add(usd(50)), f.customer(acct, ledger.CodeWallet, f.usd), "released to the wallet")
	requireQ(t, usd(950), f.customer(acct, ledger.CodeMarginCollateral, f.usd), "out of the margin account")

	// An agent can never pull collateral: TRANSFER_VALUE is forbidden to it.
	agent := Requester{Type: security.ActorAgent, ID: "agent-1", AgentID: uuid.NewString()}
	_, err = release(f.usd, usd(1), agent)
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
}

// TestIntegration_StaleMarksBlockNewLeverage: with the SOL mark stale a
// borrow and a leveraged buy are refused with RISK_ORACLE_STALE; with no
// mark at all, RISK_ORACLE_UNAVAILABLE; a fresh mark lets them through.
func TestIntegration_StaleMarksBlockNewLeverage(t *testing.T) {
	f := newFixture(t)
	f.seed(usd(1_000_000), usd(100_000))
	fresh := f.cashAccount(usd(1_000))
	_, err := f.spot(fresh, sol(1), "no-mark-"+uuid.NewString())
	require.Error(t, err, "nothing to price the buy at")
	assert.Contains(t, reasonCodes(err), risk.ReasonOracleUnavailable)

	acct, _ := f.fixedAccountOnBooks()
	f.clk.Advance(2 * time.Minute)
	_, err = f.borrow(acct, usd(1), "stale-"+uuid.NewString())
	require.Error(t, err)
	assert.Equal(t, errs.CodeRiskRejected, errs.CodeOf(err))
	assert.Contains(t, reasonCodes(err), risk.ReasonOracleStale)
	_, err = f.spot(acct, sol(1), "stale-spot-"+uuid.NewString())
	require.Error(t, err)
	assert.Contains(t, reasonCodes(err), risk.ReasonOracleStale)

	f.mark("110")
	_, err = f.borrow(acct, usd(1), "fresh-"+uuid.NewString())
	require.NoError(t, err)
}

// TestIntegration_CashAndIsolatedAccountsKeepTheirShape: a CASH account
// never borrows or buys on margin, an ISOLATED account holds only its
// numeraire and its one asset, and an agent never opens an account.
func TestIntegration_CashAndIsolatedAccountsKeepTheirShape(t *testing.T) {
	f := newFixture(t)
	f.seed(usd(1_000_000), usd(100_000))
	f.mark("100")
	cash := f.account()
	f.fund(cash, f.usd, usd(100))
	f.open(cash, ModeCash)
	f.post(cash, f.usd, usd(100))
	_, err := f.borrow(cash, usd(1), "cash-"+uuid.NewString())
	require.Error(t, err)
	assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
	_, err = f.spot(cash, sol(1), "cash-spot-"+uuid.NewString())
	require.Error(t, err)
	assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
	closeCash := func() (Account, error) {
		var a Account
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			a, err = f.svc.CloseAccount(ctx, tx, cash, f.actor)
			return err
		})
		return a, err
	}
	_, err = closeCash()
	require.Error(t, err, "an account that holds collateral does not close")
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.ReleaseCollateral(ctx, tx, CollateralRequest{AccountID: cash, AssetID: f.usd, Quantity: usd(100),
			IdempotencyKey: "cash-out-" + uuid.NewString(), Requester: userRequester})
		return err
	}), "an account that owes nothing releases everything")
	closedCash, err := closeCash()
	require.NoError(t, err)
	assert.Equal(t, StatusClosed, closedCash.Status)

	iso := f.account()
	eth := f.simAsset("METH", 9, assets.RiskMajor)
	f.fund(iso, eth, sol(1))
	f.open(iso, ModeIsolated)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.PostCollateral(ctx, tx, CollateralRequest{AccountID: iso, AssetID: eth, Quantity: sol(1),
			IdempotencyKey: "iso-" + uuid.NewString(), Requester: userRequester})
		return err
	})
	require.Error(t, err, "an ISOLATED account holds only its own asset")

	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.OpenAccount(ctx, tx, OpenRequest{AccountID: f.account(), MarginMode: ModeCross, Mode: gates.ModePaper,
			NumeraireAssetID: f.usd, Actor: econ.Actor{Type: security.ActorAgent, ID: "agent"}})
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.OpenAccount(ctx, tx, OpenRequest{AccountID: f.account(), MarginMode: ModeCross, Mode: gates.ModeLive,
			NumeraireAssetID: f.usd, Actor: f.actor})
		return err
	})
	require.Error(t, err, "never LIVE")
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.OpenAccount(ctx, tx, OpenRequest{AccountID: f.account(), MarginMode: ModeCross, Mode: gates.ModePaper,
			NumeraireAssetID: f.sol, Actor: f.actor})
		return err
	})
	require.Error(t, err, "the numeraire is a USD-pegged simulated dollar, rendered in cents at parity")
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

// TestIntegration_KillSwitchStopsNewRiskButNeverLiquidation: with MARGIN and
// LEVERAGE switched off, borrowing is refused while posting collateral and
// liquidating -- risk reduction -- still run.
func TestIntegration_KillSwitchStopsNewRiskButNeverLiquidation(t *testing.T) {
	f := newFixture(t)
	f.seed(usd(1_000_000), usd(100_000))
	acct, _ := f.fixedAccountOnBooks()
	f.fund(acct, f.usd, usd(10))
	killed := killReader{snap: killswitch.Snapshot{Switches: []killswitch.Switch{
		{Kind: killswitch.CapabilityDisable, ScopeID: string(gates.Margin), Active: true},
		{Kind: killswitch.CapabilityDisable, ScopeID: string(gates.Leverage), Active: true},
	}}}
	svc, err := NewService(f.cfg, f.dependencies(killed))
	require.NoError(t, err)

	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := svc.Borrow(ctx, tx, LoanRequest{AccountID: acct, Amount: usd(1), IdempotencyKey: "killed-" + uuid.NewString(), Requester: userRequester})
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(err))

	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := svc.PostCollateral(ctx, tx, CollateralRequest{AccountID: acct, AssetID: f.usd, Quantity: usd(10),
			IdempotencyKey: "killed-post-" + uuid.NewString(), Requester: userRequester})
		return err
	}), "posting collateral reduces risk")

	f.mark("70")
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		res, err := svc.Liquidate(ctx, tx, LiquidateRequest{AccountID: acct})
		if err == nil && res.Step == nil {
			t.Errorf("liquidation did not run under the kill switch: %s", res.Decision)
		}
		return err
	}), "liquidation is never stopped by a kill switch")
}

// TestIntegration_RejectionIsRecordedWhenTheCallerCommits: a refused borrow
// writes its REJECT decision and its REJECTED intent in the caller's
// transaction; committed, a replay returns the same refusal without
// deciding again.
func TestIntegration_RejectionIsRecordedWhenTheCallerCommits(t *testing.T) {
	f := newFixture(t)
	f.seed(usd(1_000_000), usd(100_000))
	acct := f.cashAccount(usd(100))
	f.mark("100")
	key := "refused-" + uuid.NewString()
	var refusal error
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, refusal = f.svc.Borrow(ctx, tx, LoanRequest{AccountID: acct, Amount: usd(500), IdempotencyKey: key, Requester: userRequester})
		return nil // commit the refusal
	}))
	require.Error(t, refusal)
	assert.Equal(t, errs.CodeRiskRejected, errs.CodeOf(refusal))
	var status, code string
	var intentID uuid.UUID
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT id, status, rejection_code FROM economic_intents WHERE account_id = $1 AND idempotency_key = $2`,
		acct, key).Scan(&intentID, &status, &code))
	assert.Equal(t, string(econ.StatusRejected), status)
	assert.Equal(t, risk.ReasonMarginInitial, code)
	countDecisions := func() int {
		var n int
		require.NoError(t, testDB.QueryRow(f.ctx, `SELECT count(*) FROM advanced_risk_decisions WHERE intent_id = $1 AND verdict = 'REJECT'`, intentID).Scan(&n))
		return n
	}
	assert.Equal(t, 1, countDecisions())
	_, err := f.borrow(acct, usd(500), key)
	require.Error(t, err)
	assert.Equal(t, errs.CodeRiskRejected, errs.CodeOf(err))
	assert.Equal(t, 1, countDecisions(), "a replay does not decide again")
}

// TestIntegration_SQLAndGoAgreeOnTransitionTables compares every legal-edge
// function in SQL with its Go twin.
func TestIntegration_SQLAndGoAgreeOnTransitionTables(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	check := func(fn string, statuses []string, goLegal func(a, b string) bool) {
		for _, from := range statuses {
			for _, to := range statuses {
				var legal bool
				require.NoError(t, testDB.QueryRow(ctx, `SELECT `+fn+`($1,$2)`, from, to).Scan(&legal))
				assert.Equal(t, goLegal(from, to), legal, "%s %s -> %s", fn, from, to)
			}
		}
	}
	var accts, cases, calls []string
	for _, s := range AllAccountStatuses() {
		accts = append(accts, string(s))
	}
	for _, s := range AllCaseStatuses() {
		cases = append(cases, string(s))
	}
	for _, s := range AllCallStatuses() {
		calls = append(calls, string(s))
	}
	check("cp_margin_account_transition_legal", accts, func(a, b string) bool { return CanTransitionAccount(AccountStatus(a), AccountStatus(b)) })
	check("cp_margin_case_transition_legal", cases, func(a, b string) bool { return CanTransitionCase(CaseStatus(a), CaseStatus(b)) })
	check("cp_margin_call_transition_legal", calls, func(a, b string) bool { return CanTransitionCall(CallStatus(a), CallStatus(b)) })
}

// TestIntegration_BuyingPowerV2SeesTheMarginEngine: the universal buying power
// reads an account's margin through BuyingPowerReader, so its liabilities and
// requirements are exactly the engine's (converted conservatively), and a
// customer with no margin account is a cash account.
func TestIntegration_BuyingPowerV2SeesTheMarginEngine(t *testing.T) {
	f := newFixture(t)
	f.seed(usd(1_000_000), usd(100_000))
	acct, _ := f.fixedAccountOnBooks()

	reader := f.svc.BuyingPowerReader()
	st, ok, err := reader.MarginState(f.ctx, testDB, "CUSTOMER", acct.String())
	require.NoError(t, err)
	require.True(t, ok, "a margin account reports its state")
	assert.Equal(t, "CROSS", st.Mode)
	assert.Equal(t, "2000.00", st.BorrowLiabilities.String(), "the 2,000 borrowed")
	assert.True(t, st.MaintenanceMargin.IsPositive())
	assert.True(t, st.InitialMarginUsed.IsPositive())

	state, err := f.svc.State(f.ctx, testDB, acct)
	require.NoError(t, err)
	e, err := Evaluate(f.svc.Policy(), state)
	require.NoError(t, err)
	want, _, err := v2State(e)
	require.NoError(t, err)
	assert.Equal(t, want, st, "buying power sees exactly the engine's evaluation")

	_, ok, err = reader.MarginState(f.ctx, testDB, "CUSTOMER", f.account().String())
	require.NoError(t, err)
	assert.False(t, ok, "no margin account: a cash account")
	_, ok, err = reader.MarginState(f.ctx, testDB, "HOUSE", acct.String())
	require.NoError(t, err)
	assert.False(t, ok, "only customers have margin accounts")
}
