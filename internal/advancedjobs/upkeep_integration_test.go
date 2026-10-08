//go:build integration

package advancedjobs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/delegation"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/predmarket"
	"github.com/nodal/controlplane/internal/security"
)

// poisonRow makes the database refuse every update of one row of table --
// at once, or only at COMMIT -- until the test ends: a unit that can never be
// processed. It needs the schema owner, so it connects with the migration
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

func failureOf(rep Report, unit string) (Failure, bool) {
	for _, f := range rep.Failures {
		if f.Unit == unit {
			return f, true
		}
	}
	return Failure{}, false
}

// TestIntegration_AnExpiryRunReportsTheReservationItCannotExpireAndExpiresTheRest:
// a due reservation the database refuses to expire (only at COMMIT) plus N
// good ones. One run of the expiry job expires every good one and reports the
// poison as that reservation's failure, redacted; the next run expires new
// due reservations and does not report the poison again while its backoff
// runs; the run after the backoff reports it again.
func TestIntegration_AnExpiryRunReportsTheReservationItCannotExpireAndExpiresTheRest(t *testing.T) {
	w := newWorld(t)
	c := w.customer("expiry-poison")
	w.fund(c.account, w.usd.ID, w.usdUnits(100))
	reserve := func(ttl time.Duration) capital.Reservation {
		var r capital.Reservation
		require.NoError(t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			r, err = w.capital.Reserve(ctx, tx, capital.ReserveRequest{
				AccountID: c.account.String(), AssetID: w.usd.ID, Quantity: w.usdUnits(5), ActorType: security.ActorUser,
				ActorID: c.user.String(), IdempotencyKey: "advancedjobs-expiry-poison-" + uuid.NewString(), TTL: ttl,
				Reason: "advancedjobs upkeep test",
			})
			return err
		}))
		return r
	}
	poison := reserve(time.Minute)
	var good []capital.Reservation
	for i := 0; i < 3; i++ {
		good = append(good, reserve(5*time.Minute))
	}
	poisonRow(t, "asset_reservations", poison.ID.String(), true)
	job := NewCapitalExpiry(testDB, w.capital, w.clk, Settings{})
	status := func(r capital.Reservation) capital.ReservationStatus {
		got, err := w.capital.Get(w.ctx, testDB, r.ID)
		require.NoError(t, err)
		return got.Status
	}

	w.clk.Advance(10 * time.Minute)
	rep := run(t, w.ctx, job)
	failed, ok := failureOf(rep, "reservation "+poison.ID.String())
	require.True(t, ok, "the poison is reported as its own unit: %+v", rep.Failures)
	assert.Equal(t, string(errs.CodeInternal), failed.Code)
	assert.NotContains(t, failed.Reason, "itest poison", "the database's words go to the log, not the report")
	assert.GreaterOrEqual(t, rep.Changed, len(good))
	for _, r := range good {
		assert.Equal(t, capital.ReservationExpired, status(r), "every good reservation expires in the one run")
	}
	assert.Equal(t, capital.ReservationActive, status(poison))

	fresh := reserve(10 * time.Second)
	w.clk.Advance(20 * time.Second)
	rep = run(t, w.ctx, job)
	_, again := failureOf(rep, "reservation "+poison.ID.String())
	assert.False(t, again, "not tried again inside its backoff")
	assert.Equal(t, capital.ReservationExpired, status(fresh), "a second run still expires new due reservations")

	w.clk.Advance(2 * time.Minute)
	rep = run(t, w.ctx, job)
	_, again = failureOf(rep, "reservation "+poison.ID.String())
	assert.True(t, again, "reported again once its backoff passed")
}

// TestIntegration_ACloseRunReportsTheMarketItCannotCloseAndClosesTheRest: a
// due prediction market the database refuses to close plus N good ones. One
// run of the close job closes every good one and reports the poison as that
// market's failure; a second run closes a new due market without reporting
// the poison again while its backoff runs.
func TestIntegration_ACloseRunReportsTheMarketItCannotCloseAndClosesTheRest(t *testing.T) {
	w := newWorld(t)
	poison := w.market(30 * time.Minute)
	var good []predmarket.Market
	for i := 0; i < 3; i++ {
		good = append(good, w.market(time.Hour))
	}
	poisonRow(t, "prediction_markets", poison.ID.String(), false)
	job := NewPredictionClose(testDB, w.predictions, Settings{})

	w.clk.Advance(2 * time.Hour)
	rep := run(t, w.ctx, job)
	_, ok := failureOf(rep, "market "+poison.ID.String())
	require.True(t, ok, "the poison is reported as its own unit: %+v", rep.Failures)
	for _, m := range good {
		assert.Equal(t, predmarket.StatusClosed, w.marketStatus(m.ID), "every good market closes in the one run")
	}
	assert.Equal(t, predmarket.StatusOpen, w.marketStatus(poison.ID))

	fresh := w.market(10 * time.Second)
	w.clk.Advance(20 * time.Second)
	rep = run(t, w.ctx, job)
	_, again := failureOf(rep, "market "+poison.ID.String())
	assert.False(t, again, "not tried again inside its backoff")
	assert.Equal(t, predmarket.StatusClosed, w.marketStatus(fresh.ID), "a second run still closes new due markets")
}

// TestIntegration_TheDelegationUpkeepNeverTouchesAnotherNetworksSwap: the
// delegation upkeep of one devnet network neither ends nor observes a swap
// admitted on another -- that network's own upkeep does -- and reports a
// swap of its own network it cannot end as that swap's failure while ending
// the others.
func TestIntegration_TheDelegationUpkeepNeverTouchesAnotherNetworksSwap(t *testing.T) {
	w := newWorld(t)
	here := w.delegationWorld()
	there := w.delegationWorld()
	require.NotEqual(t, here.svc.Network(), there.svc.Network())
	foreigner := there.delegator(10_000_000)
	foreign := there.swap(foreigner, there.grant(foreigner).ID, 1, 1_000_000)
	local := here.delegator(10_000_000)
	capability := here.grant(local)
	poison := here.swap(local, capability.ID, 1, 1_000_000)
	good := here.swap(local, capability.ID, 2, 1_000_000)
	poisonRow(t, "delegated_swaps", poison.ID.String(), false)
	here.advance(3 * time.Minute)
	job, err := NewDelegationUpkeep(testDB, here.svc, w.clk, Settings{})
	require.NoError(t, err)

	rep := run(t, w.ctx, job)
	_, failed := failureOf(rep, "swap "+poison.ID.String())
	assert.True(t, failed, "its own unhandled swap is reported: %+v", rep.Failures)
	assert.Equal(t, delegation.SwapRejected, here.getSwap(good.ID).Status, "and the others are ended")
	assert.Equal(t, delegation.SwapAdmitted, there.getSwap(foreign.ID).Status, "another network's swap is that network's to end")
	assert.False(t, failedUnit(rep, "swap "+foreign.ID.String()))

	own, err := NewDelegationUpkeep(testDB, there.svc, w.clk, Settings{})
	require.NoError(t, err)
	rep = run(t, w.ctx, own)
	assert.False(t, failedUnit(rep, "swap "+foreign.ID.String()), "%+v", rep.Failures)
	assert.Equal(t, delegation.SwapRejected, there.getSwap(foreign.ID).Status, "its own network ends it")
}
