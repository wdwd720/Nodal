//go:build integration && chaos

package chaos

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/reality/redpandabus"
)

// brokerSeeds returns the LOCAL Redpanda brokers, skipping when unset.
func brokerSeeds(t *testing.T) []string {
	t.Helper()
	v := os.Getenv("CP_TEST_REDPANDA_BROKERS")
	if v == "" {
		t.Skip("CP_TEST_REDPANDA_BROKERS not set; skipping broker chaos test")
	}
	var out []string
	for _, b := range strings.Split(v, ",") {
		if b = strings.TrimSpace(b); b != "" {
			out = append(out, b)
		}
	}
	require.NotEmpty(t, out)
	return out
}

// lyingBus wraps a real bus and, when armed, converts a publish failure into
// a nil return. That is precisely the pre-D-034 behavior: `ProduceSync`
// against a paused broker blocked and then reported success. It exists only
// as the negative control for TestChaos_BrokerStallNeverMarksOutboxPublished,
// so the guard can be observed failing.
type lyingBus struct {
	inner     event.Bus
	lie       bool
	lies      atomic.Int64
	published atomic.Int64
}

func (b *lyingBus) Publish(ctx context.Context, topic, key string, value []byte, headers map[string]string) error {
	err := b.inner.Publish(ctx, topic, key, value, headers)
	if err != nil && b.lie {
		b.lies.Add(1)
		return nil // "published", allegedly
	}
	if err == nil {
		b.published.Add(1)
	}
	return err
}

func (b *lyingBus) Subscribe(ctx context.Context, topic, group string, h event.Handler) error {
	return b.inner.Subscribe(ctx, topic, group, h)
}
func (b *lyingBus) Close(ctx context.Context) error { return b.inner.Close(ctx) }

// TestChaos_BrokerStallNeverMarksOutboxPublished is the invariant "no lost
// financial event", asserted one level above where D-034 was fixed.
//
// D-034 fixed the *bus*: a stalled broker must make Publish return an error
// rather than block and then report success. The consequence that actually
// matters is one level up and was never tested: the relay marks an outbox row
// `published_at` when Publish returns nil, and nothing ever re-reads a
// published row. A bus that lies about a stall therefore does not merely lose
// a Kafka record — it silently deletes a committed financial event from the
// only durable place it existed.
//
// So: enqueue real financial events, pause the broker container, run the
// relay, and assert the rows are still unpublished and marked failed with a
// retry time. Then unpause and assert every event publishes exactly once.
func TestChaos_BrokerStallNeverMarksOutboxPublished(t *testing.T) {
	requireEnv(t)
	seeds := brokerSeeds(t)
	requireHealthyStack(t, RedpandaContainer())

	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC))
	outbox := event.NewOutbox(clk)

	// Drain whatever earlier tests left in the outbox, while the broker is
	// still healthy. This is a precondition, not a convenience: the relay
	// claims the OLDEST unpublished rows, so leftovers would fill the batch
	// and this test would assert about someone else's events. The first
	// doubled run of this suite failed for exactly that reason.
	drainOutbox(t)

	// Real, registered financial topics keyed by their aggregate id, so the
	// partition-key rule the bus enforces is genuinely exercised.
	topic := event.TopicLedgerTransactionPosted
	agg := "chaos-" + chaosToken()
	envs := []event.Envelope{
		envelope(t, clk, topic, agg, 1),
		envelope(t, clk, topic, agg, 2),
		envelope(t, clk, topic, agg, 3),
	}
	require.NoError(t, testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return outbox.Enqueue(ctx, tx, topic.String(), envs...)
	}))
	require.Equal(t, 3, unpublishedFor(t, envs), "precondition: three committed, unpublished rows")
	require.Equal(t, 3, totalUnpublished(t),
		"precondition: the outbox holds only this test's rows, so the relay's batch is about this test")

	// A short produce timeout so the stalled publish fails inside the test
	// rather than on the 15s default.
	client, err := redpandabus.New(ctx, config.RedpandaConfig{Brokers: seeds},
		config.NewResolver(config.EnvTest, os.LookupEnv),
		redpandabus.Options{ClientID: "chaos-relay", ProduceTimeout: 5 * time.Second, AllowAutoTopicCreation: true})
	require.NoError(t, err)
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = client.Close(cctx)
	})

	bus := &lyingBus{inner: client, lie: chaosBreak(t, "relay_reports_stall_as_success")}
	obs := &countingObserver{}
	relay := event.NewRelay(testDB, bus, nil, event.RelayOptions{Clock: clk, Observer: obs, BatchSize: 10})

	// --- the fault -----------------------------------------------------------
	f := newFault(t, RedpandaContainer())
	f.Pause()

	// The relay's own transaction must survive the broker being dark: the
	// database is fine, only the bus is not.
	done := make(chan struct{})
	var published int
	var runErr error
	go func() {
		defer close(done)
		published, runErr = relay.RunOnce(ctx)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Minute):
		f.Unpause()
		t.Fatal("relay.RunOnce never returned while the broker was paused; " +
			"a stalled bus must not block the relay indefinitely")
	}
	require.NoError(t, runErr, "the relay must not error out; a failed publish is a per-row outcome")

	// THE INVARIANT. Everything else in this test is scaffolding.
	//
	// Counts are scoped to THIS test's event ids. RunOnce's return value
	// covers the whole outbox, and a chaos database carries rows other tests
	// left behind, so asserting on it would make this test's result depend on
	// what ran before it.
	t.Logf("chaos: relay reported %d rows published across the whole outbox", published)
	assert.Zero(t, publishedCountOf(t, envs),
		"a committed financial event of this test was marked published while the broker was unreachable")
	assert.Equal(t, 3, unpublishedFor(t, envs),
		"a committed financial event was marked published while the broker was paused: it is now unrecoverable")
	assert.Positive(t, obs.failed.Load(), "the failure must be observable, not swallowed")

	// The failed row must be scheduled for retry rather than merely left
	// alone, or the relay would either spin on it or abandon it.
	//
	// Only the FIRST row of the partition is attempted: all three share one
	// aggregate id, and once a row fails the relay holds back every later row
	// of that partition so the partition cannot reorder (D-036). A test that
	// demanded a failure record on all three would be demanding a bug.
	var attempted, deferred int
	for i, e := range envs {
		attempts, nextAt, lastErr := outboxRetryState(t, e.ID)
		switch {
		case attempts > 0:
			attempted++
			assert.Truef(t, nextAt.After(clk.Now()), "event %d (%s) has next_attempt_at %s, not in the future: "+
				"a failed row must back off, not spin", i, e.ID, nextAt)
			assert.NotEmptyf(t, lastErr, "event %d (%s) recorded no failure reason", i, e.ID)
		default:
			deferred++
			assert.Emptyf(t, lastErr, "event %d (%s) was never attempted yet recorded an error", i, e.ID)
		}
	}
	assert.Equal(t, 1, attempted, "exactly the head of the partition should have been attempted")
	assert.Equal(t, 2, deferred,
		"the rows behind a failed row must be deferred, not attempted, or the partition can reorder (D-036)")
	if bus.lie {
		assert.Positive(t, bus.lies.Load(),
			"negative control was armed but the publish never failed, so nothing was lied about; "+
				"the control did not exercise the guard")
	}

	// --- recovery ------------------------------------------------------------
	f.Unpause()
	require.NoError(t, waitHealthy(RedpandaContainer(), 3*time.Minute))

	// The retry backoff is in the future on the fake clock; move past it the
	// way a real relay's wall clock would.
	clk.Advance(10 * time.Minute)
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) && unpublishedFor(t, envs) > 0 {
		before := unpublishedFor(t, envs)
		if _, err := relay.RunOnce(ctx); err != nil {
			t.Logf("chaos: recovery pass returned %v", err)
		}
		if unpublishedFor(t, envs) == before {
			time.Sleep(500 * time.Millisecond)
		}
	}
	assert.Equal(t, 0, unpublishedFor(t, envs), "the events never published after the broker returned")
	assert.Equal(t, 3, publishedCountOf(t, envs),
		"each event must end up published exactly once after recovery")
}

// countingObserver records Observer callbacks.
type countingObserver struct{ published, failed, duplicates atomic.Int64 }

func (o *countingObserver) OnPublished(string, int, time.Duration) { o.published.Add(1) }
func (o *countingObserver) OnPublishFailed(string, int, error)     { o.failed.Add(1) }
func (o *countingObserver) OnDuplicate(string)                     { o.duplicates.Add(1) }

// envelope builds a valid envelope on a registered topic.
func envelope(t *testing.T, clk clock.Clock, topic event.Topic, aggregateID string, seq int) event.Envelope {
	t.Helper()
	sp, ok := event.Lookup(topic)
	require.Truef(t, ok, "topic %s is not registered", topic)
	payload, err := json.Marshal(map[string]any{"aggregate": aggregateID, "seq": seq})
	require.NoError(t, err)
	return event.Envelope{
		ID:            event.NewEventID().String(),
		Type:          string(topic),
		SchemaVersion: sp.SchemaVersion,
		Source:        "chaos-itest",
		AggregateType: sp.AggregateType,
		AggregateID:   aggregateID,
		CorrelationID: "corr-" + aggregateID,
		OccurredAt:    clk.Now(),
		Payload:       payload,
	}
}

// publishedCountOf counts how many of these specific events are marked
// published.
func publishedCountOf(t *testing.T, envs []event.Envelope) int {
	t.Helper()
	ids := make([]string, len(envs))
	for i, e := range envs {
		ids[i] = e.ID
	}
	var n int
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox_events WHERE id = ANY($1::uuid[]) AND published_at IS NOT NULL`, ids).Scan(&n))
	return n
}

// unpublishedFor counts how many of these specific events are still
// unpublished. Scoped to the test's own ids so the suite is re-runnable
// against one database.
func unpublishedFor(t *testing.T, envs []event.Envelope) int {
	t.Helper()
	ids := make([]string, len(envs))
	for i, e := range envs {
		ids[i] = e.ID
	}
	var n int
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox_events WHERE id = ANY($1::uuid[]) AND published_at IS NULL`, ids).Scan(&n))
	return n
}

// outboxRetryState reads the retry bookkeeping for one event.
func outboxRetryState(t *testing.T, eventID string) (attempts int, nextAttemptAt time.Time, lastError string) {
	t.Helper()
	var le *string
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT publish_attempts, next_attempt_at, last_error FROM outbox_events WHERE id = $1`, eventID).
		Scan(&attempts, &nextAttemptAt, &le))
	if le != nil {
		lastError = *le
	}
	return attempts, nextAttemptAt, lastError
}

// drainOutbox publishes every outstanding outbox row into a recorder, so a
// test that reasons about the relay's batch starts from an empty outbox.
// Rows other tests left behind are real committed events; they are published
// (to a recorder) rather than deleted, because deleting a committed financial
// event is exactly what this package exists to catch.
func drainOutbox(t *testing.T) {
	t.Helper()
	// A far-future clock. Every test in this package injects its own fake
	// clock, so rows an earlier test left behind carry a next_attempt_at
	// stamped from that clock and are not claimable at this one's "now".
	// Draining is only meaningful if it can claim all of them.
	sink := newRecordingBus()
	future := clock.NewFake(time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC))
	relay := event.NewRelay(testDB, sink, nil, event.RelayOptions{Clock: future, BatchSize: 500})
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		before := totalUnpublished(t)
		if before == 0 {
			return
		}
		if _, err := relay.RunOnce(context.Background()); err != nil {
			t.Logf("chaos: drain pass returned %v", err)
		}
		if totalUnpublished(t) == before {
			// Rows the relay will not publish (an unregistered topic, a bad
			// partition key) would otherwise loop forever. Say so and stop:
			// the test's own precondition assertion will report it.
			t.Logf("chaos: drain made no progress with %d rows outstanding", before)
			return
		}
	}
}

func totalUnpublished(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&n))
	return n
}
