package equities

import (
	"bytes"
	"encoding/json"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/money"
)

// The schedule a fill was priced under is stored next to its disclosure, so
// that an auditor can re-run fees.VerifyDisclosure from the database alone.
// fees.Schedule.Canonical is a hash input, not a decodable format, so the
// stored form is this record: every field, rounding modes by name. A decoded
// record must hash to the hash the disclosure names, which is checked.

type tierRecord struct {
	MinTrailingVolume      money.USD `json:"min_trailing_volume"`
	OverridesPercentageFee bool      `json:"overrides_percentage_fee"`
	PercentageFeeBPS       money.BPS `json:"percentage_fee_bps"`
	OverridesSpread        bool      `json:"overrides_spread"`
	SpreadBPS              money.BPS `json:"spread_bps"`
}

type roundingRecord struct {
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

type scheduleRecord struct {
	Version                 string         `json:"version"`
	QuoteAsset              assets.AssetID `json:"quote_asset"`
	QuoteDecimals           uint8          `json:"quote_decimals"`
	SpreadBPS               money.BPS      `json:"spread_bps"`
	FixedFee                money.Quantity `json:"fixed_fee"`
	PercentageFeeBPS        money.BPS      `json:"percentage_fee_bps"`
	VenuePassThroughBPS     money.BPS      `json:"venue_pass_through_bps"`
	NetworkFeeAsset         assets.AssetID `json:"network_fee_asset"`
	NetworkFeeAmount        money.Quantity `json:"network_fee_amount"`
	SlippageReserveBPS      money.BPS      `json:"slippage_reserve_bps"`
	SubscriptionDiscountBPS money.BPS      `json:"subscription_discount_bps"`
	Tiers                   []tierRecord   `json:"tiers"`
	StrategyFeeBPS          money.BPS      `json:"strategy_fee_bps"`
	MaxTotalFeeBPS          money.BPS      `json:"max_total_fee_bps"`
	MaxTotalFee             money.Quantity `json:"max_total_fee"`
	MinFee                  money.Quantity `json:"min_fee"`
	Rounding                roundingRecord `json:"rounding"`
}

// EncodeSchedule renders s in the stored form.
func EncodeSchedule(s fees.Schedule) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	tiers := make([]tierRecord, 0, len(s.Tiers))
	for _, t := range s.Tiers {
		tiers = append(tiers, tierRecord(t))
	}
	r := s.Rounding
	rec := scheduleRecord{
		Version: s.Version, QuoteAsset: s.QuoteAsset, QuoteDecimals: s.QuoteDecimals, SpreadBPS: s.SpreadBPS,
		FixedFee: s.FixedFee, PercentageFeeBPS: s.PercentageFeeBPS, VenuePassThroughBPS: s.VenuePassThroughBPS,
		NetworkFeeAsset: s.NetworkFee.Asset, NetworkFeeAmount: s.NetworkFee.Amount, SlippageReserveBPS: s.SlippageReserveBPS,
		SubscriptionDiscountBPS: s.SubscriptionDiscountBPS, Tiers: tiers, StrategyFeeBPS: s.StrategyFeeBPS,
		MaxTotalFeeBPS: s.MaxTotalFeeBPS, MaxTotalFee: s.MaxTotalFee, MinFee: s.MinFee,
		Rounding: roundingRecord{
			BuyNotional: r.BuyNotional.String(), SellNotional: r.SellNotional.String(), Spread: r.Spread.String(),
			PercentageFee: r.PercentageFee.String(), VenuePassThrough: r.VenuePassThrough.String(),
			StrategyFee: r.StrategyFee.String(), FeeCap: r.FeeCap.String(), SlippageReserve: r.SlippageReserve.String(),
			AllowPlatformFavourable: r.AllowPlatformFavourable,
		},
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "equities: encode fee schedule")
	}
	return b, nil
}

// DecodeSchedule parses the stored form strictly and refuses a schedule that
// does not validate. Callers compare its Hash with the hash they expect.
func DecodeSchedule(b []byte) (fees.Schedule, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var rec scheduleRecord
	if err := dec.Decode(&rec); err != nil {
		return fees.Schedule{}, errs.Wrap(err, errs.CodeValidationFailed, "equities: stored fee schedule does not decode")
	}
	if dec.More() {
		return fees.Schedule{}, errs.New(errs.CodeValidationFailed, "equities: trailing data after the stored fee schedule")
	}
	modes := make([]money.RoundingMode, 0, 8)
	for _, name := range []string{rec.Rounding.BuyNotional, rec.Rounding.SellNotional, rec.Rounding.Spread,
		rec.Rounding.PercentageFee, rec.Rounding.VenuePassThrough, rec.Rounding.StrategyFee, rec.Rounding.FeeCap,
		rec.Rounding.SlippageReserve} {
		m, err := money.ParseRoundingMode(name)
		if err != nil {
			return fees.Schedule{}, errs.Wrap(err, errs.CodeValidationFailed, "equities: stored fee schedule names an unknown rounding mode")
		}
		modes = append(modes, m)
	}
	tiers := make([]fees.VolumeTier, 0, len(rec.Tiers))
	for _, t := range rec.Tiers {
		tiers = append(tiers, fees.VolumeTier(t))
	}
	s := fees.Schedule{
		Version: rec.Version, QuoteAsset: rec.QuoteAsset, QuoteDecimals: rec.QuoteDecimals, SpreadBPS: rec.SpreadBPS,
		FixedFee: rec.FixedFee, PercentageFeeBPS: rec.PercentageFeeBPS, VenuePassThroughBPS: rec.VenuePassThroughBPS,
		NetworkFee:         fees.NetworkFeeEstimate{Asset: rec.NetworkFeeAsset, Amount: rec.NetworkFeeAmount},
		SlippageReserveBPS: rec.SlippageReserveBPS, SubscriptionDiscountBPS: rec.SubscriptionDiscountBPS, Tiers: tiers,
		StrategyFeeBPS: rec.StrategyFeeBPS, MaxTotalFeeBPS: rec.MaxTotalFeeBPS, MaxTotalFee: rec.MaxTotalFee, MinFee: rec.MinFee,
		Rounding: fees.RoundingPolicy{
			BuyNotional: modes[0], SellNotional: modes[1], Spread: modes[2], PercentageFee: modes[3],
			VenuePassThrough: modes[4], StrategyFee: modes[5], FeeCap: modes[6], SlippageReserve: modes[7],
			AllowPlatformFavourable: rec.Rounding.AllowPlatformFavourable,
		},
	}
	if len(s.Tiers) == 0 {
		s.Tiers = nil
	}
	if err := s.Validate(); err != nil {
		return fees.Schedule{}, err
	}
	return s, nil
}

// DecodeDisclosure parses a stored fee disclosure and requires it to be in
// canonical form: re-encoding it must reproduce the stored bytes exactly, so
// the record an auditor verifies is the record that was written.
func DecodeDisclosure(b []byte) (fees.Disclosure, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var d fees.Disclosure
	if err := dec.Decode(&d); err != nil {
		return fees.Disclosure{}, errs.Wrap(err, errs.CodeValidationFailed, "equities: stored disclosure does not decode")
	}
	again, err := d.Canonical()
	if err != nil {
		return fees.Disclosure{}, err
	}
	if !bytes.Equal(again, bytes.TrimSpace(b)) {
		return fees.Disclosure{}, errs.New(errs.CodeValidationFailed, "equities: stored disclosure is not in canonical form")
	}
	return d, nil
}
