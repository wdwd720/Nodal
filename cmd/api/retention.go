package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/identity"
)

// The login-attempt purge, running where the service actually runs.
//
// `login_attempts` holds a plaintext OIDC nonce and PKCE verifier for each
// attempt. `identity.PurgeLoginAttempts` deletes the expired ones and is called
// from exactly one place: `cmd/audit-worker`. This deployment is a single Render
// web service -- no workers, no cron -- so that purge has never run, and those
// secrets are kept forever on a database whose ceiling halts every financial
// action when it fills (F-105).
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
)

// runLoginAttemptRetention purges expired login attempts on a ticker.
//
// It takes its own pool. cp_app deliberately holds no DELETE on
// login_attempts -- an attacker with the application credential must not be
// able to erase the record of the logins they attempted -- so the purge needs
// cp_ops, and the pool is opened here rather than shared.
//
// When CP_DATABASE_OPS_URL is unset it does not start, and says so at WARN
// naming exactly what will not happen. It does NOT refuse to boot: cmd/audit-worker
// refuses because purging is why that binary was deployed, and this is a web
// service whose job is serving requests. But it must not be silent either --
// "a control that reports success having run nothing" is the defect class this
// repository keeps finding, and a retention pass that quietly does not exist is
// the same shape.
func runLoginAttemptRetention(ctx context.Context, cfg *config.Config, lookup func(string) (string, bool), log *slog.Logger) {
	days := cfg.Retention.LoginAttemptDays
	if days <= 0 {
		log.Warn("login attempt retention is not running: CP_RETENTION_LOGIN_ATTEMPT_DAYS is not positive",
			"consequence", "plaintext OIDC nonces and PKCE verifiers are kept indefinitely")
		return
	}
	if cfg.Database.OpsURL == "" {
		log.Warn("login attempt retention is not running: CP_DATABASE_OPS_URL is not set",
			"why", "cp_app holds no DELETE on login_attempts, by design",
			"consequence", "plaintext OIDC nonces and PKCE verifiers are kept indefinitely")
		return
	}
	url, err := config.NewResolver(cfg.Env, lookup).Resolve(ctx, cfg.Database.OpsURL)
	if err != nil {
		log.Error("login attempt retention is not running: could not resolve CP_DATABASE_OPS_URL", "error", err.Error())
		return
	}
	pool, err := db.Open(ctx, db.Config{
		URL: url, AppName: "api-ops", RequireTLS: cfg.Database.RequireTLS,
		MaxConns: 1, MinConns: 0,
		StatementTimeout: cfg.Database.StatementTimeout, LockTimeout: cfg.Database.LockTimeout,
	})
	if err != nil {
		log.Error("login attempt retention is not running: could not open the ops pool", "error", err.Error())
		return
	}
	defer pool.Close()

	retention := time.Duration(days) * 24 * time.Hour
	log.Info("login attempt retention started", "interval", retentionInterval, "retention", retention)

	select {
	case <-ctx.Done():
		return
	case <-time.After(retentionStartupDelay):
	}
	t := time.NewTicker(retentionInterval)
	defer t.Stop()
	for {
		purgeLoginAttemptsOnce(ctx, pool, retention, log)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
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
