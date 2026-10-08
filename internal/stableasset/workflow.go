package stableasset

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

// SettleReport summarises one pass over pending requests.
type SettleReport struct {
	Examined int `json:"examined"`
	Issued   int `json:"issued"`
	Redeemed int `json:"redeemed"`
	Queued   int `json:"queued"`
	Failed   int `json:"failed"`
	// Waiting are requests whose transaction is not final yet.
	Waiting int `json:"waiting"`
	// Failures are the requests the pass could not settle or pay; every other
	// request it took was handled: one request's failure never stops a pass.
	// (Failed counts requests the pass settled as FAILED; a Failure is a
	// request it could not act on at all.)
	Failures []RequestFailure `json:"failures,omitempty"`
}

// The operations of the request passes in the upkeep failure ledger
// (migration 00760); their items are mint and burn requests.
const (
	UpkeepSettleMint    = "stableasset.settle_mint"
	UpkeepSettleBurn    = "stableasset.settle_burn"
	UpkeepPayRedemption = "stableasset.pay_redemption"
)

// RequestFailure is a mint or burn request a pass could not act on.
type RequestFailure struct {
	// Operation is the step that failed (UpkeepSettleMint ...) and RequestID
	// the mint or burn request.
	Operation string `json:"operation"`
	RequestID string `json:"request_id"`
	// Attempts counts its consecutive failures of that step; it is not taken
	// again before RetryAfter.
	Attempts   int       `json:"attempts"`
	RetryAfter time.Time `json:"retry_after"`
	Err        error     `json:"-"`
}

func boundLimit(limit int) int {
	if limit <= 0 || limit > 500 {
		return 100
	}
	return limit
}

// The requests a pass takes are its own network's -- a request of another
// network is never observed or settled here: this host would read its
// transaction as NOT_FOUND and could fail it as expired while it stands
// final on its own chain -- leaving out one whose last attempt failed and
// whose backoff is still running (unless the clock has gone back past that
// failure); never-failed requests first, then the oldest.
const (
	// unknownMintsWhere and unknownBurnsWhere: $1 grace cutoff, $2 limit,
	// $3 network, $4 operation, $5 now.
	unknownMintsWhere = `(status = 'SUBMISSION_UNKNOWN' OR (status = 'SUBMITTING' AND created_at < $1))
		AND stable_asset_id IN (SELECT sa.id FROM stable_assets sa WHERE sa.network = $3)
		AND NOT EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $4 AND uf.item_id = stable_mint_requests.id
		                  AND uf.cleared_at IS NULL AND uf.retry_after > $5 AND uf.last_failed_at <= $5)
		ORDER BY EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $4 AND uf.item_id = stable_mint_requests.id AND uf.cleared_at IS NULL),
		         created_at, id LIMIT $2`
	unknownBurnsWhere = `(status = 'SUBMISSION_UNKNOWN' OR (status = 'SUBMITTING' AND created_at < $1))
		AND stable_asset_id IN (SELECT sa.id FROM stable_assets sa WHERE sa.network = $3)
		AND NOT EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $4 AND uf.item_id = stable_burn_requests.id
		                  AND uf.cleared_at IS NULL AND uf.retry_after > $5 AND uf.last_failed_at <= $5)
		ORDER BY EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $4 AND uf.item_id = stable_burn_requests.id AND uf.cleared_at IS NULL),
		         created_at, id LIMIT $2`
	// submittedMintsWhere and submittedBurnsWhere: $1 limit, $2 network,
	// $3 operation, $4 now.
	submittedMintsWhere = `status = 'SUBMITTED'
		AND stable_asset_id IN (SELECT sa.id FROM stable_assets sa WHERE sa.network = $2)
		AND NOT EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $3 AND uf.item_id = stable_mint_requests.id
		                  AND uf.cleared_at IS NULL AND uf.retry_after > $4 AND uf.last_failed_at <= $4)
		ORDER BY EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $3 AND uf.item_id = stable_mint_requests.id AND uf.cleared_at IS NULL),
		         created_at, id LIMIT $1`
	submittedBurnsWhere = `status = 'SUBMITTED'
		AND stable_asset_id IN (SELECT sa.id FROM stable_assets sa WHERE sa.network = $2)
		AND NOT EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $3 AND uf.item_id = stable_burn_requests.id
		                  AND uf.cleared_at IS NULL AND uf.retry_after > $4 AND uf.last_failed_at <= $4)
		ORDER BY EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $3 AND uf.item_id = stable_burn_requests.id AND uf.cleared_at IS NULL),
		         created_at, id LIMIT $1`
	// queuedRedemptionsWhere: $1 stable asset, $2 limit, $3 operation, $4 now.
	queuedRedemptionsWhere = `stable_asset_id = $1 AND status = 'REDEMPTION_QUEUED'
		AND NOT EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $3 AND uf.item_id = stable_burn_requests.id
		                  AND uf.cleared_at IS NULL AND uf.retry_after > $4 AND uf.last_failed_at <= $4)
		ORDER BY EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $3 AND uf.item_id = stable_burn_requests.id AND uf.cleared_at IS NULL),
		         created_at, id LIMIT $2`
)

// ReconcileUnknown resolves submissions of this service's network whose
// outcome is unknown: requests in SUBMISSION_UNKNOWN, and requests still
// SUBMITTING after the grace period (the process that recorded them may have
// stopped before or after submitting). Each is settled from the chain's
// status by transaction id -- FINALIZED posts and applies, FAILED (final) or
// NOT_FOUND past blockhash expiry fails and releases, anything else waits.
// Nothing is ever resubmitted. Each request is settled in its own
// transaction; one that cannot be is recorded with a backoff and reported in
// Failures, and the pass goes on.
func (s *Service) ReconcileUnknown(ctx context.Context, limit int) (SettleReport, error) {
	now := s.clk.Now().UTC()
	cutoff := now.Add(-s.params.UnknownGrace)
	limit = boundLimit(limit)
	network := s.Network()
	mints, err := listMints(ctx, s.db, unknownMintsWhere, cutoff, limit, network, UpkeepSettleMint, now)
	if err != nil {
		return SettleReport{}, err
	}
	burns, err := listBurns(ctx, s.db, unknownBurnsWhere, cutoff, limit, network, UpkeepSettleBurn, now)
	if err != nil {
		return SettleReport{}, err
	}
	return s.settleAll(ctx, mints, burns)
}

// ObservePending settles requests of this service's network the chain
// accepted (SUBMITTED): in DEVNET mode blocks come from the chain's own
// producer, and this is how a request reaches its final state. It never
// resubmits and never produces blocks. Like ReconcileUnknown, one request's
// failure is reported and never stops the pass.
func (s *Service) ObservePending(ctx context.Context, limit int) (SettleReport, error) {
	now := s.clk.Now().UTC()
	limit = boundLimit(limit)
	network := s.Network()
	mints, err := listMints(ctx, s.db, submittedMintsWhere, limit, network, UpkeepSettleMint, now)
	if err != nil {
		return SettleReport{}, err
	}
	burns, err := listBurns(ctx, s.db, submittedBurnsWhere, limit, network, UpkeepSettleBurn, now)
	if err != nil {
		return SettleReport{}, err
	}
	return s.settleAll(ctx, mints, burns)
}

// settleAll settles each request in its own transaction. A request that
// cannot be settled is recorded with a backoff and reported, and the pass
// goes on; an error means the pass could not go on (the context ended, a
// failure could not be recorded), and the report says what it did first.
func (s *Service) settleAll(ctx context.Context, mints []MintRecord, burns []BurnRecord) (SettleReport, error) {
	var rep SettleReport
	for _, m := range mints {
		rep.Examined++
		res, err := s.settleMintTx(ctx, m.ID)
		if err == nil {
			err = s.clearUpkeepFailure(ctx, UpkeepSettleMint, m.ID)
		}
		if err != nil {
			if ferr := s.requestFailed(ctx, &rep, UpkeepSettleMint, m.ID.String(), m.ID, err); ferr != nil {
				return rep, ferr
			}
			continue
		}
		switch res.Request.Status {
		case MintIssued:
			rep.Issued++
		case MintFailed:
			rep.Failed++
		default:
			rep.Waiting++
		}
	}
	for _, b := range burns {
		rep.Examined++
		res, err := s.settleBurnTx(ctx, b.ID)
		if err == nil {
			err = s.clearUpkeepFailure(ctx, UpkeepSettleBurn, b.ID)
		}
		if err != nil {
			if ferr := s.requestFailed(ctx, &rep, UpkeepSettleBurn, b.ID.String(), b.ID, err); ferr != nil {
				return rep, ferr
			}
			continue
		}
		switch res.Request.Status {
		case BurnRedeemed:
			rep.Redeemed++
		case BurnRedemptionQueued:
			rep.Queued++
		case BurnFailed:
			rep.Failed++
		default:
			rep.Waiting++
		}
	}
	return rep, nil
}

// ProcessRedemptions pays out queued redemptions (redemption mode QUEUED) of
// one stable asset, oldest first, each in its own transaction. A payout that
// cannot be made is recorded with a backoff and reported in Failures, and the
// others are paid.
func (s *Service) ProcessRedemptions(ctx context.Context, sid StableAssetID, limit int) (SettleReport, error) {
	burns, err := listBurns(ctx, s.db, queuedRedemptionsWhere, sid, boundLimit(limit), UpkeepPayRedemption, s.clk.Now().UTC())
	if err != nil {
		return SettleReport{}, err
	}
	var rep SettleReport
	for _, b := range burns {
		rep.Examined++
		var rec BurnRecord
		err := s.db.InTx(ctx, txOptions, func(ctx context.Context, tx pgx.Tx) error {
			r, err := s.PayRedemption(ctx, tx, b.ID)
			if err != nil {
				return err
			}
			rec = r
			return clearUpkeepFailure(ctx, tx, UpkeepPayRedemption, b.ID, s.clk.Now().UTC())
		})
		if err != nil {
			if ferr := s.requestFailed(ctx, &rep, UpkeepPayRedemption, b.ID.String(), b.ID, err); ferr != nil {
				return rep, ferr
			}
			continue
		}
		if rec.Status == BurnRedeemed {
			rep.Redeemed++
		}
	}
	return rep, nil
}

// Statements of the upkeep failure ledger (migration 00760).
const (
	upkeepSucceededSQL = `SELECT cp_upkeep_succeeded($1, $2, $3)`
	upkeepFailedSQL    = `SELECT failures, retry_after FROM cp_upkeep_failed($1, $2, $3, $4, $5)`
)

// requestFailed records, in a transaction of its own (the request's own has
// rolled back), that the step op could not act on the request, and adds it
// to the report. A cancelled context, or a failure that cannot be recorded,
// ends the pass instead.
func (s *Service) requestFailed(ctx context.Context, rep *SettleReport, op, request string, item any, cause error) error {
	if ctx.Err() != nil || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	f := RequestFailure{Operation: op, RequestID: request, Err: cause}
	if err := s.db.InTx(ctx, txOptions, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, upkeepFailedSQL, id.New[id.Any](), op, item, s.clk.Now().UTC(), string(errs.CodeOf(cause))).
			Scan(&f.Attempts, &f.RetryAfter)
	}); err != nil {
		return errors.Join(cause, dbErr("record the request's upkeep failure", err))
	}
	f.RetryAfter = f.RetryAfter.UTC()
	rep.Failures = append(rep.Failures, f)
	return nil
}

// clearUpkeepFailure ends the request's run of failures of op, if it had one,
// in a transaction of its own.
func (s *Service) clearUpkeepFailure(ctx context.Context, op string, item any) error {
	return s.db.InTx(ctx, txOptions, func(ctx context.Context, tx pgx.Tx) error {
		return clearUpkeepFailure(ctx, tx, op, item, s.clk.Now().UTC())
	})
}

func clearUpkeepFailure(ctx context.Context, tx pgx.Tx, op string, item any, now time.Time) error {
	if _, err := tx.Exec(ctx, upkeepSucceededSQL, op, item, now); err != nil {
		return dbErr("clear the request's upkeep failures", err)
	}
	return nil
}
