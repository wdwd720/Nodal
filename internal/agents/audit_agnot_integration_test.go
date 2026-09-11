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
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/strategy"
)

// F-agnot: Create never checks that the strategy version it binds an agent to
// belongs to the account making the grant.
//
// Goal SS18 requires the compiled strategy to be the thing the owner READ and
// APPROVED before anything is activated, and ADR-0029 states an agent "is
// created only from" a compiled version. Here a stranger creates an agent on
// their OWN account bound to somebody else's private strategy version: the
// grant records authority over IR the grantor never saw and may not read.
func TestAuditAgnot_AnAgentBindsToAnotherAccountsStrategyVersion(t *testing.T) {
	f := newFixture(t)

	// The owner's strategy is not readable by the stranger...
	_, err := f.strategies.Get(f.stranger(), f.strategyID)
	require.Error(t, err, "the stranger must not be able to read the owner's strategy")

	// ...and yet the stranger may bind an agent on their own account to it.
	v, err := f.svc.Create(f.stranger(), CreateRequest{
		AccountID:         f.otherAcct,
		StrategyID:        f.strategyID, // the OWNER's strategy
		StrategyVersionID: f.versionID,  // the OWNER's compiled version
		Name:              "borrowed strategy",
		Level:             agentauthority.LevelRecommendation,
		Limits:            fixtureLimits(t, f.assetID),
	})
	require.NoError(t, err, "expected a refusal; the create succeeded")
	assert.Equal(t, f.otherAcct, v.Agent.AccountID)
	assert.Equal(t, f.strategyID, v.Agent.StrategyID)
	assert.Equal(t, f.versionID, v.Grant.StrategyVersionID,
		"the grant records authority over a strategy version its grantor cannot read")

	var ownerAccount string
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT owner_account_id::text FROM strategies WHERE id = $1`, f.strategyID).Scan(&ownerAccount))
	assert.Equal(t, f.accountID, ownerAccount)
	t.Fatalf("an agent on account %s was created from account %s's strategy version %s",
		f.otherAcct, ownerAccount, f.versionID)
}

// F-agnot: nor that the version belongs to the strategy it names. agents
// carries two independent foreign keys and no consistency check between them,
// so strategy_id and strategy_version_id can describe two different strategies.
func TestAuditAgnot_TheStrategyVersionNeedNotBelongToTheStrategy(t *testing.T) {
	f := newFixture(t)
	ctx := f.owner()

	// A second strategy on the same account, with no versions at all.
	other := newUUID()
	_, err := testDB.Exec(ctx, `INSERT INTO strategies (id, owner_account_id, owner_user_id, name, description,
	        source_kind, status, created_by_actor_type, created_by_actor_id)
	      VALUES ($1, $2, $3::uuid, $4, 'unrelated', 'NATURAL_LANGUAGE', 'ACTIVE', 'USER', $3)`,
		other, f.accountID, f.userID, "unrelated-"+other[:8])
	require.NoError(t, err)

	v, err := f.svc.Create(ctx, CreateRequest{
		AccountID:         f.accountID,
		StrategyID:        other,       // a strategy with no compiled version
		StrategyVersionID: f.versionID, // a version belonging to a DIFFERENT strategy
		Name:              "mismatched",
		Level:             agentauthority.LevelRecommendation,
		Limits:            fixtureLimits(t, f.assetID),
	})
	require.NoError(t, err, "expected a refusal")
	var versionStrategy string
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT strategy_id::text FROM strategy_versions WHERE id = $1`, f.versionID).Scan(&versionStrategy))
	assert.NotEqual(t, v.Agent.StrategyID, versionStrategy)
	t.Fatalf("agent %s names strategy %s but version %s belongs to strategy %s",
		v.Agent.ID, v.Agent.StrategyID, f.versionID, versionStrategy)
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

// F-agnot: StrategyService.persist writes whatever the CompilerBackend returns.
// It never checks that the Version's StrategyID is the strategy that was
// compiled, that the version number is the one it just reserved, that the
// status is COMPILED, or that IRHash matches the IR. A backend (the seam
// ADR-0029 declares, satisfied by *strategy.Compiler today and by whatever a
// later deployment wires) can therefore write a strategy_versions row under a
// DIFFERENT account's strategy, with a hash that does not describe the
// document.
func TestAuditAgnot_ACompilerBackendWritesAVersionUnderSomebodyElsesStrategy(t *testing.T) {
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

	out, err := svc.Compile(ctx, mine, "", "")
	require.NoError(t, err)
	require.True(t, out.Succeeded())

	var ownerAccount, status string
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT s.owner_account_id::text, v.status FROM strategy_versions v
		   JOIN strategies s ON s.id = v.strategy_id WHERE v.id = $1`, forged.ID).Scan(&ownerAccount, &status))
	assert.Equal(t, f.otherAcct, ownerAccount,
		"a compile of the stranger's strategy wrote a version under account %s's strategy, status %s",
		ownerAccount, status)
	t.Fatalf("version %s written under strategy %s (account %s) with status %s and an unverified ir_hash, "+
		"from a compile of strategy %s", forged.ID, f.strategyID, ownerAccount, status, mine)
}

// F-agnot (same defect, third face): nothing requires the strategy version an
// agent is created from to have been ACCEPTED by anybody. Goal SS18 requires
// the user to read and approve the compiled strategy BEFORE anything is
// activated, and strategy_versions.accepted_by_user_id / accepted_at is the
// only record of that act in the schema. Create never reads either column.
func TestAuditAgnot_AnAgentIsCreatedFromANeverAcceptedVersion(t *testing.T) {
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

	v, err := f.svc.Create(ctx, CreateRequest{
		AccountID: f.accountID, StrategyID: f.strategyID, StrategyVersionID: unaccepted,
		Name: "from an unapproved version", Level: agentauthority.LevelUserApprovedRule,
		Limits: fixtureLimits(t, f.assetID),
	})
	require.NoError(t, err, "expected a refusal")

	var acceptedBy *string
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT accepted_by_user_id::text FROM strategy_versions WHERE id = $1`, unaccepted).Scan(&acceptedBy))
	assert.NotNil(t, acceptedBy,
		"agent %s at authority level %d was granted over strategy version %s, which nobody ever accepted",
		v.Agent.ID, int(v.Grant.Level), unaccepted)
}
