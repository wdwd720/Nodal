//go:build integration

package advancedjobs

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/margin"
	"github.com/nodal/controlplane/internal/security"
)

// TestIntegration_AReservationPastItsTTLIsExpiredExactlyOnce: the expiry job
// leaves a reservation alone until its TTL passes, then expires it --
// returning the reserved quantity -- and a second run changes nothing.
func TestIntegration_AReservationPastItsTTLIsExpiredExactlyOnce(t *testing.T) {
	w := newWorld(t)
	c := w.customer("expiry")
	w.fund(c.account, w.usd.ID, w.usdUnits(100))
	var r capital.Reservation
	require.NoError(t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = w.capital.Reserve(ctx, tx, capital.ReserveRequest{
			AccountID: c.account.String(), AssetID: w.usd.ID, Quantity: w.usdUnits(40), ActorType: security.ActorUser,
			ActorID: c.user.String(), IdempotencyKey: "advancedjobs-expiry-" + uuid.NewString(), TTL: 10 * time.Minute,
			Reason: "advancedjobs integration test",
		})
		return err
	}))
	job := NewCapitalExpiry(testDB, w.capital, w.clk, Settings{})
	available := func() string {
		a, err := w.capital.Availability(w.ctx, testDB, c.account.String(), w.usd.ID, w.clk.Now())
		require.NoError(t, err)
		return a.Available.String()
	}

	run(t, w.ctx, job)
	got, err := w.capital.Get(w.ctx, testDB, r.ID)
	require.NoError(t, err)
	assert.Equal(t, capital.ReservationActive, got.Status, "not due before its TTL")
	assert.Equal(t, w.usdUnits(60).String(), available())

	w.clk.Advance(11 * time.Minute)
	rep := run(t, w.ctx, job)
	assert.True(t, rep.OK())
	assert.GreaterOrEqual(t, rep.Changed, 1)
	got, err = w.capital.Get(w.ctx, testDB, r.ID)
	require.NoError(t, err)
	assert.Equal(t, capital.ReservationExpired, got.Status)
	assert.Equal(t, w.usdUnits(100).String(), available(), "the expired reservation hands its quantity back")

	run(t, w.ctx, job)
	got, err = w.capital.Get(w.ctx, testDB, r.ID)
	require.NoError(t, err)
	assert.Equal(t, capital.ReservationExpired, got.Status)
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM outbox_events WHERE event_type = $1 AND aggregate_id = $2`,
		capital.TopicReservationExpired, r.ID.String()), "expired exactly once, however often the job runs")
}

// leveraged is a CROSS paper margin account holding 20 units of a simulated
// asset bought on margin.
type leveraged struct {
	cust customer
	sol  assets.Asset
	acct margin.Account
}

// leveragedAccount opens a margin account, posts 1,000 sandbox dollars of
// collateral and buys 20 units of a fresh asset marked at 100 entirely with
// borrowed money -- scenario B's opening.
func (w *world) leveragedAccount(label string) leveraged {
	w.t.Helper()
	sol := w.simAsset("AJSOL", 9, assets.RiskMajor)
	require.NoError(w.t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.margin.SeedFacilities(ctx, tx, margin.SeedRequest{Asset: w.usd.ID, PoolAmount: w.usdUnits(1_000_000),
			FundAmount: w.usdUnits(100_000), Key: "advancedjobs-seed-" + uuid.NewString(), Actor: w.system})
		return err
	}))
	c := w.customer(label)
	w.fund(c.account, w.usd.ID, w.usdUnits(1_000))
	req := margin.Requester{Type: security.ActorUser, ID: c.user.String()}
	var acct margin.Account
	require.NoError(w.t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		acct, err = w.margin.OpenAccount(ctx, tx, margin.OpenRequest{AccountID: c.account, MarginMode: margin.ModeCross,
			Mode: gates.ModePaper, NumeraireAssetID: w.usd.ID, Actor: econ.Actor{Type: security.ActorUser, ID: c.user.String()}})
		return err
	}))
	w.mark(sol.ID, "100")
	require.NoError(w.t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.margin.PostCollateral(ctx, tx, margin.CollateralRequest{AccountID: c.account, AssetID: w.usd.ID,
			Quantity: w.usdUnits(1_000), IdempotencyKey: "advancedjobs-post-" + uuid.NewString(), Requester: req})
		return err
	}))
	require.NoError(w.t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.margin.OpenLeveragedSpot(ctx, tx, margin.SpotRequest{AccountID: c.account, AssetID: sol.ID,
			Quantity: units(20, sol.Decimals), IdempotencyKey: "advancedjobs-buy-" + uuid.NewString(), Requester: req})
		return err
	}))
	return leveraged{cust: c, sol: sol, acct: acct}
}

// TestIntegration_InterestForADayIsChargedExactlyOnceAcrossThreeRuns: the
// interest job charges a completed UTC day once and only once, however many
// times it runs that day; a day still running is not charged.
func TestIntegration_InterestForADayIsChargedExactlyOnceAcrossThreeRuns(t *testing.T) {
	w := newWorld(t)
	l := w.leveragedAccount("interest")
	job := NewMarginInterest(testDB, w.margin, Settings{})
	accruals := func() int {
		return w.count(`SELECT count(*) FROM margin_interest_accruals WHERE margin_account_id = $1`, l.acct.ID)
	}
	journals := func() int {
		return w.count(`SELECT count(*) FROM journal_transactions j JOIN margin_interest_accruals a
			ON a.journal_transaction_id = j.id WHERE a.margin_account_id = $1 AND j.kind = 'MARGIN_INTEREST'`, l.acct.ID)
	}

	run(t, w.ctx, job)
	assert.Zero(t, accruals(), "no UTC day has completed since the loan was drawn")

	// Past midnight UTC: the partial first day [14:00, 00:00) is complete.
	w.clk.Set(time.Date(2026, 10, 8, 6, 0, 0, 0, time.UTC))
	for i := 0; i < 3; i++ {
		w.mark(l.sol.ID, "100")
		rep := run(t, w.ctx, job)
		assert.False(t, failedUnit(rep, l.cust.account.String()), "run %d: %+v", i+1, rep.Failures)
		w.clk.Advance(time.Hour)
	}
	assert.Equal(t, 1, accruals(), "one period, one accrual")
	assert.Equal(t, 1, journals(), "one period, one MARGIN_INTEREST posting")
	var start, end time.Time
	require.NoError(t, testDB.QueryRow(w.ctx, `SELECT period_start, period_end FROM margin_interest_accruals WHERE margin_account_id = $1`,
		l.acct.ID).Scan(&start, &end))
	assert.Equal(t, time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC), end.UTC(), "the period ends on the UTC midnight")
	interest := w.balance(ledger.CustomerAccount(l.cust.account, ledger.CodeInterestExpense, w.usd.ID))
	assert.True(t, interest.IsPositive(), "the day's interest was charged: %s", interest)
}

// TestIntegration_AnAccountPushedUnderMaintenanceByAFallingMarkIsLiquidatedByTheQueue:
// the mark-to-market job sees the fall and issues the margin call; the
// liquidation-queue job then sells, and running both again neither re-issues
// the call nor repeats a step.
func TestIntegration_AnAccountPushedUnderMaintenanceByAFallingMarkIsLiquidatedByTheQueue(t *testing.T) {
	w := newWorld(t)
	l := w.leveragedAccount("liquidation")
	mark := NewMarginMarkToMarket(testDB, w.margin, Settings{})
	queue := NewMarginLiquidation(testDB, w.margin, Settings{})
	status := func() margin.AccountStatus {
		a, err := w.margin.Account(w.ctx, testDB, l.cust.account)
		require.NoError(t, err)
		return a.Status
	}

	run(t, w.ctx, mark)
	assert.Equal(t, margin.StatusNormal, status(), "in good standing at 100")

	w.clk.Advance(time.Minute)
	w.mark(l.sol.ID, "40")
	rep := run(t, w.ctx, mark)
	assert.False(t, failedUnit(rep, l.cust.account.String()), "%+v", rep.Failures)
	assert.Equal(t, margin.StatusMarginCall, status(), "a falling mark puts the account under maintenance")
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM margin_calls WHERE margin_account_id = $1`, l.acct.ID))

	rep = run(t, w.ctx, queue)
	assert.False(t, failedUnit(rep, l.cust.account.String()), "%+v", rep.Failures)
	steps := w.count(`SELECT count(*) FROM margin_liquidation_steps s JOIN margin_liquidation_cases c ON c.id = s.case_id
		WHERE c.margin_account_id = $1`, l.acct.ID)
	assert.GreaterOrEqual(t, steps, 1, "the queue job executed a liquidation step")
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM journal_transactions WHERE kind = 'LIQUIDATION' AND id IN (
		SELECT trade_journal_transaction_id FROM margin_liquidation_steps s JOIN margin_liquidation_cases c ON c.id = s.case_id
		WHERE c.margin_account_id = $1)`, l.acct.ID), "the forced sale is posted as LIQUIDATION")
	sold := w.balance(ledger.CustomerAccount(l.cust.account, ledger.CodeWallet, l.sol.ID))
	assert.True(t, sold.Cmp(units(20, l.sol.Decimals)) < 0, "the liquidation sold part of the position: %s left", sold)

	// Running both again: the account is LIQUIDATING (the case decides, not
	// the marks), no second call is issued, and every step is numbered once.
	run(t, w.ctx, mark)
	run(t, w.ctx, queue)
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM margin_calls WHERE margin_account_id = $1`, l.acct.ID), "one margin call")
	assert.Equal(t, 0, w.count(`SELECT count(*) - count(DISTINCT (s.case_id, s.step_no)) FROM margin_liquidation_steps s
		JOIN margin_liquidation_cases c ON c.id = s.case_id WHERE c.margin_account_id = $1`, l.acct.ID))
}

// TestIntegration_MarkToMarketPagesThroughEveryAccount: a batch smaller than
// the population still reaches every account in turn -- the reason the job
// pages instead of calling MarkToMarket, which restarts at the lowest id --
// and wraps round once it has walked them all.
func TestIntegration_MarkToMarketPagesThroughEveryAccount(t *testing.T) {
	w := newWorld(t)
	want := map[accounts.AccountID]bool{}
	for i := 0; i < 3; i++ {
		want[w.leveragedAccount("paging").cust.account] = false
	}
	total := w.count(`SELECT count(*) FROM margin_accounts WHERE status IN ('NORMAL','WARNING','MARGIN_CALL') AND policy_version = $1`,
		w.margin.Policy().Version)
	job := NewMarginMarkToMarket(testDB, w.margin, Settings{Batch: 1})
	read := func(ctx context.Context, q db.Querier, after string, limit int) ([]marginUnit, error) {
		return scanMarginUnits(ctx, q, markableAccountsSQL, w.margin.Policy().Version, after, limit)
	}
	visits := map[accounts.AccountID]int{}
	for i := 0; i < 2*total; i++ {
		page, err := readPage(w.ctx, &job.pages, testDB, 1, read)
		require.NoError(t, err)
		require.Len(t, page, 1, "a non-empty population never yields an empty page")
		visits[page[0].account]++
	}
	for a := range want {
		assert.Equal(t, 2, visits[a], "every account is reached once per walk of the population")
	}
	rep := run(t, w.ctx, job)
	assert.Equal(t, 1, rep.Examined, "a run takes exactly its batch")
	assert.True(t, rep.OK())
}
