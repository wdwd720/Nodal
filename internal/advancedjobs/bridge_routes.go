package advancedjobs

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/bridge"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/reconciliation"
)

// BridgeRoutes runs reconciliation kind BRIDGE
// (bridge.Service.ReconcileRoute, PERIODIC) for every route on the configured
// network that is ACTIVE or PAUSED: the route's books against both chains at
// one instant (locked = outstanding + in flight; the source escrow and the
// destination supply against what the books and the landed-but-unposted
// transactions explain). A mismatch opens or refreshes a material record that
// blocks new risk and pauses the route; a match converges on the route's
// standing MATCHED record -- the first matched pass records it and later ones
// write nothing while it is the route's newest record -- so the table does not
// grow with the schedule.
type BridgeRoutes struct {
	db      *db.DB
	svc     *bridge.Service
	network string
	spec    Spec
}

// NewBridgeRoutes wires the job for the routes of one devnet network.
func NewBridgeRoutes(d *db.DB, svc *bridge.Service, network string, s Settings) (*BridgeRoutes, error) {
	if err := requireBridgeNetwork(svc, network); err != nil {
		return nil, err
	}
	return &BridgeRoutes{db: d, svc: svc, network: network, spec: spec(NameBridgeRoutes, s)}, nil
}

// Spec implements Job.
func (j *BridgeRoutes) Spec() Spec { return j.spec }

// openRoutesSQL lists the routes of a network that are not CLOSED, in id
// order. Read-only; bridge exports no lister.
const openRoutesSQL = `SELECT id FROM bridge_routes WHERE network = $1 AND status IN ('ACTIVE','PAUSED') ORDER BY id LIMIT $2`

// Run reconciles each route in its own transaction; a recorded mismatch
// counts as changed.
func (j *BridgeRoutes) Run(ctx context.Context) (Report, error) {
	var ids []bridge.RouteID
	if err := readOnly(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, openRoutesSQL, j.network, j.spec.Batch)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id bridge.RouteID
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
	for _, rid := range ids {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		rep.Examined++
		var res bridge.RouteReconciliation
		err := inUnit(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			res, err = j.svc.ReconcileRoute(ctx, tx, rid, reconciliation.ModePeriodic, workerActor())
			return err
		})
		if err != nil {
			rep.fail(rid.String(), err)
			continue
		}
		if !res.Matched() {
			rep.Changed++
		}
	}
	return rep, nil
}
