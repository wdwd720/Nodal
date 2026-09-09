//go:build integration

package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/testkit/deps"
)

// The Redis-backed limiter, against a real Redis.
//
// This test lived in the untagged ratelimit_test.go, which put it in a place
// where it could not run: `make unit` invoked it with no Redis and it skipped,
// and the integration job enumerates packages by the build tag, which this
// package did not carry -- so the enumeration never reached it. Between the two
// it was never executed by anything, while REQUIREMENTS_TRACEABILITY.md cited
// "Redis integration tests" as evidence for R-180-1 (F-57).

func TestIntegration_RedisStore(t *testing.T) {
	url := deps.Need(t, "CP_TEST_REDIS_URL", "Redis")
	opts, err := redis.ParseURL(url)
	require.NoError(t, err)
	client := redis.NewClient(opts)
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, client.Ping(ctx).Err())

	store := NewRedisStore(client, "cp:test:rl:"+time.Now().Format("150405.000")+":")
	now := time.Now().UTC()
	l, err := NewLimiter("quote", store, Limit{Requests: 5, Window: 2 * time.Second}, func() time.Time { return now }, false)
	require.NoError(t, err)
	allowed := 0
	for i := 0; i < 8; i++ {
		d, err := l.Allow(ctx, "acct-1")
		require.NoError(t, err)
		if d.Allowed {
			allowed++
		}
	}
	assert.Equal(t, 5, allowed)

	// Two limiters sharing the store see the same counters (multi-replica behavior).
	l2, _ := NewLimiter("quote", store, Limit{Requests: 5, Window: 2 * time.Second}, func() time.Time { return now }, false)
	d, err := l2.Allow(ctx, "acct-1")
	require.NoError(t, err)
	assert.False(t, d.Allowed)

	// And now the thing the deployment actually does. The case above shares one
	// *RedisStore; three Fargate tasks do not share anything in the process.
	// Each opens its own connection pool, wraps it in its own store and builds
	// its own limiter, and the only thing they have in common is the server.
	// That is the arrangement that has to add up to one budget.
	prefix := "cp:test:rl:replicas:" + time.Now().Format("150405.000") + ":"
	var tasks []*Limiter
	var clients []*redis.Client
	for range 3 {
		c := redis.NewClient(opts)
		clients = append(clients, c)
		l, err := NewLimiter("general", NewRedisStore(c, prefix),
			Limit{Requests: 10, Window: time.Minute}, func() time.Time { return now }, false)
		require.NoError(t, err)
		tasks = append(tasks, l)
	}
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}()

	shared := 0
	for i := range 60 {
		d, err := tasks[i%len(tasks)].Allow(ctx, "acct-replicas")
		require.NoError(t, err)
		if d.Allowed {
			shared++
		}
	}
	assert.Equal(t, 10, shared,
		"three replicas over one Redis enforce the configured limit once, not once each")

	if keys, err := client.Keys(ctx, prefix+"*").Result(); err == nil && len(keys) > 0 {
		_ = client.Del(ctx, keys...)
	}

	// Keys carry a TTL so the store never grows without bound.
	keys, err := client.Keys(ctx, "cp:test:rl:*").Result()
	require.NoError(t, err)
	require.NotEmpty(t, keys)
	ttl, err := client.PTTL(ctx, keys[0]).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, time.Duration(0))
	_ = client.Del(ctx, keys...)
}
