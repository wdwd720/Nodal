//go:build integration

package reality_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/event/eventtest"
	"github.com/nodal/controlplane/internal/reality"
)

// The same event normalizes to the same timestamps, however many times it
// arrives (F-62).
//
// The pipeline is at-least-once and its doc comment says every step is
// idempotent downstream because ClickHouse's ReplacingMergeTree collapses
// duplicates. It could not, for a reason only a replay shows:
//
//   - `PutMeta` returns the ORIGINAL archived meta for an event already seen,
//     so `platform_received_at` -- the engine's VERSION column -- is frozen;
//   - `Normalize` was handed a fresh `clk.Now()`, so `normalized_at` and
//     therefore `decision_available_at` were strictly later on every pass.
//
// The version did not move and the replaced column did. Two rows with the same
// sort key carried different values, and which one survived was decided at
// merge time rather than by the data. Because the table partitions on
// `decision_available_at`, a replay landing in a different month produced two
// rows `FINAL` cannot collapse at all.
//
// Neither existing test could see it. The ClickHouse round-trip builds its
// duplicate by shifting `PlatformReceivedAt` AND `DecisionAvailableAt` together,
// so the version advances and "the latest platform_received_at wins" holds
// deterministically -- a premise the pipeline never produces. The pipeline test
// exercises the real path but runs on a fake clock that is never advanced
// between the delivery and the redelivery, so both normalizations produce
// identical timestamps and the divergence cannot appear.
//
// This test advances the clock, which is the whole point.

// pipelineFixture is the setup TestIntegration_Pipeline_* builds inline, minus
// the bus subscription this test does not need.
type pipelineFixture struct {
	pipeline *reality.Pipeline
	clk      *clock.Fake
	ds       reality.DataSource
	token    string
}

func newPipelineFixture(t *testing.T) *pipelineFixture {
	t.Helper()
	pool := openTestDB(t)
	ctx := context.Background()
	clk := clock.NewFake(fixedNow)
	ds := registerSource(t, pool, func(d *reality.DataSource) { d.HeartbeatTimeout = 30 * time.Second })
	ra, _ := newRawArchive(t, pool, clk)
	bus, err := eventtest.NewMemoryBus("TEST")
	require.NoError(t, err)
	norm, err := reality.NewChainNormalizer(ds.Code, reality.DedupProviderID, testPolicy)
	require.NoError(t, err)
	p, err := reality.NewPipeline(reality.PipelineConfig{
		DataSource: ds.Code, Stream: reality.StreamWalletEvents,
		Consumer: "worker-" + uniqueCode("c"), Topic: "market.test." + uniqueCode("t"),
	}, reality.PipelineDeps{DB: pool, Archive: ra, Normalizer: norm, Bus: bus, Sink: &memSink{}, Clock: clk})
	require.NoError(t, err)
	require.NoError(t, p.Start(ctx))
	require.NoError(t, p.Reconnect(ctx, "WalletAAA", fixedNow, "connected"))
	return &pipelineFixture{pipeline: p, clk: clk, ds: ds, token: uniqueToken()}
}

// now is the fixture's current instant, so a test can advance the clock and
// hand the pipeline a receipt time that moves with it -- or, for a replay,
// deliberately does not.
func (f *pipelineFixture) now() time.Time { return f.clk.Now() }

// raw builds a wallet event whose signature is stable for a given base, so the
// same base is the same logical event and a different one is not.
func (f *pipelineFixture) raw(base string, seq uint64, received time.Time) reality.RawObject {
	ev := sampleWalletEvent(received)
	ev.Observation.Signature = base + "-" + f.token
	ev.Sequence = &seq
	raw, err := reality.WalletEventRawObject(ev, f.ds.Code)
	if err != nil {
		panic(err)
	}
	return raw
}

func TestIntegration_ReplayingAnEventProducesTheSameTimestamps(t *testing.T) {
	f := newPipelineFixture(t)
	ctx := context.Background()

	// The receipt time is fixed: a replay is the SAME datum arriving again, so
	// the platform received it when it received it.
	first0 := f.now()
	first, err := f.pipeline.Ingest(ctx, f.raw("sigreplay", 40, first0))
	require.NoError(t, err)
	require.Len(t, first.Events, 1)
	require.False(t, first.Duplicate)

	// Time passes -- minutes, and enough to cross a month boundary, which is
	// where the partition key made a silent divergence into a permanent one.
	for _, advance := range []time.Duration{5 * time.Minute, 40 * 24 * time.Hour} {
		f.clk.Advance(advance)

		again, rerr := f.pipeline.Ingest(ctx, f.raw("sigreplay", 40, first0))
		require.NoError(t, rerr)
		require.True(t, again.Duplicate, "the archive must recognise this as the same object")
		require.Len(t, again.Events, 1)

		a, b := first.Events[0], again.Events[0]
		require.Equal(t, a.DedupID, b.DedupID, "the same event must keep its dedup id")

		// The sort key is (source, event_type, dedup_id) and the version is
		// platform_received_at. Every timestamp has to match, because any that
		// does not is a column the engine replaces without the version moving.
		assert.True(t, a.Timestamps.PlatformReceivedAt.Equal(b.Timestamps.PlatformReceivedAt),
			"platform_received_at is the version column and must not move")
		assert.True(t, a.Timestamps.NormalizedAt.Equal(b.Timestamps.NormalizedAt),
			"normalized_at moved on a replay: %s then %s",
			a.Timestamps.NormalizedAt, b.Timestamps.NormalizedAt)
		assert.True(t, a.Timestamps.FeatureAvailableAt.Equal(b.Timestamps.FeatureAvailableAt),
			"feature_available_at moved on a replay")
		assert.True(t, a.Timestamps.DecisionAvailableAt.Equal(b.Timestamps.DecisionAvailableAt),
			"decision_available_at moved on a replay: %s then %s -- the version column did not move with it, "+
				"so ReplacingMergeTree would resolve this event to whichever copy a merge happened to keep",
			a.Timestamps.DecisionAvailableAt, b.Timestamps.DecisionAvailableAt)

		// And the partition the row lands in, which is what makes a divergence
		// permanent rather than merely undefined.
		assert.Equal(t, a.Timestamps.DecisionAvailableAt.Format("200601"),
			b.Timestamps.DecisionAvailableAt.Format("200601"),
			"the copies land in different monthly partitions, which FINAL cannot collapse across")
	}
}

// TestIntegration_ADifferentEventStillGetsItsOwnTimestamps is the positive
// control. A pipeline that had simply frozen every timestamp -- to a constant,
// or to the first event's -- would pass every assertion above and record a
// stream in which nothing ever happens at a different time.
func TestIntegration_ADifferentEventStillGetsItsOwnTimestamps(t *testing.T) {
	f := newPipelineFixture(t)
	ctx := context.Background()

	first, err := f.pipeline.Ingest(ctx, f.raw("sigone", 50, f.now()))
	require.NoError(t, err)
	require.Len(t, first.Events, 1)

	f.clk.Advance(time.Hour)
	second, err := f.pipeline.Ingest(ctx, f.raw("sigtwo", 51, f.now()))
	require.NoError(t, err)
	require.Len(t, second.Events, 1)
	require.False(t, second.Duplicate)

	a, b := first.Events[0], second.Events[0]
	assert.NotEqual(t, a.DedupID, b.DedupID)
	assert.True(t, b.Timestamps.PlatformReceivedAt.After(a.Timestamps.PlatformReceivedAt),
		"a genuinely later event must carry a later receipt time")
	assert.True(t, b.Timestamps.DecisionAvailableAt.After(a.Timestamps.DecisionAvailableAt),
		"a genuinely later event must become decision-available later")
}

// TestReplayDeterminismIsAPropertyOfTheDerivation states the same rule without a
// database, which is where it actually lives: decision_available_at is a
// function of the archived receipt and the policy, and of nothing else.
func TestReplayDeterminismIsAPropertyOfTheDerivation(t *testing.T) {
	policy := reality.AvailabilityPolicy{FeatureLatency: 250 * time.Millisecond, PipelineLatency: time.Second}
	received := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	ts := reality.Timestamps{SourceEventAt: received.Add(-time.Minute), PlatformReceivedAt: received}

	// The value the pipeline now passes: the archived receipt, on every pass.
	out1, err := policy.Apply(ts, received)
	require.NoError(t, err)
	out2, err := policy.Apply(ts, received)
	require.NoError(t, err)
	assert.True(t, out1.DecisionAvailableAt.Equal(out2.DecisionAvailableAt))

	// The value it used to pass: a wall clock, which moves. This is the defect,
	// stated as an assertion so the reason for the change is not just a comment.
	wall, err := policy.Apply(ts, received.Add(3*time.Second))
	require.NoError(t, err)
	assert.False(t, wall.DecisionAvailableAt.Equal(out1.DecisionAvailableAt),
		"if a later normalizedAt no longer moved decision_available_at, this test would be asserting nothing")
}
