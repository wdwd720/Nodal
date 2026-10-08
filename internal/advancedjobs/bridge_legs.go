package advancedjobs

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/bridge"
	"github.com/nodal/controlplane/internal/db"
)

// BridgeLegs reconciles bridge legs in flight on the configured network:
//
//   - a DESTINATION_FINALIZED leg is checked against both chains
//     (bridge.Service.ReconcileLeg, in its own transaction): its source lock
//     final and exactly the leg's, exactly one release of it final on the
//     destination and exactly the expected one, both postings present. A
//     match is RECONCILED; anything else goes to MANUAL_REVIEW.
//   - an earlier leg -- BRIDGE_REQUESTED, SOURCE_FINALIZED, MESSAGE_IN_FLIGHT
//     or DESTINATION_OBSERVED -- is advanced by observation only
//     (bridge.Service.Observe, a driver that owns its transactions): its
//     outcome is read from the chains by transaction id, a release is relayed
//     only once no earlier one can still land, and nothing is resubmitted and
//     no block is produced. This is how a DEVNET leg -- and any leg whose
//     driving process stopped -- reaches its end.
type BridgeLegs struct {
	db      *db.DB
	svc     *bridge.Service
	network string
	spec    Spec
}

// NewBridgeLegs wires the job for the legs of one devnet network.
func NewBridgeLegs(d *db.DB, svc *bridge.Service, network string, s Settings) (*BridgeLegs, error) {
	if err := requireNetwork(network); err != nil {
		return nil, err
	}
	return &BridgeLegs{db: d, svc: svc, network: network, spec: spec(NameBridgeLegs, s)}, nil
}

// Spec implements Job.
func (j *BridgeLegs) Spec() Spec { return j.spec }

// inFlightLegsSQL lists the legs in flight on the routes of a network, oldest
// first. Read-only; bridge exports no lister.
const inFlightLegsSQL = `SELECT l.id, l.status FROM bridge_legs l JOIN bridge_routes r ON r.id = l.route_id
	WHERE r.network = $1 AND l.status IN ('BRIDGE_REQUESTED','SOURCE_FINALIZED','MESSAGE_IN_FLIGHT',
	                                      'DESTINATION_OBSERVED','DESTINATION_FINALIZED')
	ORDER BY l.created_at, l.id LIMIT $2`

type legUnit struct {
	id     bridge.LegID
	status bridge.CrossChainCapitalState
}

// Run handles each leg on its own; a leg whose status moved counts as
// changed.
func (j *BridgeLegs) Run(ctx context.Context) (Report, error) {
	var legs []legUnit
	if err := readOnly(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, inFlightLegsSQL, j.network, j.spec.Batch)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var u legUnit
			if err := rows.Scan(&u.id, &u.status); err != nil {
				return err
			}
			legs = append(legs, u)
		}
		return rows.Err()
	}); err != nil {
		return Report{}, err
	}
	var rep Report
	for _, u := range legs {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		rep.Examined++
		var after bridge.CrossChainCapitalState
		var err error
		if u.status == bridge.StateDestinationFinalized {
			err = inUnit(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
				leg, err := j.svc.ReconcileLeg(ctx, tx, u.id)
				after = leg.Status
				return err
			})
		} else {
			var res bridge.LegResult
			res, err = j.svc.Observe(ctx, u.id)
			after = res.Leg.Status
		}
		if err != nil {
			rep.fail(u.id.String(), err)
			continue
		}
		if after != u.status {
			rep.Changed++
		}
	}
	return rep, nil
}
