//go:build integration

package redpandabus_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/reality/redpandabus"

	"github.com/nodal/controlplane/internal/testkit/deps"
)

// The suite runs against the LOCAL Redpanda (docker-compose) or any broker
// named by CP_TEST_REDPANDA_BROKERS. Every test creates its own topic and
// consumer group, so runs never collide and nothing needs cleaning between
// them.
func brokers(t *testing.T) []string {
	t.Helper()
	v := deps.Need(t, "CP_TEST_REDPANDA_BROKERS", "Redpanda")
	var out []string
	for _, b := range strings.Split(v, ",") {
		if b = strings.TrimSpace(b); b != "" {
			out = append(out, b)
		}
	}
	require.NotEmpty(t, out)
	return out
}

type testKind struct{}

func token() string {
	return strings.ToLower(strings.ReplaceAll(id.New[testKind]().String(), "-", ""))[:12]
}

// newBus builds a real client and closes it with the test.
func newBus(t *testing.T, seeds []string, opts redpandabus.Options) *redpandabus.Client {
	t.Helper()
	if opts.ProduceTimeout == 0 {
		opts.ProduceTimeout = 10 * time.Second
	}
	c, err := redpandabus.New(context.Background(), config.RedpandaConfig{Brokers: seeds},
		config.NewResolver(config.EnvTest, os.LookupEnv), opts)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = c.Close(ctx)
	})
	return c
}

// createTopic provisions a topic with an exact partition count through the
// Kafka protocol, so the ordering tests control how many partitions exist
// instead of inheriting a broker default.
func createTopic(t *testing.T, seeds []string, name string, partitions int32) {
	t.Helper()
	admin, err := kgo.NewClient(kgo.SeedBrokers(seeds...), kgo.ClientID("redpandabus-test-admin"))
	require.NoError(t, err)
	defer admin.Close()

	req := kmsg.NewCreateTopicsRequest()
	topic := kmsg.NewCreateTopicsRequestTopic()
	topic.Topic = name
	topic.NumPartitions = partitions
	topic.ReplicationFactor = 1
	req.Topics = append(req.Topics, topic)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	raw, err := admin.Request(ctx, &req)
	require.NoError(t, err)
	resp, ok := raw.(*kmsg.CreateTopicsResponse)
	require.True(t, ok)
	require.Len(t, resp.Topics, 1)
	if err := kerr.ErrorForCode(resp.Topics[0].ErrorCode); err != nil {
		require.ErrorIs(t, err, kerr.TopicAlreadyExists, "create topic %s", name)
	}
}

// collector is an idempotent consumer: it is the shape every consumer of
// this bus must have, because delivery is at-least-once. It records how many
// times each event id was delivered and how many times the effect actually
// ran.
type collector struct {
	mu         sync.Mutex
	deliveries map[string]int
	effects    map[string]int
	order      map[string][]string       // partition key -> values in delivery order
	parts      map[string]map[int32]bool // partition key -> partitions it arrived on
	byWorker   map[string]int
	failOnce   map[string]bool
}

func newCollector() *collector {
	return &collector{
		deliveries: map[string]int{}, effects: map[string]int{}, order: map[string][]string{},
		parts: map[string]map[int32]bool{}, byWorker: map[string]int{}, failOnce: map[string]bool{},
	}
}

// handler returns an event.Handler labeled with a worker name so a test can
// tell which subscription handled what.
func (c *collector) handler(worker string) event.Handler {
	return func(_ context.Context, m event.Message) error {
		c.mu.Lock()
		c.deliveries[m.ID]++
		if c.failOnce[m.ID] {
			// One deliberate failure: the offset must not advance, so the
			// bus has to hand the same record back.
			delete(c.failOnce, m.ID)
			c.mu.Unlock()
			return fmt.Errorf("redpandabus test: deliberate handler failure on %s", m.ID)
		}
		if c.effects[m.ID] == 0 {
			// The dedup every consumer of an at-least-once bus must do.
			c.effects[m.ID] = 1
			c.order[m.Key] = append(c.order[m.Key], string(m.Value))
			if p, ok := partitionOf(m.ID); ok {
				if c.parts[m.Key] == nil {
					c.parts[m.Key] = map[int32]bool{}
				}
				c.parts[m.Key][p] = true
			}
			c.byWorker[worker]++
		}
		c.mu.Unlock()
		return nil
	}
}

func (c *collector) effectCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.effects)
}

func (c *collector) deliveriesOf(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deliveries[id]
}

func (c *collector) snapshotOrder(key string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.order[key]...)
}

// partitionsOf reports the distinct partitions a key's records arrived on.
func (c *collector) partitionsOf(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.parts[key])
}

// distinctPartitions is how many partitions the whole run touched. A test
// that asserts per-key ordering is only worth anything if the keys actually
// spread over more than one partition; on one partition every ordering holds
// trivially.
func (c *collector) distinctPartitions() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	all := map[int32]bool{}
	for _, ps := range c.parts {
		for p := range ps {
			all[p] = true
		}
	}
	return len(all)
}

// partitionOf reads the partition out of a fallback message id
// (topic/partition/offset), which is what messageID produces when a record
// carries no event_id header.
func partitionOf(messageID string) (int32, bool) {
	parts := strings.Split(messageID, "/")
	if len(parts) != 3 {
		return 0, false
	}
	n, err := strconv.ParseInt(parts[1], 10, 32)
	if err != nil {
		return 0, false
	}
	return int32(n), true
}

func (c *collector) workers() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]int{}
	for k, v := range c.byWorker {
		out[k] = v
	}
	return out
}

func (c *collector) armFailure(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failOnce[id] = true
}

func envelopeHeaders(eventID string) map[string]string {
	return map[string]string{event.HeaderEventID: eventID, event.HeaderContentType: event.ContentTypeJSON}
}

func TestIntegration_Redpanda_RoundTripPreservesKeyValueAndHeaders(t *testing.T) {
	seeds := brokers(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	topic, group := "test.bus."+token(), "g-"+token()
	createTopic(t, seeds, topic, 1)

	bus := newBus(t, seeds, redpandabus.Options{ClientID: "roundtrip"})
	got := make(chan event.Message, 4)
	require.NoError(t, bus.Subscribe(ctx, topic, group, func(_ context.Context, m event.Message) error {
		got <- m
		return nil
	}))

	headers := envelopeHeaders("evt-1")
	headers["custom"] = "kept"
	require.NoError(t, bus.Publish(ctx, topic, "agg-1", []byte(`{"n":1}`), headers))

	select {
	case m := <-got:
		require.Equal(t, "evt-1", m.ID, "the message id is the event_id header")
		require.Equal(t, topic, m.Topic)
		require.Equal(t, "agg-1", m.Key)
		require.JSONEq(t, `{"n":1}`, string(m.Value))
		require.Equal(t, "kept", m.Headers["custom"], "headers pass through verbatim")
		require.Equal(t, event.ContentTypeJSON, m.Headers[event.HeaderContentType])
	case <-time.After(30 * time.Second):
		t.Fatal("no delivery within 30s")
	}
}

// Kafka orders within a partition and the key picks the partition, so this
// is the test that the ordering guarantee actually holds end to end: three
// aggregates interleaved on the wire must each come back in their own
// publish order, and each must have landed on exactly one partition.
func TestIntegration_Redpanda_PerPartitionOrderingFollowsTheAggregateKey(t *testing.T) {
	seeds := brokers(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	topic, group := "test.bus."+token(), "g-"+token()
	createTopic(t, seeds, topic, 3)

	bus := newBus(t, seeds, redpandabus.Options{ClientID: "ordering"})
	col := newCollector()
	require.NoError(t, bus.Subscribe(ctx, topic, group, col.handler("only")))

	// Enough fixed keys that the partitioner demonstrably spreads them; the
	// hash is deterministic, so this stays true run to run.
	keys := []string{"agg-a", "agg-b", "agg-c", "agg-d", "agg-e", "agg-f", "agg-g", "agg-h"}
	const perKey = 20
	// Interleaved on purpose: if the key did not decide the partition, these
	// would come back shuffled.
	for i := 0; i < perKey; i++ {
		for _, k := range keys {
			// No event_id header, so Message.ID falls back to
			// topic/partition/offset and the test can see the partition.
			require.NoError(t, bus.Publish(ctx, topic, k, []byte(fmt.Sprintf("%s-%02d", k, i)), nil))
		}
	}

	require.Eventually(t, func() bool { return col.effectCount() == perKey*len(keys) }, 60*time.Second, 100*time.Millisecond,
		"expected %d records, have %d", perKey*len(keys), col.effectCount())

	for _, k := range keys {
		values := col.snapshotOrder(k)
		require.Len(t, values, perKey)
		for i, v := range values {
			require.Equalf(t, fmt.Sprintf("%s-%02d", k, i), v, "key %s arrived out of order at index %d", k, i)
		}
	}

	// Every record of one key came from exactly one partition: that is what
	// makes the ordering above a guarantee rather than a coincidence.
	for _, k := range keys {
		require.Equalf(t, 1, col.partitionsOf(k), "key %s was scattered across partitions", k)
	}
	require.Greaterf(t, col.distinctPartitions(), 1,
		"every key landed on one partition, so the ordering assertions above proved nothing")
}

// At-least-once is the contract, so a consumer must be idempotent. This
// delivers duplicates both serially and concurrently and asserts one effect
// per distinct event, and it forces a redelivery by failing a handler once
// to prove an offset never moves ahead of a record that was not handled.
func TestIntegration_Redpanda_DuplicatesSerialAndConcurrentProduceOneEffect(t *testing.T) {
	seeds := brokers(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	topic, group := "test.bus."+token(), "g-"+token()
	createTopic(t, seeds, topic, 3)

	bus := newBus(t, seeds, redpandabus.Options{ClientID: "dedup", RetryBackoff: 50 * time.Millisecond})
	col := newCollector()
	require.NoError(t, bus.Subscribe(ctx, topic, group, col.handler("only")))

	// Serial duplicates: the same event id published three times in a row,
	// as a retrying relay would.
	const serialCopies = 3
	for i := 0; i < serialCopies; i++ {
		require.NoError(t, bus.Publish(ctx, topic, "agg-serial", []byte(`{"n":1}`), envelopeHeaders("evt-serial")))
	}

	// Concurrent duplicates: several producers racing on the same event.
	const concurrentCopies = 8
	var wg sync.WaitGroup
	errCh := make(chan error, concurrentCopies)
	for i := 0; i < concurrentCopies; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- bus.Publish(ctx, topic, "agg-concurrent", []byte(`{"n":2}`), envelopeHeaders("evt-concurrent"))
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err, "a concurrent publish must succeed or fail, never be silently dropped")
	}

	// A record whose handler fails once must come back: the offset cannot
	// have moved past it.
	col.armFailure("evt-redelivered")
	require.NoError(t, bus.Publish(ctx, topic, "agg-redeliver", []byte(`{"n":3}`), envelopeHeaders("evt-redelivered")))

	require.Eventually(t, func() bool { return col.effectCount() == 3 }, 60*time.Second, 100*time.Millisecond,
		"expected exactly 3 distinct effects, have %d", col.effectCount())

	require.Equal(t, 3, col.effectCount(), "duplicates never multiply the effect")
	require.GreaterOrEqual(t, col.deliveriesOf("evt-serial"), serialCopies, "every serial copy was delivered")
	require.GreaterOrEqual(t, col.deliveriesOf("evt-concurrent"), concurrentCopies, "every concurrent copy was delivered")
	require.GreaterOrEqual(t, col.deliveriesOf("evt-redelivered"), 2, "the failed record was redelivered")

	// Nothing else appeared.
	time.Sleep(2 * time.Second)
	require.Equal(t, 3, col.effectCount())
}

// A produce failure must never be reported as a successful publish: the
// relay marks its outbox row published the moment Publish returns nil, so a
// false success loses the event with no trace.
func TestIntegration_Redpanda_ProducerErrorIsNeverReportedAsSuccess(t *testing.T) {
	seeds := brokers(t)
	ctx := context.Background()
	topic := "test.bus." + token()
	createTopic(t, seeds, topic, 1)
	bus := newBus(t, seeds, redpandabus.Options{ClientID: "produce-err"})

	// Far over any sane batch limit: the broker (or the client, on its
	// behalf) rejects it, and that rejection has to surface.
	huge := make([]byte, 4<<20)
	err := bus.Publish(ctx, topic, "agg-1", huge, envelopeHeaders("evt-huge"))
	require.Error(t, err, "an oversized record is a failure, not a silent drop")

	// A cancelled context is a failure too, never an optimistic success.
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, bus.Publish(dead, topic, "agg-1", []byte(`{}`), envelopeHeaders("evt-x")))

	// And the partition rule is enforced before anything reaches the wire.
	require.Error(t, bus.Publish(ctx, topic, "", []byte(`{}`), nil), "an empty key would round-robin")

	// After Close nothing is accepted, rather than accepted and dropped.
	require.NoError(t, bus.Close(ctx))
	require.ErrorIs(t, bus.Publish(ctx, topic, "agg-1", []byte(`{}`), envelopeHeaders("evt-y")), redpandabus.ErrClosed)
}

// Offset discipline, stated as the property that matters on a crash: a
// consumer that stops mid-stream must leave every unhandled record for the
// next member of its group. Nothing is lost and nothing is skipped.
func TestIntegration_Redpanda_UnhandledRecordsSurviveAConsumerRestart(t *testing.T) {
	seeds := brokers(t)
	topic, group := "test.bus."+token(), "g-"+token()
	createTopic(t, seeds, topic, 1)
	publisher := newBus(t, seeds, redpandabus.Options{ClientID: "restart-pub"})

	const total = 12
	ids := make([]string, total)
	for i := range ids {
		ids[i] = fmt.Sprintf("evt-%02d", i)
		require.NoError(t, publisher.Publish(context.Background(), topic, "agg-1",
			[]byte(strconv.Itoa(i)), envelopeHeaders(ids[i])))
	}

	// First consumer: handle exactly handleBeforeStop records, then stall on
	// the next one and be killed there. The stalled record was never
	// handled, so its offset must never have been committed.
	firstCtx, stopFirst := context.WithCancel(context.Background())
	first := newBus(t, seeds, redpandabus.Options{ClientID: "restart-a"})
	col := newCollector()
	handled := make(chan struct{}, total)
	const handleBeforeStop = 4
	var done atomic.Int32
	require.NoError(t, first.Subscribe(firstCtx, topic, group, func(ctx context.Context, m event.Message) error {
		if done.Load() >= handleBeforeStop {
			<-ctx.Done() // killed mid-record, with this record unhandled
			return ctx.Err()
		}
		if err := col.handler("a")(ctx, m); err != nil {
			return err
		}
		done.Add(1)
		handled <- struct{}{}
		return nil
	}))
	for i := 0; i < handleBeforeStop; i++ {
		select {
		case <-handled:
		case <-time.After(60 * time.Second):
			t.Fatalf("only %d records handled before the timeout", i)
		}
	}
	stopFirst()
	closeCtx, cancelClose := context.WithTimeout(context.Background(), 20*time.Second)
	require.NoError(t, first.Close(closeCtx))
	cancelClose()
	require.Equal(t, handleBeforeStop, col.effectCount(), "the stalled record was not handled")

	// Second consumer, same group: it must see everything the first did not
	// commit, and the union must be every record exactly once.
	secondCtx, stopSecond := context.WithCancel(context.Background())
	defer stopSecond()
	second := newBus(t, seeds, redpandabus.Options{ClientID: "restart-b"})
	require.NoError(t, second.Subscribe(secondCtx, topic, group, col.handler("b")))

	require.Eventually(t, func() bool { return col.effectCount() == total }, 90*time.Second, 200*time.Millisecond,
		"expected all %d records across the two consumers, have %d", total, col.effectCount())
	for _, want := range ids {
		require.GreaterOrEqualf(t, col.deliveriesOf(want), 1, "record %s was lost across the restart", want)
	}
	workers := col.workers()
	require.Equal(t, handleBeforeStop, workers["a"], "the first consumer committed exactly what it handled")
	require.Equal(t, total-handleBeforeStop, workers["b"], "the second consumer resumed at the uncommitted offset, skipping nothing and repeating nothing")
}

// A rebalance must not lose or double-apply a record. A second member joins
// the group mid-stream, which revokes partitions from the first; because a
// rebalance can only happen between polls, no handler is ever interrupted
// with uncommitted work, and the idempotent consumer absorbs the redeliveries
// that at-least-once allows.
func TestIntegration_Redpanda_RebalanceDuringConsumptionLosesNothing(t *testing.T) {
	seeds := brokers(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	topic, group := "test.bus."+token(), "g-"+token()
	createTopic(t, seeds, topic, 3)

	publisher := newBus(t, seeds, redpandabus.Options{ClientID: "rebalance-pub"})
	col := newCollector()

	busA := newBus(t, seeds, redpandabus.Options{ClientID: "rebalance-a", SessionTimeout: 10 * time.Second})
	require.NoError(t, busA.Subscribe(ctx, topic, group, col.handler("a")))

	publish := func(prefix string, n int) []string {
		out := make([]string, n)
		for i := 0; i < n; i++ {
			out[i] = fmt.Sprintf("%s-%02d", prefix, i)
			require.NoError(t, publisher.Publish(ctx, topic, fmt.Sprintf("agg-%s-%02d", prefix, i),
				[]byte(out[i]), envelopeHeaders(out[i])))
		}
		return out
	}
	before := publish("before", 30)
	require.Eventually(t, func() bool { return col.effectCount() >= len(before) }, 60*time.Second, 100*time.Millisecond,
		"the first consumer should drain the first batch, has %d", col.effectCount())

	// Second member joins: this is the rebalance.
	busB := newBus(t, seeds, redpandabus.Options{ClientID: "rebalance-b", SessionTimeout: 10 * time.Second})
	require.NoError(t, busB.Subscribe(ctx, topic, group, col.handler("b")))

	// Keep feeding the topic while the group rebalances. Publishing one
	// fixed batch would prove nothing: the incumbent drains it before the
	// joining member is assigned anything, and the test would pass without a
	// single record ever crossing the rebalance.
	var after []string
	deadline := time.Now().Add(90 * time.Second)
	for i := 0; time.Now().Before(deadline); {
		for j := 0; j < 10; j++ {
			evt := fmt.Sprintf("after-%04d", i)
			require.NoError(t, publisher.Publish(ctx, topic, fmt.Sprintf("agg-after-%04d", i), []byte(evt), envelopeHeaders(evt)))
			after = append(after, evt)
			i++
		}
		if col.workers()["b"] > 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.Positive(t, col.workers()["b"],
		"the joining member never received a partition, so no rebalance was exercised")
	require.Positive(t, col.workers()["a"], "the incumbent kept working across the rebalance")

	want := len(before) + len(after)
	require.Eventually(t, func() bool { return col.effectCount() == want }, 120*time.Second, 200*time.Millisecond,
		"expected %d distinct effects across the rebalance, have %d", want, col.effectCount())
	for _, evt := range append(append([]string(nil), before...), after...) {
		require.GreaterOrEqualf(t, col.deliveriesOf(evt), 1, "record %s was lost across the rebalance", evt)
	}
	require.Equal(t, want, col.effectCount(), "a rebalance never multiplies an effect")
}

// A broker lost mid-stream, for real: the container is paused, which is what
// a network partition or an unresponsive node looks like to the client. The
// two things that must hold are that a publish into the dark fails loudly,
// and that the consumer resumes on its own when the broker returns, without
// a restart and without losing what was in flight.
func TestIntegration_Redpanda_BrokerLostMidStreamFailsLoudlyThenRecovers(t *testing.T) {
	seeds := brokers(t)
	container := os.Getenv("CP_TEST_REDPANDA_CONTAINER")
	if container == "" {
		container = "cp-redpanda"
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH; skipping broker-loss test")
	}
	if err := exec.Command("docker", "inspect", container).Run(); err != nil {
		t.Skipf("container %s not found; skipping broker-loss test", container)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	topic, group := "test.bus."+token(), "g-"+token()
	createTopic(t, seeds, topic, 1)

	// A short produce timeout so the publish into a paused broker fails
	// within the test rather than hanging on the default.
	bus := newBus(t, seeds, redpandabus.Options{ClientID: "broker-loss", ProduceTimeout: 5 * time.Second, SessionTimeout: 45 * time.Second})
	col := newCollector()
	require.NoError(t, bus.Subscribe(ctx, topic, group, col.handler("only")))

	require.NoError(t, bus.Publish(ctx, topic, "agg-1", []byte("before"), envelopeHeaders("evt-before")))
	require.Eventually(t, func() bool { return col.effectCount() == 1 }, 60*time.Second, 100*time.Millisecond)

	var pauseMu sync.Mutex
	paused := false
	unpause := func() {
		pauseMu.Lock()
		defer pauseMu.Unlock()
		if !paused {
			return
		}
		// Tolerant on purpose: cleanup must restore shared infrastructure
		// even if the test body already unpaused, and must never be the
		// reason a run fails.
		_ = exec.Command("docker", "unpause", container).Run()
		paused = false
	}
	t.Cleanup(unpause)
	require.NoError(t, exec.Command("docker", "pause", container).Run())
	pauseMu.Lock()
	paused = true
	pauseMu.Unlock()

	// Publish into the dark. It must come back, and it must not come back
	// nil: a relay that is told "published" marks its outbox row and the
	// event is gone. Bounding this is the whole point — before Publish
	// enforced its own deadline, this call blocked for as long as the broker
	// stayed silent.
	type result struct {
		err  error
		took time.Duration
	}
	res := make(chan result, 1)
	go func() {
		start := time.Now()
		err := bus.Publish(context.Background(), topic, "agg-1", []byte("during"), envelopeHeaders("evt-during"))
		res <- result{err, time.Since(start)}
	}()
	select {
	case r := <-res:
		require.Error(t, r.err, "publishing into a broker that cannot answer must fail, never look successful")
		require.Lessf(t, r.took, 60*time.Second, "publish returned after %s; it must be bounded by ProduceTimeout", r.took)
	case <-time.After(90 * time.Second):
		unpause()
		t.Fatal("publish never returned: a stalled broker must not block a producer indefinitely")
	}

	unpause()

	// The consumer was never restarted; it must recover by itself.
	require.NoError(t, bus.Publish(ctx, topic, "agg-1", []byte("after"), envelopeHeaders("evt-after")))
	require.Eventually(t, func() bool { return col.deliveriesOf("evt-after") > 0 }, 120*time.Second, 250*time.Millisecond,
		"the consumer did not resume after the broker returned")

	// "before" was handled and committed; it must not have been lost.
	require.GreaterOrEqual(t, col.deliveriesOf("evt-before"), 1)
}
