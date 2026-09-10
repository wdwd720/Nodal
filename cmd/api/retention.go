package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/nodal/controlplane/internal/auth/pgstore"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/identity"
)

// The periodic work that needs the operations credential, running where the
// service actually runs.
//
// Two passes share one ticker and one pool:
//
//   - the login-attempt purge. `login_attempts` holds a plaintext OIDC nonce and
//     PKCE verifier for each attempt. `identity.PurgeLoginAttempts` deletes the
//     expired ones and is called from exactly one other place,
//     `cmd/audit-worker`.
//   - security_events partition management (00740). Creating next month's
//     partition, and dropping months past the retention period once one is
//     chosen.
//
// This deployment is a single Render web service -- no workers, no cron -- so
// neither has ever run, and both feed the same problem: a database whose ceiling
// halts every financial action when it fills (F-105).
//
// This is the same answer F-90 reached for settlement: when the deployment has
// one process, the periodic work belongs in it. `runCreditSettlement` is the
// precedent and this deliberately mirrors it, including sweeping once at
// startup, because a process that wakes, serves a login and spins down again
// would otherwise never sweep at all.
const (
	retentionInterval = time.Hour
	// A purge that deletes everything it can see in one statement would hold a
	// lock on a table the login path writes to. The delete is bounded by
	// expires_at rather than by a row count, and an hour's cadence keeps each
	// pass small after the first.
	retentionStartupDelay = 30 * time.Second
	// Twelve months of runway. The pass runs hourly, so one failed pass costs
	// nothing; what this number buys is a deployment that can be down for a
	// long time and still write into a real partition when it comes back.
	securityEventMonthsAhead = 12
)

// runOpsRetention runs the operations-credentialed periodic work on a ticker.
//
// It takes its own pool. cp_app deliberately holds no DELETE on
// login_attempts -- an attacker with the application credential must not be
// able to erase the record of the logins they attempted -- and holds no EXECUTE
// on the partition functions either, so both passes need cp_ops and the pool is
// opened here rather than shared.
//
// When CP_DATABASE_OPS_URL is unset it does not start, and says so at WARN
// naming exactly what will not happen. It does NOT refuse to boot: cmd/audit-worker
// refuses because purging is why that binary was deployed, and this is a web
// service whose job is serving requests. But it must not be silent either --
// "a control that reports success having run nothing" is the defect class this
// repository keeps finding, and a retention pass that quietly does not exist is
// the same shape.
//
// ## Why the two passes do not gate each other
//
// This function used to return early when CP_RETENTION_LOGIN_ATTEMPT_DAYS was
// not positive, which was right when the purge was the only thing here. Adding
// a second pass under that same early return would have made one unconfigured
// value silently disable an unrelated control -- and the disabled one would have
// been partition CREATION, whose failure mode is rows landing in the default
// partition, which retention can then never drop. A coupling like that is the
// shape of half this register.
//
// So the pool is what CP_DATABASE_OPS_URL gates, and each pass decides for
// itself whether it has what it needs.
func runOpsRetention(ctx context.Context, cfg *config.Config, lookup func(string) (string, bool), log *slog.Logger) {
	if cfg.Database.OpsURL == "" {
		log.Warn("operations retention is not running: CP_DATABASE_OPS_URL is not set",
			"why", "cp_app holds no DELETE on login_attempts and no EXECUTE on the partition functions, by design",
			"consequence", "plaintext OIDC nonces and PKCE verifiers are kept indefinitely, and security_events partitions are neither created nor pruned")
		return
	}
	// Said before the pool is opened, deliberately. An operator learns about a
	// misconfigured retention window without needing the database to be
	// reachable, and a WARN that only appears once a connection succeeds is a
	// WARN that does not appear on the day it matters most.
	days := cfg.Retention.LoginAttemptDays
	if days <= 0 {
		log.Warn("login attempt purging is off: CP_RETENTION_LOGIN_ATTEMPT_DAYS is not positive",
			"consequence", "plaintext OIDC nonces and PKCE verifiers, and every expired session, are kept indefinitely")
	}

	url, err := config.NewResolver(cfg.Env, lookup).Resolve(ctx, cfg.Database.OpsURL)
	if err != nil {
		log.Error("operations retention is not running: could not resolve CP_DATABASE_OPS_URL", "error", err.Error())
		return
	}
	pool, err := db.Open(ctx, db.Config{
		URL: url, AppName: "api-ops", RequireTLS: cfg.Database.RequireTLS,
		MaxConns: 1, MinConns: 0,
		StatementTimeout: cfg.Database.StatementTimeout, LockTimeout: cfg.Database.LockTimeout,
	})
	if err != nil {
		log.Error("operations retention is not running: could not open the ops pool", "error", err.Error())
		return
	}
	defer pool.Close()

	retention := time.Duration(days) * 24 * time.Hour
	log.Info("operations retention started",
		"interval", retentionInterval,
		"login_attempt_retention", retention,
		"security_event_retention_days", cfg.Retention.SecurityEventDays)

	select {
	case <-ctx.Done():
		return
	case <-time.After(retentionStartupDelay):
	}
	t := time.NewTicker(retentionInterval)
	defer t.Stop()
	for {
		if days > 0 {
			purgeLoginAttemptsOnce(ctx, pool, retention, log)
			purgeSessionsOnce(ctx, pool, retention, log)
		}
		securityEventPartitionsOnce(ctx, pool, cfg.Retention.SecurityEventDays, log)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// securityEventPartitionsOnce keeps security_events' partitions ahead of the
// clock and, when a retention period has been chosen, drops the months past it.
//
// Both halves go through SECURITY DEFINER functions (00740) rather than through
// DDL this pool could issue, because cp_ops holds no DDL and should not. The
// function is the audited boundary: it can create a partition of this one table
// and drop a whole month of this one table, and nothing else.
//
// The two halves are deliberately not conditional on each other. Creating
// runway is not destructive and runs whether or not a retention period exists,
// because a deployment that never prunes still needs somewhere for next month's
// rows to land -- and if they land in the default partition instead, they can
// never be pruned later even once someone does choose a period.
func securityEventPartitionsOnce(ctx context.Context, pool *db.DB, retainDays int, log *slog.Logger) {
	var created int
	if err := pool.QueryRow(ctx,
		`SELECT cp_security_events_ensure_partitions($1)`, securityEventMonthsAhead,
	).Scan(&created); err != nil {
		log.ErrorContext(ctx, "security event partition creation failed", "error", err.Error(),
			"consequence", "new rows will land in the default partition, which retention can never drop")
		return
	}
	if created > 0 {
		log.InfoContext(ctx, "security event partitions created", "count", created, "months_ahead", securityEventMonthsAhead)
	}

	if retainDays <= 0 {
		// Said once per pass at DEBUG rather than WARN: unlike the login-attempt
		// purge, this is not a misconfiguration. It is the documented default,
		// and ADR-0020 leaves the period open on purpose. What must not happen
		// is silence, and the capacity guard is the thing that will notice.
		log.DebugContext(ctx, "security event pruning is off: CP_RETENTION_SECURITY_EVENT_DAYS is 0",
			"consequence", "the security trail grows until the database ceiling refuses financial actions")
		return
	}
	rows, err := pool.Query(ctx, `SELECT partition_name, upper_bound FROM cp_security_events_drop_expired($1)`, retainDays)
	if err != nil {
		log.ErrorContext(ctx, "security event partition drop failed", "error", err.Error(), "retain_days", retainDays)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var ub time.Time
		if err := rows.Scan(&name, &ub); err != nil {
			log.ErrorContext(ctx, "security event partition drop: unreadable result", "error", err.Error())
			return
		}
		// One line per dropped month, at WARN. Dropping a security audit trail
		// is a thing someone should be able to find afterwards, and on this
		// deployment the log is the only place it could be found (F-118).
		log.WarnContext(ctx, "security event partition dropped", "partition", name, "upper_bound", ub.Format(time.DateOnly), "retain_days", retainDays)
	}
	if err := rows.Err(); err != nil {
		log.ErrorContext(ctx, "security event partition drop: result iteration failed", "error", err.Error())
	}
}

func purgeLoginAttemptsOnce(ctx context.Context, pool *db.DB, retention time.Duration, log *slog.Logger) {
	n, err := identity.PurgeLoginAttempts(ctx, pool, time.Now(), retention)
	if err != nil {
		// Logged, never fatal: a failed purge is a reason to look, not a reason
		// to stop serving. It is at ERROR because nothing else reports it --
		// this deployment has no alerting (F-118).
		log.ErrorContext(ctx, "login attempt purge failed", "error", err.Error())
		return
	}
	if n > 0 {
		log.InfoContext(ctx, "login attempts purged", "rows", n, "retention", retention)
	}
}

// purgeSessionsOnce deletes sessions whose expiry passed more than the
// login-attempt window ago.
//
// The job existed -- migration 00011 granted cp_ops DELETE on sessions and
// called the purge "an operations job (auth/pgstore.PurgeExpired)" -- and
// nothing on any tier called it (F-133). So every session ever issued, with
// its token hash, roles, IP and user agent, was kept forever, on a database
// whose ceiling halts every financial action when it fills.
//
// The login-attempt window is reused rather than a new variable added: both
// are transient authentication rows whose only afterlife is forensic, and one
// number for "how long after expiry does an auth row survive" is easier to
// reason about than two. Under 00754, cp_ops holds SELECT on expires_at and
// nothing else on this table, which is exactly what the DELETE filters by.
func purgeSessionsOnce(ctx context.Context, pool *db.DB, retention time.Duration, log *slog.Logger) {
	n, err := pgstore.New().PurgeExpired(ctx, pool, retention)
	if err != nil {
		log.ErrorContext(ctx, "session purge failed", "error", err.Error())
		return
	}
	if n > 0 {
		log.InfoContext(ctx, "expired sessions purged", "count", n, "retention", retention)
	}
}
