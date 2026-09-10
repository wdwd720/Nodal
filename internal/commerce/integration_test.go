//go:build integration

package commerce_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/commerce"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
	// testMigrate is the schema owner. Every "the database refuses this" test
	// writes through it, because whatever the owner cannot do, nothing in the
	// system can do.
	testMigrate *pgxpool.Pool
)

func TestMain(m *testing.M) { os.Exit(testMain(m)) }

func testMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "commerce integration: migrate up:", err)
		return 1
	}
	var err error
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "commerce-itest", MaxConns: 25}); err != nil {
		fmt.Fprintln(os.Stderr, "commerce integration: open pool:", err)
		return 1
	}
	defer testDB.Close()
	if testMigrate, err = pgxpool.New(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "commerce integration: open migrate pool:", err)
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

// There is exactly one Credit asset per deployment and migration 00711 enforces
// it, so this cannot be per-test: creating one per call passes on a fresh
// database and fails on the second run of the same one.
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
	require.False(t, creditAssetID.IsZero())
	return creditAssetID
}

type fixture struct {
	t       *testing.T
	ctx     context.Context
	clk     *clock.Fake
	led     *ledger.Service
	credits *credit.Service
	svc     *commerce.Service

	buyer  accounts.AccountID
	seller accounts.AccountID
	asset  assets.AssetID
}

func q(n int64) money.Quantity { return money.QuantityFromInt64(n) }

func newAccount(t *testing.T) accounts.AccountID {
	t.Helper()
	repo := accounts.NewRepository()
	user, err := repo.CreateUser(context.Background(), testDB, "commerce-itest", uuid.NewString(), nil)
	require.NoError(t, err)
	acct, err := repo.CreateAccount(context.Background(), testDB, user.ID, accounts.KindCustomer)
	require.NoError(t, err)
	return acct.ID
}

// activeCaps reports a fixed capability set as ACTIVE.
type activeCaps map[valuedomain.CapabilityKey]bool

func (c activeCaps) ActiveCapabilities(context.Context, db.Querier) (map[valuedomain.CapabilityKey]bool, error) {
	return c, nil
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireEnv(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC))
	led := ledger.NewService(clk, "commerce-itest")
	// The LEDGER gets no capability resolver, deliberately: a purchase is a
	// single-domain movement inside INTERNAL_CREDIT, and if it ever needed a
	// conversion capability to commit, that would mean a conversion had crept
	// into it. Leaving that resolver nil is part of the assertion.
	//
	// The COMMERCE service does get one, because running a user-to-user
	// marketplace is a deployment decision of its own, gated by MARKETPLACE.
	// TestIntegration_WithoutTheMarketplaceCapabilityNothingSells proves the
	// gate is load-bearing rather than decorative.
	credits := credit.NewService(led, clk)
	svc := commerce.NewService(led, credits, audit.NewWriter(), clk)
	svc.SetCapabilityResolver(activeCaps{commerce.CapMarketplace: true})
	return &fixture{
		t: t, ctx: ctx, clk: clk, led: led, credits: credits,
		svc:    svc,
		buyer:  newAccount(t),
		seller: newAccount(t),
		asset:  creditAsset(t),
	}
}

// fund mints Credits into an account with a given provenance and finality.
func (f *fixture) fund(account accounts.AccountID, origin valuedomain.CreditOrigin, fin valuedomain.FundingFinality, qty int64) credit.Lot {
	f.t.Helper()
	var lot credit.Lot
	require.NoError(f.t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			lot, err = f.credits.Issue(ctx, tx, credit.IssueRequest{
				AccountID: account, Quantity: q(qty), Origin: origin, Finality: fin,
				Reference:      credit.Reference{Type: "test_issue", ID: uuid.NewString()},
				IdempotencyKey: "issue-" + uuid.NewString(),
				Reason:         "test funding", EffectiveAt: f.clk.Now(),
			})
			return err
		}))
	return lot
}

// registerSeller registers f.seller, optionally attributing earnings elsewhere.
func (f *fixture) registerSeller(payoutTo *accounts.AccountID) commerce.Seller {
	f.t.Helper()
	var sel commerce.Seller
	require.NoError(f.t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			sel, err = f.svc.RegisterSeller(ctx, tx, commerce.Seller{
				AccountID: f.seller, DisplayName: "Test Creator", PayoutAccountID: payoutTo,
			})
			return err
		}))
	return sel
}

// list creates a product and publishes it, which is the state a buyer can see.
func (f *fixture) list(kind commerce.Kind, price int64, feeBPS money.BPS) commerce.Product {
	f.t.Helper()
	var p commerce.Product
	require.NoError(f.t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			created, err := f.svc.CreateProduct(ctx, tx, commerce.Product{
				SellerAccountID: f.seller, Kind: kind,
				Title: "Test " + string(kind), Description: "for the integration suite",
				Price: q(price), PlatformFeeBPS: feeBPS,
			})
			if err != nil {
				return err
			}
			p, err = f.svc.Publish(ctx, tx, created.ID)
			return err
		}))
	return p
}

func (f *fixture) purchase(p commerce.Product, buyer accounts.AccountID, expected money.Quantity, key string) (commerce.Order, error) {
	var o commerce.Order
	err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			o, err = f.svc.Purchase(ctx, tx, commerce.PurchaseRequest{
				ProductID: p.ID, BuyerAccountID: buyer, ExpectedPrice: expected,
				IdempotencyKey: key, EffectiveAt: f.clk.Now(),
				CorrelationID: "commerce-itest",
			})
			return err
		})
	return o, err
}

// balance reads a ledger balance straight from the projection. An account no
// posting has ever touched has no row at all, and that is a balance of zero
// rather than a test failure.
func (f *fixture) balance(owner accounts.AccountID, code ledger.Code) string {
	f.t.Helper()
	var bal string
	err := testDB.QueryRow(f.ctx,
		`SELECT coalesce(b.balance, 0)::text FROM ledger_accounts la
		   LEFT JOIN ledger_balances b ON b.ledger_account_id = la.id
		  WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = $1
		    AND la.code = $2 AND la.asset_id = $3`,
		owner, string(code), f.asset).Scan(&bal)
	if errors.Is(err, pgx.ErrNoRows) {
		return "0"
	}
	require.NoError(f.t, err)
	return bal
}

func (f *fixture) platformBalance(code ledger.Code) string {
	f.t.Helper()
	var bal string
	err := testDB.QueryRow(f.ctx,
		`SELECT coalesce(b.balance, 0)::text FROM ledger_accounts la
		   LEFT JOIN ledger_balances b ON b.ledger_account_id = la.id
		  WHERE la.owner_type = 'PLATFORM' AND la.code = $1 AND la.asset_id = $2`,
		string(code), f.asset).Scan(&bal)
	if err != nil {
		return "0"
	}
	return bal
}

// lotsOf returns the account's lots newest-first.
func (f *fixture) lotsOf(account accounts.AccountID) []credit.Lot {
	f.t.Helper()
	lots, err := f.credits.Lots(f.ctx, testDB, account)
	require.NoError(f.t, err)
	return lots
}

// ---------------------------------------------------------------------------
// 1. The sale itself
// ---------------------------------------------------------------------------

// TestIntegration_ASaleMovesCreditsAndRecordsCreatorProvenance is JOURNEY C in
// one test: a creator lists a dataset, a buyer pays for it with Credits they
// bought, and the creator's earning arrives carrying DATA_SALE_EARNING rather
// than the PURCHASED provenance the Credits started with.
//
// That transformation is the entire point of the package, and it is also the
// thing an attacker most wants: it is the only legitimate way a provenance
// that a payout policy might one day permit comes into existence.
func TestIntegration_ASaleMovesCreditsAndRecordsCreatorProvenance(t *testing.T) {
	f := newFixture(t)
	f.registerSeller(nil)
	f.fund(f.buyer, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 10_000)

	beforeFee := f.platformBalance(ledger.CodePlatformFeeReceivable)
	p := f.list(commerce.KindData, 1_000, 1_000) // 10% platform fee

	o, err := f.purchase(p, f.buyer, q(1_000), "buy-"+uuid.NewString())
	require.NoError(t, err)

	require.Equal(t, "1000", o.Price.String())
	require.Equal(t, "100", o.PlatformFee.String())
	require.Equal(t, "900", o.SellerProceeds.String())
	require.Equal(t, valuedomain.OriginDataSaleEarning, o.EarningOrigin,
		"a DATA product must produce DATA_SALE_EARNING, not a generic creator earning")
	require.False(t, o.JournalTxID.IsZero(), "an order must name the posting that moved the Credits")
	require.Equal(t, f.seller, o.EarningAccountID)
	require.Equal(t, p.Version, o.ProductVersion)

	require.Equal(t, "9000", f.balance(f.buyer, ledger.CodeCreditBalance))
	require.Equal(t, "900", f.balance(f.seller, ledger.CodeCreditBalance))
	require.Equal(t, addStr(beforeFee, 100), f.platformBalance(ledger.CodePlatformFeeReceivable))

	// The seller's Credits carry the sale's provenance and nothing else.
	sellerLots := f.lotsOf(f.seller)
	require.Len(t, sellerLots, 1)
	require.Equal(t, valuedomain.OriginDataSaleEarning, sellerLots[0].Origin)
	require.Equal(t, "900", sellerLots[0].Quantity.String())
	require.Equal(t, "900", sellerLots[0].Remaining.String())
	require.Equal(t, valuedomain.FinalityReversible, sellerLots[0].Finality,
		"an earning cannot be more final than the money behind it")
	require.NotNil(t, sellerLots[0].FundingReference)
	require.Equal(t, "internal_product", sellerLots[0].FundingReference.Type)
	require.Equal(t, p.ID.String(), sellerLots[0].FundingReference.ID)
	require.Equal(t, o.JournalTxID, sellerLots[0].JournalTxID)

	// Provenance and the ledger agree for both sides, and the earning
	// reconciles against the orders that produced it.
	require.NoError(t, f.credits.VerifyProvenance(f.ctx, testDB, f.buyer))
	require.NoError(t, f.credits.VerifyProvenance(f.ctx, testDB, f.seller))
	require.NoError(t, f.svc.VerifyEarnings(f.ctx, testDB, f.seller))

	// And the order is readable by both parties.
	bought, err := f.svc.OrdersByBuyer(f.ctx, testDB, f.buyer, 10)
	require.NoError(t, err)
	require.Len(t, bought, 1)
	require.Equal(t, o.ID, bought[0].ID)
	sold, err := f.svc.OrdersBySeller(f.ctx, testDB, f.seller, 10)
	require.NoError(t, err)
	require.Len(t, sold, 1)
	require.Equal(t, o.ID, sold[0].ID)
}

// TestIntegration_TheProductKindDecidesTheProvenance walks every declared kind
// and asserts the earning that lands. A kind whose provenance drifted -- or a
// new kind that silently inherited CREATOR_EARNING -- would change which
// origins a future payout determination covers, so this is checked end to end
// rather than only against the map.
func TestIntegration_TheProductKindDecidesTheProvenance(t *testing.T) {
	requireEnv(t)
	for _, kind := range commerce.AllKinds() {
		t.Run(string(kind), func(t *testing.T) {
			f := newFixture(t)
			f.registerSeller(nil)
			f.fund(f.buyer, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 5_000)
			p := f.list(kind, 500, 0)

			o, err := f.purchase(p, f.buyer, q(500), "buy-"+uuid.NewString())
			require.NoError(t, err)

			want, ok := commerce.EarningOrigin(kind)
			require.True(t, ok)
			require.Equal(t, want, o.EarningOrigin)

			lots := f.lotsOf(f.seller)
			require.Len(t, lots, 1)
			require.Equal(t, want, lots[0].Origin)
			require.Equal(t, "500", lots[0].Quantity.String(),
				"with no platform fee the creator receives the whole price")
			require.NoError(t, f.svc.VerifyEarnings(f.ctx, testDB, f.seller))
		})
	}
}

// TestIntegration_TheFeeRoundsTowardTheCreator proves the rounding direction on
// a price where it is visible. Rounding the platform's share up would take a
// sub-unit from a creator on every small sale, which across a marketplace is a
// real transfer disguised as arithmetic.
func TestIntegration_TheFeeRoundsTowardTheCreator(t *testing.T) {
	f := newFixture(t)
	f.registerSeller(nil)
	f.fund(f.buyer, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 10_000)

	// 999 at 3.33% is 33.2667: the fee must be 33, not 34.
	p := f.list(commerce.KindResearch, 999, 333)
	o, err := f.purchase(p, f.buyer, q(999), "buy-"+uuid.NewString())
	require.NoError(t, err)

	require.Equal(t, "33", o.PlatformFee.String())
	require.Equal(t, "966", o.SellerProceeds.String())
	require.Equal(t, o.Price.String(), addStr(o.PlatformFee.String(), 966),
		"the two halves must add back to exactly the price")
	require.Equal(t, "966", f.balance(f.seller, ledger.CodeCreditBalance))
}

// ---------------------------------------------------------------------------
// 2. Self-dealing: the attack this package exists to prevent
// ---------------------------------------------------------------------------

// TestIntegration_AnAccountCannotBuyFromItself covers the single most valuable
// thing an attacker could do here: converting Credits that no policy will ever
// let them withdraw into creator-earning provenance that one day might.
//
// It is refused twice -- by the service, which produces the comprehensible
// error, and by the database, which is what remains if the service is bypassed.
func TestIntegration_AnAccountCannotBuyFromItself(t *testing.T) {
	f := newFixture(t)
	f.registerSeller(nil)
	f.fund(f.seller, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 10_000)
	p := f.list(commerce.KindData, 1_000, 0)

	_, err := f.purchase(p, f.seller, q(1_000), "self-"+uuid.NewString())
	require.Error(t, err)
	require.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	// Nothing moved and no provenance was manufactured.
	require.Equal(t, "10000", f.balance(f.seller, ledger.CodeCreditBalance))
	lots := f.lotsOf(f.seller)
	require.Len(t, lots, 1)
	require.Equal(t, valuedomain.OriginPurchased, lots[0].Origin)

	// Now with the service out of the way. A real posting exists (someone else
	// bought the product), and the attacker writes an order row against it
	// naming themselves on both sides.
	other := newAccount(t)
	f.fund(other, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 2_000)
	real, err := f.purchase(p, other, q(1_000), "buy-"+uuid.NewString())
	require.NoError(t, err)

	_, err = testMigrate.Exec(f.ctx,
		`INSERT INTO internal_commerce_orders
		   (id, product_id, product_version, buyer_account_id, seller_account_id, earning_account_id,
		    price, platform_fee, seller_proceeds, earning_origin, journal_transaction_id, idempotency_key)
		 VALUES ($1,$2,$3,$4,$4,$4,1000,0,1000,'DATA_SALE_EARNING',$5,$6)`,
		uuid.New(), p.ID, p.Version, f.seller, real.JournalTxID, "forged-"+uuid.NewString())
	require.Error(t, err, "the schema owner itself must not be able to record a sale to oneself")
	require.Contains(t, err.Error(), "internal_commerce_orders_no_self_dealing")
}

// TestIntegration_SelfDealingThroughAPayoutAccountIsRefused closes the indirect
// version: the seller is a different account, but earnings are attributed to
// the buyer. The naive check -- buyer is not seller -- passes, and the sale is
// still the same laundering step.
func TestIntegration_SelfDealingThroughAPayoutAccountIsRefused(t *testing.T) {
	f := newFixture(t)
	buyer := f.buyer
	f.registerSeller(&buyer) // earnings attributed to the buyer
	f.fund(f.buyer, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 10_000)
	p := f.list(commerce.KindAgentService, 1_000, 0)

	_, err := f.purchase(p, f.buyer, q(1_000), "indirect-"+uuid.NewString())
	require.Error(t, err)
	require.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	require.Equal(t, "10000", f.balance(f.buyer, ledger.CodeCreditBalance))
}

// ---------------------------------------------------------------------------
// 3. What the database refuses when Go is bypassed
// ---------------------------------------------------------------------------

// TestIntegration_PublishedTermsAreFrozen proves SQLSTATE IC002. A buyer agreed
// to terms; the seller cannot restate them afterwards, and the attempt is
// refused for the schema owner too.
func TestIntegration_PublishedTermsAreFrozen(t *testing.T) {
	f := newFixture(t)
	f.registerSeller(nil)
	p := f.list(commerce.KindStrategyTemplate, 2_000, 500)

	for _, tc := range []struct {
		what string
		sql  string
	}{
		{"price", `UPDATE internal_products SET price = 1 WHERE id = $1`},
		{"platform fee", `UPDATE internal_products SET platform_fee_bps = 3000 WHERE id = $1`},
		{"kind", `UPDATE internal_products SET kind = 'DATA' WHERE id = $1`},
		{"version", `UPDATE internal_products SET version = 2 WHERE id = $1`},
		{"publication instant", `UPDATE internal_products SET published_at = now() WHERE id = $1`},
	} {
		t.Run(tc.what, func(t *testing.T) {
			_, err := testMigrate.Exec(f.ctx, tc.sql, p.ID)
			require.Error(t, err, "a published product's %s must be immutable", tc.what)
			require.Contains(t, err.Error(), "INTERNAL_PRODUCT_TERMS_FROZEN")
		})
	}

	// Status still moves: taking a product down is not restating its terms.
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.svc.SetStatus(ctx, tx, p.ID, commerce.StatusPaused)
			return err
		}))

	after, err := f.svc.Product(f.ctx, testDB, p.ID)
	require.NoError(t, err)
	require.Equal(t, commerce.StatusPaused, after.Status)
	require.Equal(t, "2000", after.Price.String())
	require.Equal(t, p.PublishedAt.UTC(), after.PublishedAt.UTC(),
		"published_at is the instant the terms froze and never moves")
}

// TestIntegration_AnOrderMustMatchThePostingItNames proves SQLSTATE IC001: the
// order row is not the authority on what was paid, the journal is. An order
// that claims an amount the entries do not show is refused at COMMIT.
func TestIntegration_AnOrderMustMatchThePostingItNames(t *testing.T) {
	f := newFixture(t)
	f.registerSeller(nil)
	f.fund(f.buyer, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 5_000)
	p := f.list(commerce.KindCompute, 1_000, 1_000)

	real, err := f.purchase(p, f.buyer, q(1_000), "buy-"+uuid.NewString())
	require.NoError(t, err)

	for _, tc := range []struct {
		what              string
		price, fee, procs int64
	}{
		{"an inflated price", 9_999, 0, 9_999},
		{"inflated proceeds", 1_000, 0, 1_000},
		{"a deflated price", 10, 0, 10},
	} {
		t.Run(tc.what, func(t *testing.T) {
			_, err := testMigrate.Exec(f.ctx,
				`INSERT INTO internal_commerce_orders
				   (id, product_id, product_version, buyer_account_id, seller_account_id, earning_account_id,
				    price, platform_fee, seller_proceeds, earning_origin, journal_transaction_id, idempotency_key)
				 VALUES ($1,$2,$3,$4,$5,$5,$6::numeric,$7::numeric,$8::numeric,'CREATOR_EARNING',$9,$10)`,
				uuid.New(), p.ID, p.Version, f.buyer, f.seller,
				tc.price, tc.fee, tc.procs, real.JournalTxID, "forged-"+uuid.NewString())
			require.Error(t, err, "an order claiming %s must be refused", tc.what)
			require.Contains(t, err.Error(), "INTERNAL_ORDER_UNBALANCED")
		})
	}

	// The genuine numbers are accepted against the same posting, which proves
	// the trigger is discriminating rather than simply refusing everything.
	_, err = testMigrate.Exec(f.ctx,
		`INSERT INTO internal_commerce_orders
		   (id, product_id, product_version, buyer_account_id, seller_account_id, earning_account_id,
		    price, platform_fee, seller_proceeds, earning_origin, journal_transaction_id, idempotency_key)
		 VALUES ($1,$2,$3,$4,$5,$5,1000,100,900,'CREATOR_EARNING',$6,$7)`,
		uuid.New(), p.ID, p.Version, f.buyer, f.seller, real.JournalTxID, "control-"+uuid.NewString())
	require.NoError(t, err)
}

// TestIntegration_AnOrderIsImmutable proves commerce history cannot be edited
// or erased, including by the schema owner.
func TestIntegration_AnOrderIsImmutable(t *testing.T) {
	f := newFixture(t)
	f.registerSeller(nil)
	f.fund(f.buyer, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 3_000)
	p := f.list(commerce.KindAPIAccess, 750, 0)
	o, err := f.purchase(p, f.buyer, q(750), "buy-"+uuid.NewString())
	require.NoError(t, err)

	_, err = testMigrate.Exec(f.ctx, `UPDATE internal_commerce_orders SET price = 1 WHERE id = $1`, o.ID)
	require.Error(t, err)
	_, err = testMigrate.Exec(f.ctx, `DELETE FROM internal_commerce_orders WHERE id = $1`, o.ID)
	require.Error(t, err)

	still, err := f.svc.Order(f.ctx, testDB, o.ID)
	require.NoError(t, err)
	require.Equal(t, "750", still.Price.String())
}

// ---------------------------------------------------------------------------
// 4. Refusals a buyer should see
// ---------------------------------------------------------------------------

// TestIntegration_APriceTheBuyerDidNotAgreeToIsRefused: the buyer states what
// they were shown, and a mismatch is a refusal rather than a surprise charge.
func TestIntegration_APriceTheBuyerDidNotAgreeToIsRefused(t *testing.T) {
	f := newFixture(t)
	f.registerSeller(nil)
	f.fund(f.buyer, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 5_000)
	p := f.list(commerce.KindResearch, 1_200, 0)

	_, err := f.purchase(p, f.buyer, q(900), "stale-"+uuid.NewString())
	require.Error(t, err)
	require.Equal(t, errs.CodeConflict, errs.CodeOf(err))
	require.Equal(t, "5000", f.balance(f.buyer, ledger.CodeCreditBalance))

	// The same request at the real price succeeds, so the refusal was about
	// the disagreement and not about the product.
	_, err = f.purchase(p, f.buyer, q(1_200), "ok-"+uuid.NewString())
	require.NoError(t, err)
}

// TestIntegration_OnlyAnActiveProductCanBeBought walks the lifecycle: a draft is
// not yet for sale, a paused one is temporarily not, and a withdrawn one never
// will be again.
func TestIntegration_OnlyAnActiveProductCanBeBought(t *testing.T) {
	f := newFixture(t)
	f.registerSeller(nil)
	f.fund(f.buyer, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 5_000)

	var draft commerce.Product
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			draft, err = f.svc.CreateProduct(ctx, tx, commerce.Product{
				SellerAccountID: f.seller, Kind: commerce.KindResearch,
				Title: "Unpublished", Price: q(100),
			})
			return err
		}))
	require.Equal(t, commerce.StatusDraft, draft.Status)
	require.Nil(t, draft.PublishedAt, "an unpublished product's terms are not yet frozen")

	_, err := f.purchase(draft, f.buyer, q(100), "draft-"+uuid.NewString())
	require.Error(t, err)
	require.Equal(t, errs.CodeAssetRestricted, errs.CodeOf(err))

	p := f.list(commerce.KindResearch, 100, 0)
	setStatus := func(to commerce.Status) {
		require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
			func(ctx context.Context, tx pgx.Tx) error {
				_, err := f.svc.SetStatus(ctx, tx, p.ID, to)
				return err
			}))
	}

	setStatus(commerce.StatusPaused)
	_, err = f.purchase(p, f.buyer, q(100), "paused-"+uuid.NewString())
	require.Error(t, err)
	require.Equal(t, errs.CodeAssetRestricted, errs.CodeOf(err))

	setStatus(commerce.StatusActive)
	_, err = f.purchase(p, f.buyer, q(100), "resumed-"+uuid.NewString())
	require.NoError(t, err, "PAUSED means temporarily unavailable, and a resumed product sells")

	setStatus(commerce.StatusWithdrawn)
	_, err = f.purchase(p, f.buyer, q(100), "withdrawn-"+uuid.NewString())
	require.Error(t, err)

	// WITHDRAWN is terminal: a product that can come back is PAUSED, and the
	// two mean different things to a buyer reading their purchase history.
	err = testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.svc.SetStatus(ctx, tx, p.ID, commerce.StatusActive)
			return err
		})
	require.Error(t, err)
	require.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))
}

// TestIntegration_ASuspendedSellerTakesNoOrders: suspension has to stop sales
// in flight, not merely stop new listings.
func TestIntegration_ASuspendedSellerTakesNoOrders(t *testing.T) {
	f := newFixture(t)
	f.registerSeller(nil)
	f.fund(f.buyer, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 5_000)
	p := f.list(commerce.KindData, 500, 0)

	_, err := testDB.Exec(f.ctx,
		`UPDATE internal_sellers SET status = 'SUSPENDED', suspended_reason = 'moderation' WHERE account_id = $1`,
		f.seller)
	require.NoError(t, err)

	_, err = f.purchase(p, f.buyer, q(500), "suspended-"+uuid.NewString())
	require.Error(t, err)
	require.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	require.Equal(t, "5000", f.balance(f.buyer, ledger.CodeCreditBalance))
	require.Equal(t, "0", f.balance(f.seller, ledger.CodeCreditBalance))
}

// TestIntegration_DisputedCreditsCannotBuyAnything: value whose funding is
// under dispute is not spendable, and a purchase is spending.
func TestIntegration_DisputedCreditsCannotBuyAnything(t *testing.T) {
	f := newFixture(t)
	f.registerSeller(nil)
	lot := f.fund(f.buyer, valuedomain.OriginPurchased, valuedomain.FinalityReversible, 5_000)
	p := f.list(commerce.KindData, 1_000, 0)

	// The buyer can buy while the funding merely has not settled.
	_, err := f.purchase(p, f.buyer, q(1_000), "reversible-"+uuid.NewString())
	require.NoError(t, err)

	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			return f.credits.SetFinality(ctx, tx, lot.ID, valuedomain.FinalityDisputed,
				credit.Reference{Type: "test_dispute", ID: uuid.NewString()}, "chargeback opened")
		}))

	_, err = f.purchase(p, f.buyer, q(1_000), "disputed-"+uuid.NewString())
	require.Error(t, err, "a chargeback in flight must stop the Credits behind it from buying anything")
	require.Equal(t, errs.CodeInsufficientBuyingPower, errs.CodeOf(err))
	require.Equal(t, "4000", f.balance(f.buyer, ledger.CodeCreditBalance),
		"the refused purchase moved nothing")
}

// TestIntegration_ABuyerCannotSpendMoreThanTheyHave: the price is checked
// against real Credits, not against optimism.
func TestIntegration_ABuyerCannotSpendMoreThanTheyHave(t *testing.T) {
	f := newFixture(t)
	f.registerSeller(nil)
	f.fund(f.buyer, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 900)
	p := f.list(commerce.KindData, 1_000, 0)

	_, err := f.purchase(p, f.buyer, q(1_000), "poor-"+uuid.NewString())
	require.Error(t, err)
	require.Equal(t, "900", f.balance(f.buyer, ledger.CodeCreditBalance))
	require.Equal(t, "0", f.balance(f.seller, ledger.CodeCreditBalance))
	require.NoError(t, f.credits.VerifyProvenance(f.ctx, testDB, f.buyer))
}

// ---------------------------------------------------------------------------
// 5. Repetition and concurrency
// ---------------------------------------------------------------------------

// TestIntegration_APurchaseIsIdempotent: a retried click charges once. The
// second call returns the first order rather than a second sale, and neither
// the ledger nor the provenance record moves twice.
func TestIntegration_APurchaseIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.registerSeller(nil)
	f.fund(f.buyer, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 10_000)
	p := f.list(commerce.KindCreatorProduct, 2_500, 400)

	key := "retry-" + uuid.NewString()
	first, err := f.purchase(p, f.buyer, q(2_500), key)
	require.NoError(t, err)
	second, err := f.purchase(p, f.buyer, q(2_500), key)
	require.NoError(t, err)

	require.Equal(t, first.ID, second.ID)
	require.Equal(t, first.JournalTxID, second.JournalTxID)
	require.Equal(t, "7500", f.balance(f.buyer, ledger.CodeCreditBalance))
	require.Equal(t, "2400", f.balance(f.seller, ledger.CodeCreditBalance))
	require.Len(t, f.lotsOf(f.seller), 1, "a retry must not mint a second earning lot")

	var orders int
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT count(*) FROM internal_commerce_orders WHERE product_id = $1`, p.ID).Scan(&orders))
	require.Equal(t, 1, orders)

	require.NoError(t, f.credits.VerifyProvenance(f.ctx, testDB, f.buyer))
	require.NoError(t, f.credits.VerifyProvenance(f.ctx, testDB, f.seller))
	require.NoError(t, f.svc.VerifyEarnings(f.ctx, testDB, f.seller))
}

// TestIntegration_ConcurrentPurchasesCannotOverspend fires ten simultaneous
// buys, each with its own idempotency key, against a balance that funds two.
// Exactly two must commit: an idempotency key does not help here, because
// these are ten genuinely different purchases racing for the same Credits.
func TestIntegration_ConcurrentPurchasesCannotOverspend(t *testing.T) {
	f := newFixture(t)
	f.registerSeller(nil)
	f.fund(f.buyer, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 1_000)
	p := f.list(commerce.KindCompetitionEntry, 400, 0)

	const attempts = 10
	var succeeded atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if _, err := f.purchase(p, f.buyer, q(400), fmt.Sprintf("race-%s-%d", uuid.NewString(), i)); err == nil {
				succeeded.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	require.Equal(t, int64(2), succeeded.Load(), "1,000 Credits buys exactly two 400-Credit products")
	require.Equal(t, "200", f.balance(f.buyer, ledger.CodeCreditBalance))
	require.Equal(t, "800", f.balance(f.seller, ledger.CodeCreditBalance))
	require.Len(t, f.lotsOf(f.seller), 2)

	require.NoError(t, f.credits.VerifyProvenance(f.ctx, testDB, f.buyer))
	require.NoError(t, f.credits.VerifyProvenance(f.ctx, testDB, f.seller))
	require.NoError(t, f.svc.VerifyEarnings(f.ctx, testDB, f.seller))
}

// TestIntegration_EarningsAreSpendableAndReconcile: a creator can immediately
// spend what they earned, and the reconciliation between orders paid and
// provenance issued survives that spending -- because it compares what was
// issued, not what remains.
func TestIntegration_EarningsAreSpendableAndReconcile(t *testing.T) {
	f := newFixture(t)
	f.registerSeller(nil)
	f.fund(f.buyer, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 10_000)
	p := f.list(commerce.KindAgentService, 3_000, 0)
	_, err := f.purchase(p, f.buyer, q(3_000), "buy-"+uuid.NewString())
	require.NoError(t, err)

	// The creator now buys something themselves, spending the earning.
	onward := newAccount(t)
	f2 := &fixture{
		t: t, ctx: f.ctx, clk: f.clk, led: f.led, credits: f.credits, svc: f.svc,
		buyer: f.seller, seller: onward, asset: f.asset,
	}
	f2.registerSeller(nil)
	q2 := f2.list(commerce.KindData, 1_000, 0)
	_, err = f2.purchase(q2, f.seller, q(1_000), "onward-"+uuid.NewString())
	require.NoError(t, err, "an earning is spendable")

	require.Equal(t, "2000", f.balance(f.seller, ledger.CodeCreditBalance))
	require.NoError(t, f.credits.VerifyProvenance(f.ctx, testDB, f.seller))
	require.NoError(t, f.svc.VerifyEarnings(f.ctx, testDB, f.seller),
		"reconciliation compares what was issued, which spending does not change")

	// And the provenance carried forward: the onward seller was paid with
	// value that arrived as an AGENT_SERVICE_EARNING, and receives a
	// DATA_SALE_EARNING of their own.
	onwardLots := f2.lotsOf(onward)
	require.Len(t, onwardLots, 1)
	require.Equal(t, valuedomain.OriginDataSaleEarning, onwardLots[0].Origin)
}

// ---------------------------------------------------------------------------
// 6. Isolation
// ---------------------------------------------------------------------------

// TestIntegration_APurchaseIsASingleDomainMovement: a sale is Credits moving
// between accounts, so it must commit with no capability active at all. If it
// ever required one, that would mean a conversion had crept into the path --
// and a conversion between value domains is precisely what must never happen
// silently.
func TestIntegration_APurchaseIsASingleDomainMovement(t *testing.T) {
	f := newFixture(t)
	f.registerSeller(nil)
	f.fund(f.buyer, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 5_000)
	p := f.list(commerce.KindData, 1_000, 500)

	o, err := f.purchase(p, f.buyer, q(1_000), "buy-"+uuid.NewString())
	require.NoError(t, err, "a purchase must not need a capability; nothing converts")

	// Every leg of the posting is INTERNAL_CREDIT and the transaction declares
	// no conversion, which is what the isolation trigger checks against.
	var domains []string
	rows, err := testDB.Query(f.ctx,
		`SELECT DISTINCT la.value_domain FROM journal_entries e
		   JOIN ledger_accounts la ON la.id = e.ledger_account_id
		  WHERE e.transaction_id = $1`, o.JournalTxID)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var d string
		require.NoError(t, rows.Scan(&d))
		domains = append(domains, d)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []string{string(valuedomain.InternalCredit)}, domains)

	var convFrom, convTo *string
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT conversion_from, conversion_to FROM journal_transactions WHERE id = $1`,
		o.JournalTxID).Scan(&convFrom, &convTo))
	require.Nil(t, convFrom)
	require.Nil(t, convTo)
}

// TestIntegration_WithoutTheMarketplaceCapabilityNothingSells: the gate is the
// deployment's decision about whether it runs a user-to-user marketplace at
// all, and it is enforced in the domain service rather than only at the HTTP
// edge -- a gate a worker or a script walks around is not a gate.
func TestIntegration_WithoutTheMarketplaceCapabilityNothingSells(t *testing.T) {
	f := newFixture(t)
	f.registerSeller(nil)
	f.fund(f.buyer, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 5_000)
	p := f.list(commerce.KindData, 1_000, 0)

	// Listing and publishing are fine: they move no value. Buying is not.
	closed := commerce.NewService(f.led, f.credits, audit.NewWriter(), f.clk) // no resolver at all
	shut := &fixture{
		t: t, ctx: f.ctx, clk: f.clk, led: f.led, credits: f.credits,
		svc: closed, buyer: f.buyer, seller: f.seller, asset: f.asset,
	}

	_, err := shut.purchase(p, f.buyer, q(1_000), "nogate-"+uuid.NewString())
	require.Error(t, err, "a service with no capability resolver must sell nothing")
	require.Equal(t, errs.CodeCapabilityNotApproved, errs.CodeOf(err))

	// Explicitly inactive is the same answer as unconfigured.
	off := commerce.NewService(f.led, f.credits, audit.NewWriter(), f.clk)
	off.SetCapabilityResolver(activeCaps{commerce.CapMarketplace: false})
	shut.svc = off
	_, err = shut.purchase(p, f.buyer, q(1_000), "offgate-"+uuid.NewString())
	require.Error(t, err)
	require.Equal(t, errs.CodeCapabilityNotApproved, errs.CodeOf(err))

	// Nothing moved and no order exists.
	require.Equal(t, "5000", f.balance(f.buyer, ledger.CodeCreditBalance))
	require.Equal(t, "0", f.balance(f.seller, ledger.CodeCreditBalance))
	var orders int
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT count(*) FROM internal_commerce_orders WHERE product_id = $1`, p.ID).Scan(&orders))
	require.Equal(t, 0, orders)

	// And with the gate on, the same purchase commits, so the refusal was
	// about the gate and nothing else.
	_, err = f.purchase(p, f.buyer, q(1_000), "ongate-"+uuid.NewString())
	require.NoError(t, err)
}

// TestIntegration_ARepeatedPurchaseStopsWorkingWhenTheGateIsPulled: a replay
// is not a different act. If the marketplace is switched off between the
// original purchase and the retry, the retry must be refused rather than
// answered from the idempotency record.
func TestIntegration_ARepeatedPurchaseStopsWorkingWhenTheGateIsPulled(t *testing.T) {
	f := newFixture(t)
	f.registerSeller(nil)
	f.fund(f.buyer, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 5_000)
	p := f.list(commerce.KindResearch, 500, 0)

	key := "replay-" + uuid.NewString()
	_, err := f.purchase(p, f.buyer, q(500), key)
	require.NoError(t, err)

	off := commerce.NewService(f.led, f.credits, audit.NewWriter(), f.clk)
	off.SetCapabilityResolver(activeCaps{commerce.CapMarketplace: false})
	shut := &fixture{
		t: t, ctx: f.ctx, clk: f.clk, led: f.led, credits: f.credits,
		svc: off, buyer: f.buyer, seller: f.seller, asset: f.asset,
	}

	_, err = shut.purchase(p, f.buyer, q(500), key)
	require.Error(t, err)
	require.Equal(t, errs.CodeCapabilityNotApproved, errs.CodeOf(err))

	// The original sale stands: switching the gate off stops new activity, it
	// does not unwind what already happened.
	require.Equal(t, "4500", f.balance(f.buyer, ledger.CodeCreditBalance))
	require.Equal(t, "500", f.balance(f.seller, ledger.CodeCreditBalance))
}

// addStr adds n to a decimal string balance. Balances are exact integers, so
// this stays in money rather than becoming arithmetic on floats.
func addStr(base string, n int64) string {
	v, err := money.ParseQuantity(base)
	if err != nil {
		panic(err)
	}
	return v.Add(q(n)).String()
}

// An idempotency key belongs to one account (F-106).
//
// internal_commerce_orders.idempotency_key is globally UNIQUE and the HTTP
// boundary's idempotency record is keyed by actor, so a different caller
// reusing a key reached the domain -- which returned the order it found without
// asking whose it was: buyer, seller, price, platform fee and proceeds, and the
// caller's own purchase silently discarded.
func TestIntegration_ACommerceKeyBelongsToOneAccount(t *testing.T) {
	f := newFixture(t)
	f.registerSeller(nil)
	f.fund(f.buyer, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 10_000)
	p := f.list(commerce.KindCreatorProduct, 2_500, 400)

	key := "shared-" + uuid.NewString()
	first, err := f.purchase(p, f.buyer, q(2_500), key)
	require.NoError(t, err)

	stranger := newAccount(t)
	f.fund(stranger, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 10_000)
	_, err = f.purchase(p, stranger, q(2_500), key)
	require.Error(t, err, "another account's order was returned as this caller's replay")
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, errs.CodeOf(err))

	// The control: the buyer's own retry is still a replay.
	again, err := f.purchase(p, f.buyer, q(2_500), key)
	require.NoError(t, err)
	assert.Equal(t, first.ID, again.ID)
}
