//go:build integration

package agents

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/strategy"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// F-187 (was TestAuditAgnot_AnAgentBindsToAnotherAccountsStrategyVersion on
// audit/agents-notifications @ 8a69aaa, which asserted the create succeeded).
//
// Create checked that the two ids were non-empty and nothing else. Goal SS18
// requires the compiled strategy to be the thing the owner READ and APPROVED
// before anything is activated, and ADR-0029 states an agent "is created only
// from" a compiled version. A stranger could create an agent on their OWN
// account bound to somebody else's private strategy version, and the grant --
// the document an audit of "what did this person agree to" reads -- recorded
// authority over IR the grantor never saw and may not read.
func TestAuditAgnot_AnAgentCannotBindToAnotherAccountsStrategyVersion(t *testing.T) {
	f := newFixture(t)

	// The owner's strategy is not readable by the stranger...
	_, err := f.strategies.Get(f.stranger(), f.strategyID)
	require.Error(t, err, "the stranger must not be able to read the owner's strategy")

	// ...and they cannot bind an agent on their own account to it either.
	_, err = f.svc.Create(f.stranger(), CreateRequest{
		AccountID:         f.otherAcct,
		StrategyID:        f.strategyID, // the OWNER's strategy
		StrategyVersionID: f.versionID,  // the OWNER's compiled version
		Name:              "borrowed strategy",
		Level:             agentauthority.LevelRecommendation,
		Limits:            fixtureLimits(t, f.assetID),
	})
	require.Error(t, err, "a stranger created an agent from somebody else's strategy version")
	e, ok := errs.As(err)
	require.True(t, ok)
	assert.Equal(t, errs.CodeNotFound, e.Code,
		"NOT_FOUND and not FORBIDDEN: the refusal must not confirm that the version exists")

	// Nothing was written: no agent, and no grant recording an authority.
	assert.Zero(t, countRows(t, `SELECT count(*) FROM agents WHERE account_id = $1`, f.otherAcct))
	assert.Zero(t, countRows(t, `SELECT count(*) FROM agent_grants WHERE strategy_version_id = $1 AND account_id = $2`,
		f.versionID, f.otherAcct))

	var ownerAccount string
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT owner_account_id::text FROM strategies WHERE id = $1`, f.strategyID).Scan(&ownerAccount))
	assert.Equal(t, f.accountID, ownerAccount, "the strategy still belongs to the account that owns it")
}

// F-187, second face. The version must belong to the strategy it names.
// `agents` carried two independent foreign keys and no consistency check
// between them, so strategy_id and strategy_version_id could describe two
// different strategies and every read that joined through either key answered a
// different story. 00802 makes the pair one composite foreign key; the service
// refuses it before the insert, with the same answer as a version that does not
// exist. (Was TestAuditAgnot_TheStrategyVersionNeedNotBelongToTheStrategy.)
func TestAuditAgnot_TheStrategyVersionMustBelongToTheStrategy(t *testing.T) {
	f := newFixture(t)
	ctx := f.owner()

	// A second strategy on the same account, with no versions at all.
	other := newUUID()
	_, err := testDB.Exec(ctx, `INSERT INTO strategies (id, owner_account_id, owner_user_id, name, description,
	        source_kind, status, created_by_actor_type, created_by_actor_id)
	      VALUES ($1, $2, $3::uuid, $4, 'unrelated', 'NATURAL_LANGUAGE', 'ACTIVE', 'USER', $3)`,
		other, f.accountID, f.userID, "unrelated-"+other[:8])
	require.NoError(t, err)

	_, err = f.svc.Create(ctx, CreateRequest{
		AccountID:         f.accountID,
		StrategyID:        other,       // a strategy with no compiled version
		StrategyVersionID: f.versionID, // a version belonging to a DIFFERENT strategy
		Name:              "mismatched",
		Level:             agentauthority.LevelRecommendation,
		Limits:            fixtureLimits(t, f.assetID),
	})
	require.Error(t, err, "an agent was created naming one strategy and another strategy's version")
	e, ok := errs.As(err)
	require.True(t, ok)
	assert.Equal(t, errs.CodeNotFound, e.Code)
	assert.Zero(t, countRows(t, `SELECT count(*) FROM agents WHERE strategy_id = $1`, other))

	// And the schema refuses the pair too, so a writer that is not this service
	// cannot record it either.
	var versionStrategy string
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT strategy_id::text FROM strategy_versions WHERE id = $1`, f.versionID).Scan(&versionStrategy))
	require.NotEqual(t, other, versionStrategy)
	_, derr := testDB.Exec(ctx, `INSERT INTO agents (id, account_id, strategy_id, strategy_version_id, name, stage,
	        state, version, created_by_actor_type, created_by_actor_id)
	      VALUES ($1::uuid, $2, $3, $4, 'forced mismatch', 'DRAFT', 'DRAFT', 1, 'USER', $5)`,
		newUUID(), f.accountID, other, f.versionID, f.userID)
	require.Error(t, derr, "the composite foreign key admitted a version of another strategy")
	assert.Equal(t, "23503", db.SQLState(derr), "got %v", derr)
}

// Control: a stranger cannot act on the owner's agent. (Expected to pass.)
func TestAuditAgnot_AStrangerCannotActOnAnotherOwnersAgent(t *testing.T) {
	f := newFixture(t)
	v, err := f.create(f.owner(), agentauthority.LevelRecommendation, "mine")
	require.NoError(t, err)

	for _, a := range Actions() {
		_, err := f.svc.Act(f.stranger(), ActRequest{AgentID: v.Agent.ID, Action: a, Reason: "not yours at all"})
		require.Error(t, err, "%s by a stranger must be refused", a)
	}
	_, err = f.svc.Get(f.stranger(), v.Agent.ID)
	require.Error(t, err)
}

// Control: a customer holding agent:pause cannot reach the operator pause.
func TestAuditAgnot_ACustomerWithAgentPauseCannotOperatorPause(t *testing.T) {
	f := newFixture(t)
	v, err := f.create(f.owner(), agentauthority.LevelRecommendation, "mine too")
	require.NoError(t, err)

	require.True(t, security.PermissionsForRole(security.RoleCustomer) != nil)
	held := false
	for _, p := range security.PermissionsForRole(security.RoleCustomer) {
		if p == security.PermAgentPause {
			held = true
		}
	}
	require.True(t, held, "the CUSTOMER role holds agent:pause")

	_, err = f.svc.AdminPause(f.owner(), v.Agent.ID, "an operator reason", "")
	require.Error(t, err, "a USER principal must not reach AdminPause")
}

// --- the compiler seam, with a backend that lies -------------------------

type lyingBackend struct{ v *strategy.Version }

func (b lyingBackend) CompileNL(_ context.Context, req strategy.NLRequest) (strategy.Result, error) {
	att := strategy.Attempt{
		ID: strategy.NewAttemptID(), StrategyID: req.StrategyID, RequestID: "backend-chose-this",
		AttemptNo: 1, SourceKind: "NATURAL_LANGUAGE", InputHash: bytes32("in"),
		StageReached: "ACCEPTED", Outcome: strategy.OutcomeSuccess, CreatedAt: time.Now().UTC(),
	}
	id, _ := strategy.ParseVersionID(b.v.ID.String())
	att.VersionID = &id
	return strategy.Result{Version: b.v, Attempts: []strategy.Attempt{att}, Outcome: strategy.OutcomeSuccess}, nil
}

type emptyRefs struct{}

func (emptyRefs) Refs(context.Context, db.Querier, time.Time) (strategy.ValidationRefs, error) {
	return strategy.ValidationRefs{}, nil
}

// F-189 (was TestAuditAgnot_ACompilerBackendWritesAVersionUnderSomebodyElses\
// Strategy, which asserted the row was written).
//
// StrategyService.persist wrote whatever the CompilerBackend returned. It never
// checked that the Version's StrategyID was the strategy that was compiled,
// that the version number was the one it had just reserved, that the status was
// COMPILED, or that IRHash matched the IR. A backend -- the seam ADR-0029
// declares, satisfied by *strategy.Compiler today and by whatever a later
// deployment wires -- could therefore write a strategy_versions row under a
// DIFFERENT account's strategy, with a hash that describes no document, and
// point that account's strategy at it.
func TestAuditAgnot_ACompilerBackendCannotWriteAVersionUnderSomebodyElsesStrategy(t *testing.T) {
	f := newFixture(t)
	ctx := f.stranger()

	// The stranger's own strategy is what they compile.
	mine := newUUID()
	_, err := testDB.Exec(ctx, `INSERT INTO strategies (id, owner_account_id, owner_user_id, name, description,
	        source_kind, status, created_by_actor_type, created_by_actor_id)
	      VALUES ($1, $2, $3::uuid, $4, 'mine', 'NATURAL_LANGUAGE', 'ACTIVE', 'USER', $3)`,
		mine, f.otherAcct, f.otherUser, "stranger-"+mine[:8])
	require.NoError(t, err)

	victim, verr := strategy.ParseStrategyID(f.strategyID) // the OWNER's strategy
	require.NoError(t, verr)

	forged := &strategy.Version{
		ID: strategy.NewVersionID(), StrategyID: victim, Version: 77, SchemaVersion: 1,
		IR: nil, IRHash: bytes32("not-the-hash-of-anything"),
		EffectSet: []string{"CREATE_TRADE_INTENT"}, Status: strategy.StatusCompiled,
		SourceKind: "NATURAL_LANGUAGE", SourceHash: bytes32("src"), CompilerVersion: "evil/1",
		RiskPolicy: "risk/v1", RiskPolicyHash: bytes32("risk"), HumanReadable: "trust me",
		BuiltAt: time.Now().UTC(),
	}
	svc, err := NewStrategyService(StrategyDeps{
		DB: testDB, Clock: f.clk, Compiler: lyingBackend{v: forged}, Refs: emptyRefs{}, CompilerVersion: "itest",
	})
	require.NoError(t, err)

	_, err = svc.Compile(ctx, mine, "", "")
	require.Error(t, err, "the forged version was accepted from the backend")
	e, ok := errs.As(err)
	require.True(t, ok)
	assert.Equal(t, errs.CodeInternal, e.Code,
		"the caller asked for a compile and did nothing wrong; what failed is the thing this deployment wired")

	// Nothing was written, and the victim's strategy still points where it did.
	assert.Zero(t, countRows(t, `SELECT count(*) FROM strategy_versions WHERE id = $1`, forged.ID.String()),
		"a version was written under strategy %s from a compile of strategy %s", f.strategyID, mine)
	var current *string
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT current_version_id::text FROM strategies WHERE id = $1`, f.strategyID).Scan(&current))
	assert.Nil(t, current, "the victim's strategy was pointed at the forged version")

	// The other three faces of the same check, one at a time, so a fix that
	// closed only the strategy id does not pass.
	base := func() *strategy.Version {
		v := *forged
		v.ID = strategy.NewVersionID()
		v.StrategyID = mustStrategyID(t, mine)
		v.IR = &ir.IR{SchemaVersion: ir.SchemaVersion}
		h, herr := ir.SemanticHash(v.IR)
		require.NoError(t, herr)
		v.IRHash = h
		v.Version = 1
		return &v
	}
	for _, tc := range []struct {
		name  string
		spoil func(v *strategy.Version)
	}{
		{"a version number nobody reserved", func(v *strategy.Version) { v.Version = 77 }},
		{"a status only a person may set", func(v *strategy.Version) { v.Status = strategy.StatusAccepted }},
		{"a hash that describes no document", func(v *strategy.Version) { v.IRHash = bytes32("not the hash") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := base()
			tc.spoil(v)
			svc, serr := NewStrategyService(StrategyDeps{
				DB: testDB, Clock: f.clk, Compiler: lyingBackend{v: v}, Refs: emptyRefs{}, CompilerVersion: "itest",
			})
			require.NoError(t, serr)
			_, cerr := svc.Compile(ctx, mine, "", "")
			require.Error(t, cerr)
			assert.Zero(t, countRows(t, `SELECT count(*) FROM strategy_versions WHERE id = $1`, v.ID.String()))
		})
	}

	// And the control: a backend that answers with the version it was asked for
	// is written.
	good := base()
	svc, err = NewStrategyService(StrategyDeps{
		DB: testDB, Clock: f.clk, Compiler: lyingBackend{v: good}, Refs: emptyRefs{}, CompilerVersion: "itest",
	})
	require.NoError(t, err)
	out, err := svc.Compile(ctx, mine, "", "")
	require.NoError(t, err)
	require.True(t, out.Succeeded())
	assert.Equal(t, good.ID.String(), out.Version.ID)
}

func mustStrategyID(t *testing.T, s string) strategy.StrategyID {
	t.Helper()
	id, err := strategy.ParseStrategyID(s)
	require.NoError(t, err)
	return id
}

// F-187, third face. Nothing required the strategy version an agent is created
// from to have been ACCEPTED by anybody. Goal SS18 requires the user to read
// and approve the compiled strategy BEFORE anything is activated, and
// strategy_versions.accepted_by_user_id / accepted_at is the only record of
// that act in the schema; Create read neither column. (Was
// TestAuditAgnot_AnAgentIsCreatedFromANeverAcceptedVersion.)
func TestAuditAgnot_AnAgentIsRefusedFromANeverAcceptedVersion(t *testing.T) {
	f := newFixture(t)
	ctx := f.owner()

	// A version of the owner's own strategy that nobody ever accepted.
	unaccepted := newUUID()
	_, err := testDB.Exec(ctx, `INSERT INTO strategy_versions (id, strategy_id, version, schema_version, ir, ir_hash,
	        effect_set, status, source_kind, source_hash, compiler_version, risk_policy_version, risk_policy_hash,
	        model_budget, data_budget, envelope_requirements, human_readable, built_at)
	      VALUES ($1, $2, 2, 1, '{"schema_version":1}'::jsonb, $3, ARRAY['CREATE_TRADE_INTENT']::text[],
	              'COMPILED', 'NATURAL_LANGUAGE', $4, 'c/1', 'risk/v1', $5,
	              '{}'::jsonb, '{}'::jsonb, '{}'::jsonb, 'never reviewed', now())`,
		unaccepted, f.strategyID, bytes32("ir2"), bytes32("src2"), bytes32("risk2"))
	require.NoError(t, err)

	_, err = f.svc.Create(ctx, CreateRequest{
		AccountID: f.accountID, StrategyID: f.strategyID, StrategyVersionID: unaccepted,
		Name: "from an unapproved version", Level: agentauthority.LevelUserApprovedRule,
		Limits: fixtureLimits(t, f.assetID),
	})
	require.Error(t, err, "an agent was granted authority over a version nobody ever accepted")
	e, ok := errs.As(err)
	require.True(t, ok)
	assert.Equal(t, errs.CodeInvalidStateTransition, e.Code,
		"the owner may see this one: the version is theirs, and what is missing is the approval step")
	assert.Zero(t, countRows(t, `SELECT count(*) FROM agent_grants WHERE strategy_version_id = $1`, unaccepted))

	var acceptedBy *string
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT accepted_by_user_id::text FROM strategy_versions WHERE id = $1`, unaccepted).Scan(&acceptedBy))
	require.Nil(t, acceptedBy, "the fixture's version is the unaccepted one")

	// Accepted, and the same request is allowed: the refusal is about the
	// approval and nothing else.
	_, err = testDB.Exec(ctx, `UPDATE strategy_versions SET status = 'ACCEPTED', accepted_by_user_id = $2::uuid,
	        accepted_at = now() WHERE id = $1`, unaccepted, f.userID)
	require.NoError(t, err)
	v, err := f.svc.Create(ctx, CreateRequest{
		AccountID: f.accountID, StrategyID: f.strategyID, StrategyVersionID: unaccepted,
		Name: "from an approved version", Level: agentauthority.LevelUserApprovedRule,
		Limits: fixtureLimits(t, f.assetID),
	})
	require.NoError(t, err)
	assert.Equal(t, unaccepted, v.Grant.StrategyVersionID)
}
