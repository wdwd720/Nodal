package advancedjobs

import (
	"context"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/stableasset"
)

// StableUnknown resolves stable-asset mints and burns whose outcome is unknown
// (stableasset.Service.ReconcileUnknown): requests in SUBMISSION_UNKNOWN, and
// requests still SUBMITTING after the grace period. Each is settled from the
// chain's status by transaction id through the devnet host -- FINALIZED posts
// and applies, a final FAILED or a NOT_FOUND past blockhash expiry fails and
// releases, anything else waits -- and nothing is ever resubmitted.
//
// ReconcileUnknown owns its transactions, one per request, and takes only the
// requests of its host's network: a request of another network would read as
// NOT_FOUND here and could be failed and released while its transaction
// stands final on its own chain, so it is left to that network's worker. A
// request it cannot settle is recorded with a backoff and reported as that
// request's failure, and the others are settled.
type StableUnknown struct {
	db      *db.DB
	svc     *stableasset.Service
	network string
	spec    Spec
}

// NewStableUnknown wires the job for the requests of one devnet network: the
// one the service's host carries.
func NewStableUnknown(d *db.DB, svc *stableasset.Service, network string, s Settings) (*StableUnknown, error) {
	if err := requireStableNetwork(svc, network); err != nil {
		return nil, err
	}
	return &StableUnknown{db: d, svc: svc, network: network, spec: spec(NameStableUnknown, s)}, nil
}

// Spec implements Job.
func (j *StableUnknown) Spec() Spec { return j.spec }

// Run resolves up to a batch of requests. An error means the pass could not go
// on; what it settled before is committed and counted.
func (j *StableUnknown) Run(ctx context.Context) (Report, error) {
	res, err := j.svc.ReconcileUnknown(ctx, j.spec.Batch)
	return settleReport(res), err
}

// settleReport is a stable-asset pass as a job report: every request it took
// was examined; issued, redeemed, queued and failed ones changed; the ones
// whose transaction is not final wait; the ones it could not act on failed.
func settleReport(res stableasset.SettleReport) Report {
	rep := Report{Examined: res.Examined, Changed: res.Issued + res.Redeemed + res.Queued + res.Failed, Deferred: res.Waiting}
	for _, f := range res.Failures {
		rep.fail(requestUnit(f), f.Err)
	}
	return rep
}

// requestUnit names a request a stable-asset pass could not act on.
func requestUnit(f stableasset.RequestFailure) string {
	switch f.Operation {
	case stableasset.UpkeepSettleMint:
		return "mint " + f.RequestID
	case stableasset.UpkeepPayRedemption:
		return "redemption " + f.RequestID
	}
	return "burn " + f.RequestID
}
