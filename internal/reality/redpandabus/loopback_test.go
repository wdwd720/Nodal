package redpandabus_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/reality/redpandabus"
)

func TestNewLoopback_RefusesProductionLikeEnvironments(t *testing.T) {
	t.Parallel()
	for _, env := range []config.Environment{config.EnvStaging, config.EnvProd} {
		_, err := redpandabus.NewLoopback(env, redpandabus.Options{})
		require.ErrorIs(t, err, redpandabus.ErrFakeModeForbidden, string(env))
	}
	for _, env := range []config.Environment{config.EnvLocal, config.EnvTest, config.EnvDev} {
		b, err := redpandabus.NewLoopback(env, redpandabus.Options{})
		require.NoError(t, err, string(env))
		require.NoError(t, b.Close(context.Background()))
	}
}

func TestLoopback_AtLeastOnceRedeliversOnHandlerError(t *testing.T) {
	t.Parallel()
	b, err := redpandabus.NewLoopback(config.EnvTest, redpandabus.Options{RetryBackoff: time.Millisecond})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var got []string
	failedOnce := false
	require.NoError(t, b.Subscribe(ctx, "t", "g1", func(_ context.Context, m event.Message) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, m.ID)
		if m.Key == "b" && !failedOnce {
			failedOnce = true
			return errors.New("transient")
		}
		return nil
	}))
	for _, k := range []string{"a", "b", "c"} {
		require.NoError(t, b.Publish(ctx, "t", k, []byte(k), map[string]string{event.HeaderEventID: "id-" + k}))
	}
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 4
	}, 5*time.Second, 5*time.Millisecond)
	mu.Lock()
	require.Equal(t, []string{"id-a", "id-b", "id-b", "id-c"}, got, "b was delivered twice, order preserved, nothing skipped")
	mu.Unlock()

	// A second group sees the whole log from the start; the first group does not see duplicates.
	var second []string
	var smu sync.Mutex
	require.NoError(t, b.Subscribe(ctx, "t", "g2", func(_ context.Context, m event.Message) error {
		smu.Lock()
		defer smu.Unlock()
		second = append(second, m.Key)
		return nil
	}))
	require.Eventually(t, func() bool {
		smu.Lock()
		defer smu.Unlock()
		return len(second) == 3
	}, 5*time.Second, 5*time.Millisecond)
	require.Len(t, b.Log("t"), 3)
	require.NoError(t, b.Close(context.Background()))
	require.ErrorIs(t, b.Publish(ctx, "t", "x", nil, nil), redpandabus.ErrClosed)
}

func TestLoopback_PoisonRecordBlocksPartitionAndReportsAfterRetries(t *testing.T) {
	t.Parallel()
	var reported error
	var rmu sync.Mutex
	b, err := redpandabus.NewLoopback(config.EnvLocal, redpandabus.Options{
		RetryBackoff: time.Millisecond, MaxHandlerRetries: 2,
		OnError: func(_, _ string, err error) { rmu.Lock(); reported = err; rmu.Unlock() },
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	var amu sync.Mutex
	require.NoError(t, b.Subscribe(ctx, "t", "g", func(context.Context, event.Message) error {
		amu.Lock()
		defer amu.Unlock()
		attempts++
		return errors.New("poison")
	}))
	require.NoError(t, b.Publish(ctx, "t", "k", []byte("v"), nil))
	require.NoError(t, b.Publish(ctx, "t", "k2", []byte("v2"), nil))
	require.Eventually(t, func() bool {
		rmu.Lock()
		defer rmu.Unlock()
		return reported != nil
	}, 5*time.Second, 5*time.Millisecond)
	amu.Lock()
	require.Equal(t, 3, attempts, "1 + MaxHandlerRetries attempts, then the partition stops; the second record is never skipped ahead")
	amu.Unlock()
	require.NoError(t, b.Close(context.Background()))
}

func TestLoopback_SubscribeRules(t *testing.T) {
	t.Parallel()
	b, err := redpandabus.NewLoopback(config.EnvTest, redpandabus.Options{})
	require.NoError(t, err)
	ctx := context.Background()
	require.Error(t, b.Subscribe(ctx, "", "g", func(context.Context, event.Message) error { return nil }))
	require.Error(t, b.Subscribe(ctx, "t", "g", nil))
	require.Error(t, b.Publish(ctx, "", "k", nil, nil))
	sctx, cancel := context.WithCancel(ctx)
	require.NoError(t, b.Subscribe(sctx, "t", "g", func(context.Context, event.Message) error { return nil }))
	require.Error(t, b.Subscribe(sctx, "t", "g", func(context.Context, event.Message) error { return nil }), "duplicate group")
	cancel()
	require.NoError(t, b.Close(ctx))
}
