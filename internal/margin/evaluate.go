package margin

import (
	"sort"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
)

// HoldingValuation is one holding as the engine valued it. Every value is in
// numeraire base units.
type HoldingValuation struct {
	AssetID   string         `json:"asset_id"`
	Numeraire bool           `json:"numeraire"`
	Quantity  money.Quantity `json:"quantity"`
	// MarketValue is quantity × mark, rounded down (RoundFloor). A holding
	// with no mark has no market value.
	MarketValue money.Quantity `json:"market_value"`
	Priced      bool           `json:"priced"`
	Fresh       bool           `json:"fresh"`
	MarkAt      *time.Time     `json:"mark_at,omitempty"`
	// ConcentrationShareBPS is the holding's share of net asset value.
	ConcentrationShareBPS money.BPS        `json:"concentration_share_bps"`
	Haircut               HaircutBreakdown `json:"haircut"`
	// CollateralValue is market value × (1 − haircut), rounded down.
	CollateralValue money.Quantity `json:"collateral_value"`
	// Exposure is the market value of a non-numeraire holding.
	Exposure money.Quantity `json:"exposure"`
	// InitialMargin and MaintenanceMargin are rate × weight × exposure,
	// rounded up, when the account is levered.
	InitialMargin     money.Quantity `json:"initial_margin"`
	MaintenanceMargin money.Quantity `json:"maintenance_margin"`
}

// Evaluation is the deterministic margin state of an account at an instant.
// All values are numeraire base units unless named otherwise.
//
// The canonical equity identity holds exactly:
//
//	Equity = EligibleCollateral + RealizedPnL + UnrealizedPnL
//	         − BorrowLiabilities − AccruedFees − AccruedInterest
type Evaluation struct {
	AccountID         string             `json:"account_id"`
	Mode              MarginMode         `json:"mode"`
	PolicyVersion     string             `json:"policy_version"`
	PolicyHash        string             `json:"policy_hash"`
	At                time.Time          `json:"at"`
	NumeraireAssetID  string             `json:"numeraire_asset_id"`
	NumeraireDecimals uint8              `json:"numeraire_decimals"`
	Holdings          []HoldingValuation `json:"holdings"`

	EligibleCollateral money.Quantity `json:"eligible_collateral"`
	RealizedPnL        money.Quantity `json:"realized_pnl"`
	UnrealizedPnL      money.Quantity `json:"unrealized_pnl"`
	BorrowLiabilities  money.Quantity `json:"borrow_liabilities"`
	AccruedFees        money.Quantity `json:"accrued_fees"`
	AccruedInterest    money.Quantity `json:"accrued_interest"`
	Equity             money.Quantity `json:"equity"`

	// MarketValue is Σ market value of every holding; NetAssetValue is the
	// same identity as Equity with market values instead of collateral
	// values. Concentration is measured against NetAssetValue, so borrowing
	// to buy something else can never dilute an existing position's share.
	MarketValue   money.Quantity `json:"market_value"`
	NetAssetValue money.Quantity `json:"net_asset_value"`

	GrossExposure money.Quantity `json:"gross_exposure"`
	NetExposure   money.Quantity `json:"net_exposure"`
	// InitialMargin is the margin used; MaintenanceMargin the maintenance
	// requirement.
	InitialMargin     money.Quantity `json:"initial_margin"`
	MaintenanceMargin money.Quantity `json:"maintenance_margin"`
	// WarningThreshold is maintenance plus the warning buffer.
	WarningThreshold money.Quantity `json:"warning_threshold"`
	// LiquidationBuffer is Equity − MaintenanceMargin: how far the account is
	// from liquidation (negative once eligible).
	LiquidationBuffer money.Quantity `json:"liquidation_buffer"`

	LeverageCapBPS money.BPS `json:"leverage_cap_bps"`
	// Capacity is the policy-defined capacity: the equity not needed to keep
	// gross exposure within the mode's leverage cap (all of the equity when
	// the account carries no liability).
	Capacity money.Quantity `json:"capacity"`
	// MarginAvailable is max(0, min(Equity − InitialMargin, Capacity)): what
	// could be withdrawn or committed to new risk. It never exceeds Capacity.
	MarginAvailable money.Quantity `json:"margin_available"`
	// EffectiveLeverageBPS is ceil(GrossExposure × 10_000 / Equity); with
	// exposure and no positive equity it is MaxLeverageBPS and
	// LeverageUnbounded is set.
	EffectiveLeverageBPS money.BPS `json:"effective_leverage_bps"`
	LeverageUnbounded    bool      `json:"leverage_unbounded"`

	// Levered is true when the account owes anything or carries derivative
	// exposure. An account that owes nothing puts nobody's money at risk but
	// its owner's, so it carries no requirement and is never liquidated.
	Levered bool `json:"levered"`
	// Health is NORMAL, WARNING or MARGIN_CALL.
	Health              AccountStatus `json:"health"`
	LiquidationEligible bool          `json:"liquidation_eligible"`
	// PricesFresh is false when any non-numeraire holding has a stale,
	// missing or future mark. StaleAssets names them, sorted.
	PricesFresh  bool       `json:"prices_fresh"`
	StaleAssets  []string   `json:"stale_assets"`
	MissingMarks bool       `json:"missing_marks"`
	OldestMarkAt *time.Time `json:"oldest_mark_at,omitempty"`
	NewestMarkAt *time.Time `json:"newest_mark_at,omitempty"`

	Hash string `json:"hash"`
}

// markAgeMS is the age of a mark in whole milliseconds, rounded up, so a mark
// a fraction of a millisecond past the limit is past it. It is 0 for a mark
// at or after now.
func markAgeMS(now, at time.Time) int64 {
	d := now.Sub(at)
	if d <= 0 {
		return 0
	}
	return int64((d-1)/time.Millisecond) + 1
}

// ComputeHash returns the hash an evaluation with these fields carries.
func (e Evaluation) ComputeHash() string {
	e.Hash = ""
	return hashOf(e)
}

// Target is the equity a liquidation case restores, per the policy.
func (e Evaluation) Target(p Policy) money.Quantity {
	if p.PartialTarget == TargetInitialMargin {
		return e.InitialMargin
	}
	return e.WarningThreshold
}

// Restored reports whether no (further) liquidation is needed: the account
// owes nothing, or its equity is at or above the target.
func (e Evaluation) Restored(p Policy) bool {
	return !e.Levered || e.Equity.Cmp(e.Target(p)) >= 0
}

// Evaluate computes the margin state of an account. It is pure and
// deterministic: the same policy and state always produce the same
// evaluation, byte for byte (Hash). The policy must have passed Validate.
func Evaluate(p Policy, s AccountState) (Evaluation, error) {
	e, err := evaluateCore(p, p.Hash(), s)
	if err != nil {
		return Evaluation{}, err
	}
	e.Hash = e.ComputeHash()
	return e, nil
}

func evaluateCore(p Policy, policyHash string, s AccountState) (Evaluation, error) {
	if err := s.Validate(); err != nil {
		return Evaluation{}, err
	}
	now := s.Now.UTC()
	nd := s.NumeraireDecimals
	e := Evaluation{
		AccountID: s.AccountID, Mode: s.Mode, PolicyVersion: p.Version, PolicyHash: policyHash, At: now,
		NumeraireAssetID: s.NumeraireAssetID, NumeraireDecimals: nd,
		StaleAssets: []string{}, PricesFresh: true,
	}

	holdings := append([]Holding(nil), s.Holdings...)
	sort.Slice(holdings, func(i, j int) bool { return holdings[i].AssetID < holdings[j].AssetID })

	// 1. Market values and mark freshness.
	vals := make([]HoldingValuation, len(holdings))
	for i, h := range holdings {
		v := HoldingValuation{AssetID: h.AssetID, Numeraire: h.Numeraire, Quantity: h.Quantity}
		switch {
		case h.Numeraire:
			v.MarketValue, v.Priced, v.Fresh = h.Quantity, true, true
		case h.Mark == nil:
			v.MarketValue = money.Quantity{}
		default:
			mv, err := notional(h.Quantity, h.Decimals, *h.Mark, nd, money.RoundFloor)
			if err != nil {
				return Evaluation{}, err
			}
			at := h.Mark.At.UTC()
			v.MarketValue, v.Priced, v.MarkAt = mv, true, &at
			v.Fresh = !at.After(now) && markAgeMS(now, at) <= p.OracleMaxAgeMS
		}
		if !h.Numeraire && h.Quantity.IsPositive() {
			if !v.Fresh {
				e.PricesFresh = false
				e.StaleAssets = append(e.StaleAssets, h.AssetID)
			}
			if !v.Priced {
				e.MissingMarks = true
			}
			if v.MarkAt != nil && (e.OldestMarkAt == nil || v.MarkAt.Before(*e.OldestMarkAt)) {
				t := *v.MarkAt
				e.OldestMarkAt = &t
			}
			if v.MarkAt != nil && (e.NewestMarkAt == nil || v.MarkAt.After(*e.NewestMarkAt)) {
				t := *v.MarkAt
				e.NewestMarkAt = &t
			}
		}
		e.MarketValue = e.MarketValue.Add(v.MarketValue)
		vals[i] = v
	}

	// 2. Liabilities: capitalized loans and interest accrued since the last
	// posted period.
	for _, l := range s.Loans {
		e.BorrowLiabilities = e.BorrowLiabilities.Add(l.Outstanding())
		acc, err := InterestFor(l.Principal, l.RateBPS, l.AccruedThrough, now)
		if err != nil {
			return Evaluation{}, err
		}
		e.AccruedInterest = e.AccruedInterest.Add(acc)
	}

	// 3. Derivative exposures, converted exactly from cents.
	derivIM, derivMM, derivGross, derivNet := money.Quantity{}, money.Quantity{}, money.Quantity{}, money.Quantity{}
	derivativeRisk := false
	for _, d := range s.Derivatives {
		gross, err := usdToUnits(d.GrossNotional, nd)
		if err != nil {
			return Evaluation{}, err
		}
		im, err := usdToUnits(d.InitialMargin, nd)
		if err != nil {
			return Evaluation{}, err
		}
		mm, err := usdToUnits(d.MaintenanceMargin, nd)
		if err != nil {
			return Evaluation{}, err
		}
		upnl, err := usdToUnits(d.UnrealizedPnL, nd)
		if err != nil {
			return Evaluation{}, err
		}
		derivGross, derivIM, derivMM = derivGross.Add(gross), derivIM.Add(im), derivMM.Add(mm)
		e.UnrealizedPnL = e.UnrealizedPnL.Add(upnl)
		if d.RiskIncreasingDirection {
			derivNet = derivNet.Add(gross)
		} else {
			derivNet = derivNet.Sub(gross)
		}
		if gross.IsPositive() || im.IsPositive() || mm.IsPositive() {
			derivativeRisk = true
		}
	}
	var err error
	if e.RealizedPnL, err = usdToUnits(s.RealizedPnL, nd); err != nil {
		return Evaluation{}, err
	}
	if e.AccruedFees, err = usdToUnits(s.AccruedFees, nd); err != nil {
		return Evaluation{}, err
	}

	// 4. Net asset value at market, the denominator of concentration.
	nonCollateral := e.RealizedPnL.Add(e.UnrealizedPnL).Sub(e.BorrowLiabilities).Sub(e.AccruedFees).Sub(e.AccruedInterest)
	e.NetAssetValue = e.MarketValue.Add(nonCollateral)
	e.Levered = e.BorrowLiabilities.IsPositive() || e.AccruedInterest.IsPositive() || e.AccruedFees.IsPositive() || derivativeRisk

	// 5. Haircuts, collateral values, exposures and requirements.
	for i, h := range holdings {
		v := &vals[i]
		if h.Numeraire {
			v.CollateralValue = v.MarketValue
			e.EligibleCollateral = e.EligibleCollateral.Add(v.CollateralValue)
			continue
		}
		share := money.BPS(0)
		if v.MarketValue.IsPositive() {
			if e.NetAssetValue.Sign() <= 0 {
				share = 10_000
			} else if share, err = shareBPS(v.MarketValue, e.NetAssetValue); err != nil {
				return Evaluation{}, err
			}
		}
		v.ConcentrationShareBPS = share
		f := HaircutFactors{
			Volatility: h.Profile.Volatility, Liquidity: h.Profile.Liquidity, Venue: h.Profile.Venue,
			MarkMissing: h.Mark == nil, ConcentrationShareBPS: share, SettlementDelayMS: h.SettlementDelayMS,
			InFlight: h.InFlight, InFlightMS: h.InFlightMS, BridgeDelayed: h.BridgeDelayed,
		}
		if h.Mark != nil {
			at := h.Mark.At.UTC()
			f.MarkFromFuture = at.After(now)
			if !f.MarkFromFuture {
				f.OracleAgeMS = markAgeMS(now, at)
			}
		}
		if v.Haircut, err = ComputeHaircut(p, f); err != nil {
			return Evaluation{}, err
		}
		if v.CollateralValue, err = mulBPS(v.MarketValue, 10_000-v.Haircut.TotalBPS, money.RoundFloor); err != nil {
			return Evaluation{}, err
		}
		v.Exposure = v.MarketValue
		if e.Levered {
			c, _ := p.class(h.Profile.Volatility)
			if v.InitialMargin, err = mulRateWeight(v.Exposure, c.InitialMarginBPS, h.Profile.RiskWeightBPS); err != nil {
				return Evaluation{}, err
			}
			if v.MaintenanceMargin, err = mulRateWeight(v.Exposure, c.MaintenanceMarginBPS, h.Profile.RiskWeightBPS); err != nil {
				return Evaluation{}, err
			}
		}
		e.EligibleCollateral = e.EligibleCollateral.Add(v.CollateralValue)
		e.GrossExposure = e.GrossExposure.Add(v.Exposure)
		e.NetExposure = e.NetExposure.Add(v.Exposure)
		e.InitialMargin = e.InitialMargin.Add(v.InitialMargin)
		e.MaintenanceMargin = e.MaintenanceMargin.Add(v.MaintenanceMargin)
	}
	e.Holdings = vals
	e.GrossExposure = e.GrossExposure.Add(derivGross)
	e.NetExposure = e.NetExposure.Add(derivNet)
	e.InitialMargin = e.InitialMargin.Add(derivIM)
	e.MaintenanceMargin = e.MaintenanceMargin.Add(derivMM)

	// 6. The canonical equity.
	e.Equity = e.EligibleCollateral.Add(nonCollateral)

	// 7. Thresholds, leverage, capacity and health.
	buffer, err := mulBPS(e.MaintenanceMargin, p.WarningBufferBPS, money.RoundCeil)
	if err != nil {
		return Evaluation{}, err
	}
	e.WarningThreshold = e.MaintenanceMargin.Add(buffer)
	e.LiquidationBuffer = e.Equity.Sub(e.MaintenanceMargin)
	e.LeverageCapBPS = p.LeverageCap(s.Mode)
	switch {
	case !e.GrossExposure.IsPositive():
		e.EffectiveLeverageBPS = 0
	case !e.Equity.IsPositive():
		e.EffectiveLeverageBPS, e.LeverageUnbounded = MaxLeverageBPS, true
	default:
		lev, err := ratioBPSCeil(e.GrossExposure, e.Equity)
		if err != nil {
			return Evaluation{}, err
		}
		if lev.Cmp(money.QuantityFromInt64(int64(MaxLeverageBPS))) >= 0 {
			e.EffectiveLeverageBPS = MaxLeverageBPS
		} else {
			n, err := lev.Int64()
			if err != nil {
				return Evaluation{}, errs.Wrap(err, errs.CodeInternal, "margin: leverage")
			}
			e.EffectiveLeverageBPS = money.BPS(n)
		}
	}
	if e.Levered {
		needed, err := e.GrossExposure.MulDiv(money.QuantityFromInt64(bpsDenominator), money.QuantityFromInt64(int64(e.LeverageCapBPS)), money.RoundCeil)
		if err != nil {
			return Evaluation{}, errs.Wrap(err, errs.CodeInternal, "margin: capacity")
		}
		e.Capacity = e.Equity.Sub(needed)
		e.MarginAvailable = maxQ(money.Quantity{}, minQ(e.Equity.Sub(e.InitialMargin), e.Capacity))
	} else {
		e.Capacity = e.Equity
		e.MarginAvailable = maxQ(money.Quantity{}, e.Equity)
	}
	switch {
	case !e.Levered:
		e.Health = StatusNormal
	case e.Equity.Cmp(e.MaintenanceMargin) < 0:
		e.Health, e.LiquidationEligible = StatusMarginCall, true
	case e.Equity.Cmp(e.WarningThreshold) < 0:
		e.Health = StatusWarning
	default:
		e.Health = StatusNormal
	}
	return e, nil
}

// EvaluationUSD is an evaluation rendered in cents. Each figure is rounded in
// the direction that can only make the account look less healthy: assets and
// equity down, liabilities and requirements up.
type EvaluationUSD struct {
	EligibleCollateral money.USD `json:"eligible_collateral"`
	BorrowLiabilities  money.USD `json:"borrow_liabilities"`
	AccruedInterest    money.USD `json:"accrued_interest"`
	Equity             money.USD `json:"equity"`
	MarketValue        money.USD `json:"market_value"`
	GrossExposure      money.USD `json:"gross_exposure"`
	InitialMargin      money.USD `json:"initial_margin"`
	MaintenanceMargin  money.USD `json:"maintenance_margin"`
	WarningThreshold   money.USD `json:"warning_threshold"`
	MarginAvailable    money.USD `json:"margin_available"`
	LiquidationBuffer  money.USD `json:"liquidation_buffer"`
}

// USD renders the evaluation in cents with conservative rounding.
func (e Evaluation) USD() (EvaluationUSD, error) {
	d := e.NumeraireDecimals
	var out EvaluationUSD
	var err error
	conv := []struct {
		dst  *money.USD
		src  money.Quantity
		mode money.RoundingMode
	}{
		{&out.EligibleCollateral, e.EligibleCollateral, money.RoundFloor},
		{&out.BorrowLiabilities, e.BorrowLiabilities, money.RoundCeil},
		{&out.AccruedInterest, e.AccruedInterest, money.RoundCeil},
		{&out.Equity, e.Equity, money.RoundFloor},
		{&out.MarketValue, e.MarketValue, money.RoundFloor},
		{&out.GrossExposure, e.GrossExposure, money.RoundCeil},
		{&out.InitialMargin, e.InitialMargin, money.RoundCeil},
		{&out.MaintenanceMargin, e.MaintenanceMargin, money.RoundCeil},
		{&out.WarningThreshold, e.WarningThreshold, money.RoundCeil},
		{&out.MarginAvailable, e.MarginAvailable, money.RoundFloor},
		{&out.LiquidationBuffer, e.LiquidationBuffer, money.RoundFloor},
	}
	for _, c := range conv {
		if *c.dst, err = unitsToUSD(c.src, d, c.mode); err != nil {
			return EvaluationUSD{}, err
		}
	}
	return out, nil
}

// RiskSections are the Risk Kernel V2 sections an evaluation supplies.
type RiskSections struct {
	Margin        *risk.MarginSnapshot
	Oracle        *risk.OracleSnapshot
	Concentration *risk.ConcentrationSnapshot
}

// RiskSectionsFor renders an evaluation (normally the post-operation one) as
// the margin, oracle and concentration sections of a risk input. Equity is
// rounded down and requirements up, so the kernel can only see the account
// as less healthy than it is. The oracle section reports the oldest mark of
// any held risky asset -- or a mark from the future, which the kernel treats
// as stale exactly as the engine does -- and unavailable when any of them
// has no mark at all; a portfolio of only numeraire needs no oracle and is
// reported fresh.
func RiskSectionsFor(e Evaluation) (RiskSections, error) {
	u, err := e.USD()
	if err != nil {
		return RiskSections{}, err
	}
	out := RiskSections{
		Margin: &risk.MarginSnapshot{
			EquityAfter:              u.Equity,
			InitialRequiredAfter:     u.InitialMargin,
			MaintenanceRequiredAfter: u.MaintenanceMargin,
			LeverageAfterBPS:         e.EffectiveLeverageBPS,
		},
		Oracle: &risk.OracleSnapshot{Available: !e.MissingMarks, ObservedAt: e.At},
	}
	if e.OldestMarkAt != nil {
		out.Oracle.ObservedAt = *e.OldestMarkAt
	}
	if e.NewestMarkAt != nil && e.NewestMarkAt.After(e.At) {
		out.Oracle.ObservedAt = *e.NewestMarkAt
	}
	largest := money.Quantity{}
	for _, h := range e.Holdings {
		if !h.Numeraire && h.MarketValue.Cmp(largest) > 0 {
			largest = h.MarketValue
		}
	}
	pos, err := unitsToUSD(largest, e.NumeraireDecimals, money.RoundCeil)
	if err != nil {
		return RiskSections{}, err
	}
	out.Concentration = &risk.ConcentrationSnapshot{PositionAfter: pos, Portfolio: u.MarketValue}
	return out, nil
}

// EffectiveRiskPolicy is the Risk Kernel V2 policy in force for a margin
// account: the base policy with the leverage cap tightened to the account's
// mode, the oracle age to the margin policy's, and the concentration limit
// to the margin policy's. The version names both policies and the mode, so a
// stored decision says exactly which limits it applied.
func EffectiveRiskPolicy(base risk.AdvancedPolicy, p Policy, mode MarginMode) risk.AdvancedPolicy {
	out := base
	if c := p.LeverageCap(mode); c < out.MaxLeverageBPS {
		out.MaxLeverageBPS = c
	}
	if p.OracleMaxAgeMS < out.MaxOracleAgeMS {
		out.MaxOracleAgeMS = p.OracleMaxAgeMS
	}
	if out.MaxConcentrationBPS <= 0 || p.MaxPositionShareBPS < out.MaxConcentrationBPS {
		out.MaxConcentrationBPS = p.MaxPositionShareBPS
	}
	out.Version = base.Version + "+" + p.Version + "/" + string(mode)
	return out
}
