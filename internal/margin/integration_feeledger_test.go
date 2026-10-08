//go:build integration

package margin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// feeBookings returns the fee bookings of an account.
func (f *fixture) feeBookings(acct accounts.AccountID) []feeledger.Booking {
	f.t.Helper()
	rows, err := testDB.Query(f.ctx, `SELECT id FROM fee_bookings WHERE account_id = $1 ORDER BY booked_at, id`, acct)
	require.NoError(f.t, err)
	var ids []feeledger.BookingID
	for rows.Next() {
		var bid feeledger.BookingID
		require.NoError(f.t, rows.Scan(&bid))
		ids = append(ids, bid)
	}
	rows.Close()
	require.NoError(f.t, rows.Err())
	out := make([]feeledger.Booking, 0, len(ids))
	for _, bid := range ids {
		b, err := f.deps.FeeLedger.Booking(f.ctx, testDB, bid)
		require.NoError(f.t, err)
		out = append(out, b)
	}
	return out
}

func (f *fixture) countRows(sql string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, testDB.QueryRow(f.ctx, sql, args...).Scan(&n))
	return n
}

// liquidatedAccount is the fixed account liquidated one step at a mark: at
// 75 a sale that restores it, at 40 a sale of everything that leaves its
// case in progress. Either charges the policy's fee.
func (f *fixture) liquidatedAccount(mark string) (accounts.AccountID, LiquidationResult) {
	f.t.Helper()
	f.seed(usd(1_000_000), usd(100_000))
	acct, _ := f.fixedAccountOnBooks()
	f.mark(mark)
	res, err := f.liquidate(acct, 1)
	require.NoError(f.t, err)
	require.NotNil(f.t, res.Step)
	require.Equal(f.t, ActionSell, res.Step.Action)
	require.True(f.t, res.Step.Fee.IsPositive())
	return acct, res
}

// TestIntegration_TheLiquidationFeeIsBookedThroughTheFeeLedger: a
// liquidation's fee is found through the fee ledger -- an EMBEDDED booking of
// kind LIQUIDATION against the step's own journal and the effect that applied
// it, charging exactly the step's fee as a platform fee; a quote disclosure
// to the customer that verifies against the policy's published schedule and
// prices the fee as a share of exactly the step's proceeds -- and the spread
// audit, rightly, finds no spread.
func TestIntegration_TheLiquidationFeeIsBookedThroughTheFeeLedger(t *testing.T) {
	f := newFixture(t)
	acct, res := f.liquidatedAccount("75")
	step := res.Step
	fl := f.deps.FeeLedger

	key := LiquidationFeeScheduleKey(f.usd, f.cfg.Policy.LiquidationFeeBPS)
	sched, err := fl.EffectiveSchedule(f.ctx, testDB, key, f.clk.Now())
	require.NoError(t, err, "the policy's fee is a published schedule in force")
	wantHash, err := LiquidationFeeSchedule(f.cfg.Policy, f.usd, 6).Hash()
	require.NoError(t, err)
	assert.Equal(t, wantHash, sched.Hash)

	booked := f.feeBookings(acct)
	require.Len(t, booked, 1)
	b := booked[0]
	assert.Equal(t, feeledger.BookingEmbedded, b.BookingMode)
	assert.Equal(t, ledger.KindLiquidation, b.Kind)
	assert.Equal(t, gates.ModePaper, b.Mode)
	require.NotNil(t, step.TradeJournalID)
	assert.Equal(t, *step.TradeJournalID, b.JournalTransactionID, "against the step's LIQUIDATION journal")
	var effect econ.EffectID
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT id FROM economic_effects WHERE effect_key = $1`,
		"margin:liq:"+res.Case.ID.String()+":1:trade").Scan(&effect))
	assert.Equal(t, effect, b.EffectID, "and the effect that applied it")
	requireQ(t, step.Fee, b.Charges.Platform, "the step's fee, as a platform fee")
	assert.True(t, b.Charges.Strategy.IsZero() && b.Charges.Spread.IsZero() && b.Charges.Venue.IsZero() && b.Charges.Network.IsZero())
	require.NoError(t, fl.VerifyBooking(f.ctx, testDB, b.ID))

	d, err := fl.VerifyQuoteRecord(f.ctx, testDB, b.DisclosureID)
	require.NoError(t, err, "the disclosure recomputes from its schedule")
	q, err := fl.Quote(f.ctx, testDB, b.DisclosureID)
	require.NoError(t, err)
	assert.Equal(t, key, q.ScheduleKey)
	assert.Equal(t, sched.ID, q.ScheduleID)
	assert.Equal(t, econ.SubsystemMargin, q.Subsystem)
	require.NotNil(t, q.AccountID)
	assert.Equal(t, acct, *q.AccountID)
	assert.Equal(t, fees.SideSell, d.Input.Side)
	requireQ(t, step.Proceeds, d.Input.BaseQty, "a share of exactly the proceeds")
	requireQ(t, step.Proceeds, d.NotionalAtReference, "valued at par")
	requireQ(t, step.Fee, d.Fees.Total, "the disclosed fee is the step's")
	assert.True(t, d.Rounding.AnyFavoursPlatform, "the disclosure says the fee rounds up")

	// The lines: the fee leaves the customer's wallet for FEES_PLATFORM and
	// arrives as the platform's revenue, inside the step's journal.
	jt, err := f.ledger.Transaction(f.ctx, testDB, *step.TradeJournalID)
	require.NoError(t, err)
	var expense, revenue money.Quantity
	for _, e := range jt.Entries {
		a := e.Account
		switch {
		case a.OwnerType == ledger.OwnerCustomer && a.Code == ledger.CodeFeesPlatform && e.Side == ledger.Debit:
			expense = expense.Add(e.Quantity)
		case a.OwnerType == ledger.OwnerPlatform && a.Code == ledger.CodePlatformFeeRevenue && e.Side == ledger.Credit:
			revenue = revenue.Add(e.Quantity)
		}
	}
	requireQ(t, step.Fee, expense, "FEES_PLATFORM")
	requireQ(t, step.Fee, revenue, "PLATFORM_FEE_REVENUE")
	requireQ(t, step.Fee, f.balance(ledger.PlatformAccount(ledger.CodePlatformFeeRevenue, f.usd)), "all of this numeraire's fee revenue is the booked fee")

	// A liquidation fee is no spread.
	usdID := f.usd
	spreads, err := fl.SpreadAudit(f.ctx, testDB, feeledger.SpreadFilter{QuoteAsset: &usdID, IncludeReversed: true})
	require.NoError(t, err)
	assert.Empty(t, spreads)
	total, reversed, err := fl.SpreadTotals(f.ctx, testDB, f.usd)
	require.NoError(t, err)
	assert.True(t, total.IsZero() && reversed.IsZero())
}

// TestIntegration_AReplayedLiquidationStepBooksItsFeeOnce: replaying the
// executed step, and replaying the fee ledger's own disclosure and
// registration for it, return what was recorded and book nothing twice.
func TestIntegration_AReplayedLiquidationStepBooksItsFeeOnce(t *testing.T) {
	f := newFixture(t)
	// At 40 the first step sells everything and the case stays in progress,
	// so step 1 asked for again is the recorded step, executed once.
	acct, res := f.liquidatedAccount("40")
	require.Equal(t, CaseInProgress, res.Case.Status)
	first := f.feeBookings(acct)
	require.Len(t, first, 1)

	replay, err := f.liquidate(acct, 1)
	require.NoError(t, err)
	assert.True(t, replay.Replayed, "the recorded step is returned")
	require.NotNil(t, replay.Step)
	assert.Equal(t, res.Step.ID, replay.Step.ID)
	a, err := f.svc.Account(f.ctx, testDB, acct)
	require.NoError(t, err)
	var q feeledger.StoredQuote
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		prefix := "margin:liq:" + res.Case.ID.String() + ":1"
		stored, err := f.deps.FeeLedger.Quote(ctx, tx, first[0].DisclosureID)
		if err != nil {
			return err
		}
		if q, err = f.svc.discloseLiquidationFee(ctx, tx, a, 6, prefix+":fee", res.Step.Fee, res.Step.Proceeds,
			stored.Disclosure.Input.ReferencePrice.At); err != nil {
			return err
		}
		return f.svc.bookLiquidationFee(ctx, tx, a, q, first[0].JournalTransactionID, first[0].EffectID)
	}))
	assert.Equal(t, first[0].DisclosureID, q.ID, "the replayed disclosure is the stored one")
	again := f.feeBookings(acct)
	require.Len(t, again, 1, "no second booking")
	assert.Equal(t, first[0].ID, again[0].ID)
	assert.Equal(t, 1, f.countRows(`SELECT count(*) FROM fee_quote_disclosures WHERE account_id = $1`, acct))
}

// TestIntegration_AKillSwitchStopsNeitherTheLiquidationNorItsFeeBooking: a
// liquidation reduces risk and a kill switch never stops it -- not a
// SPREAD_ENGINE switch, not the global one -- so its fee is still disclosed
// and booked under them.
func TestIntegration_AKillSwitchStopsNeitherTheLiquidationNorItsFeeBooking(t *testing.T) {
	f := newFixture(t)
	f.seed(usd(1_000_000), usd(100_000))
	acct, _ := f.fixedAccountOnBooks()
	f.mark("75")
	switched := f.dependencies(killReader{snap: killswitch.Snapshot{Switches: []killswitch.Switch{
		{Kind: killswitch.CapabilityDisable, ScopeID: string(gates.SpreadEngine), Active: true},
		{Kind: killswitch.GlobalNewRiskKill, ScopeID: killswitch.GlobalScope, Active: true},
	}}})
	svc, err := NewService(f.cfg, switched)
	require.NoError(t, err)
	var res LiquidationResult
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = svc.Liquidate(ctx, tx, LiquidateRequest{AccountID: acct, ExpectedStep: 1})
		return err
	}))
	require.NotNil(t, res.Step)
	require.True(t, res.Step.Fee.IsPositive())
	booked := f.feeBookings(acct)
	require.Len(t, booked, 1)
	requireQ(t, res.Step.Fee, booked[0].Charges.Platform, "the fee is booked under the switches")
	require.NoError(t, switched.FeeLedger.VerifyBooking(f.ctx, testDB, booked[0].ID))
}

// TestIntegration_ARefusedOrRolledBackLiquidationBooksNoFee: a liquidation of
// an account that needs none, one that waits for a fresh price, and one
// whose transaction rolls back leave no disclosure and no booking.
func TestIntegration_ARefusedOrRolledBackLiquidationBooksNoFee(t *testing.T) {
	f := newFixture(t)
	f.seed(usd(1_000_000), usd(100_000))
	acct, _ := f.fixedAccountOnBooks()
	nothing := func(what string) {
		t.Helper()
		assert.Zero(t, f.countRows(`SELECT count(*) FROM fee_quote_disclosures WHERE account_id = $1`, acct), "%s: no disclosure", what)
		assert.Empty(t, f.feeBookings(acct), "%s: no booking", what)
	}

	healthy, err := f.liquidate(acct, 0)
	require.NoError(t, err)
	assert.False(t, healthy.Eligible, "a healthy account needs no liquidation")
	nothing("not eligible")

	errRollBack := errors.New("roll the step back")
	f.mark("75")
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		res, err := f.svc.Liquidate(ctx, tx, LiquidateRequest{AccountID: acct, ExpectedStep: 1})
		if err != nil {
			return err
		}
		require.NotNil(t, res.Step)
		var inside int
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM fee_bookings WHERE journal_transaction_id = $1`,
			*res.Step.TradeJournalID).Scan(&inside))
		require.Equal(t, 1, inside, "booked in the step's own transaction")
		return errRollBack
	})
	require.ErrorIs(t, err, errRollBack)
	nothing("rolled back")

	f.clk.Advance(2 * time.Minute)
	waiting, err := f.liquidate(acct, 0)
	require.NoError(t, err)
	assert.Equal(t, DecisionAwaitPrice, waiting.Decision, "no sale, no fee, on a stale mark")
	nothing("waiting for a price")
}

// TestIntegration_TheDatabaseRefusesAnUnbookedLiquidationFee: a liquidation
// step that charges a fee the fee ledger did not book -- written around the
// service, its count kept so that only the fee is wrong -- is refused at
// COMMIT (FW001).
func TestIntegration_TheDatabaseRefusesAnUnbookedLiquidationFee(t *testing.T) {
	f := newFixture(t)
	f.seed(usd(1_000_000), usd(100_000))
	acct, a := f.fixedAccountOnBooks()
	f.mark("75")
	f.clk.Advance(2 * time.Minute) // stale: the case opens and waits, with no step
	res, err := f.liquidate(acct, 0)
	require.NoError(t, err)
	require.Equal(t, CaseAwaitingPrice, res.Case.Status)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO margin_liquidation_steps (id, case_id, step_no, action, asset_id, quantity, mark_mantissa,
			mark_scale, mark_observed_at, execution_mantissa, execution_scale, slippage_bps, slippage_cap_bps, proceeds, fee, repaid,
			excess, trade_journal_transaction_id, equity_before, equity_after, plan_hash, executed_at)
			VALUES ($1,$2,1,'SELL',$3,1,75,0,now(),75 * 9975,4,25,500,1,1,0,0,
			        (SELECT borrow_journal_transaction_id FROM margin_borrow_lots WHERE margin_account_id = $4 LIMIT 1),0,0,'h',now())`,
			uuid.New(), res.Case.ID, f.sol, a.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE margin_liquidation_cases SET steps_executed = 1 WHERE id = $1`, res.Case.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "FW001", db.SQLState(err), "a liquidation fee nobody booked: %v", err)
}
