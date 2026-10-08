package httpapi

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/derivatives"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// DerivativesDeps holds what /advanced/derivatives needs. A nil Service (or
// a zero ValuationAssetID) leaves every derivatives route UNSUPPORTED.
type DerivativesDeps struct {
	// Service is the derivatives subsystem. It decides every rule.
	Service *derivatives.Service
	// ValuationAssetID must be the USDAssetID the service was built with: the
	// asset an account's book is valued in. It is only named in answers,
	// never used to compute anything.
	ValuationAssetID assets.AssetID
	// Verification and Jurisdiction are the account facts the settlement
	// compiler routes an intent on, from the deployment's own resolvers --
	// never from the request. Nil means NONE and UNKNOWN.
	Verification VerificationResolver
	Jurisdiction JurisdictionResolver
}

// wireDerivatives wires the derivatives port when its service is present.
func wireDerivatives(p *Ports, d WireDeps) {
	dd := d.Advanced.Derivatives
	if dd.Service == nil || dd.ValuationAssetID.IsZero() || d.DB == nil {
		return
	}
	repo := d.Instruments
	if repo == nil {
		repo = instruments.NewRepository()
	}
	clk := d.Clock
	if clk == nil {
		clk = clock.System()
	}
	p.Derivatives = derivativesAdapter{svc: dd.Service, deps: dd, db: d.DB, clk: clk, instruments: repo, assets: assets.NewRepository()}
}

// derivativesAdapter carries the service's answers and reads its tables. It
// is the API's own projection where the package exports no listing (the
// position, flow, accrual and mark registers): strictly SELECTs, deciding
// nothing financial -- every figure is a stored one or one the service
// computed (AccountRisk, Analytics). Writes go only through the service.
type derivativesAdapter struct {
	svc         *derivatives.Service
	deps        DerivativesDeps
	db          *db.DB
	clk         clock.Clock
	instruments *instruments.Repository
	assets      *assets.Repository
}

var _ DerivativesPort = derivativesAdapter{}

// read runs a multi-statement read against one consistent snapshot, so a
// page, the book's valuation and the marks behind it are of one moment.
func (a derivativesAdapter) read(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return a.db.InTx(ctx, db.TxOptions{Isolation: pgx.RepeatableRead, ReadOnly: true}, fn)
}

// write runs a command the way the package's own tests do: READ COMMITTED,
// serialized by the service's clearing-house lock and row locks; exactly
// once by its UNIQUE claim rows and the effect register.
func (a derivativesAdapter) write(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, fn)
}

// --- reference data ---------------------------------------------------------

func (a derivativesAdapter) Policy(context.Context) (derivatives.Policy, error) {
	return a.svc.Policy(), nil
}

// derivativeKinds are the instrument kinds this service trades, in listing order.
var derivativeKinds = []instruments.InstrumentType{instruments.TypeFuture, instruments.TypePerpetual, instruments.TypeOption}

func (a derivativesAdapter) Contracts(ctx context.Context, kind instruments.InstrumentType, limit int) ([]DerivativeContractView, error) {
	limit = clampLimit(limit)
	kinds := derivativeKinds
	if kind != "" {
		if !kind.IsDerivative() {
			return nil, validationError("kind", "kind must be FUTURE, PERPETUAL or OPTION")
		}
		kinds = []instruments.InstrumentType{kind}
	}
	var out []DerivativeContractView
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out = nil
		var listed []instruments.Listed
		for _, k := range kinds {
			l, err := a.instruments.ListWithTerms(ctx, tx, k, limit)
			if err != nil {
				return err
			}
			listed = append(listed, l...)
		}
		if len(kinds) > 1 {
			// One order across the kinds: canonical name, then id, as each
			// kind's own listing is ordered.
			sort.SliceStable(listed, func(i, j int) bool {
				if listed[i].Instrument.CanonicalName != listed[j].Instrument.CanonicalName {
					return listed[i].Instrument.CanonicalName < listed[j].Instrument.CanonicalName
				}
				return listed[i].Instrument.ID.String() < listed[j].Instrument.ID.String()
			})
		}
		if len(listed) > limit {
			listed = listed[:limit]
		}
		lk := a.lookup(tx)
		for _, l := range listed {
			v, err := a.contractView(ctx, tx, lk, l.Instrument, l.Current)
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		return nil
	})
	return out, err
}

func (a derivativesAdapter) Contract(ctx context.Context, iid instruments.InstrumentID) (DerivativeContractView, error) {
	var out DerivativeContractView
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		ins, ver, err := a.contract(ctx, tx, iid)
		if err != nil {
			return err
		}
		out, err = a.contractView(ctx, tx, a.lookup(tx), ins, ver)
		return err
	})
	return out, err
}

func (a derivativesAdapter) contractView(ctx context.Context, q db.Querier, lk *derivativeLookup, ins instruments.Instrument, ver instruments.Version) (DerivativeContractView, error) {
	asset, err := lk.asset(ctx, ver.Terms.SettlementAssetID)
	if err != nil {
		return DerivativeContractView{}, err
	}
	v := DerivativeContractView{Instrument: ins, Current: ver, Settlement: asset}
	m, ok, err := derivatives.LatestMark(ctx, q, ins.ID)
	if err != nil {
		return DerivativeContractView{}, err
	}
	if ok {
		v.LatestMark = &m
	}
	if v.Settled, err = derivativeSettlementOf(ctx, q, ins.ID); err != nil {
		return DerivativeContractView{}, err
	}
	return v, nil
}

// contract reads a derivative instrument and its terms in force. Any other
// instrument is, to this API, not a contract at all.
func (a derivativesAdapter) contract(ctx context.Context, q db.Querier, iid instruments.InstrumentID) (instruments.Instrument, instruments.Version, error) {
	ins, err := a.instruments.Get(ctx, q, iid)
	if errs.CodeOf(err) == errs.CodeNotFound || (err == nil && !ins.Type.IsDerivative()) {
		return instruments.Instrument{}, instruments.Version{}, errs.New(errs.CodeNotFound, "no derivative contract has this id").
			WithField("instrument_id", iid.String())
	}
	if err != nil {
		return instruments.Instrument{}, instruments.Version{}, err
	}
	ver, err := a.instruments.CurrentTerms(ctx, q, iid)
	if err != nil {
		return instruments.Instrument{}, instruments.Version{}, err
	}
	return ins, ver, nil
}

const derivativeMarkColumns = `id, instrument_id, mark_mantissa, mark_scale, index_mantissa::text, index_scale,
	coalesce(index_source, ''), source, observed_at, recorded_at`

func scanDerivativeMark(row pgx.Row) (derivatives.Mark, error) {
	var m derivatives.Mark
	var idx *string
	var idxScale *int32
	if err := row.Scan(&m.ID, &m.InstrumentID, &m.MarkMantissa, &m.MarkScale, &idx, &idxScale, &m.IndexSource,
		&m.Source, &m.ObservedAt, &m.RecordedAt); err != nil {
		return derivatives.Mark{}, err
	}
	if idx != nil && idxScale != nil {
		q, err := money.ScanQuantity(*idx)
		if err != nil {
			return derivatives.Mark{}, err
		}
		m.HasIndex, m.IndexMantissa, m.IndexScale = true, q, *idxScale
	}
	m.ObservedAt, m.RecordedAt = m.ObservedAt.UTC(), m.RecordedAt.UTC()
	return m, nil
}

func (a derivativesAdapter) Marks(ctx context.Context, iid instruments.InstrumentID, cursor string, limit int) ([]derivatives.Mark, string, error) {
	limit = clampLimit(limit)
	c, hasCursor, err := decodeDerivativeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	var out []derivatives.Mark
	var next string
	err = a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out, next = nil, ""
		if _, _, err := a.contract(ctx, tx, iid); err != nil {
			return err
		}
		var args sqlArgs
		sql := `SELECT ` + derivativeMarkColumns + ` FROM derivative_marks WHERE instrument_id = ` + args.add(iid)
		if hasCursor {
			sql += ` AND (observed_at, id) < (` + args.add(c.At) + `, ` + args.add(c.ID) + `)`
		}
		sql += fmt.Sprintf(` ORDER BY observed_at DESC, id DESC LIMIT %d`, limit+1)
		rows, err := tx.Query(ctx, sql, args.list...)
		if err != nil {
			return derivativeReadErr("marks", err)
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanDerivativeMark(rows)
			if err != nil {
				return derivativeReadErr("scan mark", err)
			}
			out = append(out, m)
		}
		if err := rows.Err(); err != nil {
			return derivativeReadErr("marks", err)
		}
		if len(out) > limit {
			out = out[:limit]
			last := out[limit-1]
			next = encodeCursor(keysetCursor{At: last.ObservedAt, ID: last.ID.String()})
		}
		return nil
	})
	return out, next, err
}

func (a derivativesAdapter) Analytics(ctx context.Context, r derivatives.AnalyticsRequest) (DerivativeAnalyticsView, error) {
	var out DerivativeAnalyticsView
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		res, err := a.svc.Analytics(ctx, tx, r)
		if err != nil {
			return err
		}
		// The same snapshot: exactly the mark the model was priced at.
		m, ok, err := derivatives.LatestMark(ctx, tx, r.InstrumentID)
		if err != nil {
			return err
		}
		if !ok {
			return errs.New(errs.CodeInternal, "the analytics were computed at a mark that cannot be read back")
		}
		out = DerivativeAnalyticsView{Analytics: res, Mark: m, Inputs: r}
		return nil
	})
	return out, err
}

// --- positions ----------------------------------------------------------------

const derivativePositionColumns = `id, account_id, instrument_id, settlement_asset_id, status, quantity, cost_value, settled_value,
	realized_pnl, funding_pnl, margin_posted, shortfall, version, opened_at, closed_at`

func scanDerivativePosition(row pgx.Row) (derivatives.Position, error) {
	var p derivatives.Position
	if err := row.Scan(&p.ID, &p.AccountID, &p.InstrumentID, &p.SettlementAssetID, &p.Status, &p.Quantity, &p.CostValue,
		&p.SettledValue, &p.RealizedPnL, &p.FundingPnL, &p.MarginPosted, &p.Shortfall, &p.Version, &p.OpenedAt, &p.ClosedAt); err != nil {
		return derivatives.Position{}, err
	}
	p.OpenedAt = p.OpenedAt.UTC()
	if p.ClosedAt != nil {
		t := p.ClosedAt.UTC()
		p.ClosedAt = &t
	}
	return p, nil
}

func (a derivativesAdapter) Positions(ctx context.Context, account accounts.AccountID, status derivatives.PositionStatus, cursor string, limit int) (DerivativePositionPageView, error) {
	limit = clampLimit(limit)
	c, hasCursor, err := decodeDerivativeCursor(cursor)
	if err != nil {
		return DerivativePositionPageView{}, err
	}
	var out DerivativePositionPageView
	err = a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out = DerivativePositionPageView{}
		var args sqlArgs
		sql := `SELECT ` + derivativePositionColumns + ` FROM derivative_positions WHERE account_id = ` + args.add(account)
		if status != "" {
			sql += ` AND status = ` + args.add(string(status))
		}
		if hasCursor {
			sql += ` AND (opened_at, id) < (` + args.add(c.At) + `, ` + args.add(c.ID) + `)`
		}
		sql += fmt.Sprintf(` ORDER BY opened_at DESC, id DESC LIMIT %d`, limit+1)
		rows, err := tx.Query(ctx, sql, args.list...)
		if err != nil {
			return derivativeReadErr("positions", err)
		}
		var page []derivatives.Position
		for rows.Next() {
			p, err := scanDerivativePosition(rows)
			if err != nil {
				rows.Close()
				return derivativeReadErr("scan position", err)
			}
			page = append(page, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return derivativeReadErr("positions", err)
		}
		if len(page) > limit {
			page = page[:limit]
			last := page[limit-1]
			out.Next = encodeCursor(keysetCursor{At: last.OpenedAt, ID: last.ID.String()})
		}

		lk := a.lookup(tx)
		book, err := a.book(ctx, tx, account)
		if err != nil {
			return err
		}
		out.Book, out.Quote, out.BookRefusal = book.risk, book.quote, book.refusal
		for _, p := range page {
			v, err := lk.position(ctx, p)
			if err != nil {
				return err
			}
			if err := book.attach(&v); err != nil {
				return err
			}
			out.Items = append(out.Items, v)
		}
		return nil
	})
	return out, err
}

// derivativeBook is the service's valuation of an account's open book, in
// the asset it is valued in, or the service's refusal to value it: exactly
// one of risk and refusal is set.
type derivativeBook struct {
	risk    *derivatives.RiskContribution
	quote   DerivativeAssetView
	refusal *errs.Error
}

// book values an account's open book. The service fails closed -- a stale or
// missing mark refuses the whole book rather than understate it -- and that
// refusal is an answer to show, not a failure of the read. Only an internal
// fault fails the read.
func (a derivativesAdapter) book(ctx context.Context, q db.Querier, account accounts.AccountID) (derivativeBook, error) {
	rc, err := a.svc.AccountRisk(ctx, q, account)
	if err != nil {
		if e, ok := errs.As(err); ok && e.Code != errs.CodeInternal {
			return derivativeBook{refusal: e}, nil
		}
		return derivativeBook{}, err
	}
	// The book is named in the asset it was valued in; a deployment that has
	// not provisioned that asset has no book to state.
	quote, err := a.assets.Get(ctx, q, a.deps.ValuationAssetID)
	if errs.CodeOf(err) == errs.CodeNotFound {
		return derivativeBook{refusal: errs.New(errs.CodeUnsupported,
			"the asset derivative books are valued in is not provisioned in this deployment").
			WithField("asset_id", a.deps.ValuationAssetID.String())}, nil
	}
	if err != nil {
		return derivativeBook{}, err
	}
	return derivativeBook{risk: &rc, quote: DerivativeAssetView{ID: quote.ID, Symbol: quote.Symbol, Decimals: quote.Decimals}}, nil
}

// attach gives an open position the service's valuation of it, or the
// service's refusal to value the book. Never neither.
func (b derivativeBook) attach(v *DerivativePositionView) error {
	v.Computed = true
	if v.Position.Status != derivatives.PositionOpen {
		return nil
	}
	if b.refusal != nil {
		v.Refusal = b.refusal
		return nil
	}
	if b.risk != nil {
		for i := range b.risk.Positions {
			if r := b.risk.Positions[i]; r.PositionID == v.Position.ID.String() {
				v.Valuation = &r
				return nil
			}
		}
	}
	return errs.New(errs.CodeInternal, "the service's valuation of the book is missing an open position").
		WithField("position_id", v.Position.ID.String())
}

func (a derivativesAdapter) Position(ctx context.Context, pid derivatives.PositionID) (DerivativePositionView, error) {
	var out DerivativePositionView
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		pos, err := derivatives.GetPosition(ctx, tx, pid)
		if err != nil {
			return err
		}
		lk := a.lookup(tx)
		v, err := lk.position(ctx, pos)
		if err != nil {
			return err
		}
		var book derivativeBook
		if pos.Status == derivatives.PositionOpen {
			if book, err = a.book(ctx, tx, pos.AccountID); err != nil {
				return err
			}
		}
		if err := book.attach(&v); err != nil {
			return err
		}
		out = v
		return nil
	})
	return out, err
}

func (a derivativesAdapter) PositionAccount(ctx context.Context, pid derivatives.PositionID) (accounts.AccountID, error) {
	pos, err := derivatives.GetPosition(ctx, a.db, pid)
	if err != nil {
		return accounts.AccountID{}, err
	}
	return pos.AccountID, nil
}

// --- the flow register and funding accruals -----------------------------------

func (a derivativesAdapter) Flows(ctx context.Context, account accounts.AccountID, filter DerivativeFlowFilter, cursor string, limit int) ([]DerivativeFlowView, string, error) {
	limit = clampLimit(limit)
	c, hasCursor, err := decodeDerivativeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	var out []DerivativeFlowView
	var next string
	err = a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out, next = nil, ""
		var args sqlArgs
		sql := `SELECT id, position_id, account_id, instrument_id, settlement_asset_id, kind, amount, shortfall, journal_transaction_id,
			reference_type, reference_id, occurred_at FROM derivative_cash_flows WHERE account_id = ` + args.add(account)
		if filter.PositionID != nil {
			sql += ` AND position_id = ` + args.add(*filter.PositionID)
		}
		if filter.Kind != "" {
			sql += ` AND kind = ` + args.add(string(filter.Kind))
		}
		if hasCursor {
			sql += ` AND (occurred_at, id) < (` + args.add(c.At) + `, ` + args.add(c.ID) + `)`
		}
		sql += fmt.Sprintf(` ORDER BY occurred_at DESC, id DESC LIMIT %d`, limit+1)
		rows, err := tx.Query(ctx, sql, args.list...)
		if err != nil {
			return derivativeReadErr("flows", err)
		}
		var flows []derivatives.CashFlow
		for rows.Next() {
			var f derivatives.CashFlow
			if err := rows.Scan(&f.ID, &f.PositionID, &f.AccountID, &f.InstrumentID, &f.SettlementAssetID, &f.Kind, &f.Amount,
				&f.Shortfall, &f.JournalTransactionID, &f.ReferenceType, &f.ReferenceID, &f.OccurredAt); err != nil {
				rows.Close()
				return derivativeReadErr("scan flow", err)
			}
			f.OccurredAt = f.OccurredAt.UTC()
			flows = append(flows, f)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return derivativeReadErr("flows", err)
		}
		if len(flows) > limit {
			flows = flows[:limit]
			last := flows[limit-1]
			next = encodeCursor(keysetCursor{At: last.OccurredAt, ID: last.ID.String()})
		}
		lk := a.lookup(tx)
		for _, f := range flows {
			t, err := lk.terms(ctx, f.InstrumentID)
			if err != nil {
				return err
			}
			asset, err := lk.asset(ctx, f.SettlementAssetID)
			if err != nil {
				return err
			}
			out = append(out, DerivativeFlowView{Flow: f, Symbol: t.version.Terms.Symbol, Settlement: asset})
		}
		return nil
	})
	return out, next, err
}

func (a derivativesAdapter) FundingAccruals(ctx context.Context, account accounts.AccountID, position *derivatives.PositionID, cursor string, limit int) ([]DerivativeAccrualView, string, error) {
	limit = clampLimit(limit)
	c, hasCursor, err := decodeDerivativeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	var out []DerivativeAccrualView
	var next string
	err = a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out, next = nil, ""
		var args sqlArgs
		sql := `SELECT fa.id, fa.funding_rate_id, fa.position_id, p.instrument_id, p.settlement_asset_id, fa.interval_start,
			fr.interval_end, fa.quantity, fa.direction, fa.amount, fr.rate_e8, fr.premium_index_e8, fr.clamped,
			fr.per_contract_amount, fr.mark_mantissa, dm.mark_scale, fa.cash_flow_id
			FROM funding_accruals fa
			JOIN funding_rates fr ON fr.id = fa.funding_rate_id
			JOIN derivative_marks dm ON dm.id = fr.mark_id
			JOIN derivative_positions p ON p.id = fa.position_id
			WHERE p.account_id = ` + args.add(account)
		if position != nil {
			sql += ` AND fa.position_id = ` + args.add(*position)
		}
		if hasCursor {
			sql += ` AND (fa.interval_start, fa.id) < (` + args.add(c.At) + `, ` + args.add(c.ID) + `)`
		}
		sql += fmt.Sprintf(` ORDER BY fa.interval_start DESC, fa.id DESC LIMIT %d`, limit+1)
		rows, err := tx.Query(ctx, sql, args.list...)
		if err != nil {
			return derivativeReadErr("funding accruals", err)
		}
		var page []DerivativeAccrualView
		for rows.Next() {
			var v DerivativeAccrualView
			if err := rows.Scan(&v.ID, &v.FundingRateID, &v.PositionID, &v.InstrumentID, &v.Settlement.ID, &v.IntervalStart,
				&v.IntervalEnd, &v.Quantity, &v.Direction, &v.Amount, &v.RateE8, &v.PremiumIndexE8, &v.Clamped,
				&v.PerContract, &v.MarkMantissa, &v.PriceScale, &v.FlowID); err != nil {
				rows.Close()
				return derivativeReadErr("scan funding accrual", err)
			}
			v.IntervalStart, v.IntervalEnd = v.IntervalStart.UTC(), v.IntervalEnd.UTC()
			page = append(page, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return derivativeReadErr("funding accruals", err)
		}
		if len(page) > limit {
			page = page[:limit]
			last := page[limit-1]
			next = encodeCursor(keysetCursor{At: last.IntervalStart, ID: last.ID.String()})
		}
		lk := a.lookup(tx)
		for _, v := range page {
			t, err := lk.terms(ctx, v.InstrumentID)
			if err != nil {
				return err
			}
			if v.Settlement, err = lk.asset(ctx, v.Settlement.ID); err != nil {
				return err
			}
			v.Symbol = t.version.Terms.Symbol
			out = append(out, v)
		}
		return nil
	})
	return out, next, err
}

// --- the customer's commands --------------------------------------------------

func (a derivativesAdapter) Open(ctx context.Context, c DerivativeOpenCommand) (DerivativeTradeView, error) {
	req, err := a.intentRequest(ctx, c.AccountID, c.InstrumentID, c.Mode, c.IdempotencyKey, c.CorrelationID)
	if err != nil {
		return DerivativeTradeView{}, err
	}
	return a.trade(ctx, func(ctx context.Context, tx pgx.Tx) (derivatives.Outcome, error) {
		return a.svc.OpenPosition(ctx, tx, derivatives.OpenRequest{IntentRequest: req, Side: c.Side, Quantity: c.Quantity})
	}, nil)
}

func (a derivativesAdapter) Close(ctx context.Context, c DerivativeReduceCommand) (DerivativeTradeView, error) {
	return a.reduce(ctx, c, func(ctx context.Context, tx pgx.Tx, req derivatives.IntentRequest) (derivatives.Outcome, error) {
		return a.svc.ClosePosition(ctx, tx, derivatives.CloseRequest{IntentRequest: req, Quantity: c.Quantity})
	})
}

func (a derivativesAdapter) Exercise(ctx context.Context, c DerivativeReduceCommand) (DerivativeTradeView, error) {
	return a.reduce(ctx, c, func(ctx context.Context, tx pgx.Tx, req derivatives.IntentRequest) (derivatives.Outcome, error) {
		return a.svc.ExerciseOption(ctx, tx, derivatives.ExerciseRequest{IntentRequest: req, Quantity: c.Quantity})
	})
}

// reduce runs a close or an exercise of the position the request names. The
// service acts on the account's open position in the position's contract;
// if that is not the named position (it was closed, and another one is open
// now) the operation is rolled back and refused, so a request about one
// position can never change another.
func (a derivativesAdapter) reduce(ctx context.Context, c DerivativeReduceCommand,
	run func(ctx context.Context, tx pgx.Tx, req derivatives.IntentRequest) (derivatives.Outcome, error)) (DerivativeTradeView, error) {
	facts, err := a.routeFacts(ctx, c.AccountID)
	if err != nil {
		return DerivativeTradeView{}, err
	}
	p, err := derivativesCaller(ctx)
	if err != nil {
		return DerivativeTradeView{}, err
	}
	return a.trade(ctx, func(ctx context.Context, tx pgx.Tx) (derivatives.Outcome, error) {
		pos, err := derivatives.GetPosition(ctx, tx, c.PositionID)
		if err != nil {
			return derivatives.Outcome{}, err
		}
		// A position's identity never changes (DV002); this only proves the
		// command was scoped against this very position.
		if pos.AccountID != c.AccountID {
			return derivatives.Outcome{}, derivativePositionNotFound(c.PositionID)
		}
		return run(ctx, tx, facts.request(p, pos.AccountID, pos.InstrumentID, c.Mode, c.IdempotencyKey, c.CorrelationID))
	}, func(o derivatives.Outcome) error {
		if o.Position != nil && o.Position.ID != c.PositionID {
			return errs.New(errs.CodeConflict,
				"the named position is not the account's open position in its contract; nothing was done").
				WithField("position_id", c.PositionID.String())
		}
		return nil
	})
}

// trade runs one intent-driven operation in a write transaction. A refusal
// the service records is committed -- the REJECTED intent and, for a risk
// refusal, the persisted decision are the record of it -- and then returned
// with its stable code. check vets a successful outcome before commit; its
// error rolls the operation back.
func (a derivativesAdapter) trade(ctx context.Context, run func(ctx context.Context, tx pgx.Tx) (derivatives.Outcome, error),
	check func(derivatives.Outcome) error) (DerivativeTradeView, error) {
	var out DerivativeTradeView
	var refusal error
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out, refusal = DerivativeTradeView{}, nil // nothing survives from an earlier attempt
		res, err := run(ctx, tx)
		if _, ok := derivatives.AsRefusal(err); ok {
			refusal = err
			return nil
		}
		if err != nil {
			return err
		}
		if check != nil {
			if err := check(res); err != nil {
				return err
			}
		}
		out = DerivativeTradeView{Outcome: res}
		if res.Position != nil {
			v, err := a.lookup(tx).position(ctx, *res.Position)
			if err != nil {
				return err
			}
			out.Position = &v
		}
		return nil
	})
	if err != nil {
		return DerivativeTradeView{}, err
	}
	if refusal != nil {
		return DerivativeTradeView{}, refusal
	}
	return out, nil
}

// derivativeRouteFacts are the account facts the settlement compiler keys on.
type derivativeRouteFacts struct {
	verification valuedomain.VerificationLevel
	jurisdiction string
}

func (a derivativesAdapter) routeFacts(ctx context.Context, account accounts.AccountID) (derivativeRouteFacts, error) {
	f := derivativeRouteFacts{verification: valuedomain.VerificationNone}
	if a.deps.Verification != nil {
		lvl, err := a.deps.Verification.Level(ctx, account)
		if err != nil {
			return derivativeRouteFacts{}, err
		}
		f.verification = lvl
	}
	if a.deps.Jurisdiction != nil {
		j, err := a.deps.Jurisdiction.Jurisdiction(ctx, account)
		if err != nil {
			return derivativeRouteFacts{}, err
		}
		f.jurisdiction = j
	}
	return f, nil
}

// request is the intent request of a human caller: HTTP never carries an
// agent (authorize refuses every agent principal), so no agent fields are set.
func (f derivativeRouteFacts) request(p security.Principal, account accounts.AccountID, iid instruments.InstrumentID,
	mode gates.Mode, key, correlation string) derivatives.IntentRequest {
	return derivatives.IntentRequest{
		AccountID: account, InstrumentID: iid, Mode: mode, IdempotencyKey: key, ActorType: p.ActorType, ActorID: p.SubjectID,
		Jurisdiction: f.jurisdiction, Verification: f.verification, CorrelationID: correlation,
	}
}

func (a derivativesAdapter) intentRequest(ctx context.Context, account accounts.AccountID, iid instruments.InstrumentID,
	mode gates.Mode, key, correlation string) (derivatives.IntentRequest, error) {
	p, err := derivativesCaller(ctx)
	if err != nil {
		return derivatives.IntentRequest{}, err
	}
	facts, err := a.routeFacts(ctx, account)
	if err != nil {
		return derivatives.IntentRequest{}, err
	}
	return facts.request(p, account, iid, mode, key, correlation), nil
}

func derivativesCaller(ctx context.Context) (security.Principal, error) {
	p, ok := security.PrincipalFrom(ctx)
	if !ok || p.SubjectID == "" {
		return security.Principal{}, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	return p, nil
}

// --- the operator's commands --------------------------------------------------

func (a derivativesAdapter) RecordMark(ctx context.Context, c DerivativeMarkCommand) (derivatives.Mark, error) {
	var out derivatives.Mark
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, ver, err := a.contract(ctx, tx, c.InstrumentID)
		if err != nil {
			return err
		}
		observed := c.ObservedAt
		if observed.IsZero() {
			observed = a.clk.Now().UTC()
		}
		// A mark is quoted in the contract's settlement asset (the convention
		// of instruments.Terms); the index is recorded under the mark's source.
		quote := ver.Terms.SettlementAssetID.String()
		req := derivatives.RecordMarkRequest{
			InstrumentID: c.InstrumentID, Source: c.Source, ObservedAt: observed,
			Mark: money.Price{Mantissa: c.Mark.Mantissa, Scale: c.Mark.Scale, QuoteAsset: quote, Source: c.Source, At: observed},
		}
		if c.Index != nil {
			req.Index = &money.Price{Mantissa: c.Index.Mantissa, Scale: c.Index.Scale, QuoteAsset: quote, Source: c.Source, At: observed}
		}
		out, err = a.svc.RecordMark(ctx, tx, req)
		return err
	})
	return out, err
}

// The runs are called directly inside closures, not passed as method values,
// so test/reachability sees their call sites.
func (a derivativesAdapter) RunVariationMargin(ctx context.Context, c DerivativeRunCommand) (DerivativeRunView, error) {
	return a.run(ctx, c, func(ctx context.Context, tx pgx.Tx, r derivatives.RunRequest) (derivatives.RunResult, error) {
		return a.svc.RunVariationMargin(ctx, tx, r)
	})
}

func (a derivativesAdapter) RunFunding(ctx context.Context, c DerivativeRunCommand) (DerivativeRunView, error) {
	return a.run(ctx, c, func(ctx context.Context, tx pgx.Tx, r derivatives.RunRequest) (derivatives.RunResult, error) {
		return a.svc.RunFunding(ctx, tx, r)
	})
}

func (a derivativesAdapter) run(ctx context.Context, c DerivativeRunCommand,
	fn func(ctx context.Context, tx pgx.Tx, r derivatives.RunRequest) (derivatives.RunResult, error)) (DerivativeRunView, error) {
	var out DerivativeRunView
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, ver, err := a.contract(ctx, tx, c.InstrumentID)
		if err != nil {
			return err
		}
		res, err := fn(ctx, tx, derivatives.RunRequest{InstrumentID: c.InstrumentID, IntervalStart: c.IntervalStart, CorrelationID: c.CorrelationID})
		if err != nil {
			return err
		}
		asset, err := a.lookup(tx).asset(ctx, ver.Terms.SettlementAssetID)
		if err != nil {
			return err
		}
		out = DerivativeRunView{InstrumentID: c.InstrumentID, Result: res, Settlement: asset}
		return nil
	})
	return out, err
}

func (a derivativesAdapter) SettleExpiry(ctx context.Context, c DerivativeSettleCommand) (DerivativeSettlementView, error) {
	var out DerivativeSettlementView
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, ver, err := a.contract(ctx, tx, c.InstrumentID)
		if err != nil {
			return err
		}
		req := derivatives.SettleRequest{InstrumentID: c.InstrumentID, PriceSource: c.PriceSource, CorrelationID: c.CorrelationID}
		if c.Price != nil {
			// Expressed at the contract's price scale exactly, or not at all.
			p, err := derivatives.Rescale(c.Price.Mantissa, c.Price.Scale, ver.Terms.PriceScale, money.RoundExact)
			if err != nil {
				return validationError("price", "price is not exact at the contract's price scale").
					WithField("price_scale", ver.Terms.PriceScale)
			}
			req.Price = &p
		}
		res, err := a.svc.SettleExpiry(ctx, tx, req)
		if err != nil {
			return err
		}
		st, err := derivativeSettlementOf(ctx, tx, c.InstrumentID)
		if err != nil {
			return err
		}
		if st == nil {
			return errs.New(errs.CodeInternal, "the settlement cannot be read back")
		}
		asset, err := a.lookup(tx).asset(ctx, ver.Terms.SettlementAssetID)
		if err != nil {
			return err
		}
		out = DerivativeSettlementView{InstrumentID: c.InstrumentID, Result: res, PriceScale: st.PriceScale, Settlement: asset}
		return nil
	})
	return out, err
}

func (a derivativesAdapter) FundClearingHouse(ctx context.Context, c DerivativeClearingFundCommand) (DerivativeClearingFundView, error) {
	p, err := derivativesCaller(ctx)
	if err != nil {
		return DerivativeClearingFundView{}, err
	}
	// The service's funding key is global; the operator's own key is scoped
	// to the operator, so two operators' keys can never replay each other.
	key := "http:" + p.SubjectID + ":" + c.IdempotencyKey
	var out DerivativeClearingFundView
	err = a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		asset, err := a.requestedAsset(ctx, tx, c.AssetID)
		if err != nil {
			return err
		}
		journals, err := a.svc.FundClearingHouse(ctx, tx, asset.ID, c.Quantity, key)
		if err != nil {
			return err
		}
		out = DerivativeClearingFundView{Asset: asset, Quantity: c.Quantity, Journals: journals}
		return nil
	})
	return out, err
}

func (a derivativesAdapter) ReconcileClearing(ctx context.Context, id assets.AssetID) (DerivativeClearingReportView, error) {
	var out DerivativeClearingReportView
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		asset, err := a.requestedAsset(ctx, tx, id)
		if err != nil {
			return err
		}
		rep, err := a.svc.ReconcileClearing(ctx, tx, asset.ID)
		if err != nil {
			return err
		}
		out = DerivativeClearingReportView{Report: rep, Asset: asset}
		return nil
	})
	return out, err
}

// requestedAsset reads the asset an operator named, or the sandbox dollar
// when none was named.
func (a derivativesAdapter) requestedAsset(ctx context.Context, q db.Querier, id assets.AssetID) (DerivativeAssetView, error) {
	if id.IsZero() {
		usd, err := econ.SandboxDollar(ctx, q)
		if errs.HasCode(err, errs.CodeNotFound) {
			return DerivativeAssetView{}, errs.New(errs.CodeUnsupported,
				"the sandbox dollar is not provisioned in this deployment; name the asset")
		}
		if err != nil {
			return DerivativeAssetView{}, err
		}
		return DerivativeAssetView{ID: usd.ID, Symbol: usd.Symbol, Decimals: usd.Decimals}, nil
	}
	got, err := a.assets.Get(ctx, q, id)
	if err != nil {
		return DerivativeAssetView{}, err
	}
	return DerivativeAssetView{ID: got.ID, Symbol: got.Symbol, Decimals: got.Decimals}, nil
}

// --- reading helpers ------------------------------------------------------------

// derivativeLookup reads each instrument's terms and each asset once per
// answer, to make records readable: a symbol, a price scale, the decimals of
// the asset an amount is counted in.
type derivativeLookup struct {
	a          derivativesAdapter
	q          db.Querier
	termCache  map[instruments.InstrumentID]derivativeTerms
	assetCache map[assets.AssetID]DerivativeAssetView
}

type derivativeTerms struct {
	instrument instruments.Instrument
	version    instruments.Version
}

func (a derivativesAdapter) lookup(q db.Querier) *derivativeLookup {
	return &derivativeLookup{a: a, q: q, termCache: map[instruments.InstrumentID]derivativeTerms{}, assetCache: map[assets.AssetID]DerivativeAssetView{}}
}

// recordFault turns a NOT_FOUND about something a stored record names into
// the internal fault it is: the foreign keys guarantee it exists.
func recordFault(err error, what string) error {
	if errs.CodeOf(err) == errs.CodeNotFound {
		return errs.Wrap(err, errs.CodeInternal, "derivatives: a record names "+what+" that cannot be read")
	}
	return err
}

func (l *derivativeLookup) terms(ctx context.Context, iid instruments.InstrumentID) (derivativeTerms, error) {
	if t, ok := l.termCache[iid]; ok {
		return t, nil
	}
	ins, err := l.a.instruments.Get(ctx, l.q, iid)
	if err != nil {
		return derivativeTerms{}, recordFault(err, "an instrument")
	}
	ver, err := l.a.instruments.CurrentTerms(ctx, l.q, iid)
	if err != nil {
		return derivativeTerms{}, recordFault(err, "an instrument's terms")
	}
	t := derivativeTerms{instrument: ins, version: ver}
	l.termCache[iid] = t
	return t, nil
}

func (l *derivativeLookup) asset(ctx context.Context, id assets.AssetID) (DerivativeAssetView, error) {
	if v, ok := l.assetCache[id]; ok {
		return v, nil
	}
	got, err := l.a.assets.Get(ctx, l.q, id)
	if err != nil {
		return DerivativeAssetView{}, recordFault(err, "an asset")
	}
	v := DerivativeAssetView{ID: got.ID, Symbol: got.Symbol, Decimals: got.Decimals}
	l.assetCache[id] = v
	return v, nil
}

func (l *derivativeLookup) position(ctx context.Context, p derivatives.Position) (DerivativePositionView, error) {
	t, err := l.terms(ctx, p.InstrumentID)
	if err != nil {
		return DerivativePositionView{}, err
	}
	asset, err := l.asset(ctx, p.SettlementAssetID)
	if err != nil {
		return DerivativePositionView{}, err
	}
	return DerivativePositionView{
		Position: p, Symbol: t.version.Terms.Symbol, Kind: t.instrument.Type, PriceScale: t.version.Terms.PriceScale, Settlement: asset,
	}, nil
}

// derivativeSettlementOf is a contract's final settlement, nil before it.
func derivativeSettlementOf(ctx context.Context, q db.Querier, iid instruments.InstrumentID) (*DerivativeSettlementRecord, error) {
	var r DerivativeSettlementRecord
	err := q.QueryRow(ctx, `SELECT id, kind, settlement_mantissa, price_scale, price_source, positions, settled_at
		FROM derivative_settlements WHERE instrument_id = $1`, iid).
		Scan(&r.ID, &r.Kind, &r.PriceMantissa, &r.PriceScale, &r.PriceSource, &r.Positions, &r.SettledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, derivativeReadErr("settlement", err)
	}
	r.SettledAt = r.SettledAt.UTC()
	return &r, nil
}

// sqlArgs numbers the placeholders of a statement built clause by clause.
type sqlArgs struct{ list []any }

func (s *sqlArgs) add(v any) string {
	s.list = append(s.list, v)
	return "$" + strconv.Itoa(len(s.list))
}

// decodeDerivativeCursor reads a keyset cursor and refuses one whose position
// is not a real one, before it reaches SQL.
func decodeDerivativeCursor(raw string) (keysetCursor, bool, error) {
	c, ok, err := decodeCursor(raw)
	if err != nil || !ok {
		return c, ok, err
	}
	if c.At.IsZero() || strings.TrimSpace(c.ID) == "" {
		return keysetCursor{}, false, validationError("cursor", "cursor is not a valid pagination cursor")
	}
	if _, err := uuid.Parse(c.ID); err != nil {
		return keysetCursor{}, false, validationError("cursor", "cursor is not a valid pagination cursor")
	}
	return c, true, nil
}

// derivativeReadErr keeps a retryable failure retryable and hides every other
// database message behind INTERNAL.
func derivativeReadErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if db.IsRetryable(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if _, ok := errs.As(err); ok {
		return err
	}
	return errs.Wrap(err, errs.CodeInternal, "derivatives read model: "+op)
}
