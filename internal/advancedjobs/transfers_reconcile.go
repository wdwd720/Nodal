package advancedjobs

import (
	"context"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/transfers"
)

// Transfers reconciliation scope: one record for the whole register.
const (
	ScopeTransfers   = "transfers"
	ScopeTransfersID = "register"
)

// TransfersReconcile proves, from the database alone, that the transfer
// register, the economic effects and the ledger tell one story
// (transfers.Service.Reconcile: nothing left in flight, every transfer's
// effect and journal agree, per party and asset the ledger's TRANSFER_OUT and
// TRANSFER_IN equal the register, and the customers' WALLET change over every
// transfer journal is zero). Reconcile only reads and runs in a REPEATABLE
// READ snapshot.
//
// The transfers subsystem only reports, so a discrepancy -- every one of
// which it calls a SEV1 -- is recorded here through internal/reconciliation's
// Repository: a material LEDGER_INTERNAL record scoped to the transfer
// register, opened once and refreshed while it stays unresolved. A clean pass
// changes nothing and resolves nothing; resolving a record is a person's
// decision.
type TransfersReconcile struct {
	db    *db.DB
	svc   *transfers.Service
	recon *reconciliation.Repository
	spec  Spec
}

// NewTransfersReconcile wires the job.
func NewTransfersReconcile(d *db.DB, svc *transfers.Service, recon *reconciliation.Repository, s Settings) *TransfersReconcile {
	return &TransfersReconcile{db: d, svc: svc, recon: recon, spec: spec(NameTransfersReconcile, s)}
}

// Spec implements Job.
func (j *TransfersReconcile) Spec() Spec { return j.spec }

// maxRecordedDiscrepancies bounds the discrepancies copied into a record.
const maxRecordedDiscrepancies = 200

// Run reconciles the whole register in one snapshot and records what it
// found.
func (j *TransfersReconcile) Run(ctx context.Context) (Report, error) {
	var rep transfers.ReconcileReport
	if err := inSnapshot(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rep, err = j.svc.Reconcile(ctx, tx)
		return err
	}); err != nil {
		return Report{}, err
	}
	out := Report{Examined: 1}
	if rep.OK() {
		return out, nil
	}
	if err := inUnit(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
		return j.record(ctx, tx, rep)
	}); err != nil {
		out.fail(ScopeTransfersID, err)
		return out, nil
	}
	out.Changed++
	return out, nil
}

func (j *TransfersReconcile) record(ctx context.Context, tx pgx.Tx, rep transfers.ReconcileReport) error {
	codes := map[string]int{}
	for _, d := range rep.Discrepancies {
		codes[d.Code]++
	}
	names := make([]string, 0, len(codes))
	for c := range codes {
		names = append(names, c)
	}
	sort.Strings(names)
	byCode := make([]string, 0, len(names))
	for _, c := range names {
		byCode = append(byCode, c+"="+strconv.Itoa(codes[c]))
	}
	list := rep.Discrepancies
	if len(list) > maxRecordedDiscrepancies {
		list = list[:maxRecordedDiscrepancies]
	}
	expected := map[string]any{
		"claim": "no transfer or transfer effect is left in flight; every transfer's effect and journal agree; " +
			"per party and asset the ledger equals the register; the customers' WALLET change over every transfer journal is zero",
		"transfers": rep.Transfers,
	}
	observed := map[string]any{"checked_at": rep.CheckedAt, "assets": rep.Assets, "discrepancies": list}
	difference := map[string]any{"discrepancies": len(rep.Discrepancies), "by_code": byCode}
	prior, open, err := j.recon.FindUnresolvedByScopeForUpdate(ctx, tx, reconciliation.KindLedgerInternal, ScopeTransfers, ScopeTransfersID)
	if err != nil {
		return err
	}
	if open {
		_, err = j.recon.UpdateObservation(ctx, tx, prior.ID, expected, observed, difference, true, false)
		return err
	}
	_, err = j.recon.Open(ctx, tx, reconciliation.OpenRequest{
		Kind: reconciliation.KindLedgerInternal, Mode: reconciliation.ModePeriodic,
		ScopeType: ScopeTransfers, ScopeID: ScopeTransfersID,
		Expected: expected, Observed: observed, Difference: difference,
		Status: reconciliation.StatusMismatch, Material: true, Actor: workerReconActor(),
		Reason: "the transfer register, its effects and the ledger disagree",
	})
	return err
}
