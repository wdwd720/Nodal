//go:build integration

package predmarket

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"regexp"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/valuedomain"
	"github.com/nodal/controlplane/migrations"
)

// --- raw journal helper (bypasses internal/ledger and this package on purpose) --

type rawEntry struct {
	ref  ledger.AccountRef
	side ledger.Side
	qty  int64
}

func rawJournal(ctx context.Context, f *fixture, tx pgx.Tx, kind ledger.Kind, entries []rawEntry) error {
	txID := uuid.New()
	sum := sha256.Sum256([]byte(txID.String()))
	if _, err := tx.Exec(ctx, `INSERT INTO journal_transactions (id, kind, idempotency_key, reference_type, reference_id,
		effective_at, posted_by_actor_type, posted_by_actor_id, content_hash) VALUES ($1,$2,$3,'raw',$4,now(),'SYSTEM','raw',$5)`,
		txID, kind, "raw:"+txID.String(), txID.String(), sum[:]); err != nil {
		return err
	}
	for i, e := range entries {
		acct, err := f.ledger.EnsureAccount(ctx, tx, e.ref)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO journal_entries (id, transaction_id, seq, ledger_account_id, asset_id, side, quantity)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`, uuid.New(), txID, i, acct.ID, e.ref.AssetID, e.side, e.qty); err != nil {
			return err
		}
	}
	return nil
}

// TestIntegration_TheDatabaseRefusesAPredictionPostingThatDoesNotBalancePerParty
// bypasses both this package and internal/ledger. A "complete-set issuance"
// that moves the customer's collateral straight into the facility's escrow and
// the facility's shares straight into the customer's wallet balances per
// asset -- and per party it does not: the customer's books lose collateral
// they do not explain. The database refuses it at COMMIT (SM002), and a
// prediction posting that touches real value is refused too (SM001).
func TestIntegration_TheDatabaseRefusesAPredictionPostingThatDoesNotBalancePerParty(t *testing.T) {
	f := newFixture(t)
	a := f.customer(10)
	m := f.openMarket("YES", "NO")
	fac := m.Facility()
	cash := m.CollateralAssetID

	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		entries := []rawEntry{
			{ledger.CustomerAccount(a, ledger.CodeWallet, cash), ledger.Credit, 1_000_000},
			{fac.Account(ledger.CodePredictionEscrow, cash), ledger.Debit, 1_000_000},
		}
		for _, o := range m.Outcomes {
			entries = append(entries,
				rawEntry{ledger.CustomerAccount(a, ledger.CodeWallet, o.ShareAssetID), ledger.Debit, 1_000_000},
				rawEntry{fac.Account(ledger.CodePredictionSharesOutstanding, o.ShareAssetID), ledger.Credit, 1_000_000})
		}
		return rawJournal(ctx, f, tx, ledger.KindPredictionSetIssuance, entries)
	})
	require.Error(t, err)
	assert.Equal(t, "SM002", db.SQLState(err), "%v", err)

	// A payout of a real asset is not representable either.
	real, err := assets.NewRepository().Create(f.ctx, testDB, assets.Asset{
		Chain: "solana-itest-" + uuid.NewString(), MintAddress: uuid.NewString(), Kind: assets.KindSPLToken,
		ValueDomain: valuedomain.SelfCustodialCrypto, Symbol: "USDC", Name: "USDC", Decimals: 6,
		RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return rawJournal(ctx, f, tx, ledger.KindPredictionPayout, []rawEntry{
			{ledger.CustomerAccount(a, ledger.CodeWallet, real.ID), ledger.Debit, 5},
			{ledger.CustomerAccount(a, ledger.CodeTradingInflow, real.ID), ledger.Credit, 5},
		})
	})
	require.Error(t, err)
	assert.Equal(t, "SM001", db.SQLState(err), "%v", err)

	// Nothing moved; the market still reconciles.
	f.requireConserved(m, money.Quantity{})
	assert.Equal(t, units(10).String(), f.wallet(a, cash).String())
}

// TestIntegration_TheDatabaseBindsTheMarketLifecycle drives every guard of
// migration 00749 around this package.
func TestIntegration_TheDatabaseBindsTheMarketLifecycle(t *testing.T) {
	f := newFixture(t)
	a := f.customer(10)
	m := f.openMarket("YES", "NO")
	f.issue(m, a, 1)
	exec := func(sql string, args ...any) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, args...)
			return err
		})
	}

	// A status change without its transition row: AU001 at COMMIT.
	err := exec(`UPDATE prediction_markets SET status = 'CLOSED' WHERE id = $1`, m.ID)
	assert.Equal(t, "AU001", db.SQLState(err), "%v", err)

	// An edge outside the table: PM001, whatever rows accompany it.
	err = exec(`UPDATE prediction_markets SET status = 'SETTLED' WHERE id = $1`, m.ID)
	assert.Equal(t, "PM001", db.SQLState(err), "%v", err)

	// A transition row cannot even describe such an edge.
	err = exec(`INSERT INTO prediction_market_transitions (id, market_id, from_status, to_status, actor_type, actor_id, reason, occurred_at)
		VALUES ($1,$2,'OPEN','SETTLED','OPERATOR','x','bypass',now())`, uuid.New(), m.ID)
	assert.Equal(t, "prediction_market_transitions_edge_legal", db.ConstraintName(err), "%v", err)

	// A legal edge with its row, but no settlement record behind it: PM003.
	r := f.openMarket("YES", "NO")
	f.issue(r, a, 1)
	f.resolve(r, "YES")
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO prediction_market_transitions (id, market_id, from_status, to_status, actor_type, actor_id, reason, occurred_at)
			VALUES ($1,$2,'RESOLVED','SETTLED','OPERATOR',$3,'bypass',now())`, uuid.New(), r.ID, f.third.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE prediction_markets SET status = 'SETTLED' WHERE id = $1`, r.ID)
		return err
	})
	assert.Equal(t, "PM003", db.SQLState(err), "%v", err)

	// A settlement record that does not conserve the escrow.
	resolved := f.get(r)
	err = exec(`INSERT INTO prediction_settlements (id, market_id, kind, resolution_id, winning_outcome_index, outcome_count,
		unit_quantity, rounding_policy, escrow_before, total_paid, residual, holder_count, evidence_ref, evidence_sha256,
		settled_by_actor_type, settled_by_actor_id, settled_at)
		VALUES ($1,$2,'PAYOUT',$3,0,2,1000000,'EXACT',1000000,999999,0,1,'x',$4,'OPERATOR',$5,now())`,
		uuid.New(), r.ID, *resolved.CurrentResolutionID, make([]byte, 32), f.third.ID)
	assert.Equal(t, "prediction_settlements_conserves", db.ConstraintName(err), "%v", err)
	// A refund record must name floor(unit / N).
	err = exec(`INSERT INTO prediction_settlements (id, market_id, kind, outcome_count, unit_quantity, refund_rate_per_share,
		rounding_policy, escrow_before, total_paid, residual, holder_count, evidence_ref, evidence_sha256,
		settled_by_actor_type, settled_by_actor_id, settled_at)
		VALUES ($1,$2,'REFUND',2,1000000,500001,'FLOOR_PER_HOLDER',1000000,1000000,0,1,'x',$3,'OPERATOR',$4,now())`,
		uuid.New(), r.ID, make([]byte, 32), f.third.ID)
	assert.Equal(t, "prediction_settlements_refund_rate", db.ConstraintName(err), "%v", err)

	// The resolution pointer moves only on a transition into RESOLVED.
	err = exec(`UPDATE prediction_markets SET current_resolution_id = NULL WHERE id = $1`, r.ID)
	assert.Equal(t, "PM002", db.SQLState(err), "%v", err)

	// A live market's outcome set is frozen.
	share, err := assets.NewRepository().Create(f.ctx, testDB, assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: "PM.EXTRA", Name: "extra outcome", Decimals: 6, RiskClass: assets.RiskSpeculative, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	err = exec(`INSERT INTO prediction_outcomes_registry (id, market_id, outcome_index, symbol, share_asset_id, instrument_id)
		VALUES ($1,$2,2,'MAYBE',$3,$4)`, uuid.New(), m.ID, share.ID, m.InstrumentID)
	assert.Equal(t, "PM005", db.SQLState(err), "%v", err)

	// A market is born DRAFT, over a six-decimal SIMULATED collateral.
	insertMarket := func(status string, collateral assets.AssetID) error {
		return exec(`INSERT INTO prediction_markets (id, symbol, question, mode, facility_id, instrument_id, collateral_asset_id,
			outcome_count, unit_quantity, open_at, close_at, dispute_window_ms, oracle_source, oracle_public_key, definition_hash,
			status, created_by_actor_type, created_by_actor_id)
			VALUES ($1,$2,'q','SANDBOX',$3,$4,$5,2,1000000,now(),now() + interval '1 hour',0,'o',$6,$7,$8,'OPERATOR','x')`,
			uuid.New(), randomSymbol("RAW"), m.FacilityID, m.InstrumentID, collateral, make([]byte, 32), make([]byte, 32), status)
	}
	assert.Equal(t, "PM001", db.SQLState(insertMarket("OPEN", f.collateral)), "a market is born DRAFT")
	sevenDecimals, err := assets.NewRepository().Create(f.ctx, testDB, assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: "PM7", Name: "seven decimals", Decimals: 7, IsStablecoin: true, PegCurrency: "USD",
		RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	assert.Equal(t, "PM004", db.SQLState(insertMarket("DRAFT", sevenDecimals.ID)))

	// Identity is frozen twice over: the application role cannot name the
	// column, and the owner role is stopped by the guard.
	err = exec(`UPDATE prediction_markets SET close_at = close_at + interval '1 day' WHERE id = $1`, m.ID)
	assert.Equal(t, "42501", db.SQLState(err), "%v", err)
	owner, err := pgx.Connect(f.ctx, testMigrateURL)
	require.NoError(t, err)
	defer owner.Close(f.ctx)
	_, err = owner.Exec(f.ctx, `UPDATE prediction_markets SET close_at = close_at + interval '1 day' WHERE id = $1`, m.ID)
	assert.Equal(t, "PM002", db.SQLState(err), "%v", err)

	// History is append-only for everyone.
	for _, table := range []string{"prediction_market_transitions", "prediction_resolutions", "prediction_outcomes_registry"} {
		err = exec(`UPDATE ` + table + ` SET created_at = now()`)
		assert.Equal(t, "42501", db.SQLState(err), "%s: %v", table, err)
		_, err = owner.Exec(f.ctx, `DELETE FROM `+table+` WHERE market_id = $1`, r.ID)
		require.Error(t, err, table)
		assert.True(t, db.IsImmutableRow(err), "%s: %v", table, err)
	}
	err = exec(`UPDATE prediction_positions SET quantity = 0`)
	require.Error(t, err, "the projection cannot be written")

	// And after all that, the books are what the service left.
	f.requireConserved(m, money.Quantity{})
	f.requireConserved(r, money.Quantity{})
}

// TestIntegration_SQLAndGoAgreeOnMarketTransitions compares the two copies of
// the lifecycle table.
func TestIntegration_SQLAndGoAgreeOnMarketTransitions(t *testing.T) {
	requireEnv(t)
	all := append([]MarketStatus{statusNone}, AllMarketStatuses()...)
	for _, from := range all {
		for _, to := range all {
			var legal bool
			require.NoError(t, testDB.QueryRow(context.Background(), `SELECT cp_prediction_market_transition_legal($1,$2)`, from, to).Scan(&legal))
			assert.Equal(t, CanTransition(from, to), legal, "%s -> %s", from, to)
		}
	}
}

// TestIntegration_EveryEnumCheckMatchesItsGoList compares each enum CHECK of
// migration 00749 with the Go list that mirrors it, and fails if a new one
// appears without a pair (test/integration/enums makes the same comparison
// schema-wide once the integrator registers these pairs).
func TestIntegration_EveryEnumCheckMatchesItsGoList(t *testing.T) {
	requireEnv(t)
	str := func(vs ...string) []string { sort.Strings(vs); return vs }
	var statuses, modes, actors, kinds, settlements, policies []string
	for _, s := range AllMarketStatuses() {
		statuses = append(statuses, string(s))
	}
	for _, m := range SupportedModes() {
		modes = append(modes, string(m))
	}
	for _, a := range LifecycleActorTypes() {
		actors = append(actors, string(a))
	}
	for _, k := range AllResolutionKinds() {
		kinds = append(kinds, string(k))
	}
	for _, k := range AllSettlementKinds() {
		settlements = append(settlements, string(k))
	}
	for _, p := range AllRoundingPolicies() {
		policies = append(policies, string(p))
	}
	pairs := map[string][]string{
		"prediction_markets.prediction_markets_status_check":                           str(statuses...),
		"prediction_markets.prediction_markets_mode_check":                             str(modes...),
		"prediction_market_transitions.prediction_market_transitions_actor_type_check": str(actors...),
		"prediction_resolutions.prediction_resolutions_kind_check":                     str(kinds...),
		"prediction_settlements.prediction_settlements_kind_check":                     str(settlements...),
		"prediction_settlements.prediction_settlements_rounding_policy_check":          str(policies...),
	}
	rows, err := testDB.Query(context.Background(), `SELECT conrelid::regclass::text || '.' || conname, pg_get_constraintdef(oid)
		FROM pg_constraint WHERE contype = 'c' AND conrelid::regclass::text LIKE 'prediction\_%'
		  AND conrelid::regclass::text <> 'prediction_outcomes' AND pg_get_constraintdef(oid) LIKE '%= ANY (ARRAY[%'`)
	require.NoError(t, err)
	defer rows.Close()
	literal := regexp.MustCompile(`'([^']*)'::text`)
	found := map[string]bool{}
	for rows.Next() {
		var name, def string
		require.NoError(t, rows.Scan(&name, &def))
		if _, mine := pairs[name]; !mine && !isPredmarketTable(name) {
			continue
		}
		found[name] = true
		want, ok := pairs[name]
		if !assert.True(t, ok, "enum CHECK %s has no Go list", name) {
			continue
		}
		var got []string
		for _, m := range literal.FindAllStringSubmatch(def, -1) {
			got = append(got, m[1])
		}
		assert.Equal(t, want, str(got...), name)
	}
	require.NoError(t, rows.Err())
	for name := range pairs {
		assert.True(t, found[name], "%s is paired here but not in the schema", name)
	}
}

func isPredmarketTable(qualified string) bool {
	for _, t := range []string{"prediction_markets.", "prediction_market_transitions.", "prediction_outcomes_registry.",
		"prediction_resolutions.", "prediction_disputes.", "prediction_settlements."} {
		if len(qualified) > len(t) && qualified[:len(t)] == t {
			return true
		}
	}
	return false
}

// TestIntegration_ReconciliationDetectsAnEscrowDivergence: the
// PREDICTION_ESCROW reconciliation records a MATCHED market as matched and an
// injected divergence as a material MISMATCH, refreshes that record instead
// of opening another, and while the books do not reconcile the market issues,
// settles and refunds nothing.
func TestIntegration_ReconciliationDetectsAnEscrowDivergence(t *testing.T) {
	f := newFixture(t)
	a := f.customer(10)
	m := f.openMarket("YES", "NO")
	f.issue(m, a, 3)
	reconcile := func() reconciliation.Record {
		var rec reconciliation.Record
		require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			rec, err = f.svc.ReconcileEscrow(ctx, tx, ReconcileRequest{MarketID: m.ID, Mode: reconciliation.ModeEventDriven})
			return err
		}))
		return rec
	}
	matched := reconcile()
	assert.Equal(t, reconciliation.KindPredictionEscrow, matched.Kind)
	assert.Equal(t, reconciliation.StatusMatched, matched.Status)
	assert.Equal(t, ScopePredictionMarket, matched.ScopeType)
	assert.Equal(t, m.ID.String(), matched.ScopeID)
	assert.Equal(t, m.CollateralAssetID, matched.AssetID)

	// Inject a divergence through the real ledger: one base unit of escrow
	// that no outstanding share explains.
	fac := m.Facility()
	one := money.QuantityFromInt64(1)
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindFacilityFunding, IdempotencyKey: "itest-divergence:" + ref,
			Reference: ledger.FinancialEventReference{Type: "itest_divergence", ID: ref}, EffectiveAt: f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: fac.Account(ledger.CodePredictionEscrow, m.CollateralAssetID), Side: ledger.Debit, Quantity: one},
				{Account: fac.Account(ledger.CodePredictionCollateral, m.CollateralAssetID), Side: ledger.Credit, Quantity: one},
			},
		})
		return err
	}))
	mismatch := reconcile()
	assert.Equal(t, reconciliation.StatusMismatch, mismatch.Status)
	assert.True(t, mismatch.Material)
	assert.NotEqual(t, matched.ID, mismatch.ID)
	var diff struct {
		EscrowMinusImplied []string `json:"escrow_minus_implied"`
		Violations         []string `json:"violations"`
	}
	require.NoError(t, json.Unmarshal(mismatch.Difference, &diff))
	assert.Equal(t, []string{"1", "1"}, diff.EscrowMinusImplied)
	assert.NotEmpty(t, diff.Violations)

	again := reconcile()
	assert.Equal(t, mismatch.ID, again.ID, "an unresolved mismatch is refreshed, not duplicated")
	assert.Equal(t, 2, f.count(`SELECT count(*) FROM reconciliation_records WHERE kind = 'PREDICTION_ESCROW' AND scope_id = $1`, m.ID.String()))

	// While the books do not reconcile, nothing new happens on the market.
	_, err := f.tryIssue(f.positionRequest(m, a, 1))
	requireCode(t, err, errs.CodeReconciliationRequired)
	_, err = f.redeem(m, a, 1)
	requireCode(t, err, errs.CodeReconciliationRequired)
	_, err = f.tryVoid(m, f.resolver)
	requireCode(t, err, errs.CodeReconciliationRequired)
	f.resolve(m, "YES")
	_, err = f.trySettle(m, f.third)
	requireCode(t, err, errs.CodeReconciliationRequired)
	assert.Equal(t, StatusResolved, f.get(m).Status)
	assert.Equal(t, units(3).String(), f.wallet(a, m.Outcomes[0].ShareAssetID).String(), "no share moved")
}

// TestIntegration_TheMigrationKeepsTheSchemaContracts checks, for migration
// 00749 alone, the schema-wide contracts test/integration/migrations holds
// every migration to: least privilege for the application role, read-only
// operator roles, append-only history, and every documented SQLSTATE raised.
func TestIntegration_TheMigrationKeepsTheSchemaContracts(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	has := func(role, table, p string) bool {
		var ok bool
		require.NoError(t, testDB.QueryRow(ctx, `SELECT has_table_privilege($1, $2, $3)`, role, "public."+table, p).Scan(&ok))
		return ok
	}
	hasColumn := func(role, table, column, p string) bool {
		var ok bool
		require.NoError(t, testDB.QueryRow(ctx, `SELECT has_column_privilege($1, $2, $3, $4)`, role, "public."+table, column, p).Scan(&ok))
		return ok
	}
	history := []string{"prediction_market_transitions", "prediction_outcomes_registry", "prediction_resolutions",
		"prediction_disputes", "prediction_settlements"}
	for _, table := range append([]string{"prediction_markets"}, history...) {
		assert.True(t, has("cp_app", table, "SELECT"), table)
		assert.True(t, has("cp_app", table, "INSERT"), table)
		assert.False(t, has("cp_app", table, "DELETE"), table)
		assert.False(t, has("cp_app", table, "TRUNCATE"), table)
		for _, ro := range []string{"cp_readonly", "cp_ops"} {
			assert.True(t, has(ro, table, "SELECT"), "%s %s", ro, table)
			for _, p := range []string{"INSERT", "UPDATE", "DELETE", "TRUNCATE"} {
				assert.False(t, has(ro, table, p), "%s may not %s %s", ro, p, table)
			}
		}
	}
	for _, table := range history {
		assert.False(t, has("cp_app", table, "UPDATE"), "%s is append-only", table)
	}
	for _, column := range []string{"status", "current_resolution_id"} {
		assert.True(t, hasColumn("cp_app", "prediction_markets", column, "UPDATE"), column)
	}
	for _, column := range []string{"symbol", "facility_id", "collateral_asset_id", "close_at", "oracle_public_key", "created_by_actor_id"} {
		assert.False(t, hasColumn("cp_app", "prediction_markets", column, "UPDATE"), "%s is identity", column)
	}
	for _, role := range []string{"cp_app", "cp_readonly", "cp_ops"} {
		assert.True(t, has(role, "prediction_positions", "SELECT"), role)
		assert.False(t, has(role, "prediction_positions", "INSERT"), role)
	}

	body, err := migrations.FS.ReadFile("00749_prediction_markets.sql")
	require.NoError(t, err)
	text := string(body)
	block := regexp.MustCompile(`(?m)^--\s*Custom SQLSTATEs[^\n]*(?:\n--[^\n]*)*`).FindString(text)
	documented := regexp.MustCompile(`\b([A-Z]{2}[0-9]{3})\b`).FindAllString(block, -1)
	assert.Equal(t, []string{"PM001", "PM002", "PM003", "PM004", "PM005"}, documented)
	for _, code := range documented {
		assert.Contains(t, text, "ERRCODE = '"+code+"'", "%s is documented and must be raised", code)
	}
}
