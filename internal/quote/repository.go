package quote

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/money"
)

// Repository persists quotes. It holds no connection; Record runs on the
// caller's transaction so a quote commits with the plan step that acquired
// it.
type Repository struct{}

// NewRepository returns a Repository.
func NewRepository() *Repository { return &Repository{} }

const quoteColumns = `id, intent_id, provider, coalesce(provider_request_id, ''), instrument_id, venue_listing_id, side,
	input_asset_id, input_quantity::text, output_asset_id, expected_output::text, minimum_output::text,
	effective_price_mantissa::text, effective_price_scale, price_impact_bps, slippage_bps,
	est_network_cost::text, est_network_cost_asset_id, est_venue_fee::text, est_venue_fee_asset_id,
	platform_fee::text, platform_fee_asset_id, platform_fee_bps, coalesce(fee_policy_version, ''),
	received_at, expires_at, route_hash, route_summary, coalesce(raw_response_ref, ''), raw_response_hash, created_at`

const insertQuoteSQL = `
INSERT INTO quotes (
    id, intent_id, provider, provider_request_id, instrument_id, venue_listing_id, side,
    input_asset_id, input_quantity, output_asset_id, expected_output, minimum_output,
    effective_price_mantissa, effective_price_scale, price_impact_bps, slippage_bps,
    est_network_cost, est_network_cost_asset_id, est_venue_fee, est_venue_fee_asset_id,
    platform_fee, platform_fee_asset_id, platform_fee_bps, fee_policy_version,
    received_at, expires_at, route_hash, route_summary, raw_response_ref, raw_response_hash)
VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7,
        $8, $9::numeric, $10, $11::numeric, $12::numeric,
        $13::numeric, $14, $15, $16,
        $17::numeric, $18, $19::numeric, $20,
        $21::numeric, $22, $23, NULLIF($24, ''),
        $25, $26, $27, $28::jsonb, NULLIF($29, ''), $30)
RETURNING ` + quoteColumns

// Record validates q and inserts it. Quotes are insert-only: a duplicate id
// is a CONFLICT and an unknown intent, instrument, listing or asset is
// NOT_FOUND. The returned Quote is the stored row.
func (r *Repository) Record(ctx context.Context, tx pgx.Tx, q Quote) (Quote, error) {
	if tx == nil {
		return Quote{}, errs.New(errs.CodeInternal, "quote: Record requires a transaction")
	}
	if err := q.Validate(); err != nil {
		return Quote{}, err
	}
	summary, _ := normalizeRoute(q.RouteSummary)
	var intentID *intent.IntentID
	if q.IntentID != nil {
		v := *q.IntentID
		intentID = &v
	}
	stored, err := scanQuote(tx.QueryRow(ctx, insertQuoteSQL,
		q.ID, intentID, q.Provider, q.ProviderRequestID, q.InstrumentID, q.VenueListingID, string(q.Side),
		q.InputAssetID, q.InputQuantity.String(), q.OutputAssetID, q.ExpectedOutput.String(), q.MinimumOutput.String(),
		q.EffectivePrice.Mantissa.String(), q.EffectivePrice.Scale, int64(q.PriceImpactBPS), int64(q.SlippageBPS),
		q.EstNetworkCost.String(), q.EstNetworkCostAssetID, q.EstVenueFee.String(), q.EstVenueFeeAssetID,
		q.PlatformFee.String(), q.PlatformFeeAssetID, int64(q.PlatformFeeBPS), q.FeePolicyVersion,
		q.ReceivedAt.UTC(), q.ExpiresAt.UTC(), q.RouteHash, []byte(summary), q.RawResponseRef, q.RawResponseHash))
	if err != nil {
		switch {
		case db.IsUniqueViolation(err):
			return Quote{}, errs.Wrap(err, errs.CodeConflict, "quote: duplicate quote id").WithField("quote_id", q.ID.String())
		case db.IsForeignKeyViolation(err):
			return Quote{}, errs.Wrap(err, errs.CodeNotFound, "quote: referenced entity does not exist").
				WithField("constraint", db.ConstraintName(err))
		case db.IsCheckViolation(err):
			return Quote{}, errs.Wrap(err, errs.CodeValidationFailed, "quote: rejected by a table constraint").
				WithField("constraint", db.ConstraintName(err))
		}
		return Quote{}, errs.Wrap(err, errs.CodeInternal, "quote: record")
	}
	return stored, nil
}

// Get returns one quote.
func (r *Repository) Get(ctx context.Context, q db.Querier, quoteID QuoteID) (Quote, error) {
	if quoteID.IsZero() {
		return Quote{}, errs.New(errs.CodeValidationFailed, "quote: id required")
	}
	stored, err := scanQuote(q.QueryRow(ctx, `SELECT `+quoteColumns+` FROM quotes WHERE id = $1`, quoteID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Quote{}, errs.New(errs.CodeNotFound, "quote not found").WithField("quote_id", quoteID.String())
		}
		return Quote{}, errs.Wrap(err, errs.CodeInternal, "quote: get")
	}
	return stored, nil
}

// LatestForIntent returns the most recently received quote of an intent
// (ties broken by id, which is time-ordered), or NOT_FOUND.
func (r *Repository) LatestForIntent(ctx context.Context, q db.Querier, intentID intent.IntentID) (Quote, error) {
	if intentID.IsZero() {
		return Quote{}, errs.New(errs.CodeValidationFailed, "quote: intent id required")
	}
	stored, err := scanQuote(q.QueryRow(ctx, `SELECT `+quoteColumns+` FROM quotes WHERE intent_id = $1 ORDER BY received_at DESC, id DESC LIMIT 1`, intentID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Quote{}, errs.New(errs.CodeNotFound, "no quote for intent").WithField("intent_id", intentID.String())
		}
		return Quote{}, errs.Wrap(err, errs.CodeInternal, "quote: latest for intent")
	}
	return stored, nil
}

func scanQuote(row pgx.Row) (Quote, error) {
	var (
		q                                                            Quote
		intentID                                                     intent.IntentID
		side                                                         string
		input, expected, minimum, mantissa, network, venue, platform string
		scale                                                        int32
		impact, slippage, platformBPS                                int64
		networkAsset, venueAsset, platformAsset                      assets.AssetID
		summary                                                      []byte
	)
	err := row.Scan(&q.ID, &intentID, &q.Provider, &q.ProviderRequestID, &q.InstrumentID, &q.VenueListingID, &side,
		&q.InputAssetID, &input, &q.OutputAssetID, &expected, &minimum,
		&mantissa, &scale, &impact, &slippage,
		&network, &networkAsset, &venue, &venueAsset,
		&platform, &platformAsset, &platformBPS, &q.FeePolicyVersion,
		&q.ReceivedAt, &q.ExpiresAt, &q.RouteHash, &summary, &q.RawResponseRef, &q.RawResponseHash, &q.CreatedAt)
	if err != nil {
		return Quote{}, err
	}
	if !intentID.IsZero() {
		q.IntentID = &intentID
	}
	q.Side = Side(side)
	quantities := []struct {
		dst *money.Quantity
		src string
	}{
		{&q.InputQuantity, input},
		{&q.ExpectedOutput, expected},
		{&q.MinimumOutput, minimum},
		{&q.EstNetworkCost, network},
		{&q.EstVenueFee, venue},
		{&q.PlatformFee, platform},
	}
	for _, c := range quantities {
		v, err := money.ScanQuantity(c.src)
		if err != nil {
			return Quote{}, errs.Wrap(err, errs.CodeInternal, "quote: scan quantity")
		}
		*c.dst = v
	}
	m, err := money.ScanQuantity(mantissa)
	if err != nil {
		return Quote{}, errs.Wrap(err, errs.CodeInternal, "quote: scan price mantissa")
	}
	q.ReceivedAt = q.ReceivedAt.UTC()
	q.ExpiresAt = q.ExpiresAt.UTC()
	q.CreatedAt = q.CreatedAt.UTC()
	q.EffectivePrice = q.effectivePrice(m, scale)
	q.PriceImpactBPS = money.BPS(impact)
	q.SlippageBPS = money.BPS(slippage)
	q.PlatformFeeBPS = money.BPS(platformBPS)
	q.EstNetworkCostAssetID = optionalAsset(networkAsset)
	q.EstVenueFeeAssetID = optionalAsset(venueAsset)
	q.PlatformFeeAssetID = optionalAsset(platformAsset)
	q.RouteSummary = summary
	return q, nil
}

func optionalAsset(a assets.AssetID) *assets.AssetID {
	if a.IsZero() {
		return nil
	}
	return &a
}
