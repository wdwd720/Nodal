package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/nodal/controlplane/internal/reconciliation"
)

// Verification-sweep cadence. Matches cmd/reconciliation-worker's
// defaultVerifyInterval, which is where the same pass runs when a deployment
// has a worker tier.
const (
	verifyInterval = 5 * time.Minute
	verifyBatch    = 100
)

// runInternalVerification runs the internal consistency checks on a ticker
// for as long as this process is up: Σ journal entries against
// ledger_balances, Σ active reservations against asset_reservation_totals,
// envelope allocation against its flow -- and then escalates any material
// mismatch that has sat unresolved past the policy budget.
//
// # Why the API does this
//
// The second half of F-118. A destination for alerts (internal/alert) is
// worth nothing if nothing on this deployment ever raises one: `Raise` is
// called by the reconciliation engine, and the engine's periodic passes live
// in cmd/reconciliation-worker, which the launch tier does not run. So a
// ledger_balances row rewritten by a restored backup, a privileged session or
// a bug would have sat there until an operator happened to run the worker's
// `verify` command by hand. The finding's own words: "a ticker in cmd/api the
// way runCreditSettlement already is, plus somewhere for it to send."
//
// This is the ticker. The same reasoning as runCreditSettlement applies and is
// not repeated here.
//
// # What it deliberately does not do
//
// It does not run the execution or funding sweeps (RunPeriodic,
// RunPeriodicFunding). This engine is resolution-shaped on purpose -- no
// attempts repository, no chain observers -- because the worker owns
// detection against the chain and this plane owns the operator's answer to
// it. Bolting observers onto the API process to make those sweeps run here
// would be a different architecture, not a ticker. What runs here is the pass
// that needs only the database, which is also the pass that produces the
// alert this whole finding is about.
//
// # Cost
//
// VerifyBalances sums journal_entries per ledger account in one query. At the
// launch tier's volume that is milliseconds every five minutes; the worker
// runs it at the same cadence on the paid tier. Each drift is recorded in its
// own short transaction under the record's row lock, and upsert converges, so
// a worker added later runs alongside this with no coordination and no
// duplicate record.
func runInternalVerification(ctx context.Context, engine *reconciliation.Engine, log *slog.Logger) {
	if engine == nil {
		return
	}
	log.Info("internal verification sweep started", "interval", verifyInterval)
	t := time.NewTicker(verifyInterval)
	defer t.Stop()
	for {
		// Once at start as well as on the tick, for the same reason as the
		// settlement sweep: a free instance that wakes, serves and spins down
		// would otherwise never verify at all.
		verifyOnce(ctx, engine, log)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// verifyOnce is one pass. A failure is logged and the next tick tries again;
// it never stops the loop, because the alternative to a verification is not a
// stopped service.
func verifyOnce(ctx context.Context, engine *reconciliation.Engine, log *slog.Logger) {
	recs, err := engine.VerifyInternal(ctx)
	switch {
	case err != nil && ctx.Err() != nil:
		return // shutdown, not a failure
	case err != nil:
		log.ErrorContext(ctx, "internal verification failed", "error", err.Error())
	case len(recs) > 0:
		// Every record VerifyInternal returns is a drift: it only records
		// mismatches. Each one has already been raised as SEV1 through the
		// metrics seam; this line is the pass-level summary.
		log.ErrorContext(ctx, "internal verification found drift", "records", len(recs),
			"consequence", "new risk is blocked for the affected scope until an operator resolves the record")
	}

	escalated, err := engine.SweepEscalations(ctx, verifyBatch)
	switch {
	case err != nil && ctx.Err() != nil:
		return
	case err != nil:
		log.ErrorContext(ctx, "escalation sweep failed", "error", err.Error())
	case len(escalated) > 0:
		log.ErrorContext(ctx, "escalation sweep complete", "escalated", len(escalated))
	}
}
