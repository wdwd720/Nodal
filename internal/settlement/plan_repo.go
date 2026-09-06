package settlement

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/security"
)

// Audit actions written by the plan repository and the executor.
const (
	AuditPlanCreated    = "plan.created"
	AuditPlanApproved   = "plan.approved"
	AuditPlanSuperseded = "plan.superseded"
	AuditPlanStatus     = "plan.status"
	AuditNoValidPlan    = "plan.no_valid_plan"
	AuditStepPrefix     = "settlement.step."
)

// PlanRepository persists execution_plans and execution_plan_steps. Approve
// freezes a plan: the execution_plans_guard trigger of migration 00202
// refuses later changes to the frozen columns, and this repository never
// attempts one. Replanning goes through Supersede, which creates version+1.
type PlanRepository struct {
	clk   clock.Clock
	audit audit.Writer
}

// NewPlanRepository wires a PlanRepository.
func NewPlanRepository(clk clock.Clock, aud audit.Writer) *PlanRepository {
	if clk == nil {
		clk = clock.System()
	}
	return &PlanRepository{clk: clk, audit: aud}
}

// StepOutcome is what MarkStep records with a state change.
type StepOutcome struct {
	EvidenceOutput any
	LastError      string
}

const planColumns = `p.id, p.intent_id::text, p.version, p.planner_version, p.status, p.no_plan_reason_codes, p.hard_constraints, p.estimated_costs,
	coalesce(p.selected_venue_listing_id::text,''), p.selected_settlement_asset_id, coalesce(p.instrument_version,0), p.policy_versions,
	coalesce(p.risk_decision_id::text,''), coalesce(p.quote_id::text,''), p.plan_hash, p.dry_run, p.created_at, p.approved_at, p.finished_at,
	coalesce(i.account_id::text,'')`

const stepColumns = `id, plan_id, seq, type, depends_on, state, semantic_idempotency_key, retry_class, timeout_ms, finality_policy, compensation_policy,
	evidence_inputs, evidence_output, attempts, started_at, finished_at, coalesce(last_error,'')`

func scanPlan(row pgx.Row) (Plan, error) {
	var p Plan
	var hc, ec, pv []byte
	if err := row.Scan(&p.ID, &p.IntentID, &p.Version, &p.PlannerVersion, &p.Status, &p.NoPlanReasonCodes, &hc, &ec,
		&p.SelectedVenueListingID, &p.SelectedSettlementAssetID, &p.InstrumentVersion, &pv,
		&p.RiskDecisionID, &p.QuoteID, &p.Hash, &p.DryRun, &p.CreatedAt, &p.ApprovedAt, &p.FinishedAt, &p.AccountID); err != nil {
		return Plan{}, err
	}
	if err := json.Unmarshal(hc, &p.HardConstraints); err != nil {
		return Plan{}, errs.Wrap(err, errs.CodeInternal, "settlement: decode hard constraints")
	}
	if err := json.Unmarshal(ec, &p.EstimatedCosts); err != nil {
		return Plan{}, errs.Wrap(err, errs.CodeInternal, "settlement: decode estimated costs")
	}
	if err := json.Unmarshal(pv, &p.PolicyVersions); err != nil {
		return Plan{}, errs.Wrap(err, errs.CodeInternal, "settlement: decode policy versions")
	}
	if p.NoPlanReasonCodes == nil {
		p.NoPlanReasonCodes = []string{}
	}
	return p, nil
}

func scanStep(row pgx.Row) (Step, error) {
	var s Step
	var timeoutMS int64
	var retry string
	if err := row.Scan(&s.ID, &s.PlanID, &s.Seq, &s.Type, &s.DependsOn, &s.State, &s.SemanticIdempotencyKey, &retry, &timeoutMS,
		&s.FinalityPolicy, &s.CompensationPolicy, &s.EvidenceInputs, &s.EvidenceOutput, &s.Attempts, &s.StartedAt, &s.FinishedAt, &s.LastError); err != nil {
		return Step{}, err
	}
	s.RetryClass = retryClass(retry)
	s.Timeout = time.Duration(timeoutMS) * time.Millisecond
	if s.DependsOn == nil {
		s.DependsOn = []StepID{}
	}
	return s, nil
}

func dbErr(op string, err error) error {
	if e, ok := errs.As(err); ok {
		return e
	}
	if db.IsRetryable(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if db.SQLState(err) == "LG003" && strings.Contains(err.Error(), "PLAN_IMMUTABLE") {
		return errs.Wrap(err, errs.CodePlanImmutable, "settlement: approved plan is immutable")
	}
	return errs.Wrap(err, errs.CodeInternal, "settlement: "+op)
}

// Create inserts a DRAFT plan with its steps, or a NO_VALID_PLAN record.
// The plan hash must verify.
func (r *PlanRepository) Create(ctx context.Context, tx pgx.Tx, p Plan) (Plan, error) {
	if err := r.check(); err != nil {
		return Plan{}, err
	}
	if p.ID.IsZero() {
		return Plan{}, errs.New(errs.CodeValidationFailed, "settlement: plan id is required")
	}
	if p.Status == "" {
		p.Status = PlanDraft
	}
	if p.Status != PlanDraft && p.Status != PlanNoValidPlan {
		return Plan{}, errs.New(errs.CodeValidationFailed, "settlement: new plans are DRAFT or NO_VALID_PLAN").WithField("status", string(p.Status))
	}
	if err := p.Validate(); err != nil {
		return Plan{}, err
	}
	if ok, err := VerifyHash(p); err != nil {
		return Plan{}, err
	} else if !ok {
		return Plan{}, errs.New(errs.CodeValidationFailed, "settlement: plan hash does not match its content")
	}
	if p.AccountID == "" {
		return Plan{}, errs.New(errs.CodeValidationFailed, "settlement: plan account id is required")
	}
	hc, err := json.Marshal(p.HardConstraints)
	if err != nil {
		return Plan{}, errs.Wrap(err, errs.CodeInternal, "settlement: encode hard constraints")
	}
	ec, err := json.Marshal(p.EstimatedCosts)
	if err != nil {
		return Plan{}, errs.Wrap(err, errs.CodeInternal, "settlement: encode estimated costs")
	}
	pv := p.PolicyVersions
	if pv == nil {
		pv = PolicyVersions{}
	}
	pvb, err := json.Marshal(pv)
	if err != nil {
		return Plan{}, errs.Wrap(err, errs.CodeInternal, "settlement: encode policy versions")
	}
	now := r.clk.Now()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	reasons := p.NoPlanReasonCodes
	if reasons == nil {
		reasons = []string{}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO execution_plans (id, intent_id, version, planner_version, status, no_plan_reason_codes, hard_constraints, estimated_costs,
			selected_venue_listing_id, selected_settlement_asset_id, instrument_version, policy_versions, risk_decision_id, quote_id, plan_hash, dry_run, created_at)
		VALUES ($1,$2::uuid,$3,$4,$5,$6,$7,$8,$9::uuid,$10,$11,$12,$13::uuid,$14::uuid,$15,$16,$17)`,
		p.ID, p.IntentID, p.Version, p.PlannerVersion, string(p.Status), reasons, hc, ec,
		nullable(p.SelectedVenueListingID), nullableAsset(p.SelectedSettlementAssetID), nullableInt(p.InstrumentVersion), pvb,
		nullable(p.RiskDecisionID), nullable(p.QuoteID), p.Hash, p.DryRun, p.CreatedAt.UTC()); err != nil {
		if db.IsUniqueViolation(err) {
			return Plan{}, errs.New(errs.CodeConflict, "settlement: a plan with this intent and version already exists").
				WithField("intent_id", p.IntentID).WithField("version", p.Version)
		}
		return Plan{}, dbErr("insert plan", err)
	}
	for _, s := range p.Steps {
		if s.State == "" {
			s.State = StepPending
		}
		inputs := s.EvidenceInputs
		if len(inputs) == 0 {
			inputs = json.RawMessage("{}")
		}
		deps := s.DependsOn
		if deps == nil {
			deps = []StepID{}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO execution_plan_steps (id, plan_id, seq, type, depends_on, state, semantic_idempotency_key, retry_class, timeout_ms,
				finality_policy, compensation_policy, evidence_inputs, evidence_output, attempts)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			s.ID, p.ID, s.Seq, string(s.Type), deps, string(s.State), s.SemanticIdempotencyKey, string(s.RetryClass), s.Timeout.Milliseconds(),
			s.FinalityPolicy, string(s.CompensationPolicy), inputs, s.EvidenceOutput, s.Attempts); err != nil {
			return Plan{}, dbErr("insert plan step", err)
		}
	}
	action := AuditPlanCreated
	if p.Status == PlanNoValidPlan {
		action = AuditNoValidPlan
	}
	payload := map[string]any{
		"plan_id": p.ID.String(), "intent_id": p.IntentID, "version": p.Version, "planner_version": p.PlannerVersion, "status": string(p.Status),
		"plan_hash": p.Hash, "reasons": reasons, "dry_run": p.DryRun, "selected_venue_listing_id": p.SelectedVenueListingID, "steps": len(p.Steps),
	}
	if err := r.appendAudit(ctx, tx, p.AccountID, "", "", action, p.ID.String(), "plan created", payload, now); err != nil {
		return Plan{}, err
	}
	return r.Get(ctx, tx, p.ID)
}

// Get returns a plan with its steps in sequence order.
func (r *PlanRepository) Get(ctx context.Context, q db.Querier, planID PlanID) (Plan, error) {
	p, err := scanPlan(q.QueryRow(ctx, `SELECT `+planColumns+` FROM execution_plans p LEFT JOIN trade_intents i ON i.id = p.intent_id WHERE p.id = $1`, planID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Plan{}, errs.New(errs.CodeNotFound, "plan not found").WithField("plan_id", planID.String())
		}
		return Plan{}, dbErr("get plan", err)
	}
	steps, err := r.steps(ctx, q, planID)
	if err != nil {
		return Plan{}, err
	}
	p.Steps = steps
	return p, nil
}

// LatestForIntent returns the highest-version plan of an intent.
func (r *PlanRepository) LatestForIntent(ctx context.Context, q db.Querier, intentID string) (Plan, error) {
	p, err := scanPlan(q.QueryRow(ctx, `SELECT `+planColumns+` FROM execution_plans p LEFT JOIN trade_intents i ON i.id = p.intent_id
		WHERE p.intent_id = $1::uuid ORDER BY p.version DESC LIMIT 1`, intentID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Plan{}, errs.New(errs.CodeNotFound, "plan not found").WithField("intent_id", intentID)
		}
		return Plan{}, dbErr("get latest plan", err)
	}
	steps, err := r.steps(ctx, q, p.ID)
	if err != nil {
		return Plan{}, err
	}
	p.Steps = steps
	return p, nil
}

func (r *PlanRepository) steps(ctx context.Context, q db.Querier, planID PlanID) ([]Step, error) {
	rows, err := q.Query(ctx, `SELECT `+stepColumns+` FROM execution_plan_steps WHERE plan_id = $1 ORDER BY seq`, planID)
	if err != nil {
		return nil, dbErr("list plan steps", err)
	}
	defer rows.Close()
	steps := []Step{}
	for rows.Next() {
		s, err := scanStep(rows)
		if err != nil {
			return nil, dbErr("scan plan step", err)
		}
		steps = append(steps, s)
	}
	if err := rows.Err(); err != nil {
		return nil, dbErr("list plan steps", err)
	}
	return steps, nil
}

// Approve freezes a DRAFT plan: status APPROVED and approved_at set. After
// this call the frozen columns are guarded by the database trigger.
func (r *PlanRepository) Approve(ctx context.Context, tx pgx.Tx, planID PlanID, actorType, actorID, reason string) (Plan, error) {
	if err := r.check(); err != nil {
		return Plan{}, err
	}
	p, err := r.lock(ctx, tx, planID)
	if err != nil {
		return Plan{}, err
	}
	if p.Status != PlanDraft {
		return Plan{}, errs.Newf(errs.CodeInvalidStateTransition, "plan is %s; only DRAFT plans can be approved", p.Status).
			WithField("plan_id", p.ID.String()).WithField("from", string(p.Status))
	}
	now := r.clk.Now()
	if _, err := tx.Exec(ctx, `UPDATE execution_plans SET status = $2, approved_at = $3 WHERE id = $1 AND status = $4`,
		p.ID, string(PlanApproved), now, string(PlanDraft)); err != nil {
		return Plan{}, dbErr("approve plan", err)
	}
	payload := map[string]any{"plan_id": p.ID.String(), "intent_id": p.IntentID, "version": p.Version, "plan_hash": p.Hash}
	if err := r.appendAudit(ctx, tx, p.AccountID, actorType, actorID, AuditPlanApproved, p.ID.String(), reason, payload, now); err != nil {
		return Plan{}, err
	}
	return r.Get(ctx, tx, p.ID)
}

// Supersede marks the current plan SUPERSEDED and creates next as the
// following version of the same intent. next.Version must be
// current.Version + 1.
func (r *PlanRepository) Supersede(ctx context.Context, tx pgx.Tx, currentID PlanID, next Plan, actorType, actorID, reason string) (Plan, error) {
	if err := r.check(); err != nil {
		return Plan{}, err
	}
	cur, err := r.lock(ctx, tx, currentID)
	if err != nil {
		return Plan{}, err
	}
	if !CanTransitionPlan(cur.Status, PlanSuperseded) {
		return Plan{}, errs.Newf(errs.CodeInvalidStateTransition, "plan is %s and cannot be superseded", cur.Status).
			WithField("plan_id", cur.ID.String()).WithField("from", string(cur.Status))
	}
	if next.IntentID != cur.IntentID {
		return Plan{}, errs.New(errs.CodeValidationFailed, "settlement: a superseding plan must belong to the same intent")
	}
	if next.Version != cur.Version+1 {
		return Plan{}, errs.Newf(errs.CodeValidationFailed, "settlement: superseding plan must be version %d", cur.Version+1).
			WithField("version", next.Version)
	}
	now := r.clk.Now()
	if _, err := tx.Exec(ctx, `UPDATE execution_plans SET status = $2, finished_at = $3 WHERE id = $1 AND status = $4`,
		cur.ID, string(PlanSuperseded), now, string(cur.Status)); err != nil {
		return Plan{}, dbErr("supersede plan", err)
	}
	if next.AccountID == "" {
		next.AccountID = cur.AccountID
	}
	created, err := r.Create(ctx, tx, next)
	if err != nil {
		return Plan{}, err
	}
	payload := map[string]any{"plan_id": cur.ID.String(), "superseded_by": created.ID.String(), "version": cur.Version, "next_version": created.Version}
	if err := r.appendAudit(ctx, tx, cur.AccountID, actorType, actorID, AuditPlanSuperseded, cur.ID.String(), reason, payload, now); err != nil {
		return Plan{}, err
	}
	return created, nil
}

// SetStatus applies a lifecycle transition (compare-and-set on from).
func (r *PlanRepository) SetStatus(ctx context.Context, tx pgx.Tx, planID PlanID, from, to PlanStatus, reason string) (Plan, error) {
	if err := r.check(); err != nil {
		return Plan{}, err
	}
	if !CanTransitionPlan(from, to) {
		return Plan{}, errs.Newf(errs.CodeInvalidStateTransition, "plan %s -> %s is not allowed", from, to).
			WithField("plan_id", planID.String()).WithField("from", string(from)).WithField("to", string(to))
	}
	now := r.clk.Now()
	var finished *time.Time
	if to.Terminal() {
		t := now
		finished = &t
	}
	tag, err := tx.Exec(ctx, `UPDATE execution_plans SET status = $2, finished_at = coalesce(finished_at, $3) WHERE id = $1 AND status = $4`,
		planID, string(to), finished, string(from))
	if err != nil {
		return Plan{}, dbErr("set plan status", err)
	}
	if tag.RowsAffected() != 1 {
		return Plan{}, errs.New(errs.CodeConflict, "settlement: plan status changed concurrently").WithField("plan_id", planID.String())
	}
	p, err := r.Get(ctx, tx, planID)
	if err != nil {
		return Plan{}, err
	}
	payload := map[string]any{"plan_id": p.ID.String(), "from": string(from), "to": string(to)}
	if err := r.appendAudit(ctx, tx, p.AccountID, "", "", AuditPlanStatus, p.ID.String(), reason, payload, now); err != nil {
		return Plan{}, err
	}
	return p, nil
}

// MarkStep moves a step from one state to another (compare-and-set),
// recording evidence output and the last error. RUNNING stamps started_at
// and increments attempts; terminal states stamp finished_at.
func (r *PlanRepository) MarkStep(ctx context.Context, tx pgx.Tx, stepID StepID, from, to StepState, out StepOutcome) (Step, error) {
	if !to.Valid() {
		return Step{}, errs.New(errs.CodeValidationFailed, "settlement: unknown step state").WithField("state", string(to))
	}
	now := r.clk.Now()
	var output []byte
	if out.EvidenceOutput != nil {
		b, err := json.Marshal(out.EvidenceOutput)
		if err != nil {
			return Step{}, errs.Wrap(err, errs.CodeInternal, "settlement: encode step output")
		}
		output = b
	}
	var started, finished *time.Time
	attemptsDelta := 0
	switch to {
	case StepRunning:
		t := now
		started = &t
		attemptsDelta = 1
	case StepSucceeded, StepFailed, StepUnknown, StepSkipped, StepCompensated:
		t := now
		finished = &t
	}
	s, err := scanStep(tx.QueryRow(ctx, `UPDATE execution_plan_steps SET state = $2, evidence_output = coalesce($3, evidence_output),
			last_error = $4, attempts = attempts + $5, started_at = coalesce($6, started_at), finished_at = $7
		WHERE id = $1 AND state = $8 RETURNING `+stepColumns,
		stepID, string(to), output, nullable(out.LastError), attemptsDelta, started, finished, string(from)))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Step{}, errs.New(errs.CodeConflict, "settlement: step state changed concurrently").
				WithField("step_id", stepID.String()).WithField("expected", string(from))
		}
		return Step{}, dbErr("mark step", err)
	}
	return s, nil
}

// ResetSteps returns every step with seq >= fromSeq to PENDING so the plan
// can build a fresh attempt (PART 48 step 7). Evidence of the previous run
// stays in the audit log and on the attempt rows.
func (r *PlanRepository) ResetSteps(ctx context.Context, tx pgx.Tx, planID PlanID, fromSeq int32) error {
	if _, err := tx.Exec(ctx, `UPDATE execution_plan_steps SET state = $3, evidence_output = NULL, last_error = NULL, started_at = NULL, finished_at = NULL
		WHERE plan_id = $1 AND seq >= $2 AND state <> $3`, planID, fromSeq, string(StepPending)); err != nil {
		return dbErr("reset steps", err)
	}
	return nil
}

func (r *PlanRepository) lock(ctx context.Context, tx pgx.Tx, planID PlanID) (Plan, error) {
	p, err := scanPlan(tx.QueryRow(ctx, `SELECT `+planColumns+` FROM execution_plans p LEFT JOIN trade_intents i ON i.id = p.intent_id
		WHERE p.id = $1 FOR UPDATE OF p`, planID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Plan{}, errs.New(errs.CodeNotFound, "plan not found").WithField("plan_id", planID.String())
		}
		return Plan{}, dbErr("lock plan", err)
	}
	return p, nil
}

func (r *PlanRepository) appendAudit(ctx context.Context, tx pgx.Tx, accountID, actorType, actorID, action, resourceID, reason string, payload any, at time.Time) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "settlement: encode audit payload")
	}
	if actorType == "" {
		actorType = string(security.ActorSystem)
	}
	if actorID == "" {
		actorID = "settlement"
	}
	if _, err := r.audit.Append(ctx, tx, audit.Event{
		Stream: audit.AccountStream(accountID), ActorType: actorType, ActorID: actorID, Action: action,
		ResourceType: "execution_plan", ResourceID: resourceID, Reason: reason, Payload: body, OccurredAt: at.UTC(),
	}); err != nil {
		return errs.Wrap(err, errs.CodeInternal, "settlement: append audit event")
	}
	return nil
}

func (r *PlanRepository) check() error {
	if r.audit == nil {
		return errs.New(errs.CodeInternal, "settlement: audit writer is not configured")
	}
	return nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullableInt(n int32) *int32 {
	if n == 0 {
		return nil
	}
	return &n
}

func retryClass(s string) provider.RetryClass { return provider.RetryClass(s) }

func nullableAsset(a assets.AssetID) *assets.AssetID {
	if a.IsZero() {
		return nil
	}
	return &a
}
