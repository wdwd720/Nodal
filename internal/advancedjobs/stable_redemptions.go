package advancedjobs

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/stableasset"
)

// StableRedemptions pays out queued redemptions
// (stableasset.Service.ProcessRedemptions) of every stable asset on the
// configured network whose redemption mode queues them: each burn whose
// transaction is final and whose payout waits in REDEMPTION_QUEUED is paid
// (STABLE_REDEMPTION), oldest first, in a transaction of its own.
type StableRedemptions struct {
	db      *db.DB
	svc     *stableasset.Service
	network string
	spec    Spec
}

// NewStableRedemptions wires the job for the stable assets of one devnet
// network.
func NewStableRedemptions(d *db.DB, svc *stableasset.Service, network string, s Settings) (*StableRedemptions, error) {
	if err := requireNetwork(network); err != nil {
		return nil, err
	}
	return &StableRedemptions{db: d, svc: svc, network: network, spec: spec(NameStableRedemptions, s)}, nil
}

// Spec implements Job.
func (j *StableRedemptions) Spec() Spec { return j.spec }

// queuedRedemptionAssetsSQL lists the stable assets on the network with a
// redemption waiting to be paid, in id order. Read-only; stableasset exports
// no lister.
const queuedRedemptionAssetsSQL = `SELECT s.id FROM stable_assets s
	WHERE s.network = $1 AND EXISTS (SELECT 1 FROM stable_burn_requests b
	                                  WHERE b.stable_asset_id = s.id AND b.status = 'REDEMPTION_QUEUED')
	ORDER BY s.id`

// Run pays up to a batch of redemptions across the assets.
func (j *StableRedemptions) Run(ctx context.Context) (Report, error) {
	var ids []stableasset.StableAssetID
	if err := readOnly(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, queuedRedemptionAssetsSQL, j.network)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id stableasset.StableAssetID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	}); err != nil {
		return Report{}, err
	}
	var rep Report
	for _, sid := range ids {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		left := j.spec.Batch - rep.Examined
		if left <= 0 {
			break
		}
		res, err := j.svc.ProcessRedemptions(ctx, sid, left)
		rep.Examined += res.Examined
		rep.Changed += res.Redeemed
		if err != nil {
			// ProcessRedemptions stops at the first payout it could not
			// make; the ones before it are committed and counted above.
			rep.Examined++
			rep.fail(sid.String(), err)
		}
	}
	return rep, nil
}
