//go:build integration

package risk

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
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
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Shared with internal/db and test/integration/migrations: schema-mutating
// suites hold pg_advisory_lock(424242) exclusively; this DML-only suite holds
// it shared. Run against an isolated database:
//
//	eval "$(go run ./scripts/testdb -name risk -export)"
//	go test -count=1 -race -tags=integration ./internal/risk/
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	lockConn, err := pgx.Connect(ctx, testAppURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "risk integration: connect for advisory lock:", err)
		return 1
	}
	defer func() { _ = lockConn.Close(ctx) }()
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock_shared($1)", testAdvisoryLockID); err != nil {
		fmt.Fprintln(os.Stderr, "risk integration: advisory lock:", err)
		return 1
	}
	defer func() { _, _ = lockConn.Exec(ctx, "SELECT pg_advisory_unlock_shared($1)", testAdvisoryLockID) }()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "risk integration: migrate up:", err)
		return 1
	}
	testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "risk-itest", MaxConns: 8})
	if err != nil {
		fmt.Fprintln(os.Stderr, "risk integration: open pool:", err)
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

// isolatedEra returns a random instant in 1990-2020 so GLOBAL policies
// recorded by one test (always with an expiry) never fall inside another
// test's window; cp_app cannot delete rows, so isolation is by time.
func isolatedEra(t *testing.T) time.Time {
	t.Helper()
	var b [8]byte
	_, err := rand.Read(b[:])
	require.NoError(t, err)
	offset := time.Duration(binary.LittleEndian.Uint64(b[:])%uint64(30*365*24)) * time.Hour
	return time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC).Add(offset)
}

func inTx(t *testing.T, fn func(ctx context.Context, tx pgx.Tx) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return testDB.InTx(ctx, db.TxOptions{}, fn)
}

// jsonObject returns m[key] as a JSON object, failing the test when it is not one.
func jsonObject(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	o, ok := m[key].(map[string]any)
	require.True(t, ok, "%q is not a JSON object: %T", key, m[key])
	return o
}

func newAccount(t *testing.T) (accounts.UserID, accounts.AccountID) {
	t.Helper()
	repo := accounts.NewRepository()
	var user accounts.User
	var acct accounts.Account
	require.NoError(t, inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		user, err = repo.CreateUser(ctx, tx, "https://idp.test", "sub-"+uuid.NewString(), nil)
		if err != nil {
			return err
		}
		acct, err = repo.CreateAccount(ctx, tx, user.ID, accounts.KindCustomer)
		return err
	}))
	return user.ID, acct.ID
}

// newAgent inserts a DRAFT agent (and the strategy it needs) so rows with an
// agents(id) foreign key (risk_decisions, trade_intents) can be written.
func newAgent(t *testing.T, userID accounts.UserID, accountID accounts.AccountID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	strategyID, agentID := uuid.New(), uuid.New()
	_, err := testDB.Exec(ctx, `INSERT INTO strategies (id, owner_account_id, owner_user_id, name, source_kind, status, created_by_actor_type, created_by_actor_id)
		VALUES ($1,$2,$3,$4,'TYPESCRIPT_SDK','ACTIVE','OPERATOR','op-test')`, strategyID, accountID, userID, "strategy-"+uuid.NewString())
	require.NoError(t, err)
	_, err = testDB.Exec(ctx, `INSERT INTO agents (id, account_id, strategy_id, name, stage, state, created_by_actor_type, created_by_actor_id)
		VALUES ($1,$2,$3,'agent-test','DRAFT','DRAFT','OPERATOR','op-test')`, agentID, accountID, strategyID)
	require.NoError(t, err)
	return agentID
}

func recordPolicy(t *testing.T, s *Store, rec PolicyRecord) error {
	t.Helper()
	return inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := s.RecordPolicy(ctx, tx, rec)
		return err
	})
}

func TestIntegration_RecordPolicy_Authority(t *testing.T) {
	requireEnv(t)
	s := NewStore()
	era := isolatedEra(t)
	expires := era.Add(time.Hour)
	rules := fixtureRules(t, "global_test")
	acct := uuid.NewString()
	base := PolicyRecord{Scope: ScopeGlobal, Version: "v-" + uuid.NewString(), Rules: rules, EffectiveAt: era, ExpiresAt: &expires, ActorID: "op-1", Reason: "test"}

	rec := base
	rec.ActorType = security.ActorAgent
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(recordPolicy(t, s, rec)), "AGENT actor")

	err := inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		rec := base
		rec.ActorType = security.ActorOperator
		ctx = security.WithPrincipal(ctx, security.AgentPrincipal("agent-1", acct))
		_, err := s.RecordPolicy(ctx, tx, rec)
		return err
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "AGENT principal")

	rec = base
	rec.ActorType = security.ActorUser
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(recordPolicy(t, s, rec)), "USER cannot write GLOBAL")

	rec = base
	rec.ActorType = security.ActorOperator
	rec.Rules = deleteRule(t, rules, "max_fee_bps")
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(recordPolicy(t, s, rec)), "GLOBAL must be complete")

	rec = base
	rec.ActorType = security.ActorOperator
	rec.Rules = mutateRules(t, rules, []string{"max_leverage"}, 5)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(recordPolicy(t, s, rec)), "unknown key fails closed")

	rec = base
	rec.ActorType = security.ActorOperator
	rec.Scope = ScopeAccount
	rec.ScopeID = "not-a-uuid"
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(recordPolicy(t, s, rec)), "ACCOUNT scope id must be a uuid")

	rec = base
	rec.ActorType = security.ActorUser
	rec.Scope = ScopeAccount
	rec.ScopeID = acct
	rec.Rules = json.RawMessage(`{"max_single_trade_usd": "50.00"}`)
	assert.NoError(t, recordPolicy(t, s, rec), "a USER may tighten their own ACCOUNT scope (partial document)")

	rec.Scope = ScopeAgent
	rec.Version = "v-" + uuid.NewString()
	rec.ActorType = security.ActorOperator
	assert.NoError(t, recordPolicy(t, s, rec), "AGENT scope written by a non-agent actor")
}

func TestIntegration_EffectivePolicy_Composes(t *testing.T) {
	requireEnv(t)
	s := NewStore()
	era := isolatedEra(t)
	expires := era.Add(2 * time.Hour)
	accountID, agentID := uuid.NewString(), uuid.NewString()
	gv, av, agv := "g-"+uuid.NewString(), "a-"+uuid.NewString(), "ag-"+uuid.NewString()

	require.NoError(t, recordPolicy(t, s, PolicyRecord{Scope: ScopeGlobal, Version: gv, Rules: fixtureRules(t, "global_test"), EffectiveAt: era, ExpiresAt: &expires, ActorType: security.ActorOperator, ActorID: "op-1", Reason: "global"}))
	require.NoError(t, recordPolicy(t, s, PolicyRecord{Scope: ScopeAccount, ScopeID: accountID, Version: av, Rules: json.RawMessage(`{"max_single_trade_usd": "300.00", "allowed_venues": ["JUPITER", "RAYDIUM"], "max_position_usd": "99999.00"}`), EffectiveAt: era, ExpiresAt: &expires, ActorType: security.ActorUser, ActorID: "user-1", Reason: "self"}))
	require.NoError(t, recordPolicy(t, s, PolicyRecord{Scope: ScopeAgent, ScopeID: agentID, Version: agv, Rules: json.RawMessage(`{"max_orders_per_hour": 5, "allowed_venues": ["RAYDIUM"]}`), EffectiveAt: era.Add(time.Minute), ExpiresAt: &expires, ActorType: security.ActorOperator, ActorID: "op-2", Reason: "agent"}))

	ctx := context.Background()
	at := era.Add(30 * time.Minute)
	p, ref, err := s.EffectivePolicy(ctx, testDB, accountID, agentID, at)
	require.NoError(t, err)
	assert.Equal(t, gv, ref.GlobalVersion)
	assert.Equal(t, av, ref.AccountVersion)
	assert.Equal(t, agv, ref.AgentVersion)
	assert.Equal(t, "GLOBAL="+gv+";ACCOUNT="+av+";AGENT="+agv, ref.Version)
	assert.Equal(t, p.Version, ref.Version)
	assert.Equal(t, p.Hash(), ref.Hash)
	assert.Equal(t, usd(t, "300.00"), *p.MaxSingleTradeUSD)
	assert.Equal(t, usd(t, "5000.00"), *p.MaxPositionUSD, "an ACCOUNT row cannot loosen the GLOBAL limit")
	assert.Equal(t, 5, *p.MaxOrdersPerHour)
	assert.Equal(t, []string{}, p.AllowedVenues, "JUPITER ∩ {JUPITER,RAYDIUM} ∩ {RAYDIUM} = ∅")
	require.NoError(t, p.Validate(ScopeGlobal))

	p, ref, err = s.EffectivePolicy(ctx, testDB, accountID, "", at)
	require.NoError(t, err)
	assert.Equal(t, "GLOBAL="+gv+";ACCOUNT="+av, ref.Version)
	assert.Equal(t, []string{"JUPITER"}, p.AllowedVenues)
	assert.Equal(t, 30, *p.MaxOrdersPerHour)

	p, ref, err = s.EffectivePolicy(ctx, testDB, uuid.NewString(), uuid.NewString(), at)
	require.NoError(t, err)
	assert.Equal(t, "GLOBAL="+gv, ref.Version, "unknown account and agent fall back to GLOBAL")
	assert.Equal(t, usd(t, "1000.00"), *p.MaxSingleTradeUSD)

	_, _, err = s.EffectivePolicy(ctx, testDB, accountID, agentID, era.Add(-time.Second))
	assert.ErrorIs(t, err, ErrNoPolicy, "no GLOBAL effective before the era")
	_, _, err = s.EffectivePolicy(ctx, testDB, accountID, agentID, expires)
	assert.ErrorIs(t, err, ErrNoPolicy, "expired at expires_at")

	d := Evaluate(Policy{}, baseInput(t))
	assert.Equal(t, []string{ReasonPolicyMissing}, d.ReasonCodes, "callers evaluate the zero policy on ErrNoPolicy")
}

func TestIntegration_EffectivePolicy_TamperedHashFailsClosed(t *testing.T) {
	requireEnv(t)
	s := NewStore()
	era := isolatedEra(t)
	expires := era.Add(time.Hour)
	p := MustParsePolicy(fixtureRules(t, "global_test"))
	canon, err := p.CanonicalJSON()
	require.NoError(t, err)
	_, err = testDB.Exec(context.Background(), `INSERT INTO risk_policies
		(id, version, scope, scope_id, rules, rules_hash, effective_at, expires_at, created_by_actor_type, created_by_actor_id, reason)
		VALUES ($1,$2,'GLOBAL','*',$3,$4,$5,$6,'OPERATOR','op-x','tamper test')`,
		uuid.New(), "tampered-"+uuid.NewString(), canon, []byte("not-the-hash"), era, expires)
	require.NoError(t, err)
	_, _, err = s.EffectivePolicy(context.Background(), testDB, "", "", era.Add(time.Minute))
	require.Error(t, err)
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(err))
}

func TestIntegration_RecordDecision_RoundTrip(t *testing.T) {
	requireEnv(t)
	s := NewStore()
	userID, accountID := newAccount(t)
	p := loadPolicyFixture(t, "global_test")
	in := baseInput(t)
	in.Stage = StageFinal
	in.Intent.ID = ""
	in.Intent.AccountID = accountID.String()
	in.Intent.AgentID = newAgent(t, userID, accountID).String()
	in.Intent.NotionalUSD = usd(t, "1500.00")
	in.KillSwitches = []KillSwitch{{Kind: KillVenueDisable, Scope: "JUPITER"}}
	in.Now = time.Date(2026, 9, 5, 12, 0, 0, 654321000, time.UTC)
	d := Evaluate(p, in)
	require.Equal(t, Reject, d.Verdict)
	require.Equal(t, []string{ReasonKillSwitch, ReasonMaxSingleTrade}, d.ReasonCodes)

	var decisionID DecisionID
	require.NoError(t, inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		decisionID, err = s.RecordDecision(ctx, tx, in, d, "corr-risk-1")
		return err
	}))

	var (
		intentID, quoteID       *uuid.UUID
		gotAccount, gotAgent    uuid.UUID
		stage, version, verdict string
		policyHash              []byte
		accountSnap, marketSnap []byte
		constraints             []byte
		codes                   []string
		evaluator               string
		evaluatedAt             time.Time
		correlation             *string
	)
	require.NoError(t, testDB.QueryRow(context.Background(), `SELECT intent_id, account_id, agent_id, stage, policy_version, policy_hash,
		account_snapshot, market_snapshot, quote_id, decision, reason_codes, resulting_constraints, evaluator_version, evaluated_at, correlation_id
		FROM risk_decisions WHERE id = $1`, decisionID).
		Scan(&intentID, &gotAccount, &gotAgent, &stage, &version, &policyHash, &accountSnap, &marketSnap, &quoteID, &verdict, &codes, &constraints, &evaluator, &evaluatedAt, &correlation))
	assert.Nil(t, intentID)
	assert.Equal(t, accountID.String(), gotAccount.String())
	assert.Equal(t, in.Intent.AgentID, gotAgent.String())
	assert.Equal(t, "FINAL", stage)
	assert.Equal(t, p.Version, version)
	assert.Equal(t, d.PolicyHash, hex.EncodeToString(policyHash))
	require.NotNil(t, quoteID)
	assert.Equal(t, in.Market.Quote.ID, quoteID.String())
	assert.Equal(t, "REJECT", verdict)
	assert.Equal(t, d.ReasonCodes, codes)
	assert.Equal(t, EvaluatorVersion, evaluator)
	assert.True(t, evaluatedAt.Equal(in.Now), "%s vs %s", evaluatedAt, in.Now)
	require.NotNil(t, correlation)
	assert.Equal(t, "corr-risk-1", *correlation)

	var acct map[string]any
	require.NoError(t, json.Unmarshal(accountSnap, &acct))
	assert.Equal(t, "FINAL", acct["stage"])
	assert.Equal(t, "1500.00", jsonObject(t, acct, "intent")["notional_usd"])
	assert.Equal(t, "8000.00", jsonObject(t, acct, "account")["available_now_usd"])
	assert.Len(t, acct["kill_switches"], 1)
	var market map[string]any
	require.NoError(t, json.Unmarshal(marketSnap, &market))
	assert.Equal(t, "250000.00", jsonObject(t, market, "market")["liquidity_usd"])
	var cons map[string]any
	require.NoError(t, json.Unmarshal(constraints, &cons))
	assert.Equal(t, d.Hash, cons["decision_hash"])
	assert.Equal(t, "NEW_RISK", cons["action_class"])
	assert.Equal(t, "1000.00", jsonObject(t, cons, "constraints")["max_notional_usd"])

	tampered := d
	tampered.Verdict = Allow
	err := inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := s.RecordDecision(ctx, tx, in, tampered, "")
		return err
	})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "a modified decision is refused")

	missing := Evaluate(Policy{}, in)
	require.NoError(t, inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := s.RecordDecision(ctx, tx, in, missing, "")
		return err
	}), "a POLICY_MISSING decision persists with an empty policy hash")
}

// TestIntegration_CountOrders_FromPersistedIntents proves the order-rate
// budget comes from Postgres (PART 181): statuses, window edges and the
// agent filter are all evaluated against trade_intents rows.
func TestIntegration_CountOrders_FromPersistedIntents(t *testing.T) {
	requireEnv(t)
	s := NewStore()
	userID, accountID := newAccount(t)
	_, otherAccount := newAccount(t)
	ctx := context.Background()

	asset, err := assets.NewRepository().Create(ctx, testDB, assets.Asset{
		Chain: "solana-test-" + uuid.NewString(), MintAddress: "native", Kind: assets.KindNative, ValueDomain: valuedomain.SelfCustodialCrypto, Symbol: "SOL", Name: "Solana",
		Decimals: 9, RiskClass: assets.RiskMajor, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	exposureID, instrumentID := uuid.New(), uuid.New()
	_, err = testDB.Exec(ctx, `INSERT INTO economic_exposures (id, kind, description, underlying_asset_id) VALUES ($1,'ASSET_PRICE','SOL price',$2)`, exposureID, asset.ID)
	require.NoError(t, err)
	_, err = testDB.Exec(ctx, `INSERT INTO instruments (id, type, canonical_name, exposure_id, base_asset_id, quote_asset_id, settlement_asset_id, risk_class, status, active_from)
		VALUES ($1,'SPOT_PAIR',$2,$3,$4,$4,$4,'MAJOR','ACTIVE',now())`, instrumentID, "SOL/SOL-"+uuid.NewString(), exposureID, asset.ID)
	require.NoError(t, err)

	at := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	agentA, agentB := newAgent(t, userID, accountID), newAgent(t, userID, accountID)
	insert := func(acct accounts.AccountID, agent *uuid.UUID, receivedAt time.Time, status string) {
		t.Helper()
		var agentID any
		if agent != nil {
			agentID = *agent
		}
		// content_hash has been NOT NULL since migration 00605. This fixture
		// writes trade_intents directly rather than through internal/intent, so
		// it has to supply the column itself; the equivalent fixture in
		// internal/capital was updated when 00605 landed and this one was not,
		// which left risk.CountOrders — the input to the order-rate limit —
		// with no DB-backed test that could run. The value only has to be a
		// stable 32-byte digest of the row's identity, not the real canonical
		// hash, because nothing here reads it back.
		_, err := testDB.Exec(ctx, `INSERT INTO trade_intents
			(id, account_id, actor_type, actor_id, agent_id, action, instrument_id, notional_usd_minor, constraints, requested_at, received_at, idempotency_key, correlation_id, mode, status, content_hash)
			VALUES ($1,$2,'USER','u-1',$3,'ACQUIRE_NOTIONAL',$4,10000,'{}',$5,$5,$6,'corr','PAPER',$7,sha256(convert_to($6,'UTF8')))`,
			uuid.New(), acct, agentID, instrumentID, receivedAt, uuid.NewString(), status)
		require.NoError(t, err)
	}
	insert(accountID, &agentA, at.Add(-59*time.Minute), "RECEIVED")
	insert(accountID, &agentA, at.Add(-30*time.Minute), "COMPLETED")
	insert(accountID, &agentA, at.Add(-10*time.Minute), "REJECTED")     // not counted
	insert(accountID, &agentA, at.Add(-5*time.Minute), "NO_VALID_PLAN") // not counted
	insert(accountID, &agentA, at.Add(-time.Hour), "RECEIVED")          // window edge: excluded (received_at > at-window)
	insert(accountID, &agentA, at, "RECEIVED")                          // window edge: included (received_at <= at)
	insert(accountID, &agentA, at.Add(time.Second), "RECEIVED")         // future: excluded
	insert(accountID, &agentB, at.Add(-20*time.Minute), "CANCELLED")    // counted for the account, not for agent A
	insert(accountID, nil, at.Add(-15*time.Minute), "EXECUTING")        // manual intent: counted for the account
	insert(otherAccount, &agentA, at.Add(-15*time.Minute), "RECEIVED")  // other account: never counted

	n, err := s.CountOrders(ctx, testDB, accountID.String(), agentA.String(), time.Hour, at)
	require.NoError(t, err)
	assert.Equal(t, 3, n, "agent A in the last hour")

	n, err = s.CountOrders(ctx, testDB, accountID.String(), "", time.Hour, at)
	require.NoError(t, err)
	assert.Equal(t, 5, n, "whole account in the last hour")

	n, err = s.CountOrders(ctx, testDB, accountID.String(), agentA.String(), 20*time.Minute, at)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "shorter window")

	_, err = s.CountOrders(ctx, testDB, "nope", "", time.Hour, at)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = s.CountOrders(ctx, testDB, accountID.String(), "", 0, at)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	// The persisted count drives the kernel: 3 orders against a 3/hour policy rejects.
	p := loadPolicyFixture(t, "global_test")
	three := 3
	p.MaxOrdersPerHour = &three
	in := baseInput(t)
	in.Account.OrdersInWindow = n + 2
	d := Evaluate(p, in)
	assert.Contains(t, d.ReasonCodes, ReasonOrderRate)
}
