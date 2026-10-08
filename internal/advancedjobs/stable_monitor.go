package advancedjobs

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/stableasset"
)

// StableMonitor runs the peg monitor (stableasset.Service.RunMonitor) for
// every stable asset on the configured network: the market price against the
// peg, the reserve coverage and the supply on the books against the chain and
// the wallets. A deviation above threshold, coverage under 100% or a supply
// mismatch trips the asset's own circuit breaker (ACTIVE -> PAUSED) with an
// alert; the monitor never resumes an asset.
type StableMonitor struct {
	db      *db.DB
	svc     *stableasset.Service
	network string
	spec    Spec
}

// NewStableMonitor wires the job for the stable assets of one devnet network.
func NewStableMonitor(d *db.DB, svc *stableasset.Service, network string, s Settings) (*StableMonitor, error) {
	if err := requireStableNetwork(svc, network); err != nil {
		return nil, err
	}
	return &StableMonitor{db: d, svc: svc, network: network, spec: spec(NameStableMonitor, s)}, nil
}

// Spec implements Job.
func (j *StableMonitor) Spec() Spec { return j.spec }

// Run monitors each asset in its own transaction. An asset whose run raised
// an alert or tripped its breaker counts as changed.
func (j *StableMonitor) Run(ctx context.Context) (Report, error) {
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
		var res stableasset.MonitorReport
		err := inUnit(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			res, err = j.svc.RunMonitor(ctx, tx, sid, workerActor())
			return err
		})
		if err != nil {
			rep.fail(sid.String(), err)
			continue
		}
		if res.BreakerTripped || len(res.Alerts) > 0 {
			rep.Changed++
		}
	}
	return rep, nil
}
