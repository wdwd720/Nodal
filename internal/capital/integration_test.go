//go:build integration

package capital

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Shared with internal/db and test/integration/migrations: schema-mutating
// suites hold pg_advisory_lock(424242) exclusively; this DML-only suite
// holds it shared. Provision the database with:
//
//	go run ./scripts/testdb -name capital
const testAdvisoryLockID = 424242

var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
)

func TestMain(m *testing.M) {
	os.Exit(testMain(m))
}

func testMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	lockConn, err := pgx.Connect(ctx, testAppURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "capital integration: connect for advisory lock:", err)
		return 1
	}
	defer func() { _ = lockConn.Close(ctx) }()
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock_shared($1)", testAdvisoryLockID); err != nil {
		fmt.Fprintln(os.Stderr, "capital integration: advisory lock:", err)
		return 1
	}
	defer func() { _, _ = lockConn.Exec(ctx, "SELECT pg_advisory_unlock_shared($1)", testAdvisoryLockID) }()
	// Schema is a precondition: the isolated test database is provisioned by
	// scripts/testdb and must be fully migrated.
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "capital integration: migrate up:", err)
		return 1
	}
	if missing := missingTables(ctx, lockConn); len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "capital integration: missing tables %v\n", missing)
		return 1
	}
	// The torture test runs 100 goroutines; 40 connections keeps most of
	// them queued at the database row lock rather than at the pool.
	testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "capital-itest", MaxConns: 40})
	if err != nil {
		fmt.Fprintln(os.Stderr, "capital integration: open pool:", err)
		return 1
	}
	defer testDB.Close()
	return m.Run()
}

// missingTables returns the tables this suite depends on that do not exist.
func missingTables(ctx context.Context, conn *pgx.Conn) []string {
	var missing []string
	for _, tbl := range []string{
		"users", "accounts", "assets", "ledger_accounts", "journal_transactions", "journal_entries", "ledger_balances",
		"capital_envelopes", "capital_envelope_changes", "asset_reservation_totals", "asset_reservations", "withdrawal_holds",
		"strategies", "strategy_versions", "agents", "economic_exposures", "instruments", "trade_intents",
	} {
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, tbl).Scan(&exists); err != nil || !exists {
			missing = append(missing, tbl)
		}
	}
	return missing
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set; skipping integration test (go run ./scripts/testdb -name capital)")
	}
}

// count returns how many events were emitted on topic.
func (e *recordingEmitter) count(topic string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, ev := range e.events {
		if ev.Topic == topic {
			n++
		}
	}
	return n
}

// last returns the most recent event emitted on topic.
func (e *recordingEmitter) last(topic string) (recordedEvent, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := len(e.events) - 1; i >= 0; i-- {
		if e.events[i].Topic == topic {
			return e.events[i], true
		}
	}
	return recordedEvent{}, false
}

// testingT is the subset of *testing.T that *rapid.T also satisfies, so the
// fixture serves both example tests and property tests.
type testingT interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	FailNow()
	Logf(format string, args ...any)
}

// USDC-like base units: 6 decimals.
const (
	oneUSDC   int64 = 1_000_000
	usdcUnits       = 500 * oneUSDC // the PART 23 reservation size
)

func qty(baseUnits int64) money.Quantity { return money.QuantityFromInt64(baseUnits) }

type fixture struct {
	t         testingT
	ctx       context.Context
	clk       *clock.Fake
	emit      *recordingEmitter
	svc       *Service
	env       *EnvelopeService
	user      accounts.UserID
	accountID accounts.AccountID
	assetID   assets.AssetID
	admin     context.Context // customer principal owning accountID
	envelopes []EnvelopeID
	accounts  []accounts.AccountID
	agents    map[accounts.AccountID]agentRef // seeded strategy/version/agent per account
}

// agentRef is the seeded agent (and its strategy version) an envelope is
// bound to; capital_envelopes carries foreign keys to both (migration 00501).
type agentRef struct {
	agentID           string
	strategyVersionID string
}

// newFixture creates a fresh user, customer account and settlement asset.
// cp_app cannot delete rows, so isolation comes from fresh identities.
func newFixture(t testingT) *fixture {
	t.Helper()
	ctx := context.Background()
	f := &fixture{t: t, ctx: ctx, clk: clock.NewFake(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)), emit: &recordingEmitter{}, agents: map[accounts.AccountID]agentRef{}}
	f.svc = NewService(f.clk, f.emit)
	f.env = NewEnvelopeService(f.clk, f.emit)

	repo := accounts.NewRepository()
	u, err := repo.CreateUser(ctx, testDB, "https://idp.test", "sub-"+uuid.NewString(), nil)
	require.NoError(t, err)
	f.user = u.ID
	f.accountID = f.newAccount()
	a, err := assets.NewRepository().Create(ctx, testDB, assets.Asset{
		Chain: "solana-devnet", MintAddress: "mint-" + uuid.NewString(), Kind: assets.KindSPLToken, ValueDomain: valuedomain.SelfCustodialCrypto,
		Symbol: "USDC", Name: "USD Coin", Decimals: 6, IsStablecoin: true, PegCurrency: "USD",
		RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	f.assetID = a.ID
	f.admin = f.principalFor(f.accountID)
	return f
}

func (f *fixture) principalFor(accountIDs ...accounts.AccountID) context.Context {
	ids := make([]string, 0, len(accountIDs))
	for _, a := range accountIDs {
		ids = append(ids, a.String())
	}
	return security.WithPrincipal(f.ctx, security.Principal{
		SubjectID: f.user.String(), ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer},
		AccountIDs: ids, AuthTime: f.clk.Now(),
	})
}

func (f *fixture) newAccount() accounts.AccountID {
	f.t.Helper()
	acc, err := accounts.NewRepository().CreateAccount(f.ctx, testDB, f.user, accounts.KindCustomer)
	require.NoError(f.t, err)
	f.accounts = append(f.accounts, acc.ID)
	return acc.ID
}

func (f *fixture) inTx(fn func(ctx context.Context, tx pgx.Tx) error) error {
	return testDB.InTx(f.ctx, db.TxOptions{}, fn)
}

// agentFor seeds (once per account) the strategy, compiled strategy version
// and VALIDATED agent that an envelope must reference.
func (f *fixture) agentFor(account accounts.AccountID) agentRef {
	f.t.Helper()
	if ref, ok := f.agents[account]; ok {
		return ref
	}
	strategyID, versionID, agentID := id.New[id.Any](), id.New[id.Any](), id.New[id.Any]()
	hash := sha256.Sum256([]byte(versionID.String()))
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO strategies (id, owner_account_id, owner_user_id, name, source_kind, status, created_by_actor_type, created_by_actor_id)
			VALUES ($1, $2, $3, $4, 'TYPESCRIPT_SDK', 'ACTIVE', 'USER', $5)`, strategyID, account, f.user, "strategy-"+strategyID.String(), f.user.String()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO strategy_versions (id, strategy_id, version, schema_version, ir, ir_hash, effect_set, status, source_kind, source_hash,
				compiler_version, risk_policy_version, risk_policy_hash, model_budget, data_budget, envelope_requirements, human_readable, built_at)
			VALUES ($1, $2, 1, 1, '{}'::jsonb, $3, '{}', 'COMPILED', 'TYPESCRIPT_SDK', $3, 'test', 'risk-v1', $3, '{}'::jsonb, '{}'::jsonb, '{}'::jsonb, 'test strategy', $4)`,
			versionID, strategyID, hash[:], f.clk.Now()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO agents (id, account_id, strategy_id, strategy_version_id, name, stage, state, created_by_actor_type, created_by_actor_id)
			VALUES ($1, $2, $3, $4, $5, 'VALIDATED', 'VALIDATED', 'USER', $6)`, agentID, account, strategyID, versionID, "agent-"+agentID.String(), f.user.String())
		return err
	})
	require.NoError(f.t, err, "seed agent")
	ref := agentRef{agentID: agentID.String(), strategyVersionID: versionID.String()}
	f.agents[account] = ref
	return ref
}

// seedWallet posts a SEED journal transaction Dr WALLET / Cr CAPITAL so the
// ledger trigger materializes the balance, exactly as production funding
// would. It never touches ledger_balances directly (cp_app cannot).
func (f *fixture) seedWallet(account accounts.AccountID, amount money.Quantity) {
	f.t.Helper()
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var wallet, capital id.ID[id.Any]
		if err := tx.QueryRow(ctx, `INSERT INTO ledger_accounts (id, owner_type, owner_id, code, asset_id, normal_side)
			VALUES ($1, 'CUSTOMER', $2, 'WALLET', $3, 'DEBIT')
			ON CONFLICT (owner_type, owner_id, code, asset_id) DO UPDATE SET status = ledger_accounts.status RETURNING id`,
			id.New[id.Any](), account, f.assetID).Scan(&wallet); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO ledger_accounts (id, owner_type, owner_id, code, asset_id, normal_side)
			VALUES ($1, 'CUSTOMER', $2, 'CAPITAL', $3, 'CREDIT')
			ON CONFLICT (owner_type, owner_id, code, asset_id) DO UPDATE SET status = ledger_accounts.status RETURNING id`,
			id.New[id.Any](), account, f.assetID).Scan(&capital); err != nil {
			return err
		}
		txID := id.New[id.Any]()
		hash := sha256.Sum256([]byte(txID.String()))
		if _, err := tx.Exec(ctx, `INSERT INTO journal_transactions (id, kind, idempotency_key, reference_type, reference_id, effective_at, posted_by_actor_type, posted_by_actor_id, content_hash)
			VALUES ($1, 'SEED', $2, 'test', $3, $4, 'SYSTEM', 'capital-itest', $5)`, txID, "seed:"+txID.String(), txID.String(), f.clk.Now(), hash[:]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO journal_entries (id, transaction_id, seq, ledger_account_id, asset_id, side, quantity) VALUES ($1, $2, 0, $3, $4, 'DEBIT', $5::numeric)`,
			id.New[id.Any](), txID, wallet, f.assetID, amount); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO journal_entries (id, transaction_id, seq, ledger_account_id, asset_id, side, quantity) VALUES ($1, $2, 1, $3, $4, 'CREDIT', $5::numeric)`,
			id.New[id.Any](), txID, capital, f.assetID, amount)
		return err
	})
	require.NoError(f.t, err, "seed wallet")
}

func (f *fixture) request(account accounts.AccountID, baseUnits, usdMinor int64, env *EnvelopeID) ReserveRequest {
	// IntentID is left empty: asset_reservations.intent_id references
	// trade_intents (migration 00201), which the intent service inserts in
	// the same transaction in production. TestIntegration_IntentLink covers
	// the linked path.
	return ReserveRequest{
		AccountID: account.String(), AssetID: f.assetID, Quantity: qty(baseUnits), USDMinor: usdMinor, EnvelopeID: env,
		ActorType: security.ActorAgent, ActorID: "agent-" + f.user.String(),
		IdempotencyKey: "intent:" + uuid.NewString(), TTL: 5 * time.Minute, Reason: "test intent",
	}
}

func (f *fixture) reserve(req ReserveRequest) (Reservation, error) {
	var out Reservation
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.Reserve(ctx, tx, req)
		return err
	})
	return out, err
}

func (f *fixture) mustReserve(req ReserveRequest) Reservation {
	f.t.Helper()
	r, err := f.reserve(req)
	require.NoError(f.t, err)
	return r
}

func (f *fixture) consume(rid ReservationID, baseUnits, usdMinor int64, orderID string, final bool) (Reservation, error) {
	var out Reservation
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if final {
			out, err = f.svc.ConsumeFinal(ctx, tx, rid, qty(baseUnits), usdMinor, orderID)
		} else {
			out, err = f.svc.Consume(ctx, tx, rid, qty(baseUnits), usdMinor, orderID)
		}
		return err
	})
	return out, err
}

func (f *fixture) release(rid ReservationID, reason string) (Reservation, error) {
	var out Reservation
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.Release(ctx, tx, rid, reason)
		return err
	})
	return out, err
}

func (f *fixture) lock(rid ReservationID, orderID string) error {
	return f.inTx(func(ctx context.Context, tx pgx.Tx) error { return f.svc.LockForOrder(ctx, tx, rid, orderID) })
}

func (f *fixture) expireDue(limit int) []ReservationID {
	f.t.Helper()
	var out []ReservationID
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.ExpireDue(ctx, tx, f.clk.Now(), limit)
		return err
	}))
	return out
}

func (f *fixture) totals(account accounts.AccountID) money.Quantity {
	f.t.Helper()
	a, err := f.svc.Availability(f.ctx, testDB, account.String(), f.assetID, f.clk.Now())
	require.NoError(f.t, err)
	return a.Reserved
}

func (f *fixture) availability(account accounts.AccountID) Availability {
	f.t.Helper()
	a, err := f.svc.Availability(f.ctx, testDB, account.String(), f.assetID, f.clk.Now())
	require.NoError(f.t, err)
	return a
}

func (f *fixture) get(rid ReservationID) Reservation {
	f.t.Helper()
	r, err := f.svc.Get(f.ctx, testDB, rid)
	require.NoError(f.t, err)
	return r
}

func (f *fixture) envelope(eid EnvelopeID) Envelope {
	f.t.Helper()
	e, err := getEnvelope(f.ctx, testDB, eid, false)
	require.NoError(f.t, err)
	return e
}

// createEnvelope creates an ACTIVE envelope for account with the given
// allocation (USD minor) through the admin path.
func (f *fixture) createEnvelope(account accounts.AccountID, allocationMinor int64, mutate ...func(*Envelope)) Envelope {
	f.t.Helper()
	ref := f.agentFor(account)
	e := Envelope{
		AccountID: account, AgentID: ref.agentID, StrategyVersionID: ref.strategyVersionID, SettlementAssetID: f.assetID,
		Allocation:   money.USDFromMinor(allocationMinor),
		MaxDailyLoss: money.USDFromMinor(1_000_000_00), MaxDrawdown: money.USDFromMinor(1_000_000_00),
		MaxSingleTrade: money.USDFromMinor(allocationMinor), MaxPosition: money.USDFromMinor(allocationMinor),
		PolicyVersion: "risk-v1", Status: EnvelopeActive,
	}
	for _, m := range mutate {
		m(&e)
	}
	var out Envelope
	err := testDB.InTx(f.principalFor(account), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.env.Create(ctx, tx, e)
		return err
	})
	require.NoError(f.t, err)
	f.envelopes = append(f.envelopes, out.ID)
	return out
}

func (f *fixture) setStatus(eid EnvelopeID, to EnvelopeStatus, reason string) (Envelope, error) {
	var out Envelope
	err := testDB.InTx(f.principalFor(f.accounts...), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.env.SetStatus(ctx, tx, eid, to, reason)
		return err
	})
	return out, err
}

func (f *fixture) update(eid EnvelopeID, p EnvelopeAuthorityPatch) (Envelope, error) {
	var out Envelope
	err := testDB.InTx(f.principalFor(f.accounts...), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.env.Update(ctx, tx, eid, p)
		return err
	})
	return out, err
}

func (f *fixture) applyPnL(eid EnvelopeID, pnlMinor int64) (Envelope, error) {
	var out Envelope
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.env.ApplyRealizedPnL(ctx, tx, eid, pnlMinor, f.clk.Now())
		return err
	})
	return out, err
}

func (f *fixture) changeRows(eid EnvelopeID) []struct {
	ActorType string
	Reason    string
	Changes   map[string]FieldChange
} {
	f.t.Helper()
	rows, err := testDB.Query(f.ctx, `SELECT actor_type, reason, changes FROM capital_envelope_changes WHERE envelope_id = $1 ORDER BY occurred_at, id`, eid)
	require.NoError(f.t, err)
	defer rows.Close()
	var out []struct {
		ActorType string
		Reason    string
		Changes   map[string]FieldChange
	}
	for rows.Next() {
		var r struct {
			ActorType string
			Reason    string
			Changes   map[string]FieldChange
		}
		require.NoError(f.t, rows.Scan(&r.ActorType, &r.Reason, &r.Changes))
		out = append(out, r)
	}
	require.NoError(f.t, rows.Err())
	return out
}

// assertNoDrift checks the fixture's accounts and envelopes against the
// recomputed truth.
func (f *fixture) assertNoDrift() {
	f.t.Helper()
	drifts, err := VerifyReservationTotals(f.ctx, testDB)
	require.NoError(f.t, err)
	for _, d := range drifts {
		for _, a := range f.accounts {
			if d.AccountID == a {
				f.t.Errorf("reservation totals drift for account %s asset %s: recorded %s computed %s", d.AccountID, d.AssetID, d.Recorded, d.Computed)
			}
		}
	}
	envDrifts, err := VerifyEnvelopeBudgets(f.ctx, testDB)
	require.NoError(f.t, err)
	for _, d := range envDrifts {
		for _, e := range f.envelopes {
			if d.EnvelopeID == e {
				f.t.Errorf("envelope budget drift %s: allocation %s available %s reserved %s deployed %s active-reserved %s",
					d.EnvelopeID, d.Allocation, d.Available, d.Reserved, d.Deployed, d.ActiveReserved)
			}
		}
	}
}

func assertCode(t testingT, err error, code errs.Code, msg ...string) {
	t.Helper()
	note := ""
	if len(msg) > 0 {
		note = " (" + msg[0] + ")"
	}
	if err == nil {
		t.Fatalf("expected %s, got nil%s", code, note)
	}
	if got := errs.CodeOf(err); got != code {
		t.Fatalf("expected %s, got %s: %v%s", code, got, err, note)
	}
}

func assertUSD(t testingT, want int64, got money.USD, what string) {
	t.Helper()
	if got.Minor() != want {
		t.Errorf("%s: want %d got %d", what, want, got.Minor())
	}
}

// --- lifecycle -------------------------------------------------------------

func TestIntegration_ReserveConsumeLifecycle(t *testing.T) {
	requireEnv(t)
	f := newFixture(t)
	f.seedWallet(f.accountID, qty(1_000*oneUSDC))
	env := f.createEnvelope(f.accountID, 1_000_00)

	r := f.mustReserve(f.request(f.accountID, 400*oneUSDC, 400_00, &env.ID))
	assert.Equal(t, ReservationActive, r.Status)
	assert.Equal(t, qty(400*oneUSDC).String(), r.Quantity.String())
	assert.Equal(t, env.ID, *r.EnvelopeID)
	assert.True(t, r.ExpiresAt.Equal(f.clk.Now().Add(5*time.Minute)))
	assert.Equal(t, qty(400*oneUSDC).String(), f.totals(f.accountID).String())
	e := f.envelope(env.ID)
	assertUSD(t, 600_00, e.Available, "available")
	assertUSD(t, 400_00, e.Reserved, "reserved")
	assertUSD(t, 0, e.Deployed, "deployed")
	assert.Equal(t, 1, f.emit.count(TopicReservationCreated))

	order := uuid.NewString()
	// Partial consume: the consumed part leaves the reservation pool (the
	// fill removes it from WALLET) and the envelope moves reserved → deployed.
	got, err := f.consume(r.ID, 150*oneUSDC, 150_00, order, false)
	require.NoError(t, err)
	assert.Equal(t, ReservationActive, got.Status)
	assert.Equal(t, order, got.LockedByOrderID)
	assert.Equal(t, qty(150*oneUSDC).String(), got.ConsumedQuantity.String())
	assert.Equal(t, qty(250*oneUSDC).String(), f.totals(f.accountID).String())
	e = f.envelope(env.ID)
	assertUSD(t, 600_00, e.Available, "available")
	assertUSD(t, 250_00, e.Reserved, "reserved")
	assertUSD(t, 150_00, e.Deployed, "deployed")

	// Consuming the rest finalizes automatically.
	got, err = f.consume(r.ID, 250*oneUSDC, 250_00, order, false)
	require.NoError(t, err)
	assert.Equal(t, ReservationConsumed, got.Status)
	require.NotNil(t, got.ConsumedAt)
	assert.True(t, f.totals(f.accountID).IsZero())
	e = f.envelope(env.ID)
	assertUSD(t, 600_00, e.Available, "available")
	assertUSD(t, 0, e.Reserved, "reserved")
	assertUSD(t, 400_00, e.Deployed, "deployed")
	assert.Equal(t, 2, f.emit.count(TopicReservationConsumed))

	// A consumed reservation is final.
	_, err = f.consume(r.ID, 1, 0, order, false)
	assertCode(t, err, errs.CodeInvalidStateTransition)
	_, err = f.release(r.ID, "late")
	assertCode(t, err, errs.CodeInvalidStateTransition)

	// Undeploy hands the cost basis back to available.
	var undeployed Envelope
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		undeployed, err = f.svc.Undeploy(ctx, tx, env.ID, money.USDFromMinor(400_00))
		return err
	}))
	assertUSD(t, 1_000_00, undeployed.Available, "available after undeploy")
	assertUSD(t, 0, undeployed.Deployed, "deployed after undeploy")
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.Undeploy(ctx, tx, env.ID, money.USDFromMinor(1))
		return err
	})
	assertCode(t, err, errs.CodeValidationFailed)

	active, err := f.svc.ListActive(f.ctx, testDB, f.accountID.String(), f.assetID)
	require.NoError(t, err)
	assert.Empty(t, active)
	f.assertNoDrift()
}

func TestIntegration_ConsumeFinalReturnsRemainder(t *testing.T) {
	requireEnv(t)
	f := newFixture(t)
	f.seedWallet(f.accountID, qty(1_000*oneUSDC))
	env := f.createEnvelope(f.accountID, 1_000_00)
	r := f.mustReserve(f.request(f.accountID, 400*oneUSDC, 400_00, &env.ID))

	// Nothing consumed yet: finalizing as CONSUMED is refused.
	_, err := f.consume(r.ID, 0, 0, "", true)
	assertCode(t, err, errs.CodeValidationFailed)

	got, err := f.consume(r.ID, 100*oneUSDC, 100_00, uuid.NewString(), true)
	require.NoError(t, err)
	assert.Equal(t, ReservationConsumed, got.Status)
	assert.Equal(t, qty(100*oneUSDC).String(), got.ConsumedQuantity.String())
	assert.Equal(t, qty(300*oneUSDC).String(), got.Remaining().String(), "the remainder is recorded, not consumed")
	assert.True(t, f.totals(f.accountID).IsZero(), "remainder returned to the account pool")
	e := f.envelope(env.ID)
	assertUSD(t, 900_00, e.Available, "available")
	assertUSD(t, 0, e.Reserved, "reserved")
	assertUSD(t, 100_00, e.Deployed, "deployed")

	// Over-consumption is refused with the remaining amounts.
	r2 := f.mustReserve(f.request(f.accountID, 100*oneUSDC, 100_00, &env.ID))
	_, err = f.consume(r2.ID, 101*oneUSDC, 0, "", false)
	assertCode(t, err, errs.CodeValidationFailed)
	_, err = f.consume(r2.ID, 1, 100_01, "", false)
	assertCode(t, err, errs.CodeValidationFailed)
	_, err = f.consume(r2.ID, 0, 0, "", false)
	assertCode(t, err, errs.CodeValidationFailed)
	f.assertNoDrift()
}

// FINANCIAL_MODEL §8: a released reservation can never support execution.
func TestReservation_ConsumeAfterReleaseRejected(t *testing.T) {
	requireEnv(t)
	f := newFixture(t)
	f.seedWallet(f.accountID, qty(1_000*oneUSDC))
	env := f.createEnvelope(f.accountID, 1_000_00)
	r := f.mustReserve(f.request(f.accountID, 400*oneUSDC, 400_00, &env.ID))

	released, err := f.release(r.ID, "intent cancelled")
	require.NoError(t, err)
	assert.Equal(t, ReservationReleased, released.Status)
	assert.Equal(t, "intent cancelled", released.ReleaseReason)
	require.NotNil(t, released.ReleasedAt)
	assert.True(t, f.totals(f.accountID).IsZero())
	e := f.envelope(env.ID)
	assertUSD(t, 1_000_00, e.Available, "available")
	assertUSD(t, 0, e.Reserved, "reserved")
	assert.Equal(t, 1, f.emit.count(TopicReservationReleased))

	_, err = f.consume(r.ID, 1*oneUSDC, 1_00, uuid.NewString(), false)
	assertCode(t, err, errs.CodeInvalidStateTransition)
	_, err = f.consume(r.ID, 1*oneUSDC, 1_00, uuid.NewString(), true)
	assertCode(t, err, errs.CodeInvalidStateTransition)
	_, err = f.release(r.ID, "again")
	assertCode(t, err, errs.CodeInvalidStateTransition)
	assertCode(t, f.lock(r.ID, uuid.NewString()), errs.CodeInvalidStateTransition)

	// Nothing moved: the account and envelope are exactly as after release.
	assert.True(t, f.totals(f.accountID).IsZero())
	e = f.envelope(env.ID)
	assertUSD(t, 1_000_00, e.Available, "available")
	assertUSD(t, 0, e.Reserved, "reserved")
	assertUSD(t, 0, e.Deployed, "deployed")
	assert.Equal(t, ReservationReleased, f.get(r.ID).Status)
	f.assertNoDrift()
}

// A reservation attached to a persisted intent (the production shape: the
// intent row is inserted in the same transaction) round-trips intent_id and
// satisfies asset_reservations_intent_fk.
func TestIntegration_IntentLink(t *testing.T) {
	requireEnv(t)
	f := newFixture(t)
	f.seedWallet(f.accountID, qty(1_000*oneUSDC))
	quote, err := assets.NewRepository().Create(f.ctx, testDB, assets.Asset{
		Chain: "solana-devnet", MintAddress: "mint-" + uuid.NewString(), Kind: assets.KindSPLToken, ValueDomain: valuedomain.SelfCustodialCrypto, Symbol: "SOL", Name: "Wrapped SOL",
		Decimals: 9, RiskClass: assets.RiskMajor, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	intentID := id.New[id.Any]()
	req := f.request(f.accountID, 100*oneUSDC, 100_00, nil)
	req.IntentID = intentID.String()
	var r Reservation
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		exposure, instrument := id.New[id.Any](), id.New[id.Any]()
		if _, err := tx.Exec(ctx, `INSERT INTO economic_exposures (id, kind, description, underlying_asset_id) VALUES ($1, 'ASSET_PRICE', 'SOL price', $2)`, exposure, quote.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO instruments (id, type, canonical_name, exposure_id, base_asset_id, quote_asset_id, settlement_asset_id, risk_class, status, active_from)
			VALUES ($1, 'SPOT_PAIR', 'SOL/USDC', $2, $3, $4, $4, 'MAJOR', 'ACTIVE', $5)`, instrument, exposure, quote.ID, f.assetID, f.clk.Now()); err != nil {
			return err
		}
		// A USER intent: agent intents additionally require prediction /
		// agent linkage (enforced by a trigger in the predictions migration),
		// which is out of scope here.
		// content_hash is mandatory since migration 00605; a raw fixture row hashes its own idempotency key.
		if _, err := tx.Exec(ctx, `INSERT INTO trade_intents (id, account_id, actor_type, actor_id, action, instrument_id, notional_usd_minor, constraints, requested_at, idempotency_key, correlation_id, mode, status, content_hash)
			VALUES ($1, $2, 'USER', $3, 'ACQUIRE_NOTIONAL', $4, 100_00, '{}'::jsonb, $5, $6, $7, 'PAPER', 'RECEIVED', sha256(convert_to($6, 'UTF8')))`,
			intentID, f.accountID, f.user.String(), instrument, f.clk.Now(), req.IdempotencyKey, uuid.NewString()); err != nil {
			return err
		}
		var err error
		r, err = f.svc.Reserve(ctx, tx, req)
		if err != nil {
			return err
		}
		// Link back without a status change: intent status transitions
		// require an intent_transitions row (audit trigger) and belong to the
		// intent service.
		_, err = tx.Exec(ctx, `UPDATE trade_intents SET reservation_id = $2 WHERE id = $1`, intentID, r.ID)
		return err
	}))
	assert.Equal(t, intentID.String(), r.IntentID)
	assert.Equal(t, intentID.String(), f.get(r.ID).IntentID)
	ev, ok := f.emit.last(TopicReservationCreated)
	require.True(t, ok)
	payload, isReservation := ev.Payload.(ReservationEvent)
	require.True(t, isReservation, "payload %T", ev.Payload)
	assert.Equal(t, intentID.String(), payload.IntentID)

	// An intent that does not exist is refused by the foreign key: the
	// reservation is never inserted and the totals are untouched.
	orphan := f.request(f.accountID, oneUSDC, 0, nil)
	orphan.IntentID = uuid.NewString()
	_, err = f.reserve(orphan)
	require.Error(t, err)
	assert.True(t, db.IsForeignKeyViolation(err), err)
	assert.Equal(t, qty(100*oneUSDC).String(), f.totals(f.accountID).String())
	f.assertNoDrift()
}

func TestIntegration_InsufficientBuyingPower(t *testing.T) {
	requireEnv(t)
	f := newFixture(t)
	f.seedWallet(f.accountID, qty(100*oneUSDC))

	_, err := f.reserve(f.request(f.accountID, 100*oneUSDC+1, 0, nil))
	assertCode(t, err, errs.CodeInsufficientBuyingPower)
	e, _ := errs.As(err)
	assert.Equal(t, qty(100*oneUSDC).String(), e.Fields["available"])
	assert.Equal(t, qty(100*oneUSDC+1).String(), e.Fields["requested"])
	assert.True(t, f.totals(f.accountID).IsZero(), "a failed reservation reserves nothing")

	f.mustReserve(f.request(f.accountID, 100*oneUSDC, 0, nil))
	_, err = f.reserve(f.request(f.accountID, 1, 0, nil))
	assertCode(t, err, errs.CodeInsufficientBuyingPower)
	e, _ = errs.As(err)
	assert.Equal(t, "0", e.Fields["available"])
	assert.Equal(t, qty(100*oneUSDC).String(), e.Fields["reserved"])

	// An account with no ledger account at all has zero available.
	other := f.newAccount()
	_, err = f.reserve(f.request(other, 1, 0, nil))
	assertCode(t, err, errs.CodeInsufficientBuyingPower)
	assert.Equal(t, 1, f.emit.count(TopicReservationCreated))
	f.assertNoDrift()
}

func TestIntegration_WithdrawalHoldsReduceAvailable(t *testing.T) {
	requireEnv(t)
	f := newFixture(t)
	f.seedWallet(f.accountID, qty(1_000*oneUSDC))

	var hold WithdrawalHold
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		hold, err = f.svc.PlaceHold(ctx, tx, WithdrawalHold{AccountID: f.accountID, AssetID: f.assetID, Quantity: qty(700 * oneUSDC), Reason: "reversible funding", DepositID: uuid.NewString()})
		return err
	}))
	assert.Equal(t, 1, f.emit.count(TopicHoldPlaced))
	a := f.availability(f.accountID)
	assert.Equal(t, qty(700*oneUSDC).String(), a.Held.String())
	assert.Equal(t, qty(300*oneUSDC).String(), a.Available.String())

	_, err := f.reserve(f.request(f.accountID, 400*oneUSDC, 0, nil))
	assertCode(t, err, errs.CodeInsufficientBuyingPower)
	e, _ := errs.As(err)
	assert.Equal(t, qty(700*oneUSDC).String(), e.Fields["held"])
	f.mustReserve(f.request(f.accountID, 300*oneUSDC, 0, nil))
	_, err = f.reserve(f.request(f.accountID, 1, 0, nil))
	assertCode(t, err, errs.CodeInsufficientBuyingPower)

	// Releasing the hold frees the quantity again.
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.ReleaseHold(ctx, tx, hold.ID, "funding:reversibility-window-passed")
		return err
	}))
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.ReleaseHold(ctx, tx, hold.ID, "again")
		return err
	})
	assertCode(t, err, errs.CodeInvalidStateTransition)
	holds, err := f.svc.ActiveHolds(f.ctx, testDB, f.accountID.String(), f.assetID, f.clk.Now())
	require.NoError(t, err)
	assert.Empty(t, holds)
	f.mustReserve(f.request(f.accountID, 700*oneUSDC, 0, nil))

	// An expired hold no longer binds.
	other := f.newAccount()
	f.seedWallet(other, qty(100*oneUSDC))
	exp := f.clk.Now().Add(time.Minute)
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.PlaceHold(ctx, tx, WithdrawalHold{AccountID: other, AssetID: f.assetID, Quantity: qty(100 * oneUSDC), Reason: "review", ExpiresAt: &exp})
		return err
	}))
	_, err = f.reserve(f.request(other, 1, 0, nil))
	assertCode(t, err, errs.CodeInsufficientBuyingPower)
	f.clk.Advance(2 * time.Minute)
	f.mustReserve(f.request(other, 100*oneUSDC, 0, nil))

	// Hold validation.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.PlaceHold(ctx, tx, WithdrawalHold{AccountID: other, AssetID: f.assetID, Quantity: qty(0), Reason: ""})
		return err
	})
	assertCode(t, err, errs.CodeValidationFailed)
	f.assertNoDrift()
}

func TestIntegration_IdempotentReserve(t *testing.T) {
	requireEnv(t)
	f := newFixture(t)
	f.seedWallet(f.accountID, qty(1_000*oneUSDC))
	env := f.createEnvelope(f.accountID, 1_000_00)
	req := f.request(f.accountID, 400*oneUSDC, 400_00, &env.ID)

	first := f.mustReserve(req)
	again := f.mustReserve(req)
	assert.Equal(t, first.ID, again.ID)
	assert.Equal(t, qty(400*oneUSDC).String(), f.totals(f.accountID).String(), "counted once")
	assertUSD(t, 600_00, f.envelope(env.ID).Available, "envelope charged once")
	assert.Equal(t, 1, f.emit.count(TopicReservationCreated), "one economic effect, one event")

	// Same key, different economics.
	diff := req
	diff.Quantity = qty(401 * oneUSDC)
	_, err := f.reserve(diff)
	assertCode(t, err, errs.CodeInvalidIdempotencyReuse)
	diff = req
	diff.USDMinor = 400_01
	_, err = f.reserve(diff)
	assertCode(t, err, errs.CodeInvalidIdempotencyReuse)
	diff = req
	diff.EnvelopeID = nil
	_, err = f.reserve(diff)
	assertCode(t, err, errs.CodeInvalidIdempotencyReuse)
	diff = req
	diff.AccountID = f.newAccount().String()
	_, err = f.reserve(diff)
	assertCode(t, err, errs.CodeInvalidIdempotencyReuse)

	// A replay after release returns the existing (released) row and
	// reserves nothing new.
	_, err = f.release(first.ID, "done")
	require.NoError(t, err)
	replayed := f.mustReserve(req)
	assert.Equal(t, first.ID, replayed.ID)
	assert.Equal(t, ReservationReleased, replayed.Status)
	assert.True(t, f.totals(f.accountID).IsZero())
	f.assertNoDrift()
}

func TestIntegration_ExpireDueSkipsLocked(t *testing.T) {
	requireEnv(t)
	f := newFixture(t)
	f.seedWallet(f.accountID, qty(1_000*oneUSDC))
	env := f.createEnvelope(f.accountID, 1_000_00)
	short := f.request(f.accountID, 100*oneUSDC, 100_00, &env.ID)
	short.TTL = time.Minute
	a := f.mustReserve(short)
	short = f.request(f.accountID, 200*oneUSDC, 200_00, &env.ID)
	short.TTL = time.Minute
	b := f.mustReserve(short)
	long := f.request(f.accountID, 50*oneUSDC, 50_00, nil)
	long.TTL = time.Hour
	c := f.mustReserve(long)

	order := uuid.NewString()
	require.NoError(t, f.lock(b.ID, order))
	require.NoError(t, f.lock(b.ID, order), "locking again with the same order is a no-op")
	assertCode(t, f.lock(b.ID, uuid.NewString()), errs.CodeConflict)
	assert.Equal(t, 1, f.emit.count(TopicReservationLocked))
	_, err := f.consume(b.ID, 1, 0, uuid.NewString(), false)
	assertCode(t, err, errs.CodeConflict, "another order cannot consume a locked reservation")

	assert.Empty(t, f.expireDue(10), "nothing is due yet")
	f.clk.Advance(2 * time.Minute)
	expired := f.expireDue(10)
	assert.Equal(t, []ReservationID{a.ID}, expired, "only the unlocked, due reservation expires")
	assert.Equal(t, ReservationExpired, f.get(a.ID).Status)
	assert.Equal(t, ReservationActive, f.get(b.ID).Status, "locked reservation untouched")
	assert.Equal(t, ReservationActive, f.get(c.ID).Status, "not yet due")
	assert.Equal(t, qty(250*oneUSDC).String(), f.totals(f.accountID).String())
	e := f.envelope(env.ID)
	assertUSD(t, 800_00, e.Available, "available")
	assertUSD(t, 200_00, e.Reserved, "reserved")
	assert.Equal(t, 1, f.emit.count(TopicReservationExpired))
	assert.Empty(t, f.expireDue(10), "expiry is idempotent")

	// The locked reservation is still consumable by its order.
	got, err := f.consume(b.ID, 200*oneUSDC, 200_00, order, false)
	require.NoError(t, err)
	assert.Equal(t, ReservationConsumed, got.Status)
	f.assertNoDrift()
}

// --- envelopes -------------------------------------------------------------

func TestIntegration_EnvelopeAdminLifecycle(t *testing.T) {
	requireEnv(t)
	f := newFixture(t)
	f.seedWallet(f.accountID, qty(10_000*oneUSDC))

	draft := f.createEnvelope(f.accountID, 1_000_00, func(e *Envelope) { e.Status = EnvelopeDraft })
	assert.Equal(t, EnvelopeDraft, draft.Status)
	assertUSD(t, 1_000_00, draft.Available, "available defaults to allocation")
	assert.Equal(t, security.ActorUser, draft.CreatedByActorType)
	assert.Equal(t, f.user.String(), draft.CreatedByActorID)
	assert.True(t, draft.DailyLossResetAt.Equal(time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)))
	assert.Equal(t, int64(1), draft.Version)
	rows := f.changeRows(draft.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, "USER", rows[0].ActorType)
	assert.Equal(t, FieldChange{From: nil, To: float64(1_000_00)}, rows[0].Changes["allocation_usd_minor"], "jsonb numbers decode as float64")
	assert.Equal(t, FieldChange{From: nil, To: "DRAFT"}, rows[0].Changes["status"])

	// A DRAFT envelope backs nothing.
	_, err := f.reserve(f.request(f.accountID, oneUSDC, 1_00, &draft.ID))
	assertCode(t, err, errs.CodeInsufficientBuyingPower)
	e, _ := errs.As(err)
	assert.Equal(t, "DRAFT", e.Fields["envelope_status"])

	active, err := f.setStatus(draft.ID, EnvelopeActive, "go live")
	require.NoError(t, err)
	assert.Equal(t, EnvelopeActive, active.Status)
	assert.Equal(t, int64(2), active.Version)
	f.mustReserve(f.request(f.accountID, 100*oneUSDC, 100_00, &draft.ID))

	// Allocation up and down, with the committed floor enforced.
	up, err := f.update(draft.ID, EnvelopeAuthorityPatch{Allocation: ptrUSD(2_000_00), PolicyVersion: ptrStr("risk-v2"), Reason: "double"})
	require.NoError(t, err)
	assertUSD(t, 2_000_00, up.Allocation, "allocation")
	assertUSD(t, 1_900_00, up.Available, "available")
	assertUSD(t, 100_00, up.Reserved, "reserved")
	assert.Equal(t, "risk-v2", up.PolicyVersion)
	_, err = f.update(draft.ID, EnvelopeAuthorityPatch{Allocation: ptrUSD(99_00), Reason: "too low"})
	assertCode(t, err, errs.CodeValidationFailed)
	down, err := f.update(draft.ID, EnvelopeAuthorityPatch{Allocation: ptrUSD(100_00), Reason: "to the floor"})
	require.NoError(t, err)
	assertUSD(t, 0, down.Available, "available")
	assertUSD(t, 100_00, down.Reserved, "reserved")
	_, err = f.reserve(f.request(f.accountID, oneUSDC, 1_00, &draft.ID))
	assertCode(t, err, errs.CodeInsufficientBuyingPower)
	same, err := f.update(draft.ID, EnvelopeAuthorityPatch{Allocation: ptrUSD(100_00), Reason: "noop"})
	require.NoError(t, err)
	assert.Equal(t, down.Version, same.Version, "a no-op patch is not a version")
	assert.Len(t, f.changeRows(draft.ID), 4, "create, activate, up, down")
	_, err = f.update(draft.ID, EnvelopeAuthorityPatch{Reason: "nothing"})
	assertCode(t, err, errs.CodeValidationFailed)

	// Expired envelopes back nothing; extending the window fixes it. The
	// window must stay ordered, so effective_at moves back with expires_at.
	past := f.clk.Now().Add(-time.Second)
	earlier := past.Add(-time.Hour)
	_, err = f.update(draft.ID, EnvelopeAuthorityPatch{ExpiresAt: &past, Reason: "expire it"})
	assertCode(t, err, errs.CodeValidationFailed, "expires_at before effective_at")
	_, err = f.update(draft.ID, EnvelopeAuthorityPatch{EffectiveAt: &earlier, ExpiresAt: &past, Reason: "expire it"})
	require.NoError(t, err)
	_, err = f.update(draft.ID, EnvelopeAuthorityPatch{Allocation: ptrUSD(200_00), Reason: "more"})
	require.NoError(t, err)
	_, err = f.reserve(f.request(f.accountID, oneUSDC, 1_00, &draft.ID))
	assertCode(t, err, errs.CodeInsufficientBuyingPower)
	_, err = f.update(draft.ID, EnvelopeAuthorityPatch{ClearExpiresAt: true, Reason: "open"})
	require.NoError(t, err)
	f.mustReserve(f.request(f.accountID, oneUSDC, 1_00, &draft.ID))

	// Cross-account envelope use is refused.
	other := f.newAccount()
	f.seedWallet(other, qty(10*oneUSDC))
	_, err = f.reserve(f.request(other, oneUSDC, 1_00, &draft.ID))
	assertCode(t, err, errs.CodeValidationFailed)

	// Tenant scoping and agent containment on the real path.
	stranger := security.WithPrincipal(f.ctx, security.Principal{SubjectID: "u-x", ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer}, AccountIDs: []string{accounts.NewAccountID().String()}})
	_, err = f.env.Get(stranger, testDB, draft.ID)
	assertCode(t, err, errs.CodeForbidden)
	_, err = f.env.ListForAccount(stranger, testDB, f.accountID.String())
	assertCode(t, err, errs.CodeForbidden)
	err = testDB.InTx(stranger, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.env.SetStatus(ctx, tx, draft.ID, EnvelopePaused, "x")
		return err
	})
	assertCode(t, err, errs.CodeForbidden)
	agent := security.WithPrincipal(f.ctx, security.AgentPrincipal(draft.AgentID, f.accountID.String()))
	err = testDB.InTx(agent, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.env.Update(ctx, tx, draft.ID, EnvelopeAuthorityPatch{Allocation: ptrUSD(1_000_000_00), Reason: "moar"})
		return err
	})
	assertCode(t, err, errs.CodeForbidden)
	_, err = f.env.Get(agent, testDB, draft.ID)
	assertCode(t, err, errs.CodeForbidden)
	operator := security.WithPrincipal(f.ctx, security.Principal{SubjectID: "op-1", ActorType: security.ActorOperator, Roles: []security.Role{security.RoleRisk}})
	got, err := f.env.Get(operator, testDB, draft.ID)
	require.NoError(t, err, "operators with account:read_any see every account")
	assert.Equal(t, draft.ID, got.ID)
	list, err := f.env.ListForAccount(f.admin, testDB, f.accountID.String())
	require.NoError(t, err)
	require.Len(t, list, 1)
	_, err = f.env.Get(f.admin, testDB, NewEnvelopeID())
	assertCode(t, err, errs.CodeNotFound)

	// Status machine on the real path.
	_, err = f.setStatus(draft.ID, EnvelopeDraft, "back")
	assertCode(t, err, errs.CodeInvalidStateTransition)
	_, err = f.setStatus(draft.ID, "BOGUS", "x")
	assertCode(t, err, errs.CodeValidationFailed)
	_, err = f.setStatus(draft.ID, EnvelopePaused, "")
	assertCode(t, err, errs.CodeValidationFailed)
	paused, err := f.setStatus(draft.ID, EnvelopePaused, "operator pause")
	require.NoError(t, err)
	assert.Equal(t, EnvelopePaused, paused.Status)
	_, err = f.reserve(f.request(f.accountID, oneUSDC, 1_00, &draft.ID))
	assertCode(t, err, errs.CodeInsufficientBuyingPower)
	revoked, err := f.setStatus(draft.ID, EnvelopeRevoked, "strategy retired")
	require.NoError(t, err)
	assert.Equal(t, EnvelopeRevoked, revoked.Status)
	_, err = f.update(draft.ID, EnvelopeAuthorityPatch{Allocation: ptrUSD(1), Reason: "x"})
	assertCode(t, err, errs.CodeInvalidStateTransition)
	_, err = f.setStatus(draft.ID, EnvelopeActive, "resurrect")
	assertCode(t, err, errs.CodeInvalidStateTransition)
	assert.GreaterOrEqual(t, f.emit.count(TopicEnvelopeStatusChanged), 3)
	f.assertNoDrift()
}

func TestIntegration_CreateEnvelopeValidation(t *testing.T) {
	requireEnv(t)
	f := newFixture(t)
	create := func(ctx context.Context, e Envelope) error {
		return testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.env.Create(ctx, tx, e)
			return err
		})
	}
	ref := f.agentFor(f.accountID)
	base := Envelope{AccountID: f.accountID, AgentID: ref.agentID, StrategyVersionID: ref.strategyVersionID, SettlementAssetID: f.assetID, Allocation: money.USDFromMinor(100_00), PolicyVersion: "v1"}

	unknownAgent := base
	unknownAgent.AgentID = uuid.NewString()
	assertCode(t, create(f.admin, unknownAgent), errs.CodeValidationFailed, "unknown agent is a foreign-key failure")

	bad := base
	bad.Available = money.USDFromMinor(1)
	assertCode(t, create(f.admin, bad), errs.CodeValidationFailed)
	bad = base
	bad.SettlementAssetID = assets.NewAssetID()
	assertCode(t, create(f.admin, bad), errs.CodeValidationFailed, "unknown asset is a foreign-key failure")
	bad = base
	bad.Status = EnvelopePaused
	assertCode(t, create(f.admin, bad), errs.CodeValidationFailed)
	bad = base
	bad.AccountID = accounts.NewAccountID()
	assertCode(t, create(f.admin, bad), errs.CodeForbidden, "principal does not own that account")

	ok := base
	ok.ID = NewEnvelopeID()
	require.NoError(t, create(f.admin, ok))
	assertCode(t, create(f.admin, ok), errs.CodeConflict, "duplicate id")
	assert.Equal(t, 1, f.emit.count(TopicEnvelopeCreated))
}

func TestIntegration_ApplyRealizedPnLExhausts(t *testing.T) {
	requireEnv(t)
	f := newFixture(t)
	f.seedWallet(f.accountID, qty(10_000*oneUSDC))
	env := f.createEnvelope(f.accountID, 1_000_00, func(e *Envelope) {
		e.MaxDailyLoss = money.USDFromMinor(100_00)
		e.MaxDrawdown = money.USDFromMinor(500_00)
	})

	got, err := f.applyPnL(env.ID, 30_00)
	require.NoError(t, err)
	assertUSD(t, 30_00, got.RealizedPnL, "pnl")
	assert.Equal(t, EnvelopeActive, got.Status)
	got, err = f.applyPnL(env.ID, -99_99)
	require.NoError(t, err)
	assert.Equal(t, EnvelopeActive, got.Status)
	assertUSD(t, 99_99, got.DailyLoss, "daily loss")
	assertUSD(t, 99_99, got.CurrentDrawdown, "drawdown from the 30.00 peak")

	got, err = f.applyPnL(env.ID, -1)
	require.NoError(t, err)
	assert.Equal(t, EnvelopeExhausted, got.Status)
	assertUSD(t, 100_00, got.DailyLoss, "daily loss")
	assert.Equal(t, 1, f.emit.count(TopicEnvelopeExhausted))
	rows := f.changeRows(env.ID)
	last := rows[len(rows)-1]
	assert.Equal(t, "SYSTEM", last.ActorType)
	assert.Equal(t, "limit reached: daily_loss", last.Reason)
	assert.Equal(t, FieldChange{From: "ACTIVE", To: "EXHAUSTED"}, last.Changes["status"])

	_, err = f.reserve(f.request(f.accountID, oneUSDC, 1_00, &env.ID))
	assertCode(t, err, errs.CodeInsufficientBuyingPower)
	_, err = f.setStatus(env.ID, EnvelopeActive, "try again")
	assertCode(t, err, errs.CodeInvalidStateTransition, "limit still breached today")

	// Further losses keep accumulating without flipping status again.
	got, err = f.applyPnL(env.ID, -50_00)
	require.NoError(t, err)
	assert.Equal(t, EnvelopeExhausted, got.Status)
	assert.Equal(t, 1, f.emit.count(TopicEnvelopeExhausted))

	// After the UTC midnight rollover the daily counter resets and the
	// operator can re-activate; the cumulative loss remains.
	f.clk.Set(time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC))
	reactivated, err := f.setStatus(env.ID, EnvelopeActive, "new day")
	require.NoError(t, err)
	assert.Equal(t, EnvelopeActive, reactivated.Status)
	assertUSD(t, 0, reactivated.DailyLoss, "daily loss reset")
	assertUSD(t, 150_00, reactivated.RealizedLoss, "cumulative loss")
	assert.True(t, reactivated.DailyLossResetAt.Equal(time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)))
	f.mustReserve(f.request(f.accountID, oneUSDC, 1_00, &env.ID))

	// Drawdown exhaustion cannot be reset by the calendar; the limit must
	// be raised (an audited authority change) before re-activation.
	got, err = f.applyPnL(env.ID, -350_00)
	require.NoError(t, err)
	assert.Equal(t, EnvelopeExhausted, got.Status)
	assertUSD(t, 500_00, got.CurrentDrawdown, "drawdown")
	f.clk.Set(time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC))
	_, err = f.setStatus(env.ID, EnvelopeActive, "new day again")
	assertCode(t, err, errs.CodeInvalidStateTransition)
	_, err = f.update(env.ID, EnvelopeAuthorityPatch{MaxDrawdown: ptrUSD(600_00), Reason: "risk committee approved", ApprovalID: uuid.NewString()})
	require.NoError(t, err)
	_, err = f.setStatus(env.ID, EnvelopeActive, "limit raised")
	require.NoError(t, err)

	_, err = f.applyPnL(NewEnvelopeID(), 1)
	assertCode(t, err, errs.CodeNotFound)
	f.assertNoDrift()
}

func TestIntegration_FrozenAccountAndHaltedAsset(t *testing.T) {
	requireEnv(t)
	f := newFixture(t)
	f.seedWallet(f.accountID, qty(100*oneUSDC))

	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := accounts.NewRepository().Transition(ctx, tx, f.accountID, accounts.StatusChange{To: accounts.StatusFrozen, ActorType: "OPERATOR", ActorID: "op", Reason: "deficit"}, f.clk.Now())
		return err
	}))
	_, err := f.reserve(f.request(f.accountID, oneUSDC, 0, nil))
	assertCode(t, err, errs.CodeAccountFrozen)
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := accounts.NewRepository().Transition(ctx, tx, f.accountID, accounts.StatusChange{To: accounts.StatusRestricted, ActorType: "OPERATOR", ActorID: "op", Reason: "review"}, f.clk.Now())
		return err
	}))
	f.mustReserve(f.request(f.accountID, oneUSDC, 0, nil))

	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := assets.NewRepository().Transition(ctx, tx, f.assetID, assets.StatusChange{To: assets.StatusHalted, ActorType: "OPERATOR", ActorID: "op", Reason: "depeg"})
		return err
	}))
	_, err = f.reserve(f.request(f.accountID, oneUSDC, 0, nil))
	assertCode(t, err, errs.CodeAssetRestricted)
	f.assertNoDrift()
}

func TestIntegration_EmitterFailureRollsBackReservation(t *testing.T) {
	requireEnv(t)
	f := newFixture(t)
	f.seedWallet(f.accountID, qty(100*oneUSDC))
	env := f.createEnvelope(f.accountID, 100_00)
	f.emit.fail = fmt.Errorf("outbox unavailable")
	req := f.request(f.accountID, 50*oneUSDC, 50_00, &env.ID)
	_, err := f.reserve(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outbox unavailable")
	f.emit.fail = nil

	assert.True(t, f.totals(f.accountID).IsZero(), "nothing reserved without its event")
	assertUSD(t, 100_00, f.envelope(env.ID).Available, "envelope untouched")
	_, found, err := reservationByKey(f.ctx, testDB, req.IdempotencyKey)
	require.NoError(t, err)
	assert.False(t, found)
	f.mustReserve(req)
	f.assertNoDrift()
}

func TestIntegration_VerifyDetectsDrift(t *testing.T) {
	requireEnv(t)
	f := newFixture(t)
	f.seedWallet(f.accountID, qty(1_000*oneUSDC))
	env := f.createEnvelope(f.accountID, 1_000_00)
	f.mustReserve(f.request(f.accountID, 400*oneUSDC, 400_00, &env.ID))
	f.assertNoDrift()

	// Corrupt the totals row directly (cp_app may update it) and expect the
	// verifier to name the pair with both figures.
	_, err := testDB.Exec(f.ctx, `UPDATE asset_reservation_totals SET reserved = reserved + 1 WHERE account_id = $1 AND asset_id = $2`, f.accountID, f.assetID)
	require.NoError(t, err)
	drifts, err := VerifyReservationTotals(f.ctx, testDB)
	require.NoError(t, err)
	var mine []Drift
	for _, d := range drifts {
		if d.AccountID == f.accountID {
			mine = append(mine, d)
		}
	}
	require.Len(t, mine, 1)
	assert.Equal(t, qty(400*oneUSDC+1).String(), mine[0].Recorded.String())
	assert.Equal(t, qty(400*oneUSDC).String(), mine[0].Computed.String())
	assert.Equal(t, "1", mine[0].Delta().String())
	_, err = testDB.Exec(f.ctx, `UPDATE asset_reservation_totals SET reserved = reserved - 1 WHERE account_id = $1 AND asset_id = $2`, f.accountID, f.assetID)
	require.NoError(t, err)

	// Envelope: move budget out of reserved without a matching reservation
	// change (stays within the CHECK) and expect both inconsistencies.
	_, err = testDB.Exec(f.ctx, `UPDATE capital_envelopes SET reserved_usd_minor = reserved_usd_minor - 100 WHERE id = $1`, env.ID)
	require.NoError(t, err)
	envDrifts, err := VerifyEnvelopeBudgets(f.ctx, testDB)
	require.NoError(t, err)
	var found *EnvelopeDrift
	for i := range envDrifts {
		if envDrifts[i].EnvelopeID == env.ID {
			found = &envDrifts[i]
		}
	}
	require.NotNil(t, found)
	assertUSD(t, 400_00-100, found.Reserved, "recorded reserved")
	assertUSD(t, 400_00, found.ActiveReserved, "active reserved")
	_, err = testDB.Exec(f.ctx, `UPDATE capital_envelopes SET reserved_usd_minor = reserved_usd_minor + 100 WHERE id = $1`, env.ID)
	require.NoError(t, err)
	f.assertNoDrift()
}
