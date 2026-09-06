//go:build integration

package event_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/event/eventtest"
)

// countingObserver records Observer calls.
type countingObserver struct {
	published, failed, duplicates atomic.Int64
}

func (o *countingObserver) OnPublished(string, int, time.Duration) { o.published.Add(1) }
func (o *countingObserver) OnPublishFailed(string, int, error)     { o.failed.Add(1) }
func (o *countingObserver) OnDuplicate(string)                     { o.duplicates.Add(1) }

func TestIntegration_RelayPublishesOnlyCommittedRows(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	topic := event.TopicOrderTransitioned

	committed := []event.Envelope{f.envelope(topic, "o-1"), f.envelope(topic, "o-2"), f.envelope(topic, "o-1")}
	f.enqueue(t, topic, committed...)

	rolledBack := errors.New("business rule rejected")
	err := testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if err := f.outbox.Enqueue(ctx, tx, topic.String(), f.envelope(topic, "o-3"), f.envelope(topic, "o-4")); err != nil {
			return err
		}
		return rolledBack
	})
	require.ErrorIs(t, err, rolledBack)
	assert.Equal(t, 3, countUnpublished(t))

	rec := eventtest.NewRecorder()
	obs := &countingObserver{}
	relay := event.NewRelay(testDB, rec, nil, event.RelayOptions{Clock: f.clk, Observer: obs})
	n, err := relay.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.Equal(t, int64(3), obs.published.Load())
	assert.Equal(t, 0, countUnpublished(t))

	msgs := rec.Messages()
	require.Len(t, msgs, 3)
	assert.Equal(t, ids(msgs), []string{committed[0].ID, committed[1].ID, committed[2].ID}, "recorded_at, id order")
	for i, m := range msgs {
		want := committed[i]
		assert.Equal(t, topic.String(), m.Topic)
		assert.Equal(t, want.AggregateID, m.Key)
		assert.Equal(t, want.ID, m.Headers[event.HeaderEventID])
		assert.Equal(t, "order.transitioned", m.Headers[event.HeaderEventType])
		assert.Equal(t, "1", m.Headers[event.HeaderSchemaVersion])
		assert.Equal(t, want.CorrelationID, m.Headers[event.HeaderCorrelationID])
		assert.Equal(t, "2026-09-05T12:00:00Z", m.Headers[event.HeaderOccurredAt])
		assert.Equal(t, "t-"+want.AggregateID, m.Headers["x-trace"], "producer headers are forwarded")

		got, err := m.Envelope()
		require.NoError(t, err)
		assert.Equal(t, want.ID, got.ID)
		gotC, err := got.CanonicalBytes()
		require.NoError(t, err)
		want.RecordedAt = itestStart
		wantC, err := want.CanonicalBytes()
		require.NoError(t, err)
		assert.Equal(t, string(wantC), string(gotC), "published value is the canonical envelope with recorded_at stamped")
	}

	n, err = relay.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "nothing left")
}

func TestIntegration_OutboxDedupKeyConflictRollsBack(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	topic := event.TopicFundingDepositTransitioned

	first := f.envelope(topic, "dep-1")
	first.DedupKey = "provider-evt-1"
	f.enqueue(t, topic, first)

	second := f.envelope(topic, "dep-1")
	second.DedupKey = "provider-evt-1"
	err := testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		// Something else in the same financial transaction.
		if _, err := tx.Exec(ctx, `SELECT 1`); err != nil {
			return err
		}
		return f.outbox.Enqueue(ctx, tx, topic.String(), f.envelope(topic, "dep-2"), second)
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))
	assert.True(t, db.IsUniqueViolation(err), "SQLSTATE is preserved through the chain")
	ee, _ := errs.As(err)
	assert.Equal(t, "provider-evt-1", ee.Fields["dedup_key"])

	var n int
	require.NoError(t, testDB.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE topic = $1`, topic.String()).Scan(&n))
	assert.Equal(t, 1, n, "the whole transaction rolled back, dep-2 included")

	// Same dedup key on another topic is fine; same event id is a conflict.
	other := f.envelope(event.TopicLedgerTransactionPosted, "ltx-1")
	other.DedupKey = "provider-evt-1"
	f.enqueue(t, event.TopicLedgerTransactionPosted, other)
	dupID := f.envelope(topic, "dep-9")
	dupID.ID = first.ID
	err = testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return f.outbox.Enqueue(ctx, tx, topic.String(), dupID)
	})
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))
}

func TestIntegration_RelayFailedPublishBacksOffAndRetries(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ev := f.envelope(event.TopicIntentTransitioned, "intent-1")
	f.enqueue(t, event.TopicIntentTransitioned, ev)

	rec := eventtest.NewRecorder()
	obs := &countingObserver{}
	relay := event.NewRelay(testDB, rec, nil, event.RelayOptions{Clock: f.clk, Observer: obs, RetryBackoffBase: time.Second})

	rec.SetPublishError(errors.New("broker unavailable"))
	n, err := relay.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	st := outboxRowState(t, ev.ID)
	assert.False(t, st.published)
	assert.Equal(t, 1, st.attempts)
	require.NotNil(t, st.lastError)
	assert.Equal(t, "broker unavailable", *st.lastError)
	assert.Equal(t, int64(1), obs.failed.Load())

	// attempts=1: eligible again at recorded_at + 1s.
	f.clk.Advance(500 * time.Millisecond)
	n, err = relay.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.Equal(t, 1, outboxRowState(t, ev.ID).attempts, "not attempted during backoff")

	f.clk.Advance(500 * time.Millisecond) // recorded_at + 1s
	n, err = relay.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.Equal(t, 2, outboxRowState(t, ev.ID).attempts, "attempted once eligible")

	// attempts=2: the second failure happened at recorded_at + 1s, so the row is
	// eligible 2s after *that* (recorded_at + 3s). Backoff is measured from the
	// failure and persisted in next_attempt_at (migration 00642), not derived
	// from recorded_at, so a permanently failing row can never hot-loop.
	f.clk.Advance(1500 * time.Millisecond) // recorded_at + 2.5s: still backing off
	_, err = relay.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, outboxRowState(t, ev.ID).attempts)

	rec.SetPublishError(nil)
	f.clk.Advance(500 * time.Millisecond) // recorded_at + 3s
	n, err = relay.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	st = outboxRowState(t, ev.ID)
	assert.True(t, st.published)
	assert.Nil(t, st.lastError, "last_error cleared on success")
	assert.Equal(t, 2, st.attempts)
	assert.Equal(t, []string{ev.ID}, ids(rec.Messages()))
	assert.Equal(t, int64(1), obs.published.Load())
}

// failingBus fails publishes of specific event ids a number of times.
type failingBus struct {
	event.Bus
	mu    sync.Mutex
	fails map[string]int
}

func (b *failingBus) Publish(ctx context.Context, topic, key string, value []byte, headers map[string]string) error {
	b.mu.Lock()
	id := headers[event.HeaderEventID]
	if n := b.fails[id]; n > 0 {
		b.fails[id] = n - 1
		b.mu.Unlock()
		return errors.New("injected failure for " + id)
	}
	b.mu.Unlock()
	return b.Bus.Publish(ctx, topic, key, value, headers)
}

func TestIntegration_RelayHoldsPartitionBehindFailedPredecessor(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	topic := event.TopicFillObserved
	a := f.envelope(topic, "order-1")
	f.clk.Advance(time.Millisecond)
	b := f.envelope(topic, "order-1")
	f.clk.Advance(time.Millisecond)
	c := f.envelope(topic, "order-2")
	f.enqueue(t, topic, a)
	f.enqueue(t, topic, b)
	f.enqueue(t, topic, c)

	rec := eventtest.NewRecorder()
	bus := &failingBus{Bus: rec, fails: map[string]int{a.ID: 1}}
	relay := event.NewRelay(testDB, bus, nil, event.RelayOptions{Clock: f.clk, RetryBackoffBase: time.Second})

	n, err := relay.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "only the other partition is published")
	assert.Equal(t, []string{c.ID}, ids(rec.Messages()))
	assert.Equal(t, 1, outboxRowState(t, a.ID).attempts)
	assert.Equal(t, 0, outboxRowState(t, b.ID).attempts, "successor is deferred, not attempted")

	// While a is backing off, b stays deferred even though it is eligible.
	n, err = relay.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.Equal(t, 0, outboxRowState(t, b.ID).attempts)

	f.clk.Advance(2 * time.Second)
	n, err = relay.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Equal(t, []string{c.ID, a.ID, b.ID}, ids(rec.Messages()), "partition order preserved")
	assert.Equal(t, 0, countUnpublished(t))
}

// A row another relay instance holds must never be overtaken by the rows
// behind it in the same partition.
//
// This is the regression test for D-036, a real defect that shipped and was
// caught only by cmd/relay-worker's own suite one level up. claim uses FOR
// UPDATE SKIP LOCKED, so a batch is "the oldest rows nobody else holds", not a
// contiguous run of the outbox. The order check used to consult only the
// OLDEST row of each partition in the batch. With another instance holding row
// 2 of 4, the batch head was genuinely unblocked, so rows 3 and 4 published
// straight past the held row and a consumer saw an aggregate's 4th event
// before its 2nd.
//
// TestIntegration_TwoRelaysNeverDoublePublish covered exactly-once across two
// relays but never ordering, which is why this survived. A plain SELECT ... FOR
// UPDATE stands in for the other instance's claim transaction.
func TestIntegration_RelayNeverPublishesPastARowHeldByAnotherInstance(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	topic := event.TopicFillObserved

	envs := make([]event.Envelope, 0, 4)
	for i := 0; i < 4; i++ {
		e := f.envelope(topic, "order-held")
		f.enqueue(t, topic, e)
		f.clk.Advance(time.Millisecond)
		envs = append(envs, e)
	}

	// Exactly what a second relay's claim transaction holds: row 2 of 4.
	holder, err := testDB.Pool().Acquire(ctx)
	require.NoError(t, err)
	defer holder.Release()
	tx, err := holder.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	var held string
	require.NoError(t, tx.QueryRow(ctx,
		`SELECT id::text FROM outbox_events WHERE id = $1 FOR UPDATE`, envs[1].ID).Scan(&held))

	rec := eventtest.NewRecorder()
	relay := event.NewRelay(testDB, rec, nil, event.RelayOptions{Clock: f.clk})

	n, err := relay.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "only the head may publish while row 2 is held elsewhere")
	assert.Equal(t, []string{envs[0].ID}, ids(rec.Messages()),
		"rows behind the held row must be deferred, never published past it")
	for _, i := range []int{2, 3} {
		assert.Equal(t, 0, outboxRowState(t, envs[i].ID).attempts,
			"envs[%d] sits behind a held row: it must not even be attempted", i)
	}

	// Once the other instance releases the row, the partition drains in order.
	require.NoError(t, tx.Rollback(ctx))
	n, err = relay.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.Equal(t, []string{envs[0].ID, envs[1].ID, envs[2].ID, envs[3].ID}, ids(rec.Messages()),
		"partition order preserved end to end")
	assert.Equal(t, 0, countUnpublished(t))
}

// crashingBus delivers through the wrapped bus and then cancels the relay's
// context so the mark step fails: the crash-between-publish-and-mark case.
type crashingBus struct {
	event.Bus
	cancel   context.CancelFunc
	crashed  atomic.Bool
	oneShot  bool
	attempts atomic.Int64
}

func (b *crashingBus) Publish(ctx context.Context, topic, key string, value []byte, headers map[string]string) error {
	b.attempts.Add(1)
	if err := b.Bus.Publish(ctx, topic, key, value, headers); err != nil {
		return err
	}
	if b.oneShot && b.crashed.CompareAndSwap(false, true) {
		b.cancel()
	}
	return nil
}

func TestIntegration_RelayCrashBetweenPublishAndMark_ConsumerInboxDedups(t *testing.T) {
	f := newFixture(t)
	topic := event.TopicCapitalReservationCreated
	ev := f.envelope(topic, "res-1")
	f.enqueue(t, topic, ev)

	mem, err := eventtest.NewMemoryBus("TEST")
	require.NoError(t, err)
	obs := &countingObserver{}
	inbox := f.inbox.WithObserver(obs)

	var deliveries, effects atomic.Int64
	var outcomes []event.Outcome
	var mu sync.Mutex
	require.NoError(t, mem.Subscribe(context.Background(), topic.String(), "reservation-consumer", func(ctx context.Context, m event.Message) error {
		deliveries.Add(1)
		return testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			out, err := inbox.Process(ctx, tx, "bus:"+m.Topic, m.ID, 1, func(ctx context.Context, tx pgx.Tx) error {
				effects.Add(1)
				return nil
			})
			if err != nil {
				return err
			}
			mu.Lock()
			outcomes = append(outcomes, out)
			mu.Unlock()
			return nil
		})
	}))

	relayCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bus := &crashingBus{Bus: mem, cancel: cancel, oneShot: true}
	relay := event.NewRelay(testDB, bus, nil, event.RelayOptions{Clock: f.clk})

	n, err := relay.RunOnce(relayCtx)
	require.Error(t, err, "the run died after publishing")
	assert.Equal(t, 0, n)
	assert.False(t, outboxRowState(t, ev.ID).published, "row is still unpublished: at-least-once")
	assert.Equal(t, int64(1), deliveries.Load())

	n, err = relay.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.True(t, outboxRowState(t, ev.ID).published)
	assert.Equal(t, int64(2), deliveries.Load(), "the consumer saw the event twice")
	assert.Equal(t, int64(1), effects.Load(), "but the economic effect happened once")
	assert.Equal(t, []event.Outcome{event.Processed, event.Duplicate}, outcomes)
	assert.Equal(t, int64(1), obs.duplicates.Load())
	rec, ok, err := inbox.Get(context.Background(), testDB, "bus:"+topic.String(), ev.ID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, event.StatusProcessed, rec.Status)
}

func TestIntegration_RelayRunExitsOnCancel(t *testing.T) {
	f := newFixture(t)
	f.enqueue(t, event.TopicGateTransitioned, f.envelope(event.TopicGateTransitioned, "gate-1"))
	rec := eventtest.NewRecorder()
	relay := event.NewRelay(testDB, rec, nil, event.RelayOptions{Clock: f.clk, PollInterval: 20 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- relay.Run(ctx) }()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit after ctx cancellation")
	}
	assert.Len(t, rec.Messages(), 1)
	assert.Equal(t, 0, countUnpublished(t))
}

func TestIntegration_RelayRunBacksOffWhileBusIsDown(t *testing.T) {
	f := newFixture(t)
	f.enqueue(t, event.TopicGateTransitioned, f.envelope(event.TopicGateTransitioned, "gate-1"))
	rec := eventtest.NewRecorder()
	rec.SetPublishError(errors.New("down"))
	relay := event.NewRelay(testDB, rec, nil, event.RelayOptions{
		Clock: f.clk, PollInterval: 10 * time.Millisecond, MaxInterval: 50 * time.Millisecond, RetryBackoffBase: time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	// Keep the row eligible at every tick despite per-row backoff.
	stop := make(chan struct{})
	go func() {
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				f.clk.Advance(time.Second)
			}
		}
	}()
	require.NoError(t, relay.Run(ctx))
	close(stop)
	var attempts int
	require.NoError(t, testDB.QueryRow(context.Background(), `SELECT publish_attempts FROM outbox_events`).Scan(&attempts))
	assert.Greater(t, attempts, 0)
	assert.Less(t, attempts, 25, "run-level backoff stops the hot loop (300ms / 10ms floor would be 30 without it)")
}

func TestIntegration_TwoRelaysNeverDoublePublish(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	topic := event.TopicAuditEventAppended
	const total = 120
	var events []event.Envelope
	for i := 0; i < total; i++ {
		events = append(events, f.envelope(topic, "audit-"+itoa(i)))
	}
	f.enqueue(t, topic, events...)

	rec := eventtest.NewRecorder()
	r1 := event.NewRelay(testDB, rec, nil, event.RelayOptions{Clock: f.clk, BatchSize: 7})
	r2 := event.NewRelay(testDB, rec, nil, event.RelayOptions{Clock: f.clk, BatchSize: 11})

	var wg sync.WaitGroup
	for _, r := range []*event.Relay{r1, r2} {
		wg.Add(1)
		go func(r *event.Relay) {
			defer wg.Done()
			idle := 0
			for idle < 3 {
				n, err := r.RunOnce(ctx)
				assert.NoError(t, err)
				if n == 0 {
					idle++
				} else {
					idle = 0
				}
			}
		}(r)
	}
	wg.Wait()

	msgs := rec.Messages()
	assert.Len(t, msgs, total)
	seen := map[string]bool{}
	for _, m := range msgs {
		assert.False(t, seen[m.ID], "double publish of %s", m.ID)
		seen[m.ID] = true
	}
	assert.Equal(t, 0, countUnpublished(t))
}

func TestIntegration_EndToEnd_DuplicateAndReorderedDeliveryIsHarmless(t *testing.T) {
	f := newFixture(t)
	topic := event.TopicLedgerTransactionPosted
	const total = 12
	var events []event.Envelope
	for i := 0; i < total; i++ {
		events = append(events, f.envelope(topic, "ltx-"+itoa(i%3)))
		f.clk.Advance(time.Millisecond)
	}
	f.enqueue(t, topic, events...)

	mem, err := eventtest.NewMemoryBus("LOCAL")
	require.NoError(t, err)
	mem.DuplicateEvery(1) // every message twice
	mem.ReorderWindow(4)

	var effects atomic.Int64
	var mu sync.Mutex
	perKey := map[string][]string{}
	require.NoError(t, mem.Subscribe(context.Background(), topic.String(), "ledger-projector", func(ctx context.Context, m event.Message) error {
		return testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.inbox.Process(ctx, tx, "bus:"+m.Topic, m.ID, 1, func(ctx context.Context, tx pgx.Tx) error {
				effects.Add(1)
				mu.Lock()
				perKey[m.Key] = append(perKey[m.Key], m.ID)
				mu.Unlock()
				return nil
			})
			return err
		})
	}))

	relay := event.NewRelay(testDB, mem, nil, event.RelayOptions{Clock: f.clk, BatchSize: 5})
	for {
		n, err := relay.RunOnce(context.Background())
		require.NoError(t, err)
		if n == 0 {
			break
		}
	}
	mem.Flush()
	assert.Equal(t, int64(total), effects.Load(), "every event applied exactly once")
	assert.Equal(t, 0, countUnpublished(t))
	assert.Empty(t, mem.Failures())
	mu.Lock()
	defer mu.Unlock()
	for key, got := range perKey {
		var want []string
		for _, e := range events {
			if e.AggregateID == key {
				want = append(want, e.ID)
			}
		}
		assert.Equal(t, want, got, "per-key order for %s", key)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
