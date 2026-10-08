//go:build integration

package bridge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
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

// devnetLeg starts a DEVNET leg, which is only ever observed: its lock is
// submitted and the leg waits BRIDGE_REQUESTED.
func (f *fixture) devnetLeg(r Route, c customer, q uint64) Leg {
	f.t.Helper()
	res, err := f.bridge(r, c, q, gates.ModeDevnet)
	require.NoError(f.t, err)
	require.Equal(f.t, StateBridgeRequested, res.Leg.Status)
	return res.Leg
}

func legFailure(rep ObserveReport, lid LegID) (LegFailure, bool) {
	for _, f := range rep.Failures {
		if f.LegID == lid {
			return f, true
		}
	}
	return LegFailure{}, false
}

func (f *fixture) legTransitions(lid LegID) int {
	return f.count(`SELECT count(*) FROM bridge_leg_transitions WHERE leg_id = $1`, lid)
}

// TestIntegration_ALegOfAnotherNetworkIsNeverObservedOrSettledHere: a leg
// whose lock is final on its own network's chain is never taken by another
// network's ObservePending -- that host would read the lock as NOT_FOUND and
// could fail it as expired -- is refused by that network's Observe and
// SettleSource without anything changing, even once its blockhash is long
// expired on the foreign chain, and is advanced by its own network.
func TestIntegration_ALegOfAnotherNetworkIsNeverObservedOrSettledHere(t *testing.T) {
	here := newFixture(t)
	there := newFixture(t)
	require.NotEqual(t, here.network, there.network)
	r := there.route
	c := there.customer()
	there.fund(r, c, 1_000_000)
	leg := there.devnetLeg(r, c, 100_000)
	there.produce(r.SourceChain, 1+itDepth)
	// This host's chain moves far past the lock's blockhash: were it read
	// here, the lock would look like one that can never land.
	here.produce(r.SourceChain, itWindow+itDepth+2)
	before := there.legTransitions(leg.ID)

	rep, err := here.svc.ObservePending(here.ctx, 500)
	require.NoError(t, err)
	_, taken := legFailure(rep, leg.ID)
	assert.False(t, taken)
	assert.Equal(t, 0, rep.Examined, "another network's leg is never taken: %+v", rep)
	_, err = here.svc.Observe(here.ctx, leg.ID)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err), "observing it here is refused: %v", err)
	err = testDB.InTx(here.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := here.svc.SettleSource(ctx, tx, leg.ID)
		return err
	})
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err), "settling it here is refused: %v", err)
	err = testDB.InTx(here.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := here.svc.ReconcileRoute(ctx, tx, r.ID, reconciliation.ModePeriodic, operator)
		return err
	})
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err), "so is reconciling its route against this host's chains")
	assert.Equal(t, StateBridgeRequested, there.leg(leg.ID).Status)
	assert.Equal(t, before, there.legTransitions(leg.ID), "never observed or settled here")

	own, err := there.svc.ObservePending(there.ctx, 500)
	require.NoError(t, err)
	assert.Equal(t, 1, own.Examined)
	assert.Empty(t, own.Failures)
	assert.NotEqual(t, StateBridgeRequested, there.leg(leg.ID).Status, "its own network advances it")
	assert.NotEqual(t, StateFailed, there.leg(leg.ID).Status)
}

// TestIntegration_ALegThatCannotAdvanceNeverStopsTheOthers: one in-flight leg
// the database refuses to advance plus N good ones. In one ObservePending
// pass every good leg advances and the poison is rolled back alone and
// reported with its backoff; a second pass advances a new leg without
// retrying the poison inside its backoff; once the backoff has passed it is
// tried again and reported again.
func TestIntegration_ALegThatCannotAdvanceNeverStopsTheOthers(t *testing.T) {
	f := newFixture(t)
	r := f.route
	c := f.customer()
	f.fund(r, c, 1_000_000)
	var legs []Leg
	for i := 0; i < 4; i++ {
		legs = append(legs, f.devnetLeg(r, c, 10_000))
	}
	f.produce(r.SourceChain, 1+itDepth)
	poison := legs[0]
	poisonRow(t, "bridge_legs", poison.ID.String(), false)

	first, err := f.svc.ObservePending(f.ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, 4, first.Examined)
	assert.Equal(t, 3, first.Moved, "every good leg advances in the one pass")
	require.Len(t, first.Failures, 1, "%+v", first.Failures)
	assert.Equal(t, poison.ID, first.Failures[0].LegID)
	assert.Equal(t, 1, first.Failures[0].Attempts)
	assert.True(t, first.Failures[0].RetryAfter.Equal(f.clk.Now().Add(time.Minute)))
	assert.Equal(t, StateBridgeRequested, f.leg(poison.ID).Status, "rolled back alone")
	for _, l := range legs[1:] {
		assert.NotEqual(t, StateBridgeRequested, f.leg(l.ID).Status)
	}

	fresh := f.devnetLeg(r, c, 10_000)
	f.produce(r.SourceChain, 1+itDepth)
	second, err := f.svc.ObservePending(f.ctx, 100)
	require.NoError(t, err)
	_, retried := legFailure(second, poison.ID)
	assert.False(t, retried, "not retried inside its backoff")
	assert.NotEqual(t, StateBridgeRequested, f.leg(fresh.ID).Status, "a new leg advances behind the poison")

	f.clk.Advance(2 * time.Minute)
	third, err := f.svc.ObservePending(f.ctx, 100)
	require.NoError(t, err)
	failed, ok := legFailure(third, poison.ID)
	require.True(t, ok, "tried again once its backoff passed")
	assert.Equal(t, 2, failed.Attempts)
}

// TestIntegration_PeriodicRouteReconciliationsLeaveOneMatchedRecord: N
// scheduled reconciliations of a route whose books and chains agree leave
// one MATCHED BRIDGE record, not N; an operator's FULL check is recorded
// every time.
func TestIntegration_PeriodicRouteReconciliationsLeaveOneMatchedRecord(t *testing.T) {
	f := newFixture(t)
	r := f.route
	c := f.customer()
	f.fund(r, c, 1_000_000)
	f.mustBridge(r, c, 100_000)
	records := func() int {
		return f.count(`SELECT count(*) FROM reconciliation_records WHERE kind = 'BRIDGE' AND scope_id = $1`, r.ID.String())
	}
	first := f.reconcile(r)
	require.True(t, first.Matched())
	for i := 0; i < 4; i++ {
		f.clk.Advance(time.Hour)
		again := f.reconcile(r)
		require.True(t, again.Matched())
		assert.Equal(t, first.RecordID, again.RecordID)
	}
	assert.Equal(t, 1, records(), "five periodic passes leave one MATCHED record")
	var full RouteReconciliation
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		full, err = f.svc.ReconcileRoute(ctx, tx, r.ID, reconciliation.ModeFull, operator)
		return err
	}))
	assert.NotEqual(t, first.RecordID, full.RecordID, "an operator's FULL check is evidence of itself")
	assert.Equal(t, 2, records())
}
