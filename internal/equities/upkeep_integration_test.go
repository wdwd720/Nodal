//go:build integration

package equities

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/econ"
)

// poisonRow makes the database refuse every update of one row of table --
// at once, or only at COMMIT -- until the test ends: an item that can never
// be processed. It needs the schema owner, so it connects with the migration
// role.
func poisonRow(t *testing.T, table, rowID string, atCommit bool) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testMigrateURL)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()
	name := "itest_poison_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	_, err = conn.Exec(ctx, `CREATE FUNCTION `+name+`() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.id = '`+rowID+`'::uuid THEN
				RAISE EXCEPTION 'itest poison: % refuses every change', NEW.id;
			END IF;
			RETURN NEW;
		END $$`)
	require.NoError(t, err)
	trigger := `CREATE TRIGGER ` + name + ` BEFORE UPDATE ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION ` + name + `()`
	if atCommit {
		trigger = `CREATE CONSTRAINT TRIGGER ` + name + ` AFTER UPDATE ON ` + table +
			` DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ` + name + `()`
	}
	_, err = conn.Exec(ctx, trigger)
	require.NoError(t, err)
	t.Cleanup(func() {
		c, err := pgx.Connect(ctx, testMigrateURL)
		if err != nil {
			t.Errorf("drop poison: %v", err)
			return
		}
		defer func() { _ = c.Close(ctx) }()
		if _, err := c.Exec(ctx, `DROP TRIGGER IF EXISTS `+name+` ON `+table); err != nil {
			t.Errorf("drop poison trigger: %v", err)
		}
		if _, err := c.Exec(ctx, `DROP FUNCTION IF EXISTS `+name+`()`); err != nil {
			t.Errorf("drop poison function: %v", err)
		}
	})
}

// settleBatches runs SettleDueBatch, one account per transaction, until it
// takes on nothing, and returns what it settled and what it could not.
func (w *world) settleBatches() ([]Settlement, []SettlementFailure) {
	w.t.Helper()
	var settled []Settlement
	var failures []SettlementFailure
	for i := 0; i < 1000; i++ {
		var b SettlementBatch
		require.NoError(w.t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			b, err = w.svc.SettleDueBatch(ctx, tx, 500)
			return err
		}))
		if b.Examined() == 0 {
			return settled, failures
		}
		settled = append(settled, b.Settled...)
		failures = append(failures, b.Failures...)
	}
	w.t.Fatal("the settlement worker never ran out of due settlements")
	return nil, nil
}

// buys fills n market buys of one share each and returns their settlements.
func (w *world) buys(acct accounts.AccountID, l Listing, n int) ([]Settlement, []Order) {
	w.t.Helper()
	var out []Settlement
	var orders []Order
	for i := 0; i < n; i++ {
		o := w.trade(acct, l, market(SideBuy, 1_000_000))
		require.Equal(w.t, OrderFilled, o.Status)
		f, ok, err := w.svc.FillForOrder(w.ctx, testDB, o.ID)
		require.NoError(w.t, err)
		require.True(w.t, ok)
		st, ok, err := w.svc.SettlementForFill(w.ctx, testDB, f.ID)
		require.NoError(w.t, err)
		require.True(w.t, ok)
		out = append(out, st)
		orders = append(orders, o)
	}
	return out, orders
}

func settledFill(settled []Settlement, fid FillID) bool {
	for _, s := range settled {
		if s.FillID == fid {
			return true
		}
	}
	return false
}

func settlementFailure(failures []SettlementFailure, sid SettlementID) (SettlementFailure, bool) {
	for _, f := range failures {
		if f.SettlementID == sid {
			return f, true
		}
	}
	return SettlementFailure{}, false
}

// TestIntegration_ASettlementThatCannotSettleNeverStopsTheOthers: one due
// equity settlement the database refuses (only at COMMIT, after its journal
// was posted and its effect applied) plus N good ones of the same account.
// In one run every good settlement settles and the poison is rolled back
// alone -- no journal, no effect, its intent still EXECUTING -- and reported
// with its backoff; on the next settlement date a second run settles the new
// fills, takes the poison only after them, and reports it again.
func TestIntegration_ASettlementThatCannotSettleNeverStopsTheOthers(t *testing.T) {
	w := newWorld(t)
	acct := w.account()
	l := w.list(1, 1)
	w.fund(acct, w.cash.ID, 10_000_000_000)
	w.mark(l, 15_000)
	settlements, orders := w.buys(acct, l, 4)
	poison := settlements[0]
	poisonRow(t, "equity_settlements", poison.ID.String(), true)

	w.at(2026, 10, 8, 9, 0)
	settled, failures := w.settleBatches()
	for _, st := range settlements[1:] {
		assert.True(t, settledFill(settled, st.FillID), "every good settlement of the account settles in the one run")
	}
	failed, ok := settlementFailure(failures, poison.ID)
	require.True(t, ok, "the poison is reported: %+v", failures)
	assert.Equal(t, 1, failed.Attempts)
	assert.Equal(t, poison.FillID, failed.FillID)
	assert.True(t, failed.RetryAfter.Equal(w.clk.Now().Add(time.Minute)), "first backoff is a minute")
	got, _, err := w.svc.SettlementForFill(w.ctx, testDB, poison.FillID)
	require.NoError(t, err)
	assert.False(t, got.Settled(), "rolled back alone")
	assert.Zero(t, w.count(`SELECT count(*) FROM journal_transactions WHERE reference_type = 'equity_settlement' AND reference_id = $1`,
		poison.ID.String()), "its journal rolled back with it")
	_, applied, err := w.effects.GetByKey(w.ctx, testDB, "equities:settlement:"+poison.FillID.String())
	require.NoError(t, err)
	assert.False(t, applied, "and its effect")
	assert.Equal(t, econ.StatusExecuting, w.intent(orders[0].IntentID).Status)

	// The next trading day's fills settle in a second run behind it.
	w.at(2026, 10, 8, 10, 0)
	w.mark(l, 15_000)
	more, _ := w.buys(acct, l, 2)
	w.at(2026, 10, 9, 9, 0)
	settled, failures = w.settleBatches()
	for _, st := range more {
		assert.True(t, settledFill(settled, st.FillID), "a new due settlement settles behind the poison")
	}
	failed, ok = settlementFailure(failures, poison.ID)
	require.True(t, ok, "tried again once its backoff passed: %+v", failures)
	assert.Equal(t, 2, failed.Attempts)
}
