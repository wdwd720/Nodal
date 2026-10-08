package instruments

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/money"
)

func q(n int64) money.Quantity { return money.QuantityFromInt64(n) }

func baseTerms(kind InstrumentType) Terms {
	settle := assets.NewAssetID()
	base, quote := assets.NewAssetID(), assets.NewAssetID()
	under := NewInstrumentID()
	exp := time.Date(2026, 12, 31, 16, 0, 0, 0, time.UTC)
	strike := q(6_500_000)
	t := Terms{
		Kind: kind, Symbol: "SBX-" + string(kind), VenueCode: "NODAL-SANDBOX", SettlementAssetID: settle,
		ContractMultiplier: q(1), PriceScale: 2, TickSize: q(1), LotSize: q(1), MinQuantity: q(1), QuantityDecimals: 0,
		SettlementStyle: SettleCash, TradingCalendar: "24x7", OracleMaxAgeMS: 30_000,
		MarginModel: MarginNone, CorporateActionBehavior: CorporateActionsNone, Transferability: Transferable,
	}
	switch kind {
	case TypeSpotPair, TypeStableValue:
		t.BaseAssetID, t.QuoteAssetID = &base, &quote
	case TypeEquity, TypeETF:
		t.BaseAssetID, t.ExchangeMIC, t.TradingCalendar, t.CorporateActionBehavior = &base, "XNYS", "XNYS", CorporateActionsAdjust
		t.QuantityDecimals = 6
	case TypeFuture:
		t.UnderlyingInstrumentID, t.Expiry, t.MarginModel = &under, &exp, MarginLinear
	case TypePerpetual:
		t.UnderlyingInstrumentID, t.FundingIntervalMS, t.MarginModel = &under, 8*3600*1000, MarginLinear
	case TypeOption:
		t.UnderlyingInstrumentID, t.Expiry, t.StrikeMantissa = &under, &exp, &strike
		t.OptionRight, t.ExerciseStyle, t.MarginModel = Call, European, MarginOption
	case TypeEventOutcome:
		t.Outcomes, t.Expiry, t.MarginModel = []string{"YES", "NO"}, &exp, MarginBinaryOutcome
	case TypeSynthetic:
		t.UnderlyingInstrumentID = &under
	}
	return t
}

func TestTerms_EveryKindHasAValidShape(t *testing.T) {
	require.Len(t, AllTypes(), 9)
	for _, k := range AllTypes() {
		require.NoError(t, baseTerms(k).Validate(), "%s", k)
		assert.Equal(t, k != TypeSpotPair, k.Advanced())
	}
}

func TestTerms_KindRulesFailClosed(t *testing.T) {
	cases := map[string]func() Terms{
		"future without expiry": func() Terms { x := baseTerms(TypeFuture); x.Expiry = nil; return x },
		"perpetual that expires": func() Terms {
			x := baseTerms(TypePerpetual)
			e := time.Now()
			x.Expiry = &e
			return x
		},
		"perpetual without funding": func() Terms { x := baseTerms(TypePerpetual); x.FundingIntervalMS = 0; return x },
		"option without strike":     func() Terms { x := baseTerms(TypeOption); x.StrikeMantissa = nil; return x },
		"option with no right":      func() Terms { x := baseTerms(TypeOption); x.OptionRight = ""; return x },
		"strike on a future": func() Terms {
			x := baseTerms(TypeFuture)
			s := q(1)
			x.StrikeMantissa = &s
			return x
		},
		"equity without MIC":        func() Terms { x := baseTerms(TypeEquity); x.ExchangeMIC = ""; return x },
		"equity not adjusted":       func() Terms { x := baseTerms(TypeEquity); x.CorporateActionBehavior = CorporateActionsNone; return x },
		"single-outcome market":     func() Terms { x := baseTerms(TypeEventOutcome); x.Outcomes = []string{"YES"}; return x },
		"duplicate outcomes":        func() Terms { x := baseTerms(TypeEventOutcome); x.Outcomes = []string{"YES", "YES"}; return x },
		"outcomes on a spot pair":   func() Terms { x := baseTerms(TypeSpotPair); x.Outcomes = []string{"A", "B"}; return x },
		"zero tick":                 func() Terms { x := baseTerms(TypeSpotPair); x.TickSize = q(0); return x },
		"min quantity not in lots":  func() Terms { x := baseTerms(TypeSpotPair); x.LotSize = q(3); x.MinQuantity = q(4); return x },
		"no oracle freshness bound": func() Terms { x := baseTerms(TypeSpotPair); x.OracleMaxAgeMS = 0; return x },
		"lower-case symbol":         func() Terms { x := baseTerms(TypeSpotPair); x.Symbol = "btc-usd"; return x },
	}
	for name, build := range cases {
		assert.Error(t, build().Validate(), name)
	}
}

func TestTerms_HashIsCanonical(t *testing.T) {
	a := baseTerms(TypeEquity)
	a.JurisdictionTags = []string{"US", "EU", "US"}
	a.OracleSources = []string{"b", "a"}
	b := a
	b.JurisdictionTags = []string{"EU", "US"}
	b.OracleSources = []string{"a", "b"}
	ha, err := a.ContentHash()
	require.NoError(t, err)
	hb, err := b.ContentHash()
	require.NoError(t, err)
	assert.Equal(t, ha, hb, "order and duplicates of tag lists do not change the terms")
	c := b
	c.Symbol = "SBX-OTHER"
	hc, err := c.ContentHash()
	require.NoError(t, err)
	assert.NotEqual(t, ha, hc)
}
