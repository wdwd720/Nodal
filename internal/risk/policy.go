package risk

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Scope is the risk_policies.scope of a policy row.
type Scope string

// Policy scopes. The effective policy is GLOBAL ∧ ACCOUNT ∧ AGENT.
const (
	ScopeGlobal  Scope = "GLOBAL"
	ScopeAccount Scope = "ACCOUNT"
	ScopeAgent   Scope = "AGENT"
)

// GlobalScopeID is the scope_id of GLOBAL rows.
const GlobalScopeID = "*"

// Valid reports whether s is a known scope.
func (s Scope) Valid() bool {
	switch s {
	case ScopeGlobal, ScopeAccount, ScopeAgent:
		return true
	}
	return false
}

// Provider health states (PART 79).
const (
	HealthHealthy   = "HEALTHY"
	HealthDegraded  = "DEGRADED"
	HealthUnhealthy = "UNHEALTHY"
	HealthDisabled  = "DISABLED"
)

// Venue statuses (venues.status).
const (
	VenueActive   = "ACTIVE"
	VenueDegraded = "DEGRADED"
	VenueDisabled = "DISABLED"
)

var (
	providerHealths = []string{HealthHealthy, HealthDegraded, HealthUnhealthy, HealthDisabled}
	venueStatuses   = []string{VenueActive, VenueDegraded, VenueDisabled}
	riskClasses     = []string{string(assets.RiskSettlement), string(assets.RiskMajor), string(assets.RiskStandard), string(assets.RiskSpeculative), string(assets.RiskUnsupported)}
)

// Policy is the typed risk limits document stored in risk_policies.rules.
// A nil limit means "this scope does not constrain it"; a GLOBAL policy
// must set every limit (Validate). Lists are allowlists: nil means
// unconstrained by this scope, an explicit empty list allows nothing.
// Version is the row version (or the composite version after Compose) and
// is never part of the rules JSON.
type Policy struct {
	Version string `json:"-"`

	MaxSingleTradeUSD             *money.USD `json:"max_single_trade_usd"`
	MaxPositionUSD                *money.USD `json:"max_position_usd"`
	MaxTotalExposureUSD           *money.USD `json:"max_total_exposure_usd"`
	MaxConcentrationBPS           *money.BPS `json:"max_concentration_bps"`
	MaxAssetClassConcentrationBPS *money.BPS `json:"max_asset_class_concentration_bps"`
	MaxDailyLossUSD               *money.USD `json:"max_daily_loss_usd"`
	MaxDrawdownUSD                *money.USD `json:"max_drawdown_usd"`
	MaxOrdersPerHour              *int       `json:"max_orders_per_hour"`
	MaxSlippageBPS                *money.BPS `json:"max_slippage_bps"`
	MaxFeeBPS                     *money.BPS `json:"max_fee_bps"`
	MaxPriceImpactBPS             *money.BPS `json:"max_price_impact_bps"`
	MaxQuoteAgeMS                 *int64     `json:"max_quote_age_ms"`
	MinLiquidityUSD               *money.USD `json:"min_liquidity_usd"`

	// The two limits PART XXXII names for the Nodal-native economy. Both are
	// pure RATIOS and neither has a USD term, which is not an omission: a
	// Credit has no approved external value, so a limit on a native position
	// expressed in USD would require an exchange rate nobody set (PART LIV).
	//
	// MaxNativeMarketConcentrationBPS caps the share of an asset's TOTAL
	// SUPPLY a single account may hold after a trade. It is the limit that
	// stops one holder owning a market, which is the position from which its
	// price can be set at will.
	//
	// Total supply rather than the traded float: the first buyer in a new
	// market holds all of the float, so a limit against it would refuse the
	// opening trade of every market. See NativeMarketSnapshot.
	MaxNativeMarketConcentrationBPS *money.BPS `json:"max_native_market_concentration_bps"`
	// MaxCreatorConcentrationBPS caps the share of an account's Credit
	// position that may be committed to the assets of ONE creator.
	//
	// It is measured on Credits SPENT against Credits spent plus Credits still
	// spendable -- not on holdings valued at the current price. A holder of a
	// native asset can move that price, so a limit denominated in it would be
	// a limit the person it constrains can move. Cost basis is the number
	// nobody can rewrite.
	MaxCreatorConcentrationBPS *money.BPS `json:"max_creator_concentration_bps"`

	// MaxDataAgeMS caps the age of each data dependency kind the input
	// reports (for example "price", "wallet_event"). Compose keeps the
	// smallest cap per kind.
	MaxDataAgeMS map[string]int64 `json:"max_data_age_ms"`

	AllowedVenues           []string `json:"allowed_venues"`
	AllowedVenueStatuses    []string `json:"allowed_venue_statuses"`
	AllowedAssetRiskClasses []string `json:"allowed_asset_risk_classes"`
	AllowedProviderHealth   []string `json:"allowed_provider_health"`

	// AllowRiskReductionDuringKill lets REDUCE_RISK proceed under
	// GLOBAL_NEW_RISK_KILL. Compose ANDs it (false wins).
	AllowRiskReductionDuringKill *bool `json:"allow_risk_reduction_during_kill"`
	// BlockRiskReductionOnAccountFreeze makes ACCOUNT_FREEZE block REDUCE_RISK
	// too. Compose ORs it (true wins). Default: reductions allowed.
	BlockRiskReductionOnAccountFreeze *bool `json:"block_risk_reduction_on_account_freeze"`
}

// Missing reports whether p is the zero "no policy recorded" value.
func (p Policy) Missing() bool { return p.Version == "" }

// ParsePolicy decodes a rules document. It rejects unknown keys, fractional
// or exponent numbers where integers are expected, JSON numbers where USD
// strings are expected, trailing data and unknown enum members. It does not
// require completeness; use Validate(ScopeGlobal) for that.
func ParsePolicy(raw json.RawMessage) (Policy, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return Policy{}, errs.New(errs.CodeValidationFailed, "risk: policy rules must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var p Policy
	if err := dec.Decode(&p); err != nil {
		return Policy{}, errs.Wrap(err, errs.CodeValidationFailed, "risk: policy rules do not parse: "+err.Error())
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Policy{}, errs.New(errs.CodeValidationFailed, "risk: policy rules contain trailing data")
	}
	p.normalize()
	if err := p.validateValues(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// MustParsePolicy is ParsePolicy for compiled-in documents; it panics on error.
func MustParsePolicy(raw json.RawMessage) Policy {
	p, err := ParsePolicy(raw)
	if err != nil {
		panic("risk: MustParsePolicy: " + err.Error())
	}
	return p
}

// Hash returns the hex SHA-256 of the canonical JSON of the rules (Version
// excluded). It is the value stored in risk_policies.rules_hash and
// risk_decisions.policy_hash.
func (p Policy) Hash() string { return hashOf(p) }

// CanonicalJSON renders the normalised rules document.
func (p Policy) CanonicalJSON() ([]byte, error) { return canonicalJSON(p) }

func (p *Policy) normalize() {
	if p.MaxDataAgeMS == nil {
		p.MaxDataAgeMS = map[string]int64{}
	}
	p.AllowedVenues = sortedUniqueKeepNil(p.AllowedVenues)
	p.AllowedVenueStatuses = sortedUniqueKeepNil(p.AllowedVenueStatuses)
	p.AllowedAssetRiskClasses = sortedUniqueKeepNil(p.AllowedAssetRiskClasses)
	p.AllowedProviderHealth = sortedUniqueKeepNil(p.AllowedProviderHealth)
}

// validateValues checks ranges and enum members of whatever is set.
func (p Policy) validateValues() error {
	var problems []string
	add := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }
	nonNegativeUSD := func(name string, v *money.USD) {
		if v != nil && v.IsNegative() {
			add("%s: must not be negative", name)
		}
	}
	boundedBPS := func(name string, v *money.BPS) {
		if v != nil && (*v < 0 || *v > money.OneHundredPercent) {
			add("%s: must be between 0 and 10000 basis points", name)
		}
	}
	nonNegativeUSD("max_single_trade_usd", p.MaxSingleTradeUSD)
	nonNegativeUSD("max_position_usd", p.MaxPositionUSD)
	nonNegativeUSD("max_total_exposure_usd", p.MaxTotalExposureUSD)
	nonNegativeUSD("max_daily_loss_usd", p.MaxDailyLossUSD)
	nonNegativeUSD("max_drawdown_usd", p.MaxDrawdownUSD)
	nonNegativeUSD("min_liquidity_usd", p.MinLiquidityUSD)
	boundedBPS("max_concentration_bps", p.MaxConcentrationBPS)
	boundedBPS("max_asset_class_concentration_bps", p.MaxAssetClassConcentrationBPS)
	boundedBPS("max_native_market_concentration_bps", p.MaxNativeMarketConcentrationBPS)
	boundedBPS("max_creator_concentration_bps", p.MaxCreatorConcentrationBPS)
	boundedBPS("max_slippage_bps", p.MaxSlippageBPS)
	boundedBPS("max_fee_bps", p.MaxFeeBPS)
	boundedBPS("max_price_impact_bps", p.MaxPriceImpactBPS)
	if p.MaxOrdersPerHour != nil && *p.MaxOrdersPerHour < 0 {
		add("max_orders_per_hour: must not be negative")
	}
	if p.MaxQuoteAgeMS != nil && *p.MaxQuoteAgeMS < 0 {
		add("max_quote_age_ms: must not be negative")
	}
	for k, v := range p.MaxDataAgeMS {
		if strings.TrimSpace(k) == "" {
			add("max_data_age_ms: empty data kind")
		}
		if v <= 0 {
			add("max_data_age_ms[%s]: must be positive", k)
		}
	}
	for _, v := range p.AllowedVenues {
		if strings.TrimSpace(v) == "" || v != strings.TrimSpace(v) {
			add("allowed_venues: entries must be non-empty and trimmed")
			break
		}
	}
	for _, s := range p.AllowedVenueStatuses {
		if !contains(venueStatuses, s) {
			add("allowed_venue_statuses: %q is not a venue status", s)
		}
	}
	for _, s := range p.AllowedAssetRiskClasses {
		if !contains(riskClasses, s) {
			add("allowed_asset_risk_classes: %q is not an asset risk class", s)
		}
	}
	for _, s := range p.AllowedProviderHealth {
		if !contains(providerHealths, s) {
			add("allowed_provider_health: %q is not a provider health state", s)
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return errs.New(errs.CodeValidationFailed, "risk: invalid policy rules").WithField("problems", problems)
}

// Validate checks values and, for ScopeGlobal, completeness: a GLOBAL policy
// must set every limit and every allowlist so the composed policy can never
// be silently unbounded.
func (p Policy) Validate(scope Scope) error {
	if err := p.validateValues(); err != nil {
		return err
	}
	if scope != ScopeGlobal {
		return nil
	}
	if missing := p.MissingLimits(); len(missing) > 0 {
		return errs.New(errs.CodeValidationFailed, "risk: GLOBAL policy must set every limit").WithField("missing", missing)
	}
	return nil
}

// MissingLimits lists the JSON keys of limits and allowlists that are unset.
// Evaluate rejects with RISK_POLICY_MISSING when the list is non-empty.
func (p Policy) MissingLimits() []string {
	var missing []string
	check := func(name string, isNil bool) {
		if isNil {
			missing = append(missing, name)
		}
	}
	check("max_single_trade_usd", p.MaxSingleTradeUSD == nil)
	check("max_position_usd", p.MaxPositionUSD == nil)
	check("max_total_exposure_usd", p.MaxTotalExposureUSD == nil)
	check("max_concentration_bps", p.MaxConcentrationBPS == nil)
	check("max_asset_class_concentration_bps", p.MaxAssetClassConcentrationBPS == nil)
	check("max_native_market_concentration_bps", p.MaxNativeMarketConcentrationBPS == nil)
	check("max_creator_concentration_bps", p.MaxCreatorConcentrationBPS == nil)
	check("max_daily_loss_usd", p.MaxDailyLossUSD == nil)
	check("max_drawdown_usd", p.MaxDrawdownUSD == nil)
	check("max_orders_per_hour", p.MaxOrdersPerHour == nil)
	check("max_slippage_bps", p.MaxSlippageBPS == nil)
	check("max_fee_bps", p.MaxFeeBPS == nil)
	check("max_price_impact_bps", p.MaxPriceImpactBPS == nil)
	check("max_quote_age_ms", p.MaxQuoteAgeMS == nil)
	check("min_liquidity_usd", p.MinLiquidityUSD == nil)
	check("allowed_venues", p.AllowedVenues == nil)
	check("allowed_venue_statuses", p.AllowedVenueStatuses == nil)
	check("allowed_asset_risk_classes", p.AllowedAssetRiskClasses == nil)
	check("allowed_provider_health", p.AllowedProviderHealth == nil)
	check("allow_risk_reduction_during_kill", p.AllowRiskReductionDuringKill == nil)
	check("block_risk_reduction_on_account_freeze", p.BlockRiskReductionOnAccountFreeze == nil)
	sort.Strings(missing)
	return missing
}

// Compose builds the effective policy GLOBAL ∧ ACCOUNT ∧ AGENT. For every
// maximum the smallest set value wins; for the liquidity minimum the largest
// wins; per-kind data-age caps take the smallest per kind over the union of
// kinds; allowlists intersect over the scopes that set them;
// allow_risk_reduction_during_kill is ANDed and
// block_risk_reduction_on_account_freeze is ORed. Without a GLOBAL policy the
// result is the zero (missing) policy: ACCOUNT and AGENT rows only refine.
func Compose(global, account, agent *Policy) Policy {
	if global == nil || global.Missing() {
		return Policy{}
	}
	layers := []*Policy{global, account, agent}
	out := Policy{MaxDataAgeMS: map[string]int64{}}
	for _, l := range layers {
		if l == nil {
			continue
		}
		out.MaxSingleTradeUSD = minUSD(out.MaxSingleTradeUSD, l.MaxSingleTradeUSD)
		out.MaxPositionUSD = minUSD(out.MaxPositionUSD, l.MaxPositionUSD)
		out.MaxTotalExposureUSD = minUSD(out.MaxTotalExposureUSD, l.MaxTotalExposureUSD)
		out.MaxConcentrationBPS = minBPS(out.MaxConcentrationBPS, l.MaxConcentrationBPS)
		out.MaxAssetClassConcentrationBPS = minBPS(out.MaxAssetClassConcentrationBPS, l.MaxAssetClassConcentrationBPS)
		out.MaxNativeMarketConcentrationBPS = minBPS(out.MaxNativeMarketConcentrationBPS, l.MaxNativeMarketConcentrationBPS)
		out.MaxCreatorConcentrationBPS = minBPS(out.MaxCreatorConcentrationBPS, l.MaxCreatorConcentrationBPS)
		out.MaxDailyLossUSD = minUSD(out.MaxDailyLossUSD, l.MaxDailyLossUSD)
		out.MaxDrawdownUSD = minUSD(out.MaxDrawdownUSD, l.MaxDrawdownUSD)
		out.MaxOrdersPerHour = minInt(out.MaxOrdersPerHour, l.MaxOrdersPerHour)
		out.MaxSlippageBPS = minBPS(out.MaxSlippageBPS, l.MaxSlippageBPS)
		out.MaxFeeBPS = minBPS(out.MaxFeeBPS, l.MaxFeeBPS)
		out.MaxPriceImpactBPS = minBPS(out.MaxPriceImpactBPS, l.MaxPriceImpactBPS)
		out.MaxQuoteAgeMS = minInt64(out.MaxQuoteAgeMS, l.MaxQuoteAgeMS)
		out.MinLiquidityUSD = maxUSD(out.MinLiquidityUSD, l.MinLiquidityUSD)
		for k, v := range l.MaxDataAgeMS {
			if cur, ok := out.MaxDataAgeMS[k]; !ok || v < cur {
				out.MaxDataAgeMS[k] = v
			}
		}
		out.AllowedVenues = intersect(out.AllowedVenues, l.AllowedVenues)
		out.AllowedVenueStatuses = intersect(out.AllowedVenueStatuses, l.AllowedVenueStatuses)
		out.AllowedAssetRiskClasses = intersect(out.AllowedAssetRiskClasses, l.AllowedAssetRiskClasses)
		out.AllowedProviderHealth = intersect(out.AllowedProviderHealth, l.AllowedProviderHealth)
		out.AllowRiskReductionDuringKill = andBool(out.AllowRiskReductionDuringKill, l.AllowRiskReductionDuringKill)
		out.BlockRiskReductionOnAccountFreeze = orBool(out.BlockRiskReductionOnAccountFreeze, l.BlockRiskReductionOnAccountFreeze)
	}
	parts := []string{"GLOBAL=" + global.Version}
	if account != nil && !account.Missing() {
		parts = append(parts, "ACCOUNT="+account.Version)
	}
	if agent != nil && !agent.Missing() {
		parts = append(parts, "AGENT="+agent.Version)
	}
	out.Version = strings.Join(parts, ";")
	return out
}

func minUSD(a, b *money.USD) *money.USD {
	switch {
	case a == nil:
		return cloneUSD(b)
	case b == nil:
		return a
	case b.Cmp(*a) < 0:
		return cloneUSD(b)
	}
	return a
}

func maxUSD(a, b *money.USD) *money.USD {
	switch {
	case a == nil:
		return cloneUSD(b)
	case b == nil:
		return a
	case b.Cmp(*a) > 0:
		return cloneUSD(b)
	}
	return a
}

func cloneUSD(v *money.USD) *money.USD {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

func minBPS(a, b *money.BPS) *money.BPS {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		c := *b
		return &c
	case b == nil:
		return a
	case *b < *a:
		c := *b
		return &c
	}
	return a
}

func minInt(a, b *int) *int {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		c := *b
		return &c
	case b == nil:
		return a
	case *b < *a:
		c := *b
		return &c
	}
	return a
}

func minInt64(a, b *int64) *int64 {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		c := *b
		return &c
	case b == nil:
		return a
	case *b < *a:
		c := *b
		return &c
	}
	return a
}

func andBool(a, b *bool) *bool {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		c := *b
		return &c
	case b == nil:
		return a
	}
	c := *a && *b
	return &c
}

func orBool(a, b *bool) *bool {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		c := *b
		return &c
	case b == nil:
		return a
	}
	c := *a || *b
	return &c
}

// intersect returns the sorted intersection; a nil side is unconstrained.
func intersect(a, b []string) []string {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		return sortedUnique(b)
	case b == nil:
		return sortedUnique(a)
	}
	out := make([]string, 0, len(a))
	for _, s := range a {
		if contains(b, s) {
			out = append(out, s)
		}
	}
	return sortedUnique(out)
}

func sortedUnique(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func sortedUniqueKeepNil(in []string) []string {
	if in == nil {
		return nil
	}
	return sortedUnique(in)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// DefaultGlobalPolicyJSON is the compiled-in GLOBAL policy used only to seed
// a fresh deployment. These are INITIAL values chosen to be small; they
// require risk-desk sign-off (PART 58) and are replaced by an operator with
// risk:policy_write through Store.RecordPolicy. The venue allowlist is
// empty, so no trade is allowed anywhere until venues are listed explicitly.
const DefaultGlobalPolicyJSON = `{
  "max_single_trade_usd": "1000.00",
  "max_position_usd": "5000.00",
  "max_total_exposure_usd": "10000.00",
  "max_concentration_bps": 2500,
  "max_asset_class_concentration_bps": 5000,
  "max_native_market_concentration_bps": 2000,
  "max_creator_concentration_bps": 3000,
  "max_daily_loss_usd": "500.00",
  "max_drawdown_usd": "1000.00",
  "max_orders_per_hour": 30,
  "max_slippage_bps": 100,
  "max_fee_bps": 50,
  "max_price_impact_bps": 100,
  "max_quote_age_ms": 3000,
  "min_liquidity_usd": "50000.00",
  "max_data_age_ms": {"price": 500, "wallet_event": 2000},
  "allowed_venues": [],
  "allowed_venue_statuses": ["ACTIVE"],
  "allowed_asset_risk_classes": ["SETTLEMENT", "MAJOR"],
  "allowed_provider_health": ["HEALTHY"],
  "allow_risk_reduction_during_kill": true,
  "block_risk_reduction_on_account_freeze": false
}`
