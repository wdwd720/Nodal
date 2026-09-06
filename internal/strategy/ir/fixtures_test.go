package ir_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// Fixed identifiers so a fixture is byte-identical between runs and between
// processes: the golden hashes below depend on them. All are canonical
// UUIDv7 (version nibble 7, variant bits 10).
const (
	fxStrategyID   = "0192b8e0-1a2b-7c3d-8e4f-5a6b7c8d9e0f"
	fxAccountID    = "0192b8e0-1a2b-7c3d-8e4f-5a6b7c8d9e10"
	fxUserID       = "0192b8e0-1a2b-7c3d-8e4f-5a6b7c8d9e11"
	fxInstrumentID = "0192b8e0-1a2b-7c3d-8e4f-5a6b7c8d9e12"
	fxVersionID    = "0192b8e0-1a2b-7c3d-8e4f-5a6b7c8d9e13"
)

// fxBuiltAt is a fixed, non-UTC-zone timestamp: Normalize must convert it so
// the hash does not depend on the caller's location.
var fxBuiltAt = time.Date(2026, 9, 5, 12, 0, 0, 123456789, time.UTC)

func ref(s string) *ir.Ref { r := ir.Ref(s); return &r }

func usd(minor int64) money.USD { return money.USDFromMinor(minor) }

func usdPtr(minor int64) *money.USD { u := money.USDFromMinor(minor); return &u }

func dec(t *testing.T, s string) ir.Decimal {
	t.Helper()
	d, err := ir.ParseDecimalString(s)
	require.NoError(t, err, "fixture decimal %q", s)
	return d
}

func constExpr(t *testing.T, s string) ir.Expr {
	t.Helper()
	d := dec(t, s)
	return ir.Expr{Const: &d}
}

func every(ms int64) *int64 { return &ms }

// momentum is the "valid simple momentum" shape of the golden corpus
// (STRATEGY_IR.md §11): a five-minute return over a PRICE dependency, a
// threshold condition, a committed prediction and a sized trade intent.
// Effects: READ_MARKET_DATA, COMMIT_PREDICTION, CREATE_TRADE_INTENT.
func momentum(t *testing.T) *ir.IR {
	t.Helper()
	doc := &ir.IR{
		SchemaVersion: ir.SchemaVersion,
		StrategyID:    fxStrategyID,
		Version:       1,
		Owner:         ir.Owner{AccountID: fxAccountID, UserID: fxUserID},
		Instruments:   []ir.InstrumentDecl{{Name: "sol_usdc", InstrumentID: fxInstrumentID}},
		Triggers: []ir.Trigger{{
			Name: "tick", Kind: ir.TriggerOnInterval, EveryMS: every(5000), DedupWindowMS: 5000,
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
				L:  &ir.Expr{Signal: ref("ret_5m")},
				R:  ptrExpr(constExpr(t, "0.0200")),
			}},
		}},
		Actions: []ir.Action{
			{
				Name: "predict", Kind: ir.ActionCommitPrediction, When: ref("momentum_up"),
				Prediction: &ir.PredictionSpec{
					Instrument: "sol_usdc", HorizonMS: 900_000, Direction: ir.DirectionUp,
					Probability:         constExpr(t, "0.6500"),
					ExpectedReturnBPS:   constExpr(t, "150"),
					DownsideProbability: constExpr(t, "0.3500"),
					MaxDownsideBPS:      constExpr(t, "300"),
					Confidence:          constExpr(t, "0.6000"),
				},
			},
			{
				Name: "buy", Kind: ir.ActionCreateTradeIntent, When: ref("momentum_up"),
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
		RiskPolicy:  ir.RiskPolicyRef{Version: "risk-v1", Hash: ir.Hex{0x01, 0x02, 0x03}},
		ModelBudget: ir.ModelBudget{Required: false, Providers: []string{}, MaxCallsPerRun: 0, MaxCallsPerDay: 0, MaxSpendPerDay: usd(0)},
		DataBudget: ir.DataBudget{
			MaxToolCallsPerRun: 4, MaxToolCallsPerDay: 500, MaxSpendPerDay: usd(5_00), MaxLookbackMS: 3_600_000,
		},
		Envelope: ir.EnvelopeRequirements{
			MinAllocation: usd(100_00), MaxSingleTrade: usd(50_00), MaxPosition: usd(200_00), MaxDailyLoss: usd(30_00),
			Instruments: []string{fxInstrumentID}, AssetClasses: []string{"CRYPTO"}, Venues: []string{"JUPITER"},
			MaxIntentsPerHour: 6, MaxRunsPerMinute: 12,
		},
		Effects: []ir.Effect{ir.EffectReadMarketData, ir.EffectCommitPrediction, ir.EffectCreateTradeIntent},
		Lineage: ir.Lineage{
			Source: ir.SourceTypeScriptSDK, SourceHash: ir.Hex{0xaa, 0xbb},
			CompilerVersion: "stage9-test", SDKVersion: "0.1.0",
		},
		BuiltAt: fxBuiltAt,
	}
	doc.Normalize()
	return doc
}

func ptrExpr(e ir.Expr) *ir.Expr { return &e }

// walletTrigger is the "valid wallet trigger" corpus shape: an ON_EVENT
// trigger over wallet transfers adds READ_ONCHAIN_DATA to the effect set.
func walletTrigger(t *testing.T) *ir.IR {
	t.Helper()
	doc := momentum(t)
	doc.Triggers = []ir.Trigger{{
		Name: "on_transfer", Kind: ir.TriggerOnEvent, EventType: "wallet.transfer", DedupWindowMS: 60_000,
		Filter: &ir.Expr{Cmp: &ir.Cmp{
			Op: ir.CmpGE,
			L:  &ir.Expr{Field: &ir.FieldRef{Dependency: "on_transfer", Path: "amount_usd", Scale: 2}},
			R:  ptrExpr(constExpr(t, "100.00")),
		}},
	}}
	doc.Dependencies = append(doc.Dependencies, ir.Dependency{
		Name: "wallet_moves", Kind: ir.DepWalletEvent, ToolCode: "wallet_events", ToolVersion: 1, DependencyVersion: 1,
		Params: map[string]string{"wallet_set": "w1"}, MaxAgeMS: 2000, Required: true,
	})
	doc.Effects = append(doc.Effects, ir.EffectReadOnchainData)
	doc.Normalize()
	return doc
}

// mustJSON marshals a document the way the compiler persists it.
func mustJSON(t *testing.T, doc *ir.IR) []byte {
	t.Helper()
	b, err := json.Marshal(doc)
	require.NoError(t, err)
	return b
}
