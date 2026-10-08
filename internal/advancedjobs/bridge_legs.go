package advancedjobs

import (
	"context"

	"github.com/nodal/controlplane/internal/bridge"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
)

// BridgeLegs reconciles bridge legs in flight on the configured network
// (bridge.Service.ObservePending):
//
//   - a DESTINATION_FINALIZED leg is checked against both chains
//     (bridge.Service.ReconcileLeg): its source lock final and exactly the
//     leg's, exactly one release of it final on the destination and exactly
//     the expected one, both postings present. A match is RECONCILED;
//     anything else goes to MANUAL_REVIEW.
//   - an earlier leg -- BRIDGE_REQUESTED, SOURCE_FINALIZED, MESSAGE_IN_FLIGHT
//     or DESTINATION_OBSERVED -- is advanced by observation only (Observe's
//     drive, which owns its transactions): its outcome is read from the
//     chains by transaction id, a release is relayed only once no earlier one
//     can still land, and nothing is resubmitted and no block is produced.
//     This is how a DEVNET leg, whose chain is produced externally, reaches
//     its end, and how any leg whose driving process stopped does once blocks
//     are produced.
//
// ObservePending takes only the legs of its host's network -- another
// network's lock would read NOT_FOUND here and could be settled as expired
// while it stands final on its own chain -- and drives each on its own: a leg
// it cannot advance is recorded with a backoff and reported as that leg's
// failure, and the others are driven.
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

// Run drives up to a batch of legs; a leg whose status moved counts as
// changed. An error means the pass could not go on; what it did before is
// committed and counted.
func (j *BridgeLegs) Run(ctx context.Context) (Report, error) {
	res, err := j.svc.ObservePending(ctx, j.spec.Batch)
	rep := Report{Examined: res.Examined, Changed: res.Moved}
	for _, f := range res.Failures {
		rep.fail(f.LegID.String(), f.Err)
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
