package fees

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// ScheduleKind is the domain-separation tag written into a schedule's
// canonical form, so that a schedule hash can never collide with the hash of
// any other record this package produces.
const ScheduleKind = "fees.schedule.v1"

// Schedule limits.
const (
	// MaxSpreadBPS is the largest spread a schedule may state: one basis
	// point short of 100%, so that a SELL can never execute at a zero or
	// negative price. Whether a large spread is commercially sensible is the
	// schedule author's decision, and every quote discloses it.
	MaxSpreadBPS = money.OneHundredPercent - 1
	// MaxTiers bounds the volume ladder.
	MaxTiers = 32
)

// Component names one disclosed part of a quote. The names are part of the
// disclosure format and never change meaning.
type Component string

// Components, in disclosure order.
const (
	// ComponentReferenceNotional is quantity × reference price: what the
	// asset is worth before the platform charges anything.
	ComponentReferenceNotional Component = "REFERENCE_NOTIONAL"
	// ComponentSpread is the explicit markup on the reference price.
	ComponentSpread Component = "SPREAD"
	// ComponentFixedFee is the flat per-order fee.
	ComponentFixedFee Component = "FIXED_FEE"
	// ComponentPercentageFee is the platform's basis-point fee after the
	// volume tier and any subscription discount.
	ComponentPercentageFee Component = "PERCENTAGE_FEE"
	// ComponentVenuePassThrough recovers the venue's fee at a stated rate.
	ComponentVenuePassThrough Component = "VENUE_PASS_THROUGH"
	// ComponentStrategyFee applies only to strategy-originated orders.
	ComponentStrategyFee Component = "STRATEGY_FEE"
	// ComponentMinimumFee is the top-up that raises the explicit fees to the
	// schedule's minimum.
	ComponentMinimumFee Component = "MINIMUM_FEE"
	// ComponentFeeCap is the basis-point cap on the explicit fees.
	ComponentFeeCap Component = "FEE_CAP"
	// ComponentNetworkFee is the network cost estimate, possibly in another
	// asset.
	ComponentNetworkFee Component = "NETWORK_FEE"
	// ComponentSlippageReserve is held against adverse execution, never
	// charged.
	ComponentSlippageReserve Component = "SLIPPAGE_RESERVE"
	// ComponentEffectivePrice is the all-in price per unit, for display.
	ComponentEffectivePrice Component = "EFFECTIVE_PRICE"
)

// NetworkFeeEstimate is the expected network cost of executing an order, in
// base units of Asset. Asset may differ from the schedule's quote asset (a
// Solana fee is paid in SOL whatever the order is quoted in); such a cost is
// disclosed in its own asset and is never converted or folded into a
// quote-asset total.
type NetworkFeeEstimate struct {
	Asset  assets.AssetID
	Amount money.Quantity
}

// VolumeTier re-prices orders for customers whose trailing 30-day notional
// volume reaches MinTrailingVolume (inclusive). A tier overrides the base
// schedule's percentage fee, its spread, or both; whatever it does not
// override is the BASE schedule's value, never a lower tier's, so the rates
// an order pays depend only on the base schedule and the one tier selected.
//
// A field a tier does not override must be left at zero, so that two
// schedules meaning the same thing always hash identically.
type VolumeTier struct {
	MinTrailingVolume      money.USD
	OverridesPercentageFee bool
	PercentageFeeBPS       money.BPS
	OverridesSpread        bool
	SpreadBPS              money.BPS
}

// RoundingPolicy names the rounding mode of every component whose exact
// value can be fractional. Each mode applies to the magnitude of the amount.
//
// The defaults (DefaultRounding) round every amount in the CUSTOMER's
// favour: amounts the customer pays (the notional of a BUY, the spread and
// every basis-point fee, and the fee cap) round down, and the amount a
// customer receives (the notional of a SELL) rounds up. A mode that can round
// against the customer is refused by Validate unless AllowPlatformFavourable
// is set, and every disclosure states, per component, the mode used and
// whether it can favour the platform.
//
// The fixed fee, the minimum fee, the absolute cap and the network estimate
// are integers already and are never rounded. The slippage reserve is held,
// not charged, so no mode for it can favour the platform; its default rounds
// up so that the hold always covers the full tolerance.
type RoundingPolicy struct {
	BuyNotional      money.RoundingMode
	SellNotional     money.RoundingMode
	Spread           money.RoundingMode
	PercentageFee    money.RoundingMode
	VenuePassThrough money.RoundingMode
	StrategyFee      money.RoundingMode
	FeeCap           money.RoundingMode
	SlippageReserve  money.RoundingMode
	// AllowPlatformFavourable must be true for any mode that can round an
	// amount against the customer to be accepted. It is part of the
	// schedule hash and of every disclosure.
	AllowPlatformFavourable bool
}

// DefaultRounding returns the customer-favourable policy described on
// RoundingPolicy.
func DefaultRounding() RoundingPolicy {
	return RoundingPolicy{
		BuyNotional:      money.RoundDown,
		SellNotional:     money.RoundUp,
		Spread:           money.RoundDown,
		PercentageFee:    money.RoundDown,
		VenuePassThrough: money.RoundDown,
		StrategyFee:      money.RoundDown,
		FeeCap:           money.RoundDown,
		SlippageReserve:  money.RoundUp,
	}
}

// roundingRole says which way an amount flows, which decides what "in the
// customer's favour" means for it.
type roundingRole int

const (
	roleCharge roundingRole = iota + 1 // the customer pays it; rounding down favours the customer
	roleCredit                         // the customer receives it; rounding up favours the customer
	roleHold                           // reserved, never charged; no rounding favours anyone
)

// favoursPlatform reports whether mode can leave the customer worse off than
// the exact amount for a non-negative amount playing role.
func favoursPlatform(role roundingRole, mode money.RoundingMode) bool {
	switch role {
	case roleCharge:
		return mode != money.RoundDown && mode != money.RoundFloor && mode != money.RoundExact
	case roleCredit:
		return mode != money.RoundUp && mode != money.RoundCeil && mode != money.RoundExact
	default:
		return false
	}
}

// policyEntry is one rounding-policy slot.
type policyEntry struct {
	key  string
	mode money.RoundingMode
	role roundingRole
}

// entries lists every slot in a fixed order.
func (r RoundingPolicy) entries() []policyEntry {
	return []policyEntry{
		{"buy_notional", r.BuyNotional, roleCharge},
		{"sell_notional", r.SellNotional, roleCredit},
		{"spread", r.Spread, roleCharge},
		{"percentage_fee", r.PercentageFee, roleCharge},
		{"venue_pass_through", r.VenuePassThrough, roleCharge},
		{"strategy_fee", r.StrategyFee, roleCharge},
		{"fee_cap", r.FeeCap, roleCharge},
		{"slippage_reserve", r.SlippageReserve, roleHold},
	}
}

// notionalMode returns the mode and role of the reference notional for side.
func (r RoundingPolicy) notionalMode(side Side) (money.RoundingMode, roundingRole) {
	if side == SideSell {
		return r.SellNotional, roleCredit
	}
	return r.BuyNotional, roleCharge
}

// validate reports every unusable or undisclosed-favour mode.
func (r RoundingPolicy) validate(f fieldErrors) {
	for _, e := range r.entries() {
		key := "rounding." + e.key
		switch {
		case !e.mode.Valid():
			f.add(key, "a rounding mode is required")
		case e.mode == money.RoundExact:
			f.add(key, "must be a concrete rounding mode, not exact")
		case favoursPlatform(e.role, e.mode) && !r.AllowPlatformFavourable:
			f.addf(key, "%s can round in the platform's favour; set allow_platform_favourable to adopt it explicitly", e.mode)
		}
	}
}

// Schedule is a versioned, hashable spread and fee schedule for orders
// quoted in one asset.
//
// Every charge it can produce is a separate, named, disclosed component:
// there is no hidden spread, and there is no charge that is not a field
// here. Absolute amounts (FixedFee, MinFee, MaxTotalFee) are base units of
// QuoteAsset with QuoteDecimals decimals; a quote whose input disagrees about
// either is refused rather than priced in the wrong unit.
//
// Basis-point components all apply to the exact reference notional
// (quantity × reference price), never to an amount that already includes
// another charge, so no charge is ever levied on another charge.
type Schedule struct {
	// Version is recorded on every quote and compared by exact equality.
	Version       string
	QuoteAsset    assets.AssetID
	QuoteDecimals uint8

	// SpreadBPS is the explicit markup on the reference price: a BUY
	// executes at ref × (1 + s), a SELL at ref × (1 − s).
	SpreadBPS money.BPS
	// FixedFee is charged once per order.
	FixedFee money.Quantity
	// PercentageFeeBPS is the platform fee rate before tiers and discounts.
	PercentageFeeBPS money.BPS
	// VenuePassThroughBPS recovers the venue's fee.
	VenuePassThroughBPS money.BPS
	// NetworkFee is the network cost estimate.
	NetworkFee NetworkFeeEstimate
	// SlippageReserveBPS is HELD against adverse execution and released if
	// unused; it is never part of what a quote charges.
	SlippageReserveBPS money.BPS
	// SubscriptionDiscountBPS is the share of the percentage fee waived for
	// subscribers: 2_500 takes a quarter off, 10_000 waives it. It reduces
	// the platform percentage fee only, never the spread or any other fee.
	SubscriptionDiscountBPS money.BPS
	// Tiers is ordered by strictly ascending MinTrailingVolume.
	Tiers []VolumeTier
	// StrategyFeeBPS applies only to strategy-originated orders.
	StrategyFeeBPS money.BPS

	// MaxTotalFeeBPS caps the explicit fees at this share of the reference
	// notional; zero disables the cap.
	MaxTotalFeeBPS money.BPS
	// MaxTotalFee caps the explicit fees absolutely; zero disables the cap.
	MaxTotalFee money.Quantity
	// MinFee raises the explicit fees to at least this amount; zero
	// disables the minimum. A cap always wins over the minimum.
	MinFee money.Quantity

	Rounding RoundingPolicy
}

// ZeroSchedule returns a schedule that charges nothing: no spread, no fees,
// no network estimate and no reserve, with the customer-favourable rounding
// defaults. As with Zero, the version string makes "no charge" an explicit,
// auditable choice rather than an absence.
func ZeroSchedule(quoteAsset assets.AssetID, quoteDecimals uint8) Schedule {
	return Schedule{
		Version:       "fee-schedule-zero-v1",
		QuoteAsset:    quoteAsset,
		QuoteDecimals: quoteDecimals,
		Rounding:      DefaultRounding(),
	}
}

// Validate checks the schedule. It returns nil or a VALIDATION_FAILED
// *errs.Error whose Fields name every offending field.
func (s Schedule) Validate() error {
	f := fieldErrors{}
	checkLabel(f, "version", s.Version)
	if s.QuoteAsset.IsZero() {
		f.add("quote_asset", "required")
	}
	checkBPS(f, "spread_bps", s.SpreadBPS, 0, MaxSpreadBPS)
	if s.FixedFee.IsNegative() {
		f.add("fixed_fee", "must not be negative")
	}
	checkBPS(f, "percentage_fee_bps", s.PercentageFeeBPS, 0, money.OneHundredPercent)
	checkBPS(f, "venue_pass_through_bps", s.VenuePassThroughBPS, 0, money.OneHundredPercent)
	switch {
	case s.NetworkFee.Amount.IsNegative():
		f.add("network_fee.amount", "must not be negative")
	case s.NetworkFee.Amount.IsPositive() && s.NetworkFee.Asset.IsZero():
		f.add("network_fee.asset", "required when the estimate is positive")
	}
	checkBPS(f, "slippage_reserve_bps", s.SlippageReserveBPS, 0, money.OneHundredPercent)
	checkBPS(f, "subscription_discount_bps", s.SubscriptionDiscountBPS, 0, money.OneHundredPercent)
	checkBPS(f, "strategy_fee_bps", s.StrategyFeeBPS, 0, money.OneHundredPercent)
	checkBPS(f, "max_total_fee_bps", s.MaxTotalFeeBPS, 0, money.OneHundredPercent)
	if s.MaxTotalFee.IsNegative() {
		f.add("max_total_fee", "must not be negative")
	}
	switch {
	case s.MinFee.IsNegative():
		f.add("min_fee", "must not be negative")
	case s.MaxTotalFee.IsPositive() && s.MinFee.Cmp(s.MaxTotalFee) > 0:
		f.add("min_fee", "must not exceed max_total_fee")
	}
	s.validateTiers(f)
	s.Rounding.validate(f)
	return f.err("fees: invalid schedule")
}

func (s Schedule) validateTiers(f fieldErrors) {
	if len(s.Tiers) > MaxTiers {
		f.addf("tiers", "at most %d tiers", MaxTiers)
		return
	}
	for i, t := range s.Tiers {
		key := "tiers[" + strconv.Itoa(i) + "]"
		switch {
		case !t.MinTrailingVolume.IsPositive():
			f.add(key+".min_trailing_volume", "must be positive; the base schedule is the zero-volume tier")
		case i > 0 && t.MinTrailingVolume.Cmp(s.Tiers[i-1].MinTrailingVolume) <= 0:
			f.add(key+".min_trailing_volume", "tiers must be in strictly ascending volume order")
		}
		if !t.OverridesPercentageFee && !t.OverridesSpread {
			f.add(key, "a tier must override the percentage fee, the spread, or both")
		}
		if t.OverridesPercentageFee {
			checkBPS(f, key+".percentage_fee_bps", t.PercentageFeeBPS, 0, money.OneHundredPercent)
		} else if t.PercentageFeeBPS != 0 {
			f.add(key+".percentage_fee_bps", "must be zero unless the tier overrides the percentage fee")
		}
		if t.OverridesSpread {
			checkBPS(f, key+".spread_bps", t.SpreadBPS, 0, MaxSpreadBPS)
		} else if t.SpreadBPS != 0 {
			f.add(key+".spread_bps", "must be zero unless the tier overrides the spread")
		}
	}
}

// AppliedTier records which volume tier priced an order and the rates it
// resolved to. Index is -1 when the trailing volume is below every tier and
// the base schedule applies.
type AppliedTier struct {
	Index             int
	MinTrailingVolume money.USD
	TrailingVolume    money.USD
	PercentageFeeBPS  money.BPS
	SpreadBPS         money.BPS
}

// TierFor returns the tier that applies to a customer whose trailing 30-day
// notional volume is trailingVolume: the last tier whose MinTrailingVolume
// is at most the volume, or the base schedule. The volume is computed by the
// caller; this package reads no clock and no history.
func (s Schedule) TierFor(trailingVolume money.USD) (AppliedTier, error) {
	if err := s.Validate(); err != nil {
		return AppliedTier{}, err
	}
	if trailingVolume.IsNegative() {
		return AppliedTier{}, errs.New(errs.CodeValidationFailed, "fees: invalid trailing volume").
			WithField("trailing_volume", "must not be negative")
	}
	return s.resolveTier(trailingVolume), nil
}

// resolveTier is TierFor without validation.
func (s Schedule) resolveTier(volume money.USD) AppliedTier {
	t := AppliedTier{Index: -1, TrailingVolume: volume, PercentageFeeBPS: s.PercentageFeeBPS, SpreadBPS: s.SpreadBPS}
	for i, tier := range s.Tiers {
		if volume.Cmp(tier.MinTrailingVolume) < 0 {
			break
		}
		t.Index = i
		t.MinTrailingVolume = tier.MinTrailingVolume
		t.PercentageFeeBPS, t.SpreadBPS = s.PercentageFeeBPS, s.SpreadBPS
		if tier.OverridesPercentageFee {
			t.PercentageFeeBPS = tier.PercentageFeeBPS
		}
		if tier.OverridesSpread {
			t.SpreadBPS = tier.SpreadBPS
		}
	}
	return t
}

// canonicalSchedule is the hashed form: every field in a fixed order, amounts
// as decimal strings, rounding modes by name rather than by their integer
// value so that reordering money's constants cannot silently change a hash,
// and tiers as an always-present array.
type canonicalSchedule struct {
	Kind                    string            `json:"kind"`
	Version                 string            `json:"version"`
	QuoteAsset              assets.AssetID    `json:"quote_asset"`
	QuoteDecimals           uint8             `json:"quote_decimals"`
	SpreadBPS               money.BPS         `json:"spread_bps"`
	FixedFee                money.Quantity    `json:"fixed_fee"`
	PercentageFeeBPS        money.BPS         `json:"percentage_fee_bps"`
	VenuePassThroughBPS     money.BPS         `json:"venue_pass_through_bps"`
	NetworkFeeAsset         assets.AssetID    `json:"network_fee_asset"`
	NetworkFeeAmount        money.Quantity    `json:"network_fee_amount"`
	SlippageReserveBPS      money.BPS         `json:"slippage_reserve_bps"`
	SubscriptionDiscountBPS money.BPS         `json:"subscription_discount_bps"`
	Tiers                   []canonicalTier   `json:"tiers"`
	StrategyFeeBPS          money.BPS         `json:"strategy_fee_bps"`
	MaxTotalFeeBPS          money.BPS         `json:"max_total_fee_bps"`
	MaxTotalFee             money.Quantity    `json:"max_total_fee"`
	MinFee                  money.Quantity    `json:"min_fee"`
	Rounding                canonicalRounding `json:"rounding"`
}

type canonicalTier struct {
	MinTrailingVolume      money.USD `json:"min_trailing_volume_usd"`
	OverridesPercentageFee bool      `json:"overrides_percentage_fee"`
	PercentageFeeBPS       money.BPS `json:"percentage_fee_bps"`
	OverridesSpread        bool      `json:"overrides_spread"`
	SpreadBPS              money.BPS `json:"spread_bps"`
}

type canonicalRounding struct {
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

// Canonical returns the deterministic serialised form of the schedule.
func (s Schedule) Canonical() ([]byte, error) {
	tiers := make([]canonicalTier, 0, len(s.Tiers))
	for _, t := range s.Tiers {
		tiers = append(tiers, canonicalTier(t))
	}
	r := s.Rounding
	b, err := json.Marshal(canonicalSchedule{
		Kind:                    ScheduleKind,
		Version:                 s.Version,
		QuoteAsset:              s.QuoteAsset,
		QuoteDecimals:           s.QuoteDecimals,
		SpreadBPS:               s.SpreadBPS,
		FixedFee:                s.FixedFee,
		PercentageFeeBPS:        s.PercentageFeeBPS,
		VenuePassThroughBPS:     s.VenuePassThroughBPS,
		NetworkFeeAsset:         s.NetworkFee.Asset,
		NetworkFeeAmount:        s.NetworkFee.Amount,
		SlippageReserveBPS:      s.SlippageReserveBPS,
		SubscriptionDiscountBPS: s.SubscriptionDiscountBPS,
		Tiers:                   tiers,
		StrategyFeeBPS:          s.StrategyFeeBPS,
		MaxTotalFeeBPS:          s.MaxTotalFeeBPS,
		MaxTotalFee:             s.MaxTotalFee,
		MinFee:                  s.MinFee,
		Rounding: canonicalRounding{
			BuyNotional:             r.BuyNotional.String(),
			SellNotional:            r.SellNotional.String(),
			Spread:                  r.Spread.String(),
			PercentageFee:           r.PercentageFee.String(),
			VenuePassThrough:        r.VenuePassThrough.String(),
			StrategyFee:             r.StrategyFee.String(),
			FeeCap:                  r.FeeCap.String(),
			SlippageReserve:         r.SlippageReserve.String(),
			AllowPlatformFavourable: r.AllowPlatformFavourable,
		},
	})
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "fees: canonicalise schedule")
	}
	return b, nil
}

// Hash is the hex sha256 of the canonical form. It is recorded on every
// quote so that a stored schedule version can be proven to name the rules
// that were actually applied.
func (s Schedule) Hash() (string, error) {
	b, err := s.Canonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
