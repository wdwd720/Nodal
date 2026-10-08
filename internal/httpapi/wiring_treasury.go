package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/treasury"
)

// TreasuryService and FeeLedgerService are the services TreasuryDeps carries,
// aliased here so the shared wiring file needs no import of its own.
type (
	TreasuryService  = treasury.Service
	FeeLedgerService = feeledger.Service
)

// TreasuryDeps holds the house book and the fee ledger. Each that is nil
// leaves only its own routes UNSUPPORTED.
type TreasuryDeps struct {
	Service *TreasuryService
	Fees    *FeeLedgerService
}

// wireTreasury wires the treasury and fee-ledger ports. Both read asset
// descriptions from the asset registry, so neither is wired without it.
func wireTreasury(p *Ports, d WireDeps) {
	clk := d.Clock
	if clk == nil {
		clk = clock.System()
	}
	if t := d.Advanced.Treasury.Service; t != nil && d.Assets != nil {
		p.Treasury = treasuryAdapter{svc: t, assets: d.Assets, db: d.DB, clk: clk}
	}
	if f := d.Advanced.Treasury.Fees; f != nil && d.Assets != nil {
		p.Fees = feesAdapter{svc: f, assets: d.Assets, db: d.DB, clk: clk}
	}
}

// treasuryAdapter serves TreasuryPort over the treasury service.
//
// Every command runs exactly one treasury operation in its own READ
// COMMITTED transaction. A refusal the treasury recorded (a REJECTED action
// returned with a nil error) commits, so the record stands, and is then
// returned as the error. The report and the reconciliation take the book's
// lock and sync its register, so they run in a writing transaction too;
// every other read runs in one read-only REPEATABLE READ snapshot.
type treasuryAdapter struct {
	svc    *treasury.Service
	assets *assets.Repository
	db     *db.DB
	clk    clock.Clock
}

var _ TreasuryPort = treasuryAdapter{}

func (a treasuryAdapter) read(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return a.db.InTx(ctx, db.TxOptions{Isolation: pgx.RepeatableRead, ReadOnly: true}, fn)
}

func (a treasuryAdapter) write(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, fn)
}

// treasuryDescribe reads the symbol and decimals of every asset named, once.
func treasuryDescribe(ctx context.Context, q db.Querier, repo *assets.Repository, into TreasuryAssets, ids ...assets.AssetID) error {
	for _, aid := range ids {
		if aid.IsZero() {
			continue
		}
		if _, ok := into[aid]; ok {
			continue
		}
		asset, err := repo.Get(ctx, q, aid)
		if err != nil {
			return err
		}
		into[aid] = TreasuryAsset{ID: asset.ID, Symbol: asset.Symbol, Decimals: asset.Decimals}
	}
	return nil
}

// treasuryPrice states an exact decimal price in the book's valuation asset,
// observed now, from the operator who stated it.
func treasuryPrice(s string, valuation assets.AssetID, now time.Time) (*money.Price, error) {
	p, err := money.PriceFromDecimalString(s, valuation.String(), "treasury-operator", now)
	if err != nil {
		return nil, validationError("price", "a price is a plain decimal string in the book's valuation asset")
	}
	return &p, nil
}

// --- house books -------------------------------------------------------------

func (a treasuryAdapter) bookView(ctx context.Context, q db.Querier, b treasury.Book, detail bool) (TreasuryBookView, error) {
	known := TreasuryAssets{}
	if err := treasuryDescribe(ctx, q, a.assets, known, b.ValuationAsset); err != nil {
		return TreasuryBookView{}, err
	}
	v := TreasuryBookView{Book: b, ValuationAsset: known[b.ValuationAsset]}
	if !detail {
		return v, nil
	}
	sp, err := a.svc.SigningPolicy(ctx, q, b.HouseAccountID)
	if err != nil {
		return TreasuryBookView{}, err
	}
	v.Signing = &sp
	pv, err := a.svc.Policy(ctx, q, b.HouseAccountID)
	switch {
	case err == nil:
		v.Risk = &pv
	case treasury.ReasonOf(err) == treasury.ReasonPolicyMissing:
		// No risk policy yet: shown as absent, never as an empty policy.
	default:
		return TreasuryBookView{}, err
	}
	return v, nil
}

const treasuryBooksSQL = `SELECT house_account_id, registered_at FROM treasury_books
	WHERE ($1::timestamptz IS NULL OR (registered_at, house_account_id) < ($1::timestamptz, $2::uuid))
	ORDER BY registered_at DESC, house_account_id DESC LIMIT $3`

func (a treasuryAdapter) Books(ctx context.Context, cursor string, limit int) (TreasuryBookPage, error) {
	at, after, err := marginCursor(cursor)
	if err != nil {
		return TreasuryBookPage{}, err
	}
	limit = clampLimit(limit)
	var page TreasuryBookPage
	err = a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, treasuryBooksSQL, at, after, limit+1)
		if err != nil {
			return err
		}
		type key struct {
			id econ.HouseAccountID
			at time.Time
		}
		var keys []key
		for rows.Next() {
			var k key
			if err := rows.Scan(&k.id, &k.at); err != nil {
				rows.Close()
				return err
			}
			keys = append(keys, k)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		page = TreasuryBookPage{}
		if len(keys) > limit {
			keys = keys[:limit]
			last := keys[limit-1]
			page.NextCursor = encodeCursor(keysetCursor{At: last.at.UTC(), ID: last.id.String()})
		}
		for _, k := range keys {
			b, err := a.svc.GetBook(ctx, tx, k.id)
			if err != nil {
				return err
			}
			v, err := a.bookView(ctx, tx, b, false)
			if err != nil {
				return err
			}
			page.Items = append(page.Items, v)
		}
		return nil
	})
	return page, err
}

func (a treasuryAdapter) Book(ctx context.Context, house econ.HouseAccountID) (TreasuryBookView, error) {
	var out TreasuryBookView
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		b, err := a.svc.GetBook(ctx, tx, house)
		if err != nil {
			return err
		}
		out, err = a.bookView(ctx, tx, b, true)
		return err
	})
	return out, err
}

func (a treasuryAdapter) Register(ctx context.Context, r TreasuryRegister) (TreasuryBookView, error) {
	var out TreasuryBookView
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		valuation := r.ValuationAsset
		if valuation == nil {
			// The deployment's one sandbox dollar, read, never created here: a
			// deployment without it has no simulated value at all.
			usd, err := econ.SandboxDollar(ctx, tx)
			if errs.HasCode(err, errs.CodeNotFound) {
				return errs.New(errs.CodeUnsupported, "the sandbox dollar is not provisioned in this deployment, so no house book can be valued")
			}
			if err != nil {
				return err
			}
			valuation = &usd.ID
		}
		b, err := a.svc.Register(ctx, tx, treasury.RegisterRequest{
			Name: r.Name, Strategy: r.Strategy, ValuationAsset: *valuation, Signing: r.Signing, IdempotencyKey: r.IdempotencyKey,
			Reason: r.Reason, Actor: r.Actor,
		})
		if err != nil {
			return err
		}
		out, err = a.bookView(ctx, tx, b, true)
		return err
	})
	return out, err
}

func (a treasuryAdapter) SetSigningPolicy(ctx context.Context, r TreasurySigningChange) (treasury.SigningPolicyVersion, error) {
	var out treasury.SigningPolicyVersion
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		v, err := a.svc.SetSigningPolicy(ctx, tx, r.House, r.Policy, r.IdempotencyKey, r.Reason, r.Actor)
		out = v
		return err
	})
	return out, err
}

func (a treasuryAdapter) SetRiskPolicy(ctx context.Context, r TreasuryRiskChange) (treasury.PolicyVersion, error) {
	var out treasury.PolicyVersion
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		b, err := a.svc.GetBook(ctx, tx, r.House)
		if err != nil {
			return err
		}
		p := r.Policy
		p.ValuationAsset = b.ValuationAsset
		v, err := a.svc.SetPolicy(ctx, tx, r.House, p, r.IdempotencyKey, r.Reason, r.Actor)
		out = v
		return err
	})
	return out, err
}

func (a treasuryAdapter) Fund(ctx context.Context, r treasury.FundRequest) (TreasuryFundingView, error) {
	var out TreasuryFundingView
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		f, err := a.svc.Fund(ctx, tx, r)
		if err != nil {
			return err
		}
		known := TreasuryAssets{}
		if err := treasuryDescribe(ctx, tx, a.assets, known, f.Asset); err != nil {
			return err
		}
		out = TreasuryFundingView{Funding: f, Asset: known[f.Asset]}
		return nil
	})
	return out, err
}

// --- house orders -----------------------------------------------------------------

func (a treasuryAdapter) PlaceOrder(ctx context.Context, r TreasuryOrderCommand) (treasury.OrderOutcome, error) {
	var out treasury.OrderOutcome
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		b, err := a.svc.GetBook(ctx, tx, r.House)
		if err != nil {
			return err
		}
		price, err := treasuryPrice(r.Price, b.ValuationAsset, a.clk.Now())
		if err != nil {
			return err
		}
		o, err := a.svc.PlaceOrder(ctx, tx, treasury.PlaceOrderRequest{
			HouseAccountID: r.House, InstrumentID: r.Instrument, Activity: r.Activity, Side: r.Side, Type: r.Type,
			TimeInForce: r.TimeInForce, Price: price, Quantity: r.Quantity, PostOnly: r.PostOnly, ReduceOnly: r.ReduceOnly,
			ClientOrderID: r.ClientOrderID, IdempotencyKey: r.IdempotencyKey, CorrelationID: r.CorrelationID, Actor: r.Actor,
		})
		out = o
		return err
	})
	if err != nil {
		return treasury.OrderOutcome{}, err
	}
	return out, out.Rejection
}

func (a treasuryAdapter) CancelOrder(ctx context.Context, r treasury.CancelRequest) (treasury.OrderOutcome, error) {
	var out treasury.OrderOutcome
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		o, err := a.svc.CancelOrder(ctx, tx, r)
		out = o
		return err
	})
	if err != nil {
		return treasury.OrderOutcome{}, err
	}
	return out, out.Rejection
}

// Quote places both legs. One leg can be refused while the other rests: each
// leg's action says which, so the quote itself is not an error.
func (a treasuryAdapter) Quote(ctx context.Context, r TreasuryQuoteCommand) (treasury.QuoteOutcome, error) {
	var out treasury.QuoteOutcome
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		b, err := a.svc.GetBook(ctx, tx, r.House)
		if err != nil {
			return err
		}
		now := a.clk.Now()
		bid, err := treasuryPrice(r.BidPrice, b.ValuationAsset, now)
		if err != nil {
			return err
		}
		ask, err := treasuryPrice(r.AskPrice, b.ValuationAsset, now)
		if err != nil {
			return err
		}
		o, err := a.svc.QuoteTwoSided(ctx, tx, treasury.QuoteRequest{
			HouseAccountID: r.House, InstrumentID: r.Instrument, Activity: r.Activity, BidPrice: bid, BidQuantity: r.BidQuantity,
			AskPrice: ask, AskQuantity: r.AskQuantity, IdempotencyKey: r.IdempotencyKey, CorrelationID: r.CorrelationID, Actor: r.Actor,
		})
		out = o
		return err
	})
	return out, err
}

// --- the paper venue ---------------------------------------------------------------

func (a treasuryAdapter) paper(ctx context.Context, run func(ctx context.Context, tx pgx.Tx) (treasury.PaperOutcome, bool, error)) (TreasuryPaperView, error) {
	var out TreasuryPaperView
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		o, noop, err := run(ctx, tx)
		if err != nil {
			return err
		}
		known := TreasuryAssets{}
		if o.Fill != nil {
			if err := treasuryDescribe(ctx, tx, a.assets, known, o.Fill.BaseAsset, o.Fill.QuoteAsset); err != nil {
				return err
			}
		}
		out = TreasuryPaperView{Outcome: o, NoOp: noop, Assets: known}
		return nil
	})
	if err != nil {
		return TreasuryPaperView{}, err
	}
	return out, out.Outcome.Rejection
}

func (a treasuryAdapter) Hedge(ctx context.Context, r treasury.PaperRequest) (TreasuryPaperView, error) {
	return a.paper(ctx, func(ctx context.Context, tx pgx.Tx) (treasury.PaperOutcome, bool, error) {
		o, err := a.svc.Hedge(ctx, tx, r)
		return o, false, err
	})
}

func (a treasuryAdapter) Convert(ctx context.Context, r treasury.ConvertRequest) (TreasuryPaperView, error) {
	return a.paper(ctx, func(ctx context.Context, tx pgx.Tx) (treasury.PaperOutcome, bool, error) {
		o, err := a.svc.Convert(ctx, tx, r)
		return o, false, err
	})
}

func (a treasuryAdapter) Rebalance(ctx context.Context, r treasury.RebalanceRequest) (TreasuryPaperView, error) {
	return a.paper(ctx, func(ctx context.Context, tx pgx.Tx) (treasury.PaperOutcome, bool, error) {
		return a.svc.Rebalance(ctx, tx, r)
	})
}

func (a treasuryAdapter) ExecutePaper(ctx context.Context, r treasury.PaperRequest) (TreasuryPaperView, error) {
	return a.paper(ctx, func(ctx context.Context, tx pgx.Tx) (treasury.PaperOutcome, bool, error) {
		o, err := a.svc.ExecutePaper(ctx, tx, r)
		return o, false, err
	})
}

// --- crossing rules, sync, marking ---------------------------------------------------

func (a treasuryAdapter) SetCrossingRule(ctx context.Context, r treasury.CrossingRuleRequest) (treasury.CrossingDisclosure, error) {
	var out treasury.CrossingDisclosure
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d, err := a.svc.SetCrossingRule(ctx, tx, r)
		out = d
		return err
	})
	return out, err
}

func (a treasuryAdapter) Sync(ctx context.Context, house econ.HouseAccountID) (int, error) {
	var n int
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		n, err = a.svc.Sync(ctx, tx, house)
		return err
	})
	return n, err
}

func (a treasuryAdapter) MarkToMarket(ctx context.Context, house econ.HouseAccountID, key string, actor econ.Actor) (TreasurySnapshotView, error) {
	var out TreasurySnapshotView
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		snap, err := a.svc.MarkToMarket(ctx, tx, house, key, actor)
		if err != nil {
			return err
		}
		b, err := a.svc.GetBook(ctx, tx, house)
		if err != nil {
			return err
		}
		rep, err := a.reportView(ctx, tx, b, snap.Report)
		if err != nil {
			return err
		}
		rep.RiskPolicyVersion = snap.Action.RiskPolicyVersion
		out = TreasurySnapshotView{Snapshot: snap, Report: rep}
		return nil
	})
	return out, err
}

// --- the report and the limits in use -------------------------------------------------

// reportView describes every asset a report names.
func (a treasuryAdapter) reportView(ctx context.Context, q db.Querier, b treasury.Book, rep treasury.Report) (TreasuryReportView, error) {
	known := TreasuryAssets{}
	ids := []assets.AssetID{b.ValuationAsset}
	for _, line := range rep.Assets {
		aid, err := assets.ParseAssetID(line.Asset)
		if err != nil {
			return TreasuryReportView{}, errs.New(errs.CodeInternal, "the treasury reported an asset id that is not a UUID")
		}
		ids = append(ids, aid)
	}
	if err := treasuryDescribe(ctx, q, a.assets, known, ids...); err != nil {
		return TreasuryReportView{}, err
	}
	return TreasuryReportView{Report: rep, Valuation: known[b.ValuationAsset], Assets: known}, nil
}

func (a treasuryAdapter) Report(ctx context.Context, house econ.HouseAccountID) (TreasuryReportView, error) {
	var out TreasuryReportView
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rep, err := a.svc.Report(ctx, tx, house)
		if err != nil {
			return err
		}
		b, err := a.svc.GetBook(ctx, tx, house)
		if err != nil {
			return err
		}
		v, err := a.reportView(ctx, tx, b, rep)
		if err != nil {
			return err
		}
		pv, err := a.svc.Policy(ctx, tx, house)
		switch {
		case treasury.ReasonOf(err) == treasury.ReasonPolicyMissing:
			out = v
			return nil
		case err != nil:
			return err
		}
		v.RiskPolicyVersion = pv.Version
		if v.Limits, err = a.limitUses(ctx, tx, b, pv, rep); err != nil {
			return err
		}
		out = v
		return nil
	})
	return out, err
}

// treasuryInstrumentSQL is what an instrument's exposure limit needs from its
// book: the base asset, the decimals the notional is computed in, and the
// notional of the house book's working buys on it (at their limit prices),
// exactly as the treasury's own exposure snapshot sums it.
const treasuryInstrumentSQL = `SELECT bs.base_asset_id, bs.base_decimals, bs.quote_decimals,
	(SELECT coalesce(sum(o.price_ticks * o.remaining_lots * bs.notional_per_tick_lot), 0) FROM book_orders o
	  WHERE o.house_account_id = $1 AND o.side = 'BUY' AND o.status IN ('PENDING','OPEN','PARTIALLY_FILLED')
	    AND o.instrument_id = bs.instrument_id)::text
	FROM book_states bs WHERE bs.instrument_id = $2`

// limitUses is every limit of the policy in force with what is used of it:
// treasury.EvaluateLimits of a risk-increasing request that adds nothing, over
// the holdings, working orders, marks and loss state read in this transaction.
func (a treasuryAdapter) limitUses(ctx context.Context, tx pgx.Tx, b treasury.Book, pv treasury.PolicyVersion, rep treasury.Report) ([]TreasuryLimitUse, error) {
	loss := treasury.LossState{RealizedToday: rep.RealizedToday, OpenLosses: rep.OpenLosses}
	for _, m := range rep.MissingMarks {
		aid, err := assets.ParseAssetID(m)
		if err != nil {
			return nil, errs.New(errs.CodeInternal, "the treasury reported an asset id that is not a UUID")
		}
		loss.MissingMarks = append(loss.MissingMarks, aid)
	}
	balances, err := a.svc.Balances(ctx, tx, b.HouseAccountID)
	if err != nil {
		return nil, err
	}
	held := map[assets.AssetID]treasury.AssetBalance{}
	for _, bal := range balances {
		held[bal.Asset] = bal
	}
	marks := map[string]*treasury.MarkRecord{}
	for _, line := range rep.Assets {
		marks[line.Asset] = line.Mark
	}
	policy := pv.Policy

	daily := treasury.EvaluateLimits(policy, treasury.LimitRequest{RiskIncreasing: true, Loss: loss})
	cap0, used0 := daily.DailyLossLimit, daily.LossMeasure
	dailyCodes := []string{}
	for _, c := range daily.Codes {
		if c == treasury.ReasonDailyLossLimit || c == treasury.ReasonMarkUnavailable {
			dailyCodes = append(dailyCodes, c)
		}
	}
	out := []TreasuryLimitUse{{Kind: TreasuryLimitKindDailyLoss, Cap: &cap0, Used: &used0, Codes: dailyCodes}}

	for _, l := range policy.MaxInventory {
		asset, bal := l.Asset, held[l.Asset]
		ev := treasury.EvaluateLimits(policy, treasury.LimitRequest{
			RiskIncreasing: true, Asset: asset, Loss: loss,
			Exposure: treasury.Exposure{Held: bal.Inventory, WorkingBuys: bal.WorkingBuys},
		})
		capQ, used := l.MaxQuantity, ev.InventoryAfter
		out = append(out, TreasuryLimitUse{Kind: TreasuryLimitKindInventory, Asset: &asset, Cap: &capQ, Used: &used, Codes: ev.Codes})
	}

	for _, l := range policy.MaxInstrumentExposure {
		inst, capQ := l.Instrument, l.MaxExposure
		use := TreasuryLimitUse{Kind: TreasuryLimitKindInstrumentExposure, Instrument: &inst, Cap: &capQ, Codes: []string{}}
		var base assets.AssetID
		var baseDecimals, quoteDecimals int16
		var working string
		err := tx.QueryRow(ctx, treasuryInstrumentSQL, b.HouseAccountID, inst).Scan(&base, &baseDecimals, &quoteDecimals, &working)
		if errors.Is(err, pgx.ErrNoRows) {
			// No book trades the instrument: nothing is exposed through it, and
			// nothing can be bought through it either.
			out = append(out, use)
			continue
		}
		if err != nil {
			return nil, err
		}
		instBuys, err := money.ScanQuantity(working)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "a stored order notional is not an integer")
		}
		bal := held[base]
		exposure := treasury.Exposure{Held: bal.Inventory, WorkingBuys: bal.WorkingBuys, InstrumentWorkingBuys: instBuys}
		switch mark := marks[base.String()]; {
		case bal.Inventory.IsZero():
			zero := money.Quantity{}
			exposure.HeldValue = &zero
		case mark != nil:
			price := money.Price{Mantissa: mark.Mantissa, Scale: mark.Scale, QuoteAsset: b.ValuationAsset.String(), Source: mark.Source, At: mark.At}
			v, err := money.Notional(bal.Inventory, uint8(baseDecimals), price, uint8(quoteDecimals), treasury.ExposureRounding) // #nosec G115 -- book decimals are CHECKed to 0..18
			if err != nil {
				return nil, errs.Wrap(err, errs.CodeInternal, "the held value of an instrument's base asset")
			}
			exposure.HeldValue = &v
		}
		ev := treasury.EvaluateLimits(policy, treasury.LimitRequest{
			RiskIncreasing: true, Asset: base, Instrument: &inst, Exposure: exposure, Loss: loss,
		})
		use.Asset, use.Used, use.Codes = &base, ev.ExposureAfter, ev.Codes
		out = append(out, use)
	}
	return out, nil
}

func (a treasuryAdapter) Reconcile(ctx context.Context, house econ.HouseAccountID) ([]treasury.Discrepancy, error) {
	var out []treasury.Discrepancy
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d, err := a.svc.Reconcile(ctx, tx, house)
		out = d
		return err
	})
	return out, err
}

// --- reads of one book -----------------------------------------------------------------

// knownBook answers NOT_FOUND for a house account the treasury has not
// registered, so a list of an unknown book is never an empty success.
func (a treasuryAdapter) knownBook(ctx context.Context, q db.Querier, house econ.HouseAccountID) (treasury.Book, error) {
	return a.svc.GetBook(ctx, q, house)
}

func (a treasuryAdapter) Balances(ctx context.Context, house econ.HouseAccountID) (TreasuryBalancesView, error) {
	var out TreasuryBalancesView
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		b, err := a.knownBook(ctx, tx, house)
		if err != nil {
			return err
		}
		items, err := a.svc.Balances(ctx, tx, house)
		if err != nil {
			return err
		}
		known := TreasuryAssets{}
		ids := []assets.AssetID{b.ValuationAsset}
		for _, it := range items {
			ids = append(ids, it.Asset)
		}
		if err := treasuryDescribe(ctx, tx, a.assets, known, ids...); err != nil {
			return err
		}
		out = TreasuryBalancesView{House: house, Items: items, Assets: known}
		return nil
	})
	return out, err
}

const treasuryEventsSQL = `SELECT id, house_account_id, seq, kind, direction, asset_id, quantity::text, value::text, fees::text,
	qty_before::text, qty_after::text, cost_before::text, cost_after::text, cost_removed::text, realized_pnl::text,
	book_trade_id, paper_fill_id, funding_id, instrument_id, journal_transaction_id, occurred_at
	FROM treasury_position_events WHERE house_account_id = $1 AND ($2::bigint IS NULL OR seq < $2)
	ORDER BY seq DESC LIMIT $3`

func scanTreasuryEvent(row pgx.Row) (treasury.PositionEvent, error) {
	var e treasury.PositionEvent
	var v [9]string
	if err := row.Scan(&e.ID, &e.HouseAccountID, &e.Seq, &e.Kind, &e.Movement.Direction, &e.Asset, &v[0], &v[1], &v[2], &v[3], &v[4],
		&v[5], &v[6], &v[7], &v[8], &e.BookTradeID, &e.PaperFillID, &e.FundingID, &e.InstrumentID, &e.JournalTransactionID,
		&e.OccurredAt); err != nil {
		return treasury.PositionEvent{}, err
	}
	m := &e.Movement
	for i, dst := range []*money.Quantity{&m.Quantity, &m.Value, &m.Fees, &m.Before.Quantity, &m.After.Quantity, &m.Before.Cost,
		&m.After.Cost, &m.CostRemoved, &m.Realized} {
		q, err := money.ScanQuantity(v[i])
		if err != nil {
			return treasury.PositionEvent{}, errs.Wrap(err, errs.CodeInternal, "a stored position amount is not an integer")
		}
		*dst = q
	}
	e.OccurredAt = e.OccurredAt.UTC()
	return e, nil
}

// treasurySeqCursor decodes a register cursor: the sequence number to page
// below, nil for the first page.
func treasurySeqCursor(cursor string) (*int64, error) {
	c, ok, err := decodeCursor(cursor)
	if err != nil || !ok {
		return nil, err
	}
	seq, perr := strconv.ParseInt(c.ID, 10, 64)
	if perr != nil || seq < 1 {
		return nil, validationError("cursor", "cursor is not a valid pagination cursor")
	}
	return &seq, nil
}

func (a treasuryAdapter) Events(ctx context.Context, house econ.HouseAccountID, cursor string, limit int) (TreasuryEventPage, error) {
	below, err := treasurySeqCursor(cursor)
	if err != nil {
		return TreasuryEventPage{}, err
	}
	limit = clampLimit(limit)
	var out TreasuryEventPage
	err = a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := a.knownBook(ctx, tx, house); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, treasuryEventsSQL, house, below, limit+1)
		if err != nil {
			return err
		}
		var items []treasury.PositionEvent
		for rows.Next() {
			e, err := scanTreasuryEvent(rows)
			if err != nil {
				rows.Close()
				return err
			}
			items = append(items, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		out = TreasuryEventPage{Assets: TreasuryAssets{}}
		if len(items) > limit {
			items = items[:limit]
			out.NextCursor = encodeCursor(keysetCursor{ID: strconv.FormatInt(items[limit-1].Seq, 10)})
		}
		out.Items = items
		ids := make([]assets.AssetID, 0, len(items))
		for _, e := range items {
			ids = append(ids, e.Asset)
		}
		return treasuryDescribe(ctx, tx, a.assets, out.Assets, ids...)
	})
	return out, err
}

func (a treasuryAdapter) Actions(ctx context.Context, house econ.HouseAccountID, limit int) ([]treasury.Action, error) {
	var out []treasury.Action
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := a.knownBook(ctx, tx, house); err != nil {
			return err
		}
		items, err := a.svc.Actions(ctx, tx, house, clampLimit(limit))
		out = items
		return err
	})
	return out, err
}

const treasuryPaperFillsSQL = `SELECT id, filled_at FROM treasury_paper_fills WHERE house_account_id = $1
	AND ($2::timestamptz IS NULL OR (filled_at, id) < ($2::timestamptz, $3::uuid))
	ORDER BY filled_at DESC, id DESC LIMIT $4`

func (a treasuryAdapter) PaperFills(ctx context.Context, house econ.HouseAccountID, cursor string, limit int) (TreasuryPaperFillPage, error) {
	at, after, err := marginCursor(cursor)
	if err != nil {
		return TreasuryPaperFillPage{}, err
	}
	limit = clampLimit(limit)
	var out TreasuryPaperFillPage
	err = a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := a.knownBook(ctx, tx, house); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, treasuryPaperFillsSQL, house, at, after, limit+1)
		if err != nil {
			return err
		}
		type key struct {
			id treasury.PaperFillID
			at time.Time
		}
		var keys []key
		for rows.Next() {
			var k key
			if err := rows.Scan(&k.id, &k.at); err != nil {
				rows.Close()
				return err
			}
			keys = append(keys, k)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		out = TreasuryPaperFillPage{Assets: TreasuryAssets{}}
		if len(keys) > limit {
			keys = keys[:limit]
			last := keys[limit-1]
			out.NextCursor = encodeCursor(keysetCursor{At: last.at.UTC(), ID: last.id.String()})
		}
		for _, k := range keys {
			// Each fill is read back through the treasury, which proves its
			// disclosure is the canonical form its hash names.
			f, err := a.svc.Fill(ctx, tx, k.id)
			if err != nil {
				return err
			}
			if err := treasuryDescribe(ctx, tx, a.assets, out.Assets, f.BaseAsset, f.QuoteAsset); err != nil {
				return err
			}
			out.Items = append(out.Items, f)
		}
		return nil
	})
	return out, err
}

const treasuryDecisionsSQL = `SELECT id, house_account_id, intent_kind, instrument_id, asset_id, risk_increasing, stage, verdict,
	reason_codes, treasury_codes, limits, input_hash, decision_hash, risk_policy_version, evaluator_version,
	treasury_policy_version, treasury_policy_hash, evaluated_at
	FROM treasury_risk_decisions WHERE house_account_id = $1
	AND ($2::timestamptz IS NULL OR (evaluated_at, id) < ($2::timestamptz, $3::uuid))
	ORDER BY evaluated_at DESC, id DESC LIMIT $4`

func (a treasuryAdapter) RiskDecisions(ctx context.Context, house econ.HouseAccountID, cursor string, limit int) (TreasuryDecisionPage, error) {
	at, after, err := marginCursor(cursor)
	if err != nil {
		return TreasuryDecisionPage{}, err
	}
	limit = clampLimit(limit)
	var out TreasuryDecisionPage
	err = a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := a.knownBook(ctx, tx, house); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, treasuryDecisionsSQL, house, at, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		var items []TreasuryDecisionRecord
		for rows.Next() {
			var d TreasuryDecisionRecord
			var limits []byte
			if err := rows.Scan(&d.ID, &d.HouseAccountID, &d.IntentKind, &d.Instrument, &d.Asset, &d.RiskIncreasing, &d.Stage, &d.Verdict,
				&d.ReasonCodes, &d.TreasuryCodes, &limits, &d.InputHash, &d.DecisionHash, &d.RiskPolicyVersion, &d.EvaluatorVersion,
				&d.TreasuryPolicyVersion, &d.TreasuryPolicyHash, &d.EvaluatedAt); err != nil {
				return err
			}
			if err := json.Unmarshal(limits, &d.Limits); err != nil {
				return errs.Wrap(err, errs.CodeInternal, "a stored limit evaluation could not be decoded")
			}
			d.EvaluatedAt = d.EvaluatedAt.UTC()
			items = append(items, d)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out = TreasuryDecisionPage{}
		if len(items) > limit {
			items = items[:limit]
			last := items[limit-1]
			out.NextCursor = encodeCursor(keysetCursor{At: last.EvaluatedAt, ID: last.ID.String()})
		}
		out.Items = items
		return nil
	})
	return out, err
}

func (a treasuryAdapter) CrossingDisclosures(ctx context.Context, instrument instruments.InstrumentID) ([]treasury.CrossingDisclosure, error) {
	var out []treasury.CrossingDisclosure
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d, err := a.svc.CrossingDisclosures(ctx, tx, instrument)
		out = d
		return err
	})
	return out, err
}

// treasuryHouseTradesSQL lists the trades between a house book and one
// customer account, with the disclosure recorded for the crossing rule
// version that allowed each (absent when that version was never disclosed).
const treasuryHouseTradesSQL = `SELECT t.id, t.book_id, t.instrument_id, t.trade_seq, (t.price_ticks * bs.tick_size)::text, bs.price_scale,
	t.base_asset_id, t.quote_asset_id, t.base_quantity::text, t.quote_notional::text, t.maker_owner_type, t.maker_owner_id,
	t.taker_owner_id, t.aggressor_side, t.maker_fee::text, t.taker_fee::text, t.crossing_rule_version, coalesce(d.disclosure, ''),
	t.journal_transaction_id, j.effective_at
	FROM book_trades t
	JOIN book_states bs ON bs.id = t.book_id
	JOIN journal_transactions j ON j.id = t.journal_transaction_id
	LEFT JOIN treasury_crossing_disclosures d ON d.book_id = t.book_id AND d.rule_version = t.crossing_rule_version
	WHERE ((t.maker_owner_type = 'HOUSE' AND t.taker_owner_type = 'CUSTOMER' AND t.taker_owner_id = $1)
	    OR (t.taker_owner_type = 'HOUSE' AND t.maker_owner_type = 'CUSTOMER' AND t.maker_owner_id = $1))
	  AND ($2::timestamptz IS NULL OR (j.effective_at, t.id) < ($2::timestamptz, $3::uuid))
	ORDER BY j.effective_at DESC, t.id DESC LIMIT $4`

func (a treasuryAdapter) HouseTrades(ctx context.Context, account accounts.AccountID, cursor string, limit int) (TreasuryHouseTradePage, error) {
	at, after, err := marginCursor(cursor)
	if err != nil {
		return TreasuryHouseTradePage{}, err
	}
	limit = clampLimit(limit)
	var out TreasuryHouseTradePage
	err = a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, treasuryHouseTradesSQL, account, at, after, limit+1)
		if err != nil {
			return err
		}
		type raw struct {
			t                                  TreasuryHouseTrade
			base, quote                        assets.AssetID
			mantissa, qty, notional, mfee, tfe string
			scale                              int32
			makerType                          string
			makerOwner, takerOwner             uuid.UUID
			aggressor                          orders.Side
		}
		var rs []raw
		for rows.Next() {
			var r raw
			if err := rows.Scan(&r.t.TradeID, &r.t.BookID, &r.t.Instrument, &r.t.TradeSeq, &r.mantissa, &r.scale, &r.base, &r.quote,
				&r.qty, &r.notional, &r.makerType, &r.makerOwner, &r.takerOwner, &r.aggressor, &r.mfee, &r.tfe,
				&r.t.CrossingRuleVersion, &r.t.Disclosure, &r.t.JournalTransaction, &r.t.TradedAt); err != nil {
				rows.Close()
				return err
			}
			rs = append(rs, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		out = TreasuryHouseTradePage{}
		if len(rs) > limit {
			rs = rs[:limit]
			last := rs[limit-1].t
			out.NextCursor = encodeCursor(keysetCursor{At: last.TradedAt.UTC(), ID: last.TradeID.String()})
		}
		known := TreasuryAssets{}
		for _, r := range rs {
			t := r.t
			var mantissa, makerFee, takerFee money.Quantity
			if err := marginScanQuantities(marginQty{&mantissa, r.mantissa}, marginQty{&t.BaseQuantity, r.qty},
				marginQty{&t.QuoteNotional, r.notional}, marginQty{&makerFee, r.mfee}, marginQty{&takerFee, r.tfe}); err != nil {
				return err
			}
			if err := treasuryDescribe(ctx, tx, a.assets, known, r.base, r.quote); err != nil {
				return err
			}
			t.Base, t.Quote = known[r.base], known[r.quote]
			t.TradedAt = t.TradedAt.UTC()
			t.Price = money.Price{Mantissa: mantissa, Scale: r.scale, QuoteAsset: r.quote.String(), Source: "internal-book", At: t.TradedAt}
			// The account's side and fee are its own order's; the house book is
			// on the other side of the same trade. The aggressor is the taker.
			var houseOwner uuid.UUID
			if r.makerType == string(orders.OwnerCustomer) {
				houseOwner = r.takerOwner
				t.AccountSide, t.AccountFee = treasuryOpposite(r.aggressor), makerFee
			} else {
				houseOwner = r.makerOwner
				t.AccountSide, t.AccountFee = r.aggressor, takerFee
			}
			t.HouseSide = treasuryOpposite(t.AccountSide)
			hid, err := econ.ParseHouseAccountID(houseOwner.String())
			if err != nil {
				return errs.New(errs.CodeInternal, "a house trade names a house book id that is not a UUID")
			}
			t.House = hid
			out.Items = append(out.Items, t)
		}
		return nil
	})
	return out, err
}

func treasuryOpposite(s orders.Side) orders.Side {
	if s == orders.SideBuy {
		return orders.SideSell
	}
	return orders.SideBuy
}
