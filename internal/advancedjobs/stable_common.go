package advancedjobs

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/stableasset"
)

// stableAssetsOnNetworkSQL lists the stable assets the configured devnet host
// carries -- registered on its network and not RETIRED -- in id order.
// Read-only; stableasset exports no lister. A stable asset on another network
// lives on another chain this worker cannot read, so it is never touched
// here.
const stableAssetsOnNetworkSQL = `SELECT id FROM stable_assets
	WHERE network = $1 AND status IN ('ACTIVE','PAUSED') ORDER BY id LIMIT $2`

func stableAssetsOn(ctx context.Context, d *db.DB, network string, limit int) ([]stableasset.StableAssetID, error) {
	var out []stableasset.StableAssetID
	err := readOnly(ctx, d, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, stableAssetsOnNetworkSQL, network, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id stableasset.StableAssetID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
		}
		return rows.Err()
	})
	return out, err
}

// requireNetwork refuses to wire a chain job without a network: an empty one
// would match no stable asset or route and look like idleness.
func requireNetwork(network string) error {
	if network == "" {
		return errs.New(errs.CodeInternal, "advancedjobs: a devnet job needs the network its host carries")
	}
	return nil
}
