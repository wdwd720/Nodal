package advancedjobs

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/stableasset"
)

// StableSupply runs reconciliation kind STABLE_RESERVE
// (stableasset.Service.ReconcileSupply, PERIODIC) for every stable asset on
// the configured network: the ledger's outstanding supply against the
// customers' wallets, the devnet supply of the mint and the verified reserve.
// A mismatch opens or refreshes a material record that blocks new risk and
// trips the asset's breaker; a match is recorded (one MATCHED record per
// matched pass, which is why the default interval is an hour).
type StableSupply struct {
	db      *db.DB
	svc     *stableasset.Service
	network string
	spec    Spec
}

// NewStableSupply wires the job for the stable assets of one devnet network.
func NewStableSupply(d *db.DB, svc *stableasset.Service, network string, s Settings) (*StableSupply, error) {
	if err := requireNetwork(network); err != nil {
		return nil, err
	}
	return &StableSupply{db: d, svc: svc, network: network, spec: spec(NameStableSupply, s)}, nil
}

// Spec implements Job.
func (j *StableSupply) Spec() Spec { return j.spec }

// Run reconciles each asset in its own transaction; a recorded mismatch
// counts as changed.
func (j *StableSupply) Run(ctx context.Context) (Report, error) {
	ids, err := stableAssetsOn(ctx, j.db, j.network, j.spec.Batch)
	if err != nil {
		return Report{}, err
	}
	var rep Report
	for _, sid := range ids {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		rep.Examined++
		var res stableasset.SupplyReconciliation
		err := inUnit(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			res, err = j.svc.ReconcileSupply(ctx, tx, sid, reconciliation.ModePeriodic, workerActor())
			return err
		})
		if err != nil {
			rep.fail(sid.String(), err)
			continue
		}
		if !res.Matched() {
			rep.Changed++
		}
	}
	return rep, nil
}
