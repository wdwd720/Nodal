package advancedjobs

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/stableasset"
)

// StableUnknown resolves stable-asset mints and burns whose outcome is unknown
// (stableasset.Service.ReconcileUnknown): requests in SUBMISSION_UNKNOWN, and
// requests still SUBMITTING after the grace period. Each is settled from the
// chain's status by transaction id through the devnet host -- FINALIZED posts
// and applies, a final FAILED or a NOT_FOUND past blockhash expiry fails and
// releases, anything else waits -- and nothing is ever resubmitted.
// ReconcileUnknown owns its transactions, one per request.
//
// ReconcileUnknown takes every such request whatever network its asset is on,
// and reads them all from this worker's host. A request of another network
// would read as NOT_FOUND here and could be failed and released while its
// transaction stands final on its own chain. So the job first checks that
// every request waiting on an outcome belongs to the configured network, and
// refuses to resolve anything until one that does not is dealt with.
type StableUnknown struct {
	db      *db.DB
	svc     *stableasset.Service
	network string
	spec    Spec
}

// NewStableUnknown wires the job for the requests of one devnet network.
func NewStableUnknown(d *db.DB, svc *stableasset.Service, network string, s Settings) (*StableUnknown, error) {
	if err := requireNetwork(network); err != nil {
		return nil, err
	}
	return &StableUnknown{db: d, svc: svc, network: network, spec: spec(NameStableUnknown, s)}, nil
}

// Spec implements Job.
func (j *StableUnknown) Spec() Spec { return j.spec }

// foreignOutcomeRequestsSQL counts the mints and burns ReconcileUnknown
// could take -- SUBMISSION_UNKNOWN, or SUBMITTING of any age -- whose stable
// asset is on another network than $1. Read-only; stableasset exports no
// lister.
const foreignOutcomeRequestsSQL = `SELECT count(*) FROM (
		SELECT stable_asset_id FROM stable_mint_requests WHERE status IN ('SUBMITTING','SUBMISSION_UNKNOWN')
		UNION ALL
		SELECT stable_asset_id FROM stable_burn_requests WHERE status IN ('SUBMITTING','SUBMISSION_UNKNOWN')
	) r JOIN stable_assets s ON s.id = r.stable_asset_id WHERE s.network <> $1`

// Run resolves up to a batch of requests.
func (j *StableUnknown) Run(ctx context.Context) (Report, error) {
	var foreign int
	if err := readOnly(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, foreignOutcomeRequestsSQL, j.network).Scan(&foreign)
	}); err != nil {
		return Report{}, err
	}
	if foreign > 0 {
		return Report{}, errs.Newf(errs.CodeReconciliationRequired,
			"%d stable-asset requests waiting on an outcome are on another network than %s; resolving them from this host would read the wrong chain",
			foreign, j.network)
	}
	res, err := j.svc.ReconcileUnknown(ctx, j.spec.Batch)
	rep := Report{Examined: res.Examined, Changed: res.Issued + res.Redeemed + res.Queued + res.Failed, Deferred: res.Waiting}
	if err != nil {
		// ReconcileUnknown stops at the first request it could not settle;
		// what it settled before that is committed and counted above.
		rep.Examined++
		rep.fail("next unknown submission", err)
	}
	return rep, nil
}
