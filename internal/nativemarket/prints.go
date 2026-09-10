package nativemarket

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// The public price series and the candles built from it (product goal §14).
//
// # Why a print table rather than deriving candles from asset_prices
//
// asset_prices already carries this market's post-trade spot price, and it is
// the series the prediction resolver reads. It carries no volume, and it links
// to the trade that set it only through a text raw_ref -- so "how many Credits
// traded in this minute" cannot be answered from it at all, and "which fill
// produced this price" can only be answered by parsing a string. A chart needs
// both.
//
// The print is therefore a projection of the fill, not a second opinion about
// it: migration 00771's trigger recomputes every price from the fill's own
// amounts and the market's frozen curve and refuses a print that disagrees. The
// arithmetic below and the arithmetic in SQL are the same arithmetic, written
// twice on purpose, exactly as the constant-product invariant is.
//
// # Why the candle's high and low look at both spot prices
//
// A candle built from trade prices alone understates the range: between two
// trades the market sat at a price nobody traded at, and the pre-trade spot of
// the first trade in a bucket IS the price the bucket opened at. Using
// (spot_before, spot_after) of every print captures the whole path the marginal
// price took, which is what a chart is supposed to show.

// recordPrint writes the public print of a fill.
func (s *Service) recordPrint(ctx context.Context, tx pgx.Tx, m Market, fill Fill, fillID FillID, seq int64, at time.Time) error {
	creditVolume, assetVolume := fill.CreditsIn, fill.AssetsOut
	if fill.Side == Sell {
		creditVolume, assetVolume = fill.CreditsToPool, fill.AssetsIn
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO native_market_prints
		   (fill_id, market_id, asset_id, seq, side, price_scale,
		    spot_price_before, spot_price_after, effective_price,
		    credit_volume, asset_volume, printed_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7::numeric,$8::numeric,$9::numeric,$10::numeric,$11::numeric,$12)`,
		fillID, m.ID, m.AssetID, seq, string(fill.Side), PriceScale,
		fill.SpotBefore.String(), fill.SpotAfter.String(), fill.EffectivePrice.String(),
		creditVolume.String(), assetVolume.String(), at.UTC())
	if err != nil {
		return mapError(err)
	}
	return nil
}

// Interval is a candle width.
type Interval string

// The intervals a chart may ask for. They are a closed set because each one is
// an index-friendly bucket the database can compute; an arbitrary interval
// would be an arbitrary scan.
const (
	Interval1m  Interval = "1m"
	Interval5m  Interval = "5m"
	Interval15m Interval = "15m"
	Interval1h  Interval = "1h"
	Interval1d  Interval = "1d"
)

var intervalDurations = map[Interval]time.Duration{
	Interval1m:  time.Minute,
	Interval5m:  5 * time.Minute,
	Interval15m: 15 * time.Minute,
	Interval1h:  time.Hour,
	Interval1d:  24 * time.Hour,
}

// AllIntervals returns every declared interval, shortest first.
func AllIntervals() []Interval {
	return []Interval{Interval1m, Interval5m, Interval15m, Interval1h, Interval1d}
}

// Duration returns the interval's width, and whether it is declared.
func (i Interval) Duration() (time.Duration, bool) {
	d, ok := intervalDurations[i]
	return d, ok
}

func (i Interval) String() string { return string(i) }

// MaxCandles bounds one candle request.
//
// A range is bounded rather than paged because a chart wants a whole window at
// once and an unbounded one is a table scan a client can ask for by typing a
// date. 1,500 buckets is more than any screen renders and is what makes the
// query's cost a function of the index rather than of the market's history.
const MaxCandles = 1_500

// Candle is one OHLCV bucket. Prices are credit base units per asset base
// unit, scaled by PriceScale; volumes are exact base units.
type Candle struct {
	OpenTime     time.Time
	Open         money.Quantity
	High         money.Quantity
	Low          money.Quantity
	Close        money.Quantity
	CreditVolume money.Quantity
	AssetVolume  money.Quantity
	Trades       int64
}

// CandleRequest asks for one bounded window of a market's history.
type CandleRequest struct {
	MarketID MarketID
	Interval Interval
	// From is inclusive, To exclusive. Both are required: a chart that asks
	// for "everything" on a market with a year of prints is a scan.
	From time.Time
	To   time.Time
}

// Validate checks the window without touching the database.
func (r CandleRequest) Validate() error {
	if r.MarketID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "candles need a market")
	}
	d, ok := r.Interval.Duration()
	if !ok {
		return errs.Newf(errs.CodeValidationFailed,
			"unknown candle interval %q; the intervals are 1m, 5m, 15m, 1h and 1d", r.Interval)
	}
	if r.From.IsZero() || r.To.IsZero() {
		return errs.New(errs.CodeValidationFailed, "candles need both from and to")
	}
	if !r.To.After(r.From) {
		return errs.New(errs.CodeValidationFailed, "the candle window's `to` must be after its `from`")
	}
	if buckets := int64(r.To.Sub(r.From) / d); buckets > MaxCandles {
		return errs.Newf(errs.CodeValidationFailed,
			"that window is %d %s candles, past the %d one request may ask for; narrow it or use a wider interval",
			buckets, r.Interval, MaxCandles)
	}
	return nil
}

// candleEpoch is the origin every bucket is aligned to, so the same minute is
// the same bucket in every request and for every market.
var candleEpoch = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

// Candles computes OHLCV over a bounded window, in SQL.
//
// It is computed rather than stored: a stored candle is a second copy of the
// prints that has to be corrected when a print is corrected, and prints are
// immutable, so the computation can never be stale. `date_bin` aligns every
// bucket to a fixed epoch, so two requests over overlapping windows agree
// bucket for bucket.
//
// Buckets with no trades are ABSENT rather than filled forward. §14 asks for an
// "honest empty/limited-state UI" when there is not enough history, and a
// flat-filled candle invents a trade that did not happen.
func (s *Service) Candles(ctx context.Context, q db.Querier, r CandleRequest) ([]Candle, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	d, _ := r.Interval.Duration()
	rows, err := q.Query(ctx, candlesQuery, r.MarketID,
		fmt.Sprintf("%d seconds", int64(d/time.Second)), r.From.UTC(), r.To.UTC(), candleEpoch)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	out := make([]Candle, 0, 16)
	for rows.Next() {
		var (
			c                                Candle
			open, high, low, clo, cvol, avol string
		)
		if err := rows.Scan(&c.OpenTime, &open, &high, &low, &clo, &cvol, &avol, &c.Trades); err != nil {
			return nil, mapError(err)
		}
		for dst, src := range map[*money.Quantity]string{
			&c.Open: open, &c.High: high, &c.Low: low, &c.Close: clo,
			&c.CreditVolume: cvol, &c.AssetVolume: avol,
		} {
			v, perr := money.ParseQuantity(src)
			if perr != nil {
				return nil, errs.Wrap(perr, errs.CodeInternal, "nativemarket: a candle field is not an integer")
			}
			*dst = v
		}
		c.OpenTime = c.OpenTime.UTC()
		out = append(out, c)
	}
	return out, mapError(rows.Err())
}

// candlesQuery is the whole computation. Open is the pre-trade spot of the
// bucket's first print, close the post-trade spot of its last, and the extremes
// look at both sides of every print so the range is the range the market
// actually traversed.
const candlesQuery = `
SELECT b.bucket,
       (array_agg(b.spot_price_before ORDER BY b.printed_at, b.seq))[1]::text                AS open_price,
       GREATEST(max(b.spot_price_before), max(b.spot_price_after))::text                     AS high_price,
       LEAST(min(b.spot_price_before), min(b.spot_price_after))::text                        AS low_price,
       (array_agg(b.spot_price_after  ORDER BY b.printed_at DESC, b.seq DESC))[1]::text      AS close_price,
       sum(b.credit_volume)::text                                                            AS credit_volume,
       sum(b.asset_volume)::text                                                             AS asset_volume,
       count(*)                                                                              AS trades
  FROM (
    SELECT date_bin($2::interval, printed_at, $5::timestamptz) AS bucket,
           printed_at, seq, spot_price_before, spot_price_after, credit_volume, asset_volume
      FROM native_market_prints
     WHERE market_id = $1 AND printed_at >= $3 AND printed_at < $4
  ) b
 GROUP BY b.bucket
 ORDER BY b.bucket`

// Print is one public trade print. It carries no account identity: a tape is a
// record of what the market did, and who did it is not the public's business
// (§16's "never expose ... internal sensitive evidence" applied to other
// people's positions).
type Print struct {
	FillID         FillID
	MarketID       MarketID
	Seq            int64
	Side           Side
	PriceScale     int
	SpotBefore     money.Quantity
	SpotAfter      money.Quantity
	EffectivePrice money.Quantity
	CreditVolume   money.Quantity
	AssetVolume    money.Quantity
	PrintedAt      time.Time
}

// MaxPrints bounds one tape request.
const MaxPrints = 200

// RecentPrints returns a market's most recent trades, newest first.
func (s *Service) RecentPrints(ctx context.Context, q db.Querier, marketID MarketID, limit int) ([]Print, error) {
	if limit <= 0 || limit > MaxPrints {
		limit = 50
	}
	rows, err := q.Query(ctx,
		`SELECT fill_id, market_id, seq, side, price_scale,
		        spot_price_before::text, spot_price_after::text, effective_price::text,
		        credit_volume::text, asset_volume::text, printed_at
		   FROM native_market_prints
		  WHERE market_id = $1
		  ORDER BY printed_at DESC, seq DESC
		  LIMIT $2`, marketID, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	out := make([]Print, 0, limit)
	for rows.Next() {
		var (
			p                              Print
			side                           string
			before, after, eff, cvol, avol string
		)
		if err := rows.Scan(&p.FillID, &p.MarketID, &p.Seq, &side, &p.PriceScale,
			&before, &after, &eff, &cvol, &avol, &p.PrintedAt); err != nil {
			return nil, mapError(err)
		}
		p.Side = Side(side)
		for dst, src := range map[*money.Quantity]string{
			&p.SpotBefore: before, &p.SpotAfter: after, &p.EffectivePrice: eff,
			&p.CreditVolume: cvol, &p.AssetVolume: avol,
		} {
			v, perr := money.ParseQuantity(src)
			if perr != nil {
				return nil, errs.Wrap(perr, errs.CodeInternal, "nativemarket: a print field is not an integer")
			}
			*dst = v
		}
		p.PrintedAt = p.PrintedAt.UTC()
		out = append(out, p)
	}
	return out, mapError(rows.Err())
}
