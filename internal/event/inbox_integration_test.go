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
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
)

// process runs Process in its own transaction.
func (f fixture) process(t *testing.T, source, id string, fn func(ctx context.Context, tx pgx.Tx) error) (event.Outcome, error) {
	t.Helper()
	var out event.Outcome
	err := testDB.InTx(context.Background(), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.inbox.Process(ctx, tx, source, id, 1, fn)
		return err
	})
	return out, err
}

func counting(n *atomic.Int64) func(ctx context.Context, tx pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		n.Add(1)
		return nil
	}
}

func TestIntegration_InboxProcessOnce(t *testing.T) {
	f := newFixture(t)
	var n atomic.Int64
	out, err := f.process(t, "stripe", "evt_1", counting(&n))
	require.NoError(t, err)
	assert.Equal(t, event.Processed, out)
	f.clk.Advance(time.Minute)
	out, err = f.process(t, "stripe", "evt_1", counting(&n))
	require.NoError(t, err)
	assert.Equal(t, event.Duplicate, out)
	assert.Equal(t, int64(1), n.Load())

	rec, ok, err := f.inbox.Get(context.Background(), testDB, "stripe", "evt_1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, event.StatusProcessed, rec.Status)
	assert.Equal(t, 1, rec.SchemaVersion)
	assert.Equal(t, itestStart, rec.ReceivedAt)
	require.NotNil(t, rec.ProcessedAt)
	assert.Equal(t, itestStart, *rec.ProcessedAt)
	assert.Nil(t, rec.Error)

	_, ok, err = f.inbox.Get(context.Background(), testDB, "stripe", "missing")
	require.NoError(t, err)
	assert.False(t, ok)

	// A different source with the same message id is a different message.
	out, err = f.process(t, "helius", "evt_1", counting(&n))
	require.NoError(t, err)
	assert.Equal(t, event.Processed, out)
	assert.Equal(t, int64(2), n.Load())
}

func TestProp_InboxDuplicateDeliveryRunsFnOnce(t *testing.T) {
	f := newFixture(t)
	seq := 0
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 50).Draw(rt, "deliveries")
		seq++
		id := "msg-" + itoa(seq)
		var runs atomic.Int64
		for i := 0; i < n; i++ {
			out, err := f.process(t, "prop", id, counting(&runs))
			if err != nil {
				rt.Fatalf("delivery %d: %v", i, err)
			}
			if i == 0 && out != event.Processed {
				rt.Fatalf("first delivery outcome %v", out)
			}
			if i > 0 && out != event.Duplicate {
				rt.Fatalf("delivery %d outcome %v", i, out)
			}
		}
		if runs.Load() != 1 {
			rt.Fatalf("fn ran %d times for %d deliveries", runs.Load(), n)
		}
	})
}

func TestIntegration_InboxFailedIsReprocessable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	boom := errs.New(errs.CodeProviderUnavailable, "downstream unavailable")
	var runs atomic.Int64

	_, err := f.process(t, "stripe", "evt_2", func(ctx context.Context, tx pgx.Tx) error {
		runs.Add(1)
		return boom
	})
	require.ErrorIs(t, err, boom, "fn error is returned unchanged")
	_, ok, err := f.inbox.Get(ctx, testDB, "stripe", "evt_2")
	require.NoError(t, err)
	assert.False(t, ok, "the RECEIVED row rolled back with the transaction")

	require.NoError(t, f.inbox.MarkFailed(ctx, testDB, "stripe", "evt_2", 1, "", boom))
	rec, ok, err := f.inbox.Get(ctx, testDB, "stripe", "evt_2")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, event.StatusFailed, rec.Status)
	require.NotNil(t, rec.Error)
	assert.Equal(t, "PROVIDER_UNAVAILABLE: downstream unavailable", *rec.Error)

	// Fails again: stays FAILED, error updated.
	_, err = f.process(t, "stripe", "evt_2", func(ctx context.Context, tx pgx.Tx) error {
		runs.Add(1)
		return errors.New("still broken")
	})
	require.Error(t, err)
	require.NoError(t, f.inbox.MarkFailed(ctx, testDB, "stripe", "evt_2", 1, "", errors.New("still broken")))
	rec, _, err = f.inbox.Get(ctx, testDB, "stripe", "evt_2")
	require.NoError(t, err)
	assert.Equal(t, "still broken", *rec.Error)

	f.clk.Advance(time.Minute)
	out, err := f.process(t, "stripe", "evt_2", counting(&runs))
	require.NoError(t, err)
	assert.Equal(t, event.Processed, out)
	assert.Equal(t, int64(3), runs.Load())
	rec, _, err = f.inbox.Get(ctx, testDB, "stripe", "evt_2")
	require.NoError(t, err)
	assert.Equal(t, event.StatusProcessed, rec.Status)
	assert.Nil(t, rec.Error)
	assert.Equal(t, itestStart.Add(time.Minute), *rec.ProcessedAt)

	// MarkFailed never downgrades a processed message.
	require.NoError(t, f.inbox.MarkFailed(ctx, testDB, "stripe", "evt_2", 1, "", errors.New("late failure")))
	rec, _, err = f.inbox.Get(ctx, testDB, "stripe", "evt_2")
	require.NoError(t, err)
	assert.Equal(t, event.StatusProcessed, rec.Status)
	assert.Nil(t, rec.Error)

	out, err = f.process(t, "stripe", "evt_2", counting(&runs))
	require.NoError(t, err)
	assert.Equal(t, event.Duplicate, out)
	assert.Equal(t, int64(3), runs.Load())
}

func TestIntegration_InboxConcurrentProcessRunsFnExactlyOnce(t *testing.T) {
	f := newFixture(t)
	const workers = 20
	var runs atomic.Int64
	outcomes := make([]event.Outcome, workers)
	errsSeen := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			outcomes[i], errsSeen[i] = f.process(t, "webhook", "evt_race", func(ctx context.Context, tx pgx.Tx) error {
				runs.Add(1)
				time.Sleep(150 * time.Millisecond) // widen the window while others wait on the insert
				return nil
			})
		}(i)
	}
	close(start)
	wg.Wait()

	processed, duplicates := 0, 0
	for i := range outcomes {
		require.NoError(t, errsSeen[i], "worker %d", i)
		switch outcomes[i] {
		case event.Processed:
			processed++
		case event.Duplicate:
			duplicates++
		}
	}
	assert.Equal(t, 1, processed)
	assert.Equal(t, workers-1, duplicates)
	assert.Equal(t, int64(1), runs.Load())
}

func TestIntegration_InboxInFlightElsewhereIsInProgress(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A committed RECEIVED row (claimed by a foreign writer, never finished).
	_, err := testDB.Exec(ctx, `INSERT INTO inbox_messages (source, message_id, schema_version, status) VALUES ('ext', 'claimed', 1, 'RECEIVED')`)
	require.NoError(t, err)
	var runs atomic.Int64
	_, err = f.process(t, "ext", "claimed", counting(&runs))
	require.Error(t, err)
	assert.Equal(t, errs.CodeIdempotencyInProgress, errs.CodeOf(err))
	ee, _ := errs.As(err)
	require.NotNil(t, ee.RetryAfter)
	assert.Equal(t, int64(0), runs.Load())

	// A FAILED row locked by another transaction that is re-processing it.
	require.NoError(t, f.inbox.MarkFailed(ctx, testDB, "ext", "retrying", 1, "", errors.New("first try")))
	holder, err := testDB.Pool().Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = holder.Rollback(ctx) }()
	_, err = holder.Exec(ctx, `SELECT 1 FROM inbox_messages WHERE source = 'ext' AND message_id = 'retrying' FOR UPDATE`)
	require.NoError(t, err)

	_, err = f.process(t, "ext", "retrying", counting(&runs))
	require.Error(t, err)
	assert.Equal(t, errs.CodeIdempotencyInProgress, errs.CodeOf(err))
	assert.Equal(t, int64(0), runs.Load())

	require.NoError(t, holder.Rollback(ctx))
	out, err := f.process(t, "ext", "retrying", counting(&runs))
	require.NoError(t, err)
	assert.Equal(t, event.Processed, out)
	assert.Equal(t, int64(1), runs.Load())
}

func TestIntegration_InboxPayloadHashMismatchIsConflict(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	hashA, hashB := event.HashPayload([]byte(`{"amount":"1.00"}`)), event.HashPayload([]byte(`{"amount":"9.00"}`))
	var runs atomic.Int64
	run := func(hash string) (event.Outcome, error) {
		var out event.Outcome
		err := testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = f.inbox.ProcessHashed(ctx, tx, "stripe", "evt_h", 1, hash, counting(&runs))
			return err
		})
		return out, err
	}
	out, err := run(hashA)
	require.NoError(t, err)
	assert.Equal(t, event.Processed, out)
	rec, _, err := f.inbox.Get(ctx, testDB, "stripe", "evt_h")
	require.NoError(t, err)
	require.NotNil(t, rec.PayloadHash)
	assert.Equal(t, hashA, *rec.PayloadHash)

	_, err = run(hashB)
	require.Error(t, err)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))

	out, err = run(hashA)
	require.NoError(t, err)
	assert.Equal(t, event.Duplicate, out)
	out, err = run("")
	require.NoError(t, err)
	assert.Equal(t, event.Duplicate, out, "callers without a hash still dedup")
	assert.Equal(t, int64(1), runs.Load())
}

func TestIntegration_InboxUnderSerializable(t *testing.T) {
	f := newFixture(t)
	var runs atomic.Int64
	for i := 0; i < 3; i++ {
		var out event.Outcome
		err := testDB.Serializable(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = f.inbox.Process(ctx, tx, "ser", "m1", 1, counting(&runs))
			return err
		})
		require.NoError(t, err)
		if i == 0 {
			assert.Equal(t, event.Processed, out)
		} else {
			assert.Equal(t, event.Duplicate, out)
		}
	}
	assert.Equal(t, int64(1), runs.Load())
}
