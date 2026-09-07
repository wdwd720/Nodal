//go:build integration

package buyingpower_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/capital/buyingpower/buyingpowertest"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

var testAppURL = os.Getenv("CP_TEST_DATABASE_URL")

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	if testAppURL == "" {
		t.Skip("CP_TEST_DATABASE_URL not set; skipping integration test (provision one with `go run ./scripts/testdb -name positions`)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := db.Open(ctx, db.Config{URL: testAppURL, AppName: "buyingpower-itest", MaxConns: 4})
	require.NoError(t, err)
	t.Cleanup(d.Close)
	return d
}

func seedAccount(t *testing.T, d *db.DB) accounts.AccountID {
	t.Helper()
	repo := accounts.NewRepository()
	u, err := repo.CreateUser(t.Context(), d, "itest", "sub-"+id.New[id.Any]().String(), nil)
	require.NoError(t, err)
	a, err := repo.CreateAccount(t.Context(), d, u.ID, accounts.KindCustomer)
	require.NoError(t, err)
	return a.ID
}

func seedAssetRow(t *testing.T, d *db.DB, symbol string, decimals uint8, stable bool, risk assets.RiskClass) assets.Asset {
	t.Helper()
	a := assets.Asset{
		Chain: "solana-devnet", MintAddress: "mint-" + id.New[id.Any]().String(), Kind: assets.KindSPLToken, ValueDomain: valuedomain.SelfCustodialCrypto, Symbol: symbol, Name: symbol,
		Decimals: decimals, IsStablecoin: stable, RiskClass: risk, Status: assets.StatusActive,
	}
	if stable {
		a.PegCurrency = "USD"
	}
	created, err := assets.NewRepository().Create(t.Context(), d, a)
	require.NoError(t, err)
	return created
}

func seedWalletBalance(t *testing.T, d *db.DB, account accounts.AccountID, asset assets.AssetID, qty money.Quantity) {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		wallet := ledgerAccount(t, tx, account, asset, "WALLET", "DEBIT")
		capital := ledgerAccount(t, tx, account, asset, "CAPITAL", "CREDIT")
		txID := id.New[id.Any]()
		if _, err := tx.Exec(ctx, `INSERT INTO journal_transactions (id, kind, idempotency_key, reference_type, reference_id, effective_at, posted_by_actor_type, posted_by_actor_id, content_hash)
			VALUES ($1,'SEED',$2,'seed',$3,now(),'SYSTEM','buyingpower-itest',$4)`, txID, "seed:"+txID.String(), txID.String(), []byte{0}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO journal_entries (id, transaction_id, seq, ledger_account_id, asset_id, side, quantity) VALUES ($1,$2,0,$3,$4,'DEBIT',$5)`,
			id.New[id.Any](), txID, wallet, asset, qty); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO journal_entries (id, transaction_id, seq, ledger_account_id, asset_id, side, quantity) VALUES ($1,$2,1,$3,$4,'CREDIT',$5)`,
			id.New[id.Any](), txID, capital, asset, qty)
		return err
	}))
}

func ledgerAccount(t *testing.T, tx pgx.Tx, account accounts.AccountID, asset assets.AssetID, code, side string) id.ID[id.Any] {
	t.Helper()
	ctx := t.Context()
	_, err := tx.Exec(ctx, `INSERT INTO ledger_accounts (id, owner_type, owner_id, code, asset_id, normal_side)
		VALUES ($1,'CUSTOMER',$2,$3,$4,$5) ON CONFLICT (owner_type, owner_id, code, asset_id) DO NOTHING`, id.New[id.Any](), account, code, asset, side)
	require.NoError(t, err)
	var out id.ID[id.Any]
	require.NoError(t, tx.QueryRow(ctx, `SELECT id FROM ledger_accounts WHERE owner_type='CUSTOMER' AND owner_id=$1 AND code=$2 AND asset_id=$3`, account, code, asset).Scan(&out))
	return out
}

func seedDeposit(t *testing.T, d *db.DB, account accounts.AccountID, asset assets.AssetID, status string, expected, observed *money.Quantity, withdrawalEligible bool) {
	t.Helper()
	_, err := d.Exec(t.Context(), `INSERT INTO deposits (id, account_id, provider, status, expected_asset_id, expected_quantity, observed_quantity, withdrawal_eligible, buying_power_eligible, idempotency_key)
		VALUES ($1,$2,'fake',$3,$4,$5,$6,$7,$8,$9)`,
		id.New[id.Any](), account, status, asset, expected, observed, withdrawalEligible, status == "AVAILABLE", "dep:"+id.New[id.Any]().String())
	require.NoError(t, err)
}

func TestIntegration_Compute_OverSeededLedger(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	acct := seedAccount(t, d)
	usdc := seedAssetRow(t, d, "USDC", 6, true, assets.RiskSettlement)
	sol := seedAssetRow(t, d, "SOL", 9, false, assets.RiskMajor)
	bonk := seedAssetRow(t, d, "BONK", 5, false, assets.RiskSpeculative)
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)

	// Ledger: 1000 USDC, 1.5 SOL, 1M BONK.
	seedWalletBalance(t, d, acct, usdc.ID, q(t, "1000000000"))
	seedWalletBalance(t, d, acct, sol.ID, q(t, "1500000000"))
	seedWalletBalance(t, d, acct, bonk.ID, q(t, "100000000000"))
	// 100 USDC reserved, 50 USDC on an active hold, 25 USDC on a released hold, 10 USDC on an expired hold.
	_, err := d.Exec(ctx, `INSERT INTO asset_reservation_totals (account_id, asset_id, reserved) VALUES ($1,$2,100000000)`, acct, usdc.ID)
	require.NoError(t, err)
	_, err = d.Exec(ctx, `INSERT INTO withdrawal_holds (id, account_id, asset_id, quantity, reason) VALUES ($1,$2,$3,50000000,'REVERSIBILITY_WINDOW')`, id.New[id.Any](), acct, usdc.ID)
	require.NoError(t, err)
	_, err = d.Exec(ctx, `INSERT INTO withdrawal_holds (id, account_id, asset_id, quantity, reason, released_at) VALUES ($1,$2,$3,25000000,'RELEASED',now())`, id.New[id.Any](), acct, usdc.ID)
	require.NoError(t, err)
	_, err = d.Exec(ctx, `INSERT INTO withdrawal_holds (id, account_id, asset_id, quantity, reason, expires_at) VALUES ($1,$2,$3,10000000,'EXPIRED',$4)`, id.New[id.Any](), acct, usdc.ID, now.Add(-time.Minute))
	require.NoError(t, err)
	// Deposits: 200 USDC pending (PROVIDER_CONFIRMED), 300 USDC pending (RECONCILED, observed 300.5), 1000 USDC AVAILABLE eligible, 400 AVAILABLE not eligible, one CREATED (no amount), one FAILED.
	exp200, exp300, obs300 := q(t, "200000000"), q(t, "300000000"), q(t, "300500000")
	exp1000, exp400 := q(t, "1000000000"), q(t, "400000000")
	seedDeposit(t, d, acct, usdc.ID, "PROVIDER_CONFIRMED", &exp200, nil, false)
	seedDeposit(t, d, acct, usdc.ID, "RECONCILED", &exp300, &obs300, false)
	seedDeposit(t, d, acct, usdc.ID, "AVAILABLE", &exp1000, &exp1000, true)
	seedDeposit(t, d, acct, usdc.ID, "AVAILABLE", &exp400, &exp400, false)
	seedDeposit(t, d, acct, usdc.ID, "CREATED", nil, nil, false)
	seedDeposit(t, d, acct, usdc.ID, "FAILED", &exp400, nil, false)

	// Policies and prices through the real stores.
	policies := valuation.NewPolicyStore()
	prices := valuation.NewPriceStore(clk)
	np := func(asset assets.AssetID, status assets.Status, factor money.BPS, stable valuation.StablecoinStatus, version string) valuation.NewPolicy {
		return valuation.NewPolicy{
			AssetID: asset, Status: status, CollateralFactor: factor, StablecoinStatus: stable, MaxPriceAge: 30 * time.Second,
			PolicyVersion: version, EffectiveAt: now.Add(-time.Hour), ActorType: "OPERATOR", ActorID: "ops", Reason: "itest",
		}
	}
	_, err = policies.RecordPolicy(ctx, d, np(usdc.ID, assets.StatusActive, 10000, valuation.StablecoinNormal, "usdc-1"))
	require.NoError(t, err)
	_, err = policies.RecordPolicy(ctx, d, np(sol.ID, assets.StatusActive, 7500, "", "sol-1"))
	require.NoError(t, err)
	// BONK deliberately has no policy.
	rec := func(asset assets.AssetID, decimal string, at time.Time) {
		p, err := money.PriceFromDecimalString(decimal, "x", "pyth", at)
		require.NoError(t, err)
		_, err = prices.RecordPrice(ctx, d, valuation.PriceObservation{AssetID: asset, QuoteAssetID: usdc.ID, Mantissa: p.Mantissa, Scale: p.Scale, Source: "pyth", ObservedAt: at})
		require.NoError(t, err)
	}
	rec(sol.ID, "150.123456", now.Add(-5*time.Second))
	rec(bonk.ID, "0.00002345", now.Add(-5*time.Second))

	kills := &buyingpowertest.KillSwitches{}
	recon := &buyingpowertest.ReconciliationBlocks{}
	engine, err := buyingpower.NewEngine(buyingpower.Deps{Clock: clk, Policies: policies, Prices: prices, KillSwitches: kills, Reconciliation: recon, QuoteAssetID: usdc.ID})
	require.NoError(t, err)

	bp, err := engine.Compute(ctx, d, acct.String(), buyingpower.PurposeTrade)
	require.NoError(t, err)
	assert.Equal(t, "1225.19", bp.PortfolioValue.String(), "USDC 1000 + SOL 225.19; BONK unvalued (no policy)")
	assert.Equal(t, "1018.89", bp.BuyingPower.String(), "USDC 1000−100−50 + SOL 168.89")
	assert.Equal(t, "850.00", bp.AvailableNow.String())
	assert.Equal(t, "100.00", bp.Reserved.String())
	assert.Equal(t, "500.00", bp.Pending.String(), "PROVIDER_CONFIRMED 200 + RECONCILED 300 at expected quantity")
	assert.Equal(t, "0.00", bp.Withdrawable.String(), "the POLICY_MISSING restriction zeroes withdrawable")
	require.Len(t, bp.Restrictions, 1)
	assert.Equal(t, buyingpower.RestrictionPolicyMissing, bp.Restrictions[0].Code)
	assert.Equal(t, bonk.ID, bp.Restrictions[0].AssetID)
	assert.False(t, bp.Blocked(), "asset-scoped restriction does not block TRADE")
	assert.Equal(t, now, bp.AsOf)
	require.Len(t, bp.UnderlyingBalances, 3)

	// Give BONK a policy: everything is valued; withdrawable = min(850, 1000 eligible).
	_, err = policies.RecordPolicy(ctx, d, np(bonk.ID, assets.StatusActive, 2500, "", "bonk-1"))
	require.NoError(t, err)
	bp2, err := engine.Compute(ctx, d, acct.String(), buyingpower.PurposeWithdrawal)
	require.NoError(t, err)
	assert.Equal(t, "1248.64", bp2.PortfolioValue.String())
	assert.Equal(t, "1024.75", bp2.BuyingPower.String())
	assert.Equal(t, "850.00", bp2.Withdrawable.String())
	assert.Empty(t, bp2.Restrictions)
	assert.False(t, bp2.Blocked())
	assert.NotEqual(t, bp.PolicyVersion, bp2.PolicyVersion)

	// An in-flight withdrawal of 700 USDC reduces withdrawable to 300.
	uid := accounts.NewUserID()
	require.NoError(t, d.QueryRow(ctx, `SELECT owner_user_id FROM accounts WHERE id = $1`, acct).Scan(&uid))
	_, err = d.Exec(ctx, `INSERT INTO withdrawals (id, account_id, asset_id, quantity, destination_address, status, requested_by_user_id, idempotency_key)
		VALUES ($1,$2,$3,700000000,'addr','SUBMITTED',$4,$5)`, id.New[id.Any](), acct, usdc.ID, uid, "wd:"+id.New[id.Any]().String())
	require.NoError(t, err)
	bp3, err := engine.Compute(ctx, d, acct.String(), buyingpower.PurposeWithdrawal)
	require.NoError(t, err)
	assert.Equal(t, "300.00", bp3.Withdrawable.String())

	// Time moves past the prices' max age: SOL and BONK become STALE_PRICE;
	// USDC at face needs no price.
	clk.Advance(time.Minute)
	bp4, err := engine.Compute(ctx, d, acct.String(), buyingpower.PurposeTrade)
	require.NoError(t, err)
	assert.Equal(t, "850.00", bp4.BuyingPower.String(), "USDC 850 only")
	assert.Equal(t, "1000.00", bp4.PortfolioValue.String())
	require.Len(t, bp4.Restrictions, 2)
	assert.Equal(t, buyingpower.RestrictionStalePrice, bp4.Restrictions[0].Code)
	assert.Equal(t, buyingpower.RestrictionStalePrice, bp4.Restrictions[1].Code)
	assert.False(t, bp4.Blocked())

	// Freeze the account: no buying power for TRADE; DISPLAY reports, does not block.
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := accounts.NewRepository().Transition(ctx, tx, acct, accounts.StatusChange{To: accounts.StatusFrozen, ActorType: "OPERATOR", ActorID: "ops", Reason: "itest"}, clk.Now())
		return err
	}))
	bp5, err := engine.Compute(ctx, d, acct.String(), buyingpower.PurposeTrade)
	require.NoError(t, err)
	assert.True(t, bp5.BuyingPower.IsZero())
	assert.True(t, bp5.AvailableNow.IsZero())
	assert.Equal(t, "1000.00", bp5.PortfolioValue.String())
	assert.Equal(t, buyingpower.RestrictionAccountFrozen, bp5.Restrictions[0].Code)
	assert.True(t, bp5.Blocked())
	bp6, err := engine.Compute(ctx, d, acct.String(), buyingpower.PurposeDisplay)
	require.NoError(t, err)
	assert.False(t, bp6.Blocked())
	assert.Equal(t, bp5.BuyingPower, bp6.BuyingPower)

	// Kill switch and reconciliation block flow through the readers.
	kills.States = []buyingpower.KillSwitchState{{Kind: "GLOBAL_NEW_RISK_KILL", BlocksNewRisk: true}}
	recon.Records = []buyingpower.ReconciliationBlock{{RecordID: "r1", Kind: "WALLET_BALANCE"}}
	bp7, err := engine.Compute(ctx, d, acct.String(), buyingpower.PurposeTrade)
	require.NoError(t, err)
	assert.Equal(t, []buyingpower.RestrictionCode{
		buyingpower.RestrictionAccountFrozen, buyingpower.RestrictionKillSwitch, buyingpower.RestrictionReconciliationRequired,
		buyingpower.RestrictionStalePrice, buyingpower.RestrictionStalePrice,
	}, codes(bp7))

	// Unknown account → NOT_FOUND.
	_, err = engine.Compute(ctx, d, accounts.NewAccountID().String(), buyingpower.PurposeTrade)
	require.Error(t, err)
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
}

func TestIntegration_Compute_EmptyAccount(t *testing.T) {
	d := openTestDB(t)
	acct := seedAccount(t, d)
	usdc := seedAssetRow(t, d, "USDC", 6, true, assets.RiskSettlement)
	engine, err := buyingpower.NewEngine(buyingpower.Deps{
		Clock: clock.System(), Policies: valuation.NewPolicyStore(), Prices: valuation.NewPriceStore(nil),
		KillSwitches: &buyingpowertest.KillSwitches{}, Reconciliation: &buyingpowertest.ReconciliationBlocks{}, QuoteAssetID: usdc.ID,
	})
	require.NoError(t, err)
	bp, err := engine.Compute(t.Context(), d, acct.String(), buyingpower.PurposeDisplay)
	require.NoError(t, err)
	assert.True(t, bp.PortfolioValue.IsZero() && bp.BuyingPower.IsZero() && bp.Withdrawable.IsZero())
	assert.Empty(t, bp.UnderlyingBalances)
	assert.Empty(t, bp.Restrictions)
	assert.NotEmpty(t, bp.PolicyVersion)
}
