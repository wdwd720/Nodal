package quote_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/quote"
)

var (
	received = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	route    = json.RawMessage(`{"hops":[{"amm":"Orca","in":"USDC","out":"SOL","share_bps":10000}],"version":2}`)
	rawBody  = []byte(`{"inAmount":"250000000","outAmount":"1650000000"}`)
)

func q(n int64) money.Quantity { return money.QuantityFromInt64(n) }

func asset(a assets.AssetID) *assets.AssetID { return &a }

func listingID(t *testing.T) instruments.ListingID {
	t.Helper()
	var l instruments.ListingID
	require.NoError(t, l.UnmarshalText([]byte(id.New[id.Any]().String())))
	return l
}

// validQuote is a BUY of SOL with USDC: pay 250 USDC, expect 1.65 SOL.
func validQuote(t *testing.T) quote.Quote {
	t.Helper()
	usdc, sol := assets.NewAssetID(), assets.NewAssetID()
	qt := quote.Quote{
		ID: quote.NewQuoteID(), Provider: "jupiter", ProviderRequestID: "req-1",
		InstrumentID: instruments.NewInstrumentID(), VenueListingID: listingID(t), Side: quote.SideBuy,
		InputAssetID: usdc, InputQuantity: q(250_000000), OutputAssetID: sol,
		ExpectedOutput: q(1_650_000_000), MinimumOutput: q(1_640_000_000),
		PriceImpactBPS: 12, SlippageBPS: 50,
		EstNetworkCost: q(5_000), EstNetworkCostAssetID: asset(sol),
		EstVenueFee: q(250_000), EstVenueFeeAssetID: asset(usdc),
		PlatformFee: q(0), PlatformFeeBPS: 0,
		ReceivedAt: received, ExpiresAt: received.Add(10 * time.Second),
		RouteHash: quote.RouteHash(route), RouteSummary: route,
		RawResponseRef: "s3://evidence/q1.json", RawResponseHash: quote.HashRaw(rawBody),
	}
	qt.EffectivePrice = quote.NewEffectivePrice(q(151_515151), 6, quote.SideBuy, usdc, sol, "jupiter", received)
	return qt
}

func fieldsOf(t *testing.T, err error) map[string]any {
	t.Helper()
	e, ok := errs.As(err)
	require.True(t, ok, "expected *errs.Error, got %T: %v", err, err)
	return e.Fields
}

func TestValidate(t *testing.T) {
	t.Parallel()
	require.NoError(t, validQuote(t).Validate())
	cases := []struct {
		name  string
		mut   func(*quote.Quote)
		field string
	}{
		{"zero id", func(x *quote.Quote) { x.ID = quote.QuoteID{} }, "id"},
		{"zero intent pointer", func(x *quote.Quote) { x.IntentID = &intent.IntentID{} }, "intent_id"},
		{"empty provider", func(x *quote.Quote) { x.Provider = "" }, "provider"},
		{"blank provider", func(x *quote.Quote) { x.Provider = "  " }, "provider"},
		{"zero instrument", func(x *quote.Quote) { x.InstrumentID = instruments.InstrumentID{} }, "instrument_id"},
		{"zero listing", func(x *quote.Quote) { x.VenueListingID = instruments.ListingID{} }, "venue_listing_id"},
		{"bad side", func(x *quote.Quote) { x.Side = "SWAP" }, "side"},
		{"zero input asset", func(x *quote.Quote) { x.InputAssetID = assets.AssetID{} }, "input_asset_id"},
		{"same assets", func(x *quote.Quote) { x.OutputAssetID = x.InputAssetID }, "output_asset_id"},
		{"zero input quantity", func(x *quote.Quote) { x.InputQuantity = q(0) }, "input_quantity"},
		{"negative expected", func(x *quote.Quote) { x.ExpectedOutput = q(-1); x.MinimumOutput = q(-1) }, "expected_output"},
		{"negative minimum", func(x *quote.Quote) { x.MinimumOutput = q(-1) }, "minimum_output"},
		{"minimum above expected", func(x *quote.Quote) { x.MinimumOutput = x.ExpectedOutput.Add(q(1)) }, "minimum_output"},
		{"price invalid scale", func(x *quote.Quote) { x.EffectivePrice.Scale = 99 }, "effective_price"},
		{"price wrong quote asset", func(x *quote.Quote) { x.EffectivePrice.QuoteAsset = x.OutputAssetID.String() }, "effective_price"},
		{"price wrong source", func(x *quote.Quote) { x.EffectivePrice.Source = "other" }, "effective_price"},
		{"price wrong time", func(x *quote.Quote) { x.EffectivePrice.At = received.Add(time.Second) }, "effective_price"},
		{"impact negative", func(x *quote.Quote) { x.PriceImpactBPS = -1 }, "price_impact_bps"},
		{"slippage negative", func(x *quote.Quote) { x.SlippageBPS = -1 }, "slippage_bps"},
		{"slippage above 100%", func(x *quote.Quote) { x.SlippageBPS = 10_001 }, "slippage_bps"},
		{"network fee negative", func(x *quote.Quote) { x.EstNetworkCost = q(-1) }, "est_network_cost"},
		{"venue fee without asset", func(x *quote.Quote) { x.EstVenueFeeAssetID = nil }, "est_venue_fee_asset_id"},
		{"platform fee without asset", func(x *quote.Quote) { x.PlatformFee = q(1) }, "platform_fee_asset_id"},
		{"zero fee asset pointer", func(x *quote.Quote) { x.PlatformFeeAssetID = &assets.AssetID{} }, "platform_fee_asset_id"},
		{"platform bps above 100%", func(x *quote.Quote) { x.PlatformFeeBPS = 10_001 }, "platform_fee_bps"},
		{"platform bps without policy", func(x *quote.Quote) { x.PlatformFeeBPS = 10 }, "fee_policy_version"},
		{"zero received", func(x *quote.Quote) { x.ReceivedAt = time.Time{} }, "received_at"},
		{"expires equals received", func(x *quote.Quote) { x.ExpiresAt = x.ReceivedAt }, "expires_at"},
		{"expires before received", func(x *quote.Quote) { x.ExpiresAt = x.ReceivedAt.Add(-time.Nanosecond) }, "expires_at"},
		{"raw hash missing", func(x *quote.Quote) { x.RawResponseHash = nil }, "raw_response_hash"},
		{"raw hash wrong length", func(x *quote.Quote) { x.RawResponseHash = []byte("short") }, "raw_response_hash"},
		{"route hash missing", func(x *quote.Quote) { x.RouteHash = nil }, "route_hash"},
		{"route hash mismatch", func(x *quote.Quote) { x.RouteSummary = json.RawMessage(`{"hops":[]}`) }, "route_hash"},
		{"route summary invalid", func(x *quote.Quote) { x.RouteSummary = json.RawMessage(`{"hops":`) }, "route_summary"},
		{"provider request id control char", func(x *quote.Quote) { x.ProviderRequestID = "a\x01" }, "provider_request_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			x := validQuote(t)
			tc.mut(&x)
			err := x.Validate()
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
			assert.Contains(t, fieldsOf(t, err), tc.field)
		})
	}
	t.Run("minimum equals expected", func(t *testing.T) {
		t.Parallel()
		x := validQuote(t)
		x.MinimumOutput = x.ExpectedOutput
		assert.NoError(t, x.Validate())
	})
	t.Run("empty route summary hashes as []", func(t *testing.T) {
		t.Parallel()
		x := validQuote(t)
		x.RouteSummary = nil
		x.RouteHash = quote.RouteHash(nil)
		assert.NoError(t, x.Validate())
		assert.Equal(t, quote.RouteHash(json.RawMessage(` [ ] `)), x.RouteHash)
	})
	t.Run("sell side prices in the output asset", func(t *testing.T) {
		t.Parallel()
		x := validQuote(t)
		x.Side = quote.SideSell
		x.InputAssetID, x.OutputAssetID = x.OutputAssetID, x.InputAssetID
		x.EffectivePrice = quote.NewEffectivePrice(q(151_515151), 6, quote.SideSell, x.InputAssetID, x.OutputAssetID, "jupiter", received)
		assert.NoError(t, x.Validate())
		assert.Equal(t, x.OutputAssetID, x.QuoteAssetID())
	})
	t.Run("platform fee with policy", func(t *testing.T) {
		t.Parallel()
		x := validQuote(t)
		x.PlatformFee, x.PlatformFeeAssetID, x.PlatformFeeBPS, x.FeePolicyVersion = q(25_000), asset(x.InputAssetID), 1, "fee-v2"
		assert.NoError(t, x.Validate())
	})
}

// TestFreshness_Boundaries pins the exact-equality behavior of IsExpired
// and IsFresh.
func TestFreshness_Boundaries(t *testing.T) {
	t.Parallel()
	x := validQuote(t) // received R, expires R+10s
	r, e := x.ReceivedAt, x.ExpiresAt

	assert.False(t, x.IsExpired(e.Add(-time.Nanosecond)), "one ns before expiry is live")
	assert.True(t, x.IsExpired(e), "exactly at expiry is expired")
	assert.True(t, x.IsExpired(e.Add(time.Nanosecond)))
	assert.False(t, x.IsExpired(r))

	assert.True(t, x.IsFresh(r, time.Second), "age zero")
	assert.True(t, x.IsFresh(r.Add(500*time.Millisecond), 500*time.Millisecond), "age exactly maxAge is fresh")
	assert.False(t, x.IsFresh(r.Add(500*time.Millisecond+time.Nanosecond), 500*time.Millisecond), "one ns past maxAge is stale")
	assert.False(t, x.IsFresh(r.Add(-time.Nanosecond), time.Hour), "a quote from the future is never fresh")
	assert.False(t, x.IsFresh(e, time.Hour), "expiry dominates age")
	assert.True(t, x.IsFresh(e.Add(-time.Nanosecond), time.Hour))
	assert.False(t, x.IsFresh(r.Add(time.Second), 0), "zero max age is never fresh")
	assert.False(t, x.IsFresh(r.Add(time.Second), -time.Second))
	assert.Equal(t, 3*time.Second, x.Age(r.Add(3*time.Second)))
	assert.Equal(t, -time.Second, x.Age(r.Add(-time.Second)))
}

// TestDisclosure_Shape: expected receive is the venue's number verbatim,
// the platform fee is its own line and never folded in, and totals are per
// asset in a stable order.
func TestDisclosure_Shape(t *testing.T) {
	t.Parallel()
	x := validQuote(t)
	x.PlatformFee, x.PlatformFeeAssetID, x.PlatformFeeBPS, x.FeePolicyVersion = q(2_500_000), asset(x.OutputAssetID), 15, "fee-v1"
	x.IntentID = func() *intent.IntentID { i := intent.NewIntentID(); return &i }()
	require.NoError(t, x.Validate())

	d := x.Disclosure()
	assert.Equal(t, x.ID, d.QuoteID)
	assert.Equal(t, "jupiter", d.Provider)
	assert.True(t, d.ExpectedReceive.Equal(q(1_650_000_000)), "expected receive is reported verbatim: %s", d.ExpectedReceive)
	assert.True(t, d.MinimumReceive.Equal(q(1_640_000_000)))
	assert.Equal(t, x.OutputAssetID, d.ReceiveAssetID)
	assert.Equal(t, x.InputAssetID, d.PayAssetID)
	assert.True(t, d.PayQuantity.Equal(q(250_000000)))
	assert.Equal(t, "151.515151 "+x.InputAssetID.String(), d.EffectivePrice.String())
	assert.Equal(t, money.BPS(12), d.PriceImpactBPS)
	assert.Equal(t, money.BPS(50), d.SlippageBPS)

	// Platform fee is a separate line with its own rate and policy.
	assert.True(t, d.PlatformFee.Amount.Equal(q(2_500_000)))
	assert.Equal(t, x.OutputAssetID, *d.PlatformFee.AssetID)
	assert.Equal(t, money.BPS(15), d.PlatformFeeBPS)
	assert.Equal(t, "fee-v1", d.FeePolicyVersion)
	assert.True(t, d.VenueFee.Amount.Equal(q(250_000)))
	assert.Equal(t, x.InputAssetID, *d.VenueFee.AssetID)
	assert.True(t, d.NetworkEstimate.Amount.Equal(q(5_000)))
	assert.Equal(t, x.OutputAssetID, *d.NetworkEstimate.AssetID)

	// Totals: output asset carries network (5,000) + platform (2,500,000); input asset carries the venue fee.
	require.Len(t, d.TotalEstimatedCost, 2)
	byAsset := map[assets.AssetID]money.Quantity{}
	for _, line := range d.TotalEstimatedCost {
		byAsset[line.AssetID] = line.Amount
	}
	assert.True(t, byAsset[x.OutputAssetID].Equal(q(2_505_000)))
	assert.True(t, byAsset[x.InputAssetID].Equal(q(250_000)))
	assert.LessOrEqual(t, d.TotalEstimatedCost[0].AssetID.String(), d.TotalEstimatedCost[1].AssetID.String(), "sorted by asset id")

	assert.JSONEq(t, string(route), string(d.Route))
	assert.Equal(t, x.ReceivedAt, d.ReceivedAt)
	assert.Equal(t, x.ExpiresAt, d.ExpiresAt)

	// No fee: no total lines, expected receive untouched.
	y := validQuote(t)
	y.EstNetworkCost, y.EstNetworkCostAssetID, y.EstVenueFee, y.EstVenueFeeAssetID = q(0), nil, q(0), nil
	dy := y.Disclosure()
	assert.Empty(t, dy.TotalEstimatedCost)
	assert.True(t, dy.ExpectedReceive.Equal(y.ExpectedOutput))
	assert.Nil(t, dy.PlatformFee.AssetID)
	assert.True(t, dy.PlatformFee.Amount.IsZero())
}

func TestRouteHash_Deterministic(t *testing.T) {
	t.Parallel()
	a := json.RawMessage(`{"version":2,"hops":[{"share_bps":10000,"out":"SOL","in":"USDC","amm":"Orca"}]}`)
	b := json.RawMessage("  {\n \"hops\" : [ {\"amm\":\"Orca\", \"in\":\"USDC\", \"out\":\"SOL\", \"share_bps\":10000} ],\n \"version\": 2 }\n")
	assert.Equal(t, quote.RouteHash(route), quote.RouteHash(a), "key order does not matter")
	assert.Equal(t, quote.RouteHash(route), quote.RouteHash(b), "whitespace does not matter")
	assert.Len(t, quote.RouteHash(route), 32)
	changed := json.RawMessage(`{"hops":[{"amm":"Orca","in":"USDC","out":"SOL","share_bps":9999}],"version":2}`)
	assert.NotEqual(t, quote.RouteHash(route), quote.RouteHash(changed), "a value change changes the hash")
	assert.Equal(t, quote.RouteHash(nil), quote.RouteHash(json.RawMessage("[]")))
	assert.Equal(t, quote.RouteHash(json.RawMessage(`{"a":[1,2]}`)), quote.RouteHash(json.RawMessage(`{ "a" : [ 1 , 2 ] }`)))
	assert.NotEqual(t, quote.RouteHash(json.RawMessage(`{"a":[1,2]}`)), quote.RouteHash(json.RawMessage(`{"a":[2,1]}`)), "array order is significant")
	assert.Equal(t, quote.HashRaw(rawBody), quote.HashRaw([]byte(string(rawBody))))
	assert.NotEqual(t, quote.HashRaw(rawBody), quote.HashRaw([]byte("x")))
}

func FuzzRouteHash(f *testing.F) {
	f.Add([]byte(`{"a":1}`))
	f.Add([]byte(`[`))
	f.Add([]byte(``))
	f.Add([]byte(`"str"`))
	f.Fuzz(func(t *testing.T, b []byte) {
		h1 := quote.RouteHash(b)
		h2 := quote.RouteHash(b)
		require.Len(t, h1, 32)
		require.Equal(t, h1, h2)
	})
}

func TestSideAndIDs(t *testing.T) {
	t.Parallel()
	assert.True(t, quote.SideBuy.Valid())
	assert.True(t, quote.SideSell.Valid())
	assert.False(t, quote.Side("buy").Valid())
	id1 := quote.NewQuoteID()
	parsed, err := quote.ParseQuoteID(id1.String())
	require.NoError(t, err)
	assert.Equal(t, id1, parsed)
	_, err = quote.ParseQuoteID("nope")
	assert.Error(t, err)
}
