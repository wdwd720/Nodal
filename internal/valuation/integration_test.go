//go:build integration

package valuation

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
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
	d, err := db.Open(ctx, db.Config{URL: testAppURL, AppName: "valuation-itest", MaxConns: 4})
	require.NoError(t, err)
	t.Cleanup(d.Close)
	return d
}

// seedAsset registers a unique asset so parallel packages sharing the
// database never collide on (chain, mint_address).
func seedAsset(t *testing.T, d *db.DB, symbol string, decimals uint8, stable bool) assets.Asset {
	t.Helper()
	mint := "mint-" + id.New[id.Any]().String()
	kind := assets.KindSPLToken
	a := assets.Asset{
		Chain: "solana-devnet", MintAddress: mint, Kind: kind, ValueDomain: valuedomain.SelfCustodialCrypto,
		Symbol: symbol, Name: symbol, Decimals: decimals,
		IsStablecoin: stable, RiskClass: assets.RiskStandard, Status: assets.StatusActive,
	}
	if stable {
		a.PegCurrency = "USD"
		a.RiskClass = assets.RiskSettlement
	}
	created, err := assets.NewRepository().Create(t.Context(), d, a)
	require.NoError(t, err)
	return created
}

func TestIntegration_PolicyStore_CurrentIsLatestEffectiveUnexpired(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	sol := seedAsset(t, d, "SOL", 9, false)
	store := NewPolicyStore()
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	// Nothing recorded: fail closed.
	p, err := store.Current(ctx, d, sol.ID, t0)
	require.NoError(t, err)
	assert.True(t, p.PolicyMissing)
	assert.Equal(t, assets.StatusRestricted, p.Status)
	assert.Zero(t, p.CollateralFactor)

	base := validNewPolicy()
	base.AssetID = sol.ID
	base.EffectiveAt = t0
	base.PolicyVersion = "v1"
	v1, err := store.RecordPolicy(ctx, d, base)
	require.NoError(t, err)
	assert.Equal(t, "v1", v1.PolicyVersion)
	assert.Equal(t, 30*time.Second, v1.MaxPriceAge)
	assert.Equal(t, money.BPS(8000), v1.CollateralFactor)
	assert.False(t, v1.PolicyMissing)

	// A future policy does not apply yet.
	future := base
	future.EffectiveAt = t0.Add(48 * time.Hour)
	future.PolicyVersion = "v3"
	future.CollateralFactor = 5000
	_, err = store.RecordPolicy(ctx, d, future)
	require.NoError(t, err)

	// A temporary override that expires.
	exp := t0.Add(2 * time.Hour)
	temp := base
	temp.EffectiveAt = t0.Add(time.Hour)
	temp.ExpiresAt = &exp
	temp.PolicyVersion = "v2-temp"
	temp.Status = assets.StatusCloseOnly
	_, err = store.RecordPolicy(ctx, d, temp)
	require.NoError(t, err)

	cases := []struct {
		at      time.Time
		version string
	}{
		{t0.Add(-time.Second), MissingPolicyVersion},
		{t0, "v1"},
		{t0.Add(time.Hour), "v2-temp"},
		{t0.Add(2*time.Hour - time.Nanosecond), "v2-temp"},
		{t0.Add(2 * time.Hour), "v1"}, // expired override falls back
		{t0.Add(48 * time.Hour), "v3"},
	}
	for _, tc := range cases {
		got, err := store.Current(ctx, d, sol.ID, tc.at)
		require.NoError(t, err)
		assert.Equal(t, tc.version, got.PolicyVersion, "at %s", tc.at)
		assert.Equal(t, tc.version == MissingPolicyVersion, got.PolicyMissing)
	}

	// Same effective_at recorded twice: the later row wins.
	again := base
	again.PolicyVersion = "v1b"
	_, err = store.RecordPolicy(ctx, d, again)
	require.NoError(t, err)
	got, err := store.Current(ctx, d, sol.ID, t0.Add(30*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, "v1b", got.PolicyVersion)
}

func TestIntegration_PolicyStore_RejectsAgentAndUnknownAsset(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	sol := seedAsset(t, d, "SOL", 9, false)
	store := NewPolicyStore()

	agent := validNewPolicy()
	agent.AssetID = sol.ID
	agent.ActorType = "AGENT"
	_, err := store.RecordPolicy(ctx, d, agent)
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	// The schema enforces the same rule independently of the Go check.
	_, err = d.Exec(ctx, `INSERT INTO asset_policies (id, asset_id, status, collateral_factor_bps, max_price_age_ms, policy_version, effective_at, created_by_actor_type, created_by_actor_id, reason)
		VALUES ($1,$2,'ACTIVE',8000,30000,'x',now(),'AGENT','agent-1','bypass attempt')`, NewPolicyID(), sol.ID)
	require.Error(t, err)
	assert.True(t, db.IsCheckViolation(err), "SQLSTATE %s", db.SQLState(err))

	missing := validNewPolicy()
	missing.AssetID = assets.NewAssetID()
	_, err = store.RecordPolicy(ctx, d, missing)
	require.Error(t, err)
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))

	// Policy history is immutable.
	rec, err := store.RecordPolicy(ctx, d, func() NewPolicy { p := validNewPolicy(); p.AssetID = sol.ID; return p }())
	require.NoError(t, err)
	_, err = d.Exec(ctx, `UPDATE asset_policies SET collateral_factor_bps = 10000 WHERE id = $1`, rec.ID)
	require.Error(t, err)
	assert.True(t, db.IsMutationForbidden(err), "got %v", err)
}

func TestIntegration_PriceStore_LatestAndStaleness(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	sol := seedAsset(t, d, "SOL", 9, false)
	usdc := seedAsset(t, d, "USDC", 6, true)
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)
	store := NewPriceStore(clk)

	obs := func(decimal string, at time.Time, source string) PriceObservation {
		p, err := money.PriceFromDecimalString(decimal, "x", source, at)
		require.NoError(t, err)
		return PriceObservation{AssetID: sol.ID, QuoteAssetID: usdc.ID, Mantissa: p.Mantissa, Scale: p.Scale, Source: source, ObservedAt: at}
	}

	// Nothing yet: stale.
	_, err := store.Latest(ctx, d, sol.ID, usdc.ID, time.Minute, now)
	require.Error(t, err)
	assert.Equal(t, errs.CodeStaleMarketData, errs.CodeOf(err))

	old, err := store.RecordPrice(ctx, d, obs("149.000000", now.Add(-2*time.Minute), "pyth"))
	require.NoError(t, err)
	assert.False(t, old.Duplicate)
	fresh, err := store.RecordPrice(ctx, d, obs("150.123456", now.Add(-10*time.Second), "pyth"))
	require.NoError(t, err)
	assert.False(t, fresh.Duplicate)

	// Idempotent replay of the same identity and value.
	dup, err := store.RecordPrice(ctx, d, obs("150.123456", now.Add(-10*time.Second), "pyth"))
	require.NoError(t, err)
	assert.True(t, dup.Duplicate)
	assert.Equal(t, fresh.ID, dup.ID)

	// Same identity, different value: CONFLICT, never silently ignored.
	_, err = store.RecordPrice(ctx, d, obs("151.000000", now.Add(-10*time.Second), "pyth"))
	require.Error(t, err)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))

	// Within max age: the freshest observation, correctly typed.
	p, err := store.Latest(ctx, d, sol.ID, usdc.ID, 30*time.Second, now)
	require.NoError(t, err)
	assert.Equal(t, "150.123456 "+usdc.ID.String(), p.String())
	assert.Equal(t, "pyth", p.Source)
	assert.Equal(t, now.Add(-10*time.Second), p.At)
	assert.Equal(t, int32(6), p.Scale)

	// Boundary: age == maxAge is fresh, one microsecond (the timestamptz
	// resolution) more is stale.
	_, err = store.Latest(ctx, d, sol.ID, usdc.ID, 10*time.Second, now)
	require.NoError(t, err)
	_, err = store.Latest(ctx, d, sol.ID, usdc.ID, 10*time.Second-time.Microsecond, now)
	require.Error(t, err)
	assert.Equal(t, errs.CodeStaleMarketData, errs.CodeOf(err))

	// An older window still finds the 2-minute-old observation; ordering by
	// observed_at wins over received_at.
	//
	// The as-of instant used to be now-1m, and every row here is RECEIVED at
	// now -- so under F-119's knowledge-time bound the honest answer to "what
	// did we know a minute ago" is "none of this". That the test expected an
	// answer was the defect, in miniature: an as-of read returning rows the
	// platform had not yet received.
	//
	// The point it was making is about ORDERING, so it is made at an instant
	// where the rows are known: both were received at now, and the fresher
	// observation is excluded by the WINDOW rather than by the as-of bound.
	late, err := store.RecordPrice(ctx, d, obs("148.500000", now.Add(-3*time.Minute), "rpc-fallback"))
	require.NoError(t, err)
	assert.False(t, late.Duplicate)
	p, err = store.Latest(ctx, d, sol.ID, usdc.ID, 5*time.Minute, now)
	require.NoError(t, err)
	assert.Equal(t, "150.123456", p.Mantissa.ToDecimalString(uint8(p.Scale)),
		"the freshest observed_at wins, whatever order the rows arrived in")

	// And with the freshest two outside the window, the three-minute-old one is
	// what remains -- which is the ordering claim, stated without asking an
	// as-of question the data cannot answer.
	p, err = store.Latest(ctx, d, sol.ID, usdc.ID, 4*time.Minute, now.Add(-90*time.Second))
	require.Error(t, err, "at an instant before these rows were received, nothing is known")
	assert.Equal(t, errs.CodeStaleMarketData, errs.CodeOf(err))

	// The reverse pair is a different series.
	_, err = store.Latest(ctx, d, usdc.ID, sol.ID, time.Hour, now)
	require.Error(t, err)
	assert.Equal(t, errs.CodeStaleMarketData, errs.CodeOf(err))

	// received_at is stamped from the injected clock when not supplied.
	var received time.Time
	require.NoError(t, d.QueryRow(ctx, `SELECT received_at FROM asset_prices WHERE id = $1`, fresh.ID).Scan(&received))
	assert.Equal(t, now, received.UTC())
}

// An as-of read is bounded by knowledge time as well as event time (F-119).
//
// observed_at is when the price happened; received_at is when this platform
// learned it. Latest bounded only the first, and its own doc comment said
// "an as-of valuation never looks ahead" -- so a price observed at T-1m but
// INSERTED at T+5m was returned by Latest(..., now=T): an answer the platform
// did not have yet.
//
// Latent rather than live, because the sole production caller passes a live
// clock and at a live now such a row does not exist. It stops being latent the
// moment anything replays as of a past instant, which is the entire reason
// received_at is stored. The same table's other reader, prediction/outcome.go,
// already filters on it.
func TestIntegration_PriceStore_AnAsOfReadDoesNotSeeWhatItDidNotKnow(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)
	store := NewPriceStore(clk)
	sol, usdc := seedAsset(t, d, "SOLKT", 9, false), seedAsset(t, d, "USDKT", 6, true)

	obs := func(mantissa string, observed, received time.Time) PriceObservation {
		q, err := money.ParseQuantity(mantissa)
		require.NoError(t, err)
		return PriceObservation{
			AssetID: sol.ID, QuoteAssetID: usdc.ID, Mantissa: q, Scale: 6,
			Source: "pyth", ObservedAt: observed, ReceivedAt: received,
		}
	}

	// Observed before the as-of instant, learned five minutes after it.
	asOf := now.Add(-10 * time.Minute)
	_, err := store.RecordPrice(ctx, d, obs("999000000", asOf.Add(-time.Minute), asOf.Add(5*time.Minute)))
	require.NoError(t, err)

	_, err = store.Latest(ctx, d, sol.ID, usdc.ID, time.Hour, asOf)
	require.Error(t, err, "an as-of read returned a price the platform had not received yet")
	assert.Equal(t, errs.CodeStaleMarketData, errs.CodeOf(err))

	// The control: the same observation IS visible once the as-of instant is
	// past the moment it was learned, so the bound is on knowledge time and not
	// on the row.
	p, err := store.Latest(ctx, d, sol.ID, usdc.ID, time.Hour, asOf.Add(6*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, "999.000000", p.Mantissa.ToDecimalString(uint8(p.Scale)))
}
