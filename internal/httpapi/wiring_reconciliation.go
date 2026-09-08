package httpapi

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/security"
)

// Reconciliation, from the admin plane.
//
// # Why this exists
//
// The two halves of reconciliation were wired asymmetrically, and the gap was a
// trap for customers (F-52). `cmd/reconciliation-worker` raises records on a
// ticker, and `blocks_new_risk` is read by buying power — so an automated check
// can remove an account's capacity to trade. The API set `Reconcile: nil`, so
// both admin endpoints answered UNSUPPORTED and `Engine.ResolveManual` had no
// caller outside its own tests.
//
// A deployment could freeze somebody's account and had no button to unfreeze
// it. The only recourse was a hand-written UPDATE against the database, which
// is the thing the admin plane exists to replace. That is F-29 one level up:
// there it was money reserved with no path to release, here it is capacity.
//
// # What is wired, and what is deliberately not
//
// Listing and resolution are. A resolution moves the record to
// RESOLVED_MANUAL, which is not one of the blocking statuses, so the account
// gets its capacity back — which is the whole point.
//
// A COMPENSATING POSTING is not. `ManualResolution.Compensation` is the only
// way a resolution changes financial state (PART 195), and building one from an
// API request means choosing the posting's kind, its idempotency key and its
// reference, and mapping each entry's account code onto an owner — decisions
// with financial consequences that deserve their own design, their own tests
// and their own review. Asking for one here is refused, by name, rather than
// approximated.
//
// The refusal is honest in the direction that matters: an operator who needs to
// unblock an account can, and an operator who needs to move value cannot do it
// by accident through a path nobody designed.

// ReconciliationDeps are the services behind the admin reconciliation routes.
type ReconciliationDeps struct {
	// Engine resolves records. Nil leaves the routes answering UNSUPPORTED,
	// which is what a deployment without it should say.
	Engine *reconciliation.Engine
	// ReadModel serves the listing. It reads the same table the worker writes.
	ReadModel *ReadModel
	DB        *db.DB
}

type reconciliationAdapter struct {
	deps ReconciliationDeps
}

// NewReconciliationPort returns the admin reconciliation port, or nil when the
// deployment has no engine — a nil port is what makes the routes answer
// UNSUPPORTED rather than fail later.
func NewReconciliationPort(d ReconciliationDeps) ReconciliationPort {
	if d.Engine == nil || d.ReadModel == nil || d.DB == nil {
		return nil
	}
	return reconciliationAdapter{deps: d}
}

func (a reconciliationAdapter) List(ctx context.Context, status string, accountID *accounts.AccountID, cursor string, limit int) (ReconciliationPage, error) {
	return a.deps.ReadModel.ListReconciliationRecords(ctx, status, accountID, cursor, limit)
}

// Resolve records a manual resolution and returns the record as it now stands.
//
// The operator is taken from the principal, never from the request: the engine
// re-derives it too and refuses a resolution whose named operator is not the
// authenticated one, so passing it through the body would only create a way for
// the two to disagree.
func (a reconciliationAdapter) Resolve(ctx context.Context, recordID string, res ReconciliationResolution) (ReconciliationRecord, error) {
	if res.Compensation != nil {
		return ReconciliationRecord{}, errs.New(errs.CodeUnsupported,
			"a compensating posting cannot be made through this endpoint in this deployment; "+
				"the record can be resolved without one, and a correction that moves value needs a "+
				"ledger correction proposed and approved in its own right").
			WithField("record_id", recordID)
	}
	id, err := reconciliation.ParseRecordID(recordID)
	if err != nil {
		return ReconciliationRecord{}, validationError("recordId", "recordId must be a canonical UUID")
	}
	actor, err := reconciliationActor(ctx)
	if err != nil {
		return ReconciliationRecord{}, err
	}

	// The state machine requires INVESTIGATING before RESOLVED_MANUAL, and says
	// why: somebody looked at the evidence before resolving. A worker-raised
	// record is MISMATCH or, after the escalation sweep, ESCALATED — so a
	// resolve-only endpoint could not clear a single record the worker actually
	// produces, and F-52 would have been fixed in name only.
	//
	// TWO TRANSACTIONS, not one. The repository refuses more than one status
	// change per transaction ("a record may change status at most once per
	// transaction"), which keeps each transition bound to exactly one audit row.
	// So the investigate commits first and the resolution follows.
	//
	// If the second fails the record is left INVESTIGATING, which still blocks —
	// the operator has lost nothing and repeating the same call finishes the
	// job, because the first step is skipped for a record already there. The
	// alternative, holding both in one transaction, would need the domain's own
	// rule relaxed, and that rule is what makes each transition auditable.
	if err := a.investigateIfNeeded(ctx, id, res); err != nil {
		return ReconciliationRecord{}, err
	}

	var rec reconciliation.Record
	if err := a.deps.DB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		var rerr error
		rec, rerr = a.deps.Engine.ResolveManual(ctx, tx, id, reconciliation.ManualResolution{
			Operator:    actor,
			Reason:      res.Reason,
			EvidenceRef: res.EvidenceRef,
			ApprovalID:  res.ApprovalID,
		})
		return rerr
	}); err != nil {
		return ReconciliationRecord{}, err
	}
	// Projected from what the engine returned, not re-read. The engine's
	// Record IS the post-resolution state; a second read could see a later one
	// and report something the caller did not cause.
	return toPortReconciliationRecord(rec), nil
}

// investigateIfNeeded moves a record into INVESTIGATING when that is the step
// standing between it and a resolution.
//
// It does not force the path for a record that cannot take it. An OPEN record
// is deliberately not operator-resolvable: OPEN means the engine has not yet
// decided whether there is a mismatch at all, and its only transitions are the
// engine's own (MATCHED, MISMATCH). Trying anyway would produce a confusing
// refusal from two levels down, so the state machine's own error is left to
// speak.
func (a reconciliationAdapter) investigateIfNeeded(ctx context.Context, id reconciliation.RecordID, res ReconciliationResolution) error {
	return a.deps.DB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		current, err := a.deps.Engine.Records().GetForUpdate(ctx, tx, id)
		if err != nil {
			return err
		}
		switch current.Status {
		case reconciliation.StatusMismatch, reconciliation.StatusEscalated:
			_, err = a.deps.Engine.Investigate(ctx, tx, id, res.Reason)
			return err
		default:
			return nil
		}
	})
}

// toPortReconciliationRecord projects the domain record onto the API shape.
//
// The three JSON documents are handed over as decoded objects, unchanged. A
// malformed one becomes nil rather than an error: they are evidence an operator
// reads, and refusing to show a resolved record because its `difference` blob
// will not decode would be refusing the one thing the operator came for.
func toPortReconciliationRecord(r reconciliation.Record) ReconciliationRecord {
	out := ReconciliationRecord{
		ID: r.ID.String(), Kind: string(r.Kind), Mode: string(r.Mode),
		ScopeType: r.ScopeType, ScopeID: r.ScopeID,
		Expected: decodeJSONObject(r.Expected), Observed: decodeJSONObject(r.Observed),
		Difference:    decodeJSONObject(r.Difference),
		Status:        string(r.Status),
		Material:      r.Material,
		BlocksNewRisk: r.BlocksNewRisk,
		OpenedAt:      r.OpenedAt, ResolvedAt: r.ResolvedAt,
		ResolutionReason:      r.ResolutionReason,
		ResolutionEvidenceRef: r.ResolutionEvidenceRef,
		CompensatingJournalTx: r.CompensatingJournalTxID,
	}
	if !r.AccountID.IsZero() {
		out.AccountID = r.AccountID.String()
	}
	if !r.AssetID.IsZero() {
		out.AssetID = r.AssetID.String()
	}
	return out
}

func decodeJSONObject(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// reconciliationActor is the authenticated principal, as the engine's own
// operator type.
func reconciliationActor(ctx context.Context) (reconciliation.Actor, error) {
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return reconciliation.Actor{}, errs.New(errs.CodeUnauthenticated, "no principal in context")
	}
	return reconciliation.Actor{Type: p.ActorType, ID: p.SubjectID}, nil
}
