//go:build integration

package predmarket

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/reconciliation"
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

func (f *fixture) closeBatch(limit int) CloseBatch {
	f.t.Helper()
	var out CloseBatch
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.CloseDueBatch(ctx, tx, f.system, limit)
		return err
	}))
	return out
}

func (f *fixture) marketClosingIn(d time.Duration) Market {
	f.t.Helper()
	spec := f.spec("YES", "NO")
	spec.CloseAt = f.clk.Now().Add(d)
	return f.openMarketFrom(spec)
}

// TestIntegration_AMarketThatCannotCloseNeverStopsTheOthers: one due market
// the database refuses to close (only at COMMIT, the hardest case) plus N
// good ones. In one run every good market closes and the poison is rolled
// back alone -- still OPEN, its outcome instruments still ACTIVE -- and
// reported with its backoff; a second run closes new due markets without
// retrying the poison inside its backoff (CloseDue, the scheduler's entry
// point, never fails for it either); once the backoff has passed it is tried
// again, after every never-failed due market, and reported again.
func TestIntegration_AMarketThatCannotCloseNeverStopsTheOthers(t *testing.T) {
	f := newFixture(t)
	// A clock of its own, years before every other test's: the only due
	// markets are this test's.
	f.clk.Set(time.Date(2021, 3, 1, 12, 0, 0, 0, time.UTC))
	poison := f.marketClosingIn(time.Minute)
	var good []MarketID
	for i := 0; i < 3; i++ {
		good = append(good, f.marketClosingIn(5*time.Minute).ID)
	}
	poisonRow(t, "prediction_markets", poison.ID.String(), true)

	f.clk.Advance(10 * time.Minute)
	first := f.closeBatch(100)
	assert.ElementsMatch(t, good, first.Closed, "every good market closes in the one run")
	require.Len(t, first.Failures, 1, "%+v", first.Failures)
	assert.Equal(t, poison.ID, first.Failures[0].MarketID)
	assert.Equal(t, 1, first.Failures[0].Attempts)
	assert.True(t, first.Failures[0].RetryAfter.Equal(f.clk.Now().Add(time.Minute)))
	assert.Equal(t, StatusOpen, f.get(poison).Status, "rolled back alone")
	for _, o := range poison.Outcomes {
		assert.Equal(t, assets.StatusActive, f.instrumentStatus(o.InstrumentID), "its instruments too")
	}
	for _, id := range good {
		assert.Equal(t, StatusClosed, f.get(Market{ID: id}).Status)
	}

	fresh := []MarketID{f.marketClosingIn(10 * time.Second).ID, f.marketClosingIn(10 * time.Second).ID}
	f.clk.Advance(20 * time.Second)
	second := f.closeBatch(1)
	assert.Len(t, second.Closed, 1, "a batch of one is never spent on a poison in its backoff")
	assert.Empty(t, second.Failures)
	var closed []MarketID
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		closed, err = f.svc.CloseDue(ctx, tx, f.system, 100)
		return err
	}))
	assert.ElementsMatch(t, fresh, append(second.Closed, closed...))
	assert.Equal(t, 1, f.count(`SELECT failures FROM upkeep_failures WHERE operation = $1 AND item_id = $2`, UpkeepCloseDue, poison.ID),
		"not retried inside its backoff")

	last := f.marketClosingIn(10 * time.Second)
	f.clk.Advance(2 * time.Minute)
	third := f.closeBatch(100)
	assert.Equal(t, []MarketID{last.ID}, third.Closed)
	require.Len(t, third.Failures, 1)
	assert.Equal(t, poison.ID, third.Failures[0].MarketID)
	assert.Equal(t, 2, third.Failures[0].Attempts, "tried again once its backoff passed")
	assert.True(t, third.Failures[0].RetryAfter.Equal(f.clk.Now().Add(2*time.Minute)), "the backoff doubles")
}

// TestIntegration_PeriodicEscrowReconciliationsLeaveOneMatchedRecord: N
// scheduled reconciliations of a market whose books agree leave one MATCHED
// PREDICTION_ESCROW record, not N; an operator's FULL check is recorded every
// time.
func TestIntegration_PeriodicEscrowReconciliationsLeaveOneMatchedRecord(t *testing.T) {
	f := newFixture(t)
	a := f.customer(10)
	m := f.openMarket("YES", "NO")
	f.issue(m, a, 2)
	reconcile := func(mode reconciliation.Mode) reconciliation.Record {
		var rec reconciliation.Record
		require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			rec, err = f.svc.ReconcileEscrow(ctx, tx, ReconcileRequest{MarketID: m.ID, Mode: mode})
			return err
		}))
		require.Equal(t, reconciliation.StatusMatched, rec.Status)
		return rec
	}
	records := func() int {
		return f.count(`SELECT count(*) FROM reconciliation_records WHERE kind = 'PREDICTION_ESCROW' AND scope_id = $1`, m.ID.String())
	}
	first := reconcile(reconciliation.ModePeriodic)
	for i := 0; i < 4; i++ {
		f.clk.Advance(time.Hour)
		assert.Equal(t, first.ID, reconcile(reconciliation.ModePeriodic).ID)
	}
	assert.Equal(t, 1, records(), "five periodic passes leave one MATCHED record")
	f.clk.Advance(time.Hour)
	full := reconcile(reconciliation.ModeFull)
	assert.NotEqual(t, first.ID, full.ID, "an operator's FULL check is evidence of itself")
	assert.Equal(t, 2, records())
}
