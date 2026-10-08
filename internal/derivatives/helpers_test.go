package derivatives

import (
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

func q(n int64) money.Quantity { return money.QuantityFromInt64(n) }

// testAssets returns a sandbox dollar (6 decimals) and a sandbox bitcoin (8
// decimals, MAJOR).
func testAssets() (usd, btc assets.Asset) {
	usd = assets.Asset{ID: assets.NewAssetID(), Chain: assets.SandboxChain, Kind: assets.KindSimulated,
		ValueDomain: valuedomain.Simulated, Symbol: "SBXUSD", Decimals: 6, RiskClass: assets.RiskSettlement, Status: assets.StatusActive}
	btc = assets.Asset{ID: assets.NewAssetID(), Chain: assets.SandboxChain, Kind: assets.KindSimulated,
		ValueDomain: valuedomain.Simulated, Symbol: "SBXBTC", Decimals: 8, RiskClass: assets.RiskMajor, Status: assets.StatusActive}
	return usd, btc
}

// testTerms returns derivative terms on BTC settling in USD: price scale 2,
// tick 0.50, one contract = 0.01 BTC, so one tick of one contract is 5 000
// base units ($0.005) and a contract at $65 000.00 is worth 650 000 000.
func testTerms(kind instruments.InstrumentType, usd, btc assets.Asset) instruments.Terms {
	under := instruments.NewInstrumentID()
	t := instruments.Terms{
		Kind: kind, Symbol: "SBXBTC-" + string(kind), VenueCode: "NODAL-SANDBOX", SettlementAssetID: usd.ID,
		BaseAssetID: &btc.ID, UnderlyingInstrumentID: &under, ContractMultiplier: q(1_000_000), PriceScale: 2,
		TickSize: q(50), LotSize: q(1), MinQuantity: q(1), SettlementStyle: instruments.SettleCash,
		TradingCalendar: Calendar24x7, OracleMaxAgeMS: 60_000, OracleSources: []string{"sandbox"},
		CorporateActionBehavior: instruments.CorporateActionsNone, Transferability: instruments.NonTransferable,
	}
	expiry := time.Date(2026, 12, 25, 8, 0, 0, 0, time.UTC)
	switch kind {
	case instruments.TypePerpetual:
		t.MarginModel, t.FundingIntervalMS = instruments.MarginLinear, 8*3600*1000
	case instruments.TypeFuture:
		t.MarginModel, t.Expiry = instruments.MarginLinear, &expiry
	case instruments.TypeOption:
		k := q(6_500_000)
		t.MarginModel, t.Expiry, t.StrikeMantissa = instruments.MarginOption, &expiry, &k
		t.OptionRight, t.ExerciseStyle = instruments.Call, instruments.European
	}
	return t
}

// tb is what the helpers need from *testing.T and *rapid.T alike.
type tb interface {
	Helper()
	require.TestingT
}

func specFor(t tb, terms instruments.Terms, usd, btc assets.Asset) Spec {
	t.Helper()
	ins := instruments.Instrument{ID: instruments.NewInstrumentID(), Type: terms.Kind, SettlementAssetID: usd.ID, Status: assets.StatusActive}
	s, err := NewSpec(ins, terms, usd, btc)
	require.NoError(t, err)
	return s
}

func perpSpec(t tb) Spec {
	usd, btc := testAssets()
	return specFor(t, testTerms(instruments.TypePerpetual, usd, btc), usd, btc)
}

func futureSpec(t tb) Spec {
	usd, btc := testAssets()
	return specFor(t, testTerms(instruments.TypeFuture, usd, btc), usd, btc)
}

func optionSpec(t tb, right instruments.OptionRight, strike int64) Spec {
	usd, btc := testAssets()
	terms := testTerms(instruments.TypeOption, usd, btc)
	k := q(strike)
	terms.StrikeMantissa, terms.OptionRight = &k, right
	return specFor(t, terms, usd, btc)
}

// usd returns a dollar amount (a whole number of dollars) in base units.
func usdUnits(dollars int64) money.Quantity { return q(dollars).Mul(q(1_000_000)) }
