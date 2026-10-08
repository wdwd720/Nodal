package marketplace

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxrt"
	nodalsdk "github.com/nodal/controlplane/internal/sandboxrt/sdk"
	"github.com/nodal/controlplane/internal/settlement"
)

// DispatchEffectKind is the economic effect kind of handing one intent to
// paper execution.
const DispatchEffectKind = "PAPER_DISPATCH"

// FollowRequest is one paper-follow run of an install.
type FollowRequest struct {
	InstallID InstallID
	// Input is the snapshot the strategy runs on. Its configuration is
	// replaced by the version's approved configuration.
	Input         sandboxrt.Input
	Trigger       FollowTrigger
	Actor         econ.Actor
	CorrelationID string
}

// FollowIntent is what became of one proposal of a follow run.
type FollowIntent struct {
	ProposalSeq  int             `json:"proposal_seq"`
	IntentID     econ.IntentID   `json:"intent_id"`
	Kind         econ.IntentKind `json:"kind"`
	Outcome      FollowOutcome   `json:"outcome"`
	DecisionHash string          `json:"decision_hash"`
	ReasonCodes  []string        `json:"reason_codes"`
	EffectID     *econ.EffectID  `json:"effect_id"`
	Notional     string          `json:"notional"`
	ExternalRef  string          `json:"external_ref"`
}

// FollowRun is a recorded paper-follow run.
type FollowRun struct {
	ID                  FollowRunID
	InstallID           InstallID
	VersionID           VersionID
	AccountID           accounts.AccountID
	SandboxRunID        sandboxrt.RunID
	Trigger             FollowTrigger
	AsOf                time.Time
	InputDigest         []byte
	RunDigest           []byte
	Termination         sandboxrt.Termination
	ProposalsCount      int
	AcceptedCount       int
	ViolationsCount     int
	IntentsSubmitted    int
	IntentsDispatched   int
	IntentsRiskRejected int
	IntentsFailed       int
	// Equity is the snapshot portfolio's exact value; Turnover the exact
	// notional dispatched. Both in simulated dollars.
	Equity         string
	Turnover       string
	EvidenceRef    string
	EvidenceSHA256 []byte
	Intents        []FollowIntent
	// Replayed is true when the run was recorded by an earlier call with the
	// same snapshot: nothing was run or dispatched again.
	Replayed  bool
	CreatedAt time.Time
}

// PaperFollow runs an installed version on one snapshot and puts every
// proposal its manifest permits through the same path as any other intent:
// econ intent (SERVICE, PAPER, STRATEGY_MARKETPLACE) -> settlement compiler
// -> Risk V2 with the strategy and sandbox sections -> exactly-once dispatch
// to the IntentExecutor, only on ALLOW. It records the follow run, what
// became of each intent, the exact equity and turnover, and an evidence
// document, in the caller's transaction.
//
// One snapshot of one install is one follow run: calling again with the same
// as-of time and the same input returns the recorded run and does nothing;
// the same as-of with a different input is INVALID_IDEMPOTENCY_REUSE.
func (s *Service) PaperFollow(ctx context.Context, tx pgx.Tx, req FollowRequest) (FollowRun, error) {
	if err := requireActor(req.Actor, TransitionActorTypes(), "follow a strategy"); err != nil {
		return FollowRun{}, err
	}
	if req.Trigger != TriggerExplicit && req.Trigger != TriggerScheduled {
		return FollowRun{}, errs.Newf(errs.CodeValidationFailed, "marketplace: unknown follow trigger %q", req.Trigger)
	}
	if err := checkEconTransitionsImmediately(ctx, tx); err != nil {
		return FollowRun{}, err
	}
	// The install row lock serializes every follow of one install.
	install, err := s.loadInstall(ctx, tx, req.InstallID, true)
	if err != nil {
		return FollowRun{}, err
	}
	if install.Status != InstallActive || install.Mode != gates.ModePaper {
		return FollowRun{}, errs.Newf(errs.CodeForbidden, "marketplace: install is %s/%s", install.Status, install.Mode)
	}
	v, err := s.Version(ctx, tx, install.VersionID)
	if err != nil {
		return FollowRun{}, err
	}
	if !v.Followable() {
		return FollowRun{}, errs.Newf(errs.CodeForbidden, "marketplace: a %s/%s version cannot be followed", v.Status, v.Moderation).
			WithField("version_id", v.ID.String())
	}
	in := req.Input
	in.Config = make(map[string]string, len(v.Config))
	for k, val := range v.Config {
		in.Config[k] = val
	}
	inputDigest, err := in.Digest()
	if err != nil {
		return FollowRun{}, err
	}
	asOf := in.AsOf.UTC()
	if existing, found, err := s.followAt(ctx, tx, install.ID, asOf); err != nil {
		return FollowRun{}, err
	} else if found {
		if !bytes.Equal(existing.InputDigest, inputDigest) {
			return FollowRun{}, errs.New(errs.CodeInvalidIdempotencyReuse, "marketplace: this snapshot time was already followed with a different input").
				WithField("follow_run_id", existing.ID.String())
		}
		existing.Replayed = true
		return existing, nil
	}
	now := s.now()
	if asOf.After(now) || now.Sub(asOf) > s.cfg.MaxSnapshotAge {
		return FollowRun{}, errs.New(errs.CodeStaleMarketData, "marketplace: the snapshot is stale or from the future").
			WithField("as_of", asOf).WithField("now", now)
	}
	var last *time.Time
	if err := tx.QueryRow(ctx, `SELECT max(as_of) FROM marketplace_follow_runs WHERE install_id = $1`, install.ID).Scan(&last); err != nil {
		return FollowRun{}, dbErr("last follow", err)
	}
	if last != nil {
		next := last.UTC().Add(time.Duration(v.Manifest.MinIntervalSeconds) * time.Second)
		if asOf.Before(next) {
			return FollowRun{}, errs.New(errs.CodeRateLimited, "marketplace: the manifest's execution frequency does not allow another run yet").
				WithField("next_allowed_at", next)
		}
	}
	if err := s.authorize(ctx, tx, gates.ModePaper); err != nil {
		return FollowRun{}, err
	}
	val, ok, err := s.ValidationOf(ctx, tx, v.ID)
	if err != nil {
		return FollowRun{}, err
	}
	if !ok || val.Outcome != ValidationPassed {
		return FollowRun{}, errs.New(errs.CodeInternal, "marketplace: an approved version has no passed validation")
	}
	exec, err := s.cfg.Sandbox.Execute(ctx, tx, sandboxrt.ExecuteRequest{
		ArtifactDigest: val.ArtifactDigest, Input: in, Limits: s.cfg.Limits, Manifest: v.Manifest,
		StrategyRef: v.StrategyRef(), Actor: req.Actor, CorrelationID: req.CorrelationID,
	})
	if err != nil {
		return FollowRun{}, err
	}

	run := FollowRun{
		ID: id.New[followKind](), InstallID: install.ID, VersionID: v.ID, AccountID: install.AccountID, SandboxRunID: exec.RunID,
		Trigger: req.Trigger, AsOf: asOf, InputDigest: inputDigest, RunDigest: exec.Result.RunDigest,
		Termination: exec.Result.Termination, ProposalsCount: len(exec.Result.Proposals), AcceptedCount: len(exec.Accepted),
		ViolationsCount: len(exec.Violations),
	}
	turnover := nodalsdk.Decimal{}
	for _, a := range exec.Accepted {
		fi, err := s.followIntent(ctx, tx, install, v, exec, run.ID, a, req.CorrelationID)
		if err != nil {
			return FollowRun{}, err
		}
		run.Intents = append(run.Intents, fi)
		run.IntentsSubmitted++
		switch fi.Outcome {
		case OutcomeDispatched:
			run.IntentsDispatched++
			n, err := nodalsdk.ParseDecimal(fi.Notional)
			if err != nil {
				return FollowRun{}, errs.Wrap(err, errs.CodeInternal, "marketplace: notional unreadable")
			}
			turnover = turnover.Add(n)
		case OutcomeRiskRejected:
			run.IntentsRiskRejected++
		default:
			run.IntentsFailed++
		}
	}
	equity, err := in.Equity()
	if err != nil {
		return FollowRun{}, err
	}
	run.Equity, run.Turnover = equity.String(), turnover.String()

	canonicalInput, err := in.Canonical()
	if err != nil {
		return FollowRun{}, err
	}
	ev, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "marketplace/follow/" + run.ID.String(), Document: followEvidence{
		FollowRunID: run.ID.String(), InstallID: install.ID.String(), VersionID: v.ID.String(), AccountID: install.AccountID.String(),
		StrategyRef: v.StrategyRef(), ManifestHash: hex.EncodeToString(v.ManifestHash), ArtifactDigest: hex.EncodeToString(val.ArtifactDigest),
		SandboxRunID: exec.RunID.String(), InputDigest: hex.EncodeToString(inputDigest), RunDigest: hex.EncodeToString(exec.Result.RunDigest),
		OutputDigest: hex.EncodeToString(exec.Result.OutputDigest), Termination: string(exec.Result.Termination),
		Input: json.RawMessage(canonicalInput), Violations: violationsOrEmpty(exec.Violations), Intents: intentsOrEmpty(run.Intents),
		Equity: run.Equity, Turnover: run.Turnover, AsOf: asOf, Trigger: string(req.Trigger),
	}})
	if err != nil {
		return FollowRun{}, err
	}
	run.EvidenceRef, run.EvidenceSHA256 = ev.URI, ev.SHA256
	if err := tx.QueryRow(ctx, `INSERT INTO marketplace_follow_runs (id, install_id, version_id, account_id, sandbox_run_id, trigger, as_of,
		input_digest, run_digest, proposals_count, accepted_count, violations_count, intents_submitted, intents_dispatched,
		intents_risk_rejected, intents_failed, equity, turnover, evidence_ref, evidence_sha256, actor_type, actor_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17::numeric,$18::numeric,$19,$20,$21,$22) RETURNING created_at`,
		run.ID, run.InstallID, run.VersionID, run.AccountID, run.SandboxRunID, run.Trigger, run.AsOf, run.InputDigest, run.RunDigest,
		run.ProposalsCount, run.AcceptedCount, run.ViolationsCount, run.IntentsSubmitted, run.IntentsDispatched, run.IntentsRiskRejected,
		run.IntentsFailed, run.Equity, run.Turnover, run.EvidenceRef, run.EvidenceSHA256, req.Actor.Type, req.Actor.ID).Scan(&run.CreatedAt); err != nil {
		return FollowRun{}, dbErr("record follow run", err)
	}
	run.CreatedAt = run.CreatedAt.UTC()
	for _, fi := range run.Intents {
		var decision any
		if fi.DecisionHash != "" {
			decision = fi.DecisionHash
		}
		if _, err := tx.Exec(ctx, `INSERT INTO marketplace_follow_intents (id, follow_run_id, proposal_seq, intent_id, outcome,
			risk_decision_hash, effect_id, notional) VALUES ($1,$2,$3,$4,$5,$6,$7,$8::numeric)`,
			id.New[id.Any](), run.ID, fi.ProposalSeq, fi.IntentID, fi.Outcome, decision, fi.EffectID, fi.Notional); err != nil {
			return FollowRun{}, dbErr("record follow intent", err)
		}
	}
	if err := s.auditEvent(ctx, tx, audit.AccountStream(install.AccountID.String()), req.Actor, "marketplace.follow.recorded",
		"marketplace_follow_run", run.ID.String(), string(exec.Result.Termination), map[string]any{
			"install_id": install.ID.String(), "strategy_ref": v.StrategyRef(), "termination": string(exec.Result.Termination),
			"submitted": run.IntentsSubmitted, "dispatched": run.IntentsDispatched, "risk_rejected": run.IntentsRiskRejected,
			"failed": run.IntentsFailed, "violations": run.ViolationsCount,
		}); err != nil {
		return FollowRun{}, err
	}
	return run, nil
}

type dispatchRequest struct {
	IntentID     string `json:"intent_id"`
	ContentHash  string `json:"content_hash"`
	DecisionHash string `json:"decision_hash"`
}

type dispatchEvidence struct {
	IntentID      string                     `json:"intent_id"`
	StrategyRef   string                     `json:"strategy_ref"`
	SandboxRunID  string                     `json:"sandbox_run_id"`
	RunDigest     string                     `json:"run_digest"`
	Proposal      sandboxrt.AcceptedProposal `json:"proposal"`
	DecisionHash  string                     `json:"decision_hash"`
	DecisionID    string                     `json:"decision_id"`
	ReceiptRef    string                     `json:"receipt_ref"`
	PolicyVersion string                     `json:"policy_version"`
}

type followEvidence struct {
	FollowRunID    string                `json:"follow_run_id"`
	InstallID      string                `json:"install_id"`
	VersionID      string                `json:"version_id"`
	AccountID      string                `json:"account_id"`
	StrategyRef    string                `json:"strategy_ref"`
	ManifestHash   string                `json:"manifest_hash"`
	ArtifactDigest string                `json:"artifact_digest"`
	SandboxRunID   string                `json:"sandbox_run_id"`
	InputDigest    string                `json:"input_digest"`
	RunDigest      string                `json:"run_digest"`
	OutputDigest   string                `json:"output_digest"`
	Termination    string                `json:"termination"`
	Trigger        string                `json:"trigger"`
	AsOf           time.Time             `json:"as_of"`
	Input          json.RawMessage       `json:"input"`
	Violations     []sandboxrt.Violation `json:"violations"`
	Intents        []FollowIntent        `json:"intents"`
	Equity         string                `json:"equity"`
	Turnover       string                `json:"turnover"`
}

func violationsOrEmpty(v []sandboxrt.Violation) []sandboxrt.Violation {
	if v == nil {
		return []sandboxrt.Violation{}
	}
	return v
}

func intentsOrEmpty(v []FollowIntent) []FollowIntent {
	out := make([]FollowIntent, len(v))
	for i, fi := range v {
		if fi.ReasonCodes == nil {
			fi.ReasonCodes = []string{}
		}
		out[i] = fi
	}
	return out
}

// followIntent takes one accepted proposal through intent -> route -> risk ->
// dispatch, and reports what became of it.
func (s *Service) followIntent(ctx context.Context, tx pgx.Tx, install Install, v Version, exec sandboxrt.Execution, followID FollowRunID,
	a sandboxrt.AcceptedProposal, correlationID string) (FollowIntent, error) {
	actor := s.serviceActor()
	proposed, err := a.Intent(sandboxrt.IntentContext{
		AccountID: install.AccountID, Mode: gates.ModePaper, Capability: gates.StrategyMarketplace, ActorID: s.cfg.ServiceID,
		StrategyRef: v.StrategyRef(), RunID: exec.RunID, RunDigest: exec.Result.RunDigest, ArtifactDigest: exec.Result.ArtifactDigest,
		KeyPrefix: "mkt-follow:" + followID.String(), CorrelationID: correlationID,
	})
	if err != nil {
		return FollowIntent{}, err
	}
	intent, _, err := s.cfg.Intents.Submit(ctx, tx, proposed)
	if err != nil {
		return FollowIntent{}, err
	}
	fi := FollowIntent{ProposalSeq: a.Seq, IntentID: intent.ID, Kind: intent.Kind, Notional: a.Notional, ReasonCodes: []string{}}

	rc, err := s.cfg.Sections.RiskContext(ctx, tx, intent, a)
	if err != nil {
		return FollowIntent{}, err
	}
	route, err := econ.Route(econ.RouteRequest{
		Intent: intent, Subject: settlement.Subject{Type: settlement.SubjectInstrument, ID: intent.InstrumentID},
		Jurisdiction: rc.Jurisdiction, Verification: rc.Verification,
	}, s.cfg.Router, s.now())
	if err != nil {
		return FollowIntent{}, err
	}
	if !route.Permitted {
		reasons := append([]string(nil), route.Reasons...)
		sort.Strings(reasons)
		fi.Outcome, fi.ReasonCodes = OutcomeRouteRefused, reasons
		_, err := s.cfg.Intents.Transition(ctx, tx, intent.ID, econ.StatusRejected, actor, "route refused: "+strings.Join(reasons, ","), "ROUTE_REFUSED")
		return fi, err
	}
	if intent, err = s.cfg.Intents.Transition(ctx, tx, intent.ID, econ.StatusRouted, actor, "routed", ""); err != nil {
		return FollowIntent{}, err
	}

	ri, err := sandboxrt.RiskIntent(intent, a)
	if err != nil {
		return FollowIntent{}, err
	}
	input := rc.Input
	input.Stage = risk.StagePreTrade
	input.Now = s.now()
	input.Intent = ri
	// The two sections only the marketplace can vouch for. A provider's
	// versions of them are discarded.
	input.Strategy = &risk.StrategySnapshot{Approved: v.Followable(), ManifestPermits: true}
	input.Sandbox = sandboxrt.SandboxSection(exec.Result.ArtifactVerified, len(exec.Violations))
	decision := risk.EvaluateAdvanced(s.cfg.RiskPolicy, input)
	decisionID, err := s.cfg.Risk.RecordAdvancedDecision(ctx, tx, input, decision, correlationID)
	if err != nil {
		return FollowIntent{}, dbErr("record risk decision", err)
	}
	fi.DecisionHash, fi.ReasonCodes = decision.Hash, append([]string{}, decision.ReasonCodes...)
	if decision.Verdict != risk.Allow {
		fi.Outcome = OutcomeRiskRejected
		_, err := s.cfg.Intents.Transition(ctx, tx, intent.ID, econ.StatusRejected, actor,
			"risk rejected: "+strings.Join(decision.ReasonCodes, ","), "RISK_REJECTED")
		return fi, err
	}
	if intent, err = s.cfg.Intents.Transition(ctx, tx, intent.ID, econ.StatusRiskApproved, actor, "risk approved", ""); err != nil {
		return FollowIntent{}, err
	}

	iid := intent.ID
	effect, replay, err := s.cfg.Effects.Begin(ctx, tx, econ.BeginRequest{
		Key: "mkt-dispatch:" + intent.ID.String(), Subsystem: econ.SubsystemMarketplace, Kind: DispatchEffectKind, IntentID: &iid,
		AccountID: intent.AccountID.String(), PostsLedger: false, Actor: actor, CorrelationID: correlationID,
		Request: dispatchRequest{IntentID: intent.ID.String(), ContentHash: hex.EncodeToString(intent.ContentHash), DecisionHash: decision.Hash},
	})
	if err != nil {
		return FollowIntent{}, err
	}
	eid := effect.ID
	fi.EffectID = &eid
	if replay && effect.Status != econ.EffectPending {
		// Exactly once: a dispatch already decided is never repeated, and an
		// unknown one is reconciled, never retried.
		switch effect.Status {
		case econ.EffectApplied:
			fi.Outcome, fi.ExternalRef = OutcomeDispatched, effect.ExternalRef
		case econ.EffectUnknown:
			fi.Outcome = OutcomeDispatchUnknown
		default:
			fi.Outcome = OutcomeDispatchFailed
		}
		return fi, nil
	}

	sp, err := tx.Begin(ctx)
	if err != nil {
		return FollowIntent{}, dbErr("open dispatch savepoint", err)
	}
	receipt, execErr := s.cfg.Executor.ExecutePaper(ctx, sp, PaperExecution{Intent: intent, Decision: decision, DecisionID: decisionID, Proposal: a, EffectID: effect.ID})
	switch {
	case execErr == nil:
		if err := sp.Commit(ctx); err != nil {
			return FollowIntent{}, dbErr("release dispatch savepoint", err)
		}
		ev, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "marketplace/dispatch/" + intent.ID.String(), Document: dispatchEvidence{
			IntentID: intent.ID.String(), StrategyRef: v.StrategyRef(), SandboxRunID: exec.RunID.String(),
			RunDigest: hex.EncodeToString(exec.Result.RunDigest), Proposal: a, DecisionHash: decision.Hash, DecisionID: decisionID.String(),
			ReceiptRef: receipt.ExternalRef, PolicyVersion: decision.PolicyVersion,
		}})
		if err != nil {
			return FollowIntent{}, err
		}
		if _, err := s.cfg.Effects.Apply(ctx, tx, effect.ID, econ.ApplyRequest{ExternalRef: receipt.ExternalRef, Evidence: &ev,
			Actor: actor, Reason: "dispatched to paper execution"}); err != nil {
			return FollowIntent{}, err
		}
		fi.Outcome, fi.ExternalRef = OutcomeDispatched, receipt.ExternalRef
	case errs.CodeOf(execErr) == errs.CodeSubmissionStateUnknown:
		// What execution recorded is kept: it describes the attempt whose
		// outcome reconciliation must establish.
		if err := sp.Commit(ctx); err != nil {
			_ = sp.Rollback(ctx)
		}
		if _, err := s.cfg.Effects.MarkUnknown(ctx, tx, effect.ID, receipt.ExternalRef, actor, "paper execution outcome unknown"); err != nil {
			return FollowIntent{}, err
		}
		fi.Outcome, fi.ExternalRef = OutcomeDispatchUnknown, receipt.ExternalRef
	default:
		_ = sp.Rollback(ctx)
		code := string(errs.CodeOf(execErr))
		if _, err := s.cfg.Effects.Fail(ctx, tx, effect.ID, code, actor, "paper execution refused: "+truncate(execErr.Error(), 200)); err != nil {
			return FollowIntent{}, err
		}
		if _, err := s.cfg.Intents.Transition(ctx, tx, intent.ID, econ.StatusRejected, actor, "paper execution refused", "DISPATCH_FAILED"); err != nil {
			return FollowIntent{}, err
		}
		fi.Outcome, fi.ReasonCodes = OutcomeDispatchFailed, []string{code}
	}
	return fi, nil
}

const followColumns = `f.id, f.install_id, f.version_id, f.account_id, f.sandbox_run_id, f.trigger, f.as_of, f.input_digest, f.run_digest,
	r.termination_reason, f.proposals_count, f.accepted_count, f.violations_count, f.intents_submitted, f.intents_dispatched,
	f.intents_risk_rejected, f.intents_failed, f.equity::text, f.turnover::text, f.evidence_ref, f.evidence_sha256, f.created_at`

func scanFollow(row pgx.Row) (FollowRun, error) {
	var f FollowRun
	if err := row.Scan(&f.ID, &f.InstallID, &f.VersionID, &f.AccountID, &f.SandboxRunID, &f.Trigger, &f.AsOf, &f.InputDigest, &f.RunDigest,
		&f.Termination, &f.ProposalsCount, &f.AcceptedCount, &f.ViolationsCount, &f.IntentsSubmitted, &f.IntentsDispatched,
		&f.IntentsRiskRejected, &f.IntentsFailed, &f.Equity, &f.Turnover, &f.EvidenceRef, &f.EvidenceSHA256, &f.CreatedAt); err != nil {
		return FollowRun{}, err
	}
	f.AsOf, f.CreatedAt = f.AsOf.UTC(), f.CreatedAt.UTC()
	return f, nil
}

func (s *Service) followAt(ctx context.Context, q db.Querier, iid InstallID, asOf time.Time) (FollowRun, bool, error) {
	f, err := scanFollow(q.QueryRow(ctx, `SELECT `+followColumns+` FROM marketplace_follow_runs f
		JOIN sandbox_runs r ON r.id = f.sandbox_run_id WHERE f.install_id = $1 AND f.as_of = $2`, iid, asOf))
	if errors.Is(err, pgx.ErrNoRows) {
		return FollowRun{}, false, nil
	}
	if err != nil {
		return FollowRun{}, false, dbErr("load follow run", err)
	}
	if f.Intents, err = s.FollowIntents(ctx, q, f.ID); err != nil {
		return FollowRun{}, false, err
	}
	return f, true, nil
}

// FollowRuns returns an install's follow history, oldest first.
func (s *Service) FollowRuns(ctx context.Context, q db.Querier, iid InstallID) ([]FollowRun, error) {
	rows, err := q.Query(ctx, `SELECT `+followColumns+` FROM marketplace_follow_runs f
		JOIN sandbox_runs r ON r.id = f.sandbox_run_id WHERE f.install_id = $1 ORDER BY f.as_of, f.id`, iid)
	if err != nil {
		return nil, dbErr("list follow runs", err)
	}
	defer rows.Close()
	var out []FollowRun
	for rows.Next() {
		f, err := scanFollow(rows)
		if err != nil {
			return nil, dbErr("scan follow run", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FollowIntents returns what became of each proposal of a follow run.
func (s *Service) FollowIntents(ctx context.Context, q db.Querier, fid FollowRunID) ([]FollowIntent, error) {
	rows, err := q.Query(ctx, `SELECT fi.proposal_seq, fi.intent_id, ei.kind, fi.outcome, coalesce(fi.risk_decision_hash, ''), fi.effect_id,
		fi.notional::text, coalesce(ee.external_ref, '') FROM marketplace_follow_intents fi
		JOIN economic_intents ei ON ei.id = fi.intent_id
		LEFT JOIN economic_effects ee ON ee.id = fi.effect_id
		WHERE fi.follow_run_id = $1 ORDER BY fi.proposal_seq`, fid)
	if err != nil {
		return nil, dbErr("list follow intents", err)
	}
	defer rows.Close()
	var out []FollowIntent
	for rows.Next() {
		var fi FollowIntent
		if err := rows.Scan(&fi.ProposalSeq, &fi.IntentID, &fi.Kind, &fi.Outcome, &fi.DecisionHash, &fi.EffectID, &fi.Notional, &fi.ExternalRef); err != nil {
			return nil, dbErr("scan follow intent", err)
		}
		out = append(out, fi)
	}
	return out, rows.Err()
}

// DueInstalls returns ACTIVE installs of followable versions whose next run
// is due at now under their manifest's execution frequency.
func (s *Service) DueInstalls(ctx context.Context, q db.Querier, now time.Time, limit int) ([]Install, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.Query(ctx, `SELECT i.id, i.account_id, i.version_id, i.mode, i.status, i.installed_by_actor_type, i.installed_by_actor_id,
		i.created_at, (SELECT max(as_of) FROM marketplace_follow_runs f WHERE f.install_id = i.id)
		FROM marketplace_installs i JOIN marketplace_listing_versions v ON v.id = i.version_id
		WHERE i.status = 'ACTIVE' AND v.status = 'APPROVED' AND v.moderation_state = 'CLEARED'
		ORDER BY i.created_at, i.id`)
	if err != nil {
		return nil, dbErr("list installs", err)
	}
	type candidate struct {
		in   Install
		last *time.Time
	}
	var cands []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.in.ID, &c.in.AccountID, &c.in.VersionID, &c.in.Mode, &c.in.Status, &c.in.InstalledBy.Type,
			&c.in.InstalledBy.ID, &c.in.CreatedAt, &c.last); err != nil {
			rows.Close()
			return nil, dbErr("scan install", err)
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, dbErr("list installs", err)
	}
	var out []Install
	for _, c := range cands {
		if len(out) >= limit {
			break
		}
		if c.last != nil {
			v, err := s.Version(ctx, q, c.in.VersionID)
			if err != nil {
				return nil, err
			}
			if now.Before(c.last.UTC().Add(time.Duration(v.Manifest.MinIntervalSeconds) * time.Second)) {
				continue
			}
		}
		c.in.CreatedAt = c.in.CreatedAt.UTC()
		out = append(out, c.in)
	}
	return out, nil
}

// FollowScheduled runs a due install on the snapshot the SnapshotSource
// builds for asOf.
func (s *Service) FollowScheduled(ctx context.Context, tx pgx.Tx, iid InstallID, asOf time.Time) (FollowRun, error) {
	if s.cfg.Snapshots == nil {
		return FollowRun{}, errs.New(errs.CodeUnsupported, "marketplace: no snapshot source is configured for scheduled follows")
	}
	install, err := s.Install(ctx, tx, iid)
	if err != nil {
		return FollowRun{}, err
	}
	v, err := s.Version(ctx, tx, install.VersionID)
	if err != nil {
		return FollowRun{}, err
	}
	in, err := s.cfg.Snapshots.Snapshot(ctx, tx, install, v, asOf)
	if err != nil {
		return FollowRun{}, err
	}
	return s.PaperFollow(ctx, tx, FollowRequest{InstallID: iid, Input: in, Trigger: TriggerScheduled, Actor: econ.SystemActor("marketplace.scheduler")})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
