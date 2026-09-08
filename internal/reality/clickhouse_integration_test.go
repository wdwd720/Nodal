//go:build integration

package reality_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reality"

	"github.com/nodal/controlplane/internal/testkit/deps"
)

// The suite needs the LOCAL ClickHouse (docker-compose) or any server named
// by CP_TEST_CLICKHOUSE_ADDR (native 127.0.0.1:19000 or HTTP 127.0.0.1:18123).
// Every test creates its own suffixed tables and drops them afterwards.
func newTestClickHouse(t *testing.T) *reality.ClickHouseStore {
	t.Helper()
	addr := deps.Need(t, "CP_TEST_CLICKHOUSE_ADDR", "ClickHouse")
	cfg := config.ClickHouseConfig{
		Addr: addr, Database: envOr("CP_TEST_CLICKHOUSE_DATABASE", "controlplane"),
		UsernameRef: config.SecretRef(envOr("CP_TEST_CLICKHOUSE_USERNAME", "cp")), PasswordRef: config.SecretRef(envOr("CP_TEST_CLICKHOUSE_PASSWORD", "cp_local")),
	}
	suffix := "_t" + strings.ToLower(strings.ReplaceAll(id.New[testKind]().String(), "-", ""))[:12]
	store, err := reality.NewClickHouseStore(context.Background(), cfg, config.NewResolver(config.EnvTest, os.LookupEnv),
		reality.ClickHouseOptions{TableSuffix: suffix, Protocol: os.Getenv("CP_TEST_CLICKHOUSE_PROTOCOL"), TTLDays: 90})
	require.NoError(t, err)
	require.NoError(t, store.EnsureSchema(context.Background()))
	require.NoError(t, store.EnsureSchema(context.Background()), "schema application is idempotent")
	t.Cleanup(func() {
		_ = store.DropSchema(context.Background())
		_ = store.Close()
	})
	return store
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// newEvent builds a contract-valid event knowable at decision. The platform
// chain must satisfy platform_received_at <= normalized_at <=
// feature_available_at <= decision_available_at (POINT_IN_TIME.md §1), so the
// two intermediate instants are placed inside [received, decision] rather
// than at fixed offsets: a fixture with a decision latency under 2 ms would
// otherwise claim a decision could use a datum before the feature it derives
// from existed, which is not a state the pipeline can produce.
func newEvent(dedup string, received, decision time.Time) reality.NormalizedEvent {
	seq := uint64(len(dedup))
	step := time.Millisecond
	if span := decision.Sub(received); span < 2*step {
		step = span / 2 // zero when the datum is decision-available on receipt
	}
	normalized := received.Add(step)
	return reality.NormalizedEvent{
		EventID: reality.NewEventID().String(), DedupID: dedup, SchemaVersion: 1, Source: "helius.wallet_events", EventType: reality.EventTypeWalletTransaction,
		Sequence: &seq, SourcePartition: "WalletAAA", SourceOffset: dedup,
		Timestamps: reality.Timestamps{
			SourceEventAt: received.Add(-time.Second), ProviderPublishedAt: received.Add(-time.Millisecond), PlatformReceivedAt: received,
			NormalizedAt: normalized, FeatureAvailableAt: normalized.Add(step), DecisionAvailableAt: decision,
		},
		Wallet: "WalletAAA", RawObjectID: reality.NewRawObjectID().String(), RawObjectHash: []byte(strings.Repeat("h", 32)),
		Payload: json.RawMessage(fmt.Sprintf(`{"fee":"5000","signature":%q,"slot":"250000000"}`, dedup)),
	}
}

func TestIntegration_ClickHouse_NormalizedRoundTripAndReplacingDedup(t *testing.T) {
	store := newTestClickHouse(t)
	ctx := context.Background()
	base := fixedNow
	e1 := newEvent("sig1/WalletAAA", base, base.Add(250*time.Millisecond+123))
	e1.InstrumentID = id.New[testKind]().String()
	e2 := newEvent("sig2/WalletAAA", base.Add(time.Second), base.Add(time.Second+250*time.Millisecond))
	e2.Sequence = nil
	e2.Timestamps.ProviderPublishedAt = time.Time{}
	e2.Flags = []string{reality.GapKindOrderingAnomaly}
	require.NoError(t, store.InsertNormalized(ctx, []reality.NormalizedEvent{e1, e2}))
	// A redelivery of e1 received 5 s later (its whole platform-clock chain
	// shifts with it) replaces the first copy (ReplacingMergeTree + FINAL).
	dup := e1
	dup.EventID = reality.NewEventID().String()
	dup.Timestamps.PlatformReceivedAt = e1.Timestamps.PlatformReceivedAt.Add(5 * time.Second)
	dup.Timestamps.NormalizedAt = e1.Timestamps.NormalizedAt.Add(5 * time.Second)
	dup.Timestamps.FeatureAvailableAt = e1.Timestamps.FeatureAvailableAt.Add(5 * time.Second)
	dup.Timestamps.DecisionAvailableAt = e1.Timestamps.DecisionAvailableAt.Add(5 * time.Second)
	require.NoError(t, store.InsertNormalized(ctx, []reality.NormalizedEvent{dup}))

	got, err := store.QueryNormalized(ctx, reality.HistoricalQuery{Source: e1.Source, AsOf: base.Add(time.Hour)})
	require.NoError(t, err)
	require.Len(t, got, 2, "duplicates collapse on (source, event_type, dedup_id)")
	byDedup := map[string]reality.NormalizedEvent{}
	for _, e := range got {
		byDedup[e.DedupID] = e
	}
	g1 := byDedup[e1.DedupID]
	require.Equal(t, dup.EventID, g1.EventID, "the latest platform_received_at wins")
	require.Equal(t, dup.Timestamps.DecisionAvailableAt, g1.Timestamps.DecisionAvailableAt, "nanosecond precision survives")
	require.Equal(t, e1.Timestamps.ProviderPublishedAt, g1.Timestamps.ProviderPublishedAt)
	require.Equal(t, *e1.Sequence, *g1.Sequence)
	require.Equal(t, e1.InstrumentID, g1.InstrumentID)
	require.Equal(t, e1.RawObjectID, g1.RawObjectID)
	require.Equal(t, e1.RawObjectHash, g1.RawObjectHash)
	require.JSONEq(t, string(e1.Payload), string(g1.Payload))
	require.Equal(t, "WalletAAA", g1.Wallet)
	g2 := byDedup[e2.DedupID]
	require.Nil(t, g2.Sequence)
	require.True(t, g2.Timestamps.ProviderPublishedAt.IsZero())
	require.Empty(t, g2.InstrumentID)

	filtered, err := store.QueryNormalized(ctx, reality.HistoricalQuery{Source: e1.Source, EventType: e1.EventType, Wallet: "WalletAAA", From: base.Add(time.Second), AsOf: base.Add(time.Hour), Limit: 1})
	require.NoError(t, err)
	require.Len(t, filtered, 1)
	require.Equal(t, e2.DedupID, filtered[0].DedupID)

	_, err = store.QueryNormalized(ctx, reality.HistoricalQuery{Source: e1.Source})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "as_of is mandatory")
	bad := e1
	bad.Timestamps.DecisionAvailableAt = bad.Timestamps.PlatformReceivedAt.Add(-time.Nanosecond)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(store.InsertNormalized(ctx, []reality.NormalizedEvent{bad})), "disordered timestamps never reach the store")
}

func TestIntegration_ClickHouse_MarketPricesAreExactInt128(t *testing.T) {
	store := newTestClickHouse(t)
	ctx := context.Background()
	instrument, quote, raw := id.New[testKind]().String(), id.New[testKind]().String(), id.New[testKind]().String()
	huge, ok := new(big.Int).SetString("1267650600228229401496703205383", 10) // 2^100 + 7
	require.True(t, ok)
	hugeQ, err := money.ParseQuantity(huge.String())
	require.NoError(t, err)
	minQ, err := money.ParseQuantity("-170141183460469231731687303715884105728") // -2^127
	require.NoError(t, err)
	maxQ, err := money.ParseQuantity("170141183460469231731687303715884105727") // 2^127-1
	require.NoError(t, err)
	liq, err := money.ParseQuantity("999999999999999999999999999999")
	require.NoError(t, err)
	t0 := fixedNow
	prices := []reality.MarketPrice{
		{InstrumentID: instrument, Source: "jupiter", PriceMantissa: hugeQ, PriceScale: 18, QuoteAssetID: quote, LiquidityMantissa: &liq, LiquidityScale: 6, SourceEventAt: t0, PlatformReceivedAt: t0, DecisionAvailableAt: t0.Add(250 * time.Millisecond), RawObjectID: raw, DedupID: "p1"},
		{InstrumentID: instrument, Source: "jupiter", PriceMantissa: minQ, PriceScale: 0, QuoteAssetID: quote, SourceEventAt: t0.Add(time.Second), PlatformReceivedAt: t0.Add(time.Second), DecisionAvailableAt: t0.Add(time.Second + 250*time.Millisecond), RawObjectID: raw, DedupID: "p2"},
		{InstrumentID: instrument, Source: "jupiter", PriceMantissa: maxQ, PriceScale: 9, QuoteAssetID: quote, SourceEventAt: t0.Add(2 * time.Second), PlatformReceivedAt: t0.Add(2 * time.Second), DecisionAvailableAt: t0.Add(2*time.Second + 250*time.Millisecond), RawObjectID: raw, DedupID: "p3"},
	}
	require.NoError(t, store.InsertMarketPrices(ctx, prices))
	got, err := store.QueryMarketPrices(ctx, reality.PriceQuery{InstrumentID: instrument, AsOf: t0.Add(time.Hour)})
	require.NoError(t, err)
	require.Len(t, got, 3)
	require.Equal(t, "p3", got[0].DedupID, "newest first")
	byID := map[string]reality.MarketPrice{}
	for _, p := range got {
		byID[p.DedupID] = p
	}
	require.Equal(t, huge.String(), byID["p1"].PriceMantissa.String(), "2^100+7 survives exactly (no float64 path)")
	require.Equal(t, uint8(18), byID["p1"].PriceScale)
	require.Equal(t, liq.String(), byID["p1"].LiquidityMantissa.String())
	require.Equal(t, minQ.String(), byID["p2"].PriceMantissa.String())
	require.Nil(t, byID["p2"].LiquidityMantissa)
	require.Equal(t, maxQ.String(), byID["p3"].PriceMantissa.String())

	latest, found, err := store.LatestPrice(ctx, instrument, "jupiter", t0.Add(time.Second+250*time.Millisecond))
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "p2", latest.DedupID, "p3 is not yet decision-available at that instant")
	_, found, err = store.LatestPrice(ctx, instrument, "jupiter", t0)
	require.NoError(t, err)
	require.False(t, found, "nothing is knowable before the first decision_available_at")

	tooBig, err := money.ParseQuantity("170141183460469231731687303715884105728") // 2^127
	require.NoError(t, err)
	over := prices[0]
	over.PriceMantissa, over.DedupID = tooBig, "p4"
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(store.InsertMarketPrices(ctx, []reality.MarketPrice{over})), "Int128 overflow is refused, never truncated")
}

// PART 226: the explicit look-ahead leakage test. Events whose
// decision_available_at is after the simulated time must be invisible, and
// the boundary is exact to the nanosecond.
func TestIntegration_ClickHouse_LookAheadLeakage(t *testing.T) {
	store := newTestClickHouse(t)
	ctx := context.Background()
	simulated := fixedNow.Add(10 * time.Second)
	var events []reality.NormalizedEvent
	offsets := []time.Duration{-5 * time.Second, -time.Second, -time.Nanosecond, 0, time.Nanosecond, time.Millisecond, time.Second, time.Hour}
	for i, off := range offsets {
		received := simulated.Add(off - 250*time.Millisecond)
		events = append(events, newEvent(fmt.Sprintf("leak-%d", i), received, simulated.Add(off)))
	}
	require.NoError(t, store.InsertNormalized(ctx, events))

	visible, err := store.QueryNormalized(ctx, reality.HistoricalQuery{Source: events[0].Source, AsOf: simulated})
	require.NoError(t, err)
	var ids []string
	for _, e := range visible {
		ids = append(ids, e.DedupID)
		require.False(t, e.Timestamps.DecisionAvailableAt.After(simulated), "leak: %s decision-available %s after simulated %s", e.DedupID, e.Timestamps.DecisionAvailableAt, simulated)
	}
	sort.Strings(ids)
	require.Equal(t, []string{"leak-0", "leak-1", "leak-2", "leak-3"}, ids, "exactly the events knowable at T, including the one at T itself")

	earlier, err := store.QueryNormalized(ctx, reality.HistoricalQuery{Source: events[0].Source, AsOf: simulated.Add(-time.Nanosecond)})
	require.NoError(t, err)
	require.Len(t, earlier, 3, "one nanosecond earlier drops the event available exactly at T")

	all, err := store.QueryNormalized(ctx, reality.HistoricalQuery{Source: events[0].Source, AsOf: simulated.Add(2 * time.Hour)})
	require.NoError(t, err)
	require.Len(t, all, len(offsets), "every event exists; only knowledge time hides them")
}

// Property (POINT_IN_TIME.md §11): for random decision times and a random
// T, the result set is exactly {decision_available_at <= T}; shifting every
// decision_available_at by +ε drops precisely the events that cross T.
func TestProp_ClickHouse_SnapshotNeverReturnsFutureKnowledge(t *testing.T) {
	store := newTestClickHouse(t)
	ctx := context.Background()
	rapid.Check(t, func(rt *rapid.T) {
		// One source per iteration: rapid may replay an iteration while
		// shrinking, and the store keeps earlier rows.
		token := strings.ToLower(strings.ReplaceAll(id.New[testKind]().String(), "-", ""))[:12]
		source := "prop." + token
		run := 0
		n := rapid.IntRange(1, 12).Draw(rt, "n")
		epsilon := time.Duration(rapid.Int64Range(1, int64(time.Second)).Draw(rt, "epsilon"))
		base := fixedNow
		T := base.Add(time.Duration(rapid.Int64Range(0, int64(10*time.Second)).Draw(rt, "T")))
		var original, shifted []reality.NormalizedEvent
		expected, expectedShifted := map[string]bool{}, map[string]bool{}
		for i := 0; i < n; i++ {
			received := base.Add(time.Duration(rapid.Int64Range(0, int64(10*time.Second)).Draw(rt, fmt.Sprintf("recv_%d", i))))
			decision := received.Add(time.Duration(rapid.Int64Range(0, int64(2*time.Second)).Draw(rt, fmt.Sprintf("lat_%d", i))))
			e := newEvent(fmt.Sprintf("r%d-e%d", run, i), received, decision)
			e.Source = source
			original = append(original, e)
			if !decision.After(T) {
				expected[e.DedupID] = true
			}
			s := e
			s.EventID = reality.NewEventID().String()
			s.DedupID = e.DedupID + "+eps"
			s.Timestamps.DecisionAvailableAt = decision.Add(epsilon)
			shifted = append(shifted, s)
			if !s.Timestamps.DecisionAvailableAt.After(T) {
				expectedShifted[s.DedupID] = true
			}
		}
		require.NoError(rt, store.InsertNormalized(ctx, append(original, shifted...)))
		got, err := store.QueryNormalized(ctx, reality.HistoricalQuery{Source: source, AsOf: T, Limit: reality.MaxQueryLimit})
		require.NoError(rt, err)
		seen := map[string]bool{}
		for _, e := range got {
			require.False(rt, e.Timestamps.DecisionAvailableAt.After(T), "leak at T=%s: %s", T, e.DedupID)
			seen[e.DedupID] = true
		}
		for d := range expected {
			require.True(rt, seen[d], "knowable event %s missing", d)
		}
		for d := range expectedShifted {
			require.True(rt, seen[d], "knowable shifted event %s missing", d)
		}
		require.Len(rt, seen, len(expected)+len(expectedShifted))
		for d := range seen {
			require.True(rt, expected[d] || expectedShifted[d], "unexpected %s", d)
		}
	})
}

func TestIntegration_ClickHouse_ConstructorRules(t *testing.T) {
	addr := deps.Need(t, "CP_TEST_CLICKHOUSE_ADDR", "ClickHouse")
	ctx := context.Background()
	resolver := config.NewResolver(config.EnvTest, os.LookupEnv)
	_, err := reality.NewClickHouseStore(ctx, config.ClickHouseConfig{Addr: addr, Database: "controlplane", UsernameRef: "cp", PasswordRef: "cp_local"}, resolver, reality.ClickHouseOptions{TableSuffix: "Bad-Suffix"})
	require.Error(t, err, "table suffix is validated (it is interpolated into DDL)")
	_, err = reality.NewClickHouseStore(ctx, config.ClickHouseConfig{Addr: addr, Database: "controlplane", UsernameRef: "cp", PasswordRef: "wrong"}, resolver, reality.ClickHouseOptions{TableSuffix: "_x"})
	require.Error(t, err, "bad credentials fail at construction (ping)")
	_, err = reality.NewClickHouseStore(ctx, config.ClickHouseConfig{Addr: "127.0.0.1:1", Database: "controlplane"}, resolver, reality.ClickHouseOptions{DialTimeout: time.Second})
	require.Error(t, err, "unreachable server fails fast")
	unsuffixed, err := reality.NewClickHouseStore(ctx, config.ClickHouseConfig{Addr: addr, Database: "controlplane", UsernameRef: "cp", PasswordRef: "cp_local"}, resolver, reality.ClickHouseOptions{})
	require.NoError(t, err)
	defer func() { _ = unsuffixed.Close() }()
	require.Equal(t, errs.CodeForbidden, errs.CodeOf(unsuffixed.DropSchema(ctx)), "production tables can never be dropped through the store")
}
