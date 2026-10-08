package margin

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

func TestPolicy_DefaultIsValidAndHashedDeterministically(t *testing.T) {
	p := DefaultPolicy()
	require.NoError(t, p.Validate())
	assert.Len(t, p.Hash(), 64)
	assert.Equal(t, p.Hash(), DefaultPolicy().Hash())
	q := DefaultPolicy()
	q.LiquidationFeeBPS++
	assert.NotEqual(t, p.Hash(), q.Hash(), "any change is a different policy")
}

func TestPolicy_ValidateRefusesUnsafeRules(t *testing.T) {
	cases := map[string]func(*Policy){
		"no version":                    func(p *Policy) { p.Version = "" },
		"maintenance above initial":     func(p *Policy) { p.Classes[1].MaintenanceMarginBPS = p.Classes[1].InitialMarginBPS + 1 },
		"riskier class lower haircut":   func(p *Policy) { p.Classes[4].HaircutBPS = 0 },
		"riskier class lower margin":    func(p *Policy) { p.Classes[3].InitialMarginBPS = 100; p.Classes[3].MaintenanceMarginBPS = 50 },
		"missing class":                 func(p *Policy) { p.Classes = p.Classes[:4] },
		"duplicate class":               func(p *Policy) { p.Classes = append(p.Classes, p.Classes[0]) },
		"illiquid treated as liquid":    func(p *Policy) { p.Liquidity[3].HaircutBPS = 0 },
		"venue tier 3 safer":            func(p *Policy) { p.Venues[2].HaircutBPS = 0 },
		"cash account with leverage":    func(p *Policy) { p.LeverageCaps.CashBPS = 20_000 },
		"cross cap below 1x":            func(p *Policy) { p.LeverageCaps.CrossBPS = 5_000 },
		"no oracle age":                 func(p *Policy) { p.OracleMaxAgeMS = 0 },
		"aging past the max":            func(p *Policy) { p.OracleAgingMS = p.OracleMaxAgeMS + 1 },
		"lookback shorter than max age": func(p *Policy) { p.MarkLookbackMS = 1 },
		"threshold at 100%":             func(p *Policy) { p.ConcentrationThresholdBPS = 10_000 },
		"no step budget":                func(p *Policy) { p.MaxLiquidationSteps = 0 },
		"unknown target":                func(p *Policy) { p.PartialTarget = "EVERYTHING" },
		"bad asset rule id":             func(p *Policy) { p.AssetRules = []AssetRule{{AssetID: "SOL", RiskWeightBPS: 10_000}} },
		"weight out of range": func(p *Policy) {
			p.AssetRules = []AssetRule{{AssetID: "0193b2e0-0000-7000-8000-000000000001", RiskWeightBPS: 1}}
		},
		"rate out of range": func(p *Policy) { p.BorrowRateBPS = 200_000 },
		"zero partial fill": func(p *Policy) { p.StressPartialFillBPS = 0 },
		// A forced sale at a slippage and fee above haircut plus maintenance
		// would deepen the shortfall it is meant to cure.
		"liquidation could deepen the shortfall": func(p *Policy) {
			p.LiquidationFeeBPS = 4_000
			p.LiquidationSlippageCapBPS = 2_000
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := DefaultPolicy()
			mutate(&p)
			err := p.Validate()
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

func TestPolicy_ProfileForMapsRiskClassesAndRules(t *testing.T) {
	p := DefaultPolicy()
	prof, ok := p.ProfileFor("0193b2e0-0000-7000-8000-000000000001", "MAJOR")
	require.True(t, ok)
	assert.Equal(t, VolLow, prof.Volatility)
	assert.Equal(t, LiqNormal, prof.Liquidity)
	assert.Equal(t, money.BPS(10_000), prof.RiskWeightBPS)
	_, ok = p.ProfileFor("0193b2e0-0000-7000-8000-000000000001", "UNSUPPORTED")
	assert.False(t, ok, "an unmapped risk class is not eligible")

	p.AssetRules = []AssetRule{{AssetID: "0193b2e0-0000-7000-8000-000000000001", RiskWeightBPS: 20_000, Volatility: VolHigh, Liquidity: LiqThin}}
	require.NoError(t, p.Validate())
	prof, ok = p.ProfileFor("0193b2e0-0000-7000-8000-000000000001", "UNSUPPORTED")
	require.True(t, ok, "an asset rule can make an asset eligible")
	assert.Equal(t, VolHigh, prof.Volatility)
	assert.Equal(t, LiqThin, prof.Liquidity)
	assert.Equal(t, money.BPS(20_000), prof.RiskWeightBPS)
}

func TestParsePolicy_RoundTripsAndRefusesMalformedDocuments(t *testing.T) {
	p := DefaultPolicy()
	b, err := json.Marshal(p)
	require.NoError(t, err)
	back, err := ParsePolicy(b)
	require.NoError(t, err)
	assert.Equal(t, p.Hash(), back.Hash())

	for name, doc := range map[string]string{
		"unknown field": `{"version":"x","surprise":1}`,
		"trailing data": string(b) + `{}`,
		"not json":      `version: x`,
		"a float":       `{"version":"x","borrow_rate_bps":8.5}`,
		"invalid rules": `{"version":"x"}`,
	} {
		_, err := ParsePolicy([]byte(doc))
		require.Error(t, err, name)
	}
}

// FuzzParsePolicy: the decoder never panics, and anything it accepts is a
// valid policy whose hash is stable across a re-encode.
func FuzzParsePolicy(f *testing.F) {
	b, err := json.Marshal(DefaultPolicy())
	if err != nil {
		f.Fatal(err)
	}
	f.Add(b)
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"version":"v","classes":[{"class":"STABLE","haircut_bps":-1}]}`))
	f.Fuzz(func(t *testing.T, doc []byte) {
		p, err := ParsePolicy(doc)
		if err != nil {
			return
		}
		require.NoError(t, p.Validate())
		again, err := json.Marshal(p)
		require.NoError(t, err)
		q, err := ParsePolicy(again)
		require.NoError(t, err)
		require.Equal(t, p.Hash(), q.Hash())
	})
}
