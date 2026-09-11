//go:build integration

package prediction

import (
	"context"
	"fmt"
	"math/big"
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
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// These tests need an isolated database (go run ./scripts/testdb -name agentrt):
// predictions, outcomes and calibration snapshots are append-only, cp_app holds
// no DELETE grant on any of them, and that is exactly the property under test.
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
		fmt.Fprintln(os.Stderr, "prediction integration: migrate up:", err)
		return 1
	}
	var err error
	testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "prediction-itest", MaxConns: 8})
	if err != nil {
		fmt.Fprintln(os.Stderr, "prediction integration: open pool:", err)
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
	t   *testing.T
	clk *clock.Fake

	userID       string
	accountID    string
	strategyID   string
	versionID    string
	agentID      string
	runID        string
	instrumentID instruments.InstrumentID
	baseAssetID  string
	quoteAssetID string
}

func newUUID() string { return id.New[struct{}]().String() }

func bytes32(seed string) []byte {
	out := make([]byte, 32)
	copy(out, seed)
	return out
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireEnv(t)
	ctx := context.Background()
	f := &fixture{
		t:            t,
		clk:          clock.NewFake(time.Now().UTC().Truncate(time.Microsecond)),
		userID:       newUUID(),
		accountID:    newUUID(),
		strategyID:   newUUID(),
		versionID:    newUUID(),
		agentID:      newUUID(),
		runID:        newUUID(),
		baseAssetID:  newUUID(),
		quoteAssetID: newUUID(),
	}
	f.instrumentID = instruments.NewInstrumentID()
	suffix := f.accountID
	exposureID := newUUID()

	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := testDB.Exec(ctx, sql, args...)
		require.NoError(t, err, sql)
	}
	exec(`INSERT INTO users (id, idp_issuer, idp_subject, status) VALUES ($1, 'pitest', $2, 'ACTIVE')`,
		f.userID, "owner-"+suffix)
	exec(`INSERT INTO accounts (id, owner_user_id, kind, status) VALUES ($1, $2, 'CUSTOMER', 'ACTIVE')`,
		f.accountID, f.userID)
	exec(`INSERT INTO assets (id, chain, mint_address, kind, symbol, name, decimals, risk_class, status, value_domain)
	      VALUES ($1, 'solana-devnet', $2, 'SPL_TOKEN', 'SOL', 'Solana', 9, 'MAJOR', 'ACTIVE', 'SELF_CUSTODIAL_CRYPTO')`,
		f.baseAssetID, "base-"+suffix)
	exec(`INSERT INTO assets (id, chain, mint_address, kind, symbol, name, decimals, is_stablecoin, peg_currency, risk_class, status, value_domain)
	      VALUES ($1, 'solana-devnet', $2, 'SPL_TOKEN', 'USDC', 'USD Coin', 6, true, 'USD', 'SETTLEMENT', 'ACTIVE', 'SELF_CUSTODIAL_CRYPTO')`,
		f.quoteAssetID, "quote-"+suffix)
	exec(`INSERT INTO economic_exposures (id, kind, description, underlying_asset_id)
	      VALUES ($1, 'ASSET_PRICE', 'SOL price', $2)`, exposureID, f.baseAssetID)
	exec(`INSERT INTO instruments (id, type, canonical_name, exposure_id, base_asset_id, quote_asset_id,
	          settlement_asset_id, risk_class, status, active_from)
	      VALUES ($1, 'SPOT_PAIR', $2, $3, $4, $5, $5, 'MAJOR', 'ACTIVE', now())`,
		f.instrumentID, "SOL-USDC-"+suffix, exposureID, f.baseAssetID, f.quoteAssetID)
	exec(`INSERT INTO strategies (id, owner_account_id, owner_user_id, name, source_kind, status,
	          created_by_actor_type, created_by_actor_id)
	      VALUES ($1, $2, $3, $4, 'NATURAL_LANGUAGE', 'ACTIVE', 'USER', $5)`,
		f.strategyID, f.accountID, f.userID, "strategy-"+suffix, f.userID)
	exec(`INSERT INTO strategy_versions (id, strategy_id, version, schema_version, ir, ir_hash, effect_set,
	          status, source_kind, source_hash, compiler_version, risk_policy_version, risk_policy_hash,
	          model_budget, data_budget, envelope_requirements, human_readable, built_at,
	          accepted_by_user_id, accepted_at)
	      VALUES ($1, $2, 1, 1, '{}'::jsonb, $3, ARRAY['READ_MARKET_DATA','COMMIT_PREDICTION','CREATE_TRADE_INTENT'],
	              'ACCEPTED', 'NATURAL_LANGUAGE', $4, 'c/1', 'risk/v1', $5,
	              '{}'::jsonb, '{}'::jsonb, '{}'::jsonb, 'rendered', now(), $6, now())`,
		f.versionID, f.strategyID, bytes32("ir"), bytes32("src"), bytes32("risk"), f.userID)
	exec(`INSERT INTO agents (id, account_id, strategy_id, strategy_version_id, name, stage, state, mode,
	          version, created_by_actor_type, created_by_actor_id)
	      VALUES ($1, $2, $3, $4, 'prediction itest', 'SHADOW', 'SHADOW', 'SHADOW', 1, 'OPERATOR', 'op')`,
		f.agentID, f.accountID, f.strategyID, f.versionID)
	f.newRun(t, f.runID)
	return f
}

func (f *fixture) newRun(t *testing.T, runID string) {
	t.Helper()
	_, err := testDB.Exec(context.Background(),
		`INSERT INTO agent_runs (id, agent_id, agent_version, strategy_version_id, account_id, mode,
		          trigger_name, trigger_kind, trigger_dedup_key, decision_time, status, correlation_id)
		 VALUES ($1, $2, 1, $3, $4, 'SHADOW', 'tick', 'ON_INTERVAL', $5, now(), 'STARTED', $6)`,
		runID, f.agentID, f.versionID, f.accountID, bytes32(runID), "corr-"+runID)
	require.NoError(t, err)
}

// agentCtx is the only principal that may commit a prediction.
func (f *fixture) agentCtx() context.Context {
	return security.WithPrincipal(context.Background(), security.AgentPrincipal(f.agentID, f.accountID))
}

func (f *fixture) ledger(t *testing.T) *PGLedger {
	t.Helper()
	l, err := NewLedger(f.clk)
	require.NoError(t, err)
	return l
}

func (f *fixture) draft(t *testing.T, action string, horizon time.Duration, probability string) Prediction {
	t.Helper()
	return Prediction{
		AgentID: f.agentID, AgentVersion: 1, RunID: f.runID, ActionName: action,
		StrategyVersionID: f.versionID, AccountID: f.accountID, Mode: ModeShadow,
		InstrumentID: f.instrumentID, Horizon: horizon,
		Direction: DirectionUp, ProbabilityDirection: prob(t, probability),
		ExpectedReturnBPS: money.BPS(120), DownsideProbability: prob(t, "0.3"),
		MaxDownsideBPS: money.BPS(200), Confidence: prob(t, "0.55"),
		InformationSetHash:  InformationSetHash([]InformationItem{{Dependency: "price", InvocationID: "inv-1"}}),
		DecisionAvailableAt: f.clk.Now().Add(-time.Minute),
	}
}

func (f *fixture) commit(t *testing.T, p Prediction) Prediction {
	t.Helper()
	l := f.ledger(t)
	var out Prediction
	require.NoError(t, testDB.InTx(f.agentCtx(), db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 0},
		func(ctx context.Context, tx pgx.Tx) error {
			res, err := l.Commit(ctx, tx, p)
			out = res
			return err
		}))
	return out
}

// ------------------------------------------------------------ immutability --

// TestPredictionIsImmutable is the property that makes a prediction worth
// anything: once written, it cannot be amended or deleted, so it cannot be
// rewritten after the outcome is known.
func TestPredictionIsImmutable(t *testing.T) {
	f := newFixture(t)
	p := f.commit(t, f.draft(t, "enter", time.Hour, "0.62"))
	require.False(t, p.ID.IsZero())

	for name, sql := range map[string]string{
		"probability": `UPDATE predictions SET probability_direction = 0.999999 WHERE id = $1`,
		"direction":   `UPDATE predictions SET direction = 'DOWN' WHERE id = $1`,
		"expected":    `UPDATE predictions SET expected_return_bps = 9999 WHERE id = $1`,
		"committed":   `UPDATE predictions SET committed_at = now() + interval '1 day' WHERE id = $1`,
		"info hash":   `UPDATE predictions SET information_set_hash = sha256('other') WHERE id = $1`,
		"rationale":   `UPDATE predictions SET rationale = '{"rewritten":true}'::jsonb WHERE id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			err := testDB.InTx(context.Background(), db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 0},
				func(ctx context.Context, tx pgx.Tx) error {
					_, err := tx.Exec(ctx, sql, p.ID)
					return err
				})
			require.Errorf(t, err, "amending the %s of a committed prediction must be refused", name)
		})
	}
	t.Run("delete", func(t *testing.T) {
		err := testDB.InTx(context.Background(), db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 0},
			func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `DELETE FROM predictions WHERE id = $1`, p.ID)
				return err
			})
		require.Error(t, err, "a prediction is never deleted")
	})

	// And it still reads back exactly as committed.
	got, err := f.ledger(t).Get(context.Background(), testDB, p.ID)
	require.NoError(t, err)
	assert.Equal(t, "0.620000", got.ProbabilityDirection.String())
	assert.Equal(t, DirectionUp, got.Direction)
	assert.Equal(t, p.InformationSetHash, got.InformationSetHash)
}

// TestCommitStampsTheClockNotTheCaller: a caller cannot backdate a prediction
// to make it look as if it preceded a trade it actually followed.
func TestCommitStampsTheClockNotTheCaller(t *testing.T) {
	f := newFixture(t)
	draft := f.draft(t, "enter", time.Hour, "0.5")
	draft.CommittedAt = f.clk.Now().Add(-72 * time.Hour) // an attempted backdate
	p := f.commit(t, draft)
	assert.Equal(t, f.clk.Now(), p.CommittedAt, "committed_at comes from the platform clock")

	stored, err := f.ledger(t).Get(context.Background(), testDB, p.ID)
	require.NoError(t, err)
	assert.WithinDuration(t, f.clk.Now(), stored.CommittedAt, time.Millisecond)
}

func TestCommitRefusesAnInformationSetFromTheFuture(t *testing.T) {
	f := newFixture(t)
	draft := f.draft(t, "enter", time.Hour, "0.5")
	draft.DecisionAvailableAt = f.clk.Now().Add(time.Hour)
	l := f.ledger(t)
	err := testDB.InTx(f.agentCtx(), db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 0},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := l.Commit(ctx, tx, draft)
			return err
		})
	require.Error(t, err, "a prediction cannot rest on data it did not have")
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestCommitRequiresTheAgentPermission(t *testing.T) {
	f := newFixture(t)
	l := f.ledger(t)
	human := security.WithPrincipal(context.Background(), security.Principal{
		SubjectID: f.userID, ActorType: security.ActorUser,
		Roles: []security.Role{security.RoleCustomer}, AccountIDs: []string{f.accountID},
		AuthTime: f.clk.Now(), AMR: []string{"mfa"},
	})
	err := testDB.InTx(human, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 0},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := l.Commit(ctx, tx, f.draft(t, "enter", time.Hour, "0.5"))
			return err
		})
	require.Error(t, err, "a person cannot file a prediction on the agent path")
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	anonymous := context.Background()
	err = testDB.InTx(anonymous, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 0},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := l.Commit(ctx, tx, f.draft(t, "enter2", time.Hour, "0.5"))
			return err
		})
	require.Error(t, err)
}

// TestCommitIsIdempotentPerRunAction: a retry after a crash returns the
// original forecast rather than writing a second one.
func TestCommitIsIdempotentPerRunAction(t *testing.T) {
	f := newFixture(t)
	first := f.commit(t, f.draft(t, "enter", time.Hour, "0.62"))
	f.clk.Advance(time.Minute)
	// A retry with a different probability must not overwrite or duplicate.
	retry := f.draft(t, "enter", time.Hour, "0.99")
	second := f.commit(t, retry)

	assert.Equal(t, first.ID, second.ID, "the same run and action commit exactly one prediction")
	assert.Equal(t, "0.620000", second.ProbabilityDirection.String(),
		"the original forecast stands; a retry cannot revise it")

	var n int
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT count(*) FROM predictions WHERE agent_run_id = $1`, f.runID).Scan(&n))
	assert.Equal(t, 1, n)
}

// ------------------------------------------- prediction predates execution --

// TestIntentRequiresAPredictionThatPredatesIt exercises the AG001, AG002 and
// AG003 triggers directly: no application code can bypass them.
func TestIntentRequiresAPredictionThatPredatesIt(t *testing.T) {
	f := newFixture(t)
	p := f.commit(t, f.draft(t, "enter", time.Hour, "0.62"))

	insertIntent := func(t *testing.T, requestedAt time.Time, predictionID, agentID, versionID, accountID, mode string) error {
		t.Helper()
		return testDB.InTx(context.Background(), db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 0},
			func(ctx context.Context, tx pgx.Tx) error {
				key := newUUID()
				_, err := tx.Exec(ctx, `
INSERT INTO trade_intents (id, account_id, actor_type, actor_id, agent_id, strategy_version_id, prediction_id,
    action, instrument_id, notional_usd_minor, constraints, requested_at, idempotency_key, correlation_id,
    mode, status, content_hash)
VALUES ($1, $2, 'AGENT', $3, $4, $5, $6, 'ACQUIRE_NOTIONAL', $7, 1000, '{}'::jsonb, $8, $9, 'corr', $10,
        'RECEIVED', $11)`,
					newUUID(), accountID, agentID, nullable(agentID), nullable(versionID), nullable(predictionID),
					f.instrumentID, requestedAt, key, mode, bytes32(key))
				return err
			})
	}

	t.Run("an agent intent without a prediction is refused", func(t *testing.T) {
		err := insertIntent(t, f.clk.Now(), "", f.agentID, f.versionID, f.accountID, "SHADOW")
		require.Error(t, err)
		assert.Equal(t, "AG001", db.SQLState(err), "expected AG001, got %v", err)
	})

	t.Run("a prediction committed after the intent is refused", func(t *testing.T) {
		err := insertIntent(t, p.CommittedAt.Add(-time.Second), p.ID.String(), f.agentID, f.versionID, f.accountID, "SHADOW")
		require.Error(t, err)
		assert.Equal(t, "AG002", db.SQLState(err), "expected AG002, got %v", err)
	})

	t.Run("a prediction of a different mode is refused", func(t *testing.T) {
		err := insertIntent(t, p.CommittedAt.Add(time.Second), p.ID.String(), f.agentID, f.versionID, f.accountID, "LIVE")
		require.Error(t, err)
		assert.Equal(t, "AG003", db.SQLState(err), "expected AG003, got %v", err)
	})

	t.Run("a prediction of a different agent is refused", func(t *testing.T) {
		other := newFixture(t)
		err := insertIntent(t, p.CommittedAt.Add(time.Second), p.ID.String(), other.agentID, f.versionID, f.accountID, "SHADOW")
		require.Error(t, err)
		assert.Equal(t, "AG003", db.SQLState(err), "expected AG003, got %v", err)
	})

	t.Run("a matching prediction that predates the intent is accepted", func(t *testing.T) {
		require.NoError(t, insertIntent(t, p.CommittedAt.Add(time.Second), p.ID.String(),
			f.agentID, f.versionID, f.accountID, "SHADOW"))
	})
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ---------------------------------------------------------------- outcomes --

func TestDueForResolutionExcludesOpenHorizons(t *testing.T) {
	f := newFixture(t)
	short := f.commit(t, f.draft(t, "short", time.Minute, "0.6"))
	longRunID := newUUID()
	f.newRun(t, longRunID)
	longDraft := f.draft(t, "long", 24*time.Hour, "0.6")
	longDraft.RunID = longRunID
	long := f.commit(t, longDraft)

	l := f.ledger(t)
	due, err := l.DueForResolution(context.Background(), testDB, f.clk.Now().Add(30*time.Second), 100)
	require.NoError(t, err)
	assert.False(t, containsPrediction(due, short.ID), "a prediction inside its horizon is never due")
	assert.False(t, containsPrediction(due, long.ID))

	due, err = l.DueForResolution(context.Background(), testDB, f.clk.Now().Add(2*time.Minute), 100)
	require.NoError(t, err)
	assert.True(t, containsPrediction(due, short.ID), "an elapsed horizon becomes due")
	assert.False(t, containsPrediction(due, long.ID), "the long horizon is still open")
}

func containsPrediction(ps []Prediction, want PredictionID) bool {
	for _, p := range ps {
		if p.ID == want {
			return true
		}
	}
	return false
}

func TestResolverRefusesBeforeTheHorizonEnds(t *testing.T) {
	f := newFixture(t)
	p := f.commit(t, f.draft(t, "enter", time.Hour, "0.6"))
	r, err := NewResolver(f.clk, NewPriceReader(), time.Hour)
	require.NoError(t, err)
	_, err = r.Resolve(context.Background(), testDB, p)
	require.Error(t, err, "scoring inside the window would judge a prediction against its own future")
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))
}

// TestResolveScoresFromPointInTimePrices walks a full horizon: prices are
// written with explicit knowledge times, the resolver reads them under a
// cut-off, and the scores are exact.
func TestResolveScoresFromPointInTimePrices(t *testing.T) {
	f := newFixture(t)
	p := f.commit(t, f.draft(t, "enter", time.Hour, "0.75"))

	// 100.00 at commit, 101.00 at the horizon end, a 99.00 dip in between.
	f.writePrice(t, "10000", 2, p.CommittedAt.Add(-time.Second))
	f.writePrice(t, "9900", 2, p.CommittedAt.Add(30*time.Minute))
	f.writePrice(t, "10100", 2, p.HorizonEnd())
	// A price the platform only learned about after the horizon: it must not
	// influence the score at all.
	f.writePrice(t, "50000", 2, p.HorizonEnd().Add(time.Hour))

	f.clk.Set(p.HorizonEnd().Add(2 * time.Hour))
	r, err := NewResolver(f.clk, NewPriceReader(), time.Hour)
	require.NoError(t, err)
	o, err := r.Resolve(context.Background(), testDB, p)
	require.NoError(t, err)

	assert.Equal(t, DirectionUp, o.RealizedDirection)
	assert.Equal(t, money.BPS(100), o.RealizedReturnBPS, "100.00 -> 101.00 is exactly 100 bps, not a rounded float")
	assert.Equal(t, money.BPS(100), o.RealizedMaxDrawdownBPS, "the 99.00 dip is a 100 bps adverse excursion")
	require.NotNil(t, o.DirectionHit)
	assert.True(t, *o.DirectionHit)
	assert.Equal(t, "0.06250000", o.Brier.String(), "(1 - 0.75)^2")
	assert.Equal(t, "0.28768207", o.LogLoss.String(), "-ln(0.75)")
	assert.Equal(t, money.BPS(20), o.AbsReturnErrorBPS, "|100 realized - 120 expected|")
	assert.Equal(t, ResolverVersion, o.ResolverVersion)
	assert.NotEmpty(t, o.ValuationSource)
	assert.NotEmpty(t, o.PriceRefStart)
	assert.NotEmpty(t, o.PriceRefEnd)

	// The outcome is written once and never revised.
	l := f.ledger(t)
	require.NoError(t, testDB.InTx(context.Background(), db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 0},
		func(ctx context.Context, tx pgx.Tx) error { return l.RecordOutcome(ctx, tx, o) }))

	second := o
	second.ID = NewOutcomeID()
	second.RealizedReturnBPS = money.BPS(9999)
	err = testDB.InTx(context.Background(), db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 0},
		func(ctx context.Context, tx pgx.Tx) error { return l.RecordOutcome(ctx, tx, second) })
	require.Error(t, err, "a prediction is resolved once; a second resolution is a conflict")
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))

	err = testDB.InTx(context.Background(), db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 0},
		func(ctx context.Context, tx pgx.Tx) error {
			_, uerr := tx.Exec(ctx, `UPDATE prediction_outcomes SET realized_return_bps = 9999 WHERE id = $1`, o.ID)
			return uerr
		})
	require.Error(t, err, "an outcome is never amended")

	// And it is no longer due for resolution.
	due, err := l.DueForResolution(context.Background(), testDB, f.clk.Now(), 200)
	require.NoError(t, err)
	assert.False(t, containsPrediction(due, p.ID))
}

func (f *fixture) writePrice(t *testing.T, mantissa string, scale int, receivedAt time.Time) {
	t.Helper()
	_, err := testDB.Exec(context.Background(),
		`INSERT INTO asset_prices (id, asset_id, quote_asset_id, mantissa, scale, source, observed_at, received_at)
		 VALUES ($1, $2, $3, $4::numeric, $5, 'itest-oracle', $6, $6)`,
		newUUID(), f.baseAssetID, f.quoteAssetID, mantissa, scale, receivedAt)
	require.NoError(t, err)
}

func TestResolverRefusesWhenThereIsNoPrice(t *testing.T) {
	f := newFixture(t)
	p := f.commit(t, f.draft(t, "enter", time.Minute, "0.6"))
	f.clk.Set(p.HorizonEnd().Add(time.Minute))
	r, err := NewResolver(f.clk, NewPriceReader(), time.Hour)
	require.NoError(t, err)
	_, err = r.Resolve(context.Background(), testDB, p)
	require.Error(t, err, "an invented price would be worse than an unresolved prediction")
	assert.Equal(t, errs.CodeStaleMarketData, errs.CodeOf(err))
}

// ------------------------------------------------------------- calibration --

// TestCalibrationAggregatesResolvedOutcomes builds a small, exactly known
// sample and checks every reported number.
func TestCalibrationAggregatesResolvedOutcomes(t *testing.T) {
	f := newFixture(t)
	l := f.ledger(t)
	windowStart := f.clk.Now().Add(-time.Hour)

	// Four predictions at 0.60 (bucket 6), two of which come true, and two at
	// 0.90 (bucket 9), both of which come true.
	type spec struct {
		probability string
		hit         bool
	}
	specs := []spec{
		{"0.6", true},
		{"0.6", true},
		{"0.6", false},
		{"0.6", false},
		{"0.9", true},
		{"0.9", true},
	}
	for i, s := range specs {
		runID := newUUID()
		f.newRun(t, runID)
		draft := f.draft(t, fmt.Sprintf("action-%d", i), time.Minute, s.probability)
		draft.RunID = runID
		p := f.commit(t, draft)

		brier, err := Brier(p.ProbabilityDirection, s.hit)
		require.NoError(t, err)
		logLoss, err := LogLoss(p.ProbabilityDirection, s.hit, DefaultEpsilon())
		require.NoError(t, err)
		hit := s.hit
		realized := money.BPS(100)
		if !s.hit {
			realized = money.BPS(-100)
		}
		o := Outcome{
			ID: NewOutcomeID(), PredictionID: p.ID, StrategyVersionID: f.versionID, Mode: ModeShadow,
			HorizonEndAt: p.HorizonEnd(), ResolvedAt: p.HorizonEnd().Add(time.Minute),
			RealizedDirection: DirectionUp, RealizedReturnBPS: realized, RealizedMaxDrawdownBPS: money.BPS(10),
			DirectionHit: &hit, Brier: brier, LogLoss: logLoss,
			AbsReturnErrorBPS: money.BPS(20), RegimeLabel: UnlabelledRegime,
			ValuationSource: "itest-oracle", PriceRefStart: "a", PriceRefEnd: "b",
			ResolverVersion: ResolverVersion,
		}
		require.NoError(t, testDB.InTx(context.Background(), db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 0},
			func(ctx context.Context, tx pgx.Tx) error { return l.RecordOutcome(ctx, tx, o) }))
		f.clk.Advance(time.Second)
	}

	// Past every horizon and every resolution. These outcomes are stamped a
	// minute after a one-minute horizon, so at the loop's clock they were still
	// in the future: the fixture recorded outcomes the platform could not yet
	// have had. Nothing compared resolved_at to anything until F-120 bounded a
	// calibration by knowledge time, which is what surfaced it.
	f.clk.Advance(5 * time.Minute)

	c, err := NewCalibrator(f.clk)
	require.NoError(t, err)
	rows, err := c.Compute(context.Background(), testDB, CalibrationScope{
		StrategyVersionID: f.versionID, AgentID: f.agentID, Mode: ModeShadow,
		WindowStart: windowStart, WindowEnd: f.clk.Now().Add(time.Hour),
	}, f.clk.Now())
	require.NoError(t, err)
	require.Len(t, rows, 2, "only the two non-empty buckets are reported")

	byBucket := map[string]CalibrationRow{}
	for _, r := range rows {
		byBucket[r.BucketLower.String()] = r
	}
	six, ok := byBucket["0.600000"]
	require.True(t, ok, "the 0.6 bucket must be present, got %v", byBucket)
	assert.Equal(t, 4, six.NPredictions)
	assert.Equal(t, "0.60000000", six.MeanPredicted.String())
	assert.Equal(t, "0.50000000", six.RealizedFrequency.String(),
		"two of four came true; the bucket is over-confident and the number says so")
	// Brier: two hits at (1-0.6)^2 = 0.16, two misses at 0.6^2 = 0.36.
	assert.Equal(t, "0.26000000", six.BrierMean.String())
	assert.Equal(t, money.BPS(120), six.ExpectedReturnBPSMean)
	assert.Equal(t, money.BPS(0), six.RealizedReturnBPSMean, "two at +100 and two at -100 average to zero")
	assert.Equal(t, ComputerVersion, six.ComputerVersion)

	nine, ok := byBucket["0.900000"]
	require.True(t, ok)
	assert.Equal(t, 2, nine.NPredictions)
	assert.Equal(t, "0.90000000", nine.MeanPredicted.String())
	assert.Equal(t, "1.00000000", nine.RealizedFrequency.String())
	assert.Equal(t, "0.01000000", nine.BrierMean.String())
	assert.Equal(t, "0.10536052", nine.LogLossMean.String(), "-ln(0.9)")

	// Return and calibration stay separate columns: the 0.9 bucket is better
	// calibrated than the 0.6 bucket while both realized the same mean return
	// per correct call. Nothing in the row combines them.
	assert.NotEqual(t, nine.BrierMean.String(), nine.RealizedReturnBPSMean.String())

	require.NoError(t, testDB.InTx(context.Background(), db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 0},
		func(ctx context.Context, tx pgx.Tx) error { return c.Persist(ctx, tx, rows) }))

	var persisted int
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT count(*) FROM calibration_snapshots WHERE strategy_version_id = $1`, f.versionID).Scan(&persisted))
	assert.Equal(t, 2, persisted)

	// Snapshots are append-only: a recomputation adds rows, it never edits the
	// numbers an earlier promotion relied on.
	err = testDB.InTx(context.Background(), db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 0},
		func(ctx context.Context, tx pgx.Tx) error {
			_, uerr := tx.Exec(ctx, `UPDATE calibration_snapshots SET brier_mean = 0 WHERE id = $1`, rows[0].ID)
			return uerr
		})
	require.Error(t, err, "a calibration snapshot is never amended")
}

func TestCalibrationIgnoresUnresolvedPredictions(t *testing.T) {
	f := newFixture(t)
	_ = f.commit(t, f.draft(t, "open", time.Hour, "0.7"))
	c, err := NewCalibrator(f.clk)
	require.NoError(t, err)
	rows, err := c.Compute(context.Background(), testDB, CalibrationScope{
		StrategyVersionID: f.versionID, AgentID: f.agentID, Mode: ModeShadow,
		// The window is a day rather than an hour because resolveIt now moves the
		// clock to the resolution instant: a prediction with a one-hour horizon
		// cannot be resolved two seconds after it is committed, and the fixture
		// used to stamp resolved_at an hour in the future to pretend otherwise.
		// F-120's bound on knowledge time is what surfaced that.
		WindowStart: f.clk.Now().Add(-24 * time.Hour), WindowEnd: f.clk.Now().Add(time.Hour),
	}, f.clk.Now())
	require.NoError(t, err)
	assert.Empty(t, rows, "a bucket built from open horizons would describe the future")
}

func TestReturnInBPSIsExact(t *testing.T) {
	t.Parallel()
	cases := []struct {
		start, finish string
		scale         int32
		want          money.BPS
	}{
		{"10000", "10100", 2, 100},
		{"10000", "9900", 2, -100},
		{"10000", "10000", 2, 0},
		{"3", "1", 0, -6667},  // -2/3 rounded half-even at bps
		{"3", "10", 0, 23333}, // +7/3
	}
	for _, tc := range cases {
		start := PricePoint{Mantissa: mustBig(tc.start), Scale: tc.scale}
		finish := PricePoint{Mantissa: mustBig(tc.finish), Scale: tc.scale}
		got, err := returnInBPS(start, finish)
		require.NoError(t, err)
		assert.Equalf(t, tc.want, got, "%s -> %s", tc.start, tc.finish)
	}
}

func mustBig(s string) *big.Int {
	v, _ := new(big.Int).SetString(s, 10)
	return v
}
