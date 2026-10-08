package orders

import (
	"bytes"
	"encoding/json"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/money"
)

// The internal book charges explicit maker and taker fees from a fees.Schedule
// and nothing else, and books each through the fee ledger (feebooking.go).
// Its price is always the counterparty's resting price, so
// the schedule may carry no spread at all, and every component it may carry
// is one that is charged per fill, in proportion to the fill, and can be
// bounded in advance -- which is what lets a buy order reserve "quantity times
// limit price plus the maximum fees" and never need more:
//
//   - allowed: a percentage fee, a strategy fee, a subscription discount and a
//     basis-point cap;
//   - refused: a spread (the internal book has no undisclosed or disclosed
//     markup: the price is the counterparty's), a fixed fee or minimum fee
//     (per order, not per fill), an absolute cap (per order), a venue
//     pass-through (there is no external venue), a network cost, a slippage
//     reserve (the price is known), volume tiers (no trailing volume is kept
//     here) and any rounding mode that can favour the platform.

// ValidateBookSchedule reports whether s may price fills on a book with these
// terms.
func ValidateBookSchedule(s fees.Schedule, t BookTerms) error {
	if err := s.Validate(); err != nil {
		return err
	}
	var problems []string
	if s.QuoteAsset != t.QuoteAsset || s.QuoteDecimals != t.QuoteDecimals {
		problems = append(problems, "the schedule must be quoted in the book's quote asset and decimals")
	}
	if s.SpreadBPS != 0 {
		problems = append(problems, "spread_bps must be 0: on the internal book the price is the counterparty's price")
	}
	if !s.FixedFee.IsZero() || !s.MinFee.IsZero() || !s.MaxTotalFee.IsZero() {
		problems = append(problems, "fixed fee, minimum fee and absolute cap are per order; the book charges per fill")
	}
	if s.VenuePassThroughBPS != 0 {
		problems = append(problems, "venue_pass_through_bps must be 0: there is no external venue")
	}
	if !s.NetworkFee.Amount.IsZero() {
		problems = append(problems, "network fee must be 0: nothing is submitted to a network")
	}
	if s.SlippageReserveBPS != 0 {
		problems = append(problems, "slippage_reserve_bps must be 0: a fill is at the resting price")
	}
	if len(s.Tiers) > 0 {
		problems = append(problems, "volume tiers are not supported on the internal book")
	}
	if s.Rounding.AllowPlatformFavourable {
		problems = append(problems, "platform-favourable rounding is not allowed on the internal book")
	}
	if len(problems) > 0 {
		return rejection(ReasonScheduleNotInternalBook, "fee schedule cannot price internal-book fills").
			WithField("problems", problems)
	}
	return nil
}

// FeeBound is the most an order of the given notional can pay in fees over
// all of its fills, whatever mix of maker and taker fills it gets:
// ceil(notional x (max percentage rate + max strategy rate) / 10_000). A buy
// fills at or below its limit, so a reservation of quantity x limit + this
// bound always covers every fill and its fee.
func FeeBound(maker, taker fees.Schedule, notional money.Quantity, strategy bool) (money.Quantity, error) {
	rate := max(maker.PercentageFeeBPS, taker.PercentageFeeBPS)
	if strategy {
		rate += max(maker.StrategyFeeBPS, taker.StrategyFeeBPS)
	}
	if rate == 0 || notional.IsZero() {
		return money.Quantity{}, nil
	}
	return notional.MulDiv(money.QuantityFromInt64(int64(rate)), money.QuantityFromInt64(int64(money.OneHundredPercent)), money.RoundUp)
}

// fillQuoteInput is what one party's share of a fill is priced as: its side,
// the fill's base quantity at the fill price (the counterparty's resting
// price) and whether its order was strategy-originated. The book computes the
// fee from it and the fee ledger discloses the same input.
func fillQuoteInput(side Side, t BookTerms, lots int64, strategy bool, price money.Price) fees.QuoteInput {
	return fees.QuoteInput{
		Side:               fees.Side(side),
		BaseQty:            t.LotsToQuantity(lots),
		BaseDecimals:       t.BaseDecimals,
		QuoteDecimals:      t.QuoteDecimals,
		ReferencePrice:     price,
		StrategyOriginated: strategy,
	}
}

// fillFee prices one fill under s and returns the explicit fee it charges.
// The fill's notional is exact by construction of the book's terms; the quote
// must agree with it, and it must carry no spread.
func fillFee(s fees.Schedule, side Side, t BookTerms, lots, ticks int64, strategy bool, price money.Price) (money.Quantity, error) {
	q, err := s.Quote(fillQuoteInput(side, t, lots, strategy, price))
	if err != nil {
		return money.Quantity{}, err
	}
	if !q.Spread.Amount.IsZero() {
		return money.Quantity{}, errs.New(errs.CodeInternal, "orders: an internal-book fill was quoted with a spread")
	}
	if !q.NotionalAtReference.Equal(t.Notional(ticks, lots)) {
		return money.Quantity{}, errs.New(errs.CodeInternal, "orders: the fee quote disagrees with the exact fill notional")
	}
	return q.Fees.Total, nil
}

// scheduleDoc mirrors the canonical form fees.Schedule.Canonical writes, so a
// stored schedule can be decoded and proven to be the one its hash names.
type scheduleDoc struct {
	Kind                    string              `json:"kind"`
	Version                 string              `json:"version"`
	QuoteAsset              assets.AssetID      `json:"quote_asset"`
	QuoteDecimals           uint8               `json:"quote_decimals"`
	SpreadBPS               money.BPS           `json:"spread_bps"`
	FixedFee                money.Quantity      `json:"fixed_fee"`
	PercentageFeeBPS        money.BPS           `json:"percentage_fee_bps"`
	VenuePassThroughBPS     money.BPS           `json:"venue_pass_through_bps"`
	NetworkFeeAsset         assets.AssetID      `json:"network_fee_asset"`
	NetworkFeeAmount        money.Quantity      `json:"network_fee_amount"`
	SlippageReserveBPS      money.BPS           `json:"slippage_reserve_bps"`
	SubscriptionDiscountBPS money.BPS           `json:"subscription_discount_bps"`
	Tiers                   []scheduleTierDoc   `json:"tiers"`
	StrategyFeeBPS          money.BPS           `json:"strategy_fee_bps"`
	MaxTotalFeeBPS          money.BPS           `json:"max_total_fee_bps"`
	MaxTotalFee             money.Quantity      `json:"max_total_fee"`
	MinFee                  money.Quantity      `json:"min_fee"`
	Rounding                scheduleRoundingDoc `json:"rounding"`
}

type scheduleTierDoc struct {
	MinTrailingVolume      money.USD `json:"min_trailing_volume_usd"`
	OverridesPercentageFee bool      `json:"overrides_percentage_fee"`
	PercentageFeeBPS       money.BPS `json:"percentage_fee_bps"`
	OverridesSpread        bool      `json:"overrides_spread"`
	SpreadBPS              money.BPS `json:"spread_bps"`
}

type scheduleRoundingDoc struct {
	BuyNotional             string `json:"buy_notional"`
	SellNotional            string `json:"sell_notional"`
	Spread                  string `json:"spread"`
	PercentageFee           string `json:"percentage_fee"`
	VenuePassThrough        string `json:"venue_pass_through"`
	StrategyFee             string `json:"strategy_fee"`
	FeeCap                  string `json:"fee_cap"`
	SlippageReserve         string `json:"slippage_reserve"`
	AllowPlatformFavourable bool   `json:"allow_platform_favourable"`
}

// DecodeSchedule rebuilds a schedule from its canonical form and proves it:
// the result must validate and re-encode to exactly the stored bytes, so the
// hash recorded with the bytes names the schedule returned.
func DecodeSchedule(canonical []byte) (fees.Schedule, error) {
	dec := json.NewDecoder(bytes.NewReader(canonical))
	dec.DisallowUnknownFields()
	var d scheduleDoc
	if err := dec.Decode(&d); err != nil {
		return fees.Schedule{}, errs.Wrap(err, errs.CodeValidationFailed, "orders: stored fee schedule is not a schedule")
	}
	if d.Kind != fees.ScheduleKind {
		return fees.Schedule{}, errs.New(errs.CodeValidationFailed, "orders: stored fee schedule has the wrong kind")
	}
	modes := make([]money.RoundingMode, 8)
	for i, name := range []string{d.Rounding.BuyNotional, d.Rounding.SellNotional, d.Rounding.Spread, d.Rounding.PercentageFee,
		d.Rounding.VenuePassThrough, d.Rounding.StrategyFee, d.Rounding.FeeCap, d.Rounding.SlippageReserve} {
		m, err := money.ParseRoundingMode(name)
		if err != nil {
			return fees.Schedule{}, errs.Wrap(err, errs.CodeValidationFailed, "orders: stored fee schedule has an unknown rounding mode")
		}
		modes[i] = m
	}
	s := fees.Schedule{
		Version:                 d.Version,
		QuoteAsset:              d.QuoteAsset,
		QuoteDecimals:           d.QuoteDecimals,
		SpreadBPS:               d.SpreadBPS,
		FixedFee:                d.FixedFee,
		PercentageFeeBPS:        d.PercentageFeeBPS,
		VenuePassThroughBPS:     d.VenuePassThroughBPS,
		NetworkFee:              fees.NetworkFeeEstimate{Asset: d.NetworkFeeAsset, Amount: d.NetworkFeeAmount},
		SlippageReserveBPS:      d.SlippageReserveBPS,
		SubscriptionDiscountBPS: d.SubscriptionDiscountBPS,
		StrategyFeeBPS:          d.StrategyFeeBPS,
		MaxTotalFeeBPS:          d.MaxTotalFeeBPS,
		MaxTotalFee:             d.MaxTotalFee,
		MinFee:                  d.MinFee,
		Rounding: fees.RoundingPolicy{
			BuyNotional: modes[0], SellNotional: modes[1], Spread: modes[2], PercentageFee: modes[3],
			VenuePassThrough: modes[4], StrategyFee: modes[5], FeeCap: modes[6], SlippageReserve: modes[7],
			AllowPlatformFavourable: d.Rounding.AllowPlatformFavourable,
		},
	}
	for _, t := range d.Tiers {
		s.Tiers = append(s.Tiers, fees.VolumeTier(t))
	}
	if err := s.Validate(); err != nil {
		return fees.Schedule{}, err
	}
	again, err := s.Canonical()
	if err != nil {
		return fees.Schedule{}, err
	}
	if !bytes.Equal(again, canonical) {
		return fees.Schedule{}, errs.New(errs.CodeValidationFailed, "orders: stored fee schedule is not in canonical form")
	}
	return s, nil
}
