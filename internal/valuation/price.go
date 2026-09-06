package valuation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
)

type priceKind struct{}

// PriceID identifies one price observation.
type PriceID = id.ID[priceKind]

// NewPriceID returns a fresh price observation identifier.
func NewPriceID() PriceID { return id.New[priceKind]() }

// PriceSource is the fixed read contract (FINANCIAL_MODEL §7). Latest
// returns STALE_MARKET_DATA when no observation younger than maxAge exists.
type PriceSource interface {
	Latest(ctx context.Context, q db.Querier, assetID, quoteAssetID assets.AssetID, maxAge time.Duration, now time.Time) (money.Price, error)
}

// PriceObservation is the input to RecordPrice. The observation is
// identified by (asset, quote, source, observed_at); recording the same
// identity twice is a no-op.
type PriceObservation struct {
	AssetID      assets.AssetID
	QuoteAssetID assets.AssetID
	Mantissa     money.Quantity
	Scale        int32
	Source       string
	ObservedAt   time.Time // provider/event time
	ReceivedAt   time.Time // knowledge time; zero means the store's clock
	RawRef       string
}

// Validate checks the observation before it is stored.
func (o PriceObservation) Validate() error {
	var problems []string
	if o.AssetID.IsZero() {
		problems = append(problems, "asset_id required")
	}
	if o.QuoteAssetID.IsZero() {
		problems = append(problems, "quote_asset_id required")
	}
	if o.Mantissa.IsNegative() {
		problems = append(problems, "mantissa must not be negative")
	}
	if o.Scale < 0 || o.Scale > money.MaxPriceScale {
		problems = append(problems, fmt.Sprintf("scale must be within [0, %d]", money.MaxPriceScale))
	}
	if strings.TrimSpace(o.Source) == "" {
		problems = append(problems, "source required")
	}
	if o.ObservedAt.IsZero() {
		problems = append(problems, "observed_at required")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "invalid price observation").WithField("problems", problems)
	}
	return nil
}

// RecordedPrice is the result of RecordPrice.
type RecordedPrice struct {
	ID        PriceID
	Duplicate bool // an identical observation was already stored
}

// PriceStore reads and appends asset_prices.
type PriceStore struct {
	clk clock.Clock
}

// NewPriceStore returns a PriceStore that stamps received_at from clk.
func NewPriceStore(clk clock.Clock) *PriceStore {
	if clk == nil {
		clk = clock.System()
	}
	return &PriceStore{clk: clk}
}

var _ PriceSource = (*PriceStore)(nil)

// Latest returns the freshest observation for (assetID, quoteAssetID) whose
// observed_at lies in [now − maxAge, now] (age <= maxAge is fresh).
// Staleness is judged on observed_at, the event time, never on received_at:
// a price that arrived a moment ago but was observed an hour ago is stale.
// Observations after now are ignored so that an as-of valuation never
// looks ahead (and a skewed provider clock fails closed). A non-positive
// maxAge accepts nothing. Missing or too old → STALE_MARKET_DATA.
//
// The returned Price carries QuoteAsset = quoteAssetID in canonical string
// form (identity, never a symbol), Source and At = observed_at.
func (s *PriceStore) Latest(ctx context.Context, q db.Querier, assetID, quoteAssetID assets.AssetID, maxAge time.Duration, now time.Time) (money.Price, error) {
	if assetID.IsZero() || quoteAssetID.IsZero() {
		return money.Price{}, errs.New(errs.CodeValidationFailed, "asset_id and quote_asset_id required")
	}
	stale := errs.New(errs.CodeStaleMarketData, "no price observation within the maximum age").
		WithField("asset_id", assetID.String()).
		WithField("quote_asset_id", quoteAssetID.String()).
		WithField("max_age", maxAge.String())
	if maxAge <= 0 {
		return money.Price{}, stale
	}
	now = now.UTC()
	oldest := now.Add(-maxAge)
	var mantissa money.Quantity
	var scale int32
	var source string
	var observedAt time.Time
	err := q.QueryRow(ctx, `SELECT mantissa, scale, source, observed_at FROM asset_prices
		WHERE asset_id = $1 AND quote_asset_id = $2 AND observed_at >= $3 AND observed_at <= $4
		ORDER BY observed_at DESC, received_at DESC, id DESC LIMIT 1`, assetID, quoteAssetID, oldest, now).
		Scan(&mantissa, &scale, &source, &observedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return money.Price{}, stale
		}
		return money.Price{}, fmt.Errorf("valuation: latest price: %w", err)
	}
	p, err := money.NewPrice(mantissa, scale, quoteAssetID.String(), source, observedAt.UTC())
	if err != nil {
		return money.Price{}, errs.Wrap(err, errs.CodeValidationFailed, "stored price observation is invalid")
	}
	return p, nil
}

// RecordPrice stores an observation. It is idempotent on
// (asset, quote, source, observed_at): an identical replay returns the
// existing id with Duplicate = true; a replay with a different mantissa or
// scale for the same identity is a provider inconsistency and fails with
// CONFLICT rather than being silently ignored.
func (s *PriceStore) RecordPrice(ctx context.Context, q db.Querier, o PriceObservation) (RecordedPrice, error) {
	if err := o.Validate(); err != nil {
		return RecordedPrice{}, err
	}
	received := o.ReceivedAt
	if received.IsZero() {
		received = s.clk.Now()
	}
	var newID PriceID
	err := q.QueryRow(ctx, `INSERT INTO asset_prices (id, asset_id, quote_asset_id, mantissa, scale, source, observed_at, received_at, raw_ref)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''))
		ON CONFLICT (asset_id, quote_asset_id, source, observed_at) DO NOTHING
		RETURNING id`,
		NewPriceID(), o.AssetID, o.QuoteAssetID, o.Mantissa, o.Scale, o.Source, o.ObservedAt.UTC(), received.UTC(), o.RawRef).Scan(&newID)
	if err == nil {
		return RecordedPrice{ID: newID}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		if db.IsForeignKeyViolation(err) {
			return RecordedPrice{}, errs.Wrap(err, errs.CodeNotFound, "asset or quote asset not found")
		}
		if db.IsCheckViolation(err) {
			return RecordedPrice{}, errs.Wrap(err, errs.CodeValidationFailed, "price observation rejected by schema constraint")
		}
		return RecordedPrice{}, fmt.Errorf("valuation: record price: %w", err)
	}
	// Conflict: compare with the stored observation.
	var existingID PriceID
	var mantissa money.Quantity
	var scale int32
	err = q.QueryRow(ctx, `SELECT id, mantissa, scale FROM asset_prices
		WHERE asset_id = $1 AND quote_asset_id = $2 AND source = $3 AND observed_at = $4`,
		o.AssetID, o.QuoteAssetID, o.Source, o.ObservedAt.UTC()).Scan(&existingID, &mantissa, &scale)
	if err != nil {
		return RecordedPrice{}, fmt.Errorf("valuation: record price: read existing: %w", err)
	}
	if !mantissa.Equal(o.Mantissa) || scale != o.Scale {
		return RecordedPrice{}, errs.New(errs.CodeConflict, "a different price is already recorded for this observation identity").
			WithField("price_id", existingID.String())
	}
	return RecordedPrice{ID: existingID, Duplicate: true}, nil
}
