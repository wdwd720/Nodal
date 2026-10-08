package treasury

import (
	"context"
	"encoding/json"
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
)

// snapshot is the house book's holdings and working orders around one asset,
// read in ONE statement so a concurrent fill (which moves quantity from a
// working buy into inventory in one transaction) is seen entirely or not at
// all.
type snapshot struct {
	heldBase      money.Quantity
	heldQuote     money.Quantity
	reservedBase  money.Quantity
	reservedQuote money.Quantity
	workingBuys   money.Quantity
	instrumentBuy money.Quantity
}

func (s *Service) snapshotOf(ctx context.Context, tx pgx.Tx, book Book, base assets.AssetID, instrument *instruments.InstrumentID) (snapshot, error) {
	var v [6]string
	err := tx.QueryRow(ctx, `SELECT
		coalesce((SELECT b.balance FROM ledger_accounts a JOIN ledger_balances b ON b.ledger_account_id = a.id
		           WHERE a.owner_type = 'HOUSE' AND a.owner_id = $1 AND a.code = 'HOUSE_INVENTORY' AND a.asset_id = $2), 0)::text,
		coalesce((SELECT b.balance FROM ledger_accounts a JOIN ledger_balances b ON b.ledger_account_id = a.id
		           WHERE a.owner_type = 'HOUSE' AND a.owner_id = $1 AND a.code = 'HOUSE_INVENTORY' AND a.asset_id = $3), 0)::text,
		(SELECT coalesce(sum(quantity - consumed_quantity), 0) FROM book_house_reservations
		  WHERE house_account_id = $1 AND asset_id = $2 AND status = 'ACTIVE')::text,
		(SELECT coalesce(sum(quantity - consumed_quantity), 0) FROM book_house_reservations
		  WHERE house_account_id = $1 AND asset_id = $3 AND status = 'ACTIVE')::text,
		(SELECT coalesce(sum(o.remaining_lots * bs.lot_size), 0) FROM book_orders o JOIN book_states bs ON bs.id = o.book_id
		  WHERE o.house_account_id = $1 AND o.side = 'BUY' AND o.status IN ('PENDING','OPEN','PARTIALLY_FILLED')
		    AND bs.base_asset_id = $2)::text,
		(SELECT coalesce(sum(o.price_ticks * o.remaining_lots * bs.notional_per_tick_lot), 0) FROM book_orders o
		   JOIN book_states bs ON bs.id = o.book_id
		  WHERE o.house_account_id = $1 AND o.side = 'BUY' AND o.status IN ('PENDING','OPEN','PARTIALLY_FILLED')
		    AND o.instrument_id = $4)::text`,
		book.HouseAccountID, base, book.ValuationAsset, instrument).Scan(&v[0], &v[1], &v[2], &v[3], &v[4], &v[5])
	if err != nil {
		return snapshot{}, dbErr("house exposure snapshot", err)
	}
	var out snapshot
	for i, dst := range []*money.Quantity{&out.heldBase, &out.heldQuote, &out.reservedBase, &out.reservedQuote, &out.workingBuys, &out.instrumentBuy} {
		q, err := money.ScanQuantity(v[i])
		if err != nil {
			return snapshot{}, errs.Wrap(err, errs.CodeInternal, "treasury: stored exposure amounts")
		}
		*dst = q
	}
	return out, nil
}

// markOf returns a fresh mark of asset in the valuation asset, or nil when no
// observation is fresh enough. A stale mark is "no mark", never an old one.
func (s *Service) markOf(ctx context.Context, q db.Querier, asset, valuationAsset assets.AssetID, maxAge time.Duration, now time.Time) (*money.Price, error) {
	p, err := s.prices.Latest(ctx, q, asset, valuationAsset, maxAge, now)
	if err != nil {
		if errs.CodeOf(err) == errs.CodeStaleMarketData {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

// MarkRecord is a mark as a report states it.
type MarkRecord struct {
	Mantissa money.Quantity `json:"mantissa"`
	Scale    int32          `json:"scale"`
	Source   string         `json:"source"`
	At       time.Time      `json:"at"`
}

// AssetPnL is one position of a PnL report, in valuation-asset base units.
type AssetPnL struct {
	Asset    string         `json:"asset"`
	Quantity money.Quantity `json:"quantity"`
	Cost     money.Quantity `json:"cost"`
	// Mark, MarketValue and Unrealized are absent when no fresh mark exists.
	Mark          *MarkRecord     `json:"mark"`
	MarketValue   *money.Quantity `json:"market_value"`
	Unrealized    *money.Quantity `json:"unrealized"`
	RealizedTotal money.Quantity  `json:"realized_total"`
	RealizedToday money.Quantity  `json:"realized_today"`
}

// Report is a house book's realized and unrealized PnL from its fills and the
// marks, as of its position register.
type Report struct {
	Kind            string         `json:"kind"`
	HouseAccountID  string         `json:"house_account_id"`
	ValuationAsset  string         `json:"valuation_asset"`
	At              time.Time      `json:"at"`
	TradingDay      string         `json:"trading_day"`
	ThroughEventSeq int64          `json:"through_event_seq"`
	Assets          []AssetPnL     `json:"assets"`
	RealizedTotal   money.Quantity `json:"realized_total"`
	RealizedToday   money.Quantity `json:"realized_today"`
	UnrealizedTotal money.Quantity `json:"unrealized_total"`
	// OpenLosses is the sum of the negative unrealized PnL of every marked
	// position: an open gain never offsets another position's loss.
	OpenLosses money.Quantity `json:"open_losses"`
	// LossMeasure is what the daily loss limit binds: RealizedToday +
	// OpenLosses.
	LossMeasure  money.Quantity    `json:"loss_measure"`
	MissingMarks []string          `json:"missing_marks"`
	Rounding     map[string]string `json:"rounding"`
}

// ReportKind is the format tag of a PnL report.
const ReportKind = "treasury.pnl_report.v1"

// report computes the PnL report from the register (the caller has synced it)
// at marks no older than maxAge.
func (s *Service) report(ctx context.Context, q db.Querier, book Book, maxAge time.Duration, now time.Time) (Report, error) {
	now = now.UTC()
	dayStart, dayEnd := tradingDay(now)
	r := Report{
		Kind: ReportKind, HouseAccountID: book.HouseAccountID.String(), ValuationAsset: book.ValuationAsset.String(), At: now,
		TradingDay: dayStart.Format("2006-01-02"), Assets: []AssetPnL{}, MissingMarks: []string{},
		Rounding: map[string]string{
			"cost_released": CostRounding.String(), "market_value": MarketValueRounding.String(),
		},
	}
	rows, err := q.Query(ctx, `SELECT asset_id, qty_after::text, cost_after::text, seq,
		(SELECT sum(realized_pnl) FROM treasury_position_events x WHERE x.house_account_id = e.house_account_id AND x.asset_id = e.asset_id)::text,
		(SELECT coalesce(sum(realized_pnl), 0) FROM treasury_position_events x WHERE x.house_account_id = e.house_account_id
		   AND x.asset_id = e.asset_id AND x.occurred_at >= $2 AND x.occurred_at < $3)::text
		FROM (SELECT DISTINCT ON (asset_id) * FROM treasury_position_events WHERE house_account_id = $1 ORDER BY asset_id, seq DESC) e
		ORDER BY asset_id`, book.HouseAccountID, dayStart, dayEnd)
	if err != nil {
		return Report{}, dbErr("position report", err)
	}
	type line struct {
		asset                        assets.AssetID
		pos                          Position
		realizedTotal, realizedToday money.Quantity
	}
	var lines []line
	for rows.Next() {
		var l line
		var qty, cost, total, today string
		var seq int64
		if err := rows.Scan(&l.asset, &qty, &cost, &seq, &total, &today); err != nil {
			rows.Close()
			return Report{}, dbErr("scan position report", err)
		}
		vals := []*money.Quantity{&l.pos.Quantity, &l.pos.Cost, &l.realizedTotal, &l.realizedToday}
		for i, src := range []string{qty, cost, total, today} {
			v, err := money.ScanQuantity(src)
			if err != nil {
				rows.Close()
				return Report{}, errs.Wrap(err, errs.CodeInternal, "treasury: stored position amounts")
			}
			*vals[i] = v
		}
		if seq > r.ThroughEventSeq {
			r.ThroughEventSeq = seq
		}
		lines = append(lines, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Report{}, dbErr("position report", err)
	}
	if err := q.QueryRow(ctx, `SELECT coalesce(max(seq), 0) FROM treasury_position_events WHERE house_account_id = $1`,
		book.HouseAccountID).Scan(&r.ThroughEventSeq); err != nil {
		return Report{}, dbErr("position report", err)
	}
	valuationAsset, err := s.assets.Get(ctx, q, book.ValuationAsset)
	if err != nil {
		return Report{}, err
	}
	for _, l := range lines {
		a := AssetPnL{Asset: l.asset.String(), Quantity: l.pos.Quantity, Cost: l.pos.Cost, RealizedTotal: l.realizedTotal,
			RealizedToday: l.realizedToday}
		r.RealizedTotal = r.RealizedTotal.Add(l.realizedTotal)
		r.RealizedToday = r.RealizedToday.Add(l.realizedToday)
		if l.pos.Quantity.IsPositive() {
			mark, err := s.markOf(ctx, q, l.asset, book.ValuationAsset, maxAge, now)
			if err != nil {
				return Report{}, err
			}
			if mark == nil {
				r.MissingMarks = append(r.MissingMarks, l.asset.String())
			} else {
				asset, err := s.assets.Get(ctx, q, l.asset)
				if err != nil {
					return Report{}, err
				}
				value, err := money.Notional(l.pos.Quantity, asset.Decimals, *mark, valuationAsset.Decimals, MarketValueRounding)
				if err != nil {
					return Report{}, errs.Wrap(err, errs.CodeInternal, "treasury: market value")
				}
				unrealized := l.pos.Unrealized(value)
				a.Mark = &MarkRecord{Mantissa: mark.Mantissa, Scale: mark.Scale, Source: mark.Source, At: mark.At.UTC()}
				a.MarketValue, a.Unrealized = &value, &unrealized
				r.UnrealizedTotal = r.UnrealizedTotal.Add(unrealized)
				if unrealized.IsNegative() {
					r.OpenLosses = r.OpenLosses.Add(unrealized)
				}
			}
		}
		r.Assets = append(r.Assets, a)
	}
	sort.Strings(r.MissingMarks)
	r.LossMeasure = r.RealizedToday.Add(r.OpenLosses)
	return r, nil
}

// lossState is the loss-limit view of a report.
func (r Report) lossState() (LossState, error) {
	out := LossState{RealizedToday: r.RealizedToday, OpenLosses: r.OpenLosses}
	for _, a := range r.MissingMarks {
		aid, err := assets.ParseAssetID(a)
		if err != nil {
			return LossState{}, errs.Wrap(err, errs.CodeInternal, "treasury: report asset")
		}
		out.MissingMarks = append(out.MissingMarks, aid)
	}
	return out, nil
}

// maxMarkAge is the policy's mark age, or the funding default without one.
func (s *Service) maxMarkAge(pv PolicyVersion, found bool) time.Duration {
	if found {
		return pv.Policy.MaxMarkAge
	}
	return s.fundingMarkAge
}

// Report syncs the house book's register and returns its PnL at fresh marks.
// It takes the book's lock.
func (s *Service) Report(ctx context.Context, tx pgx.Tx, house econ.HouseAccountID) (Report, error) {
	book, err := s.lockBook(ctx, tx, house)
	if err != nil {
		return Report{}, err
	}
	if _, err := s.syncLocked(ctx, tx, book); err != nil {
		return Report{}, err
	}
	pv, found, err := s.currentPolicy(ctx, tx, house)
	if err != nil {
		return Report{}, err
	}
	return s.report(ctx, tx, book, s.maxMarkAge(pv, found), s.clk.Now())
}

// Snapshot is a persisted mark-to-market of a house book.
type Snapshot struct {
	ID           SnapshotID
	Report       Report
	Document     []byte
	DocumentHash string
	Action       Action
}

// MarkToMarket persists the house book's PnL report as a hashed snapshot and
// records the MARK_TO_MARKET action. The house service or an operator takes
// it.
func (s *Service) MarkToMarket(ctx context.Context, tx pgx.Tx, house econ.HouseAccountID, idempotencyKey string, actor econ.Actor) (Snapshot, error) {
	if err := validKey(idempotencyKey); err != nil {
		return Snapshot{}, err
	}
	book, err := s.lockBook(ctx, tx, house)
	if err != nil {
		return Snapshot{}, err
	}
	sp, err := s.currentSigningPolicy(ctx, tx, house)
	if err != nil {
		return Snapshot{}, err
	}
	if err := sp.Authorize(actor, ActionMarkToMarket); err != nil {
		return Snapshot{}, err
	}
	if prior, found, err := s.actionByKey(ctx, tx, house, idempotencyKey); err != nil {
		return Snapshot{}, err
	} else if found {
		if prior.Kind != ActionMarkToMarket || prior.SnapshotID == nil {
			return Snapshot{}, errs.New(errs.CodeInvalidIdempotencyReuse, "treasury: this idempotency key was used for a different house action")
		}
		return s.snapshotByID(ctx, tx, *prior.SnapshotID, prior)
	}
	if _, err := s.syncLocked(ctx, tx, book); err != nil {
		return Snapshot{}, err
	}
	pv, found, err := s.currentPolicy(ctx, tx, house)
	if err != nil {
		return Snapshot{}, err
	}
	now := s.clk.Now().UTC()
	rep, err := s.report(ctx, tx, book, s.maxMarkAge(pv, found), now)
	if err != nil {
		return Snapshot{}, err
	}
	doc, err := json.Marshal(rep)
	if err != nil {
		return Snapshot{}, errs.Wrap(err, errs.CodeInternal, "treasury: encode PnL report")
	}
	snap := Snapshot{ID: id.New[snapshotKind](), Report: rep, Document: doc, DocumentHash: hashHex(doc)}
	if _, err := tx.Exec(ctx, `INSERT INTO treasury_pnl_snapshots (id, house_account_id, through_event_seq, realized_total,
		realized_today, unrealized_total, open_losses, document, document_hash, taken_at)
		VALUES ($1,$2,$3,$4::numeric,$5::numeric,$6::numeric,$7::numeric,$8,$9,$10)`,
		snap.ID, house, rep.ThroughEventSeq, rep.RealizedTotal.String(), rep.RealizedToday.String(), rep.UnrealizedTotal.String(),
		rep.OpenLosses.String(), doc, snap.DocumentHash, now); err != nil {
		return Snapshot{}, dbErr("record PnL snapshot", err)
	}
	reqHash, err := econ.RequestHash(map[string]any{"mark_to_market": house.String()})
	if err != nil {
		return Snapshot{}, err
	}
	a := Action{
		ID: id.New[actionKind](), HouseAccountID: house, Kind: ActionMarkToMarket, Outcome: OutcomeApplied, Actor: actor,
		SigningPolicyVersion: sp.Version, IdempotencyKey: idempotencyKey, RequestHash: reqHash, SnapshotID: &snap.ID,
		OccurredAt: now,
	}
	if found {
		a.RiskPolicyVersion = pv.Version
	}
	if err := s.recordAction(ctx, tx, book, &a, "mark to market", map[string]any{
		"snapshot_id": snap.ID.String(), "document_hash": snap.DocumentHash, "loss_measure": rep.LossMeasure.String(),
	}); err != nil {
		return Snapshot{}, err
	}
	snap.Action = a
	return snap, nil
}

func (s *Service) snapshotByID(ctx context.Context, q db.Querier, sid SnapshotID, a Action) (Snapshot, error) {
	snap := Snapshot{ID: sid, Action: a}
	if err := q.QueryRow(ctx, `SELECT document, document_hash FROM treasury_pnl_snapshots WHERE id = $1`, sid).
		Scan(&snap.Document, &snap.DocumentHash); err != nil {
		return Snapshot{}, notFound(err, "PnL snapshot", sid.String())
	}
	if err := json.Unmarshal(snap.Document, &snap.Report); err != nil {
		return Snapshot{}, errs.Wrap(err, errs.CodeInternal, "treasury: stored PnL snapshot is unreadable")
	}
	return snap, nil
}

// VenueFlow is what one venue moved into and out of a position.
type VenueFlow struct {
	Bought money.Quantity
	Sold   money.Quantity
}

// AssetBalance is the house book's standing in one asset: what the ledger
// holds, what working book orders have reserved, what is available, and how
// each venue and the fundings built it up.
type AssetBalance struct {
	Asset        assets.AssetID
	Inventory    money.Quantity
	Reserved     money.Quantity
	Available    money.Quantity
	Funded       money.Quantity
	InternalBook VenueFlow
	PaperVenue   VenueFlow
	// WorkingBuys and WorkingSells are the remaining base quantities of the
	// book's working orders in this asset.
	WorkingBuys  money.Quantity
	WorkingSells money.Quantity
}

// Balances returns the house book's inventory and venue balances, read only,
// asset by asset.
func (s *Service) Balances(ctx context.Context, q db.Querier, house econ.HouseAccountID) ([]AssetBalance, error) {
	rows, err := q.Query(ctx, `WITH inv AS (
			SELECT a.asset_id, coalesce(b.balance, 0) AS inventory FROM ledger_accounts a LEFT JOIN ledger_balances b ON b.ledger_account_id = a.id
			 WHERE a.owner_type = 'HOUSE' AND a.owner_id = $1 AND a.code = 'HOUSE_INVENTORY'),
		res AS (SELECT asset_id, sum(quantity - consumed_quantity) AS reserved FROM book_house_reservations
			 WHERE house_account_id = $1 AND status = 'ACTIVE' GROUP BY asset_id),
		fund AS (SELECT asset_id, sum(quantity) AS funded FROM treasury_fundings WHERE house_account_id = $1 GROUP BY asset_id),
		ev AS (SELECT asset_id,
			 sum(quantity) FILTER (WHERE kind = 'BOOK_FILL' AND direction = 'INCREASE') AS book_bought,
			 sum(quantity) FILTER (WHERE kind = 'BOOK_FILL' AND direction = 'DECREASE') AS book_sold,
			 sum(quantity) FILTER (WHERE kind = 'PAPER_FILL' AND direction = 'INCREASE') AS paper_bought,
			 sum(quantity) FILTER (WHERE kind = 'PAPER_FILL' AND direction = 'DECREASE') AS paper_sold
			 FROM treasury_position_events WHERE house_account_id = $1 GROUP BY asset_id),
		work AS (SELECT bs.base_asset_id AS asset_id,
			 sum(o.remaining_lots * bs.lot_size) FILTER (WHERE o.side = 'BUY') AS buys,
			 sum(o.remaining_lots * bs.lot_size) FILTER (WHERE o.side = 'SELL') AS sells
			 FROM book_orders o JOIN book_states bs ON bs.id = o.book_id
			 WHERE o.house_account_id = $1 AND o.status IN ('PENDING','OPEN','PARTIALLY_FILLED') GROUP BY bs.base_asset_id),
		ids AS (SELECT asset_id FROM inv UNION SELECT asset_id FROM res UNION SELECT asset_id FROM fund
			 UNION SELECT asset_id FROM ev UNION SELECT asset_id FROM work)
		SELECT ids.asset_id, coalesce(inv.inventory, 0)::text, coalesce(res.reserved, 0)::text, coalesce(fund.funded, 0)::text,
		       coalesce(ev.book_bought, 0)::text, coalesce(ev.book_sold, 0)::text, coalesce(ev.paper_bought, 0)::text,
		       coalesce(ev.paper_sold, 0)::text, coalesce(work.buys, 0)::text, coalesce(work.sells, 0)::text
		  FROM ids LEFT JOIN inv USING (asset_id) LEFT JOIN res USING (asset_id) LEFT JOIN fund USING (asset_id)
		  LEFT JOIN ev USING (asset_id) LEFT JOIN work USING (asset_id)
		 ORDER BY ids.asset_id`, house)
	if err != nil {
		return nil, dbErr("house balances", err)
	}
	defer rows.Close()
	var out []AssetBalance
	for rows.Next() {
		var b AssetBalance
		var v [9]string
		if err := rows.Scan(&b.Asset, &v[0], &v[1], &v[2], &v[3], &v[4], &v[5], &v[6], &v[7], &v[8]); err != nil {
			return nil, dbErr("scan house balances", err)
		}
		dst := []*money.Quantity{&b.Inventory, &b.Reserved, &b.Funded, &b.InternalBook.Bought, &b.InternalBook.Sold,
			&b.PaperVenue.Bought, &b.PaperVenue.Sold, &b.WorkingBuys, &b.WorkingSells}
		for i, p := range dst {
			q, err := money.ScanQuantity(v[i])
			if err != nil {
				return nil, errs.Wrap(err, errs.CodeInternal, "treasury: stored balance amounts")
			}
			*p = q
		}
		b.Available = b.Inventory.Sub(b.Reserved)
		out = append(out, b)
	}
	return out, rows.Err()
}

// Discrepancy is one disagreement Reconcile found.
type Discrepancy struct {
	Asset    assets.AssetID
	What     string
	Expected money.Quantity
	Actual   money.Quantity
}

// Reconcile proves the house book's register against its ledger, asset by
// asset, after syncing it (it takes the book's lock):
//
//   - every non-cash position's quantity is its HOUSE_INVENTORY balance, and
//     is exactly what fundings and both venues moved;
//   - the valuation asset's HOUSE_INVENTORY is the cash funded, less every
//     acquisition's cost and fees, plus every disposal's proceeds less fees;
//   - HOUSE_FEES is exactly the fees in the register;
//   - no house account of the book holds anything in another owner's name
//     (the ledger's owner type is HOUSE by construction).
//
// An empty result means the books agree.
func (s *Service) Reconcile(ctx context.Context, tx pgx.Tx, house econ.HouseAccountID) ([]Discrepancy, error) {
	book, err := s.lockBook(ctx, tx, house)
	if err != nil {
		return nil, err
	}
	if _, err := s.syncLocked(ctx, tx, book); err != nil {
		return nil, err
	}
	balances, err := s.Balances(ctx, tx, house)
	if err != nil {
		return nil, err
	}
	var out []Discrepancy
	for _, b := range balances {
		if b.Asset == book.ValuationAsset {
			continue
		}
		pos, err := positionOf(ctx, tx, house, b.Asset)
		if err != nil {
			return nil, err
		}
		if !pos.Quantity.Equal(b.Inventory) {
			out = append(out, Discrepancy{Asset: b.Asset, What: "position vs HOUSE_INVENTORY", Expected: b.Inventory, Actual: pos.Quantity})
		}
		moved := b.Funded.Add(b.InternalBook.Bought).Sub(b.InternalBook.Sold).Add(b.PaperVenue.Bought).Sub(b.PaperVenue.Sold)
		if !moved.Equal(b.Inventory) {
			out = append(out, Discrepancy{Asset: b.Asset, What: "fundings and venues vs HOUSE_INVENTORY", Expected: b.Inventory, Actual: moved})
		}
	}
	var cash, fees string
	if err := tx.QueryRow(ctx, `SELECT
		((SELECT coalesce(sum(quantity), 0) FROM treasury_fundings WHERE house_account_id = $1 AND asset_id = $2)
		 - coalesce((SELECT sum(value + fees) FROM treasury_position_events WHERE house_account_id = $1 AND direction = 'INCREASE'
		              AND kind <> 'FUNDING'), 0)
		 + coalesce((SELECT sum(value - fees) FROM treasury_position_events WHERE house_account_id = $1 AND direction = 'DECREASE'), 0))::text,
		(SELECT coalesce(sum(fees), 0) FROM treasury_position_events WHERE house_account_id = $1)::text`,
		house, book.ValuationAsset).Scan(&cash, &fees); err != nil {
		return nil, dbErr("reconcile cash", err)
	}
	wantCash, err := money.ScanQuantity(cash)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "treasury: reconcile cash")
	}
	wantFees, err := money.ScanQuantity(fees)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "treasury: reconcile fees")
	}
	haveCash, err := s.ledger.Balance(ctx, tx, book.account(ledger.CodeHouseInventory, book.ValuationAsset))
	if err != nil {
		return nil, err
	}
	if !haveCash.Equal(wantCash) {
		out = append(out, Discrepancy{Asset: book.ValuationAsset, What: "cash vs fundings, costs, proceeds and fees", Expected: haveCash, Actual: wantCash})
	}
	haveFees, err := s.ledger.Balance(ctx, tx, book.account(ledger.CodeHouseFees, book.ValuationAsset))
	if err != nil {
		return nil, err
	}
	if !haveFees.Equal(wantFees) {
		out = append(out, Discrepancy{Asset: book.ValuationAsset, What: "HOUSE_FEES vs the register's fees", Expected: haveFees, Actual: wantFees})
	}
	return out, nil
}
