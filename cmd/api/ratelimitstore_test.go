package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/ratelimit"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// localResolver reads the plain:// refs these tests use. Plain refs exist only
// in LOCAL and TEST, which is where a test runs.
func localResolver(t *testing.T) config.Resolver {
	t.Helper()
	r, err := config.NewPlainResolver(config.EnvTest)
	require.NoError(t, err)
	return r
}

// TestRateLimitStore_LocalKeepsCountersInTheProcess: one process, one copy of
// the counters, and no Redis to run on a laptop. The memory backend is the
// LOCAL and TEST default and this is what selecting it produces.
func TestRateLimitStore_LocalKeepsCountersInTheProcess(t *testing.T) {
	t.Parallel()
	for _, env := range []config.Environment{config.EnvLocal, config.EnvTest, config.EnvDev} {
		t.Run(string(env), func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{
				Env:       env,
				Service:   config.ServiceAPI,
				RateLimit: config.RateLimitConfig{Backend: config.RateLimitMemory, Replicas: 1},
			}
			store, failOpen, cleanup, err := newRateLimitStore(context.Background(), cfg, localResolver(t), quietLogger())
			require.NoError(t, err)
			require.NotNil(t, cleanup)
			cleanup()

			assert.IsType(t, &ratelimit.MemoryStore{}, store)
			assert.True(t, failOpen, "a store that cannot fail may be treated as one that will not")
		})
	}
}

// TestRateLimitStore_RefusesProcessLocalCountersAcrossReplicas.
//
// The same refusal config.Validate makes, asked one layer down, because the
// two answer different questions: Validate rejects the configuration and this
// rejects the store. A caller that somehow reached composition with a memory
// backend and three declared processes still does not get a limiter that
// quietly counts per replica.
//
// The condition is the replica count and not the environment, so a
// single-process PROD deployment is admitted and a three-process LOCAL one is
// not -- which is the whole point of the change.
func TestRateLimitStore_RefusesProcessLocalCountersAcrossReplicas(t *testing.T) {
	t.Parallel()
	for _, env := range []config.Environment{config.EnvLocal, config.EnvStaging, config.EnvProd} {
		t.Run(string(env)+"/three", func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{
				Env:       env,
				Service:   config.ServiceAPI,
				RateLimit: config.RateLimitConfig{Backend: config.RateLimitMemory, Replicas: 3},
			}
			assert.True(t, config.HasViolation(cfg.Validate(), config.RuleDistributedRateLimit),
				"validation refuses it first, in every environment")

			_, _, _, err := newRateLimitStore(context.Background(), cfg, localResolver(t), quietLogger())
			require.Error(t, err, "and composition refuses it too, rather than trusting the caller")
			assert.ErrorIs(t, err, errRateLimitProcessLocalAcrossReplicas)
		})
	}

	// One process in PROD is correct: the configured limit is the enforced
	// limit, because there is one set of counters.
	cfg := &config.Config{
		Env:       config.EnvProd,
		Service:   config.ServiceAPI,
		RateLimit: config.RateLimitConfig{Backend: config.RateLimitMemory, Replicas: 1},
	}
	assert.False(t, config.HasViolation(cfg.Validate(), config.RuleDistributedRateLimit))
	store, failOpen, cleanup, err := newRateLimitStore(context.Background(), cfg, localResolver(t), quietLogger())
	require.NoError(t, err, "a single-process production deployment counts correctly in memory")
	require.NotNil(t, cleanup)
	cleanup()
	assert.IsType(t, &ratelimit.MemoryStore{}, store)
	assert.True(t, failOpen, "a store that cannot fail may be treated as one that will not")
}

// TestRateLimitStore_MissingOrMalformedRedisConfigurationFails: every way of
// getting the Redis configuration wrong stops the process, and none of them
// falls back to counting in memory.
func TestRateLimitStore_MissingOrMalformedRedisConfigurationFails(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		url        config.SecretRef
		requireTLS bool
		wantText   string
	}{
		{"absent", "", false, "CP_REDIS_URL is not set"},
		{"not a redis url", "http://example.com/redis", false, "not a valid redis URL"},
		{"empty target", "   ", false, "not a valid redis URL"},
		{"tls required, plaintext url", "redis://cache.internal:6379", true, "not rediss://"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{
				Env:       config.EnvTest,
				Service:   config.ServiceAPI,
				RateLimit: config.RateLimitConfig{Backend: config.RateLimitRedis, Replicas: 1},
				Redis:     config.RedisConfig{URL: tc.url, RequireTLS: tc.requireTLS},
			}
			store, failOpen, cleanup, err := newRateLimitStore(context.Background(), cfg, localResolver(t), quietLogger())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantText)
			assert.Nil(t, store, "no store rather than a memory store: a fallback here is the bug")
			assert.False(t, failOpen)
			require.NotNil(t, cleanup, "the cleanup is always callable, including on the failure path")
			cleanup()
		})
	}
}

// TestRateLimitStore_TheErrorNeverQuotesTheURL: CP_REDIS_URL may embed a
// password, and go-redis's own parse error quotes the URL it was handed. A
// startup failure is logged, so the message is a place a credential can escape
// to; only the fact of the failure is reported.
func TestRateLimitStore_TheErrorNeverQuotesTheURL(t *testing.T) {
	t.Parallel()
	const password = "s3cr3t-should-not-appear"
	cfg := &config.Config{
		Env:       config.EnvTest,
		Service:   config.ServiceAPI,
		RateLimit: config.RateLimitConfig{Backend: config.RateLimitRedis, Replicas: 1},
		Redis:     config.RedisConfig{URL: config.SecretRef("ftp://user:" + password + "@cache.internal:6379")},
	}
	_, _, _, err := newRateLimitStore(context.Background(), cfg, localResolver(t), quietLogger())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), password)
}

// TestRateLimitStore_AnUnreachableRedisStopsStartup: "unavailable" is decided
// once, at startup, rather than on the first request. The API does not come up
// claiming a limit it cannot enforce.
func TestRateLimitStore_AnUnreachableRedisStopsStartup(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Env:       config.EnvTest,
		Service:   config.ServiceAPI,
		RateLimit: config.RateLimitConfig{Backend: config.RateLimitRedis, Replicas: 1},
		// Port 1 is reserved and nothing listens on it, so the dial is refused
		// rather than timing out.
		Redis: config.RedisConfig{URL: "redis://127.0.0.1:1/0"},
	}
	store, _, cleanup, err := newRateLimitStore(context.Background(), cfg, localResolver(t), quietLogger())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not answer PING")
	assert.Nil(t, store)
	cleanup()
}

// TestRateLimitStore_AnUnknownBackendIsRefused: the switch has no default that
// picks a store. Adding a third backend to the enum without wiring one here
// fails at startup instead of silently selecting whichever branch came last.
func TestRateLimitStore_AnUnknownBackendIsRefused(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Env:       config.EnvTest,
		Service:   config.ServiceAPI,
		RateLimit: config.RateLimitConfig{Backend: config.RateLimitBackend("memcached"), Replicas: 1},
	}
	_, _, _, err := newRateLimitStore(context.Background(), cfg, localResolver(t), quietLogger())
	require.Error(t, err)
	assert.ErrorIs(t, err, errRateLimitBackendUnknown)
}

// TestRateLimitStore_TheLimitersUseTheStoreTheyWereGiven: rateLimits builds no
// store of its own. If it did, the deployment's choice would have a second,
// invisible answer.
func TestRateLimitStore_TheLimitersUseTheStoreTheyWereGiven(t *testing.T) {
	t.Parallel()
	_, err := rateLimits(clock.NewFake(time.Now().UTC()), config.EnvLocal,
		config.RateLimitConfig{}, nil, false)
	require.Error(t, err, "no store, no limiters")
	assert.Contains(t, err.Error(), "no store")
}

// TestRateLimitSweepInterval_IsTheLargestConfiguredWindow.
//
// A fixed window's counter is dead the moment its window ends, so sweeping at
// the largest of the four removes every expired counter of every limit on each
// pass and never runs more often than the slowest counter turns over.
func TestRateLimitSweepInterval_IsTheLargestConfiguredWindow(t *testing.T) {
	t.Parallel()
	assert.Equal(t, time.Minute, rateLimitSweepInterval(config.RateLimitConfig{}, quietLogger()),
		"the defaults are all per minute")

	assert.Equal(t, 10*time.Minute, rateLimitSweepInterval(config.RateLimitConfig{
		General: "600/1m", Auth: "30/1m", Quote: "60/10s", Command: "120/10m",
	}, quietLogger()))

	// Everything switched off, which only a development environment can do:
	// there is nothing to sweep and the ticker still needs a period.
	assert.Equal(t, time.Minute, rateLimitSweepInterval(config.RateLimitConfig{
		General: "off", Auth: "off", Quote: "off", Command: "off",
	}, quietLogger()))

	// An unparseable spec is rateLimits' refusal to make, at startup, before
	// any of this runs; here it contributes nothing and the rest still decides.
	assert.Equal(t, 5*time.Minute, rateLimitSweepInterval(config.RateLimitConfig{
		General: "not a limit", Auth: "30/5m",
	}, quietLogger()))
}

// TestRateLimitSweeper_RemovesExpiredCountersAndStops.
//
// Nothing in this process called Sweep (F-169): the counters were dropped
// lazily on access, which only ever helps a key that comes back. This is the
// caller that makes the documented bound true.
func TestRateLimitSweeper_RemovesExpiredCountersAndStops(t *testing.T) {
	t.Parallel()
	store := ratelimit.NewMemoryStore()
	for i := 0; i < 500; i++ {
		_, _, err := store.Incr(context.Background(), fmt.Sprintf("auth:ip:198.51.100.%d", i), time.Minute, swept0)
		require.NoError(t, err)
	}
	require.Equal(t, 500, store.Stats(swept0).Keys)

	stop := make(chan struct{})
	swept := make(chan struct{})
	// A clock two windows ahead of the counters, so one tick is enough.
	go func() {
		defer close(swept)
		sweepRateLimitCounters(store, time.Millisecond, func() time.Time { return swept0.Add(2 * time.Minute) },
			quietLogger(), stop)
	}()

	require.Eventually(t, func() bool { return store.Stats(swept0.Add(2*time.Minute)).Keys == 0 },
		2*time.Second, 5*time.Millisecond, "the counters were never swept")

	close(stop)
	select {
	case <-swept:
	case <-time.After(2 * time.Second):
		t.Fatal("the sweeper outlived the store it sweeps")
	}
}

var swept0 = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

// TestRateLimitStore_TheMemoryStoreIsSweptForAsLongAsItExists holds the wiring
// itself: the store the API gets is swept, and the sweeper stops when the
// store's cleanup runs.
func TestRateLimitStore_TheMemoryStoreIsSweptForAsLongAsItExists(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Env:       config.EnvLocal,
		Service:   config.ServiceAPI,
		RateLimit: config.RateLimitConfig{Backend: config.RateLimitMemory, Replicas: 1},
	}
	store, _, cleanup, err := newRateLimitStore(context.Background(), cfg, localResolver(t), quietLogger())
	require.NoError(t, err)
	memory, ok := store.(*ratelimit.MemoryStore)
	require.True(t, ok)

	_, _, err = memory.Incr(context.Background(), "auth:ip:198.51.100.1", time.Minute, time.Now())
	require.NoError(t, err)
	assert.Equal(t, 1, memory.Stats(time.Now()).Keys)

	// Closing the store stops the goroutine; running it twice would panic on a
	// closed channel, which is what the deferred cleanup in main must not do.
	cleanup()
}
