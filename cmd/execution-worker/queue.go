package main

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/settlement"
)

// Lease is one claimed plan. Owner is the claiming process; Expires is when
// the claim lapses unless renewed.
type Lease struct {
	PlanID   settlement.PlanID
	Owner    string
	Attempts int32
	Expires  time.Time
}

// Outcome classifies how a run of a plan ended. It is stored on the lease so
// operators can see, without reading the audit chain, why a plan is waiting.
type Outcome string

// Run outcomes.
const (
	// OutcomeCompleted: the plan reached COMPLETED.
	OutcomeCompleted Outcome = "COMPLETED"
	// OutcomeFailed: the plan reached a terminal failure; compensation ran.
	OutcomeFailed Outcome = "FAILED"
	// OutcomePaused: the plan is on the operator/reconciliation path
	// (RECONCILIATION_REQUIRED or SUBMISSION_STATE_UNKNOWN). It is NOT
	// retried by the worker; reconciliation resolves it.
	OutcomePaused Outcome = "PAUSED"
	// OutcomeRetry: a transient error; the plan is retried after a backoff.
	OutcomeRetry Outcome = "RETRY"
	// OutcomeNotRunnable: the plan refused to run at all — it is already
	// terminal, or a step is FAILED and cannot resume. Re-running would give
	// the same answer, so this is not a retry; an operator or a replan owns
	// it now.
	OutcomeNotRunnable Outcome = "NOT_RUNNABLE"
	// OutcomeAbandoned: shutdown interrupted the run before it concluded.
	// Nothing was torn down mid-transaction; the plan is released for
	// immediate re-claim by any worker, including this one on restart.
	OutcomeAbandoned Outcome = "ABANDONED"
)

// PlanQueue hands runnable plans to the worker under a lease. The Postgres
// implementation is PGPlanQueue; tests use an in-memory fake.
type PlanQueue interface {
	// Claim leases up to limit runnable plans for owner until now+ttl.
	Claim(ctx context.Context, owner string, limit int, ttl time.Duration) ([]Lease, error)
	// Renew extends a lease held by owner. It reports false when the lease
	// was lost (expired and taken by another worker), which tells the runner
	// to stop touching the plan.
	Renew(ctx context.Context, l Lease, ttl time.Duration) (bool, error)
	// Release ends a lease, recording the outcome and scheduling the next
	// attempt. A zero retryAfter means "immediately eligible".
	Release(ctx context.Context, l Lease, out Outcome, cause error, retryAfter time.Duration) error
}

// PGPlanQueue is the Postgres PlanQueue over execution_plan_leases
// (migration 00700).
//
// A plan is runnable when it is APPROVED or EXECUTING, is not a dry run, and
// is not currently leased or backed off. Claim takes the plan rows with FOR
// UPDATE SKIP LOCKED so concurrent claimers never consider the same plan; the
// lease row's primary key is the second line of defense, because the
// ON CONFLICT branch only fires for a lease that is genuinely expired and due.
type PGPlanQueue struct {
	db *db.DB
	// Now supplies the claim instant. Tests inject a fake clock.
	Now func() time.Time
}

// NewPGPlanQueue builds a Postgres plan queue.
func NewPGPlanQueue(d *db.DB, now func() time.Time) *PGPlanQueue {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &PGPlanQueue{db: d, Now: now}
}

var _ PlanQueue = (*PGPlanQueue)(nil)

// claimSQL leases the oldest runnable plans.
//
// The NOT EXISTS clause is checked under READ COMMITTED, so a lease inserted
// but not yet committed by a peer is invisible here. That is safe: both
// claimers then reach the INSERT, one blocks on the primary key until the
// other commits, and its ON CONFLICT branch re-evaluates the WHERE against the
// now-committed row, which is live, so it returns nothing. Exactly one worker
// wins in every interleaving.
const claimSQL = `
WITH candidate AS (
    SELECT p.id
      FROM execution_plans p
     WHERE p.status IN ('APPROVED', 'EXECUTING')
       AND p.dry_run = false
       AND NOT EXISTS (
             SELECT 1
               FROM execution_plan_leases l
              WHERE l.plan_id = p.id
                AND (l.lease_expires_at > $1 OR l.next_attempt_at > $1))
     ORDER BY p.created_at, p.id
     LIMIT $2
     FOR UPDATE OF p SKIP LOCKED
)
INSERT INTO execution_plan_leases AS l (plan_id, owner, leased_at, lease_expires_at, next_attempt_at, attempts)
SELECT c.id, $3, $1, $4, $1, 1 FROM candidate c
ON CONFLICT (plan_id) DO UPDATE
   SET owner = EXCLUDED.owner,
       leased_at = EXCLUDED.leased_at,
       lease_expires_at = EXCLUDED.lease_expires_at,
       attempts = l.attempts + 1
 WHERE l.lease_expires_at <= $1 AND l.next_attempt_at <= $1
RETURNING l.plan_id, l.attempts, l.lease_expires_at`

// Claim leases up to limit runnable plans.
func (q *PGPlanQueue) Claim(ctx context.Context, owner string, limit int, ttl time.Duration) ([]Lease, error) {
	if owner == "" {
		return nil, errs.New(errs.CodeInternal, "execution-worker: claim needs an owner")
	}
	if limit <= 0 {
		return nil, nil
	}
	if ttl <= 0 {
		return nil, errs.New(errs.CodeInternal, "execution-worker: claim needs a positive lease ttl")
	}
	now := q.Now().UTC()
	var out []Lease
	err := q.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		out = out[:0]
		rows, err := tx.Query(ctx, claimSQL, now, limit, owner, now.Add(ttl))
		if err != nil {
			return errs.Wrap(err, errs.CodeInternal, "execution-worker: claim plans")
		}
		defer rows.Close()
		for rows.Next() {
			var l Lease
			if err := rows.Scan(&l.PlanID, &l.Attempts, &l.Expires); err != nil {
				return errs.Wrap(err, errs.CodeInternal, "execution-worker: scan claimed plan")
			}
			l.Owner = owner
			l.Expires = l.Expires.UTC()
			out = append(out, l)
		}
		if err := rows.Err(); err != nil {
			return errs.Wrap(err, errs.CodeInternal, "execution-worker: read claimed plans")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

const renewSQL = `
UPDATE execution_plan_leases
   SET lease_expires_at = $3
 WHERE plan_id = $1 AND owner = $2 AND lease_expires_at > $4
RETURNING lease_expires_at`

// Renew extends a live lease. It reports false when the lease is gone.
func (q *PGPlanQueue) Renew(ctx context.Context, l Lease, ttl time.Duration) (bool, error) {
	now := q.Now().UTC()
	var expires time.Time
	err := q.db.QueryRow(ctx, renewSQL, l.PlanID, l.Owner, now.Add(ttl), now).Scan(&expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, errs.Wrap(err, errs.CodeInternal, "execution-worker: renew lease").WithField("plan_id", l.PlanID.String())
	}
	return true, nil
}

// releaseSQL expires the lease now and schedules the next attempt. The owner
// guard means a worker that lost its lease cannot rewrite the new holder's row.
const releaseSQL = `
UPDATE execution_plan_leases
   SET lease_expires_at = $3,
       next_attempt_at = $4,
       last_outcome = $5,
       last_error = $6
 WHERE plan_id = $1 AND owner = $2`

// Release ends the lease and records the outcome.
func (q *PGPlanQueue) Release(ctx context.Context, l Lease, out Outcome, cause error, retryAfter time.Duration) error {
	now := q.Now().UTC()
	if retryAfter < 0 {
		retryAfter = 0
	}
	if _, err := q.db.Exec(ctx, releaseSQL, l.PlanID, l.Owner, now, now.Add(retryAfter), string(out), nullable(errorText(cause))); err != nil {
		return errs.Wrap(err, errs.CodeInternal, "execution-worker: release lease").WithField("plan_id", l.PlanID.String())
	}
	return nil
}

// maxLastError bounds what is written to execution_plan_leases.last_error.
const maxLastError = 1024

// errorText renders an error for storage: the stable code and client-safe
// detail of an *errs.Error, the text otherwise, bounded in length. Nothing
// here is ever shown to a customer; it is operator triage material.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	var s string
	if e, ok := errs.As(err); ok {
		s = string(e.Code)
		if e.Detail != "" {
			s += ": " + e.Detail
		}
	} else {
		s = err.Error()
	}
	if len(s) > maxLastError {
		s = s[:maxLastError]
	}
	return s
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
