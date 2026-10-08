//go:build integration

package margin

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// TestIntegration_FixedAccountOnTheBooksEvaluatesLikeTheEngine: the account
// the service builds from the ledger, the lots and the price store is the
// fixed account of the unit tests, figure for figure.
func TestIntegration_FixedAccountOnTheBooksEvaluatesLikeTheEngine(t *testing.T) {
	f := newFixture(t)
	f.seed(usd(1_000_000), usd(100_000))
	acct, _ := f.fixedAccountOnBooks()
	r := f.evaluate(acct)
	assert.Equal(t, StatusNormal, r.Status)
	requireQ(t, usd(500), r.Evaluation.Equity, "equity")
	requireQ(t, usd(400), r.Evaluation.InitialMargin, "initial")
	requireQ(t, usd(200), r.Evaluation.MaintenanceMargin, "maintenance")
	assert.Equal(t, money.BPS(40_000), r.Evaluation.EffectiveLeverageBPS)
	requireQ(t, usd(2_000), f.customer(acct, ledger.CodeMarginLoan, f.usd), "the loan")
	requireQ(t, usd(2_000), f.customer(acct, ledger.CodeWallet, f.usd), "the borrowed cash is in the wallet")
}

func (f *fixture) spot(acct accounts.AccountID, qty money.Quantity, key string) (SpotResult, error) {
	var r SpotResult
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = f.svc.OpenLeveragedSpot(ctx, tx, SpotRequest{AccountID: acct, AssetID: f.sol, Quantity: qty, IdempotencyKey: key, Requester: userRequester})
		return err
	})
	return r, err
}

func (f *fixture) coverDeficit(cid CaseID) (DeficitResult, error) {
	var r DeficitResult
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = f.svc.CoverDeficit(ctx, tx, cid)
		return err
	})
	return r, err
}

// requirePartiesBalance checks, over every journal entry ever posted for the
// given owners, that each owner's books balance per asset: Σ debits = Σ
// credits for every (owner, asset). Every simulated-only posting balances
// per party, so their sum must too.
func requirePartiesBalance(t *testing.T, f *fixture, owners ...string) {
	t.Helper()
	rows, err := testDB.Query(f.ctx, `SELECT la.owner_type, la.owner_id::text, e.asset_id::text,
		sum(CASE WHEN e.side = 'DEBIT' THEN e.quantity ELSE -e.quantity END)::text
		FROM journal_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
		WHERE la.owner_id::text = ANY($1) GROUP BY 1, 2, 3`, owners)
	require.NoError(t, err)
	defer rows.Close()
	n := 0
	for rows.Next() {
		var ot, oid, asset, net string
		require.NoError(t, rows.Scan(&ot, &oid, &asset, &net))
		assert.Equal(t, "0", net, "%s %s asset %s is out of balance", ot, oid, asset)
		n++
	}
	require.NoError(t, rows.Err())
	require.Positive(t, n)
}

// TestIntegration_ScenarioB_FromFundingToACoveredDeficit is the whole margin
// lifecycle on real books: fund, post collateral, buy on margin, the mark
// falls (WARNING), falls further (MARGIN_CALL, a call issued), crashes; the
// liquidation sells everything, applies the cash, ends BANKRUPT; the
// insurance fund covers the deficit. Then every party's books balance and
// the customer, pool and fund balances reconcile with each other.
func TestIntegration_ScenarioB_FromFundingToACoveredDeficit(t *testing.T) {
	f := newFixture(t)
	facs := f.seed(usd(1_000_000), usd(100_000))
	acct := f.account()
	f.fund(acct, f.usd, usd(1_000))
	a := f.open(acct, ModeCross)
	assert.Equal(t, StatusNormal, a.Status)
	f.mark("100")
	f.post(acct, f.usd, usd(1_000))

	// Buy 20 SOL on margin at 100 plus the disclosed 10 BPS: 2,002 borrowed.
	sp, err := f.spot(acct, sol(20), "buy-"+uuid.NewString())
	require.NoError(t, err)
	requireQ(t, usd(2_002), sp.Cost, "cost at mark × 1.001")
	assert.Equal(t, money.BPS(10), sp.SlippageBPS)
	assert.Equal(t, StatusNormal, sp.Status)
	requireQ(t, sol(20), f.customer(acct, ledger.CodeMarginCollateral, f.sol), "the position is collateral")
	requireQ(t, money.Quantity{}, f.customer(acct, ledger.CodeWallet, f.usd), "the borrowed cash bought the position")
	requireQ(t, usd(2_002), f.customer(acct, ledger.CodeMarginLoan, f.usd), "the loan")

	// 79: equity 15 × 79 − 1,002 ≈ 183 is below 158 + 25% = 197.5 but above 158.
	f.clk.Advance(time.Minute)
	f.mark("79")
	r := f.evaluate(acct)
	assert.Equal(t, StatusWarning, r.Status)
	assert.Nil(t, r.Call)

	// 76: equity ≈ 138 is below maintenance 152: a margin call.
	f.clk.Advance(time.Minute)
	f.mark("76")
	r = f.evaluate(acct)
	require.Equal(t, StatusMarginCall, r.Status)
	require.NotNil(t, r.Call)
	assert.Equal(t, CallOpen, r.Call.Status)
	assert.True(t, r.Evaluation.LiquidationEligible)

	// 40: the mark crashes before anything is sold.
	f.clk.Advance(time.Minute)
	f.mark("40")
	step1, err := f.liquidate(acct, 1)
	require.NoError(t, err)
	require.Equal(t, DecisionSellAll, step1.Decision)
	require.NotNil(t, step1.Step)
	assert.Equal(t, StatusLiquidating, step1.AccountStatus)
	assert.Equal(t, CaseInProgress, step1.Case.Status)
	requireQ(t, sol(20), step1.Step.Quantity, "every SOL is sold")
	assert.LessOrEqual(t, step1.Step.SlippageBPS, step1.Step.SlippageCapBPS)
	// 20 × 40 × 0.9975 = 798 of proceeds and a 1% fee of 7.98 to the fund.
	requireQ(t, usd(798), step1.Step.Proceeds, "proceeds")
	requireQ(t, units(t, "7.98"), step1.Step.Fee, "fee")
	requireQ(t, units(t, "790.02"), step1.Step.Repaid, "repaid")

	step2, err := f.liquidate(acct, 2)
	require.NoError(t, err)
	require.Equal(t, DecisionApplyCollateral, step2.Decision)
	requireQ(t, usd(1_000), step2.Step.Quantity, "the cash is applied")

	final, err := f.liquidate(acct, 0)
	require.NoError(t, err)
	require.Equal(t, DecisionBankrupt, final.Decision)
	assert.Equal(t, StatusBankrupt, final.AccountStatus)
	assert.Equal(t, CaseBankrupt, final.Case.Status)
	require.NotNil(t, final.Case.DeficitUnits)
	deficit := *final.Case.DeficitUnits
	// 2,002 + three minutes of interest − 790.02 − 1,000.
	assert.True(t, deficit.Cmp(units(t, "211.98")) > 0 && deficit.Cmp(units(t, "211.99")) < 0, "deficit %s", deficit)
	requireQ(t, deficit, f.customer(acct, ledger.CodeMarginLoan, f.usd), "the deficit is the loan left")

	// Liquidation never runs on a bankrupt account again.
	_, err = f.liquidate(acct, 0)
	require.Error(t, err)

	cover, err := f.coverDeficit(final.Case.ID)
	require.NoError(t, err)
	requireQ(t, deficit, cover.Deficit, "covered")
	again, err := f.coverDeficit(final.Case.ID)
	require.NoError(t, err)
	assert.True(t, again.Replayed, "a deficit is covered exactly once")
	assert.Equal(t, cover.JournalTransactionID, again.JournalTransactionID)

	// The customer owes the deficit; the loan is gone; every lot is closed.
	requireQ(t, money.Quantity{}, f.customer(acct, ledger.CodeMarginLoan, f.usd), "no loan")
	requireQ(t, deficit, f.customer(acct, ledger.CodeDeficit, f.usd), "the customer's deficit")
	var open int
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT count(*) FROM margin_borrow_lots WHERE margin_account_id = $1 AND closed_at IS NULL`, a.ID).Scan(&open))
	assert.Zero(t, open)

	// The pool is whole: its receivable is gone and its cash is its capital
	// plus the interest it earned.
	pool, fund := facs.Pool, facs.Fund
	interest := f.balance(pool.Account(ledger.CodeInterestIncome, f.usd))
	assert.True(t, interest.IsPositive())
	requireQ(t, money.Quantity{}, f.balance(pool.Account(ledger.CodeLoanReceivable, f.usd)), "pool receivable")
	requireQ(t, usd(1_000_000).Add(interest), f.balance(pool.Account(ledger.CodePoolCash, f.usd)), "pool cash")
	requireQ(t, f.customer(acct, ledger.CodeInterestExpense, f.usd), interest, "interest paid = interest earned")

	// The fund paid the deficit, kept the fee and holds the claim.
	requireQ(t, usd(100_000).Add(step1.Step.Fee), f.balance(fund.Account(ledger.CodeInsuranceFundCapital, f.usd)), "fund capital")
	requireQ(t, deficit, f.balance(fund.Account(ledger.CodeDeficitReceivable, f.usd)), "fund claim = customer deficit")
	requireQ(t, usd(100_000).Add(step1.Step.Fee).Sub(deficit), f.balance(fund.Account(ledger.CodeInsuranceFundCash, f.usd)), "fund cash")
	requireQ(t, step1.Step.Fee, f.customer(acct, ledger.CodeFeesPlatform, f.usd), "fee paid = fee received")

	requirePartiesBalance(t, f, acct.String(), pool.ID.String(), fund.ID.String())

	// Every margin effect of the account is APPLIED, backed by a journal and
	// by an archived evidence document.
	var notApplied, effects int
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT count(*) FILTER (WHERE status <> 'APPLIED' OR journal_transaction_id IS NULL
		OR evidence_ref IS NULL OR evidence_sha256 IS NULL), count(*) FROM economic_effects WHERE subsystem = 'MARGIN' AND account_id = $1`,
		acct).Scan(&notApplied, &effects))
	assert.Zero(t, notApplied)
	assert.GreaterOrEqual(t, effects, 9, "posts, borrow, trade, collateral, interest, liquidation, repayments, release, coverage")

	// The margin call was LIQUIDATED when the case opened; the transitions
	// tell the whole story.
	var callStatus string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT status FROM margin_calls WHERE id = $1`, r.Call.ID).Scan(&callStatus))
	assert.Equal(t, string(CallLiquidated), callStatus)
	var path []string
	rows, err := testDB.Query(f.ctx, `SELECT to_status FROM margin_account_transitions WHERE margin_account_id = $1 ORDER BY occurred_at, created_at`, a.ID)
	require.NoError(t, err)
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		path = append(path, s)
	}
	rows.Close()
	assert.Equal(t, []string{"WARNING", "MARGIN_CALL", "LIQUIDATING", "BANKRUPT"}, path)

	// Covered, the bankrupt account holds and owes the pool nothing and can
	// close; the customer's deficit stays on the books.
	var closed Account
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		closed, err = f.svc.CloseAccount(ctx, tx, acct, f.actor)
		return err
	}))
	assert.Equal(t, StatusClosed, closed.Status)
	requireQ(t, deficit, f.customer(acct, ledger.CodeDeficit, f.usd), "the deficit is still owed")
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.PostCollateral(ctx, tx, CollateralRequest{AccountID: acct, AssetID: f.usd, Quantity: usd(1),
			IdempotencyKey: "after-close-" + uuid.NewString(), Requester: userRequester})
		return err
	})
	require.Error(t, err, "a closed account takes nothing")
}

// TestIntegration_PartialLiquidationRestoresTheAccount: below maintenance at
// 75, one step sells the least SOL that restores maintenance plus the
// buffer; the case completes and the account is NORMAL again.
func TestIntegration_PartialLiquidationRestoresTheAccount(t *testing.T) {
	f := newFixture(t)
	facs := f.seed(usd(1_000_000), usd(100_000))
	acct, a := f.fixedAccountOnBooks()
	f.mark("75")
	r := f.evaluate(acct)
	require.Equal(t, StatusMarginCall, r.Status)

	res, err := f.liquidate(acct, 1)
	require.NoError(t, err)
	require.Equal(t, DecisionSell, res.Decision)
	assert.Equal(t, CaseCompleted, res.Case.Status)
	assert.Equal(t, StatusNormal, res.AccountStatus)
	q := res.Step.Quantity
	assert.True(t, q.Cmp(money.QuantityFromInt64(2_298_000_000)) > 0 && q.Cmp(money.QuantityFromInt64(2_300_000_000)) < 0, "about 2.2987 SOL, got %s", q)
	requireQ(t, sol(20).Sub(q), f.customer(acct, ledger.CodeMarginCollateral, f.sol), "the rest of the position stays")
	// The step first accrued interest to the instant (capitalized), then the
	// proceeds net of the fee repaid the loan.
	interest := f.customer(acct, ledger.CodeInterestExpense, f.usd)
	assert.True(t, interest.IsPositive())
	requireQ(t, usd(2_000).Add(interest).Sub(res.Step.Repaid), f.customer(acct, ledger.CodeMarginLoan, f.usd), "proceeds net of the fee repaid the loan")
	requireQ(t, usd(100_000).Add(res.Step.Fee), f.balance(facs.Fund.Account(ledger.CodeInsuranceFundCash, f.usd)), "the fee reached the fund")

	after := f.evaluate(acct)
	assert.Equal(t, StatusNormal, after.Status)
	assert.True(t, after.Evaluation.Equity.Cmp(after.Evaluation.WarningThreshold) >= 0)

	// Replaying step 1 returns the recorded step and posts nothing.
	trades := f.countJournals(acct, ledger.KindLiquidation)
	replay, err := f.liquidate(acct, 1)
	require.NoError(t, err)
	assert.True(t, replay.Replayed)
	assert.Equal(t, res.Step.ID, replay.Step.ID)
	assert.Equal(t, trades, f.countJournals(acct, ledger.KindLiquidation))
	requirePartiesBalance(t, f, acct.String(), facs.Pool.ID.String(), facs.Fund.ID.String())
	_ = a
}

// TestIntegration_StalePriceLiquidationIsRefused: an eligible account whose
// mark has gone stale is not liquidated. The case waits in AWAITING_PRICE
// and nothing is posted; a fresh mark lets the next step run.
func TestIntegration_StalePriceLiquidationIsRefused(t *testing.T) {
	f := newFixture(t)
	f.seed(usd(1_000_000), usd(100_000))
	acct, a := f.fixedAccountOnBooks()
	f.mark("75")
	require.Equal(t, StatusMarginCall, f.evaluate(acct).Status)

	f.clk.Advance(2 * time.Minute) // the 75 mark is now stale
	res, err := f.liquidate(acct, 1)
	require.NoError(t, err)
	assert.Equal(t, DecisionAwaitPrice, res.Decision)
	assert.Equal(t, CaseAwaitingPrice, res.Case.Status)
	assert.Nil(t, res.Step)
	assert.Equal(t, StatusLiquidating, res.AccountStatus)
	assert.Zero(t, f.countJournals(acct, ledger.KindLiquidation), "no posting on a stale price")
	f.clk.Advance(time.Second)
	again, err := f.liquidate(acct, 0)
	require.NoError(t, err)
	assert.Equal(t, CaseAwaitingPrice, again.Case.Status, "still waiting")
	assert.Zero(t, f.countJournals(acct, ledger.KindMarginInterest), "a waiting case posts nothing at all, however often it is retried")
	var steps int
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT count(*) FROM margin_liquidation_steps WHERE case_id = $1`, res.Case.ID).Scan(&steps))
	assert.Zero(t, steps)

	f.mark("75")
	res, err = f.liquidate(acct, 1)
	require.NoError(t, err)
	assert.Equal(t, DecisionSell, res.Decision)
	require.NotNil(t, res.Step)
	assert.Equal(t, 1, f.countJournals(acct, ledger.KindLiquidation))
	_ = a
}

// TestIntegration_VenueUnavailableCaseWaits: with the venue down the case
// waits in AWAITING_VENUE; nothing is sold.
func TestIntegration_VenueUnavailableCaseWaits(t *testing.T) {
	f := newFixture(t)
	f.seed(usd(1_000_000), usd(100_000))
	acct, _ := f.fixedAccountOnBooks()
	f.mark("75")
	f.venue.down.Store(true)
	res, err := f.liquidate(acct, 0)
	require.NoError(t, err)
	assert.Equal(t, CaseAwaitingVenue, res.Case.Status)
	assert.Zero(t, f.countJournals(acct, ledger.KindLiquidation))
	f.venue.down.Store(false)
	res, err = f.liquidate(acct, 0)
	require.NoError(t, err)
	assert.Equal(t, CaseCompleted, res.Case.Status)
}

// TestIntegration_LiquidationQueueTakesTheMostUnderMarginedFirst: two
// accounts below maintenance; a queue run of one step serves the deeper
// shortfall first, the next run the other.
func TestIntegration_LiquidationQueueTakesTheMostUnderMarginedFirst(t *testing.T) {
	f := newFixture(t)
	f.seed(usd(1_000_000), usd(100_000))
	shallow, _ := f.fixedAccountOnBooks() // equity 125 at 75
	deep := f.account()                   // 100 less cash: equity 25 at 75
	f.fund(deep, f.usd, usd(900))
	f.fund(deep, f.sol, sol(20))
	f.open(deep, ModeCross)
	f.mark("100")
	f.post(deep, f.usd, usd(900))
	f.post(deep, f.sol, sol(20))
	_, err := f.borrow(deep, usd(1_900), "deep-"+uuid.NewString())
	require.NoError(t, err)
	_, err = f.borrow(deep, usd(100), "deep2-"+uuid.NewString())
	require.NoError(t, err, "equity 400 exactly covers initial margin 400")
	_, err = f.borrow(deep, money.QuantityFromInt64(1), "deep3-"+uuid.NewString())
	require.Error(t, err, "one more base unit would breach initial margin")

	f.mark("75")
	var marked []EvaluateResult
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var failures []error
		var err error
		marked, failures, err = f.svc.MarkToMarket(ctx, tx, 100)
		require.Empty(t, failures)
		return err
	}))
	require.Len(t, marked, 2, "mark-to-market evaluates every open account of the policy")
	for _, m := range marked {
		assert.Equal(t, StatusMarginCall, m.Status)
		require.NotNil(t, m.Call)
	}
	run := func() QueueResult {
		var q QueueResult
		require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			q, err = f.svc.ProcessLiquidationQueue(ctx, tx, 1)
			return err
		}))
		return q
	}
	first := run()
	require.Empty(t, first.Failures)
	require.Len(t, first.Results, 1)
	deepAcct, err := f.svc.Account(f.ctx, testDB, deep)
	require.NoError(t, err)
	assert.Equal(t, deepAcct.ID, first.Results[0].MarginAccountID, "the deeper shortfall goes first")
	second := run()
	require.Len(t, second.Results, 1)
	shallowAcct, err := f.svc.Account(f.ctx, testDB, shallow)
	require.NoError(t, err)
	assert.Equal(t, shallowAcct.ID, second.Results[0].MarginAccountID)
}

// TestIntegration_StressRunIsStoredWithTheExpectedNumbers: the 5/10/25/50%
// gaps of the fixed account, on real books, give the numbers the unit test
// derives by hand, and the stored run carries its canonical hash.
func TestIntegration_StressRunIsStoredWithTheExpectedNumbers(t *testing.T) {
	f := newFixture(t)
	f.seed(usd(1_000_000), usd(100_000))
	acct, a := f.fixedAccountOnBooks()
	var run StressRun
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		run, err = f.svc.RunStress(ctx, tx, acct)
		return err
	}))
	got := map[StressScenario]StressResult{}
	for _, r := range run.Report.Results {
		got[r.Scenario] = r
	}
	for sc, want := range map[StressScenario][3]string{
		StressGapDown5:  {"425", "190", "0"},
		StressGapDown10: {"350", "180", "0"},
		StressGapDown25: {"125", "150", "0"},
		StressGapDown50: {"-250", "100", "12.475"},
	} {
		requireQ(t, units(t, want[0]), got[sc].EquityAfter, string(sc)+" equity")
		requireQ(t, units(t, want[1]), got[sc].MaintenanceAfter, string(sc)+" maintenance")
		requireQ(t, units(t, want[2]), got[sc].Deficit, string(sc)+" deficit")
		requireQ(t, got[sc].Deficit, got[sc].InsuranceFundDraw, string(sc)+" fund draw")
	}
	assert.False(t, got[StressGapDown10].WouldLiquidate)
	assert.True(t, got[StressGapDown25].WouldLiquidate)
	assert.Equal(t, DecisionBankrupt, got[StressGapDown50].Outcome)

	var hash, version string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT report_hash, policy_version FROM margin_stress_runs WHERE id = $1`, run.ID).Scan(&hash, &version))
	assert.Equal(t, run.Report.Hash, hash)
	assert.Equal(t, f.cfg.Policy.Version, version)
	_, err := testDB.Exec(f.ctx, `UPDATE margin_stress_runs SET report_hash = 'x' WHERE id = $1`, run.ID)
	require.Error(t, err, "a stress run is history")
	_ = a
	_ = econ.SubsystemMargin
}
