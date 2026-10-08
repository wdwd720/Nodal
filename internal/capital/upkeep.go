package capital

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

// Batch upkeep (migration 00760). A batch operation processes every item it
// selects in its own savepoint of the caller's transaction, so an item that
// cannot be processed is rolled back alone and the batch goes on; the item is
// recorded in upkeep_failures with its error code and a backoff, and is not
// selected again until the backoff has passed, so an item that always fails
// is never in front of the items behind it.

// UpkeepExpireDue names ExpireDueBatch in the upkeep failure ledger.
const UpkeepExpireDue = "capital.expire_due"

// upkeepFailure is an item a batch could not process, as the ledger now
// records it.
type upkeepFailure struct {
	attempts   int
	retryAfter time.Time
	err        error
}

// Statements of the upkeep failure ledger (migration 00760).
const (
	upkeepCheckDeferredSQL = `SELECT cp_upkeep_check_deferred()`
	upkeepSucceededSQL     = `SELECT cp_upkeep_succeeded($1, $2, $3)`
	upkeepFailedSQL        = `SELECT failures, retry_after FROM cp_upkeep_failed($1, $2, $3, $4, $5)`
)

// deferredChecksPass reports whether the deferred checks the caller's
// transaction already has pending pass now. When they do, they are run (and
// pass) here, and each item's own checks can then be run at the end of the
// item; when they do not, a check run per item would blame every item for
// the caller's change, so the batch leaves its items' checks to COMMIT.
func deferredChecksPass(ctx context.Context, tx pgx.Tx) (bool, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return false, dbErr("open upkeep savepoint", err)
	}
	if _, err := sp.Exec(ctx, upkeepCheckDeferredSQL); err != nil {
		if rerr := sp.Rollback(ctx); rerr != nil {
			return false, errors.Join(err, dbErr("roll back upkeep savepoint", rerr))
		}
		if upkeepFatal(ctx, err) {
			return false, err
		}
		return false, nil
	}
	if err := sp.Commit(ctx); err != nil {
		return false, dbErr("release upkeep savepoint", err)
	}
	return true, nil
}

// upkeepItem runs fn as one item of the batch operation op, in a savepoint of
// tx:
//
//   - fn's work is kept only when fn returns nil and, with checkDeferred,
//     every deferred check it queued passes now (cp_upkeep_check_deferred);
//     the item's run of failures, if it had one, is over;
//   - otherwise its work is rolled back to the savepoint, the failure is
//     recorded with its backoff (cp_upkeep_failed) and returned, and tx stays
//     usable for the next item;
//   - a serialization failure, a deadlock or a cancelled context is fatal: it
//     ends the batch, and the caller's transaction must be retried from the
//     top or abandoned.
func upkeepItem(ctx context.Context, tx pgx.Tx, op string, item any, now time.Time, checkDeferred bool,
	fn func(sp pgx.Tx) error) (*upkeepFailure, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return nil, dbErr("open item savepoint", err)
	}
	ferr := fn(sp)
	if ferr == nil && checkDeferred {
		if _, err := sp.Exec(ctx, upkeepCheckDeferredSQL); err != nil {
			ferr = dbErr("run the item's deferred checks", err)
		}
	}
	if ferr == nil {
		if _, err := sp.Exec(ctx, upkeepSucceededSQL, op, item, now); err != nil {
			_ = sp.Rollback(ctx) // the error below is the one that matters
			return nil, dbErr("clear the item's upkeep failures", err)
		}
		if err := sp.Commit(ctx); err != nil {
			return nil, dbErr("release item savepoint", err)
		}
		return nil, nil
	}
	if err := sp.Rollback(ctx); err != nil {
		return nil, errors.Join(ferr, dbErr("roll back item savepoint", err))
	}
	if upkeepFatal(ctx, ferr) {
		return nil, ferr
	}
	f := &upkeepFailure{err: ferr}
	if err := tx.QueryRow(ctx, upkeepFailedSQL, id.New[id.Any](), op, item, now, string(errs.CodeOf(ferr))).
		Scan(&f.attempts, &f.retryAfter); err != nil {
		return nil, dbErr("record the item's upkeep failure", err)
	}
	f.retryAfter = f.retryAfter.UTC()
	return f, nil
}

// upkeepFatal reports whether an item's error ends the whole batch: a
// transient database failure, which the caller retries from the top, or a
// cancelled context.
func upkeepFatal(ctx context.Context, err error) bool {
	return ctx.Err() != nil || db.IsRetryable(err) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
