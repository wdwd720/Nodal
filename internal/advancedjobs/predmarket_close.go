package advancedjobs

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/predmarket"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/security"
)

// WorkerActorID names the worker as the SYSTEM actor of the work it starts
// on its own.
const WorkerActorID = "advanced-worker"

func workerActor() econ.Actor { return econ.SystemActor(WorkerActorID) }

func workerReconActor() reconciliation.Actor {
	return reconciliation.Actor{Type: security.ActorSystem, ID: WorkerActorID}
}

// PredictionClose closes prediction markets whose close time has passed
// (predmarket.Service.CloseDue, the scheduler's path; an operator may close
// one by hand through the API): the outcome instruments go HALTED and the
// market instrument CLOSE_ONLY, so nothing is issued, redeemed or traded after
// close. Closing only removes activity, needs no authorization and is never
// stopped by a kill switch.
type PredictionClose struct {
	db   *db.DB
	svc  *predmarket.Service
	spec Spec
}

// NewPredictionClose wires the job.
func NewPredictionClose(d *db.DB, svc *predmarket.Service, s Settings) *PredictionClose {
	return &PredictionClose{db: d, svc: svc, spec: spec(NamePredictionClose, s)}
}

// Spec implements Job.
func (j *PredictionClose) Spec() Spec { return j.spec }

// Run drains CloseDueBatch one market per transaction until nothing is due
// or the batch is spent. CloseDueBatch takes the earliest due market another
// closer does not hold (SKIP LOCKED) and isolates a market it cannot close --
// rolled back alone, recorded with a backoff and left out until the backoff
// has passed -- so the drain reports it as that market's failure and goes on.
// Only a transaction that fails as a whole ends the drain.
func (j *PredictionClose) Run(ctx context.Context) (Report, error) {
	var rep Report
	for rep.Examined < j.spec.Batch {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		var batch predmarket.CloseBatch
		err := inUnit(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			batch, err = j.svc.CloseDueBatch(ctx, tx, workerActor(), 1)
			return err
		})
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return rep, ctxErr
			}
			rep.Examined++
			rep.fail("next due market", err)
			return rep, nil
		}
		if len(batch.Closed)+len(batch.Failures) == 0 {
			return rep, nil
		}
		rep.Examined += len(batch.Closed) + len(batch.Failures)
		rep.Changed += len(batch.Closed)
		for _, f := range batch.Failures {
			rep.fail("market "+f.MarketID.String(), f.Err)
		}
	}
	return rep, nil
}
