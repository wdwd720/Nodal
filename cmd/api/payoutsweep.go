package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/security"
)

// The payout submission and settlement passes (D-085).
//
// # What was missing
//
// payout.Service.Submit had no caller anywhere in cmd/, and neither did
// Reconcile. Everything up to the reservation worked: a person quoted, asked,
// and watched their Credits leave their spendable balance into PAYOUT_RESERVED.
// Then nothing. The request sat in VERIFIED forever, the provider was never
// told, and the sandbox provider's ten-second settlement -- built so the
// journey could be rehearsed end to end -- was never observed by anything.
//
// Value reserved with no path out is the same trap F-52 found on reconciliation
// records and F-90 found on credit settlement: a control that takes something
// and has nowhere to put it.
//
// # Why here
//
// The same answer as runCreditSettlement (D-046), runInternalVerification
// (F-118) and the notification follower (D-069): this deployment runs one web
// service and no workers (render.yaml), so the alternative to running it here
// is not running it somewhere better, it is not running it at all.
//
// # Why fifteen seconds and not five minutes
//
// A person is standing in front of this one. The other passes reconcile
// invariants nobody is waiting on; this is the step between "I asked for my
// money" and "it is on its way", and the sandbox provider settles after ten
// seconds precisely so the whole journey fits inside a browser session. It is
// the notification follower's cadence, for the same reason.
//
// # Idempotence and the crash between submit and record
//
// Neither pass owns any of the correctness here, deliberately.
//
// Submit writes and COMMITS the provider idempotency key before it calls the
// provider, so a crash anywhere after that leaves a key on disk the provider
// can be asked about -- and Submit's own re-entry branch returns without
// calling again for a request already in SUBMITTED, PROVIDER_PENDING or
// PAYOUT_STATUS_UNKNOWN. The provider is idempotent on that key. So a pass that
// runs twice over the same request submits once, and this pass could run every
// second without changing the outcome.
//
// The advisory lock below is therefore about load and not about safety: it
// stops two instances (or a worker tier added later) doing the same provider
// round trip at the same time, and a loser skips rather than queues.
const (
	payoutSweepInterval = 15 * time.Second
	// payoutSweepBatch bounds one pass. A backlog drains over several passes
	// rather than in one long run holding a pool connection while it waits on
	// an external call per request.
	payoutSweepBatch = 25
	// payoutSettleGrace is how long a submitted payout is left alone before the
	// settlement pass asks the provider about it. Asking in the same second it
	// was accepted is a round trip whose answer is always "still pending".
	payoutSettleGrace = 5 * time.Second
	// payoutSweepLockKey namespaces the advisory lock each pass takes.
	payoutSweepLockKey int64 = 0x7061796f // "payo"
)

// runPayoutSweeps hands reserved payouts to the provider and applies the
// provider's answers, for as long as this process is up.
func runPayoutSweeps(ctx context.Context, database *db.DB, svc *payout.Service, clk clock.Clock, log *slog.Logger) {
	if database == nil || svc == nil {
		return
	}
	names := svc.ProviderNames()
	if len(names) != 1 {
		// Zero is the honest state of a deployment with no conversion contract
		// (BLOCKERS B-PAYOUT-PROVIDER) and every payout answers
		// PROVIDER_UNAVAILABLE long before reaching this. More than one is a
		// deployment that has not decided which provider a payout belongs to,
		// and a sweep that guessed would be choosing for it.
		log.Info("payout sweeps not started", "providers", names,
			"consequence", "no payout is submitted by this process; a deployment runs one payout slot")
		return
	}
	provider := names[0]
	log.Info("payout submission and settlement sweeps started",
		"interval", payoutSweepInterval, "provider", provider, "batch", payoutSweepBatch)
	t := time.NewTicker(payoutSweepInterval)
	defer t.Stop()
	for {
		// Once at start as well as on the tick: a free instance that wakes,
		// serves and spins down would otherwise never drain the backlog its own
		// downtime created, and the backlog here is somebody's money.
		payoutSweepOnce(ctx, database, svc, provider, clk, log)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// payoutSweepOnce is one pass: submit what is reserved, then ask the provider
// about what is in flight.
//
// Submission runs first so a request created since the last tick reaches the
// provider this tick rather than next, and the settlement pass then picks it up
// on a later one once the grace period has passed.
func payoutSweepOnce(ctx context.Context, database *db.DB, svc *payout.Service, provider string, clk clock.Clock, log *slog.Logger) {
	// The sweeps act as the system: they move other people's payouts, which no
	// customer principal may do and no operator should be asked to do on a
	// timer.
	ctx = security.WithPrincipal(ctx, security.Principal{
		SubjectID: "payout-sweep", ActorType: security.ActorSystem, AuthTime: clk.Now(),
	})
	submitReservedPayouts(ctx, database, svc, provider, clk, log)
	settleInFlightPayouts(ctx, database, svc, clk, log)
}

// submitReservedPayouts hands every reserved request to the provider.
func submitReservedPayouts(ctx context.Context, database *db.DB, svc *payout.Service, provider string, clk clock.Clock, log *slog.Logger) {
	locked, release, err := claimSweep(ctx, database, "payout_submit")
	if err != nil {
		log.ErrorContext(ctx, "payout submission sweep could not claim its pass", "error", err.Error())
		return
	}
	if !locked {
		return
	}
	defer release()

	pending, err := svc.AwaitingSubmission(ctx, database, clk.Now().UTC(), payoutSweepBatch)
	if err != nil {
		log.ErrorContext(ctx, "payout submission sweep failed to read", "error", err.Error())
		return
	}
	submitted, held, skipped := 0, 0, 0
	heldReason, skippedReason := "", ""
	for _, r := range pending {
		if ctx.Err() != nil {
			return
		}
		out, serr := svc.Submit(ctx, database, r.ID, provider)
		switch {
		case serr == nil:
			submitted++
			log.InfoContext(ctx, "payout submitted", "payout_id", r.ID.String(), "state", string(out.State))
		case errs.CodeOf(serr) == errs.CodeInvalidStateTransition:
			// Not submittable right now. Two causes, and they look the same from
			// here: something moved it between the read and the call (a cancel,
			// or another instance's pass), or the destination it names has
			// stopped being usable because the holder removed it (F-263).
			//
			// Neither is this pass's problem and neither stops the rest. The
			// first is transient; the second is not, and it is the reason this
			// branch counts and reports instead of returning silently. A request
			// pointing at a removed destination holds the person's value
			// reserved until they cancel it, and a sweep that says nothing about
			// it every fifteen seconds for ever is how that goes unnoticed. The
			// person was told at the moment they removed the destination -- the
			// disable response names the requests it stranded -- and this is the
			// operator's half of the same fact.
			skipped++
			if skippedReason == "" {
				skippedReason = serr.Error()
			}
			continue
		case errs.CodeOf(serr) == errs.CodeKillSwitchActive,
			errs.CodeOf(serr) == errs.CodeAccountFrozen,
			errs.CodeOf(serr) == errs.CodeForbidden:
			// An operator asked for this: a kill switch is active, or the
			// account is not in a state that may take value out (D-092). It is
			// the requested state of the system, not a failure of this pass, and
			// an ERROR per payout every fifteen seconds is the kind of alarm an
			// operator learns to ignore. Counted and reported once below; the
			// Credits stay reserved and the pass after the release submits them.
			held++
			if heldReason == "" {
				heldReason = serr.Error()
			}
			continue
		default:
			// One payout's failure does not abandon the rest: each is an
			// independent person's money, and a pass that stopped at the first
			// problem would leave every later one reserved and unsent.
			log.ErrorContext(ctx, "payout could not be submitted",
				"payout_id", r.ID.String(), "error", serr.Error(),
				"consequence", "the Credits stay reserved and the next pass tries again")
		}
	}
	if held > 0 {
		log.WarnContext(ctx, "payouts were held by an emergency control or an account status",
			"held", held, "first_reason", heldReason,
			"consequence", "the Credits stay reserved and the pass after the release submits them")
	}
	if skipped > 0 {
		// INFO rather than WARN: a request moved between the read and the call
		// is ordinary, and an alarm for it is one an operator learns to ignore.
		// The reason is carried so the persistent case -- a destination the
		// holder removed -- is readable rather than inferred from a count.
		log.InfoContext(ctx, "payouts were not submittable on this pass",
			"skipped", skipped, "first_reason", skippedReason,
			"consequence", "the Credits stay reserved; a request whose destination was removed stays "+
				"there until its holder cancels it or points it at a usable one")
	}
	if submitted > 0 {
		log.InfoContext(ctx, "payout submission sweep complete", "submitted", submitted)
	}
}

// settleInFlightPayouts asks the provider what happened to everything in flight
// and applies the answer through the service's own transitions.
//
// SETTLED, FAILED and "the provider does not know" are all outcomes
// payout.Reconcile already defines; nothing is decided here. A provider that
// will not answer leaves the request exactly as it was, which is why
// PAYOUT_STATUS_UNKNOWN exists: in flight, never failed, until somebody says
// otherwise.
func settleInFlightPayouts(ctx context.Context, database *db.DB, svc *payout.Service, clk clock.Clock, log *slog.Logger) {
	locked, release, err := claimSweep(ctx, database, "payout_settle")
	if err != nil {
		log.ErrorContext(ctx, "payout settlement sweep could not claim its pass", "error", err.Error())
		return
	}
	if !locked {
		return
	}
	defer release()

	open, err := svc.OpenRequests(ctx, database, clk.Now().UTC().Add(-payoutSettleGrace), payoutSweepBatch)
	if err != nil {
		log.ErrorContext(ctx, "payout settlement sweep failed to read", "error", err.Error())
		return
	}
	moved := 0
	for _, r := range open {
		if ctx.Err() != nil {
			return
		}
		out, rerr := svc.Reconcile(ctx, database, r.ID)
		switch {
		case rerr != nil:
			// RECONCILIATION_REQUIRED is the provider refusing to say, which is
			// the state this pass exists to keep re-asking about rather than
			// resolve. It is logged at info, not error: a payout the provider
			// has not decided yet is normal and an alarm on it would be noise.
			if errs.CodeOf(rerr) == errs.CodeReconciliationRequired {
				log.InfoContext(ctx, "payout still unresolved at the provider",
					"payout_id", r.ID.String(), "state", string(r.State))
				continue
			}
			log.ErrorContext(ctx, "payout could not be reconciled",
				"payout_id", r.ID.String(), "error", rerr.Error())
		case out.State != r.State:
			moved++
			log.InfoContext(ctx, "payout moved by its provider",
				"payout_id", r.ID.String(), "from", string(r.State), "to", string(out.State))
		}
	}
	if moved > 0 {
		log.InfoContext(ctx, "payout settlement sweep complete", "moved", moved)
	}
}

// claimSweep takes a session-scoped try-advisory lock so two instances never
// run the same pass at once, and returns the function that releases it.
//
// Session-scoped rather than transaction-scoped because the pass makes external
// calls and must not hold a transaction open across them. A loser skips the
// pass; the next tick tries again. Correctness does not depend on this -- see
// the file comment -- so a lock that could not be taken is not an error.
func claimSweep(ctx context.Context, database *db.DB, name string) (bool, func(), error) {
	conn, err := database.Pool().Acquire(ctx)
	if err != nil {
		return false, nil, err
	}
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, hashtext($2))`,
		payoutSweepLockKey, name).Scan(&locked); err != nil {
		conn.Release()
		return false, nil, err
	}
	if !locked {
		conn.Release()
		return false, nil, nil
	}
	return true, func() {
		// Released on the same connection that took it, before that connection
		// goes back to the pool: an advisory lock left behind on a pooled
		// connection is a lock nothing can ever release.
		if _, err := conn.Exec(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock($1, hashtext($2))`, payoutSweepLockKey, name); err != nil &&
			!errors.Is(err, pgx.ErrTxClosed) {
			_ = err
		}
		conn.Release()
	}, nil
}
