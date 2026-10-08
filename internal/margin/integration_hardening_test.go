//go:build integration

package margin

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// --- F2: the wallet is swept before bankruptcy -----------------------------

// TestIntegration_BorrowedCashInTheWalletIsSweptBeforeBankruptcy (F2): the
// cash a customer borrowed into the wallet repays the loans before the account
// can be bankrupt. The sweep is a recorded liquidation step (SWEEP_WALLET)
// with its own MARGIN_REPAY journal and APPLIED effect, it takes only what no
// reservation holds, and with enough free cash the case simply completes.
func TestIntegration_BorrowedCashInTheWalletIsSweptBeforeBankruptcy(t *testing.T) {
	f := newFixture(t)
	f.seed(usd(1_000_000), usd(100_000))

	// The fixed account owes 2,000 and the 2,000 sits in its wallet.
	whole, _ := f.fixedAccountOnBooks()
	f.clk.Advance(time.Minute)
	f.mark("5")
	var last LiquidationResult
	var sweep *Step
	for i := 0; i < 6 && (last.Case == nil || last.Case.Status == CaseInProgress); i++ {
		r, err := f.liquidate(whole, 0)
		require.NoError(t, err)
		if r.Step != nil && r.Step.Action == ActionSweepWallet {
			sweep = r.Step
		}
		last = r
	}
	require.NotNil(t, sweep, "the free cash was swept")
	assert.Equal(t, CaseCompleted, last.Case.Status, "the borrowed cash repaid the loans: no bankruptcy")
	assert.Equal(t, StatusNormal, last.AccountStatus)
	requireQ(t, money.Quantity{}, f.customer(whole, ledger.CodeMarginLoan, f.usd), "nothing owed")
	requireQ(t, money.Quantity{}, f.customer(whole, ledger.CodeDeficit, f.usd), "no deficit")
	requireQ(t, usd(2_000).Sub(sweep.Quantity), f.customer(whole, ledger.CodeWallet, f.usd), "the sweep took only what was owed")
	require.NotNil(t, sweep.RepayJournalID)
	var kind, ref, refID string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT kind, reference_type, reference_id FROM journal_transactions WHERE id = $1`,
		*sweep.RepayJournalID).Scan(&kind, &ref, &refID))
	assert.Equal(t, string(ledger.KindMarginRepay), kind)
	assert.Equal(t, refLiquidationStep, ref)
	assert.Equal(t, sweep.ID.String(), refID)
	var status, effectKind string
	var reservation *string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT status, effect_kind, reservation_id::text FROM economic_effects WHERE journal_transaction_id = $1`,
		*sweep.RepayJournalID).Scan(&status, &effectKind, &reservation))
	assert.Equal(t, "APPLIED", status)
	assert.Equal(t, "LIQUIDATION_SWEEP", effectKind)
	require.NotNil(t, reservation, "the sweep went through a capital reservation")

	// The same account with 1,500 of its wallet reserved by something else:
	// the sweep takes the free 500 and the rest is a deficit.
	held, _ := f.fixedAccountOnBooks()
	var other capital.Reservation
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		other, err = f.deps.Capital.Reserve(ctx, tx, capital.ReserveRequest{AccountID: held.String(), AssetID: f.usd, Quantity: usd(1_500),
			ActorType: security.ActorUser, ActorID: "itest", IdempotencyKey: "other-" + uuid.NewString(), TTL: time.Hour,
			Reason: "an order in flight"})
		return err
	}))
	f.clk.Advance(time.Minute)
	f.mark("5")
	var steps []Step
	last = LiquidationResult{}
	for i := 0; i < 6 && (last.Case == nil || last.Case.Status == CaseInProgress); i++ {
		r, err := f.liquidate(held, 0)
		require.NoError(t, err)
		if r.Step != nil {
			steps = append(steps, *r.Step)
		}
		last = r
	}
	require.Len(t, steps, 3)
	assert.Equal(t, []StepAction{ActionSell, ActionApplyCollateral, ActionSweepWallet}, []StepAction{steps[0].Action, steps[1].Action, steps[2].Action})
	requireQ(t, usd(500), steps[2].Quantity, "only the unreserved cash is swept")
	assert.Equal(t, DecisionBankrupt, last.Decision)
	requireQ(t, usd(1_500), f.customer(held, ledger.CodeWallet, f.usd), "the reserved cash is untouched")
	r, err := f.deps.Capital.Get(f.ctx, testDB, other.ID)
	require.NoError(t, err)
	assert.Equal(t, capital.ReservationActive, r.Status)
	requireQ(t, f.customer(held, ledger.CodeMarginLoan, f.usd), *last.Case.DeficitUnits, "the deficit is what the sweep could not repay")
}

// TestIntegration_ACoveredDeficitRestrictsTheCustomerAccount (F2): once the
// insurance fund covers a deficit, the customer account that owes it is
// RESTRICTED through the accounts status machine (its transition row names
// the case), so buying power V2 shows nothing to trade or withdraw; a replay
// changes nothing, and an account already FROZEN is never loosened.
func TestIntegration_ACoveredDeficitRestrictsTheCustomerAccount(t *testing.T) {
	f := newFixture(t)
	f.seed(usd(1_000_000), usd(100_000))
	acct, _, c := f.bankruptAccount()
	cover, err := f.coverDeficit(c.ID)
	require.NoError(t, err)
	assert.True(t, cover.CustomerRestricted)

	repo := accounts.NewRepository()
	cust, err := repo.Get(f.ctx, testDB, acct)
	require.NoError(t, err)
	assert.Equal(t, accounts.StatusRestricted, cust.Status)
	assert.False(t, cust.Status.AllowsNewRisk(), "no new risk")
	assert.True(t, cust.Status.AllowsRiskReduction(), "the deficit can still be paid back")
	var actorType, reason string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT actor_type, reason FROM account_status_transitions
		WHERE account_id = $1 AND to_status = 'RESTRICTED'`, acct).Scan(&actorType, &reason))
	assert.Equal(t, "SERVICE", actorType)
	assert.Contains(t, reason, c.ID.String())

	loader, err := buyingpower.NewV2Loader(buyingpower.V2LoaderConfig{Clock: f.clk, Prices: f.prices, QuoteAssetID: f.usd,
		MaxPriceAge: time.Hour, Margin: f.svc.BuyingPowerReader()})
	require.NoError(t, err)
	f.fund(acct, f.usd, usd(100))
	bp, err := loader.Compute(f.ctx, testDB, "CUSTOMER", acct.String())
	require.NoError(t, err)
	assert.True(t, bp.Withdrawable.IsZero(), "nothing is withdrawable")
	assert.True(t, bp.TradeBuyingPower.IsZero(), "nothing is tradable")
	blocked := false
	for _, r := range bp.Reasons {
		if r.Code == buyingpower.V2ReasonBlocked && r.Detail == "ACCOUNT_RESTRICTED" {
			blocked = true
		}
	}
	assert.True(t, blocked, "buying power names the restriction: %+v", bp.Reasons)

	again, err := f.coverDeficit(c.ID)
	require.NoError(t, err)
	assert.True(t, again.Replayed)
	assert.False(t, again.CustomerRestricted)

	// A frozen customer stays frozen: margin only tightens a status.
	frozen, _, fc := f.bankruptAccount()
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := repo.Transition(ctx, tx, frozen, accounts.StatusChange{To: accounts.StatusFrozen, ActorType: "OPERATOR",
			ActorID: "itest-compliance", Reason: "under review"}, f.clk.Now())
		return err
	}))
	fcover, err := f.coverDeficit(fc.ID)
	require.NoError(t, err)
	assert.False(t, fcover.CustomerRestricted)
	still, err := repo.Get(f.ctx, testDB, frozen)
	require.NoError(t, err)
	assert.Equal(t, accounts.StatusFrozen, still.Status)
}

// TestIntegration_ADeficitIsRepaidToTheFundExactlyOnce (F2): a covered deficit
// is paid back to the insurance fund through a REPAY intent, in parts if the
// customer likes, never twice: a replay posts nothing, concurrent repayments
// of the whole remainder settle exactly once, nothing beyond the deficit is
// taken, and the database refuses a repayment row beyond the deficit
// (MG011). An uncovered deficit is not repaid.
func TestIntegration_ADeficitIsRepaidToTheFundExactlyOnce(t *testing.T) {
	f := newFixture(t)
	facs := f.seed(usd(1_000_000), usd(100_000))
	acct, a, c := f.bankruptAccount()
	deficit := *c.DeficitUnits
	_, err := f.repayDeficit(acct, usd(1), "too-soon-"+uuid.NewString())
	require.Error(t, err, "an uncovered deficit is the pool's loan, not the fund's claim")
	_, err = f.coverDeficit(c.ID)
	require.NoError(t, err)
	f.fund(acct, f.usd, deficit.Add(usd(10)))

	half, err := deficit.Div(money.QuantityFromInt64(2), money.RoundFloor)
	require.NoError(t, err)
	key := "repay-deficit-" + uuid.NewString()
	first, err := f.repayDeficit(acct, half, key)
	require.NoError(t, err)
	requireQ(t, deficit.Sub(half), first.Outstanding, "outstanding")
	requireQ(t, deficit.Sub(half), f.customer(acct, ledger.CodeDeficit, f.usd), "the customer owes the rest")
	requireQ(t, deficit.Sub(half), f.balance(facs.Fund.Account(ledger.CodeDeficitReceivable, f.usd)), "the fund's claim shrinks")
	var intentKind, intentStatus string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT kind, status FROM economic_intents WHERE id = $1`, first.IntentID).Scan(&intentKind, &intentStatus))
	assert.Equal(t, "REPAY", intentKind)
	assert.Equal(t, "COMPLETED", intentStatus)

	journals := f.countJournals(acct, ledger.KindMarginRepay)
	replay, err := f.repayDeficit(acct, half, key)
	require.NoError(t, err)
	assert.True(t, replay.Replayed)
	assert.Equal(t, first.JournalTransactionID, replay.JournalTransactionID)
	assert.Equal(t, journals, f.countJournals(acct, ledger.KindMarginRepay), "a replay posts nothing")

	_, err = f.repayDeficit(acct, deficit, "too-much-"+uuid.NewString())
	require.Error(t, err, "never more than is owed")

	rest := deficit.Sub(half)
	const n = 6
	var wg sync.WaitGroup
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.repayDeficit(acct, rest, "race-"+uuid.NewString())
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		if err == nil {
			ok++
		}
	}
	assert.Equal(t, 1, ok, "the remainder is repaid exactly once")
	requireQ(t, money.Quantity{}, f.customer(acct, ledger.CodeDeficit, f.usd), "nothing owed")
	requireQ(t, money.Quantity{}, f.balance(facs.Fund.Account(ledger.CodeDeficitReceivable, f.usd)), "the fund's claim is settled")
	requireQ(t, usd(10), f.customer(acct, ledger.CodeWallet, f.usd), "exactly the deficit left the wallet")
	var repaid string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT sum(amount)::text FROM margin_deficit_repayments WHERE case_id = $1`, c.ID).Scan(&repaid))
	assert.Equal(t, deficit.String(), repaid)
	requirePartiesBalance(t, f, acct.String(), facs.Fund.ID.String())

	// Around Go: one more repayment row is refused by the database.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO margin_deficit_repayments (id, case_id, margin_account_id, amount, journal_transaction_id,
			effect_id, intent_id, repaid_at)
			SELECT $1, case_id, margin_account_id, 1, journal_transaction_id, effect_id, intent_id, now()
			  FROM margin_deficit_repayments WHERE case_id = $2 LIMIT 1`, uuid.New(), c.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "MG011", db.SQLState(err), "%v", err)
	_ = a
}

// --- F4: a route that needs a person is not executed -----------------------

// TestIntegration_ARouteThatNeedsConfirmationIsNeverExecuted (F4): when the
// legal router permits margin's intents only after a person confirms them,
// the settlement compiler's route is Permitted but not MayExecuteNow, and
// margin executes nothing: the borrow and the collateral post are refused and
// nothing moves.
func TestIntegration_ARouteThatNeedsConfirmationIsNeverExecuted(t *testing.T) {
	f := newFixture(t)
	f.seed(usd(1_000_000), usd(100_000))
	acct := f.cashAccount(usd(1_000))
	f.fund(acct, f.usd, usd(100))
	f.mark("100")
	router, err := legalrouter.New(legalrouter.Policy{Version: "itest-confirm-" + uuid.NewString(), Rules: []legalrouter.Rule{
		{Match: legalrouter.Key{Product: legalrouter.ProductSimulation}, Outcome: legalrouter.RequiresUserConfirmation,
			ReasonCode: "ITEST_SIM_NEEDS_CONFIRMATION", Detail: "a person must confirm", ApprovalReference: "ITEST-1"},
		{Match: legalrouter.Key{}, Outcome: legalrouter.Deny, ReasonCode: "ITEST_DENY", Detail: "catch-all"},
	}})
	require.NoError(t, err)
	svc, err := NewService(f.cfg, func() Dependencies { d := f.deps; d.Router = router; return d }())
	require.NoError(t, err)

	key := "confirm-" + uuid.NewString()
	var refusal error
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, refusal = svc.Borrow(ctx, tx, LoanRequest{AccountID: acct, Amount: usd(100), IdempotencyKey: key, Requester: userRequester})
		return nil // keep the refusal on the record
	}))
	require.Error(t, refusal)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(refusal))
	ee, ok := errs.As(refusal)
	require.True(t, ok)
	assert.Equal(t, "USER_CONFIRMATION", ee.Fields["required_confirmation"])
	requireQ(t, money.Quantity{}, f.customer(acct, ledger.CodeMarginLoan, f.usd), "nothing was lent")
	var status string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT status FROM economic_intents WHERE account_id = $1 AND idempotency_key = $2`,
		acct, key).Scan(&status))
	assert.Equal(t, "REJECTED", status, "the intent never reached ROUTED")

	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := svc.PostCollateral(ctx, tx, CollateralRequest{AccountID: acct, AssetID: f.usd, Quantity: usd(100),
			IdempotencyKey: "confirm-post-" + uuid.NewString(), Requester: userRequester})
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	requireQ(t, usd(1_000), f.customer(acct, ledger.CodeMarginCollateral, f.usd), "nothing moved")
}

// --- F5: collateral posting is an intent, never an agent's ------------------

// TestIntegration_AnAgentCannotPostCollateral (F5): an agent requester is
// refused outright: no intent, no reservation, no move.
func TestIntegration_AnAgentCannotPostCollateral(t *testing.T) {
	f := newFixture(t)
	f.seed(usd(1_000_000), usd(100_000))
	acct := f.account()
	f.fund(acct, f.usd, usd(500))
	f.open(acct, ModeCross)
	agent := Requester{Type: security.ActorAgent, ID: uuid.NewString(), AgentID: uuid.NewString()}
	key := "agent-post-" + uuid.NewString()
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.PostCollateral(ctx, tx, CollateralRequest{AccountID: acct, AssetID: f.usd, Quantity: usd(500),
			IdempotencyKey: key, Requester: agent})
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	requireQ(t, money.Quantity{}, f.customer(acct, ledger.CodeMarginCollateral, f.usd), "nothing moved")
	requireQ(t, usd(500), f.customer(acct, ledger.CodeWallet, f.usd), "the wallet is whole")
	assert.Zero(t, countRows(t, `SELECT count(*) FROM asset_reservations WHERE account_id = $1`, acct), "no reservation")
	assert.Zero(t, countRows(t, `SELECT count(*) FROM economic_intents WHERE account_id = $1`, acct), "no intent")
}

// TestIntegration_PostingCollateralIsARoutedRiskCheckedIntent (F5): a person's
// post is a TRANSFER_VALUE intent that is routed, decided by Risk Kernel V2
// and completed; its reservation and its effect name the intent; a replay of
// the key returns the first post and moves nothing; the key with other terms
// is refused.
func TestIntegration_PostingCollateralIsARoutedRiskCheckedIntent(t *testing.T) {
	f := newFixture(t)
	f.seed(usd(1_000_000), usd(100_000))
	acct := f.account()
	f.fund(acct, f.usd, usd(500))
	f.open(acct, ModeCross)
	key := "post-" + uuid.NewString()
	post := func(qty money.Quantity) (CollateralResult, error) {
		var r CollateralResult
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			r, err = f.svc.PostCollateral(ctx, tx, CollateralRequest{AccountID: acct, AssetID: f.usd, Quantity: qty,
				IdempotencyKey: key, Requester: userRequester})
			return err
		})
		return r, err
	}
	r, err := post(usd(300))
	require.NoError(t, err)
	require.NotNil(t, r.IntentID, "the post is an economic intent")
	var kind, status string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT kind, status FROM economic_intents WHERE id = $1`, *r.IntentID).Scan(&kind, &status))
	assert.Equal(t, "TRANSFER_VALUE", kind)
	assert.Equal(t, "COMPLETED", status)
	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM economic_intent_transitions WHERE intent_id = $1 AND to_status = 'ROUTED'`, *r.IntentID),
		"routed through the settlement compiler")
	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM advanced_risk_decisions WHERE intent_id = $1 AND verdict = 'ALLOW'`, *r.IntentID),
		"decided by Risk Kernel V2")
	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM asset_reservations WHERE id = $1 AND economic_intent_id = $2`, r.ReservationID, *r.IntentID))
	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM economic_effects WHERE id = $1 AND intent_id = $2`, r.EffectID, *r.IntentID))
	requireQ(t, usd(300), f.customer(acct, ledger.CodeMarginCollateral, f.usd), "posted")

	replay, err := post(usd(300))
	require.NoError(t, err)
	assert.True(t, replay.Replayed)
	assert.Equal(t, r.MoveID, replay.MoveID)
	assert.Equal(t, r.JournalTransactionID, replay.JournalTransactionID)
	requireQ(t, usd(300), f.customer(acct, ledger.CodeMarginCollateral, f.usd), "a replay moves nothing")
	_, err = post(usd(301))
	require.Error(t, err)
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, errs.CodeOf(err))
}

// --- F6: a step number belongs to the open case ----------------------------

// TestIntegration_AStepNumberReplaysOnlyWithinTheOpenCase (F6): inside an open
// case, asking again for a step it already took returns that step and posts
// nothing. Once a case has completed, its step numbers are history: a new
// shortfall liquidated with ExpectedStep 1 opens a new case and sells.
func TestIntegration_AStepNumberReplaysOnlyWithinTheOpenCase(t *testing.T) {
	f := newFixture(t)
	f.seed(usd(1_000_000), usd(100_000))
	open, _ := f.fixedAccountOnBooks()
	f.withdraw(open, f.usd, usd(2_000))
	f.clk.Advance(time.Minute)
	f.mark("40")
	s1, err := f.liquidate(open, 1)
	require.NoError(t, err)
	require.Equal(t, CaseInProgress, s1.Case.Status)
	trades := f.countJournals(open, ledger.KindLiquidation)
	replay, err := f.liquidate(open, 1)
	require.NoError(t, err)
	assert.True(t, replay.Replayed, "inside the open case a step is exactly once")
	assert.Equal(t, s1.Step.ID, replay.Step.ID)
	assert.Equal(t, trades, f.countJournals(open, ledger.KindLiquidation))
	s2, err := f.liquidate(open, 2)
	require.NoError(t, err)
	assert.False(t, s2.Replayed)
	assert.Equal(t, 2, s2.Step.StepNo)

	acct, a := f.fixedAccountOnBooks()
	f.mark("75")
	first, err := f.liquidate(acct, 1)
	require.NoError(t, err)
	require.Equal(t, CaseCompleted, first.Case.Status)
	f.mark("65")
	require.Equal(t, StatusMarginCall, f.evaluate(acct).Status, "a new shortfall")
	trades = f.countJournals(acct, ledger.KindLiquidation)
	second, err := f.liquidate(acct, 1)
	require.NoError(t, err)
	assert.False(t, second.Replayed, "a finished case's step is not this case's step")
	assert.NotEqual(t, first.Case.ID, second.Case.ID, "a new case")
	require.NotNil(t, second.Step)
	assert.Equal(t, 1, second.Step.StepNo)
	assert.Equal(t, trades+1, f.countJournals(acct, ledger.KindLiquidation), "something was sold")
	assert.Equal(t, 2, countRows(t, `SELECT count(*) FROM margin_liquidation_cases WHERE margin_account_id = $1`, a.ID))
}

// --- F11: one lock order, one account per transaction ----------------------

// TestIntegration_FacilityRowsAreLockedPoolFirstEverywhere (F11): with the
// insurance fund's row held elsewhere, a deficit coverage blocks holding the
// lending pool's row (it locks the pool first), and a liquidation step then
// blocks on the pool behind it; released, both finish. Had either taken the
// fund before the pool, the step would not have waited.
func TestIntegration_FacilityRowsAreLockedPoolFirstEverywhere(t *testing.T) {
	f := newFixture(t)
	facs := f.seed(usd(1_000_000), usd(100_000))
	broke, _, c := f.bankruptAccount()
	called, _ := f.fixedAccountOnBooks()
	f.mark("75")

	holder, err := testDB.Pool().Begin(f.ctx)
	require.NoError(t, err)
	defer func() { _ = holder.Rollback(f.ctx) }()
	_, err = holder.Exec(f.ctx, `SELECT id FROM facilities WHERE id = $1 FOR NO KEY UPDATE`, facs.Fund.ID)
	require.NoError(t, err)

	type outcome struct {
		what string
		err  error
	}
	done := make(chan outcome, 2)
	go func() {
		_, err := f.coverDeficit(c.ID)
		done <- outcome{"cover", err}
	}()
	waitForLockWaiters(t, 1)
	go func() {
		_, err := f.liquidate(called, 0)
		done <- outcome{"step", err}
	}()
	waitForLockWaiters(t, 2)
	select {
	case o := <-done:
		t.Fatalf("%s finished while the fund was held: the pool was not locked first", o.what)
	case <-time.After(200 * time.Millisecond):
	}
	require.NoError(t, holder.Rollback(f.ctx))
	for i := 0; i < 2; i++ {
		select {
		case o := <-done:
			require.NoError(t, o.err, o.what)
		case <-time.After(30 * time.Second):
			t.Fatal("deadlocked")
		}
	}
	_ = broke
}

// waitForLockWaiters waits until n of the test's connections wait on a lock.
func waitForLockWaiters(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		require.NoError(t, testDB.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND application_name = 'margin-itest' AND wait_event_type = 'Lock'`).Scan(&waiting))
		if waiting >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fewer than %d transactions ever waited on a lock", n)
}

// TestIntegration_ConcurrentLiquidationsAndCoveragesNeverDeadlock (F11):
// coverages, liquidation steps, borrows and repayments on many accounts, all
// sharing one lending pool and one insurance fund, race each in its own
// transaction and every one of them succeeds.
func TestIntegration_ConcurrentLiquidationsAndCoveragesNeverDeadlock(t *testing.T) {
	f := newFixture(t)
	f.seed(usd(1_000_000), usd(100_000))
	var cases []CaseID
	for i := 0; i < 3; i++ {
		_, _, c := f.bankruptAccount()
		cases = append(cases, c.ID)
	}
	var called []accounts.AccountID
	for i := 0; i < 3; i++ {
		acct, _ := f.fixedAccountOnBooks()
		called = append(called, acct)
	}
	healthy := f.cashAccount(usd(5_000))
	f.mark("75")
	for _, acct := range called {
		require.Equal(t, StatusMarginCall, f.evaluate(acct).Status)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	errsCh := make(chan error, 16)
	run := func(fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errsCh <- fn()
		}()
	}
	for _, cid := range cases {
		run(func() error { _, err := f.coverDeficit(cid); return err })
	}
	for _, acct := range called {
		run(func() error { _, err := f.liquidate(acct, 0); return err })
	}
	for i := 0; i < 3; i++ {
		run(func() error { _, err := f.borrow(healthy, usd(10), "race-borrow-"+uuid.NewString()); return err })
	}
	close(start)
	wg.Wait()
	close(errsCh)
	for err := range errsCh {
		require.NoError(t, err)
	}
}

// TestIntegration_TheQueueCommitsEachAccountInItsOwnTransaction (F11): a
// refusal the database makes only at COMMIT (here a test trap on one
// account's steps) fails that account's step and nothing else: the queue's
// other account is liquidated and committed.
func TestIntegration_TheQueueCommitsEachAccountInItsOwnTransaction(t *testing.T) {
	f := newFixture(t)
	f.seed(usd(1_000_000), usd(100_000))
	trapped, ta := f.fixedAccountOnBooks()
	spared, _ := f.fixedAccountOnBooks()
	f.mark("75")
	require.Equal(t, StatusMarginCall, f.evaluate(trapped).Status)
	require.Equal(t, StatusMarginCall, f.evaluate(spared).Status)

	owner, err := pgx.Connect(f.ctx, testMigrateURL)
	require.NoError(t, err)
	defer owner.Close(f.ctx)
	fn := "margin_itest_trap_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = owner.Exec(f.ctx, `CREATE FUNCTION `+fn+`() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF EXISTS (SELECT 1 FROM margin_liquidation_cases WHERE id = NEW.case_id AND margin_account_id = '`+ta.ID.String()+`') THEN
				RAISE EXCEPTION 'itest trap: refused at commit' USING ERRCODE = 'P0001';
			END IF;
			RETURN NULL;
		END $$`)
	require.NoError(t, err)
	_, err = owner.Exec(f.ctx, `CREATE CONSTRAINT TRIGGER `+fn+` AFTER INSERT ON margin_liquidation_steps
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION `+fn+`()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = owner.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+fn+` ON margin_liquidation_steps`)
		_, _ = owner.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+fn+`()`)
	})

	q, err := f.svc.ProcessLiquidationQueue(f.ctx, testDB, 10)
	require.NoError(t, err)
	require.Len(t, q.Failures, 1)
	assert.Equal(t, trapped, q.Failures[0].AccountID)
	assert.Contains(t, q.Failures[0].Err.Error(), "itest trap")
	require.Len(t, q.Results, 1)
	sparedAcct, err := f.svc.Account(f.ctx, testDB, spared)
	require.NoError(t, err)
	assert.Equal(t, sparedAcct.ID, q.Results[0].MarginAccountID)
	assert.Equal(t, 1, f.countJournals(spared, ledger.KindLiquidation), "the spared account's step committed")
	assert.Zero(t, f.countJournals(trapped, ledger.KindLiquidation), "the trapped account's step rolled back alone")
}

// --- F12: an effect applies the journal its key posted ----------------------

// TestIntegration_AMarginEffectAppliesOnlyTheJournalItsKeyPosted (F12): a
// MARGIN effect applied with any journal but the one posted under its own
// key, of its kind and reference type, on its own customer's books, is
// refused at COMMIT (MG008) -- through econ's own Apply and around it.
func TestIntegration_AMarginEffectAppliesOnlyTheJournalItsKeyPosted(t *testing.T) {
	f := newFixture(t)
	acct, other := f.account(), f.account()
	actor := econ.Actor{Type: security.ActorSystem, ID: "margin-itest"}
	begin := func(ctx context.Context, tx pgx.Tx, key, kind string, account accounts.AccountID) (econ.Effect, error) {
		e, _, err := f.deps.Effects.Begin(ctx, tx, econ.BeginRequest{Key: key, Subsystem: econ.SubsystemMargin, Kind: kind,
			AccountID: account.String(), Request: map[string]string{"k": key}, PostsLedger: true, Actor: actor})
		return e, err
	}
	post := func(ctx context.Context, tx pgx.Tx, key string, kind ledger.Kind, ref string, account accounts.AccountID) (ledger.TransactionID, error) {
		q := money.QuantityFromInt64(5)
		entries := []ledger.Entry{
			{Account: ledger.CustomerAccount(account, ledger.CodeWallet, f.usd), Side: ledger.Debit, Quantity: q},
			{Account: ledger.CustomerAccount(account, ledger.CodeCapital, f.usd), Side: ledger.Credit, Quantity: q},
		}
		if kind == ledger.KindMarginCollateralMove {
			entries[0].Account = ledger.CustomerAccount(account, ledger.CodeMarginCollateral, f.usd)
			entries[1].Account = ledger.CustomerAccount(account, ledger.CodeWallet, f.usd)
		}
		res, err := f.ledger.Post(ctx, tx, ledger.Posting{Kind: kind, IdempotencyKey: key,
			Reference: ledger.FinancialEventReference{Type: ref, ID: uuid.NewString()}, EffectiveAt: f.clk.Now(), Entries: entries})
		return res.TransactionID, err
	}
	f.fund(acct, f.usd, usd(10))
	f.fund(other, f.usd, usd(10))

	// Any unused journal: a sandbox funding of the customer.
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		key := "margin:itest:" + uuid.NewString()
		e, err := begin(ctx, tx, key, "COLLATERAL_POST", acct)
		if err != nil {
			return err
		}
		j, err := post(ctx, tx, "sbx-fund:"+uuid.NewString(), ledger.KindSandboxFunding, "sandbox_funding", acct)
		if err != nil {
			return err
		}
		_, err = f.deps.Effects.Apply(ctx, tx, e.ID, econ.ApplyRequest{JournalTransactionID: &j, Actor: actor})
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "MG008", db.SQLState(err), "%v", err)

	// The right kind and reference, but posted under another key.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		e, err := begin(ctx, tx, "margin:itest:"+uuid.NewString(), "COLLATERAL_POST", acct)
		if err != nil {
			return err
		}
		j, err := post(ctx, tx, "margin:itest:"+uuid.NewString(), ledger.KindMarginCollateralMove, refCollateralMove, acct)
		if err != nil {
			return err
		}
		_, err = f.deps.Effects.Apply(ctx, tx, e.ID, econ.ApplyRequest{JournalTransactionID: &j, Actor: actor})
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "MG008", db.SQLState(err), "%v", err)

	// Under its own key and the right shape, but another customer's books.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		key := "margin:itest:" + uuid.NewString()
		e, err := begin(ctx, tx, key, "COLLATERAL_POST", acct)
		if err != nil {
			return err
		}
		j, err := post(ctx, tx, key, ledger.KindMarginCollateralMove, refCollateralMove, other)
		if err != nil {
			return err
		}
		_, err = f.deps.Effects.Apply(ctx, tx, e.ID, econ.ApplyRequest{JournalTransactionID: &j, Actor: actor})
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "MG008", db.SQLState(err), "%v", err)

	// Around econ: a bare transition row and UPDATE naming a foreign journal.
	key := "margin:itest:" + uuid.NewString()
	var e econ.Effect
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		e, err = begin(ctx, tx, key, "COLLATERAL_POST", acct)
		return err
	}))
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		j, err := post(ctx, tx, "sbx-fund:"+uuid.NewString(), ledger.KindSandboxFunding, "sandbox_funding", acct)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO economic_effect_transitions (id, effect_id, from_status, to_status, actor_type, actor_id, reason)
			VALUES ($1,$2,'PENDING','APPLIED','SYSTEM','x','bypass')`, uuid.New(), e.ID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE economic_effects SET status = 'APPLIED', journal_transaction_id = $2 WHERE id = $1`, e.ID, j)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "MG008", db.SQLState(err), "%v", err)

	// The journal its own key posted, of its shape, on its own books: applied.
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		j, err := post(ctx, tx, key, ledger.KindMarginCollateralMove, refCollateralMove, acct)
		if err != nil {
			return err
		}
		_, err = f.deps.Effects.Apply(ctx, tx, e.ID, econ.ApplyRequest{JournalTransactionID: &j, Actor: actor})
		return err
	}))
}

// TestIntegration_SQLAndGoAgreeOnEffectJournalShapes (F12) compares
// cp_margin_effect_journal_shape with EffectJournalShapes, and an effect kind
// margin does not post has no shape at all.
func TestIntegration_SQLAndGoAgreeOnEffectJournalShapes(t *testing.T) {
	requireEnv(t)
	for _, s := range EffectJournalShapes() {
		var kind, ref *string
		require.NoError(t, testDB.QueryRow(context.Background(), `SELECT journal_kind, reference_type FROM cp_margin_effect_journal_shape($1)`,
			s.EffectKind).Scan(&kind, &ref))
		require.NotNil(t, kind, s.EffectKind)
		assert.Equal(t, string(s.JournalKind), *kind, s.EffectKind)
		assert.Equal(t, s.ReferenceType, *ref, s.EffectKind)
	}
	var unknown *string
	require.NoError(t, testDB.QueryRow(context.Background(), `SELECT journal_kind FROM cp_margin_effect_journal_shape('NOT_A_MARGIN_EFFECT')`).Scan(&unknown))
	assert.Nil(t, unknown, "an effect kind margin does not post has no journal shape, so it can never be APPLIED")
}

// --- F13: the liquidation fee is platform fee revenue ----------------------

// TestIntegration_TheLiquidationFeeIsBookedAsPlatformFeeRevenue (F13): on real
// books, a liquidation's fee leaves the customer as FEES_PLATFORM and arrives
// as the platform's PLATFORM_FEE_RECEIVABLE against PLATFORM_FEE_REVENUE; the
// insurance fund's books do not move, and every party balances.
func TestIntegration_TheLiquidationFeeIsBookedAsPlatformFeeRevenue(t *testing.T) {
	f := newFixture(t)
	facs := f.seed(usd(1_000_000), usd(100_000))
	acct, _ := f.fixedAccountOnBooks()
	f.mark("75")
	res, err := f.liquidate(acct, 1)
	require.NoError(t, err)
	require.NotNil(t, res.Step)
	fee := res.Step.Fee
	require.True(t, fee.IsPositive())
	requireQ(t, fee, f.customer(acct, ledger.CodeFeesPlatform, f.usd), "the customer paid it")
	requireQ(t, fee, f.balance(ledger.PlatformAccount(ledger.CodePlatformFeeReceivable, f.usd)), "the platform is owed it")
	requireQ(t, fee, f.balance(ledger.PlatformAccount(ledger.CodePlatformFeeRevenue, f.usd)), "the platform earned it")
	requireQ(t, usd(100_000), f.balance(facs.Fund.Account(ledger.CodeInsuranceFundCash, f.usd)), "the fund's cash did not move")
	requireQ(t, usd(100_000), f.balance(facs.Fund.Account(ledger.CodeInsuranceFundCapital, f.usd)), "the fund's capital did not move")
	requirePartiesBalance(t, f, acct.String(), ledger.PlatformOwnerID, facs.Fund.ID.String())
}
