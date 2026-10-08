//go:build integration

package capital

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
)

// poisonRow makes the database refuse every update of one row of table --
// at once (a BEFORE UPDATE trigger) or only at COMMIT (a deferred constraint
// trigger) -- until the test ends: an item that can never be processed. It
// needs the schema owner, so it connects with the migration role.
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

func (f *fixture) expireBatch(limit int) ExpiryBatch {
	f.t.Helper()
	var out ExpiryBatch
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.ExpireDueBatch(ctx, tx, f.clk.Now(), limit)
		return err
	}))
	return out
}

func failureOf(b ExpiryBatch, rid ReservationID) (ExpiryFailure, bool) {
	for _, f := range b.Failures {
		if f.ReservationID == rid {
			return f, true
		}
	}
	return ExpiryFailure{}, false
}

func (f *fixture) upkeepRow(rid ReservationID) (failures int, cleared bool) {
	f.t.Helper()
	require.NoError(f.t, testDB.QueryRow(f.ctx, `SELECT failures, cleared_at IS NOT NULL FROM upkeep_failures
		WHERE operation = $1 AND item_id = $2`, UpkeepExpireDue, rid).Scan(&failures, &cleared))
	return failures, cleared
}

// TestIntegration_AReservationThatCannotExpireNeverStopsTheOthers: one due
// reservation the database refuses to expire -- one at once, one only at
// COMMIT -- plus N good ones. In one run every good one expires and each
// poison is rolled back alone and reported with its backoff; a second run
// expires new due reservations without retrying the poison inside its
// backoff; once the backoff has passed the poison is tried again and reported
// again (ExpireDue, the Reserver entry point, too, never fails for it); and
// once it can be expired, it is, and its run of failures is cleared.
func TestIntegration_AReservationThatCannotExpireNeverStopsTheOthers(t *testing.T) {
	requireEnv(t)
	f := newFixture(t)
	// A clock of its own, years before every other test's: the only due
	// reservations are this test's.
	f.clk.Set(time.Date(2020, 1, 6, 9, 0, 0, 0, time.UTC))
	f.seedWallet(f.accountID, qty(1_000_000))
	reserve := func(ttl time.Duration) Reservation {
		req := f.request(f.accountID, 10, 1, nil)
		req.TTL = ttl
		return f.mustReserve(req)
	}
	// The poisons expire first, so the batch meets them first.
	poisonNow := reserve(time.Minute)
	poisonAtCommit := reserve(time.Minute)
	var good []ReservationID
	for i := 0; i < 5; i++ {
		good = append(good, reserve(5*time.Minute).ID)
	}
	poisonRow(t, "asset_reservations", poisonNow.ID.String(), false)
	poisonRow(t, "asset_reservations", poisonAtCommit.ID.String(), true)

	f.clk.Advance(10 * time.Minute)
	first := f.expireBatch(100)
	assert.ElementsMatch(t, good, first.Expired, "every good reservation expires in the one run")
	require.Len(t, first.Failures, 2, "%+v", first.Failures)
	for _, poison := range []Reservation{poisonNow, poisonAtCommit} {
		failed, ok := failureOf(first, poison.ID)
		require.True(t, ok, "the poison %s is reported", poison.ID)
		assert.Equal(t, 1, failed.Attempts)
		assert.True(t, failed.RetryAfter.Equal(f.clk.Now().Add(time.Minute)), "first backoff is a minute: %s", failed.RetryAfter)
		assert.Equal(t, errs.CodeInternal, errs.CodeOf(failed.Err))
		assert.Equal(t, ReservationActive, f.get(poison.ID).Status, "rolled back alone")
		n, cleared := f.upkeepRow(poison.ID)
		assert.Equal(t, 1, n)
		assert.False(t, cleared)
	}
	for _, rid := range good {
		assert.Equal(t, ReservationExpired, f.get(rid).Status)
	}

	// A second run inside the backoff: new due reservations expire, and the
	// poisons are not even tried.
	var fresh []ReservationID
	for i := 0; i < 3; i++ {
		fresh = append(fresh, reserve(10*time.Second).ID)
	}
	f.clk.Advance(20 * time.Second)
	second := f.expireBatch(1)
	assert.Len(t, second.Expired, 1, "a batch of one is never spent on a poison in its backoff")
	assert.Empty(t, second.Failures)
	third := f.expireBatch(100)
	assert.ElementsMatch(t, fresh, append(second.Expired, third.Expired...))
	assert.Empty(t, third.Failures)
	n, _ := f.upkeepRow(poisonNow.ID)
	assert.Equal(t, 1, n, "not retried inside its backoff")

	// After the backoff the poisons are tried again, after every never-failed
	// due reservation, and reported again with a doubled backoff; ExpireDue
	// returns what it expired and no error.
	last := reserve(time.Second)
	f.clk.Advance(time.Minute)
	var expired []ReservationID
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		expired, err = f.svc.ExpireDue(ctx, tx, f.clk.Now(), 100)
		return err
	}))
	assert.Equal(t, []ReservationID{last.ID}, expired)
	n, _ = f.upkeepRow(poisonNow.ID)
	assert.Equal(t, 2, n, "tried again once its backoff passed")
	var retryAfter time.Time
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT retry_after FROM upkeep_failures WHERE operation = $1 AND item_id = $2`,
		UpkeepExpireDue, poisonAtCommit.ID).Scan(&retryAfter))
	assert.True(t, retryAfter.Equal(f.clk.Now().Add(2*time.Minute)), "the backoff doubles: %s", retryAfter)
}

// TestIntegration_TheUpkeepFailureLedgerCannotBeRewritten: the database keeps
// the upkeep failure ledger honest whatever the code does. The application
// role writes it only through its functions' columns -- it can delete
// nothing and cannot move a failure to another item or operation -- the
// schema owner cannot either (UK001), and a backoff always lies after the
// failure it follows.
func TestIntegration_TheUpkeepFailureLedgerCannotBeRewritten(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	item := NewReservationID()
	var attempts int
	var retryAfter time.Time
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	require.NoError(t, testDB.QueryRow(ctx, upkeepFailedSQL, uuid.New(), UpkeepExpireDue, item, at, "CONFLICT").Scan(&attempts, &retryAfter))
	assert.Equal(t, 1, attempts)
	assert.True(t, retryAfter.Equal(at.Add(time.Minute)))

	_, err := testDB.Exec(ctx, `DELETE FROM upkeep_failures WHERE item_id = $1`, item)
	assert.Equal(t, "42501", db.SQLState(err), "cp_app deletes nothing: %v", err)
	_, err = testDB.Exec(ctx, `UPDATE upkeep_failures SET item_id = $2 WHERE item_id = $1`, item, NewReservationID())
	assert.Equal(t, "42501", db.SQLState(err), "cp_app cannot move a failure to another item: %v", err)
	_, err = testDB.Exec(ctx, `UPDATE upkeep_failures SET retry_after = last_failed_at WHERE item_id = $1`, item)
	assert.Equal(t, "23514", db.SQLState(err), "a backoff lies after its failure: %v", err)

	owner, err := pgx.Connect(ctx, testMigrateURL)
	require.NoError(t, err)
	defer func() { _ = owner.Close(ctx) }()
	_, err = owner.Exec(ctx, `UPDATE upkeep_failures SET operation = 'capital.something_else' WHERE item_id = $1`, item)
	assert.Equal(t, "UK001", db.SQLState(err), "not even the owner moves a failure to another operation: %v", err)
	_, err = owner.Exec(ctx, `UPDATE upkeep_failures SET item_id = $2 WHERE item_id = $1`, item, NewReservationID())
	assert.Equal(t, "UK001", db.SQLState(err), "or to another item: %v", err)

	require.NoError(t, testDB.QueryRow(ctx, upkeepFailedSQL, uuid.New(), UpkeepExpireDue, item, at.Add(time.Minute), "CONFLICT").
		Scan(&attempts, &retryAfter))
	assert.Equal(t, 2, attempts, "one row per item: a second failure is the second of its run")
	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM upkeep_failures WHERE operation = $1 AND item_id = $2`, UpkeepExpireDue, item))
}

func countRows(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

// TestIntegration_AReservationThatRecoversClearsItsFailures: a reservation
// that failed to expire and then can be expired is expired on its next try
// after the backoff, and its run of failures is closed (cleared, never
// deleted).
func TestIntegration_AReservationThatRecoversClearsItsFailures(t *testing.T) {
	requireEnv(t)
	f := newFixture(t)
	f.clk.Set(time.Date(2020, 2, 3, 9, 0, 0, 0, time.UTC))
	f.seedWallet(f.accountID, qty(1_000_000))
	req := f.request(f.accountID, 10, 1, nil)
	req.TTL = time.Minute
	r := f.mustReserve(req)
	t.Run("poisoned", func(t *testing.T) {
		poisonRow(t, "asset_reservations", r.ID.String(), true)
		f.clk.Advance(2 * time.Minute)
		b := f.expireBatch(100)
		_, ok := failureOf(b, r.ID)
		require.True(t, ok)
	})
	f.clk.Advance(2 * time.Minute)
	b := f.expireBatch(100)
	assert.Equal(t, []ReservationID{r.ID}, b.Expired)
	assert.Equal(t, ReservationExpired, f.get(r.ID).Status)
	n, cleared := f.upkeepRow(r.ID)
	assert.Equal(t, 1, n)
	assert.True(t, cleared, "the run of failures is over, and its history kept")
}
