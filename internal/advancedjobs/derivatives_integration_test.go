//go:build integration

package advancedjobs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/derivatives"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// derivativesBook is a world's derivatives fixture: an underlying spot pair
// in the sandbox dollar and a funded clearing house.
type derivativesBook struct {
	w    *world
	spot instruments.InstrumentID
}

func (w *world) derivativesBook() derivativesBook {
	w.t.Helper()
	btc := w.simAsset("AJBTC", 8, assets.RiskMajor)
	var spot instruments.InstrumentID
	require.NoError(w.t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ins, err := w.instr.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{
			Base: btc.ID, Quote: w.usd.ID, Settlement: w.usd.ID, CanonicalName: "AJBTC/SBX-USD " + uuid.NewString(),
			RiskClass: assets.RiskMajor, Status: assets.StatusActive, ActiveFrom: w.clk.Now().Add(-time.Hour),
		})
		spot = ins.ID
		if err != nil {
			return err
		}
		_, err = w.derivatives.FundClearingHouse(ctx, tx, w.usd.ID, w.usdUnits(10_000_000), "advancedjobs-"+uuid.NewString())
		return err
	}))
	return derivativesBook{w: w, spot: spot}
}

func dollars(n int64) money.Quantity { return money.QuantityFromInt64(n * 100) }

// contract creates a derivative instrument settling in the sandbox dollar.
func (b derivativesBook) contract(kind instruments.InstrumentType, adjust func(*instruments.Terms)) instruments.InstrumentID {
	b.w.t.Helper()
	under := b.spot
	t := instruments.Terms{
		Kind: kind, Symbol: "AJ-" + string(kind) + "-" + strings.ToUpper(uuid.NewString()[:8]), VenueCode: "NODAL-SANDBOX",
		SettlementAssetID: b.w.usd.ID, UnderlyingInstrumentID: &under, ContractMultiplier: money.QuantityFromInt64(1_000_000),
		PriceScale: 2, TickSize: money.QuantityFromInt64(50), LotSize: money.QuantityFromInt64(1), MinQuantity: money.QuantityFromInt64(1),
		SettlementStyle: instruments.SettleCash, TradingCalendar: derivatives.Calendar24x7, OracleMaxAgeMS: 60_000,
		OracleSources: []string{"advancedjobs-itest"}, CorporateActionBehavior: instruments.CorporateActionsNone,
		Transferability: instruments.NonTransferable, MarginModel: instruments.MarginLinear,
	}
	if adjust != nil {
		adjust(&t)
	}
	var id instruments.InstrumentID
	require.NoError(b.w.t, b.w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ins, _, err := b.w.instr.CreateAdvanced(ctx, tx, instruments.AdvancedSpec{
			Terms: t, CanonicalName: t.Symbol, RiskClass: assets.RiskSpeculative, Status: assets.StatusActive,
			ActiveFrom: b.w.clk.Now().Add(-time.Hour), ActorType: "OPERATOR", ActorID: "advancedjobs-itest-operator",
		})
		id = ins.ID
		return err
	}))
	return id
}

// mark records a mark and an explicit index (whole dollars) observed at the
// world's time.
func (b derivativesBook) mark(ins instruments.InstrumentID, markDollars, indexDollars int64) {
	b.w.t.Helper()
	at := b.w.clk.Now()
	require.NoError(b.w.t, b.w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		mp := money.Price{Mantissa: dollars(markDollars), Scale: 2, QuoteAsset: b.w.usd.ID.String(), Source: "advancedjobs-itest", At: at}
		ip := money.Price{Mantissa: dollars(indexDollars), Scale: 2, QuoteAsset: b.w.usd.ID.String(), Source: "advancedjobs-itest-index", At: at}
		_, err := b.w.derivatives.RecordMark(ctx, tx, derivatives.RecordMarkRequest{InstrumentID: ins, Mark: mp, Source: "advancedjobs-itest", ObservedAt: at, Index: &ip})
		return err
	}))
}

// open opens a position of n contracts for a fresh, funded customer.
func (b derivativesBook) open(ins instruments.InstrumentID, side derivatives.Side, n int64) accounts.AccountID {
	b.w.t.Helper()
	c := b.w.customer("derivatives")
	b.w.fund(c.account, b.w.usd.ID, b.w.usdUnits(100_000))
	key := "advancedjobs-" + uuid.NewString()
	require.NoError(b.w.t, b.w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := b.w.derivatives.OpenPosition(ctx, tx, derivatives.OpenRequest{IntentRequest: derivatives.IntentRequest{
			AccountID: c.account, InstrumentID: ins, Mode: gates.ModePaper, IdempotencyKey: key,
			ActorType: security.ActorUser, ActorID: c.user.String(), Jurisdiction: "US-CA",
			Verification: valuedomain.VerificationNodalIdentity, CorrelationID: "corr-" + key,
		}, Side: side, Quantity: money.QuantityFromInt64(n)})
		return err
	}))
	return c.account
}

// TestIntegration_AFundingPeriodIsChargedOnce: the funding job charges a
// completed perpetual funding interval once -- the funding_rates claim, one
// accrual per position -- and a second run is a replay that posts nothing.
func TestIntegration_AFundingPeriodIsChargedOnce(t *testing.T) {
	w := newWorld(t)
	b := w.derivativesBook()
	perp := b.contract(instruments.TypePerpetual, func(t *instruments.Terms) { t.FundingIntervalMS = 3600 * 1000 })
	b.mark(perp, 65_000, 64_935)
	b.open(perp, derivatives.Long, 2)
	b.open(perp, derivatives.Short, 2)
	w.clk.Advance(30 * time.Minute)
	b.mark(perp, 65_100, 65_000)
	job := NewDerivativesFunding(testDB, w.derivatives, w.instr, w.clk, Settings{})

	rep := run(t, w.ctx, job)
	assert.Zero(t, w.count(`SELECT count(*) FROM funding_rates WHERE instrument_id = $1`, perp), "the interval has not ended")

	w.clk.Set(time.Date(2026, 10, 7, 15, 5, 0, 0, time.UTC))
	rep = run(t, w.ctx, job)
	assert.False(t, failedUnit(rep, perp.String()), "%+v", rep.Failures)
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM funding_rates WHERE instrument_id = $1 AND interval_start = $2`,
		perp, time.Date(2026, 10, 7, 14, 0, 0, 0, time.UTC)))
	accruals := w.count(`SELECT count(*) FROM funding_accruals a JOIN funding_rates r ON r.id = a.funding_rate_id WHERE r.instrument_id = $1`, perp)
	assert.Equal(t, 2, accruals, "one accrual per position open at the interval's end")
	flows := w.count(`SELECT count(*) FROM derivative_cash_flows WHERE instrument_id = $1 AND kind = 'FUNDING'`, perp)

	rep = run(t, w.ctx, job)
	assert.False(t, failedUnit(rep, perp.String()), "%+v", rep.Failures)
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM funding_rates WHERE instrument_id = $1`, perp), "charged once")
	assert.Equal(t, accruals, w.count(`SELECT count(*) FROM funding_accruals a JOIN funding_rates r ON r.id = a.funding_rate_id WHERE r.instrument_id = $1`, perp))
	assert.Equal(t, flows, w.count(`SELECT count(*) FROM derivative_cash_flows WHERE instrument_id = $1 AND kind = 'FUNDING'`, perp),
		"no second cash flow")
}

// TestIntegration_VariationMarginSettlesTheLatestIntervalOnce: the
// variation-margin job settles the latest completed interval at a fresh mark
// and a second run in the same interval is a replay.
func TestIntegration_VariationMarginSettlesTheLatestIntervalOnce(t *testing.T) {
	w := newWorld(t)
	b := w.derivativesBook()
	perp := b.contract(instruments.TypePerpetual, func(t *instruments.Terms) { t.FundingIntervalMS = 8 * 3600 * 1000 })
	b.mark(perp, 65_000, 65_000)
	b.open(perp, derivatives.Long, 1)
	b.open(perp, derivatives.Short, 1)
	job := NewDerivativesVariationMargin(testDB, w.derivatives, w.instr, w.clk, Settings{})

	w.clk.Set(time.Date(2026, 10, 7, 15, 5, 0, 0, time.UTC))
	b.mark(perp, 65_500, 65_400)
	rep := run(t, w.ctx, job)
	assert.False(t, failedUnit(rep, perp.String()), "%+v", rep.Failures)
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM variation_margin_runs WHERE instrument_id = $1 AND interval_start = $2`,
		perp, time.Date(2026, 10, 7, 14, 0, 0, 0, time.UTC)), "the latest completed hour is settled")
	vm := w.count(`SELECT count(*) FROM derivative_cash_flows WHERE instrument_id = $1 AND kind = 'VARIATION_MARGIN'`, perp)
	assert.Positive(t, vm, "the mark moved, so variation margin flowed")

	rep = run(t, w.ctx, job)
	assert.False(t, failedUnit(rep, perp.String()), "%+v", rep.Failures)
	assert.Zero(t, rep.Changed, "a second run in the same interval is a replay")
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM variation_margin_runs WHERE instrument_id = $1`, perp))
	assert.Equal(t, vm, w.count(`SELECT count(*) FROM derivative_cash_flows WHERE instrument_id = $1 AND kind = 'VARIATION_MARGIN'`, perp))
}

// TestIntegration_AnExpiredFutureSettlesAtItsOwnMarkOnce: the expiry job
// settles an expired future at the subsystem's mark-derived price -- never an
// override -- once.
func TestIntegration_AnExpiredFutureSettlesAtItsOwnMarkOnce(t *testing.T) {
	w := newWorld(t)
	b := w.derivativesBook()
	expiry := time.Date(2026, 10, 7, 14, 30, 0, 0, time.UTC)
	fut := b.contract(instruments.TypeFuture, func(t *instruments.Terms) { t.Expiry = &expiry })
	b.mark(fut, 65_000, 65_000)
	b.open(fut, derivatives.Long, 1)
	b.open(fut, derivatives.Short, 1)
	job := NewDerivativesExpiry(testDB, w.derivatives, w.instr, w.clk, Settings{})

	run(t, w.ctx, job)
	assert.Zero(t, w.count(`SELECT count(*) FROM derivative_settlements WHERE instrument_id = $1`, fut), "not expired yet")

	w.clk.Set(expiry.Add(-10 * time.Second))
	b.mark(fut, 65_250, 65_200)
	w.clk.Set(expiry.Add(5 * time.Minute))
	rep := run(t, w.ctx, job)
	assert.False(t, failedUnit(rep, fut.String()), "%+v", rep.Failures)
	var source string
	var price money.Quantity
	require.NoError(t, testDB.QueryRow(w.ctx, `SELECT price_source, settlement_mantissa FROM derivative_settlements WHERE instrument_id = $1`, fut).
		Scan(&source, &price))
	assert.True(t, strings.HasPrefix(source, "mark:"), "settled at the subsystem's own mark, not an override: %s", source)
	assert.Equal(t, dollars(65_250).String(), price.String())
	assert.Zero(t, w.count(`SELECT count(*) FROM derivative_positions WHERE instrument_id = $1 AND status = 'OPEN'`, fut))

	rep = run(t, w.ctx, job)
	assert.False(t, failedUnit(rep, fut.String()), "a settled contract is no longer a candidate")
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM derivative_settlements WHERE instrument_id = $1`, fut))
}

// TestIntegration_AnExpiredFutureWithoutAFreshMarkIsRefusedNotGuessed: with
// no mark fresh at expiry the subsystem refuses, the job reports the failure
// every run, and nothing is settled.
func TestIntegration_AnExpiredFutureWithoutAFreshMarkIsRefusedNotGuessed(t *testing.T) {
	w := newWorld(t)
	b := w.derivativesBook()
	expiry := time.Date(2026, 10, 7, 14, 30, 0, 0, time.UTC)
	fut := b.contract(instruments.TypeFuture, func(t *instruments.Terms) { t.Expiry = &expiry })
	b.mark(fut, 65_000, 65_000)
	b.open(fut, derivatives.Long, 1)
	w.clk.Set(expiry.Add(time.Hour))
	job := NewDerivativesExpiry(testDB, w.derivatives, w.instr, w.clk, Settings{})
	for i := 0; i < 2; i++ {
		rep := run(t, w.ctx, job)
		var mine *Failure
		for k := range rep.Failures {
			if rep.Failures[k].Unit == fut.String() {
				mine = &rep.Failures[k]
			}
		}
		require.NotNil(t, mine, "run %d reports the contract it could not settle", i+1)
		assert.Equal(t, "STALE_MARKET_DATA", mine.Code)
	}
	assert.Zero(t, w.count(`SELECT count(*) FROM derivative_settlements WHERE instrument_id = $1`, fut))
}

// failedUnit reports whether a run failed the given unit. Other tests' units
// share the database, so a test asserts only about its own.
func failedUnit(rep Report, unit string) bool {
	for _, f := range rep.Failures {
		if f.Unit == unit || strings.HasPrefix(f.Unit, unit+"@") {
			return true
		}
	}
	return false
}

// TestIntegration_ClearingReconciliationMatchesTheRegister: after real flows
// the clearing house's books agree with the register, the job records no
// mismatch, and a second run is the same.
func TestIntegration_ClearingReconciliationMatchesTheRegister(t *testing.T) {
	w := newWorld(t)
	b := w.derivativesBook()
	perp := b.contract(instruments.TypePerpetual, func(t *instruments.Terms) { t.FundingIntervalMS = 3600 * 1000 })
	b.mark(perp, 65_000, 65_000)
	b.open(perp, derivatives.Long, 1)
	b.open(perp, derivatives.Short, 1)
	job := NewDerivativesClearing(testDB, w.derivatives, Settings{})
	for i := 0; i < 2; i++ {
		rep := run(t, w.ctx, job)
		assert.True(t, rep.OK(), "%+v", rep.Failures)
		assert.Positive(t, rep.Examined)
		assert.Zero(t, rep.Changed, "nothing disagrees")
	}
}
