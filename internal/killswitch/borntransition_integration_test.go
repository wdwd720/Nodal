//go:build integration

package killswitch

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
)

// A kill switch created already active carries its transition row (F-70).
//
// 00603's header says it binds "every state change of an audited entity to a
// transition row written in the SAME transaction", and for `kill_switches` it
// bound `AFTER UPDATE OF active`. A switch's FIRST activation updates nothing:
// `Controller.Activate` calls `insertActive`, which writes `active = true` in
// the INSERT, and the trigger never saw it.
//
// `Activate` does write the row -- it calls `c.record` in the same transaction
// -- so the trail was complete in practice. That is F-49's shape, and it matters
// here because this table is the record of who stopped the platform and why, and
// the first activation is the one an incident review reads.
//
// `kill_switches` was also the one table carrying that trigger with no negative
// test at all: `withdrawals`, `orders`, `trade_intents`, `deposits`,
// `capability_gates`, `agents` and the credit tables each have one.

// venueScope is a scope id no other row will have. It takes the TAIL of a
// UUIDv7, not the head: the head is the millisecond timestamp, so two ids
// minted in the same millisecond share it and collide on
// UNIQUE (kind, scope_id) -- which is what the first version of this test did.
func venueScope() string {
	raw := id.New[id.Any]().String()
	return "venue-" + raw[len(raw)-8:]
}

func TestIntegration_ASwitchBornActiveNeedsItsTransitionRow(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()

	// The write the hole allowed: a switch that arrives already stopping the
	// platform, with nothing recording who did it.
	err := testDB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `INSERT INTO kill_switches
				(id, kind, scope_id, active, severity, reason, activated_by_actor_id, activated_at)
				VALUES ($1, 'VENUE_DISABLE', $2, true, 'STANDARD', 'no audit row', 'itest', now())`,
				id.New[id.Any](), venueScope())
			return e
		})
	require.Error(t, err, "a kill switch was created active with no transition row")
	assert.Equal(t, "AU001", db.SQLState(err), "got %v", err)
	assert.Contains(t, err.Error(), "was created as true")

	// The same INSERT with its transition row commits. This is the positive
	// control and it is the shape Controller.Activate actually writes.
	scope := venueScope()
	switchID := id.New[id.Any]()
	require.NoError(t, testDB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			if _, e := tx.Exec(ctx, `INSERT INTO kill_switches
				(id, kind, scope_id, active, severity, reason, activated_by_actor_id, activated_at)
				VALUES ($1, 'VENUE_DISABLE', $2, true, 'STANDARD', 'with its audit row', 'itest', now())`,
				switchID, scope); e != nil {
				return e
			}
			_, e := tx.Exec(ctx, `INSERT INTO kill_switch_transitions
				(id, switch_id, kind, scope_id, to_active, actor_type, actor_id, reason)
				VALUES ($1, $2, 'VENUE_DISABLE', $3, true, 'OPERATOR', 'itest', 'with its audit row')`,
				id.New[id.Any](), switchID, scope)
			return e
		}), "a switch inserted with its transition row must commit")

	// A row born INACTIVE is not a state change from anything -- it is the
	// absence of a switch, written down -- and needs no audit row. Without this
	// the rule would demand evidence for nothing having happened.
	require.NoError(t, testDB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `INSERT INTO kill_switches
				(id, kind, scope_id, active, severity, reason)
				VALUES ($1, 'VENUE_DISABLE', $2, false, 'STANDARD', 'never activated')`,
				id.New[id.Any](), venueScope())
			return e
		}), "a switch born inactive must not need a transition row")
}

// TestIntegration_ActivateStillWorksThroughTheController is the positive control
// that matters most: the guard must not break the only path that legitimately
// creates an active switch. A trigger one condition too broad would pass every
// refusal above and stop an operator halting the platform -- and it would look
// like the kill switch failing at the worst possible moment.
func TestIntegration_ActivateStillWorksThroughTheController(t *testing.T) {
	requireEnv(t)
	f := newFixture(t)
	scope := venueScope()

	sw, err := f.activate(t, f.op("ops-1", security.RoleOperations), VenueDisable, scope, "a real incident")
	require.NoError(t, err, "the controller could not activate a switch under the new guard")
	assert.True(t, sw.Active)

	var transitions int
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT count(*) FROM kill_switch_transitions WHERE switch_id = $1`, sw.ID).Scan(&transitions))
	assert.Equal(t, 1, transitions, "the activation recorded exactly one transition")

	// And it still blocks, so the switch is a switch and not just a row.
	blocked := Blocks(sw, Action{Class: NewRisk, Venue: scope}, Policy{})
	assert.True(t, blocked, "an activated VENUE_DISABLE must block new risk on that venue")
}
