package margin

import (
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// HaircutFactors are the per-holding inputs of the haircut engine.
type HaircutFactors struct {
	Volatility VolatilityClass `json:"volatility"`
	Liquidity  LiquidityClass  `json:"liquidity"`
	Venue      VenueTier       `json:"venue"`
	// OracleAgeMS is the age of the mark. MarkMissing means there is none.
	OracleAgeMS int64 `json:"oracle_age_ms"`
	MarkMissing bool  `json:"mark_missing"`
	// MarkFromFuture is a mark observed after the evaluation time. It is
	// treated as stale: an as-of valuation never looks ahead.
	MarkFromFuture bool `json:"mark_from_future"`
	// ConcentrationShareBPS is the holding's share of the account's net
	// asset value at market, capped at 10_000.
	ConcentrationShareBPS money.BPS `json:"concentration_share_bps"`
	SettlementDelayMS     int64     `json:"settlement_delay_ms"`
	// InFlight is collateral moving between chains; InFlightMS is how long
	// it has been moving. BridgeDelayed marks a bridge known to be stuck.
	InFlight      bool  `json:"in_flight"`
	InFlightMS    int64 `json:"in_flight_ms"`
	BridgeDelayed bool  `json:"bridge_delayed"`
}

// HaircutBreakdown is the haircut, component by component.
type HaircutBreakdown struct {
	VolatilityBPS    money.BPS `json:"volatility_bps"`
	LiquidityBPS     money.BPS `json:"liquidity_bps"`
	OracleBPS        money.BPS `json:"oracle_bps"`
	VenueBPS         money.BPS `json:"venue_bps"`
	ConcentrationBPS money.BPS `json:"concentration_bps"`
	SettlementBPS    money.BPS `json:"settlement_bps"`
	CrossChainBPS    money.BPS `json:"cross_chain_bps"`
	// ZeroValue is set when a single factor makes the collateral worthless:
	// a stale, missing or future mark, or a bridge known to be delayed.
	ZeroValue bool `json:"zero_value"`
	// TotalBPS is the haircut applied: 10_000 when ZeroValue, otherwise the
	// sum of the components capped at 10_000.
	TotalBPS money.BPS `json:"total_bps"`
}

const msPerHour = 3_600_000

// ceilHours returns the number of started hours in ms (0 for ms <= 0).
func ceilHours(ms int64) int64 {
	if ms <= 0 {
		return 0
	}
	return (ms-1)/msPerHour + 1
}

// satBPS returns n × perUnit saturated at 10_000, for n >= 0.
func satBPS(n int64, perUnit money.BPS) money.BPS {
	if n <= 0 || perUnit <= 0 {
		return 0
	}
	if n > 10_000 {
		n = 10_000
	}
	v := n * int64(perUnit)
	if v > 10_000 {
		return 10_000
	}
	return money.BPS(v)
}

// ComputeHaircut composes the haircut of one holding. The composition is
// additive and capped, so it is exact and monotone in every factor:
//
//	total = min(10000, volatility(class) + liquidity(class) + oracle aging
//	              + venue(tier) + concentration + settlement + cross-chain)
//
// with these components:
//
//   - volatility: the class's base haircut.
//   - liquidity: the liquidity class's add-on.
//   - oracle: OracleAgingHaircutBPS once the mark is older than
//     OracleAgingMS; a mark older than OracleMaxAgeMS, a missing mark or a
//     mark from the future makes the whole haircut 10000 (zero collateral
//     value) regardless of everything else.
//   - venue: the chain/venue tier's add-on.
//   - concentration: 0 up to ConcentrationThresholdBPS of net asset value,
//     then ceil(ConcentrationMaxHaircutBPS × (share − threshold) /
//     (10000 − threshold)), reaching the maximum at a 100% share.
//   - settlement: SettlementDelayHaircutBPSPerHour per started hour of
//     settlement delay.
//   - cross-chain: for collateral flagged in flight, CrossChainBaseHaircutBPS
//     plus CrossChainHaircutBPSPerHour per started hour in flight; a bridge
//     known to be delayed makes the whole haircut 10000.
//
// Factors are ranked (classes and tiers in ascending order of risk) and the
// policy refuses tables that are not monotone in rank, so raising any one
// factor can never lower the haircut.
func ComputeHaircut(p Policy, f HaircutFactors) (HaircutBreakdown, error) {
	c, ok := p.class(f.Volatility)
	if !ok {
		return HaircutBreakdown{}, errs.Newf(errs.CodeValidationFailed, "margin: volatility class %q is not in the policy", f.Volatility)
	}
	l, ok := p.liquidity(f.Liquidity)
	if !ok {
		return HaircutBreakdown{}, errs.Newf(errs.CodeValidationFailed, "margin: liquidity class %q is not in the policy", f.Liquidity)
	}
	v, ok := p.venue(f.Venue)
	if !ok {
		return HaircutBreakdown{}, errs.Newf(errs.CodeValidationFailed, "margin: venue tier %q is not in the policy", f.Venue)
	}
	if f.ConcentrationShareBPS < 0 || f.SettlementDelayMS < 0 || f.InFlightMS < 0 {
		return HaircutBreakdown{}, errs.New(errs.CodeValidationFailed, "margin: haircut factors must not be negative")
	}
	b := HaircutBreakdown{
		VolatilityBPS: c.HaircutBPS,
		LiquidityBPS:  l.HaircutBPS,
		VenueBPS:      v.HaircutBPS,
	}
	stale := f.MarkMissing || f.MarkFromFuture || f.OracleAgeMS > p.OracleMaxAgeMS
	if !stale && f.OracleAgeMS > p.OracleAgingMS {
		b.OracleBPS = p.OracleAgingHaircutBPS
	}
	share := f.ConcentrationShareBPS
	if share > 10_000 {
		share = 10_000
	}
	if share > p.ConcentrationThresholdBPS {
		num := int64(p.ConcentrationMaxHaircutBPS) * int64(share-p.ConcentrationThresholdBPS)
		den := int64(10_000 - p.ConcentrationThresholdBPS)
		b.ConcentrationBPS = money.BPS((num + den - 1) / den)
	}
	b.SettlementBPS = satBPS(ceilHours(f.SettlementDelayMS), p.SettlementDelayHaircutBPSPerHour)
	if f.InFlight {
		cc := int64(p.CrossChainBaseHaircutBPS) + int64(satBPS(ceilHours(f.InFlightMS), p.CrossChainHaircutBPSPerHour))
		if cc > 10_000 {
			cc = 10_000
		}
		b.CrossChainBPS = money.BPS(cc)
	}
	if stale || (f.InFlight && f.BridgeDelayed) {
		b.ZeroValue = true
		b.TotalBPS = 10_000
		return b, nil
	}
	total := int64(b.VolatilityBPS) + int64(b.LiquidityBPS) + int64(b.OracleBPS) + int64(b.VenueBPS) +
		int64(b.ConcentrationBPS) + int64(b.SettlementBPS) + int64(b.CrossChainBPS)
	if total > 10_000 {
		total = 10_000
	}
	b.TotalBPS = money.BPS(total)
	return b, nil
}
