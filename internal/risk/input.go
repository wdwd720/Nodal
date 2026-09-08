package risk

import (
	"sort"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/money"
)

// Stage is the evaluation stage (risk_decisions.stage).
type Stage string

// Stages.
const (
	StagePreTrade   Stage = "PRE_TRADE"
	StageFinal      Stage = "FINAL"
	StageContinuous Stage = "CONTINUOUS"
)

// Valid reports whether s is a known stage.
func (s Stage) Valid() bool {
	switch s {
	case StagePreTrade, StageFinal, StageContinuous:
		return true
	}
	return false
}

// Action is the trade_intents.action of the intent under evaluation.
type Action string

// Intent actions (PART 35).
const (
	ActionAcquireNotional Action = "ACQUIRE_NOTIONAL"
	ActionReduceNotional  Action = "REDUCE_NOTIONAL"
	ActionClosePosition   Action = "CLOSE_POSITION"
	ActionTargetExposure  Action = "TARGET_EXPOSURE"
)

// ActionClass is the kill-switch action class of an intent (§2).
type ActionClass string

// Action classes the kernel evaluates.
const (
	ClassNewRisk    ActionClass = "NEW_RISK"
	ClassReduceRisk ActionClass = "REDUCE_RISK"
)

// Verdict is the persisted decision value.
type Verdict string

// Verdicts.
const (
	Allow  Verdict = "ALLOW"
	Reject Verdict = "REJECT"
)

// Envelope statuses (capital_envelopes.status).
const (
	EnvelopeDraft     = "DRAFT"
	EnvelopeActive    = "ACTIVE"
	EnvelopePaused    = "PAUSED"
	EnvelopeExhausted = "EXHAUSTED"
	EnvelopeExpired   = "EXPIRED"
	EnvelopeRevoked   = "REVOKED"
)

// Kill-switch kinds (kill_switches.kind) and the wildcard scope.
const (
	KillGlobalNewRisk          = "GLOBAL_NEW_RISK_KILL"
	KillAccountFreeze          = "ACCOUNT_FREEZE"
	KillAgentPause             = "AGENT_PAUSE"
	KillStrategyVersionDisable = "STRATEGY_VERSION_DISABLE"
	KillVenueDisable           = "VENUE_DISABLE"
	KillInstrumentCloseOnly    = "INSTRUMENT_CLOSE_ONLY"
	KillInstrumentHalt         = "INSTRUMENT_HALT"
	KillChainDisable           = "CHAIN_DISABLE_NEW_ACTIONS"
	KillProviderDisable        = "PROVIDER_DISABLE_NEW_ACTIONS"
	KillFundingDisable         = "FUNDING_DISABLE"
	KillWithdrawalsDisable     = "WITHDRAWALS_DISABLE"
	KillModelDisable           = "MODEL_DISABLE"

	KillScopeAll = "*"
)

// Input is the typed evaluation snapshot. Every value the decision depends
// on is inside it; the kernel reads nothing else.
type Input struct {
	Stage    Stage             `json:"stage"`
	Intent   Intent            `json:"intent"`
	Account  AccountSnapshot   `json:"account"`
	Envelope *EnvelopeSnapshot `json:"envelope"`
	Market   MarketSnapshot    `json:"market"`
	// NativeMarket is present only for a Nodal-native trade. Nil means the
	// intent is not one, and the two native limits are not evaluated.
	NativeMarket *NativeMarketSnapshot `json:"native_market"`
	// KillSwitches lists the currently active switches (kind, scope).
	KillSwitches []KillSwitch `json:"kill_switches"`
	// Now is the evaluation time supplied by the caller.
	Now time.Time `json:"now"`
}

// Intent is the part of the trade intent the kernel evaluates. IDs are
// canonical UUID strings; empty means absent (for example a manual intent
// has no AgentID).
type Intent struct {
	ID                string `json:"id"`
	AccountID         string `json:"account_id"`
	AgentID           string `json:"agent_id"`
	StrategyVersionID string `json:"strategy_version_id"`
	ModelID           string `json:"model_id"`

	Action Action `json:"action"`
	// NotionalUSD is the requested notional for ACQUIRE_NOTIONAL and
	// REDUCE_NOTIONAL (the caller converts a quantity into USD).
	NotionalUSD money.USD `json:"notional_usd"`
	// TargetExposureUSD is the target for TARGET_EXPOSURE.
	TargetExposureUSD money.USD `json:"target_exposure_usd"`

	InstrumentID   string           `json:"instrument_id"`
	AssetClass     string           `json:"asset_class"`
	AssetRiskClass assets.RiskClass `json:"asset_risk_class"`
	AssetStatus    assets.Status    `json:"asset_status"`
	Venue          string           `json:"venue"`
	Chain          string           `json:"chain"`
	Provider       string           `json:"provider"`

	Constraints IntentConstraints `json:"constraints"`
}

// IntentConstraints are the caller's own bounds; zero means unspecified.
// The kernel only ever tightens them.
type IntentConstraints struct {
	MaxSlippageBPS    money.BPS `json:"max_slippage_bps"`
	MaxFeeBPS         money.BPS `json:"max_fee_bps"`
	MaxPriceImpactBPS money.BPS `json:"max_price_impact_bps"`
}

// AccountSnapshot carries the buying-power engine output (FINANCIAL_MODEL
// §6) and the account-level risk figures.
type AccountSnapshot struct {
	Status accounts.Status `json:"status"`

	PortfolioValueUSD        money.USD `json:"portfolio_value_usd"`
	BuyingPowerUSD           money.USD `json:"buying_power_usd"`
	AvailableNowUSD          money.USD `json:"available_now_usd"`
	ReservedUSD              money.USD `json:"reserved_usd"`
	PendingUSD               money.USD `json:"pending_usd"`
	WithdrawableUSD          money.USD `json:"withdrawable_usd"`
	BuyingPowerPolicyVersion string    `json:"buying_power_policy_version"`
	AsOf                     time.Time `json:"as_of"`

	// PositionsUSD is the current USD exposure per instrument id;
	// AssetClassExposureUSD the same per asset class.
	PositionsUSD          map[string]money.USD `json:"positions_usd"`
	AssetClassExposureUSD map[string]money.USD `json:"asset_class_exposure_usd"`
	TotalExposureUSD      money.USD            `json:"total_exposure_usd"`

	DailyRealizedLossUSD money.USD `json:"daily_realized_loss_usd"`
	DrawdownUSD          money.USD `json:"drawdown_usd"`
	// OrdersInWindow is the persisted count of intents in the current
	// one-hour window (Store.CountOrders).
	OrdersInWindow               int `json:"orders_in_window"`
	UnresolvedMaterialMismatches int `json:"unresolved_material_mismatches"`
}

// EnvelopeSnapshot is the capital envelope bound to an autonomous agent
// (PART 24). Zero limits mean the envelope does not constrain that value.
type EnvelopeSnapshot struct {
	ID                  string     `json:"id"`
	Status              string     `json:"status"`
	AvailableUSD        money.USD  `json:"available_usd"`
	MaxSingleTradeUSD   money.USD  `json:"max_single_trade_usd"`
	MaxPositionUSD      money.USD  `json:"max_position_usd"`
	MaxDailyLossUSD     money.USD  `json:"max_daily_loss_usd"`
	MaxDrawdownUSD      money.USD  `json:"max_drawdown_usd"`
	DailyLossUSD        money.USD  `json:"daily_loss_usd"`
	DrawdownUSD         money.USD  `json:"drawdown_usd"`
	AllowedInstruments  []string   `json:"allowed_instruments"`
	AllowedAssetClasses []string   `json:"allowed_asset_classes"`
	AllowedVenues       []string   `json:"allowed_venues"`
	MaxOrderRatePerHour int        `json:"max_order_rate_per_hour"`
	ExpiresAt           *time.Time `json:"expires_at"`
}

// MarketSnapshot carries the quote (absent before planning), the venue
// status, the liquidity estimate (nil when unknown), provider health for
// every provider involved and the freshness of every data dependency.
type MarketSnapshot struct {
	Quote          *Quote             `json:"quote"`
	VenueStatus    string             `json:"venue_status"`
	LiquidityUSD   *money.USD         `json:"liquidity_usd"`
	ProviderHealth map[string]string  `json:"provider_health"`
	DataFreshness  map[string]DataAge `json:"data_freshness"`
}

// NativeMarketSnapshot is what the kernel needs to evaluate the two limits
// PART XXXII names for the Nodal-native economy.
//
// Every field is a base-unit COUNT or a Credit amount. There is no USD here
// and there is no price: a Credit has no approved external value, and a
// native-asset price is one the holder being constrained can move. Both
// limits are ratios of numbers nobody can rewrite.
//
// The caller supplies the snapshot; internal/risk never reads a market.
type NativeMarketSnapshot struct {
	MarketID string `json:"market_id"`
	AssetID  string `json:"asset_id"`
	// CreatorAccountID is whose asset this is. Creator concentration is
	// measured against it.
	CreatorAccountID string `json:"creator_account_id"`

	// HoldingAfter is the units this account would hold after the trade, and
	// TotalSupply is every unit that exists. Their ratio is the market
	// concentration.
	//
	// # Why total supply and not the float
	//
	// The obvious denominator is the float -- units the pool has sold. It is
	// unusable: the first buyer in a new market holds ALL of it, so a limit
	// against the float refuses the opening trade of every market that will
	// ever exist. That is not a risk control, it is a market that cannot open.
	//
	// Total supply is fixed at mint, is never zero, and no participant can
	// move it (PART XIII: there is exactly one mint path). A share of it is
	// therefore a number that means the same thing on day one and day one
	// thousand.
	HoldingAfter money.Quantity `json:"holding_after"`
	TotalSupply  money.Quantity `json:"total_supply"`

	// SpendOnThisCreatorAfter is the Credits this account would have committed
	// to this creator's assets after the trade, and CreditBaseAfter is every
	// Credit it has committed to native assets plus every Credit it can still
	// spend. Their ratio is the creator concentration: how much of what this
	// account has is riding on one person.
	//
	// # Why the base includes unspent Credits
	//
	// Measuring one creator's share of native SPEND alone has the same defect
	// as the float: an account's first native purchase is necessarily 100% of
	// its native spend, so every account's first trade would be refused. Adding
	// what the account can still spend makes the denominator the account's
	// actual Credit position, which is what "concentrated" is supposed to mean.
	//
	// # Why spend and not current value
	//
	// The numerator is cost basis, not what the holding is worth now. A holder
	// of a thinly traded native asset can move its price, so a limit measured
	// on current value would be a limit its subject can move.
	SpendOnThisCreatorAfter money.Quantity `json:"spend_on_this_creator_after"`
	CreditBaseAfter         money.Quantity `json:"credit_base_after"`
}

// Quote is the executable quote under evaluation.
type Quote struct {
	ID             string    `json:"id"`
	Provider       string    `json:"provider"`
	PriceImpactBPS money.BPS `json:"price_impact_bps"`
	SlippageBPS    money.BPS `json:"slippage_bps"`
	FeeBPS         money.BPS `json:"fee_bps"`
	ReceivedAt     time.Time `json:"received_at"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// DataAge is the observed age of one data dependency and the maximum age the
// strategy declared for it (PART 174; zero when undeclared).
type DataAge struct {
	AgeMS            int64 `json:"age_ms"`
	DeclaredMaxAgeMS int64 `json:"declared_max_age_ms"`
}

// KillSwitch is one active switch.
type KillSwitch struct {
	Kind  string `json:"kind"`
	Scope string `json:"scope"`
}

// normalized trims identifiers, sorts and de-duplicates lists, replaces nil
// collections with empty ones and converts times to UTC so equal inputs hash
// equally and no output depends on input ordering.
func (in Input) normalized() Input {
	in.Stage = Stage(strings.TrimSpace(string(in.Stage)))
	in.Now = in.Now.UTC()

	it := &in.Intent
	it.ID = strings.TrimSpace(it.ID)
	it.AccountID = strings.TrimSpace(it.AccountID)
	it.AgentID = strings.TrimSpace(it.AgentID)
	it.StrategyVersionID = strings.TrimSpace(it.StrategyVersionID)
	it.ModelID = strings.TrimSpace(it.ModelID)
	it.Action = Action(strings.TrimSpace(string(it.Action)))
	it.InstrumentID = strings.TrimSpace(it.InstrumentID)
	it.AssetClass = strings.TrimSpace(it.AssetClass)
	it.AssetRiskClass = assets.RiskClass(strings.TrimSpace(string(it.AssetRiskClass)))
	it.AssetStatus = assets.Status(strings.TrimSpace(string(it.AssetStatus)))
	it.Venue = strings.TrimSpace(it.Venue)
	it.Chain = strings.TrimSpace(it.Chain)
	it.Provider = strings.TrimSpace(it.Provider)

	acct := &in.Account
	acct.Status = accounts.Status(strings.TrimSpace(string(acct.Status)))
	acct.AsOf = acct.AsOf.UTC()
	acct.PositionsUSD = cloneUSDMap(acct.PositionsUSD)
	acct.AssetClassExposureUSD = cloneUSDMap(acct.AssetClassExposureUSD)

	if in.Envelope != nil {
		env := *in.Envelope
		env.ID = strings.TrimSpace(env.ID)
		env.Status = strings.TrimSpace(env.Status)
		env.AllowedInstruments = sortedUnique(env.AllowedInstruments)
		env.AllowedAssetClasses = sortedUnique(env.AllowedAssetClasses)
		env.AllowedVenues = sortedUnique(env.AllowedVenues)
		if env.ExpiresAt != nil {
			t := env.ExpiresAt.UTC()
			env.ExpiresAt = &t
		}
		in.Envelope = &env
	}

	m := &in.Market
	m.VenueStatus = strings.TrimSpace(m.VenueStatus)
	if m.Quote != nil {
		q := *m.Quote
		q.ID = strings.TrimSpace(q.ID)
		q.Provider = strings.TrimSpace(q.Provider)
		q.ReceivedAt = q.ReceivedAt.UTC()
		q.ExpiresAt = q.ExpiresAt.UTC()
		m.Quote = &q
	}
	if m.LiquidityUSD != nil {
		l := *m.LiquidityUSD
		m.LiquidityUSD = &l
	}
	health := make(map[string]string, len(m.ProviderHealth))
	for k, v := range m.ProviderHealth {
		health[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	m.ProviderHealth = health
	fresh := make(map[string]DataAge, len(m.DataFreshness))
	for k, v := range m.DataFreshness {
		fresh[strings.TrimSpace(k)] = v
	}
	m.DataFreshness = fresh

	if in.NativeMarket != nil {
		n := *in.NativeMarket
		n.MarketID = strings.TrimSpace(n.MarketID)
		n.AssetID = strings.TrimSpace(n.AssetID)
		n.CreatorAccountID = strings.TrimSpace(n.CreatorAccountID)
		in.NativeMarket = &n
	}

	switches := make([]KillSwitch, 0, len(in.KillSwitches))
	seen := map[KillSwitch]struct{}{}
	for _, ks := range in.KillSwitches {
		ks.Kind = strings.TrimSpace(ks.Kind)
		ks.Scope = strings.TrimSpace(ks.Scope)
		if ks.Scope == "" {
			ks.Scope = KillScopeAll
		}
		if _, ok := seen[ks]; ok {
			continue
		}
		seen[ks] = struct{}{}
		switches = append(switches, ks)
	}
	sortSwitches(switches)
	in.KillSwitches = switches
	return in
}

func cloneUSDMap(m map[string]money.USD) map[string]money.USD {
	out := make(map[string]money.USD, len(m))
	for k, v := range m {
		out[strings.TrimSpace(k)] = v
	}
	return out
}

func sortSwitches(s []KillSwitch) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].Kind != s[j].Kind {
			return s[i].Kind < s[j].Kind
		}
		return s[i].Scope < s[j].Scope
	})
}

// sortedKeys returns the keys of m in ascending order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
