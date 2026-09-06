package eventtest_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/event/eventtest"
)

func newBus(t *testing.T) *eventtest.MemoryBus {
	t.Helper()
	b, err := eventtest.NewMemoryBus("TEST")
	require.NoError(t, err)
	return b
}

func publish(t *testing.T, b event.Bus, topic, key, id string) {
	t.Helper()
	require.NoError(t, b.Publish(context.Background(), topic, key, []byte(id), map[string]string{event.HeaderEventID: id}))
}

// collector records delivered message ids in order, safely.
type collector struct {
	mu  sync.Mutex
	ids []string
}

func (c *collector) handler(_ context.Context, m event.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids = append(c.ids, m.ID)
	return nil
}

func (c *collector) got() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ids...)
}

func TestNewMemoryBus_EnvGate(t *testing.T) {
	t.Parallel()
	for _, env := range []string{"PROD", "STAGING", "", "local", "Test", "DEV\n"} {
		b, err := eventtest.NewMemoryBus(env)
		assert.ErrorIs(t, err, eventtest.ErrNotAllowed, env)
		assert.Nil(t, b)
		assert.False(t, eventtest.Allowed(env))
	}
	for _, env := range []string{"LOCAL", "TEST", "DEV"} {
		b, err := eventtest.NewMemoryBus(env)
		require.NoError(t, err)
		assert.Equal(t, env, b.Env())
		assert.True(t, eventtest.Allowed(env))
	}
}

func TestMemoryBus_EveryGroupGetsEveryMessageOnceInOrder(t *testing.T) {
	t.Parallel()
	b := newBus(t)
	ctx := context.Background()
	var g1, g2, g3 collector
	require.NoError(t, b.Subscribe(ctx, "t", "g1", g1.handler))
	require.NoError(t, b.Subscribe(ctx, "t", "g2", g2.handler))

	want := []string{"m1", "m2", "m3", "m4", "m5"}
	for _, id := range want {
		publish(t, b, "t", "k", id)
	}
	assert.Equal(t, want, g1.got())
	assert.Equal(t, want, g2.got())

	// A late group replays the retained log from the beginning.
	require.NoError(t, b.Subscribe(ctx, "t", "g3", g3.handler))
	assert.Equal(t, want, g3.got())
	assert.Equal(t, 5, b.Delivered("t", "g3"))
	assert.Len(t, b.Log("t"), 5)
	assert.Nil(t, b.Log("other"))
	assert.Empty(t, b.Failures())
}

func TestMemoryBus_PerKeyOrderingAcrossHandlersOfAGroup(t *testing.T) {
	t.Parallel()
	b := newBus(t)
	ctx := context.Background()
	var mu sync.Mutex
	owner := map[string]int{}  // key -> handler index
	seen := map[string][]int{} // key -> sequence numbers delivered
	for i := 0; i < 3; i++ {
		i := i
		require.NoError(t, b.Subscribe(ctx, "t", "g", func(_ context.Context, m event.Message) error {
			mu.Lock()
			defer mu.Unlock()
			if prev, ok := owner[m.Key]; ok && prev != i {
				return fmt.Errorf("key %s moved from handler %d to %d", m.Key, prev, i)
			}
			owner[m.Key] = i
			_, seqText, _ := strings.Cut(m.ID, "-")
			seq, err := strconv.Atoi(seqText)
			if err != nil {
				return err
			}
			seen[m.Key] = append(seen[m.Key], seq)
			return nil
		}))
	}
	keys := []string{"a", "b", "c", "d", "e", "f"}
	for seq := 0; seq < 60; seq++ {
		publish(t, b, "t", keys[seq%len(keys)], fmt.Sprintf("%s-%d", keys[seq%len(keys)], seq))
	}
	require.Empty(t, b.Failures())
	mu.Lock()
	defer mu.Unlock()
	handlersUsed := map[int]bool{}
	for _, k := range keys {
		handlersUsed[owner[k]] = true
		require.Len(t, seen[k], 10, k)
		for i := 1; i < len(seen[k]); i++ {
			assert.Less(t, seen[k][i-1], seen[k][i], "key %s out of order: %v", k, seen[k])
		}
	}
	assert.Greater(t, len(handlersUsed), 1, "keys should spread over handlers")
}

func TestMemoryBus_DuplicateEvery(t *testing.T) {
	t.Parallel()
	b := newBus(t)
	var c collector
	require.NoError(t, b.Subscribe(context.Background(), "t", "g", c.handler))
	b.DuplicateEvery(2)
	for _, id := range []string{"1", "2", "3", "4"} {
		publish(t, b, "t", "k", id)
	}
	assert.Equal(t, []string{"1", "2", "2", "3", "4", "4"}, c.got())
	b.DuplicateEvery(0)
	publish(t, b, "t", "k", "5")
	assert.Equal(t, []string{"1", "2", "2", "3", "4", "4", "5"}, c.got())
}

func TestMemoryBus_ReorderWindowKeepsPerKeyOrder(t *testing.T) {
	t.Parallel()
	b := newBus(t)
	var c collector
	require.NoError(t, b.Subscribe(context.Background(), "t", "g", c.handler))
	b.ReorderWindow(3)

	publish(t, b, "t", "a", "a1")
	publish(t, b, "t", "b", "b1")
	assert.Empty(t, c.got(), "window not full yet")
	publish(t, b, "t", "a", "a2")
	assert.Equal(t, []string{"b1", "a1", "a2"}, c.got(), "keys reversed, per-key order kept")

	publish(t, b, "t", "c", "c1")
	assert.Len(t, c.got(), 3, "partial window is held")
	b.Flush()
	assert.Equal(t, []string{"b1", "a1", "a2", "c1"}, c.got())

	b.ReorderWindow(1)
	publish(t, b, "t", "d", "d1")
	assert.Equal(t, []string{"b1", "a1", "a2", "c1", "d1"}, c.got(), "window disabled delivers immediately")
}

func TestMemoryBus_FailPublishFor(t *testing.T) {
	t.Parallel()
	b := newBus(t)
	ctx := context.Background()
	b.FailPublishFor("t", 2)
	err := b.Publish(ctx, "t", "k", []byte("x"), nil)
	assert.ErrorIs(t, err, eventtest.ErrInjectedPublishFailure)
	require.NoError(t, b.Publish(ctx, "other", "k", []byte("x"), nil), "other topics unaffected")
	err = b.Publish(ctx, "t", "k", []byte("x"), nil)
	assert.ErrorIs(t, err, eventtest.ErrInjectedPublishFailure)
	require.NoError(t, b.Publish(ctx, "t", "k", []byte("x"), nil))
	assert.Len(t, b.Log("t"), 1, "failed publishes are not retained")

	b.FailPublishFor("t", 1)
	b.FailPublishFor("t", 0)
	require.NoError(t, b.Publish(ctx, "t", "k", []byte("x"), nil), "cleared")
}

func TestMemoryBus_HandlerErrorsAreRedelivered(t *testing.T) {
	t.Parallel()
	b := newBus(t)
	ctx := context.Background()
	calls := 0
	require.NoError(t, b.Subscribe(ctx, "t", "flaky", func(context.Context, event.Message) error {
		calls++
		if calls < 3 {
			return errors.New("try again")
		}
		return nil
	}))
	publish(t, b, "t", "k", "m1")
	assert.Equal(t, 3, calls)
	assert.Empty(t, b.Failures())

	always := 0
	require.NoError(t, b.Subscribe(ctx, "t", "broken", func(context.Context, event.Message) error {
		always++
		return errors.New("never")
	}))
	assert.Equal(t, 1+eventtest.DefaultMaxRedeliveries, always)
	f := b.Failures()
	require.Len(t, f, 1)
	assert.Equal(t, "broken", f[0].Group)
	assert.Equal(t, "m1", f[0].Message.ID)
	assert.EqualError(t, f[0].Err, "never")
}

func TestMemoryBus_HandlerMayPublish(t *testing.T) {
	t.Parallel()
	b := newBus(t)
	ctx := context.Background()
	var downstream collector
	require.NoError(t, b.Subscribe(ctx, "second", "g", downstream.handler))
	require.NoError(t, b.Subscribe(ctx, "first", "g", func(ctx context.Context, m event.Message) error {
		return b.Publish(ctx, "second", m.Key, m.Value, map[string]string{event.HeaderEventID: m.ID + "-fwd"})
	}))
	publish(t, b, "first", "k", "m1")
	publish(t, b, "first", "k", "m2")
	assert.Equal(t, []string{"m1-fwd", "m2-fwd"}, downstream.got())
}

func TestMemoryBus_CancelledSubscriptionStopsAndGroupRetains(t *testing.T) {
	t.Parallel()
	b := newBus(t)
	sctx, cancel := context.WithCancel(context.Background())
	var first, second collector
	require.NoError(t, b.Subscribe(sctx, "t", "g", first.handler))
	publish(t, b, "t", "k", "m1")
	cancel()
	publish(t, b, "t", "k", "m2")
	assert.Equal(t, []string{"m1"}, first.got())

	// The group had no live handler: m2 waits for the next subscriber.
	require.NoError(t, b.Subscribe(context.Background(), "t", "g", second.handler))
	assert.Equal(t, []string{"m2"}, second.got())
}

func TestMemoryBus_ConcurrentPublishers(t *testing.T) {
	t.Parallel()
	b := newBus(t)
	var c collector
	require.NoError(t, b.Subscribe(context.Background(), "t", "g", c.handler))
	var wg sync.WaitGroup
	for p := 0; p < 8; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				publish(t, b, "t", fmt.Sprint(p), fmt.Sprintf("%d-%d", p, i))
			}
		}(p)
	}
	wg.Wait()
	got := c.got()
	assert.Len(t, got, 400)
	unique := map[string]bool{}
	for _, id := range got {
		unique[id] = true
	}
	assert.Len(t, unique, 400)
	assert.Equal(t, 400, b.Delivered("t", "g"))
}

func TestMemoryBus_CloseAndValidation(t *testing.T) {
	t.Parallel()
	b := newBus(t)
	ctx := context.Background()
	assert.Error(t, b.Subscribe(ctx, "t", "g", nil))
	assert.Error(t, b.Subscribe(ctx, "", "g", func(context.Context, event.Message) error { return nil }))
	require.NoError(t, b.Close(ctx))
	assert.ErrorIs(t, b.Publish(ctx, "t", "k", nil, nil), eventtest.ErrClosed)
	assert.ErrorIs(t, b.Subscribe(ctx, "t", "g", func(context.Context, event.Message) error { return nil }), eventtest.ErrClosed)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	assert.ErrorIs(t, newBus(t).Publish(cctx, "t", "k", nil, nil), context.Canceled)
}

func TestMemoryBus_DeliversCopies(t *testing.T) {
	t.Parallel()
	b := newBus(t)
	var got event.Message
	require.NoError(t, b.Subscribe(context.Background(), "t", "g", func(_ context.Context, m event.Message) error {
		got = m
		m.Value[0] = 'Z'
		m.Headers["h"] = "changed"
		return nil
	}))
	value := []byte("abc")
	headers := map[string]string{"h": "v"}
	publish := b.Publish(context.Background(), "t", "k", value, headers)
	require.NoError(t, publish)
	assert.Equal(t, "abc", string(value))
	assert.Equal(t, "v", headers["h"])
	assert.Equal(t, "abc", string(b.Log("t")[0].Value), "log is not aliased by handlers")
	assert.Equal(t, "Z", string(got.Value[:1]))
}

func TestRecorder(t *testing.T) {
	t.Parallel()
	r := eventtest.NewRecorder()
	ctx := context.Background()
	require.NoError(t, r.Subscribe(ctx, "t", "g", func(context.Context, event.Message) error { return nil }))
	assert.Equal(t, []string{"g"}, r.Groups("t"))

	require.NoError(t, r.Publish(ctx, "t", "k", []byte("v"), map[string]string{event.HeaderEventID: "e1"}))
	require.NoError(t, r.Publish(ctx, "u", "k", []byte("w"), nil))
	msgs := r.Messages()
	require.Len(t, msgs, 2)
	assert.Equal(t, "e1", msgs[0].ID)
	assert.Equal(t, "rec-2", msgs[1].ID)
	assert.Len(t, r.ByTopic("t"), 1)

	boom := errors.New("boom")
	r.SetPublishError(boom)
	assert.ErrorIs(t, r.Publish(ctx, "t", "k", nil, nil), boom)
	r.SetPublishError(nil)
	r.FailNext(2, boom)
	assert.ErrorIs(t, r.Publish(ctx, "t", "k", nil, nil), boom)
	assert.ErrorIs(t, r.Publish(ctx, "t", "k", nil, nil), boom)
	require.NoError(t, r.Publish(ctx, "t", "k", nil, nil))
	assert.Len(t, r.Messages(), 3)
	r.Reset()
	assert.Empty(t, r.Messages())
	require.NoError(t, r.Close(ctx))
	assert.ErrorIs(t, r.Publish(ctx, "t", "k", nil, nil), eventtest.ErrClosed)
}
