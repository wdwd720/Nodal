//go:build integration

package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/agent"
	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/strategy"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// These tests need an isolated database, for the reason internal/agent's own
// suite gives: cp_app cannot delete an agent, a transition, a pause or a grant,
// so every test builds its own fixture rather than resetting shared state, and
// the suite is safe to run twice against the same database.
var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
	// The migration role, for the probe that must reach a column cp_app cannot
	// write, so the guard trigger is what refuses it rather than a privilege.
	testOwnerDB *db.DB
)

func TestMain(m *testing.M) { os.Exit(testMain(m)) }

func testMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "agents integration: migrate up:", err)
		return 1
	}
	var err error
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "agents-itest", MaxConns: 8}); err != nil {
		fmt.Fprintln(os.Stderr, "agents integration: open pool:", err)
		return 1
	}
	if testOwnerDB, err = db.Open(ctx, db.Config{URL: testMigrateURL, AppName: "agents-itest-owner", MaxConns: 2}); err != nil {
		fmt.Fprintln(os.Stderr, "agents integration: open owner pool:", err)
		return 1
	}
	code := m.Run()
	testOwnerDB.Close()
	testDB.Close()
	return code
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set; skipping integration test")
	}
}

func newUUID() string { return id.New[id.Any]().String() }

// --------------------------------------------------------------- fixture --

// fakeCapabilities answers the gate question without a gate plane, so a test
// can exercise both sides of the level-3 refusal. It is a double for the
// CHECKER, never for a gate: nothing here writes a capability_gates row, and a
// sandbox activation still says it is a sandbox one.
type fakeCapabilities struct {
	active  bool
	sandbox bool
	reason  string
	err     error
}

func (f *fakeCapabilities) Active(context.Context, db.Querier, string) (bool, bool, string, error) {
	if f.err != nil {
		return false, false, "", f.err
	}
	return f.active, f.sandbox, f.reason, nil
}

type recordingPublisher struct{ events []Event }

func (r *recordingPublisher) Publish(_ context.Context, e Event) error {
	r.events = append(r.events, e)
	return nil
}

type fixture struct {
	t          *testing.T
	clk        *clock.Fake
	svc        *Service
	strategies *StrategyService
	caps       *fakeCapabilities
	published  *recordingPublisher

	userID     string
	otherUser  string
	operatorID string
	accountID  string
	otherAcct  string
	strategyID string
	versionID  string
	assetID    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireEnv(t)
	ctx := context.Background()
	f := &fixture{
		t:         t,
		clk:       clock.NewFake(time.Now().UTC().Truncate(time.Microsecond)),
		caps:      &fakeCapabilities{reason: "no gate row"},
		published: &recordingPublisher{},
	}
	f.userID, f.otherUser, f.operatorID = newUUID(), newUUID(), newUUID()
	f.accountID, f.otherAcct = newUUID(), newUUID()
	f.strategyID, f.versionID, f.assetID = newUUID(), newUUID(), newUUID()

	svc, err := NewService(Deps{
		DB: testDB, Clock: f.clk, Capabilities: f.caps,
		Runtime: Deployment{}, Events: f.published, BuildVersion: "itest",
	})
	require.NoError(t, err)
	f.svc = svc

	st, err := NewStrategyService(StrategyDeps{DB: testDB, Clock: f.clk, CompilerVersion: "itest"})
	require.NoError(t, err)
	f.strategies = st

	suffix := f.accountID
	exec := func(sql string, args ...any) {
		t.Helper()
		_, eerr := testDB.Exec(ctx, sql, args...)
		require.NoError(t, eerr, sql)
	}
	for _, u := range []struct{ id, sub string }{
		{f.userID, "owner-" + suffix},
		{f.otherUser, "other-" + suffix},
		{f.operatorID, "operator-" + suffix},
	} {
		exec(`INSERT INTO users (id, idp_issuer, idp_subject, status) VALUES ($1, 'itest', $2, 'ACTIVE')`, u.id, u.sub)
	}
	exec(`INSERT INTO accounts (id, owner_user_id, kind, status) VALUES ($1, $2, 'CUSTOMER', 'ACTIVE')`, f.accountID, f.userID)
	exec(`INSERT INTO accounts (id, owner_user_id, kind, status) VALUES ($1, $2, 'CUSTOMER', 'ACTIVE')`, f.otherAcct, f.otherUser)
	// A Nodal-native asset: chain and mint_address are pinned by the internal
	// asset shape CHECK (00710), so the id is the mint.
	exec(`INSERT INTO assets (id, chain, mint_address, kind, symbol, name, decimals, risk_class, status, value_domain)
	      VALUES ($1::uuid, 'nodal-internal', $1, 'NATIVE_ASSET', $2, 'Fixture native asset', 6,
	              'SPECULATIVE', 'ACTIVE', 'INTERNAL_NATIVE_ASSET')`,
		f.assetID, "NDL"+suffix[:6])
	exec(`INSERT INTO strategies (id, owner_account_id, owner_user_id, name, description, source_kind, status,
	          created_by_actor_type, created_by_actor_id)
	      VALUES ($1, $2, $3::uuid, $4, 'buy the dip, slowly', 'NATURAL_LANGUAGE', 'ACTIVE', 'USER', $3)`,
		f.strategyID, f.accountID, f.userID, "strategy-"+suffix)
	exec(`INSERT INTO strategy_versions (id, strategy_id, version, schema_version, ir, ir_hash, effect_set,
	          status, source_kind, source_hash, compiler_version, risk_policy_version, risk_policy_hash,
	          model_budget, data_budget, envelope_requirements, human_readable, built_at,
	          accepted_by_user_id, accepted_at)
	      VALUES ($1, $2, 1, 1, '{"schema_version":1}'::jsonb, $3, ARRAY['READ_MARKET_DATA']::text[],
	              'ACCEPTED', 'NATURAL_LANGUAGE', $4, 'c/1', 'risk/v1', $5,
	              '{}'::jsonb, '{}'::jsonb, '{}'::jsonb, 'buy the dip, slowly', now(), $6, now())`,
		f.versionID, f.strategyID, bytes32("ir"), bytes32("src"), bytes32("risk"), f.userID)
	return f
}

func bytes32(seed string) []byte {
	out := make([]byte, 32)
	copy(out, seed)
	return out
}

// owner is the account's own customer principal with a recent strong
// authentication.
func (f *fixture) owner() context.Context {
	return security.WithPrincipal(context.Background(), security.Principal{
		SubjectID: f.userID, ActorType: security.ActorUser,
		Roles: []security.Role{security.RoleCustomer}, AccountIDs: []string{f.accountID},
		SessionID: newUUID(), AuthTime: f.clk.Now().Add(-time.Minute), AMR: []string{"pwd", "mfa"},
	})
}

// ownerStale is the same customer with an authentication too old to grant
// authority.
func (f *fixture) ownerStale() context.Context {
	return security.WithPrincipal(context.Background(), security.Principal{
		SubjectID: f.userID, ActorType: security.ActorUser,
		Roles: []security.Role{security.RoleCustomer}, AccountIDs: []string{f.accountID},
		SessionID: newUUID(), AuthTime: f.clk.Now().Add(-2 * time.Hour), AMR: []string{"pwd", "mfa"},
	})
}

func (f *fixture) stranger() context.Context {
	return security.WithPrincipal(context.Background(), security.Principal{
		SubjectID: f.otherUser, ActorType: security.ActorUser,
		Roles: []security.Role{security.RoleCustomer}, AccountIDs: []string{f.otherAcct},
		SessionID: newUUID(), AuthTime: f.clk.Now().Add(-time.Minute), AMR: []string{"pwd", "mfa"},
	})
}

func (f *fixture) operator() context.Context {
	return security.WithPrincipal(context.Background(), security.Principal{
		SubjectID: f.operatorID, ActorType: security.ActorOperator,
		Roles: []security.Role{security.RoleOperations}, SessionID: newUUID(),
		AuthTime: f.clk.Now().Add(-time.Minute), AMR: []string{"pwd", "mfa"},
	})
}

func (f *fixture) create(ctx context.Context, level agentauthority.Level, name string) (View, error) {
	f.t.Helper()
	return f.svc.Create(ctx, CreateRequest{
		AccountID: f.accountID, StrategyID: f.strategyID, StrategyVersionID: f.versionID,
		Name: name, Level: level, Limits: fixtureLimits(f.t, f.assetID),
	})
}

// missingAgentError is the error an id that names nothing produces, so the test
// above can assert that a stranger gets the identical answer rather than
// merely a 404 of its own.
func (f *fixture) missingAgentError(t *testing.T) error {
	t.Helper()
	_, err := f.svc.Get(f.stranger(), agent.NewAgentID())
	require.Error(t, err)
	return err
}

func fixtureLimits(t *testing.T, assetID string) Limits {
	t.Helper()
	a, err := assets.ParseAssetID(assetID)
	require.NoError(t, err)
	return Limits{
		BudgetCredits:        parseQty(t, "1000000"),
		PerTradeCapCredits:   parseQty(t, "50000"),
		DailyLossStopCredits: parseQty(t, "100000"),
		MaxPositionShareBPS:  2000,
		AllowedAssets:        []assets.AssetID{a},
		Schedule:             Schedule{Kind: ScheduleInterval, IntervalMinutes: 60},
	}
}

// ------------------------------------------------------------------ tests --

// TestIntegration_AnAgentIsBornStoppedAndItsGrantIsRecorded is the create path:
// the agent is DRAFT/DRAFT with no mode and no envelope (00736 refuses anything
// else), the grant records what the person authorised, and the read model tells
// the truth about a runtime that is not there.
func TestIntegration_AnAgentIsBornStoppedAndItsGrantIsRecorded(t *testing.T) {
	f := newFixture(t)
	ctx := f.owner()

	v, err := f.create(ctx, agentauthority.LevelRecommendation, "dip buyer")
	require.NoError(t, err)

	assert.Equal(t, agent.StageDraft, v.Agent.Stage)
	assert.Equal(t, agent.StateDraft, v.Agent.State)
	assert.Equal(t, agent.Mode(""), v.Agent.Mode, "an agent is born with no mode")
	assert.Equal(t, "", v.Agent.EnvelopeID, "an agent is born with no capital envelope")
	assert.Equal(t, agentauthority.LevelRecommendation, v.Grant.Level)
	assert.Equal(t, "1000000", v.Grant.Limits.BudgetCredits.String())
	assert.Equal(t, f.userID, v.Grant.GrantedByUserID)
	assert.False(t, v.Grant.Archived())

	// The budget is a limit and nothing moved.
	assert.Equal(t, "0", v.BudgetUsedCredits.String())
	assert.Equal(t, BudgetSourceNoRuns, v.BudgetUsedSource)

	// The runtime is honest.
	assert.Equal(t, ComponentNotDeployed, v.Runtime.Evaluator)
	assert.Equal(t, ComponentNotDeployed, v.Runtime.Executor)
	assert.Nil(t, v.Runtime.LastHeartbeat)

	// Creation is itself a transition, so the history starts with the act that
	// created the agent.
	assert.Equal(t, 1, countTransitions(t, v.Agent.ID))

	// A budget is a ceiling and not a reservation: creating an agent mints,
	// moves and freezes nothing. The account holds no Credit lot at all.
	assert.Zero(t, countRows(t, `SELECT count(*) FROM credit_lots WHERE account_id = $1`, f.accountID),
		"creating an agent must not touch the Credit ledger")

	require.NotEmpty(t, f.published.events)
	assert.Equal(t, EventAgentCreated, f.published.events[0].Kind)
	assert.Equal(t, ComponentNotDeployed, f.published.events[0].Runtime.Evaluator,
		"the event carries the runtime state so a notification cannot imply the agent is running")
}

func TestIntegration_AuthorityAboveThreeIsRefusedAtCreate(t *testing.T) {
	f := newFixture(t)
	for _, level := range []agentauthority.Level{
		agentauthority.LevelBoundedDiscretion,
		agentauthority.LevelAutonomousSelection,
		agentauthority.LevelAutonomousPortfolio,
	} {
		_, err := f.create(f.owner(), level, "over-"+level.Name())
		require.Error(t, err)
		e, ok := errs.As(err)
		require.True(t, ok)
		assert.Equal(t, errs.CodeCapabilityNotApproved, e.Code)
		assert.NotEmpty(t, e.Fields["required_capability"])
	}
}

// TestIntegration_EnablingWalksTheLadderOneRungAtATime: DRAFT to
// BACKTEST_ELIGIBLE is three rungs, none of them skipped, each with its own
// immutable transition row — which is what the ladder is for.
func TestIntegration_EnablingWalksTheLadderOneRungAtATime(t *testing.T) {
	f := newFixture(t)
	ctx := f.owner()
	v, err := f.create(ctx, agentauthority.LevelRecommendation, "walker")
	require.NoError(t, err)

	out, err := f.svc.Act(ctx, ActRequest{AgentID: v.Agent.ID, Action: ActionEnable, Reason: "turning it on"})
	require.NoError(t, err)
	assert.Equal(t, agent.StageBacktestEligible, out.Agent.Stage)
	assert.Equal(t, agent.StateBacktestEligible, out.Agent.State)
	assert.Equal(t, agent.ModePaper, out.Agent.Mode, "the safer of the two modes the stage admits")
	assert.True(t, Runnable(out.Agent.State))

	// 1 creation + 3 rungs.
	assert.Equal(t, 4, countTransitions(t, v.Agent.ID))
	assert.Equal(t, []string{"DRAFT", "COMPILED", "VALIDATED", "BACKTEST_ELIGIBLE"}, transitionStates(t, v.Agent.ID))

	// Enabled is still not running.
	assert.Equal(t, ComponentNotDeployed, out.Runtime.Evaluator)
	assert.Contains(t, out.Runtime.Detail, "not being evaluated")

	// And enabling twice is a conflict rather than a second walk.
	_, err = f.svc.Act(ctx, ActRequest{AgentID: v.Agent.ID, Action: ActionEnable})
	require.Error(t, err)
	e, _ := errs.As(err)
	assert.Equal(t, errs.CodeConflict, e.Code)
}

func TestIntegration_EnablingRequiresARecentStrongAuthentication(t *testing.T) {
	f := newFixture(t)
	v, err := f.create(f.owner(), agentauthority.LevelRecommendation, "stale")
	require.NoError(t, err)

	_, err = f.svc.Act(f.ownerStale(), ActRequest{AgentID: v.Agent.ID, Action: ActionEnable})
	require.Error(t, err)
	e, ok := errs.As(err)
	require.True(t, ok)
	assert.Equal(t, errs.CodeStepUpRequired, e.Code)
	assert.Equal(t, agent.StateDraft, reloadState(t, v.Agent.ID), "a refused enable moves nothing")
}

// TestIntegration_LevelThreeNeedsTheCapabilityAndSaysWhoseReasonItIs is the
// gate the brief asks for: an authority that acts without a person confirming
// cannot be enabled while the capability is inactive, and the refusal carries
// the gate's own reason rather than a paraphrase.
func TestIntegration_LevelThreeNeedsTheCapabilityAndSaysWhoseReasonItIs(t *testing.T) {
	f := newFixture(t)
	ctx := f.owner()
	f.caps.active = false
	f.caps.reason = "gate state is not ACTIVE"

	v, err := f.create(ctx, agentauthority.LevelUserApprovedRule, "executor")
	require.NoError(t, err, "creating a level-3 agent is permitted; enabling it is what is gated")

	_, err = f.svc.Act(ctx, ActRequest{AgentID: v.Agent.ID, Action: ActionEnable})
	require.Error(t, err)
	e, ok := errs.As(err)
	require.True(t, ok)
	assert.Equal(t, errs.CodeCapabilityNotApproved, e.Code)
	assert.Equal(t, ExecutionCapability, e.Fields["required_capability"])
	assert.Equal(t, "gate state is not ACTIVE", e.Fields["gate_reason"])
	assert.Equal(t, agent.StateDraft, reloadState(t, v.Agent.ID))

	// With the capability active the same request succeeds, so the refusal was
	// the gate and not something else wearing its name.
	f.caps.active = true
	out, err := f.svc.Act(ctx, ActRequest{AgentID: v.Agent.ID, Action: ActionEnable})
	require.NoError(t, err)
	assert.Equal(t, agent.StateBacktestEligible, out.Agent.State)
}

func TestIntegration_LevelsBelowThreeDoNotConsultTheCapability(t *testing.T) {
	f := newFixture(t)
	ctx := f.owner()
	f.caps.err = errs.New(errs.CodeInternal, "the gate plane must not be consulted for this level")

	for _, level := range []agentauthority.Level{
		agentauthority.LevelResearchOnly,
		agentauthority.LevelRecommendation,
		agentauthority.LevelPrepareTransaction,
	} {
		v, err := f.create(ctx, level, "below-"+level.Name())
		require.NoError(t, err)
		out, err := f.svc.Act(ctx, ActRequest{AgentID: v.Agent.ID, Action: ActionEnable})
		require.NoErrorf(t, err, "level %s proposes and never executes; it needs no capability", level)
		assert.Equal(t, agent.StateBacktestEligible, out.Agent.State)
	}
}

func TestIntegration_PauseResumeDisableArchive(t *testing.T) {
	f := newFixture(t)
	ctx := f.owner()
	v, err := f.create(ctx, agentauthority.LevelRecommendation, "lifecycle")
	require.NoError(t, err)
	_, err = f.svc.Act(ctx, ActRequest{AgentID: v.Agent.ID, Action: ActionEnable})
	require.NoError(t, err)

	paused, err := f.svc.Act(ctx, ActRequest{AgentID: v.Agent.ID, Action: ActionPause, Reason: "stopping for now"})
	require.NoError(t, err)
	assert.Equal(t, agent.StatePaused, paused.Agent.State)
	assert.Equal(t, agent.StageBacktestEligible, paused.Agent.Stage, "a pause returns to its stage, so it keeps it")
	require.True(t, paused.PauseOpen)
	assert.Equal(t, agent.PauseOwnerRequest, paused.Pause.ReasonCode)
	assert.Equal(t, agent.LeaveOpenOrders, paused.Pause.OpenOrdersPolicy,
		"PART 71: nothing is ever cancelled implicitly")

	// A second pause is a conflict, not a second open row.
	_, err = f.svc.Act(ctx, ActRequest{AgentID: v.Agent.ID, Action: ActionPause, Reason: "again please"})
	require.Error(t, err)
	e, _ := errs.As(err)
	assert.Equal(t, errs.CodeConflict, e.Code)

	resumed, err := f.svc.Act(ctx, ActRequest{AgentID: v.Agent.ID, Action: ActionResume, Reason: "back on"})
	require.NoError(t, err)
	assert.Equal(t, agent.StateBacktestEligible, resumed.Agent.State)
	assert.False(t, resumed.PauseOpen)
	assert.Zero(t, countRows(t, `SELECT count(*) FROM agent_pauses WHERE agent_id = $1 AND resumed_at IS NULL`, v.Agent.ID))

	// Archiving a live agent is refused: stopping it is a separate decision.
	_, err = f.svc.Act(ctx, ActRequest{AgentID: v.Agent.ID, Action: ActionArchive})
	require.Error(t, err)
	e, _ = errs.As(err)
	assert.Equal(t, errs.CodeInvalidStateTransition, e.Code)

	disabled, err := f.svc.Act(ctx, ActRequest{AgentID: v.Agent.ID, Action: ActionDisable, Reason: "done with it"})
	require.NoError(t, err)
	assert.Equal(t, agent.StateRevoked, disabled.Agent.State)

	// REVOKED is terminal: 00734 refuses a transition that claims to leave it,
	// and the service refuses before the database has to.
	_, err = f.svc.Act(ctx, ActRequest{AgentID: v.Agent.ID, Action: ActionEnable})
	require.Error(t, err)
	e, _ = errs.As(err)
	assert.Equal(t, errs.CodeInvalidStateTransition, e.Code)

	archived, err := f.svc.Act(ctx, ActRequest{AgentID: v.Agent.ID, Action: ActionArchive})
	require.NoError(t, err)
	assert.True(t, archived.Grant.Archived())
	assert.Equal(t, agent.StateRevoked, archived.Agent.State, "archiving changes no lifecycle state")

	// Archived agents leave the default list and come back when asked for.
	list, err := f.svc.List(ctx, f.accountID, false, 50)
	require.NoError(t, err)
	assert.Empty(t, list)
	list, err = f.svc.List(ctx, f.accountID, true, 50)
	require.NoError(t, err)
	require.Len(t, list, 1)

	// A second archive is refused by the guard rather than silently accepted.
	_, err = f.svc.Act(ctx, ActRequest{AgentID: v.Agent.ID, Action: ActionArchive})
	require.Error(t, err)
}

func TestIntegration_ResumeIsRefusedWhenNothingIsPaused(t *testing.T) {
	f := newFixture(t)
	ctx := f.owner()
	v, err := f.create(ctx, agentauthority.LevelRecommendation, "not paused")
	require.NoError(t, err)
	_, err = f.svc.Act(ctx, ActRequest{AgentID: v.Agent.ID, Action: ActionResume})
	require.Error(t, err)
	e, _ := errs.As(err)
	assert.Equal(t, errs.CodeInvalidStateTransition, e.Code)
}

// TestIntegration_OnlyTheOwnerActsOnTheirAgent: the ownership check is per
// request, not per route, and an operator has an operator surface instead.
func TestIntegration_OnlyTheOwnerActsOnTheirAgent(t *testing.T) {
	f := newFixture(t)
	v, err := f.create(f.owner(), agentauthority.LevelRecommendation, "mine")
	require.NoError(t, err)

	// NOT_FOUND rather than FORBIDDEN: to a principal who does not own it, this
	// agent does not exist. FORBIDDEN told a stranger that the id they guessed
	// names a real agent, and 404 on the next one told them it does not.
	_, err = f.svc.Act(f.stranger(), ActRequest{AgentID: v.Agent.ID, Action: ActionEnable})
	require.Error(t, err)
	e, _ := errs.As(err)
	assert.Equal(t, errs.CodeNotFound, e.Code)

	_, err = f.svc.Get(f.stranger(), v.Agent.ID)
	require.Error(t, err)
	e, _ = errs.As(err)
	assert.Equal(t, errs.CodeNotFound, e.Code)
	assert.Equal(t, errs.CodeOf(f.missingAgentError(t)), e.Code,
		"a stranger's answer is the same as the answer for an id that does not exist")

	// An operator cannot enable somebody's agent either, whatever they hold.
	_, err = f.svc.Act(f.operator(), ActRequest{AgentID: v.Agent.ID, Action: ActionEnable})
	require.Error(t, err)
	e, _ = errs.As(err)
	assert.Equal(t, errs.CodeForbidden, e.Code)
	assert.Equal(t, string(security.ActorOperator), e.Fields["actor_type"])
}

// TestIntegration_AnOperatorPauseIsRecordedAsAnOperatorPause is F-42's shape
// for this surface: same table, same transition, a reason code that says who
// stopped it, so the owner's own history shows plainly that somebody else did.
func TestIntegration_AnOperatorPauseIsRecordedAsAnOperatorPause(t *testing.T) {
	f := newFixture(t)
	owner := f.owner()
	v, err := f.create(owner, agentauthority.LevelRecommendation, "operator target")
	require.NoError(t, err)
	_, err = f.svc.Act(owner, ActRequest{AgentID: v.Agent.ID, Action: ActionEnable})
	require.NoError(t, err)

	out, err := f.svc.AdminPause(f.operator(), v.Agent.ID, "INC-1: suspected wash trading", "corr-1")
	require.NoError(t, err)
	assert.Equal(t, agent.StatePaused, out.Agent.State)
	require.True(t, out.PauseOpen)
	assert.Equal(t, agent.PauseOperator, out.Pause.ReasonCode)
	assert.Equal(t, security.ActorOperator, out.Pause.PausedByActorType)
	assert.Equal(t, f.operatorID, out.Pause.PausedByActorID)

	// The owner can lift it, because a resume is always by a person and the
	// owner is one. What they cannot do is undo the record.
	resumed, err := f.svc.Act(owner, ActRequest{AgentID: v.Agent.ID, Action: ActionResume, Reason: "cleared with support"})
	require.NoError(t, err)
	assert.False(t, resumed.PauseOpen)
	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM agent_pauses WHERE agent_id = $1 AND reason_code = 'OPERATOR'`, v.Agent.ID))
}

func TestIntegration_AnOperatorPauseNeedsAnOperatorAndAReason(t *testing.T) {
	f := newFixture(t)
	v, err := f.create(f.owner(), agentauthority.LevelRecommendation, "reasons")
	require.NoError(t, err)

	_, err = f.svc.AdminPause(f.owner(), v.Agent.ID, "not an operator at all", "")
	require.Error(t, err)
	e, _ := errs.As(err)
	assert.Equal(t, errs.CodeForbidden, e.Code)

	_, err = f.svc.AdminPause(f.operator(), v.Agent.ID, "short", "")
	require.Error(t, err)
	e, _ = errs.As(err)
	assert.Equal(t, errs.CodeValidationFailed, e.Code)
}

func TestIntegration_AdminListSeesEveryAccountAndTheOwnerListSeesOne(t *testing.T) {
	f := newFixture(t)
	_, err := f.create(f.owner(), agentauthority.LevelRecommendation, "listed")
	require.NoError(t, err)

	mine, err := f.svc.List(f.owner(), f.accountID, false, 50)
	require.NoError(t, err)
	require.Len(t, mine, 1)

	all, err := f.svc.AdminList(f.operator(), ListFilter{Limit: 200})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(all), 1)

	// A customer cannot reach the operator read model.
	_, err = f.svc.AdminList(f.owner(), ListFilter{})
	require.Error(t, err)
	e, _ := errs.As(err)
	assert.Equal(t, errs.CodeForbidden, e.Code)
}

// TestIntegration_TheGrantIsImmutableButForItsArchive: raising a budget is a
// new decision by the person who made the old one, not an UPDATE.
func TestIntegration_TheGrantIsImmutableButForItsArchive(t *testing.T) {
	f := newFixture(t)
	v, err := f.create(f.owner(), agentauthority.LevelRecommendation, "immutable")
	require.NoError(t, err)

	ctx := context.Background()
	// As the owner role, so the guard trigger is what refuses rather than the
	// column privilege cp_app lacks.
	_, err = testOwnerDB.Exec(ctx, `UPDATE agent_grants SET budget_credits = 999999999 WHERE agent_id = $1`, v.Agent.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AGENT_GRANT_IMMUTABLE")

	_, err = testOwnerDB.Exec(ctx, `UPDATE agent_grants SET authority_level = 5 WHERE agent_id = $1`, v.Agent.ID)
	require.Error(t, err, "an authority level is not raised by an UPDATE")

	_, err = testOwnerDB.Exec(ctx, `DELETE FROM agent_grants WHERE agent_id = $1`, v.Agent.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AGENT_GRANT_IMMUTABLE")
}

// TestIntegration_TheAuthorityCeilingInTheSchemaMatchesTheMatrix keeps the CHECK
// and agentauthority.MaxSupportedLevel from drifting: the schema is what stops a
// level nobody may grant from being stored, and Go is what decides which that
// is.
func TestIntegration_TheAuthorityCeilingInTheSchemaMatchesTheMatrix(t *testing.T) {
	requireEnv(t)
	var def string
	err := testDB.QueryRow(context.Background(),
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
		  WHERE conrelid = 'agent_grants'::regclass AND conname = 'agent_grants_authority_level_check'`).Scan(&def)
	require.NoError(t, err)
	assert.Contains(t, def, fmt.Sprintf("<= %d", int(agentauthority.MaxSupportedLevel)),
		"the column CHECK must stop where agentauthority.MaxSupportedLevel does: %s", def)
}

// --------------------------------------------------------------- strategy --

func TestIntegration_ADeploymentWithNoCompilerRecordsTheAttemptAndSaysSo(t *testing.T) {
	f := newFixture(t)
	ctx := f.owner()
	require.False(t, f.strategies.CompilerConfigured())

	st, err := f.strategies.Create(ctx, CreateStrategyRequest{
		AccountID: f.accountID, Name: "uncompilable " + f.accountID[:8],
		Description: "buy when the price falls ten percent and sell when it recovers",
	})
	require.NoError(t, err)
	assert.Nil(t, st.CurrentVersion)

	out, err := f.strategies.Compile(ctx, st.ID, "req-"+newUUID(), "corr")
	require.NoError(t, err, "an unconfigured compiler is an outcome, not an error")
	assert.Equal(t, strategy.OutcomeModelUnavailable, out.Outcome)
	assert.Equal(t, []string{CompilerUnavailable}, out.FailureCodes)
	assert.Contains(t, out.Detail, "no strategy compiler backend configured")
	assert.Nil(t, out.Version, "no IR is fabricated")

	// The attempt is a row, with the outcome the CHECK admits and the code that
	// says why.
	assert.Equal(t, 1, countRows(t,
		`SELECT count(*) FROM compile_attempts WHERE strategy_id = $1
		   AND outcome = 'MODEL_UNAVAILABLE' AND 'COMPILER_UNAVAILABLE' = ANY(failure_codes)
		   AND parse_result = 'NOT_ATTEMPTED' AND stage_reached = 'PROMPT'`, mustUUID(t, st.ID)))
	assert.Zero(t, countRows(t, `SELECT count(*) FROM strategy_versions WHERE strategy_id = $1`, mustUUID(t, st.ID)))
}

func TestIntegration_ACompilerBackendProducesAVersionAndEveryAttemptIsRecorded(t *testing.T) {
	f := newFixture(t)
	ctx := f.owner()
	st, err := f.strategies.Create(ctx, CreateStrategyRequest{
		AccountID: f.accountID, Name: "compilable " + f.accountID[:8],
		Description: "buy the dip", Constraints: json.RawMessage(`{"max_daily_trades":3}`),
	})
	require.NoError(t, err)
	assert.Contains(t, st.Description, "structured constraints",
		"a constraint the user stated up front is recorded with the description, not applied silently")

	sid, err := strategy.ParseStrategyID(st.ID)
	require.NoError(t, err)
	svc := f.withCompiler(&fakeCompiler{result: successResult(t, sid)})

	out, err := svc.Compile(ctx, st.ID, "req-"+newUUID(), "corr")
	require.NoError(t, err)
	assert.Equal(t, strategy.OutcomeSuccess, out.Outcome)
	require.NotNil(t, out.Version)
	assert.Equal(t, "buy the dip, in words a person can check", out.Version.HumanReadable)

	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM strategy_versions WHERE strategy_id = $1`, mustUUID(t, st.ID)))
	assert.Equal(t, 1, countRows(t,
		`SELECT count(*) FROM compile_attempts WHERE strategy_id = $1 AND outcome = 'SUCCESS'
		   AND strategy_version_id IS NOT NULL`, mustUUID(t, st.ID)))

	// The strategy now offers the version, which is what the review step reads.
	reloaded, err := svc.Get(ctx, st.ID)
	require.NoError(t, err)
	require.NotNil(t, reloaded.CurrentVersion)
	assert.Equal(t, out.Version.ID, reloaded.CurrentVersion.ID)
}

func TestIntegration_ARejectedCompileRecordsTheAttemptAndCreatesNoVersion(t *testing.T) {
	f := newFixture(t)
	ctx := f.owner()
	st, err := f.strategies.Create(ctx, CreateStrategyRequest{
		AccountID: f.accountID, Name: "rejected " + f.accountID[:8], Description: "do something clever",
	})
	require.NoError(t, err)
	sid, err := strategy.ParseStrategyID(st.ID)
	require.NoError(t, err)

	svc := f.withCompiler(&fakeCompiler{result: strategy.Result{
		Outcome: strategy.OutcomeRejected,
		Codes:   []string{"EFFECT_FORBIDDEN"},
		Attempts: []strategy.Attempt{{
			ID: strategy.NewAttemptID(), StrategyID: sid, RequestID: "r", AttemptNo: 1,
			SourceKind: "NATURAL_LANGUAGE", InputHash: bytes32("in"), StageReached: "EFFECT",
			Outcome: strategy.OutcomeRejected, FailureCodes: []string{"EFFECT_FORBIDDEN"},
			CreatedAt: f.clk.Now().UTC(),
		}},
	}})

	out, err := svc.Compile(ctx, st.ID, "req-"+newUUID(), "corr")
	require.NoError(t, err)
	assert.Equal(t, strategy.OutcomeRejected, out.Outcome)
	assert.Equal(t, []string{"EFFECT_FORBIDDEN"}, out.FailureCodes)
	assert.Nil(t, out.Version)
	assert.Zero(t, countRows(t, `SELECT count(*) FROM strategy_versions WHERE strategy_id = $1`, mustUUID(t, st.ID)))
	assert.Equal(t, 1, countRows(t,
		`SELECT count(*) FROM compile_attempts WHERE strategy_id = $1 AND outcome = 'REJECTED'`, mustUUID(t, st.ID)))
}

func TestIntegration_AStrangerCannotReadOrCompileSomebodyElsesStrategy(t *testing.T) {
	f := newFixture(t)
	st, err := f.strategies.Create(f.owner(), CreateStrategyRequest{
		AccountID: f.accountID, Name: "private " + f.accountID[:8], Description: "mine alone",
	})
	require.NoError(t, err)

	// Not yours is not found: the id itself is a fact about somebody else's
	// account, and Compile answers the same way because it reads first.
	_, err = f.strategies.Get(f.stranger(), st.ID)
	require.Error(t, err)
	e, _ := errs.As(err)
	assert.Equal(t, errs.CodeNotFound, e.Code)

	_, err = f.strategies.Compile(f.stranger(), st.ID, "req-"+newUUID(), "")
	require.Error(t, err)
	e, _ = errs.As(err)
	assert.Equal(t, errs.CodeNotFound, e.Code)
}

// ---------------------------------------------------------------- helpers --

type fakeCompiler struct {
	result strategy.Result
	err    error
	calls  int
}

func (f *fakeCompiler) CompileNL(context.Context, strategy.NLRequest) (strategy.Result, error) {
	f.calls++
	return f.result, f.err
}

// fakeRefs supplies an empty registry. It exists so the compile path can be
// exercised without a model and without a network; the fake compiler never
// looks at it, which is the point — nothing in this suite validates IR against
// a registry it invented.
type fakeRefs struct{}

func (fakeRefs) Refs(context.Context, db.Querier, time.Time) (strategy.ValidationRefs, error) {
	return strategy.ValidationRefs{}, nil
}

func (f *fixture) withCompiler(c CompilerBackend) *StrategyService {
	f.t.Helper()
	svc, err := NewStrategyService(StrategyDeps{
		DB: testDB, Clock: f.clk, Compiler: c, Refs: fakeRefs{}, CompilerVersion: "itest",
	})
	require.NoError(f.t, err)
	require.True(f.t, svc.CompilerConfigured())
	return svc
}

func successResult(t *testing.T, sid strategy.StrategyID) strategy.Result {
	t.Helper()
	vid := strategy.NewVersionID()
	aid := strategy.NewAttemptID()
	// A real document and its real hash. The fixture used to return a nil IR
	// and the word "irhash", which a compiler cannot produce and which
	// persist now refuses: the hash is what every later comparison is made
	// against, so one that does not describe its document makes all of them
	// agree about nothing (F-189).
	doc := &ir.IR{SchemaVersion: ir.SchemaVersion}
	hash, err := ir.SemanticHash(doc)
	require.NoError(t, err)
	return strategy.Result{
		Outcome: strategy.OutcomeSuccess,
		Version: &strategy.Version{
			ID: vid, StrategyID: sid, Version: 1, SchemaVersion: ir.SchemaVersion,
			IR: doc, IRHash: hash, EffectSet: []string{"READ_MARKET_DATA"},
			Status: strategy.StatusCompiled, SourceKind: "NATURAL_LANGUAGE", SourceHash: bytes32("src"),
			CompilerVersion: "fake/1", RiskPolicy: "risk/v1", RiskPolicyHash: bytes32("risk"),
			HumanReadable: "buy the dip, in words a person can check", BuiltAt: time.Now().UTC(),
		},
		Attempts: []strategy.Attempt{{
			ID: aid, StrategyID: sid, RequestID: "r", AttemptNo: 1, SourceKind: "NATURAL_LANGUAGE",
			InputHash: bytes32("in"), StageReached: "ACCEPTED", Outcome: strategy.OutcomeSuccess,
			VersionID: &vid, CreatedAt: time.Now().UTC(),
		}},
	}
}

func parseQty(t *testing.T, s string) money.Quantity {
	t.Helper()
	q, err := money.ParseQuantity(s)
	require.NoError(t, err)
	return q
}

func countRows(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

func countTransitions(t *testing.T, agentID agent.AgentID) int {
	t.Helper()
	return countRows(t, `SELECT count(*) FROM agent_lifecycle_transitions WHERE agent_id = $1`, agentID)
}

func transitionStates(t *testing.T, agentID agent.AgentID) []string {
	t.Helper()
	rows, err := testDB.Query(context.Background(),
		`SELECT to_state FROM agent_lifecycle_transitions WHERE agent_id = $1 ORDER BY occurred_at, id`, agentID)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	return out
}

func reloadState(t *testing.T, agentID agent.AgentID) agent.State {
	t.Helper()
	var s string
	require.NoError(t, testDB.QueryRow(context.Background(), `SELECT state FROM agents WHERE id = $1`, agentID).Scan(&s))
	return agent.State(s)
}

func mustUUID(t *testing.T, s string) string {
	t.Helper()
	require.NotEmpty(t, s)
	return s
}
