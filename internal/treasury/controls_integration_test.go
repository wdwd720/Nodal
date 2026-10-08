//go:build integration

package treasury

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/security"
)

// TestIntegration_HouseAndCustomerLedgersAreSegregated: everything the
// treasury posts touches only HOUSE accounts of its own house book; house
// activity never moves a customer balance; the order book keeps house and
// customer orders apart by default; and the database refuses a treasury
// record whose journal reaches into a customer's books (TY004).
func TestIntegration_HouseAndCustomerLedgersAreSegregated(t *testing.T) {
	f := newFixture(t)
	m := f.market("100.00")
	cust := f.customer(f.usd.ID, units(5_000))
	b := f.register(econ.HouseMarketMaking, false)
	p := f.policy(m)
	p.PaperSpreadBPS, p.PaperFeeBPS = 25, 5
	f.setPolicy(b, p)
	fund := f.fund(b, f.usd.ID, units(10_000))
	f.requireOnlyHouseLegs(b, fund.JournalTransactionID)
	buy := f.paper(b, econ.HouseHedging, m.base.ID, orders.SideBuy, units(3))
	require.NoError(t, buy.Rejection)
	f.requireOnlyHouseLegs(b, buy.Fill.JournalTransactionID)
	walletBefore := f.balance(ledger.CustomerAccount(cust, ledger.CodeWallet, f.usd.ID))

	// A customer bid and a house ask that would cross: the order book refuses
	// the house order rather than let the two books meet.
	bid, err := f.customerOrder(m, cust, orders.SideBuy, "101.00", units(1))
	require.NoError(t, err)
	assert.Equal(t, orders.StatusOpen, bid.Order.Status)
	ask := f.place(f.limit(b, m, econ.HouseMarketMaking, orders.SideSell, "101.00", units(1)))
	requireRefused(t, ask.Rejection, ask.Action, orders.ReasonHouseCrossingForbidden)
	assert.Equal(t, walletBefore.String(), f.balance(ledger.CustomerAccount(cust, ledger.CodeWallet, f.usd.ID)).String())

	// Every journal the treasury recorded is house-only, and no customer
	// account appears on any of them.
	assert.Zero(t, f.count(`SELECT count(*) FROM journal_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
		WHERE e.transaction_id IN (SELECT journal_transaction_id FROM treasury_paper_fills WHERE house_account_id = $1
		                           UNION SELECT journal_transaction_id FROM treasury_fundings WHERE house_account_id = $1)
		  AND (la.owner_type <> 'HOUSE' OR la.owner_id <> $1)`, b.HouseAccountID))
	// A HOUSE code can never be a customer's, and a customer code never a house's.
	assert.Error(t, ledger.CustomerAccount(cust, ledger.CodeHouseInventory, f.usd.ID).Validate())
	assert.Error(t, b.account(ledger.CodeWallet, f.usd.ID).Validate())

	// Around Go: a SIM_TRADE that moves value between the house book and a
	// customer -- each party balanced on its own -- recorded as a house paper
	// fill. The database refuses the record.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		fillID := uuid.New()
		qty := units(1)
		res, err := f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSimTrade, IdempotencyKey: "bypass:" + fillID.String(),
			Reference: ledger.FinancialEventReference{Type: "treasury_paper_fill", ID: fillID.String()}, EffectiveAt: f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: b.account(ledger.CodeHouseInventory, m.base.ID), Side: ledger.Credit, Quantity: qty},
				{Account: b.account(ledger.CodeHouseTradingOutflow, m.base.ID), Side: ledger.Debit, Quantity: qty},
				{Account: ledger.CustomerAccount(cust, ledger.CodeWallet, m.base.ID), Side: ledger.Debit, Quantity: qty},
				{Account: ledger.CustomerAccount(cust, ledger.CodeTradingInflow, m.base.ID), Side: ledger.Credit, Quantity: qty},
			},
		})
		if err != nil {
			return err
		}
		eff, _, err := f.effects.Begin(ctx, tx, econ.BeginRequest{Key: "bypass:" + fillID.String(), Subsystem: econ.SubsystemTreasury,
			Kind: "HOUSE_PAPER_FILL", Request: map[string]string{"x": fillID.String()}, PostsLedger: true, Actor: operator})
		if err != nil {
			return err
		}
		if _, err := f.effects.Apply(ctx, tx, eff.ID, econ.ApplyRequest{JournalTransactionID: &res.TransactionID, Actor: operator}); err != nil {
			return err
		}
		doc := []byte(`{"bypass":true}`)
		sum := sha256.Sum256(doc)
		_, err = tx.Exec(ctx, `INSERT INTO treasury_paper_fills (id, house_account_id, activity, side, base_asset_id, quote_asset_id,
			base_quantity, reference_mantissa, reference_scale, mark_observed_at, notional_at_reference, spread_bps, spread_amount,
			fee_amount, total_quote, schedule, schedule_hash, disclosure, disclosure_hash, idempotency_key, effect_id,
			journal_transaction_id, evidence_ref, evidence_sha256, filled_at)
			VALUES ($1,$2,'HEDGING','SELL',$3,$4,1000000,10000,2,now(),0,0,0,0,0,$5,$6,$5,$6,$7,$8,$9,'bypass',$10,now())`,
			fillID, b.HouseAccountID, m.base.ID, f.usd.ID, doc, hex.EncodeToString(sum[:]), "bypass-"+fillID.String(), eff.ID,
			res.TransactionID, sum[:])
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "TY004", db.SQLState(err), "got %v", err)
	assert.Empty(t, f.reconcile(b))
}

// TestIntegration_HouseActionsAreExactlyOnce: a replayed command returns its
// first outcome and creates nothing; the same key with another request is
// refused; concurrent replays of one key make one order; concurrent syncs
// record each trade once; a paper fill's effect, journal and fill are one.
func TestIntegration_HouseActionsAreExactlyOnce(t *testing.T) {
	f := newFixture(t)
	m := f.market("100.00")
	b := f.register(econ.HouseMarketMaking, false)
	f.setPolicy(b, f.policy(m))
	f.fund(b, f.usd.ID, units(10_000))

	r := f.limit(b, m, econ.HouseMarketMaking, orders.SideBuy, "95.00", units(1))
	first := f.place(r)
	require.NoError(t, first.Rejection)
	second := f.place(r)
	require.NoError(t, second.Rejection)
	assert.Equal(t, first.Action.ID, second.Action.ID)
	assert.Equal(t, first.Order.ID, second.Order.ID)
	changed := r
	changed.Quantity = units(2)
	_, err := do(f, func(ctx context.Context, tx pgx.Tx) (OrderOutcome, error) { return f.svc.PlaceOrder(ctx, tx, changed) })
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, errs.CodeOf(err))

	// Eight concurrent submissions of one key: one order.
	race := f.limit(b, m, econ.HouseMarketMaking, orders.SideBuy, "94.00", units(1))
	var wg sync.WaitGroup
	ids := make(chan orders.OrderID, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := do(f, func(ctx context.Context, tx pgx.Tx) (OrderOutcome, error) { return f.svc.PlaceOrder(ctx, tx, race) })
			if assert.NoError(t, err) && assert.NoError(t, out.Rejection) {
				ids <- out.Order.ID
			}
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[orders.OrderID]bool{}
	for oid := range ids {
		seen[oid] = true
	}
	assert.Len(t, seen, 1)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM treasury_actions WHERE house_account_id = $1 AND idempotency_key = $2`,
		b.HouseAccountID, race.IdempotencyKey))

	// Another house book takes both bids; eight concurrent reports ingest
	// the two trades exactly once.
	seller := f.register(econ.HouseHedging, false)
	f.setPolicy(seller, f.policy(m))
	f.fund(seller, m.base.ID, units(5))
	take := f.place(PlaceOrderRequest{HouseAccountID: seller.HouseAccountID, InstrumentID: m.inst, Activity: econ.HouseHedging,
		Side: orders.SideSell, Type: orders.TypeLimit, TimeInForce: orders.IOC, Price: f.price("90.00"), Quantity: units(2),
		IdempotencyKey: "take-" + uuid.NewString(), Actor: houseRobot})
	require.NoError(t, take.Rejection)
	require.Len(t, take.Trades, 2)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := do(f, func(ctx context.Context, tx pgx.Tx) (Report, error) { return f.svc.Report(ctx, tx, b.HouseAccountID) })
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	assert.Equal(t, 2, f.count(`SELECT count(*) FROM treasury_position_events WHERE house_account_id = $1 AND kind = 'BOOK_FILL'`,
		b.HouseAccountID))
	// A trade between two house books is one event in each register.
	assert.Equal(t, 2, f.count(`SELECT count(*) FROM treasury_position_events WHERE house_account_id = $1 AND kind = 'BOOK_FILL'`,
		seller.HouseAccountID))

	// A paper fill replayed: one fill, one effect, one journal.
	pr := PaperRequest{HouseAccountID: b.HouseAccountID, Activity: econ.HouseHedging, Asset: m.base.ID, Side: orders.SideSell,
		Quantity: units(1), IdempotencyKey: "paper-" + uuid.NewString(), Actor: houseRobot}
	var fills []PaperOutcome
	for i := 0; i < 3; i++ {
		out, err := do(f, func(ctx context.Context, tx pgx.Tx) (PaperOutcome, error) { return f.svc.ExecutePaper(ctx, tx, pr) })
		require.NoError(t, err)
		require.NoError(t, out.Rejection)
		fills = append(fills, out)
	}
	assert.Equal(t, fills[0].Fill.ID, fills[2].Fill.ID)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM treasury_paper_fills WHERE house_account_id = $1 AND idempotency_key = $2`,
		b.HouseAccountID, pr.IdempotencyKey))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM economic_effects WHERE effect_key = $1`,
		"treasury:paper:"+b.HouseAccountID.String()+":"+pr.IdempotencyKey))
	assert.Empty(t, f.reconcile(b))
	assert.Empty(t, f.reconcile(seller))
}

// TestIntegration_OnlyTheHouseServiceOrAnOperatorActsForTheHouse: the signing
// policy names one house service; operators trade only when it allows them
// and may always cancel and administer; customers, agents, system actors and
// any other service are refused, in Go before anything is written.
func TestIntegration_OnlyTheHouseServiceOrAnOperatorActsForTheHouse(t *testing.T) {
	f := newFixture(t)
	m := f.market("100.00")
	b := f.register(econ.HouseMarketMaking, false)
	f.setPolicy(b, f.policy(m))
	f.fund(b, f.usd.ID, units(10_000))
	before := f.count(`SELECT count(*) FROM treasury_actions WHERE house_account_id = $1`, b.HouseAccountID)

	for _, actor := range []econ.Actor{
		{Type: security.ActorUser, ID: uuid.NewString()},
		{Type: security.ActorAgent, ID: uuid.NewString()},
		systemActor,
		otherRobot,
		operator, // operators may not trade for this book
	} {
		r := f.limit(b, m, econ.HouseMarketMaking, orders.SideBuy, "95.00", units(1))
		r.Actor = actor
		_, err := do(f, func(ctx context.Context, tx pgx.Tx) (OrderOutcome, error) { return f.svc.PlaceOrder(ctx, tx, r) })
		require.Error(t, err, "%s %s", actor.Type, actor.ID)
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
		assert.Equal(t, ReasonNotAuthorized, ReasonOf(err))
	}
	_, err := do(f, func(ctx context.Context, tx pgx.Tx) (PolicyVersion, error) {
		return f.svc.SetPolicy(ctx, tx, b.HouseAccountID, f.policy(m), "p-"+uuid.NewString(), "x", houseRobot)
	})
	assert.Equal(t, ReasonNotAuthorized, ReasonOf(err), "a policy is an operator's")
	_, err = do(f, func(ctx context.Context, tx pgx.Tx) (Funding, error) {
		return f.svc.Fund(ctx, tx, FundRequest{HouseAccountID: b.HouseAccountID, Asset: f.usd.ID, Quantity: units(1),
			IdempotencyKey: "f-" + uuid.NewString(), Actor: houseRobot})
	})
	assert.Equal(t, ReasonNotAuthorized, ReasonOf(err), "funding is an operator's")
	assert.Equal(t, before, f.count(`SELECT count(*) FROM treasury_actions WHERE house_account_id = $1`, b.HouseAccountID),
		"a refused actor leaves no action behind")

	// The house service trades; an operator cancels.
	placed := f.place(f.limit(b, m, econ.HouseMarketMaking, orders.SideBuy, "95.00", units(1)))
	require.NoError(t, placed.Rejection)
	c, err := do(f, func(ctx context.Context, tx pgx.Tx) (OrderOutcome, error) {
		return f.svc.CancelOrder(ctx, tx, CancelRequest{HouseAccountID: b.HouseAccountID, OrderID: placed.Order.ID,
			IdempotencyKey: "c-" + uuid.NewString(), Actor: operator})
	})
	require.NoError(t, err)
	require.NoError(t, c.Rejection)

	// Rotating the house service: the old one is refused, the new one acts.
	v, err := do(f, func(ctx context.Context, tx pgx.Tx) (SigningPolicyVersion, error) {
		return f.svc.SetSigningPolicy(ctx, tx, b.HouseAccountID, SigningPolicy{ServiceActorID: otherRobot.ID},
			"sp-"+uuid.NewString(), "rotate the house service", operator)
	})
	require.NoError(t, err)
	assert.Equal(t, 2, v.Version)
	r := f.limit(b, m, econ.HouseMarketMaking, orders.SideBuy, "95.00", units(1))
	_, err = do(f, func(ctx context.Context, tx pgx.Tx) (OrderOutcome, error) { return f.svc.PlaceOrder(ctx, tx, r) })
	assert.Equal(t, ReasonNotAuthorized, ReasonOf(err))
	r.Actor = otherRobot
	out, err := do(f, func(ctx context.Context, tx pgx.Tx) (OrderOutcome, error) { return f.svc.PlaceOrder(ctx, tx, r) })
	require.NoError(t, err)
	require.NoError(t, out.Rejection)
	assert.Equal(t, 2, out.Action.SigningPolicyVersion)

	// The signing policy holds no key, says so, and is hashed.
	var custody, hash string
	var doc []byte
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT key_custody, policy, policy_hash FROM treasury_signing_policies
		WHERE house_account_id = $1 AND version = 2`, b.HouseAccountID).Scan(&custody, &doc, &hash))
	assert.Equal(t, KeyCustodyNone, custody)
	sum := sha256.Sum256(doc)
	assert.Equal(t, hex.EncodeToString(sum[:]), hash)
}

// TestIntegration_TheDatabaseRefusesInvalidTreasuryStates writes around Go
// and watches the database refuse each invalid state the treasury depends on.
func TestIntegration_TheDatabaseRefusesInvalidTreasuryStates(t *testing.T) {
	f := newFixture(t)
	m := f.market("100.00")
	b := f.register(econ.HouseMarketMaking, false)
	pv := f.setPolicy(b, f.policy(m))
	f.fund(b, f.usd.ID, units(1_000))
	buy := f.paper(b, econ.HouseHedging, m.base.ID, orders.SideBuy, units(1))
	require.NoError(t, buy.Rejection)

	action := func(actorType security.ActorType, actorID, kind string) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			sum := sha256.Sum256([]byte(uuid.NewString()))
			activity := any(nil)
			if kind == "PLACE_ORDER" || kind == "PAPER_FILL" {
				activity = "HEDGING"
			}
			_, err := tx.Exec(ctx, `INSERT INTO treasury_actions (id, house_account_id, kind, activity, outcome, rejection_code,
				actor_type, actor_id, signing_policy_version, idempotency_key, request_hash, occurred_at)
				VALUES ($1,$2,$3,$4,'REJECTED','FORGED',$5,$6,1,$7,$8,now())`,
				uuid.New(), b.HouseAccountID, kind, activity, actorType, actorID, "forged-"+uuid.NewString(), sum[:])
			return err
		})
	}
	// A customer, an agent, an unnamed service, an operator trading where the
	// policy forbids it, a service administering: all refused (TY002).
	for _, c := range []struct {
		t    security.ActorType
		id   string
		kind string
	}{
		{security.ActorUser, uuid.NewString(), "PLACE_ORDER"},
		{security.ActorAgent, uuid.NewString(), "PAPER_FILL"},
		{security.ActorService, otherRobot.ID, "PLACE_ORDER"},
		{security.ActorOperator, operator.ID, "PLACE_ORDER"},
		{security.ActorService, houseRobot.ID, "SET_POLICY"},
	} {
		err := action(c.t, c.id, c.kind)
		require.Error(t, err, "%s %s", c.t, c.kind)
		assert.Equal(t, "TY002", db.SQLState(err), "%s %s: %v", c.t, c.kind, err)
	}
	require.NoError(t, action(security.ActorService, houseRobot.ID, "PLACE_ORDER"), "the named house service may act")

	// A policy version out of sequence, or one whose columns lie (TY003).
	pol := f.policy(m)
	doc, err := pol.Document(b.HouseAccountID)
	require.NoError(t, err)
	insertPolicy := func(version int, lossLimit string) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO treasury_policies (id, house_account_id, version, label, valuation_asset_id,
				daily_loss_limit, max_mark_age_ms, policy, policy_hash, actor_type, actor_id, reason, effective_at)
				VALUES ($1,$2,$3,$4,$5,$6::numeric,$7,$8,$9,'OPERATOR','op','raw',now())`, uuid.New(), b.HouseAccountID, version,
				pol.Label, f.usd.ID, lossLimit, pol.MaxMarkAge.Milliseconds(), doc, hashHex(doc))
			return err
		})
	}
	err = insertPolicy(pv.Version+2, pol.DailyLossLimit.String())
	assert.Equal(t, "TY003", db.SQLState(err), "a version gap: %v", err)
	err = insertPolicy(pv.Version+1, "999999999999")
	assert.Equal(t, "TY003", db.SQLState(err), "a loss limit the document does not state: %v", err)

	// A position event that does not continue the position (TY005).
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		seq := f.count(`SELECT max(seq) FROM treasury_position_events WHERE house_account_id = $1`, b.HouseAccountID) + 1
		_, err := tx.Exec(ctx, `INSERT INTO treasury_position_events (id, house_account_id, seq, kind, direction, asset_id, quantity,
			value, fees, qty_before, qty_after, cost_before, cost_after, cost_removed, realized_pnl, paper_fill_id,
			journal_transaction_id, occurred_at)
			VALUES ($1,$2,$3,'PAPER_FILL','INCREASE',$4,1000000,100000000,0,0,1000000,0,100000000,0,0,$5,$6,now())`,
			uuid.New(), b.HouseAccountID, seq, m.base.ID, buy.Fill.ID, buy.Fill.JournalTransactionID)
		return err
	})
	assert.Equal(t, "TY005", db.SQLState(err), "a second record of one fill from a zero position: %v", err)

	// Average cost is checked exactly. The house holds 3 units that cost
	// 300.02; another house book takes 1 from a resting ask, and before the
	// register has seen the trade, a forged record releases floor(300.02 / 3)
	// instead of ceil: refused (CHECK), though it continues the chain and
	// names the trade.
	f.mark(m.base.ID, "100.01")
	require.NoError(t, f.paper(b, econ.HouseHedging, m.base.ID, orders.SideBuy, units(2)).Rejection)
	require.NoError(t, f.place(f.limit(b, m, econ.HouseMarketMaking, orders.SideSell, "100.00", units(1))).Rejection)
	taker := f.register(econ.HouseHedging, false)
	f.setPolicy(taker, f.policy(m))
	f.fund(taker, f.usd.ID, units(1_000))
	took := f.place(PlaceOrderRequest{HouseAccountID: taker.HouseAccountID, InstrumentID: m.inst, Activity: econ.HouseHedging,
		Side: orders.SideBuy, Type: orders.TypeLimit, TimeInForce: orders.IOC, Price: f.price("100.00"), Quantity: units(1),
		IdempotencyKey: "take-" + uuid.NewString(), Actor: houseRobot})
	require.NoError(t, took.Rejection)
	require.Len(t, took.Trades, 1)
	trade := took.Trades[0]
	forge := func(removed int64) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			seq := 1 + f.count(`SELECT max(seq) FROM treasury_position_events WHERE house_account_id = $1`, b.HouseAccountID)
			_, err := tx.Exec(ctx, `INSERT INTO treasury_position_events (id, house_account_id, seq, kind, direction, asset_id,
				quantity, value, fees, qty_before, qty_after, cost_before, cost_after, cost_removed, realized_pnl, book_trade_id,
				instrument_id, journal_transaction_id, occurred_at)
				VALUES ($1,$2,$3,'BOOK_FILL','DECREASE',$4,1000000,100000000,0,3000000,2000000,300020000,$5::bigint,$6::bigint,$7::bigint,
				        $8,$9,$10,now())`,
				uuid.New(), b.HouseAccountID, seq, m.base.ID, 300020000-removed, removed, 100000000-removed, trade.ID, m.inst,
				trade.JournalTransactionID)
			return err
		})
	}
	err = forge(100006666)
	assert.Equal(t, "23514", db.SQLState(err), "ceil(300020000 / 3) is 100006667: %v", err)
	require.NoError(t, forge(100006667), "the exact record is the one the register itself would have written")
	assert.Empty(t, f.reconcile(b), "and the sync recognizes it as the record of that trade")

	// A crossing disclosure that names no rule (TY006), and one that names a
	// rule but does not say "simulated" (CHECK).
	disclose := func(version int, text string) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO treasury_crossing_disclosures (id, instrument_id, book_id, rule_version, allow,
				disclosure, reason, actor_type, actor_id, evidence_ref, evidence_sha256, disclosed_at)
				VALUES ($1,$2,$3,$4,true,$5,'x','OPERATOR','op','e',$6,now())`, uuid.New(), m.inst, m.book.ID, version, text,
				make([]byte, 32))
			return err
		})
	}
	err = disclose(7, "the house may trade with you in this simulated market")
	assert.Equal(t, "TY006", db.SQLState(err), "%v", err)
	_, err = do(f, func(ctx context.Context, tx pgx.Tx) (orders.CrossingRule, error) {
		return f.orders.SetCrossingRule(ctx, tx, m.inst, true, operator, "raw disclosure test")
	})
	require.NoError(t, err)
	err = disclose(1, "the house may trade against you on this book")
	assert.Equal(t, "23514", db.SQLState(err), "%v", err)

	// A class that does not fit the strategy (TY001), and a frozen book.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		h, err := econ.NewRegistry().CreateHouseAccount(ctx, tx, "itest raw "+uuid.NewString(), econ.HouseMarketMaking, operator)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO treasury_books (house_account_id, class, valuation_asset_id, registered_by_actor_type,
			registered_by_actor_id, registered_at) VALUES ($1,'TREASURY',$2,'OPERATOR','op',now())`, h.ID, f.usd.ID)
		return err
	})
	assert.Equal(t, "TY001", db.SQLState(err), "%v", err)
	owner, err := pgx.Connect(f.ctx, testMigrateURL)
	require.NoError(t, err)
	defer owner.Close(f.ctx)
	_, err = owner.Exec(f.ctx, `UPDATE treasury_books SET class = 'TREASURY' WHERE house_account_id = $1`, b.HouseAccountID)
	assert.Equal(t, "TY001", db.SQLState(err), "%v", err)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE treasury_books SET class = 'TREASURY' WHERE house_account_id = $1`, b.HouseAccountID)
		return err
	})
	assert.Equal(t, "42501", db.SQLState(err), "the application cannot even name the column: %v", err)

	// History is append-only, for the application and for the owner alike.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE treasury_actions SET outcome = 'APPLIED' WHERE house_account_id = $1`, b.HouseAccountID)
		return err
	})
	assert.Equal(t, "42501", db.SQLState(err))
	_, err = owner.Exec(f.ctx, `DELETE FROM treasury_position_events WHERE house_account_id = $1`, b.HouseAccountID)
	assert.Equal(t, "P0001", db.SQLState(err))

	// A risk-increasing decision cannot be ALLOW while a treasury limit is
	// breached (CHECK).
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO treasury_risk_decisions (id, house_account_id, intent_kind, asset_id, risk_increasing, stage,
			verdict, reason_codes, treasury_codes, input, decision, limits, input_hash, decision_hash, risk_policy_version,
			risk_policy_hash, evaluator_version, treasury_policy_version, treasury_policy_hash, evaluated_at)
			VALUES ($1,$2,'PROVIDE_LIQUIDITY',$3,true,'PRE_TRADE','ALLOW','{}',ARRAY['TREASURY_DAILY_LOSS_LIMIT'],'{}','{}','{}',
			        'h','h','v','h','e',1,$4,now())`, uuid.New(), b.HouseAccountID, m.base.ID, pv.Hash)
		return err
	})
	assert.Equal(t, "23514", db.SQLState(err), "%v", err)

	// A funding whose journal is not its HOUSE_FUNDING (TY004).
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO treasury_fundings (id, house_account_id, asset_id, quantity, cost_basis, idempotency_key,
			effect_id, journal_transaction_id, evidence_ref, evidence_sha256, funded_at)
			VALUES ($1,$2,$3,1,1,$4,$5,$6,'e',$7,now())`, uuid.New(), b.HouseAccountID, f.usd.ID, "raw-"+uuid.NewString(),
			*buy.Action.EffectID, buy.Fill.JournalTransactionID, make([]byte, 32))
		return err
	})
	assert.Equal(t, "TY004", db.SQLState(err), "%v", err)
}
