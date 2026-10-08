package orders

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/matching"
	"github.com/nodal/controlplane/internal/money"
)

// Order returns one order.
func (s *Service) Order(ctx context.Context, q db.Querier, oid OrderID) (OrderView, error) {
	o, err := loadOrder(ctx, q, oid, false)
	if err != nil {
		return OrderView{}, err
	}
	c, _, err := s.readBookByID(ctx, q, o.BookID, false)
	if err != nil {
		return OrderView{}, err
	}
	return viewOf(c, o), nil
}

// OrderEvents returns the engine events that name an order, in sequence.
func (s *Service) OrderEvents(ctx context.Context, q db.Querier, oid OrderID) ([]matching.Event, error) {
	rows, err := q.Query(ctx, `SELECT event FROM book_events WHERE order_id = $1 ORDER BY seq`, oid.String())
	if err != nil {
		return nil, dbErr("order events", err)
	}
	defer rows.Close()
	var out []matching.Event
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, dbErr("order events", err)
		}
		e, err := DecodeEvent(b)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

const tradeColumns = `id, book_id, trade_seq, command_seq, event_seq, price_ticks, qty_lots, base_quantity::text, quote_notional::text,
	maker_order_id, taker_order_id, maker_owner_type, taker_owner_type, maker_owner_id::text, taker_owner_id::text, aggressor_side,
	maker_fee::text, taker_fee::text, maker_schedule_hash, taker_schedule_hash, crossing_rule_version, journal_transaction_id,
	effect_id, evidence_ref, evidence_sha256, created_at`

func scanTrade(row pgx.Row, t BookTerms) (Trade, error) {
	var tr Trade
	var base, notional, makerFee, takerFee string
	var created time.Time
	if err := row.Scan(&tr.ID, &tr.BookID, &tr.TradeSeq, &tr.CommandSeq, &tr.EventSeq, &tr.PriceTicks, &tr.QtyLots, &base, &notional,
		&tr.MakerOrderID, &tr.TakerOrderID, &tr.MakerOwnerType, &tr.TakerOwnerType, &tr.MakerOwnerID, &tr.TakerOwnerID,
		&tr.AggressorSide, &makerFee, &takerFee, &tr.MakerScheduleHash, &tr.TakerScheduleHash, &tr.CrossingRuleVersion,
		&tr.JournalTransactionID, &tr.EffectID, &tr.EvidenceRef, &tr.EvidenceSHA256, &created); err != nil {
		return Trade{}, err
	}
	for _, p := range []struct {
		dst *money.Quantity
		src string
	}{{&tr.BaseQuantity, base}, {&tr.QuoteNotional, notional}, {&tr.MakerFee, makerFee}, {&tr.TakerFee, takerFee}} {
		v, err := money.ScanQuantity(p.src)
		if err != nil {
			return Trade{}, err
		}
		*p.dst = v
	}
	tr.Price = t.TicksToPrice(tr.PriceTicks, "internal-book", created)
	return tr, nil
}

// Trades returns a book's trades after a trade sequence, oldest first.
func (s *Service) Trades(ctx context.Context, q db.Querier, instrumentID instruments.InstrumentID, afterSeq int64, limit int) ([]Trade, error) {
	if limit <= 0 || limit > 10_000 {
		limit = 500
	}
	c, _, err := s.readBook(ctx, q, instrumentID, false)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT `+tradeColumns+` FROM book_trades WHERE book_id = $1 AND trade_seq > $2 ORDER BY trade_seq LIMIT $3`,
		c.ID, afterSeq, limit)
	if err != nil {
		return nil, dbErr("trades", err)
	}
	defer rows.Close()
	var out []Trade
	for rows.Next() {
		t, err := scanTrade(rows, c.Terms)
		if err != nil {
			return nil, dbErr("scan trade", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DepthLevel is one aggregated price level of the persisted book.
type DepthLevel struct {
	PriceTicks int64
	Price      string
	QtyLots    int64
	Orders     int
}

// Depth is the aggregated resting book as persisted, best level first.
type Depth struct {
	Bids []DepthLevel
	Asks []DepthLevel
}

// Depth returns up to levels price levels per side of the persisted book.
func (s *Service) Depth(ctx context.Context, q db.Querier, instrumentID instruments.InstrumentID, levels int) (Depth, error) {
	if levels <= 0 || levels > 1000 {
		levels = 50
	}
	c, _, err := s.readBook(ctx, q, instrumentID, false)
	if err != nil {
		return Depth{}, err
	}
	d := Depth{Bids: []DepthLevel{}, Asks: []DepthLevel{}}
	for _, side := range AllSides() {
		order := "DESC"
		if side == SideSell {
			order = "ASC"
		}
		rows, err := q.Query(ctx, `SELECT price_ticks, sum(remaining_lots)::bigint, count(*) FROM book_orders
			WHERE book_id = $1 AND side = $2 AND status IN ('OPEN','PARTIALLY_FILLED')
			GROUP BY price_ticks ORDER BY price_ticks `+order+` LIMIT $3`, c.ID, side, levels)
		if err != nil {
			return Depth{}, dbErr("depth", err)
		}
		for rows.Next() {
			var l DepthLevel
			if err := rows.Scan(&l.PriceTicks, &l.QtyLots, &l.Orders); err != nil {
				rows.Close()
				return Depth{}, dbErr("depth", err)
			}
			l.Price = c.Terms.PriceString(l.PriceTicks)
			if side == SideBuy {
				d.Bids = append(d.Bids, l)
			} else {
				d.Asks = append(d.Asks, l)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return Depth{}, dbErr("depth", err)
		}
	}
	return d, nil
}

// JournalProof is what VerifyJournal proved about a book.
type JournalProof struct {
	BookID       BookID
	InstrumentID instruments.InstrumentID
	Commands     int64
	Events       int64
	Trades       int64
	// GenesisHash is the hash of the empty book; StateHash the one the
	// journal recorded after its last command.
	GenesisHash [32]byte
	StateHash   [32]byte
	// FullReplayHash is the state after replaying every command from the
	// empty book; SnapshotReplayHash the state after restoring the latest
	// snapshot and replaying the tail. Both equal StateHash.
	FullReplayHash     [32]byte
	SnapshotReplayHash [32]byte
	// SnapshotSeq is the command sequence of the snapshot used (0: none).
	SnapshotSeq int64
	// OrdersChecked is how many persisted orders were compared, field by
	// field, with the replayed engine's record of them.
	OrdersChecked int
}

// VerifyJournal proves, from the database alone, that a book's journal is a
// deterministic record of it:
//
//   - replaying every journalled command from the empty book reproduces, for
//     every command, exactly the events the database recorded for it (byte
//     for byte in their canonical encoding) and exactly the state hash it
//     recorded after it;
//   - restoring the latest snapshot and replaying the tail reproduces the same
//     final state;
//   - every order that reached the book is persisted exactly as the replayed
//     engine holds it.
//
// It reads nothing from the in-memory engines and changes nothing. On a book
// that is still trading, run it in a REPEATABLE READ (snapshot) transaction so
// that the cursor, the journal and the order rows it compares are one view.
func (s *Service) VerifyJournal(ctx context.Context, q db.Querier, instrumentID instruments.InstrumentID) (JournalProof, error) {
	c, cur, err := s.readBook(ctx, q, instrumentID, false)
	if err != nil {
		return JournalProof{}, err
	}
	proof := JournalProof{BookID: c.ID, InstrumentID: c.InstrumentID, Commands: cur.LastCommandSeq, GenesisHash: c.GenesisHash, StateHash: cur.StateHash}
	events, err := loadEvents(ctx, q, c.ID, cur.LastEventSeq)
	if err != nil {
		return JournalProof{}, err
	}
	proof.Events = int64(len(events))
	full, err := matching.NewBook(c.engineConfig())
	if err != nil {
		return JournalProof{}, err
	}
	if full.StateHash() != c.GenesisHash {
		return JournalProof{}, errs.New(errs.CodeInternal, "orders: the empty book does not hash to the genesis hash")
	}
	rows, err := q.Query(ctx, `SELECT seq, command, state_hash_after, first_event_seq, last_event_seq FROM book_commands
		WHERE book_id = $1 AND seq <= $2 ORDER BY seq`, c.ID, cur.LastCommandSeq)
	if err != nil {
		return JournalProof{}, dbErr("read journal", err)
	}
	type cmdRow struct {
		seq, first, last int64
		cmd, hash        []byte
	}
	var cmds []cmdRow
	for rows.Next() {
		var r cmdRow
		if err := rows.Scan(&r.seq, &r.cmd, &r.hash, &r.first, &r.last); err != nil {
			rows.Close()
			return JournalProof{}, dbErr("read journal", err)
		}
		cmds = append(cmds, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return JournalProof{}, dbErr("read journal", err)
	}
	if int64(len(cmds)) != cur.LastCommandSeq {
		return JournalProof{}, errs.New(errs.CodeInternal, "orders: the journal does not hold every command up to the cursor")
	}
	for _, r := range cmds {
		cmd, err := DecodeCommand(r.cmd)
		if err != nil {
			return JournalProof{}, err
		}
		res, err := full.Apply(cmd)
		if err != nil {
			return JournalProof{}, err
		}
		if res.CommandSeq != r.seq || len(res.Events) == 0 || res.Events[0].Seq != r.first || res.Events[len(res.Events)-1].Seq != r.last {
			return JournalProof{}, errs.New(errs.CodeInternal, "orders: replay produced a different event range").WithField("seq", r.seq)
		}
		for _, e := range res.Events {
			want, ok := events[e.Seq]
			if !ok {
				return JournalProof{}, errs.New(errs.CodeInternal, "orders: an event of the replay is missing from the database").WithField("event_seq", e.Seq)
			}
			got, err := EncodeEvent(e)
			if err != nil {
				return JournalProof{}, err
			}
			if !bytes.Equal(got, want) {
				return JournalProof{}, errs.New(errs.CodeInternal, "orders: replay produced a different event").WithField("event_seq", e.Seq)
			}
			if e.Type == matching.EventTrade {
				proof.Trades++
			}
		}
		h := full.StateHash()
		if !bytes.Equal(h[:], r.hash) {
			return JournalProof{}, errs.New(errs.CodeInternal, "orders: replay diverged from the recorded state hash").WithField("seq", r.seq)
		}
	}
	proof.FullReplayHash = full.StateHash()
	if proof.FullReplayHash != cur.StateHash {
		return JournalProof{}, errs.New(errs.CodeInternal, "orders: the full replay does not end at the book's state hash")
	}
	if err := q.QueryRow(ctx, `SELECT coalesce(max(command_seq), 0) FROM book_snapshots WHERE book_id = $1 AND command_seq <= $2`,
		c.ID, cur.LastCommandSeq).Scan(&proof.SnapshotSeq); err != nil {
		return JournalProof{}, dbErr("read snapshot", err)
	}
	fromSnapshot, err := rebuild(ctx, q, c, cur, true)
	if err != nil {
		return JournalProof{}, err
	}
	proof.SnapshotReplayHash = fromSnapshot.StateHash()
	if !bytes.Equal(fromSnapshot.Snapshot(), full.Snapshot()) {
		return JournalProof{}, errs.New(errs.CodeInternal, "orders: snapshot-and-tail replay differs from the full replay")
	}
	checked, err := compareOrders(ctx, q, c, full)
	if err != nil {
		return JournalProof{}, err
	}
	proof.OrdersChecked = checked
	return proof, nil
}

func loadEvents(ctx context.Context, q db.Querier, bookID BookID, upTo int64) (map[int64][]byte, error) {
	rows, err := q.Query(ctx, `SELECT seq, event FROM book_events WHERE book_id = $1 AND seq <= $2`, bookID, upTo)
	if err != nil {
		return nil, dbErr("read events", err)
	}
	defer rows.Close()
	out := map[int64][]byte{}
	for rows.Next() {
		var seq int64
		var b []byte
		if err := rows.Scan(&seq, &b); err != nil {
			return nil, dbErr("read events", err)
		}
		out[seq] = b
	}
	return out, rows.Err()
}

// compareOrders checks every persisted order that reached the engine against
// the engine's record of it.
func compareOrders(ctx context.Context, q db.Querier, c *bookConfig, b *matching.Book) (int, error) {
	rows, err := q.Query(ctx, `SELECT `+orderColumns+` FROM book_orders WHERE book_id = $1 AND last_command_seq IS NOT NULL ORDER BY id`, c.ID)
	if err != nil {
		return 0, dbErr("read orders", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return 0, dbErr("read orders", err)
		}
		st, ok := b.Order(o.ID.String())
		if !ok {
			return 0, errs.New(errs.CodeInternal, "orders: a persisted order is unknown to the replayed engine").WithField("order_id", o.ID.String())
		}
		want := withEngineState(o, st, *o.LastCommandSeq)
		if want.Status != o.Status || want.FilledLots != o.FilledLots || want.RemainingLots != o.RemainingLots ||
			want.CancelledLots != o.CancelledLots || want.PriceTicks != o.PriceTicks || want.QtyLots != o.QtyLots {
			return 0, errs.New(errs.CodeInternal, "orders: a persisted order differs from the replayed engine").WithField("order_id", o.ID.String())
		}
		n++
	}
	return n, rows.Err()
}

// BookProof is the audit handle of a book at its current state.
type BookProof struct {
	BookID         BookID
	InstrumentID   instruments.InstrumentID
	VenueLabel     string
	LastCommandSeq int64
	StateHash      [32]byte
	SnapshotSeq    int64
	SnapshotHash   []byte
}

// Proof returns the book's current journal cursor and latest snapshot.
func (s *Service) Proof(ctx context.Context, q db.Querier, instrumentID instruments.InstrumentID) (BookProof, error) {
	c, cur, err := s.readBook(ctx, q, instrumentID, false)
	if err != nil {
		return BookProof{}, err
	}
	p := BookProof{BookID: c.ID, InstrumentID: c.InstrumentID, VenueLabel: VenueLabelInternalBook, LastCommandSeq: cur.LastCommandSeq, StateHash: cur.StateHash}
	err = q.QueryRow(ctx, `SELECT command_seq, state_hash FROM book_snapshots WHERE book_id = $1 ORDER BY command_seq DESC LIMIT 1`, c.ID).
		Scan(&p.SnapshotSeq, &p.SnapshotHash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return BookProof{}, dbErr("read snapshot", err)
	}
	return p, nil
}

// OrderTrades returns the trades an order took part in, as maker or taker.
func (s *Service) OrderTrades(ctx context.Context, q db.Querier, oid OrderID) ([]Trade, error) {
	o, err := loadOrder(ctx, q, oid, false)
	if err != nil {
		return nil, err
	}
	c, _, err := s.readBookByID(ctx, q, o.BookID, false)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT `+tradeColumns+` FROM book_trades WHERE maker_order_id = $1 OR taker_order_id = $1 ORDER BY trade_seq`, oid)
	if err != nil {
		return nil, dbErr("order trades", err)
	}
	defer rows.Close()
	var out []Trade
	for rows.Next() {
		t, err := scanTrade(rows, c.Terms)
		if err != nil {
			return nil, dbErr("scan trade", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
