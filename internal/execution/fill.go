package execution

import (
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// FillSource says who reported the fill.
type FillSource string

// Fill sources (fills.source).
const (
	FillFromProvider       FillSource = "PROVIDER"
	FillFromChainObserver  FillSource = "CHAIN_OBSERVER"
	FillFromReconciliation FillSource = "RECONCILIATION"
)

// Valid reports whether s is declared.
func (s FillSource) Valid() bool {
	return s == FillFromProvider || s == FillFromChainObserver || s == FillFromReconciliation
}

// Fill mirrors one fills row (migration 00300). Economic fields are
// immutable once inserted; JournalTransactionID and PositionAppliedAt are
// set exactly once by MarkFillPosted / MarkPositionApplied.
//
// Existing is not persisted: RecordFill sets it when the (venue,
// external_fill_id) pair was already stored and the returned Fill is the
// stored one.
type Fill struct {
	ID                     FillID
	OrderID                OrderID
	AttemptID              AttemptID // zero when unknown (reconciliation-discovered)
	AccountID              accounts.AccountID
	Venue                  string
	ExternalFillID         string
	TxSignature            string
	Slot                   *int64
	InputAssetID           assets.AssetID
	InputQuantity          money.Quantity
	OutputAssetID          assets.AssetID
	OutputQuantity         money.Quantity
	NetworkFeeQuantity     money.Quantity
	NetworkFeeAssetID      assets.AssetID
	VenueFeeQuantity       money.Quantity
	VenueFeeAssetID        assets.AssetID
	PlatformFeeQuantity    money.Quantity
	PlatformFeeAssetID     assets.AssetID
	EffectivePriceMantissa money.Quantity
	EffectivePriceScale    int32
	Source                 FillSource
	Finality               FinalityLevel
	ObservedAt             time.Time
	ReceivedAt             time.Time
	RawRef                 string
	JournalTransactionID   string // uuid text; "" until posted
	PositionAppliedAt      *time.Time
	CreatedAt              time.Time

	Existing bool
}

// Validate checks the structural rules of a new fill.
func (f Fill) Validate() error {
	problems := map[string]any{}
	if f.ID.IsZero() {
		problems["id"] = "required"
	}
	if f.OrderID.IsZero() {
		problems["order_id"] = "required"
	}
	if f.AccountID.IsZero() {
		problems["account_id"] = "required"
	}
	if f.Venue == "" {
		problems["venue"] = "required"
	}
	if f.ExternalFillID == "" {
		problems["external_fill_id"] = "required"
	}
	if f.InputAssetID.IsZero() || f.OutputAssetID.IsZero() {
		problems["assets"] = "input and output assets are required"
	}
	if !f.InputQuantity.IsPositive() {
		problems["input_quantity"] = "must be positive"
	}
	if f.OutputQuantity.IsNegative() {
		problems["output_quantity"] = "must not be negative"
	}
	if f.NetworkFeeQuantity.IsNegative() || f.VenueFeeQuantity.IsNegative() || f.PlatformFeeQuantity.IsNegative() {
		problems["fees"] = "must not be negative"
	}
	if f.NetworkFeeQuantity.IsPositive() && f.NetworkFeeAssetID.IsZero() {
		problems["network_fee_asset_id"] = "required when a network fee is present"
	}
	if f.VenueFeeQuantity.IsPositive() && f.VenueFeeAssetID.IsZero() {
		problems["venue_fee_asset_id"] = "required when a venue fee is present"
	}
	if f.PlatformFeeQuantity.IsPositive() && f.PlatformFeeAssetID.IsZero() {
		problems["platform_fee_asset_id"] = "required when a platform fee is present"
	}
	if f.EffectivePriceMantissa.IsNegative() || f.EffectivePriceScale < 0 || f.EffectivePriceScale > money.MaxPriceScale {
		problems["effective_price"] = "mantissa must not be negative and scale must be within [0, 38]"
	}
	if !f.Source.Valid() {
		problems["source"] = "unknown source"
	}
	if !f.Finality.Valid() || f.Finality == FinalitySubmitted {
		problems["finality"] = "must be OBSERVED, CONFIRMED or FINALIZED"
	}
	if f.ObservedAt.IsZero() {
		problems["observed_at"] = "required"
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "execution: invalid fill").WithFields(problems)
	}
	return nil
}

// Posted reports whether the fill has been posted to the ledger.
func (f Fill) Posted() bool { return f.JournalTransactionID != "" }

// PositionApplied reports whether the position engine has applied the fill.
func (f Fill) PositionApplied() bool { return f.PositionAppliedAt != nil }

// EffectivePriceScale is the scale used when the price is derived from fill
// quantities: twelve fractional digits of the quote asset per whole base
// unit.
const EffectivePriceScale int32 = 12

// EffectivePrice derives the price of one whole base unit in quote units
// from the two legs of a fill, as a mantissa at EffectivePriceScale:
//
//	price = (quoteQty / 10^quoteDecimals) / (baseQty / 10^baseDecimals)
//	mantissa = quoteQty × 10^(baseDecimals + scale) / (baseQty × 10^quoteDecimals)   (RoundHalfEven)
//
// A zero base quantity yields a zero price rather than an error so a fill
// with no output (a failed leg) can still be recorded.
func EffectivePrice(baseQty money.Quantity, baseDecimals uint8, quoteQty money.Quantity, quoteDecimals uint8) (money.Quantity, int32, error) {
	if baseQty.IsZero() {
		return money.Quantity{}, EffectivePriceScale, nil
	}
	num := quoteQty.ScaleUp(baseDecimals).ScaleUp(uint8(EffectivePriceScale))
	den := baseQty.ScaleUp(quoteDecimals)
	m, err := num.Div(den, money.RoundHalfEven)
	if err != nil {
		return money.Quantity{}, 0, errs.Wrap(err, errs.CodePrecisionLoss, "execution: effective price")
	}
	return m, EffectivePriceScale, nil
}
