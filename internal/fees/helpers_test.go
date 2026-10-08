package fees

import (
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Fixed identities and times so that canonical forms and hashes in these
// tests are reproducible.
var (
	quoteAsset = mustAsset("01920000-0000-7000-8000-0000000000a1")
	solAsset   = mustAsset("01920000-0000-7000-8000-0000000000b2")
	refTime    = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
)

func mustAsset(s string) assets.AssetID {
	id, err := assets.ParseAssetID(s)
	if err != nil {
		panic(err)
	}
	return id
}

// refPrice is a reference price in quoteAsset observed at refTime.
func refPrice(mantissa string, scale int32) money.Price {
	return money.Price{Mantissa: q(mantissa), Scale: scale, QuoteAsset: quoteAsset.String(), Source: "pyth", At: refTime}
}

func usd(t testing.TB, s string) money.USD {
	t.Helper()
	u, err := money.ParseUSD(s)
	require.NoError(t, err)
	return u
}

// workedSchedule is the schedule of the worked example in quote_test.go:
// USDC (6 decimals), 25 bps spread, 0.50 fixed, 10 bps platform fee, 5 bps
// venue pass-through, 0.000005 SOL network, 50 bps reserve, 25% subscription
// discount, 20 bps strategy fee, no bounds, customer-favourable rounding.
func workedSchedule() Schedule {
	return Schedule{
		Version:                 "fees-sched-v1",
		QuoteAsset:              quoteAsset,
		QuoteDecimals:           6,
		SpreadBPS:               25,
		FixedFee:                q("500000"),
		PercentageFeeBPS:        10,
		VenuePassThroughBPS:     5,
		NetworkFee:              NetworkFeeEstimate{Asset: solAsset, Amount: q("5000")},
		SlippageReserveBPS:      50,
		SubscriptionDiscountBPS: 2_500,
		StrategyFeeBPS:          20,
		Rounding:                DefaultRounding(),
	}
}

// workedInput is 1.5 SOL (9 decimals) at 150.123456 USDC.
func workedInput(side Side) QuoteInput {
	return QuoteInput{Side: side, BaseQty: q("1500000000"), BaseDecimals: 9, QuoteDecimals: 6, ReferencePrice: refPrice("150123456", 6)}
}

func mustQuote(t testing.TB, s Schedule, in QuoteInput) Quote {
	t.Helper()
	quote, err := s.Quote(in)
	require.NoError(t, err)
	require.NoError(t, VerifyQuote(s, in, quote), "every quote the engine produces must verify")
	return quote
}

// fieldKeys returns the sorted Fields keys of an *errs.Error.
func fieldKeys(err error) []string {
	e, ok := errs.As(err)
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// requireFields asserts err is an *errs.Error with code whose Fields include
// every key.
func requireFields(t testing.TB, err error, code errs.Code, keys ...string) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, code, errs.CodeOf(err), "error: %v", err)
	got := fieldKeys(err)
	for _, k := range keys {
		require.Contains(t, got, k, "error fields: %v", got)
	}
}

// strs renders quantities for compact table assertions.
func strs(qs ...money.Quantity) []string {
	out := make([]string, len(qs))
	for i, v := range qs {
		out[i] = v.String()
	}
	return out
}
