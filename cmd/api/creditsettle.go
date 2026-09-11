package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/config"
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

// The sandbox tier's settlement window (D-086).
//
// # The problem it solves
//
// CP_CREDIT_SETTLEMENT_WINDOW is 720 hours on STAGING, which is right: it is
// the card chargeback window, and a captured payment IS reversible for that
// long. Only SETTLED value is payout-eligible under any policy in this build --
// including the sandbox one -- so on the tier whose entire purpose is to
// rehearse the product, nothing anybody bought could ever be withdrawn. The
// withdrawal journey ADR-0023 exists to let a sandbox tier exercise was
// unreachable past its first step, not by a refusal anybody could see but by a
// clock that had not run yet.
//
// # Why two minutes, and why not configuration
//
// Two minutes is long enough to observe REVERSIBLE -- the state matters and a
// rehearsal that skipped it would rehearse the wrong thing -- and short enough
// that a person testing the product does not go and do something else.
//
// It is compiled in and keyed off cfg.SandboxTier() rather than being a second
// environment variable, for the reason the config table already gives for the
// real window: a risk determination read straight from the environment is
// outside scripts/configcheck and outside the configuration hash. A sandbox
// window is not a risk determination at all -- no card was charged and nothing
// can be charged back -- so it is a property of the tier, stated once here,
// where it is refused in PROD by construction.
//
// # Why the sweep also ticks faster there
//
// A two-minute window swept every fifteen minutes is a fifteen-minute window.
const (
	sandboxSettleWait     = 2 * time.Minute
	sandboxSettleInterval = 20 * time.Second
)

// creditSettlement returns the window and the cadence this deployment sweeps
// with.
//
// PROD never reaches the sandbox branch: cfg.SandboxTier() is false there by
// construction (config.Validate refuses the declaration in PROD, and
// SandboxTier checks the environment again on its own account). The second
// condition is belt and braces for a Config nobody validated, and it is the
// same pair every other sandbox affordance in this binary is guarded by.
func creditSettlement(cfg *config.Config) (window, interval time.Duration) {
	if cfg != nil && cfg.SandboxTier() && cfg.Env != config.EnvProd {
		return sandboxSettleWait, sandboxSettleInterval
	}
	window = defaultSettleWait
	if cfg != nil && cfg.Credit.SettlementWindow > 0 {
		window = cfg.Credit.SettlementWindow
	}
	return window, settleInterval
}

// runCreditSettlement promotes fundings whose reversibility window has closed,
// asks the provider about the ones still in flight, and ends the ones nobody
// completed -- on a ticker, for as long as this process is up.
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
//
// # Why the other two passes are here as well
//
// For the same reason, discovered twice more.
//
// RECONCILIATION had exactly one caller in the repository --
// cmd/reconciliation-worker -- and render.yaml declares two services, both
// `type: web`, neither of them that worker. The window it closes is real:
// StartPurchase commits the provider reference in phase 3, AFTER the provider
// call returns, and a provider can deliver payment_intent.succeeded before that
// commit lands. An event naming a reference no funding holds is Ignored, the
// inbox records the event id as processed, and the provider's redelivery of the
// same id is answered Duplicate and never reaches Dispatch again. So on the
// deployed tier the card was charged and no Credits were ever minted, and the
// one thing that would have found it did not run (F-154).
//
// EXPIRY had no caller anywhere, because it did not exist. Every state before
// capture counts against the money-at-risk ceiling and nothing moved a funding
// out of one: SettleDue selects REVERSIBLE, and an abandoned checkout's
// provider reports "still waiting" forever. Two abandoned $1,000 checkouts
// exhausted the blueprint's $2,000 ceiling and every honest purchase after them
// was refused AT_CAPACITY with no remedy (F-153).
//
// Both are idempotent, take their rows FOR UPDATE SKIP LOCKED and hold no
// provider call inside a transaction, so a worker tier added later runs them
// alongside this with no coordination -- the same property that makes the
// settlement sweep safe here.
func runCreditSettlement(ctx context.Context, database *db.DB, svc *credit.PurchaseService, credits *credit.Service, cfg *config.Config, log *slog.Logger) {
	if svc == nil || database == nil {
		return
	}
	window, interval := creditSettlement(cfg)
	if cfg != nil && cfg.SandboxTier() {
		log.Warn("SANDBOX settlement window: a captured payment settles in minutes, not in a chargeback window",
			"window", window, "interval", interval, "environment", string(cfg.Env),
			"consequence", "only SETTLED value is payout-eligible, so this is what makes a rehearsal withdrawal reachable; PROD keeps its real window")
	}
	log.Info("credit settlement sweep started", "interval", interval, "window", window)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		// Once at start as well as on the tick: a process that wakes, serves a
		// purchase and spins down again would otherwise never sweep at all.
		settleOnce(ctx, database, svc, window, log)
		settleDerivedOnce(ctx, database, credits, log)
		reconcileOnce(ctx, database, svc, log)
		expireOnce(ctx, database, svc, log)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// reconcileOnce asks the provider what became of every purchase that has been
// in flight longer than a customer would wait, one transaction per purchase
// with the provider call outside it.
func reconcileOnce(ctx context.Context, database *db.DB, svc *credit.PurchaseService, log *slog.Logger) {
	if svc == nil || database == nil {
		return
	}
	n, err := svc.ReconcileDue(ctx, database, credit.DefaultReconcileAfter, settleBatch)
	switch {
	case err != nil && ctx.Err() != nil:
		// Shutdown, not a failure.
	case err != nil:
		log.ErrorContext(ctx, "credit purchase reconciliation failed", "error", err.Error())
	case n > 0:
		log.InfoContext(ctx, "credit purchase reconciliation complete", "checked", n)
	}
}

// expireOnce ends purchases nobody completed, so an abandoned checkout stops
// holding money-at-risk headroom for the life of the deployment.
func expireOnce(ctx context.Context, database *db.DB, svc *credit.PurchaseService, log *slog.Logger) {
	if svc == nil || database == nil {
		return
	}
	n, err := svc.ExpireInFlight(ctx, database, credit.DefaultInFlightLifetime, settleBatch)
	switch {
	case err != nil && ctx.Err() != nil:
		// Shutdown, not a failure.
	case err != nil:
		log.ErrorContext(ctx, "credit purchase expiry failed", "error", err.Error())
	case n > 0:
		log.InfoContext(ctx, "credit purchase expiry complete", "expired", n,
			"lifetime", credit.DefaultInFlightLifetime)
	}
}

// settleDerivedOnce moves the lots that were minted out of other lots: trading
// proceeds, creator earnings, marketplace proceeds and the fees on them.
//
// It runs immediately after settleOnce and in the same ticker, because it reads
// what that pass just wrote: SettleDue promotes a funding's own lot to SETTLED,
// and this promotes everything that was derived from it once EVERY parent has
// reached a payout-eligible finality. Running it first would make every earning
// one sweep late for no reason.
//
// Before D-124 there was nothing to run. An earning has no `credit_fundings`
// row, SettleFunding is keyed on one, and PayoutEligible admits only SETTLED and
// UNFUNDED -- so five of the six origins the sandbox payout policy marks
// withdrawable could never be withdrawn on any deployment (F-230).
func settleDerivedOnce(ctx context.Context, database *db.DB, credits *credit.Service, log *slog.Logger) {
	if credits == nil || database == nil {
		return
	}
	var res credit.SettleDerivedResult
	err := database.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		var serr error
		res, serr = credits.SettleDerived(ctx, tx, settleBatch)
		return serr
	})
	switch {
	case err != nil && ctx.Err() != nil:
		// Shutdown, not a failure.
	case err != nil:
		log.ErrorContext(ctx, "derived credit settlement sweep failed", "error", err.Error())
	case res.Promoted > 0 || res.Frozen > 0:
		log.InfoContext(ctx, "derived credit settlement sweep complete",
			"promoted", res.Promoted, "frozen", res.Frozen)
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
