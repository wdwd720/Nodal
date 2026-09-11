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

// errRateLimitProcessLocalAcrossReplicas is the same refusal config.Validate
// makes, asked one layer down. Both exist because they answer different
// questions -- Validate rejects the configuration, this rejects the store --
// and because the consequence of getting past either is a limiter that reports
// a number it is not enforcing.
var errRateLimitProcessLocalAcrossReplicas = errors.New("process-local rate-limit counters cannot enforce one budget across several processes")

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
		// The replica count, not the environment. One process counting in its
		// own memory enforces exactly the limit it was given; three do not,
		// wherever they run.
		if cfg.RateLimit.Replicas != 1 {
			return nil, false, noop, fmt.Errorf(
				"rate limit: %w: this deployment declares %d processes, so a limit of N would admit %d*N",
				errRateLimitProcessLocalAcrossReplicas, cfg.RateLimit.Replicas, cfg.RateLimit.Replicas,
			)
		}
		// Fail open. A MemoryStore.Incr cannot return an error, so this is a
		// statement about a store that cannot fail rather than a policy for
		// one that can -- and the environments that reach this branch run a
		// single process, where there is no shared budget to protect.
		// Logged at startup because it is an assumption about the world that
		// the process cannot verify: nothing here can tell whether the
		// platform really runs one instance. An operator reading this line
		// alongside a platform that says three has found the bug.
		log.Info("rate limit counters are process-local",
			"backend", string(config.RateLimitMemory),
			"env", string(cfg.Env),
			"declared_replicas", cfg.RateLimit.Replicas,
			"note", "correct for exactly one process; the platform must agree")
		memory := ratelimit.NewMemoryStore()
		// Nothing called Sweep (F-169). The lazy drop inside Incr only ever
		// helps a key that comes back, so a key seen once was held for the
		// life of the process. The store now caps itself as well, but a cap
		// reached is a degradation and a sweep is what keeps it from being
		// reached in the first place.
		stop := make(chan struct{})
		go sweepRateLimitCounters(memory, rateLimitSweepInterval(cfg.RateLimit, log), time.Now, log, stop)
		return memory, true, func() { close(stop) }, nil

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

// rateLimitSweepInterval is the largest window any configured limit uses.
//
// A fixed window's counter is dead the moment the window ends, so sweeping at
// the largest of them removes every expired counter of every limit on each
// pass, and a pass never runs more often than the slowest counter can turn
// over. Sweep decides per entry using the window that entry was written with,
// so a one-minute counter is not kept alive by a ten-minute ticker.
//
// A specification that will not parse is not this function's business to
// refuse -- rateLimits does that, at startup, before any of this runs -- so an
// unparseable one contributes nothing here and the default stands.
func rateLimitSweepInterval(cfg config.RateLimitConfig, log *slog.Logger) time.Duration {
	longest := time.Duration(0)
	for name, spec := range map[string]string{
		envRateLimitGeneral: cfg.General,
		envRateLimitAuth:    cfg.Auth,
		envRateLimitQuote:   cfg.Quote,
		envRateLimitCommand: cfg.Command,
	} {
		limit, enabled, err := ratelimit.ParseLimit(spec, defaultRateLimits[name])
		if err != nil || !enabled {
			continue
		}
		if limit.Window > longest {
			longest = limit.Window
		}
	}
	if longest <= 0 {
		// Every limit switched off, which only a development environment can
		// do (rateLimits refuses it anywhere production-like). There is
		// nothing to count and therefore nothing to sweep, but the ticker
		// still wants a period it will not spin on.
		longest = time.Minute
	}
	log.Info("rate limit counters are swept in this process",
		"interval", longest, "max_keys", ratelimit.DefaultMaxKeys)
	return longest
}

// sweepRateLimitCounters drops expired counters for as long as the process is
// up, and says so when the store had to stop counting keys one at a time.
//
// It returns when stop is closed, which the store's cleanup does -- the same
// lifetime as the store itself, rather than the request context's.
func sweepRateLimitCounters(store *ratelimit.MemoryStore, every time.Duration, now func() time.Time, log *slog.Logger, stop <-chan struct{}) {
	if store == nil || every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	reported := 0
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		at := now()
		removed := store.Sweep(at, every)
		stats := store.Stats(at)
		// Overflow is the thing worth waking somebody for: it means callers
		// shared a budget, which is a refusal somebody did not earn. Reported
		// once per overflow rather than once per pass, because a store that
		// stays saturated would otherwise repeat the line every window.
		if stats.Overflows > reported {
			log.Warn("the rate limiter ran out of counters and shared one budget for a window",
				"overflows", stats.Overflows, "max_keys", stats.Max, "saturated_now", stats.Saturated,
				"consequence", "callers in that window were counted together, so some were refused "+
					"for traffic that was not theirs; the next window counts them separately again")
			reported = stats.Overflows
		}
		if removed > 0 {
			log.Debug("rate limit counters swept", "removed", removed, "held", stats.Keys)
		}
	}
}
