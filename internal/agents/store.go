package agents

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/agent"
	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// Store reads and writes the management-surface tables. It holds no connection
// and no state: every method takes the caller's Querier or transaction, so a
// grant, its agent row and the transition that licenses the agent's state
// commit together or not at all.
type Store struct{}

// NewStore returns the PostgreSQL store.
func NewStore() Store { return Store{} }

const insertAgentSQL = `
INSERT INTO agents (
    id, account_id, strategy_id, strategy_version_id, name, stage, state, mode, envelope_id,
    risk_policy_version, version, created_by_actor_type, created_by_actor_id
) VALUES ($1, $2, $3, $4, $5, 'DRAFT', 'DRAFT', NULL, NULL, $6, 1, $7, $8)`

// InsertAgent writes the agents row.
//
// Stage and state are literals rather than parameters, and mode and envelope
// are literal NULLs, because 00736 refuses an agent born anything but
// DRAFT/DRAFT with neither. Passing them in would make this function look like
// it had a choice about the one thing it must not have a choice about.
func (Store) InsertAgent(ctx context.Context, tx pgx.Tx, a agent.Agent) error {
	_, err := tx.Exec(ctx, insertAgentSQL,
		a.ID, a.AccountID, a.StrategyID, nullUUID(a.StrategyVersionID), a.Name,
		nullText(a.RiskPolicyVersion), a.CreatedByActorType, a.CreatedByActorID)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return errs.New(errs.CodeConflict, "agents: an agent with that name already exists on this account").
				WithField("name", a.Name)
		}
		return errs.Wrap(err, errs.CodeInternal, "agents: insert agent")
	}
	return nil
}

const insertTransitionSQL = `
INSERT INTO agent_lifecycle_transitions (
    id, agent_id, from_state, to_state, from_stage, to_stage, actor_type, actor_id, reason,
    strategy_version_id, risk_policy_version, build_version, correlation_id, occurred_at,
    to_mode, to_strategy_version_id, to_risk_policy_version, to_failure_reason
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`

// LifecycleChange is one move a person asked for. It is the only way this
// package changes an agent's state: since 00750 the application holds no UPDATE
// privilege on `agents` at all, and an AFTER INSERT trigger on the transitions
// table writes every mutable column from the `to_*` values below. Inserting the
// row IS the state change, which is why there is no second method here.
type LifecycleChange struct {
	AgentID           agent.AgentID
	From              agent.Agent
	ToState           agent.State
	ToStage           agent.Stage
	ToMode            agent.Mode
	StrategyVersionID string
	RiskPolicyVersion string
	FailureReason     string
	ActorType         security.ActorType
	ActorID           string
	Reason            string
	BuildVersion      string
	CorrelationID     string
	OccurredAt        time.Time
}

// ApplyLifecycle writes the transition row that moves the agent.
func (Store) ApplyLifecycle(ctx context.Context, tx pgx.Tx, c LifecycleChange) error {
	_, err := tx.Exec(ctx, insertTransitionSQL,
		agent.NewTransitionID(), c.AgentID,
		string(c.From.State), string(c.ToState), string(c.From.Stage), string(c.ToStage),
		string(c.ActorType), c.ActorID, c.Reason,
		nullUUID(c.StrategyVersionID), nullText(c.RiskPolicyVersion),
		nullText(c.BuildVersion), nullText(c.CorrelationID), c.OccurredAt,
		nullText(string(c.ToMode)), nullUUID(c.StrategyVersionID),
		nullText(c.RiskPolicyVersion), nullText(c.FailureReason))
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "agents: record lifecycle transition")
	}
	return nil
}

const insertGrantSQL = `
INSERT INTO agent_grants (
    id, agent_id, account_id, strategy_version_id, authority_level,
    budget_credits, per_trade_cap_credits, daily_loss_stop_credits, max_position_share_bps,
    allowed_asset_ids, schedule_kind, schedule_interval_minutes, granted_by_user_id, granted_at
) VALUES ($1, $2, $3, $4, $5, $6::numeric, $7::numeric, $8::numeric, $9, $10::uuid[], $11, $12, $13, $14)`

// InsertGrant writes the authority a person granted.
func (Store) InsertGrant(ctx context.Context, tx pgx.Tx, g Grant) error {
	var interval *int
	if g.Limits.Schedule.Kind == ScheduleInterval {
		v := g.Limits.Schedule.IntervalMinutes
		interval = &v
	}
	_, err := tx.Exec(ctx, insertGrantSQL,
		g.ID, g.AgentID, g.AccountID, g.StrategyVersionID, int(g.Level),
		g.Limits.BudgetCredits.String(), g.Limits.PerTradeCapCredits.String(),
		g.Limits.DailyLossStopCredits.String(), int64(g.Limits.MaxPositionShareBPS),
		g.Limits.AllowedAssetStrings(), string(g.Limits.Schedule.Kind), interval,
		g.GrantedByUserID, g.GrantedAt)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return errs.New(errs.CodeConflict, "agents: this agent already has a grant of authority").
				WithField("agent_id", g.AgentID.String())
		}
		return errs.Wrap(err, errs.CodeInternal, "agents: insert grant")
	}
	return nil
}

// ArchiveGrant stamps archived_at. The guard trigger refuses a second archive
// and refuses every other column, so this is the whole of what may change.
func (Store) ArchiveGrant(ctx context.Context, tx pgx.Tx, agentID agent.AgentID, at time.Time) error {
	tag, err := tx.Exec(ctx, `UPDATE agent_grants SET archived_at = $2 WHERE agent_id = $1 AND archived_at IS NULL`, agentID, at)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "agents: archive grant")
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.CodeConflict, "agents: this agent is already archived").
			WithField("agent_id", agentID.String())
	}
	return nil
}

// View is one agent as the product shows it: the agent row, the grant behind
// it, what its runs say, and the honest runtime state.
type View struct {
	Agent   agent.Agent
	Grant   Grant
	Runs    RuntimeEvidence
	Runtime RuntimeStatus
	// BudgetUsedCredits is Credits this agent has actually committed. It is
	// derived, never stored: a stored counter and a ledger disagree eventually,
	// and the ledger wins.
	BudgetUsedCredits money.Quantity
	// BudgetUsedSource says where that number came from, so a zero is never
	// mistaken for a measurement that was made and came out at zero.
	BudgetUsedSource string
	// Pause is the open pause, when there is one.
	Pause      agent.Pause
	PauseOpen  bool
	StrategyID string
	// Sandbox is true when the strategy version this agent deploys was compiled
	// by a compiler that exists only on a sandbox tier. Everything about this
	// agent is then a rehearsal, and the API and the page say so.
	Sandbox bool
}

// Sources of BudgetUsedCredits.
const (
	// BudgetSourceNoRuns: this agent has never been evaluated, so nothing has
	// been committed. The zero is a fact about the agent, not a placeholder.
	BudgetSourceNoRuns = "NO_RUNS_RECORDED"
	// BudgetSourceNoIntents: it has been evaluated and proposed nothing.
	BudgetSourceNoIntents = "NO_INTENTS_CREATED"
	// BudgetSourceCommittedIntents: summed from the intents its runs created
	// that reached a state where value is committed.
	BudgetSourceCommittedIntents = "COMMITTED_INTENTS"
)

const viewColumns = `
    a.id, a.account_id::text, a.strategy_id::text, coalesce(a.strategy_version_id::text, ''), a.name,
    a.stage, a.state, coalesce(a.mode, ''), coalesce(a.risk_policy_version, ''),
    coalesce(a.failure_reason, ''), a.version, a.created_by_actor_type, a.created_by_actor_id,
    a.created_at, a.updated_at,
    g.id, g.strategy_version_id::text, g.authority_level,
    g.budget_credits::text, g.per_trade_cap_credits::text, g.daily_loss_stop_credits::text,
    g.max_position_share_bps, g.allowed_asset_ids::text[], g.schedule_kind,
    coalesce(g.schedule_interval_minutes, 0), g.granted_by_user_id::text, g.granted_at, g.archived_at,
    (SELECT count(*) FROM agent_runs r WHERE r.agent_id = a.id),
    (SELECT count(*) FROM agent_runs r WHERE r.agent_id = a.id
        AND r.status IN ('STARTED','GATHERING','EVALUATED','PREDICTED')),
    (SELECT max(r.started_at) FROM agent_runs r WHERE r.agent_id = a.id),
    coalesce((SELECT r.status FROM agent_runs r WHERE r.agent_id = a.id ORDER BY r.started_at DESC LIMIT 1), ''),
    (SELECT count(*) FROM agent_runs r WHERE r.agent_id = a.id AND r.intent_id IS NOT NULL),
    coalesce((SELECT sum(i.quantity) FROM agent_runs r
                JOIN trade_intents i ON i.id = r.intent_id
               WHERE r.agent_id = a.id AND i.status = ANY($1::text[])), 0)::text,
    -- Whether the strategy version this agent deploys was compiled by a sandbox
    -- compiler. It is read from the VERSION rather than kept on the agent,
    -- because it is a fact about the document and an agent that could carry a
    -- different answer from its own strategy would be the mislabelling the
    -- temperature rule exists to prevent (D-129).
    coalesce((SELECT sv.sandbox FROM strategy_versions sv WHERE sv.id = a.strategy_version_id), false)`

// committedIntentStatuses are the intent states in which value is actually
// committed. RECEIVED, ELIGIBILITY_CHECKED and RISK_CHECKED are not among them:
// nothing is reserved yet and counting them would report a budget as spent
// because an agent thought about spending it.
func committedIntentStatuses() []string {
	return []string{
		string(intent.StatusReserved), string(intent.StatusPlanned),
		string(intent.StatusExecuting), string(intent.StatusCompleted),
	}
}

func scanView(row pgx.Row) (View, error) {
	var (
		v                      View
		stage, state, mode     string
		level                  int
		budget, perTrade, loss string
		shareBPS               int64
		assetIDs               []string
		schedKind              string
		schedInterval          int
		archivedAt             *time.Time
		totalRuns, openRuns    int
		lastRunAt              *time.Time
		lastRunStatus          string
		runsWithIntent         int
		committed              string
	)
	err := row.Scan(
		&v.Agent.ID, &v.Agent.AccountID, &v.Agent.StrategyID, &v.Agent.StrategyVersionID, &v.Agent.Name,
		&stage, &state, &mode, &v.Agent.RiskPolicyVersion,
		&v.Agent.FailureReason, &v.Agent.Version, &v.Agent.CreatedByActorType, &v.Agent.CreatedByActorID,
		&v.Agent.CreatedAt, &v.Agent.UpdatedAt,
		&v.Grant.ID, &v.Grant.StrategyVersionID, &level,
		&budget, &perTrade, &loss,
		&shareBPS, &assetIDs, &schedKind, &schedInterval, &v.Grant.GrantedByUserID, &v.Grant.GrantedAt, &archivedAt,
		&totalRuns, &openRuns, &lastRunAt, &lastRunStatus, &runsWithIntent, &committed, &v.Sandbox,
	)
	if err != nil {
		return View{}, err
	}
	v.Agent.Stage = agent.Stage(stage)
	v.Agent.State = agent.State(state)
	v.Agent.Mode = agent.Mode(mode)
	v.StrategyID = v.Agent.StrategyID

	v.Grant.AgentID = v.Agent.ID
	v.Grant.AccountID = v.Agent.AccountID
	v.Grant.Level = agentauthority.Level(level)
	v.Grant.ArchivedAt = archivedAt
	if v.Grant.Limits.BudgetCredits, err = money.ParseQuantity(budget); err != nil {
		return View{}, errs.Wrap(err, errs.CodeInternal, "agents: budget is not an exact quantity")
	}
	if v.Grant.Limits.PerTradeCapCredits, err = money.ParseQuantity(perTrade); err != nil {
		return View{}, errs.Wrap(err, errs.CodeInternal, "agents: per-trade cap is not an exact quantity")
	}
	if v.Grant.Limits.DailyLossStopCredits, err = money.ParseQuantity(loss); err != nil {
		return View{}, errs.Wrap(err, errs.CodeInternal, "agents: daily loss stop is not an exact quantity")
	}
	v.Grant.Limits.MaxPositionShareBPS = money.BPS(shareBPS)
	for _, s := range assetIDs {
		a, perr := assets.ParseAssetID(s)
		if perr != nil {
			return View{}, errs.Wrap(perr, errs.CodeInternal, "agents: allowed asset is not a canonical id")
		}
		v.Grant.Limits.AllowedAssets = append(v.Grant.Limits.AllowedAssets, a)
	}
	v.Grant.Limits.Schedule = Schedule{Kind: ScheduleKind(schedKind), IntervalMinutes: schedInterval}

	v.Runs = RuntimeEvidence{
		OpenRuns: openRuns, TotalRuns: totalRuns,
		LastRunAt: lastRunAt, LastRunStatus: lastRunStatus,
	}
	used, perr := money.ParseQuantity(committed)
	if perr != nil {
		return View{}, errs.Wrap(perr, errs.CodeInternal, "agents: committed quantity is not exact")
	}
	v.BudgetUsedCredits = used
	switch {
	case totalRuns == 0:
		v.BudgetUsedSource = BudgetSourceNoRuns
	case runsWithIntent == 0:
		v.BudgetUsedSource = BudgetSourceNoIntents
	default:
		v.BudgetUsedSource = BudgetSourceCommittedIntents
	}
	return v, nil
}

// Get returns one agent's view. A missing agent, or one with no grant, is
// NOT_FOUND: an agents row this package did not create is not part of the
// product surface, and inventing a grant for it would be inventing the
// authority a person is supposed to have given.
func (Store) Get(ctx context.Context, q db.Querier, agentID agent.AgentID) (View, error) {
	row := q.QueryRow(ctx, `SELECT `+viewColumns+`
		FROM agents a JOIN agent_grants g ON g.agent_id = a.id
		WHERE a.id = $2`, committedIntentStatuses(), agentID)
	v, err := scanView(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return View{}, errs.New(errs.CodeNotFound, "agents: no such agent").WithField("agent_id", agentID.String())
	}
	if err != nil {
		return View{}, errs.Wrap(err, errs.CodeInternal, "agents: read agent")
	}
	return v, nil
}

// ListFilter bounds a listing.
type ListFilter struct {
	// AccountID restricts to one account. Empty lists every account, which
	// only the admin read model asks for.
	AccountID string
	// IncludeArchived brings archived grants back into the list.
	IncludeArchived bool
	Limit           int
}

func (f ListFilter) limit() int {
	if f.Limit <= 0 || f.Limit > 200 {
		return 50
	}
	return f.Limit
}

// List returns agents newest first.
func (Store) List(ctx context.Context, q db.Querier, f ListFilter) ([]View, error) {
	var where []string
	args := []any{committedIntentStatuses()}
	if f.AccountID != "" {
		args = append(args, f.AccountID)
		where = append(where, "a.account_id = $2")
	}
	if !f.IncludeArchived {
		where = append(where, "g.archived_at IS NULL")
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, f.limit())
	sql := `SELECT ` + viewColumns + `
		FROM agents a JOIN agent_grants g ON g.agent_id = a.id` + clause +
		` ORDER BY a.created_at DESC, a.id DESC LIMIT $` + strconv.Itoa(len(args))
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "agents: list agents")
	}
	defer rows.Close()
	out := []View{}
	for rows.Next() {
		v, serr := scanView(rows)
		if serr != nil {
			return nil, errs.Wrap(serr, errs.CodeInternal, "agents: scan agent")
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "agents: list agents")
	}
	return out, nil
}

const insertPauseSQL = `
INSERT INTO agent_pauses (
    id, agent_id, reason_code, reason, open_orders_policy,
    paused_by_actor_type, paused_by_actor_id, paused_at, correlation_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

// OpenPause writes the pause record. The partial unique index makes a second
// open pause impossible, so a double pause is a CONFLICT rather than two rows
// only one of which anybody would ever close.
func (Store) OpenPause(ctx context.Context, tx pgx.Tx, p agent.Pause) error {
	_, err := tx.Exec(ctx, insertPauseSQL,
		p.ID, p.AgentID, string(p.ReasonCode), p.Reason, string(p.OpenOrdersPolicy),
		string(p.PausedByActorType), p.PausedByActorID, p.PausedAt, nullText(p.CorrelationID))
	if err != nil {
		if db.IsUniqueViolation(err) {
			return errs.New(errs.CodeConflict, "agents: this agent is already paused").
				WithField("agent_id", p.AgentID.String())
		}
		return errs.Wrap(err, errs.CodeInternal, "agents: open pause")
	}
	return nil
}

// ClosePause closes the open pause. A resume is always by a person, which the
// table's CHECK enforces and the service refuses to attempt for SYSTEM.
func (Store) ClosePause(ctx context.Context, tx pgx.Tx, agentID agent.AgentID, at time.Time,
	actorType security.ActorType, actorID, reason string,
) error {
	tag, err := tx.Exec(ctx, `
		UPDATE agent_pauses
		   SET resumed_at = $2, resumed_by_actor_type = $3, resumed_by_actor_id = $4, resume_reason = $5
		 WHERE agent_id = $1 AND resumed_at IS NULL`,
		agentID, at, string(actorType), actorID, reason)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "agents: close pause")
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.CodeConflict, "agents: this agent has no open pause").
			WithField("agent_id", agentID.String())
	}
	return nil
}

// LockAgent takes the row lock the lifecycle change needs. cp_app holds UPDATE
// on `name` only (00750), which is exactly enough privilege for FOR UPDATE and
// exactly no privilege to write anything that matters.
func (Store) LockAgent(ctx context.Context, tx pgx.Tx, agentID agent.AgentID) (agent.Agent, error) {
	row := tx.QueryRow(ctx, `
		SELECT id, account_id::text, strategy_id::text, coalesce(strategy_version_id::text, ''), name,
		       stage, state, coalesce(mode, ''), coalesce(envelope_id::text, ''),
		       coalesce(risk_policy_version, ''), coalesce(superseded_by_agent_id::text, ''),
		       coalesce(failure_reason, ''), version, created_by_actor_type, created_by_actor_id,
		       created_at, updated_at
		  FROM agents WHERE id = $1 FOR UPDATE`, agentID)
	var (
		a                  agent.Agent
		stage, state, mode string
	)
	err := row.Scan(&a.ID, &a.AccountID, &a.StrategyID, &a.StrategyVersionID, &a.Name,
		&stage, &state, &mode, &a.EnvelopeID, &a.RiskPolicyVersion,
		&a.SupersededByAgentID, &a.FailureReason, &a.Version,
		&a.CreatedByActorType, &a.CreatedByActorID, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return agent.Agent{}, errs.New(errs.CodeNotFound, "agents: no such agent").WithField("agent_id", agentID.String())
	}
	if err != nil {
		return agent.Agent{}, errs.Wrap(err, errs.CodeInternal, "agents: lock agent")
	}
	a.Stage = agent.Stage(stage)
	a.State = agent.State(state)
	a.Mode = agent.Mode(mode)
	return a, nil
}

func nullText(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

func nullUUID(s string) *string {
	if s == "" || s == "00000000-0000-0000-0000-000000000000" {
		return nil
	}
	return &s
}
