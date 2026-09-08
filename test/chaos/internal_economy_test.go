//go:build integration && chaos

package chaos

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/commerce"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/nativeasset"
	"github.com/nodal/controlplane/internal/nativemarket"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Fault injection against the Nodal-native economy (gola.md Stage 20).
//
// The internal economy has a property the external rails do not: every one of
// its financial acts is a SINGLE database transaction. A native-market trade
// posts six ledger entries, moves the curve's state version, records a fill
// and writes provenance; an internal purchase posts three entries, consumes
// the buyer's lots and records the seller's earning. There is no submit, no
// provider, and therefore no legitimate half-done state.
//
// That makes the invariant sharper than "recover correctly": after a fault
// there must be NOTHING. Not a compensating entry, not a reconciliation task —
// nothing at all, because the transaction either committed or did not.
//
// Each test below asserts three things after the fault:
//
//  1. no partial financial effect — no order, no fill, no lot, no posting;
//  2. no drift — VerifyProvenance and VerifyReserves still hold, so the
//     provenance record and the ledger have not come apart;
//  3. the system still works — the same command, retried, succeeds.
//
// (3) matters as much as the others. A guard that survives a fault by being
// permanently broken afterwards has not survived it.

// activeCapsChaos reports a fixed capability set as ACTIVE for the ledger's
// conversion check and for the commerce service's own gate.
type activeCapsChaos map[valuedomain.CapabilityKey]bool

func (c activeCapsChaos) ActiveCapabilities(context.Context, db.Querier) (map[valuedomain.CapabilityKey]bool, error) {
	return c, nil
}

func (c activeCapsChaos) ActiveConversionCapabilities(context.Context, db.Querier) (map[valuedomain.CapabilityKey]bool, error) {
	return c, nil
}

// internalWorld is a live internal economy: a Credit asset, a funded buyer, a
// registered seller with a published product, and a live native market.
type internalWorld struct {
	t   *testing.T
	ctx context.Context
	clk *clock.Fake

	ledger   *ledger.Service
	credits  *credit.Service
	commerce *commerce.Service
	assetSvc *nativeasset.Service
	markets  *nativemarket.Service

	creditAsset assets.AssetID
	buyer       accounts.AccountID
	seller      accounts.AccountID
	product     commerce.Product
	asset       nativeasset.Asset
	market      nativemarket.Market
}

func qty(n int64) money.Quantity { return money.QuantityFromInt64(n) }

func qtyString(t *testing.T, s string) money.Quantity {
	t.Helper()
	q, err := money.ParseQuantity(s)
	require.NoError(t, err)
	return q
}

// creditAssetOnce provisions THE Credit asset. Migration 00711 permits exactly
// one per database, and the chaos suite runs many tests against one.
var (
	creditAssetOnce sync.Once
	creditAssetID   assets.AssetID
)

func chaosCreditAsset(t *testing.T) assets.AssetID {
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
	require.False(t, creditAssetID.IsZero())
	return creditAssetID
}

// seedGlobalRiskPolicy records the GLOBAL risk policy the internal economy is
// evaluated against, once for the whole package. It is the compiled-in default
// -- what `go run ./scripts/riskpolicy` gives a fresh deployment -- so a chaos
// run exercises the limits a deployment actually has.
func seedGlobalRiskPolicy(t *testing.T, at time.Time) {
	t.Helper()
	globalRiskPolicy.Do(func() {
		ctx := security.WithPrincipal(context.Background(), security.Principal{
			SubjectID: "chaos", ActorType: security.ActorSystem, AuthTime: at,
		})
		if _, _, err := risk.NewStore().EffectivePolicy(ctx, testDB, "", "", at); err == nil {
			return // an earlier run against this database already recorded it
		}
		require.NoError(t, testDB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
			func(ctx context.Context, tx pgx.Tx) error {
				_, err := risk.NewStore().RecordPolicy(ctx, tx, risk.PolicyRecord{
					Scope:       risk.ScopeGlobal,
					Version:     "chaos-global",
					Rules:       json.RawMessage(risk.DefaultGlobalPolicyJSON),
					EffectiveAt: at.Add(-time.Hour),
					ActorType:   security.ActorSystem,
					ActorID:     "chaos",
					Reason:      "the compiled-in default limits, as a fresh deployment gets them",
				})
				return err
			}))
	})
}

var globalRiskPolicy sync.Once

func newInternalWorld(t *testing.T, fundBuyer int64) *internalWorld {
	t.Helper()
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC))
	caps := activeCapsChaos{
		valuedomain.CapNativeMarketTrading: true,
		commerce.CapMarketplace:            true,
	}
	led := ledger.NewService(clk, "chaos-internal")
	led.SetCapabilityResolver(caps)
	credits := credit.NewService(led, clk)
	com := commerce.NewService(led, credits, audit.NewWriter(), clk)
	com.SetCapabilityResolver(caps)

	_, buyer := newAccount(t)
	_, seller := newAccount(t)
	seedGlobalRiskPolicy(t, clk.Now())

	w := &internalWorld{
		t: t, ctx: ctx, clk: clk,
		ledger: led, credits: credits, commerce: com,
		assetSvc: nativeasset.NewService(clk, nil),
		markets: nativemarket.NewService(led, credits, valuation.NewPriceStore(clk), audit.NewWriter(),
			instruments.NewRepository(), nativemarket.NewRiskGate(risk.NewStore(), clk), clk),
		creditAsset: chaosCreditAsset(t), buyer: buyer, seller: seller,
	}

	w.fund(buyer, fundBuyer)

	token := chaosToken()
	require.NoError(t, testDB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			if _, err := com.RegisterSeller(ctx, tx, commerce.Seller{
				AccountID: seller, DisplayName: "Chaos creator",
			}); err != nil {
				return err
			}
			p, err := com.CreateProduct(ctx, tx, commerce.Product{
				SellerAccountID: seller, Kind: commerce.KindData,
				Title: "Chaos dataset " + token, Price: qty(1_000), PlatformFeeBPS: 1_000,
			})
			if err != nil {
				return err
			}
			w.product, err = com.Publish(ctx, tx, p.ID)
			return err
		}))

	require.NoError(t, testDB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			a, _, err := w.assetSvc.CreateDraft(ctx, tx, nativeasset.CreateRequest{
				CreatorAccountID: seller,
				Name:             "Chaos asset " + token,
				Symbol:           "CH" + token[:4],
				Description:      "for fault injection",
				Supply: nativeasset.SupplyModel{
					MaxSupply:         qtyString(t, "1000000000000000"),
					CreatorAllocation: qtyString(t, "100000000000000"),
				},
			})
			if err != nil {
				return err
			}
			w.asset = a
			if _, err := w.assetSvc.SetStatus(ctx, tx, a.AssetID, nativeasset.StatusPendingReview, "chaos"); err != nil {
				return err
			}
			if _, err := w.assetSvc.SetModeration(ctx, tx, a.AssetID, nativeasset.ModerationApproved, "chaos"); err != nil {
				return err
			}
			if _, err := w.assetSvc.Activate(ctx, tx, a.AssetID, "chaos"); err != nil {
				return err
			}
			m, err := w.markets.Create(ctx, tx, nativemarket.CreateRequest{
				AssetID: a.AssetID, CreditAssetID: w.creditAsset, CreatorID: seller,
				PoolSupply: a.Supply.PoolSupply(), CreatorAllocation: a.Supply.CreatorAllocation,
				VirtualCreditReserve: qtyString(t, "30000000000"),
				Fees:                 nativemarket.Fees{PlatformBPS: 100, CreatorBPS: 50},
				IdempotencyKey:       "chaos-mint-" + token,
				EffectiveAt:          clk.Now(),
			})
			if err != nil {
				return err
			}
			w.market, err = w.markets.SetStatus(ctx, tx, m.ID, nativemarket.StatusActive, "chaos")
			return err
		}))

	return w
}

func (w *internalWorld) fund(account accounts.AccountID, amount int64) {
	w.t.Helper()
	require.NoError(w.t, testDB.InTx(w.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := w.credits.Issue(ctx, tx, credit.IssueRequest{
				AccountID: account, Quantity: qty(amount),
				Origin: valuedomain.OriginPurchased, Finality: valuedomain.FinalitySettled,
				Reference:      credit.Reference{Type: "chaos_fund", ID: uuid.NewString()},
				IdempotencyKey: "chaos-fund-" + uuid.NewString(),
				Reason:         "chaos fixture", EffectiveAt: w.clk.Now(),
			})
			return err
		}))
}

func (w *internalWorld) creditBalance(account accounts.AccountID) string {
	w.t.Helper()
	var bal string
	err := testDB.QueryRow(w.ctx,
		`SELECT coalesce(b.balance, 0)::text FROM ledger_accounts la
		   LEFT JOIN ledger_balances b ON b.ledger_account_id = la.id
		  WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = $1
		    AND la.code = 'CREDIT_BALANCE' AND la.asset_id = $2`,
		account, w.creditAsset).Scan(&bal)
	if err != nil {
		return "0"
	}
	return bal
}

func (w *internalWorld) marketVersion() int64 {
	w.t.Helper()
	st, err := w.markets.State(w.ctx, testDB, w.market.ID)
	require.NoError(w.t, err)
	return st.Version
}

// checkNoDrift is the invariant that matters most after a fault: provenance
// and the ledger still agree, the market's control account still reconciles
// against its subsidiary state, and commerce earnings still tie to orders.
func (w *internalWorld) checkNoDrift() {
	w.t.Helper()
	require.NoError(w.t, w.credits.VerifyProvenance(w.ctx, testDB, w.buyer),
		"buyer provenance and ledger diverged after the fault")
	require.NoError(w.t, w.credits.VerifyProvenance(w.ctx, testDB, w.seller),
		"seller provenance and ledger diverged after the fault")
	require.NoError(w.t, w.commerce.VerifyEarnings(w.ctx, testDB, w.seller),
		"commerce earnings no longer reconcile against the orders that produced them")
	require.NoError(w.t, w.markets.VerifyReserves(w.ctx, testDB, w.creditAsset),
		"the market reserve control account no longer reconciles")
}

// ---------------------------------------------------------------------------

// TestChaos_APurchaseDiesMidTransaction kills the exact backend running an
// internal purchase after every write has been issued and before COMMIT.
//
// A purchase is one transaction: three ledger entries, the buyer's lots
// consumed, the seller's earning recorded, and the order row written. There is
// no provider and nothing to reconcile with, so the only correct outcome of a
// fault is that none of it happened.
//
// Negative control: CP_CHAOS_BREAK=commerce_partial_write consumes the buyer's
// lots in a SEPARATE, already-committed transaction, so the fault leaves
// provenance that the ledger does not back — exactly the drift checkNoDrift
// exists to catch.
func TestChaos_APurchaseDiesMidTransaction(t *testing.T) {
	requireEnv(t)
	requireHealthyStack(t, PostgresContainer())

	w := newInternalWorld(t, 10_000)
	separate := chaosBreak(t, "commerce_partial_write")

	buyerBefore := w.creditBalance(w.buyer)
	sellerBefore := w.creditBalance(w.seller)
	key := "chaos-buy-" + uuid.NewString()

	if separate {
		// NEGATIVE CONTROL: move the buyer's Credits on the ledger and do NOT
		// consume the lots behind them. The ledger now says the buyer paid and
		// provenance still claims every unit, which is the drift
		// VerifyProvenance exists to catch.
		//
		// Note what is NOT usable as a control: consuming lots with no posting.
		// Migration 00711 (SQLSTATE CR004) refuses a lot event whose journal
		// transaction never touched the account, so that drift is
		// unrepresentable and a "control" built on it would pass while proving
		// nothing. It was tried first and did exactly that.
		require.NoError(t, testDB.InTx(w.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
			func(ctx context.Context, tx pgx.Tx) error {
				_, err := w.ledger.Post(ctx, tx, ledger.Posting{
					Kind:           ledger.KindInternalPurchase,
					IdempotencyKey: "chaos-orphan-" + key,
					Reference:      ledger.FinancialEventReference{Type: "chaos", ID: key},
					EffectiveAt:    w.clk.Now(),
					Entries: []ledger.Entry{
						{
							Account: ledger.CustomerAccount(w.buyer, ledger.CodeCreditBalance, w.creditAsset),
							Side:    ledger.Credit, Quantity: qty(1_000),
						},
						{
							Account: ledger.PlatformAccount(ledger.CodePlatformFeeReceivable, w.creditAsset),
							Side:    ledger.Debit, Quantity: qty(1_000),
						},
					},
				})
				return err
			}))
	}

	var killed int64
	err := testDB.InTx(w.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var pid int
			if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				return err
			}
			if _, err := w.commerce.Purchase(ctx, tx, commerce.PurchaseRequest{
				ProductID: w.product.ID, BuyerAccountID: w.buyer, ExpectedPrice: qty(1_000),
				IdempotencyKey: key, EffectiveAt: w.clk.Now(),
			}); err != nil {
				return err
			}
			n, kerr := terminateBackend(w.ctx, adminDSN, pid)
			if kerr != nil {
				return kerr
			}
			killed += n
			// One more statement so the client observes the death here rather
			// than at COMMIT.
			var one int
			return tx.QueryRow(ctx, "SELECT 1").Scan(&one)
		})
	require.Error(t, err, "the transaction must not appear to succeed after its backend was terminated")
	require.EqualValues(t, 1, killed)
	waitDB(t, 30*time.Second)

	// 1. Nothing partial.
	var orders int
	require.NoError(t, testDB.QueryRow(w.ctx,
		`SELECT count(*) FROM internal_commerce_orders WHERE idempotency_key = $1`, key).Scan(&orders))
	require.Equal(t, 0, orders, "a killed purchase must leave no order")
	require.Equal(t, 0, countPostingsByKey(t, "commerce:"+key),
		"a killed purchase must leave no posting")

	if !separate {
		require.Equal(t, buyerBefore, w.creditBalance(w.buyer), "the buyer paid nothing")
		require.Equal(t, sellerBefore, w.creditBalance(w.seller), "the seller received nothing")
	}

	// 2. No drift.
	w.checkNoDrift()

	// 3. Still works: the same purchase, retried, succeeds.
	var order commerce.Order
	require.NoError(t, testDB.InTx(w.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var perr error
			order, perr = w.commerce.Purchase(ctx, tx, commerce.PurchaseRequest{
				ProductID: w.product.ID, BuyerAccountID: w.buyer, ExpectedPrice: qty(1_000),
				IdempotencyKey: key, EffectiveAt: w.clk.Now(),
			})
			return perr
		}), "the same purchase must succeed after the fault")
	require.Equal(t, "900", order.SellerProceeds.String())
	w.checkNoDrift()
}

// TestChaos_ATradeDiesMidTransaction does the same to a native-market trade.
//
// A trade is the most intricate single transaction in the system: six ledger
// entries across two assets, a declared cross-domain conversion, an optimistic
// bump of the curve's state version, a fill row and two provenance lots. The
// database refuses a fill that breaks the constant product and refuses one
// priced against a stale version, so a half-applied trade is not merely
// undesirable — it is unrepresentable. This proves the fault produces nothing
// rather than something the triggers happened to catch.
func TestChaos_ATradeDiesMidTransaction(t *testing.T) {
	requireEnv(t)
	requireHealthyStack(t, PostgresContainer())

	w := newInternalWorld(t, 5_000_000_000)
	separate := chaosBreak(t, "native_trade_partial_write")
	versionBefore := w.marketVersion()
	balanceBefore := w.creditBalance(w.buyer)
	key := "chaos-trade-" + uuid.NewString()

	if separate {
		// NEGATIVE CONTROL: the trader's Credits leave their balance in a
		// separate, already-committed transaction, so the fault leaves them
		// having paid for a trade that produced no fill and moved no curve.
		require.NoError(t, testDB.InTx(w.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
			func(ctx context.Context, tx pgx.Tx) error {
				_, err := w.ledger.Post(ctx, tx, ledger.Posting{
					Kind:           ledger.KindInternalPurchase,
					IdempotencyKey: "chaos-orphan-trade-" + key,
					Reference:      ledger.FinancialEventReference{Type: "chaos", ID: key},
					EffectiveAt:    w.clk.Now(),
					Entries: []ledger.Entry{
						{
							Account: ledger.CustomerAccount(w.buyer, ledger.CodeCreditBalance, w.creditAsset),
							Side:    ledger.Credit, Quantity: qty(1_000_000_000),
						},
						{
							Account: ledger.PlatformAccount(ledger.CodePlatformFeeReceivable, w.creditAsset),
							Side:    ledger.Debit, Quantity: qty(1_000_000_000),
						},
					},
				})
				return err
			}))
	}

	var killed int64
	err := testDB.InTx(w.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var pid int
			if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				return err
			}
			if _, err := w.markets.Execute(ctx, tx, nativemarket.ExecuteRequest{
				MarketID: w.market.ID, AccountID: w.buyer, Side: nativemarket.Buy,
				Amount: qty(1_000_000_000), MinOutput: money.Quantity{},
				IdempotencyKey: key, EffectiveAt: w.clk.Now(),
			}); err != nil {
				return err
			}
			n, kerr := terminateBackend(w.ctx, adminDSN, pid)
			if kerr != nil {
				return kerr
			}
			killed += n
			var one int
			return tx.QueryRow(ctx, "SELECT 1").Scan(&one)
		})
	require.Error(t, err)
	require.EqualValues(t, 1, killed)
	waitDB(t, 30*time.Second)

	require.Equal(t, versionBefore, w.marketVersion(),
		"a killed trade must not move the market's state version")
	require.Equal(t, balanceBefore, w.creditBalance(w.buyer), "the trader spent nothing")

	var fills int
	require.NoError(t, testDB.QueryRow(w.ctx,
		`SELECT count(*) FROM native_market_fills WHERE idempotency_key = $1`, key).Scan(&fills))
	require.Equal(t, 0, fills, "a killed trade must leave no fill")

	st, err := w.markets.State(w.ctx, testDB, w.market.ID)
	require.NoError(t, err)
	require.True(t, st.HoldsInvariant(w.market.Curve),
		"the constant product must still hold after the fault")
	w.checkNoDrift()

	// Still works.
	require.NoError(t, testDB.InTx(w.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, eerr := w.markets.Execute(ctx, tx, nativemarket.ExecuteRequest{
				MarketID: w.market.ID, AccountID: w.buyer, Side: nativemarket.Buy,
				Amount: qty(1_000_000_000), MinOutput: money.Quantity{},
				IdempotencyKey: key, EffectiveAt: w.clk.Now(),
			})
			return eerr
		}), "the same trade must succeed after the fault")
	require.Equal(t, versionBefore+1, w.marketVersion())
	w.checkNoDrift()
}

// TestChaos_ConcurrentPurchasesSurviveABackendBeingKilled runs eight buyers at
// one product while one of their backends is terminated mid-flight.
//
// The interesting failure is not the killed transaction — that is the previous
// test — but what the SURVIVORS do. They contend on the same product row and
// the same buyer lots, and a fault in one must not corrupt the accounting of
// the others.
func TestChaos_ConcurrentPurchasesSurviveABackendBeingKilled(t *testing.T) {
	requireEnv(t)
	requireHealthyStack(t, PostgresContainer())

	w := newInternalWorld(t, 10_000)
	buyerBefore := w.creditBalance(w.buyer)

	const workers = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
	)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			key := "chaos-race-" + uuid.NewString()
			err := testDB.InTx(w.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
				func(ctx context.Context, tx pgx.Tx) error {
					if idx == 0 {
						var pid int
						if perr := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); perr != nil {
							return perr
						}
						defer func() { _, _ = terminateBackend(w.ctx, adminDSN, pid) }()
					}
					_, perr := w.commerce.Purchase(ctx, tx, commerce.PurchaseRequest{
						ProductID: w.product.ID, BuyerAccountID: w.buyer, ExpectedPrice: qty(1_000),
						IdempotencyKey: key, EffectiveAt: w.clk.Now(),
					})
					return perr
				})
			if err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()
	waitDB(t, 30*time.Second)

	// 10,000 Credits at 1,000 each funds ten purchases, so every worker that
	// committed did so with real value behind it.
	require.LessOrEqual(t, succeeded, workers)
	spent := int64(succeeded) * 1_000
	expected := qtyString(t, buyerBefore).Sub(qty(spent))
	require.Equal(t, expected.String(), w.creditBalance(w.buyer),
		"the buyer paid for exactly the purchases that committed and no others")

	var orders int
	require.NoError(t, testDB.QueryRow(w.ctx,
		`SELECT count(*) FROM internal_commerce_orders WHERE product_id = $1`, w.product.ID).Scan(&orders))
	require.Equal(t, succeeded, orders, "one order per committed purchase, and no others")

	w.checkNoDrift()
}
