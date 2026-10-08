//go:build integration

package orders

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
// be processed.
func (f *fixture) poisonRow(table, rowID string, atCommit bool) {
	f.t.Helper()
	name := "itest_poison_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	owner := f.owner()
	_, err := owner.Exec(f.ctx, `CREATE FUNCTION `+name+`() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.id = '`+rowID+`'::uuid THEN
				RAISE EXCEPTION 'itest poison: % refuses every change', NEW.id;
			END IF;
			RETURN NEW;
		END $$`)
	require.NoError(f.t, err)
	trigger := `CREATE TRIGGER ` + name + ` BEFORE UPDATE ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION ` + name + `()`
	if atCommit {
		trigger = `CREATE CONSTRAINT TRIGGER ` + name + ` AFTER UPDATE ON ` + table +
			` DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ` + name + `()`
	}
	_, err = owner.Exec(f.ctx, trigger)
	require.NoError(f.t, err)
	f.t.Cleanup(func() {
		ctx := context.Background()
		c, err := pgx.Connect(ctx, testMigrateURL)
		if err != nil {
			f.t.Errorf("drop poison: %v", err)
			return
		}
		defer func() { _ = c.Close(ctx) }()
		if _, err := c.Exec(ctx, `DROP TRIGGER IF EXISTS `+name+` ON `+table); err != nil {
			f.t.Errorf("drop poison trigger: %v", err)
		}
		if _, err := c.Exec(ctx, `DROP FUNCTION IF EXISTS `+name+`()`); err != nil {
			f.t.Errorf("drop poison function: %v", err)
		}
	})
}

// crashedBuys leaves n buy orders FILLED with their intents still EXECUTING:
// each crosses a resting sell, and the process stops after Execute, before
// Complete.
func (f *fixture) crashedBuys(m spotMarket, seller, buyer accounts.AccountID, n int) []econ.IntentID {
	f.t.Helper()
	var out []econ.IntentID
	for i := 0; i < n; i++ {
		f.submit(f.limit(m, seller, SideSell, "100.00", units(1)))
		var accepted StageOutcome
		require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			accepted, err = f.svc.Accept(ctx, tx, f.limit(m, buyer, SideBuy, "100.00", units(1)))
			return err
		}))
		for _, stage := range []func(context.Context, pgx.Tx, OrderID) (StageOutcome, error){f.svc.Approve, f.svc.Reserve, f.svc.Execute} {
			require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error { _, err := stage(ctx, tx, accepted.Order.ID); return err }))
		}
		o := f.orderView(accepted.Order.ID)
		require.Equal(f.t, StatusFilled, o.Status)
		require.Equal(f.t, econ.StatusExecuting, f.intentStatus(o))
		out = append(out, *o.IntentID)
	}
	return out
}

func (f *fixture) completeTerminal(limit int) CompletionBatch {
	f.t.Helper()
	var out CompletionBatch
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.CompleteTerminal(ctx, tx, limit)
		return err
	}))
	return out
}

func completionFailure(b CompletionBatch, iid econ.IntentID) (CompletionFailure, bool) {
	for _, f := range b.Failures {
		if f.IntentID == iid {
			return f, true
		}
	}
	return CompletionFailure{}, false
}

func (f *fixture) intentStatusOf(iid econ.IntentID) econ.IntentStatus {
	f.t.Helper()
	in, err := f.intents.Get(f.ctx, testDB, iid)
	require.NoError(f.t, err)
	return in.Status
}

// TestIntegration_AnIntentThatCannotCompleteNeverStopsTheSweep: one finished
// order whose intent the database refuses to complete plus N good ones. In
// one sweep every good intent completes and the poison is rolled back alone
// and reported with its backoff; a second sweep completes new finished orders
// without retrying the poison inside its backoff; once the backoff has
// passed it is tried again and reported again.
func TestIntegration_AnIntentThatCannotCompleteNeverStopsTheSweep(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{})
	accts := f.fundedAccounts(m, 2)
	intents := f.crashedBuys(m, accts[0], accts[1], 4)
	poison := intents[0]
	f.poisonRow("economic_intents", poison.String(), false)

	first := f.completeTerminal(1000)
	for _, iid := range intents[1:] {
		assert.Contains(t, first.Completed, iid)
		assert.Equal(t, econ.StatusCompleted, f.intentStatusOf(iid))
	}
	failed, ok := completionFailure(first, poison)
	require.True(t, ok, "the poison is reported: %+v", first.Failures)
	assert.Equal(t, 1, failed.Attempts)
	assert.True(t, failed.RetryAfter.Equal(f.clk.Now().Add(time.Minute)))
	assert.Equal(t, econ.StatusExecuting, f.intentStatusOf(poison), "rolled back alone")

	more := f.crashedBuys(m, accts[0], accts[1], 2)
	second := f.completeTerminal(1000)
	for _, iid := range more {
		assert.Contains(t, second.Completed, iid, "a new finished order completes behind the poison")
	}
	_, retried := completionFailure(second, poison)
	assert.False(t, retried, "not retried inside its backoff")

	f.clk.Advance(2 * time.Minute)
	third := f.completeTerminal(1000)
	failed, ok = completionFailure(third, poison)
	require.True(t, ok, "tried again once its backoff passed")
	assert.Equal(t, 2, failed.Attempts)
	assert.True(t, failed.RetryAfter.Equal(f.clk.Now().Add(2*time.Minute)), "the backoff doubles")
	assert.Equal(t, econ.StatusExecuting, f.intentStatusOf(poison))
}
