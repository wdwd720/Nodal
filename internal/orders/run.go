package orders

import (
	"context"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/matching"
	"github.com/nodal/controlplane/internal/money"
)

// commandSpec is one command against a book whose row lock the transaction
// holds and whose engine the caller has acquired.
type commandSpec struct {
	cfg     *bookConfig
	cur     bookCursor
	eng     *engineEntry
	kind    CommandKind
	cmd     matching.Command
	orderID OrderID
	actor   econ.Actor
	now     time.Time
	// birth is the row of a house order that does not exist yet; it is
	// inserted with the state the engine gives it.
	birth *orderRow
	// stageTaker is the customer order whose intent the calling stage moves
	// itself; this command does not complete that intent.
	stageTaker *OrderID
	// crossingRuleVersion is the rule version that allowed a house book to
	// trade with a customer in this command (0 when none was needed).
	crossingRuleVersion int
}

// commandResult is what a command did.
type commandResult struct {
	Seq          int64
	Events       []matching.Event
	Rejected     bool
	RejectReason string
	Orders       map[string]orderRow
	Trades       []Trade
	StateHash    [32]byte
	Evidence     econ.EvidenceRef
}

// Trade is a book_trades row.
type Trade struct {
	ID                   id.ID[id.Any]
	BookID               BookID
	TradeSeq             int64
	CommandSeq           int64
	EventSeq             int64
	PriceTicks           int64
	QtyLots              int64
	Price                money.Price
	BaseQuantity         money.Quantity
	QuoteNotional        money.Quantity
	MakerOrderID         OrderID
	TakerOrderID         OrderID
	MakerOwnerType       OwnerType
	TakerOwnerType       OwnerType
	MakerOwnerID         string
	TakerOwnerID         string
	AggressorSide        Side
	MakerFee             money.Quantity
	TakerFee             money.Quantity
	MakerScheduleHash    string
	TakerScheduleHash    string
	CrossingRuleVersion  int
	JournalTransactionID ledger.TransactionID
	EffectID             econ.EffectID
	EvidenceRef          string
	EvidenceSHA256       []byte
}

// runCommand applies spec.cmd to the engine and makes every consequence of
// it durable in the caller's transaction. On any error the cached engine is
// invalidated: it already holds the command, and the transaction that would
// have recorded it is about to roll back.
func (s *Service) runCommand(ctx context.Context, tx pgx.Tx, spec commandSpec) (out commandResult, err error) {
	book := spec.eng.book
	res, err := book.Apply(spec.cmd)
	if err != nil {
		return commandResult{}, errs.Wrap(err, errs.CodeInternal, "orders: the engine refused a command value")
	}
	defer func() {
		if err != nil {
			spec.eng.invalidate()
		}
	}()
	if res.CommandSeq != spec.cur.LastCommandSeq+1 || len(res.Events) == 0 {
		return commandResult{}, errs.New(errs.CodeInternal, "orders: the engine is not at the journal's cursor")
	}
	hash := book.StateHash()
	spec.eng.seq, spec.eng.hash = res.CommandSeq, hash
	out = commandResult{Seq: res.CommandSeq, Events: res.Events, StateHash: hash, Orders: map[string]orderRow{}}
	if reason, rejected := res.Rejected(); rejected {
		out.Rejected, out.RejectReason = true, string(reason)
	}
	if err := s.journal(ctx, tx, spec, res, hash, book.TradeSeq()); err != nil {
		return commandResult{}, err
	}
	if res.CommandSeq%spec.cfg.SnapshotInterval == 0 {
		snap := book.Snapshot()
		if _, err := tx.Exec(ctx, `INSERT INTO book_snapshots (id, book_id, command_seq, snapshot, state_hash) VALUES ($1,$2,$3,$4,$5)`,
			id.New[id.Any](), spec.cfg.ID, res.CommandSeq, snap, sha(snap)); err != nil {
			return commandResult{}, dbErr("record snapshot", err)
		}
	}
	if err := s.persistOrders(ctx, tx, spec, res, &out); err != nil {
		return commandResult{}, err
	}
	fullFills := map[string]bool{}
	for _, e := range res.Events {
		if e.Type == matching.EventFullFill {
			fullFills[e.OrderID+"#"+strconv.FormatInt(e.TradeSeq, 10)] = true
		}
	}
	for _, e := range res.Events {
		if e.Type != matching.EventTrade {
			continue
		}
		t, err := s.settleTrade(ctx, tx, spec, e, out.Orders, fullFills)
		if err != nil {
			return commandResult{}, err
		}
		out.Trades = append(out.Trades, t)
	}
	if err := s.finishOrders(ctx, tx, spec, &out); err != nil {
		return commandResult{}, err
	}
	ref, err := s.commandEvidence(ctx, tx, spec, out)
	if err != nil {
		return commandResult{}, err
	}
	out.Evidence = ref
	return out, nil
}

// journal writes the command row, moves the book's cursor to it and records
// the command's events. The command row and the cursor move together: the
// database refuses one without the other (OB001, OB002).
func (s *Service) journal(ctx context.Context, tx pgx.Tx, spec commandSpec, res matching.Result, hash [32]byte, tradeSeq int64) error {
	b, err := EncodeCommand(spec.cmd)
	if err != nil {
		return err
	}
	first, last := res.Events[0].Seq, res.Events[len(res.Events)-1].Seq
	reason, rejected := res.Rejected()
	var rejectReason any
	if rejected {
		rejectReason = string(reason)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO book_commands (id, book_id, seq, kind, order_id, command, command_sha256,
		timestamp_unix_ns, rejected, reject_reason, first_event_seq, last_event_seq, trade_seq_after, prev_state_hash,
		state_hash_after, actor_type, actor_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		id.New[id.Any](), spec.cfg.ID, res.CommandSeq, spec.kind, spec.orderID, b, sha(b), commandTimestamp(spec.cmd),
		rejected, rejectReason, first, last, tradeSeq, spec.cur.StateHash[:], hash[:], spec.actor.Type, spec.actor.ID); err != nil {
		return dbErr("journal command", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE book_states SET last_command_seq = $2, last_event_seq = $3, last_trade_seq = $4, state_hash = $5
		WHERE id = $1`, spec.cfg.ID, res.CommandSeq, last, tradeSeq, hash[:]); err != nil {
		return dbErr("advance book cursor", err)
	}
	batch := &pgx.Batch{}
	for _, e := range res.Events {
		eb, err := EncodeEvent(e)
		if err != nil {
			return err
		}
		var orderID, ownerID, trade any
		switch {
		case e.Type == matching.EventTrade:
			orderID, ownerID, trade = e.TakerOrderID, e.TakerOwnerID, e.TradeSeq
		case e.OrderID != "":
			orderID, ownerID = e.OrderID, e.OwnerID
			if e.TradeSeq != 0 {
				trade = e.TradeSeq
			}
		}
		batch.Queue(`INSERT INTO book_events (id, book_id, seq, command_seq, event_type, order_id, owner_id, trade_seq, event)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			id.New[id.Any](), spec.cfg.ID, e.Seq, e.CommandSeq, e.Type.String(), orderID, ownerID, trade, eb)
	}
	br := tx.SendBatch(ctx, batch)
	for range res.Events {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return dbErr("record events", err)
		}
	}
	if err := br.Close(); err != nil {
		return dbErr("record events", err)
	}
	return nil
}

func commandTimestamp(c matching.Command) int64 {
	switch v := c.(type) {
	case matching.NewOrder:
		return v.Timestamp
	case matching.Cancel:
		return v.Timestamp
	case matching.Replace:
		return v.Timestamp
	}
	return 0
}

// touchedOrders lists, in first-touched order, every order a command's
// events name.
func touchedOrders(events []matching.Event) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, e := range events {
		add(e.OrderID)
		add(e.MakerOrderID)
		add(e.TakerOrderID)
	}
	return out
}

// persistOrders writes the engine's state of every order the command touched:
// one state change per order per command, each with its transition row.
func (s *Service) persistOrders(ctx context.Context, tx pgx.Tx, spec commandSpec, res matching.Result, out *commandResult) error {
	ids := touchedOrders(res.Events)
	var existing []string
	for _, oid := range ids {
		if spec.birth == nil || oid != spec.birth.ID.String() {
			existing = append(existing, oid)
		}
	}
	rows, err := lockOrders(ctx, tx, existing)
	if err != nil {
		return err
	}
	reason := "command " + strconv.FormatInt(res.CommandSeq, 10) + " " + string(spec.kind)
	for _, oid := range ids {
		st, ok := spec.eng.book.Order(oid)
		if !ok {
			return errs.New(errs.CodeInternal, "orders: an event names an order the engine does not know").WithField("order_id", oid)
		}
		if spec.birth != nil && oid == spec.birth.ID.String() {
			after := withEngineState(*spec.birth, st, res.CommandSeq)
			if err := insertOrder(ctx, tx, after); err != nil {
				return err
			}
			out.Orders[oid] = after
			continue
		}
		before, ok := rows[oid]
		if !ok {
			return errs.New(errs.CodeInternal, "orders: the book holds an order the database does not").WithField("order_id", oid)
		}
		after := withEngineState(before, st, res.CommandSeq)
		why := reason
		if after.Status == StatusCancelled && after.CancelReason != "" {
			why += ": " + after.CancelReason
		}
		if after.Status == StatusRejected && after.RejectReason != "" {
			why += ": " + after.RejectReason
		}
		if err := updateOrder(ctx, tx, before, after, &out.Seq, spec.actor, why, spec.now); err != nil {
			return err
		}
		out.Orders[oid] = after
	}
	return nil
}

// settleTrade makes one TRADE event an economic fact: the SIM_TRADE posting
// between the two parties (each balanced on its own books, explicit fees to
// PLATFORM_FEE_REVENUE, no spread), the consumption of both reservations, the
// effect that names the journal, the evidence, and the trade row.
func (s *Service) settleTrade(ctx context.Context, tx pgx.Tx, spec commandSpec, e matching.Event, orders map[string]orderRow, fullFills map[string]bool) (Trade, error) {
	maker, mok := orders[e.MakerOrderID]
	taker, tok := orders[e.TakerOrderID]
	if !mok || !tok {
		return Trade{}, errs.New(errs.CodeInternal, "orders: a trade names an order that was not persisted")
	}
	if maker.OwnerID == taker.OwnerID {
		return Trade{}, errs.New(errs.CodeInternal, "orders: the engine produced a self-trade")
	}
	crossRule := 0
	if maker.OwnerType != taker.OwnerType {
		if spec.crossingRuleVersion < 1 {
			return Trade{}, errs.New(errs.CodeInternal, "orders: a house book traded with a customer without a crossing rule")
		}
		crossRule = spec.crossingRuleVersion
	}
	t := spec.cfg.Terms
	buyer, seller := taker, maker
	if e.AggressorSide == matching.SideSell {
		buyer, seller = maker, taker
	}
	lots, ticks := e.QtyLots, e.PriceTicks
	trade := Trade{
		ID: id.New[id.Any](), BookID: spec.cfg.ID, TradeSeq: e.TradeSeq, CommandSeq: e.CommandSeq, EventSeq: e.Seq,
		PriceTicks: ticks, QtyLots: lots, Price: t.TicksToPrice(ticks, "internal-book", spec.now),
		BaseQuantity: t.LotsToQuantity(lots), QuoteNotional: t.Notional(ticks, lots),
		MakerOrderID: maker.ID, TakerOrderID: taker.ID, MakerOwnerType: maker.OwnerType, TakerOwnerType: taker.OwnerType,
		MakerOwnerID: maker.OwnerID, TakerOwnerID: taker.OwnerID, AggressorSide: sideOf(e.AggressorSide),
		MakerScheduleHash: spec.cfg.MakerScheduleHash, TakerScheduleHash: spec.cfg.TakerScheduleHash,
		CrossingRuleVersion: crossRule,
	}
	var err error
	if maker.OwnerType == OwnerCustomer {
		if trade.MakerFee, err = fillFee(spec.cfg.MakerSchedule, maker.Side, t, lots, ticks, maker.strategy(), trade.Price); err != nil {
			return Trade{}, err
		}
	}
	if taker.OwnerType == OwnerCustomer {
		if trade.TakerFee, err = fillFee(spec.cfg.TakerSchedule, taker.Side, t, lots, ticks, taker.strategy(), trade.Price); err != nil {
			return Trade{}, err
		}
	}
	buyerFee, sellerFee := trade.TakerFee, trade.MakerFee
	if buyer.ID == maker.ID {
		buyerFee, sellerFee = trade.MakerFee, trade.TakerFee
	}

	key := "orders:trade:" + spec.cfg.ID.String() + ":" + strconv.FormatInt(e.TradeSeq, 10)
	request := map[string]any{
		"book_id": spec.cfg.ID.String(), "trade_seq": e.TradeSeq, "command_seq": e.CommandSeq,
		"price_ticks": ticks, "qty_lots": lots, "maker_order_id": maker.ID.String(), "taker_order_id": taker.ID.String(),
		"base_quantity": trade.BaseQuantity.String(), "quote_notional": trade.QuoteNotional.String(),
		"maker_fee": trade.MakerFee.String(), "taker_fee": trade.TakerFee.String(),
	}
	effectAccount, effectIntent := tradeEffectOwner(taker, maker)
	eff, existed, err := s.effects.Begin(ctx, tx, econ.BeginRequest{
		Key: key, Subsystem: econ.SubsystemOrders, Kind: "SIM_TRADE", IntentID: effectIntent, AccountID: effectAccount,
		Request: request, PostsLedger: true, Actor: s.actor,
	})
	if err != nil {
		return Trade{}, err
	}
	if existed {
		return Trade{}, errs.New(errs.CodeConflict, "orders: this trade's effect was already claimed").WithField("effect_key", key)
	}
	posting := ledger.Posting{
		Kind: ledger.KindSimTrade, IdempotencyKey: key,
		Reference:   ledger.FinancialEventReference{Type: "book_trade", ID: trade.ID.String()},
		EffectiveAt: spec.now, Description: "simulated internal-book trade",
		Entries: tradeEntries(buyer, seller, t.BaseAsset, t.QuoteAsset, trade.BaseQuantity, trade.QuoteNotional, buyerFee, sellerFee),
		Metadata: map[string]any{
			"venue_label": VenueLabelInternalBook, "book_id": spec.cfg.ID.String(), "trade_seq": e.TradeSeq,
			"maker_order_id": maker.ID.String(), "taker_order_id": taker.ID.String(),
			"maker_fee": trade.MakerFee.String(), "taker_fee": trade.TakerFee.String(),
			"maker_schedule_hash": spec.cfg.MakerScheduleHash, "taker_schedule_hash": spec.cfg.TakerScheduleHash,
		},
	}
	posted, err := s.ledger.Post(s.postingContext(ctx), tx, posting)
	if err != nil {
		return Trade{}, err
	}
	trade.JournalTransactionID = posted.TransactionID
	ev, err := econ.RecordEvidence(ctx, tx, econ.Evidence{
		Key: "orders/book/" + spec.cfg.ID.String() + "/trade/" + strconv.FormatInt(e.TradeSeq, 10),
		Document: map[string]any{
			"kind": "orders.book_trade.v1", "venue_label": VenueLabelInternalBook, "book_id": spec.cfg.ID.String(),
			"instrument_id": spec.cfg.InstrumentID.String(), "trade_seq": e.TradeSeq, "command_seq": e.CommandSeq,
			"event_seq": e.Seq, "price": t.PriceString(ticks), "price_ticks": ticks, "qty_lots": lots,
			"base_asset_id": t.BaseAsset.String(), "quote_asset_id": t.QuoteAsset.String(),
			"base_quantity": trade.BaseQuantity.String(), "quote_notional": trade.QuoteNotional.String(),
			"maker": map[string]any{"order_id": maker.ID.String(), "owner_type": string(maker.OwnerType), "owner_id": maker.OwnerID,
				"side": string(maker.Side), "fee": trade.MakerFee.String(), "schedule_hash": spec.cfg.MakerScheduleHash},
			"taker": map[string]any{"order_id": taker.ID.String(), "owner_type": string(taker.OwnerType), "owner_id": taker.OwnerID,
				"side": string(taker.Side), "fee": trade.TakerFee.String(), "schedule_hash": spec.cfg.TakerScheduleHash},
			"spread": "0", "crossing_rule_version": crossRule,
			"journal_transaction_id": posted.TransactionID.String(),
		},
	})
	if err != nil {
		return Trade{}, err
	}
	trade.EvidenceRef, trade.EvidenceSHA256 = ev.URI, ev.SHA256
	applied, err := s.effects.Apply(ctx, tx, eff.ID, econ.ApplyRequest{
		JournalTransactionID: &posted.TransactionID, Evidence: &ev, Actor: s.actor, Reason: "internal-book trade posted",
	})
	if err != nil {
		return Trade{}, err
	}
	trade.EffectID = applied.ID

	// The buyer pays the notional and its own fee from its reservation; the
	// seller delivers the base quantity from its own.
	if err := s.consume(ctx, tx, buyer, trade.QuoteNotional.Add(buyerFee), fullFills[buyer.ID.String()+"#"+strconv.FormatInt(e.TradeSeq, 10)]); err != nil {
		return Trade{}, err
	}
	if err := s.consume(ctx, tx, seller, trade.BaseQuantity, fullFills[seller.ID.String()+"#"+strconv.FormatInt(e.TradeSeq, 10)]); err != nil {
		return Trade{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO book_trades (id, book_id, instrument_id, trade_seq, command_seq, event_seq, price_ticks,
		qty_lots, maker_order_id, taker_order_id, maker_owner_type, maker_owner_id, taker_owner_type, taker_owner_id, aggressor_side,
		base_asset_id, quote_asset_id, base_quantity, quote_notional, maker_fee, taker_fee, maker_schedule_hash, taker_schedule_hash,
		crossing_rule_version, journal_transaction_id, effect_id, evidence_ref, evidence_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::uuid,$13,$14::uuid,$15,$16,$17,$18::numeric,$19::numeric,$20::numeric,$21::numeric,
		        $22,$23,$24,$25,$26,$27,$28)`,
		trade.ID, spec.cfg.ID, spec.cfg.InstrumentID, e.TradeSeq, e.CommandSeq, e.Seq, ticks, lots, maker.ID, taker.ID,
		maker.OwnerType, maker.OwnerID, taker.OwnerType, taker.OwnerID, trade.AggressorSide, t.BaseAsset, t.QuoteAsset,
		trade.BaseQuantity.String(), trade.QuoteNotional.String(), trade.MakerFee.String(), trade.TakerFee.String(),
		spec.cfg.MakerScheduleHash, spec.cfg.TakerScheduleHash, crossRule, posted.TransactionID, applied.ID, ev.URI, ev.SHA256); err != nil {
		return Trade{}, dbErr("record trade", err)
	}
	return trade, nil
}

// tradeEffectOwner names the account (and intent) a trade's effect is filed
// under: the customer whose command caused it, else the customer it traded
// with, else none (two house books).
func tradeEffectOwner(taker, maker orderRow) (string, *econ.IntentID) {
	if taker.OwnerType == OwnerCustomer {
		return taker.OwnerID, taker.IntentID
	}
	if maker.OwnerType == OwnerCustomer {
		return maker.OwnerID, maker.IntentID
	}
	return "", nil
}

// strategy reports whether the order was originated by a strategy, which is
// what a schedule's strategy fee keys on.
func (o orderRow) strategy() bool { return o.StrategyOriginated }

func hexOf(b []byte) string { return hex.EncodeToString(b) }

// lockIntent locks an economic intent's row and returns it.
func lockIntent(ctx context.Context, tx pgx.Tx, store *econ.IntentStore, iid econ.IntentID) (econ.Intent, error) {
	var got econ.IntentID
	if err := tx.QueryRow(ctx, `SELECT id FROM economic_intents WHERE id = $1 FOR UPDATE`, iid).Scan(&got); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return econ.Intent{}, errs.New(errs.CodeNotFound, "orders: economic intent not found").WithField("intent_id", iid.String())
		}
		return econ.Intent{}, dbErr("lock intent", err)
	}
	return store.Get(ctx, tx, iid)
}

// walletRef, outflowRef and inflowRef are a party's accounts for an asset:
// the customer chart for a customer, the HOUSE_* codes for a house book.
func walletRef(o orderRow, asset assets.AssetID) ledger.AccountRef {
	if o.OwnerType == OwnerHouse {
		return ledger.AccountRef{OwnerType: ledger.OwnerHouse, OwnerID: o.OwnerID, Code: ledger.CodeHouseInventory, AssetID: asset}
	}
	return ledger.CustomerAccount(*o.AccountID, ledger.CodeWallet, asset)
}

func outflowRef(o orderRow, asset assets.AssetID) ledger.AccountRef {
	if o.OwnerType == OwnerHouse {
		return ledger.AccountRef{OwnerType: ledger.OwnerHouse, OwnerID: o.OwnerID, Code: ledger.CodeHouseTradingOutflow, AssetID: asset}
	}
	return ledger.CustomerAccount(*o.AccountID, ledger.CodeTradingOutflow, asset)
}

func inflowRef(o orderRow, asset assets.AssetID) ledger.AccountRef {
	if o.OwnerType == OwnerHouse {
		return ledger.AccountRef{OwnerType: ledger.OwnerHouse, OwnerID: o.OwnerID, Code: ledger.CodeHouseTradingInflow, AssetID: asset}
	}
	return ledger.CustomerAccount(*o.AccountID, ledger.CodeTradingInflow, asset)
}

// tradeEntries is the SIM_TRADE template of ADVANCED_FINANCIAL_MODEL §5:
//
//	buyer:  Cr WALLET(quote) / Dr TRADING_OUTFLOW(quote), Dr WALLET(base) / Cr TRADING_INFLOW(base)
//	seller: Cr WALLET(base) / Dr TRADING_OUTFLOW(base), Dr WALLET(quote) / Cr TRADING_INFLOW(quote)
//	explicit fee of a customer party: C: Dr FEES_PLATFORM / Cr WALLET; P: Dr PLATFORM_FEE_RECEIVABLE / Cr PLATFORM_FEE_REVENUE
//
// Every party balances on its own books per asset, and a house party uses the
// HOUSE_* codes. A house book pays no platform fee: it is the platform's own
// book, and a fee from it to the platform would be revenue from itself.
func tradeEntries(buyer, seller orderRow, base, quote assets.AssetID, qty, notional, buyerFee, sellerFee money.Quantity) []ledger.Entry {
	out := []ledger.Entry{
		{Account: walletRef(buyer, quote), Side: ledger.Credit, Quantity: notional},
		{Account: outflowRef(buyer, quote), Side: ledger.Debit, Quantity: notional},
		{Account: walletRef(buyer, base), Side: ledger.Debit, Quantity: qty},
		{Account: inflowRef(buyer, base), Side: ledger.Credit, Quantity: qty},
		{Account: walletRef(seller, base), Side: ledger.Credit, Quantity: qty},
		{Account: outflowRef(seller, base), Side: ledger.Debit, Quantity: qty},
		{Account: walletRef(seller, quote), Side: ledger.Debit, Quantity: notional},
		{Account: inflowRef(seller, quote), Side: ledger.Credit, Quantity: notional},
	}
	for _, p := range []struct {
		o   orderRow
		fee money.Quantity
	}{{buyer, buyerFee}, {seller, sellerFee}} {
		if p.o.OwnerType != OwnerCustomer || !p.fee.IsPositive() {
			continue
		}
		out = append(out, feeEntries(*p.o.AccountID, quote, ledger.CodeFeesPlatform, ledger.CodePlatformFeeRevenue, p.fee)...)
	}
	return out
}

// feeEntries is one disclosed charge: the customer's expense against its
// wallet, and the platform's receivable against the revenue code.
func feeEntries(account accounts.AccountID, asset assets.AssetID, expense, revenue ledger.Code, amount money.Quantity) []ledger.Entry {
	return []ledger.Entry{
		{Account: ledger.CustomerAccount(account, expense, asset), Side: ledger.Debit, Quantity: amount},
		{Account: ledger.CustomerAccount(account, ledger.CodeWallet, asset), Side: ledger.Credit, Quantity: amount},
		{Account: ledger.PlatformAccount(ledger.CodePlatformFeeReceivable, asset), Side: ledger.Debit, Quantity: amount},
		{Account: ledger.PlatformAccount(revenue, asset), Side: ledger.Credit, Quantity: amount},
	}
}

// consume records that an order used amount of its reservation; final closes
// the reservation and hands its unused remainder back.
func (s *Service) consume(ctx context.Context, tx pgx.Tx, o orderRow, amount money.Quantity, final bool) error {
	if o.OwnerType == OwnerHouse {
		return s.consumeHouse(ctx, tx, o, amount, final)
	}
	rid, err := capital.ParseReservationID(o.ReservationID)
	if err != nil || rid.IsZero() {
		return errs.New(errs.CodeInternal, "orders: a customer order traded without a reservation").WithField("order_id", o.ID.String())
	}
	if final {
		_, err = s.capital.ConsumeFinal(ctx, tx, rid, amount, 0, o.ID.String())
	} else {
		_, err = s.capital.Consume(ctx, tx, rid, amount, 0, o.ID.String())
	}
	return err
}

// release hands an order's unused reservation back.
func (s *Service) release(ctx context.Context, tx pgx.Tx, o orderRow, reason string) error {
	if o.ReservationID == "" {
		return nil
	}
	if o.OwnerType == OwnerHouse {
		return s.releaseHouse(ctx, tx, o, reason)
	}
	rid, err := capital.ParseReservationID(o.ReservationID)
	if err != nil {
		return errs.New(errs.CodeInternal, "orders: an order names a malformed reservation").WithField("order_id", o.ID.String())
	}
	r, err := s.capital.Get(ctx, tx, rid)
	if err != nil {
		return err
	}
	if r.Status != capital.ReservationActive {
		return nil
	}
	_, err = s.capital.Release(ctx, tx, rid, reason)
	return err
}

// finishOrders releases what cancelled and rejected orders no longer need and
// completes the intents of customer orders that reached the end of their life
// in this command.
func (s *Service) finishOrders(ctx context.Context, tx pgx.Tx, spec commandSpec, out *commandResult) error {
	for _, oid := range touchedOrders(out.Events) {
		o := out.Orders[oid]
		if o.Status == StatusCancelled || o.Status == StatusRejected {
			if err := s.release(ctx, tx, o, "order "+string(o.Status)+": "+o.CancelReason+o.RejectReason); err != nil {
				return err
			}
		}
		if o.OwnerType != OwnerCustomer || !o.Status.Terminal() || o.Status == StatusRejected {
			continue
		}
		if spec.stageTaker != nil && *spec.stageTaker == o.ID {
			continue
		}
		if err := s.completeIntent(ctx, tx, o, spec.actor); err != nil {
			return err
		}
	}
	return nil
}

// completeIntent moves the intent of a finished customer order from EXECUTING
// to COMPLETED. An intent that is not EXECUTING is left alone: its own stage
// will see the finished order.
func (s *Service) completeIntent(ctx context.Context, tx pgx.Tx, o orderRow, actor econ.Actor) error {
	if o.IntentID == nil {
		return nil
	}
	in, err := lockIntent(ctx, tx, s.intents, *o.IntentID)
	if err != nil {
		return err
	}
	if in.Status != econ.StatusExecuting {
		return nil
	}
	_, err = s.intents.Transition(ctx, tx, in.ID, econ.StatusCompleted, actor, "order "+string(o.Status), "")
	return err
}

// commandEvidence archives what one command did.
func (s *Service) commandEvidence(ctx context.Context, tx pgx.Tx, spec commandSpec, out commandResult) (econ.EvidenceRef, error) {
	trades := make([]any, 0, len(out.Trades))
	for _, t := range out.Trades {
		trades = append(trades, map[string]any{"trade_seq": t.TradeSeq, "journal_transaction_id": t.JournalTransactionID.String(),
			"effect_id": t.EffectID.String()})
	}
	orders := make([]any, 0, len(out.Orders))
	for _, oid := range touchedOrders(out.Events) {
		o := out.Orders[oid]
		orders = append(orders, map[string]any{"order_id": oid, "status": string(o.Status), "filled_lots": o.FilledLots,
			"remaining_lots": o.RemainingLots, "cancelled_lots": o.CancelledLots})
	}
	cmdBytes, err := EncodeCommand(spec.cmd)
	if err != nil {
		return econ.EvidenceRef{}, err
	}
	return econ.RecordEvidence(ctx, tx, econ.Evidence{
		Key: "orders/book/" + spec.cfg.ID.String() + "/command/" + strconv.FormatInt(out.Seq, 10),
		Document: map[string]any{
			"kind": "orders.book_command.v1", "venue_label": VenueLabelInternalBook, "book_id": spec.cfg.ID.String(),
			"instrument_id": spec.cfg.InstrumentID.String(), "command_seq": out.Seq, "command_kind": string(spec.kind),
			"order_id": spec.orderID.String(), "command_sha256": hexOf(sha(cmdBytes)), "rejected": out.Rejected,
			"reject_reason": out.RejectReason, "events": len(out.Events), "prev_state_hash": hexOf(spec.cur.StateHash[:]),
			"state_hash_after": hexOf(out.StateHash[:]), "orders": orders, "trades": trades,
			"crossing_rule_version": spec.crossingRuleVersion, "actor_type": string(spec.actor.Type), "actor_id": spec.actor.ID,
		},
	})
}
