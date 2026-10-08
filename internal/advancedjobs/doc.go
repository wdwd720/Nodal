// Package advancedjobs is the time-driven half of the advanced capabilities
// (ADR-0020; docs/architecture/ADVANCED_FINANCIAL_MODEL.md): the jobs that
// expire reservations, accrue margin interest, mark margin accounts to market,
// work the liquidation queue, run derivative variation margin and funding,
// settle expired derivatives, close due prediction markets, settle equity
// fills at T+1, apply due corporate actions, follow due marketplace installs,
// resolve unknown stable-asset submissions, pay queued redemptions, and
// reconcile the clearing house, prediction-market escrow, order-book journals,
// stable-asset supply, bridge legs and routes and customer transfers -- and
// the scheduler that runs them (cmd/advanced-worker).
//
// # The model
//
// A Job has a stable name, an interval, a per-run batch bound and a Run that
// returns a Report: how many units it examined, how many it changed, how
// many were due but told to wait, and the units it could not process, each
// with a redacted reason (Redact). The Scheduler starts every job that is due
// by an injected clock.Clock -- Step runs every due job and returns when they
// finish, which is what makes it deterministic under clock.Fake -- never
// starts a job while its previous run is still going, runs jobs concurrently
// so a slow or panicking one stops nothing else, turns a panic, a returned
// error or an overrun of the per-run deadline into a failed run, and keeps,
// per job, the last success, the last duration and the consecutive failures
// (Health), which an Observer may export.
//
// # Transactions
//
// A unit of work -- one account, one instrument and interval, one market,
// one book, one leg -- runs in its own READ COMMITTED transaction with a
// small retry budget for serialization failures and deadlocks, so one unit's
// failure, including a refusal the database makes only at COMMIT, never
// rolls back another's. Where a subsystem's operation is itself a driver that
// owns its transactions (margin.Service.ProcessLiquidationQueue,
// stableasset.Service.ReconcileUnknown and ProcessRedemptions,
// bridge.Service.Observe), the job hands it the database and lets it. Where
// a subsystem exports only a batch operation over a queue it selects itself
// (capital.Service.ExpireDue, predmarket.Service.CloseDue,
// orders.Service.CompleteTerminal, equities.Service.SettleDue), the job
// drains it one unit per transaction until it reports nothing left or the
// batch bound is reached, and stops the drain at the first failure: the
// failing unit would be selected first again.
//
// Which units are due is read through the subsystem's own exported reader
// where one exists (instruments.Repository.ListWithTerms,
// marketplace.Service.DueInstalls) and otherwise by a documented read-only
// SELECT in this package. Nothing here writes a subsystem's table: every
// change is made by the subsystem's own exported operation, or -- for the
// order-book proof and the transfers reconciliation, whose subsystems only
// report -- by internal/reconciliation's Repository.
//
// # Kill switches
//
// Kill switches stop new risk. Settlement, liquidation, expiry, interest,
// marks and reconciliation reduce or record risk and must keep running while
// they are active (PART 52 of the base goal). Every subsystem enforces its own
// rule -- margin authorizes these operations as risk-reducing, closing a
// prediction market needs no authorization, a derivative run or settlement
// consults no switch -- and this package adds no switch of its own that could
// stop them, consults none to skip a job, and bypasses none: the one job that
// adds risk, the marketplace's scheduled paper follow, is refused by the
// marketplace itself while its gate or a switch says so.
//
// # What this package must never do
//
//   - Write a subsystem's tables directly, or call anything but the
//     subsystem's exported operations and readers.
//   - Pass a settlement price override to derivatives.Service.SettleExpiry:
//     an expired contract settles only at the price the subsystem derives
//     from its own marks.
//   - Enable LIVE, activate a capability gate, set or release a kill switch,
//     or add a setting that stops a settlement, liquidation, expiry or
//     reconciliation job.
//   - Retry an economic effect whose outcome is UNKNOWN: unknown outcomes are
//     resolved only by the subsystems' own reconcilers.
//   - Let one unit's failure roll back another's, or start a job while its
//     previous run is still going.
//   - Expose an internal error's text in a Report: reasons are redacted, and
//     the full error goes only to the worker's log.
//   - Hold mutable state at package level, read the wall clock except
//     through the injected clock (and the monotonic stopwatch for durations),
//     or use floating point.
package advancedjobs
