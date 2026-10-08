package marketplace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/sandboxrt"
)

// MaxSimulationSnapshots is the most snapshots any one evaluation may have.
// The sandbox budget bounds a call more tightly still (sandboxrt.Budget's
// runs and CPU per call, 20 runs by default): a simulation is never a
// thousand runs in one transaction (review-2 F13).
const MaxSimulationSnapshots = 100

// SimulationRequest runs a version against a fixed historical snapshot set.
type SimulationRequest struct {
	VersionID VersionID
	// AccountID is the account the simulation is run for and charged to:
	// the listing creator's account, by its owner.
	AccountID accounts.AccountID
	// DatasetVersion names the snapshot set, so the evaluation can be
	// reproduced from the same data.
	DatasetVersion string
	Snapshots      []sandboxrt.Input
	Actor          econ.Actor
}

// EvaluationRun is one snapshot of an evaluation.
type EvaluationRun struct {
	Seq          int                   `json:"seq"`
	SandboxRunID sandboxrt.RunID       `json:"sandbox_run_id"`
	AsOf         time.Time             `json:"as_of"`
	InputDigest  string                `json:"input_digest"`
	RunDigest    string                `json:"run_digest"`
	Termination  sandboxrt.Termination `json:"termination"`
	Proposals    int                   `json:"proposals"`
	Accepted     int                   `json:"accepted"`
	Violations   int                   `json:"violations"`
}

// Evaluation is a recorded simulation. Simulations propose nothing: their
// proposals are counted and kept in the run log, never submitted.
type Evaluation struct {
	ID        EvaluationID
	VersionID VersionID
	// AccountID is the account the simulation was run for and charged to
	// (zero for an evaluation recorded before review-2 F13).
	AccountID       accounts.AccountID
	Kind            EvaluationKind
	DatasetVersion  string
	SnapshotsDigest []byte
	ResultsDigest   []byte
	RunsOK          int
	ProposalsCount  int
	AcceptedCount   int
	ViolationsCount int
	Runs            []EvaluationRun
	EvidenceRef     string
	EvidenceSHA256  []byte
}

// Simulate runs a built version (VALIDATED, IN_REVIEW, APPROVED or
// DEPRECATED) against every snapshot, in order, and records the evaluation:
// the snapshots digest (sha256 of the input digests in order) and the
// results digest (sha256 of the run digests in order) make it reproducible
// from the dataset.
//
// A simulation is the listing creator's, run by the owner of the creator's
// account and charged to it. It is admitted whole or not at all: a call with
// more runs or more granted CPU than one call may have, or than the
// account's sandbox window has left, is refused before the first run starts
// (RATE_LIMITED with the budget's stable reason).
func (s *Service) Simulate(ctx context.Context, tx pgx.Tx, req SimulationRequest) (Evaluation, error) {
	if err := requireActor(req.Actor, TransitionActorTypes(), "simulate a strategy"); err != nil {
		return Evaluation{}, err
	}
	if d := strings.TrimSpace(req.DatasetVersion); d == "" || d != req.DatasetVersion || len(d) > 100 {
		return Evaluation{}, errs.New(errs.CodeValidationFailed, "marketplace: a dataset version (1..100 characters) is required")
	}
	if len(req.Snapshots) == 0 || len(req.Snapshots) > MaxSimulationSnapshots {
		return Evaluation{}, errs.Newf(errs.CodeValidationFailed, "marketplace: a simulation needs 1..%d snapshots", MaxSimulationSnapshots)
	}
	v, err := s.Version(ctx, tx, req.VersionID)
	if err != nil {
		return Evaluation{}, err
	}
	l, err := s.Listing(ctx, tx, v.ListingID)
	if err != nil {
		return Evaluation{}, err
	}
	if req.AccountID != l.CreatorAccountID {
		return Evaluation{}, errs.New(errs.CodeForbidden, "marketplace: a simulation is run for the listing creator's account").
			WithField("version_id", v.ID.String())
	}
	if err := requireOwner(ctx, tx, req.Actor, req.AccountID, "simulate a strategy"); err != nil {
		return Evaluation{}, err
	}
	switch v.Status {
	case StatusValidated, StatusInReview, StatusApproved, StatusDeprecated:
	default:
		return Evaluation{}, errs.Newf(errs.CodeInvalidStateTransition, "marketplace: a %s version has no artifact to simulate", v.Status)
	}
	if err := s.authorize(ctx, tx, gates.ModeSandbox); err != nil {
		return Evaluation{}, err
	}
	val, ok, err := s.ValidationOf(ctx, tx, v.ID)
	if err != nil {
		return Evaluation{}, err
	}
	if !ok || val.Outcome != ValidationPassed {
		return Evaluation{}, errs.New(errs.CodeInternal, "marketplace: a built version has no passed validation")
	}
	if err := s.cfg.Sandbox.Admit(ctx, tx, req.AccountID, len(req.Snapshots), s.cfg.Limits); err != nil {
		return Evaluation{}, err
	}
	ev := Evaluation{ID: id.New[evaluationKind](), VersionID: v.ID, AccountID: req.AccountID, Kind: EvaluationSimulation,
		DatasetVersion: req.DatasetVersion}
	snapHash, runHash := sha256.New(), sha256.New()
	for i, snap := range req.Snapshots {
		in := snap
		in.Config = make(map[string]string, len(v.Config))
		for k, val := range v.Config {
			in.Config[k] = val
		}
		exec, err := s.cfg.Sandbox.Execute(ctx, tx, sandboxrt.ExecuteRequest{ArtifactDigest: val.ArtifactDigest, Input: in, Limits: s.cfg.Limits,
			Manifest: v.Manifest, StrategyRef: v.StrategyRef(), Actor: req.Actor, AccountID: req.AccountID, Purpose: sandboxrt.PurposeSimulation})
		if err != nil {
			return Evaluation{}, err
		}
		snapHash.Write(exec.Result.InputDigest)
		runHash.Write(exec.Result.RunDigest)
		if exec.Result.Termination == sandboxrt.TerminationOK {
			ev.RunsOK++
		}
		ev.ProposalsCount += len(exec.Result.Proposals)
		ev.AcceptedCount += len(exec.Accepted)
		ev.ViolationsCount += len(exec.Violations)
		ev.Runs = append(ev.Runs, EvaluationRun{Seq: i + 1, SandboxRunID: exec.RunID, AsOf: in.AsOf.UTC(),
			InputDigest: hex.EncodeToString(exec.Result.InputDigest), RunDigest: hex.EncodeToString(exec.Result.RunDigest),
			Termination: exec.Result.Termination, Proposals: len(exec.Result.Proposals), Accepted: len(exec.Accepted), Violations: len(exec.Violations)})
	}
	ev.SnapshotsDigest, ev.ResultsDigest = snapHash.Sum(nil), runHash.Sum(nil)
	ref, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "marketplace/evaluation/" + ev.ID.String(), Document: map[string]any{
		"evaluation_id": ev.ID.String(), "version_id": v.ID.String(), "strategy_ref": v.StrategyRef(), "kind": string(ev.Kind),
		"dataset_version": ev.DatasetVersion, "manifest_hash": hex.EncodeToString(v.ManifestHash),
		"artifact_digest": hex.EncodeToString(val.ArtifactDigest), "snapshots_digest": hex.EncodeToString(ev.SnapshotsDigest),
		"results_digest": hex.EncodeToString(ev.ResultsDigest), "runs": ev.Runs,
	}})
	if err != nil {
		return Evaluation{}, err
	}
	ev.EvidenceRef, ev.EvidenceSHA256 = ref.URI, ref.SHA256
	if _, err := tx.Exec(ctx, `INSERT INTO marketplace_evaluations (id, version_id, kind, dataset_version, snapshots_count, snapshots_digest,
		results_digest, runs_ok, proposals_count, accepted_count, violations_count, evidence_ref, evidence_sha256, actor_type, actor_id,
		account_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		ev.ID, v.ID, ev.Kind, ev.DatasetVersion, len(req.Snapshots), ev.SnapshotsDigest, ev.ResultsDigest, ev.RunsOK, ev.ProposalsCount,
		ev.AcceptedCount, ev.ViolationsCount, ev.EvidenceRef, ev.EvidenceSHA256, req.Actor.Type, req.Actor.ID, req.AccountID); err != nil {
		return Evaluation{}, dbErr("record evaluation", err)
	}
	for _, r := range ev.Runs {
		if _, err := tx.Exec(ctx, `INSERT INTO marketplace_evaluation_runs (id, evaluation_id, seq, sandbox_run_id) VALUES ($1,$2,$3,$4)`,
			id.New[id.Any](), ev.ID, r.Seq, r.SandboxRunID); err != nil {
			return Evaluation{}, dbErr("record evaluation run", err)
		}
	}
	if err := s.auditEvent(ctx, tx, audit.SystemStream, req.Actor, "marketplace.evaluation.recorded", "marketplace_evaluation", ev.ID.String(),
		req.DatasetVersion, map[string]any{"version_id": v.ID.String(), "snapshots": len(req.Snapshots), "runs_ok": ev.RunsOK}); err != nil {
		return Evaluation{}, err
	}
	return ev, nil
}
