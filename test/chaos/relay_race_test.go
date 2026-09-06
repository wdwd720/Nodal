//go:build integration && chaos

package chaos

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/event"
)

// recordingBus records every publish. It is not a stand-in for a broker: the
// broker's own behavior is covered by the redpandabus suite and by
// TestChaos_BrokerStallNeverMarksOutboxPublished. Here the question is what
// the RELAY does, so what matters is how many times each event id was handed
// to a bus at all.
type recordingBus struct {
	mu       sync.Mutex
	byID     map[string]int
	order    []string
	onPublis func(id string) error
}

func newRecordingBus() *recordingBus { return &recordingBus{byID: map[string]int{}} }

func (b *recordingBus) Publish(_ context.Context, _, _ string, _ []byte, headers map[string]string) error {
	id := headers[event.HeaderEventID]
	if b.onPublis != nil {
		if err := b.onPublis(id); err != nil {
			return err
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.byID[id]++
	b.order = append(b.order, id)
	return nil
}

func (b *recordingBus) Subscribe(context.Context, string, string, event.Handler) error { return nil }

func (b *recordingBus) Close(context.Context) error { return nil }

func (b *recordingBus) count(id string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.byID[id]
}

func (b *recordingBus) sequence() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.order...)
}

// TestChaos_RelayInstanceDiesMidPassNeverLosesOrDuplicates is the
// "two workers race for the same work, and one of them dies holding it" case.
//
// `Relay.claim` uses FOR UPDATE SKIP LOCKED: a claim IS a lease, held for the
// life of one transaction. D-037 covers two healthy relays. What is not
// covered anywhere is the failure that makes a lease interesting — the holder
// dying while it holds rows. Postgres releases the locks at that moment, so
// the rows become claimable again with the first instance's work rolled back.
//
// The invariants, in the order they matter:
//   - NO LOST EVENT: every committed outbox row is eventually published and
//     ends up marked published;
//   - NO SILENT DIVERGENCE: a row is never marked published without having
//     been handed to the bus (that is the pairing the whole outbox rests on);
//   - AT-LEAST-ONCE, BOUNDED: a duplicate publish is allowed — consumers
//     dedup by event id — but the rows the dead instance never reached must
//     not be duplicated at all, or the "skip locked" claim is not exclusive.
func TestChaos_RelayInstanceDiesMidPassNeverLosesOrDuplicates(t *testing.T) {
	requireEnv(t)
	requireHealthyStack(t, PostgresContainer())

	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC))
	outbox := event.NewOutbox(clk)
	topic := event.TopicOrderTransitioned

	// Several partitions so the relay has real work to distribute, and so a
	// death mid-pass leaves some rows published and some not.
	const partitions, perPartition = 4, 5
	var envs []event.Envelope
	aggregates := make([]string, partitions)
	for p := range partitions {
		aggregates[p] = "relayrace-" + chaosToken()
		for s := range perPartition {
			envs = append(envs, envelope(t, clk, topic, aggregates[p], s))
		}
	}
	require.NoError(t, testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return outbox.Enqueue(ctx, tx, topic.String(), envs...)
	}))
	require.Equal(t, len(envs), unpublishedFor(t, envs), "precondition: every row is committed and unpublished")

	rogue := chaosBreak(t, "rogue_relay_ignores_the_claim")

	busA, busB := newRecordingBus(), newRecordingBus()
	var killed atomic.Int64
	var publishedByA atomic.Int64

	// Instance A dies after publishing a few rows but before its transaction
	// commits. Everything it "published" is therefore un-marked, and every
	// row it held is released for B.
	const dieAfter = 3
	busA.onPublis = func(string) error {
		if publishedByA.Add(1) == dieAfter {
			// Kill A's own backend from outside. The relay is mid-transaction
			// with rows locked; this is a lease-holder crash.
			n, err := terminateBackendsOfApp(ctx, adminDSN, testDBName, "chaos-relay-a")
			if err == nil {
				killed.Add(n)
			}
		}
		return nil
	}

	// A has its own pool so its backends can be identified and killed without
	// touching the shared one.
	poolA, err := db.Open(ctx, db.Config{URL: testAppURL, AppName: "chaos-relay-a", MaxConns: 2})
	require.NoError(t, err)
	t.Cleanup(poolA.Close)

	relayA := event.NewRelay(poolA, busA, nil, event.RelayOptions{Clock: clk, BatchSize: len(envs)})
	nA, errA := relayA.RunOnce(ctx)
	t.Logf("chaos: instance A published=%d err=%v killed=%d handed-to-bus=%d",
		nA, errA, killed.Load(), publishedByA.Load())

	require.Positive(t, killed.Load(),
		"instance A's backend was never terminated; the lease-holder crash never happened and this test proves nothing")
	require.Error(t, errA, "instance A's pass must fail once its backend dies")
	require.Zero(t, nA, "a pass that never committed must not report rows published")

	// Nothing A did may have survived: it was all one transaction.
	assert.Equal(t, len(envs), unpublishedFor(t, envs),
		"instance A's uncommitted work survived its death; rows were marked published by a transaction that rolled back")

	waitDB(t, 60*time.Second)

	// --- instance B takes over -----------------------------------------------
	if rogue {
		// NEGATIVE CONTROL: B publishes every unpublished row it can SEE,
		// rather than every row it could CLAIM, and marks none of them. That
		// is what a second worker ignoring the lease looks like, and it shows
		// up as the same event handed to a bus twice.
		rows, err := testDB.Pool().Query(ctx,
			`SELECT id, aggregate_id FROM outbox_events WHERE id = ANY($1::uuid[]) AND published_at IS NULL ORDER BY recorded_at, id`,
			idsOf(envs))
		require.NoError(t, err)
		type row struct{ id, agg string }
		var seen []row
		for rows.Next() {
			var r row
			require.NoError(t, rows.Scan(&r.id, &r.agg))
			seen = append(seen, r)
		}
		rows.Close()
		require.NoError(t, rows.Err())
		for _, r := range seen {
			require.NoError(t, busB.Publish(ctx, topic.String(), r.agg, nil, map[string]string{event.HeaderEventID: r.id}))
		}
		// And it publishes them a second time, because nothing told it the
		// rows were already in flight.
		for _, r := range seen {
			require.NoError(t, busB.Publish(ctx, topic.String(), r.agg, nil, map[string]string{event.HeaderEventID: r.id}))
		}
	}

	relayB := event.NewRelay(testDB, busB, nil, event.RelayOptions{Clock: clk, BatchSize: len(envs)})
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) && unpublishedFor(t, envs) > 0 {
		before := unpublishedFor(t, envs)
		_, err := relayB.RunOnce(ctx)
		require.NoError(t, err)
		// RunOnce's count covers the whole outbox, including rows other tests
		// left on this database, so progress is measured on our own rows only.
		if unpublishedFor(t, envs) == before {
			clk.Advance(time.Minute)
		}
	}

	// --- the invariants ------------------------------------------------------
	assert.Zero(t, unpublishedFor(t, envs),
		"instance B never drained the rows the dead instance left behind: events are lost")

	// NO LOST EVENT, and a bounded duplicate window.
	//
	// The bound is not arbitrary. `Relay.runOnce` hands the ENTIRE claimed
	// batch to the bus and only then marks the successful rows, so a crash at
	// the mark step re-publishes every row of that batch — the duplicate blast
	// radius of one crashed relay is its BatchSize, not one row. That is
	// correct at-least-once behavior and the deliberate D-034 trade (a
	// duplicate is routine, a lost event is unrecoverable), but it is worth
	// pinning: an operator choosing BatchSize is choosing that radius.
	handedByA := int(publishedByA.Load())
	dupes := 0
	for _, e := range envs {
		a, b := busA.count(e.ID), busB.count(e.ID)
		assert.GreaterOrEqualf(t, a+b, 1, "event %s was never handed to any bus: it is lost", e.ID)
		assert.LessOrEqualf(t, b, 1,
			"event %s was published %d times by the SURVIVING instance alone; "+
				"one instance must publish a row once per claim", e.ID, b)
		if a+b > 1 {
			dupes++
		}
	}
	assert.LessOrEqualf(t, dupes, handedByA,
		"%d events were duplicated but the dead instance only ever handed %d to a bus; "+
			"the extra duplicates mean a row was published without being claimed", dupes, handedByA)
	t.Logf("chaos: dead instance handed %d events to its bus before dying; %d events were duplicated by the takeover",
		handedByA, dupes)

	// Per-partition ordering must still hold in what B published: an
	// aggregate's events may repeat, but they may never go backwards.
	assertPartitionOrder(t, busB.sequence(), envs, aggregates)
}

// idsOf returns the event ids as a []string for a uuid[] parameter.
func idsOf(envs []event.Envelope) []string {
	out := make([]string, len(envs))
	for i, e := range envs {
		out[i] = e.ID
	}
	return out
}

// assertPartitionOrder checks that, within each aggregate, the published
// sequence never delivers an event before one enqueued earlier. Duplicates are
// tolerated; going backwards is not.
func assertPartitionOrder(t *testing.T, sequence []string, envs []event.Envelope, aggregates []string) {
	t.Helper()
	rank := map[string]int{}
	agg := map[string]string{}
	perAgg := map[string]int{}
	for _, e := range envs {
		rank[e.ID] = perAgg[e.AggregateID]
		perAgg[e.AggregateID]++
		agg[e.ID] = e.AggregateID
	}
	// A precondition, because an ordering assertion over one partition — or
	// over none — holds trivially and proves nothing (the exact defect D-034
	// found in the bus suite).
	touched := map[string]bool{}
	for _, id := range sequence {
		if a, ok := agg[id]; ok {
			touched[a] = true
		}
	}
	require.GreaterOrEqualf(t, len(touched), 2,
		"the published sequence covered %d aggregates; an ordering assertion over fewer than two proves nothing", len(touched))

	highest := map[string]int{}
	for _, a := range aggregates {
		highest[a] = -1
	}
	for _, id := range sequence {
		a, ok := agg[id]
		if !ok {
			continue
		}
		r := rank[id]
		assert.GreaterOrEqualf(t, r, highest[a],
			"aggregate %s delivered event rank %d after rank %d: the partition reordered", a, r, highest[a])
		if r > highest[a] {
			highest[a] = r
		}
	}
}

// terminateBackendsOfApp kills every backend on dbName whose application_name
// matches, from an admin connection elsewhere. Scoped by application_name so
// only the intended relay instance dies and every other pool survives.
func terminateBackendsOfApp(ctx context.Context, admin, dbName, appName string) (int64, error) {
	conn, err := pgxConnect(ctx, admin)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close(ctx) }()
	var n int64
	err = conn.QueryRow(ctx,
		`SELECT count(*) FROM (
		   SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		    WHERE datname = $1 AND application_name = $2 AND pid <> pg_backend_pid()
		 ) t`, dbName, appName).Scan(&n)
	return n, err
}
