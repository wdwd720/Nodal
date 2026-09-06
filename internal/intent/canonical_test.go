package intent_test

import (
	"bytes"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// TestCanonical_Golden pins the exact encoding: a change here is a change
// to every stored content hash and must be a deliberate migration.
func TestCanonical_Golden(t *testing.T) {
	t.Parallel()
	price := money.Price{Mantissa: money.QuantityFromInt64(150_123456), Scale: 6, QuoteAsset: "USDC", Source: "pyth", At: t0}
	intentID, err := intent.ParseIntentID("019930a0-0000-7000-8000-000000000001")
	require.NoError(t, err)
	instrumentID, err := instruments.ParseInstrumentID("019930a0-0000-7000-8000-0000000000d1")
	require.NoError(t, err)
	ti := intent.TradeIntent{
		ID:                intentID,
		AccountID:         "019930A0-0000-7000-8000-0000000000AA",
		ActorType:         security.ActorAgent,
		ActorID:           "019930a0-0000-7000-8000-0000000000a1",
		AgentID:           str("019930A0-0000-7000-8000-0000000000A1"),
		StrategyVersionID: str("019930a0-0000-7000-8000-0000000000b1"),
		PredictionID:      str("019930a0-0000-7000-8000-0000000000c1"),
		Action:            intent.ActionReduceNotional,
		InstrumentID:      instrumentID,
		Quantity:          qty(1_500_000_000),
		Constraints: intent.Constraints{
			MaxSlippageBPS: 50, MaxFeeBPS: 30, MaxPriceImpactBPS: 100,
			MaxPrice: &price, MinReceive: qty(220_000000),
			AllowedVenues: []string{"ORCA", "JUPITER"}, QuoteFreshness: 500 * time.Millisecond,
			ExecutionDeadline: t0.Add(30 * time.Second),
		},
		Deadline:       t0.Add(time.Minute).In(time.FixedZone("X", 3600)),
		RequestedAt:    t0,
		IdempotencyKey: "run:abc:sell <SOL>",
		CorrelationID:  "corr",
		Mode:           intent.ModeCanary,
		Status:         intent.StatusExecuting,
	}
	want := `{"account_id":"019930a0-0000-7000-8000-0000000000aa","action":"REDUCE_NOTIONAL","actor_id":"019930a0-0000-7000-8000-0000000000a1","actor_type":"AGENT",` +
		`"agent_id":"019930a0-0000-7000-8000-0000000000a1",` +
		`"constraints":{"allowed_venues":["JUPITER","ORCA"],"execution_deadline":"2026-09-05T12:00:30Z","max_fee_bps":30,` +
		`"max_price":{"at":"2026-09-05T12:00:00Z","mantissa":"150123456","quote_asset":"USDC","scale":6,"source":"pyth"},` +
		`"max_price_impact_bps":100,"max_slippage_bps":50,"min_receive":"220000000","quote_freshness_ms":500},` +
		`"deadline":"2026-09-05T12:01:00Z","idempotency_key":"run:abc:sell <SOL>","instrument_id":"019930a0-0000-7000-8000-0000000000d1",` +
		`"mode":"CANARY","prediction_id":"019930a0-0000-7000-8000-0000000000c1","quantity":"1500000000","strategy_version_id":"019930a0-0000-7000-8000-0000000000b1"}`
	assert.Equal(t, want, string(intent.Canonical(ti)))
	assert.Len(t, intent.ContentHash(ti), 32)
	assert.True(t, json.Valid(intent.Canonical(ti)))
}

func TestCanonical_OmitsAbsentOptionals(t *testing.T) {
	t.Parallel()
	ti := baseIntent()
	ti.Deadline = time.Time{}
	ti.Constraints = intent.Constraints{}
	c := string(intent.Canonical(ti))
	for _, absent := range []string{"deadline", "agent_id", "max_price", "min_receive", "allowed_venues", "execution_deadline", "quantity", "target_exposure_usd"} {
		assert.NotContains(t, c, `"`+absent+`"`, absent)
	}
	assert.Contains(t, c, `"notional_usd":"200.00"`)
	assert.Contains(t, c, `"quote_freshness_ms":0`)
}

// genIntent draws a structurally valid intent.
func genIntent(rt *rapid.T) intent.TradeIntent {
	action := rapid.SampledFrom(intent.Actions()).Draw(rt, "action")
	actor := rapid.SampledFrom([]security.ActorType{security.ActorUser, security.ActorOperator, security.ActorAgent}).Draw(rt, "actor")
	ti := intent.TradeIntent{
		ID:             intent.NewIntentID(),
		AccountID:      accounts.NewAccountID().String(),
		ActorType:      actor,
		ActorID:        rapid.StringMatching(`[a-z0-9:-]{1,40}`).Draw(rt, "actor_id"),
		Action:         action,
		InstrumentID:   instruments.NewInstrumentID(),
		RequestedAt:    t0.Add(time.Duration(rapid.Int64Range(0, 3600).Draw(rt, "req")) * time.Second),
		IdempotencyKey: rapid.StringMatching(`[A-Za-z0-9:_-]{1,64}`).Draw(rt, "key"),
		CorrelationID:  rapid.StringMatching(`[a-z0-9-]{1,32}`).Draw(rt, "corr"),
		Mode:           rapid.SampledFrom(intent.Modes()).Draw(rt, "mode"),
		Constraints: intent.Constraints{
			MaxSlippageBPS:    money.BPS(rapid.Int64Range(0, 10_000).Draw(rt, "slip")),
			MaxFeeBPS:         money.BPS(rapid.Int64Range(0, 10_000).Draw(rt, "fee")),
			MaxPriceImpactBPS: money.BPS(rapid.Int64Range(0, 10_000).Draw(rt, "impact")),
			QuoteFreshness:    time.Duration(rapid.Int64Range(0, 60_000).Draw(rt, "fresh")) * time.Millisecond,
		},
	}
	switch action {
	case intent.ActionAcquireNotional:
		ti.NotionalUSD = usd(rapid.Int64Range(1, 1_000_000_00).Draw(rt, "notional"))
	case intent.ActionReduceNotional:
		if rapid.Bool().Draw(rt, "byQuantity") {
			ti.Quantity = qty(rapid.Int64Range(1, 1<<40).Draw(rt, "quantity"))
		} else {
			ti.NotionalUSD = usd(rapid.Int64Range(1, 1_000_000_00).Draw(rt, "notional"))
		}
	case intent.ActionTargetExposure:
		ti.TargetExposureUSD = usd(rapid.Int64Range(0, 1_000_000_00).Draw(rt, "target"))
	}
	if actor == security.ActorAgent {
		ti.AgentID, ti.StrategyVersionID, ti.PredictionID = str(uuidStr()), str(uuidStr()), str(uuidStr())
	}
	if rapid.Bool().Draw(rt, "deadline") {
		ti.Deadline = ti.RequestedAt.Add(time.Duration(rapid.Int64Range(1, 3600).Draw(rt, "dl")) * time.Second)
	}
	if rapid.Bool().Draw(rt, "minReceive") {
		ti.Constraints.MinReceive = qty(rapid.Int64Range(1, 1<<40).Draw(rt, "minr"))
	}
	if rapid.Bool().Draw(rt, "maxPrice") {
		p := money.Price{Mantissa: money.QuantityFromInt64(rapid.Int64Range(1, 1<<40).Draw(rt, "mant")), Scale: int32(rapid.IntRange(0, 18).Draw(rt, "scale")), QuoteAsset: "USDC", Source: "test", At: ti.RequestedAt}
		ti.Constraints.MaxPrice = &p
	}
	venues := rapid.SliceOfNDistinct(rapid.StringMatching(`[A-Z]{2,8}`), 0, 6, rapid.ID[string]).Draw(rt, "venues")
	ti.Constraints.AllowedVenues = venues
	return ti
}

// TestProp_CanonicalStableUnderFieldOrder: the canonical form does not
// depend on the order of set-valued fields, on any excluded field, or on
// time zones, is valid JSON with sorted keys at every level, and changes
// whenever a semantic field changes.
func TestProp_CanonicalStableUnderFieldOrder(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		ti := genIntent(rt)
		require.NoError(rt, ti.Validate())
		base := intent.Canonical(ti)
		require.True(rt, json.Valid(base), "%s", base)
		assertSortedKeys(rt, base)

		// Set-valued field order.
		shuffled := ti
		if len(ti.Constraints.AllowedVenues) > 1 {
			shuffled.Constraints.AllowedVenues = rapid.Permutation(append([]string(nil), ti.Constraints.AllowedVenues...)).Draw(rt, "perm")
		}
		require.Equal(rt, string(base), string(intent.Canonical(shuffled)), "venue order")

		// Excluded fields and representation details.
		other := ti
		other.ID = intent.NewIntentID()
		other.RequestedAt = ti.RequestedAt.Add(time.Hour)
		other.CorrelationID = "different"
		other.Status = intent.StatusCompleted
		other.RejectionCode = "X"
		other.Links = intent.Links{OrderID: uuidStr()}
		other.ContentHash = []byte("nope")
		other.Existing = true
		other.ReceivedAt, other.CreatedAt, other.UpdatedAt = t0, t0, t0
		other.AccountID = upper(ti.AccountID)
		if !ti.Deadline.IsZero() {
			other.Deadline = ti.Deadline.In(time.FixedZone("plus5", 5*3600))
		}
		require.Equal(rt, string(base), string(intent.Canonical(other)), "excluded fields")
		require.Equal(rt, intent.ContentHash(ti), intent.ContentHash(other))

		// Semantic changes are visible.
		mutated := ti
		mutated.Constraints.MaxSlippageBPS = (ti.Constraints.MaxSlippageBPS + 1) % 10_001
		require.NotEqual(rt, string(base), string(intent.Canonical(mutated)), "bps change")
		mutated = ti
		mutated.IdempotencyKey = ti.IdempotencyKey + "x"
		require.NotEqual(rt, string(base), string(intent.Canonical(mutated)), "key change")
		mutated = ti
		mutated.Mode = nextMode(ti.Mode)
		require.NotEqual(rt, string(base), string(intent.Canonical(mutated)), "mode change")
		mutated = ti
		mutated.InstrumentID = instruments.NewInstrumentID()
		require.NotEqual(rt, string(base), string(intent.Canonical(mutated)), "instrument change")
		if ti.NotionalUSD != nil {
			mutated = ti
			mutated.NotionalUSD = usd(ti.NotionalUSD.Minor() + 1)
			require.NotEqual(rt, string(base), string(intent.Canonical(mutated)), "notional change")
		}
	})
}

func nextMode(m intent.Mode) intent.Mode {
	modes := intent.Modes()
	for i, x := range modes {
		if x == m {
			return modes[(i+1)%len(modes)]
		}
	}
	return modes[0]
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'f' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

// assertSortedKeys walks the JSON and checks that every object has its keys
// in bytewise order and that there is no whitespace outside strings.
func assertSortedKeys(rt *rapid.T, b []byte) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	require.NoError(rt, dec.Decode(&v))
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sorted := append([]string(nil), keys...)
			sort.Strings(sorted)
			// Re-encode the object's keys in document order to compare.
			require.True(rt, sort.StringsAreSorted(keysInDocumentOrder(b, keys)), "keys not sorted in %s", b)
			for _, e := range x {
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(v)
	require.NotContains(rt, string(b), " \"", "no whitespace")
}

// keysInDocumentOrder returns keys ordered by their first appearance as a
// JSON key in b (good enough for canonical output where keys are unique).
func keysInDocumentOrder(b []byte, keys []string) []string {
	type pos struct {
		k string
		i int
	}
	ps := make([]pos, 0, len(keys))
	for _, k := range keys {
		kb, _ := json.Marshal(k)
		ps = append(ps, pos{k, bytes.Index(b, append(kb, ':'))})
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].i < ps[j].i })
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.k
	}
	return out
}

func TestCanonical_LinkedIDsAreCaseInsensitive(t *testing.T) {
	t.Parallel()
	a := agentIntent()
	b := a
	b.AgentID = str(upper(*a.AgentID))
	b.PredictionID = str(upper(*a.PredictionID))
	assert.Equal(t, intent.Canonical(a), intent.Canonical(b))
	assert.NotEqual(t, intent.Canonical(a), intent.Canonical(func() intent.TradeIntent {
		c := a
		c.PredictionID = str(id.New[id.Any]().String())
		return c
	}()))
}
