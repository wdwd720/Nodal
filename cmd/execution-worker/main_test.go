package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/quote"
)

func TestRun_Usage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no arguments", nil, exitUsage},
		{"unknown command", []string{"frobnicate"}, exitUsage},
		{"run with arguments", []string{"run", "extra"}, exitUsage},
		{"plan without an id", []string{"plan"}, exitUsage},
		{"plan with two ids", []string{"plan", "a", "b"}, exitUsage},
		{"help", []string{"help"}, exitOK},
		{"long help", []string{"--help"}, exitOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			code := run(tc.args, func(string) (string, bool) { return "", false }, &out, &errOut)
			assert.Equal(t, tc.want, code)
			if tc.want == exitOK {
				assert.Contains(t, out.String(), "usage: execution-worker")
			}
		})
	}
}

// No environment variable may turn this binary into a live trader. wire must
// refuse to build an executor it cannot fully assemble, rather than starting
// with a half-wired one.
func TestWire_RefusesWithoutAProviderBinding(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	code := run([]string{"run"}, func(k string) (string, bool) {
		switch k {
		case "CP_ENV":
			return "LOCAL", true
		case "CP_DATABASE_URL":
			return "postgres://cp_app:cp_app_local@127.0.0.1:5433/controlplane_test_workers?sslmode=disable", true
		}
		return "", false
	}, &out, &errOut)
	require.Equal(t, exitFailure, code)
	assert.Contains(t, errOut.String(), "no execution provider binding",
		"a worker that cannot name its venue adapter refuses to start")
}

func TestRunnerOptions_FromEnvironment(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		envConcurrency:  "9",
		envPollInterval: "42ms",
		envLeaseTTL:     "2m",
		envDrainTimeout: "7s",
		envOwner:        "  worker-7  ",
	}
	o, err := runnerOptions(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	require.NoError(t, err)
	assert.Equal(t, 9, o.Concurrency)
	assert.Equal(t, 42*time.Millisecond, o.PollInterval)
	assert.Equal(t, 2*time.Minute, o.LeaseTTL)
	assert.Equal(t, 7*time.Second, o.DrainTimeout)
	assert.Equal(t, "worker-7", o.Owner)

	// Defaults, and a non-empty owner without configuration.
	o, err = runnerOptions(func(string) (string, bool) { return "", false })
	require.NoError(t, err)
	assert.Equal(t, DefaultConcurrency, o.Concurrency)
	assert.NotEmpty(t, o.Owner, "a lease always names an owner")

	for name, bad := range map[string]string{
		envConcurrency:  "0",
		envPollInterval: "-1s",
		envLeaseTTL:     "soon",
		envDrainTimeout: "0s",
	} {
		_, err := runnerOptions(func(k string) (string, bool) {
			if k == name {
				return bad, true
			}
			return "", false
		})
		assert.Error(t, err, "%s=%s", name, bad)
	}
}

func TestErrorText(t *testing.T) {
	t.Parallel()
	assert.Empty(t, errorText(nil))
	assert.Equal(t, "NOT_FOUND: no such plan", errorText(errs.New(errs.CodeNotFound, "no such plan")))
	assert.Equal(t, "boom", errorText(errors.New("boom")))

	long := errs.New(errs.CodeInternal, string(bytes.Repeat([]byte("x"), 4000)))
	assert.Len(t, errorText(long), maxLastError, "the stored error is bounded")
}

// toQuoteRow is the one place the executor's provider-neutral snapshot meets
// the quotes table. A quote that does not validate cannot be stored, so the
// mapping is checked directly rather than only through the executor.
func TestToQuoteRow_ProducesAValidQuote(t *testing.T) {
	t.Parallel()
	snap := sampleSnapshot()
	row, err := toQuoteRow(snap)
	require.NoError(t, err)
	require.NoError(t, row.Validate())

	assert.Equal(t, quote.SideBuy, row.Side)
	assert.Equal(t, snap.InputQuantity, row.InputQuantity)
	assert.Equal(t, snap.ExpectedOutput, row.ExpectedOutput)
	assert.Equal(t, snap.MinimumOutput, row.MinimumOutput)
	assert.Equal(t, snap.PriceImpactBPS, row.PriceImpactBPS)
	assert.Equal(t, snap.SlippageBPS, row.SlippageBPS)
	assert.Equal(t, snap.RawResponseHash, row.RawResponseHash, "the provider's own digest is preserved")
	assert.Equal(t, quote.RouteHash(snap.RouteSummary), row.RouteHash,
		"route_hash is the canonical hash of route_summary, not whatever the adapter computed")
	assert.Equal(t, row.QuoteAssetID().String(), row.EffectivePrice.QuoteAsset)
	assert.Equal(t, snap.Provider, row.EffectivePrice.Source)
	assert.True(t, row.EffectivePrice.At.Equal(row.ReceivedAt))
	require.NotNil(t, row.EstNetworkCostAssetID)
	assert.Nil(t, row.PlatformFeeAssetID, "a zero platform fee names no asset")
}

func TestToQuoteRow_SellSideUsesTheOutputAsset(t *testing.T) {
	t.Parallel()
	snap := sampleSnapshot()
	snap.Side = execution.SideSell
	row, err := toQuoteRow(snap)
	require.NoError(t, err)
	require.NoError(t, row.Validate())
	assert.Equal(t, quote.SideSell, row.Side)
	assert.Equal(t, row.OutputAssetID.String(), row.EffectivePrice.QuoteAsset)
}

func TestToQuoteRow_RejectsMalformedIdentifiers(t *testing.T) {
	t.Parallel()
	for _, mutate := range []func(*execution.QuoteSnapshot){
		func(q *execution.QuoteSnapshot) { q.InstrumentID = "not-a-uuid" },
		func(q *execution.QuoteSnapshot) { q.VenueListingID = "not-a-uuid" },
		func(q *execution.QuoteSnapshot) { q.IntentID = "not-a-uuid" },
	} {
		snap := sampleSnapshot()
		mutate(&snap)
		_, err := toQuoteRow(snap)
		require.Error(t, err)
		assert.True(t, errs.HasCode(err, errs.CodeValidationFailed), "%v", err)
	}
}

func sampleSnapshot() execution.QuoteSnapshot {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	route := json.RawMessage(`[{"venue":"jupiter","pool":"fake"}]`)
	return execution.QuoteSnapshot{
		IntentID:          id.New[id.Any]().String(),
		Provider:          "jupiter",
		ProviderRequestID: "qreq-1",
		InstrumentID:      id.New[id.Any]().String(),
		VenueListingID:    id.New[id.Any]().String(),
		Side:              execution.SideBuy,
		InputAsset:        assets.NewAssetID(),
		InputQuantity:     money.QuantityFromInt64(100_000_000),
		OutputAsset:       assets.NewAssetID(),
		ExpectedOutput:    money.QuantityFromInt64(15_000_000),
		MinimumOutput:     money.QuantityFromInt64(14_925_000),
		EffectivePrice:    money.Price{Mantissa: money.QuantityFromInt64(150), Scale: 0},
		PriceImpactBPS:    5,
		SlippageBPS:       50,
		EstNetworkCost:    money.QuantityFromInt64(5000),
		EstNetworkAsset:   assets.NewAssetID(),
		ReceivedAt:        now,
		ExpiresAt:         now.Add(30 * time.Second),
		RouteSummary:      route,
		RawResponseRef:    "evidence://quote/1",
		RawResponseHash:   bytes.Repeat([]byte{0xab}, 32),
	}
}

// The waker acknowledges anything it does not care about. Refusing to
// acknowledge would stall the partition for no gain: a wake is an
// optimisation, never a financial effect.
func TestWaker_IgnoresUnknownTopics(t *testing.T) {
	t.Parallel()
	w := &Waker{}
	msg := unwiredMessage(t, "gate.transitioned", `{"gate_id":"g1"}`)
	require.NoError(t, w.Handle(t.Context(), msg))
}

func TestWaker_RejectsAnUndecodableMessage(t *testing.T) {
	t.Parallel()
	w := &Waker{}
	err := w.Handle(t.Context(), event.Message{ID: "x", Topic: "order.transitioned", Value: []byte("not json")})
	require.Error(t, err)
	assert.True(t, errs.HasCode(err, errs.CodeValidationFailed), "%v", err)
}

func TestNewWaker_RequiresDependencies(t *testing.T) {
	t.Parallel()
	_, err := NewWaker(nil, event.NewInbox(nil), nil)
	require.Error(t, err)
	_, err = NewWaker(nil, nil, nil)
	require.Error(t, err)
}

func TestWakeTopicsAreRegistered(t *testing.T) {
	t.Parallel()
	require.NotEmpty(t, WakeTopics)
	for _, topic := range WakeTopics {
		_, ok := event.Lookup(topic)
		assert.True(t, ok, "%s must be a registered topic", topic)
		assert.True(t, isWakeTopic(topic))
	}
	assert.False(t, isWakeTopic(event.TopicKillSwitchChanged))
}

func unwiredMessage(t *testing.T, eventType, payload string) event.Message {
	t.Helper()
	env := event.Envelope{
		ID: event.NewEventID().String(), Type: eventType, SchemaVersion: event.Topic(eventType).Version(),
		Source: "test", AggregateType: "capability_gate", AggregateID: id.New[id.Any]().String(),
		OccurredAt: time.Now().UTC(), RecordedAt: time.Now().UTC(), Payload: json.RawMessage(payload),
	}
	value, err := env.CanonicalBytes()
	require.NoError(t, err)
	return event.Message{ID: env.ID, Topic: eventType, Value: value}
}

// The wake-up subscription is an optimisation on top of polling. A worker
// with no bus must still start, and must say so rather than failing.
func TestBindBus_DefaultsToNoBus(t *testing.T) {
	t.Parallel()
	bus, err := bindBus(nil)
	require.NoError(t, err)
	assert.Nil(t, bus, "no bus is wired in this build; plans are picked up by polling")
}
