//go:build integration

package derivatives

import (
	"context"
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/security"
)

// TestIntegration_TheDatabaseRefusesAClearingFlowThatDoesNotBalancePerParty:
// a FUNDING_PAYMENT that hands a customer the clearing house's cash directly
// balances per asset but not per party. Written around internal/ledger, the
// database refuses it at COMMIT (SM002).
func TestIntegration_TheDatabaseRefusesAClearingFlowThatDoesNotBalancePerParty(t *testing.T) {
	f := newFixture(t)
	a := f.account()
	ch := f.clearingHouse()
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := rawJournal(ctx, f, tx, ledger.KindFundingPayment, []rawEntry{
			{ledger.CustomerAccount(a, ledger.CodeWallet, f.usd), ledger.Debit, 100},
			{ch.Account(ledger.CodeClearingCash, f.usd), ledger.Credit, 100},
		})
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "SM002", db.SQLState(err), "%v", err)
}

func (f *fixture) openPosition(n int64) Position {
	f.t.Helper()
	perp := f.perp()
	a := f.account()
	f.fund(a, usdUnits(100_000))
	f.mark(perp, 65_000, 65_000)
	f.open(a, perp, Long, n)
	return f.position(a, perp)
}

func (f *fixture) rawFill(ctx context.Context, tx pgx.Tx, p Position, source string, qty int64, decision any) error {
	side := "BUY"
	if qty < 0 {
		side = "SELL"
	}
	_, err := tx.Exec(ctx, `INSERT INTO derivative_fills (id, fill_key, position_id, account_id, instrument_id, source, side, quantity,
		price_mantissa, price_scale, value, risk_decision_id, occurred_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,6500000,2,0,$9,now())`,
		uuid.New(), "raw:"+uuid.NewString(), p.ID, p.AccountID, p.InstrumentID, source, side, qty, decision)
	return err
}

// TestIntegration_APositionCannotChangeWithoutAFill: the quantity of a
// position is the sum of its fills, from both directions, at COMMIT (DV001).
func TestIntegration_APositionCannotChangeWithoutAFill(t *testing.T) {
	f := newFixture(t)
	p := f.openPosition(2)
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE derivative_positions SET quantity = quantity + 1 WHERE id = $1`, p.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "DV001", db.SQLState(err), "%v", err)

	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error { return f.rawFill(ctx, tx, p, "EXPIRY", 1, nil) })
	require.Error(t, err)
	assert.Equal(t, "DV001", db.SQLState(err), "a fill nobody applied to the position")

	// A trade with no risk decision cannot even be written.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error { return f.rawFill(ctx, tx, p, "PAPER", 1, nil) })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "derivative_fills_trades_carry_a_decision")

	// A status change with a matching fill but no transition row is refused
	// by the transition binding (AU001).
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if err := f.rawFill(ctx, tx, p, "EXPIRY", -2, nil); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE derivative_positions SET quantity = 0, cost_value = 0, settled_value = 0, margin_posted = 0,
			status = 'CLOSED', closed_at = now() WHERE id = $1`, p.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "AU001", db.SQLState(err), "%v", err)
	assert.Equal(t, "2", f.position(p.AccountID, p.InstrumentID).Quantity.String())
}

// TestIntegration_AFlatPositionIsHistory: a CLOSED position cannot reopen,
// even with a transition row describing it (DV003); identity is frozen
// (DV002 for a role that may name the column, 42501 for the application);
// fills are append-only.
func TestIntegration_AFlatPositionIsHistory(t *testing.T) {
	f := newFixture(t)
	p := f.openPosition(1)
	f.closeAll(p.AccountID, p.InstrumentID)
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO derivative_position_transitions (id, position_id, from_status, to_status, actor_type,
			actor_id, reason, occurred_at) VALUES ($1,$2,'CLOSED','OPEN','SYSTEM','raw','reopen',now())`, uuid.New(), p.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE derivative_positions SET status = 'OPEN', closed_at = NULL WHERE id = $1`, p.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "DV003", db.SQLState(err), "%v", err)

	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE derivative_positions SET account_id = $2 WHERE id = $1`, p.ID, f.account())
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "42501", db.SQLState(err), "the application role cannot name an identity column")

	owner, err := pgx.Connect(f.ctx, testMigrateURL)
	require.NoError(t, err)
	defer owner.Close(f.ctx)
	_, err = owner.Exec(f.ctx, `UPDATE derivative_positions SET account_id = $2 WHERE id = $1`, p.ID, f.account())
	require.Error(t, err)
	assert.Equal(t, "DV002", db.SQLState(err))
	_, err = owner.Exec(f.ctx, `UPDATE derivative_fills SET price_mantissa = 1 WHERE position_id = $1`, p.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "immutable row")
	_, err = owner.Exec(f.ctx, `DELETE FROM derivative_positions WHERE id = $1`, p.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "immutable row")
}

// TestIntegration_ACashFlowMustAgreeWithItsJournal: a register row naming a
// journal of the wrong kind, or one that moves CLEARING_RESULT by a different
// amount, is refused at COMMIT (DV004).
func TestIntegration_ACashFlowMustAgreeWithItsJournal(t *testing.T) {
	f := newFixture(t)
	p := f.openPosition(1)
	ch := f.clearingHouse()
	insertFlow := func(ctx context.Context, tx pgx.Tx, kind string, amount int64, journal any, effect econ.EffectID) error {
		_, err := tx.Exec(ctx, `INSERT INTO derivative_cash_flows (id, flow_key, position_id, account_id, instrument_id,
			settlement_asset_id, clearing_house_id, kind, amount, journal_transaction_id, effect_id, reference_type, reference_id, occurred_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'raw','raw',now())`,
			uuid.New(), "raw:"+uuid.NewString(), p.ID, p.AccountID, p.InstrumentID, f.usd, ch.ID, kind, amount, journal, effect)
		return err
	}
	claim := func(ctx context.Context, tx pgx.Tx) (econ.EffectID, error) {
		e, _, err := f.effects.Begin(ctx, tx, econ.BeginRequest{Key: "raw:" + uuid.NewString(), Subsystem: econ.SubsystemDerivatives,
			Kind: "RAW", Request: map[string]string{"raw": "1"}, PostsLedger: true, Actor: econ.Actor{Type: security.ActorSystem, ID: "raw"}})
		return e.ID, err
	}
	// The margin move of the open is a journal with no clearing-house side.
	var marginJournal ledger.TransactionID
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT journal_transaction_id FROM economic_effects WHERE effect_key LIKE 'deriv:fill:%:margin-post'
		AND account_id = $1`, p.AccountID).Scan(&marginJournal))
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		eid, err := claim(ctx, tx)
		if err != nil {
			return err
		}
		return insertFlow(ctx, tx, "FUNDING", -5, marginJournal, eid)
	})
	require.Error(t, err)
	assert.Equal(t, "DV004", db.SQLState(err), "%v", err)

	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		jid, err := rawJournal(ctx, f, tx, ledger.KindVariationMargin, []rawEntry{
			{ledger.CustomerAccount(p.AccountID, ledger.CodeWallet, f.usd), ledger.Debit, 50},
			{ledger.CustomerAccount(p.AccountID, ledger.CodeRealizedPnL, f.usd), ledger.Credit, 50},
			{ch.Account(ledger.CodeClearingResult, f.usd), ledger.Debit, 50},
			{ch.Account(ledger.CodeClearingCash, f.usd), ledger.Credit, 50},
		})
		if err != nil {
			return err
		}
		eid, err := claim(ctx, tx)
		if err != nil {
			return err
		}
		return insertFlow(ctx, tx, "VARIATION_MARGIN", 40, jid, eid) // the journal moved 50
	})
	require.Error(t, err)
	assert.Equal(t, "DV004", db.SQLState(err), "%v", err)
}

// TestIntegration_EveryEnumCheckIsItsGoList pairs each CHECK (col IN (...))
// of migration 00741 with the Go function returning the same list.
func TestIntegration_EveryEnumCheckIsItsGoList(t *testing.T) {
	requireEnv(t)
	pairs := []struct {
		table, constraint string
		want              []string
	}{
		{"derivative_positions", "derivative_positions_status_check", enumStrings(AllPositionStatuses())},
		{"derivative_position_transitions", "derivative_position_transitions_from_status_check", enumStrings(AllPositionStatuses())},
		{"derivative_position_transitions", "derivative_position_transitions_to_status_check", enumStrings(AllPositionStatuses())},
		{"derivative_fills", "derivative_fills_source_check", enumStrings(AllFillSources())},
		{"derivative_fills", "derivative_fills_side_check", enumStrings(AllFillSides())},
		{"derivative_cash_flows", "derivative_cash_flows_kind_check", enumStrings(AllCashFlowKinds())},
		{"derivative_settlements", "derivative_settlements_kind_check", enumStrings(AllSettlementKinds())},
		{"funding_accruals", "funding_accruals_direction_check", enumStrings(AllFundingDirections())},
		{"option_exercises", "option_exercises_kind_check", enumStrings(AllExerciseKinds())},
	}
	quoted := regexp.MustCompile(`'([^']*)'::text`)
	for _, p := range pairs {
		var def string
		require.NoError(t, testDB.QueryRow(context.Background(), `SELECT pg_get_constraintdef(oid) FROM pg_constraint
			WHERE conrelid = $1::regclass AND conname = $2`, p.table, p.constraint).Scan(&def), p.constraint)
		var got []string
		for _, m := range quoted.FindAllStringSubmatch(def, -1) {
			got = append(got, m[1])
		}
		assert.Equal(t, p.want, got, "%s.%s: %s", p.table, p.constraint, def)
	}
}

func enumStrings[T ~string](xs []T) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = string(x)
	}
	return out
}

func TestIntegration_SQLAndGoAgreeOnPositionTransitionsAndJournalKinds(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	for _, from := range AllPositionStatuses() {
		for _, to := range AllPositionStatuses() {
			var legal bool
			require.NoError(t, testDB.QueryRow(ctx, `SELECT cp_derivative_position_transition_legal($1,$2)`, from, to).Scan(&legal))
			assert.Equal(t, CanTransitionPosition(from, to), legal, "%s -> %s", from, to)
		}
	}
	for _, k := range AllCashFlowKinds() {
		var sqlKind *string
		require.NoError(t, testDB.QueryRow(ctx, `SELECT cp_derivative_cash_flow_journal_kind($1)`, k).Scan(&sqlKind))
		goKind, ok := JournalKindFor(k)
		require.True(t, ok)
		require.NotNil(t, sqlKind, "%s", k)
		assert.Equal(t, string(goKind), *sqlKind, "%s", k)
	}
	var none *string
	require.NoError(t, testDB.QueryRow(ctx, `SELECT cp_derivative_cash_flow_journal_kind('NOT_A_KIND')`).Scan(&none))
	assert.Nil(t, none)
}
