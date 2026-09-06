//go:build integration

package intent_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/idempotency"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/security"
)

// Shared with internal/db and test/integration/migrations: schema-mutating
// suites hold pg_advisory_lock(424242) exclusively; this DML-only suite holds
// it shared. Run against an isolated database, never controlplane_test:
//
//	eval "$(go run ./scripts/testdb -name intent -export)"
//	go test -count=1 -race -tags=integration ./internal/intent/ ./internal/quote/
//	go run ./scripts/testdb -name intent -drop
const testAdvisoryLockID = 424242

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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	lockConn, err := pgx.Connect(ctx, testAppURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "intent integration: connect for advisory lock:", err)
		return 1
	}
	defer func() { _ = lockConn.Close(ctx) }()
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock_shared($1)", testAdvisoryLockID); err != nil {
		fmt.Fprintln(os.Stderr, "intent integration: advisory lock:", err)
		return 1
	}
	defer func() { _, _ = lockConn.Exec(ctx, "SELECT pg_advisory_unlock_shared($1)", testAdvisoryLockID) }()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "intent integration: migrate up:", err)
		return 1
	}
	testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "intent-itest", MaxConns: 8})
	if err != nil {
		fmt.Fprintln(os.Stderr, "intent integration: open pool:", err)
		return 1
	}
	defer testDB.Close()
	return m.Run()
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set; skipping integration test")
	}
}

type fixture struct {
	t          *testing.T
	ctx        context.Context
	clk        *clock.Fake
	repo       *intent.Repository
	svc        *intent.Service
	user       accounts.UserID
	account    accounts.AccountID
	sol, usdc  assets.Asset
	instrument instruments.Instrument
}

// agentRef is a seeded SHADOW agent with a committed prediction.
type agentRef struct {
	agentID, strategyVersionID, predictionID string
	committedAt                              time.Time
}

// newFixture seeds a fresh user, account, two assets and a spot pair. cp_app
// cannot delete rows, so isolation comes from fresh identities.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireEnv(t)
	ctx := context.Background()
	f := &fixture{t: t, ctx: ctx, clk: clock.NewFake(t0)}
	f.repo = intent.NewRepository(f.clk, event.NewOutbox(f.clk), audit.NewWriter())
	f.svc = intent.NewService(f.repo, idempotency.NewStore(f.clk.Now), f.clk, 0)

	acctRepo := accounts.NewRepository()
	u, err := acctRepo.CreateUser(ctx, testDB, "https://idp.test", "sub-"+uuidStr(), nil)
	require.NoError(t, err)
	f.user = u.ID
	f.account = f.newAccount()

	suffix := uuidStr()
	f.sol, err = assets.NewRepository().Create(ctx, testDB, assets.Asset{Chain: "solana-test", MintAddress: "SOL-" + suffix, Kind: assets.KindSPLToken, Symbol: "SOL", Name: "Solana", Decimals: 9, RiskClass: assets.RiskMajor, Status: assets.StatusActive})
	require.NoError(t, err)
	f.usdc, err = assets.NewRepository().Create(ctx, testDB, assets.Asset{Chain: "solana-test", MintAddress: "USDC-" + suffix, Kind: assets.KindSPLToken, Symbol: "USDC", Name: "USD Coin", Decimals: 6, IsStablecoin: true, PegCurrency: "USD", RiskClass: assets.RiskSettlement, Status: assets.StatusActive})
	require.NoError(t, err)
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		f.instrument, err = instruments.NewRepository().CreateSpotPair(ctx, tx, instruments.SpotPairSpec{Base: f.sol.ID, Quote: f.usdc.ID, Settlement: f.usdc.ID, CanonicalName: "SOL/USDC " + suffix, RiskClass: assets.RiskMajor, Status: assets.StatusActive, ActiveFrom: t0.Add(-time.Hour)})
		return err
	}))
	return f
}

func (f *fixture) newAccount() accounts.AccountID {
	f.t.Helper()
	a, err := accounts.NewRepository().CreateAccount(f.ctx, testDB, f.user, accounts.KindCustomer)
	require.NoError(f.t, err)
	return a.ID
}

func (f *fixture) inTx(fn func(ctx context.Context, tx pgx.Tx) error) error {
	ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
	defer cancel()
	return testDB.InTx(ctx, db.TxOptions{}, fn)
}

// userIntent returns a valid USER intent for the fixture account.
func (f *fixture) userIntent(key string) intent.TradeIntent {
	ti := baseIntent()
	ti.AccountID = f.account.String()
	ti.ActorID = f.user.String()
	ti.InstrumentID = f.instrument.ID
	ti.IdempotencyKey = key
	ti.RequestedAt = f.clk.Now()
	ti.Deadline = f.clk.Now().Add(time.Minute)
	return ti
}

func (f *fixture) create(ti intent.TradeIntent) intent.TradeIntent {
	f.t.Helper()
	var out intent.TradeIntent
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.repo.Create(ctx, tx, ti)
		return err
	}))
	return out
}

func (f *fixture) transition(iid intent.IntentID, to intent.Status, ev intent.TransitionEvidence) (intent.TradeIntent, error) {
	f.t.Helper()
	var out intent.TradeIntent
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.repo.Transition(ctx, tx, iid, to, ev)
		return err
	})
	return out, err
}

func (f *fixture) principal() security.Principal {
	return customer(f.user.String(), f.account.String())
}

// seedAgent inserts a strategy, a compiled strategy version, a SHADOW agent,
// a run and a committed prediction for the fixture account and instrument.
func (f *fixture) seedAgent(committedAt time.Time) agentRef {
	f.t.Helper()
	strategyID, versionID, agentID, runID, predictionID := id.New[id.Any](), id.New[id.Any](), id.New[id.Any](), id.New[id.Any](), id.New[id.Any]()
	hash := sha256.Sum256([]byte(versionID.String()))
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO strategies (id, owner_account_id, owner_user_id, name, source_kind, status, created_by_actor_type, created_by_actor_id)
			VALUES ($1, $2, $3, $4, 'TYPESCRIPT_SDK', 'ACTIVE', 'USER', $5)`, strategyID, f.account, f.user, "strategy-"+strategyID.String(), f.user.String()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO strategy_versions (id, strategy_id, version, schema_version, ir, ir_hash, effect_set, status, source_kind, source_hash,
				compiler_version, risk_policy_version, risk_policy_hash, model_budget, data_budget, envelope_requirements, human_readable, built_at)
			VALUES ($1, $2, 1, 1, '{}'::jsonb, $3, '{}', 'COMPILED', 'TYPESCRIPT_SDK', $3, 'test', 'risk-v1', $3, '{}'::jsonb, '{}'::jsonb, '{}'::jsonb, 'test strategy', $4)`,
			versionID, strategyID, hash[:], t0.Add(-time.Hour)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO agents (id, account_id, strategy_id, strategy_version_id, name, stage, state, mode, created_by_actor_type, created_by_actor_id)
			VALUES ($1, $2, $3, $4, $5, 'SHADOW', 'SHADOW', 'SHADOW', 'USER', $6)`, agentID, f.account, strategyID, versionID, "agent-"+agentID.String(), f.user.String()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO agent_runs (id, agent_id, agent_version, strategy_version_id, account_id, mode, trigger_name, trigger_kind, trigger_dedup_key, decision_time, status, correlation_id)
			VALUES ($1, $2, 1, $3, $4, 'SHADOW', 'tick', 'ON_INTERVAL', $5, $6, 'PREDICTED', $7)`, runID, agentID, versionID, f.account, hash[:], committedAt.Add(-time.Second), "corr-"+runID.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO predictions (id, agent_id, agent_version, agent_run_id, action_name, strategy_version_id, account_id, mode, instrument_id, horizon_ms,
				expected_return_bps, downside_probability, max_downside_bps, confidence, information_set_hash, decision_available_at, committed_at)
			VALUES ($1, $2, 1, $3, 'buy', $4, $5, 'SHADOW', $6, 60000, 25, 0.2, 80, 0.6, $7, $8, $9)`,
			predictionID, agentID, runID, versionID, f.account, f.instrument.ID, hash[:], committedAt.Add(-time.Second), committedAt)
		return err
	}), "seed agent")
	return agentRef{agentID: agentID.String(), strategyVersionID: versionID.String(), predictionID: predictionID.String(), committedAt: committedAt}
}

func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, testDB.QueryRow(f.ctx, query, args...).Scan(&n))
	return n
}

func (f *fixture) outboxRows(iid intent.IntentID) int {
	return f.count(`SELECT count(*) FROM outbox_events WHERE topic = 'intent.transitioned' AND aggregate_id = $1`, iid.String())
}

func (f *fixture) auditRows(iid intent.IntentID, action string) int {
	return f.count(`SELECT count(*) FROM audit_events WHERE stream = $1 AND resource_id = $2 AND action = $3`, audit.AccountStream(f.account.String()), iid.String(), action)
}

func TestIntegration_Create_IdempotentReplayAndConflict(t *testing.T) {
	f := newFixture(t)
	first := f.create(f.userIntent("k-create"))
	assert.Equal(t, intent.StatusReceived, first.Status)
	assert.False(t, first.Existing)
	assert.Len(t, first.ContentHash, 32)
	assert.Equal(t, t0, first.ReceivedAt)
	assert.Equal(t, f.account.String(), first.AccountID)
	assert.Equal(t, "200.00", first.NotionalUSD.String())
	assert.Equal(t, []string{"JUPITER"}, first.Constraints.AllowedVenues)
	assert.Equal(t, 500*time.Millisecond, first.Constraints.QuoteFreshness)
	assert.Equal(t, 1, f.outboxRows(first.ID), "intent.transitioned outbox row for RECEIVED")
	assert.Equal(t, 1, f.auditRows(first.ID, intent.AuditActionReceived))

	// Same key, same content, different server-side fields → replay, nothing written.
	retry := f.userIntent("k-create")
	retry.ID = intent.NewIntentID()
	retry.CorrelationID = "another-request"
	retry.RequestedAt = t0.Add(time.Second)
	again := f.create(retry)
	assert.True(t, again.Existing)
	assert.Equal(t, first.ID, again.ID)
	assert.Equal(t, first.CorrelationID, again.CorrelationID, "the original intent is returned")
	assert.Equal(t, 1, f.outboxRows(first.ID))
	assert.Equal(t, 1, f.auditRows(first.ID, intent.AuditActionReceived))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM trade_intents WHERE account_id = $1 AND idempotency_key = 'k-create'`, f.account))

	// Same key, different content → deterministic conflict.
	conflict := f.userIntent("k-create")
	conflict.NotionalUSD = usd(300_00)
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.repo.Create(ctx, tx, conflict)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, errs.CodeOf(err))

	got, err := f.repo.Get(f.ctx, testDB, first.ID)
	require.NoError(t, err)
	assert.Equal(t, first.ContentHash, got.ContentHash)
	assert.Equal(t, first.Constraints, got.Constraints)
	_, err = f.repo.Get(f.ctx, testDB, intent.NewIntentID())
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))

	// Structural validation and referential integrity.
	bad := f.userIntent("k-bad")
	bad.Mode = ""
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error { _, err := f.repo.Create(ctx, tx, bad); return err })
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	missing := f.userIntent("k-missing-instrument")
	missing.InstrumentID = instruments.NewInstrumentID()
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error { _, err := f.repo.Create(ctx, tx, missing); return err })
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
}

func TestIntegration_Transition_WritesEvidenceAndBindingRefusesBareUpdate(t *testing.T) {
	f := newFixture(t)
	ti := f.create(f.userIntent("k-transition"))

	f.clk.Advance(time.Second)
	ev := intent.TransitionEvidence{ActorType: security.ActorService, ActorID: "eligibility-worker", Reason: "ALLOW policy elig-v1", EvidenceRef: "eligibility_decision:" + uuidStr()}
	moved, err := f.transition(ti.ID, intent.StatusEligibilityChecked, ev)
	require.NoError(t, err)
	assert.Equal(t, intent.StatusEligibilityChecked, moved.Status)
	assert.Nil(t, moved.TerminalAt)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM intent_transitions WHERE intent_id = $1 AND from_status = 'RECEIVED' AND to_status = 'ELIGIBILITY_CHECKED' AND actor_type = 'SERVICE' AND evidence_ref = $2`, ti.ID, ev.EvidenceRef))
	assert.Equal(t, 2, f.outboxRows(ti.ID), "received + transition")
	assert.Equal(t, 1, f.auditRows(ti.ID, intent.AuditActionTransitions))

	// Illegal transition writes nothing.
	_, err = f.transition(ti.ID, intent.StatusPlanned, ev)
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM intent_transitions WHERE intent_id = $1`, ti.ID))
	assert.Equal(t, 2, f.outboxRows(ti.ID))

	// Evidence rules.
	_, err = f.transition(ti.ID, intent.StatusRejected, ev)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "REJECTED needs a rejection code")
	_, err = f.transition(ti.ID, intent.StatusRiskChecked, intent.TransitionEvidence{ActorType: security.ActorService, ActorID: "risk", Reason: "x", RejectionCode: "NOPE"})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "non-terminal transitions carry no rejection code")
	_, err = f.transition(ti.ID, intent.StatusRiskChecked, intent.TransitionEvidence{ActorType: "ROBOT", ActorID: "risk", Reason: "x"})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = f.transition(intent.NewIntentID(), intent.StatusRiskChecked, ev)
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))

	// Bare status UPDATE as cp_app is refused at COMMIT by migration 00603.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE trade_intents SET status = 'RISK_CHECKED' WHERE id = $1`, ti.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "AU001", db.SQLState(err), "got %v", err)
	cur, err := f.repo.Get(f.ctx, testDB, ti.ID)
	require.NoError(t, err)
	assert.Equal(t, intent.StatusEligibilityChecked, cur.Status, "the bare update did not commit")

	// Terminal transition records the rejection code and terminal_at; nothing follows it.
	f.clk.Advance(time.Second)
	rejected, err := f.transition(ti.ID, intent.StatusRejected, intent.TransitionEvidence{ActorType: security.ActorService, ActorID: "risk-kernel", Reason: "REJECT RISK_MAX_POSITION", RejectionCode: string(errs.CodeRiskMaxPosition)})
	require.NoError(t, err)
	assert.Equal(t, intent.StatusRejected, rejected.Status)
	assert.Equal(t, string(errs.CodeRiskMaxPosition), rejected.RejectionCode)
	require.NotNil(t, rejected.TerminalAt)
	assert.Equal(t, f.clk.Now(), *rejected.TerminalAt)
	for _, to := range intent.Statuses() {
		ev := intent.TransitionEvidence{ActorType: security.ActorSystem, ActorID: "sys", Reason: "x"}
		if to.IsTerminal() {
			ev.RejectionCode = "X"
		}
		_, err := f.transition(ti.ID, to, ev)
		assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err), "%s from REJECTED", to)
	}
	assert.Equal(t, 3, f.outboxRows(ti.ID))
	assert.Equal(t, 2, f.auditRows(ti.ID, intent.AuditActionTransitions))
	var toStatus, code, from string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT payload->>'to_status', payload->>'rejection_code', payload->>'from_status' FROM outbox_events
		WHERE topic = 'intent.transitioned' AND aggregate_id = $1 ORDER BY recorded_at DESC, id DESC LIMIT 1`, ti.ID.String()).Scan(&toStatus, &code, &from))
	assert.Equal(t, "REJECTED", toStatus)
	assert.Equal(t, "RISK_MAX_POSITION", code)
	assert.Equal(t, "ELIGIBILITY_CHECKED", from)
	var auditStatus string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT payload->>'to_status' FROM audit_events WHERE stream = $1 AND resource_id = $2 AND action = $3 ORDER BY stream_seq DESC LIMIT 1`,
		audit.AccountStream(f.account.String()), ti.ID.String(), intent.AuditActionTransitions).Scan(&auditStatus))
	assert.Equal(t, "REJECTED", auditStatus)
}

func TestIntegration_Link_SetOnceAndSupersede(t *testing.T) {
	f := newFixture(t)
	ti := f.create(f.userIntent("k-link"))
	link := func(l intent.Links) (intent.TradeIntent, error) {
		var out intent.TradeIntent
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = f.repo.Link(ctx, tx, ti.ID, l)
			return err
		})
		return out, err
	}
	// Every link column is a foreign key, so seed real targets.
	e1, d1, r1, r2, p1, p2 := f.seedEligibilityDecision(ti.ID), f.seedRiskDecision(ti.ID), f.seedReservation(ti.ID), f.seedReservation(ti.ID), f.seedPlan(ti.ID, 1), f.seedPlan(ti.ID, 2)

	out, err := link(intent.Links{ReservationID: r1, EligibilityDecisionID: e1})
	require.NoError(t, err)
	assert.Equal(t, r1, out.Links.ReservationID)
	assert.Equal(t, e1, out.Links.EligibilityDecisionID)
	_, err = link(intent.Links{ReservationID: r1})
	require.NoError(t, err, "same value is a no-op")
	_, err = link(intent.Links{ReservationID: r2})
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err), "reservation is set once")
	_, err = link(intent.Links{PlanID: p1})
	require.NoError(t, err)
	out, err = link(intent.Links{PlanID: p2, RiskDecisionID: d1})
	require.NoError(t, err, "a re-plan supersedes")
	assert.Equal(t, p2, out.Links.PlanID)
	assert.Equal(t, d1, out.Links.RiskDecisionID)
	assert.Equal(t, r1, out.Links.ReservationID)
	_, err = link(intent.Links{OrderID: "order-1"})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = link(intent.Links{OrderID: uuidStr()})
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err), "an unknown order is a foreign-key failure")
	_, err = link(intent.Links{})
	require.NoError(t, err, "nothing to link is a no-op")
	assert.Equal(t, 3, f.auditRows(ti.ID, intent.AuditActionLinked))
	got, err := f.repo.Get(f.ctx, testDB, ti.ID)
	require.NoError(t, err)
	assert.Equal(t, intent.Links{EligibilityDecisionID: e1, RiskDecisionID: d1, ReservationID: r1, PlanID: p2}, got.Links)
	assert.Equal(t, intent.StatusReceived, got.Status, "linking never changes status")
}

func (f *fixture) seedEligibilityDecision(iid intent.IntentID) string {
	f.t.Helper()
	did := id.New[id.Any]()
	hash := sha256.Sum256([]byte(did.String()))
	_, err := testDB.Exec(f.ctx, `INSERT INTO eligibility_decisions (id, account_id, user_id, intent_id, context_kind, eligible, policy_version, context_hash, evaluated_at)
		VALUES ($1, $2, $3, $4, 'TRADE', true, 'elig-v1', $5, $6)`, did, f.account, f.user, iid, hash[:], f.clk.Now())
	require.NoError(f.t, err)
	return did.String()
}

func (f *fixture) seedRiskDecision(iid intent.IntentID) string {
	f.t.Helper()
	did := id.New[id.Any]()
	hash := sha256.Sum256([]byte(did.String()))
	_, err := testDB.Exec(f.ctx, `INSERT INTO risk_decisions (id, intent_id, account_id, stage, policy_version, policy_hash, account_snapshot, market_snapshot, decision, evaluator_version, evaluated_at)
		VALUES ($1, $2, $3, 'PRE_TRADE', 'risk-v1', $4, '{}'::jsonb, '{}'::jsonb, 'ALLOW', 'test', $5)`, did, iid, f.account, hash[:], f.clk.Now())
	require.NoError(f.t, err)
	return did.String()
}

func (f *fixture) seedReservation(iid intent.IntentID) string {
	f.t.Helper()
	rid := id.New[id.Any]()
	_, err := testDB.Exec(f.ctx, `INSERT INTO asset_reservations (id, account_id, asset_id, intent_id, actor_type, actor_id, quantity, usd_minor, status, idempotency_key, expires_at)
		VALUES ($1, $2, $3, $4, 'USER', $5, 1000000, 100, 'ACTIVE', $6, $7)`, rid, f.account, f.usdc.ID, iid, f.user.String(), "res-"+rid.String(), f.clk.Now().Add(time.Hour))
	require.NoError(f.t, err)
	return rid.String()
}

func (f *fixture) seedPlan(iid intent.IntentID, version int) string {
	f.t.Helper()
	pid := id.New[id.Any]()
	hash := sha256.Sum256([]byte(pid.String()))
	_, err := testDB.Exec(f.ctx, `INSERT INTO execution_plans (id, intent_id, version, planner_version, status, hard_constraints, plan_hash)
		VALUES ($1, $2, $3, 'planner-test', 'DRAFT', '{}'::jsonb, $4)`, pid, iid, version, hash[:])
	require.NoError(f.t, err)
	return pid.String()
}

func TestIntegration_ListForAccount_Cursor(t *testing.T) {
	f := newFixture(t)
	other := f.newAccount()
	var created []intent.IntentID
	for i := 0; i < 5; i++ {
		ti := f.userIntent(fmt.Sprintf("k-list-%d", i))
		ti.RequestedAt = t0.Add(time.Duration(i/2) * time.Second) // pairs share a requested_at to exercise the id tie-break
		created = append(created, f.create(ti).ID)
	}
	foreign := f.userIntent("k-list-other")
	foreign.AccountID = other.String()
	f.create(foreign)

	var seen []intent.IntentID
	cursor := ""
	pages := 0
	for {
		page, err := f.repo.ListForAccount(f.ctx, testDB, f.account.String(), cursor, 2)
		require.NoError(t, err)
		pages++
		for i := 1; i < len(page.Items); i++ {
			a, b := page.Items[i-1], page.Items[i]
			assert.True(t, a.RequestedAt.After(b.RequestedAt) || (a.RequestedAt.Equal(b.RequestedAt) && id.Compare(a.ID, b.ID) > 0), "ordered by (requested_at desc, id desc)")
		}
		for _, it := range page.Items {
			assert.Equal(t, f.account.String(), it.AccountID)
			seen = append(seen, it.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	assert.Equal(t, 3, pages)
	assert.ElementsMatch(t, created, seen, "every intent appears exactly once across pages")

	_, err := f.repo.ListForAccount(f.ctx, testDB, f.account.String(), "not-a-cursor", 2)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = f.repo.ListForAccount(f.ctx, testDB, "acct", "", 2)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	page, err := f.repo.ListForAccount(f.ctx, testDB, other.String(), "", 0)
	require.NoError(t, err)
	assert.Len(t, page.Items, 1)
	assert.Empty(t, page.NextCursor)
}

func TestIntegration_ListOpen(t *testing.T) {
	f := newFixture(t)
	open := f.create(f.userIntent("k-open"))
	closed := f.create(f.userIntent("k-closed"))
	_, err := f.transition(closed.ID, intent.StatusCancelled, intent.TransitionEvidence{ActorType: security.ActorUser, ActorID: f.user.String(), Reason: "changed my mind"})
	require.NoError(t, err)
	items, err := f.repo.ListOpen(f.ctx, testDB, intent.MaxPageSize)
	require.NoError(t, err)
	ids := map[intent.IntentID]bool{}
	for _, it := range items {
		assert.False(t, it.Status.IsTerminal())
		ids[it.ID] = true
	}
	assert.True(t, ids[open.ID])
	assert.False(t, ids[closed.ID])
}

func TestIntegration_Submit_IdempotentReplayAndTenancy(t *testing.T) {
	f := newFixture(t)
	req := intent.SubmitRequest{
		AccountID: f.account.String(), Action: intent.ActionAcquireNotional, InstrumentID: f.instrument.ID,
		NotionalUSD: usd(150_00), Deadline: t0.Add(time.Minute), IdempotencyKey: "cmd-1", Mode: intent.ModeLive,
		Constraints: intent.Constraints{MaxSlippageBPS: 50},
	}
	first, err := f.svc.Submit(f.ctx, testDB, f.principal(), req)
	require.NoError(t, err)
	assert.Equal(t, intent.StatusReceived, first.Status)
	assert.Equal(t, security.ActorUser, first.ActorType)
	assert.Equal(t, f.user.String(), first.ActorID)
	assert.NotEmpty(t, first.CorrelationID)
	assert.False(t, first.Existing)

	// Same command again (double click / browser retry) → the original result.
	f.clk.Advance(5 * time.Second)
	again, err := f.svc.Submit(f.ctx, testDB, f.principal(), req)
	require.NoError(t, err)
	assert.True(t, again.Existing)
	assert.Equal(t, first.ID, again.ID)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM trade_intents WHERE account_id = $1 AND idempotency_key = 'cmd-1'`, f.account))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM idempotency_keys WHERE actor_id = $1 AND endpoint = $2 AND key = 'cmd-1' AND status = 'COMPLETED' AND resource_id = $3`, "USER:"+f.user.String(), intent.SubmitEndpoint, first.ID.String()))

	// Same key, different body → deterministic conflict; still one intent.
	changed := req
	changed.NotionalUSD = usd(151_00)
	_, err = f.svc.Submit(f.ctx, testDB, f.principal(), changed)
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, errs.CodeOf(err))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM trade_intents WHERE account_id = $1`, f.account))

	// Cross-tenant: principal A cannot submit for account B, even with a fresh key.
	other := f.newAccount()
	foreign := req
	foreign.AccountID, foreign.IdempotencyKey = other.String(), "cmd-foreign"
	_, err = f.svc.Submit(f.ctx, testDB, customer("stranger-"+uuidStr(), f.account.String()), foreign)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM trade_intents WHERE account_id = $1`, other))

	// A business rejection concludes the command and replays as the same rejection.
	missing := req
	missing.IdempotencyKey, missing.InstrumentID = "cmd-missing", instruments.NewInstrumentID()
	_, err = f.svc.Submit(f.ctx, testDB, f.principal(), missing)
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
	_, err = f.svc.Submit(f.ctx, testDB, f.principal(), missing)
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err), "replayed rejection")
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM idempotency_keys WHERE actor_id = $1 AND endpoint = $2 AND key = 'cmd-missing' AND status = 'COMPLETED' AND response_status = 404`, "USER:"+f.user.String(), intent.SubmitEndpoint))
}

func TestIntegration_Submit_AgentIntents(t *testing.T) {
	f := newFixture(t)
	ref := f.seedAgent(t0.Add(-time.Second))
	agent := security.AgentPrincipal(ref.agentID, f.account.String())
	req := intent.SubmitRequest{
		AccountID: f.account.String(), Action: intent.ActionAcquireNotional, InstrumentID: f.instrument.ID,
		NotionalUSD: usd(25_00), Deadline: t0.Add(time.Minute), IdempotencyKey: "run:" + uuidStr() + ":buy", Mode: intent.ModeShadow,
		StrategyVersionID: ref.strategyVersionID, PredictionID: ref.predictionID,
	}
	created, err := f.svc.Submit(f.ctx, testDB, agent, req)
	require.NoError(t, err)
	assert.Equal(t, security.ActorAgent, created.ActorType)
	require.NotNil(t, created.AgentID)
	assert.Equal(t, ref.agentID, *created.AgentID)
	assert.Equal(t, ref.predictionID, *created.PredictionID)
	assert.Equal(t, intent.ModeShadow, created.Mode)

	// Agent cannot submit USER intents, nor for another account.
	asUser := req
	asUser.ActorType, asUser.IdempotencyKey = security.ActorUser, "run:x:user"
	_, err = f.svc.Submit(f.ctx, testDB, agent, asUser)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	elsewhere := req
	elsewhere.AccountID, elsewhere.IdempotencyKey = f.newAccount().String(), "run:x:elsewhere"
	_, err = f.svc.Submit(f.ctx, testDB, agent, elsewhere)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	// Mode disagreement and a prediction committed after the request are refused by the prediction guard.
	wrongMode := req
	wrongMode.Mode, wrongMode.IdempotencyKey = intent.ModeLive, "run:x:mode"
	_, err = f.svc.Submit(f.ctx, testDB, agent, wrongMode)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "%v", err)
	late := req
	late.RequestedAt, late.IdempotencyKey = ref.committedAt.Add(-time.Second), "run:x:late"
	late.Deadline = late.RequestedAt.Add(time.Minute)
	_, err = f.svc.Submit(f.ctx, testDB, agent, late)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "%v", err)

	// A user cannot smuggle agent linkage.
	linked := req
	linked.IdempotencyKey = "user-linked"
	_, err = f.svc.Submit(f.ctx, testDB, f.principal(), linked)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM trade_intents WHERE account_id = $1`, f.account))
}

func TestIntegration_IdentityIsImmutable(t *testing.T) {
	f := newFixture(t)
	ti := f.create(f.userIntent("k-immutable"))
	for _, stmt := range []string{
		`UPDATE trade_intents SET mode = 'PAPER' WHERE id = $1`,
		`UPDATE trade_intents SET notional_usd_minor = 1 WHERE id = $1`,
		`UPDATE trade_intents SET account_id = account_id, content_hash = sha256('x'::bytea) WHERE id = $1`,
		`UPDATE trade_intents SET idempotency_key = 'other' WHERE id = $1`,
	} {
		_, err := testDB.Exec(f.ctx, stmt, ti.ID)
		require.Error(t, err, stmt)
		assert.True(t, intent.IsImmutableIntent(err), "%s: %v", stmt, err)
	}
	got, err := f.repo.Get(f.ctx, testDB, ti.ID)
	require.NoError(t, err)
	assert.Equal(t, intent.ModeLive, got.Mode)
	assert.Equal(t, "200.00", got.NotionalUSD.String())
}
