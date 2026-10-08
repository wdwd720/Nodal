//go:build integration

package buyingpower_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

func simAssetV2(t *testing.T, d *db.DB, symbol string, decimals uint8, risk assets.RiskClass) assets.Asset {
	t.Helper()
	a, err := assets.NewRepository().Create(context.Background(), d.Pool(), assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: symbol, Name: symbol + " (sandbox)", Decimals: decimals, RiskClass: risk, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	return a
}

func postV2(t *testing.T, d *db.DB, svc *ledger.Service, kind ledger.Kind, entries []ledger.Entry) {
	t.Helper()
	ref := uuid.NewString()
	require.NoError(t, d.InTx(context.Background(), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := svc.Post(ctx, tx, ledger.Posting{Kind: kind, IdempotencyKey: "bp2:" + ref,
			Reference: ledger.FinancialEventReference{Type: "bp2_fixture", ID: ref}, EffectiveAt: time.Now().UTC(), Entries: entries})
		return err
	}))
}

func q6(n int64) money.Quantity { return money.QuantityFromInt64(n) }

// TestIntegration_V2ComposesEverySimulatedComponentAndIgnoresRealCapital
// drives buying power V2 over real ledger balances: a sandbox dollar wallet,
// BTC posted as margin collateral at a recorded mark and a margin loan from a
// lending-pool facility -- plus a REAL-capital wallet on the same account that
// V2 must never value. A bridge leg in flight is covered where a real one can
// be made: a bridge journal exists only for a leg of a route (BR005), so
// internal/bridge's TestIntegration_TheV2LoaderCountsALegInFlightOnlyAsItsHaircut
// runs this loader over a leg the bridge itself put in flight.
func TestIntegration_V2ComposesEverySimulatedComponentAndIgnoresRealCapital(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Now().UTC())
	svc := ledger.NewService(clk, "itest")
	acct := seedAccount(t, d)
	usd := simAssetV2(t, d, "SBXUSD", 6, assets.RiskSettlement)
	btc := simAssetV2(t, d, "SBXBTC", 8, assets.RiskMajor)
	real := seedAssetRow(t, d, "USDC", 6, true, assets.RiskSettlement)
	seedWalletBalance(t, d, acct, real.ID, q6(9_999_000_000)) // real capital: must not appear

	cust := func(code ledger.Code, a assets.AssetID) ledger.AccountRef {
		return ledger.CustomerAccount(acct, code, a)
	}
	// $2,000 sandbox cash; 0.1 BTC funded then posted as margin collateral.
	postV2(t, d, svc, ledger.KindSandboxFunding, []ledger.Entry{
		{Account: cust(ledger.CodeWallet, usd.ID), Side: ledger.Debit, Quantity: q6(2_000_000_000)},
		{Account: cust(ledger.CodeCapital, usd.ID), Side: ledger.Credit, Quantity: q6(2_000_000_000)},
	})
	postV2(t, d, svc, ledger.KindSandboxFunding, []ledger.Entry{
		{Account: cust(ledger.CodeWallet, btc.ID), Side: ledger.Debit, Quantity: q6(10_000_000)},
		{Account: cust(ledger.CodeCapital, btc.ID), Side: ledger.Credit, Quantity: q6(10_000_000)},
	})
	postV2(t, d, svc, ledger.KindMarginCollateralMove, []ledger.Entry{
		{Account: cust(ledger.CodeMarginCollateral, btc.ID), Side: ledger.Debit, Quantity: q6(10_000_000)},
		{Account: cust(ledger.CodeWallet, btc.ID), Side: ledger.Credit, Quantity: q6(10_000_000)},
	})
	// Borrow $500 from a lending pool.
	reg := econ.NewRegistry()
	var pool econ.Facility
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		pool, err = reg.EnsureFacility(ctx, tx, ledger.FacilityLendingPool, "bp2 pool "+uuid.NewString(),
			econ.Actor{Type: security.ActorSystem, ID: "itest"})
		return err
	}))
	postV2(t, d, svc, ledger.KindFacilityFunding, []ledger.Entry{
		{Account: pool.Account(ledger.CodePoolCash, usd.ID), Side: ledger.Debit, Quantity: q6(1_000_000_000)},
		{Account: pool.Account(ledger.CodePoolCapital, usd.ID), Side: ledger.Credit, Quantity: q6(1_000_000_000)},
	})
	postV2(t, d, svc, ledger.KindMarginBorrow, []ledger.Entry{
		{Account: cust(ledger.CodeWallet, usd.ID), Side: ledger.Debit, Quantity: q6(500_000_000)},
		{Account: cust(ledger.CodeMarginLoan, usd.ID), Side: ledger.Credit, Quantity: q6(500_000_000)},
		{Account: pool.Account(ledger.CodeLoanReceivable, usd.ID), Side: ledger.Debit, Quantity: q6(500_000_000)},
		{Account: pool.Account(ledger.CodePoolCash, usd.ID), Side: ledger.Credit, Quantity: q6(500_000_000)},
	})
	// Mark: BTC $60,000.00.
	prices := valuation.NewPriceStore(clk)
	_, err := prices.RecordPrice(ctx, d.Pool(), valuation.PriceObservation{AssetID: btc.ID, QuoteAssetID: usd.ID,
		Mantissa: money.QuantityFromInt64(6_000_000), Scale: 2, Source: "bp2-itest", ObservedAt: clk.Now().Add(-time.Second)})
	require.NoError(t, err)

	loader, err := buyingpower.NewV2Loader(buyingpower.V2LoaderConfig{Clock: clk, Prices: prices, QuoteAssetID: usd.ID, MaxPriceAge: time.Minute})
	require.NoError(t, err)
	bp, err := loader.Compute(ctx, d.Pool(), "CUSTOMER", acct.String())
	require.NoError(t, err)

	// Marked: cash 2,500 + BTC collateral 6,000 = 8,500; debt 500.
	assert.Equal(t, money.USDFromMinor(8_000_00), bp.GrossEquity, "real capital must not appear: %+v", bp)
	// Eligible: 2,500 + 6,000 × 80% = 7,300.
	assert.Equal(t, money.USDFromMinor(7_300_00), bp.EligibleCollateral)
	assert.Equal(t, money.USDFromMinor(1_200_00), bp.Haircuts, "the BTC haircut")
	assert.True(t, bp.PendingBridgeHaircut.IsZero(), "nothing is in flight")
	assert.Equal(t, money.USDFromMinor(500_00), bp.BorrowLiabilities)
	assert.Equal(t, money.USDFromMinor(2_500_00), bp.TradeBuyingPower, "spendable sandbox cash")
	assert.Equal(t, money.USDFromMinor(2_500_00), bp.Withdrawable)

	// A stale mark removes BTC from collateral rather than valuing it.
	clk.Advance(10 * time.Minute)
	bp, err = loader.Compute(ctx, d.Pool(), "CUSTOMER", acct.String())
	require.NoError(t, err)
	assert.Equal(t, money.USDFromMinor(2_500_00), bp.EligibleCollateral, "stale BTC contributes nothing")
}

// TestIntegration_V2CountsWhatIsOwedAgainstTheOwner: liabilities round up
// and holdings round down, so a sub-cent never shows as buying power, and a
// deficit left by a bankrupt margin account is a liability that blocks new
// risk and withdrawals until it is repaid.
func TestIntegration_V2CountsWhatIsOwedAgainstTheOwner(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Now().UTC())
	svc := ledger.NewService(clk, "itest")
	acct := seedAccount(t, d)
	usd := simAssetV2(t, d, "SBXOWED", 6, assets.RiskSettlement)
	cust := func(code ledger.Code) ledger.AccountRef { return ledger.CustomerAccount(acct, code, usd.ID) }

	reg := econ.NewRegistry()
	var pool, fund econ.Facility
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		by := econ.Actor{Type: security.ActorSystem, ID: "itest"}
		var err error
		if pool, err = reg.EnsureFacility(ctx, tx, ledger.FacilityLendingPool, "owed pool "+uuid.NewString(), by); err != nil {
			return err
		}
		fund, err = reg.EnsureFacility(ctx, tx, ledger.FacilityInsuranceFund, "owed fund "+uuid.NewString(), by)
		return err
	}))
	postV2(t, d, svc, ledger.KindSandboxFunding, []ledger.Entry{
		{Account: cust(ledger.CodeWallet), Side: ledger.Debit, Quantity: q6(1_000_000_000)},
		{Account: cust(ledger.CodeCapital), Side: ledger.Credit, Quantity: q6(1_000_000_000)},
	})
	for _, f := range []struct {
		facility econ.Facility
		cash     ledger.Code
		capital  ledger.Code
	}{{pool, ledger.CodePoolCash, ledger.CodePoolCapital}, {fund, ledger.CodeInsuranceFundCash, ledger.CodeInsuranceFundCapital}} {
		postV2(t, d, svc, ledger.KindFacilityFunding, []ledger.Entry{
			{Account: f.facility.Account(f.cash, usd.ID), Side: ledger.Debit, Quantity: q6(10_000_000_000)},
			{Account: f.facility.Account(f.capital, usd.ID), Side: ledger.Credit, Quantity: q6(10_000_000_000)},
		})
	}
	// Borrow 500.000001: one base unit past a cent.
	postV2(t, d, svc, ledger.KindMarginBorrow, []ledger.Entry{
		{Account: cust(ledger.CodeWallet), Side: ledger.Debit, Quantity: q6(500_000_001)},
		{Account: cust(ledger.CodeMarginLoan), Side: ledger.Credit, Quantity: q6(500_000_001)},
		{Account: pool.Account(ledger.CodeLoanReceivable, usd.ID), Side: ledger.Debit, Quantity: q6(500_000_001)},
		{Account: pool.Account(ledger.CodePoolCash, usd.ID), Side: ledger.Credit, Quantity: q6(500_000_001)},
	})
	loader, err := buyingpower.NewV2Loader(buyingpower.V2LoaderConfig{Clock: clk, Prices: valuation.NewPriceStore(clk),
		QuoteAssetID: usd.ID, MaxPriceAge: time.Hour})
	require.NoError(t, err)
	bp, err := loader.Compute(ctx, d, "CUSTOMER", acct.String())
	require.NoError(t, err)
	assert.Equal(t, "500.01", bp.BorrowLiabilities.String(), "what is owed rounds up")
	assert.Equal(t, "999.99", bp.GrossEquity.String(), "1500.000001 held rounds down to 1500.00, less 500.01 owed")

	// The insurance fund covers 100 of the loan; the customer now owes it.
	postV2(t, d, svc, ledger.KindDeficitCoverage, []ledger.Entry{
		{Account: cust(ledger.CodeMarginLoan), Side: ledger.Debit, Quantity: q6(100_000_000)},
		{Account: cust(ledger.CodeDeficit), Side: ledger.Credit, Quantity: q6(100_000_000)},
		{Account: pool.Account(ledger.CodePoolCash, usd.ID), Side: ledger.Debit, Quantity: q6(100_000_000)},
		{Account: pool.Account(ledger.CodeLoanReceivable, usd.ID), Side: ledger.Credit, Quantity: q6(100_000_000)},
		{Account: fund.Account(ledger.CodeDeficitReceivable, usd.ID), Side: ledger.Debit, Quantity: q6(100_000_000)},
		{Account: fund.Account(ledger.CodeInsuranceFundCash, usd.ID), Side: ledger.Credit, Quantity: q6(100_000_000)},
	})
	bp, err = loader.Compute(ctx, d, "CUSTOMER", acct.String())
	require.NoError(t, err)
	assert.Equal(t, "500.01", bp.BorrowLiabilities.String(), "loan 400.000001 rounds to 400.01, plus the 100 deficit")
	assert.True(t, bp.Withdrawable.IsZero(), "nothing is withdrawable while a deficit is outstanding")
	assert.True(t, bp.RiskIncreasingBuyingPower.IsZero(), "no new risk while a deficit is outstanding")
	found := false
	for _, r := range bp.Reasons {
		if r.Code == buyingpower.V2ReasonBlocked && strings.HasPrefix(r.Detail, "DEFICIT_OUTSTANDING") {
			found = true
		}
	}
	assert.True(t, found, "the block names the deficit: %+v", bp.Reasons)
}
