//go:build integration

package migrations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/id"
)

// The four invariants whose SQLSTATEs 00722 withdraws are still enforced.
//
// CR002, CR005, PO002 and PO004 were documented in a migration header and
// raised by nothing (F-58). Withdrawing a code is only honest if the invariant
// it named still holds, so this drives all four and watches each refusal
// happen. Without it, "withdrawn" and "abandoned" would look identical from
// outside, and a future reader would have only 00722's word for it.
//
// Each assertion names the SQLSTATE that actually fires, which is the point: the
// property is enforced, under a different code than the header claimed.

func TestIntegration_TheWithdrawnInvariantsAreStillEnforced(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	admin := connect(t, migrateURL)

	// A migrated database carries the schema and no rows: migrations build the
	// tables, seeding is a separate job. So the Credit asset is written here,
	// and its acceptance is the positive control for the index that refuses the
	// second one below.
	//
	// It has to survive a second run against the same database, which is how
	// CONVENTIONS.md tells developers to work and how a `-keep` run behaves. A
	// fixture that can only run once is a fixture that passes in CI and fails
	// on the desk of whoever is debugging it.
	creditAsset := ensureCreditAsset(t, admin)

	t.Run("CR005 there is at most one Credit asset", func(t *testing.T) {
		second := id.New[id.Any]()
		_, err := admin.Exec(ctx, `INSERT INTO assets
			(id, chain, mint_address, kind, value_domain, symbol, name, decimals, is_stablecoin, risk_class, status)
			VALUES ($1, 'nodal-internal', $2, 'CREDIT', 'INTERNAL_CREDIT', 'CRD2', 'Second Credit', 6, false, 'STANDARD', 'ACTIVE')`,
			second, second.String())
		require.Error(t, err, "a second Credit asset was accepted")
		assert.Equal(t, "23505", db.SQLState(err), "got %v", err)
		assert.Contains(t, err.Error(), "assets_single_credit_asset")
	})

	t.Run("CR002 the credit tables carry the immutability trigger", func(t *testing.T) {
		// This one is a composition of two facts rather than one probe, and it
		// is worth being explicit about why.
		//
		// forbid_mutation's refusal is already driven directly by
		// TestIntegration_Migrations, which attaches it to a temp table, updates
		// and deletes, and asserts db.IsImmutableRow both times. What nothing
		// checked was that the three credit tables actually carry it -- a
		// trigger that exists and is attached to nothing refuses nothing.
		//
		// Driving a real credit lot needs a journal transaction, which needs
		// balanced entries, which is a fixture belonging to internal/credit
		// rather than to a migration test. So this asserts the attachment, and
		// the refusal is proven next door. Stated as two halves because that is
		// what it is.
		for _, tbl := range []string{"credit_lots", "credit_lot_events", "credit_funding_transitions"} {
			var fn string
			var events int16
			err := admin.QueryRow(ctx, `SELECT p.proname, t.tgtype
				  FROM pg_trigger t
				  JOIN pg_proc p ON p.oid = t.tgfoid
				 WHERE t.tgrelid = $1::regclass AND NOT t.tgisinternal
				   AND p.proname = 'forbid_mutation'`, tbl).Scan(&fn, &events)
			require.NoError(t, err, "%s carries no forbid_mutation trigger", tbl)
			assert.Equal(t, "forbid_mutation", fn)
			// tgtype bits: 1 = ROW, 2 = BEFORE, 8 = UPDATE, 16 = DELETE.
			assert.NotZero(t, events&1, "%s: the trigger must be FOR EACH ROW", tbl)
			assert.NotZero(t, events&2, "%s: the trigger must be BEFORE", tbl)
			assert.NotZero(t, events&8, "%s: the trigger must cover UPDATE", tbl)
			assert.NotZero(t, events&16, "%s: the trigger must cover DELETE", tbl)
		}
	})

	t.Run("PO002 a payout state outside the set is refused", func(t *testing.T) {
		reqID := seedPayoutRequest(t, admin, creditAsset)
		_, err := admin.Exec(ctx, `UPDATE payout_requests SET state = 'ABSCONDED' WHERE id = $1`, reqID)
		require.Error(t, err, "a payout reached a state the schema does not define")
		assert.Equal(t, "23514", db.SQLState(err), "got %v", err)
	})

	t.Run("PO002 a legal state change still needs its transition row", func(t *testing.T) {
		reqID := seedPayoutRequest(t, admin, creditAsset)
		tx, err := admin.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		_, err = tx.Exec(ctx, `UPDATE payout_requests SET state = 'ELIGIBILITY_CHECK' WHERE id = $1`, reqID)
		require.NoError(t, err, "the UPDATE itself is fine; the binding is deferred to COMMIT")
		err = tx.Commit(ctx)
		require.Error(t, err, "a payout state changed with no transition row")
		assert.Equal(t, "AU001", db.SQLState(err), "got %v", err)
	})

	t.Run("PO004 a payout cannot settle more than it reserved", func(t *testing.T) {
		reqID := seedPayoutRequest(t, admin, creditAsset)
		_, err := admin.Exec(ctx,
			`UPDATE payout_requests SET reserved_quantity = 10, settled_quantity = 11 WHERE id = $1`, reqID)
		require.Error(t, err, "a payout settled more than it reserved")
		assert.Equal(t, "23514", db.SQLState(err), "got %v", err)

		_, err = admin.Exec(ctx,
			`UPDATE payout_requests SET reserved_quantity = requested_quantity + 1 WHERE id = $1`, reqID)
		require.Error(t, err, "a payout reserved more than was requested")
		assert.Equal(t, "23514", db.SQLState(err), "got %v", err)
	})

	// PO001, anchored where the number is written (00815, F-264).
	//
	// cp_payout_reservation_balanced is a constraint trigger on
	// payout_allocations, so it fires when an ALLOCATION is written and never
	// when a reserved_quantity is. A reservation with no allocation rows behind
	// it touched that table not at all and was therefore compared to nothing --
	// which is what let a same-state transition row write the whole requested
	// amount onto a REJECTED payout as reserved and settled.
	//
	// This case used to be the positive control below, written as
	// `SET reserved_quantity = 5, settled_quantity = 5`. It passed because the
	// invariant it violates had nowhere to fire. It is a refusal now.
	t.Run("PO001 a reservation with no allocations behind it is refused", func(t *testing.T) {
		reqID := seedPayoutRequest(t, admin, creditAsset)
		tx, err := admin.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		_, err = tx.Exec(ctx,
			`UPDATE payout_requests SET reserved_quantity = 5, settled_quantity = 5 WHERE id = $1`, reqID)
		require.NoError(t, err, "the UPDATE itself is fine; the invariant is deferred to COMMIT")
		err = tx.Commit(ctx)
		require.Error(t, err, "a payout reserved 5 with nothing allocated to it")
		assert.Equal(t, "PO001", db.SQLState(err), "got %v", err)
	})

	// The positive control. Every case above expects a refusal, so a schema
	// that refused everything -- a broken fixture, a missing grant -- would pass
	// all of them. A legal write must still work.
	//
	// It is a write of ZERO now, and that is not a weakening: zero IS the
	// balanced pair for a request with no allocations, which is every request
	// this file seeds, because seeding one with allocations means seeding a
	// credit lot, a journal transaction and its entries. The path that reserves
	// a non-zero quantity against real allocations is driven end to end by
	// internal/payout's own integration suite, which is where a fixture that
	// heavy belongs.
	t.Run("the legal writes still work", func(t *testing.T) {
		reqID := seedPayoutRequest(t, admin, creditAsset)
		_, err := admin.Exec(ctx,
			`UPDATE payout_requests
			    SET reserved_quantity = 0, settled_quantity = 0,
			        verification_level = 'PAYOUT_KYC', failure_reason = 'a legal write'
			  WHERE id = $1`, reqID)
		require.NoError(t, err, "a payout whose reservation balances its allocations was refused")

		var level, reason string
		require.NoError(t, admin.QueryRow(ctx,
			`SELECT verification_level, failure_reason FROM payout_requests WHERE id = $1`, reqID).
			Scan(&level, &reason))
		assert.Equal(t, "PAYOUT_KYC", level)
		assert.Equal(t, "a legal write", reason)
	})
}

// ensureCreditAsset returns the database's single Credit asset, writing it if
// there is none.
//
// The write is the positive control: an index that refused the FIRST Credit
// asset would make every refusal below vacuous. On a database that already
// carries one -- a second run, which the -keep flag and the local workflow both
// produce -- the existing row IS that evidence, so the control is satisfied
// either way and the test does not depend on being the first thing to touch the
// schema.
func ensureCreditAsset(t *testing.T, c *pgx.Conn) id.ID[id.Any] {
	t.Helper()
	ctx := context.Background()
	var existing id.ID[id.Any]
	switch err := c.QueryRow(ctx, `SELECT id FROM assets WHERE kind = 'CREDIT'`).Scan(&existing); {
	case err == nil:
		t.Log("a Credit asset already exists; it was accepted by a previous run")
		return existing
	case !errors.Is(err, pgx.ErrNoRows):
		require.NoError(t, err)
	}
	assetID := id.New[id.Any]()
	_, err := c.Exec(ctx, `INSERT INTO assets
		(id, chain, mint_address, kind, value_domain, symbol, name, decimals, is_stablecoin, risk_class, status)
		VALUES ($1, 'nodal-internal', $2, 'CREDIT', 'INTERNAL_CREDIT', 'CRD1', 'Nodal Credit', 6, false, 'STANDARD', 'ACTIVE')`,
		assetID, assetID.String())
	require.NoError(t, err, "the FIRST Credit asset must be accepted; an index that refused it would make every case below vacuous")
	return assetID
}

// seedPayoutRequest writes a minimal DRAFT payout request and returns its id.
func seedPayoutRequest(t *testing.T, c *pgx.Conn, creditAsset id.ID[id.Any]) id.ID[id.Any] {
	t.Helper()
	ctx := context.Background()
	account := seedAccountRow(t, c)
	reqID := id.New[id.Any]()
	_, err := c.Exec(ctx, `INSERT INTO payout_requests
		(id, account_id, credit_asset_id, state, requested_quantity, policy_version, policy_hash, idempotency_key)
		VALUES ($1, $2, $3, 'DRAFT', 100, 'v1', 'h1', $4)`,
		reqID, account, creditAsset, "payout-"+reqID.String())
	require.NoError(t, err)
	return reqID
}

// seedAccountRow writes a user and an account, which every fixture here needs.
func seedAccountRow(t *testing.T, c *pgx.Conn) id.ID[id.Any] {
	t.Helper()
	ctx := context.Background()
	userID, accountID := id.New[id.Any](), id.New[id.Any]()
	_, err := c.Exec(ctx, `INSERT INTO users (id, idp_issuer, idp_subject, status) VALUES ($1, 'test', $2, 'ACTIVE')`,
		userID, "sub-"+userID.String())
	require.NoError(t, err)
	_, err = c.Exec(ctx, `INSERT INTO accounts (id, owner_user_id, kind, status) VALUES ($1, $2, 'CUSTOMER', 'ACTIVE')`,
		accountID, userID)
	require.NoError(t, err)
	return accountID
}
