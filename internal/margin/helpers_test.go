package margin

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
)

const (
	usdID = "usd"
	solID = "sol"
	ethID = "eth"
	stbID = "stb"
)

var t0 = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// px is a mark in the test numeraire: s quote units per whole base unit.
func px(t require.TestingT, s string, at time.Time) *money.Price {
	if h, ok := t.(interface{ Helper() }); ok {
		h.Helper()
	}
	p, err := money.PriceFromDecimalString(s, usdID, "test", at)
	require.NoError(t, err)
	return &p
}

// usd is n whole paper dollars in numeraire base units (6 decimals).
func usd(n int64) money.Quantity { return money.QuantityFromInt64(n * 1_000_000) }

// units parses a decimal number of paper dollars into numeraire base units.
func units(t testing.TB, s string) money.Quantity {
	t.Helper()
	q, err := money.QuantityFromDecimalString(s, 6, money.RoundExact)
	require.NoError(t, err)
	return q
}

// sol is n whole SOL in base units (9 decimals).
func sol(n int64) money.Quantity { return money.QuantityFromInt64(n * 1_000_000_000) }

func lowDeep() AssetProfile {
	return AssetProfile{Volatility: VolLow, Liquidity: LiqDeep, Venue: VenueTier1, RiskWeightBPS: 10_000}
}

// fixedAccount: 1,000 paper dollars and 20 SOL at 100 held as collateral
// against a 2,000 dollar loan, CROSS margin, at t0.
func fixedAccount(t testing.TB) AccountState {
	return AccountState{
		AccountID: "acct-1", Mode: ModeCross, NumeraireAssetID: usdID, NumeraireDecimals: 6,
		Holdings: []Holding{
			{AssetID: usdID, Decimals: 6, Quantity: usd(1_000), Numeraire: true},
			{AssetID: solID, Decimals: 9, Quantity: sol(20), Mark: px(t, "100", t0), Profile: lowDeep()},
		},
		Loans:       []Loan{{LotID: "lot-1", Principal: usd(2_000), Interest: money.Quantity{}, RateBPS: 800, AccruedThrough: t0}},
		Derivatives: []DerivativeExposure{},
		Now:         t0,
	}
}

func mustEval(t testing.TB, p Policy, s AccountState) Evaluation {
	t.Helper()
	e, err := Evaluate(p, s)
	require.NoError(t, err)
	return e
}

func requireQ(t testing.TB, want, got money.Quantity, msg string) {
	t.Helper()
	require.Truef(t, want.Equal(got), "%s: want %s got %s", msg, want, got)
}

func riskBase() risk.AdvancedPolicy { return risk.DefaultAdvancedPolicy() }
