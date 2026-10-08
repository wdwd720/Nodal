package sandboxrt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
)

// Service persists artifacts and runs. Every mutating method runs in the
// caller's transaction.
type Service struct {
	clk    clock.Clock
	auth   *econ.Authorizer
	engine Engine
	audit  audit.Writer
	budget Budget
}

// NewService wires a Service under DefaultBudget. Every dependency is
// required: a service that could not see the kill switch would run code
// through it.
func NewService(clk clock.Clock, auth *econ.Authorizer, engine Engine, aud audit.Writer) *Service {
	if clk == nil || auth == nil || engine == nil || aud == nil {
		panic("sandboxrt: NewService requires a clock, an authorizer, an engine and an audit writer")
	}
	return &Service{clk: clk, auth: auth, engine: engine, audit: aud, budget: DefaultBudget()}
}

// WithBudget returns a copy of the service that charges runs under b instead.
func (s *Service) WithBudget(b Budget) (*Service, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	out := *s
	out.budget = b
	return &out, nil
}

// ArtifactRecord is a strategy_artifacts row.
type ArtifactRecord struct {
	ID               ArtifactID
	ArtifactDigest   []byte
	SourceDigest     []byte
	WasmSize         int
	ToolchainVersion string
	SDKVersion       string
	BuildLogDigest   []byte
	CreatedBy        econ.Actor
	CreatedAt        time.Time
}

// RecordingActorTypes are the actor types that may record an artifact or a
// run (the created_by_actor_type checks of strategy_artifacts and
// strategy_artifact_sources, and sandbox_runs_actor_type_check). Never an
// agent.
func RecordingActorTypes() []security.ActorType {
	return []security.ActorType{security.ActorUser, security.ActorOperator, security.ActorService, security.ActorSystem}
}

func validActor(a econ.Actor) error {
	if !a.Type.Valid() || strings.TrimSpace(a.ID) == "" {
		return errs.New(errs.CodeValidationFailed, "sandboxrt: an actor type and id are required")
	}
	for _, t := range RecordingActorTypes() {
		if t == a.Type {
			return nil
		}
	}
	return errs.New(errs.CodeForbidden, "sandboxrt: an agent neither publishes nor runs strategy artifacts")
}

// RecordArtifact stores a successful build. It is idempotent on the artifact
// digest (the artifact is its bytes), and it records that this build's
// source produced it. existing reports that the bytes were already stored.
func (s *Service) RecordArtifact(ctx context.Context, tx pgx.Tx, b Build, by econ.Actor) (ArtifactRecord, bool, error) {
	if err := validActor(by); err != nil {
		return ArtifactRecord{}, false, err
	}
	sum := sha256.Sum256(b.Wasm)
	switch {
	case b.Failure != "" || len(b.Wasm) == 0:
		return ArtifactRecord{}, false, errs.New(errs.CodeValidationFailed, "sandboxrt: only a successful build is an artifact")
	case !b.Scan.OK():
		return ArtifactRecord{}, false, errs.New(errs.CodeValidationFailed, "sandboxrt: an artifact needs a clean scan")
	case !bytes.Equal(sum[:], b.ArtifactDigest):
		return ArtifactRecord{}, false, errs.New(errs.CodeValidationFailed, "sandboxrt: the artifact digest is not the digest of its bytes")
	case len(b.SourceDigest) != 32 || len(b.BuildLogDigest) != 32:
		return ArtifactRecord{}, false, errs.New(errs.CodeValidationFailed, "sandboxrt: source and build log digests are required")
	}
	report, err := audit.CanonicalJSON(b.Scan)
	if err != nil {
		return ArtifactRecord{}, false, errs.Wrap(err, errs.CodeInternal, "sandboxrt: encode scan report")
	}
	tag, err := tx.Exec(ctx, `INSERT INTO strategy_artifacts (id, artifact_digest, source_digest, wasm, wasm_size, toolchain_version,
		sdk_version, build_log_digest, scan_report, scan_codes, created_by_actor_type, created_by_actor_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'{}',$10,$11) ON CONFLICT (artifact_digest) DO NOTHING`,
		id.New[artifactKind](), b.ArtifactDigest, b.SourceDigest, b.Wasm, len(b.Wasm), b.ToolchainVersion, b.SDKVersion,
		b.BuildLogDigest, report, by.Type, by.ID)
	if err != nil {
		return ArtifactRecord{}, false, dbErr("record artifact", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO strategy_artifact_sources (id, artifact_digest, source_digest, toolchain_version,
		sdk_version, build_log_digest, created_by_actor_type, created_by_actor_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (artifact_digest, source_digest) DO NOTHING`,
		id.New[id.Any](), b.ArtifactDigest, b.SourceDigest, b.ToolchainVersion, b.SDKVersion, b.BuildLogDigest, by.Type, by.ID); err != nil {
		return ArtifactRecord{}, false, dbErr("record artifact source", err)
	}
	rec, err := s.Artifact(ctx, tx, b.ArtifactDigest)
	if err != nil {
		return ArtifactRecord{}, false, err
	}
	return rec, tag.RowsAffected() == 0, nil
}

const artifactColumns = `id, artifact_digest, source_digest, wasm_size, toolchain_version, sdk_version, build_log_digest,
	created_by_actor_type, created_by_actor_id, created_at`

// Artifact returns an artifact's metadata.
func (s *Service) Artifact(ctx context.Context, q db.Querier, digest []byte) (ArtifactRecord, error) {
	var r ArtifactRecord
	err := q.QueryRow(ctx, `SELECT `+artifactColumns+` FROM strategy_artifacts WHERE artifact_digest = $1`, digest).Scan(
		&r.ID, &r.ArtifactDigest, &r.SourceDigest, &r.WasmSize, &r.ToolchainVersion, &r.SDKVersion, &r.BuildLogDigest,
		&r.CreatedBy.Type, &r.CreatedBy.ID, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArtifactRecord{}, errs.New(errs.CodeNotFound, "sandboxrt: artifact not found")
	}
	if err != nil {
		return ArtifactRecord{}, dbErr("load artifact", err)
	}
	r.CreatedAt = r.CreatedAt.UTC()
	return r, nil
}

// ExecuteRequest is one recorded run.
type ExecuteRequest struct {
	ArtifactDigest []byte
	Input          Input
	Limits         Limits
	// Manifest is the strategy version's declared permissions; every
	// proposal beyond it is dropped and recorded.
	Manifest Manifest
	// ReferencePrices are the prices the host read itself; every accepted
	// proposal's notional is computed at them (FilterAt). Nil means the
	// input's own marks (MarksOf), which is sound only where the host built
	// the input from prices it read, or nothing the run accepts becomes an
	// intent.
	ReferencePrices ReferencePrices
	// AccountID is the account the run is charged to (the sandbox budget)
	// and Purpose what it is for.
	AccountID accounts.AccountID
	Purpose   RunPurpose
	// StrategyRef is "<listing id>@<version>".
	StrategyRef   string
	Actor         econ.Actor
	CorrelationID string
}

// Execution is a recorded run.
type Execution struct {
	RunID  RunID
	Result Result
	// Accepted are the proposals the manifest permits; empty unless the run
	// is OK.
	Accepted []AcceptedProposal
	// Violations is every violation of the run -- protocol, proposal and
	// manifest -- in record order. Its length is what Risk V2's sandbox
	// section counts.
	Violations   []Violation
	ManifestHash []byte
	StartedAt    time.Time
}

// Execute runs an artifact against an input and records the run, its
// violations, its charge to the account's sandbox budget and an audit event
// in the caller's transaction.
//
// The account's budget is locked and checked before anything starts: a run
// the window has no room for is refused (RATE_LIMITED with a stable reason)
// and nothing is recorded. While CAPABILITY_DISABLE for USER_CODE_SANDBOX (or
// the global kill) is active, the module is never started: the run is
// recorded as KILLED_BY_SWITCH, charged nothing, and yields nothing. A run
// that is not OK yields nothing either; only an OK run's proposals reach the
// manifest, valued at the reference prices, and only the manifest's
// survivors are returned as Accepted.
func (s *Service) Execute(ctx context.Context, tx pgx.Tx, req ExecuteRequest) (Execution, error) {
	if err := validActor(req.Actor); err != nil {
		return Execution{}, err
	}
	if err := req.Limits.Validate(); err != nil {
		return Execution{}, err
	}
	if strings.TrimSpace(req.StrategyRef) == "" || len(req.StrategyRef) > 200 {
		return Execution{}, errs.New(errs.CodeValidationFailed, "sandboxrt: a strategy ref is required")
	}
	if len(req.ArtifactDigest) != 32 {
		return Execution{}, errs.New(errs.CodeValidationFailed, "sandboxrt: an artifact digest is required")
	}
	if req.AccountID.IsZero() || !req.Purpose.Valid() {
		return Execution{}, errs.New(errs.CodeValidationFailed, "sandboxrt: a sandbox run is charged to an account, for a declared purpose")
	}
	if req.Limits.WallTimeMS > s.budget.MaxCPUMSPerCall {
		return Execution{}, budgetRefusal(BudgetCallCPU, fmt.Sprintf("one run may be granted at most %d ms of sandbox CPU", s.budget.MaxCPUMSPerCall), nil)
	}
	if err := lockBudget(ctx, tx, req.AccountID); err != nil {
		return Execution{}, err
	}
	if err := s.fitsWindow(ctx, tx, req.AccountID, 1, req.Limits.WallTimeMS); err != nil {
		return Execution{}, err
	}
	manifestHash, err := req.Manifest.Hash()
	if err != nil {
		return Execution{}, err
	}
	snap, err := req.Input.Snapshot()
	if err != nil {
		return Execution{}, err
	}
	inputDigest, err := req.Input.Digest()
	if err != nil {
		return Execution{}, err
	}
	exec := Execution{RunID: NewRunID(), ManifestHash: manifestHash, StartedAt: s.clk.Now().UTC()}

	if err := s.auth.Require(ctx, tx, gates.UserCodeSandbox, gates.ModeSandbox); err != nil {
		if errs.CodeOf(err) != errs.CodeKillSwitchActive {
			return Execution{}, err
		}
		if _, err := s.Artifact(ctx, tx, req.ArtifactDigest); err != nil {
			return Execution{}, err
		}
		res, err := Interpret(RawRun{Termination: TerminationKilledBySwitch, Detail: "CAPABILITY_DISABLE for USER_CODE_SANDBOX is active",
			ArtifactDigest: req.ArtifactDigest, InputDigest: inputDigest}, req.Limits)
		if err != nil {
			return Execution{}, err
		}
		exec.Result = res
		return exec, s.record(ctx, tx, req, exec)
	}

	var wasm []byte
	err = tx.QueryRow(ctx, `SELECT wasm FROM strategy_artifacts WHERE artifact_digest = $1`, req.ArtifactDigest).Scan(&wasm)
	if errors.Is(err, pgx.ErrNoRows) {
		return Execution{}, errs.New(errs.CodeNotFound, "sandboxrt: artifact not found")
	}
	if err != nil {
		return Execution{}, dbErr("load artifact bytes", err)
	}
	raw, err := s.engine.Run(ctx, RunRequest{Artifact: Artifact{Digest: req.ArtifactDigest, Wasm: wasm}, Input: req.Input, Limits: req.Limits})
	if err != nil {
		return Execution{}, err
	}
	res, err := Interpret(raw, req.Limits)
	if err != nil {
		return Execution{}, err
	}
	exec.Result = res
	exec.Violations = append(exec.Violations, res.Violations...)
	if res.Termination == TerminationOK {
		refs := req.ReferencePrices
		if refs == nil {
			refs = MarksOf(snap)
		}
		accepted, dropped := req.Manifest.FilterAt(res.Proposals, snap, refs)
		exec.Accepted = accepted
		exec.Violations = append(exec.Violations, dropped...)
	}
	sortViolations(exec.Violations)
	return exec, s.record(ctx, tx, req, exec)
}

func (s *Service) record(ctx context.Context, tx pgx.Tx, req ExecuteRequest, e Execution) error {
	limits, err := audit.CanonicalJSON(req.Limits)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "sandboxrt: encode limits")
	}
	limitsHash, err := req.Limits.Hash()
	if err != nil {
		return err
	}
	r := e.Result
	stderrDigest := sha256.Sum256(r.Stderr)
	output := r.Stdout
	if output == nil {
		output = []byte{}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sandbox_runs (id, artifact_digest, input_digest, limits_version, limits_hash, limits,
		termination_reason, termination_detail, exit_code, artifact_verified, wall_ms, output, output_bytes, stderr_bytes,
		stderr_digest, peak_memory_bytes, output_digest, run_digest, proposals_count, accepted_count, violations_count,
		manifest_hash, strategy_ref, actor_type, actor_id, correlation_id, started_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,NULLIF($26,''),$27)`,
		e.RunID, req.ArtifactDigest, r.InputDigest, req.Limits.Version, limitsHash, limits,
		r.Termination, truncate(r.Detail, 1000), int64(r.ExitCode), r.ArtifactVerified, r.Usage.WallTime.Milliseconds(),
		output, len(output), len(r.Stderr), stderrDigest[:], int64(r.Usage.PeakMemoryBytes), r.OutputDigest, r.RunDigest, // #nosec G115 -- bounded by the 4 GiB address space
		len(r.Proposals), len(e.Accepted), len(e.Violations), e.ManifestHash, req.StrategyRef, req.Actor.Type, req.Actor.ID,
		req.CorrelationID, e.StartedAt); err != nil {
		return dbErr("record sandbox run", err)
	}
	for i, v := range e.Violations {
		if _, err := tx.Exec(ctx, `INSERT INTO sandbox_run_violations (id, run_id, seq, record_seq, code, detail)
			VALUES ($1,$2,$3,$4,$5,$6)`, id.New[id.Any](), e.RunID, i+1, v.Seq, v.Code, truncate(v.Detail, 1000)); err != nil {
			return dbErr("record sandbox violation", err)
		}
	}
	if err := s.charge(ctx, tx, req, e); err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{
		"run_id": e.RunID.String(), "termination": string(r.Termination), "strategy_ref": req.StrategyRef,
		"proposals": len(r.Proposals), "accepted": len(e.Accepted), "violations": len(e.Violations),
	})
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "sandboxrt: encode audit payload")
	}
	_, err = s.audit.Append(ctx, tx, audit.Event{
		Stream: audit.SystemStream, ActorType: string(req.Actor.Type), ActorID: req.Actor.ID,
		Action: "sandbox.run." + strings.ToLower(string(r.Termination)), ResourceType: "sandbox_run", ResourceID: e.RunID.String(),
		CorrelationID: req.CorrelationID, Reason: truncate(r.Detail, 200), Payload: payload, OccurredAt: s.clk.Now().UTC(),
	})
	return err
}

// RunRecord is a sandbox_runs row.
type RunRecord struct {
	ID               RunID
	ArtifactDigest   []byte
	InputDigest      []byte
	LimitsVersion    string
	Termination      Termination
	ExitCode         int64
	ArtifactVerified bool
	WallMS           int64
	OutputBytes      int64
	OutputDigest     []byte
	RunDigest        []byte
	ProposalsCount   int
	AcceptedCount    int
	ViolationsCount  int
	StrategyRef      string
	CreatedAt        time.Time
}

// Run returns one recorded run.
func (s *Service) Run(ctx context.Context, q db.Querier, runID RunID) (RunRecord, error) {
	var r RunRecord
	err := q.QueryRow(ctx, `SELECT id, artifact_digest, input_digest, limits_version, termination_reason, exit_code,
		artifact_verified, wall_ms, output_bytes, output_digest, run_digest, proposals_count, accepted_count, violations_count,
		strategy_ref, created_at FROM sandbox_runs WHERE id = $1`, runID).Scan(
		&r.ID, &r.ArtifactDigest, &r.InputDigest, &r.LimitsVersion, &r.Termination, &r.ExitCode, &r.ArtifactVerified,
		&r.WallMS, &r.OutputBytes, &r.OutputDigest, &r.RunDigest, &r.ProposalsCount, &r.AcceptedCount, &r.ViolationsCount,
		&r.StrategyRef, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunRecord{}, errs.New(errs.CodeNotFound, "sandboxrt: run not found").WithField("run_id", runID.String())
	}
	if err != nil {
		return RunRecord{}, dbErr("load run", err)
	}
	r.CreatedAt = r.CreatedAt.UTC()
	return r, nil
}

// RunViolations returns a run's violations in order.
func (s *Service) RunViolations(ctx context.Context, q db.Querier, runID RunID) ([]Violation, error) {
	rows, err := q.Query(ctx, `SELECT code, record_seq, detail FROM sandbox_run_violations WHERE run_id = $1 ORDER BY seq`, runID)
	if err != nil {
		return nil, dbErr("load violations", err)
	}
	defer rows.Close()
	var out []Violation
	for rows.Next() {
		var v Violation
		if err := rows.Scan(&v.Code, &v.Seq, &v.Detail); err != nil {
			return nil, dbErr("scan violation", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func dbErr(op string, err error) error {
	if db.IsRetryable(err) {
		return err
	}
	if ee, ok := errs.As(err); ok {
		return ee
	}
	return errs.Wrap(err, errs.CodeInternal, "sandboxrt: "+op)
}
