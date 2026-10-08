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
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
)

// Service persists artifacts and runs. RecordArtifact runs in the caller's
// transaction; a run is reserved, charged and recorded in short transactions
// of the budget's own (Execute).
type Service struct {
	clk    clock.Clock
	auth   *econ.Authorizer
	engine Engine
	audit  audit.Writer
	budget Budget
	conns  BudgetConns
	// wait bounds how long an admission or a reservation waits for the
	// budget (budgetWait).
	wait time.Duration
}

// NewService wires a Service under DefaultBudget. Every dependency is
// required: a service that could not see the kill switch would run code
// through it, and one without the budget's own connections could charge a
// run only by holding the budget's lock in its caller's transaction. An
// engine that says it runs in live mode is refused: no strategy runtime
// does (NewRuntimeProvider).
func NewService(clk clock.Clock, auth *econ.Authorizer, engine Engine, aud audit.Writer, conns BudgetConns) *Service {
	if clk == nil || auth == nil || engine == nil || aud == nil || conns == nil {
		panic("sandboxrt: NewService requires a clock, an authorizer, an engine, an audit writer and the budget's connections")
	}
	if m, ok := engine.(moded); ok && m.Mode() == config.ProviderModeLive {
		panic("sandboxrt: a live strategy runtime is refused (" + ReasonLiveRuntimeUnsupported + ")")
	}
	return &Service{clk: clk, auth: auth, engine: engine, audit: aud, budget: DefaultBudget(), conns: conns, wait: budgetWait}
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

// Execute runs an artifact against an input, records the run, its violations
// and an audit event, and charges it to the account's sandbox budget. No lock
// is held while the code runs (the package documentation's "Budget"):
//
//  1. The kill switch is read in the caller's transaction. While
//     CAPABILITY_DISABLE for USER_CODE_SANDBOX (or the global kill) is active
//     the module is never started: the run is recorded as KILLED_BY_SWITCH,
//     charged one run and no CPU, and yields nothing.
//  2. The run is reserved. A run of a call Admit admitted in tx takes up one
//     of the call's reservations; any other run is admitted in a short budget
//     transaction -- the account's budget lock, the window checked against
//     its charges and every reservation not yet charged, the reservation
//     taken in tx -- that commits before the code starts. A run the window
//     has no room for is refused (RATE_LIMITED with a stable reason) and
//     nothing is recorded; so is one that waited for the budget longer than
//     it may (BudgetContended).
//  3. The code runs. Nothing is locked but the reservation, which only ever
//     counts against the window and never blocks anyone.
//  4. The run is recorded and charged in a second short budget transaction:
//     its row, its violations, its audit event (on the account's audit
//     stream) and its charge, whose commit retires the reservation in the
//     same instant it makes the charge count.
//
// A run is therefore recorded and charged in transactions of its own,
// committed before Execute returns: a caller that rolls back afterwards
// keeps the run on the account's budget -- it ran -- and nothing else of it:
// the reservation goes with the caller's transaction. The artifact must be
// committed, since the run's own transaction references it, and the caller's
// transaction must not hold the account's audit stream (an account event
// appended before the run) or the account's row FOR UPDATE: the run's record
// would wait on them, and after finalizeWait be refused.
//
// A run that is not OK yields nothing; only an OK run's proposals reach the
// manifest, valued at the reference prices, and only the manifest's survivors
// are returned as Accepted.
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
	killed := false
	if err := s.auth.Require(ctx, tx, gates.UserCodeSandbox, gates.ModeSandbox); err != nil {
		if errs.CodeOf(err) != errs.CodeKillSwitchActive {
			return Execution{}, err
		}
		killed = true
	}
	pid, err := backendPID(ctx, tx)
	if err != nil {
		return Execution{}, err
	}
	// A fresh id, unless the run takes up a reservation the caller already
	// holds (reserve, recordKilled); drawn once, so a retried budget
	// transaction reserves the same run.
	exec := Execution{RunID: NewRunID(), ManifestHash: manifestHash}

	if killed {
		res, err := Interpret(RawRun{
			Termination: TerminationKilledBySwitch, Detail: "CAPABILITY_DISABLE for USER_CODE_SANDBOX is active",
			ArtifactDigest: req.ArtifactDigest, InputDigest: inputDigest,
		}, req.Limits)
		if err != nil {
			return Execution{}, err
		}
		exec.Result = res
		if err := s.recordKilled(ctx, pid, req, &exec); err != nil {
			return Execution{}, err
		}
		return exec, nil
	}

	wasm, err := s.reserve(ctx, tx, pid, req, &exec)
	if err != nil {
		return Execution{}, err
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
	if err := s.finalize(ctx, req, exec); err != nil {
		return Execution{}, err
	}
	return exec, nil
}

// reserve loads the artifact a run executes -- as committed: the run's record
// will reference it -- and either takes up a reservation the caller's
// transaction tx (on backend pid) already holds, or admits the run against the
// account's window and reserves it in tx, all in one short budget
// transaction. It stamps the run's start.
func (s *Service) reserve(ctx context.Context, tx pgx.Tx, pid int32, req ExecuteRequest, exec *Execution) ([]byte, error) {
	var wasm []byte
	fresh := exec.RunID
	err := s.budgetTx(ctx, s.wait, func(ctx context.Context, btx pgx.Tx) error {
		err := btx.QueryRow(ctx, `SELECT wasm FROM strategy_artifacts WHERE artifact_digest = $1`, req.ArtifactDigest).Scan(&wasm)
		if errors.Is(err, pgx.ErrNoRows) {
			return errs.New(errs.CodeNotFound, "sandboxrt: artifact not found")
		}
		if err != nil {
			return dbErr("load artifact bytes", err)
		}
		now := s.clk.Now().UTC()
		exec.StartedAt = now
		claimed, ok, err := claim(ctx, btx, pid, req.AccountID, req.Limits.WallTimeMS, now.Add(-s.budget.Window))
		if err != nil {
			return err
		}
		if ok {
			exec.RunID = claimed
			return nil
		}
		exec.RunID = fresh
		if err := lockBudget(ctx, btx, req.AccountID); err != nil {
			return err
		}
		if err := s.fitsWindow(ctx, btx, req.AccountID, 1, req.Limits.WallTimeMS); err != nil {
			return err
		}
		return reserveIn(ctx, tx, req.AccountID, req.Limits.WallTimeMS, []uint32{runTag(exec.RunID)})
	})
	if err != nil {
		return nil, err
	}
	return wasm, nil
}

// sqlStateBudgetExceeded is SX003: a charge that is not its run's, or beyond
// the budget it states (migration 00757).
const sqlStateBudgetExceeded = "SX003"

// finalize records and charges a run that ran, in a short budget transaction
// that waits up to finalizeWait: the code has run, and its record is worth
// the wait. A charge the window no longer has room for -- SX003, reached only
// when the window's accounting moved under the run (a charge stamped by
// another process's clock, a run that took longer than it was granted) -- is
// BudgetContended, and nothing of the run is recorded.
func (s *Service) finalize(ctx context.Context, req ExecuteRequest, e Execution) error {
	err := s.budgetTx(ctx, finalizeWait, func(ctx context.Context, btx pgx.Tx) error { return s.record(ctx, btx, req, e) })
	if db.SQLState(err) == sqlStateBudgetExceeded {
		return contended("the run's charge no longer fits the account's window; nothing of the run is recorded")
	}
	return err
}

// recordKilled records a run the kill switch stopped before it started -- one
// run and no CPU -- in one short budget transaction, admitted against the
// account's window like any run unless it takes up a reservation the
// caller's transaction (on backend pid) already holds.
func (s *Service) recordKilled(ctx context.Context, pid int32, req ExecuteRequest, exec *Execution) error {
	fresh := exec.RunID
	return s.budgetTx(ctx, s.wait, func(ctx context.Context, btx pgx.Tx) error {
		if _, err := s.Artifact(ctx, btx, req.ArtifactDigest); err != nil {
			return err
		}
		now := s.clk.Now().UTC()
		exec.StartedAt = now
		claimed, reserved, err := claim(ctx, btx, pid, req.AccountID, req.Limits.WallTimeMS, now.Add(-s.budget.Window))
		if err != nil {
			return err
		}
		if reserved {
			exec.RunID = claimed
			return s.record(ctx, btx, req, *exec)
		}
		exec.RunID = fresh
		if err := lockBudget(ctx, btx, req.AccountID); err != nil {
			return err
		}
		if err := s.fitsWindow(ctx, btx, req.AccountID, 1, 0); err != nil {
			return err
		}
		return s.record(ctx, btx, req, *exec)
	})
}

// record writes a run in the budget transaction btx: its row, its violations,
// its audit event and, last, its charge -- the statement that takes the
// account's budget lock, so the lock is held only from there to the commit.
func (s *Service) record(ctx context.Context, btx pgx.Tx, req ExecuteRequest, e Execution) error {
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
	if _, err := btx.Exec(ctx, `INSERT INTO sandbox_runs (id, artifact_digest, input_digest, limits_version, limits_hash, limits,
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
		if _, err := btx.Exec(ctx, `INSERT INTO sandbox_run_violations (id, run_id, seq, record_seq, code, detail)
			VALUES ($1,$2,$3,$4,$5,$6)`, id.New[id.Any](), e.RunID, i+1, v.Seq, v.Code, truncate(v.Detail, 1000)); err != nil {
			return dbErr("record sandbox violation", err)
		}
	}
	payload, err := json.Marshal(map[string]any{
		"run_id": e.RunID.String(), "termination": string(r.Termination), "strategy_ref": req.StrategyRef,
		"proposals": len(r.Proposals), "accepted": len(e.Accepted), "violations": len(e.Violations),
	})
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "sandboxrt: encode audit payload")
	}
	// The account's stream, not the system's: the run is charged to the
	// account, and the system stream's lock is held by many transactions this
	// one must never wait on.
	if _, err := s.audit.Append(ctx, btx, audit.Event{
		Stream: audit.AccountStream(req.AccountID.String()), ActorType: string(req.Actor.Type), ActorID: req.Actor.ID,
		Action: "sandbox.run." + strings.ToLower(string(r.Termination)), ResourceType: "sandbox_run", ResourceID: e.RunID.String(),
		CorrelationID: req.CorrelationID, Reason: truncate(r.Detail, 200), Payload: payload, OccurredAt: s.clk.Now().UTC(),
	}); err != nil {
		return err
	}
	return s.charge(ctx, btx, req, e)
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
