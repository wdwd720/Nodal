//go:build integration

package quote_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/quote"
	"github.com/nodal/controlplane/internal/security"
)

// Same advisory-lock protocol as internal/intent; run against an isolated
// database provisioned with `go run ./scripts/testdb -name intent -export`.
const testAdvisoryLockID = 424242

var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
)

func TestMain(m *testing.M) { os.Exit(testMain(m)) }

func testMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	lockConn, err := pgx.Connect(ctx, testAppURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "quote integration: connect for advisory lock:", err)
		return 1
	}
	defer func() { _ = lockConn.Close(ctx) }()
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock_shared($1)", testAdvisoryLockID); err != nil {
		fmt.Fprintln(os.Stderr, "quote integration: advisory lock:", err)
		return 1
	}
	defer func() { _, _ = lockConn.Exec(ctx, "SELECT pg_advisory_unlock_shared($1)", testAdvisoryLockID) }()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "quote integration: migrate up:", err)
		return 1
	}
	testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "quote-itest", MaxConns: 8})
	if err != nil {
		fmt.Fprintln(os.Stderr, "quote integration: open pool:", err)
		return 1
	}
	defer testDB.Close()
	return m.Run()
}

type fixture struct {
	t          *testing.T
	ctx        context.Context
	clk        *clock.Fake
	sol, usdc  assets.Asset
	instrument instruments.Instrument
	listing    instruments.VenueListing
	intentID   intent.IntentID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if testDB == nil {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	f := &fixture{t: t, ctx: ctx, clk: clock.NewFake(received)}
	suffix := id.New[id.Any]().String()
	var err error
	f.sol, err = assets.NewRepository().Create(ctx, testDB, assets.Asset{Chain: "solana-test", MintAddress: "SOL-" + suffix, Kind: assets.KindSPLToken, Symbol: "SOL", Name: "Solana", Decimals: 9, RiskClass: assets.RiskMajor, Status: assets.StatusActive})
	require.NoError(t, err)
	f.usdc, err = assets.NewRepository().Create(ctx, testDB, assets.Asset{Chain: "solana-test", MintAddress: "USDC-" + suffix, Kind: assets.KindSPLToken, Symbol: "USDC", Name: "USD Coin", Decimals: 6, IsStablecoin: true, PegCurrency: "USD", RiskClass: assets.RiskSettlement, Status: assets.StatusActive})
	require.NoError(t, err)
	ins := instruments.NewRepository()
	venue, err := ins.CreateVenue(ctx, testDB, instruments.Venue{Code: "JUP-" + suffix, Name: "Jupiter", Kind: instruments.VenueDEXAggregator, Chain: "solana-test", Status: instruments.VenueActive})
	require.NoError(t, err)
	require.NoError(t, testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		f.instrument, err = ins.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{Base: f.sol.ID, Quote: f.usdc.ID, Settlement: f.usdc.ID, CanonicalName: "SOL/USDC " + suffix, RiskClass: assets.RiskMajor, Status: assets.StatusActive, ActiveFrom: received.Add(-time.Hour)})
		return err
	}))
	f.listing, err = ins.CreateListing(ctx, testDB, instruments.VenueListing{
		VenueID: venue.ID, InstrumentID: f.instrument.ID, VenueNativeID: "SOL/USDC-" + suffix, Network: "solana-test",
		BaseMint: f.sol.MintAddress, QuoteMint: f.usdc.MintAddress, BasePrecision: 9, QuotePrecision: 6,
		MinNotionalQuote: money.QuantityFromInt64(1_000000), Status: instruments.VenueActive,
	})
	require.NoError(t, err)

	// An intent to attach quotes to.
	acctRepo := accounts.NewRepository()
	u, err := acctRepo.CreateUser(ctx, testDB, "https://idp.test", "sub-"+suffix, nil)
	require.NoError(t, err)
	a, err := acctRepo.CreateAccount(ctx, testDB, u.ID, accounts.KindCustomer)
	require.NoError(t, err)
	repo := intent.NewRepository(f.clk, event.NewOutbox(f.clk), audit.NewWriter())
	notional := money.USDFromMinor(250_00)
	require.NoError(t, testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		created, err := repo.Create(ctx, tx, intent.TradeIntent{
			ID: intent.NewIntentID(), AccountID: a.ID.String(), ActorType: security.ActorUser, ActorID: u.ID.String(),
			Action: intent.ActionAcquireNotional, InstrumentID: f.instrument.ID, NotionalUSD: &notional,
			RequestedAt: received.Add(-time.Second), IdempotencyKey: "q-" + suffix, CorrelationID: "corr", Mode: intent.ModeLive,
		})
		f.intentID = created.ID
		return err
	}))
	return f
}

func (f *fixture) quote(receivedAt time.Time) quote.Quote {
	qt := validQuote(f.t)
	qt.IntentID = &f.intentID
	qt.InstrumentID, qt.VenueListingID = f.instrument.ID, f.listing.ID
	qt.InputAssetID, qt.OutputAssetID = f.usdc.ID, f.sol.ID
	qt.EstNetworkCostAssetID, qt.EstVenueFeeAssetID = asset(f.sol.ID), asset(f.usdc.ID)
	qt.ReceivedAt, qt.ExpiresAt = receivedAt, receivedAt.Add(10*time.Second)
	qt.EffectivePrice = quote.NewEffectivePrice(q(151_515151), 6, quote.SideBuy, f.usdc.ID, f.sol.ID, "jupiter", receivedAt)
	return qt
}

func (f *fixture) record(qt quote.Quote) (quote.Quote, error) {
	var out quote.Quote
	err := testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = quote.NewRepository().Record(ctx, tx, qt)
		return err
	})
	return out, err
}

func assertSameQuote(t *testing.T, want, got quote.Quote) {
	t.Helper()
	assert.Equal(t, want.ID, got.ID)
	assert.Equal(t, want.IntentID, got.IntentID)
	assert.Equal(t, want.Provider, got.Provider)
	assert.Equal(t, want.ProviderRequestID, got.ProviderRequestID)
	assert.Equal(t, want.InstrumentID, got.InstrumentID)
	assert.Equal(t, want.VenueListingID, got.VenueListingID)
	assert.Equal(t, want.Side, got.Side)
	assert.Equal(t, want.InputAssetID, got.InputAssetID)
	assert.Equal(t, want.OutputAssetID, got.OutputAssetID)
	for name, pair := range map[string][2]money.Quantity{
		"input": {want.InputQuantity, got.InputQuantity}, "expected": {want.ExpectedOutput, got.ExpectedOutput},
		"minimum": {want.MinimumOutput, got.MinimumOutput}, "network": {want.EstNetworkCost, got.EstNetworkCost},
		"venue": {want.EstVenueFee, got.EstVenueFee}, "platform": {want.PlatformFee, got.PlatformFee},
	} {
		assert.True(t, pair[0].Equal(pair[1]), "%s: %s vs %s", name, pair[0], pair[1])
	}
	assert.Equal(t, want.EffectivePrice.String(), got.EffectivePrice.String())
	assert.Equal(t, want.EffectivePrice, got.EffectivePrice)
	assert.Equal(t, want.PriceImpactBPS, got.PriceImpactBPS)
	assert.Equal(t, want.SlippageBPS, got.SlippageBPS)
	assert.Equal(t, want.EstNetworkCostAssetID, got.EstNetworkCostAssetID)
	assert.Equal(t, want.EstVenueFeeAssetID, got.EstVenueFeeAssetID)
	assert.Equal(t, want.PlatformFeeAssetID, got.PlatformFeeAssetID)
	assert.Equal(t, want.PlatformFeeBPS, got.PlatformFeeBPS)
	assert.Equal(t, want.FeePolicyVersion, got.FeePolicyVersion)
	assert.Equal(t, want.ReceivedAt, got.ReceivedAt)
	assert.Equal(t, want.ExpiresAt, got.ExpiresAt)
	assert.Equal(t, want.RouteHash, got.RouteHash)
	assert.JSONEq(t, string(want.RouteSummary), string(got.RouteSummary))
	assert.Equal(t, want.RawResponseRef, got.RawResponseRef)
	assert.Equal(t, want.RawResponseHash, got.RawResponseHash)
	assert.False(t, got.CreatedAt.IsZero())
}

func TestIntegration_Quote_RecordGetLatestAndImmutability(t *testing.T) {
	f := newFixture(t)
	repo := quote.NewRepository()

	first := f.quote(received)
	stored, err := f.record(first)
	require.NoError(t, err)
	assertSameQuote(t, first, stored)
	require.NoError(t, stored.Validate(), "a stored quote reloads as a valid quote")

	got, err := repo.Get(f.ctx, testDB, first.ID)
	require.NoError(t, err)
	assertSameQuote(t, first, got)
	assert.True(t, got.IsFresh(received.Add(200*time.Millisecond), 500*time.Millisecond))
	assert.True(t, got.IsExpired(received.Add(10*time.Second)))

	// Disclosure from the stored row reports the venue's numbers verbatim.
	d := got.Disclosure()
	assert.True(t, d.ExpectedReceive.Equal(first.ExpectedOutput))
	assert.True(t, d.PlatformFee.Amount.IsZero())

	// Latest for the intent is the most recently received quote.
	second := f.quote(received.Add(time.Second))
	_, err = f.record(second)
	require.NoError(t, err)
	latest, err := repo.LatestForIntent(f.ctx, testDB, f.intentID)
	require.NoError(t, err)
	assert.Equal(t, second.ID, latest.ID)
	_, err = repo.LatestForIntent(f.ctx, testDB, intent.NewIntentID())
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
	_, err = repo.Get(f.ctx, testDB, quote.NewQuoteID())
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))

	// Insert-only: duplicate ids conflict, unknown references are NOT_FOUND, invalid quotes never reach SQL.
	_, err = f.record(first)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))
	orphan := f.quote(received)
	orphan.VenueListingID = listingID(t)
	_, err = f.record(orphan)
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
	invalid := f.quote(received)
	invalid.MinimumOutput = invalid.ExpectedOutput.Add(q(1))
	_, err = f.record(invalid)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	// Immutable: the application role can neither update nor delete a quote.
	_, err = testDB.Exec(f.ctx, `UPDATE quotes SET expected_output = expected_output + 1 WHERE id = $1`, first.ID)
	require.Error(t, err)
	assert.True(t, db.IsMutationForbidden(err), "%v", err)
	_, err = testDB.Exec(f.ctx, `DELETE FROM quotes WHERE id = $1`, first.ID)
	require.Error(t, err)
	assert.True(t, db.IsMutationForbidden(err), "%v", err)
	unchanged, err := repo.Get(f.ctx, testDB, first.ID)
	require.NoError(t, err)
	assert.True(t, unchanged.ExpectedOutput.Equal(first.ExpectedOutput))

	// A quote without an intent is legal (indicative quotes).
	loose := f.quote(received)
	loose.IntentID = nil
	storedLoose, err := f.record(loose)
	require.NoError(t, err)
	assert.Nil(t, storedLoose.IntentID)
}
