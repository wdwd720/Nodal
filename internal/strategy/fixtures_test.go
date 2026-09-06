package strategy_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/strategy"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// Fixed identifiers, so a corpus expectation is stable between runs.
const (
	fxStrategyID   = "0192b8e0-1a2b-7c3d-8e4f-5a6b7c8d9e0f"
	fxAccountID    = "0192b8e0-1a2b-7c3d-8e4f-5a6b7c8d9e10"
	fxUserID       = "0192b8e0-1a2b-7c3d-8e4f-5a6b7c8d9e11"
	fxInstrumentID = "0192b8e0-1a2b-7c3d-8e4f-5a6b7c8d9e12"
	fxHaltedID     = "0192b8e0-1a2b-7c3d-8e4f-5a6b7c8d9e14"
)

var fxNow = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

func refExpr(s string) *ir.Ref { r := ir.Ref(s); return &r }

func usdPtr(minor int64) *money.USD { u := money.USDFromMinor(minor); return &u }

func bpsPtr(v money.BPS) *money.BPS { return &v }

func intPtr(v int) *int { return &v }

func int64Ptr(v int64) *int64 { return &v }

func boolPtr(v bool) *bool { return &v }

func decOf(t *testing.T, s string) ir.Decimal {
	t.Helper()
	d, err := ir.ParseDecimalString(s)
	require.NoError(t, err)
	return d
}

func constOf(t *testing.T, s string) ir.Expr {
	t.Helper()
	d := decOf(t, s)
	return ir.Expr{Const: &d}
}

func exprPtr(e ir.Expr) *ir.Expr { return &e }

func everyMS(ms int64) *int64 { return &ms }

// testPolicy is the risk policy the corpus validates against: deliberately
// tight, so "use all available funds" and an oversized trade both fail.
func testPolicy() risk.Policy {
	p := risk.MustParsePolicy(json.RawMessage(risk.DefaultGlobalPolicyJSON))
	p.Version = "risk-v1"
	p.AllowedVenues = []string{"JUPITER", "ORCA"}
	p.MaxDataAgeMS = map[string]int64{"price": 500, "wallet_event": 2000, "feature": 300_000}
	p.MaxOrdersPerHour = intPtr(30)
	p.MaxSingleTradeUSD = usdPtr(1000_00)
	p.MaxPositionUSD = usdPtr(5000_00)
	p.MaxDailyLossUSD = usdPtr(500_00)
	p.MaxSlippageBPS = bpsPtr(100)
	p.MaxFeeBPS = bpsPtr(50)
	p.MaxPriceImpactBPS = bpsPtr(100)
	p.MaxQuoteAgeMS = int64Ptr(3000)
	p.AllowRiskReductionDuringKill = boolPtr(true)
	p.BlockRiskReductionOnAccountFreeze = boolPtr(false)
	return p
}

// testRefs is the registry snapshot the corpus validates against.
func testRefs() strategy.ValidationRefs {
	policy := testPolicy()
	activeID, _ := instruments.ParseInstrumentID(fxInstrumentID)
	haltedID, _ := instruments.ParseInstrumentID(fxHaltedID)
	active := instruments.Instrument{
		ID:         activeID,
		Status:     assets.StatusActive,
		ActiveFrom: fxNow.Add(-24 * time.Hour),
	}
	halted := instruments.Instrument{
		ID:         haltedID,
		Status:     assets.StatusHalted,
		ActiveFrom: fxNow.Add(-24 * time.Hour),
	}
	return strategy.ValidationRefs{
		Instruments: map[string]instruments.Instrument{
			fxInstrumentID: active,
			fxHaltedID:     halted,
		},
		Venues: map[string]instruments.Venue{
			"JUPITER": {Code: "JUPITER", Status: instruments.VenueActive},
			"ORCA":    {Code: "ORCA", Status: instruments.VenueActive},
			"DEADDEX": {Code: "DEADDEX", Status: instruments.VenueDisabled},
		},
		Tools: map[string]strategy.Tool{
			strategy.ToolKey("price_spot", 1):     {Code: "price_spot", Version: 1, Effect: ir.EffectReadMarketData, Status: "ACTIVE"},
			strategy.ToolKey("volume_feature", 1): {Code: "volume_feature", Version: 1, Effect: ir.EffectReadMarketData, Status: "ACTIVE"},
			strategy.ToolKey("wallet_events", 1):  {Code: "wallet_events", Version: 1, Effect: ir.EffectReadOnchainData, Status: "ACTIVE"},
			strategy.ToolKey("llm_judge", 1):      {Code: "llm_judge", Version: 1, Effect: ir.EffectCallModel, Status: "ACTIVE"},
			strategy.ToolKey("retired_feed", 1):   {Code: "retired_feed", Version: 1, Effect: ir.EffectReadMarketData, Status: "DISABLED"},
		},
		Policy:       policy,
		PolicyHash:   policy.Hash(),
		AssetClasses: map[string]struct{}{"CRYPTO": {}},
		Now:          fxNow,
	}
}

// momentum is the "valid simple momentum" corpus case: a five-minute return
// over a price feed, a threshold, a prediction and a $50 buy.
func momentum(t *testing.T) *ir.IR {
	t.Helper()
	policy := testPolicy()
	hash, err := hexBytes(policy.Hash())
	require.NoError(t, err)

	doc := &ir.IR{
		SchemaVersion: ir.SchemaVersion,
		StrategyID:    fxStrategyID,
		Version:       1,
		Owner:         ir.Owner{AccountID: fxAccountID, UserID: fxUserID},
		Instruments:   []ir.InstrumentDecl{{Name: "sol_usdc", InstrumentID: fxInstrumentID}},
		Triggers: []ir.Trigger{{
			Name: "tick", Kind: ir.TriggerOnInterval, EveryMS: everyMS(5000), DedupWindowMS: 5000,
		}},
		Dependencies: []ir.Dependency{{
			Name: "price", Kind: ir.DepPrice, ToolCode: "price_spot", ToolVersion: 1, DependencyVersion: 1,
			Params: map[string]string{"instrument_id": "sol_usdc"}, MaxAgeMS: 500, Required: true,
		}},
		Signals: []ir.Signal{{
			Name: "ret_5m", Scale: 4, Rounding: "half_even",
			Expr: ir.Expr{Window: &ir.WindowOp{
				Fn: ir.WinReturn, Dependency: "price", Path: "mid", LookbackMS: 300_000, Scale: 4, Rounding: "half_even",
			}},
		}},
		Conditions: []ir.Condition{{
			Name: "momentum_up",
			Expr: ir.Expr{Cmp: &ir.Cmp{
				Op: ir.CmpGT,
				L:  &ir.Expr{Signal: refExpr("ret_5m")},
				R:  exprPtr(constOf(t, "0.0200")),
			}},
		}},
		Actions: []ir.Action{
			{
				Name: "predict", Kind: ir.ActionCommitPrediction, When: refExpr("momentum_up"),
				Prediction: &ir.PredictionSpec{
					Instrument: "sol_usdc", HorizonMS: 900_000, Direction: ir.DirectionUp,
					Probability:         constOf(t, "0.6500"),
					ExpectedReturnBPS:   constOf(t, "150"),
					DownsideProbability: constOf(t, "0.3500"),
					MaxDownsideBPS:      constOf(t, "300"),
					Confidence:          constOf(t, "0.6000"),
				},
			},
			{
				Name: "buy", Kind: ir.ActionCreateTradeIntent, When: refExpr("momentum_up"),
				Intent: &ir.IntentSpec{
					Action: intent.ActionAcquireNotional, Instrument: "sol_usdc",
					Sizing: ir.Sizing{Kind: ir.SizingFixedNotional, NotionalUSD: usdPtr(50_00)},
					Constraints: ir.IntentConstraints{
						MaxSlippageBPS: 50, MaxFeeBPS: 30, MaxPriceImpactBPS: 50,
						QuoteFreshnessMS: 500, AllowedVenues: []string{"JUPITER"},
					},
					DeadlineMS: 30_000, Prediction: "predict",
				},
			},
		},
		RiskPolicy:  ir.RiskPolicyRef{Version: policy.Version, Hash: hash},
		ModelBudget: ir.ModelBudget{Providers: []string{}, MaxSpendPerDay: money.USDFromMinor(0)},
		DataBudget: ir.DataBudget{
			MaxToolCallsPerRun: 4, MaxToolCallsPerDay: 500, MaxSpendPerDay: money.USDFromMinor(5_00), MaxLookbackMS: 3_600_000,
		},
		Envelope: ir.EnvelopeRequirements{
			MinAllocation: money.USDFromMinor(100_00), MaxSingleTrade: money.USDFromMinor(50_00),
			MaxPosition: money.USDFromMinor(200_00), MaxDailyLoss: money.USDFromMinor(30_00),
			Instruments: []string{fxInstrumentID}, AssetClasses: []string{"CRYPTO"}, Venues: []string{"JUPITER"},
			MaxIntentsPerHour: 6, MaxRunsPerMinute: 12,
		},
		Effects: []ir.Effect{ir.EffectReadMarketData, ir.EffectCommitPrediction, ir.EffectCreateTradeIntent},
		Lineage: ir.Lineage{Source: ir.SourceTypeScriptSDK, CompilerVersion: "corpus"},
		BuiltAt: fxNow,
	}
	doc.Normalize()
	return doc
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// hexBytes decodes a hex string into an ir.Hex.
func hexBytes(s string) (ir.Hex, error) {
	out := make([]byte, 0, len(s)/2)
	for i := 0; i+1 < len(s); i += 2 {
		var b byte
		for j := 0; j < 2; j++ {
			c := s[i+j]
			var v byte
			switch {
			case c >= '0' && c <= '9':
				v = c - '0'
			case c >= 'a' && c <= 'f':
				v = c - 'a' + 10
			case c >= 'A' && c <= 'F':
				v = c - 'A' + 10
			default:
				return nil, json.Unmarshal([]byte(`"bad hex"`), &struct{}{})
			}
			b = b<<4 | v
		}
		out = append(out, b)
	}
	return out, nil
}
