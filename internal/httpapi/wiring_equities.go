package httpapi

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/equities"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/valuation"
)

// EquitiesDeps holds the equities subsystem behind /advanced/equities.
type EquitiesDeps struct {
	// Service is the equities subsystem; nil leaves every equities route
	// UNSUPPORTED.
	Service *equities.Service
	// Prices is the price store the service marks orders with; the listing
	// reads show the freshest mark from it. Nil leaves the mark out of the
	// reads (the service still prices every order itself).
	Prices valuation.PriceSource
}

// wireEquities wires the equities port when its service is present.
func wireEquities(p *Ports, d WireDeps) {
	e := d.Advanced.Equities
	if e.Service == nil {
		return
	}
	p.Equities = equitiesAdapter{svc: e.Service, prices: e.Prices, db: d.DB, assets: assets.NewRepository(),
		instruments: instruments.NewRepository(), clk: d.Clock}
}

// equitiesAdapter serves AdvancedEquitiesPort over internal/equities.
//
// The order lifecycle is the service's, one transaction per step: the intent
// is recorded and routed, the order accepted, the venue called OUTSIDE any
// transaction, and its answer applied in a transaction of its own. The
// adapter only sequences those calls; each of them is idempotent or
// exactly-once by the service's own keys, so a retried request never places
// or applies anything twice.
//
// Its reads follow the read model's rule (readmodel.go): where the service
// exposes no listing -- an account's orders, fills, settlements and
// corporate-action applications, and the corporate actions -- the adapter
// selects keys from the tables internal/equities owns and renders every row
// through the service's own getters. Every statement here is a SELECT.
type equitiesAdapter struct {
	svc         *equities.Service
	prices      valuation.PriceSource
	db          *db.DB
	assets      *assets.Repository
	instruments *instruments.Repository
	clk         clock.Clock
}

// equitiesSnapshot runs a multi-statement read against one consistent
// snapshot.
func equitiesSnapshot(ctx context.Context, d *db.DB, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return d.InTx(ctx, db.TxOptions{Isolation: pgx.RepeatableRead, ReadOnly: true}, fn)
}

// equitiesWriteTx runs one step of the lifecycle the way the service's own
// tests do: READ COMMITTED, serialized by its row and advisory locks. Every
// step is idempotent, so a deadlock or serialization retry is safe.
func equitiesWriteTx(ctx context.Context, d *db.DB, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 5}, fn)
}

// --- listings and the calendar ------------------------------------------------

func (a equitiesAdapter) Listings(ctx context.Context, limit int) (EquityListingsView, error) {
	limit = clampLimit(limit)
	var out EquityListingsView
	err := equitiesSnapshot(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		out = EquityListingsView{}
		var listed []instruments.Listed
		for _, kind := range []instruments.InstrumentType{instruments.TypeEquity, instruments.TypeETF} {
			l, lerr := a.instruments.ListWithTerms(ctx, tx, kind, limit)
			if lerr != nil {
				return lerr
			}
			listed = append(listed, l...)
		}
		if len(listed) > limit {
			listed = listed[:limit]
		}
		now := a.clk.Now().UTC()
		for _, l := range listed {
			v, verr := a.listingView(ctx, tx, l.Instrument.ID, now)
			if verr != nil {
				return verr
			}
			out.Items = append(out.Items, v)
		}
		var serr error
		out.Session, serr = a.session(ctx, tx, now)
		return serr
	})
	return out, err
}

func (a equitiesAdapter) Listing(ctx context.Context, instrumentID instruments.InstrumentID) (EquityListingView, error) {
	var out EquityListingView
	err := equitiesSnapshot(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		var lerr error
		out, lerr = a.listingView(ctx, tx, instrumentID, a.clk.Now().UTC())
		return lerr
	})
	return out, err
}

// listingView reads a listing through the service and adds the freshest mark
// of its shares, under the same bound the service prices orders with.
func (a equitiesAdapter) listingView(ctx context.Context, q pgx.Tx, instrumentID instruments.InstrumentID, now time.Time) (EquityListingView, error) {
	l, err := a.svc.LoadListing(ctx, q, instrumentID)
	if err != nil {
		return EquityListingView{}, err
	}
	v := EquityListingView{Listing: l}
	if a.prices == nil {
		return v, nil
	}
	maxAge := time.Duration(l.Terms.Terms.OracleMaxAgeMS) * time.Millisecond
	p, err := a.prices.Latest(ctx, q, l.ShareAsset.ID, l.CashAsset.ID, maxAge, now)
	switch {
	case errs.CodeOf(err) == errs.CodeStaleMarketData:
		return v, nil
	case err != nil:
		return EquityListingView{}, err
	}
	if p.Mantissa.IsPositive() {
		v.Mark = &equities.Mark{Mantissa: p.Mantissa, Scale: p.Scale, Source: p.Source, ObservedAt: p.At.UTC()}
	}
	return v, nil
}

func (a equitiesAdapter) ListEquity(ctx context.Context, r EquityListingRequest) (EquityListingView, error) {
	actor, err := booksActor(ctx)
	if err != nil {
		return EquityListingView{}, err
	}
	var out EquityListingView
	err = equitiesWriteTx(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		spec := r.Spec
		spec.Actor = actor
		if r.DefaultSettlement {
			// The platform's sandbox dollar, provisioned on first use inside
			// this transaction exactly as the faucet provisions it; never a
			// dollar of the listing's own.
			usd, uerr := econ.EnsureSandboxDollar(ctx, tx)
			if uerr != nil {
				return uerr
			}
			spec.CashAssetID = usd.ID
		}
		l, lerr := a.svc.ListEquity(ctx, tx, spec)
		if lerr != nil {
			return lerr
		}
		out = EquityListingView{Listing: l}
		return nil
	})
	return out, err
}

func (a equitiesAdapter) Calendar(ctx context.Context) (EquityCalendarView, error) {
	var out EquityCalendarView
	err := equitiesSnapshot(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		var cerr error
		out, cerr = a.calendarView(ctx, tx)
		return cerr
	})
	return out, err
}

func (a equitiesAdapter) AddHoliday(ctx context.Context, h equities.Holiday) (EquityCalendarView, error) {
	actor, err := booksActor(ctx)
	if err != nil {
		return EquityCalendarView{}, err
	}
	var out EquityCalendarView
	err = equitiesWriteTx(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		if herr := a.svc.AddHoliday(ctx, tx, equities.CalendarXNYS, h, actor); herr != nil {
			return herr
		}
		var cerr error
		out, cerr = a.calendarView(ctx, tx)
		return cerr
	})
	return out, err
}

func (a equitiesAdapter) calendarView(ctx context.Context, q pgx.Tx) (EquityCalendarView, error) {
	state, err := a.session(ctx, q, a.clk.Now().UTC())
	if err != nil {
		return EquityCalendarView{}, err
	}
	hs, err := equities.Holidays(ctx, q, equities.CalendarXNYS)
	if err != nil {
		return EquityCalendarView{}, err
	}
	return EquityCalendarView{State: state, SettlementCycleDays: equities.SettlementCycleDays, Holidays: hs}, nil
}

// session reads the calendar at now through the calendar's own rules.
func (a equitiesAdapter) session(ctx context.Context, q pgx.Tx, now time.Time) (EquitySessionView, error) {
	cal, err := a.svc.Calendar(ctx, q, equities.CalendarXNYS)
	if err != nil {
		return EquitySessionView{}, err
	}
	day := cal.TradeDate(now)
	v := EquitySessionView{
		Calendar: cal.Code(), Timezone: cal.Location().String(), At: now, Session: cal.SessionAt(now),
		TradeDate: day, TradingDay: cal.IsTradingDay(day),
	}
	if name, ok := cal.HolidayName(day); ok {
		v.HolidayName = name
	}
	if v.NextTradingDay, err = cal.NextTradingDay(day); err != nil {
		return EquitySessionView{}, err
	}
	if v.TradingDay {
		if v.SettlementDate, err = cal.SettlementDate(day); err != nil {
			return EquitySessionView{}, err
		}
	}
	return v, nil
}

// --- orders -------------------------------------------------------------------

// Submit runs the order through the service one step per transaction:
// SubmitOrderIntent, then AcceptOrder, then -- for an ACCEPTED order -- the
// venue call outside any transaction and ApplyOrderReport in its own. The
// service allows the placement here: the order is recorded before it is
// sent, and the answer applied after, exactly once. Once the venue has
// answered, the answer is applied even if the client has gone away.
func (a equitiesAdapter) Submit(ctx context.Context, r EquityOrderSubmit) (EquityOrderSubmitted, error) {
	var in econ.Intent
	if err := equitiesWriteTx(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		var serr error
		in, serr = a.svc.SubmitOrderIntent(ctx, tx, r.Request)
		return serr
	}); err != nil {
		return EquityOrderSubmitted{}, err
	}
	if in.Status == econ.StatusRejected {
		// A recorded refusal: the settlement compiler refused the route, and
		// the REJECTED intent is the record.
		return EquityOrderSubmitted{}, errs.New(errs.CodeValidationFailed, "equities: the settlement compiler refused the order").
			WithField("intent_id", in.ID.String()).WithField("rejection_code", in.RejectionCode)
	}
	var o equities.Order
	if err := equitiesWriteTx(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		var aerr error
		o, aerr = a.svc.AcceptOrder(ctx, tx, in.ID)
		return aerr
	}); err != nil {
		return EquityOrderSubmitted{}, err
	}
	if refusal := o.Refusal(); refusal != nil {
		return EquityOrderSubmitted{}, refusal
	}
	out := EquityOrderSubmitted{}
	if o.Status != equities.OrderAccepted {
		out.Placement = EquityPlacement{Placed: true, Earlier: true}
	} else {
		placed, report, perr := a.place(ctx, o)
		if perr != nil {
			out.Placement = EquityPlacement{Err: perr}
		} else {
			o = placed
			out.Placement = EquityPlacement{Placed: true, Report: &report}
		}
	}
	if refusal := o.Refusal(); refusal != nil {
		// The venue refused it; the REJECTED order and its released
		// reservation are recorded.
		return EquityOrderSubmitted{}, refusal
	}
	view, err := a.Order(ctx, o.ID)
	if err != nil {
		return EquityOrderSubmitted{}, err
	}
	out.Order = view
	return out, nil
}

// place sends an ACCEPTED order to the venue and applies its answer.
func (a equitiesAdapter) place(ctx context.Context, o equities.Order) (equities.Order, equities.OrderReport, error) {
	report, err := a.svc.PlaceAtVenue(ctx, a.db, o.ID)
	if err != nil {
		return equities.Order{}, equities.OrderReport{}, err
	}
	applied, err := a.apply(ctx, o.ID, report)
	if err != nil {
		return equities.Order{}, equities.OrderReport{}, err
	}
	return applied, report, nil
}

// apply records a venue answer in its own transaction. The venue has
// answered, so the answer is recorded whether or not the request is still
// waiting for it.
func (a equitiesAdapter) apply(ctx context.Context, oid equities.OrderID, report equities.OrderReport) (equities.Order, error) {
	var out equities.Order
	err := equitiesWriteTx(context.WithoutCancel(ctx), a.db, func(ctx context.Context, tx pgx.Tx) error {
		var aerr error
		out, aerr = a.svc.ApplyOrderReport(ctx, tx, oid, report)
		return aerr
	})
	return out, err
}

func (a equitiesAdapter) Cancel(ctx context.Context, oid equities.OrderID) (EquityOrderView, error) {
	report, err := a.svc.CancelAtVenue(ctx, a.db, oid)
	if err != nil {
		return EquityOrderView{}, err
	}
	if _, err := a.apply(ctx, oid, report); err != nil {
		return EquityOrderView{}, err
	}
	return a.Order(ctx, oid)
}

func (a equitiesAdapter) Order(ctx context.Context, oid equities.OrderID) (EquityOrderView, error) {
	var out EquityOrderView
	err := equitiesSnapshot(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		var oerr error
		out, oerr = a.orderView(ctx, tx, oid, equityAssetCache{})
		return oerr
	})
	return out, err
}

// equityAssetCache saves an asset read per repeated asset within one read.
type equityAssetCache map[assets.AssetID]assets.Asset

func (a equitiesAdapter) asset(ctx context.Context, q pgx.Tx, id assets.AssetID, seen equityAssetCache) (assets.Asset, error) {
	if v, ok := seen[id]; ok {
		return v, nil
	}
	v, err := a.assets.Get(ctx, q, id)
	if err != nil {
		if errs.HasCode(err, errs.CodeNotFound) {
			// Foreign keys make this unreachable; if it happens the record
			// exists and NOT_FOUND would be false.
			return assets.Asset{}, errs.Wrap(err, errs.CodeInternal, "an equity record names an asset the registry does not hold")
		}
		return assets.Asset{}, err
	}
	seen[id] = v
	return v, nil
}

// orderAssets names the order's share and settlement assets.
func (a equitiesAdapter) orderAssets(ctx context.Context, q pgx.Tx, o equities.Order, seen equityAssetCache) (EquityAssets, error) {
	share, err := a.asset(ctx, q, o.ShareAssetID, seen)
	if err != nil {
		return EquityAssets{}, err
	}
	settle, err := a.asset(ctx, q, o.CashAssetID, seen)
	if err != nil {
		return EquityAssets{}, err
	}
	return EquityAssets{
		ShareSymbol: share.Symbol, ShareDecimals: o.QuantityDecimals, SettlementAssetID: settle.ID,
		SettlementSymbol: settle.Symbol, SettlementDecimals: settle.Decimals,
	}, nil
}

// orderView reads an order through the service, with its fill and the fill's
// settlement.
func (a equitiesAdapter) orderView(ctx context.Context, q pgx.Tx, oid equities.OrderID, seen equityAssetCache) (EquityOrderView, error) {
	o, err := a.svc.Order(ctx, q, oid)
	if err != nil {
		return EquityOrderView{}, err
	}
	v := EquityOrderView{Order: o}
	f, ok, err := a.svc.FillForOrder(ctx, q, oid)
	if err != nil || !ok {
		return v, err
	}
	fv, err := a.fillView(ctx, q, o, f, seen)
	if err != nil {
		return EquityOrderView{}, err
	}
	v.Fill = &fv
	return v, nil
}

func (a equitiesAdapter) fillView(ctx context.Context, q pgx.Tx, o equities.Order, f equities.Fill, seen equityAssetCache) (EquityFillView, error) {
	as, err := a.orderAssets(ctx, q, o, seen)
	if err != nil {
		return EquityFillView{}, err
	}
	v := EquityFillView{Fill: f, Assets: as}
	st, ok, err := a.svc.SettlementForFill(ctx, q, f.ID)
	if err != nil {
		return EquityFillView{}, err
	}
	if ok {
		sv, serr := a.settlementView(ctx, q, st, seen)
		if serr != nil {
			return EquityFillView{}, serr
		}
		v.Settlement = &sv
	}
	return v, nil
}

func (a equitiesAdapter) settlementView(ctx context.Context, q pgx.Tx, st equities.Settlement, seen equityAssetCache) (EquitySettlementView, error) {
	asset, err := a.asset(ctx, q, st.AssetID, seen)
	if err != nil {
		return EquitySettlementView{}, err
	}
	return EquitySettlementView{Settlement: st, AssetSymbol: asset.Symbol, AssetDecimals: asset.Decimals}, nil
}

// equityOrdersPageSQL pages an account's orders, newest first
// (equity_orders_account_idx).
const equityOrdersPageSQL = `SELECT id, created_at FROM equity_orders
	WHERE account_id = $1
	  AND ($2::timestamptz IS NULL OR (created_at, id) < ($2::timestamptz, $3::uuid))
	ORDER BY created_at DESC, id DESC LIMIT $4`

func (a equitiesAdapter) Orders(ctx context.Context, account accounts.AccountID, cursor string, limit int) ([]EquityOrderView, string, error) {
	var out []EquityOrderView
	next, err := a.page(ctx, equityOrdersPageSQL, account, cursor, limit, func(ctx context.Context, tx pgx.Tx, key equityKey, seen equityAssetCache) error {
		oid, perr := equities.ParseOrderID(key.id)
		if perr != nil {
			return errs.Wrap(perr, errs.CodeInternal, "internal error")
		}
		v, verr := a.orderView(ctx, tx, oid, seen)
		if verr != nil {
			return verr
		}
		out = append(out, v)
		return nil
	}, func() { out = nil })
	return out, next, err
}

// equityFillsPageSQL pages an account's fills by execution time, newest
// first (equity_fills_account_idx).
const equityFillsPageSQL = `SELECT order_id::text, executed_at, id FROM equity_fills
	WHERE account_id = $1
	  AND ($2::timestamptz IS NULL OR (executed_at, id) < ($2::timestamptz, $3::uuid))
	ORDER BY executed_at DESC, id DESC LIMIT $4`

func (a equitiesAdapter) Fills(ctx context.Context, account accounts.AccountID, cursor string, limit int) ([]EquityFillView, string, error) {
	var out []EquityFillView
	next, err := a.page(ctx, equityFillsPageSQL, account, cursor, limit, func(ctx context.Context, tx pgx.Tx, key equityKey, seen equityAssetCache) error {
		oid, perr := equities.ParseOrderID(key.ref)
		if perr != nil {
			return errs.Wrap(perr, errs.CodeInternal, "internal error")
		}
		o, oerr := a.svc.Order(ctx, tx, oid)
		if oerr != nil {
			return oerr
		}
		f, ok, ferr := a.svc.FillForOrder(ctx, tx, oid)
		if ferr != nil {
			return ferr
		}
		if !ok {
			return errs.New(errs.CodeInternal, "a listed fill is missing from its order")
		}
		v, verr := a.fillView(ctx, tx, o, f, seen)
		if verr != nil {
			return verr
		}
		out = append(out, v)
		return nil
	}, func() { out = nil })
	return out, next, err
}

// equitySettlementsPageSQL pages an account's settlements, newest first;
// $2 keeps all of them (the empty string), the PENDING ones or the SETTLED
// ones.
const equitySettlementsPageSQL = `SELECT fill_id::text, created_at, id FROM equity_settlements
	WHERE account_id = $1
	  AND ($2::text = '' OR ($2::text = 'PENDING' AND settled_at IS NULL) OR ($2::text = 'SETTLED' AND settled_at IS NOT NULL))
	  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3::timestamptz, $4::uuid))
	ORDER BY created_at DESC, id DESC LIMIT $5`

func (a equitiesAdapter) Settlements(ctx context.Context, account accounts.AccountID, state string, cursor string, limit int) ([]EquitySettlementView, string, error) {
	var out []EquitySettlementView
	limit = clampLimit(limit)
	c, has, err := decodeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	var next string
	err = equitiesSnapshot(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		out, next = nil, ""
		at, after := cursorArgs(c, has)
		keys, nxt, kerr := equityKeys(ctx, tx, equitySettlementsPageSQL, limit, account, state, at, after, limit+1)
		if kerr != nil {
			return kerr
		}
		next = nxt
		seen := equityAssetCache{}
		for _, k := range keys {
			fid, perr := equityFillID(k.ref)
			if perr != nil {
				return perr
			}
			st, ok, serr := a.svc.SettlementForFill(ctx, tx, fid)
			if serr != nil {
				return serr
			}
			if !ok {
				return errs.New(errs.CodeInternal, "a listed settlement is missing")
			}
			v, verr := a.settlementView(ctx, tx, st, seen)
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

// equityApplicationsPageSQL pages what corporate actions did to an account,
// newest first.
const equityApplicationsPageSQL = `SELECT action_id::text, created_at, id FROM corporate_action_applications
	WHERE account_id = $1
	  AND ($2::timestamptz IS NULL OR (created_at, id) < ($2::timestamptz, $3::uuid))
	ORDER BY created_at DESC, id DESC LIMIT $4`

func (a equitiesAdapter) Applications(ctx context.Context, account accounts.AccountID, cursor string, limit int) ([]EquityApplicationView, string, error) {
	var out []EquityApplicationView
	next, err := a.page(ctx, equityApplicationsPageSQL, account, cursor, limit, func(ctx context.Context, tx pgx.Tx, key equityKey, seen equityAssetCache) error {
		aid, perr := equities.ParseCorporateActionID(key.ref)
		if perr != nil {
			return errs.Wrap(perr, errs.CodeInternal, "internal error")
		}
		apps, aerr := equities.Applications(ctx, tx, aid)
		if aerr != nil {
			return aerr
		}
		for _, app := range apps {
			if app.ID.String() != key.id {
				continue
			}
			v, verr := a.applicationView(ctx, tx, app, seen)
			if verr != nil {
				return verr
			}
			out = append(out, v)
			return nil
		}
		return errs.New(errs.CodeInternal, "a listed corporate action application is missing")
	}, func() { out = nil })
	return out, next, err
}

// applicationView adds the action's type and instrument and its listing's
// assets to one application.
func (a equitiesAdapter) applicationView(ctx context.Context, q pgx.Tx, app equities.Application, seen equityAssetCache) (EquityApplicationView, error) {
	act, err := a.svc.CorporateAction(ctx, q, app.ActionID)
	if err != nil {
		return EquityApplicationView{}, err
	}
	share, err := a.asset(ctx, q, act.ShareAssetID, seen)
	if err != nil {
		return EquityApplicationView{}, err
	}
	settle, err := a.asset(ctx, q, act.CashAssetID, seen)
	if err != nil {
		return EquityApplicationView{}, err
	}
	return EquityApplicationView{Application: app, ActionType: act.Type, InstrumentID: act.InstrumentID, Assets: EquityAssets{
		ShareSymbol: share.Symbol, ShareDecimals: share.Decimals, SettlementAssetID: settle.ID,
		SettlementSymbol: settle.Symbol, SettlementDecimals: settle.Decimals,
	}}, nil
}

// equityKey is one row of a key page: the row's own id, its keyset instant,
// and the id of the record it points at.
type equityKey struct {
	ref string
	at  time.Time
	id  string
}

// equityKeys runs a page query whose columns are (ref, at, id) -- or (id, at)
// for a page of the records themselves -- and returns at most limit keys and
// the cursor of the next page.
func equityKeys(ctx context.Context, q pgx.Tx, sql string, limit int, args ...any) ([]equityKey, string, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, "", errs.Wrap(err, errs.CodeInternal, "internal error")
	}
	defer rows.Close()
	var keys []equityKey
	two := len(rows.FieldDescriptions()) == 2
	for rows.Next() {
		var k equityKey
		var serr error
		if two {
			var id string
			serr = rows.Scan(&id, &k.at)
			k.ref, k.id = id, id
		} else {
			serr = rows.Scan(&k.ref, &k.at, &k.id)
		}
		if serr != nil {
			return nil, "", errs.Wrap(serr, errs.CodeInternal, "internal error")
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, "", errs.Wrap(err, errs.CodeInternal, "internal error")
	}
	next := ""
	if len(keys) > limit {
		keys = keys[:limit]
		last := keys[len(keys)-1]
		next = encodeCursor(keysetCursor{At: last.at, ID: last.id})
	}
	return keys, next, nil
}

// page runs an account-scoped key page in one snapshot and renders each key.
func (a equitiesAdapter) page(ctx context.Context, sql string, account accounts.AccountID, cursor string, limit int,
	render func(ctx context.Context, tx pgx.Tx, key equityKey, seen equityAssetCache) error, reset func()) (string, error) {
	limit = clampLimit(limit)
	c, has, err := decodeCursor(cursor)
	if err != nil {
		return "", err
	}
	var next string
	err = equitiesSnapshot(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		reset()
		at, after := cursorArgs(c, has)
		keys, nxt, kerr := equityKeys(ctx, tx, sql, limit, account, at, after, limit+1)
		if kerr != nil {
			return kerr
		}
		next = nxt
		seen := equityAssetCache{}
		for _, k := range keys {
			if rerr := render(ctx, tx, k, seen); rerr != nil {
				return rerr
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return next, nil
}

func equityFillID(s string) (equities.FillID, error) {
	var fid equities.FillID
	if err := fid.Scan(s); err != nil {
		return equities.FillID{}, errs.Wrap(err, errs.CodeInternal, "internal error")
	}
	return fid, nil
}

// --- the workers' steps, run on request ----------------------------------------

// equitySettlementBatches bounds one settlement run: each batch is one
// account's due settlements in one transaction.
const equitySettlementBatches = 1_000

// RunSettlements is the settlement worker's step: SettleDue, one account per
// transaction, until nothing due is left or limit settlements are settled.
func (a equitiesAdapter) RunSettlements(ctx context.Context, limit int) (EquitySettlementRun, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	run := EquitySettlementRun{At: a.clk.Now().UTC()}
	var settled []equities.Settlement
	for run.Batches < equitySettlementBatches && len(settled) < limit {
		var batch []equities.Settlement
		if err := equitiesWriteTx(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
			var serr error
			batch, serr = a.svc.SettleDue(ctx, tx, limit-len(settled))
			return serr
		}); err != nil {
			return EquitySettlementRun{}, err
		}
		if len(batch) == 0 {
			break
		}
		run.Batches++
		settled = append(settled, batch...)
	}
	err := equitiesSnapshot(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		cal, cerr := a.svc.Calendar(ctx, tx, equities.CalendarXNYS)
		if cerr != nil {
			return cerr
		}
		run.TradeDate = cal.TradeDate(run.At)
		run.Settled = make([]EquitySettlementView, 0, len(settled))
		seen := equityAssetCache{}
		for _, st := range settled {
			v, verr := a.settlementView(ctx, tx, st, seen)
			if verr != nil {
				return verr
			}
			run.Settled = append(run.Settled, v)
		}
		return nil
	})
	if err != nil {
		return EquitySettlementRun{}, err
	}
	return run, nil
}

// RunVenue is the venue worker's step for every order the venue still owes an
// answer, oldest first: an ACCEPTED order is placed -- unless its submission
// outcome is UNKNOWN, which the service never sends again (it answers
// RECONCILIATION_REQUIRED) and which is polled instead -- and an OPEN order is
// polled. Each answer is applied in its own transaction; one order's failure
// is reported on its item and never stops the others.
func (a equitiesAdapter) RunVenue(ctx context.Context, limit int) (EquityVenueRun, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	run := EquityVenueRun{At: a.clk.Now().UTC()}
	open, err := a.svc.OpenOrders(ctx, a.db, limit)
	if err != nil {
		return EquityVenueRun{}, err
	}
	for _, o := range open {
		item := EquityVenueRunItem{OrderID: o.ID, Before: o.Status}
		var (
			report equities.OrderReport
			verr   error
		)
		if o.Status == equities.OrderAccepted {
			item.Action = EquityVenuePlace
			report, verr = a.svc.PlaceAtVenue(ctx, a.db, o.ID)
			if errs.CodeOf(verr) == errs.CodeReconciliationRequired {
				item.Action = EquityVenuePoll
				report, verr = a.svc.PollVenue(ctx, a.db, o.ID)
			}
		} else {
			item.Action = EquityVenuePoll
			report, verr = a.svc.PollVenue(ctx, a.db, o.ID)
		}
		if verr != nil {
			item.Err = verr
			run.Items = append(run.Items, item)
			continue
		}
		item.Report = &report
		after, aerr := a.apply(ctx, o.ID, report)
		if aerr != nil {
			item.Err = aerr
		} else {
			item.After = after.Status
		}
		run.Items = append(run.Items, item)
	}
	return run, nil
}

// --- corporate actions -----------------------------------------------------------

// equityActionsPageSQL pages corporate actions, newest effective date first,
// optionally of one instrument.
const equityActionsPageSQL = `SELECT id::text, effective_date::timestamp AT TIME ZONE 'UTC', id FROM equity_corporate_actions
	WHERE ($1::uuid IS NULL OR instrument_id = $1::uuid)
	  AND ($2::timestamptz IS NULL OR (effective_date::timestamp AT TIME ZONE 'UTC', id) < ($2::timestamptz, $3::uuid))
	ORDER BY effective_date DESC, id DESC LIMIT $4`

func (a equitiesAdapter) CorporateActions(ctx context.Context, instrumentID *instruments.InstrumentID, cursor string, limit int) ([]equities.CorporateAction, string, error) {
	limit = clampLimit(limit)
	c, has, err := decodeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	var (
		out  []equities.CorporateAction
		next string
	)
	var scope any
	if instrumentID != nil {
		scope = *instrumentID
	}
	err = equitiesSnapshot(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		out, next = nil, ""
		at, after := cursorArgs(c, has)
		keys, nxt, kerr := equityKeys(ctx, tx, equityActionsPageSQL, limit, scope, at, after, limit+1)
		if kerr != nil {
			return kerr
		}
		next = nxt
		for _, k := range keys {
			aid, perr := equities.ParseCorporateActionID(k.ref)
			if perr != nil {
				return errs.Wrap(perr, errs.CodeInternal, "internal error")
			}
			act, aerr := a.svc.CorporateAction(ctx, tx, aid)
			if aerr != nil {
				return aerr
			}
			out = append(out, act)
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return out, next, nil
}

func (a equitiesAdapter) CorporateAction(ctx context.Context, aid equities.CorporateActionID) (equities.CorporateAction, error) {
	return a.svc.CorporateAction(ctx, a.db, aid)
}

func (a equitiesAdapter) AnnounceCorporateAction(ctx context.Context, spec equities.CorporateActionSpec) (equities.CorporateAction, error) {
	actor, err := booksActor(ctx)
	if err != nil {
		return equities.CorporateAction{}, err
	}
	spec.Actor = actor
	var out equities.CorporateAction
	err = equitiesWriteTx(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		var aerr error
		out, aerr = a.svc.AnnounceCorporateAction(ctx, tx, spec)
		return aerr
	})
	return out, err
}

func (a equitiesAdapter) CancelCorporateAction(ctx context.Context, aid equities.CorporateActionID, reason string) (equities.CorporateAction, error) {
	actor, err := booksActor(ctx)
	if err != nil {
		return equities.CorporateAction{}, err
	}
	var out equities.CorporateAction
	err = equitiesWriteTx(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		var cerr error
		out, cerr = a.svc.CancelCorporateAction(ctx, tx, aid, actor, reason)
		return cerr
	})
	return out, err
}

func (a equitiesAdapter) ApplyCorporateAction(ctx context.Context, aid equities.CorporateActionID) (EquityCorporateActionResultView, error) {
	actor, err := booksActor(ctx)
	if err != nil {
		return EquityCorporateActionResultView{}, err
	}
	var out EquityCorporateActionResultView
	err = equitiesWriteTx(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		out = EquityCorporateActionResultView{}
		res, aerr := a.svc.ApplyCorporateAction(ctx, tx, aid, actor)
		if aerr != nil {
			return aerr
		}
		out.Result = res
		seen := equityAssetCache{}
		for _, app := range res.Applications {
			v, verr := a.applicationView(ctx, tx, app, seen)
			if verr != nil {
				return verr
			}
			out.Applications = append(out.Applications, v)
		}
		return nil
	})
	return out, err
}
