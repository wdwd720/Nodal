package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nodal/controlplane/internal/db"
)

// The one-instance assumption, turned into an assertion.
//
// `CP_HTTP_REPLICAS` is a number an operator types, and `newRateLimitStore`
// refuses the memory backend when it is not 1 — because a per-process counter
// gives each replica its own copy of the budget, so a configured 100 admits
// 100 per instance.
//
// That check compares configuration against configuration. Its own comment said
// so: *"it is an assumption about the world that the process cannot verify:
// nothing here can tell whether the platform really runs one instance."*
// `render.yaml` sets no `numInstances`, so the dashboard is authoritative and a
// second instance can appear without any file in this repository changing
// (F-93).
//
// A session-scoped advisory lock can tell. `pg_try_advisory_lock` succeeds for
// exactly one session across the whole database; a second process asking for the
// same key is refused immediately and without blocking. Held for the life of the
// process, it makes "there is one of me" a fact the process checked rather than
// a number it was given.
//
// ## Why this refuses to start rather than warning
//
// A warning here is the defect this repository keeps recording — a control that
// reports a problem to a log nobody reads while the wrong behaviour continues.
// And the wrong behaviour is not cosmetic: two instances silently double every
// transport rate limit, including the ones in front of the money path and the
// unauthenticated webhook surface.
//
// It is also the safe direction on this tier. One free Render web service that
// refuses to start a second copy loses nothing, because a second copy is not
// something this deployment is meant to have.
//
// ## What it deliberately does not do
//
// Nothing, when the counters are in Redis. A shared store is what makes several
// instances correct, so the lock would be refusing a topology that works.
const singleInstanceLockKey int64 = 0x6E6F64616C617069 // "nodalapi"

// errSecondInstance is returned when another process already holds the lock.
var errSecondInstance = errors.New("another API instance is already running against this database")

// holdSingleInstanceLock takes the advisory lock and returns a release function.
//
// The lock is session-scoped, so it is released when the connection closes even
// if the process dies without calling the returned function. That is the
// property that makes it safe: a crashed instance does not lock the next
// deployment out.
func holdSingleInstanceLock(ctx context.Context, pool *db.DB, log *slog.Logger) (func(), error) {
	conn, err := pool.Pool().Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("single-instance lock: acquire connection: %w", err)
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, singleInstanceLockKey).Scan(&got); err != nil {
		conn.Release()
		return nil, fmt.Errorf("single-instance lock: %w", err)
	}
	if !got {
		conn.Release()
		return nil, fmt.Errorf(
			"%w: process-local rate-limit counters enforce one budget per process, so two instances admit twice the configured limit. "+
				"Either scale this service back to one instance, or set CP_RATELIMIT_BACKEND=redis so the counters are shared",
			errSecondInstance,
		)
	}
	log.Info("single-instance lock held",
		"note", "process-local rate-limit counters are correct because this process verified it is the only one, not because a variable said so")
	return func() {
		// Released explicitly on a clean shutdown so a redeploy does not wait
		// for the old connection to time out.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, singleInstanceLockKey)
		conn.Release()
	}, nil
}
