//go:build integration

package orders

import (
	"context"
	"crypto/sha256"
	"regexp"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/security"
)

func (f *fixture) owner() *pgx.Conn {
	f.t.Helper()
	c, err := pgx.Connect(f.ctx, testMigrateURL)
	require.NoError(f.t, err)
	f.t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// rawTx runs raw SQL in an application-role transaction and returns the
// error, including one raised at COMMIT by a deferred constraint.
func (f *fixture) rawTx(sql string, args ...any) error {
	return testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}

// tradedMarket is a market with a filled order, a partially filled order,
// an open order and one trade's rows to copy.
type tradedMarket struct {
	m                     spotMarket
	book                  Book
	filled, partial, open OrderView
}

func (f *fixture) tradedMarket() tradedMarket {
	f.t.Helper()
	m := f.spotMarket(marketOpts{makerBPS: 10, takerBPS: 20, snapshotInterval: 2})
	accts := f.fundedAccounts(m, 3)
	maker := f.submit(f.limit(m, accts[0], SideSell, "100.00", units(2)))
	taker := f.submit(f.limit(m, accts[1], SideBuy, "100.00", units(1)))
	open := f.submit(f.limit(m, accts[2], SideBuy, "90.00", units(1)))
	b, err := f.svc.GetBook(f.ctx, testDB, m.inst)
	require.NoError(f.t, err)
	return tradedMarket{m: m, book: b, filled: taker.Order, partial: f.orderView(maker.Order.ID), open: open.Order}
}

// TestIntegration_TheDatabaseRefusesABrokenJournal: a command that is not the
// next link of the book's hash chain, a cursor that moves without its
// command, a snapshot that is not the journal's state, and any edit of
// journal history are refused by the database itself, whoever writes them.
func TestIntegration_TheDatabaseRefusesABrokenJournal(t *testing.T) {
	f := newFixture(t)
	tm := f.tradedMarket()
	b := tm.book
	cmd := []byte(`{"kind":2}`)
	sum := sha256.Sum256(cmd)
	insert := func(seq int64, prev []byte, firstEvent int64) error {
		return f.rawTx(`INSERT INTO book_commands (id, book_id, seq, kind, order_id, command, command_sha256, timestamp_unix_ns,
			rejected, reject_reason, first_event_seq, last_event_seq, trade_seq_after, prev_state_hash, state_hash_after, actor_type, actor_id)
			VALUES ($1,$2,$3,'CANCEL',$4,$5,$6,0,true,'UNKNOWN_ORDER',$7,$7,$8,$9,$10,'SYSTEM','bypass')`,
			uuid.New(), b.ID, seq, uuid.New(), cmd, sum[:], firstEvent, b.LastTradeSeq, prev, sum[:])
	}
	err := insert(b.LastCommandSeq+2, b.StateHash[:], b.LastEventSeq+1)
	assert.Equal(t, "OB001", db.SQLState(err), "a gap in the sequence: %v", err)
	wrong := sha256.Sum256([]byte("not the recorded state"))
	err = insert(b.LastCommandSeq+1, wrong[:], b.LastEventSeq+1)
	assert.Equal(t, "OB001", db.SQLState(err), "a broken hash chain: %v", err)
	err = insert(b.LastCommandSeq+1, b.StateHash[:], b.LastEventSeq+5)
	assert.Equal(t, "OB001", db.SQLState(err), "a gap in the events: %v", err)
	err = f.rawTx(`INSERT INTO book_commands (id, book_id, seq, kind, order_id, command, command_sha256, timestamp_unix_ns,
		rejected, first_event_seq, last_event_seq, trade_seq_after, prev_state_hash, state_hash_after, actor_type, actor_id)
		VALUES ($1,$2,$3,'CANCEL',$4,$5,$6,0,false,$7,$7,$8,$9,$10,'SYSTEM','bypass')`,
		uuid.New(), b.ID, b.LastCommandSeq+1, uuid.New(), cmd, wrong[:], b.LastEventSeq+1, b.LastTradeSeq, b.StateHash[:], sum[:])
	assert.Equal(t, "23514", db.SQLState(err), "a command whose bytes do not match its hash: %v", err)

	err = f.rawTx(`UPDATE book_states SET last_command_seq = last_command_seq + 1 WHERE id = $1`, b.ID)
	assert.Equal(t, "OB002", db.SQLState(err), "a cursor ahead of the journal: %v", err)
	err = f.rawTx(`UPDATE book_states SET state_hash = $2 WHERE id = $1`, b.ID, wrong[:])
	assert.Equal(t, "OB002", db.SQLState(err), "a state the journal never recorded: %v", err)

	snap := []byte("NCLOB-not-this-book")
	snapSum := sha256.Sum256(snap)
	err = f.rawTx(`INSERT INTO book_snapshots (id, book_id, command_seq, snapshot, state_hash) VALUES ($1,$2,$3,$4,$5)`,
		uuid.New(), b.ID, b.LastCommandSeq, snap, snapSum[:])
	assert.Equal(t, "OB005", db.SQLState(err), "a snapshot of another state: %v", err)
	err = f.rawTx(`INSERT INTO book_snapshots (id, book_id, command_seq, snapshot, state_hash) VALUES ($1,$2,$3,$4,$5)`,
		uuid.New(), b.ID, b.LastCommandSeq, snap, b.StateHash[:])
	assert.Equal(t, "23514", db.SQLState(err), "a snapshot whose bytes do not hash to its hash: %v", err)

	require.Positive(t, f.count(`SELECT count(*) FROM book_snapshots WHERE book_id = $1`, b.ID))
	for _, sql := range []string{
		`UPDATE book_commands SET rejected = NOT rejected WHERE book_id = $1`,
		`DELETE FROM book_events WHERE book_id = $1`,
		`UPDATE book_trades SET maker_fee = 0 WHERE book_id = $1`,
		`DELETE FROM book_snapshots WHERE book_id = $1`,
		`DELETE FROM book_order_transitions WHERE order_id IN (SELECT id FROM book_orders WHERE book_id = $1)`,
	} {
		_, err := f.owner().Exec(f.ctx, sql, b.ID)
		require.Error(t, err, sql)
		assert.Equal(t, "P0001", db.SQLState(err), "history is append-only, even for the owner: %s", sql)
	}
	// After all of that, the journal still verifies.
	_, err = f.svc.VerifyJournal(f.ctx, testDB, tm.m.inst)
	require.NoError(t, err)
}

// TestIntegration_TheDatabaseRefusesOverfillAndIllegalOrderStates: an order
// can never hold more fills than its quantity, never leave a terminal state,
// never take an edge the status machine lacks, never change status without a
// transition row naming the change, never shrink its fills and never change
// identity.
func TestIntegration_TheDatabaseRefusesOverfillAndIllegalOrderStates(t *testing.T) {
	f := newFixture(t)
	tm := f.tradedMarket()
	err := f.rawTx(`UPDATE book_orders SET filled_lots = qty_lots + 1 WHERE id = $1`, tm.partial.ID)
	assert.Equal(t, "23514", db.SQLState(err), "an overfill: %v", err)
	assert.Contains(t, err.Error(), "book_orders_quantity_conserved")
	err = f.rawTx(`UPDATE book_orders SET remaining_lots = 0, cancelled_lots = qty_lots WHERE id = $1`, tm.open.ID)
	assert.Equal(t, "23514", db.SQLState(err), "an OPEN order with nothing working: %v", err)
	err = f.rawTx(`UPDATE book_orders SET cancel_reason = 'USER' WHERE id = $1`, tm.filled.ID)
	assert.Equal(t, "OB003", db.SQLState(err), "a filled order is final: %v", err)
	err = f.rawTx(`UPDATE book_orders SET filled_lots = filled_lots - 100, remaining_lots = remaining_lots + 100 WHERE id = $1`, tm.partial.ID)
	assert.Equal(t, "OB004", db.SQLState(err), "fills never shrink: %v", err)

	// OPEN -> FILLED, consistent quantities, but no transition row: refused at
	// COMMIT.
	err = f.rawTx(`UPDATE book_orders SET status = 'FILLED', filled_lots = qty_lots, remaining_lots = 0 WHERE id = $1`, tm.open.ID)
	assert.Equal(t, "AU001", db.SQLState(err), "a status change without its transition row: %v", err)
	// OPEN -> REJECTED is not an edge, even with a transition row naming it.
	err = testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO book_order_transitions (id, order_id, from_status, to_status, reason, actor_type, actor_id)
			VALUES ($1,$2,'OPEN','REJECTED','bypass','SYSTEM','bypass')`, uuid.New(), tm.open.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE book_orders SET status = 'REJECTED', reject_reason = 'X', remaining_lots = 0 WHERE id = $1`, tm.open.ID)
		return err
	})
	assert.Equal(t, "OB003", db.SQLState(err), "an illegal edge: %v", err)

	// Identity: the application role cannot even name the column, and the
	// owner is stopped by the trigger.
	err = f.rawTx(`UPDATE book_orders SET side = 'SELL' WHERE id = $1`, tm.open.ID)
	assert.Equal(t, "42501", db.SQLState(err), "%v", err)
	_, err = f.owner().Exec(f.ctx, `UPDATE book_orders SET side = 'SELL' WHERE id = $1`, tm.open.ID)
	assert.Equal(t, "OB004", db.SQLState(err), "%v", err)
	assert.Equal(t, StatusOpen, f.orderView(tm.open.ID).Status)
}

// TestIntegration_TheDatabaseRefusesSelfTradesUnruledCrossingAndDuplicateClientIDs:
// a trade row between two orders of one owner, or between a house book and a
// customer without a crossing rule version, cannot exist; nor can two working
// orders of one owner share a client order id.
func TestIntegration_TheDatabaseRefusesSelfTradesUnruledCrossingAndDuplicateClientIDs(t *testing.T) {
	f := newFixture(t)
	tm := f.tradedMarket()
	copyTrade := func(set string) error {
		return f.rawTx(`INSERT INTO book_trades (id, book_id, instrument_id, trade_seq, command_seq, event_seq, price_ticks, qty_lots,
			maker_order_id, taker_order_id, maker_owner_type, maker_owner_id, taker_owner_type, taker_owner_id, aggressor_side,
			base_asset_id, quote_asset_id, base_quantity, quote_notional, maker_fee, taker_fee, maker_schedule_hash,
			taker_schedule_hash, crossing_rule_version, journal_transaction_id, effect_id, evidence_ref, evidence_sha256)
			SELECT $1, book_id, instrument_id, trade_seq + 100, command_seq, event_seq, price_ticks, qty_lots, maker_order_id,
			taker_order_id, `+set+`, aggressor_side, base_asset_id, quote_asset_id, base_quantity, quote_notional, maker_fee,
			taker_fee, maker_schedule_hash, taker_schedule_hash, 0, journal_transaction_id, effect_id, evidence_ref, evidence_sha256
			FROM book_trades WHERE book_id = $2 LIMIT 1`, uuid.New(), tm.book.ID)
	}
	err := copyTrade(`maker_owner_type, maker_owner_id, taker_owner_type, maker_owner_id`)
	assert.Equal(t, "23514", db.SQLState(err), "%v", err)
	assert.Contains(t, err.Error(), "book_trades_no_self_trade")
	err = copyTrade(`'HOUSE', maker_owner_id, taker_owner_type, taker_owner_id`)
	assert.Equal(t, "23514", db.SQLState(err), "%v", err)
	assert.Contains(t, err.Error(), "book_trades_house_crossing_needs_rule")

	// A second working order of the same owner with the same client order id,
	// written around the service: the unique index refuses it.
	h := f.houseBook("dup")
	f.fundHouse(h, tm.m.base.ID, units(5))
	hr := f.houseOrder(tm.m, h, SideSell, "150.00", units(1))
	hr.ClientOrderID = "raw-dup"
	first, err := f.pipe.SubmitHouse(f.ctx, hr)
	require.NoError(t, err)
	err = f.rawTx(`INSERT INTO book_orders (id, book_id, instrument_id, owner_type, owner_id, house_account_id,
		idempotency_key, request_hash, client_order_id, side, order_type, time_in_force, stp_mode, liquidity_intent, post_only,
		reduce_only, mode, price_ticks, qty_lots, remaining_lots, status)
		SELECT $1, book_id, instrument_id, owner_type, owner_id, house_account_id, 'other-key', request_hash, client_order_id, side,
		order_type, time_in_force, stp_mode, liquidity_intent, post_only, reduce_only, mode, price_ticks, qty_lots, qty_lots,
		'PENDING' FROM book_orders WHERE id = $2`, uuid.New(), first.Order.ID)
	assert.Equal(t, "23505", db.SQLState(err), "%v", err)
	assert.Equal(t, "book_orders_owner_client_order_id", db.ConstraintName(err))
}

// TestIntegration_TheDatabaseBindsBooksRulesAndHouseReservations: a book's
// identity is frozen and its status machine enforced; crossing rules are
// append-only and consecutive; a house reservation follows its own machine.
func TestIntegration_TheDatabaseBindsBooksRulesAndHouseReservations(t *testing.T) {
	f := newFixture(t)
	tm := f.tradedMarket()
	err := f.rawTx(`UPDATE book_states SET tick_size = 2 WHERE id = $1`, tm.book.ID)
	assert.Equal(t, "42501", db.SQLState(err), "%v", err)
	_, err = f.owner().Exec(f.ctx, `UPDATE book_states SET tick_size = 2 WHERE id = $1`, tm.book.ID)
	assert.Equal(t, "OB008", db.SQLState(err), "%v", err)
	err = f.rawTx(`UPDATE book_states SET status = 'HALTED' WHERE id = $1`, tm.book.ID)
	assert.Equal(t, "AU001", db.SQLState(err), "a book status change without its transition row: %v", err)

	empty := f.spotMarket(marketOpts{})
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.SetBookStatus(ctx, tx, empty.inst, BookClosed, systemActor, "end of session")
		return err
	}))
	err = testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO book_state_transitions (id, book_id, from_status, to_status, actor_type, actor_id, reason)
			VALUES ($1,$2,'CLOSED','OPEN','SYSTEM','bypass','reopen')`, uuid.New(), empty.book.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE book_states SET status = 'OPEN' WHERE id = $1`, empty.book.ID)
		return err
	})
	assert.Equal(t, "OB009", db.SQLState(err), "a closed book stays closed: %v", err)
	_, err = f.pipe.Submit(f.ctx, f.limit(empty, f.account(), SideBuy, "99.00", units(1)))
	assert.Equal(t, ReasonBookNotOpen, ReasonOf(err))

	err = f.rawTx(`INSERT INTO book_crossing_rules (id, book_id, version, allow_house_customer_crossing, reason, actor_type, actor_id)
		VALUES ($1,$2,3,true,'skip ahead','SYSTEM','bypass')`, uuid.New(), tm.book.ID)
	assert.Equal(t, "OB006", db.SQLState(err), "%v", err)
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.SetCrossingRule(ctx, tx, tm.m.inst, false, systemActor, "explicitly forbid")
		return err
	}))
	_, err = f.owner().Exec(f.ctx, `UPDATE book_crossing_rules SET allow_house_customer_crossing = true WHERE book_id = $1`, tm.book.ID)
	require.Error(t, err, "a crossing rule is never edited, only superseded")
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.SetCrossingRule(ctx, tx, tm.m.inst, true, econ.Actor{Type: security.ActorAgent, ID: "agent"}, "let me")
		return err
	})
	require.Error(t, err, "an agent never sets a crossing rule")

	h := f.houseBook("bypass")
	f.fundHouse(h, tm.m.base.ID, units(5))
	out, err := f.pipe.SubmitHouse(f.ctx, f.houseOrder(tm.m, h, SideSell, "150.00", units(2)))
	require.NoError(t, err)
	rid := out.Order.ReservationID
	err = f.rawTx(`UPDATE book_house_reservations SET consumed_quantity = 1 WHERE id = $1`, rid)
	require.NoError(t, err, "consuming is allowed")
	err = f.rawTx(`UPDATE book_house_reservations SET consumed_quantity = 0 WHERE id = $1`, rid)
	assert.Equal(t, "OB007", db.SQLState(err), "consumption never shrinks: %v", err)
	err = f.rawTx(`UPDATE book_house_reservations SET status = 'RELEASED' WHERE id = $1`, rid)
	assert.Equal(t, "AU001", db.SQLState(err), "a reservation status change without its transition row: %v", err)
	err = testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO book_house_reservation_transitions (id, reservation_id, from_status, to_status, reason,
			actor_type, actor_id) VALUES ($1,$2,'ACTIVE','CONSUMED','bypass','SYSTEM','bypass')`, uuid.New(), rid); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE book_house_reservations SET status = 'CONSUMED' WHERE id = $1`, rid)
		return err
	})
	assert.Equal(t, "23514", db.SQLState(err), "CONSUMED means used up: %v", err)
}

// TestIntegration_SQLAndGoAgreeOnTransitions: the status machines the database
// enforces are exactly the ones the Go code follows.
func TestIntegration_SQLAndGoAgreeOnTransitions(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	for _, from := range AllStatuses() {
		for _, to := range AllStatuses() {
			var legal bool
			require.NoError(t, testDB.QueryRow(ctx, `SELECT cp_book_order_transition_legal($1,$2)`, from, to).Scan(&legal))
			assert.Equal(t, CanTransition(from, to), legal, "%s -> %s", from, to)
		}
	}
	for _, from := range AllBookStatuses() {
		for _, to := range AllBookStatuses() {
			var legal bool
			require.NoError(t, testDB.QueryRow(ctx, `SELECT cp_book_status_transition_legal($1,$2)`, from, to).Scan(&legal))
			assert.Equal(t, CanTransitionBook(from, to), legal, "%s -> %s", from, to)
		}
	}
}

func toStrings[T ~string](xs []T) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = string(x)
	}
	return out
}

// TestIntegration_EnumChecksMatchTheirGoLists pairs every enumerated CHECK of
// migration 00742 with the Go function that returns its list.
func TestIntegration_EnumChecksMatchTheirGoLists(t *testing.T) {
	requireEnv(t)
	strs := func(xs ...string) []string { return xs }
	actors := toStrings(econ.CreatorActorTypes())
	pairs := []struct {
		table, constraint string
		want              []string
	}{
		{"book_states", "book_states_mode_check", toStrings(AllBookModes())},
		{"book_states", "book_states_status_check", toStrings(AllBookStatuses())},
		{"book_states", "book_states_instrument_kind_check", toStrings(AllBookInstrumentKinds())},
		{"book_states", "book_states_terms_source_check", toStrings(AllTermsSources())},
		{"book_states", "book_states_venue_label_check", strs(VenueLabelInternalBook)},
		{"book_states", "book_states_value_domain_check", strs("SIMULATED")},
		{"book_states", "book_states_created_by_actor_type_check", actors},
		{"book_state_transitions", "book_state_transitions_actor_type_check", actors},
		{"book_orders", "book_orders_owner_type_check", toStrings(AllOwnerTypes())},
		{"book_orders", "book_orders_side_check", toStrings(AllSides())},
		{"book_orders", "book_orders_order_type_check", toStrings(AllOrderTypes())},
		{"book_orders", "book_orders_time_in_force_check", toStrings(AllTimesInForce())},
		{"book_orders", "book_orders_stp_mode_check", toStrings(AllSTPModes())},
		{"book_orders", "book_orders_liquidity_intent_check", toStrings(AllLiquidityIntents())},
		{"book_orders", "book_orders_mode_check", toStrings(AllBookModes())},
		{"book_orders", "book_orders_status_check", toStrings(AllStatuses())},
		{"book_order_transitions", "book_order_transitions_actor_type_check", actors},
		{"book_commands", "book_commands_kind_check", toStrings(AllCommandKinds())},
		{"book_commands", "book_commands_actor_type_check", actors},
		{"book_events", "book_events_event_type_check", AllEventTypes()},
		{"book_trades", "book_trades_maker_owner_type_check", toStrings(AllOwnerTypes())},
		{"book_trades", "book_trades_taker_owner_type_check", toStrings(AllOwnerTypes())},
		{"book_trades", "book_trades_aggressor_side_check", toStrings(AllSides())},
		{"book_crossing_rules", "book_crossing_rules_value_domain_check", strs("SIMULATED")},
		{"book_crossing_rules", "book_crossing_rules_actor_type_check", actors},
		{"book_crossing_decisions", "book_crossing_decisions_command_kind_check", toStrings(AllCrossingCommandKinds())},
		{"book_crossing_decisions", "book_crossing_decisions_taker_owner_type_check", toStrings(AllOwnerTypes())},
		{"book_house_reservations", "book_house_reservations_status_check", toStrings(AllHouseReservationStatuses())},
		{"book_house_reservation_transitions", "book_house_reservation_transitions_actor_type_check", actors},
		{"paper_fills", "paper_fills_venue_label_check", strs(VenueLabelPaper)},
		{"paper_fills", "paper_fills_side_check", toStrings(AllSides())},
	}
	quoted := regexp.MustCompile(`'([^']*)'`)
	for _, p := range pairs {
		var def string
		require.NoError(t, testDB.QueryRow(context.Background(), `SELECT pg_get_constraintdef(c.oid) FROM pg_constraint c
			JOIN pg_class r ON r.oid = c.conrelid WHERE r.relname = $1 AND c.conname = $2`, p.table, p.constraint).Scan(&def),
			"%s.%s", p.table, p.constraint)
		var got []string
		for _, m := range quoted.FindAllStringSubmatch(def, -1) {
			got = append(got, m[1])
		}
		want := append([]string(nil), p.want...)
		sort.Strings(got)
		sort.Strings(want)
		assert.Equal(t, want, got, "%s.%s: %s", p.table, p.constraint, def)
	}
}
