package advancedjobs

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/derivatives"
)

// DerivativesClearing runs reconciliation kind CLEARING
// (derivatives.Service.ReconcileClearing) for every asset the clearing house
// settles in: the clearing house's books against the cash-flow register. A
// mismatch opens, or refreshes, a material CLEARING record -- which refuses
// new derivative risk while it is unresolved -- and a match leaves an open
// record for a person to resolve. The subsystem takes the clearing house's
// lock, so the comparison is made at one quiescent moment.
type DerivativesClearing struct {
	db   *db.DB
	svc  *derivatives.Service
	spec Spec
}

// NewDerivativesClearing wires the job.
func NewDerivativesClearing(d *db.DB, svc *derivatives.Service, s Settings) *DerivativesClearing {
	return &DerivativesClearing{db: d, svc: svc, spec: spec(NameDerivativesClearing, s)}
}

// Spec implements Job.
func (j *DerivativesClearing) Spec() Spec { return j.spec }

// clearingAssetsSQL lists the assets the clearing house's books or the
// derivative positions use: the settlement asset of every position ever
// opened, and the asset of every CLEARING_* ledger account (a CLEARING code
// is held only by a CLEARING_HOUSE facility, FA001). Read-only; derivatives
// exports no lister.
const clearingAssetsSQL = `SELECT asset_id FROM (
		SELECT DISTINCT settlement_asset_id AS asset_id FROM derivative_positions
		UNION
		SELECT DISTINCT asset_id FROM ledger_accounts
		 WHERE owner_type = 'FACILITY' AND code IN ('CLEARING_CASH','CLEARING_CAPITAL','CLEARING_RESULT')
	) a ORDER BY asset_id LIMIT $1`

// Run reconciles each asset in its own transaction.
func (j *DerivativesClearing) Run(ctx context.Context) (Report, error) {
	var list []assets.AssetID
	if err := readOnly(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, clearingAssetsSQL, j.spec.Batch)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a assets.AssetID
			if err := rows.Scan(&a); err != nil {
				return err
			}
			list = append(list, a)
		}
		return rows.Err()
	}); err != nil {
		return Report{}, err
	}
	var rep Report
	for _, asset := range list {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		rep.Examined++
		var res derivatives.ClearingReport
		err := inUnit(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			res, err = j.svc.ReconcileClearing(ctx, tx, asset)
			return err
		})
		if err != nil {
			rep.fail(asset.String(), err)
			continue
		}
		if !res.Matched {
			// A mismatch was recorded: a CLEARING record opened or refreshed.
			rep.Changed++
		}
	}
	return rep, nil
}
