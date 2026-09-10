//go:build integration

package migrations_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/id"
)

// A transition row licenses only the change it describes, on every audited
// entity (F-94, migration 00731).
//
// 00603 bound a state change to a transition row and compared one thing: the
// row's DESTINATION against the entity's new value. Nothing read from_status or
// from_state anywhere in the schema, so a row recording an origin the entity was
// never in committed cleanly and licensed a change it did not describe.
//
// 00712 then widened it further. The flag began accumulating and the check
// became membership, with the header claiming "Membership is exactly as strong
// as equality was". Under equality only the LAST flagged destination satisfied
// the check; under membership any of them does, so two rows written in one
// transaction license a single jump that skips the middle.
//
// Both are closed by binding the EDGE. These cases are written against
// `accounts` because it is the cheapest entity to construct; the property is
// the trigger pair's, and TestIntegration_EveryAuditedEntityBindsTheEdge below
// asserts every table carries it.
func TestIntegration_ATransitionRowCannotDenyTheChangeItLicenses(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	require.NoError(t, migrate.Up(ctx, migrateURL))
	app := connect(t, appURL)

	newAccount := func() id.ID[id.Any] {
		userID, accountID := id.New[id.Any](), id.New[id.Any]()
		_, err := app.Exec(ctx, `INSERT INTO users (id, idp_issuer, idp_subject, status) VALUES ($1, 'edge-test', $2, 'ACTIVE')`,
			userID, "sub-"+userID.String())
		require.NoError(t, err)
		_, err = app.Exec(ctx, `INSERT INTO accounts (id, owner_user_id, kind, status) VALUES ($1, $2, 'CUSTOMER', 'ACTIVE')`,
			accountID, userID)
		require.NoError(t, err)
		return accountID
	}
	inTx := func(fn func(tx pgx.Tx) error) error {
		tx, err := app.Begin(ctx)
		require.NoError(t, err)
		if err := fn(tx); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		return tx.Commit(ctx)
	}
	transition := func(tx pgx.Tx, acct id.ID[id.Any], from, to, reason string) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO account_status_transitions (id, account_id, from_status, to_status, actor_type, actor_id, reason)
			 VALUES ($1, $2, $3, $4, 'OPERATOR', 'op', $5)`,
			id.New[id.Any](), acct, from, to, reason)
		return err
	}
	statusOf := func(acct id.ID[id.Any]) string {
		var s string
		require.NoError(t, app.QueryRow(ctx, `SELECT status FROM accounts WHERE id = $1`, acct).Scan(&s))
		return s
	}

	t.Run("a row claiming the entity never moved licenses no move", func(t *testing.T) {
		acct := newAccount()
		err := inTx(func(tx pgx.Tx) error {
			// F-78's shape: from == to, so any CHECK keyed on "did the state
			// change" is satisfied, and the destination matches what we are
			// about to write.
			if err := transition(tx, acct, "FROZEN", "FROZEN", "nothing moved"); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE accounts SET status = 'FROZEN' WHERE id = $1`, acct)
			return err
		})
		require.Error(t, err, "a row saying nothing moved licensed a move")
		assert.Equal(t, "AU001", db.SQLState(err), "got %v", err)
		assert.Equal(t, "ACTIVE", statusOf(acct))
	})

	t.Run("a row claiming an origin the entity was never in licenses nothing", func(t *testing.T) {
		acct := newAccount()
		err := inTx(func(tx pgx.Tx) error {
			// The destination is right and the origin is a fiction. Under the
			// destination-only binding this committed, and the trail then said
			// the account had been RESTRICTED when it never was.
			if err := transition(tx, acct, "RESTRICTED", "FROZEN", "invented origin"); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE accounts SET status = 'FROZEN' WHERE id = $1`, acct)
			return err
		})
		require.Error(t, err, "a row with a forged origin licensed the change")
		assert.Equal(t, "AU001", db.SQLState(err), "got %v", err)
		assert.Equal(t, "ACTIVE", statusOf(acct))
	})

	t.Run("two rows do not license a jump that skips the middle", func(t *testing.T) {
		acct := newAccount()
		err := inTx(func(tx pgx.Tx) error {
			// This is 00712's widening: under membership over DESTINATIONS,
			// 'FROZEN' is flagged and the single update to FROZEN was allowed,
			// while the trail claims two steps the entity never took.
			if err := transition(tx, acct, "ACTIVE", "RESTRICTED", "step one"); err != nil {
				return err
			}
			if err := transition(tx, acct, "RESTRICTED", "FROZEN", "step two"); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE accounts SET status = 'FROZEN' WHERE id = $1`, acct)
			return err
		})
		require.Error(t, err, "two rows licensed a jump neither of them describes")
		assert.Equal(t, "AU001", db.SQLState(err), "got %v", err)
		assert.Equal(t, "ACTIVE", statusOf(acct))
	})

	// F-101. The two above are forgeries the edge CHECK refuses. These two are
	// the same forgeries written so that the check never sees them.
	//
	// The edge is encoded `<from>` `>` `<to>` and the edges of one transaction
	// are joined with `|`. Both delimiters are in band, and from_status and
	// to_status are unconstrained text on fourteen of the fifteen bound tables.
	// So a single row carrying them splits the flag into more elements than the
	// row describes -- and one of those elements can be an edge nobody wrote
	// down. 00731's header reasoned that licensing an extra edge takes more than
	// one row, each describing its own step. That was true of honest rows only.
	//
	// Both were observed committing against the pre-00732 function; the
	// undelimited form of the same forgery is refused in the same session.

	t.Run("a destination carrying the delimiters licenses nothing", func(t *testing.T) {
		acct := newAccount()
		err := inTx(func(tx pgx.Tx) error {
			// flag  = 'RESTRICTED>FROZEN|ACTIVE>FROZEN'
			// split = {'RESTRICTED>FROZEN', 'ACTIVE>FROZEN'}  <- the second is free
			if err := transition(tx, acct, "RESTRICTED", "FROZEN|ACTIVE>FROZEN", "delimiter in the destination"); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE accounts SET status = 'FROZEN' WHERE id = $1`, acct)
			return err
		})
		require.Error(t, err, "one row licensed an edge it does not describe")
		assert.Equal(t, "AU001", db.SQLState(err), "got %v", err)
		assert.Equal(t, "ACTIVE", statusOf(acct))
	})

	t.Run("an origin carrying the delimiters licenses nothing", func(t *testing.T) {
		acct := newAccount()
		err := inTx(func(tx pgx.Tx) error {
			// The same injection from the other end, which is why the guard
			// reads both columns rather than the destination it was written for.
			// flag  = 'RESTRICTED|ACTIVE>FROZEN'
			// split = {'RESTRICTED', 'ACTIVE>FROZEN'}
			if err := transition(tx, acct, "RESTRICTED|ACTIVE", "FROZEN", "delimiter in the origin"); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE accounts SET status = 'FROZEN' WHERE id = $1`, acct)
			return err
		})
		require.Error(t, err, "a forged origin licensed the change it was built to name")
		assert.Equal(t, "AU001", db.SQLState(err), "got %v", err)
		assert.Equal(t, "ACTIVE", statusOf(acct))
	})

	// The controls. A binding one condition too strict refuses every real
	// change, and the five refusals above would all still pass.
	t.Run("an honest single step commits", func(t *testing.T) {
		acct := newAccount()
		require.NoError(t, inTx(func(tx pgx.Tx) error {
			if err := transition(tx, acct, "ACTIVE", "FROZEN", "compliance hold"); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE accounts SET status = 'FROZEN' WHERE id = $1`, acct)
			return err
		}))
		assert.Equal(t, "FROZEN", statusOf(acct))
	})

	t.Run("an honest two-step transaction commits both steps", func(t *testing.T) {
		// The reason the edge flag accumulates. 00712 made the destination flag
		// accumulate because a flow that submits and approves in one call --
		// creating and launching a native asset -- performs two updates, and
		// under a single-valued flag the first was refused at commit. Edges
		// accumulate for the same reason and give nothing back, because an edge
		// names both ends.
		acct := newAccount()
		require.NoError(t, inTx(func(tx pgx.Tx) error {
			if err := transition(tx, acct, "ACTIVE", "RESTRICTED", "step one"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE accounts SET status = 'RESTRICTED' WHERE id = $1`, acct); err != nil {
				return err
			}
			if err := transition(tx, acct, "RESTRICTED", "FROZEN", "step two"); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE accounts SET status = 'FROZEN' WHERE id = $1`, acct)
			return err
		}))
		assert.Equal(t, "FROZEN", statusOf(acct))
	})

	t.Run("one entity's row does not license another's change", func(t *testing.T) {
		mine, theirs := newAccount(), newAccount()
		err := inTx(func(tx pgx.Tx) error {
			if err := transition(tx, theirs, "ACTIVE", "FROZEN", "somebody else"); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE accounts SET status = 'FROZEN' WHERE id = $1`, mine)
			return err
		})
		require.Error(t, err, "one account's transition row licensed another account's change")
		assert.Equal(t, "AU001", db.SQLState(err), "got %v", err)
		assert.Equal(t, "ACTIVE", statusOf(mine))
	})
}

// TestIntegration_EveryAuditedEntityBindsTheEdge: the property above belongs to
// every audited entity, not to accounts.
//
// It reads pg_trigger rather than a list typed out here, so a table that gains a
// destination-only binding tomorrow fails this rather than joining the set
// quietly. kill_switches is the one deliberate exception and it says why.
func TestIntegration_EveryAuditedEntityBindsTheEdge(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	require.NoError(t, migrate.Up(ctx, migrateURL))
	app := connect(t, appURL)

	rows, err := app.Query(ctx, `
		SELECT c.relname, regexp_replace(pg_get_triggerdef(t.oid), '.*EXECUTE FUNCTION ', '')
		  FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
		 WHERE NOT t.tgisinternal
		   AND pg_get_triggerdef(t.oid) LIKE '%cp_require_transition(%'
		 ORDER BY 1`)
	require.NoError(t, err)
	defer rows.Close()

	var stragglers []string
	for rows.Next() {
		var table, def string
		require.NoError(t, rows.Scan(&table, &def))
		if table == "kill_switches" {
			// kill_switch_transitions has no from_active column and needs none:
			// for a two-valued column the destination determines the origin, so
			// the destination form is already edge-complete.
			continue
		}
		stragglers = append(stragglers, fmt.Sprintf("%s -> %s", table, def))
	}
	require.NoError(t, rows.Err())
	assert.Empty(t, stragglers,
		"these entities still bind only the destination of a transition, so a row recording an origin "+
			"they were never in licenses a change it does not describe (F-94)")

	// The positive control: the edge form is actually in use. Without it this
	// test would pass on a schema that had dropped every binding.
	var edges int
	require.NoError(t, app.QueryRow(ctx, `
		SELECT count(*) FROM pg_trigger t
		 WHERE NOT t.tgisinternal
		   AND pg_get_triggerdef(t.oid) LIKE '%cp_require_transition_edge(%'`).Scan(&edges))
	assert.GreaterOrEqual(t, edges, 17,
		"the fifteen conversions plus agents' two columns should all bind the edge; found %d", edges)
}

// Nothing is born finished (F-122, migrations 00735-00737).
//
// Every binding in this schema is about CHANGES: 00603 tied a state change to a
// transition row, 00726 and 00731 made that row name both endpoints, 00732
// stopped the endpoints being forgeable, 00734 closed the exit from a terminal
// state. A row INSERTED in a privileged state never changed, so none of it
// applied -- and until 00735, capability_gates was the only entity in the
// schema that could not be born decided.
//
// The rows below are written with random foreign keys on purpose. A BEFORE
// INSERT trigger fires before the foreign keys are checked, so a refusal here
// is the birth control speaking and not an accident of the fixture; the
// assertions name the SQLSTATE and the message to make sure of it.
func TestIntegration_NothingIsBornFinished(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	require.NoError(t, migrate.Up(ctx, migrateURL))
	app := connect(t, appURL)

	for _, tc := range []struct {
		name   string
		sql    string
		expect string
	}{
		{
			// The whole promotion ladder, skipped at INSERT: every evidence and
			// approval CHECK on agent_lifecycle_transitions guards a
			// transition, and creating an agent is not one.
			name: "an agent born LIVE",
			sql: `INSERT INTO agents (id, account_id, strategy_id, name, stage, state, mode, version,
			                          created_by_actor_type, created_by_actor_id)
			      VALUES (gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'forged',
			              'LIVE', 'LIVE', 'LIVE', 1, 'SYSTEM', 'x')`,
			expect: "AGENT_BORN_PROMOTED",
		},
		{
			// SETTLED carries FinalitySettled, which is what makes value
			// payout-eligible. Born with a lot, it is minted from nothing.
			name: "a funding born SETTLED with a lot",
			sql: `INSERT INTO credit_fundings (id, account_id, provider, provider_reference, state,
			                                   credit_quantity, paid_amount_minor, paid_currency,
			                                   fee_amount_minor, idempotency_key, lot_id)
			      VALUES (gen_random_uuid(), gen_random_uuid(), 'p', 'r', 'SETTLED', 100, 100, 'USD', 0,
			              'k-' || gen_random_uuid(), gen_random_uuid())`,
			expect: "FUNDING_BORN_FINISHED",
		},
		{
			// settled <= reserved <= requested is satisfied by naming all three.
			name: "a payout born SETTLED",
			sql: `INSERT INTO payout_requests (id, account_id, credit_asset_id, state, requested_quantity,
			                                   reserved_quantity, settled_quantity, policy_version,
			                                   policy_hash, verification_level, idempotency_key)
			      VALUES (gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'SETTLED',
			              1000, 1000, 1000, 'v', 'h', 'NONE', 'k-' || gen_random_uuid())`,
			expect: "PAYOUT_BORN_FINISHED",
		},
		{
			// Dual control forged outright: no proposal, no second person, and
			// no transition row because there was no transition. This is the
			// one that matters most -- agent_lifecycle_transitions.approval_id
			// references this table, so a forged row completes a
			// fully-evidenced promotion to LIVE.
			name: "an admin action born APPROVED",
			sql: `INSERT INTO admin_actions (id, kind, target_type, target_id, params, params_hash, reason,
			                                 requires_dual, status, proposed_by_user_id, proposed_at,
			                                 proposer_step_up_at, approved_by_user_id, approved_at, expires_at)
			      VALUES (gen_random_uuid(), 'LEDGER_CORRECTION', 'ACCOUNT', gen_random_uuid()::text, '{}', 'h',
			              'forging dual control', true, 'APPROVED', gen_random_uuid(), now(), now(),
			              gen_random_uuid(), now(), now() + interval '1 hour')`,
			expect: "ADMIN_ACTION_BORN_DECIDED",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := app.Exec(ctx, tc.sql)
			require.Error(t, err, "the row committed")
			assert.Equal(t, "AD001", db.SQLState(err), "got %v", err)
			assert.Contains(t, err.Error(), tc.expect)
		})
	}

	// The negative control. Three of these tables are still open at birth, and
	// listing them here is what stops that being forgotten: kill_switches has
	// an INSERT binding (00724) and capability_gates a birth trigger (00701),
	// but wallets, assets and instruments have neither, and a wallet born
	// ACTIVE or an asset born SETTLEMENT has no audited provenance at all.
	//
	// It is an assertion rather than a comment so that closing one of them
	// fails here and forces this list to be updated with what changed.
	stillOpen := []string{"wallets", "assets", "instruments"}
	for _, table := range stillOpen {
		var n int
		require.NoError(t, app.QueryRow(ctx, `
			SELECT count(*) FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
			 WHERE c.relname = $1 AND NOT t.tgisinternal
			   AND pg_get_triggerdef(t.oid) LIKE '%BEFORE INSERT%'`, table).Scan(&n))
		assert.Zero(t, n,
			"%s gained a birth control; that is good, and this list is now out of date", table)
	}
}
