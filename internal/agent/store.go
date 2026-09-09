package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

const agentColumns = `id, account_id::text, strategy_id::text, coalesce(strategy_version_id::text, ''), name,
       stage, state, coalesce(mode, ''), coalesce(envelope_id::text, ''), coalesce(risk_policy_version, ''),
       coalesce(superseded_by_agent_id::text, ''), coalesce(failure_reason, ''), version,
       created_by_actor_type, created_by_actor_id, created_at, updated_at`

func scanAgent(row pgx.Row) (Agent, error) {
	var (
		a                 Agent
		stage, state, mod string
	)
	err := row.Scan(&a.ID, &a.AccountID, &a.StrategyID, &a.StrategyVersionID, &a.Name,
		&stage, &state, &mod, &a.EnvelopeID, &a.RiskPolicyVersion,
		&a.SupersededByAgentID, &a.FailureReason, &a.Version,
		&a.CreatedByActorType, &a.CreatedByActorID, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return Agent{}, err
	}
	a.Stage = Stage(stage)
	a.State = State(state)
	a.Mode = Mode(mod)
	return a, nil
}

const insertAgentSQL = `
INSERT INTO agents (
    id, account_id, strategy_id, strategy_version_id, name, stage, state, mode, envelope_id,
    risk_policy_version, version, created_by_actor_type, created_by_actor_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`

const updateAgentStateSQL = `
UPDATE agents
   SET stage = $2, state = $3, mode = $4, envelope_id = $5, strategy_version_id = $6,
       risk_policy_version = $7, superseded_by_agent_id = $8, failure_reason = $9
 WHERE id = $1`

// Store reads and writes the agent-runtime tables. It holds no connection and
// no state; every method takes the caller's Querier or transaction so a
// change and its transition row commit together.
type Store struct{}

// NewStore returns the PostgreSQL store.
func NewStore() Store { return Store{} }

// Get returns one agent, or NOT_FOUND.
func (Store) Get(ctx context.Context, q db.Querier, agentID AgentID) (Agent, error) {
	a, err := scanAgent(q.QueryRow(ctx, `SELECT `+agentColumns+` FROM agents WHERE id = $1`, agentID))
	if err != nil {
		if isNoRows(err) {
			return Agent{}, errs.Newf(errs.CodeNotFound, "agent: %s not found", agentID).
				WithField("agent_id", agentID.String())
		}
		return Agent{}, errs.Wrap(err, errs.CodeInternal, "agent: load agent")
	}
	return a, nil
}

// GetForUpdate returns one agent with its row locked, so two concurrent
// lifecycle commands serialize.
func (Store) GetForUpdate(ctx context.Context, tx pgx.Tx, agentID AgentID) (Agent, error) {
	a, err := scanAgent(tx.QueryRow(ctx, `SELECT `+agentColumns+` FROM agents WHERE id = $1 FOR UPDATE`, agentID))
	if err != nil {
		if isNoRows(err) {
			return Agent{}, errs.Newf(errs.CodeNotFound, "agent: %s not found", agentID).
				WithField("agent_id", agentID.String())
		}
		return Agent{}, errs.Wrap(err, errs.CodeInternal, "agent: lock agent")
	}
	return a, nil
}

// AgentCursor is a keyset position in the runnable-agent listing. The zero
// value starts at the beginning.
type AgentCursor struct {
	CreatedAt time.Time
	ID        AgentID
}

// IsZero reports whether the cursor is the start of the listing.
func (c AgentCursor) IsZero() bool { return c.CreatedAt.IsZero() }

// ListRunnable returns the first page of agents whose stage is on the ladder
// at BACKTEST_ELIGIBLE or above. Use ListRunnablePage to walk the rest.
func (s Store) ListRunnable(ctx context.Context, q db.Querier, limit int) ([]Agent, error) {
	return s.ListRunnablePage(ctx, q, AgentCursor{}, limit)
}

// ListRunnablePage returns one page of runnable agents after the cursor,
// ordered by (created_at, id) so a dispatcher can walk every one of them.
// Paging matters: a deployment with more agents than one page would otherwise
// silently never trigger the ones past the limit.
//
// It deliberately does not filter out paused agents, and judges eligibility on
// the stage rather than the state. A pause is decided in one place — the
// dispatcher's own pause check — so the suppression can be recorded as
// SKIPPED{AGENT_PAUSED} rather than vanishing. FAILED, REVOKED and SUPERSEDED
// agents are excluded outright: they are finished, not suppressed.
func (Store) ListRunnablePage(ctx context.Context, q db.Querier, after AgentCursor, limit int) ([]Agent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	const runnableStages = `a.stage IN ('BACKTEST_ELIGIBLE','SHADOW','CANARY','LIMITED','LIVE')
   AND a.state NOT IN ('FAILED','REVOKED','SUPERSEDED')`
	var (
		rows pgx.Rows
		err  error
	)
	if after.IsZero() {
		rows, err = q.Query(ctx, `SELECT `+agentColumns+` FROM agents a
 WHERE `+runnableStages+`
 ORDER BY a.created_at, a.id LIMIT $1`, limit)
	} else {
		rows, err = q.Query(ctx, `SELECT `+agentColumns+` FROM agents a
 WHERE `+runnableStages+`
   AND (a.created_at, a.id) > ($1, $2)
 ORDER BY a.created_at, a.id LIMIT $3`, after.CreatedAt, after.ID, limit)
	}
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "agent: list runnable agents")
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		a, serr := scanAgent(rows)
		if serr != nil {
			return nil, errs.Wrap(serr, errs.CodeInternal, "agent: scan agent")
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "agent: iterate agents")
	}
	return out, nil
}

// insertAgent writes a new agents row.
func insertAgent(ctx context.Context, tx pgx.Tx, a Agent) error {
	_, err := tx.Exec(ctx, insertAgentSQL,
		a.ID, a.AccountID, a.StrategyID, nullUUID(a.StrategyVersionID), a.Name,
		string(a.Stage), string(a.State), nullText(string(a.Mode)), nullUUID(a.EnvelopeID),
		nullText(a.RiskPolicyVersion), a.Version, a.CreatedByActorType, a.CreatedByActorID)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return errs.New(errs.CodeConflict, "agent: already exists").WithField("agent_id", a.ID.String())
		}
		return errs.Wrap(err, errs.CodeInternal, "agent: insert agent")
	}
	return nil
}

// updateAgentState writes the new lifecycle columns. Migration 00690 refuses
// the UPDATE at COMMIT (SQLSTATE AU001) unless a matching
// agent_lifecycle_transitions row was inserted in the same transaction.
func updateAgentState(ctx context.Context, tx pgx.Tx, a Agent) error {
	_, err := tx.Exec(ctx, updateAgentStateSQL,
		a.ID, string(a.Stage), string(a.State), nullText(string(a.Mode)), nullUUID(a.EnvelopeID),
		nullUUID(a.StrategyVersionID), nullText(a.RiskPolicyVersion),
		nullUUID(a.SupersededByAgentID), nullText(a.FailureReason))
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "agent: update agent state")
	}
	return nil
}

const insertTransitionSQL = `
INSERT INTO agent_lifecycle_transitions (
    id, agent_id, from_state, to_state, from_stage, to_stage, actor_type, actor_id, reason,
    approval_id, strategy_version_id, ir_hash, evaluation_dataset_ref, evaluation_dataset_hash,
    risk_policy_version, risk_policy_hash, backtest_id, performance_snapshot_id,
    calibration_snapshot_id, error_rate_bps, operational_health, evidence, evidence_hash,
    build_version, correlation_id, occurred_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9,
    $10, $11, $12, $13, $14,
    $15, $16, $17, $18,
    $19, $20, $21, $22, $23,
    $24, $25, $26
)`

// Transition is one immutable agent_lifecycle_transitions row.
type Transition struct {
	ID                    TransitionID
	AgentID               AgentID
	FromState             State
	ToState               State
	FromStage             Stage
	ToStage               Stage
	ActorType             security.ActorType
	ActorID               string
	Reason                string
	ApprovalID            string
	StrategyVersionID     string
	IRHash                []byte
	DatasetRef            string
	DatasetHash           []byte
	RiskPolicyVersion     string
	RiskPolicyHash        []byte
	BacktestID            string
	PerformanceSnapshotID string
	CalibrationSnapshotID string
	ErrorRateBPS          *int
	OperationalHealth     json.RawMessage
	Evidence              json.RawMessage
	EvidenceHash          []byte
	BuildVersion          string
	CorrelationID         string
	OccurredAt            time.Time
}

// insertTransition writes the immutable lifecycle row.
func insertTransition(ctx context.Context, tx pgx.Tx, t Transition) error {
	health := t.OperationalHealth
	if len(health) == 0 {
		health = json.RawMessage(`{}`)
	}
	evidence := t.Evidence
	if len(evidence) == 0 {
		evidence = json.RawMessage(`[]`)
	}
	_, err := tx.Exec(ctx, insertTransitionSQL,
		t.ID, t.AgentID, string(t.FromState), string(t.ToState), string(t.FromStage), string(t.ToStage),
		string(t.ActorType), t.ActorID, t.Reason,
		nullUUID(t.ApprovalID), nullUUID(t.StrategyVersionID), nilBytes(t.IRHash), nullText(t.DatasetRef), nilBytes(t.DatasetHash),
		nullText(t.RiskPolicyVersion), nilBytes(t.RiskPolicyHash), nullUUID(t.BacktestID), nullUUID(t.PerformanceSnapshotID),
		nullUUID(t.CalibrationSnapshotID), t.ErrorRateBPS, health, evidence, nilBytes(t.EvidenceHash),
		nullText(t.BuildVersion), nullText(t.CorrelationID), t.OccurredAt)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "agent: record lifecycle transition")
	}
	return nil
}

// ListTransitions returns an agent's lifecycle history in order.
func (Store) ListTransitions(ctx context.Context, q db.Querier, agentID AgentID, limit int) ([]Transition, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := q.Query(ctx, `
SELECT id, agent_id, from_state, to_state, from_stage, to_stage, actor_type, actor_id, reason,
       coalesce(approval_id::text, ''), coalesce(strategy_version_id::text, ''), ir_hash,
       coalesce(evaluation_dataset_ref, ''), evaluation_dataset_hash,
       coalesce(risk_policy_version, ''), risk_policy_hash,
       coalesce(backtest_id::text, ''), coalesce(performance_snapshot_id::text, ''),
       coalesce(calibration_snapshot_id::text, ''), error_rate_bps, operational_health, evidence,
       evidence_hash, coalesce(build_version, ''), coalesce(correlation_id, ''), occurred_at
  FROM agent_lifecycle_transitions WHERE agent_id = $1 ORDER BY occurred_at, id LIMIT $2`, agentID, limit)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "agent: list lifecycle transitions")
	}
	defer rows.Close()
	var out []Transition
	for rows.Next() {
		var (
			t                                          Transition
			fromState, toState, fromStage, toStage, at string
		)
		if err := rows.Scan(&t.ID, &t.AgentID, &fromState, &toState, &fromStage, &toStage, &at, &t.ActorID, &t.Reason,
			&t.ApprovalID, &t.StrategyVersionID, &t.IRHash, &t.DatasetRef, &t.DatasetHash,
			&t.RiskPolicyVersion, &t.RiskPolicyHash, &t.BacktestID, &t.PerformanceSnapshotID,
			&t.CalibrationSnapshotID, &t.ErrorRateBPS, &t.OperationalHealth, &t.Evidence,
			&t.EvidenceHash, &t.BuildVersion, &t.CorrelationID, &t.OccurredAt); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "agent: scan lifecycle transition")
		}
		t.FromState, t.ToState = State(fromState), State(toState)
		t.FromStage, t.ToStage = Stage(fromStage), Stage(toStage)
		t.ActorType = security.ActorType(at)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "agent: iterate lifecycle transitions")
	}
	return out, nil
}

const runColumns = `id, agent_id, agent_version, strategy_version_id::text, account_id::text,
       coalesce(envelope_id::text, ''), mode, trigger_name, trigger_kind, trigger_dedup_key,
       coalesce(trigger_event_id, ''), trigger_source_event_at, decision_time, status,
       coalesce(skip_reason, ''), coalesce(evaluator_version, ''), eval_input_hash, eval_output_hash,
       information_set_hash, signals, condition_results, rationale, evidence,
       tool_calls, model_calls, data_cost_usd_minor, model_cost_usd_minor,
       coalesce(prediction_id::text, ''), coalesce(intent_id::text, ''), coalesce(error, ''),
       started_at, finished_at, correlation_id, coalesce(build_version, ''), updated_at`

func scanRun(row pgx.Row) (Run, error) {
	var (
		r                        Run
		mode, kind, status, skip string
		dataCost, modelCost      int64
	)
	err := row.Scan(&r.ID, &r.AgentID, &r.AgentVersion, &r.StrategyVersionID, &r.AccountID,
		&r.EnvelopeID, &mode, &r.TriggerName, &kind, &r.TriggerDedupKey,
		&r.TriggerEventID, &r.TriggerSourceEventAt, &r.DecisionTime, &status,
		&skip, &r.EvaluatorVersion, &r.EvalInputHash, &r.EvalOutputHash,
		&r.InformationSetHash, &r.Signals, &r.ConditionResults, &r.Rationale, &r.Evidence,
		&r.ToolCalls, &r.ModelCalls, &dataCost, &modelCost,
		&r.PredictionID, &r.IntentID, &r.Error,
		&r.StartedAt, &r.FinishedAt, &r.CorrelationID, &r.BuildVersion, &r.UpdatedAt)
	if err != nil {
		return Run{}, err
	}
	r.Mode = Mode(mode)
	r.TriggerKind = TriggerKind(kind)
	r.Status = RunStatus(status)
	r.SkipReason = SkipReason(skip)
	r.DataCostUSD = money.USDFromMinor(dataCost)
	r.ModelCostUSD = money.USDFromMinor(modelCost)
	return r, nil
}

// insertRunSQL carries skip_reason and finished_at because a run may be
// created already terminal: the dispatcher records a suppressed trigger as one
// SKIPPED row, and the agent_runs CHECK requires a skipped run to name its
// reason in the same statement.
const insertRunSQL = `
INSERT INTO agent_runs (
    id, agent_id, agent_version, strategy_version_id, account_id, envelope_id, mode,
    trigger_name, trigger_kind, trigger_dedup_key, trigger_event_id, trigger_source_event_at,
    decision_time, status, skip_reason, started_at, finished_at, correlation_id, build_version
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
ON CONFLICT (agent_id, trigger_dedup_key) DO NOTHING
RETURNING id`

// CreateRun inserts a run, deduplicated by (agent_id, trigger_dedup_key). It
// returns the run and whether it already existed, so a re-delivered event
// produces exactly one run.
func (s Store) CreateRun(ctx context.Context, tx pgx.Tx, r Run) (Run, bool, error) {
	if err := r.Validate(); err != nil {
		return Run{}, false, err
	}
	var got RunID
	err := tx.QueryRow(ctx, insertRunSQL,
		r.ID, r.AgentID, r.AgentVersion, r.StrategyVersionID, r.AccountID, nullUUID(r.EnvelopeID), string(r.Mode),
		r.TriggerName, string(r.TriggerKind), r.TriggerDedupKey, nullText(r.TriggerEventID), r.TriggerSourceEventAt,
		r.DecisionTime, string(r.Status), nullText(string(r.SkipReason)), r.StartedAt, r.FinishedAt,
		r.CorrelationID, nullText(r.BuildVersion)).Scan(&got)
	switch {
	case isNoRows(err):
		existing, gerr := s.GetRunByDedup(ctx, tx, r.AgentID, r.TriggerDedupKey)
		if gerr != nil {
			return Run{}, false, gerr
		}
		return existing, true, nil
	case err != nil:
		return Run{}, false, errs.Wrap(err, errs.CodeInternal, "agent: create run")
	}
	created, err := s.GetRun(ctx, tx, got)
	if err != nil {
		return Run{}, false, err
	}
	return created, false, nil
}

// GetRun returns one run, or NOT_FOUND.
func (Store) GetRun(ctx context.Context, q db.Querier, runID RunID) (Run, error) {
	r, err := scanRun(q.QueryRow(ctx, `SELECT `+runColumns+` FROM agent_runs WHERE id = $1`, runID))
	if err != nil {
		if isNoRows(err) {
			return Run{}, errs.Newf(errs.CodeNotFound, "agent: run %s not found", runID).
				WithField("run_id", runID.String())
		}
		return Run{}, errs.Wrap(err, errs.CodeInternal, "agent: load run")
	}
	return r, nil
}

// GetRunForUpdate returns one run with its row locked.
func (Store) GetRunForUpdate(ctx context.Context, tx pgx.Tx, runID RunID) (Run, error) {
	r, err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM agent_runs WHERE id = $1 FOR UPDATE`, runID))
	if err != nil {
		if isNoRows(err) {
			return Run{}, errs.Newf(errs.CodeNotFound, "agent: run %s not found", runID).
				WithField("run_id", runID.String())
		}
		return Run{}, errs.Wrap(err, errs.CodeInternal, "agent: lock run")
	}
	return r, nil
}

// GetRunByDedup returns the run created for a trigger dedup key.
func (Store) GetRunByDedup(ctx context.Context, q db.Querier, agentID AgentID, key []byte) (Run, error) {
	r, err := scanRun(q.QueryRow(ctx,
		`SELECT `+runColumns+` FROM agent_runs WHERE agent_id = $1 AND trigger_dedup_key = $2`, agentID, key))
	if err != nil {
		if isNoRows(err) {
			return Run{}, errs.New(errs.CodeNotFound, "agent: no run for that trigger key")
		}
		return Run{}, errs.Wrap(err, errs.CodeInternal, "agent: load run by dedup key")
	}
	return r, nil
}

const updateRunSQL = `
UPDATE agent_runs
   SET status = $2, skip_reason = $3, evaluator_version = $4, eval_input_hash = $5,
       eval_output_hash = $6, information_set_hash = $7, signals = $8, condition_results = $9,
       rationale = $10, evidence = $11, tool_calls = $12, model_calls = $13,
       data_cost_usd_minor = $14, model_cost_usd_minor = $15, prediction_id = $16,
       intent_id = $17, error = $18, finished_at = $19
 WHERE id = $1`

// UpdateRun writes the mutable columns of a run. The database refuses any
// change to identity, mode or already-set linkage (AG004), so this can only
// ever move a run forward.
func (Store) UpdateRun(ctx context.Context, tx pgx.Tx, r Run) error {
	if err := r.Validate(); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, updateRunSQL, r.ID, string(r.Status), nullText(string(r.SkipReason)),
		nullText(r.EvaluatorVersion), nilBytes(r.EvalInputHash), nilBytes(r.EvalOutputHash),
		nilBytes(r.InformationSetHash), jsonOr(r.Signals, "[]"), jsonOr(r.ConditionResults, "[]"),
		jsonOr(r.Rationale, "{}"), jsonOr(r.Evidence, "[]"), r.ToolCalls, r.ModelCalls,
		r.DataCostUSD.Minor(), r.ModelCostUSD.Minor(), nullUUID(r.PredictionID),
		nullUUID(r.IntentID), nullText(r.Error), r.FinishedAt)
	if err != nil {
		if IsAgentRunImmutable(err) {
			return errs.Wrap(err, errs.CodeConflict, "agent: run identity, mode and linkage are immutable")
		}
		return errs.Wrap(err, errs.CodeInternal, "agent: update run")
	}
	return nil
}

// CountSkipsSince counts runs of an agent skipped for reason since t. The
// dispatcher uses it to record AGENT_PAUSED at most once per hour.
func (Store) CountSkipsSince(ctx context.Context, q db.Querier, agentID AgentID, reason SkipReason, since time.Time) (int, error) {
	var n int
	err := q.QueryRow(ctx,
		`SELECT count(*) FROM agent_runs WHERE agent_id = $1 AND skip_reason = $2 AND started_at >= $3`,
		agentID, string(reason), since).Scan(&n)
	if err != nil {
		return 0, errs.Wrap(err, errs.CodeInternal, "agent: count skipped runs")
	}
	return n, nil
}

// jsonOr returns raw, or the given JSON default when raw is empty.
func jsonOr(raw json.RawMessage, def string) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(def)
	}
	return raw
}

// ListOpenRuns returns runs that have not reached a terminal status, oldest
// first, so a worker can resume work left by a crashed process.
func (Store) ListOpenRuns(ctx context.Context, q db.Querier, limit int) ([]Run, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := q.Query(ctx, `SELECT `+runColumns+` FROM agent_runs
 WHERE status IN ('STARTED','GATHERING','EVALUATED','PREDICTED')
 ORDER BY started_at LIMIT $1`, limit)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "agent: list open runs")
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "agent: scan open run")
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "agent: iterate open runs")
	}
	return out, nil
}

// LiveTradingCapability is the capability gate that must be ACTIVE in this
// environment before any agent may run in LIVE mode (PART 70).
const LiveTradingCapability = "LIVE_AGENT_TRADING"

// LiveTrading is this package's read of the LIVE_AGENT_TRADING gate at one
// instant. Reason names the condition that refused and is empty when Enabled;
// it is the string an operator sees next to a refused LIVE run, so "expired"
// must not be reported as "ACTIVE".
type LiveTrading struct {
	Enabled bool
	State   string
	Reason  string
}

// Refusal reasons. They deliberately match the wording of the equivalent
// conditions in internal/gates so that the two readers of one gate can be
// compared by anyone reading the logs, as well as by the agreement test.
const (
	reasonNoGateRow       = "no gate row"
	reasonStateNotActive  = "gate state is not ACTIVE"
	reasonRevoked         = "gate is revoked"
	reasonNoEffectiveAt   = "effective_at is not set"
	reasonNotYetEffective = "effective_at is in the future"
	reasonExpired         = "expires_at has passed"
)

// LiveTradingEnabled reports whether the LIVE_AGENT_TRADING gate permits live
// agent trading in env at now. It is a SELECT on capability_gates: this package
// must not import internal/gates (an agent tree can never reach the gate
// controller), and it must never be able to change a gate. The gate defaults
// to DISABLED and no environment variable can turn it on: only the dual-
// controlled gate workflow in internal/gates can, under an operator principal.
//
// It used to select effective_at and expires_at and then decide on `state`
// alone, discarding both -- so a gate whose window had closed still read as
// live here, while gates.Evaluate called the same row inactive (F-72). Nothing
// calls gates.Admin.ExpireDue, so the persisted state never catches up on its
// own: the divergence lasted for as long as the row sat there.
//
// The conditions below are the ones that can change after activation without a
// state transition. The quorum and evidence conditions gates.Evaluate also
// applies are not re-checked here, and need not be: Activate enforces them
// before it writes ACTIVE, and 00701 leaves cp_app no UPDATE on any column of
// this table except `version`, so no approver, evidence reference or window can
// be moved underneath us by the application at all.
func (Store) LiveTradingEnabled(ctx context.Context, q db.Querier, env string, now time.Time) (LiveTrading, error) {
	var (
		state       string
		effectiveAt *time.Time
		expiresAt   *time.Time
		revokedAt   *time.Time
	)
	err := q.QueryRow(ctx,
		`SELECT state, effective_at, expires_at, revoked_at FROM capability_gates
			WHERE capability = $1 AND environment = $2`,
		LiveTradingCapability, env).Scan(&state, &effectiveAt, &expiresAt, &revokedAt)
	switch {
	case isNoRows(err):
		// Absent means disabled: fail closed.
		return LiveTrading{State: "DISABLED", Reason: reasonNoGateRow}, nil
	case err != nil:
		return LiveTrading{}, errs.Wrap(err, errs.CodeInternal, "agent: read live trading gate")
	}
	now = now.UTC()
	refuse := func(reason string) (LiveTrading, error) {
		return LiveTrading{State: state, Reason: reason}, nil
	}
	switch {
	case state != "ACTIVE":
		return refuse(reasonStateNotActive)
	case revokedAt != nil:
		return refuse(reasonRevoked)
	case effectiveAt == nil:
		return refuse(reasonNoEffectiveAt)
	case now.Before(*effectiveAt):
		return refuse(reasonNotYetEffective)
	case expiresAt != nil && !now.Before(*expiresAt):
		return refuse(reasonExpired)
	}
	return LiveTrading{Enabled: true, State: state}, nil
}
