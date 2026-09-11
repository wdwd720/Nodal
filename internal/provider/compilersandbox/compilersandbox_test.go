package compilersandbox_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/agents"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/provider/compilersandbox"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/strategy"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

const (
	fxAccountID    = "0192b8e0-1a2b-7c3d-8e4f-5a6b7c8d9e10"
	fxUserID       = "0192b8e0-1a2b-7c3d-8e4f-5a6b7c8d9e11"
	fxInstrumentID = "0192b8e0-1a2b-7c3d-8e4f-5a6b7c8d9e12"
	fxHaltedID     = "0192b8e0-1a2b-7c3d-8e4f-5a6b7c8d9e14"
)

var fxNow = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

func usdPtr(minor int64) *money.USD { u := money.USDFromMinor(minor); return &u }

func bpsPtr(v money.BPS) *money.BPS { return &v }

func intPtr(v int) *int { return &v }

func int64Ptr(v int64) *int64 { return &v }

func boolPtr(v bool) *bool { return &v }

func testPolicy(t *testing.T) risk.Policy {
	t.Helper()
	p := risk.MustParsePolicy(json.RawMessage(risk.DefaultGlobalPolicyJSON))
	p.Version = "risk-v1"
	p.AllowedVenues = []string{"JUPITER"}
	p.MaxDataAgeMS = map[string]int64{"price": 500}
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

func testRefs(t *testing.T) strategy.ValidationRefs {
	t.Helper()
	policy := testPolicy(t)
	activeID, err := instruments.ParseInstrumentID(fxInstrumentID)
	require.NoError(t, err)
	haltedID, err := instruments.ParseInstrumentID(fxHaltedID)
	require.NoError(t, err)
	return strategy.ValidationRefs{
		Instruments: map[string]instruments.Instrument{
			fxInstrumentID: {ID: activeID, CanonicalName: "SOL/USDC", Status: assets.StatusActive, ActiveFrom: fxNow.Add(-24 * time.Hour)},
			fxHaltedID:     {ID: haltedID, CanonicalName: "DEAD/USDC", Status: assets.StatusHalted, ActiveFrom: fxNow.Add(-24 * time.Hour)},
		},
		Venues: map[string]instruments.Venue{
			"JUPITER": {Code: "JUPITER", Status: instruments.VenueActive},
			"DEADDEX": {Code: "DEADDEX", Status: instruments.VenueDisabled},
		},
		Tools: map[string]strategy.Tool{
			strategy.ToolKey(compilersandbox.PreferredPriceTool, 1): {
				Code: compilersandbox.PreferredPriceTool, Version: 1,
				Effect: ir.EffectReadMarketData, Status: "ACTIVE",
			},
		},
		Policy:       policy,
		PolicyHash:   policy.Hash(),
		AssetClasses: map[string]struct{}{},
		Now:          fxNow,
	}
}

func testRegistry() agents.StructuredRegistry {
	return agents.StructuredRegistry{
		InstrumentIDsByCanonicalName: map[string]string{
			"SOL/USDC":  fxInstrumentID,
			"DEAD/USDC": fxHaltedID,
		},
		VenueCodesByInstrumentID: map[string][]string{
			fxInstrumentID: {"JUPITER"},
			fxHaltedID:     {"DEADDEX"},
		},
	}
}

// declared is the complete, valid structured strategy every case starts from.
// Each test mutates one field, so the thing under test is the difference.
func declared() map[string]any {
	return map[string]any{
		"schema_version": 1,
		"universe":       map[string]any{"instrument": "SOL/USDC", "venue": "JUPITER"},
		"entry":          map[string]any{"kind": "PRICE_THRESHOLD", "comparator": "LTE", "price_usd": "13500"},
		"exit":           map[string]any{"kind": "PRICE_THRESHOLD", "comparator": "GTE", "price_usd": "16500"},
		"risk_limits": map[string]any{
			"max_single_trade_usd": "5000",
			"max_position_usd":     "20000",
			"max_daily_loss_usd":   "3000",
		},
		"capital_limit": map[string]any{"min_allocation_usd": "10000"},
		"frequency":     map[string]any{"interval_minutes": 5, "max_intents_per_hour": 6},
		"mode":          "PAPER",
	}
}

func raw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

func newCompiler(t *testing.T) *compilersandbox.Compiler {
	t.Helper()
	c, err := compilersandbox.New(config.EnvTest, clock.NewFake(fxNow), "test-build")
	require.NoError(t, err)
	return c
}

func compile(t *testing.T, c *compilersandbox.Compiler, body json.RawMessage) strategy.Result {
	t.Helper()
	return compileAs(t, c, strategy.NewStrategyID(), body)
}

// compileAs pins the strategy id, which is part of the semantic hash: two
// compiles of the same declared strategy under different strategies are
// different documents, and deliberately so.
func compileAs(t *testing.T, c *compilersandbox.Compiler, sid strategy.StrategyID, body json.RawMessage) strategy.Result {
	t.Helper()
	res, err := c.CompileStructured(context.Background(), agents.StructuredCompileRequest{
		StrategyID: sid, RequestID: "req-1", AttemptNo: 1,
		OwnerAccountID: fxAccountID, OwnerUserID: fxUserID, Version: 1,
		Constraints: body, Refs: testRefs(t), Registry: testRegistry(), Environment: "TEST",
	})
	require.NoError(t, err)
	return res
}

// TestPRODIsRefusedAtConstruction is the first of the three PROD refusals: the
// provider's own, which does not depend on any wiring being right.
func TestPRODIsRefusedAtConstruction(t *testing.T) {
	t.Parallel()
	c, err := compilersandbox.New(config.EnvProd, clock.NewFake(fxNow), "build")
	require.Error(t, err)
	assert.Nil(t, c)
	assert.Contains(t, err.Error(), "cannot exist in PROD")

	for _, env := range []config.Environment{config.EnvLocal, config.EnvTest, config.EnvDev, config.EnvStaging} {
		got, gerr := compilersandbox.New(env, clock.NewFake(fxNow), "build")
		require.NoError(t, gerr, "%s", env)
		require.NotNil(t, got)
		assert.True(t, got.SandboxCompiler(), "everything this compiler produces is a rehearsal")
	}
}

// TestACompleteStrategyCompiles is the whole claim in one case: a strategy
// stated field by field becomes a validated document whose every element traces
// back to one of those fields.
func TestACompleteStrategyCompiles(t *testing.T) {
	t.Parallel()
	res := compile(t, newCompiler(t), raw(t, declared()))

	require.Equal(t, strategy.OutcomeSuccess, res.Outcome, "codes: %v", res.Codes)
	require.NotNil(t, res.Version)
	v := res.Version

	assert.Equal(t, strategy.StatusCompiled, v.Status, "a compiler never returns an accepted version")
	assert.Equal(t, ir.SourceStructuredSandbox, v.SourceKind)
	assert.NotEmpty(t, v.HumanReadable, "the rendered strategy is what a person reviews")

	// The hash describes the document it came with, which is what the strategy
	// service's own checkVersion refuses a backend for getting wrong.
	want, err := ir.SemanticHash(v.IR)
	require.NoError(t, err)
	assert.Equal(t, want, []byte(v.IRHash))
	assert.Equal(t, want, []byte(v.IR.Hash))

	doc := v.IR
	require.Len(t, doc.Instruments, 1)
	assert.Equal(t, fxInstrumentID, doc.Instruments[0].InstrumentID)
	assert.Equal(t, ir.Ref("sol_usdc"), doc.Instruments[0].Name)

	require.Len(t, doc.Triggers, 1)
	assert.Equal(t, ir.TriggerOnInterval, doc.Triggers[0].Kind)
	require.NotNil(t, doc.Triggers[0].EveryMS)
	assert.Equal(t, int64(5*60_000), *doc.Triggers[0].EveryMS, "the interval is the one that was stated")

	require.Len(t, doc.Dependencies, 1)
	assert.Equal(t, ir.DepPrice, doc.Dependencies[0].Kind)
	assert.Equal(t, compilersandbox.PreferredPriceTool, doc.Dependencies[0].ToolCode)
	assert.Equal(t, int64(500), doc.Dependencies[0].MaxAgeMS, "the policy's own cap for price data")

	// No model, and the document does not require a runtime to call one.
	assert.False(t, doc.ModelBudget.Required)
	assert.Empty(t, doc.ModelBudget.Providers)
	assert.True(t, doc.ModelBudget.MaxSpendPerDay.IsZero())
	assert.Equal(t, 0, doc.ModelBudget.MaxCallsPerRun)

	// Effects are derived, and CALL_MODEL is not among them.
	assert.Equal(t, ir.EffectStrings(ir.DeriveEffects(doc)), ir.EffectStrings(doc.Effects))
	assert.NotContains(t, ir.EffectStrings(doc.Effects), string(ir.EffectCallModel))
	assert.Contains(t, ir.EffectStrings(doc.Effects), string(ir.EffectCreateTradeIntent))

	// The limits are the stated ones, to the cent.
	assert.Equal(t, money.USDFromMinor(5000), doc.Envelope.MaxSingleTrade)
	assert.Equal(t, money.USDFromMinor(20000), doc.Envelope.MaxPosition)
	assert.Equal(t, money.USDFromMinor(3000), doc.Envelope.MaxDailyLoss)
	assert.Equal(t, money.USDFromMinor(10000), doc.Envelope.MinAllocation)
	assert.Equal(t, 6, doc.Envelope.MaxIntentsPerHour)
	assert.Equal(t, []string{"JUPITER"}, doc.Envelope.Venues)

	// The prediction claims nothing, and says so in the direction that cannot
	// flatter: no expected gain, a loss not ruled out, downside of everything.
	pred, ok := doc.Action("no_forecast")
	require.True(t, ok)
	require.NotNil(t, pred.Prediction)
	assert.Equal(t, ir.DirectionFlat, pred.Prediction.Direction)
	assert.Equal(t, "0", pred.Prediction.Probability.Const.Mantissa)
	assert.Equal(t, "0", pred.Prediction.ExpectedReturnBPS.Const.Mantissa)
	assert.Equal(t, "10000", pred.Prediction.DownsideProbability.Const.Mantissa)
	assert.Equal(t, "10000", pred.Prediction.MaxDownsideBPS.Const.Mantissa)
	assert.Equal(t, "0", pred.Prediction.Confidence.Const.Mantissa)

	// The trade is sized at the stated per-trade maximum, and every intent
	// names the prediction that precedes it.
	enter, ok := doc.Action("enter")
	require.True(t, ok)
	require.NotNil(t, enter.Intent)
	require.NotNil(t, enter.Intent.Sizing.NotionalUSD)
	assert.Equal(t, money.USDFromMinor(5000), *enter.Intent.Sizing.NotionalUSD)
	assert.Equal(t, ir.Ref("no_forecast"), enter.Intent.Prediction)

	// One attempt, recorded, with no model provenance on it at all.
	require.Len(t, res.Attempts, 1)
	a := res.Attempts[0]
	assert.Equal(t, strategy.OutcomeSuccess, a.Outcome)
	assert.Equal(t, ir.SourceStructuredSandbox, a.SourceKind)
	assert.Empty(t, a.Provenance.Provider, "no provider answered")
	assert.Empty(t, a.Provenance.ModelID, "no model answered")
	assert.Empty(t, a.Provenance.TemplateVersion, "there was no prompt")
	assert.True(t, a.Provenance.Usage.Cost.IsZero())

	// The rationale names the fields.
	assert.Contains(t, res.Rationale.Summary, "SOL/USDC")
	assert.NotEmpty(t, res.Rationale.Assumptions)
	joined := ""
	for _, note := range res.Rationale.Assumptions {
		joined += note + "\n"
	}
	assert.Contains(t, joined, "universe.instrument")
	assert.Contains(t, joined, "risk_limits.max_single_trade_usd")
	assert.Contains(t, joined, "not read by this compiler")
}

// TestTheSameInputCompilesToTheSameDocument: a compiler whose answer depended
// on map iteration order would produce a different ir_hash for one strategy,
// and the hash is what acceptance is bound to.
func TestTheSameInputCompilesToTheSameDocument(t *testing.T) {
	t.Parallel()
	c := newCompiler(t)
	sid := strategy.NewStrategyID()
	first := compileAs(t, c, sid, raw(t, declared()))
	require.NotNil(t, first.Version)
	for i := 0; i < 8; i++ {
		again := compileAs(t, c, sid, raw(t, declared()))
		require.NotNil(t, again.Version)
		assert.Equal(t, first.Version.IRHash, again.Version.IRHash, "run %d", i)
	}
	// And a different strategy is a different document, which is what makes the
	// hash an identity rather than a checksum of the form.
	other := compileAs(t, c, strategy.NewStrategyID(), raw(t, declared()))
	require.NotNil(t, other.Version)
	assert.NotEqual(t, first.Version.IRHash, other.Version.IRHash)
}

// TestAnUnstatedStrategyIsRefusedByName is the refusal that replaces a default.
func TestAnUnstatedStrategyIsRefusedByName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		body  json.RawMessage
		field string
	}{
		{"no constraints at all", json.RawMessage(`{}`), "constraints"},
		{"null", json.RawMessage(`null`), "constraints"},
		{"empty body", json.RawMessage(``), "constraints"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := compile(t, newCompiler(t), tc.body)
			assert.Nil(t, res.Version, "nothing is compiled from nothing")
			assert.Equal(t, strategy.OutcomeRejected, res.Outcome)
			assert.Equal(t, []string{agents.StructuredConstraintsRequired}, res.Codes)
			require.Len(t, res.Attempts, 1)
			assert.Equal(t, strategy.StagePrompt, res.Attempts[0].StageReached)
			require.NotEmpty(t, res.Clarifications)
			assert.Contains(t, res.Clarifications[0], tc.field)
		})
	}
}

// TestEveryMissingFieldIsNamedAtOnce: a person filling in a form is told
// everything that is missing once, not led through them one refusal at a time.
func TestEveryMissingFieldIsNamedAtOnce(t *testing.T) {
	t.Parallel()
	body := declared()
	delete(body, "risk_limits")
	delete(body, "frequency")
	delete(body, "mode")
	res := compile(t, newCompiler(t), raw(t, body))

	assert.Nil(t, res.Version)
	fields := map[string]bool{}
	for _, sentence := range res.Clarifications {
		fields[sentence[:indexOf(sentence, ':')]] = true
	}
	for _, want := range []string{
		"risk_limits.max_single_trade_usd", "risk_limits.max_position_usd", "risk_limits.max_daily_loss_usd",
		"frequency.interval_minutes", "frequency.max_intents_per_hour", "mode",
	} {
		assert.True(t, fields[want], "%s is named; got %v", want, res.Clarifications)
	}
	require.Len(t, res.Attempts, 1)
	assert.NotEmpty(t, res.Attempts[0].Fields["mode"], "the attempt records the finding per field")
}

func indexOf(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return len(s)
}

// TestAModeThatMovesValueIsRefusedNotDowngraded.
func TestAModeThatMovesValueIsRefusedNotDowngraded(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"LIVE", "SHADOW", "CANARY", "LIMITED", "paper", ""} {
		body := declared()
		body["mode"] = mode
		res := compile(t, newCompiler(t), raw(t, body))
		assert.Nil(t, res.Version, "mode %q produced a version", mode)
		assert.Equal(t, []string{agents.StructuredConstraintsRequired}, res.Codes, "mode %q", mode)
	}
}

// TestAnUnknownFieldIsRefusedRatherThanIgnored: a misspelled key is a named
// refusal, never a setting that silently did not apply.
func TestAnUnknownFieldIsRefusedRatherThanIgnored(t *testing.T) {
	t.Parallel()
	body := declared()
	body["max_dail_loss_usd"] = "1"
	res := compile(t, newCompiler(t), raw(t, body))
	assert.Nil(t, res.Version)
	require.NotEmpty(t, res.Clarifications)
	assert.Contains(t, res.Clarifications[0], "max_dail_loss_usd")
}

// TestAHalfStatedRuleIsRefused: EVERY_INTERVAL carrying a threshold is somebody
// who changed their mind halfway, and compiling either half would be a guess.
func TestAHalfStatedRuleIsRefused(t *testing.T) {
	t.Parallel()
	body := declared()
	body["entry"] = map[string]any{"kind": "EVERY_INTERVAL", "comparator": "LTE", "price_usd": "13500"}
	res := compile(t, newCompiler(t), raw(t, body))
	assert.Nil(t, res.Version)
	assert.Equal(t, []string{agents.StructuredConstraintsRequired}, res.Codes)
}

// TestARebalanceRuleCarriesNoCondition.
func TestARebalanceRuleCarriesNoCondition(t *testing.T) {
	t.Parallel()
	body := declared()
	body["entry"] = map[string]any{"kind": "EVERY_INTERVAL"}
	body["exit"] = map[string]any{"kind": "EVERY_INTERVAL"}
	res := compile(t, newCompiler(t), raw(t, body))
	require.Equal(t, strategy.OutcomeSuccess, res.Outcome, "codes: %v", res.Codes)
	require.NotNil(t, res.Version)
	assert.Empty(t, res.Version.IR.Conditions, "a rule with no threshold has no condition")
	for _, a := range res.Version.IR.Actions {
		assert.Nil(t, a.When, "%s fires on every evaluation", a.Name)
	}
}

// TestMoneyIsNeverAFloat: the grammar takes exact minor units and refuses
// everything a float would arrive as.
func TestMoneyIsNeverAFloat(t *testing.T) {
	t.Parallel()
	for _, bad := range []any{"50.00", "5e3", " 5000", "5000 ", "-5000", "05000", "5,000", 5000, "", "0x10"} {
		body := declared()
		limits, _ := body["risk_limits"].(map[string]any)
		limits["max_single_trade_usd"] = bad
		res := compile(t, newCompiler(t), raw(t, body))
		assert.Nil(t, res.Version, "%v produced a version", bad)
	}
	// And the one shape it takes is exact.
	minor, err := compilersandbox.ParseMinorUSD("5000")
	require.NoError(t, err)
	assert.Equal(t, int64(5000), minor)
}

// TestARegistryTheDeploymentDoesNotHaveIsNamed.
func TestARegistryTheDeploymentDoesNotHaveIsNamed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"an instrument nobody lists", func(b map[string]any) {
			b["universe"] = map[string]any{"instrument": "BTC/USDC", "venue": "JUPITER"}
		}, "universe.instrument"},
		{"an instrument that is not active", func(b map[string]any) {
			b["universe"] = map[string]any{"instrument": "DEAD/USDC", "venue": "DEADDEX"}
		}, "universe.instrument"},
		{"a venue nobody lists", func(b map[string]any) {
			b["universe"] = map[string]any{"instrument": "SOL/USDC", "venue": "NOPEDEX"}
		}, "universe.venue"},
		{"a venue that does not list the instrument", func(b map[string]any) {
			b["universe"] = map[string]any{"instrument": "SOL/USDC", "venue": "DEADDEX"}
		}, "universe.venue"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := declared()
			tc.mutate(body)
			res := compile(t, newCompiler(t), raw(t, body))
			assert.Nil(t, res.Version)
			require.NotEmpty(t, res.Clarifications)
			found := false
			for _, s := range res.Clarifications {
				if len(s) >= len(tc.want) && s[:len(tc.want)] == tc.want {
					found = true
				}
			}
			assert.True(t, found, "%s is named; got %v", tc.want, res.Clarifications)
		})
	}
}

// TestLimitsLooserThanThePolicyAreRefusedByThePipeline: the structured path
// runs the same Validate the model path does, and gets no additional trust for
// having been typed into a form.
func TestLimitsLooserThanThePolicyAreRefusedByThePipeline(t *testing.T) {
	t.Parallel()
	body := declared()
	limits, _ := body["risk_limits"].(map[string]any)
	limits["max_single_trade_usd"] = "500000" // $5,000, above the policy's $1,000
	res := compile(t, newCompiler(t), raw(t, body))

	assert.Nil(t, res.Version, "nothing was loosened to make it pass")
	assert.Equal(t, strategy.OutcomeRejected, res.Outcome)
	assert.Contains(t, res.Codes, strategy.CodeRiskIncompatible)
	require.Len(t, res.Attempts, 1)
	assert.Equal(t, strategy.StageRiskCompat, res.Attempts[0].StageReached)
}

// TestAMissingPriceToolIsNamedByTheRegistry: a deployment whose tools table has
// no market-data tool gets UNKNOWN_TOOL naming the one it lacks, rather than a
// compiler that refuses to start.
func TestAMissingPriceToolIsNamedByTheRegistry(t *testing.T) {
	t.Parallel()
	refs := testRefs(t)
	refs.Tools = map[string]strategy.Tool{}
	c := newCompiler(t)
	res, err := c.CompileStructured(context.Background(), agents.StructuredCompileRequest{
		StrategyID: strategy.NewStrategyID(), RequestID: "req-1", AttemptNo: 1,
		OwnerAccountID: fxAccountID, OwnerUserID: fxUserID, Version: 1,
		Constraints: raw(t, declared()), Refs: refs, Registry: testRegistry(), Environment: "TEST",
	})
	require.NoError(t, err)
	assert.Nil(t, res.Version)
	assert.Contains(t, res.Codes, strategy.CodeUnknownTool)
	require.Len(t, res.Attempts, 1)
	assert.Contains(t, res.Attempts[0].Fields["dependencies[0].tool_code"], compilersandbox.PreferredPriceTool)
}

// TestTheGrammarIsTheOneTheDocumentationClaims keeps FieldNames honest: every
// name it lists is a field of the decoder, and a field the decoder gained and
// the list did not would leave the API description describing a different
// grammar from the one that runs.
func TestTheGrammarIsTheOneTheDocumentationClaims(t *testing.T) {
	t.Parallel()
	names := compilersandbox.FieldNames()
	assert.Len(t, names, 16)

	// Every listed leaf is reachable: a document containing exactly the listed
	// fields decodes, and the decoder refuses anything else (proved above).
	body := declared()
	entry, _ := body["entry"].(map[string]any)
	assert.Contains(t, names, "entry.comparator")
	assert.Contains(t, entry, "comparator")
	res := compile(t, newCompiler(t), raw(t, body))
	require.Equal(t, strategy.OutcomeSuccess, res.Outcome, "codes: %v", res.Codes)
}
