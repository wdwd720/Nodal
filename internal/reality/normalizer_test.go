package reality_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reality"
)

var testPolicy = reality.AvailabilityPolicy{FeatureLatency: 5 * time.Millisecond, PipelineLatency: 250 * time.Millisecond, Version: "test"}

func sampleWalletEvent(received time.Time) chain.WalletEvent {
	block := received.Add(-3 * time.Second)
	published := received.Add(-time.Second)
	seq := chain.SequenceOf(250_000_000, 7)
	return chain.WalletEvent{
		Kind: chain.EventTransaction, Origin: chain.OriginStream, Wallet: "WalletAAA", Source: "helius", ReceivedAt: received,
		Sequence: &seq, ProviderPublishedAt: &published,
		Observation: chain.TxObservation{
			Signature: "5VfYd8sig", Found: true, Slot: 250_000_000, BlockTime: &block, Commitment: chain.CommitmentConfirmed, Version: chain.VersionV0,
			AccountKeys: []string{"WalletAAA"}, Fee: money.QuantityFromInt64(5000),
			TokenBalanceDeltas: []chain.TokenDelta{{Owner: "WalletAAA", Mint: "USDC", TokenAccount: "ata-usdc", Pre: money.QuantityFromInt64(1_000_000), Post: money.QuantityFromInt64(250_000), Decimals: 6}},
			LamportDeltas:      []chain.LamportDelta{{Account: "WalletAAA", Pre: money.QuantityFromInt64(10_000_000), Post: money.QuantityFromInt64(9_995_000)}},
			Source:             "helius", ObservedAt: received, ReceivedAt: received, RawRef: "s3://raw-events/x",
		},
	}
}

func sampleMeta(received time.Time) reality.RawObjectMeta {
	return reality.RawObjectMeta{
		ObjectID: reality.NewRawObjectID().String(), DataSource: "helius.wallet_events", Provider: "helius", EventType: reality.RawEventTypeWalletEvent,
		DedupKey: "5VfYd8sig/WalletAAA", SchemaVersion: 1, Hash: make([]byte, 32), Partition: "WalletAAA", Offset: "5VfYd8sig",
		Timestamps: reality.Timestamps{PlatformReceivedAt: received},
	}
}

// failer is the subset of testing.TB that both *testing.T/F and *rapid.T
// provide.
type failer interface {
	Helper()
	Fatalf(format string, args ...any)
}

func mustJSON(t failer, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestChainNormalizer_ProducesCanonicalEventWithSixTimestamps(t *testing.T) {
	t.Parallel()
	n, err := reality.NewChainNormalizer("helius.wallet_events", reality.DedupProviderID, testPolicy)
	require.NoError(t, err)
	ev := sampleWalletEvent(fixedNow)
	now := fixedNow.Add(2 * time.Millisecond)
	events, err := n.Normalize(sampleMeta(fixedNow), mustJSON(t, ev), now)
	require.NoError(t, err)
	require.Len(t, events, 1)
	e := events[0]
	require.NoError(t, e.Validate())
	require.Equal(t, "5VfYd8sig/WalletAAA", e.DedupID)
	require.Equal(t, reality.EventTypeWalletTransaction, e.EventType)
	require.Equal(t, "WalletAAA", e.Wallet)
	require.Equal(t, *ev.Sequence, *e.Sequence)
	ts := e.Timestamps
	require.Equal(t, ev.Observation.BlockTime.UTC(), ts.SourceEventAt, "source_event_at is the chain clock")
	require.Equal(t, ev.ProviderPublishedAt.UTC(), ts.ProviderPublishedAt)
	require.Equal(t, fixedNow, ts.PlatformReceivedAt)
	require.Equal(t, now, ts.NormalizedAt)
	require.Equal(t, now.Add(5*time.Millisecond), ts.FeatureAvailableAt)
	require.Equal(t, now.Add(255*time.Millisecond), ts.DecisionAvailableAt)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(e.Payload, &payload))
	require.Equal(t, "5000", payload["fee"], "numbers are strings")
	require.Equal(t, "250000000", payload["slot"])
	deltas, _ := payload["token_deltas"].([]any)
	require.Len(t, deltas, 1)
	d, _ := deltas[0].(map[string]any)
	require.Equal(t, "-750000", d["delta"])
	require.Equal(t, "6", d["decimals"])
	for _, v := range payload {
		_, isNumber := v.(float64)
		require.False(t, isNumber, "payload must not carry JSON numbers")
	}
	// Canonical: sorted keys, stable across runs.
	events2, err := n.Normalize(sampleMeta(fixedNow), mustJSON(t, ev), now)
	require.NoError(t, err)
	require.JSONEq(t, string(e.Payload), string(events2[0].Payload))
	require.Equal(t, string(e.Payload), string(events2[0].Payload))
	require.True(t, strings.HasPrefix(string(e.Payload), `{"block_time":`), "keys sorted: %s", e.Payload)
	require.NotEqual(t, e.EventID, events2[0].EventID, "each normalization mints its own event id; dedup is by dedup_id")
	require.Equal(t, e.DedupID, events2[0].DedupID)
}

func TestChainNormalizer_ReconnectYieldsNoEventAndCompositeStrategy(t *testing.T) {
	t.Parallel()
	n, err := reality.NewChainNormalizer("helius.wallet_events", reality.DedupCompositeHash, testPolicy)
	require.NoError(t, err)
	events, err := n.Normalize(sampleMeta(fixedNow), mustJSON(t, chain.WalletEvent{Kind: chain.EventReconnect, ReceivedAt: fixedNow}), fixedNow)
	require.NoError(t, err)
	require.Empty(t, events)

	events, err = n.Normalize(sampleMeta(fixedNow), mustJSON(t, sampleWalletEvent(fixedNow)), fixedNow)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, reality.CompositeHash("helius.wallet_events", reality.EventTypeWalletTransaction, map[string]string{"signature": "5VfYd8sig", "wallet": "WalletAAA"}), events[0].DedupID)
}

func TestChainNormalizer_UnknownBlockTimeFallsBackToReceipt(t *testing.T) {
	t.Parallel()
	n, err := reality.NewChainNormalizer("src", reality.DedupProviderID, testPolicy)
	require.NoError(t, err)
	ev := sampleWalletEvent(fixedNow)
	ev.Observation.BlockTime = nil
	ev.Sequence = nil
	events, err := n.Normalize(sampleMeta(fixedNow), mustJSON(t, ev), fixedNow)
	require.NoError(t, err)
	require.Equal(t, fixedNow, events[0].Timestamps.SourceEventAt)
	require.Equal(t, chain.SequenceOf(250_000_000, 0), *events[0].Sequence, "slot-only sequence when the index is unknown")
	require.Equal(t, 0*time.Second, events[0].Timestamps.Age(fixedNow))
}

func TestDecodeWalletEvent_Rejections(t *testing.T) {
	t.Parallel()
	base := sampleWalletEvent(fixedNow)
	cases := map[string]func() []byte{
		"empty":            func() []byte { return nil },
		"not json":         func() []byte { return []byte("{") },
		"trailing":         func() []byte { return append(mustJSON(t, base), '1') },
		"unknown kind":     func() []byte { e := base; e.Kind = "WEIRD"; return mustJSON(t, e) },
		"missing sig":      func() []byte { e := base; e.Observation.Signature = ""; return mustJSON(t, e) },
		"missing wallet":   func() []byte { e := base; e.Wallet = ""; return mustJSON(t, e) },
		"not found":        func() []byte { e := base; e.Observation.Found = false; return mustJSON(t, e) },
		"control in sig":   func() []byte { e := base; e.Observation.Signature = "a\x01b"; return mustJSON(t, e) },
		"bad commitment":   func() []byte { e := base; e.Observation.Commitment = "eventually"; return mustJSON(t, e) },
		"bad origin":       func() []byte { e := base; e.Origin = "MAGIC"; return mustJSON(t, e) },
		"missing received": func() []byte { e := base; e.ReceivedAt = time.Time{}; return mustJSON(t, e) },
		"bad quantity": func() []byte {
			return []byte(strings.Replace(string(mustJSON(t, base)), `"fee":"5000"`, `"fee":"5e3"`, 1))
		},
		"too large": func() []byte { return append(mustJSON(t, base), make([]byte, reality.MaxWalletEventBytes)...) },
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := reality.DecodeWalletEvent(body())
			require.Error(t, err)
			require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

func TestNormalizedEvent_EncodeDecodeRoundTrip(t *testing.T) {
	t.Parallel()
	n, err := reality.NewChainNormalizer("src", reality.DedupProviderID, testPolicy)
	require.NoError(t, err)
	events, err := n.Normalize(sampleMeta(fixedNow), mustJSON(t, sampleWalletEvent(fixedNow)), fixedNow)
	require.NoError(t, err)
	b, err := events[0].Encode()
	require.NoError(t, err)
	got, err := reality.DecodeNormalizedEvent(b)
	require.NoError(t, err)
	require.Equal(t, events[0].EventID, got.EventID)
	require.Equal(t, events[0].Timestamps, got.Timestamps)
	require.Equal(t, events[0].RawObjectHash, got.RawObjectHash)
	require.JSONEq(t, string(events[0].Payload), string(got.Payload))
	_, err = reality.DecodeNormalizedEvent([]byte(`{"event_id":"nope"}`))
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

// Property: whatever the provider says about time, the normalized event's
// platform timestamps are ordered and the decision instant is never before
// receipt + pipeline latency.
func TestProp_NormalizerTimestampInvariant(t *testing.T) {
	t.Parallel()
	n, err := reality.NewChainNormalizer("src", reality.DedupProviderID, testPolicy)
	require.NoError(t, err)
	rapid.Check(t, func(rt *rapid.T) {
		received := genTime(rt, "received")
		ev := sampleWalletEvent(received)
		block := genTime(rt, "block")
		published := genTime(rt, "published")
		if rapid.Bool().Draw(rt, "nil_block") {
			ev.Observation.BlockTime = nil
		} else {
			ev.Observation.BlockTime = &block
		}
		if rapid.Bool().Draw(rt, "nil_published") {
			ev.ProviderPublishedAt = nil
		} else {
			ev.ProviderPublishedAt = &published
		}
		ev.Observation.Slot = rapid.Uint64Range(0, 1<<40).Draw(rt, "slot")
		ev.Sequence = nil
		now := received.Add(time.Duration(rapid.Int64Range(-int64(time.Second), int64(time.Minute)).Draw(rt, "skew")))
		events, err := n.Normalize(sampleMeta(received), mustJSON(rt, ev), now)
		require.NoError(rt, err)
		require.Len(rt, events, 1)
		ts := events[0].Timestamps
		require.NoError(rt, ts.Validate())
		require.False(rt, ts.DecisionAvailableAt.Before(received.Add(testPolicy.PipelineLatency)))
		require.False(rt, ts.NormalizedAt.Before(ts.PlatformReceivedAt))
		require.False(rt, ts.FeatureAvailableAt.Before(ts.NormalizedAt))
		require.False(rt, ts.DecisionAvailableAt.Before(ts.FeatureAvailableAt))
		require.True(rt, ts.Age(ts.DecisionAvailableAt) >= testPolicy.PipelineLatency, "a provider clock in the future cannot make the datum look fresher than receipt")
	})
}

func FuzzDecodeWalletEvent(f *testing.F) {
	f.Add(mustJSON(f, sampleWalletEvent(fixedNow)))
	f.Add(mustJSON(f, chain.WalletEvent{Kind: chain.EventReconnect, ReceivedAt: fixedNow}))
	f.Add([]byte(`{"kind":"TRANSACTION","wallet":"w","received_at":"2026-09-05T12:00:00Z","observation":{"signature":"s","found":true,"fee":"1"}}`))
	f.Add([]byte(`{"kind":"TRANSACTION","observation":{"fee":"-"}}`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"observation":{"token_balance_deltas":[{"pre":"1e9"}]}}`))
	n, err := reality.NewChainNormalizer("src", reality.DedupProviderID, testPolicy)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		ev, err := reality.DecodeWalletEvent(body)
		if err != nil {
			if errs.CodeOf(err) != errs.CodeValidationFailed {
				t.Fatalf("decoder must fail with VALIDATION_FAILED, got %v", err)
			}
			return
		}
		events, nerr := n.Normalize(sampleMeta(fixedNow), body, fixedNow)
		if nerr != nil {
			if errs.CodeOf(nerr) != errs.CodeValidationFailed {
				t.Fatalf("normalizer must fail with VALIDATION_FAILED, got %v", nerr)
			}
			return
		}
		if ev.Kind == chain.EventReconnect {
			if len(events) != 0 {
				t.Fatal("reconnect must not produce events")
			}
			return
		}
		for _, e := range events {
			if err := e.Validate(); err != nil {
				t.Fatalf("normalized event invalid: %v", err)
			}
			if e.Timestamps.DecisionAvailableAt.Before(fixedNow) {
				t.Fatal("decision availability before receipt")
			}
		}
	})
}
