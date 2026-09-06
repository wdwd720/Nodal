//go:build integration

package instruments_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
)

func openDB(t *testing.T) *db.DB {
	t.Helper()
	url := os.Getenv("CP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CP_TEST_DATABASE_URL not set; skipping integration test")
	}
	d, err := db.Open(context.Background(), db.Config{URL: url, MaxConns: 8, MinConns: 1, AppName: "instruments-test"})
	require.NoError(t, err)
	t.Cleanup(d.Close)
	return d
}

func seedAssets(t *testing.T, d *db.DB) (sol, usdc assets.Asset) {
	t.Helper()
	ctx := context.Background()
	repo := assets.NewRepository()
	suffix := time.Now().UnixNano()
	var err error
	sol, err = repo.Create(ctx, d.Pool(), assets.Asset{Chain: "solana-test", MintAddress: "So1" + itoa(suffix), Kind: assets.KindSPLToken, Symbol: "SOL", Name: "Solana", Decimals: 9, RiskClass: assets.RiskMajor, Status: assets.StatusActive})
	require.NoError(t, err)
	usdc, err = repo.Create(ctx, d.Pool(), assets.Asset{Chain: "solana-test", MintAddress: "USDC" + itoa(suffix), Kind: assets.KindSPLToken, Symbol: "USDC", Name: "USD Coin", Decimals: 6, IsStablecoin: true, PegCurrency: "USD", RiskClass: assets.RiskSettlement, Status: assets.StatusActive})
	require.NoError(t, err)
	return sol, usdc
}

func itoa(n int64) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{digits[n%10]}, b...)
		n /= 10
	}
	return string(b)
}

func TestIntegration_Instruments_SpotPairLifecycle(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	repo := instruments.NewRepository()
	sol, usdc := seedAssets(t, d)

	venue, err := repo.CreateVenue(ctx, d.Pool(), instruments.Venue{Code: "JUPITER-" + itoa(time.Now().UnixNano()), Name: "Jupiter", Kind: instruments.VenueDEXAggregator, Chain: "solana-test", Status: instruments.VenueActive})
	require.NoError(t, err)
	_, err = repo.CreateVenue(ctx, d.Pool(), instruments.Venue{Code: venue.Code, Name: "dup", Kind: instruments.VenueDEX, Status: instruments.VenueActive})
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))

	var ins instruments.Instrument
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ins, err = repo.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{Base: sol.ID, Quote: usdc.ID, Settlement: usdc.ID, CanonicalName: "SOL/USDC", RiskClass: assets.RiskMajor, Status: assets.StatusActive, ActiveFrom: time.Now().UTC().Add(-time.Minute)})
		return err
	}))
	assert.Equal(t, instruments.TypeSpotPair, ins.Type)
	assert.True(t, ins.IsActiveAt(time.Now()))
	assert.False(t, ins.IsActiveAt(time.Now().Add(-time.Hour)))

	// Duplicate pair → CONFLICT (inside its own tx so the failure rolls back cleanly).
	err = d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := repo.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{Base: sol.ID, Quote: usdc.ID, Settlement: usdc.ID, CanonicalName: "SOL/USDC dup", RiskClass: assets.RiskMajor, Status: assets.StatusActive, ActiveFrom: time.Now()})
		return err
	})
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))

	// Validation.
	err = d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := repo.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{Base: sol.ID, Quote: sol.ID, Settlement: usdc.ID, CanonicalName: "x", RiskClass: assets.RiskMajor, Status: assets.StatusActive, ActiveFrom: time.Now()})
		return err
	})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	// Listing with exact numerics.
	minNotional, _ := money.ParseQuantity("1000000") // 1 USDC
	maxNotional, _ := money.ParseQuantity("100000000000")
	listing, err := repo.CreateListing(ctx, d.Pool(), instruments.VenueListing{
		VenueID: venue.ID, InstrumentID: ins.ID, VenueNativeID: sol.MintAddress + "/" + usdc.MintAddress, Network: "solana-test",
		BaseMint: sol.MintAddress, QuoteMint: usdc.MintAddress, BasePrecision: 9, QuotePrecision: 6,
		MinNotionalQuote: minNotional, MaxNotionalQuote: &maxNotional, Status: instruments.VenueActive,
	})
	require.NoError(t, err)
	assert.Equal(t, "1000000", listing.MinNotionalQuote.String())
	require.NotNil(t, listing.MaxNotionalQuote)
	assert.Equal(t, "100000000000", listing.MaxNotionalQuote.String())
	assert.Nil(t, listing.TickSize)

	got, err := repo.ListingsForInstrument(ctx, d.Pool(), ins.ID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, venue.ID, got[0].Venue.ID)
	assert.Equal(t, listing.ID, got[0].Listing.ID)

	_, err = repo.CreateListing(ctx, d.Pool(), instruments.VenueListing{VenueID: venue.ID, InstrumentID: ins.ID, VenueNativeID: "again", Network: "solana-test", MinNotionalQuote: minNotional, Status: instruments.VenueActive})
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))

	// Status transitions: ACTIVE → CLOSE_ONLY legal; DELISTED → anything illegal; agents rejected.
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		upd, err := repo.TransitionStatus(ctx, tx, ins.ID, instruments.StatusChange{To: assets.StatusCloseOnly, ActorType: "OPERATOR", ActorID: "op-1", Reason: "liquidity thinning"})
		if err != nil {
			return err
		}
		assert.Equal(t, assets.StatusCloseOnly, upd.Status)
		assert.False(t, upd.Status.AllowsIncreasingExposure())
		assert.True(t, upd.Status.AllowsReducingExposure())
		return nil
	}))
	err = d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := repo.TransitionStatus(ctx, tx, ins.ID, instruments.StatusChange{To: assets.StatusDelisted, ActorType: "OPERATOR", ActorID: "op-1", Reason: "skip delisting"})
		return err
	})
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))
	err = d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := repo.TransitionStatus(ctx, tx, ins.ID, instruments.StatusChange{To: assets.StatusHalted, ActorType: "AGENT", ActorID: "agent-1", Reason: "nope"})
		return err
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	var n int
	require.NoError(t, d.Pool().QueryRow(ctx, `SELECT count(*) FROM instrument_status_transitions WHERE instrument_id = $1`, ins.ID).Scan(&n))
	assert.Equal(t, 1, n, "only the legal transition was recorded")

	// Venue / listing status.
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		v, err := repo.SetVenueStatus(ctx, tx, venue.ID, instruments.VenueDisabled, "OPERATOR", "op-1", "provider outage")
		if err != nil {
			return err
		}
		assert.False(t, v.Status.AllowsNewActions())
		l, err := repo.SetListingStatus(ctx, tx, listing.ID, instruments.VenueDegraded, "OPERATOR", "thin book")
		if err != nil {
			return err
		}
		assert.True(t, l.Status.AllowsNewActions())
		return nil
	}))

	// External identifiers.
	require.NoError(t, repo.AddExternalIdentifier(ctx, d.Pool(), "INSTRUMENT", ins.ID.String(), "jupiter", "SOL-USDC"))
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(repo.AddExternalIdentifier(ctx, d.Pool(), "INSTRUMENT", ins.ID.String(), "jupiter", "SOL-USDC")))
	resolved, err := repo.ResolveExternal(ctx, d.Pool(), "INSTRUMENT", "jupiter", "SOL-USDC")
	require.NoError(t, err)
	assert.Equal(t, ins.ID.String(), resolved)
	_, err = repo.ResolveExternal(ctx, d.Pool(), "INSTRUMENT", "jupiter", "missing")
	assert.True(t, errors.Is(err, errs.New(errs.CodeNotFound, "")) || errs.CodeOf(err) == errs.CodeNotFound)
}
