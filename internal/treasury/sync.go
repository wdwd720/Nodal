package treasury

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/orders"
)

// PositionEvent is one row of the position register.
type PositionEvent struct {
	ID                   PositionEventID
	HouseAccountID       econ.HouseAccountID
	Seq                  int64
	Kind                 PositionEventKind
	Asset                assets.AssetID
	Movement             Movement
	BookTradeID          *id.ID[id.Any]
	PaperFillID          *PaperFillID
	FundingID            *FundingID
	InstrumentID         *instruments.InstrumentID
	JournalTransactionID ledger.TransactionID
	OccurredAt           time.Time
}

// eventSource is what a position event records.
type eventSource struct {
	bookTrade  *id.ID[id.Any]
	paperFill  *PaperFillID
	funding    *FundingID
	instrument *instruments.InstrumentID
	journal    ledger.TransactionID
}

// positionOf returns the register's latest position in an asset.
func positionOf(ctx context.Context, q db.Querier, house econ.HouseAccountID, asset assets.AssetID) (Position, error) {
	var qty, cost string
	err := q.QueryRow(ctx, `SELECT qty_after::text, cost_after::text FROM treasury_position_events
		WHERE house_account_id = $1 AND asset_id = $2 ORDER BY seq DESC LIMIT 1`, house, asset).Scan(&qty, &cost)
	if errors.Is(err, pgx.ErrNoRows) {
		return Position{}, nil
	}
	if err != nil {
		return Position{}, dbErr("load position", err)
	}
	var p Position
	if p.Quantity, err = money.ScanQuantity(qty); err != nil {
		return Position{}, errs.Wrap(err, errs.CodeInternal, "treasury: stored position quantity")
	}
	if p.Cost, err = money.ScanQuantity(cost); err != nil {
		return Position{}, errs.Wrap(err, errs.CodeInternal, "treasury: stored position cost")
	}
	return p, nil
}

// insertEvent appends one event to the register with the next sequence
// number. The caller holds the book's lock; the database re-checks the
// sequence, the chain from the previous position and the source (TY005).
func insertEvent(ctx context.Context, tx pgx.Tx, house econ.HouseAccountID, kind PositionEventKind, asset assets.AssetID,
	m Movement, src eventSource, at time.Time) (PositionEvent, error) {
	var seq int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(seq), 0) + 1 FROM treasury_position_events WHERE house_account_id = $1`,
		house).Scan(&seq); err != nil {
		return PositionEvent{}, dbErr("next position event", err)
	}
	ev := PositionEvent{
		ID: id.New[eventKind](), HouseAccountID: house, Seq: seq, Kind: kind, Asset: asset, Movement: m,
		BookTradeID: src.bookTrade, PaperFillID: src.paperFill, FundingID: src.funding, InstrumentID: src.instrument,
		JournalTransactionID: src.journal, OccurredAt: at.UTC(),
	}
	if _, err := tx.Exec(ctx, `INSERT INTO treasury_position_events (id, house_account_id, seq, kind, direction, asset_id,
		quantity, value, fees, qty_before, qty_after, cost_before, cost_after, cost_removed, realized_pnl, book_trade_id,
		paper_fill_id, funding_id, instrument_id, journal_transaction_id, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7::numeric,$8::numeric,$9::numeric,$10::numeric,$11::numeric,$12::numeric,$13::numeric,
		        $14::numeric,$15::numeric,$16,$17,$18,$19,$20,$21)`,
		ev.ID, house, seq, kind, m.Direction, asset, m.Quantity.String(), m.Value.String(), m.Fees.String(),
		m.Before.Quantity.String(), m.After.Quantity.String(), m.Before.Cost.String(), m.After.Cost.String(),
		m.CostRemoved.String(), m.Realized.String(), src.bookTrade, src.paperFill, src.funding, src.instrument, src.journal,
		ev.OccurredAt); err != nil {
		return PositionEvent{}, dbErr("record position event", err)
	}
	return ev, nil
}

// Sync ingests every internal-book trade of the house book the register has
// not recorded yet. It takes the book's lock; each trade is recorded exactly
// once (and the database refuses a second record, UNIQUE(house, trade)).
func (s *Service) Sync(ctx context.Context, tx pgx.Tx, house econ.HouseAccountID) (int, error) {
	book, err := s.lockBook(ctx, tx, house)
	if err != nil {
		return 0, err
	}
	return s.syncLocked(ctx, tx, book)
}

type pendingFill struct {
	trade      id.ID[id.Any]
	instrument instruments.InstrumentID
	base       assets.AssetID
	quote      assets.AssetID
	qty        money.Quantity
	notional   money.Quantity
	side       orders.Side
	journal    ledger.TransactionID
	at         time.Time
}

// syncLocked is Sync for a caller that already holds the book's lock.
//
// Trades are applied in batches: every acquisition before any disposal, each
// in fill-time order. Every trade in a batch is committed, and the ledger
// never let the house sell what it did not hold, so applying acquisitions
// first can never take the register below zero, whatever order the trades
// committed in.
func (s *Service) syncLocked(ctx context.Context, tx pgx.Tx, book Book) (int, error) {
	rows, err := tx.Query(ctx, `SELECT t.id, t.instrument_id, t.base_asset_id, t.quote_asset_id, t.base_quantity::text,
		t.quote_notional::text, t.aggressor_side, t.maker_owner_type, t.maker_owner_id, t.journal_transaction_id, j.effective_at
		FROM book_trades t JOIN journal_transactions j ON j.id = t.journal_transaction_id
		WHERE ((t.maker_owner_type = 'HOUSE' AND t.maker_owner_id = $1) OR (t.taker_owner_type = 'HOUSE' AND t.taker_owner_id = $1))
		  AND NOT EXISTS (SELECT 1 FROM treasury_position_events e WHERE e.house_account_id = $1 AND e.book_trade_id = t.id)
		ORDER BY j.effective_at, t.created_at, t.id`, book.HouseAccountID)
	if err != nil {
		return 0, dbErr("pending house trades", err)
	}
	var pending []pendingFill
	for rows.Next() {
		var f pendingFill
		var qty, notional, makerType string
		var makerOwner id.ID[id.Any]
		var aggressor orders.Side
		if err := rows.Scan(&f.trade, &f.instrument, &f.base, &f.quote, &qty, &notional, &aggressor, &makerType, &makerOwner,
			&f.journal, &f.at); err != nil {
			rows.Close()
			return 0, dbErr("scan house trade", err)
		}
		if f.qty, err = money.ScanQuantity(qty); err == nil {
			f.notional, err = money.ScanQuantity(notional)
		}
		if err != nil {
			rows.Close()
			return 0, errs.Wrap(err, errs.CodeInternal, "treasury: stored trade amounts")
		}
		f.side = aggressor
		if makerType == string(orders.OwnerHouse) && makerOwner.String() == book.HouseAccountID.String() {
			f.side = opposite(aggressor)
		}
		pending = append(pending, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, dbErr("pending house trades", err)
	}
	sort.SliceStable(pending, func(i, j int) bool {
		return pending[i].side == orders.SideBuy && pending[j].side != orders.SideBuy
	})
	positions := map[assets.AssetID]Position{}
	for _, f := range pending {
		if f.quote != book.ValuationAsset {
			return 0, rejection(errs.CodeConflict, ReasonForeignTrade,
				"the house book traded on a book that is not quoted in its valuation asset; its PnL cannot be stated").
				WithField("trade_id", f.trade.String())
		}
		pos, ok := positions[f.base]
		if !ok {
			if pos, err = positionOf(ctx, tx, book.HouseAccountID, f.base); err != nil {
				return 0, err
			}
		}
		var m Movement
		if f.side == orders.SideBuy {
			m, err = pos.Increase(f.qty, f.notional, money.Quantity{})
		} else {
			m, err = pos.Decrease(f.qty, f.notional, money.Quantity{})
		}
		if err != nil {
			return 0, err
		}
		trade, inst := f.trade, f.instrument
		if _, err := insertEvent(ctx, tx, book.HouseAccountID, EventBookFill, f.base, m,
			eventSource{bookTrade: &trade, instrument: &inst, journal: f.journal}, f.at); err != nil {
			return 0, err
		}
		positions[f.base] = m.After
	}
	return len(pending), nil
}

func opposite(s orders.Side) orders.Side {
	if s == orders.SideBuy {
		return orders.SideSell
	}
	return orders.SideBuy
}

const eventColumns = `id, house_account_id, seq, kind, direction, asset_id, quantity::text, value::text, fees::text,
	qty_before::text, qty_after::text, cost_before::text, cost_after::text, cost_removed::text, realized_pnl::text,
	book_trade_id, paper_fill_id, funding_id, instrument_id, journal_transaction_id, occurred_at`

func scanEvent(row pgx.Row) (PositionEvent, error) {
	var e PositionEvent
	var q [9]string
	if err := row.Scan(&e.ID, &e.HouseAccountID, &e.Seq, &e.Kind, &e.Movement.Direction, &e.Asset, &q[0], &q[1], &q[2], &q[3],
		&q[4], &q[5], &q[6], &q[7], &q[8], &e.BookTradeID, &e.PaperFillID, &e.FundingID, &e.InstrumentID,
		&e.JournalTransactionID, &e.OccurredAt); err != nil {
		return PositionEvent{}, err
	}
	dst := []*money.Quantity{&e.Movement.Quantity, &e.Movement.Value, &e.Movement.Fees, &e.Movement.Before.Quantity,
		&e.Movement.After.Quantity, &e.Movement.Before.Cost, &e.Movement.After.Cost, &e.Movement.CostRemoved, &e.Movement.Realized}
	for i, p := range dst {
		v, err := money.ScanQuantity(q[i])
		if err != nil {
			return PositionEvent{}, err
		}
		*p = v
	}
	e.OccurredAt = e.OccurredAt.UTC()
	return e, nil
}

// Events returns the house book's position register in sequence order.
func (s *Service) Events(ctx context.Context, q db.Querier, house econ.HouseAccountID) ([]PositionEvent, error) {
	rows, err := q.Query(ctx, `SELECT `+eventColumns+` FROM treasury_position_events WHERE house_account_id = $1 ORDER BY seq`, house)
	if err != nil {
		return nil, dbErr("position events", err)
	}
	defer rows.Close()
	var out []PositionEvent
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, dbErr("scan position event", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
