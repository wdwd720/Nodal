//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/observability"
)

// The suite needs an isolated database (go run ./scripts/testdb -name relay).
// It truncates outbox_events between tests, so it refuses the shared
// controlplane_test database.
var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
	testAdmin      *pgx.Conn
)

func TestMain(m *testing.M) { os.Exit(testMain(m)) }

func testMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	for _, u := range []string{testAppURL, testMigrateURL} {
		parsed, err := url.Parse(u)
		if err != nil || strings.TrimPrefix(parsed.Path, "/") == "controlplane_test" {
			fmt.Fprintln(os.Stderr, "relay-worker integration: refusing the shared controlplane_test database; use scripts/testdb -name relay")
			return 1
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "relay-worker integration: migrate up:", err)
		return 1
	}
	var err error
	testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "relay-worker-itest", MaxConns: 20})
	if err != nil {
		fmt.Fprintln(os.Stderr, "relay-worker integration: open pool:", err)
		return 1
	}
	defer testDB.Close()
	testAdmin, err = pgx.Connect(ctx, testMigrateURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "relay-worker integration: connect as migrate role:", err)
		return 1
	}
	defer func() { _ = testAdmin.Close(ctx) }()
	return m.Run()
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set; skipping integration test")
	}
}

// ---- fixture ---------------------------------------------------------------

type fixture struct {
	t      *testing.T
	clk    *clock.Fake
	outbox *event.Outbox
}

// newFixture truncates the outbox and returns a producer whose clock
// advances one millisecond per envelope, so (recorded_at, id) is exactly the
// order the test wrote them in and every ordering assertion is unambiguous.
// The clock starts a minute in the past so relay lag is realistic.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireEnv(t)
	_, err := testAdmin.Exec(context.Background(), "TRUNCATE outbox_events")
	require.NoError(t, err)
	start := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	clk := clock.NewFake(start)
	return &fixture{t: t, clk: clk, outbox: event.NewOutbox(clk)}
}

func (f *fixture) envelope(topic event.Topic, aggregateID string) event.Envelope {
	f.clk.Advance(time.Millisecond)
	sp, ok := event.Lookup(topic)
	require.True(f.t, ok, "%s must be registered", topic)
	return event.Envelope{
		ID:            event.NewEventID().String(),
		Type:          string(topic),
		SchemaVersion: sp.SchemaVersion,
		Source:        "relay-worker-itest",
		AggregateType: sp.AggregateType,
		AggregateID:   aggregateID,
		CorrelationID: "corr-" + aggregateID,
		OccurredAt:    f.clk.Now(),
		Payload:       json.RawMessage(`{"aggregate":"` + aggregateID + `","amount":"1.00"}`),
	}
}

// enqueue commits one transaction per topic, exactly as a financial
// transaction would.
func (f *fixture) enqueue(topic event.Topic, events ...event.Envelope) {
	f.t.Helper()
	err := testDB.InTx(context.Background(), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return f.outbox.Enqueue(ctx, tx, topic.String(), events...)
	})
	require.NoError(f.t, err)
}

func countUnpublished(t *testing.T) int64 {
	t.Helper()
	n, err := unpublishedCount(context.Background())
	require.NoError(t, err)
	return n
}

func unpublishedCount(ctx context.Context) (int64, error) {
	var n int64
	err := testDB.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&n)
	return n, err
}

type rowState struct {
	published     bool
	attempts      int
	lastError     *string
	nextAttemptAt time.Time
	recordedAt    time.Time
}

func stateOf(t *testing.T, id string) rowState {
	t.Helper()
	var (
		s           rowState
		publishedAt *time.Time
	)
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT published_at, publish_attempts, last_error, next_attempt_at, recorded_at FROM outbox_events WHERE id = $1`, id).
		Scan(&publishedAt, &s.attempts, &s.lastError, &s.nextAttemptAt, &s.recordedAt))
	s.published = publishedAt != nil
	return s
}

// ---- bus double ------------------------------------------------------------

var errInjected = errors.New("relay-worker-itest: injected publish failure")

// controlBus is an event.Bus with exactly the two controls these tests need:
// fail a named event (or everything), and hold the first publish open so a
// shutdown can be timed against a pass that is mid-transaction.
type controlBus struct {
	mu         sync.Mutex
	msgs       []event.Message
	publishers []int // instance index per message, parallel to msgs
	failIDs    map[string]int
	failAll    error

	blockNext bool
	blocked   chan struct{}
	release   chan struct{}
}

func newControlBus() *controlBus {
	return &controlBus{failIDs: map[string]int{}, blocked: make(chan struct{}), release: make(chan struct{})}
}

var _ event.Bus = (*controlBus)(nil)

func (b *controlBus) Publish(ctx context.Context, topic, key string, value []byte, headers map[string]string) error {
	return b.publishAs(ctx, anyInstance, topic, key, value, headers)
}

// anyInstance tags a publish that no instanceBus attributed.
const anyInstance = -1

func (b *controlBus) publishAs(ctx context.Context, instance int, topic, key string, value []byte, headers map[string]string) error {
	id := headers[event.HeaderEventID]
	b.mu.Lock()
	if b.failAll != nil {
		err := b.failAll
		b.mu.Unlock()
		return err
	}
	if n := b.failIDs[id]; n > 0 {
		b.failIDs[id] = n - 1
		b.mu.Unlock()
		return fmt.Errorf("%w: %s", errInjected, id)
	}
	hold := b.blockNext
	b.blockNext = false
	b.mu.Unlock()

	if hold {
		close(b.blocked)
		select {
		case <-b.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs = append(b.msgs, event.Message{
		ID: id, Topic: topic, Key: key,
		Value: append([]byte(nil), value...), Headers: maps.Clone(headers),
	})
	b.publishers = append(b.publishers, instance)
	return nil
}

// instanceBus tags every publish with the relay instance that made it.
//
// It exists so a concurrency test can prove it is testing concurrency: an
// ordering assertion over partitions that one instance happened to drain by
// itself proves nothing, because the interleaving that breaks ordering never
// occurred. splitPartitions counts the partitions two instances actually
// shared.
type instanceBus struct {
	inner    *controlBus
	instance int
}

var _ event.Bus = instanceBus{}

func (b instanceBus) Publish(ctx context.Context, topic, key string, value []byte, headers map[string]string) error {
	return b.inner.publishAs(ctx, b.instance, topic, key, value, headers)
}

func (b instanceBus) Subscribe(context.Context, string, string, event.Handler) error { return nil }
func (b instanceBus) Close(context.Context) error                                    { return nil }

// splitPartitions counts the (topic, partition key) pairs whose events were
// published by more than one instance: the precondition for the reordering
// D-036 fixed.
func (b *controlBus) splitPartitions() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	seen := map[string]map[int]bool{}
	for i, m := range b.msgs {
		k := m.Topic + "\x00" + m.Key
		if seen[k] == nil {
			seen[k] = map[int]bool{}
		}
		seen[k][b.publishers[i]] = true
	}
	split := 0
	for _, instances := range seen {
		if len(instances) > 1 {
			split++
		}
	}
	return split
}

func (b *controlBus) Subscribe(context.Context, string, string, event.Handler) error { return nil }
func (b *controlBus) Close(context.Context) error                                    { return nil }

func (b *controlBus) failEvent(id string, times int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failIDs[id] = times
}

func (b *controlBus) setFailAll(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failAll = err
}

func (b *controlBus) holdNextPublish() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.blockNext = true
}

func (b *controlBus) messages() []event.Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]event.Message(nil), b.msgs...)
}

// order returns the published event ids per (topic, partition key), in the
// order the bus accepted them.
func (b *controlBus) order() map[string][]string {
	out := map[string][]string{}
	for _, m := range b.messages() {
		out[m.Topic+"\x00"+m.Key] = append(out[m.Topic+"\x00"+m.Key], m.ID)
	}
	return out
}

// ---- helpers ---------------------------------------------------------------

func newTestRelay(t *testing.T, bus event.Bus, batch int, m *relayMetrics) *event.Relay {
	t.Helper()
	return event.NewRelay(testDB, bus, nil, event.RelayOptions{
		BatchSize:        batch,
		RetryBackoffBase: 10 * time.Millisecond,
		RetryBackoffMax:  40 * time.Millisecond,
		RunTimeout:       20 * time.Second,
		Observer:         m,
	})
}

func newTestMetrics(t *testing.T) *relayMetrics {
	t.Helper()
	m, err := newRelayMetrics(nil)
	require.NoError(t, err)
	return m
}

// drainConcurrently runs every relay in its own goroutine until the outbox is
// empty or the deadline passes. That is the whole point of the exercise:
// several relay instances against one database, coordinating only through
// SKIP LOCKED and the blocked-partition rule.
func drainConcurrently(t *testing.T, relays []*event.Relay, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var wg sync.WaitGroup
	for i, r := range relays {
		wg.Add(1)
		go func(instance int, r *event.Relay) {
			defer wg.Done()
			for ctx.Err() == nil {
				n, err := r.RunOnce(ctx)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					t.Errorf("relay %d: RunOnce: %v", instance, err)
					return
				}
				if n > 0 {
					continue
				}
				left, err := unpublishedCount(ctx)
				if err != nil {
					if ctx.Err() == nil {
						t.Errorf("relay %d: count unpublished: %v", instance, err)
					}
					return
				}
				if left == 0 {
					return
				}
				// Everything left is waiting out a persisted retry deadline.
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Millisecond):
				}
			}
		}(i, r)
	}
	wg.Wait()
}

// assertOrderMatches checks that every partition published in exactly the
// order it was written, with no gaps and no duplicates.
func assertOrderMatches(t *testing.T, want, got map[string][]string) {
	t.Helper()
	assert.Len(t, got, len(want), "every partition published")
	for key, wantIDs := range want {
		assert.Equal(t, wantIDs, got[key], "partition %q published out of order or incompletely", strings.ReplaceAll(key, "\x00", "/"))
	}
}

// ---- tests -----------------------------------------------------------------

// The core safety property, and the reason this worker does not need a
// singleton lease: several relay instances run at once and between them
// publish every event exactly once and in per-partition order.
//
// Nothing but Postgres coordinates them. SELECT ... FOR UPDATE SKIP LOCKED
// hands each row to exactly one instance, and marking it published happens in
// the same transaction as the claim, so a row cannot be handed to a second
// instance while the first still has it. Order comes from internal/event's
// per-row blocked check (D-036): a claimed row whose predecessor is held
// anywhere else waits, and everything behind it waits too.
//
// The small batch size is deliberate. It forces partitions to be split across
// instances and across passes, which is exactly the interleaving that made
// the pre-D-036 relay reorder a partition.
func TestIntegration_ConcurrentRelaysPublishExactlyOnceAndInPartitionOrder(t *testing.T) {
	f := newFixture(t)

	topics := []event.Topic{event.TopicOrderTransitioned, event.TopicFillObserved, event.TopicIntentTransitioned}
	const (
		partitionsPerTopic = 5
		eventsPerPartition = 10
		instances          = 4
		batchSize          = 5
	)
	want := map[string][]string{}
	total := 0
	// Interleave the partitions so a single batch spans several of them.
	for round := 0; round < eventsPerPartition; round++ {
		for _, topic := range topics {
			batch := make([]event.Envelope, 0, partitionsPerTopic)
			for p := 0; p < partitionsPerTopic; p++ {
				aggregate := fmt.Sprintf("%s-agg-%d", topic, p)
				env := f.envelope(topic, aggregate)
				batch = append(batch, env)
				key := topic.String() + "\x00" + aggregate
				want[key] = append(want[key], env.ID)
				total++
			}
			f.enqueue(topic, batch...)
		}
	}
	require.Equal(t, int64(total), countUnpublished(t))

	bus := newControlBus()
	m := newTestMetrics(t)
	relays := make([]*event.Relay, instances)
	for i := range relays {
		relays[i] = newTestRelay(t, instanceBus{inner: bus, instance: i}, batchSize, m)
	}
	drainConcurrently(t, relays, 60*time.Second)

	assert.Zero(t, countUnpublished(t), "the outbox is drained")
	msgs := bus.messages()
	assert.Len(t, msgs, total, "every event published exactly once: no instance re-published another's row")
	assert.Positive(t, bus.splitPartitions(),
		"no partition was shared between instances, so the ordering assertion below tested nothing; raise the event count or lower the batch size")

	seen := map[string]int{}
	for _, msg := range msgs {
		seen[msg.ID]++
	}
	for id, n := range seen {
		assert.Equal(t, 1, n, "event %s was published %d times", id, n)
	}
	assertOrderMatches(t, want, bus.order())
	assert.Equal(t, int64(total), m.counters().published)
	assert.Zero(t, m.counters().failed)
}

// Ordering has to survive both hard cases at once: several relay instances
// claiming concurrently, and a publish that fails in the middle of a
// partition.
//
// The failed row stays unpublished with its persisted deadline pushed out,
// and every later row of that partition has to wait for it. While it is
// backed off it is invisible to the claim, so a concurrent instance sees a
// gap where it sits: only the per-row blocked check stops the rows behind it
// from overtaking it. This is the combination that found D-036.
func TestIntegration_OrderingSurvivesConcurrentRelaysAndAMidPartitionFailure(t *testing.T) {
	f := newFixture(t)

	const (
		partitions         = 6
		eventsPerPartition = 8
		instances          = 3
		batchSize          = 4
		failAtIndex        = 3 // the fourth event of each partition
		failTimes          = 2
	)
	topic := event.TopicOrderTransitioned
	want := map[string][]string{}
	var failed []string
	total := 0
	bus := newControlBus()

	for round := 0; round < eventsPerPartition; round++ {
		batch := make([]event.Envelope, 0, partitions)
		for p := 0; p < partitions; p++ {
			aggregate := fmt.Sprintf("order-%d", p)
			env := f.envelope(topic, aggregate)
			batch = append(batch, env)
			want[topic.String()+"\x00"+aggregate] = append(want[topic.String()+"\x00"+aggregate], env.ID)
			if round == failAtIndex {
				bus.failEvent(env.ID, failTimes)
				failed = append(failed, env.ID)
			}
			total++
		}
		f.enqueue(topic, batch...)
	}
	require.Len(t, failed, partitions)

	m := newTestMetrics(t)
	relays := make([]*event.Relay, instances)
	for i := range relays {
		relays[i] = newTestRelay(t, instanceBus{inner: bus, instance: i}, batchSize, m)
	}
	drainConcurrently(t, relays, 60*time.Second)

	assert.Zero(t, countUnpublished(t), "every event eventually published; a transport failure never drops one")
	assert.Len(t, bus.messages(), total, "a failed publish is retried, not duplicated on success")
	assert.Positive(t, bus.splitPartitions(),
		"no partition was shared between instances, so this exercised a single relay and not the case D-036 fixed")
	assertOrderMatches(t, want, bus.order())

	assert.GreaterOrEqual(t, m.counters().failed, int64(partitions*failTimes), "every injected failure was observed")
	assert.Positive(t, m.counters().retried, "retries are counted, not hidden")
	for _, id := range failed {
		s := stateOf(t, id)
		assert.True(t, s.published)
		assert.GreaterOrEqual(t, s.attempts, failTimes, "the failed attempts are recorded on the row")
		assert.Nil(t, s.lastError, "last_error is cleared when the row finally publishes")
	}
}

// A row held by another relay must not be overtaken by the rows behind it.
//
// This was a real defect, fixed in internal/event on 2026-09-06 (D-036). claim
// uses FOR UPDATE SKIP LOCKED, so a batch is the oldest rows nobody else holds
// rather than a contiguous run of the outbox. The order check previously
// consulted only the OLDEST batch row of each partition: with another instance
// holding row 2 of 4, the head was genuinely unblocked and rows 3 and 4
// published straight past the held row, so a consumer saw an aggregate's 4th
// event before its 2nd. The check now runs per row and, once a row is held
// back, defers every later row of its partition.
//
// The test now asserts the CORRECT behavior. Before the fix it recorded the
// defect (rows 1, 3, 4 published); if it ever reports 3 published rows again,
// per-partition ordering across concurrent relays has regressed.
func TestIntegration_ConcurrentClaimsNeverOvertakeALockedRowInAPartition(t *testing.T) {
	f := newFixture(t)
	topic := event.TopicOrderTransitioned

	envs := make([]event.Envelope, 0, 4)
	for i := 0; i < 4; i++ {
		env := f.envelope(topic, "order-1")
		f.enqueue(topic, env) // one transaction each, so recorded_at is distinct
		envs = append(envs, env)
	}

	ctx := context.Background()
	holder, err := testDB.Pool().Acquire(ctx)
	require.NoError(t, err)
	defer holder.Release()
	tx, err := holder.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	// Exactly what another relay instance's claim transaction holds.
	var lockedID string
	require.NoError(t, tx.QueryRow(ctx,
		`SELECT id::text FROM outbox_events WHERE id = $1 FOR UPDATE`, envs[1].ID).Scan(&lockedID))
	require.Equal(t, envs[1].ID, lockedID)

	bus := newControlBus()
	relay := newTestRelay(t, bus, 10, newTestMetrics(t))
	n, err := relay.RunOnce(ctx)
	require.NoError(t, err)

	var publishedIDs []string
	for _, m := range bus.messages() {
		publishedIDs = append(publishedIDs, m.ID)
	}
	assert.Equal(t, 1, n, "only the head of the partition may publish while row 2 is held elsewhere")
	assert.Equal(t, []string{envs[0].ID}, publishedIDs,
		"rows behind the held row must be deferred, not published past it")
	assert.False(t, stateOf(t, envs[1].ID).published, "the row another instance holds is untouched by this relay")
	for _, i := range []int{2, 3} {
		assert.False(t, stateOf(t, envs[i].ID).published,
			"envs[%d] sits behind a held row and must wait, or the partition reorders", i)
	}
}

// A transport failure is not a data failure. While the bus is unreachable
// the relay must publish nothing, mark nothing, drop nothing, and push each
// row's persisted deadline out so it cannot hot-loop. When the bus comes
// back, everything is still there and still in order.
func TestIntegration_TransportFailureNeverMarksPublishedAndRecovers(t *testing.T) {
	f := newFixture(t)
	topic := event.TopicCapitalReservationCreated

	var ids []string
	batch := make([]event.Envelope, 0, 4)
	for i := 0; i < 4; i++ {
		env := f.envelope(topic, "reservation-1")
		batch = append(batch, env)
		ids = append(ids, env.ID)
	}
	f.enqueue(topic, batch...)

	bus := newControlBus()
	bus.setFailAll(errors.New("relay-worker-itest: bus unreachable"))
	m := newTestMetrics(t)
	relay := newTestRelay(t, bus, 10, m)

	var deadlines []time.Time
	for pass := 0; pass < 3; pass++ {
		n, err := relay.RunOnce(context.Background())
		require.NoError(t, err, "a rejected publish is not a relay error: the pass commits the failure bookkeeping")
		assert.Zero(t, n, "nothing may be marked published while the bus is down")
		assert.Equal(t, int64(4), countUnpublished(t), "no event is ever dropped to make progress")
		s := stateOf(t, ids[0])
		assert.False(t, s.published)
		require.NotNil(t, s.lastError)
		assert.Contains(t, *s.lastError, "bus unreachable", "the failure is recorded on the row for an operator to read")
		assert.Equal(t, pass+1, s.attempts)
		deadlines = append(deadlines, s.nextAttemptAt)
		// Wait out the persisted deadline so the next pass can claim again.
		time.Sleep(60 * time.Millisecond)
	}
	for i := 1; i < len(deadlines); i++ {
		assert.True(t, deadlines[i].After(deadlines[i-1]),
			"each failure pushes next_attempt_at further out: %s then %s", deadlines[i-1], deadlines[i])
	}
	for _, id := range ids[1:] {
		s := stateOf(t, id)
		assert.Zero(t, s.attempts, "the rest of the partition is held back, not attempted out of order")
	}
	assert.Zero(t, m.counters().published)
	assert.Equal(t, int64(3), m.counters().failed)

	// The bus comes back.
	bus.setFailAll(nil)
	n, err := relay.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 4, n)
	assert.Zero(t, countUnpublished(t))
	assert.Equal(t, ids, bus.order()[topic.String()+"\x00reservation-1"], "order survived the outage")
}

// internal/event's registry is authoritative and test/contract/eventtopics
// keeps producers inside it, so a row naming an unregistered topic is a real
// defect. It must be loud — a WARN and a counter, and visible in `status` —
// and it must never be a reason to drop a committed financial event.
func TestIntegration_UnregisteredTopicIsLoudAndNeverDropped(t *testing.T) {
	f := newFixture(t)
	f.enqueue(event.TopicOrderTransitioned, f.envelope(event.TopicOrderTransitioned, "order-1"))

	// Outbox.Enqueue refuses an unregistered topic, which is the point, so
	// the row is written the only way it could ever appear: directly.
	ghostID := event.NewEventID().String()
	recorded := f.clk.Now().Add(time.Second)
	_, err := testAdmin.Exec(context.Background(), `
		INSERT INTO outbox_events (id, topic, partition_key, event_type, schema_version, source,
			aggregate_type, aggregate_id, occurred_at, recorded_at, headers, payload, next_attempt_at)
		VALUES ($1, 'ghost.topic', 'ghost-1', 'ghost.topic', 1, 'itest', 'ghost', 'ghost-1', $2, $2, '{}'::jsonb, '{"a":1}'::jsonb, $2)`,
		ghostID, recorded)
	require.NoError(t, err)

	st, err := TakeStatus(context.Background(), testDB, time.Now().UTC(), 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"ghost.topic"}, st.UnregisteredTopic, "status names the unregistered topic")

	logs := &syncBuffer{}
	m := newTestMetrics(t)
	guard := &topicGuardBus{inner: newControlBus(), log: testLogger(logs), metrics: m}
	relay := newTestRelay(t, guard, 10, m)

	n, err := relay.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, n, "the unregistered row is published, never dropped")
	assert.True(t, stateOf(t, ghostID).published)
	assert.Equal(t, int64(1), m.counters().unregistered)
	assert.Contains(t, logs.String(), "not in the event registry")
	assert.Contains(t, logs.String(), "ghost.topic")
}

// `status` is what an operator runs during an incident, with the broker
// down and no appetite for SQL. It has to name the depth, the lag, the
// per-topic backlog and every partition whose head is stuck.
func TestIntegration_StatusReportsDepthLagAndBlockedPartitions(t *testing.T) {
	f := newFixture(t)
	topic := event.TopicOrderTransitioned

	var stuck []event.Envelope
	for p := 0; p < 3; p++ {
		batch := make([]event.Envelope, 0, 3)
		for i := 0; i < 3; i++ {
			batch = append(batch, f.envelope(topic, fmt.Sprintf("order-%d", p)))
		}
		stuck = append(stuck, batch[0])
		f.enqueue(topic, batch...)
	}
	f.enqueue(event.TopicFillObserved, f.envelope(event.TopicFillObserved, "order-0"))

	bus := newControlBus()
	// Wedge the head of one partition only.
	bus.failEvent(stuck[1].ID, 5)
	m := newTestMetrics(t)
	// A long retry backoff keeps the wedged partition blocked while the
	// assertions run.
	relay := event.NewRelay(testDB, bus, nil, event.RelayOptions{
		BatchSize: 100, RetryBackoffBase: time.Hour, RetryBackoffMax: time.Hour, Observer: m,
	})
	_, err := relay.RunOnce(context.Background())
	require.NoError(t, err)

	now := time.Now().UTC()
	st, err := TakeStatus(context.Background(), testDB, now, 10)
	require.NoError(t, err)

	assert.Equal(t, int64(3), st.Snapshot.Unpublished, "the wedged partition's three rows are still there")
	assert.Equal(t, int64(2), st.Snapshot.Eligible,
		"the two rows behind the wedged head are still claimable; they are held back at publish time, not at claim time, which is why blocked partitions are reported separately")
	assert.Equal(t, int64(1), st.Snapshot.Failing)
	assert.Equal(t, int64(1), st.Snapshot.MaxAttempts)
	assert.Equal(t, int64(1), st.Snapshot.Partitions)
	assert.Equal(t, int64(1), st.Snapshot.BlockedPartitions)
	require.NotNil(t, st.Snapshot.OldestRecordedAt)
	assert.Positive(t, st.Snapshot.OldestAge(), "relay lag is measured from the oldest unpublished row")
	assert.Equal(t, int64(st.Snapshot.OldestAge()/time.Second), st.OldestAgeSeconds)
	require.NotNil(t, st.Snapshot.NextAttemptAt)

	require.Len(t, st.Topics, 1)
	assert.Equal(t, topic.String(), st.Topics[0].Topic)
	assert.True(t, st.Topics[0].Registered)
	assert.Equal(t, int64(3), st.Topics[0].Unpublished)
	assert.Empty(t, st.UnregisteredTopic)

	require.Len(t, st.Blocked, 1)
	blocked := st.Blocked[0]
	assert.Equal(t, topic.String(), blocked.Topic)
	assert.Equal(t, "order-1", blocked.PartitionKey)
	assert.Equal(t, int64(3), blocked.Depth)
	assert.Equal(t, int64(1), blocked.Attempts)
	assert.Positive(t, blocked.RetryInSeconds)
	assert.Contains(t, blocked.LastError, "injected publish failure")
	assert.False(t, blocked.UnregisteredHead)

	// Both renderings must survive real data, and -max-lag must fire.
	var text, jsonOut strings.Builder
	require.NoError(t, st.WriteText(&text))
	assert.Contains(t, text.String(), "blocked partitions")
	assert.Contains(t, text.String(), "order-1")
	require.NoError(t, st.WriteJSON(&jsonOut))
	var decoded Status
	require.NoError(t, json.Unmarshal([]byte(jsonOut.String()), &decoded))
	assert.Equal(t, st.Snapshot.Unpublished, decoded.Snapshot.Unpublished)
}

// Shutdown is a drain, not a kill: the signal stops the next claim, and the
// pass already in flight keeps a context the signal does not touch, so it
// commits instead of rolling back everything the broker already accepted.
func TestIntegration_ShutdownLetsTheInFlightPassCommit(t *testing.T) {
	f := newFixture(t)
	topic := event.TopicOrderTransitioned
	batch := make([]event.Envelope, 0, 5)
	for i := 0; i < 5; i++ {
		batch = append(batch, f.envelope(topic, fmt.Sprintf("order-%d", i)))
	}
	f.enqueue(topic, batch...)

	bus := newControlBus()
	bus.holdNextPublish()
	m := newTestMetrics(t)
	logs := &syncBuffer{}
	r := &Runner{relay: newTestRelay(t, bus, 100, m), metrics: m, db: testDB, clk: clock.System(), log: testLogger(logs), drain: 30 * time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	select {
	case <-bus.blocked:
	case <-time.After(20 * time.Second):
		cancel()
		t.Fatal("the relay never reached the bus")
	}
	cancel() // SIGTERM lands while the pass holds an open transaction.
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int64(5), countUnpublished(t), "nothing is marked published until the pass commits")
	close(bus.release)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return after the drain")
	}
	assert.Zero(t, countUnpublished(t), "the in-flight pass finished and committed")
	assert.Len(t, bus.messages(), 5)
	assert.Contains(t, logs.String(), "no further batches will be claimed")
}

// If the in-flight pass outruns the drain budget it is abandoned, and
// abandoning has to be safe: the pass is one transaction, so the rollback
// marks nothing published, releases every row lock and leaves the backlog
// exactly as it was. No row is left claimed-but-unpublished and no partition
// stalls because a process stopped.
func TestIntegration_ShutdownAbandonsAPassThatOutrunsTheDrainBudget(t *testing.T) {
	f := newFixture(t)
	topic := event.TopicOrderTransitioned
	batch := make([]event.Envelope, 0, 3)
	for i := 0; i < 3; i++ {
		batch = append(batch, f.envelope(topic, "order-1"))
	}
	f.enqueue(topic, batch...)

	bus := newControlBus()
	bus.holdNextPublish()
	m := newTestMetrics(t)
	logs := &syncBuffer{}
	r := &Runner{relay: newTestRelay(t, bus, 100, m), metrics: m, db: testDB, clk: clock.System(), log: testLogger(logs), drain: 200 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	select {
	case <-bus.blocked:
	case <-time.After(20 * time.Second):
		t.Fatal("the relay never reached the bus")
	}
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "an abandoned pass is a clean stop, not a worker failure")
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return inside the drain budget")
	}
	close(bus.release)

	assert.Equal(t, int64(3), countUnpublished(t), "the rollback left every row exactly as claimable as before")
	for _, env := range batch {
		s := stateOf(t, env.ID)
		assert.False(t, s.published)
		assert.Zero(t, s.attempts, "an abandoned pass records no attempt: the transaction rolled back")
		assert.False(t, s.nextAttemptAt.After(time.Now().UTC()), "the row is immediately claimable again, so its partition does not stall")
	}
	assert.Contains(t, logs.String(), "outran the drain budget")

	// The proof that nothing is stuck: a fresh relay drains it. Eventually,
	// because the abandoned transaction's row locks are released when the
	// server notices the closed connection, which is not instantaneous.
	fresh := newTestRelay(t, newControlBus(), 100, m)
	require.Eventually(t, func() bool {
		if _, err := fresh.RunOnce(context.Background()); err != nil {
			return false
		}
		n, err := unpublishedCount(context.Background())
		return err == nil && n == 0
	}, 15*time.Second, 100*time.Millisecond, "the abandoned pass left rows unclaimable")
}

// `once` is the incident-response entry point: run a bounded number of
// passes, then say what is left.
func TestIntegration_OnceDrainsAndReportsWhatIsLeft(t *testing.T) {
	f := newFixture(t)
	topic := event.TopicGateTransitioned
	batch := make([]event.Envelope, 0, 6)
	for i := 0; i < 6; i++ {
		batch = append(batch, f.envelope(topic, fmt.Sprintf("gate-%d", i)))
	}
	f.enqueue(topic, batch...)

	bus := newControlBus()
	m := newTestMetrics(t)
	d := &deps{
		log: testLogger(&syncBuffer{}), clk: clock.System(), metrics: m, db: testDB,
		drain: 10 * time.Second, exclusive: true,
		relayOpts: event.RelayOptions{BatchSize: 2, RunTimeout: 20 * time.Second, Observer: m},
	}
	var out strings.Builder
	// -passes 2 with a batch size of 2 moves four of the six rows.
	require.NoError(t, relayOnce(context.Background(), d, bus, 2, &out))
	assert.Contains(t, out.String(), "passes=2 published=4")
	assert.Contains(t, out.String(), "remaining=2")
	assert.Equal(t, int64(2), countUnpublished(t))

	out.Reset()
	// -passes 0 keeps going until a pass publishes nothing.
	require.NoError(t, relayOnce(context.Background(), d, bus, 0, &out))
	assert.Contains(t, out.String(), "published=2")
	assert.Contains(t, out.String(), "remaining=0")
	assert.Zero(t, countUnpublished(t))
}

// The exclusive lease is the opt-in singleton switch (CP_RELAY_WORKER_EXCLUSIVE,
// default off): one instance relays and the rest stand by. The lock is
// session-scoped, so a holder that goes away loses it without any lease
// table, heartbeat or clock — and equally, a released lease is immediately
// available to a standby.
func TestIntegration_ExclusiveLeaseIsHeldByOneSessionAtATime(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	const key = relayAdvisoryLockKey + 1 // a key of this test's own

	first, err := TryAcquireLease(ctx, testDB, key)
	require.NoError(t, err)
	require.NotNil(t, first, "the first instance takes the lease")
	require.NoError(t, first.Alive(ctx))

	second, err := TryAcquireLease(ctx, testDB, key)
	require.NoError(t, err)
	assert.Nil(t, second, "a second instance is refused without waiting")

	first.Release(ctx)
	assert.Error(t, first.Alive(ctx), "a released lease is not held")
	first.Release(ctx) // idempotent

	third, err := TryAcquireLease(ctx, testDB, key)
	require.NoError(t, err)
	require.NotNil(t, third, "the standby takes over once the holder lets go")
	third.Release(ctx)

	var nilLease *Lease
	assert.Error(t, nilLease.Alive(ctx))
	nilLease.Release(ctx)
}

// With the lease on, several relay-worker instances still drain the outbox
// correctly: the loop's standby path is a real code path, not a stub, so it
// has to yield a full, ordered drain rather than a stall.
//
// Correctness no longer depends on it — the test above proves order holds
// with every instance relaying — so what this defends is the switch itself:
// that turning it on serializes the instances without losing, duplicating or
// reordering anything, including through a mid-partition publish failure.
func TestIntegration_ExclusiveLeaseStillDrainsInOrder(t *testing.T) {
	f := newFixture(t)
	topic := event.TopicOrderTransitioned

	const (
		partitions         = 5
		eventsPerPartition = 8
		instances          = 3
		batchSize          = 3
		failAtRound        = 3
	)
	bus := newControlBus()
	want := map[string][]string{}
	total := 0
	for round := 0; round < eventsPerPartition; round++ {
		batch := make([]event.Envelope, 0, partitions)
		for p := 0; p < partitions; p++ {
			aggregate := fmt.Sprintf("order-%d", p)
			env := f.envelope(topic, aggregate)
			batch = append(batch, env)
			want[topic.String()+"\x00"+aggregate] = append(want[topic.String()+"\x00"+aggregate], env.ID)
			if round == failAtRound {
				bus.failEvent(env.ID, 2)
			}
			total++
		}
		f.enqueue(topic, batch...)
	}

	m := newTestMetrics(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < instances; i++ {
		r := &Runner{
			relay: event.NewRelay(testDB, bus, nil, event.RelayOptions{
				BatchSize: batchSize, PollInterval: 10 * time.Millisecond,
				RetryBackoffBase: 10 * time.Millisecond, RetryBackoffMax: 40 * time.Millisecond,
				RunTimeout: 20 * time.Second, Observer: m,
			}),
			metrics: m, db: testDB, clk: clock.System(), log: testLogger(&syncBuffer{}),
			drain: 10 * time.Second, exclusive: true,
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			assert.NoError(t, r.Run(ctx))
		}()
	}
	require.Eventually(t, func() bool {
		n, err := unpublishedCount(context.Background())
		return err == nil && n == 0
	}, 45*time.Second, 50*time.Millisecond, "the instances never drained the outbox")
	cancel()
	wg.Wait()

	assert.Len(t, bus.messages(), total, "every event published exactly once")
	assertOrderMatches(t, want, bus.order())
	assert.GreaterOrEqual(t, m.counters().failed, int64(partitions*2), "the injected failures happened")
}

// Relay lag has to be a first-class signal, not a log line: an operator
// alerts on outbox_depth and outbox_oldest_unpublished_age, and on the
// publish outcome counters, so those instruments must actually be exported.
func TestIntegration_MetricsExportDepthLagAndOutcomes(t *testing.T) {
	f := newFixture(t)
	topic := event.TopicOrderTransitioned

	published := f.envelope(topic, "order-ok")
	wedged := f.envelope(topic, "order-stuck")
	trailing := f.envelope(topic, "order-stuck")
	f.enqueue(topic, published, wedged, trailing)

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	m, err := newRelayMetrics(provider.Meter("relay-worker-itest"))
	require.NoError(t, err)

	bus := newControlBus()
	bus.failEvent(wedged.ID, 5)
	relay := event.NewRelay(testDB, bus, nil, event.RelayOptions{
		BatchSize: 100, RetryBackoffBase: time.Hour, RetryBackoffMax: time.Hour, Observer: m,
	})
	_, err = relay.RunOnce(context.Background())
	require.NoError(t, err)

	r := &Runner{relay: relay, metrics: m, db: testDB, clk: clock.System(), log: testLogger(&syncBuffer{})}
	snap := r.Sample(context.Background())
	require.Equal(t, int64(2), snap.Unpublished)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	require.Len(t, rm.ScopeMetrics, 1)
	got := rm.ScopeMetrics[0].Metrics

	assert.Equal(t, int64(1), sumOf(t, got, metricPublished))
	assert.Equal(t, int64(1), sumOf(t, got, metricFailures))
	assert.Equal(t, int64(2), gaugeOf(t, got, metricDepth))
	assert.Equal(t, int64(1), gaugeOf(t, got, metricEligible),
		"the wedged head is backed off; the row behind it is still claimable and shows up as blocked, not as ineligible")
	assert.Equal(t, int64(1), gaugeOf(t, got, metricBlocked))
	assert.Equal(t, int64(1), gaugeOf(t, got, metricPartitions))
	assert.Positive(t, gaugeOf(t, got, metricOldestAge), "relay lag in seconds is exported")
	assert.Contains(t, names(got), metricLag, "publish lag is a histogram, not only a gauge")

	// The topic label is present and bounded.
	assert.Equal(t, topic.String(), attrOf(t, got, metricPublished, "topic"))
	assert.Equal(t, topic.String(), attrOf(t, got, metricFailures, "topic"))
}

func names(ms []metricdata.Metrics) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

func findMetric(t *testing.T, ms []metricdata.Metrics, name string) metricdata.Metrics {
	t.Helper()
	for _, m := range ms {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("instrument %q was not exported; got %v", name, names(ms))
	return metricdata.Metrics{}
}

func sumOf(t *testing.T, ms []metricdata.Metrics, name string) int64 {
	t.Helper()
	sum, ok := findMetric(t, ms, name).Data.(metricdata.Sum[int64])
	require.True(t, ok, "%s is not an int64 sum", name)
	var total int64
	for _, dp := range sum.DataPoints {
		total += dp.Value
	}
	return total
}

func gaugeOf(t *testing.T, ms []metricdata.Metrics, name string) int64 {
	t.Helper()
	g, ok := findMetric(t, ms, name).Data.(metricdata.Gauge[int64])
	require.True(t, ok, "%s is not an int64 gauge", name)
	require.Len(t, g.DataPoints, 1)
	return g.DataPoints[0].Value
}

func attrOf(t *testing.T, ms []metricdata.Metrics, name, key string) string {
	t.Helper()
	sum, ok := findMetric(t, ms, name).Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.NotEmpty(t, sum.DataPoints)
	v, ok := sum.DataPoints[0].Attributes.Value(attribute.Key(key))
	require.True(t, ok, "%s has no %s attribute", name, key)
	return v.AsString()
}

// syncBuffer is a writer several goroutines may log into while the test
// reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func testLogger(w *syncBuffer) *slog.Logger {
	return observability.NewLogger(config.EnvTest, w)
}
