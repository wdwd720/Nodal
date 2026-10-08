package fees

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// DisclosureKind is the domain-separation tag and format version of a
// quote disclosure's canonical form.
const DisclosureKind = "fees.quote_disclosure.v1"

// Disclosure is the canonical audit record of one quote: the schedule it was
// priced under (version and hash), the complete input, every component with
// its rate, inputs and rounding mode, and the totals. It carries enough to
// recompute the quote from the schedule alone (VerifyDisclosure), which is
// what makes every unit of spread and fee discoverable from the record.
//
// Amounts are decimal strings of base units, rates are integers of basis
// points, rounding modes are their canonical names, and times are UTC. The
// canonical form is the JSON encoding of this struct: fields in declaration
// order, no maps, so equal disclosures always produce identical bytes.
type Disclosure struct {
	Kind                string            `json:"kind"`
	ScheduleVersion     string            `json:"schedule_version"`
	ScheduleHash        string            `json:"schedule_hash"`
	Input               DisclosedInput    `json:"input"`
	QuoteAsset          assets.AssetID    `json:"quote_asset"`
	Tier                DisclosedTier     `json:"tier"`
	ReferencePrice      PriceRecord       `json:"reference_price"`
	ExecutionPrice      PriceRecord       `json:"execution_price"`
	NotionalAtReference money.Quantity    `json:"notional_at_reference"`
	NotionalAtExecution money.Quantity    `json:"notional_at_execution"`
	Spread              DisclosedSpread   `json:"spread"`
	Fees                DisclosedFees     `json:"fees"`
	Network             DisclosedNetwork  `json:"network"`
	SlippageReserve     DisclosedReserve  `json:"slippage_reserve"`
	TotalDebit          money.Quantity    `json:"total_debit"`
	TotalCredit         money.Quantity    `json:"total_credit"`
	WorstCaseTotal      money.Quantity    `json:"worst_case_total"`
	EffectivePrice      PriceRecord       `json:"effective_price"`
	Rounding            DisclosedRounding `json:"rounding"`
}

// PriceRecord is a money.Price in canonical form (time in UTC).
type PriceRecord struct {
	Mantissa   money.Quantity `json:"mantissa"`
	Scale      int32          `json:"scale"`
	QuoteAsset string         `json:"quote_asset"`
	Source     string         `json:"source"`
	At         time.Time      `json:"at"`
}

// NewPriceRecord returns the canonical record of p.
func NewPriceRecord(p money.Price) PriceRecord {
	return PriceRecord{Mantissa: p.Mantissa, Scale: p.Scale, QuoteAsset: p.QuoteAsset, Source: p.Source, At: p.At.UTC()}
}

// Price returns the recorded price.
func (r PriceRecord) Price() money.Price {
	return money.Price{Mantissa: r.Mantissa, Scale: r.Scale, QuoteAsset: r.QuoteAsset, Source: r.Source, At: r.At}
}

// DisclosedInput is the complete QuoteInput.
type DisclosedInput struct {
	Side               Side           `json:"side"`
	BaseQty            money.Quantity `json:"base_qty"`
	BaseDecimals       uint8          `json:"base_decimals"`
	QuoteDecimals      uint8          `json:"quote_decimals"`
	ReferencePrice     PriceRecord    `json:"reference_price"`
	TrailingVolume     money.USD      `json:"trailing_volume_usd"`
	HasSubscription    bool           `json:"has_subscription"`
	StrategyOriginated bool           `json:"strategy_originated"`
}

// NewDisclosedInput returns the canonical record of in.
func NewDisclosedInput(in QuoteInput) DisclosedInput {
	return DisclosedInput{
		Side:               in.Side,
		BaseQty:            in.BaseQty,
		BaseDecimals:       in.BaseDecimals,
		QuoteDecimals:      in.QuoteDecimals,
		ReferencePrice:     NewPriceRecord(in.ReferencePrice),
		TrailingVolume:     in.TrailingVolume,
		HasSubscription:    in.HasSubscription,
		StrategyOriginated: in.StrategyOriginated,
	}
}

// QuoteInput returns the recorded input.
func (d DisclosedInput) QuoteInput() QuoteInput {
	return QuoteInput{
		Side:               d.Side,
		BaseQty:            d.BaseQty,
		BaseDecimals:       d.BaseDecimals,
		QuoteDecimals:      d.QuoteDecimals,
		ReferencePrice:     d.ReferencePrice.Price(),
		TrailingVolume:     d.TrailingVolume,
		HasSubscription:    d.HasSubscription,
		StrategyOriginated: d.StrategyOriginated,
	}
}

// DisclosedTier is the volume tier that priced the order (index -1: the
// base schedule) and the rates it resolved to.
type DisclosedTier struct {
	Index             int       `json:"index"`
	MinTrailingVolume money.USD `json:"min_trailing_volume_usd"`
	PercentageFeeBPS  money.BPS `json:"percentage_fee_bps"`
	SpreadBPS         money.BPS `json:"spread_bps"`
}

// DisclosedSpread is the spread line.
type DisclosedSpread struct {
	BPS      money.BPS      `json:"bps"`
	Amount   money.Quantity `json:"amount"`
	Rounding string         `json:"rounding"`
}

// DisclosedFeeLine is one explicit fee line.
type DisclosedFeeLine struct {
	Component    Component      `json:"component"`
	RateBPS      money.BPS      `json:"rate_bps"`
	DiscountBPS  money.BPS      `json:"discount_bps"`
	FixedAmount  money.Quantity `json:"fixed_amount"`
	Rounding     string         `json:"rounding"`
	Computed     money.Quantity `json:"computed"`
	CapReduction money.Quantity `json:"cap_reduction"`
	Charged      money.Quantity `json:"charged"`
}

// DisclosedFees is the explicit fee section, including the bounds.
type DisclosedFees struct {
	Lines              []DisclosedFeeLine `json:"lines"`
	MinimumTopUp       money.Quantity     `json:"minimum_fee_top_up"`
	ComputedTotal      money.Quantity     `json:"computed_total"`
	Total              money.Quantity     `json:"total"`
	MinFee             money.Quantity     `json:"min_fee"`
	MaxTotalFeeBPS     money.BPS          `json:"max_total_fee_bps"`
	MaxBPSAmount       money.Quantity     `json:"max_total_fee_bps_amount"`
	MaxBPSRounding     string             `json:"max_total_fee_bps_rounding"`
	MaxTotalFee        money.Quantity     `json:"max_total_fee"`
	MinimumApplied     bool               `json:"minimum_applied"`
	MaxBPSApplied      bool               `json:"max_bps_applied"`
	MaxAbsoluteApplied bool               `json:"max_absolute_applied"`
}

// DisclosedNetwork is the network estimate, in its own asset.
type DisclosedNetwork struct {
	Asset        assets.AssetID `json:"asset"`
	Amount       money.Quantity `json:"amount"`
	InQuoteAsset bool           `json:"in_quote_asset"`
}

// DisclosedReserve is the slippage reserve. HeldNotCharged is always true:
// it is in the record so that nobody reading it can mistake the reserve for
// a charge.
type DisclosedReserve struct {
	BPS            money.BPS      `json:"bps"`
	Amount         money.Quantity `json:"amount"`
	Rounding       string         `json:"rounding"`
	HeldNotCharged bool           `json:"held_not_charged"`
}

// DisclosedRounding states every rounding mode the quote used, whether each
// can favour the platform, and whether the schedule explicitly allowed that.
type DisclosedRounding struct {
	Components                []DisclosedMode `json:"components"`
	PlatformFavourableAllowed bool            `json:"platform_favourable_allowed"`
	AnyFavoursPlatform        bool            `json:"any_favours_platform"`
}

// DisclosedMode is one component's rounding mode.
type DisclosedMode struct {
	Component       Component `json:"component"`
	Mode            string    `json:"mode"`
	FavoursPlatform bool      `json:"favours_platform"`
}

// Disclosure returns the canonical audit record of q.
func (q Quote) Disclosure() Disclosure {
	lines := make([]DisclosedFeeLine, 0, 4)
	for _, l := range q.Fees.Lines() {
		lines = append(lines, DisclosedFeeLine{
			Component:    l.Component,
			RateBPS:      l.RateBPS,
			DiscountBPS:  l.DiscountBPS,
			FixedAmount:  l.FixedAmount,
			Rounding:     l.Rounding.String(),
			Computed:     l.Computed,
			CapReduction: l.CapReduction,
			Charged:      l.Charged,
		})
	}
	b := q.Fees.Bounds
	return Disclosure{
		Kind:            DisclosureKind,
		ScheduleVersion: q.ScheduleVersion,
		ScheduleHash:    q.ScheduleHash,
		Input:           NewDisclosedInput(q.Input),
		QuoteAsset:      q.QuoteAsset,
		Tier: DisclosedTier{
			Index:             q.Tier.Index,
			MinTrailingVolume: q.Tier.MinTrailingVolume,
			PercentageFeeBPS:  q.Tier.PercentageFeeBPS,
			SpreadBPS:         q.Tier.SpreadBPS,
		},
		ReferencePrice:      NewPriceRecord(q.ReferencePrice),
		ExecutionPrice:      NewPriceRecord(q.ExecutionPrice),
		NotionalAtReference: q.NotionalAtReference,
		NotionalAtExecution: q.NotionalAtExecution,
		Spread:              DisclosedSpread{BPS: q.Spread.BPS, Amount: q.Spread.Amount, Rounding: q.Spread.Rounding.String()},
		Fees: DisclosedFees{
			Lines:              lines,
			MinimumTopUp:       q.Fees.MinimumTopUp,
			ComputedTotal:      q.Fees.ComputedTotal,
			Total:              q.Fees.Total,
			MinFee:             b.MinFee,
			MaxTotalFeeBPS:     b.MaxTotalFeeBPS,
			MaxBPSAmount:       b.MaxBPSAmount,
			MaxBPSRounding:     b.MaxBPSRounding.String(),
			MaxTotalFee:        b.MaxTotalFee,
			MinimumApplied:     b.MinimumApplied,
			MaxBPSApplied:      b.MaxBPSApplied,
			MaxAbsoluteApplied: b.MaxAbsoluteApplied,
		},
		Network: DisclosedNetwork{Asset: q.Network.Asset, Amount: q.Network.Amount, InQuoteAsset: q.Network.InQuoteAsset},
		SlippageReserve: DisclosedReserve{
			BPS:            q.SlippageReserve.BPS,
			Amount:         q.SlippageReserve.Amount,
			Rounding:       q.SlippageReserve.Rounding.String(),
			HeldNotCharged: true,
		},
		TotalDebit:     q.TotalDebit,
		TotalCredit:    q.TotalCredit,
		WorstCaseTotal: q.WorstCaseTotal,
		EffectivePrice: NewPriceRecord(q.EffectivePrice),
		Rounding:       q.disclosedRounding(),
	}
}

// disclosedRounding lists the mode of every rounded component of q.
func (q Quote) disclosedRounding() DisclosedRounding {
	_, notionalRole := q.Rounding.notionalMode(q.Input.Side)
	used := []struct {
		c    Component
		mode money.RoundingMode
		role roundingRole
	}{
		{ComponentReferenceNotional, q.NotionalRounding, notionalRole},
		{ComponentSpread, q.Spread.Rounding, roleCharge},
		{ComponentPercentageFee, q.Fees.Percentage.Rounding, roleCharge},
		{ComponentVenuePassThrough, q.Fees.VenuePassThrough.Rounding, roleCharge},
		{ComponentStrategyFee, q.Fees.Strategy.Rounding, roleCharge},
		{ComponentFeeCap, q.Fees.Bounds.MaxBPSRounding, roleCharge},
		{ComponentSlippageReserve, q.SlippageReserve.Rounding, roleHold},
		// The effective price is display-only and its mode is fixed so that
		// it never flatters the platform; it charges nothing.
		{ComponentEffectivePrice, q.EffectivePriceRounding, roleHold},
	}
	r := DisclosedRounding{
		Components:                make([]DisclosedMode, 0, len(used)),
		PlatformFavourableAllowed: q.Rounding.AllowPlatformFavourable,
	}
	for _, u := range used {
		fav := favoursPlatform(u.role, u.mode)
		r.AnyFavoursPlatform = r.AnyFavoursPlatform || fav
		r.Components = append(r.Components, DisclosedMode{Component: u.c, Mode: u.mode.String(), FavoursPlatform: fav})
	}
	return r
}

// Canonical returns the deterministic serialised form of the disclosure.
func (d Disclosure) Canonical() ([]byte, error) {
	d.Input.ReferencePrice.At = d.Input.ReferencePrice.At.UTC()
	d.ReferencePrice.At = d.ReferencePrice.At.UTC()
	d.ExecutionPrice.At = d.ExecutionPrice.At.UTC()
	d.EffectivePrice.At = d.EffectivePrice.At.UTC()
	b, err := json.Marshal(d)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "fees: canonicalise disclosure")
	}
	return b, nil
}

// Hash is the hex sha256 of the canonical form.
func (d Disclosure) Hash() (string, error) {
	b, err := d.Canonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// diffDisclosures returns the top-level fields in which got differs from
// want, sorted.
func diffDisclosures(want, got Disclosure) ([]string, error) {
	wb, err := want.Canonical()
	if err != nil {
		return nil, err
	}
	gb, err := got.Canonical()
	if err != nil {
		return nil, err
	}
	return diffCanonical(wb, gb)
}

// diffCanonical returns the top-level keys in which two canonical JSON
// objects differ, sorted.
func diffCanonical(want, got []byte) ([]string, error) {
	if bytes.Equal(want, got) {
		return nil, nil
	}
	var wm, gm map[string]json.RawMessage
	if err := json.Unmarshal(want, &wm); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "fees: compare canonical records")
	}
	if err := json.Unmarshal(got, &gm); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "fees: compare canonical records")
	}
	var diff []string
	for k, w := range wm {
		if g, ok := gm[k]; !ok || !bytes.Equal(w, g) {
			diff = append(diff, k)
		}
	}
	for k := range gm {
		if _, ok := wm[k]; !ok {
			diff = append(diff, k)
		}
	}
	sort.Strings(diff)
	return diff, nil
}
