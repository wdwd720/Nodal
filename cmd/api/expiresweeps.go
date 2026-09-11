package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/verification"
)

// The expiry sweeps (F-170).
//
// Four things in this system carry an `expires_at` and a state that is supposed
// to reach EXPIRED when it passes. Four `ExpireDue` methods were written,
// documented as "meant for a periodic worker", tested, and called by nothing in
// any binary:
//
//	gates.Admin.ExpireDue       a capability window that has closed
//	admin.Service.ExpireDue     a proposed or approved admin action nobody executed
//	capital.Service.ExpireDue   an asset reservation nobody used
//	verification sessions       an attempt that can no longer be decided
//
// # Why a state nobody writes still matters
//
// Three of the four are not safety controls, and saying so is the point: the
// gate checker already treats a closed window as inactive, `admin.checkPending`
// refuses to execute an expired action, and a reservation past its window is
// not counted as buying power. Nothing unsafe happened because these did not
// run. What did happen is that every surface disagreed with its own row -- an
// operator reading APPROVED on a gate that cannot be exercised, a reservation
// holding capital that is not held -- and a disagreement an operator cannot
// explain is how a real incident gets misread.
//
// The fourth is different, and is the reason this exists rather than being
// deferred again. Migration 00762 permits exactly ONE open verification session
// per person. A hosted link that ran out is not open in any sense the person
// can use, and nothing moved its status, so its owner could not start
// verification again -- ever. §20 says "your link expired, start again"; until
// this ran, the product could not honour that sentence.
//
// # Why here
//
// The answer D-046, F-118, D-069, D-084 and D-085 have already given: this
// deployment runs one web service and no workers (render.yaml), so the
// alternative to running periodic work in the API process is not running it
// somewhere better, it is not running it at all.
//
// # Cost, and what bounds it
//
// Four indexed reads every five minutes, each in its own short transaction, on
// a database that on a launch tier finds nothing almost every time. The
// verification and capital passes take an explicit batch, and every pass is
// bounded by expirySweepTimeout so one slow sweep cannot hold a pool connection
// until the next tick. Gate and admin passes are bounded by the shape of their
// data rather than by a LIMIT -- one environment's gates, and the admin actions
// still pending -- which is tens of rows on any deployment of this system;
// neither package is this one's to change.
const (
	expirySweepInterval = 5 * time.Minute
	expirySweepTimeout  = 60 * time.Second
)

// expiryDeps is what one pass needs. Each may be nil, and a nil one is skipped:
// a binary that does not construct a service does not sweep it.
type expiryDeps struct {
	Gates        *gates.Admin
	Admin        *admin.Service
	Capital      *capital.Service
	Verification *verification.Service
}

// runExpirySweeps moves everything past its expiry to EXPIRED for as long as
// this process is up.
func runExpirySweeps(ctx context.Context, database *db.DB, deps expiryDeps, clk clock.Clock, log *slog.Logger) {
	if database == nil {
		return
	}
	log.Info("expiry sweeps started", "interval", expirySweepInterval,
		"sweeps", []string{"capability_gates", "admin_actions", "asset_reservations", "verification_sessions"})
	t := time.NewTicker(expirySweepInterval)
	defer t.Stop()
	for {
		// Once at start as well as on the tick, for the same reason as every
		// other pass here: a free instance that wakes, serves and spins down
		// would otherwise never run it at all.
		expireDueOnce(ctx, database, deps, clk, log)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// expireDueOnce is one pass over all four. Each runs in its own transaction and
// its own error path: one sweep failing must not abandon the other three, which
// are about different rows written by different packages for different reasons.
func expireDueOnce(ctx context.Context, database *db.DB, deps expiryDeps, clk clock.Clock, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, expirySweepTimeout)
	defer cancel()
	now := clk.Now().UTC()

	report := func(what string, n int, err error, consequence string) {
		switch {
		case err != nil && ctx.Err() != nil:
			return // shutdown or the pass's own deadline, not a failure of the data
		case err != nil:
			log.ErrorContext(ctx, "expiry sweep failed", "sweep", what, "error", err.Error(),
				"moved", n, "consequence", consequence)
		case n > 0:
			log.InfoContext(ctx, "expired what was due", "sweep", what, "moved", n)
		}
	}

	if deps.Gates != nil {
		var n int
		err := database.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			due, derr := deps.Gates.ExpireDue(ctx, tx, now)
			n = len(due)
			return derr
		})
		report("capability_gates", n, err,
			"a gate whose window has closed still reads APPROVED or ACTIVE; the checker already refuses it")
	}

	if deps.Admin != nil {
		var n int
		err := database.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			var derr error
			n, derr = deps.Admin.ExpireDue(ctx, tx, now)
			return derr
		})
		report("admin_actions", n, err,
			"an action past its window still reads PROPOSED or APPROVED; executing it is already refused")
	}

	if deps.Capital != nil {
		var n int
		err := database.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			ids, derr := deps.Capital.ExpireDue(ctx, tx, now, capital.DefaultExpireBatch)
			n = len(ids)
			return derr
		})
		report("asset_reservations", n, err,
			"a reservation past its window still reads ACTIVE; it is not counted as buying power either way")
	}

	if deps.Verification != nil {
		// The one with a customer waiting on it: until the session is expired,
		// the one-open-session index refuses its owner a new attempt.
		n, err := deps.Verification.ExpireOverdueSessions(ctx, database, now, verification.SweepBatch)
		report("verification_sessions", n, err,
			"a person whose verification link ran out cannot start another one until this runs")
	}
}
