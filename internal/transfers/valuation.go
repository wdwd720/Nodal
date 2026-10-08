package transfers

import (
	"context"
	"time"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuation"
)

// Valuation methods.
const (
	// ValuationPar values a SIMULATED asset that the asset registry declares
	// a USD-pegged stable unit (a paper dollar) at one dollar per whole unit.
	// Its value is its definition: nothing is observed.
	ValuationPar = "PAR"
	// ValuationMark values any other SIMULATED asset at its latest fresh
	// price in a par-valued quote asset.
	ValuationMark = "MARK"
)

// Quote is how one asset is valued in USD for limits and Risk Kernel V2. It
// is fetched once per transfer and then applied to every quantity the
// decision needs, so the notional, the required amount and the available
// amount are all valued at the same price.
type Quote struct {
	Method   string         `json:"method"`
	AssetID  assets.AssetID `json:"asset_id"`
	Decimals uint8          `json:"decimals"`
	// Price and QuoteDecimals are set for MARK.
	Price         *money.Price `json:"price,omitempty"`
	QuoteAssetID  string       `json:"quote_asset_id,omitempty"`
	QuoteDecimals uint8        `json:"quote_decimals,omitempty"`
}

// USD values qty at this quote, rounding in mode. Positive quantities only:
// a transfer never values a negative amount.
func (q Quote) USD(qty money.Quantity, mode money.RoundingMode) (money.USD, error) {
	if qty.IsNegative() {
		return money.USD{}, errs.New(errs.CodeValidationFailed, "transfers: cannot value a negative quantity")
	}
	switch q.Method {
	case ValuationPar:
		u, err := money.QuoteQuantityToUSD(qty, q.Decimals, mode)
		if err != nil {
			return money.USD{}, errs.Wrap(err, errs.CodeOverflow, "transfers: value at par")
		}
		return u, nil
	case ValuationMark:
		if q.Price == nil {
			return money.USD{}, errs.New(errs.CodeStaleMarketData, "transfers: a MARK quote without a price")
		}
		inQuote, err := money.Notional(qty, q.Decimals, *q.Price, q.QuoteDecimals, mode)
		if err != nil {
			return money.USD{}, errs.Wrap(err, errs.CodeOverflow, "transfers: value at mark")
		}
		u, err := money.QuoteQuantityToUSD(inQuote, q.QuoteDecimals, mode)
		if err != nil {
			return money.USD{}, errs.Wrap(err, errs.CodeOverflow, "transfers: value at mark")
		}
		return u, nil
	}
	return money.USD{}, errs.Newf(errs.CodeUnsupported, "transfers: unknown valuation method %q", q.Method)
}

// Valuer prices an asset for a transfer decision. It fails closed: an asset
// it cannot value now is refused, never valued at zero.
type Valuer interface {
	Quote(ctx context.Context, q db.Querier, asset assets.Asset, now time.Time) (Quote, error)
}

// ParValued reports whether the registry declares a SIMULATED asset to be a
// USD-pegged stable unit, the same declaration the buying-power engine and
// valuation read for real stablecoins.
func ParValued(a assets.Asset) bool {
	return a.Kind == assets.KindSimulated && a.IsStablecoin && a.PegCurrency == valuation.USDPeg
}

// RegistryValuer is the default Valuer: par for ParValued assets, otherwise
// the latest mark younger than MaxMarkAge quoted in QuoteAssetID (itself a
// ParValued asset). With no price source configured only par assets can be
// valued, and anything else is refused.
type RegistryValuer struct {
	Prices       valuation.PriceSource
	Assets       *assets.Repository
	QuoteAssetID assets.AssetID
	MaxMarkAge   time.Duration
}

// Quote implements Valuer.
func (v RegistryValuer) Quote(ctx context.Context, q db.Querier, asset assets.Asset, now time.Time) (Quote, error) {
	if asset.Kind != assets.KindSimulated {
		return Quote{}, errs.New(errs.CodeForbidden, "transfers value only SIMULATED assets").
			WithField("asset_id", asset.ID.String())
	}
	if ParValued(asset) {
		return Quote{Method: ValuationPar, AssetID: asset.ID, Decimals: asset.Decimals}, nil
	}
	if v.Prices == nil || v.Assets == nil || v.QuoteAssetID.IsZero() || v.MaxMarkAge <= 0 {
		return Quote{}, errs.New(errs.CodeStaleMarketData,
			"no valuation is configured for this asset; a transfer is not valued at zero").
			WithField("asset_id", asset.ID.String())
	}
	quoteAsset, err := v.Assets.Get(ctx, q, v.QuoteAssetID)
	if err != nil {
		return Quote{}, err
	}
	if !ParValued(quoteAsset) {
		return Quote{}, errs.New(errs.CodeStaleMarketData, "the configured quote asset is not a par-valued simulated dollar").
			WithField("quote_asset_id", quoteAsset.ID.String())
	}
	price, err := v.Prices.Latest(ctx, q, asset.ID, quoteAsset.ID, v.MaxMarkAge, now)
	if err != nil {
		if ee, ok := errs.As(err); ok {
			return Quote{}, ee
		}
		return Quote{}, errs.Wrap(err, errs.CodeStaleMarketData, "transfers: latest mark")
	}
	return Quote{
		Method: ValuationMark, AssetID: asset.ID, Decimals: asset.Decimals, Price: &price,
		QuoteAssetID: quoteAsset.ID.String(), QuoteDecimals: quoteAsset.Decimals,
	}, nil
}
