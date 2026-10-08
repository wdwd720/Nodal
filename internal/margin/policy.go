package margin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
)

// MaxLeverageBPS is the largest leverage the engine reports. An account with
// exposure and no positive equity has unbounded leverage, which is reported
// as this value with Evaluation.LeverageUnbounded set. It stays far below
// 2^53 so every canonical JSON encoder renders it exactly.
const MaxLeverageBPS money.BPS = 1_000_000_000

// ClassParams are the haircut and margin rates of one volatility class.
type ClassParams struct {
	Class VolatilityClass `json:"class"`
	// HaircutBPS is the base collateral haircut of the class.
	HaircutBPS money.BPS `json:"haircut_bps"`
	// InitialMarginBPS is the initial requirement on the market value of an
	// exposure of this class (before its risk weight).
	InitialMarginBPS money.BPS `json:"initial_margin_bps"`
	// MaintenanceMarginBPS is the maintenance requirement.
	MaintenanceMarginBPS money.BPS `json:"maintenance_margin_bps"`
}

// LiquidityParams are the haircut add-on and the liquidation slippage of one
// liquidity class.
type LiquidityParams struct {
	Class      LiquidityClass `json:"class"`
	HaircutBPS money.BPS      `json:"haircut_bps"`
	// LiquidationSlippageBPS is the price concession a forced sale of an
	// asset of this class is assumed to make. The liquidation slippage cap
	// bounds it.
	LiquidationSlippageBPS money.BPS `json:"liquidation_slippage_bps"`
}

// VenueParams are the haircut add-on of one chain/venue risk tier.
type VenueParams struct {
	Tier       VenueTier `json:"tier"`
	HaircutBPS money.BPS `json:"haircut_bps"`
}

// AssetRule is the instrument-specific part of the policy: a risk weight on
// the class margin rates and optional class overrides.
type AssetRule struct {
	AssetID string `json:"asset_id"`
	// RiskWeightBPS multiplies the class margin rates (10_000 = 1x).
	RiskWeightBPS money.BPS       `json:"risk_weight_bps"`
	Volatility    VolatilityClass `json:"volatility,omitempty"`
	Liquidity     LiquidityClass  `json:"liquidity,omitempty"`
	Venue         VenueTier       `json:"venue,omitempty"`
}

// RiskClassMapping maps an asset's registry risk class (assets.RiskClass) to
// a volatility class. An asset whose risk class is not mapped is not
// eligible as margin collateral or as a margin position.
type RiskClassMapping struct {
	AssetRiskClass string          `json:"asset_risk_class"`
	Volatility     VolatilityClass `json:"volatility"`
}

// LeverageCaps bound gross exposure / equity per margin mode.
type LeverageCaps struct {
	CashBPS     money.BPS `json:"cash_bps"`
	IsolatedBPS money.BPS `json:"isolated_bps"`
	CrossBPS    money.BPS `json:"cross_bps"`
}

// Policy is the versioned, hashed margin policy. Every limit is an exact
// integer. Two policies with the same Version must be the same document;
// Store.EnsurePolicy refuses a version whose content changed.
type Policy struct {
	Version string `json:"version"`

	Classes      []ClassParams      `json:"classes"`
	Liquidity    []LiquidityParams  `json:"liquidity"`
	Venues       []VenueParams      `json:"venues"`
	AssetRules   []AssetRule        `json:"asset_rules"`
	RiskClassMap []RiskClassMapping `json:"risk_class_map"`

	DefaultLiquidity LiquidityClass `json:"default_liquidity"`
	DefaultVenue     VenueTier      `json:"default_venue"`

	LeverageCaps LeverageCaps `json:"leverage_caps"`

	// OracleMaxAgeMS: a mark older than this is stale, and a stale or missing
	// mark is a 100% haircut. OracleAgingMS..OracleMaxAgeMS is the aging band
	// in which OracleAgingHaircutBPS is added.
	OracleMaxAgeMS        int64     `json:"oracle_max_age_ms"`
	OracleAgingMS         int64     `json:"oracle_aging_ms"`
	OracleAgingHaircutBPS money.BPS `json:"oracle_aging_haircut_bps"`
	// MarkLookbackMS is how far back the service looks for the last known
	// mark of an asset; a stale mark is still reported (and haircut to zero),
	// a mark older than the lookback is missing.
	MarkLookbackMS int64 `json:"mark_lookback_ms"`

	// A position whose share of the account's net asset value at market
	// exceeds ConcentrationThresholdBPS carries an add-on growing linearly to
	// ConcentrationMaxHaircutBPS at a 100% share.
	ConcentrationThresholdBPS  money.BPS `json:"concentration_threshold_bps"`
	ConcentrationMaxHaircutBPS money.BPS `json:"concentration_max_haircut_bps"`
	// MaxPositionShareBPS is the concentration limit: a risk-increasing
	// operation may not leave one position above this share of the
	// account's collateral market value. 10_000 disables the limit.
	MaxPositionShareBPS money.BPS `json:"max_position_share_bps"`

	SettlementDelayHaircutBPSPerHour money.BPS `json:"settlement_delay_haircut_bps_per_hour"`
	CrossChainBaseHaircutBPS         money.BPS `json:"cross_chain_base_haircut_bps"`
	CrossChainHaircutBPSPerHour      money.BPS `json:"cross_chain_haircut_bps_per_hour"`

	// WarningBufferBPS is the buffer above maintenance, as a share of the
	// maintenance requirement, below which an account is in WARNING.
	WarningBufferBPS money.BPS `json:"warning_buffer_bps"`

	LiquidationSlippageCapBPS money.BPS         `json:"liquidation_slippage_cap_bps"`
	LiquidationFeeBPS         money.BPS         `json:"liquidation_fee_bps"`
	PartialTarget             LiquidationTarget `json:"partial_target"`
	MaxLiquidationSteps       int               `json:"max_liquidation_steps"`

	// BorrowRateBPS is the annual simple interest rate of a new borrow lot.
	BorrowRateBPS money.BPS `json:"borrow_rate_bps"`
	// OpenSlippageBPS is the disclosed price concession of a paper leveraged
	// buy: it executes at mark × (1 + OpenSlippageBPS).
	OpenSlippageBPS money.BPS `json:"open_slippage_bps"`

	// StressExecutionGapBPS is the gap down the execution-condition stress
	// scenarios (venue unavailable, partial fill, extreme spread) apply first,
	// so that each of them shows how the condition changes a real
	// liquidation rather than an idle account.
	StressExecutionGapBPS money.BPS `json:"stress_execution_gap_bps"`
	StressDepegBPS        money.BPS `json:"stress_depeg_bps"`
	StressPartialFillBPS  money.BPS `json:"stress_partial_fill_bps"`
}

// DefaultPolicy is the sandbox margin policy. It is fully specified and is
// never a statement that any live lending is acceptable.
func DefaultPolicy() Policy {
	return Policy{
		Version: "margin-sandbox-v1",
		Classes: []ClassParams{
			{Class: VolStable, HaircutBPS: 200, InitialMarginBPS: 500, MaintenanceMarginBPS: 300},
			{Class: VolLow, HaircutBPS: 1_000, InitialMarginBPS: 2_000, MaintenanceMarginBPS: 1_000},
			{Class: VolMedium, HaircutBPS: 2_000, InitialMarginBPS: 3_300, MaintenanceMarginBPS: 1_700},
			{Class: VolHigh, HaircutBPS: 3_000, InitialMarginBPS: 5_000, MaintenanceMarginBPS: 2_500},
			{Class: VolExtreme, HaircutBPS: 5_000, InitialMarginBPS: 10_000, MaintenanceMarginBPS: 5_000},
		},
		Liquidity: []LiquidityParams{
			{Class: LiqDeep, HaircutBPS: 0, LiquidationSlippageBPS: 25},
			{Class: LiqNormal, HaircutBPS: 100, LiquidationSlippageBPS: 50},
			{Class: LiqThin, HaircutBPS: 500, LiquidationSlippageBPS: 150},
			{Class: LiqIlliquid, HaircutBPS: 2_000, LiquidationSlippageBPS: 400},
		},
		Venues: []VenueParams{
			{Tier: VenueTier1, HaircutBPS: 0},
			{Tier: VenueTier2, HaircutBPS: 250},
			{Tier: VenueTier3, HaircutBPS: 1_000},
		},
		AssetRules: []AssetRule{},
		RiskClassMap: []RiskClassMapping{
			{AssetRiskClass: "SETTLEMENT", Volatility: VolStable},
			{AssetRiskClass: "MAJOR", Volatility: VolLow},
			{AssetRiskClass: "STANDARD", Volatility: VolMedium},
			{AssetRiskClass: "SPECULATIVE", Volatility: VolHigh},
		},
		DefaultLiquidity: LiqNormal,
		DefaultVenue:     VenueTier1,
		LeverageCaps:     LeverageCaps{CashBPS: 10_000, IsolatedBPS: 30_000, CrossBPS: 50_000},

		OracleMaxAgeMS:        30_000,
		OracleAgingMS:         15_000,
		OracleAgingHaircutBPS: 100,
		MarkLookbackMS:        7 * 24 * 3_600_000,

		ConcentrationThresholdBPS:  5_000,
		ConcentrationMaxHaircutBPS: 1_500,
		MaxPositionShareBPS:        10_000,

		SettlementDelayHaircutBPSPerHour: 50,
		CrossChainBaseHaircutBPS:         2_500,
		CrossChainHaircutBPSPerHour:      100,

		WarningBufferBPS: 2_500,

		LiquidationSlippageCapBPS: 500,
		LiquidationFeeBPS:         100,
		PartialTarget:             TargetMaintenancePlusBuffer,
		MaxLiquidationSteps:       10,

		BorrowRateBPS:   800,
		OpenSlippageBPS: 10,

		StressExecutionGapBPS: 2_500,
		StressDepegBPS:        1_000,
		StressPartialFillBPS:  5_000,
	}
}

func bpsIn(v money.BPS, lo, hi money.BPS) bool { return v >= lo && v <= hi }

// Validate checks every limit and the invariants the engine relies on:
// requirements and haircuts never decrease with risk, maintenance never
// exceeds initial, and a forced sale always improves an account's margin
// (every class's haircut plus maintenance rate exceeds the slippage plus the
// fee a liquidation of it can cost), so liquidation can only make progress.
func (p Policy) Validate() error {
	var problems []string
	add := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	if v := strings.TrimSpace(p.Version); v == "" || v != p.Version || len(v) > 100 {
		add("version must be 1..100 characters without surrounding whitespace")
	}

	classes := map[VolatilityClass]ClassParams{}
	for _, c := range p.Classes {
		if c.Class.Rank() < 0 {
			add("unknown volatility class %q", c.Class)
			continue
		}
		if _, dup := classes[c.Class]; dup {
			add("volatility class %s listed twice", c.Class)
		}
		classes[c.Class] = c
		if !bpsIn(c.HaircutBPS, 0, 10_000) || !bpsIn(c.InitialMarginBPS, 0, 10_000) || !bpsIn(c.MaintenanceMarginBPS, 0, 10_000) {
			add("class %s rates must be within [0, 10000]", c.Class)
		}
		if c.MaintenanceMarginBPS > c.InitialMarginBPS {
			add("class %s maintenance exceeds initial", c.Class)
		}
	}
	var prevClass *ClassParams
	for _, vc := range AllVolatilityClasses() {
		c, ok := classes[vc]
		if !ok {
			add("volatility class %s is not configured", vc)
			continue
		}
		if prevClass != nil && (c.HaircutBPS < prevClass.HaircutBPS || c.InitialMarginBPS < prevClass.InitialMarginBPS ||
			c.MaintenanceMarginBPS < prevClass.MaintenanceMarginBPS) {
			add("class %s is treated as less risky than %s", vc, prevClass.Class)
		}
		cc := c
		prevClass = &cc
	}

	liq := map[LiquidityClass]LiquidityParams{}
	for _, l := range p.Liquidity {
		if l.Class.Rank() < 0 {
			add("unknown liquidity class %q", l.Class)
			continue
		}
		if _, dup := liq[l.Class]; dup {
			add("liquidity class %s listed twice", l.Class)
		}
		liq[l.Class] = l
		if !bpsIn(l.HaircutBPS, 0, 10_000) || !bpsIn(l.LiquidationSlippageBPS, 0, 9_999) {
			add("liquidity class %s rates out of range", l.Class)
		}
	}
	var prevLiq *LiquidityParams
	for _, lc := range AllLiquidityClasses() {
		l, ok := liq[lc]
		if !ok {
			add("liquidity class %s is not configured", lc)
			continue
		}
		if prevLiq != nil && (l.HaircutBPS < prevLiq.HaircutBPS || l.LiquidationSlippageBPS < prevLiq.LiquidationSlippageBPS) {
			add("liquidity class %s is treated as more liquid than %s", lc, prevLiq.Class)
		}
		ll := l
		prevLiq = &ll
	}

	venues := map[VenueTier]VenueParams{}
	for _, v := range p.Venues {
		if v.Tier.Rank() < 0 {
			add("unknown venue tier %q", v.Tier)
			continue
		}
		if _, dup := venues[v.Tier]; dup {
			add("venue tier %s listed twice", v.Tier)
		}
		venues[v.Tier] = v
		if !bpsIn(v.HaircutBPS, 0, 10_000) {
			add("venue tier %s haircut out of range", v.Tier)
		}
	}
	var prevVenue *VenueParams
	for _, vt := range AllVenueTiers() {
		v, ok := venues[vt]
		if !ok {
			add("venue tier %s is not configured", vt)
			continue
		}
		if prevVenue != nil && v.HaircutBPS < prevVenue.HaircutBPS {
			add("venue tier %s is treated as safer than %s", vt, prevVenue.Tier)
		}
		vv := v
		prevVenue = &vv
	}

	minWeight := money.BPS(10_000)
	seenAssets := map[string]bool{}
	for _, r := range p.AssetRules {
		parsed, err := id.ParseAny(r.AssetID)
		if err != nil || parsed.String() != r.AssetID {
			add("asset rule asset_id %q must be a canonical lowercase uuid", r.AssetID)
		}
		if seenAssets[r.AssetID] {
			add("asset %s has two rules", r.AssetID)
		}
		seenAssets[r.AssetID] = true
		if !bpsIn(r.RiskWeightBPS, 1_000, 50_000) {
			add("asset %s risk weight must be within [1000, 50000]", r.AssetID)
		}
		if r.RiskWeightBPS < minWeight {
			minWeight = r.RiskWeightBPS
		}
		if r.Volatility != "" && r.Volatility.Rank() < 0 {
			add("asset %s volatility override %q is unknown", r.AssetID, r.Volatility)
		}
		if r.Liquidity != "" && r.Liquidity.Rank() < 0 {
			add("asset %s liquidity override %q is unknown", r.AssetID, r.Liquidity)
		}
		if r.Venue != "" && r.Venue.Rank() < 0 {
			add("asset %s venue override %q is unknown", r.AssetID, r.Venue)
		}
	}
	seenRC := map[string]bool{}
	for _, m := range p.RiskClassMap {
		if strings.TrimSpace(m.AssetRiskClass) == "" || seenRC[m.AssetRiskClass] {
			add("risk class mapping %q is empty or duplicated", m.AssetRiskClass)
		}
		seenRC[m.AssetRiskClass] = true
		if m.Volatility.Rank() < 0 {
			add("risk class %s maps to unknown volatility class %q", m.AssetRiskClass, m.Volatility)
		}
	}
	if p.DefaultLiquidity.Rank() < 0 {
		add("default liquidity %q is unknown", p.DefaultLiquidity)
	}
	if p.DefaultVenue.Rank() < 0 {
		add("default venue %q is unknown", p.DefaultVenue)
	}

	if p.LeverageCaps.CashBPS != 10_000 {
		add("a CASH account takes no leverage: its cap must be exactly 10000")
	}
	if !bpsIn(p.LeverageCaps.IsolatedBPS, 10_000, MaxLeverageBPS) || !bpsIn(p.LeverageCaps.CrossBPS, 10_000, MaxLeverageBPS) {
		add("leverage caps must be within [10000, %d]", MaxLeverageBPS)
	}

	if p.OracleMaxAgeMS <= 0 {
		add("oracle_max_age_ms must be positive")
	}
	if p.OracleAgingMS < 0 || p.OracleAgingMS > p.OracleMaxAgeMS {
		add("oracle_aging_ms must be within [0, oracle_max_age_ms]")
	}
	if !bpsIn(p.OracleAgingHaircutBPS, 0, 10_000) {
		add("oracle_aging_haircut_bps out of range")
	}
	if p.MarkLookbackMS < p.OracleMaxAgeMS {
		add("mark_lookback_ms must be at least oracle_max_age_ms")
	}
	if !bpsIn(p.ConcentrationThresholdBPS, 0, 9_999) || !bpsIn(p.ConcentrationMaxHaircutBPS, 0, 10_000) {
		add("concentration haircut parameters out of range")
	}
	if !bpsIn(p.MaxPositionShareBPS, 1, 10_000) {
		add("max_position_share_bps must be within [1, 10000]")
	}
	if !bpsIn(p.SettlementDelayHaircutBPSPerHour, 0, 10_000) || !bpsIn(p.CrossChainBaseHaircutBPS, 0, 10_000) ||
		!bpsIn(p.CrossChainHaircutBPSPerHour, 0, 10_000) {
		add("settlement and cross-chain haircut parameters out of range")
	}
	if !bpsIn(p.WarningBufferBPS, 0, 10_000) {
		add("warning_buffer_bps out of range")
	}
	if !bpsIn(p.LiquidationSlippageCapBPS, 0, 9_999) || !bpsIn(p.LiquidationFeeBPS, 0, 9_999) {
		add("liquidation slippage cap and fee must be within [0, 9999]")
	}
	if !p.PartialTarget.Valid() {
		add("partial_target %q is unknown", p.PartialTarget)
	}
	if p.MaxLiquidationSteps < 1 || p.MaxLiquidationSteps > 100 {
		add("max_liquidation_steps must be within [1, 100]")
	}
	if !bpsIn(p.BorrowRateBPS, 0, 100_000) {
		add("borrow_rate_bps must be within [0, 100000]")
	}
	if !bpsIn(p.OpenSlippageBPS, 0, 9_999) {
		add("open_slippage_bps must be within [0, 9999]")
	}
	if !bpsIn(p.StressExecutionGapBPS, 0, 10_000) || !bpsIn(p.StressDepegBPS, 0, 10_000) || !bpsIn(p.StressPartialFillBPS, 1, 10_000) {
		add("stress parameters out of range")
	}

	// A forced sale must improve the margin it is meant to restore: the
	// collateral a sale gives up is worth (1 - haircut) of its market value
	// and the requirement it releases is maintenance x weight of it, while
	// the sale costs at most the capped slippage plus the fee. So for every
	// class and liquidity: haircut + liquidity add-on + MM x min weight must
	// exceed slippage + fee.
	if len(problems) == 0 {
		for _, vc := range AllVolatilityClasses() {
			c := classes[vc]
			for _, lc := range AllLiquidityClasses() {
				l := liq[lc]
				slip := l.LiquidationSlippageBPS
				if slip > p.LiquidationSlippageCapBPS {
					slip = p.LiquidationSlippageCapBPS
				}
				mm := int64(c.MaintenanceMarginBPS) * int64(minWeight) / 10_000
				if int64(c.HaircutBPS)+int64(l.HaircutBPS)+mm <= int64(slip)+int64(p.LiquidationFeeBPS) {
					add("a forced sale of %s/%s could deepen the shortfall: haircut %d + liquidity %d + maintenance %d <= slippage %d + fee %d",
						vc, lc, c.HaircutBPS, l.HaircutBPS, mm, slip, p.LiquidationFeeBPS)
				}
			}
		}
	}

	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "invalid margin policy").WithField("problems", problems)
	}
	return nil
}

// CanonicalJSON renders the policy deterministically.
func (p Policy) CanonicalJSON() ([]byte, error) { return audit.CanonicalJSON(p) }

// Hash is the hex sha256 of the policy's canonical JSON. It is recorded with
// every evaluation, decision, case and stress run.
func (p Policy) Hash() string { return hashOf(p) }

// ParsePolicy decodes and validates a policy document. Unknown fields and
// trailing data are refused; numbers must be integers.
func ParsePolicy(b []byte) (Policy, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var p Policy
	if err := dec.Decode(&p); err != nil {
		return Policy{}, errs.Wrap(err, errs.CodeValidationFailed, "margin policy is not a valid document")
	}
	if dec.More() {
		return Policy{}, errs.New(errs.CodeValidationFailed, "margin policy has trailing data")
	}
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

func (p Policy) class(c VolatilityClass) (ClassParams, bool) {
	for _, x := range p.Classes {
		if x.Class == c {
			return x, true
		}
	}
	return ClassParams{}, false
}

func (p Policy) liquidity(l LiquidityClass) (LiquidityParams, bool) {
	for _, x := range p.Liquidity {
		if x.Class == l {
			return x, true
		}
	}
	return LiquidityParams{}, false
}

func (p Policy) venue(t VenueTier) (VenueParams, bool) {
	for _, x := range p.Venues {
		if x.Tier == t {
			return x, true
		}
	}
	return VenueParams{}, false
}

// LeverageCap returns the cap of a margin mode.
func (p Policy) LeverageCap(m MarginMode) money.BPS {
	switch m {
	case ModeIsolated:
		return p.LeverageCaps.IsolatedBPS
	case ModeCross:
		return p.LeverageCaps.CrossBPS
	}
	return p.LeverageCaps.CashBPS
}

// AssetProfile is how the policy sees one asset.
type AssetProfile struct {
	Volatility    VolatilityClass `json:"volatility"`
	Liquidity     LiquidityClass  `json:"liquidity"`
	Venue         VenueTier       `json:"venue"`
	RiskWeightBPS money.BPS       `json:"risk_weight_bps"`
}

// ProfileFor resolves an asset's profile from its rule (if any) and its
// registry risk class. ok is false when the asset is not eligible: its risk
// class is not mapped and no rule names a volatility class for it.
func (p Policy) ProfileFor(assetID, assetRiskClass string) (AssetProfile, bool) {
	prof := AssetProfile{Liquidity: p.DefaultLiquidity, Venue: p.DefaultVenue, RiskWeightBPS: 10_000}
	for _, m := range p.RiskClassMap {
		if m.AssetRiskClass == assetRiskClass {
			prof.Volatility = m.Volatility
		}
	}
	for _, r := range p.AssetRules {
		if r.AssetID != strings.ToLower(assetID) {
			continue
		}
		prof.RiskWeightBPS = r.RiskWeightBPS
		if r.Volatility != "" {
			prof.Volatility = r.Volatility
		}
		if r.Liquidity != "" {
			prof.Liquidity = r.Liquidity
		}
		if r.Venue != "" {
			prof.Venue = r.Venue
		}
	}
	return prof, prof.Volatility.Rank() >= 0
}

// hashOf is the hex sha256 of v's canonical JSON. Every value hashed here is
// built from strings and integers only, so encoding cannot fail; a failure
// is reported as a distinct, never-matching hash rather than a panic.
func hashOf(v any) string {
	b, err := audit.CanonicalJSON(v)
	if err != nil {
		sum := sha256.Sum256([]byte("unhashable:" + err.Error()))
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
