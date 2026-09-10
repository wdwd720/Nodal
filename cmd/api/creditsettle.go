package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
)

// Settlement-sweep defaults. The window matches
// cmd/reconciliation-worker's, which is where the same sweep runs when a
// deployment has a worker tier.
const (
	settleInterval    = 15 * time.Minute
	settleBatch       = 100
	defaultSettleWait = 30 * 24 * time.Hour
)

// runCreditSettlement promotes fundings whose reversibility window has closed,
// on a ticker, for as long as this process is up.
//
// # Why the API does this
//
// It should not have to. `SettleDue` belongs to cmd/reconciliation-worker, and
// on a deployment with a worker tier that is where it runs.
//
// The launch tier has no worker tier -- Render charges for one -- so the
// blueprint deploys a single web service and records the sweep as
// "operator-run for now". That turns the money-at-risk ceiling into something
// it was explicitly designed not to be. internal/capacity counts CAPTURED and
// REVERSIBLE as money at risk and deliberately excludes SETTLED, because
// counting SETTLED "would turn the ceiling into a lifetime cumulative cap that
// can only ever rise, so the tier would end up refusing every purchase forever
// -- an outage, not a ceiling."
//
// With nothing settling, that is exactly what the ceiling became: the only
// exits from REVERSIBLE are SettleDue and a won dispute, so the sum was
// monotonically non-decreasing and the deployment would have refused every
// Credit purchase with AT_CAPACITY, permanently, at $2,000 of lifetime sales
// (F-90). The reasoning behind the exclusion was right; the deployment removed
// the component that made it true.
//
// # Why a ticker in the API process is acceptable here
//
// The sweep is idempotent and takes its rows FOR UPDATE SKIP LOCKED, so a
// worker tier added later can run alongside this with no coordination and no
// double effect. It holds no provider call and no lock across a network hop:
// one bounded UPDATE per pass.
//
// A free instance spins down when idle, so this does not run continuously.
// That is tolerable because it only has to run while purchases are happening,
// and purchases require the process to be up. What it must never do is be the
// only reason a number is correct -- it is not: the ceiling refuses when it
// cannot measure, and the settlement window is a risk decision recorded in
// configuration, not an artefact of how often this fires.
func runCreditSettlement(ctx context.Context, database *db.DB, svc *credit.PurchaseService, window time.Duration, log *slog.Logger) {
	if svc == nil || database == nil {
		return
	}
	if window <= 0 {
		window = defaultSettleWait
	}
	log.Info("credit settlement sweep started", "interval", settleInterval, "window", window)
	t := time.NewTicker(settleInterval)
	defer t.Stop()
	for {
		// Once at start as well as on the tick: a process that wakes, serves a
		// purchase and spins down again would otherwise never sweep at all.
		settleOnce(ctx, database, svc, window, log)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func settleOnce(ctx context.Context, database *db.DB, svc *credit.PurchaseService, window time.Duration, log *slog.Logger) {
	var n int
	err := database.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		var serr error
		n, serr = svc.SettleDue(ctx, tx, window, settleBatch)
		return serr
	})
	switch {
	case err != nil && ctx.Err() != nil:
		// Shutdown, not a failure.
	case err != nil:
		log.ErrorContext(ctx, "credit settlement sweep failed", "error", err.Error())
	case n > 0:
		log.InfoContext(ctx, "credit settlement sweep complete", "settled", n, "window", window)
	}
}
