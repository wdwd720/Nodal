//go:build integration

package equities

import (
	"context"
	"regexp"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/security"
)

// TestIntegration_DatabaseRefusesATradeThatDoesNotBalancePerParty: a
// SIM_TRADE that takes a spread from the customer's WALLET straight into the
// platform's books -- balanced per asset, but with no FEES_SPREAD leg in the
// customer's own books -- is refused by the application and, when the
// application is bypassed, by the database at COMMIT (SM002). Selling shares
// the customer does not hold is refused by the database too (LG001).
func TestIntegration_DatabaseRefusesATradeThatDoesNotBalancePerParty(t *testing.T) {
	w := newWorld(t)
	acct := w.account()
	l := w.list(1, 1)
	w.fund(acct, w.cash.ID, 1_000_000)
	skim := []ledger.Entry{
		{Account: ledger.CustomerAccount(acct, ledger.CodeWallet, w.cash.ID), Side: ledger.Credit, Quantity: q(100)},
		{Account: ledger.PlatformAccount(ledger.CodePlatformFeeReceivable, w.cash.ID), Side: ledger.Debit, Quantity: q(100)},
	}
	err := w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.ledger.Post(ctx, tx, ledger.Posting{Kind: ledger.KindSimTrade, IdempotencyKey: "skim:" + uuid.NewString(),
			Reference: ledger.FinancialEventReference{Type: "itest", ID: uuid.NewString()}, EffectiveAt: w.clk.Now(), Entries: skim})
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeLedgerUnbalanced, errs.CodeOf(err))

	err = w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return w.rawJournal(ctx, tx, ledger.KindSimTrade, []rawEntry{
			{ledger.CustomerAccount(acct, ledger.CodeWallet, w.cash.ID), ledger.Credit, 100},
			{ledger.PlatformAccount(ledger.CodePlatformFeeReceivable, w.cash.ID), ledger.Debit, 100},
		})
	})
	require.Error(t, err)
	assert.Equal(t, "SM002", db.SQLState(err), "%v", err)

	err = w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return w.rawJournal(ctx, tx, ledger.KindSimTrade, []rawEntry{
			{ledger.CustomerAccount(acct, ledger.CodeWallet, l.ShareAsset.ID), ledger.Credit, 1_000_000},
			{ledger.CustomerAccount(acct, ledger.CodeTradingOutflow, l.ShareAsset.ID), ledger.Debit, 1_000_000},
		})
	})
	require.Error(t, err)
	assert.Equal(t, ledger.SQLStateNegativeBalance, db.SQLState(err), "%v", err)
	assert.Equal(t, "1000000", w.cust(acct, ledger.CodeWallet, w.cash.ID).String(), "nothing moved")
}

// TestIntegration_DatabaseGuardsTheEquitiesRegisters: the state the
// subsystem depends on is enforced by the database when Go is bypassed.
func TestIntegration_DatabaseGuardsTheEquitiesRegisters(t *testing.T) {
	w := newWorld(t)
	acct := w.account()
	w.fund(acct, w.cash.ID, 10_000_000_000)
	l := w.list(1, 1)
	w.mark(l, 15_000)
	open := w.trade(acct, l, limit(SideBuy, 1_000_000, 14_000))
	require.Equal(t, OrderOpen, open.Status)
	filled := w.trade(acct, l, market(SideBuy, 1_000_000))
	require.Equal(t, OrderFilled, filled.Status)
	f, _, err := w.svc.FillForOrder(w.ctx, testDB, filled.ID)
	require.NoError(t, err)
	exec := func(sql string, args ...any) error {
		return w.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, args...)
			return err
		})
	}
	owner, err := pgx.Connect(w.ctx, testMigrateURL)
	require.NoError(t, err)
	defer owner.Close(w.ctx)

	// An order changes status only with a transition row naming the change.
	err = exec(`UPDATE equity_orders SET status = 'CANCELLED' WHERE id = $1`, open.ID)
	require.Error(t, err)
	assert.Equal(t, "AU001", db.SQLState(err))
	// ...and only along a legal edge, whatever row is written.
	err = w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO equity_order_transitions (id, order_id, from_status, to_status, actor_type, actor_id,
			reason, occurred_at) VALUES ($1,$2,'FILLED','OPEN','SYSTEM','bypass','bypass',now())`, uuid.New(), filled.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE equity_orders SET status = 'OPEN' WHERE id = $1`, filled.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "EQ001", db.SQLState(err))
	// An agent never moves an order.
	err = exec(`INSERT INTO equity_order_transitions (id, order_id, from_status, to_status, actor_type, actor_id, reason, occurred_at)
		VALUES ($1,$2,'OPEN','CANCELLED','AGENT','agent','x',now())`, uuid.New(), open.ID)
	require.Error(t, err)
	assert.Equal(t, db.SQLStateCheckViolation, db.SQLState(err))

	// Identity is frozen: the application role cannot even name the column,
	// and the owner is stopped by the trigger.
	err = exec(`UPDATE equity_orders SET quantity = 1 WHERE id = $1`, filled.ID)
	require.Error(t, err)
	assert.Equal(t, "42501", db.SQLState(err))
	_, err = owner.Exec(w.ctx, `UPDATE equity_orders SET quantity = 1 WHERE id = $1`, filled.ID)
	require.Error(t, err)
	assert.Equal(t, "EQ002", db.SQLState(err))
	_, err = owner.Exec(w.ctx, `UPDATE equity_orders SET rejection_code = 'RISK_REJECTED' WHERE id = $1`, filled.ID)
	require.Error(t, err)
	assert.Equal(t, "EQ002", db.SQLState(err), "a rejection is written only with the move to REJECTED")

	// Fills are history.
	err = exec(`UPDATE equity_fills SET price = price + 1 WHERE order_id = $1`, filled.ID)
	require.Error(t, err)
	assert.Equal(t, "42501", db.SQLState(err))
	_, err = owner.Exec(w.ctx, `UPDATE equity_fills SET price = price + 1 WHERE order_id = $1`, filled.ID)
	require.Error(t, err)
	assert.True(t, db.IsImmutableRow(err), "%v", err)
	// A fill that goes through its order's limit, or overfills it, is refused
	// before anything else about it is checked.
	forge := func(qty, price int64) error {
		return exec(`INSERT INTO equity_fills (id, order_id, account_id, instrument_id, side, quantity, price, price_scale,
			mark_mantissa, mark_scale, mark_source, mark_observed_at, slippage_bps, venue, venue_fill_id, executed_at, trade_date,
			settlement_date, notional, spread_amount, platform_fees, venue_fees, total, fee_schedule_version, fee_schedule_hash,
			fee_schedule, fee_disclosure, fee_disclosure_hash, journal_transaction_id, effect_id)
			VALUES ($1,$2,$3,$4,'BUY',$5,$6,2,$6,2,'forged',$7,0,'nodal-paper-equities',$8,$7,'2026-10-07','2026-10-08',1,0,0,0,1,
			        'v',repeat('0',64),'{}','{}',repeat('0',64),$9,$10)`,
			uuid.New(), open.ID, acct, l.Instrument.ID, qty, price, w.clk.Now(), "forged-"+uuid.NewString(), uuid.New(), uuid.New())
	}
	err = forge(1_000_000, 14_100)
	require.Error(t, err)
	assert.Equal(t, "EQ003", db.SQLState(err), "through the 140.00 limit")
	err = forge(2_000_000, 13_900)
	require.Error(t, err)
	assert.Equal(t, "EQ003", db.SQLState(err), "more than the order")
	err = exec(`INSERT INTO equity_fills SELECT * FROM equity_fills WHERE order_id = $1`, filled.ID)
	require.Error(t, err, "a filled order fills no more")
	assert.Equal(t, "EQ003", db.SQLState(err))

	// A settlement settles once, with its journal, and never changes after.
	w.at(2026, 10, 8, 10, 0)
	w.mark(l, 15_000)
	w.settleDue()
	st, _, err := w.svc.SettlementForFill(w.ctx, testDB, f.ID)
	require.NoError(t, err)
	require.True(t, st.Settled())
	err = exec(`UPDATE equity_settlements SET settled_at = NULL, journal_transaction_id = NULL WHERE id = $1`, st.ID)
	require.Error(t, err)
	assert.Equal(t, "EQ004", db.SQLState(err))
	err = exec(`UPDATE equity_settlements SET settled_at = settled_at + interval '1 second' WHERE id = $1`, st.ID)
	require.Error(t, err)
	assert.Equal(t, "EQ004", db.SQLState(err))
	other := w.trade(acct, l, market(SideBuy, 1_000_000))
	require.Equal(t, OrderFilled, other.Status)
	of, ok, err := w.svc.FillForOrder(w.ctx, testDB, other.ID)
	require.NoError(t, err)
	require.True(t, ok)
	err = exec(`UPDATE equity_settlements SET settled_at = now() WHERE fill_id = $1`, of.ID)
	require.Error(t, err, "settled without its journal")
	assert.Equal(t, db.SQLStateCheckViolation, db.SQLState(err))
	// A settlement is recorded unsettled; it never arrives settled.
	err = exec(`INSERT INTO equity_settlements (id, fill_id, order_id, account_id, instrument_id, asset_id, quantity, trade_date,
		due_date, settled_at, journal_transaction_id)
		SELECT $2, fill_id, order_id, account_id, instrument_id, asset_id, quantity, trade_date, due_date, settled_at, journal_transaction_id
		  FROM equity_settlements WHERE id = $1`, st.ID, uuid.New())
	require.Error(t, err)
	assert.Equal(t, "EQ004", db.SQLState(err), "%v", err)

	// At COMMIT an order is FILLED exactly when it has its fill: a fill that
	// agrees with an OPEN order but leaves it OPEN is refused, and so is an
	// order made FILLED without one.
	settleEffect, ok, err := w.effects.GetByKey(w.ctx, testDB, "equities:settlement:"+f.ID.String())
	require.NoError(t, err)
	require.True(t, ok)
	err = exec(`INSERT INTO equity_fills (id, order_id, account_id, instrument_id, side, quantity, price, price_scale,
		mark_mantissa, mark_scale, mark_source, mark_observed_at, slippage_bps, venue, venue_fill_id, executed_at, trade_date,
		settlement_date, notional, spread_amount, platform_fees, venue_fees, total, fee_schedule_version, fee_schedule_hash,
		fee_schedule, fee_disclosure, fee_disclosure_hash, journal_transaction_id, effect_id)
		VALUES ($1,$2,$3,$4,'BUY',1000000,13900,2,13900,2,'forged',$5,0,'nodal-paper-equities',$6,$5,'2026-10-07','2026-10-08',
		        1,0,0,0,1,'v',repeat('0',64),'{}','{}',repeat('0',64),$7,$8)`,
		uuid.New(), open.ID, acct, l.Instrument.ID, open.CreatedAt, "forged-"+uuid.NewString(), *st.JournalTransactionID, settleEffect.ID)
	require.Error(t, err)
	assert.Equal(t, "EQ003", db.SQLState(err), "%v", err)
	err = w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO equity_order_transitions (id, order_id, from_status, to_status, actor_type, actor_id,
			reason, occurred_at) VALUES ($1,$2,'OPEN','FILLED','SYSTEM','bypass','bypass',now())`, uuid.New(), open.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE equity_orders SET status = 'FILLED' WHERE id = $1`, open.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "EQ003", db.SQLState(err), "%v", err)
	assert.Equal(t, OrderOpen, w.order(open.ID).Status)

	// An order is recorded ACCEPTED or REJECTED, and LIVE is not a mode it
	// can be stored in.
	cloneOrder := func(status, mode string) error {
		_, err := owner.Exec(w.ctx, `INSERT INTO equity_orders (id, account_id, instrument_id, intent_id, share_asset_id, cash_asset_id,
			symbol, exchange_mic, terms_version, side, order_type, mode, quantity, quantity_decimals, price_scale, tick_size, limit_price,
			protection_price, extended_hours, session, trade_date, expires_at, status, rejection_code, rejection_reasons, reservation_id,
			reserved_quantity, risk_decision_id, broker, actor_type, actor_id, created_at)
			SELECT $2, account_id, instrument_id, intent_id, share_asset_id, cash_asset_id, symbol, exchange_mic, terms_version, side,
			       order_type, $4, quantity, quantity_decimals, price_scale, tick_size, limit_price, protection_price, extended_hours,
			       session, trade_date, expires_at, $3, rejection_code, rejection_reasons, reservation_id, reserved_quantity,
			       risk_decision_id, broker, actor_type, actor_id, created_at
			  FROM equity_orders WHERE id = $1`, filled.ID, uuid.New(), status, mode)
		return err
	}
	err = cloneOrder("FILLED", "PAPER")
	require.Error(t, err)
	assert.Equal(t, "EQ001", db.SQLState(err), "%v", err)
	err = cloneOrder("ACCEPTED", "LIVE")
	require.Error(t, err)
	assert.Equal(t, db.SQLStateCheckViolation, db.SQLState(err))
	assert.Equal(t, "equity_orders_mode_check", db.ConstraintName(err))

	// One application per account per action, and applications are history.
	action, err := w.announce(CorporateActionSpec{InstrumentID: l.Instrument.ID, Type: ActionSplit, EffectiveDate: NewDate(2026, 10, 9),
		Ratio: &Ratio{Numerator: 2, Denominator: 1}, Reason: "guard test"})
	require.NoError(t, err)
	insertApp := func() error {
		return exec(`INSERT INTO corporate_action_applications (id, action_id, account_id, entitled_quantity, shares_delta,
			cash_amount, exact_cash_numerator, exact_cash_denominator, rounding_policy)
			VALUES ($1,$2,$3,1,0,0,0,1,'SHARES_DOWN_CASH_DOWN')`, uuid.New(), action.ID, acct)
	}
	require.NoError(t, insertApp())
	err = insertApp()
	require.Error(t, err)
	assert.True(t, db.IsUniqueViolation(err), "%v", err)
	err = exec(`UPDATE corporate_action_applications SET cash_amount = 1 WHERE action_id = $1`, action.ID)
	require.Error(t, err)
	err = exec(`INSERT INTO corporate_action_applications (id, action_id, account_id, entitled_quantity, shares_delta,
		cash_amount, exact_cash_numerator, exact_cash_denominator, rounding_policy)
		VALUES ($1,$2,$3,1,5,0,0,1,'SHARES_DOWN_CASH_DOWN')`, uuid.New(), action.ID, w.account())
	require.Error(t, err, "an application that moved shares names its journal")
	assert.Equal(t, db.SQLStateCheckViolation, db.SQLState(err))

	// A corporate action's status is bound like an order's.
	err = exec(`UPDATE equity_corporate_actions SET status = 'CANCELLED' WHERE id = $1`, action.ID)
	require.Error(t, err)
	assert.Equal(t, "AU001", db.SQLState(err))
	err = w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO equity_corporate_action_transitions (id, action_id, from_status, to_status, actor_type,
			actor_id, reason, occurred_at) VALUES ($1,$2,'ANNOUNCED','APPLYING','SYSTEM','bypass','bypass',now())`, uuid.New(), action.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE equity_corporate_actions SET status = 'APPLYING' WHERE id = $1`, action.ID)
		return err
	})
	require.Error(t, err, "only a delisting is ever APPLYING")
	_, err = owner.Exec(w.ctx, `UPDATE equity_corporate_actions SET ratio_numerator = 3 WHERE id = $1`, action.ID)
	require.Error(t, err)
	assert.Equal(t, "EQ002", db.SQLState(err))
	err = w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO equity_corporate_action_transitions (id, action_id, from_status, to_status, actor_type,
			actor_id, reason, occurred_at) VALUES ($1,$2,'ANNOUNCED','CANCELLED','SYSTEM','bypass','bypass',now())`, uuid.New(), action.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE equity_corporate_actions SET status = 'CANCELLED' WHERE id = $1`, action.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO equity_corporate_action_transitions (id, action_id, from_status, to_status, actor_type,
			actor_id, reason, occurred_at) VALUES ($1,$2,'CANCELLED','ANNOUNCED','SYSTEM','bypass','bypass',now())`, uuid.New(), action.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE equity_corporate_actions SET status = 'ANNOUNCED' WHERE id = $1`, action.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "EQ005", db.SQLState(err), "a cancelled action stays cancelled")

	// A corporate action is recorded ANNOUNCED, and each type carries only
	// its own fields: a symbol change with a record date, or with a ratio, is
	// refused.
	rawAction := func(status string, recordDate, ratioNumerator any) error {
		return exec(`INSERT INTO equity_corporate_actions (id, instrument_id, action_type, status, effective_date, share_asset_id,
			cash_asset_id, new_symbol, record_date, ratio_numerator, rounding_policy, reason, content_hash, announced_by_actor_type,
			announced_by_actor_id, applied_at)
			VALUES ($1,$2,'SYMBOL_CHANGE',$3::text,'2026-10-14',$4,$5,'RAWSYM',$6::date,$7::bigint,'SHARES_DOWN_CASH_DOWN','raw',$8,
			        'SYSTEM','raw',CASE WHEN $3::text = 'APPLIED' THEN now() END)`,
			uuid.New(), l.Instrument.ID, status, l.ShareAsset.ID, w.cash.ID, recordDate, ratioNumerator, make([]byte, 32))
	}
	err = rawAction("APPLIED", nil, nil)
	require.Error(t, err)
	assert.Equal(t, "EQ005", db.SQLState(err), "%v", err)
	err = rawAction("ANNOUNCED", "2026-10-14", nil)
	require.Error(t, err)
	assert.Equal(t, "equity_corporate_actions_dividend_shape", db.ConstraintName(err), "%v", err)
	err = rawAction("ANNOUNCED", nil, 2)
	require.Error(t, err)
	assert.Equal(t, "equity_corporate_actions_ratio_shape", db.ConstraintName(err), "%v", err)
	require.NoError(t, rawAction("ANNOUNCED", nil, nil), "the same row carrying only its own fields is accepted")

	// Holidays are append-only and never on a weekend.
	err = exec(`UPDATE equity_calendar_holidays SET name = 'x'`)
	require.Error(t, err)
	assert.Equal(t, "42501", db.SQLState(err))
	err = exec(`INSERT INTO equity_calendar_holidays (id, calendar_code, holiday_date, name, created_by_actor_type, created_by_actor_id)
		VALUES ($1,'XNYS','2026-10-10','a Saturday','SYSTEM','x')`, uuid.New())
	require.Error(t, err)
	assert.Equal(t, db.SQLStateCheckViolation, db.SQLState(err))
	err = exec(`INSERT INTO equity_calendar_holidays (id, calendar_code, holiday_date, name, created_by_actor_type, created_by_actor_id)
		VALUES ($1,'XLON','2026-10-12','another calendar','SYSTEM','x')`, uuid.New())
	require.Error(t, err)
	assert.Equal(t, db.SQLStateCheckViolation, db.SQLState(err))
}

// TestIntegration_SQLAndGoAgreeOnTransitions compares every edge.
func TestIntegration_SQLAndGoAgreeOnTransitions(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	for _, from := range AllOrderStatuses() {
		for _, to := range AllOrderStatuses() {
			var legal bool
			require.NoError(t, testDB.QueryRow(ctx, `SELECT cp_equity_order_transition_legal($1,$2)`, string(from), string(to)).Scan(&legal))
			assert.Equal(t, CanTransitionOrder(from, to), legal, "order %s -> %s", from, to)
		}
	}
	for _, from := range AllCorporateActionStatuses() {
		for _, to := range AllCorporateActionStatuses() {
			var legal bool
			require.NoError(t, testDB.QueryRow(ctx, `SELECT cp_equity_action_transition_legal($1,$2)`, string(from), string(to)).Scan(&legal))
			assert.Equal(t, CanTransitionAction(from, to), legal, "action %s -> %s", from, to)
		}
	}
}

var quoted = regexp.MustCompile(`'([^']*)'`)

// TestIntegration_CheckListsMatchTheirGoTwins: every CHECK (col IN (...)) of
// migration 00743 lists exactly what its Go function returns.
func TestIntegration_CheckListsMatchTheirGoTwins(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	strs := func(xs []security.ActorType) []string { return mapStrings(xs) }
	for constraint, want := range map[string][]string{
		"equity_orders_side_check":                               mapStrings(AllSides()),
		"equity_orders_order_type_check":                         mapStrings(AllOrderTypes()),
		"equity_orders_mode_check":                               mapStrings(AllOrderModes()),
		"equity_orders_session_check":                            mapStrings(AllSessions()),
		"equity_orders_status_check":                             mapStrings(AllOrderStatuses()),
		"equity_orders_rejection_code_check":                     mapStrings(AllRejectionCodes()),
		"equity_orders_actor_type_check":                         strs(security.AllActorTypes()),
		"equity_order_transitions_actor_type_check":              strs(AllTransitionActorTypes()),
		"equity_fills_side_check":                                mapStrings(AllSides()),
		"equity_corporate_actions_action_type_check":             mapStrings(AllCorporateActionTypes()),
		"equity_corporate_actions_status_check":                  mapStrings(AllCorporateActionStatuses()),
		"equity_corporate_actions_rounding_policy_check":         mapStrings(AllCorporateRoundings()),
		"equity_corporate_actions_announced_by_actor_type_check": strs(AllTransitionActorTypes()),
		"equity_corporate_action_transitions_actor_type_check":   strs(AllTransitionActorTypes()),
		"corporate_action_applications_rounding_policy_check":    mapStrings(AllCorporateRoundings()),
		"equity_calendar_holidays_calendar_code_check":           AllCalendarCodes(),
		"equity_calendar_holidays_created_by_actor_type_check":   strs(AllTransitionActorTypes()),
	} {
		var def string
		require.NoError(t, testDB.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`, constraint).Scan(&def),
			constraint)
		var got []string
		for _, m := range quoted.FindAllStringSubmatch(def, -1) {
			got = append(got, m[1])
		}
		sort.Strings(got)
		sorted := append([]string(nil), want...)
		sort.Strings(sorted)
		assert.Equal(t, sorted, got, constraint)
	}
}
