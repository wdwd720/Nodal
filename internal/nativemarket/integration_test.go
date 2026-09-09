//go:build integration

package nativemarket

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/nativeasset"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
	testMigrate    *db.DB
)

func TestMain(m *testing.M) { os.Exit(testMain(m)) }

func testMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "nativemarket integration: migrate up:", err)
		return 1
	}
	var err error
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "nm-itest", MaxConns: 30}); err != nil {
		fmt.Fprintln(os.Stderr, "nativemarket integration: open pool:", err)
		return 1
	}
	defer testDB.Close()
	if testMigrate, err = db.Open(ctx, db.Config{URL: testMigrateURL, AppName: "nm-itest-migrate", MaxConns: 4}); err != nil {
		fmt.Fprintln(os.Stderr, "nativemarket integration: open migrate pool:", err)
		return 1
	}
	defer testMigrate.Close()
	return m.Run()
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set")
	}
}

// activeCaps lets the ledger commit the cross-domain postings a trade needs.
// A market trade moves value between INTERNAL_CREDIT and INTERNAL_NATIVE_ASSET,
// which migration 00710 refuses unless NATIVE_MARKET_TRADING is ACTIVE.
type activeCaps map[valuedomain.CapabilityKey]bool

func (c activeCaps) ActiveConversionCapabilities(context.Context, db.Querier) (map[valuedomain.CapabilityKey]bool, error) {
	return c, nil
}

var (
	creditAssetOnce sync.Once
	creditAssetID   assets.AssetID
)

func creditAsset(t *testing.T) assets.AssetID {
	t.Helper()
	creditAssetOnce.Do(func() {
		ctx := context.Background()
		var existing assets.AssetID
		if err := testDB.QueryRow(ctx, `SELECT id FROM assets WHERE kind = 'CREDIT'`).Scan(&existing); err == nil {
			creditAssetID = existing
			return
		}
		created, err := assets.NewRepository().Create(ctx, testDB, assets.Asset{
			Chain: assets.InternalChain, Kind: assets.KindCredit,
			ValueDomain: valuedomain.InternalCredit,
			Symbol:      "CREDIT", Name: "Nodal Credit", Decimals: 6,
			RiskClass: assets.RiskUnsupported, Status: assets.StatusActive,
		})
		require.NoError(t, err)
		creditAssetID = created.ID
	})
	return creditAssetID
}

type fixture struct {
	t       *testing.T
	ctx     context.Context
	clk     *clock.Fake
	led     *ledger.Service
	credits *credit.Service
	assetSv *nativeasset.Service
	svc     *Service

	creditAsset assets.AssetID
	creator     accounts.AccountID
	trader      accounts.AccountID
	asset       nativeasset.Asset
	market      Market
}

func newAccount(t *testing.T) accounts.AccountID {
	t.Helper()
	repo := accounts.NewRepository()
	user, err := repo.CreateUser(context.Background(), testDB, "nm-itest", uuid.NewString(), nil)
	require.NoError(t, err)
	acct, err := repo.CreateAccount(context.Background(), testDB, user.ID, accounts.KindCustomer)
	require.NoError(t, err)
	return acct.ID
}

// newFixture builds a live market with a funded trader, which is the shape
// every test below starts from.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireEnv(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC))
	led := ledger.NewService(clk, "nm-itest")
	led.SetCapabilityResolver(activeCaps{valuedomain.CapNativeMarketTrading: true})
	credits := credit.NewService(led, clk)

	f := &fixture{
		t: t, ctx: ctx, clk: clk, led: led, credits: credits,
		assetSv:     nativeasset.NewService(clk, nil),
		creditAsset: creditAsset(t),
		creator:     newAccount(t),
		trader:      newAccount(t),
	}
	f.svc = NewService(led, credits, valuation.NewPriceStore(clk), audit.NewWriter(), instruments.NewRepository(),
		NewRiskGate(risk.NewStore(), clk), clk)
	seedGlobalRiskPolicy(t, clk.Now())

	suffix := uuid.NewString()[:6]
	require.NoError(t, testDB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		a, _, err := f.assetSv.CreateDraft(ctx, tx, nativeasset.CreateRequest{
			CreatorAccountID: f.creator,
			Name:             "Doggu " + suffix,
			Symbol:           "DG" + suffix[:4],
			Description:      "a test asset",
			Supply: nativeasset.SupplyModel{
				MaxSupply:         qs("1000000000000000"), // 1e9 tokens at 6dp
				CreatorAllocation: qs("100000000000000"),  // 10%
			},
		})
		if err != nil {
			return err
		}
		f.asset = a
		if _, err := f.assetSv.SetStatus(ctx, tx, a.AssetID, nativeasset.StatusPendingReview, "submitted"); err != nil {
			return err
		}
		if _, err := f.assetSv.SetModeration(ctx, tx, a.AssetID, nativeasset.ModerationApproved, "test fixture"); err != nil {
			return err
		}
		if _, err := f.assetSv.Activate(ctx, tx, a.AssetID, "test fixture"); err != nil {
			return err
		}
		m, err := f.svc.Create(ctx, tx, CreateRequest{
			AssetID:              a.AssetID,
			CreditAssetID:        f.creditAsset,
			CreatorID:            f.creator,
			PoolSupply:           a.Supply.PoolSupply(),
			CreatorAllocation:    a.Supply.CreatorAllocation,
			VirtualCreditReserve: qs("30000000000"), // 30,000 Credits
			Fees:                 Fees{PlatformBPS: 100, CreatorBPS: 50},
			IdempotencyKey:       "mint-" + suffix,
			EffectiveAt:          clk.Now(),
		})
		if err != nil {
			return err
		}
		m, err = f.svc.SetStatus(ctx, tx, m.ID, StatusActive, "test fixture")
		if err != nil {
			return err
		}
		f.market = m
		return nil
	}))

	f.fund(f.trader, 100_000_000_000) // 100,000 Credits
	return f
}

// seedGlobalRiskPolicy records the GLOBAL risk policy every trade below is
// evaluated against, once for the whole package.
//
// It records the COMPILED-IN DEFAULT rather than a permissive fixture policy,
// because a fixture that relaxes the limits it is meant to prove would be the
// test equivalent of turning the control off. What these tests therefore run
// against is what `go run ./scripts/riskpolicy` gives a fresh deployment.
//
// A test that wants a tighter limit records an ACCOUNT policy for its own
// account: Compose takes the minimum, so an ACCOUNT row can only tighten, and
// tightening one account does not touch any other test.
func seedGlobalRiskPolicy(t *testing.T, at time.Time) {
	t.Helper()
	globalRiskPolicy.Do(func() {
		ctx := security.WithPrincipal(context.Background(), security.Principal{
			SubjectID: "nm-itest", ActorType: security.ActorSystem, AuthTime: at,
		})
		// Already recorded by an earlier run against this database. Policy
		// versions are unique and the table is append-only, so recording it
		// again is a CONFLICT rather than a no-op.
		if _, _, err := risk.NewStore().EffectivePolicy(ctx, testDB, "", "", at); err == nil {
			return
		}
		require.NoError(t, testDB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
			func(ctx context.Context, tx pgx.Tx) error {
				_, err := risk.NewStore().RecordPolicy(ctx, tx, risk.PolicyRecord{
					Scope:       risk.ScopeGlobal,
					Version:     "nm-itest-global",
					Rules:       json.RawMessage(risk.DefaultGlobalPolicyJSON),
					EffectiveAt: at.Add(-time.Hour),
					ActorType:   security.ActorSystem,
					ActorID:     "nm-itest",
					Reason:      "the compiled-in default limits, as a fresh deployment gets them",
				})
				return err
			}))
	})
}

var globalRiskPolicy sync.Once

// tightenNativeLimits records an ACCOUNT policy that lowers the two native
// concentration limits for one account and nothing else.
//
// ACCOUNT policies can only tighten (Compose takes the minimum), which is what
// makes this safe to use in a package whose tests share a database: no other
// account's limits move.
func tightenNativeLimits(t *testing.T, account accounts.AccountID, market, creator money.BPS, at time.Time) {
	t.Helper()
	rules, err := json.Marshal(map[string]any{
		"max_native_market_concentration_bps": market,
		"max_creator_concentration_bps":       creator,
	})
	require.NoError(t, err)
	ctx := security.WithPrincipal(context.Background(), security.Principal{
		SubjectID: "nm-itest", ActorType: security.ActorSystem, AuthTime: at,
	})
	require.NoError(t, testDB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, rerr := risk.NewStore().RecordPolicy(ctx, tx, risk.PolicyRecord{
				Scope:       risk.ScopeAccount,
				ScopeID:     account.String(),
				Version:     "nm-itest-account-" + uuid.NewString(),
				Rules:       json.RawMessage(rules),
				EffectiveAt: at.Add(-time.Minute),
				ActorType:   security.ActorSystem,
				ActorID:     "nm-itest",
				Reason:      "a tighter native concentration limit for one test account",
			})
			return rerr
		}))
}

// fund issues settled Credits to an account.
func (f *fixture) fund(account accounts.AccountID, amount int64) {
	f.t.Helper()
	require.NoError(f.t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.credits.Issue(ctx, tx, credit.IssueRequest{
				AccountID: account, Quantity: q(amount),
				Origin: valuedomain.OriginPurchased, Finality: valuedomain.FinalitySettled,
				Reference:      credit.Reference{Type: "test_fund", ID: uuid.NewString()},
				IdempotencyKey: "fund-" + uuid.NewString(),
				Reason:         "test funding", EffectiveAt: f.clk.Now(),
			})
			return err
		}))
}

func (f *fixture) buy(account accounts.AccountID, credits int64, minOut money.Quantity) (ExecuteResult, error) {
	var res ExecuteResult
	err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = f.svc.Execute(ctx, tx, ExecuteRequest{
			MarketID: f.market.ID, AccountID: account, Side: Buy,
			Amount: q(credits), MinOutput: minOut,
			IdempotencyKey: "buy-" + uuid.NewString(), EffectiveAt: f.clk.Now(),
		})
		return err
	})
	return res, err
}

func (f *fixture) sell(account accounts.AccountID, units, minOut money.Quantity) (ExecuteResult, error) {
	var res ExecuteResult
	err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = f.svc.Execute(ctx, tx, ExecuteRequest{
			MarketID: f.market.ID, AccountID: account, Side: Sell,
			Amount: units, MinOutput: minOut,
			IdempotencyKey: "sell-" + uuid.NewString(), EffectiveAt: f.clk.Now(),
		})
		return err
	})
	return res, err
}

func (f *fixture) balance(account accounts.AccountID, asset assets.AssetID, code ledger.Code) money.Quantity {
	f.t.Helper()
	var raw string
	require.NoError(f.t, testDB.QueryRow(f.ctx,
		`SELECT coalesce((SELECT b.balance FROM ledger_accounts la
		    JOIN ledger_balances b ON b.ledger_account_id = la.id
		   WHERE la.owner_type='CUSTOMER' AND la.owner_id=$1 AND la.code=$2 AND la.asset_id=$3), 0)::text`,
		account, string(code), asset).Scan(&raw))
	return qs(raw)
}

func (f *fixture) platformBalance(asset assets.AssetID, code ledger.Code) money.Quantity {
	f.t.Helper()
	var raw string
	require.NoError(f.t, testDB.QueryRow(f.ctx,
		`SELECT coalesce((SELECT b.balance FROM ledger_accounts la
		    JOIN ledger_balances b ON b.ledger_account_id = la.id
		   WHERE la.owner_type='PLATFORM' AND la.code=$1 AND la.asset_id=$2), 0)::text`,
		string(code), asset).Scan(&raw))
	return qs(raw)
}

// ---------------------------------------------------------------------------

func TestIntegration_CreationMintsExactlyTheSupplyAndNoMore(t *testing.T) {
	f := newFixture(t)

	inventory := f.platformBalance(f.asset.AssetID, ledger.CodeMarketInventory)
	creatorHeld := f.balance(f.creator, f.asset.AssetID, ledger.CodeNativeAssetBalance)

	require.Equal(t, f.asset.Supply.PoolSupply().String(), inventory.String())
	require.Equal(t, f.asset.Supply.CreatorAllocation.String(), creatorHeld.String())
	require.Equal(t, f.asset.Supply.MaxSupply.String(), inventory.Add(creatorHeld).String(),
		"every unit that exists must be in the pool or in the creator's hands")

	st, err := f.svc.State(f.ctx, testDB, f.market.ID)
	require.NoError(t, err)
	require.Equal(t, "0", st.RealCreditReserve.String())
	require.Equal(t, f.asset.Supply.PoolSupply().String(), st.AssetReserve.String())
	require.EqualValues(t, 0, st.Version)
	require.True(t, st.HoldsInvariant(f.market.Curve))
}

func TestIntegration_BuyMovesValueExactlyWhereItShould(t *testing.T) {
	f := newFixture(t)

	// Deltas, not absolute balances. Every market quotes against the same
	// Credit asset, so the platform's MARKET_RESERVE and fee accounts are
	// shared by every market in the deployment -- and, in a test, by every
	// test that ran before this one. Asserting an absolute balance would pass
	// only on a virgin database, which is the kind of fixture that goes green
	// once and red forever after.
	traderBefore := f.balance(f.trader, f.creditAsset, ledger.CodeCreditBalance)
	creatorBefore := f.balance(f.creator, f.creditAsset, ledger.CodeCreditBalance)
	poolBefore := f.platformBalance(f.creditAsset, ledger.CodeMarketReserve)
	feeBefore := f.platformBalance(f.creditAsset, ledger.CodePlatformFeeReceivable)

	res, err := f.buy(f.trader, 1_000_000_000, money.Quantity{}) // 1,000 Credits
	require.NoError(t, err)

	fill := res.Fill
	require.Equal(t, "1000000000", fill.CreditsIn.String())
	require.Equal(t, "10000000", fill.PlatformFee.String(), "1%")
	require.Equal(t, "5000000", fill.CreatorFee.String(), "0.5%")
	require.Equal(t, "985000000", fill.CreditsToPool.String())

	// The trader paid exactly what they were charged.
	require.Equal(t, traderBefore.Sub(fill.CreditsIn).String(),
		f.balance(f.trader, f.creditAsset, ledger.CodeCreditBalance).String())

	// The pool, the platform and the creator received exactly the rest.
	require.Equal(t, poolBefore.Add(fill.CreditsToPool).String(),
		f.platformBalance(f.creditAsset, ledger.CodeMarketReserve).String())
	require.Equal(t, feeBefore.Add(fill.PlatformFee).String(),
		f.platformBalance(f.creditAsset, ledger.CodePlatformFeeReceivable).String())
	require.Equal(t, creatorBefore.Add(fill.CreatorFee).String(),
		f.balance(f.creator, f.creditAsset, ledger.CodeCreditBalance).String())

	// The trader holds exactly the units the pool gave up.
	require.Equal(t, fill.AssetsOut.String(),
		f.balance(f.trader, f.asset.AssetID, ledger.CodeNativeAssetBalance).String())

	st, err := f.svc.State(f.ctx, testDB, f.market.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, st.Version)
	require.True(t, st.HoldsInvariant(f.market.Curve))
}

// TestIntegration_CreatorFeeCarriesItsOwnProvenance is PART LXXX: a market
// creator's fee income is not ordinary creator revenue, because its source is
// speculative trading, and the two must be separable for payout policy.
func TestIntegration_CreatorFeeCarriesItsOwnProvenance(t *testing.T) {
	f := newFixture(t)
	_, err := f.buy(f.trader, 1_000_000_000, money.Quantity{})
	require.NoError(t, err)

	lots, err := f.credits.Lots(f.ctx, testDB, f.creator)
	require.NoError(t, err)
	require.Len(t, lots, 1)
	require.Equal(t, valuedomain.OriginMarketCreatorEarning, lots[0].Origin)
	require.Equal(t, valuedomain.FinalityReversible, lots[0].Finality,
		"value leaving the pool is funded by buyers whose own funding may still reverse")
	require.False(t, lots[0].Finality.PayoutEligible())
}

func TestIntegration_SellReturnsCreditsWithTradingProceedsProvenance(t *testing.T) {
	f := newFixture(t)
	buy, err := f.buy(f.trader, 5_000_000_000, money.Quantity{})
	require.NoError(t, err)

	sell, err := f.sell(f.trader, buy.Fill.AssetsOut, money.Quantity{})
	require.NoError(t, err)
	require.True(t, sell.Fill.CreditsOut.IsPositive())

	// Round-tripping is a loss, as it must be: fees plus rounding.
	require.Less(t, sell.Fill.CreditsOut.Cmp(buy.Fill.CreditsIn), 0,
		"an immediate round trip must not be profitable")

	// The trader holds no units and the pool is whole again.
	require.Equal(t, "0", f.balance(f.trader, f.asset.AssetID, ledger.CodeNativeAssetBalance).String())

	lots, err := f.credits.Lots(f.ctx, testDB, f.trader)
	require.NoError(t, err)
	var proceeds int
	for _, l := range lots {
		if l.Origin == valuedomain.OriginMarketTradingProceeds {
			proceeds++
		}
	}
	require.Equal(t, 1, proceeds, "sale proceeds must be their own provenance, not merged into the trader's purchased Credits")

	require.NoError(t, f.credits.VerifyProvenance(f.ctx, testDB, f.trader))
}

func TestIntegration_MinOutputRefusesATradeThatMoved(t *testing.T) {
	f := newFixture(t)
	// Ask for more units than 1,000 Credits could ever buy.
	_, err := f.buy(f.trader, 1_000_000_000, qs("999999999999999999"))
	require.Error(t, err)
	require.Equal(t, errs.CodeQuoteExpired, errs.CodeOf(err))
	require.Contains(t, err.Error(), "the market moved")

	// Nothing happened.
	require.Equal(t, "0", f.balance(f.trader, f.asset.AssetID, ledger.CodeNativeAssetBalance).String())
	st, err := f.svc.State(f.ctx, testDB, f.market.ID)
	require.NoError(t, err)
	require.EqualValues(t, 0, st.Version)
}

// TestIntegration_ExecutionIsIdempotent covers PART LXXII item 3. Because the
// market moves on every trade, a retried order allowed through twice would
// execute at a different price and take the user's Credits again.
func TestIntegration_ExecutionIsIdempotent(t *testing.T) {
	f := newFixture(t)
	key := "buy-" + uuid.NewString()
	run := func() (ExecuteResult, error) {
		var res ExecuteResult
		err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			res, err = f.svc.Execute(ctx, tx, ExecuteRequest{
				MarketID: f.market.ID, AccountID: f.trader, Side: Buy,
				Amount: q(2_000_000_000), IdempotencyKey: key, EffectiveAt: f.clk.Now(),
			})
			return err
		})
		return res, err
	}
	first, err := run()
	require.NoError(t, err)
	require.False(t, first.Existing)

	second, err := run()
	require.NoError(t, err)
	require.True(t, second.Existing)
	require.Equal(t, first.FillID, second.FillID)
	require.Equal(t, first.Fill.AssetsOut.String(), second.Fill.AssetsOut.String())

	st, err := f.svc.State(f.ctx, testDB, f.market.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, st.Version, "the retry must not have moved the market a second time")
	require.Equal(t, first.Fill.AssetsOut.String(),
		f.balance(f.trader, f.asset.AssetID, ledger.CodeNativeAssetBalance).String())
}

// TestIntegration_AStaleFillIsRefusedByTheDatabase is PART XIV's "execution
// revalidates market state", proved at the layer that cannot be bypassed: the
// fill is written by hand with a version that was correct a moment ago.
func TestIntegration_AStaleFillIsRefusedByTheDatabase(t *testing.T) {
	f := newFixture(t)
	res, err := f.buy(f.trader, 1_000_000_000, money.Quantity{})
	require.NoError(t, err)

	err = testMigrate.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO native_market_fills
			   (id, market_id, seq, account_id, side, credits_in, assets_out, credits_to_pool,
			    state_version_before, real_credit_reserve_after, asset_reserve_after,
			    journal_transaction_id, idempotency_key)
			 VALUES ($1,$2,2,$3,'BUY',1000,1000,1000,0,1000,1000,$4,$5)`,
			NewFillID(), f.market.ID, f.trader, mustJournalTx(t), "forged-"+uuid.NewString())
		return e
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "NATIVE_MARKET_STALE",
		"a fill priced against a superseded version must be refused")
	_ = res
}

// TestIntegration_TheDatabaseRefusesAFillThatBreaksTheInvariant writes a fill
// that would take Credits out of the pool without giving units back.
func TestIntegration_TheDatabaseRefusesAFillThatBreaksTheInvariant(t *testing.T) {
	f := newFixture(t)
	// 5,000 Credits, not 10,000: a 10,000-Credit order against this curve buys
	// 22.5% of the asset's entire supply, which the deployment's own risk
	// policy refuses (max_native_market_concentration_bps 2000). This test is
	// about a database trigger and needs a trade, not a large one.
	_, err := f.buy(f.trader, 5_000_000_000, money.Quantity{})
	require.NoError(t, err)

	st, err := f.svc.State(f.ctx, testDB, f.market.ID)
	require.NoError(t, err)

	err = testMigrate.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		// A "sell" that drains the entire reserve while returning one unit.
		_, e := tx.Exec(ctx,
			`INSERT INTO native_market_fills
			   (id, market_id, seq, account_id, side, assets_in, credits_out, credits_to_pool,
			    state_version_before, real_credit_reserve_after, asset_reserve_after,
			    journal_transaction_id, idempotency_key)
			 VALUES ($1,$2,$3,$4,'SELL',1,$5::numeric,$5::numeric,$6,0,$7::numeric,$8,$9)`,
			NewFillID(), f.market.ID, st.Version+1, f.trader,
			st.RealCreditReserve.String(), st.Version,
			st.AssetReserve.Add(q(1)).String(), mustJournalTx(t), "drain-"+uuid.NewString())
		return e
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "NATIVE_MARKET_INVARIANT",
		"the database must refuse a fill that puts the pool below its constant product")
}

func TestIntegration_CloseOnlyLetsHoldersOutAndNobodyIn(t *testing.T) {
	f := newFixture(t)
	buy, err := f.buy(f.trader, 3_000_000_000, money.Quantity{})
	require.NoError(t, err)

	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, e := f.svc.SetStatus(ctx, tx, f.market.ID, StatusCloseOnly, "winding down")
			return e
		}))
	f.market.Status = StatusCloseOnly

	_, err = f.buy(f.trader, 1_000_000_000, money.Quantity{})
	require.Error(t, err, "a close-only market must refuse new exposure")
	require.Equal(t, errs.CodeAssetRestricted, errs.CodeOf(err))

	_, err = f.sell(f.trader, buy.Fill.AssetsOut, money.Quantity{})
	require.NoError(t, err, "a close-only market must still let holders exit; trapping them is worse")
}

func TestIntegration_HaltedRefusesBothSides(t *testing.T) {
	f := newFixture(t)
	buy, err := f.buy(f.trader, 3_000_000_000, money.Quantity{})
	require.NoError(t, err)

	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, e := f.svc.SetStatus(ctx, tx, f.market.ID, StatusHalted, "investigation")
			return e
		}))
	f.market.Status = StatusHalted

	_, err = f.buy(f.trader, 1_000_000, money.Quantity{})
	require.Error(t, err)
	_, err = f.sell(f.trader, buy.Fill.AssetsOut, money.Quantity{})
	require.Error(t, err)
}

// TestIntegration_ACreatorCannotChangeEconomicsAfterLaunch is acceptance test
// MKT-003.
func TestIntegration_ACreatorCannotChangeEconomicsAfterLaunch(t *testing.T) {
	f := newFixture(t)

	for _, stmt := range []struct {
		name string
		sql  string
	}{
		{"raise max supply", `UPDATE native_assets SET max_supply = max_supply * 2 WHERE asset_id = $1`},
		{"raise creator allocation", `UPDATE native_assets SET creator_allocation = creator_allocation * 2 WHERE asset_id = $1`},
		{"change symbol", `UPDATE native_assets SET symbol = 'SNEAKY' WHERE asset_id = $1`},
		{"make it cashable", `UPDATE native_assets SET cashout_eligible = true WHERE asset_id = $1`},
		{"lower the age policy", `UPDATE native_assets SET minimum_age = 0 WHERE asset_id = $1`},
	} {
		t.Run(stmt.name, func(t *testing.T) {
			err := testMigrate.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
				func(ctx context.Context, tx pgx.Tx) error {
					_, e := tx.Exec(ctx, stmt.sql, f.asset.AssetID)
					return e
				})
			require.Error(t, err, "%s must be refused after activation", stmt.name)
			require.Contains(t, err.Error(), "NATIVE_ASSET_ECONOMICS_FROZEN")
		})
	}

	t.Run("raise the fee", func(t *testing.T) {
		err := testMigrate.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
			func(ctx context.Context, tx pgx.Tx) error {
				_, e := tx.Exec(ctx, `UPDATE native_markets SET creator_fee_bps = 900 WHERE id = $1`, f.market.ID)
				return e
			})
		require.Error(t, err, "a creator must not be able to raise the fee on holders after launch")
		require.Contains(t, err.Error(), "NATIVE_MARKET_CURVE_FROZEN")
	})
}

// TestIntegration_SupplyIsConservedAcrossManyTrades is MKT-001 and the supply
// half of PART LXXVIII.
func TestIntegration_SupplyIsConservedAcrossManyTrades(t *testing.T) {
	f := newFixture(t)
	traders := []accounts.AccountID{f.trader, newAccount(t), newAccount(t)}
	for _, tr := range traders[1:] {
		f.fund(tr, 50_000_000_000)
	}

	held := map[accounts.AccountID]money.Quantity{}
	for i := 0; i < 30; i++ {
		tr := traders[i%len(traders)]
		res, err := f.buy(tr, int64(100_000_000+(i*37_000_000)), money.Quantity{})
		require.NoError(t, err)
		held[tr] = held[tr].Add(res.Fill.AssetsOut)

		if i%3 == 2 && held[tr].IsPositive() {
			half, derr := held[tr].Div(q(2), money.RoundDown)
			require.NoError(t, derr)
			if half.IsPositive() {
				_, err := f.sell(tr, half, money.Quantity{})
				require.NoError(t, err)
				held[tr] = held[tr].Sub(half)
			}
		}

		st, err := f.svc.State(f.ctx, testDB, f.market.ID)
		require.NoError(t, err)
		require.True(t, st.HoldsInvariant(f.market.Curve), "invariant broke at trade %d", i)
		require.False(t, st.RealCreditReserve.IsNegative())
	}

	// Every unit ever minted is either in the pool or in somebody's hands.
	st, err := f.svc.State(f.ctx, testDB, f.market.ID)
	require.NoError(t, err)
	total := st.AssetReserve
	holders, err := f.svc.Holders(f.ctx, testDB, f.asset.AssetID, 100)
	require.NoError(t, err)
	for _, h := range holders {
		total = total.Add(h.Quantity)
	}
	require.Equal(t, f.asset.Supply.MaxSupply.String(), total.String(),
		"units were created or destroyed across 30 trades")

	// And the reserve reconciles. Every market shares one platform ledger
	// account for Credits, so the check is the control-account one: the sum of
	// every market's recorded reserve equals the ledger balance.
	require.NoError(t, f.svc.VerifyReserves(f.ctx, testDB, f.creditAsset))
}

// TestIntegration_ConcurrentBuyersSerialiseWithoutBreakingAnything is the
// concurrency property of PART LXXII item 2.
func TestIntegration_ConcurrentBuyersSerialiseWithoutBreakingAnything(t *testing.T) {
	f := newFixture(t)
	// A hundred, because PART LXXII item 2 asks for a hundred. It ran at 25
	// for a while and the coverage table said "100 concurrent native-asset
	// buys" next to it, which is the sort of small gap between a claim and a
	// test that F-18 was about.
	const workers = 100
	traders := make([]accounts.AccountID, workers)
	for i := range traders {
		traders[i] = newAccount(t)
		// 5,000 Credits each for a 1,000-Credit order. The buy is 20% of the
		// trader's Credit position, inside the deployment's creator
		// concentration limit of 30%; funding 2,000 made every one of these
		// buyers 50% concentrated in one creator and the risk kernel refused
		// all hundred of them.
		f.fund(traders[i], 5_000_000_000)
	}

	var ok, failed, retried atomic.Int64
	// firstErr keeps one failure to report. "4 failed" with no reason is a
	// result nobody can act on, and this test discarded every error until a
	// run failed and there was nothing to look at.
	var (
		errMu    sync.Mutex
		firstErr error
	)

	// A hundred buyers contend for one market row, and the deployment's
	// lock_timeout is 5 seconds. On a loaded machine some of them wait longer
	// than that and are refused with CONFLICT and SQLSTATE 55P03 -- which is
	// the correct outcome of a defended system under contention, not a defect,
	// and F-31 is the finding that recorded it as unexplained. It is explained
	// now: observed in a full 50-package sweep as "13 failed, first error:
	// CONFLICT: ledger operation timed out waiting for the database: ERROR:
	// canceling statement due to lock timeout (SQLSTATE 55P03)", and passing
	// three times out of three when the package runs alone.
	//
	// So the buyer retries, because that is what a client does with a CONFLICT.
	// The claim this test makes is unchanged and still strong: a hundred
	// concurrent buyers all get their units, the version moves exactly once per
	// trade, the curve invariant holds and supply reconciles. What is no longer
	// asserted is that all hundred succeed inside one five-second lock wait on
	// whatever machine happens to be running, which was never a property of the
	// system.
	//
	// The retries are bounded and counted: a regression that turned contention
	// into livelock would exhaust them and fail here, rather than hiding behind
	// an unbounded loop.
	const maxAttempts = 6
	buyWithRetry := func(trader accounts.AccountID) error {
		var last error
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			_, err := f.buy(trader, 1_000_000_000, money.Quantity{})
			if err == nil {
				return nil
			}
			last = err
			if errs.CodeOf(err) != errs.CodeConflict {
				return err // anything but contention is a real failure
			}
			retried.Add(1)
		}
		return last
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			if err := buyWithRetry(traders[idx]); err != nil {
				failed.Add(1)
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
				return
			}
			ok.Add(1)
		}(i)
	}
	close(start)
	wg.Wait()

	require.EqualValues(t, workers, ok.Load(),
		"every funded buyer should succeed; %d failed after %d attempts each, first error: %v",
		failed.Load(), maxAttempts, firstErr)
	t.Logf("contention retries: %d across %d buyers", retried.Load(), workers)

	st, err := f.svc.State(f.ctx, testDB, f.market.ID)
	require.NoError(t, err)
	require.EqualValues(t, workers, st.Version, "each trade must move the version exactly once")
	require.True(t, st.HoldsInvariant(f.market.Curve))

	total := st.AssetReserve
	holders, err := f.svc.Holders(f.ctx, testDB, f.asset.AssetID, 200)
	require.NoError(t, err)
	for _, h := range holders {
		total = total.Add(h.Quantity)
	}
	require.Equal(t, f.asset.Supply.MaxSupply.String(), total.String())
}

func TestIntegration_QuotesAreRecordedAndExpire(t *testing.T) {
	f := newFixture(t)
	var qt Quote
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			qt, err = f.svc.Quote(ctx, tx, QuoteRequest{
				MarketID: f.market.ID, AccountID: f.trader, Side: Buy, Amount: q(1_000_000_000),
			})
			return err
		}))
	require.True(t, qt.ExpectedOutput.IsPositive())
	require.EqualValues(t, 0, qt.StateVersion)
	require.False(t, qt.Expired(f.clk.Now()))
	require.True(t, qt.Expired(f.clk.Now().Add(QuoteTTL)))

	stored, err := f.svc.StoredQuote(f.ctx, testDB, qt.ID)
	require.NoError(t, err)
	require.Equal(t, qt.ExpectedOutput.String(), stored.ExpectedOutput.String())
	require.Equal(t, qt.SlippageBPS, stored.SlippageBPS)

	// A quote is immutable evidence.
	err = testMigrate.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `UPDATE native_market_quotes SET expected_output = 1 WHERE id = $1`, qt.ID)
			return e
		})
	require.Error(t, err)
}

// TestIntegration_ADeliveredQuoteDoesNotSetThePrice: the market moves between
// quote and execution, and the trade prices against the new state, not the old.
func TestIntegration_ADeliveredQuoteDoesNotSetThePrice(t *testing.T) {
	f := newFixture(t)
	var qt Quote
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			qt, err = f.svc.Quote(ctx, tx, QuoteRequest{
				MarketID: f.market.ID, AccountID: f.trader, Side: Buy, Amount: q(1_000_000_000),
			})
			return err
		}))

	// Somebody else buys first, moving the price up.
	other := newAccount(t)
	// Enough to move the price hard while staying inside both concentration
	// limits: 8,000 Credits buys 18.9% of supply, and is 20% of this account's
	// Credits. The original 15,000 of 20,000 was over both.
	f.fund(other, 40_000_000_000)
	_, err := f.buy(other, 8_000_000_000, money.Quantity{})
	require.NoError(t, err)

	// The original trader executes with the stale quote attached.
	var res ExecuteResult
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var e error
			res, e = f.svc.Execute(ctx, tx, ExecuteRequest{
				MarketID: f.market.ID, AccountID: f.trader, Side: Buy,
				Amount: q(1_000_000_000), QuoteID: &qt.ID,
				IdempotencyKey: "stale-" + uuid.NewString(), EffectiveAt: f.clk.Now(),
			})
			return e
		}))
	require.Less(t, res.Fill.AssetsOut.Cmp(qt.ExpectedOutput), 0,
		"the trade must price against current state, so it receives less than the stale quote promised")
}

func TestIntegration_SurveillanceRaisesAlertsWithoutBlocking(t *testing.T) {
	f := newFixture(t)

	// A creator trading their own market. Funded 10,000 for a 2,000 order so
	// the trade is inside the creator concentration limit -- self-dealing is
	// what this test is about, and a trade refused by a different control
	// would prove nothing about surveillance.
	f.fund(f.creator, 10_000_000_000)
	buy, err := f.buy(f.creator, 2_000_000_000, money.Quantity{})
	require.NoError(t, err, "creator self-dealing is visible, not forbidden")
	requireAlert(t, buy.Alerts, AlertCreatorSelfDealing)

	// An immediate round trip by the same account.
	sell, err := f.sell(f.creator, buy.Fill.AssetsOut, money.Quantity{})
	require.NoError(t, err)
	requireAlert(t, sell.Alerts, AlertRapidRoundTrip)

	stored, err := f.svc.Alerts(f.ctx, testDB, f.market.ID, 50)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(stored), 2)
	for _, a := range stored {
		require.NotNil(t, a.FillID, "every alert must point at the trade that produced it")
		require.NotEmpty(t, a.Detail["reason"], "an alert with no stated reason is not actionable")
	}
}

func requireAlert(t *testing.T, alerts []Alert, want AlertKind) {
	t.Helper()
	for _, a := range alerts {
		if a.Kind == want {
			return
		}
	}
	t.Fatalf("expected a %s alert, got %v", want, alerts)
}

func TestIntegration_ATradeIsADeclaredCrossDomainConversion(t *testing.T) {
	f := newFixture(t)
	buy, err := f.buy(f.trader, 1_000_000_000, money.Quantity{})
	require.NoError(t, err)

	// Scoped to THIS fill's journal transaction. The query used to take the
	// most recent NATIVE_TRADE row in the database, which is only this trade
	// while no other test has traded later on a faster clock.
	var from, to string
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT coalesce(jt.conversion_from,''), coalesce(jt.conversion_to,'')
		   FROM journal_transactions jt
		   JOIN native_market_fills fl ON fl.journal_transaction_id = jt.id
		  WHERE fl.id = $1 AND jt.kind = 'NATIVE_TRADE'`, buy.FillID).Scan(&from, &to))
	require.Equal(t, string(valuedomain.InternalCredit), from)
	require.Equal(t, string(valuedomain.InternalNativeAsset), to)
}

// TestIntegration_WithoutTheCapabilityNoTradeCommits proves the gate has teeth:
// the same trade that works above is refused when NATIVE_MARKET_TRADING is off.
func TestIntegration_WithoutTheCapabilityNoTradeCommits(t *testing.T) {
	f := newFixture(t)
	f.led.SetCapabilityResolver(nil)
	defer f.led.SetCapabilityResolver(activeCaps{valuedomain.CapNativeMarketTrading: true})

	_, err := f.buy(f.trader, 1_000_000_000, money.Quantity{})
	require.Error(t, err)
	require.Equal(t, errs.CodeCapabilityNotApproved, errs.CodeOf(err))

	st, err := f.svc.State(f.ctx, testDB, f.market.ID)
	require.NoError(t, err)
	require.EqualValues(t, 0, st.Version, "a refused trade must not have moved the market")
}

// mustJournalTx returns the id of some existing journal transaction, so a
// hand-written fill can satisfy its foreign key and be refused for the reason
// under test rather than for a missing reference.
func mustJournalTx(t *testing.T) string {
	t.Helper()
	var id string
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT id::text FROM journal_transactions ORDER BY posted_at DESC LIMIT 1`).Scan(&id))
	return id
}

// secondAccountFor creates another account owned by the same user as an
// existing one, which is the only "same controlled actor" this system can
// prove.
func secondAccountFor(t *testing.T, existing accounts.AccountID) accounts.AccountID {
	t.Helper()
	var owner accounts.UserID
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT owner_user_id FROM accounts WHERE id = $1`, existing).Scan(&owner))
	acct, err := accounts.NewRepository().CreateAccount(context.Background(), testDB, owner, accounts.KindCustomer)
	require.NoError(t, err)
	return acct.ID
}

// TestIntegration_WashTradingAcrossTwoAccountsOfOneUserIsDetected is PART LXXII
// item 14: two accounts under the same controlled actor wash-trading, where
// detectable.
//
// The qualifier matters. Shared ownership is a recorded fact and is the only
// form of common control this system can prove; coordination between two
// people who merely know each other is invisible here, and the alert says so
// rather than implying a reach it does not have.
//
// The alert is CRITICAL and still does not block. Blocking on a surveillance
// heuristic is a denial-of-service vector against creators, and the operator
// controls that DO stop a market are the ones somebody chose.
func TestIntegration_WashTradingAcrossTwoAccountsOfOneUserIsDetected(t *testing.T) {
	f := newFixture(t)
	other := secondAccountFor(t, f.trader)
	f.fund(other, 50_000_000_000)

	// The second account acquires a position first. Wash trading needs both
	// sides to be possible, and in an AMM a seller must already hold units.
	stock, err := f.buy(other, 2_000_000_000, money.Quantity{})
	require.NoError(t, err)
	requireNoAlert(t, stock.Alerts, AlertWashTrade)

	// The first account buys, which is one side of the wash.
	buy, err := f.buy(f.trader, 2_000_000_000, money.Quantity{})
	require.NoError(t, err)
	requireNoAlert(t, buy.Alerts, AlertWashTrade)

	// The OTHER account, same owner, sells inside the window. One actor has
	// now taken both sides of the same market, manufacturing volume and a
	// price without transferring risk to anybody outside itself.
	sell, err := f.sell(other, stock.Fill.AssetsOut, money.Quantity{})
	require.NoError(t, err, "the trade is not blocked; it is recorded and flagged")
	requireAlert(t, sell.Alerts, AlertWashTrade)

	stored, err := f.svc.Alerts(f.ctx, testDB, f.market.ID, 50)
	require.NoError(t, err)
	var wash *Alert
	for i := range stored {
		if stored[i].Kind == AlertWashTrade {
			wash = &stored[i]
			break
		}
	}
	require.NotNil(t, wash, "the alert must be persisted, not only returned")
	require.Equal(t, SeverityCritical, wash.Severity)
	require.Equal(t, f.trader.String(), wash.Detail["other_account_id"],
		"the alert must name the other account so a reviewer can check the claim")
	require.NotEmpty(t, wash.Detail["shared_owner_user_id"])
	require.Contains(t, wash.Detail["detection_basis"], "shared account ownership")
	require.NotNil(t, wash.FillID)
}

// TestIntegration_TwoUnrelatedAccountsTradingIsNotWashTrading is the control
// that makes the test above mean something. A CRITICAL accusation that a
// coincidence can trigger is worse than no detector: it teaches reviewers to
// dismiss the alert.
func TestIntegration_TwoUnrelatedAccountsTradingIsNotWashTrading(t *testing.T) {
	f := newFixture(t)
	stranger := newAccount(t) // a different user entirely
	f.fund(stranger, 50_000_000_000)

	// The stranger acquires a position, the fixture's trader buys, and the
	// stranger sells inside the window — the same SHAPE as the wash above,
	// with the one difference that matters: different owners.
	stock, err := f.buy(stranger, 2_000_000_000, money.Quantity{})
	require.NoError(t, err)

	buy, err := f.buy(f.trader, 2_000_000_000, money.Quantity{})
	require.NoError(t, err)
	requireNoAlert(t, buy.Alerts, AlertWashTrade)

	sell, err := f.sell(stranger, stock.Fill.AssetsOut, money.Quantity{})
	require.NoError(t, err)
	requireNoAlert(t, sell.Alerts, AlertWashTrade)

	// The stranger's own buy-then-sell is still a rapid round trip, which is a
	// different and lesser finding. Both detectors firing on the same trade
	// would mean the wash detector had learned nothing the round-trip detector
	// did not already know.
	requireAlert(t, sell.Alerts, AlertRapidRoundTrip)
}

func requireNoAlert(t *testing.T, alerts []Alert, unwanted AlertKind) {
	t.Helper()
	for _, a := range alerts {
		if a.Kind == unwanted {
			t.Fatalf("did not expect a %s alert, got %v", unwanted, alerts)
		}
	}
}
