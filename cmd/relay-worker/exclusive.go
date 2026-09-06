package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
)

// relayAdvisoryLockKey is the session-level Postgres advisory lock that makes
// relaying a singleton across processes. The value is the ASCII bytes of
// "nodalrly" read as a big-endian int64; it only has to be stable and
// unlikely to collide with another feature's key.
const relayAdvisoryLockKey int64 = 0x6E6F64616C726C79

// leaseReleaseTimeout bounds giving the lock back during shutdown.
const leaseReleaseTimeout = 5 * time.Second

// Lease is the exclusive right to relay, held as a session-level advisory
// lock on a connection this process owns outright. It backs
// CP_RELAY_WORKER_EXCLUSIVE, which is OFF by default.
//
// It is not a correctness control and must never be described as one.
// Concurrent relaying is safe on its own: SKIP LOCKED gives each row to one
// instance, and since D-036 internal/event's per-row blocked check keeps a
// partition in order even when its rows are split across instances. This
// lease originally existed to work around that defect; with the defect fixed
// it survives only as an operator switch, and the honest reasons to leave it
// off are stronger than the reasons to turn it on:
//
//   - It cannot be airtight anyway. A holder whose lease connection dies
//     mid-pass still overlaps a standby for the length of that pass, because
//     the lock is not taken inside the relay's own claim transaction. A
//     safety property that holds "almost always" is not one to build on, and
//     since D-036 there is nothing left for it to protect.
//   - It trades throughput and, worse, failover latency for nothing. One
//     active relay is a hard ceiling on drain rate, and when the holder dies
//     the lock is released only once Postgres reaps the connection — which
//     for an abruptly lost host is TCP keepalive time, minutes, during which
//     a live standby relays nothing and lag grows. With every instance
//     relaying, the survivors simply carry on. Lag is the metric this worker
//     exists to keep low; a singleton with slow failover is the wrong trade.
//
// What it still buys, and why the switch is kept: a deliberate single
// publisher. An operator who needs exactly one process touching the outbox —
// draining by hand into a fragile downstream, or bisecting a consumer-side
// ordering complaint without a fleet writing underneath them — can set the
// variable and get it, on every instance, without scaling a deployment to
// one. `once` takes the same lease for the same reason.
//
// The lock is session-scoped, so a holder that dies loses it with no lease
// table, heartbeat or clock involved.
type Lease struct {
	conn *pgx.Conn
	key  int64
}

// TryAcquireLease takes the advisory lock without waiting. It returns
// (nil, nil) when another session holds it.
//
// The connection is hijacked out of the pool rather than borrowed: a
// session-level advisory lock outlives the borrow, and a pooled connection
// handed back still holding one would silently wedge every later user of it.
// Owning the connection means Release can always end the session, which
// Postgres treats as releasing the lock no matter what else failed.
func TryAcquireLease(ctx context.Context, database *db.DB, key int64) (*Lease, error) {
	pooled, err := database.Pool().Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("relay-worker: acquire a connection for the relay lease: %w", err)
	}
	conn := pooled.Hijack()
	var held bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&held); err != nil {
		closeConn(ctx, conn)
		return nil, fmt.Errorf("relay-worker: take the relay lease: %w", err)
	}
	if !held {
		closeConn(ctx, conn)
		return nil, nil
	}
	return &Lease{conn: conn, key: key}, nil
}

// Alive reports whether the lock is still held by this session. A dead
// connection means Postgres has already released the lock and a standby may
// have taken it, so the caller must stop relaying.
func (l *Lease) Alive(ctx context.Context) error {
	if l == nil || l.conn == nil {
		return fmt.Errorf("relay-worker: the relay lease is not held")
	}
	return l.conn.Ping(ctx)
}

// Release gives the lock back and ends the session. It is safe to call more
// than once and on a nil Lease, and it runs on a context the shutdown signal
// has already cancelled.
func (l *Lease) Release(ctx context.Context) {
	if l == nil || l.conn == nil {
		return
	}
	conn := l.conn
	l.conn = nil
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaseReleaseTimeout)
	defer cancel()
	// Best effort: closing the connection releases the lock anyway, but
	// unlocking first hands it over without waiting for Postgres to notice
	// the socket is gone.
	_, _ = conn.Exec(rctx, `SELECT pg_advisory_unlock($1)`, l.key)
	closeConn(rctx, conn)
}

func closeConn(ctx context.Context, conn *pgx.Conn) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaseReleaseTimeout)
	defer cancel()
	_ = conn.Close(cctx)
}
