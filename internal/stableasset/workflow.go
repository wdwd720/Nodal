package stableasset

import (
	"context"

	"github.com/jackc/pgx/v5"
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
}

func boundLimit(limit int) int {
	if limit <= 0 || limit > 500 {
		return 100
	}
	return limit
}

// ReconcileUnknown resolves submissions whose outcome is unknown: requests in
// SUBMISSION_UNKNOWN, and requests still SUBMITTING after the grace period
// (the process that recorded them may have stopped before or after
// submitting). Each is settled from the chain's status by transaction id --
// FINALIZED posts and applies, FAILED (final) or NOT_FOUND past blockhash
// expiry fails and releases, anything else waits. Nothing is ever resubmitted.
func (s *Service) ReconcileUnknown(ctx context.Context, limit int) (SettleReport, error) {
	cutoff := s.clk.Now().UTC().Add(-s.params.UnknownGrace)
	limit = boundLimit(limit)
	mints, err := listMints(ctx, s.db, `status = 'SUBMISSION_UNKNOWN' OR (status = 'SUBMITTING' AND created_at < $1)
		ORDER BY created_at, id LIMIT $2`, cutoff, limit)
	if err != nil {
		return SettleReport{}, err
	}
	burns, err := listBurns(ctx, s.db, `status = 'SUBMISSION_UNKNOWN' OR (status = 'SUBMITTING' AND created_at < $1)
		ORDER BY created_at, id LIMIT $2`, cutoff, limit)
	if err != nil {
		return SettleReport{}, err
	}
	return s.settleAll(ctx, mints, burns)
}

// ObservePending settles requests the chain accepted (SUBMITTED): in DEVNET
// mode blocks come from the chain's own producer, and this is how a request
// reaches its final state. It never resubmits and never produces blocks.
func (s *Service) ObservePending(ctx context.Context, limit int) (SettleReport, error) {
	limit = boundLimit(limit)
	mints, err := listMints(ctx, s.db, `status = 'SUBMITTED' ORDER BY created_at, id LIMIT $1`, limit)
	if err != nil {
		return SettleReport{}, err
	}
	burns, err := listBurns(ctx, s.db, `status = 'SUBMITTED' ORDER BY created_at, id LIMIT $1`, limit)
	if err != nil {
		return SettleReport{}, err
	}
	return s.settleAll(ctx, mints, burns)
}

func (s *Service) settleAll(ctx context.Context, mints []MintRecord, burns []BurnRecord) (SettleReport, error) {
	var rep SettleReport
	for _, m := range mints {
		rep.Examined++
		res, err := s.settleMintTx(ctx, m.ID)
		if err != nil {
			return rep, err
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
		if err != nil {
			return rep, err
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
// one stable asset, oldest first, each in its own transaction.
func (s *Service) ProcessRedemptions(ctx context.Context, sid StableAssetID, limit int) (SettleReport, error) {
	burns, err := listBurns(ctx, s.db, `stable_asset_id = $1 AND status = 'REDEMPTION_QUEUED' ORDER BY created_at, id LIMIT $2`,
		sid, boundLimit(limit))
	if err != nil {
		return SettleReport{}, err
	}
	var rep SettleReport
	for _, b := range burns {
		rep.Examined++
		var rec BurnRecord
		if err := s.db.InTx(ctx, txOptions, func(ctx context.Context, tx pgx.Tx) error {
			r, err := s.PayRedemption(ctx, tx, b.ID)
			rec = r
			return err
		}); err != nil {
			return rep, err
		}
		if rec.Status == BurnRedeemed {
			rep.Redeemed++
		}
	}
	return rep, nil
}
