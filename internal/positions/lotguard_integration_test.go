//go:build integration

package positions

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
)

// A consumed lot is not refilled, and a lot's status agrees with its quantity
// (F-61).
//
// 00104 guards position_lots on per-row arithmetic and on nothing relational.
// quantity_original had to be positive and quantity_open had to sit between
// zero and it, but nothing said the status agreed with the quantity, nothing
// said the open quantity only ever fell, and cp_app held table-wide UPDATE. So
// `UPDATE position_lots SET quantity_open = quantity_original` satisfied every
// constraint and every trigger and refilled a fully consumed lot -- one whose
// cost basis a realized-P&L figure is computed from.
//
// The asymmetry is what makes it a finding rather than a preference:
// credit_lots, doing the same job for Credits, gets a trigger raising CR001
// CREDIT_LOT_OVERCONSUMED. The two lot tables were written to different
// standards, and the weaker one holds the tax lots.
//
// No test wrote an invalid lot through SQL before this. Every position_lots
// reference in a test was a SELECT, and the concurrency test drives Dispose --
// so it proved the Go compare-and-set and said nothing about the database.

// openOwnerDB connects as the owning role. cp_app cannot reach the acquisition
// columns at all once the grant is narrowed, so a test that drove it would pass
// on SQLSTATE 42501 and never see the trigger it names.
func openOwnerDB(t *testing.T) *db.DB {
	t.Helper()
	url := os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	if url == "" {
		t.Skip("CP_TEST_MIGRATE_DATABASE_URL not set; skipping (provision one with `go run ./scripts/testdb -name positions`)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := db.Open(ctx, db.Config{URL: url, AppName: "positions-itest-owner", MaxConns: 2})
	require.NoError(t, err)
	t.Cleanup(d.Close)
	return d
}

// lotWorld seeds an account, an asset and one open lot of 1000 base units.
func lotWorld(t *testing.T, d *db.DB, e *Engine) (accounts.AccountID, assets.Asset, Lot) {
	t.Helper()
	acct := seedAccount(t, d)
	sol := seedAsset(t, d, "SOL", 9)
	lot := acquire(t, d, e, AcquireLot{
		AccountID: acct, AssetID: sol.ID, Quantity: q(t, "1000"), AcquiredAt: t0,
		Cost: usd(t, "100.00"), BasisSource: "fill", ValuationSource: "x",
		AcquisitionRef: Ref{Type: "fill", ID: "guard-" + t.Name()},
	})
	return acct, sol, lot
}

func ownerExec(t *testing.T, owner *db.DB, sql string, args ...any) error {
	t.Helper()
	return owner.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx, sql, args...)
			return e
		})
}

func TestIntegration_ADisposedLotCannotBeRefilled(t *testing.T) {
	d, owner, e := openTestDB(t), openOwnerDB(t), NewEngine()
	acct, sol, lot := lotWorld(t, d, e)

	_, _, err := dispose(t, d, e, Disposal{
		AccountID: acct, AssetID: sol.ID, Quantity: q(t, "1000"), DisposedAt: t0.Add(time.Hour),
		Proceeds: usd(t, "150.00"), ValuationSource: "x", DispositionRef: Ref{Type: "fill", ID: "d1"},
	})
	require.NoError(t, err)

	var open, status string
	require.NoError(t, d.QueryRow(t.Context(),
		`SELECT quantity_open::text, status FROM position_lots WHERE id = $1`, lot.ID).Scan(&open, &status))
	require.Equal(t, "0", open)
	require.Equal(t, "CLOSED", status)

	// The refill, as the owner -- so a refusal is the trigger rather than the
	// narrowed grant, which is checked separately below.
	err = ownerExec(t, owner,
		`UPDATE position_lots SET quantity_open = quantity_original, status = 'OPEN' WHERE id = $1`, lot.ID)
	require.Error(t, err, "a fully consumed lot was refilled")
	assert.Contains(t, err.Error(), "POSITION_LOT_REFILLED")
	assert.Equal(t, "PL001", db.SQLState(err), "got %v", err)

	// A smaller refill is the same rule and is easier to miss by eye.
	err = ownerExec(t, owner,
		`UPDATE position_lots SET quantity_open = 1, status = 'OPEN' WHERE id = $1`, lot.ID)
	require.Error(t, err)
	assert.Equal(t, "PL001", db.SQLState(err), "got %v", err)
}

// TestIntegration_ALotsAcquisitionFactsAreFixed: the columns a FIFO ordering
// and a cost basis are computed from cannot be edited after the fact. Changing
// acquired_at reorders every disposal that has not happened yet; changing
// cost_basis_usd_minor rewrites a realized gain that was already reported.
func TestIntegration_ALotsAcquisitionFactsAreFixed(t *testing.T) {
	d, owner, e := openTestDB(t), openOwnerDB(t), NewEngine()
	_, _, lot := lotWorld(t, d, e)

	for _, tc := range []struct{ name, set string }{
		{"original quantity", `quantity_original = 2000`},
		{"acquisition time", `acquired_at = acquired_at - interval '1 day'`},
		{"cost basis", `cost_basis_usd_minor = cost_basis_usd_minor + 1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ownerExec(t, owner, `UPDATE position_lots SET `+tc.set+` WHERE id = $1`, lot.ID)
			require.Error(t, err, "a lot's %s was edited after acquisition", tc.name)
			assert.Equal(t, "PL001", db.SQLState(err), "got %v", err)
		})
	}
}

// TestIntegration_ALotsStatusAgreesWithItsQuantity is the CHECK rather than the
// trigger, so it raises 23514 rather than a custom code. Both halves matter: an
// OPEN lot with nothing left is picked up by the FIFO query and contributes
// nothing, and a CLOSED lot with quantity left is value the disposal walk can
// never reach.
func TestIntegration_ALotsStatusAgreesWithItsQuantity(t *testing.T) {
	d, owner, e := openTestDB(t), openOwnerDB(t), NewEngine()
	acct, sol, lot := lotWorld(t, d, e)

	// CLOSED while quantity remains. quantity_open is untouched, so the refill
	// trigger has no opinion and the CHECK is the only thing standing here.
	err := ownerExec(t, owner, `UPDATE position_lots SET status = 'CLOSED' WHERE id = $1`, lot.ID)
	require.Error(t, err, "a lot was closed with quantity still open")
	assert.Contains(t, err.Error(), "position_lots_status_matches_quantity")

	// OPEN with nothing left, reached by disposing everything through the real
	// path and then trying to reopen the status alone.
	_, _, err = dispose(t, d, e, Disposal{
		AccountID: acct, AssetID: sol.ID, Quantity: q(t, "1000"), DisposedAt: t0.Add(time.Hour),
		Proceeds: usd(t, "150.00"), ValuationSource: "x", DispositionRef: Ref{Type: "fill", ID: "d2"},
	})
	require.NoError(t, err)

	err = ownerExec(t, owner, `UPDATE position_lots SET status = 'OPEN' WHERE id = $1`, lot.ID)
	require.Error(t, err, "an empty lot was marked OPEN")
	assert.Contains(t, err.Error(), "position_lots_status_matches_quantity")
}

// TestIntegration_TheApplicationRoleCannotTouchAcquisitionFacts is the
// privilege half. The trigger covers every role; the grant refuses the
// application before a trigger is reached, which is the house pattern (00604,
// 00701, 00712, 00713) and means a mistake in either mechanism is not enough on
// its own.
func TestIntegration_TheApplicationRoleCannotTouchAcquisitionFacts(t *testing.T) {
	d, e := openTestDB(t), NewEngine()
	_, _, lot := lotWorld(t, d, e)

	for _, set := range []string{
		`cost_basis_usd_minor = 1`,
		`acquired_at = now()`,
		`quantity_original = 5000`,
		`venue = 'somewhere-else'`,
	} {
		err := d.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted},
			func(ctx context.Context, tx pgx.Tx) error {
				_, e := tx.Exec(ctx, `UPDATE position_lots SET `+set+` WHERE id = $1`, lot.ID)
				return e
			})
		require.Error(t, err, "cp_app updated %q", set)
		assert.Equal(t, db.SQLStateInsufficientPrivilege, db.SQLState(err), "got %v", err)
	}
}

// TestIntegration_DisposalStillWorksUnderTheGuard is the positive control, and
// the reason the grant names exactly two columns rather than being reasoned
// about. A guard that refused everything, or a grant one column too narrow,
// would pass every assertion above and break the only path that writes these
// rows -- and the failure would surface as disposals not happening rather than
// as an error anybody was looking for.
func TestIntegration_DisposalStillWorksUnderTheGuard(t *testing.T) {
	d, e := openTestDB(t), NewEngine()
	acct, sol, lot := lotWorld(t, d, e)

	// A partial disposal: quantity falls, status stays OPEN.
	_, _, err := dispose(t, d, e, Disposal{
		AccountID: acct, AssetID: sol.ID, Quantity: q(t, "400"), DisposedAt: t0.Add(time.Hour),
		Proceeds: usd(t, "60.00"), ValuationSource: "x", DispositionRef: Ref{Type: "fill", ID: "p1"},
	})
	require.NoError(t, err)
	var open, status string
	require.NoError(t, d.QueryRow(t.Context(),
		`SELECT quantity_open::text, status FROM position_lots WHERE id = $1`, lot.ID).Scan(&open, &status))
	assert.Equal(t, "600", open)
	assert.Equal(t, "OPEN", status)

	// And the rest: the quantity reaches zero and the status becomes CLOSED in
	// the one UPDATE the CHECK requires them to agree in.
	_, _, err = dispose(t, d, e, Disposal{
		AccountID: acct, AssetID: sol.ID, Quantity: q(t, "600"), DisposedAt: t0.Add(2 * time.Hour),
		Proceeds: usd(t, "90.00"), ValuationSource: "x", DispositionRef: Ref{Type: "fill", ID: "p2"},
	})
	require.NoError(t, err)
	require.NoError(t, d.QueryRow(t.Context(),
		`SELECT quantity_open::text, status FROM position_lots WHERE id = $1`, lot.ID).Scan(&open, &status))
	assert.Equal(t, "0", open)
	assert.Equal(t, "CLOSED", status)

	// updated_at moved, which is what says set_updated_at still fires without
	// cp_app holding a grant on that column -- the thing the migration claims
	// was verified rather than reasoned about.
	var moved bool
	require.NoError(t, d.QueryRow(t.Context(),
		`SELECT updated_at > created_at FROM position_lots WHERE id = $1`, lot.ID).Scan(&moved))
	assert.True(t, moved, "set_updated_at did not fire; the column grant is too narrow after all")
}
