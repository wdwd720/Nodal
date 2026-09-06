package risk

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

func fixtureRules(t *testing.T, name string) json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile("testdata/policies/" + name + ".json")
	require.NoError(t, err)
	var pf policyFixture
	require.NoError(t, json.Unmarshal(raw, &pf))
	return pf.Rules
}

func mutateRules(t *testing.T, rules json.RawMessage, path []string, value any) json.RawMessage {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(rules, &doc))
	cur := doc
	for _, k := range path[:len(path)-1] {
		next, ok := cur[k].(map[string]any)
		require.True(t, ok, "rules path %v: %q is not an object", path, k)
		cur = next
	}
	cur[path[len(path)-1]] = value
	out, err := json.Marshal(doc)
	require.NoError(t, err)
	return out
}

func deleteRule(t *testing.T, rules json.RawMessage, key string) json.RawMessage {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(rules, &doc))
	delete(doc, key)
	out, err := json.Marshal(doc)
	require.NoError(t, err)
	return out
}

func usd(t *testing.T, s string) money.USD {
	t.Helper()
	v, err := money.ParseUSD(s)
	require.NoError(t, err)
	return v
}

func TestParsePolicy_AcceptsFixtureAndDefault(t *testing.T) {
	p, err := ParsePolicy(fixtureRules(t, "global_test"))
	require.NoError(t, err)
	require.NoError(t, p.Validate(ScopeGlobal))
	assert.Equal(t, usd(t, "1000.00"), *p.MaxSingleTradeUSD)
	assert.Equal(t, money.BPS(100), *p.MaxSlippageBPS)
	assert.Equal(t, int64(3000), *p.MaxQuoteAgeMS)
	assert.Equal(t, []string{"MAJOR", "SETTLEMENT", "STANDARD"}, p.AllowedAssetRiskClasses, "lists are normalised")
	assert.Empty(t, p.MissingLimits())
	assert.True(t, p.Missing(), "ParsePolicy never invents a version")

	d := MustParsePolicy([]byte(DefaultGlobalPolicyJSON))
	require.NoError(t, d.Validate(ScopeGlobal), "the shipped default is complete")
	assert.Equal(t, []string{}, d.AllowedVenues, "the shipped default allows no venue")
	assert.Equal(t, usd(t, "1000.00"), *d.MaxSingleTradeUSD)
	assert.Equal(t, usd(t, "500.00"), *d.MaxDailyLossUSD)
	assert.Equal(t, 30, *d.MaxOrdersPerHour)
	assert.Equal(t, []string{HealthHealthy}, d.AllowedProviderHealth)
}

func TestParsePolicy_RejectsUnknownKeys(t *testing.T) {
	_, err := ParsePolicy(mutateRules(t, fixtureRules(t, "global_test"), []string{"max_leverage"}, 3))
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	assert.Contains(t, err.Error(), "unknown field")
}

func TestParsePolicy_RejectsFloatsAndWrongTypes(t *testing.T) {
	rules := fixtureRules(t, "global_test")
	for name, raw := range map[string]json.RawMessage{
		"slippage 10.5":           mutateRules(t, rules, []string{"max_slippage_bps"}, json.Number("10.5")),
		"slippage 100.0":          mutateRules(t, rules, []string{"max_slippage_bps"}, json.Number("100.0")),
		"orders 1e1":              mutateRules(t, rules, []string{"max_orders_per_hour"}, json.Number("1e1")),
		"quote age 2500.5":        mutateRules(t, rules, []string{"max_quote_age_ms"}, json.Number("2500.5")),
		"data age 0.5":            mutateRules(t, rules, []string{"max_data_age_ms", "price"}, json.Number("0.5")),
		"usd as number":           mutateRules(t, rules, []string{"max_single_trade_usd"}, json.Number("1000")),
		"usd three decimals":      mutateRules(t, rules, []string{"max_single_trade_usd"}, "1000.005"),
		"usd negative":            mutateRules(t, rules, []string{"max_position_usd"}, "-1.00"),
		"bps over 100 percent":    mutateRules(t, rules, []string{"max_fee_bps"}, 10001),
		"bps negative":            mutateRules(t, rules, []string{"max_price_impact_bps"}, -1),
		"orders negative":         mutateRules(t, rules, []string{"max_orders_per_hour"}, -1),
		"data age zero":           mutateRules(t, rules, []string{"max_data_age_ms", "price"}, 0),
		"unknown venue status":    mutateRules(t, rules, []string{"allowed_venue_statuses"}, []string{"OPEN"}),
		"unknown provider health": mutateRules(t, rules, []string{"allowed_provider_health"}, []string{"OK"}),
		"unknown risk class":      mutateRules(t, rules, []string{"allowed_asset_risk_classes"}, []string{"MEME"}),
		"bool as string":          mutateRules(t, rules, []string{"allow_risk_reduction_during_kill"}, "yes"),
		"non-object":              json.RawMessage(`[]`),
		"trailing data":           json.RawMessage(`{} {}`),
		"empty":                   json.RawMessage(``),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParsePolicy(raw)
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

func TestValidate_GlobalMustBeComplete(t *testing.T) {
	rules := deleteRule(t, fixtureRules(t, "global_test"), "max_fee_bps")
	p, err := ParsePolicy(rules)
	require.NoError(t, err, "a partial document parses (ACCOUNT/AGENT scopes are partial by design)")
	assert.Equal(t, []string{"max_fee_bps"}, p.MissingLimits())
	assert.NoError(t, p.Validate(ScopeAccount))
	assert.NoError(t, p.Validate(ScopeAgent))
	err = p.Validate(ScopeGlobal)
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	empty, err := ParsePolicy(json.RawMessage(`{}`))
	require.NoError(t, err)
	assert.Len(t, empty.MissingLimits(), 19)
	assert.Error(t, empty.Validate(ScopeGlobal))
}

func TestPolicy_HashIsCanonical(t *testing.T) {
	a := MustParsePolicy(fixtureRules(t, "global_test"))
	reordered := mutateRules(t, fixtureRules(t, "global_test"), []string{"allowed_asset_risk_classes"}, []string{"STANDARD", "MAJOR", "SETTLEMENT", "MAJOR"})
	b := MustParsePolicy(reordered)
	assert.Equal(t, a.Hash(), b.Hash())
	c := MustParsePolicy(mutateRules(t, fixtureRules(t, "global_test"), []string{"max_fee_bps"}, 49))
	assert.NotEqual(t, a.Hash(), c.Hash())
	a.Version = "v1"
	assert.Equal(t, b.Hash(), a.Hash(), "version is not part of the rules hash")
	assert.NotEqual(t, a.Hash(), Policy{}.Hash())
}

func TestCompose_StrictestWins(t *testing.T) {
	global := MustParsePolicy(fixtureRules(t, "global_test"))
	global.Version = "g1"
	account, err := ParsePolicy(json.RawMessage(`{
		"max_single_trade_usd": "300.00",
		"max_position_usd": "9000.00",
		"max_orders_per_hour": 50,
		"min_liquidity_usd": "75000.00",
		"max_data_age_ms": {"price": 250, "social": 60000},
		"allowed_venues": ["JUPITER", "RAYDIUM"],
		"allowed_asset_risk_classes": ["MAJOR"],
		"allow_risk_reduction_during_kill": false
	}`))
	require.NoError(t, err)
	account.Version = "a1"
	agent, err := ParsePolicy(json.RawMessage(`{
		"max_orders_per_hour": 5,
		"max_slippage_bps": 30,
		"allowed_venues": ["RAYDIUM", "JUPITER", "ORCA"],
		"allowed_provider_health": ["HEALTHY", "DEGRADED"],
		"block_risk_reduction_on_account_freeze": true
	}`))
	require.NoError(t, err)
	agent.Version = "ag1"

	c := Compose(&global, &account, &agent)
	assert.Equal(t, "GLOBAL=g1;ACCOUNT=a1;AGENT=ag1", c.Version)
	assert.Equal(t, usd(t, "300.00"), *c.MaxSingleTradeUSD, "account tightened")
	assert.Equal(t, usd(t, "5000.00"), *c.MaxPositionUSD, "account cannot loosen")
	assert.Equal(t, 5, *c.MaxOrdersPerHour, "agent tightened")
	assert.Equal(t, money.BPS(30), *c.MaxSlippageBPS)
	assert.Equal(t, money.BPS(50), *c.MaxFeeBPS, "untouched limits pass through")
	assert.Equal(t, usd(t, "75000.00"), *c.MinLiquidityUSD, "minimum: largest wins")
	assert.Equal(t, map[string]int64{"price": 250, "wallet_event": 2000, "social": 60000}, c.MaxDataAgeMS)
	assert.Equal(t, []string{"JUPITER"}, c.AllowedVenues, "allowlists intersect")
	assert.Equal(t, []string{"MAJOR"}, c.AllowedAssetRiskClasses)
	assert.Equal(t, []string{"HEALTHY"}, c.AllowedProviderHealth)
	assert.Equal(t, []string{"ACTIVE"}, c.AllowedVenueStatuses)
	assert.False(t, *c.AllowRiskReductionDuringKill, "AND")
	assert.True(t, *c.BlockRiskReductionOnAccountFreeze, "OR")
	assert.NoError(t, c.Validate(ScopeGlobal), "composition of a complete GLOBAL stays complete")

	only := Compose(&global, nil, nil)
	assert.Equal(t, "GLOBAL=g1", only.Version)
	assert.Equal(t, global.Hash(), only.Hash(), "composing with nothing changes nothing")

	assert.True(t, Compose(nil, &account, &agent).Missing(), "no GLOBAL means no policy")
	assert.True(t, Compose(&Policy{}, &account, nil).Missing())

	empty, err := ParsePolicy(json.RawMessage(`{"allowed_venues": []}`))
	require.NoError(t, err)
	empty.Version = "e1"
	assert.Equal(t, []string{}, Compose(&global, &empty, nil).AllowedVenues, "an explicit empty allowlist allows nothing")
}

func TestReasonCodes_SortedAndUnique(t *testing.T) {
	codes := ReasonCodes()
	seen := map[string]bool{}
	for i, c := range codes {
		assert.True(t, strings.HasPrefix(c, "RISK_"), c)
		assert.False(t, seen[c], "duplicate %s", c)
		seen[c] = true
		if i > 0 {
			assert.Less(t, codes[i-1], c)
		}
	}
	for _, want := range []string{
		"RISK_MAX_SINGLE_TRADE", "RISK_MAX_POSITION", "RISK_CONCENTRATION", "RISK_DAILY_LOSS", "RISK_MAX_DRAWDOWN",
		"RISK_ORDER_RATE", "RISK_SLIPPAGE", "RISK_FEE", "RISK_PRICE_IMPACT", "RISK_QUOTE_AGE", "RISK_LIQUIDITY",
		"RISK_ASSET_STATUS", "RISK_VENUE_STATUS", "RISK_PROVIDER_HEALTH", "RISK_STALE_DATA", "RISK_RECONCILIATION_PENDING",
		"RISK_ENVELOPE_EXHAUSTED", "RISK_ENVELOPE_INSTRUMENT_NOT_ALLOWED", "RISK_ENVELOPE_VENUE_NOT_ALLOWED",
		"RISK_KILL_SWITCH", "RISK_POLICY_MISSING",
	} {
		assert.Contains(t, codes, want, "POLICY_AUTHORITY §4 code")
	}
}

// TestNoFloatingPointInSource enforces PART 17 and PART 224 mechanically.
func TestNoFloatingPointInSource(t *testing.T) {
	banned := []string{"float32", "float64", "big.Float", "big.Rat", "ParseFloat", "FormatFloat", "math/rand", "time.Now"}
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		require.NoError(t, err)
		checked++
		for _, tok := range banned {
			assert.NotContains(t, string(src), tok, "%s must not mention %q", name, tok)
		}
	}
	require.GreaterOrEqual(t, checked, 5)
}
