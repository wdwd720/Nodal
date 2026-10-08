package httpapi

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// OrdersDeps holds the internal order book behind /advanced/books. Both are
// required together: a book that could be read but not traded (or the
// reverse) would half-answer, so either missing leaves every route
// UNSUPPORTED.
type OrdersDeps struct {
	// Service is the book: its commands, reads and operator controls.
	Service *orders.Service
	// Pipeline runs a customer submission one stage per transaction.
	Pipeline *orders.Pipeline
}

// wireOrders wires the books port when its services are present.
func wireOrders(p *Ports, d WireDeps) {
	o := d.Advanced.Orders
	if o.Service == nil || o.Pipeline == nil {
		return
	}
	p.Books = booksAdapter{svc: o.Service, pipe: o.Pipeline, db: d.DB, assets: assets.NewRepository(), clk: d.Clock}
}

// AdvancedVerificationLevels adapts the advanced capabilities' one identity
// rule (compliance.AccountVerification, DECISION_REGISTER D-055) to the
// verification-level reader the order book and equities take: an account the
// rule verifies is NODAL_IDENTITY -- the level both require of simulated
// activity -- and every other account is NONE. It reads in the caller's
// transaction and never answers "verified" on an error.
type AdvancedVerificationLevels struct {
	Rule compliance.AccountVerification
}

// Level implements orders.VerificationSource and equities.VerificationReader.
func (v AdvancedVerificationLevels) Level(ctx context.Context, q db.Querier, account accounts.AccountID) (valuedomain.VerificationLevel, error) {
	ok, err := v.Rule.IdentityVerified(ctx, q, account)
	if err != nil {
		return valuedomain.VerificationNone, err
	}
	if !ok {
		return valuedomain.VerificationNone, nil
	}
	return valuedomain.VerificationNodalIdentity, nil
}

// booksAdapter serves AdvancedBooksPort over internal/orders.
//
// Its reads follow the read model's rule (readmodel.go): where the order book
// exposes no listing -- the books, an account's orders, an account's trades,
// a book's stored fee schedules and crossing rule -- the adapter selects the
// keys from the tables internal/orders owns and renders every row through
// the service's own getters (GetBook, Order, Trades), so the figures are
// always the service's. Every statement here is a SELECT; every write goes
// through the service.
type booksAdapter struct {
	svc    *orders.Service
	pipe   *orders.Pipeline
	db     *db.DB
	assets *assets.Repository
	clk    clock.Clock
}

// booksSnapshot runs a multi-statement read against one consistent snapshot,
// which is also what VerifyJournal asks for on a book that is still trading.
func booksSnapshot(ctx context.Context, d *db.DB, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return d.InTx(ctx, db.TxOptions{Isolation: pgx.RepeatableRead, ReadOnly: true}, fn)
}

// booksWriteTx runs an operator command the way the order book's own tests
// do: READ COMMITTED, serialized by the book's row lock.
func booksWriteTx(ctx context.Context, d *db.DB, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: orders.PipelineMaxRetries}, fn)
}

// booksActor is who an operator command is recorded against: the
// authenticated principal. The service refuses an agent itself.
func booksActor(ctx context.Context) (econ.Actor, error) {
	p, ok := security.PrincipalFrom(ctx)
	if !ok || p.SubjectID == "" {
		return econ.Actor{}, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	return econ.Actor{Type: p.ActorType, ID: p.SubjectID}, nil
}

// --- reads ------------------------------------------------------------------

// booksPageSQL pages the books, oldest first, by (created_at, id).
const booksPageSQL = `SELECT instrument_id, created_at, id FROM book_states
	WHERE ($1::timestamptz IS NULL OR (created_at, id) > ($1::timestamptz, $2::uuid))
	ORDER BY created_at, id LIMIT $3`

func (a booksAdapter) Books(ctx context.Context, cursor string, limit int) ([]BookView, string, error) {
	limit = clampLimit(limit)
	c, has, err := decodeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	var (
		out  []BookView
		next string
	)
	err = booksSnapshot(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		out, next = nil, ""
		at, after := cursorArgs(c, has)
		rows, qerr := tx.Query(ctx, booksPageSQL, at, after, limit+1)
		if qerr != nil {
			return errs.Wrap(qerr, errs.CodeInternal, "internal error")
		}
		type key struct {
			instrument instruments.InstrumentID
			created    time.Time
			id         string
		}
		var keys []key
		for rows.Next() {
			var k key
			var bid orders.BookID
			if serr := rows.Scan(&k.instrument, &k.created, &bid); serr != nil {
				rows.Close()
				return errs.Wrap(serr, errs.CodeInternal, "internal error")
			}
			k.id = bid.String()
			keys = append(keys, k)
		}
		rows.Close()
		if rerr := rows.Err(); rerr != nil {
			return errs.Wrap(rerr, errs.CodeInternal, "internal error")
		}
		if len(keys) > limit {
			keys = keys[:limit]
			last := keys[len(keys)-1]
			next = encodeCursor(keysetCursor{At: last.created, ID: last.id})
		}
		seen := map[assets.AssetID]assets.Asset{}
		for _, k := range keys {
			v, verr := a.bookView(ctx, tx, k.instrument, seen)
			if verr != nil {
				return verr
			}
			out = append(out, v)
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return out, next, nil
}

func (a booksAdapter) Book(ctx context.Context, instrumentID instruments.InstrumentID) (BookView, error) {
	var out BookView
	err := booksSnapshot(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		var verr error
		out, verr = a.bookView(ctx, tx, instrumentID, nil)
		return verr
	})
	return out, err
}

func (a booksAdapter) Depth(ctx context.Context, instrumentID instruments.InstrumentID, levels int) (BookDepthView, error) {
	var out BookDepthView
	err := booksSnapshot(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		var derr error
		if out.Book, derr = a.svc.GetBook(ctx, tx, instrumentID); derr != nil {
			return derr
		}
		out.Depth, derr = a.svc.Depth(ctx, tx, instrumentID, levels)
		return derr
	})
	return out, err
}

func (a booksAdapter) VerifyJournal(ctx context.Context, instrumentID instruments.InstrumentID) (orders.JournalProof, error) {
	var out orders.JournalProof
	err := booksSnapshot(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		var verr error
		out, verr = a.svc.VerifyJournal(ctx, tx, instrumentID)
		return verr
	})
	return out, err
}

func (a booksAdapter) Order(ctx context.Context, oid orders.OrderID) (BookOrderView, error) {
	var out BookOrderView
	err := booksSnapshot(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		var oerr error
		out, oerr = a.orderView(ctx, tx, oid, map[instruments.InstrumentID]BookAssets{})
		return oerr
	})
	return out, err
}

// bookOrdersPageSQL pages a customer's orders, newest first. The owner is the
// account (book_orders_owner_idx); $2 keeps only the working ones.
const bookOrdersPageSQL = `SELECT id, created_at FROM book_orders
	WHERE owner_type = 'CUSTOMER' AND owner_id = $1::uuid
	  AND (NOT $2::boolean OR status IN ('PENDING','OPEN','PARTIALLY_FILLED'))
	  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3::timestamptz, $4::uuid))
	ORDER BY created_at DESC, id DESC LIMIT $5`

func (a booksAdapter) Orders(ctx context.Context, account accounts.AccountID, workingOnly bool, cursor string, limit int) ([]BookOrderView, string, error) {
	limit = clampLimit(limit)
	c, has, err := decodeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	var (
		out  []BookOrderView
		next string
	)
	err = booksSnapshot(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		out, next = nil, ""
		at, after := cursorArgs(c, has)
		rows, qerr := tx.Query(ctx, bookOrdersPageSQL, account, workingOnly, at, after, limit+1)
		if qerr != nil {
			return errs.Wrap(qerr, errs.CodeInternal, "internal error")
		}
		type key struct {
			id      orders.OrderID
			created time.Time
		}
		var keys []key
		for rows.Next() {
			var k key
			if serr := rows.Scan(&k.id, &k.created); serr != nil {
				rows.Close()
				return errs.Wrap(serr, errs.CodeInternal, "internal error")
			}
			keys = append(keys, k)
		}
		rows.Close()
		if rerr := rows.Err(); rerr != nil {
			return errs.Wrap(rerr, errs.CodeInternal, "internal error")
		}
		if len(keys) > limit {
			keys = keys[:limit]
			last := keys[len(keys)-1]
			next = encodeCursor(keysetCursor{At: last.created, ID: last.id.String()})
		}
		seen := map[instruments.InstrumentID]BookAssets{}
		for _, k := range keys {
			v, verr := a.orderView(ctx, tx, k.id, seen)
			if verr != nil {
				return verr
			}
			out = append(out, v)
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return out, next, nil
}

// bookTradesPageSQL pages the trades a customer took part in, as maker or as
// taker, newest first. It reaches the trades through the account's own orders
// (book_orders_owner_idx, then book_trades_maker_idx / book_trades_taker_idx).
const bookTradesPageSQL = `SELECT b.instrument_id, t.trade_seq, t.created_at, t.id
	FROM book_trades t JOIN book_states b ON b.id = t.book_id
	WHERE t.id IN (
	        SELECT m.id FROM book_trades m JOIN book_orders o ON o.id = m.maker_order_id
	         WHERE o.owner_type = 'CUSTOMER' AND o.owner_id = $1::uuid
	        UNION ALL
	        SELECT k.id FROM book_trades k JOIN book_orders o ON o.id = k.taker_order_id
	         WHERE o.owner_type = 'CUSTOMER' AND o.owner_id = $1::uuid)
	  AND ($2::timestamptz IS NULL OR (t.created_at, t.id) < ($2::timestamptz, $3::uuid))
	ORDER BY t.created_at DESC, t.id DESC LIMIT $4`

func (a booksAdapter) Trades(ctx context.Context, account accounts.AccountID, cursor string, limit int) ([]BookTradeView, string, error) {
	limit = clampLimit(limit)
	c, has, err := decodeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	var (
		out  []BookTradeView
		next string
	)
	err = booksSnapshot(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		out, next = nil, ""
		at, after := cursorArgs(c, has)
		rows, qerr := tx.Query(ctx, bookTradesPageSQL, account, at, after, limit+1)
		if qerr != nil {
			return errs.Wrap(qerr, errs.CodeInternal, "internal error")
		}
		type key struct {
			instrument instruments.InstrumentID
			seq        int64
			created    time.Time
			id         string
		}
		var keys []key
		for rows.Next() {
			var k key
			var tid string
			if serr := rows.Scan(&k.instrument, &k.seq, &k.created, &tid); serr != nil {
				rows.Close()
				return errs.Wrap(serr, errs.CodeInternal, "internal error")
			}
			k.id = tid
			keys = append(keys, k)
		}
		rows.Close()
		if rerr := rows.Err(); rerr != nil {
			return errs.Wrap(rerr, errs.CodeInternal, "internal error")
		}
		if len(keys) > limit {
			keys = keys[:limit]
			last := keys[len(keys)-1]
			next = encodeCursor(keysetCursor{At: last.created, ID: last.id})
		}
		seen := map[instruments.InstrumentID]BookAssets{}
		for _, k := range keys {
			v, verr := a.tradeView(ctx, tx, k.instrument, k.seq, seen)
			if verr != nil {
				return verr
			}
			out = append(out, v)
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return out, next, nil
}

func (a booksAdapter) TradeProof(ctx context.Context, instrumentID instruments.InstrumentID, tradeSeq int64) (BookTradeProof, error) {
	var out BookTradeProof
	err := booksSnapshot(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		var perr error
		if out.Trade, perr = a.tradeView(ctx, tx, instrumentID, tradeSeq, map[instruments.InstrumentID]BookAssets{}); perr != nil {
			return perr
		}
		out.Proof, perr = a.svc.Proof(ctx, tx, instrumentID)
		return perr
	})
	return out, err
}

// tradeView reads one trade by its book's trade sequence through the
// service's own reader.
func (a booksAdapter) tradeView(ctx context.Context, q pgx.Tx, instrumentID instruments.InstrumentID, seq int64,
	seen map[instruments.InstrumentID]BookAssets) (BookTradeView, error) {
	if seq < 1 {
		return BookTradeView{}, bookTradeNotFound(instrumentID, seq)
	}
	trades, err := a.svc.Trades(ctx, q, instrumentID, seq-1, 1)
	if err != nil {
		return BookTradeView{}, err
	}
	if len(trades) != 1 || trades[0].TradeSeq != seq {
		return BookTradeView{}, bookTradeNotFound(instrumentID, seq)
	}
	assetsView, err := a.bookAssets(ctx, q, instrumentID, seen)
	if err != nil {
		return BookTradeView{}, err
	}
	return BookTradeView{Trade: trades[0], InstrumentID: instrumentID, Assets: assetsView}, nil
}

// bookTradeNotFound is the one answer for a trade that does not exist and a
// trade the reader is not a party to.
func bookTradeNotFound(instrumentID instruments.InstrumentID, seq int64) error {
	return errs.New(errs.CodeNotFound, "no such trade").WithField("instrument_id", instrumentID.String()).
		WithField("trade_seq", seq)
}

// orderView reads one order through the service and adds its book's assets.
func (a booksAdapter) orderView(ctx context.Context, q pgx.Tx, oid orders.OrderID, seen map[instruments.InstrumentID]BookAssets) (BookOrderView, error) {
	o, err := a.svc.Order(ctx, q, oid)
	if err != nil {
		return BookOrderView{}, err
	}
	assetsView, err := a.bookAssets(ctx, q, o.InstrumentID, seen)
	if err != nil {
		return BookOrderView{}, err
	}
	return BookOrderView{Order: o, Assets: assetsView}, nil
}

// bookAssets names a book's assets. seen saves the reads per repeated book.
func (a booksAdapter) bookAssets(ctx context.Context, q pgx.Tx, instrumentID instruments.InstrumentID, seen map[instruments.InstrumentID]BookAssets) (BookAssets, error) {
	if v, ok := seen[instrumentID]; ok {
		return v, nil
	}
	b, err := a.svc.GetBook(ctx, q, instrumentID)
	if err != nil {
		return BookAssets{}, err
	}
	v, err := a.assetsOf(ctx, q, b, nil)
	if err != nil {
		return BookAssets{}, err
	}
	seen[instrumentID] = v
	return v, nil
}

func (a booksAdapter) assetsOf(ctx context.Context, q pgx.Tx, b orders.Book, seen map[assets.AssetID]assets.Asset) (BookAssets, error) {
	base, err := a.asset(ctx, q, b.Terms.BaseAsset, seen)
	if err != nil {
		return BookAssets{}, err
	}
	quote, err := a.asset(ctx, q, b.Terms.QuoteAsset, seen)
	if err != nil {
		return BookAssets{}, err
	}
	return BookAssets{
		BaseSymbol: base.Symbol, BaseDecimals: b.Terms.BaseDecimals,
		QuoteSymbol: quote.Symbol, QuoteDecimals: b.Terms.QuoteDecimals,
	}, nil
}

func (a booksAdapter) asset(ctx context.Context, q pgx.Tx, id assets.AssetID, seen map[assets.AssetID]assets.Asset) (assets.Asset, error) {
	if v, ok := seen[id]; ok {
		return v, nil
	}
	v, err := a.assets.Get(ctx, q, id)
	if err != nil {
		if errs.HasCode(err, errs.CodeNotFound) {
			// The book's foreign keys make this unreachable; if it happens the
			// book exists and NOT_FOUND would be false.
			return assets.Asset{}, errs.Wrap(err, errs.CodeInternal, "a book names an asset the registry does not hold")
		}
		return assets.Asset{}, err
	}
	if seen != nil {
		seen[id] = v
	}
	return v, nil
}

// bookSchedulesSQL reads what a book recorded about its fee schedules and the
// latest crossing rule. The schedules are proven below (orders.DecodeSchedule
// and the hashes the book carries); the rule is the latest version, exactly
// the one the service enforces.
const bookSchedulesSQL = `SELECT b.maker_schedule, b.taker_schedule,
	       r.version, r.allow_house_customer_crossing, r.reason, r.actor_type, r.actor_id, r.created_at
	FROM book_states b
	LEFT JOIN LATERAL (SELECT version, allow_house_customer_crossing, reason, actor_type, actor_id, created_at
	                     FROM book_crossing_rules WHERE book_id = b.id ORDER BY version DESC LIMIT 1) r ON true
	WHERE b.id = $1`

// bookView is a book as its read discloses it.
func (a booksAdapter) bookView(ctx context.Context, q pgx.Tx, instrumentID instruments.InstrumentID, seen map[assets.AssetID]assets.Asset) (BookView, error) {
	b, err := a.svc.GetBook(ctx, q, instrumentID)
	if err != nil {
		return BookView{}, err
	}
	v := BookView{Book: b}
	if v.Assets, err = a.assetsOf(ctx, q, b, seen); err != nil {
		return BookView{}, err
	}
	var (
		makerRaw, takerRaw []byte
		version            *int
		allow              *bool
		reason, actorType  *string
		actorID            *string
		createdAt          *time.Time
	)
	if err := q.QueryRow(ctx, bookSchedulesSQL, b.ID).Scan(&makerRaw, &takerRaw, &version, &allow, &reason, &actorType,
		&actorID, &createdAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return BookView{}, errs.New(errs.CodeInternal, "the book vanished inside one snapshot")
		}
		return BookView{}, errs.Wrap(err, errs.CodeInternal, "internal error")
	}
	if v.MakerSchedule, err = provenSchedule(makerRaw, b.MakerScheduleHash); err != nil {
		return BookView{}, err
	}
	if v.TakerSchedule, err = provenSchedule(takerRaw, b.TakerScheduleHash); err != nil {
		return BookView{}, err
	}
	v.Crossing = orders.CrossingRule{BookID: b.ID}
	if version == nil {
		v.CrossingDefault = true
		return v, nil
	}
	v.Crossing.Version = *version
	v.Crossing.AllowHouseCustomerCrossing = allow != nil && *allow
	if reason != nil {
		v.Crossing.Reason = *reason
	}
	if actorType != nil {
		v.Crossing.Actor.Type = security.ActorType(*actorType)
	}
	if actorID != nil {
		v.Crossing.Actor.ID = *actorID
	}
	if createdAt != nil {
		v.Crossing.CreatedAt = createdAt.UTC()
	}
	return v, nil
}

// provenSchedule decodes a stored schedule and proves it is the one the book
// names: the service's decoder re-encodes it to the stored bytes, and its hash
// must be the book's own.
func provenSchedule(raw []byte, wantHash string) (fees.Schedule, error) {
	s, err := orders.DecodeSchedule(raw)
	if err != nil {
		return fees.Schedule{}, errs.Wrap(err, errs.CodeInternal, "a book's stored fee schedule does not decode")
	}
	h, err := s.Hash()
	if err != nil {
		return fees.Schedule{}, errs.Wrap(err, errs.CodeInternal, "a book's stored fee schedule does not hash")
	}
	if h != wantHash {
		return fees.Schedule{}, errs.New(errs.CodeInternal, "a book's stored fee schedule is not the one its hash names")
	}
	return s, nil
}

// cursorArgs are the keyset arguments of a page query: NULLs on the first
// page, so one constant statement serves both.
func cursorArgs(c keysetCursor, has bool) (any, any) {
	if !has {
		return nil, nil
	}
	return c.At, c.ID
}

// --- operator commands ------------------------------------------------------

func (a booksAdapter) OpenBook(ctx context.Context, r BookOpen) (BookView, error) {
	actor, err := booksActor(ctx)
	if err != nil {
		return BookView{}, err
	}
	var out BookView
	err = booksWriteTx(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		out = BookView{}
		_, gerr := a.svc.GetBook(ctx, tx, r.InstrumentID)
		existed := gerr == nil
		if gerr != nil && !errs.HasCode(gerr, errs.CodeNotFound) {
			return gerr
		}
		if _, oerr := a.svc.OpenBook(ctx, tx, orders.OpenBookRequest{
			InstrumentID: r.InstrumentID, Mode: r.Mode, SpotTerms: r.SpotTerms,
			MakerSchedule: r.MakerSchedule, TakerSchedule: r.TakerSchedule,
			SnapshotInterval: r.SnapshotInterval, Actor: actor,
		}); oerr != nil {
			return oerr
		}
		v, verr := a.bookView(ctx, tx, r.InstrumentID, nil)
		if verr != nil {
			return verr
		}
		v.Existed = existed
		out = v
		return nil
	})
	return out, err
}

func (a booksAdapter) SetBookStatus(ctx context.Context, instrumentID instruments.InstrumentID, to orders.BookStatus, reason string) (BookView, error) {
	actor, err := booksActor(ctx)
	if err != nil {
		return BookView{}, err
	}
	var out BookView
	err = booksWriteTx(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		if _, serr := a.svc.SetBookStatus(ctx, tx, instrumentID, to, actor, reason); serr != nil {
			return serr
		}
		var verr error
		out, verr = a.bookView(ctx, tx, instrumentID, nil)
		return verr
	})
	return out, err
}

func (a booksAdapter) SetCrossingRule(ctx context.Context, instrumentID instruments.InstrumentID, allow bool, reason string) (orders.CrossingRule, error) {
	actor, err := booksActor(ctx)
	if err != nil {
		return orders.CrossingRule{}, err
	}
	var out orders.CrossingRule
	err = booksWriteTx(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		var serr error
		out, serr = a.svc.SetCrossingRule(ctx, tx, instrumentID, allow, actor, reason)
		return serr
	})
	return out, err
}

// --- customer commands ------------------------------------------------------

// Submit runs a customer order through orders.Pipeline: Accept, Approve,
// Reserve, Execute and Complete, each in its own READ COMMITTED transaction.
//
// It is called from runCommand's function, which runs OUTSIDE any database
// transaction: the idempotency port claims the key in one transaction, runs
// the command, and records the outcome in another (idempotencyAdapter.Run).
// So the pipeline's own transactions nest in nothing, the book's row lock is
// held only for the Execute stage, and each stage commits its refusal before
// it is answered. The service is idempotent on (account, idempotency key)
// too, so a retry after an HTTP record was lost resumes the same order
// rather than placing a second one.
func (a booksAdapter) Submit(ctx context.Context, r BookOrderSubmit) (BookCommandView, error) {
	p, err := booksActor(ctx)
	if err != nil {
		return BookCommandView{}, err
	}
	book, err := a.svc.GetBook(ctx, a.db, r.InstrumentID)
	if err != nil {
		return BookCommandView{}, err
	}
	price, err := bookPrice(r.Price, book, a.clk.Now())
	if err != nil {
		return BookCommandView{}, err
	}
	out, err := a.pipe.Submit(ctx, orders.OrderRequest{
		InstrumentID: r.InstrumentID, AccountID: r.AccountID, Mode: r.Mode, Side: r.Side, Type: r.Type,
		TimeInForce: r.TimeInForce, Price: &price, Quantity: r.Quantity, PostOnly: r.PostOnly, ReduceOnly: r.ReduceOnly,
		STP: r.STP, ClientOrderID: r.ClientOrderID, IdempotencyKey: r.IdempotencyKey, ActorType: p.Type, ActorID: p.ID,
		CorrelationID: r.CorrelationID,
	})
	if err != nil {
		return BookCommandView{}, withOrderID(err, out.Order.ID)
	}
	v, err := a.commandView(ctx, out.Order, out.CommandSeq, out.StateHash, out.Trades)
	if err != nil {
		return BookCommandView{}, err
	}
	v.IntentStatus = string(out.Intent.Status)
	return v, nil
}

func (a booksAdapter) Cancel(ctx context.Context, r BookOrderCancel) (BookCommandView, error) {
	p, err := booksActor(ctx)
	if err != nil {
		return BookCommandView{}, err
	}
	out, err := a.pipe.Cancel(ctx, orders.CancelRequest{OrderID: r.OrderID, AccountID: r.AccountID, ActorType: p.Type, ActorID: p.ID})
	if err != nil {
		return BookCommandView{}, withOrderID(err, out.Order.ID)
	}
	return a.commandView(ctx, out.Order, out.CommandSeq, out.StateHash, out.Trades)
}

func (a booksAdapter) Replace(ctx context.Context, r BookOrderReplace) (BookCommandView, error) {
	p, err := booksActor(ctx)
	if err != nil {
		return BookCommandView{}, err
	}
	o, err := a.svc.Order(ctx, a.db, r.OrderID)
	if err != nil {
		return BookCommandView{}, err
	}
	book, err := a.svc.GetBook(ctx, a.db, o.InstrumentID)
	if err != nil {
		return BookCommandView{}, err
	}
	price, err := bookPrice(r.Price, book, a.clk.Now())
	if err != nil {
		return BookCommandView{}, err
	}
	out, err := a.pipe.Replace(ctx, orders.ReplaceRequest{
		OrderID: r.OrderID, AccountID: r.AccountID, NewPrice: price, NewQuantity: r.Quantity,
		IdempotencyKey: r.IdempotencyKey, ActorType: p.Type, ActorID: p.ID,
	})
	if err != nil {
		return BookCommandView{}, withOrderID(err, out.Order.ID)
	}
	return a.commandView(ctx, out.Order, out.CommandSeq, out.StateHash, out.Trades)
}

// commandView adds the book's assets to a command's outcome.
func (a booksAdapter) commandView(ctx context.Context, o orders.OrderView, seq int64, hash [32]byte, trades []orders.Trade) (BookCommandView, error) {
	var out BookCommandView
	err := booksSnapshot(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		v, aerr := a.bookAssets(ctx, tx, o.InstrumentID, map[instruments.InstrumentID]BookAssets{})
		if aerr != nil {
			return aerr
		}
		out = BookCommandView{Order: BookOrderView{Order: o, Assets: v}, CommandSeq: seq, StateHash: hash, Trades: trades}
		return nil
	})
	return out, err
}

// bookPrice reads the client's decimal as an exact price in the book's quote
// asset. It never rounds: digits the book cannot price are the service's to
// refuse (ORDER_PRICE_TOO_PRECISE, ORDER_PRICE_OFF_TICK).
func bookPrice(s string, b orders.Book, now time.Time) (money.Price, error) {
	p, err := money.PriceFromDecimalString(s, b.Terms.QuoteAsset.String(), "order", now.UTC())
	if err != nil {
		return money.Price{}, validationError("price", "price must be an exact decimal price, such as 2500.00")
	}
	return p, nil
}

// withOrderID adds the id of the order a refusal recorded to the refusal, so
// the client can read the REJECTED order it stands for. Anything that is not
// a coded refusal, or that recorded no order, is returned as it is.
func withOrderID(err error, oid orders.OrderID) error {
	if oid.IsZero() {
		return err
	}
	e, ok := errs.As(err)
	if !ok {
		return err
	}
	if _, named := e.Fields["order_id"]; named {
		return err
	}
	out := errs.Wrap(err, e.Code, e.Detail)
	for k, v := range e.Fields {
		out = out.WithField(k, v)
	}
	if e.RetryAfter != nil {
		out = out.WithRetryAfter(*e.RetryAfter)
	}
	return out.WithField("order_id", oid.String())
}
