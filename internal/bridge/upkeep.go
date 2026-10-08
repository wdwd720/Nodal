package bridge

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

// Batch upkeep (migration 00760). A batch operation that owns its
// transactions handles every item on its own; an item it cannot handle is
// recorded in upkeep_failures with its error code and a backoff, in a
// transaction of its own, and is not selected again until the backoff has
// passed, so an item that always fails is never in front of the items behind
// it.

// Statements of the upkeep failure ledger.
const (
	upkeepSucceededSQL = `SELECT cp_upkeep_succeeded($1, $2, $3)`
	upkeepFailedSQL    = `SELECT failures, retry_after FROM cp_upkeep_failed($1, $2, $3, $4, $5)`
)

// upkeepFailure is how the ledger now records an item's failure.
type upkeepFailure struct {
	attempts   int
	retryAfter time.Time
}

// recordUpkeepFailure records, in a transaction of its own -- the item's own
// transactions have rolled back -- that op could not handle item because of
// cause. A cancelled context, or a failure that cannot be recorded, is
// returned instead: the pass ends.
func (s *Service) recordUpkeepFailure(ctx context.Context, op string, item any, cause error) (upkeepFailure, error) {
	if ctx.Err() != nil || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return upkeepFailure{}, cause
	}
	var f upkeepFailure
	if err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, upkeepFailedSQL, id.New[id.Any](), op, item, s.clk.Now().UTC(), string(errs.CodeOf(cause))).
			Scan(&f.attempts, &f.retryAfter)
	}); err != nil {
		return upkeepFailure{}, errors.Join(cause, dbErr("record the item's upkeep failure", err))
	}
	f.retryAfter = f.retryAfter.UTC()
	return f, nil
}

// clearUpkeepFailure ends the item's run of failures of op, if it had one.
func clearUpkeepFailure(ctx context.Context, tx pgx.Tx, op string, item any, now time.Time) error {
	if _, err := tx.Exec(ctx, upkeepSucceededSQL, op, item, now); err != nil {
		return dbErr("clear the item's upkeep failures", err)
	}
	return nil
}
