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

// requireStableNetwork refuses to wire a job whose stable-asset service
// carries another network than the one configured: the service works on its
// own host's network, so the job would not do what its configuration says.
func requireStableNetwork(svc *stableasset.Service, network string) error {
	if err := requireNetwork(network); err != nil {
		return err
	}
	if svc == nil {
		return errs.New(errs.CodeInternal, "advancedjobs: a stable-asset job needs the stable-asset service")
	}
	if got := svc.Network(); got != network {
		return errs.Newf(errs.CodeInternal, "advancedjobs: the stable-asset service carries devnet network %s, not the configured %s", got, network)
	}
	return nil
}
