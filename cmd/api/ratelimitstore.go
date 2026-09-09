package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/ratelimit"
)

// redisPingTimeout bounds the one call that decides whether the API may start.
// It is short on purpose: a Redis that cannot answer PING in two seconds is not
// going to serve a rate-limit INCR on the request path either.
const redisPingTimeout = 2 * time.Second

// errRateLimitBackendUnknown is returned for a backend Load parsed but this
// binary has no store for. It cannot happen today -- ParseRateLimitBackend
// accepts exactly the two values below -- and exists so that adding a third
// backend to the enum without wiring it here fails at startup rather than
// silently selecting whichever branch came last.
var errRateLimitBackendUnknown = errors.New("no rate-limit store is wired for this backend")

// errRateLimitProcessLocalInProduction is the same refusal config.Validate
// makes, asked one layer down. Both exist because they answer different
// questions -- Validate rejects the configuration, this rejects the store --
// and because the consequence of getting past either is a limiter that reports
// a number it is not enforcing.
var errRateLimitProcessLocalInProduction = errors.New("process-local rate-limit counters are refused in STAGING/PROD")

// rateLimitStore builds the counter store the transport rate limiter uses, and
// says whether a store failure should allow the request through.
//
// Where the counters live is a property of the deployment rather than of the
// code. One process on a laptop can keep them in memory. Three Fargate tasks
// behind one load balancer cannot: the limit is a budget, and a per-process
// store gives each replica its own copy of it, so a configured 100 admits 300
// and twelve times that at maximum capacity. A number that is not the enforced
// number is worse than a wrong number, because it reads as a right one.
//
// So config.Validate refuses the memory backend for an HTTP binary in
// STAGING/PROD, and this function refuses to start when the distributed
// backend it was told to use does not answer. That is the same decision made
// twice, deliberately: the first catches a deployment that never says where its
// counters go, the second catches one that says Redis and does not have one.
//
// The returned cleanup closes anything opened here and is never nil.
func newRateLimitStore(ctx context.Context, cfg *config.Config, resolver config.Resolver, log *slog.Logger) (store ratelimit.Store, failOpen bool, cleanup func(), err error) {
	noop := func() {}
	switch cfg.RateLimit.Backend {
	case config.RateLimitMemory:
		if cfg.Env.IsProductionLike() {
			return nil, false, noop, fmt.Errorf("rate limit: %w: the API runs as more than one task there, and each would keep its own copy of the budget", errRateLimitProcessLocalInProduction)
		}
		// Fail open. A MemoryStore.Incr cannot return an error, so this is a
		// statement about a store that cannot fail rather than a policy for
		// one that can -- and the environments that reach this branch run a
		// single process, where there is no shared budget to protect.
		log.Info("rate limit counters are process-local",
			"backend", string(config.RateLimitMemory),
			"env", string(cfg.Env),
			"note", "the budget is per replica; STAGING and PROD require redis")
		return ratelimit.NewMemoryStore(), true, noop, nil

	case config.RateLimitRedis:
		client, err := redisClient(ctx, cfg, resolver)
		if err != nil {
			return nil, false, noop, err
		}
		pingCtx, cancel := context.WithTimeout(ctx, redisPingTimeout)
		defer cancel()
		if err := client.Ping(pingCtx).Err(); err != nil {
			// Closed here rather than handed back, because a client that
			// cannot reach its server is not a resource the caller wants to
			// carry: the caller is about to abandon startup.
			_ = client.Close()
			return nil, false, noop, fmt.Errorf("rate limit: redis is the configured backend and did not answer PING: %w", err)
		}
		log.Info("rate limit counters are shared",
			"backend", string(config.RateLimitRedis),
			"prefix", rateLimitKeyPrefix,
			"fail_mode", "closed")
		// Fail CLOSED. A shared store that has stopped answering is not a
		// reason to stop counting: with three or more replicas, failing open
		// removes the only limit that exists, exactly when something unusual
		// is already happening. It is also the difference between "the limiter
		// is degraded" and "the limiter is gone", and only one of those is
		// visible.
		return ratelimit.NewRedisStore(client, rateLimitKeyPrefix), false, func() { _ = client.Close() }, nil

	default:
		return nil, false, noop, fmt.Errorf("rate limit: %q: %w", string(cfg.RateLimit.Backend), errRateLimitBackendUnknown)
	}
}

// rateLimitKeyPrefix namespaces the counters in Redis. It is explicit rather
// than defaulted so that a Redis shared with anything else -- another
// environment, another product -- cannot collide silently.
const rateLimitKeyPrefix = "nodal:rl:"

// redisClient builds the client from CP_REDIS_URL, enforcing the TLS
// requirement the configuration states.
func redisClient(ctx context.Context, cfg *config.Config, resolver config.Resolver) (*redis.Client, error) {
	if cfg.Redis.URL.IsZero() {
		return nil, errors.New("rate limit: redis backend selected and CP_REDIS_URL is not set")
	}
	if resolver == nil {
		return nil, errors.New("rate limit: no secret resolver, so CP_REDIS_URL cannot be read")
	}
	raw, err := resolver.Resolve(ctx, cfg.Redis.URL)
	if err != nil {
		// The ref, not the value: the ref is a name and the value is a
		// credential.
		return nil, fmt.Errorf("rate limit: resolving %s: %w", cfg.Redis.URL.Redacted(), err)
	}
	opts, err := redis.ParseURL(strings.TrimSpace(raw))
	if err != nil {
		// go-redis's parse error quotes the URL it was given, which carries the
		// password. Only the failure is reported.
		return nil, errors.New("rate limit: CP_REDIS_URL is not a valid redis URL")
	}
	if cfg.Redis.RequireTLS {
		// ParseURL sets TLSConfig only for rediss://. A plain redis:// URL
		// under CP_REDIS_REQUIRE_TLS=true is a deployment that believes it is
		// encrypted and is not, which is the one outcome worth refusing rather
		// than upgrading silently.
		if opts.TLSConfig == nil {
			return nil, errors.New("rate limit: CP_REDIS_REQUIRE_TLS is true and CP_REDIS_URL is not rediss://")
		}
		opts.TLSConfig.MinVersion = tls.VersionTLS12
	}
	return redis.NewClient(opts), nil
}
