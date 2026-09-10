//go:build integration

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// These tests need an isolated database (go run ./scripts/testdb -name agentrt)
// because cp_app cannot delete rows: agents, runs, predictions, invocations and
// lifecycle transitions are all append-only or update-only by design. Every
// test therefore builds its own fresh fixture rather than resetting shared
// state, and the whole suite is safe to run twice against the same database.
var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
)

func TestMain(m *testing.M) { os.Exit(testMain(m)) }

func testMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "agent integration: migrate up:", err)
		return 1
	}
	var err error
	testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "agent-itest", MaxConns: 8})
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent integration: open pool:", err)
		return 1
	}
	code := m.Run()
	testDB.Close()
	return code
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set; skipping integration test")
	}
}

// ---------------------------------------------------------------- fixture --

type fixture struct {
	t         *testing.T
	clk       *clock.Fake
	lifecycle *Lifecycle
	store     Store
	audit     *recordingAudit
	approvals *fakeApprovals

	userID       string
	operatorID   string
	approverID   string
	accountID    string
	strategyID   string
	versionID    string
	envelopeID   string
	instrumentID string
	assetID      string
	quoteAssetID string
	toolID       ToolID
	modelToolID  ToolID
	toolCode     string
	modelCode    string
}

// recordingAudit captures the audit events a lifecycle change produced, so a
// test can assert that a change without an audit event is impossible.
type recordingAudit struct {
	events []AuditEvent
	fail   error
}

func (r *recordingAudit) Append(_ context.Context, _ pgx.Tx, e AuditEvent) error {
	if r.fail != nil {
		return r.fail
	}
	r.events = append(r.events, e)
	return nil
}

// fakeApprovals stands in for the admin_actions reader. The real adapter lives
// in cmd/agent-worker; internal/agent must not import internal/admin.
type fakeApprovals struct {
	byID map[string]Approval
	err  error
}

func (f *fakeApprovals) VerifyApproved(_ context.Context, _ db.Querier, approvalID, kind, targetID string) (Approval, error) {
	if f.err != nil {
		return Approval{}, f.err
	}
	a, ok := f.byID[approvalID]
	if !ok {
		return Approval{}, errs.New(errs.CodeForbidden, "no such approval")
	}
	if a.Kind != kind || a.TargetID != targetID {
		return Approval{}, errs.New(errs.CodeForbidden, "approval does not match")
	}
	return a, nil
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireEnv(t)
	ctx := context.Background()
	f := &fixture{
		t:         t,
		clk:       clock.NewFake(time.Now().UTC().Truncate(time.Microsecond)),
		store:     NewStore(),
		audit:     &recordingAudit{},
		approvals: &fakeApprovals{byID: map[string]Approval{}},
	}
	lc, err := NewLifecycle(LifecycleDeps{
		Clock: f.clk, Audit: f.audit, Approvals: f.approvals, BuildVersion: "itest",
	})
	require.NoError(t, err)
	f.lifecycle = lc

	f.userID = newUUID()
	f.operatorID = newUUID()
	f.approverID = newUUID()
	f.accountID = newUUID()
	f.strategyID = newUUID()
	f.versionID = newUUID()
	f.instrumentID = newUUID()
	f.assetID = newUUID()
	f.quoteAssetID = newUUID()
	f.toolID = NewToolID()
	f.modelToolID = NewToolID()

	exposureID := newUUID()
	// The whole uuid is the uniqueness suffix: a UUIDv7 prefix is a timestamp
	// and two fixtures built in the same millisecond would collide.
	suffix := f.accountID
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := testDB.Exec(ctx, sql, args...)
		require.NoError(t, err, sql)
	}
	for _, u := range []struct{ id, sub string }{
		{f.userID, "owner-" + suffix},
		{f.operatorID, "operator-" + suffix},
		{f.approverID, "approver-" + suffix},
	} {
		exec(`INSERT INTO users (id, idp_issuer, idp_subject, status) VALUES ($1, 'itest', $2, 'ACTIVE')`, u.id, u.sub)
	}
	exec(`INSERT INTO accounts (id, owner_user_id, kind, status) VALUES ($1, $2, 'CUSTOMER', 'ACTIVE')`,
		f.accountID, f.userID)
	exec(`INSERT INTO assets (id, chain, mint_address, kind, symbol, name, decimals, risk_class, status, value_domain)
	      VALUES ($1, 'solana-devnet', $2, 'SPL_TOKEN', 'SOL', 'Solana', 9, 'MAJOR', 'ACTIVE', 'SELF_CUSTODIAL_CRYPTO')`,
		f.assetID, "base-"+suffix)
	exec(`INSERT INTO assets (id, chain, mint_address, kind, symbol, name, decimals, is_stablecoin, peg_currency, risk_class, status, value_domain)
	      VALUES ($1, 'solana-devnet', $2, 'SPL_TOKEN', 'USDC', 'USD Coin', 6, true, 'USD', 'SETTLEMENT', 'ACTIVE', 'SELF_CUSTODIAL_CRYPTO')`,
		f.quoteAssetID, "quote-"+suffix)
	exec(`INSERT INTO economic_exposures (id, kind, description, underlying_asset_id)
	      VALUES ($1, 'ASSET_PRICE', 'SOL price', $2)`, exposureID, f.assetID)
	exec(`INSERT INTO instruments (id, type, canonical_name, exposure_id, base_asset_id, quote_asset_id,
	          settlement_asset_id, risk_class, status, active_from)
	      VALUES ($1, 'SPOT_PAIR', $2, $3, $4, $5, $5, 'MAJOR', 'ACTIVE', now())`,
		f.instrumentID, "SOL-USDC-"+suffix, exposureID, f.assetID, f.quoteAssetID)
	exec(`INSERT INTO strategies (id, owner_account_id, owner_user_id, name, source_kind, status,
	          created_by_actor_type, created_by_actor_id)
	      VALUES ($1, $2, $3, $4, 'NATURAL_LANGUAGE', 'ACTIVE', 'USER', $5)`,
		f.strategyID, f.accountID, f.userID, "strategy-"+suffix, f.userID)

	doc := fixtureIR(f.instrumentID)
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	exec(`INSERT INTO strategy_versions (id, strategy_id, version, schema_version, ir, ir_hash, effect_set,
	          status, source_kind, source_hash, compiler_version, risk_policy_version, risk_policy_hash,
	          model_budget, data_budget, envelope_requirements, human_readable, built_at,
	          accepted_by_user_id, accepted_at)
	      VALUES ($1, $2, 1, 1, $3, $4, $5, 'ACCEPTED', 'NATURAL_LANGUAGE', $6, 'c/1', 'risk/v1', $7,
	              '{}'::jsonb, '{}'::jsonb, '{}'::jsonb, 'rendered', now(), $8, now())`,
		f.versionID, f.strategyID, raw, bytes32("ir"), ir.EffectStrings(doc.Effects), bytes32("src"), bytes32("risk"), f.userID)

	// Tools: one ACTIVE market-data tool and one ACTIVE model tool.
	exec(`INSERT INTO tools (id, code, version, effect, provider, output_schema, egress_hosts,
	          cost_per_call_usd_minor, max_calls_per_minute, max_calls_per_run, timeout_ms,
	          pipeline_latency_ms, status, created_by_actor_type, created_by_actor_id)
	      VALUES ($1, $2, 1, 'READ_MARKET_DATA', 'test-oracle', '{"type":"object"}'::jsonb, ARRAY['oracle.test'],
	              5, 60, 3, 2000, 0, 'ACTIVE', 'OPERATOR', 'op')`,
		f.toolID, "price-oracle-"+suffix)
	exec(`INSERT INTO tools (id, code, version, effect, provider, output_schema, egress_hosts,
	          cost_per_call_usd_minor, max_calls_per_minute, max_calls_per_run, timeout_ms,
	          pipeline_latency_ms, status, created_by_actor_type, created_by_actor_id)
	      VALUES ($1, $2, 1, 'CALL_MODEL', 'test-model', '{"type":"object"}'::jsonb, ARRAY['model.test'],
	              20, 60, 2, 5000, 0, 'ACTIVE', 'OPERATOR', 'op')`,
		f.modelToolID, "model-"+suffix)
	f.toolCode = "price-oracle-" + suffix
	f.modelCode = "model-" + suffix

	return f
}

func fixtureIR(instrumentID string) *ir.IR {
	return &ir.IR{
		SchemaVersion: ir.SchemaVersion,
		StrategyID:    "s",
		Version:       1,
		Instruments:   []ir.InstrumentDecl{{Name: "target", InstrumentID: instrumentID}},
		Effects: []ir.Effect{
			ir.EffectReadMarketData, ir.EffectCallModel,
			ir.EffectCommitPrediction, ir.EffectCreateTradeIntent,
		},
		DataBudget: ir.DataBudget{
			MaxToolCallsPerRun: 10, MaxToolCallsPerDay: 50,
			MaxSpendPerDay: money.USDFromMinor(100), MaxLookbackMS: 3_600_000,
		},
		ModelBudget: ir.ModelBudget{
			MaxCallsPerRun: 1, MaxCallsPerDay: 10,
			MaxOutputTokens: 256, MaxSpendPerDay: money.USDFromMinor(200),
		},
		Envelope: ir.EnvelopeRequirements{MaxIntentsPerHour: 5, MaxRunsPerMinute: 6},
	}
}

func (f *fixture) authorityIR() *ir.IR {
	doc := fixtureIR(f.instrumentID)
	doc.Dependencies = []ir.Dependency{{
		Name: "price", Kind: ir.DepPrice, ToolCode: f.toolCode, ToolVersion: 1,
		DependencyVersion: 1, Params: map[string]string{"instrument": f.instrumentID},
		MaxAgeMS: 60_000, Required: true,
	}}
	return doc
}

func (f *fixture) operator(roles ...security.Role) security.Principal {
	if len(roles) == 0 {
		roles = []security.Role{security.RoleOperations}
	}
	return security.Principal{
		SubjectID: f.operatorID, ActorType: security.ActorOperator, Roles: roles,
		AuthTime: f.clk.Now(), AMR: []string{"mfa"},
	}
}

func (f *fixture) owner() security.Principal {
	return security.Principal{
		SubjectID: f.userID, ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer},
		AccountIDs: []string{f.accountID}, AuthTime: f.clk.Now(), AMR: []string{"mfa"},
	}
}

func (f *fixture) system() security.Principal {
	return security.Principal{SubjectID: "system", ActorType: security.ActorSystem}
}

func (f *fixture) agentPrincipal(agentID AgentID) security.Principal {
	return security.AgentPrincipal(agentID.String(), f.accountID)
}

func ctxAs(p security.Principal) context.Context {
	return security.WithPrincipal(context.Background(), p)
}

// inTx runs fn in a transaction against the isolated test database.
func inTx(ctx context.Context, t *testing.T, fn func(ctx context.Context, tx pgx.Tx) error) error {
	t.Helper()
	return testDB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 0}, fn)
}

func newUUID() string { return id.New[struct{}]().String() }

func bytes32(seed string) []byte {
	out := make([]byte, 32)
	copy(out, seed)
	return out
}

// createDraft makes a DRAFT agent owned by the fixture account.
func (f *fixture) createDraft(t *testing.T) Agent {
	t.Helper()
	var a Agent
	require.NoError(t, inTx(ctxAs(f.owner()), t, func(ctx context.Context, tx pgx.Tx) error {
		out, err := f.lifecycle.Create(ctx, tx, CreateAgent{
			AccountID: f.accountID, StrategyID: f.strategyID, StrategyVersionID: f.versionID,
			Name: "itest agent", RiskPolicyVersion: "risk/v1",
		})
		a = out
		return err
	}))
	return a
}

// promote walks an agent up one rung with complete evidence.
func (f *fixture) promote(t *testing.T, a Agent, to Stage, p security.Principal, mutate func(*PromotionEvidence)) (Agent, error) {
	t.Helper()
	ev := f.evidenceFor(to)
	if mutate != nil {
		mutate(&ev)
	}
	var out Agent
	err := inTx(ctxAs(p), t, func(ctx context.Context, tx pgx.Tx) error {
		res, err := f.lifecycle.Promote(ctx, tx, a.ID, to.State(), ev)
		out = res
		return err
	})
	return out, err
}

func (f *fixture) evidenceFor(to Stage) PromotionEvidence {
	ev := PromotionEvidence{
		StrategyVersionID: f.versionID,
		IRHash:            bytes32("ir"),
		DatasetRef:        "dataset://itest",
		DatasetHash:       bytes32("dataset"),
		RiskPolicyVersion: "risk/v1",
		RiskPolicyHash:    bytes32("risk"),
		Reason:            "integration test promotion",
	}
	for _, kind := range RequiredEvidence(to) {
		ev.Evidence = append(ev.Evidence, EvidenceRef{Kind: kind, Ref: "ref://" + kind, Hash: HashOf([]byte(kind))})
	}
	if RequiresApproval(to) {
		ev.EnvelopeID = f.envelopeID
	}
	return ev
}

// ensureEnvelope creates the capital envelope bound to agentID. It cannot be
// created before the agent exists: capital_envelopes.agent_id is a foreign key
// into agents (migration 00501), which is itself the guarantee that an
// envelope always belongs to a real, recorded agent.
func (f *fixture) ensureEnvelope(t *testing.T, agentID AgentID) string {
	t.Helper()
	if f.envelopeID != "" {
		return f.envelopeID
	}
	envelopeID := newUUID()
	_, err := testDB.Exec(context.Background(),
		`INSERT INTO capital_envelopes (id, account_id, agent_id, strategy_version_id, settlement_asset_id,
		          allocation_usd_minor, available_usd_minor, daily_loss_reset_at, max_daily_loss_usd_minor,
		          max_drawdown_usd_minor, max_single_trade_usd_minor, max_position_usd_minor,
		          allowed_instruments, max_model_spend_usd_minor, max_data_spend_usd_minor,
		          max_order_rate_per_hour, policy_version, status, effective_at,
		          created_by_actor_type, created_by_actor_id)
		      VALUES ($1, $2, $3, $4, $5, 100000, 100000, now(), 10000, 10000, 50000, 50000,
		              ARRAY[$6::uuid], 1000, 1000, 10, 'risk/v1', 'ACTIVE', now(), 'OPERATOR', 'op')`,
		envelopeID, f.accountID, agentID, f.versionID, f.quoteAssetID, f.instrumentID)
	require.NoError(t, err)
	f.envelopeID = envelopeID
	return envelopeID
}

// approveFor writes a real, dual-controlled admin_actions row for agentID and
// registers it with the verifier. It must be a real row: the
// agent_lifecycle_transitions.approval_id foreign key means a promotion can
// never cite an approval that does not exist.
// approveFor creates a dual-controlled approval the way one is really created:
// proposed by one principal, then moved to APPROVED by another, with the
// transition row that move requires.
//
// It used to INSERT the row directly as APPROVED. That is precisely the forgery
// F-42 records and F-121 closed -- two-person control with no proposal and no
// second person -- and migration 00735 now refuses it at INSERT, which is how
// this fixture was found. internal/admin's own comment had already named the
// shape: "a fixture that has to commit the exploit to reach the code is a
// fixture that should not exist."
//
// It is written in SQL rather than through internal/admin because the authority
// boundary forbids this tree from importing it (test/security asserts that), so
// the honest path has to be spelled out here.
func (f *fixture) approveFor(agentID AgentID) string {
	f.t.Helper()
	approvalID := newUUID()
	ctx := context.Background()
	_, err := testDB.Exec(ctx,
		`INSERT INTO admin_actions (id, kind, target_type, target_id, params_hash, reason, requires_dual,
		          status, proposed_by_user_id, proposer_step_up_at, expires_at)
		 VALUES ($1, $2, 'agent', $3, $4, 'integration test promotion approval', true,
		         'PROPOSED', $5, now(), now() + interval '1 hour')`,
		approvalID, ApprovalKindPromote, agentID.String(), bytes32("params"), f.operatorID)
	require.NoError(f.t, err)

	// The decision, by a different principal, with its transition row. The
	// row and the UPDATE are one transaction because AU001 binds them.
	require.NoError(f.t, testDB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 0},
		func(ctx context.Context, tx pgx.Tx) error {
			if _, terr := tx.Exec(ctx,
				`INSERT INTO admin_action_transitions (id, action_id, from_status, to_status, actor_id, note)
				 VALUES ($1, $2, 'PROPOSED', 'APPROVED', $3, 'integration test approval')`,
				newUUID(), approvalID, f.approverID); terr != nil {
				return terr
			}
			_, uerr := tx.Exec(ctx,
				`UPDATE admin_actions SET status = 'APPROVED', approved_by_user_id = $2,
				        approved_at = now(), approver_step_up_at = now()
				  WHERE id = $1`, approvalID, f.approverID)
			return uerr
		}))
	f.approvals.byID[approvalID] = Approval{
		ID: approvalID, Kind: ApprovalKindPromote, TargetID: agentID.String(),
		ProposedBy: f.operatorID, ApprovedBy: f.approverID,
		ExpiresAt: f.clk.Now().Add(time.Hour),
	}
	return approvalID
}

// registerApproval records an approval with the verifier only, without an
// admin_actions row. It is for the negative cases, where the promotion must be
// refused before any row is written.
func (f *fixture) registerApproval(a Approval) string {
	f.approvals.byID[a.ID] = a
	return a.ID
}

// walkTo promotes an agent all the way to the given stage.
func (f *fixture) walkTo(t *testing.T, target Stage) Agent {
	t.Helper()
	a := f.createDraft(t)
	for _, stage := range []Stage{StageCompiled, StageValidated, StageBacktestEligible, StageShadow, StageCanary, StageLimited, StageLive} {
		if Stage(a.Stage).rung() >= target.rung() {
			break
		}
		principal := f.operator()
		if stage == StageLive {
			principal = f.operator(security.RoleOperations, security.RoleRisk)
		}
		if RequiresApproval(stage) {
			f.ensureEnvelope(t, a.ID)
		}
		out, err := f.promote(t, a, stage, principal, func(ev *PromotionEvidence) {
			if RequiresApproval(stage) {
				ev.EnvelopeID = f.ensureEnvelope(t, a.ID)
				ev.ApprovalID = f.approveFor(a.ID)
			}
		})
		require.NoErrorf(t, err, "promoting to %s", stage)
		a = out
	}
	require.Equal(t, target, a.Stage)
	return a
}

// ------------------------------------------------------------- lifecycle --

func TestLifecycleWalksTheLadderAndRecordsEveryStep(t *testing.T) {
	f := newFixture(t)
	a := f.walkTo(t, StageLive)

	require.Equal(t, StageLive, a.Stage)
	require.Equal(t, StateLive, a.State)
	require.Equal(t, ModeLive, a.Mode)
	require.Equal(t, f.envelopeID, a.EnvelopeID)

	transitions, err := f.store.ListTransitions(context.Background(), testDB, a.ID, 100)
	require.NoError(t, err)
	// One creation row plus seven promotions.
	require.Len(t, transitions, 8)
	for _, tr := range transitions[1:] {
		assert.Equal(t, security.ActorOperator, tr.ActorType)
		if RequiresEvidence(tr.ToStage) {
			assert.NotEmpty(t, tr.EvidenceHash, "%s must carry an aggregate evidence hash", tr.ToStage)
			assert.NotEmpty(t, tr.IRHash)
			assert.Equal(t, "risk/v1", tr.RiskPolicyVersion)
		}
		if RequiresApproval(tr.ToStage) {
			assert.NotEmpty(t, tr.ApprovalID, "%s must carry an approval", tr.ToStage)
		}
	}
	assert.GreaterOrEqual(t, len(f.audit.events), 8, "every lifecycle change appends an audit event")
}

func TestPromotionRefusesASkippedRung(t *testing.T) {
	f := newFixture(t)
	a := f.createDraft(t)
	_, err := f.promote(t, a, StageValidated, f.operator(), nil)
	require.Error(t, err)
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))

	live, err := f.promote(t, a, StageLive, f.operator(security.RoleOperations, security.RoleRisk), func(ev *PromotionEvidence) {
		ev.ApprovalID = f.approveFor(a.ID)
	})
	require.Error(t, err, "DRAFT can never jump to LIVE")
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))
	assert.Zero(t, live.ID)
}

func TestPromotionIntoCanaryRequiresADualControlledApproval(t *testing.T) {
	f := newFixture(t)
	a := f.walkTo(t, StageShadow)

	envelopeID := f.ensureEnvelope(t, a.ID)
	withEnvelope := func(ev *PromotionEvidence) { ev.EnvelopeID = envelopeID }

	// No approval at all.
	_, err := f.promote(t, a, StageCanary, f.operator(), withEnvelope)
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	// An approval whose proposer and approver are the same person.
	selfApproved := f.registerApproval(Approval{
		ID: newUUID(), Kind: ApprovalKindPromote, TargetID: a.ID.String(),
		ProposedBy: f.operatorID, ApprovedBy: f.operatorID, ExpiresAt: f.clk.Now().Add(time.Hour),
	})
	_, err = f.promote(t, a, StageCanary, f.operator(), func(ev *PromotionEvidence) {
		withEnvelope(ev)
		ev.ApprovalID = selfApproved
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	assert.Contains(t, err.Error(), "dual-controlled")

	// An approval that the promoting principal themselves approved: one
	// person must not hold both halves of the two-person rule.
	bothHalves := f.registerApproval(Approval{
		ID: newUUID(), Kind: ApprovalKindPromote, TargetID: a.ID.String(),
		ProposedBy: f.approverID, ApprovedBy: f.operatorID, ExpiresAt: f.clk.Now().Add(time.Hour),
	})
	_, err = f.promote(t, a, StageCanary, f.operator(), func(ev *PromotionEvidence) {
		withEnvelope(ev)
		ev.ApprovalID = bothHalves
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	// An approval for a different agent.
	other := f.createDraft(t)
	wrongTarget := f.approveFor(other.ID)
	_, err = f.promote(t, a, StageCanary, f.operator(), func(ev *PromotionEvidence) {
		withEnvelope(ev)
		ev.ApprovalID = wrongTarget
	})
	require.Error(t, err)

	// An expired approval.
	expired := f.registerApproval(Approval{
		ID: newUUID(), Kind: ApprovalKindPromote, TargetID: a.ID.String(),
		ProposedBy: f.operatorID, ApprovedBy: f.approverID, ExpiresAt: f.clk.Now().Add(-time.Minute),
	})
	_, err = f.promote(t, a, StageCanary, f.operator(), func(ev *PromotionEvidence) {
		withEnvelope(ev)
		ev.ApprovalID = expired
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expired")

	// The agent never moved.
	current, err := f.store.Get(context.Background(), testDB, a.ID)
	require.NoError(t, err)
	assert.Equal(t, StageShadow, current.Stage, "a refused promotion must leave the agent where it was")
}

func TestPromotionIntoLiveRequiresTheRiskRole(t *testing.T) {
	f := newFixture(t)
	a := f.walkTo(t, StageLimited)
	_, err := f.promote(t, a, StageLive, f.operator(security.RoleOperations), func(ev *PromotionEvidence) {
		ev.EnvelopeID = f.ensureEnvelope(t, a.ID)
		ev.ApprovalID = f.approveFor(a.ID)
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	assert.Contains(t, err.Error(), "RISK")

	out, err := f.promote(t, a, StageLive, f.operator(security.RoleOperations, security.RoleRisk), func(ev *PromotionEvidence) {
		ev.EnvelopeID = f.ensureEnvelope(t, a.ID)
		ev.ApprovalID = f.approveFor(a.ID)
	})
	require.NoError(t, err)
	assert.Equal(t, StageLive, out.Stage)
}

func TestPromotionRefusesMissingEvidence(t *testing.T) {
	f := newFixture(t)
	a := f.walkTo(t, StageBacktestEligible)
	for _, missing := range RequiredEvidence(StageShadow) {
		_, err := f.promote(t, a, StageShadow, f.operator(), func(ev *PromotionEvidence) {
			kept := ev.Evidence[:0]
			for _, ref := range ev.Evidence {
				if ref.Kind != missing {
					kept = append(kept, ref)
				}
			}
			ev.Evidence = kept
		})
		require.Errorf(t, err, "promotion without %s must be refused", missing)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	}
}

func TestAgentPrincipalCannotChangeItsOwnLifecycle(t *testing.T) {
	f := newFixture(t)
	a := f.walkTo(t, StageShadow)
	agentCtx := ctxAs(f.agentPrincipal(a.ID))

	err := inTx(agentCtx, t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.lifecycle.Promote(ctx, tx, a.ID, StateCanary, f.evidenceFor(StageCanary))
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	err = inTx(agentCtx, t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.lifecycle.Pause(ctx, tx, a.ID, PauseRequest{
			ReasonCode: PauseOwnerRequest, Reason: "let me stop myself", OpenOrdersPolicy: LeaveOpenOrders,
		})
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	err = inTx(agentCtx, t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.lifecycle.Revoke(ctx, tx, a.ID, "self revocation")
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
}

// TestBareStateUpdateIsRefused proves migration 00690: an UPDATE of agents.state
// without a matching lifecycle transition row in the same transaction is
// refused at COMMIT with SQLSTATE AU001. This is what makes the evidence and
// approval CHECKs on agent_lifecycle_transitions unavoidable.
func TestBareStateUpdateIsRefused(t *testing.T) {
	f := newFixture(t)
	a := f.walkTo(t, StageShadow)

	// PAUSED is a side state, so no CHECK on agents refuses it first: the only
	// thing standing between the application role and a silent state change is
	// the transition binding.
	err := inTx(context.Background(), t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agents SET state = 'PAUSED' WHERE id = $1`, a.ID)
		return err
	})
	require.Error(t, err, "a bare state update must be refused")
	assert.True(t, IsTransitionRequired(err), "expected SQLSTATE AU001, got %v", err)

	current, err := f.store.Get(context.Background(), testDB, a.ID)
	require.NoError(t, err)
	assert.Equal(t, StageShadow, current.Stage)
	assert.Equal(t, StateShadow, current.State)
}

// TestStateUpdateWithAMismatchedTransitionIsRefused: the transition row must
// name the same target state, so a row for one state cannot license another.
func TestStateUpdateWithAMismatchedTransitionIsRefused(t *testing.T) {
	f := newFixture(t)
	a := f.walkTo(t, StageShadow)

	err := inTx(context.Background(), t, func(ctx context.Context, tx pgx.Tx) error {
		if err := insertTransition(ctx, tx, Transition{
			ID: NewTransitionID(), AgentID: a.ID,
			FromState: StateShadow, ToState: StatePaused, FromStage: StageShadow, ToStage: StageShadow,
			ActorType: security.ActorOperator, ActorID: f.operatorID, Reason: "mismatched",
			OccurredAt: f.clk.Now(),
		}); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE agents SET state = 'FAILED' WHERE id = $1`, a.ID)
		return err
	})
	require.Error(t, err)
	assert.True(t, IsTransitionRequired(err), "expected AU001, got %v", err)
}

func TestLifecycleTransitionsAreImmutable(t *testing.T) {
	f := newFixture(t)
	a := f.walkTo(t, StageShadow)
	transitions, err := f.store.ListTransitions(context.Background(), testDB, a.ID, 10)
	require.NoError(t, err)
	require.NotEmpty(t, transitions)

	err = inTx(context.Background(), t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agent_lifecycle_transitions SET reason = 'rewritten' WHERE id = $1`, transitions[0].ID)
		return err
	})
	require.Error(t, err, "lifecycle history is never rewritten")

	err = inTx(context.Background(), t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM agent_lifecycle_transitions WHERE id = $1`, transitions[0].ID)
		return err
	})
	require.Error(t, err, "lifecycle history is never deleted")
}

func TestPromotionWithoutAnAuditEventRollsBack(t *testing.T) {
	f := newFixture(t)
	a := f.walkTo(t, StageBacktestEligible)
	f.audit.fail = errs.New(errs.CodeInternal, "audit sink down")
	_, err := f.promote(t, a, StageShadow, f.operator(), nil)
	require.Error(t, err, "a promotion that cannot be audited must not happen")
	f.audit.fail = nil

	current, err := f.store.Get(context.Background(), testDB, a.ID)
	require.NoError(t, err)
	assert.Equal(t, StageBacktestEligible, current.Stage)
	transitions, err := f.store.ListTransitions(context.Background(), testDB, a.ID, 100)
	require.NoError(t, err)
	for _, tr := range transitions {
		assert.NotEqual(t, StageShadow, tr.ToStage, "the transition row must have rolled back with the audit failure")
	}
}

// ------------------------------------------------------------------ pause --

func TestPauseAndResume(t *testing.T) {
	f := newFixture(t)
	a := f.walkTo(t, StageShadow)
	pauses := NewPauseChecker()

	var p Pause
	require.NoError(t, inTx(ctxAs(f.owner()), t, func(ctx context.Context, tx pgx.Tx) error {
		out, err := f.lifecycle.Pause(ctx, tx, a.ID, PauseRequest{
			ReasonCode: PauseOwnerRequest, Reason: "owner requested a stop",
			OpenOrdersPolicy: LeaveOpenOrders,
		})
		p = out
		return err
	}))
	assert.Equal(t, LeaveOpenOrders, p.OpenOrdersPolicy)

	current, err := f.store.Get(context.Background(), testDB, a.ID)
	require.NoError(t, err)
	assert.Equal(t, StatePaused, current.State)
	assert.Equal(t, StageShadow, current.Stage, "a pause never changes the stage")

	open, isPaused, err := pauses.OpenPause(context.Background(), testDB, a.ID)
	require.NoError(t, err)
	require.True(t, isPaused)
	assert.Equal(t, PauseOwnerRequest, open.ReasonCode)

	// A second pause while one is open is a conflict.
	err = inTx(ctxAs(f.owner()), t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.lifecycle.Pause(ctx, tx, a.ID, PauseRequest{
			ReasonCode: PauseOwnerRequest, Reason: "stop again please", OpenOrdersPolicy: LeaveOpenOrders,
		})
		return err
	})
	require.Error(t, err)

	// SYSTEM may never resume.
	err = inTx(ctxAs(f.system()), t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.lifecycle.Resume(ctx, tx, a.ID, "automatic recovery")
		return err
	})
	require.Error(t, err, "SYSTEM must never resume a paused agent")
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	// An agent may never resume itself.
	err = inTx(ctxAs(f.agentPrincipal(a.ID)), t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.lifecycle.Resume(ctx, tx, a.ID, "i am fine now")
		return err
	})
	require.Error(t, err)

	// The owner resumes, and the agent returns to its stage.
	var resumed Agent
	require.NoError(t, inTx(ctxAs(f.owner()), t, func(ctx context.Context, tx pgx.Tx) error {
		out, err := f.lifecycle.Resume(ctx, tx, a.ID, "condition cleared after review")
		resumed = out
		return err
	}))
	assert.Equal(t, StateShadow, resumed.State)
	_, isPaused, err = pauses.OpenPause(context.Background(), testDB, a.ID)
	require.NoError(t, err)
	assert.False(t, isPaused)
}

func TestSystemMayRaiseAnAutomaticPauseButNotAnOwnerRequest(t *testing.T) {
	f := newFixture(t)
	a := f.walkTo(t, StageShadow)

	err := inTx(ctxAs(f.system()), t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.lifecycle.Pause(ctx, tx, a.ID, PauseRequest{
			ReasonCode: PauseOwnerRequest, Reason: "pretending to be the owner",
			OpenOrdersPolicy: LeaveOpenOrders,
		})
		return err
	})
	require.Error(t, err, "SYSTEM cannot raise an OWNER_REQUEST pause")

	require.NoError(t, inTx(ctxAs(f.system()), t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.lifecycle.Pause(ctx, tx, a.ID, PauseRequest{
			ReasonCode: PauseBudgetExhausted, Reason: "daily model budget exhausted",
			OpenOrdersPolicy: LeaveOpenOrders,
		})
		return err
	}))
	current, err := f.store.Get(context.Background(), testDB, a.ID)
	require.NoError(t, err)
	assert.Equal(t, StatePaused, current.State)
}

func TestPromotingAPausedAgentIsRefused(t *testing.T) {
	f := newFixture(t)
	a := f.walkTo(t, StageShadow)
	require.NoError(t, inTx(ctxAs(f.operator()), t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.lifecycle.Pause(ctx, tx, a.ID, PauseRequest{
			ReasonCode: PauseOperator, Reason: "under investigation", OpenOrdersPolicy: CancelCancelableOrders,
			CancelWorkflowID: "wf-1",
		})
		return err
	}))
	_, err := f.promote(t, a, StageCanary, f.operator(), func(ev *PromotionEvidence) {
		ev.EnvelopeID = f.ensureEnvelope(t, a.ID)
		ev.ApprovalID = f.approveFor(a.ID)
	})
	require.Error(t, err, "a paused agent is resumed before it is promoted")
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))
}

// A revoked agent stays revoked, and the database says so (F-114, 00734).
//
// `State.IsTerminal` returns true for REVOKED and SUPERSEDED and CanTransition
// gives both an empty destination list, so in Go a terminal state has no
// outgoing edge. The schema said nothing about it, and both promotion CHECKs on
// agent_lifecycle_transitions open with `from_stage = to_stage OR ...`.
//
// That clause is right for what it was written for: a pause, a resume or a
// revocation does not move the STAGE, and demanding promotion evidence for one
// would demand evidence for nothing happening. It is wrong for coming BACK.
// Revoke goes through sideTransition, which keeps the agent's stage -- so a
// revoked LIVE agent still has stage='LIVE', and a row saying
// REVOKED -> LIVE with from_stage = to_stage = 'LIVE' satisfies both CHECKs
// through that first clause: no approval, no ir_hash, no risk_policy_hash, no
// evidence_hash. It committed, and an agent that had been revoked was trading
// live capital again with nothing recorded about why.
//
// 00726 closed the promotion form of that short-circuit. This is the
// resurrection form, which survived because returning from a side state does
// not move the stage either.
func TestRevokedIsTerminalInTheDatabase(t *testing.T) {
	f := newFixture(t)
	a := f.walkTo(t, StageShadow)

	require.NoError(t, inTx(ctxAs(f.operator(security.RoleOperations, security.RoleRisk)), t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.lifecycle.Revoke(ctx, tx, a.ID, "revoked for the test")
		return err
	}))

	// The row that used to commit. Written directly, because no Go path offers
	// it -- which is exactly why the schema has to refuse it.
	err := inTx(ctxAs(f.operator(security.RoleOperations, security.RoleRisk)), t, func(ctx context.Context, tx pgx.Tx) error {
		_, ierr := tx.Exec(ctx,
			`INSERT INTO agent_lifecycle_transitions
			   (id, agent_id, from_state, to_state, from_stage, to_stage, actor_type, actor_id, reason)
			 VALUES (gen_random_uuid(), $1, 'REVOKED', 'LIVE', $2, $2, 'SYSTEM', 'x', 'resurrect')`,
			a.ID, string(a.Stage))
		return ierr
	})
	require.Error(t, err, "a revoked agent was returned to an operating state with no approval and no evidence")
	assert.Contains(t, err.Error(), "terminal_is_terminal")

	// The other terminal state, so the rule is about terminality and not about
	// one value.
	err = inTx(ctxAs(f.operator(security.RoleOperations, security.RoleRisk)), t, func(ctx context.Context, tx pgx.Tx) error {
		_, ierr := tx.Exec(ctx,
			`INSERT INTO agent_lifecycle_transitions
			   (id, agent_id, from_state, to_state, from_stage, to_stage, actor_type, actor_id, reason)
			 VALUES (gen_random_uuid(), $1, 'SUPERSEDED', 'SHADOW', $2, $2, 'SYSTEM', 'x', 'resurrect')`,
			a.ID, string(a.Stage))
		return ierr
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "terminal_is_terminal")

	// The control: the agent is still revoked, and the constraint refused the
	// rows rather than the transaction failing for some other reason.
	after, err := f.store.Get(context.Background(), testDB, a.ID)
	require.NoError(t, err)
	assert.Equal(t, StateRevoked, after.State)
}
