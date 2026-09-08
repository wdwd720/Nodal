package risk

import (
	"math/big"
	"sort"
	"time"

	"github.com/nodal/controlplane/internal/money"
)

// EvaluatorVersion is stored with every decision so a change in kernel
// semantics is distinguishable from a change in policy.
const EvaluatorVersion = "risk-kernel/1"

// Reason codes. Every violated check contributes its code; the decision
// carries the sorted, de-duplicated set.
const (
	ReasonInputInvalid                 = "RISK_INPUT_INVALID"
	ReasonPolicyMissing                = "RISK_POLICY_MISSING"
	ReasonKillSwitch                   = "RISK_KILL_SWITCH"
	ReasonAccountStatus                = "RISK_ACCOUNT_STATUS"
	ReasonMaxSingleTrade               = "RISK_MAX_SINGLE_TRADE"
	ReasonMaxPosition                  = "RISK_MAX_POSITION"
	ReasonMaxExposure                  = "RISK_MAX_EXPOSURE"
	ReasonConcentration                = "RISK_CONCENTRATION"
	ReasonNativeMarketConcentration    = "RISK_NATIVE_MARKET_CONCENTRATION"
	ReasonCreatorConcentration         = "RISK_CREATOR_CONCENTRATION"
	ReasonDailyLoss                    = "RISK_DAILY_LOSS"
	ReasonMaxDrawdown                  = "RISK_MAX_DRAWDOWN"
	ReasonOrderRate                    = "RISK_ORDER_RATE"
	ReasonSlippage                     = "RISK_SLIPPAGE"
	ReasonFee                          = "RISK_FEE"
	ReasonPriceImpact                  = "RISK_PRICE_IMPACT"
	ReasonQuoteAge                     = "RISK_QUOTE_AGE"
	ReasonLiquidity                    = "RISK_LIQUIDITY"
	ReasonAssetStatus                  = "RISK_ASSET_STATUS"
	ReasonAssetRiskClass               = "RISK_ASSET_RISK_CLASS"
	ReasonVenueNotAllowed              = "RISK_VENUE_NOT_ALLOWED"
	ReasonVenueStatus                  = "RISK_VENUE_STATUS"
	ReasonProviderHealth               = "RISK_PROVIDER_HEALTH"
	ReasonStaleData                    = "RISK_STALE_DATA"
	ReasonReconciliationPending        = "RISK_RECONCILIATION_PENDING"
	ReasonInsufficientBuyingPower      = "RISK_INSUFFICIENT_BUYING_POWER"
	ReasonEnvelopeStatus               = "RISK_ENVELOPE_STATUS"
	ReasonEnvelopeExhausted            = "RISK_ENVELOPE_EXHAUSTED"
	ReasonEnvelopeInstrumentNotAllowed = "RISK_ENVELOPE_INSTRUMENT_NOT_ALLOWED"
	ReasonEnvelopeAssetClassNotAllowed = "RISK_ENVELOPE_ASSET_CLASS_NOT_ALLOWED"
	ReasonEnvelopeVenueNotAllowed      = "RISK_ENVELOPE_VENUE_NOT_ALLOWED"
)

var allReasons = []string{
	ReasonInputInvalid, ReasonPolicyMissing, ReasonKillSwitch, ReasonAccountStatus, ReasonMaxSingleTrade,
	ReasonMaxPosition, ReasonMaxExposure, ReasonConcentration, ReasonDailyLoss, ReasonMaxDrawdown, ReasonOrderRate,
	ReasonSlippage, ReasonFee, ReasonPriceImpact, ReasonQuoteAge, ReasonLiquidity, ReasonAssetStatus,
	ReasonAssetRiskClass, ReasonVenueNotAllowed, ReasonVenueStatus, ReasonProviderHealth, ReasonStaleData,
	ReasonReconciliationPending, ReasonInsufficientBuyingPower, ReasonEnvelopeStatus, ReasonEnvelopeExhausted,
	ReasonEnvelopeInstrumentNotAllowed, ReasonEnvelopeAssetClassNotAllowed, ReasonEnvelopeVenueNotAllowed,
}

// ReasonCodes returns every reason code the kernel can emit, sorted.
func ReasonCodes() []string {
	out := append([]string(nil), allReasons...)
	sort.Strings(out)
	return out
}

// ResultingConstraints are the bounds the settlement compiler must respect.
// They are the policy bounds tightened by the intent's own constraints and,
// for the notional, by every remaining budget (single trade, position,
// concentration, total exposure, buying power, envelope, liquidity).
type ResultingConstraints struct {
	MaxNotionalUSD    money.USD `json:"max_notional_usd"`
	MaxSlippageBPS    money.BPS `json:"max_slippage_bps"`
	MaxFeeBPS         money.BPS `json:"max_fee_bps"`
	MaxPriceImpactBPS money.BPS `json:"max_price_impact_bps"`
	MaxQuoteAgeMS     int64     `json:"max_quote_age_ms"`
	MinLiquidityUSD   money.USD `json:"min_liquidity_usd"`
}

// Decision is the persisted outcome of one evaluation.
type Decision struct {
	Verdict     Verdict     `json:"decision"`
	Stage       Stage       `json:"stage"`
	ActionClass ActionClass `json:"action_class"`
	// EffectiveNotionalUSD is the notional the kernel evaluated: the intent
	// notional, or the delta for TARGET_EXPOSURE, or the position for
	// CLOSE_POSITION.
	EffectiveNotionalUSD money.USD            `json:"effective_notional_usd"`
	PolicyVersion        string               `json:"policy_version"`
	PolicyHash           string               `json:"policy_hash"`
	EvaluatorVersion     string               `json:"evaluator_version"`
	ReasonCodes          []string             `json:"reason_codes"`
	MatchedKillSwitches  []KillSwitch         `json:"matched_kill_switches"`
	Constraints          ResultingConstraints `json:"resulting_constraints"`
	// InputHash is the SHA-256 of the canonical JSON of the normalised input.
	InputHash   string    `json:"input_hash"`
	EvaluatedAt time.Time `json:"evaluated_at"`
	// Hash is the SHA-256 of the canonical JSON of the decision with Hash
	// blank: the determinism witness.
	Hash string `json:"hash"`
}

// CanonicalJSON renders the decision deterministically.
func (d Decision) CanonicalJSON() ([]byte, error) { return canonicalJSON(d) }

// ComputeHash returns the hash a decision with these fields must carry.
func (d Decision) ComputeHash() string {
	d.Hash = ""
	return hashOf(d)
}

// Evaluate is the pure risk function: no I/O, no clock, no randomness, no
// map-order dependence. It returns every violated reason.
func Evaluate(p Policy, in Input) Decision {
	in = in.normalized()
	rs := reasonSet{}
	d := Decision{
		Stage:               in.Stage,
		PolicyVersion:       p.Version,
		EvaluatorVersion:    EvaluatorVersion,
		EvaluatedAt:         in.Now,
		MatchedKillSwitches: []KillSwitch{},
	}
	if !in.Stage.Valid() || in.Now.IsZero() {
		rs.add(ReasonInputInvalid)
	}
	intentStage := in.Stage == StagePreTrade || in.Stage == StageFinal
	position := in.Account.PositionsUSD[in.Intent.InstrumentID]
	class, notional := ClassNewRisk, money.USD{}
	if intentStage {
		var ok bool
		class, notional, ok = classify(in.Intent, position)
		if !ok {
			rs.add(ReasonInputInvalid)
		}
	}
	d.ActionClass = class
	d.EffectiveNotionalUSD = notional

	complete := !p.Missing() && len(p.MissingLimits()) == 0
	if !complete {
		rs.add(ReasonPolicyMissing)
	}
	if intentStage {
		if matched := matchKillSwitches(p, in, class); len(matched) > 0 {
			rs.add(ReasonKillSwitch)
			d.MatchedKillSwitches = matched
		}
	}
	if complete {
		ev := evaluator{p: p, in: in, rs: rs, class: class, notional: notional, position: position, intentStage: intentStage}
		ev.accountChecks()
		if intentStage {
			ev.intentChecks()
			ev.nativeMarketChecks()
		}
		d.Constraints = ev.constraints()
	}
	d.ReasonCodes = rs.sorted()
	d.Verdict = Reject
	if len(d.ReasonCodes) == 0 {
		d.Verdict = Allow
	}
	if !p.Missing() {
		d.PolicyHash = p.Hash()
	}
	d.InputHash = hashOf(in)
	d.Hash = d.ComputeHash()
	return d
}

// classify maps the intent action to an action class and the notional the
// kernel must evaluate. ok is false for an unknown action or a non-positive
// notional where one is required.
func classify(it Intent, position money.USD) (ActionClass, money.USD, bool) {
	switch it.Action {
	case ActionAcquireNotional:
		return ClassNewRisk, it.NotionalUSD, it.NotionalUSD.IsPositive()
	case ActionReduceNotional:
		return ClassReduceRisk, it.NotionalUSD, it.NotionalUSD.IsPositive()
	case ActionClosePosition:
		if position.IsNegative() {
			return ClassReduceRisk, money.USD{}, true
		}
		return ClassReduceRisk, position, true
	case ActionTargetExposure:
		if it.TargetExposureUSD.IsNegative() {
			return ClassReduceRisk, money.USD{}, false
		}
		delta, err := it.TargetExposureUSD.Sub(position)
		if err != nil {
			return ClassNewRisk, money.USD{}, false
		}
		if delta.IsPositive() {
			return ClassNewRisk, delta, true
		}
		reduce, err := delta.NegChecked()
		if err != nil {
			return ClassReduceRisk, money.USD{}, false
		}
		return ClassReduceRisk, reduce, true
	}
	return ClassNewRisk, money.USD{}, false
}

// matchKillSwitches returns the active switches that block the action class
// for this intent, sorted.
func matchKillSwitches(p Policy, in Input, class ActionClass) []KillSwitch {
	allowDuringKill := p.AllowRiskReductionDuringKill != nil && *p.AllowRiskReductionDuringKill
	blockOnFreeze := p.BlockRiskReductionOnAccountFreeze != nil && *p.BlockRiskReductionOnAccountFreeze
	providers := map[string]struct{}{}
	if in.Intent.Provider != "" {
		providers[in.Intent.Provider] = struct{}{}
	}
	if in.Market.Quote != nil && in.Market.Quote.Provider != "" {
		providers[in.Market.Quote.Provider] = struct{}{}
	}
	var matched []KillSwitch
	for _, ks := range in.KillSwitches {
		if !ks.matches(in.Intent, providers) {
			continue
		}
		if ks.blocks(class, allowDuringKill, blockOnFreeze) {
			matched = append(matched, ks)
		}
	}
	sortSwitches(matched)
	return matched
}

// matches reports whether the switch's scope covers this intent. A scope of
// "*" covers every value of the dimension; switches scoped to an agent,
// strategy version or model only match intents that carry one. Unknown
// kinds match everything (fail closed).
func (ks KillSwitch) matches(it Intent, providers map[string]struct{}) bool {
	scoped := func(value string) bool {
		return value != "" && (ks.Scope == KillScopeAll || ks.Scope == value)
	}
	switch ks.Kind {
	case KillGlobalNewRisk:
		return true
	case KillFundingDisable, KillWithdrawalsDisable:
		return false // not a trade action class
	case KillAccountFreeze:
		return ks.Scope == KillScopeAll || scoped(it.AccountID)
	case KillAgentPause:
		return scoped(it.AgentID)
	case KillStrategyVersionDisable:
		return scoped(it.StrategyVersionID)
	case KillVenueDisable:
		return ks.Scope == KillScopeAll || scoped(it.Venue)
	case KillInstrumentCloseOnly, KillInstrumentHalt:
		return ks.Scope == KillScopeAll || scoped(it.InstrumentID)
	case KillChainDisable:
		return ks.Scope == KillScopeAll || scoped(it.Chain)
	case KillProviderDisable:
		if ks.Scope == KillScopeAll {
			return true
		}
		_, ok := providers[ks.Scope]
		return ok
	case KillModelDisable:
		return scoped(it.ModelID)
	}
	return true
}

// blocks applies the §2 action-class table.
func (ks KillSwitch) blocks(class ActionClass, allowDuringKill, blockOnFreeze bool) bool {
	if class != ClassReduceRisk {
		return true
	}
	switch ks.Kind {
	case KillInstrumentHalt, KillChainDisable, KillProviderDisable:
		return true
	case KillAccountFreeze:
		return blockOnFreeze
	case KillGlobalNewRisk:
		return !allowDuringKill
	case KillAgentPause, KillStrategyVersionDisable, KillVenueDisable, KillInstrumentCloseOnly, KillModelDisable:
		return false
	}
	return true
}

type evaluator struct {
	p           Policy
	in          Input
	rs          reasonSet
	class       ActionClass
	notional    money.USD
	position    money.USD
	intentStage bool
}

// accountChecks run at every stage: account status, loss budgets, total
// exposure, reconciliation, order rate, data freshness and provider health.
func (e *evaluator) accountChecks() {
	acct := e.in.Account
	switch e.class {
	case ClassReduceRisk:
		if !acct.Status.AllowsRiskReduction() {
			e.rs.add(ReasonAccountStatus)
		}
	default:
		if !acct.Status.AllowsNewRisk() {
			e.rs.add(ReasonAccountStatus)
		}
	}
	if e.class == ClassNewRisk {
		if acct.DailyRealizedLossUSD.Cmp(*e.p.MaxDailyLossUSD) >= 0 {
			e.rs.add(ReasonDailyLoss)
		}
		if acct.DrawdownUSD.Cmp(*e.p.MaxDrawdownUSD) >= 0 {
			e.rs.add(ReasonMaxDrawdown)
		}
		after, err := acct.TotalExposureUSD.Add(e.notional)
		if err != nil || after.Cmp(*e.p.MaxTotalExposureUSD) > 0 {
			e.rs.add(ReasonMaxExposure)
		}
	}
	if acct.UnresolvedMaterialMismatches > 0 {
		e.rs.add(ReasonReconciliationPending)
	}
	if e.intentStage {
		limit := *e.p.MaxOrdersPerHour
		if env := e.in.Envelope; env != nil && env.MaxOrderRatePerHour > 0 && env.MaxOrderRatePerHour < limit {
			limit = env.MaxOrderRatePerHour
		}
		if acct.OrdersInWindow >= limit {
			e.rs.add(ReasonOrderRate)
		}
	}
	for _, kind := range sortedKeys(e.in.Market.DataFreshness) {
		age := e.in.Market.DataFreshness[kind]
		bound := e.p.MaxDataAgeMS[kind]
		if age.DeclaredMaxAgeMS > 0 && (bound <= 0 || age.DeclaredMaxAgeMS < bound) {
			bound = age.DeclaredMaxAgeMS
		}
		if bound <= 0 || age.AgeMS < 0 || age.AgeMS > bound {
			e.rs.add(ReasonStaleData)
		}
	}
	for _, provider := range sortedKeys(e.in.Market.ProviderHealth) {
		if !contains(e.p.AllowedProviderHealth, e.in.Market.ProviderHealth[provider]) {
			e.rs.add(ReasonProviderHealth)
		}
	}
	if e.intentStage {
		if len(e.in.Market.ProviderHealth) == 0 {
			e.rs.add(ReasonProviderHealth)
		}
		for _, required := range []string{e.in.Intent.Provider, quoteProvider(e.in.Market.Quote)} {
			if required == "" {
				continue
			}
			if _, ok := e.in.Market.ProviderHealth[required]; !ok {
				e.rs.add(ReasonProviderHealth)
			}
		}
	}
}

func quoteProvider(q *Quote) string {
	if q == nil {
		return ""
	}
	return q.Provider
}

// intentChecks run at PRE_TRADE and FINAL: asset and venue state, sizing,
// buying power, envelope, quote quality and liquidity.
func (e *evaluator) intentChecks() {
	it := e.in.Intent
	m := e.in.Market
	switch e.class {
	case ClassReduceRisk:
		if !it.AssetStatus.AllowsReducingExposure() {
			e.rs.add(ReasonAssetStatus)
		}
	default:
		if !it.AssetStatus.AllowsIncreasingExposure() {
			e.rs.add(ReasonAssetStatus)
		}
		if !contains(e.p.AllowedAssetRiskClasses, string(it.AssetRiskClass)) {
			e.rs.add(ReasonAssetRiskClass)
		}
	}
	if !contains(e.p.AllowedVenues, it.Venue) {
		e.rs.add(ReasonVenueNotAllowed)
	}
	if !contains(e.p.AllowedVenueStatuses, m.VenueStatus) {
		e.rs.add(ReasonVenueStatus)
	}

	if e.class == ClassNewRisk {
		if e.notional.Cmp(*e.p.MaxSingleTradeUSD) > 0 {
			e.rs.add(ReasonMaxSingleTrade)
		}
		posAfter, ok := e.positionAfter()
		if !ok || posAfter.Cmp(*e.p.MaxPositionUSD) > 0 {
			e.rs.add(ReasonMaxPosition)
		}
		if limit, lok := share(e.in.Account.PortfolioValueUSD, *e.p.MaxConcentrationBPS); !ok || !lok || posAfter.Cmp(limit) > 0 {
			e.rs.add(ReasonConcentration)
		}
		classAfter, err := e.in.Account.AssetClassExposureUSD[it.AssetClass].Add(e.notional)
		if limit, lok := share(e.in.Account.PortfolioValueUSD, *e.p.MaxAssetClassConcentrationBPS); err != nil || !lok || classAfter.Cmp(limit) > 0 {
			e.rs.add(ReasonConcentration)
		}
		if e.notional.Cmp(e.in.Account.AvailableNowUSD) > 0 {
			e.rs.add(ReasonInsufficientBuyingPower)
		}
		if e.in.Envelope != nil {
			e.envelopeChecks(posAfter)
		}
	}

	switch q := m.Quote; q {
	case nil:
		if e.in.Stage == StageFinal {
			e.rs.add(ReasonQuoteAge)
		}
	default:
		if q.SlippageBPS > tighten(*e.p.MaxSlippageBPS, it.Constraints.MaxSlippageBPS) {
			e.rs.add(ReasonSlippage)
		}
		if q.FeeBPS > tighten(*e.p.MaxFeeBPS, it.Constraints.MaxFeeBPS) {
			e.rs.add(ReasonFee)
		}
		if q.PriceImpactBPS > tighten(*e.p.MaxPriceImpactBPS, it.Constraints.MaxPriceImpactBPS) {
			e.rs.add(ReasonPriceImpact)
		}
		if q.ReceivedAt.IsZero() {
			e.rs.add(ReasonQuoteAge)
		} else if age := e.in.Now.Sub(q.ReceivedAt).Milliseconds(); age < 0 || age > *e.p.MaxQuoteAgeMS {
			e.rs.add(ReasonQuoteAge)
		}
		if !q.ExpiresAt.IsZero() && !e.in.Now.Before(q.ExpiresAt) {
			e.rs.add(ReasonQuoteAge)
		}
	}

	switch liq := m.LiquidityUSD; liq {
	case nil:
		e.rs.add(ReasonLiquidity)
	default:
		if e.class == ClassNewRisk && liq.Cmp(*e.p.MinLiquidityUSD) < 0 {
			e.rs.add(ReasonLiquidity)
		}
		if e.notional.Cmp(*liq) > 0 {
			e.rs.add(ReasonLiquidity)
		}
	}
}

// nativeMarketChecks evaluates the two limits PART XXXII names for the
// Nodal-native economy: native-market concentration and creator concentration.
//
// It runs only when the caller supplied a NativeMarketSnapshot. An intent that
// is not a native trade has none, and neither limit applies to it.
//
// # Why there is no USD anywhere in here
//
// Every other limit in this kernel is denominated in USD. These two cannot be,
// and that is a property of what they constrain rather than a shortcut. A
// Credit has no approved external value (PART LIV), so a USD limit on a native
// position would need an exchange rate nobody set. Both limits are therefore
// ratios: units held against units outstanding, and Credits spent on one
// creator against Credits spent on all of them.
//
// # Why spend and not value
//
// Creator concentration could have been measured on holdings valued at each
// market's current price. It is not, because the holder of a thinly traded
// native asset can move that price -- so the limit would be one the person it
// constrains can move. Cost basis is the number nobody can rewrite.
//
// # Both denominators are chosen so the FIRST trade is not automatically a
// violation
//
// See NativeMarketSnapshot. A share of the float, or a share of native spend,
// is 100% for the opening trade in a market and for an account's first native
// purchase respectively -- so limits built on them would refuse everybody's
// first trade forever, which is a broken market rather than a control.
//
// A zero denominator is still not a violation, for the same reason: an account
// that holds nothing of an asset with no supply is not concentrated in it.
func (e *evaluator) nativeMarketChecks() {
	n := e.in.NativeMarket
	if n == nil {
		return
	}
	if e.p.MaxNativeMarketConcentrationBPS != nil &&
		shareExceeds(n.HoldingAfter, n.TotalSupply, *e.p.MaxNativeMarketConcentrationBPS) {
		e.rs.add(ReasonNativeMarketConcentration)
	}
	if e.p.MaxCreatorConcentrationBPS != nil &&
		shareExceeds(n.SpendOnThisCreatorAfter, n.CreditBaseAfter, *e.p.MaxCreatorConcentrationBPS) {
		e.rs.add(ReasonCreatorConcentration)
	}
}

// shareExceeds reports whether part/whole is above limit, in basis points,
// using integer arithmetic only.
//
// It compares part * 10000 against whole * limit rather than dividing, so
// there is no rounding step to argue about and no float anywhere. A
// non-positive whole is never a violation.
func shareExceeds(part, whole money.Quantity, limit money.BPS) bool {
	if !whole.IsPositive() || !part.IsPositive() {
		return false
	}
	left := new(big.Int).Mul(part.BigInt(), big.NewInt(int64(money.OneHundredPercent)))
	right := new(big.Int).Mul(whole.BigInt(), big.NewInt(int64(limit)))
	return left.Cmp(right) > 0
}

func (e *evaluator) envelopeChecks(posAfter money.USD) {
	env := e.in.Envelope
	it := e.in.Intent
	if env.Status != EnvelopeActive {
		e.rs.add(ReasonEnvelopeStatus)
	}
	if env.ExpiresAt != nil && !e.in.Now.Before(*env.ExpiresAt) {
		e.rs.add(ReasonEnvelopeStatus)
	}
	if e.notional.Cmp(env.AvailableUSD) > 0 {
		e.rs.add(ReasonEnvelopeExhausted)
	}
	if !contains(env.AllowedInstruments, it.InstrumentID) {
		e.rs.add(ReasonEnvelopeInstrumentNotAllowed)
	}
	if !contains(env.AllowedAssetClasses, it.AssetClass) {
		e.rs.add(ReasonEnvelopeAssetClassNotAllowed)
	}
	if !contains(env.AllowedVenues, it.Venue) {
		e.rs.add(ReasonEnvelopeVenueNotAllowed)
	}
	if env.MaxSingleTradeUSD.IsPositive() && e.notional.Cmp(env.MaxSingleTradeUSD) > 0 {
		e.rs.add(ReasonMaxSingleTrade)
	}
	if env.MaxPositionUSD.IsPositive() && posAfter.Cmp(env.MaxPositionUSD) > 0 {
		e.rs.add(ReasonMaxPosition)
	}
	if env.MaxDailyLossUSD.IsPositive() && env.DailyLossUSD.Cmp(env.MaxDailyLossUSD) >= 0 {
		e.rs.add(ReasonDailyLoss)
	}
	if env.MaxDrawdownUSD.IsPositive() && env.DrawdownUSD.Cmp(env.MaxDrawdownUSD) >= 0 {
		e.rs.add(ReasonMaxDrawdown)
	}
}

// positionAfter is the instrument position after the evaluated notional is
// added; ok is false on overflow.
func (e *evaluator) positionAfter() (money.USD, bool) {
	after, err := e.position.Add(e.notional)
	if err != nil {
		return money.MaxUSD(), false
	}
	return after, true
}

// constraints computes the bounds the plan must respect, whatever the
// verdict. For NEW_RISK the notional bound is the smallest remaining budget;
// for REDUCE_RISK it is the current (non-negative) position.
func (e *evaluator) constraints() ResultingConstraints {
	it := e.in.Intent
	c := ResultingConstraints{
		MaxSlippageBPS:    tighten(*e.p.MaxSlippageBPS, it.Constraints.MaxSlippageBPS),
		MaxFeeBPS:         tighten(*e.p.MaxFeeBPS, it.Constraints.MaxFeeBPS),
		MaxPriceImpactBPS: tighten(*e.p.MaxPriceImpactBPS, it.Constraints.MaxPriceImpactBPS),
		MaxQuoteAgeMS:     *e.p.MaxQuoteAgeMS,
		MinLiquidityUSD:   *e.p.MinLiquidityUSD,
	}
	if e.class == ClassReduceRisk {
		if e.position.IsPositive() {
			c.MaxNotionalUSD = e.position
		}
		return c
	}
	acct := e.in.Account
	room := []money.USD{
		*e.p.MaxSingleTradeUSD,
		remaining(*e.p.MaxPositionUSD, e.position),
		remaining(*e.p.MaxTotalExposureUSD, acct.TotalExposureUSD),
		acct.AvailableNowUSD,
	}
	if limit, ok := share(acct.PortfolioValueUSD, *e.p.MaxConcentrationBPS); ok {
		room = append(room, remaining(limit, e.position))
	} else {
		room = append(room, money.USD{})
	}
	if limit, ok := share(acct.PortfolioValueUSD, *e.p.MaxAssetClassConcentrationBPS); ok {
		room = append(room, remaining(limit, acct.AssetClassExposureUSD[it.AssetClass]))
	} else {
		room = append(room, money.USD{})
	}
	if env := e.in.Envelope; env != nil {
		room = append(room, env.AvailableUSD)
		if env.MaxSingleTradeUSD.IsPositive() {
			room = append(room, env.MaxSingleTradeUSD)
		}
		if env.MaxPositionUSD.IsPositive() {
			room = append(room, remaining(env.MaxPositionUSD, e.position))
		}
	}
	if liq := e.in.Market.LiquidityUSD; liq != nil {
		room = append(room, *liq)
	}
	bound := room[0]
	for _, r := range room[1:] {
		if r.Cmp(bound) < 0 {
			bound = r
		}
	}
	if bound.IsNegative() {
		bound = money.USD{}
	}
	c.MaxNotionalUSD = bound
	return c
}

// tighten returns the stricter of the policy bound and the intent's own
// bound (zero meaning unspecified).
func tighten(policy, intent money.BPS) money.BPS {
	if intent > 0 && intent < policy {
		return intent
	}
	return policy
}

// share returns portfolio × bps rounded down; ok is false on overflow.
func share(portfolio money.USD, bps money.BPS) (money.USD, bool) {
	v, err := portfolio.MulBPS(bps, money.RoundDown)
	if err != nil {
		return money.USD{}, false
	}
	return v, true
}

// remaining returns limit − used, or zero when nothing remains or the
// subtraction overflows.
func remaining(limit, used money.USD) money.USD {
	r, err := limit.Sub(used)
	if err != nil || r.IsNegative() {
		return money.USD{}
	}
	return r
}

type reasonSet map[string]struct{}

func (r reasonSet) add(code string) { r[code] = struct{}{} }

func (r reasonSet) sorted() []string {
	out := make([]string, 0, len(r))
	for c := range r {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
