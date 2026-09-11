//go:build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/id"
)

// 00825 refuses to migrate a database whose provenance cannot be computed
// (F-283, D-137 amended).
//
// 00819 computes a lot's roots with a recursive query whose UNION makes a cycle
// terminate rather than recurse; what it terminates with is nothing, because in
// a cycle every node is somebody's child and the aggregate runs over an empty
// set. 00819's backfill then coalesced that NULL to the lot's OWN origin -- the
// mint-time rule for a lot with no parents, applied to a lot that has them --
// and for trading proceeds out of a promotional grant that is the permissive
// answer, in the migration whose subject is that a floor must be conservative.
//
// A cycle is unwritable through 00819's triggers, so this is reachable only in
// data restored from a deployment that ran on 00816 to 00818. That is exactly
// what this test builds: a database at 00824, a cycle written as the MIGRATION
// role with the user triggers off, and then the migration.
func TestIntegration_AProvenanceCycleStopsTheMigration(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	admin := connect(t, migrateURL)

	// Whatever happens below, leave the database fully migrated for the rest of
	// the suite, exactly as TestIntegration_Migrations does.
	t.Cleanup(func() { require.NoError(t, migrate.Up(context.Background(), migrateURL)) })

	resetSchema(t, admin)
	require.NoError(t, migrate.UpTo(ctx, migrateURL, 824))
	v, err := migrate.Version(ctx, migrateURL)
	require.NoError(t, err)
	require.Equal(t, int64(824), v, "the fixture starts one migration short")

	// Two lots and the edge that closes a cycle between them. Every write here
	// is the migration role's with the user triggers off, which is the only way
	// this shape can exist -- and is what a restore of a pre-00819 dump is.
	account := seedAccountRow(t, admin)
	asset := ensureCreditAsset(t, admin)
	txID := id.New[id.Any]()
	withTriggersOff(t, admin, "journal_transactions", func() {
		_, terr := admin.Exec(ctx, `INSERT INTO journal_transactions
			(id, kind, idempotency_key, reference_type, reference_id, effective_at,
			 posted_by_actor_type, posted_by_actor_id, content_hash)
			VALUES ($1,'SEED',$2,'test','cycle', now(), 'SYSTEM','test', '\x00'::bytea)`,
			txID, "cycle-"+txID.String())
		require.NoError(t, terr)
	})

	grant, proceeds := id.New[id.Any](), id.New[id.Any]()
	withTriggersOff(t, admin, "credit_lots", func() {
		for _, lot := range []struct {
			id     id.ID[id.Any]
			origin string
		}{{grant, "PROMOTIONAL"}, {proceeds, "MARKET_TRADING_PROCEEDS"}} {
			_, lerr := admin.Exec(ctx, `INSERT INTO credit_lots
				(id, account_id, asset_id, origin, initial_finality, quantity,
				 journal_transaction_id, issued_by_actor_type, issued_by_actor_id, reason)
				VALUES ($1,$2,$3,$4,'UNFUNDED',1000,$5,'SYSTEM','test','provenance cycle fixture')`,
				lot.id, account, asset, lot.origin, txID)
			require.NoError(t, lerr)
		}
	})
	withTriggersOff(t, admin, "credit_lot_parents", func() {
		_, perr := admin.Exec(ctx,
			`INSERT INTO credit_lot_parents (lot_id, parent_lot_id, quantity)
			 VALUES ($1,$2,1),($2,$1,1)`, proceeds, grant)
		require.NoError(t, perr)
	})

	// The state of the data, in the database's own words: the recursion answers
	// nothing for both lots.
	for _, lot := range []id.ID[id.Any]{grant, proceeds} {
		var roots []string
		require.NoError(t, admin.QueryRow(ctx, `SELECT cp_credit_lot_root_origins($1)`, lot).Scan(&roots))
		assert.Empty(t, roots, "a lot in a cycle has no reachable root")
	}

	// And the migration refuses rather than writing the guess.
	err = migrate.UpTo(ctx, migrateURL, 825)
	require.Error(t, err, "00825 must refuse a database whose provenance cannot be computed")
	assert.Contains(t, err.Error(), "CREDIT_PROVENANCE_UNKNOWABLE")
	assert.Contains(t, err.Error(), "must not be guessed")
	v, err = migrate.Version(ctx, migrateURL)
	require.NoError(t, err)
	assert.Equal(t, int64(824), v, "a refused migration leaves the schema where it was")

	// The repair is the operator's: break the cycle so that every lot with
	// parents has a reachable root. Then the migration applies.
	withTriggersOff(t, admin, "credit_lot_parents", func() {
		_, derr := admin.Exec(ctx,
			`DELETE FROM credit_lot_parents WHERE lot_id = $1 AND parent_lot_id = $2`, grant, proceeds)
		require.NoError(t, derr)
	})
	require.NoError(t, migrate.UpTo(ctx, migrateURL, 825),
		"with the cycle gone every provenance is computable and the schema moves")
	v, err = migrate.Version(ctx, migrateURL)
	require.NoError(t, err)
	assert.Equal(t, int64(825), v)

	// The function the refusal named is there for the operator who has to find
	// them, and it is empty on a database this build wrote.
	var remaining int
	require.NoError(t, admin.QueryRow(ctx,
		`SELECT count(*) FROM cp_credit_lots_without_computable_roots()`).Scan(&remaining))
	assert.Zero(t, remaining)

	require.NoError(t, migrate.Up(ctx, migrateURL))
	require.NoError(t, migrate.Verify(ctx, migrateURL))
}

// withTriggersOff runs fn with the table's USER triggers disabled, as only the
// migration role can. It is how a restore of data written before a trigger
// existed is simulated, and it always turns them back on.
func withTriggersOff(t *testing.T, admin *pgx.Conn, table string, fn func()) {
	t.Helper()
	ctx := context.Background()
	_, err := admin.Exec(ctx, `ALTER TABLE `+table+` DISABLE TRIGGER USER`)
	require.NoError(t, err)
	defer func() {
		_, eerr := admin.Exec(ctx, `ALTER TABLE `+table+` ENABLE TRIGGER USER`)
		require.NoError(t, eerr)
	}()
	fn()
}
