package reconciliation

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

// Resolver is the PART 51 resolution contract. Every method writes the
// transition row that migration 00603 requires in the same transaction as the
// status change, and every method refuses an AGENT before it touches the
// database.
type Resolver interface {
	Investigate(ctx context.Context, tx pgx.Tx, recordID RecordID, note string) (Record, error)
	ResolveAutomatic(ctx context.Context, tx pgx.Tx, recordID RecordID, cause AutoCause) (Record, error)
	ResolveManual(ctx context.Context, tx pgx.Tx, recordID RecordID, res ManualResolution) (Record, error)
	Escalate(ctx context.Context, tx pgx.Tx, recordID RecordID, reason string) (Record, error)
}

var _ Resolver = (*Engine)(nil)

// Investigate moves a mismatch (or an escalated record) to INVESTIGATING and
// records who picked it up. The principal in ctx is the investigator; an
// agent is refused.
func (e *Engine) Investigate(ctx context.Context, tx pgx.Tx, recordID RecordID, note string) (Record, error) {
	actor, err := e.humanActor(ctx, security.PermReconciliationRead)
	if err != nil {
		return Record{}, err
	}
	if len([]rune(note)) < MinReasonLength {
		return Record{}, errs.New(errs.CodeValidationFailed, "reconciliation: an investigation note is required").
			WithField("note", "at least 8 characters")
	}
	return e.records.Transition(ctx, tx, recordID, StatusInvestigating, TransitionEvidence{
		Actor: actor, Reason: note,
	})
}

// ResolveAutomatic resolves a record for one of the enumerated, deterministic
// causes (RECONCILIATION.md §2). It is the only resolution path that needs no
// human, and it is deliberately narrow:
//
//   - FEE_DUST needs a difference at or below the asset's dust threshold, and
//     posts a RECONCILIATION_ADJUSTMENT only when the policy allows automatic
//     dust posting;
//   - FINALITY_UPGRADE and DUPLICATE_PROVIDER_EVENT have no economic effect;
//   - OBSERVATION_CAUGHT_UP needs the recorded difference to be exactly zero.
//
// A material record can never resolve automatically: materiality is precisely
// the statement that a human must look.
func (e *Engine) ResolveAutomatic(ctx context.Context, tx pgx.Tx, recordID RecordID, cause AutoCause) (Record, error) {
	if !cause.Valid() {
		return Record{}, errs.Newf(errs.CodeValidationFailed, "reconciliation: %q is not an automatic resolution cause", cause).
			WithField("cause", string(cause))
	}
	rec, err := e.records.GetForUpdate(ctx, tx, recordID)
	if err != nil {
		return Record{}, err
	}
	if rec.Material {
		return Record{}, errs.New(errs.CodeForbidden,
			"reconciliation: a material record requires a human resolution").
			WithField("record_id", rec.ID.String())
	}
	// The two causes whose contract is about an AMOUNT are checked here, where
	// the resolution is decided, rather than inside the repair.
	//
	// They were not. The dust threshold was tested inside dustRepair, which is
	// reached only when AutoPostDustAdjustment is true -- and that is false by
	// default and unreachable from configuration. So on the default path
	// FEE_DUST closed a record of ANY size with no amount test at all. And the
	// OBSERVATION_CAUGHT_UP rule, which the comment above states plainly, did
	// not exist anywhere in the function. The only gate was !rec.Material, a
	// plain boolean on a table cp_app may update (F-110).
	//
	// Which direction that resolved in matters: repair.go writes a customer's
	// ledger balance DOWN when the chain holds less, by the SYSTEM actor, with
	// no human.
	if err := e.autoCauseFits(rec, cause); err != nil {
		return Record{}, err
	}
	patch := &ResolutionPatch{
		ResolvedByActorType: security.ActorSystem,
		ResolvedByActorID:   ActorName,
		Reason:              "automatic: " + string(cause),
		EvidenceRef:         "policy:" + e.policy.Version,
	}
	if cause == AutoCauseFeeDust && e.policy.AutoPostDustAdjustment {
		repair, err := e.dustRepair(ctx, rec)
		if err != nil {
			return Record{}, err
		}
		journalID, err := e.applyRepair(ctx, tx, rec, repair)
		if err != nil {
			return Record{}, err
		}
		patch.CompensatingJournalTxID = journalID
	}
	return e.records.Transition(ctx, tx, recordID, StatusResolvedAutomatic, TransitionEvidence{
		Actor: SystemActor(), Reason: patch.Reason, EvidenceRef: patch.EvidenceRef, Patch: patch,
	})
}

// ResolveManual resolves a record with an operator, a reason and evidence, and
// — when the record is material — an approved admin_actions row of kind
// RECONCILIATION_RESOLVE_MATERIAL naming this record (PART 51 dual control).
//
// A resolution that changes financial state does so only through the
// compensating journal transaction in res.Compensation, whose id is stored on
// the record. No balance is edited and no position is overwritten (PART 195).
func (e *Engine) ResolveManual(ctx context.Context, tx pgx.Tx, recordID RecordID, res ManualResolution) (Record, error) {
	if err := res.Validate(); err != nil {
		return Record{}, err
	}
	ctxActor, err := e.humanActor(ctx, security.PermReconciliationResolve)
	if err != nil {
		return Record{}, err
	}
	operator := res.Operator.normalize()
	if ctxActor.ID != operator.ID {
		return Record{}, errs.New(errs.CodeForbidden,
			"reconciliation: the resolving operator must be the authenticated principal").
			WithField("operator_id", operator.ID)
	}
	rec, err := e.records.GetForUpdate(ctx, tx, recordID)
	if err != nil {
		return Record{}, err
	}
	if rec.Material {
		if strings.TrimSpace(res.ApprovalID) == "" {
			return Record{}, errs.New(errs.CodeStepUpRequired,
				"reconciliation: a material record needs an approved admin action to resolve").
				WithField("record_id", rec.ID.String()).WithField("admin_action_kind", string(admin.KindReconciliationResolveMaterial))
		}
		if e.approvals == nil {
			return Record{}, errs.New(errs.CodeInternal, "reconciliation: no approval verifier is configured")
		}
		approval, err := e.approvals.VerifyApproved(ctx, tx, res.ApprovalID, admin.KindReconciliationResolveMaterial, rec.ID.String())
		if err != nil {
			return Record{}, err
		}
		if approval.ApprovedBy != nil && *approval.ApprovedBy == operator.ID {
			return Record{}, errs.New(errs.CodeForbidden,
				"reconciliation: the approver may not also be the resolver").
				WithField("record_id", rec.ID.String())
		}
	}
	patch := &ResolutionPatch{
		ResolvedByActorType: operator.Type,
		ResolvedByActorID:   operator.ID,
		Reason:              res.Reason,
		EvidenceRef:         res.EvidenceRef,
		ApprovalID:          res.ApprovalID,
	}
	if res.Compensation != nil {
		journalID, err := e.applyRepair(ctx, tx, rec, *res.Compensation)
		if err != nil {
			return Record{}, err
		}
		patch.CompensatingJournalTxID = journalID
		if err := e.records.AppendEvidence(ctx, tx, rec, operator, AuditRepairPosted, res.Reason, res.EvidenceRef,
			map[string]any{
				"record_id": rec.ID.String(), "journal_transaction_id": journalID,
				"kind": string(res.Compensation.Posting.Kind), "reason_code": res.Compensation.Posting.ReasonCode(),
			}); err != nil {
			return Record{}, err
		}
	}
	return e.records.Transition(ctx, tx, recordID, StatusResolvedManual, TransitionEvidence{
		Actor: operator, Reason: res.Reason, EvidenceRef: res.EvidenceRef, Patch: patch,
	})
}

// Escalate raises a record to ESCALATED. The engine escalates unattended
// (SYSTEM actor) when a mismatch outlives the policy budget; an operator may
// also escalate explicitly.
func (e *Engine) Escalate(ctx context.Context, tx pgx.Tx, recordID RecordID, reason string) (Record, error) {
	if len([]rune(reason)) < MinReasonLength {
		return Record{}, errs.New(errs.CodeValidationFailed, "reconciliation: an escalation reason is required")
	}
	actor := SystemActor()
	if a, err := operatorActor(ctx); err == nil {
		actor = a
	}
	if err := actor.validate(); err != nil {
		return Record{}, err
	}
	return e.records.Transition(ctx, tx, recordID, StatusEscalated, TransitionEvidence{Actor: actor, Reason: reason})
}

// humanActor resolves the principal in ctx, refuses agents, and checks the
// permission the operation needs. It is the Go half of the rule the database
// also enforces: resolved_by_actor_type is never 'AGENT'.
func (e *Engine) humanActor(ctx context.Context, perm security.Permission) (Actor, error) {
	actor, err := operatorActor(ctx)
	if err != nil {
		return Actor{}, err
	}
	if actor.Type == security.ActorAgent {
		return Actor{}, errs.New(errs.CodeForbidden, "reconciliation: an agent may never resolve a reconciliation record")
	}
	if err := security.Require(ctx, perm); err != nil {
		return Actor{}, err
	}
	return actor, nil
}

// dustRepair builds the RECONCILIATION_ADJUSTMENT for a dust difference. The
// difference is read back from the record's own document, so an operator and
// the posting can never disagree about what is being corrected.
// autoCauseFits enforces the per-cause conditions ResolveAutomatic's contract
// states.
//
// A record with no difference recorded is refused for both amount-bearing
// causes rather than treated as zero: "nobody wrote down what the difference
// was" is not the same fact as "the difference was nothing", and only one of
// them justifies closing the record without a person.
func (e *Engine) autoCauseFits(rec Record, cause AutoCause) error {
	switch cause {
	case AutoCauseFeeDust:
		diff, err := recordDifferenceQuantity(rec)
		if err != nil {
			return err
		}
		if rec.AssetID.IsZero() {
			return errs.New(errs.CodeValidationFailed,
				"reconciliation: FEE_DUST needs an asset-scoped record to have a dust threshold at all").
				WithField("record_id", rec.ID.String())
		}
		if !e.policy.IsDust(rec.AssetID, diff) {
			return errs.New(errs.CodeForbidden,
				"reconciliation: the difference is above the asset's dust threshold and is not dust").
				WithField("record_id", rec.ID.String()).WithField("difference", diff.String())
		}
	case AutoCauseObservationCaughtUp:
		diff, err := recordDifferenceQuantity(rec)
		if err != nil {
			return err
		}
		if !diff.IsZero() {
			return errs.New(errs.CodeForbidden,
				"reconciliation: OBSERVATION_CAUGHT_UP means the observation now agrees, so the recorded difference must be exactly zero").
				WithField("record_id", rec.ID.String()).WithField("difference", diff.String())
		}
	}
	// FINALITY_UPGRADE and DUPLICATE_PROVIDER_EVENT are statements about how an
	// observation was made rather than about an amount, and the contract claims
	// no amount condition for them.
	return nil
}

func (e *Engine) dustRepair(ctx context.Context, rec Record) (Repair, error) {
	diff, err := recordDifferenceQuantity(rec)
	if err != nil {
		return Repair{}, err
	}
	if rec.AccountID.IsZero() || rec.AssetID.IsZero() {
		return Repair{}, errs.New(errs.CodeValidationFailed,
			"reconciliation: a dust adjustment needs an account-scoped, asset-scoped record").
			WithField("record_id", rec.ID.String())
	}
	if !e.policy.IsDust(rec.AssetID, diff) {
		return Repair{}, errs.New(errs.CodeForbidden, "reconciliation: the difference is above the dust threshold").
			WithField("record_id", rec.ID.String()).WithField("difference", diff.String())
	}
	a, err := e.asset(ctx, rec.AssetID)
	if err != nil {
		return Repair{}, err
	}
	in := BalanceRepairInputs{
		RecordID: rec.ID, AccountID: rec.AccountID, AssetID: rec.AssetID, Difference: diff,
		ReasonCode: "DUST", Description: "automatic dust adjustment", EffectiveAt: e.clk.Now(),
		USDValue: e.valueUSD(ctx, a, diff, e.clk.Now()), CorrelationID: rec.CorrelationID,
	}
	repair, err := BalanceRepair(in)
	if err != nil {
		return Repair{}, err
	}
	if e.positions != nil {
		return repair.WithPositionRepair(in, "reconciliation:dust")
	}
	return repair, nil
}
