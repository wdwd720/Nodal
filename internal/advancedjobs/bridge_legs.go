package advancedjobs

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/bridge"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
)

// BridgeLegs reconciles bridge legs in flight on the configured network, in
// two passes:
//
//   - a DESTINATION_FINALIZED leg is checked against both chains
//     (bridge.Service.ReconcileLeg, in its own transaction): its source lock
//     final and exactly the leg's, exactly one release of it final on the
//     destination and exactly the expected one, both postings present. A
//     match is RECONCILED; anything else goes to MANUAL_REVIEW.
//   - every leg still open after that -- BRIDGE_REQUESTED, SOURCE_FINALIZED,
//     MESSAGE_IN_FLIGHT or DESTINATION_OBSERVED, and a finalized one the
//     first pass could not reconcile -- is advanced by observation only
//     (bridge.Service.ObservePending, Observe's drive per leg, owning its
//     transactions): its outcome is read from the chains by transaction id, a
//     release is relayed only once no earlier one can still land, and nothing
//     is resubmitted and no block is produced. This is how a DEVNET leg, whose
//     chain is produced externally, reaches its end, and how any leg whose
//     driving process stopped does once blocks are produced.
//
// Both passes take only the legs of the service's host network -- another
// network's lock would read NOT_FOUND here and could be settled as expired
// while it stands final on its own chain -- and one leg's failure is that
// leg's. ObservePending records a leg it cannot advance with a backoff and
// leaves it out until the backoff has passed; the first pass leaves out every
// leg with such a run of failures, so a leg that keeps failing is retried
// only on its backoff and never fills a batch.
type BridgeLegs struct {
	db      *db.DB
	svc     *bridge.Service
	network string
	spec    Spec
}

// NewBridgeLegs wires the job for the legs of one devnet network: the one the
// bridge service's host carries.
func NewBridgeLegs(d *db.DB, svc *bridge.Service, network string, s Settings) (*BridgeLegs, error) {
	if err := requireBridgeNetwork(svc, network); err != nil {
		return nil, err
	}
	return &BridgeLegs{db: d, svc: svc, network: network, spec: spec(NameBridgeLegs, s)}, nil
}

// Spec implements Job.
func (j *BridgeLegs) Spec() Spec { return j.spec }

// finalizedLegsSQL lists the network's DESTINATION_FINALIZED legs, oldest
// first, leaving out a leg with a run of failures in the upkeep failure
// ledger ($3, ObservePending's operation): that one is ObservePending's to
// retry on its backoff. Read-only; bridge exports no lister.
const finalizedLegsSQL = `SELECT l.id FROM bridge_legs l JOIN bridge_routes r ON r.id = l.route_id
	WHERE r.network = $1 AND l.status = 'DESTINATION_FINALIZED'
	  AND NOT EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $3 AND uf.item_id = l.id AND uf.cleared_at IS NULL)
	ORDER BY l.created_at, l.id LIMIT $2`

// Run reconciles the finalized legs, then drives the rest; a leg whose status
// moved counts as changed. An error means a pass could not go on; what was
// done before it is committed and counted.
func (j *BridgeLegs) Run(ctx context.Context) (Report, error) {
	var finalized []bridge.LegID
	if err := readOnly(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, finalizedLegsSQL, j.network, j.spec.Batch, bridge.UpkeepObserveLeg)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id bridge.LegID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			finalized = append(finalized, id)
		}
		return rows.Err()
	}); err != nil {
		return Report{}, err
	}
	var rep Report
	failed := map[string]bool{}
	for _, lid := range finalized {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		rep.Examined++
		var leg bridge.Leg
		err := inUnit(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			leg, err = j.svc.ReconcileLeg(ctx, tx, lid)
			return err
		})
		if err != nil {
			rep.fail(lid.String(), err)
			failed[lid.String()] = true
			continue
		}
		if leg.Status != bridge.StateDestinationFinalized {
			rep.Changed++
		}
	}
	res, err := j.svc.ObservePending(ctx, j.spec.Batch)
	rep.Examined += res.Examined
	rep.Changed += res.Moved
	for _, f := range res.Failures {
		if !failed[f.LegID.String()] {
			rep.fail(f.LegID.String(), f.Err)
		}
	}
	return rep, err
}

// requireBridgeNetwork refuses to wire a job whose bridge service carries
// another network than the one configured: the service works on its own
// host's network, so the job would not do what its configuration says.
func requireBridgeNetwork(svc *bridge.Service, network string) error {
	if err := requireNetwork(network); err != nil {
		return err
	}
	if svc == nil {
		return errs.New(errs.CodeInternal, "advancedjobs: a bridge job needs the bridge service")
	}
	if got := svc.Network(); got != network {
		return errs.Newf(errs.CodeInternal, "advancedjobs: the bridge service carries devnet network %s, not the configured %s", got, network)
	}
	return nil
}
