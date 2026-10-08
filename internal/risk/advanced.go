package risk

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
)

// Risk Kernel V2: the deterministic risk function of the advanced capabilities
// (ADR-0020). It extends the kernel
// rather than replacing it: Evaluate (V1) still decides spot trade intents;
// EvaluateAdvanced decides every economic intent of internal/econ.
//
// It is built from small composable checks, one per category. Each check reads
// only its own section of the input and returns stable reason codes. A section
// an intent kind requires but the caller did not supply is a violation
// (RISK_INPUT_INVALID), never a pass: the kernel fails closed on missing data
// exactly as V1 does.
//
// Same input, same policy, same decision -- byte for byte. No clock (Now is
// input), no I/O, no randomness, no floating point, no map iteration that
// reaches the output.

// AdvancedEvaluatorVersion is stored with every V2 decision.
const AdvancedEvaluatorVersion = "risk-kernel/2"

// AdvancedCategory names one composable check.
type AdvancedCategory string

// Categories (goal §21).
const (
	CatIdentity           AdvancedCategory = "IDENTITY"
	CatEligibility        AdvancedCategory = "ELIGIBILITY"
	CatInstrument         AdvancedCategory = "INSTRUMENT"
	CatJurisdiction       AdvancedCategory = "JURISDICTION"
	CatAccount            AdvancedCategory = "ACCOUNT"
	CatBalance            AdvancedCategory = "BALANCE"
	CatMargin             AdvancedCategory = "MARGIN"
	CatConcentration      AdvancedCategory = "CONCENTRATION"
	CatLiquidity          AdvancedCategory = "LIQUIDITY"
	CatVolatility         AdvancedCategory = "VOLATILITY"
	CatOracleFreshness    AdvancedCategory = "ORACLE_FRESHNESS"
	CatVenueHealth        AdvancedCategory = "VENUE_HEALTH"
	CatChainHealth        AdvancedCategory = "CHAIN_HEALTH"
	CatSettlement         AdvancedCategory = "SETTLEMENT"
	CatVelocity           AdvancedCategory = "VELOCITY"
	CatDestination        AdvancedCategory = "DESTINATION"
	CatStrategy           AdvancedCategory = "STRATEGY"
	CatAuthority          AdvancedCategory = "AUTHORITY"
	CatCodeSandbox        AdvancedCategory = "CODE_SANDBOX"
	CatCapitalReservation AdvancedCategory = "CAPITAL_RESERVATION"
	CatCircuitBreaker     AdvancedCategory = "CIRCUIT_BREAKER"
)

var allAdvancedCategories = []AdvancedCategory{
	CatIdentity, CatEligibility, CatInstrument, CatJurisdiction, CatAccount, CatBalance, CatMargin,
	CatConcentration, CatLiquidity, CatVolatility, CatOracleFreshness, CatVenueHealth, CatChainHealth,
	CatSettlement, CatVelocity, CatDestination, CatStrategy, CatAuthority, CatCodeSandbox,
	CatCapitalReservation, CatCircuitBreaker,
}

// AllAdvancedCategories returns every category in evaluation order.
func AllAdvancedCategories() []AdvancedCategory {
	return append([]AdvancedCategory(nil), allAdvancedCategories...)
}

// V2 reason codes. Stable and machine-readable; RISK_ prefix like V1, and the
// V1 codes are reused where the meaning is the same.
const (
	ReasonIdentityUnverified        = "RISK_IDENTITY_UNVERIFIED"
	ReasonEligibilityDenied         = "RISK_ELIGIBILITY_DENIED"
	ReasonInstrumentNotTradable     = "RISK_INSTRUMENT_NOT_TRADABLE"
	ReasonInstrumentCloseOnly       = "RISK_INSTRUMENT_CLOSE_ONLY"
	ReasonInstrumentSessionClosed   = "RISK_INSTRUMENT_SESSION_CLOSED"
	ReasonInstrumentExpired         = "RISK_INSTRUMENT_EXPIRED"
	ReasonJurisdictionBlocked       = "RISK_JURISDICTION_BLOCKED"
	ReasonInsufficientBalance       = "RISK_INSUFFICIENT_BALANCE"
	ReasonMarginInitial             = "RISK_MARGIN_INITIAL"
	ReasonMarginMaintenance         = "RISK_MARGIN_MAINTENANCE"
	ReasonLeverageCap               = "RISK_LEVERAGE_CAP"
	ReasonWithdrawBreachesMargin    = "RISK_WITHDRAW_BREACHES_MARGIN"
	ReasonVolatilityHalt            = "RISK_VOLATILITY_HALT"
	ReasonOracleUnavailable         = "RISK_ORACLE_UNAVAILABLE"
	ReasonOracleStale               = "RISK_ORACLE_STALE"
	ReasonOracleDivergence          = "RISK_ORACLE_DIVERGENCE"
	ReasonChainUnhealthy            = "RISK_CHAIN_UNHEALTHY"
	ReasonSettlementUnknown         = "RISK_SETTLEMENT_UNKNOWN"
	ReasonVelocity                  = "RISK_VELOCITY"
	ReasonDestinationNotAllowed     = "RISK_DESTINATION_NOT_ALLOWED"
	ReasonDestinationTooNew         = "RISK_DESTINATION_TOO_NEW"
	ReasonStrategyNotApproved       = "RISK_STRATEGY_NOT_APPROVED"
	ReasonStrategyPermission        = "RISK_STRATEGY_PERMISSION"
	ReasonAuthorityMissing          = "RISK_AUTHORITY_MISSING"
	ReasonAuthorityExpired          = "RISK_AUTHORITY_EXPIRED"
	ReasonAuthorityRevoked          = "RISK_AUTHORITY_REVOKED"
	ReasonAuthorityLimit            = "RISK_AUTHORITY_LIMIT"
	ReasonAuthorityScope            = "RISK_AUTHORITY_SCOPE"
	ReasonSandboxArtifactUnverified = "RISK_SANDBOX_ARTIFACT_UNVERIFIED"
	ReasonSandboxViolation          = "RISK_SANDBOX_VIOLATION"
	ReasonReservationMissing        = "RISK_RESERVATION_MISSING"
	ReasonCircuitBreaker            = "RISK_CIRCUIT_BREAKER"
)

var allAdvancedReasons = []string{
	ReasonInputInvalid, ReasonPolicyMissing, ReasonKillSwitch, ReasonAccountStatus, ReasonMaxSingleTrade,
	ReasonConcentration, ReasonLiquidity, ReasonVenueStatus,
	ReasonIdentityUnverified, ReasonEligibilityDenied, ReasonInstrumentNotTradable, ReasonInstrumentCloseOnly,
	ReasonInstrumentSessionClosed, ReasonInstrumentExpired, ReasonJurisdictionBlocked, ReasonInsufficientBalance,
	ReasonMarginInitial, ReasonMarginMaintenance, ReasonLeverageCap, ReasonWithdrawBreachesMargin,
	ReasonVolatilityHalt, ReasonOracleUnavailable, ReasonOracleStale, ReasonOracleDivergence, ReasonChainUnhealthy,
	ReasonSettlementUnknown, ReasonVelocity, ReasonDestinationNotAllowed, ReasonDestinationTooNew,
	ReasonStrategyNotApproved, ReasonStrategyPermission, ReasonAuthorityMissing, ReasonAuthorityExpired,
	ReasonAuthorityRevoked, ReasonAuthorityLimit, ReasonAuthorityScope, ReasonSandboxArtifactUnverified,
	ReasonSandboxViolation, ReasonReservationMissing, ReasonCircuitBreaker,
}

// AdvancedReasonCodes returns every reason code EvaluateAdvanced can emit, sorted.
func AdvancedReasonCodes() []string {
	out := append([]string(nil), allAdvancedReasons...)
	sort.Strings(out)
	return out
}

// AdvancedPolicy is the typed limits document of the V2 kernel. Every limit is
// an exact integer; there is no floating point anywhere.
type AdvancedPolicy struct {
	Version string `json:"version"`
	// MaxSingleNotional bounds one intent's USD notional.
	MaxSingleNotional money.USD `json:"max_single_notional"`
	// MaxLeverageBPS bounds gross exposure / equity after the intent
	// (20_000 = 2x).
	MaxLeverageBPS money.BPS `json:"max_leverage_bps"`
	// MaxOracleAgeMS bounds the age of the price a risk-increasing intent is
	// evaluated against.
	MaxOracleAgeMS int64 `json:"max_oracle_age_ms"`
	// MaxOracleDivergenceBPS bounds mark/index (or source/source) divergence.
	MaxOracleDivergenceBPS money.BPS `json:"max_oracle_divergence_bps"`
	// MaxConcentrationBPS bounds one position as a share of the portfolio.
	MaxConcentrationBPS money.BPS `json:"max_concentration_bps"`
	// VolatilityHaltBPS halts new risk when the reference moved more than this
	// over the volatility window.
	VolatilityHaltBPS money.BPS `json:"volatility_halt_bps"`
	// Velocity limits for value leaving an account (transfers, withdrawals).
	VelocityMaxCount    int       `json:"velocity_max_count"`
	VelocityMaxNotional money.USD `json:"velocity_max_notional"`
	// MinDestinationAgeMS is how long a destination must have been on the
	// allowlist before value may go to it.
	MinDestinationAgeMS int64 `json:"min_destination_age_ms"`
	// RequireVerifiedIdentityAbove: intents above this notional need a
	// verified identity. Zero means always.
	RequireVerifiedIdentityAbove money.USD `json:"require_verified_identity_above"`
}

// DefaultAdvancedPolicy is the sandbox policy: deliberately small, fully
// specified, and never a statement that any live activity is acceptable.
func DefaultAdvancedPolicy() AdvancedPolicy {
	return AdvancedPolicy{
		Version:                      "advanced-sandbox-v1",
		MaxSingleNotional:            money.USDFromMinor(1_000_000_00), // $1,000,000 of simulated value
		MaxLeverageBPS:               50_000,                           // 5x
		MaxOracleAgeMS:               30_000,
		MaxOracleDivergenceBPS:       500,
		MaxConcentrationBPS:          10_000,
		VolatilityHaltBPS:            2_500,
		VelocityMaxCount:             50,
		VelocityMaxNotional:          money.USDFromMinor(500_000_00),
		MinDestinationAgeMS:          0,
		RequireVerifiedIdentityAbove: money.USDFromMinor(0),
	}
}

// Missing reports whether the policy is unusable (no version or no limits).
func (p AdvancedPolicy) Missing() bool {
	return p.Version == "" || p.MaxSingleNotional.IsZero() || p.MaxLeverageBPS <= 0 || p.MaxOracleAgeMS <= 0
}

// Hash is the canonical hash of the policy.
func (p AdvancedPolicy) Hash() string { return hashOf(p) }

// AdvancedIntent is the part of an economic intent the kernel evaluates.
type AdvancedIntent struct {
	ID             string    `json:"id"`
	Kind           string    `json:"kind"`
	AccountID      string    `json:"account_id"`
	ActorType      string    `json:"actor_type"`
	AgentID        string    `json:"agent_id"`
	StrategyRef    string    `json:"strategy_ref"`
	Capability     string    `json:"capability"`
	Mode           string    `json:"mode"`
	InstrumentID   string    `json:"instrument_id"`
	InstrumentKind string    `json:"instrument_kind"`
	NotionalUSD    money.USD `json:"notional_usd"`
	// RiskIncreasing is decided by the subsystem per request: closing,
	// repaying and reduce-only are not.
	RiskIncreasing bool `json:"risk_increasing"`
	// SandboxOriginated marks an intent proposed by code running in the
	// strategy sandbox.
	SandboxOriginated bool `json:"sandbox_originated"`
}

// Snapshot sections. A nil section means the caller did not supply it.
type (
	IdentitySnapshot struct {
		Verified bool `json:"verified"`
	}
	EligibilitySnapshot struct {
		Eligible bool   `json:"eligible"`
		Code     string `json:"code"`
	}
	AdvancedAccountSnapshot struct {
		Status string `json:"status"`
	}
	InstrumentSnapshot struct {
		Status      string `json:"status"` // ACTIVE | CLOSE_ONLY | HALTED | ...
		SessionOpen bool   `json:"session_open"`
		Expired     bool   `json:"expired"`
	}
	JurisdictionSnapshot struct {
		Allowed bool   `json:"allowed"`
		Code    string `json:"code"`
	}
	BalanceSnapshot struct {
		Available money.USD `json:"available"`
		Required  money.USD `json:"required"`
	}
	MarginSnapshot struct {
		EquityAfter              money.USD `json:"equity_after"`
		InitialRequiredAfter     money.USD `json:"initial_required_after"`
		MaintenanceRequiredAfter money.USD `json:"maintenance_required_after"`
		LeverageAfterBPS         money.BPS `json:"leverage_after_bps"`
	}
	ConcentrationSnapshot struct {
		PositionAfter money.USD `json:"position_after"`
		Portfolio     money.USD `json:"portfolio"`
	}
	LiquiditySnapshot struct {
		AvailableUSD money.USD `json:"available_usd"`
	}
	VolatilitySnapshot struct {
		MoveBPS money.BPS `json:"move_bps"`
	}
	OracleSnapshot struct {
		Available     bool      `json:"available"`
		ObservedAt    time.Time `json:"observed_at"`
		DivergenceBPS money.BPS `json:"divergence_bps"`
	}
	VenueSnapshot struct {
		Healthy bool `json:"healthy"`
	}
	ChainSnapshot struct {
		Healthy bool `json:"healthy"`
	}
	SettlementSnapshot struct {
		UnknownEffects int `json:"unknown_effects"`
	}
	VelocitySnapshot struct {
		CountInWindow    int       `json:"count_in_window"`
		NotionalInWindow money.USD `json:"notional_in_window"`
	}
	DestinationSnapshot struct {
		Allowlisted bool      `json:"allowlisted"`
		AddedAt     time.Time `json:"added_at"`
	}
	StrategySnapshot struct {
		Approved        bool `json:"approved"`
		ManifestPermits bool `json:"manifest_permits"`
	}
	AuthoritySnapshot struct {
		Present      bool `json:"present"`
		Expired      bool `json:"expired"`
		Revoked      bool `json:"revoked"`
		WithinLimits bool `json:"within_limits"`
		InScope      bool `json:"in_scope"`
	}
	SandboxSnapshot struct {
		ArtifactVerified bool `json:"artifact_verified"`
		Violations       int  `json:"violations"`
	}
	ReservationSnapshot struct {
		Held bool `json:"held"`
	}
)

// AdvancedInput is the typed evaluation snapshot. Every value the decision
// depends on is inside it.
type AdvancedInput struct {
	Stage         Stage                    `json:"stage"`
	Intent        AdvancedIntent           `json:"intent"`
	Identity      *IdentitySnapshot        `json:"identity"`
	Eligibility   *EligibilitySnapshot     `json:"eligibility"`
	Account       *AdvancedAccountSnapshot `json:"account"`
	Instrument    *InstrumentSnapshot      `json:"instrument"`
	Jurisdiction  *JurisdictionSnapshot    `json:"jurisdiction"`
	Balance       *BalanceSnapshot         `json:"balance"`
	Margin        *MarginSnapshot          `json:"margin"`
	Concentration *ConcentrationSnapshot   `json:"concentration"`
	Liquidity     *LiquiditySnapshot       `json:"liquidity"`
	Volatility    *VolatilitySnapshot      `json:"volatility"`
	Oracle        *OracleSnapshot          `json:"oracle"`
	Venue         *VenueSnapshot           `json:"venue"`
	Chain         *ChainSnapshot           `json:"chain"`
	Settlement    *SettlementSnapshot      `json:"settlement"`
	Velocity      *VelocitySnapshot        `json:"velocity"`
	Destination   *DestinationSnapshot     `json:"destination"`
	Strategy      *StrategySnapshot        `json:"strategy"`
	Authority     *AuthoritySnapshot       `json:"authority"`
	Sandbox       *SandboxSnapshot         `json:"sandbox"`
	Reservation   *ReservationSnapshot     `json:"reservation"`
	// CircuitBreakers lists the names of active circuit breakers that apply
	// to this intent (peg breaker, reserve mismatch, liquidation storm...).
	CircuitBreakers []string `json:"circuit_breakers"`
	// KillSwitches lists the active switches, as in V1.
	KillSwitches []KillSwitch `json:"kill_switches"`
	Now          time.Time    `json:"now"`
}

// Section names, used in RISK_INPUT_INVALID details.
const (
	SecIdentity    = "identity"
	SecAccount     = "account"
	SecInstrument  = "instrument"
	SecBalance     = "balance"
	SecMargin      = "margin"
	SecOracle      = "oracle"
	SecChain       = "chain"
	SecSettlement  = "settlement"
	SecVelocity    = "velocity"
	SecDestination = "destination"
	SecStrategy    = "strategy"
	SecAuthority   = "authority"
	SecSandbox     = "sandbox"
	SecReservation = "reservation"
)

// RequiredSections returns the sections an intent must supply. It is the
// fail-closed half of composability: a check that cannot run because its data
// is missing rejects rather than passes.
func RequiredSections(in AdvancedInput) []string {
	req := []string{SecAccount, SecIdentity}
	switch in.Intent.Kind {
	case "ACQUIRE_EXPOSURE", "OPEN_POSITION", "PROVIDE_LIQUIDITY", "TAKE_LIQUIDITY", "CREATE_PREDICTION_POSITION":
		req = append(req, SecInstrument)
		if in.Intent.RiskIncreasing {
			req = append(req, SecOracle)
		}
	case "REDUCE_EXPOSURE", "CLOSE_POSITION", "EXERCISE_OPTION", "RESOLVE_PREDICTION_POSITION":
		req = append(req, SecInstrument)
	case "BORROW":
		req = append(req, SecMargin, SecOracle)
	case "REPAY":
		req = append(req, SecBalance)
	case "TRANSFER_VALUE":
		req = append(req, SecBalance, SecVelocity, SecDestination)
	case "WITHDRAW":
		req = append(req, SecBalance, SecVelocity, SecDestination, SecChain, SecSettlement)
	case "MINT_STABLE_ASSET", "BURN_STABLE_ASSET":
		req = append(req, SecBalance, SecChain, SecSettlement)
	case "BRIDGE_VALUE":
		req = append(req, SecBalance, SecChain)
	}
	if in.Intent.ActorType == "AGENT" {
		req = append(req, SecAuthority)
	}
	if in.Intent.StrategyRef != "" {
		req = append(req, SecStrategy)
	}
	if in.Intent.SandboxOriginated {
		req = append(req, SecSandbox)
	}
	sort.Strings(req)
	out := req[:0]
	for i, s := range req {
		if i == 0 || s != req[i-1] {
			out = append(out, s)
		}
	}
	return out
}

func present(in AdvancedInput, section string) bool {
	switch section {
	case SecIdentity:
		return in.Identity != nil
	case SecAccount:
		return in.Account != nil
	case SecInstrument:
		return in.Instrument != nil
	case SecBalance:
		return in.Balance != nil
	case SecMargin:
		return in.Margin != nil
	case SecOracle:
		return in.Oracle != nil
	case SecChain:
		return in.Chain != nil
	case SecSettlement:
		return in.Settlement != nil
	case SecVelocity:
		return in.Velocity != nil
	case SecDestination:
		return in.Destination != nil
	case SecStrategy:
		return in.Strategy != nil
	case SecAuthority:
		return in.Authority != nil
	case SecSandbox:
		return in.Sandbox != nil
	case SecReservation:
		return in.Reservation != nil
	}
	return false
}

// CheckResult is one category's outcome.
type CheckResult struct {
	Category AdvancedCategory `json:"category"`
	// Applied is false when the category's section was absent and not
	// required, so the check had nothing to evaluate.
	Applied bool     `json:"applied"`
	Codes   []string `json:"codes"`
}

// AdvancedDecision is the persisted outcome of one V2 evaluation.
type AdvancedDecision struct {
	Verdict          Verdict       `json:"decision"`
	Stage            Stage         `json:"stage"`
	IntentKind       string        `json:"intent_kind"`
	ReasonCodes      []string      `json:"reason_codes"`
	MissingSections  []string      `json:"missing_sections"`
	Checks           []CheckResult `json:"checks"`
	PolicyVersion    string        `json:"policy_version"`
	PolicyHash       string        `json:"policy_hash"`
	EvaluatorVersion string        `json:"evaluator_version"`
	InputHash        string        `json:"input_hash"`
	EvaluatedAt      time.Time     `json:"evaluated_at"`
	Hash             string        `json:"hash"`
}

// ComputeHash returns the hash a decision with these fields must carry.
func (d AdvancedDecision) ComputeHash() string {
	d.Hash = ""
	return hashOf(d)
}

// CanonicalJSON renders the decision deterministically.
func (d AdvancedDecision) CanonicalJSON() ([]byte, error) { return canonicalJSON(d) }

// advancedCheck is one composable check.
type advancedCheck struct {
	category AdvancedCategory
	// applies reports whether the check has data to evaluate.
	applies func(in AdvancedInput) bool
	run     func(p AdvancedPolicy, in AdvancedInput) []string
}

var advancedChecks = []advancedCheck{
	{CatIdentity, func(in AdvancedInput) bool { return in.Identity != nil }, checkIdentity},
	{CatEligibility, func(in AdvancedInput) bool { return in.Eligibility != nil }, checkEligibility},
	{CatInstrument, func(in AdvancedInput) bool { return in.Instrument != nil }, checkInstrument},
	{CatJurisdiction, func(in AdvancedInput) bool { return in.Jurisdiction != nil }, checkJurisdiction},
	{CatAccount, func(in AdvancedInput) bool { return in.Account != nil }, checkAccount},
	{CatBalance, func(in AdvancedInput) bool { return in.Balance != nil }, checkBalance},
	{CatMargin, func(in AdvancedInput) bool { return in.Margin != nil }, checkMargin},
	{CatConcentration, func(in AdvancedInput) bool { return in.Concentration != nil }, checkConcentration},
	{CatLiquidity, func(in AdvancedInput) bool { return in.Liquidity != nil }, checkLiquidity},
	{CatVolatility, func(in AdvancedInput) bool { return in.Volatility != nil }, checkVolatility},
	{CatOracleFreshness, func(in AdvancedInput) bool { return in.Oracle != nil }, checkOracle},
	{CatVenueHealth, func(in AdvancedInput) bool { return in.Venue != nil }, checkVenue},
	{CatChainHealth, func(in AdvancedInput) bool { return in.Chain != nil }, checkChain},
	{CatSettlement, func(in AdvancedInput) bool { return in.Settlement != nil }, checkSettlement},
	{CatVelocity, func(in AdvancedInput) bool { return in.Velocity != nil }, checkVelocity},
	{CatDestination, func(in AdvancedInput) bool { return in.Destination != nil }, checkDestination},
	{CatStrategy, func(in AdvancedInput) bool { return in.Strategy != nil }, checkStrategy},
	{CatAuthority, func(in AdvancedInput) bool { return in.Authority != nil }, checkAuthority},
	{CatCodeSandbox, func(in AdvancedInput) bool { return in.Sandbox != nil }, checkSandbox},
	{CatCapitalReservation, func(in AdvancedInput) bool { return in.Reservation != nil }, checkReservation},
	{CatCircuitBreaker, func(AdvancedInput) bool { return true }, checkCircuitBreakers},
}

// EvaluateAdvanced is the pure V2 risk function. It returns every violated
// reason, sorted, and the per-category results.
func EvaluateAdvanced(p AdvancedPolicy, in AdvancedInput) AdvancedDecision {
	in = in.normalized()
	d := AdvancedDecision{
		Stage:            in.Stage,
		IntentKind:       in.Intent.Kind,
		PolicyVersion:    p.Version,
		PolicyHash:       p.Hash(),
		EvaluatorVersion: AdvancedEvaluatorVersion,
		InputHash:        hashOf(in),
		EvaluatedAt:      in.Now,
	}
	rs := reasonSet{}
	if p.Missing() {
		rs.add(ReasonPolicyMissing)
	}
	if !in.Stage.Valid() || in.Intent.Kind == "" || in.Intent.AccountID == "" || in.Now.IsZero() {
		rs.add(ReasonInputInvalid)
	}
	for _, s := range RequiredSections(in) {
		if !present(in, s) {
			d.MissingSections = append(d.MissingSections, s)
		}
	}
	if len(d.MissingSections) > 0 {
		rs.add(ReasonInputInvalid)
	}
	if !p.MaxSingleNotional.IsZero() && in.Intent.NotionalUSD.Cmp(p.MaxSingleNotional) > 0 && in.Intent.RiskIncreasing {
		rs.add(ReasonMaxSingleTrade)
	}
	if advancedKillSwitched(in) {
		rs.add(ReasonKillSwitch)
	}
	for _, c := range advancedChecks {
		res := CheckResult{Category: c.category}
		if c.applies(in) {
			res.Applied = true
			codes := c.run(p, in)
			sort.Strings(codes)
			res.Codes = codes
			for _, code := range codes {
				rs.add(code)
			}
		}
		if res.Codes == nil {
			res.Codes = []string{}
		}
		d.Checks = append(d.Checks, res)
	}
	d.ReasonCodes = rs.sorted()
	if d.MissingSections == nil {
		d.MissingSections = []string{}
	}
	if len(d.ReasonCodes) == 0 {
		d.Verdict = Allow
	} else {
		d.Verdict = Reject
	}
	d.Hash = d.ComputeHash()
	return d
}

func (in AdvancedInput) normalized() AdvancedInput {
	in.Now = in.Now.UTC()
	if in.Oracle != nil {
		o := *in.Oracle
		o.ObservedAt = o.ObservedAt.UTC()
		in.Oracle = &o
	}
	if in.Destination != nil {
		dst := *in.Destination
		dst.AddedAt = dst.AddedAt.UTC()
		in.Destination = &dst
	}
	cb := append([]string(nil), in.CircuitBreakers...)
	sort.Strings(cb)
	if cb == nil {
		cb = []string{}
	}
	in.CircuitBreakers = cb
	ks := append([]KillSwitch(nil), in.KillSwitches...)
	sortSwitches(ks)
	if ks == nil {
		ks = []KillSwitch{}
	}
	in.KillSwitches = ks
	return in
}

// advancedKillSwitched: the global kill and CAPABILITY_DISABLE for the
// intent's capability stop risk-increasing intents; ACCOUNT_FREEZE stops
// everything that is not risk-reducing; INSTRUMENT_HALT stops everything on
// the instrument.
func advancedKillSwitched(in AdvancedInput) bool {
	for _, ks := range in.KillSwitches {
		switch ks.Kind {
		case KillGlobalNewRisk:
			if in.Intent.RiskIncreasing {
				return true
			}
		case KillCapabilityDisable:
			if ks.Scope == in.Intent.Capability && in.Intent.RiskIncreasing {
				return true
			}
		case KillAccountFreeze:
			if (ks.Scope == KillScopeAll || ks.Scope == in.Intent.AccountID) && in.Intent.RiskIncreasing {
				return true
			}
		case KillAgentPause:
			if in.Intent.AgentID != "" && (ks.Scope == KillScopeAll || ks.Scope == in.Intent.AgentID) && in.Intent.RiskIncreasing {
				return true
			}
		case KillInstrumentHalt:
			if in.Intent.InstrumentID != "" && (ks.Scope == KillScopeAll || ks.Scope == in.Intent.InstrumentID) {
				return true
			}
		case KillInstrumentCloseOnly:
			if in.Intent.InstrumentID != "" && (ks.Scope == KillScopeAll || ks.Scope == in.Intent.InstrumentID) && in.Intent.RiskIncreasing {
				return true
			}
		case KillWithdrawalsDisable:
			if in.Intent.Kind == "WITHDRAW" {
				return true
			}
		}
	}
	return false
}

func checkIdentity(p AdvancedPolicy, in AdvancedInput) []string {
	if in.Identity.Verified {
		return nil
	}
	if in.Intent.NotionalUSD.Cmp(p.RequireVerifiedIdentityAbove) > 0 || p.RequireVerifiedIdentityAbove.IsZero() {
		return []string{ReasonIdentityUnverified}
	}
	return nil
}

func checkEligibility(_ AdvancedPolicy, in AdvancedInput) []string {
	if !in.Eligibility.Eligible {
		return []string{ReasonEligibilityDenied}
	}
	return nil
}

func checkInstrument(_ AdvancedPolicy, in AdvancedInput) []string {
	var out []string
	switch in.Instrument.Status {
	case "ACTIVE":
	case "CLOSE_ONLY", "DELISTING":
		if in.Intent.RiskIncreasing {
			out = append(out, ReasonInstrumentCloseOnly)
		}
	default:
		out = append(out, ReasonInstrumentNotTradable)
	}
	if !in.Instrument.SessionOpen {
		out = append(out, ReasonInstrumentSessionClosed)
	}
	if in.Instrument.Expired && in.Intent.RiskIncreasing {
		out = append(out, ReasonInstrumentExpired)
	}
	return out
}

func checkJurisdiction(_ AdvancedPolicy, in AdvancedInput) []string {
	if !in.Jurisdiction.Allowed {
		return []string{ReasonJurisdictionBlocked}
	}
	return nil
}

func checkAccount(_ AdvancedPolicy, in AdvancedInput) []string {
	switch in.Account.Status {
	case "ACTIVE":
		return nil
	case "RESTRICTED":
		if in.Intent.RiskIncreasing {
			return []string{ReasonAccountStatus}
		}
		return nil
	}
	return []string{ReasonAccountStatus}
}

func checkBalance(_ AdvancedPolicy, in AdvancedInput) []string {
	if in.Balance.Available.Cmp(in.Balance.Required) < 0 {
		return []string{ReasonInsufficientBalance}
	}
	return nil
}

// checkMargin: a risk-increasing intent must leave equity at or above the
// initial requirement and leverage within the cap; any intent that would leave
// equity below maintenance is refused (that is what makes a withdrawal that
// breaches maintenance impossible).
func checkMargin(p AdvancedPolicy, in AdvancedInput) []string {
	m := in.Margin
	var out []string
	if in.Intent.RiskIncreasing {
		if m.EquityAfter.Cmp(m.InitialRequiredAfter) < 0 {
			out = append(out, ReasonMarginInitial)
		}
		if m.LeverageAfterBPS > p.MaxLeverageBPS {
			out = append(out, ReasonLeverageCap)
		}
	}
	if m.EquityAfter.Cmp(m.MaintenanceRequiredAfter) < 0 {
		if in.Intent.Kind == "WITHDRAW" || in.Intent.Kind == "TRANSFER_VALUE" {
			out = append(out, ReasonWithdrawBreachesMargin)
		} else if in.Intent.RiskIncreasing {
			out = append(out, ReasonMarginMaintenance)
		}
	}
	return out
}

func checkConcentration(p AdvancedPolicy, in AdvancedInput) []string {
	c := in.Concentration
	if !in.Intent.RiskIncreasing || c.Portfolio.Sign() <= 0 {
		return nil
	}
	// position / portfolio > limit  <=>  position * 10000 > limit * portfolio
	if shareExceedsUSD(c.PositionAfter, c.Portfolio, p.MaxConcentrationBPS) {
		return []string{ReasonConcentration}
	}
	return nil
}

func checkLiquidity(_ AdvancedPolicy, in AdvancedInput) []string {
	if in.Intent.RiskIncreasing && in.Liquidity.AvailableUSD.Cmp(in.Intent.NotionalUSD) < 0 {
		return []string{ReasonLiquidity}
	}
	return nil
}

func checkVolatility(p AdvancedPolicy, in AdvancedInput) []string {
	move := in.Volatility.MoveBPS
	if move < 0 {
		move = -move
	}
	if in.Intent.RiskIncreasing && p.VolatilityHaltBPS > 0 && move > p.VolatilityHaltBPS {
		return []string{ReasonVolatilityHalt}
	}
	return nil
}

// checkOracle: stale or unavailable marks block new risk; they do not block
// risk reduction, because refusing to let a customer close on a stale mark
// traps them in the risk the stale mark hides.
func checkOracle(p AdvancedPolicy, in AdvancedInput) []string {
	o := in.Oracle
	if !in.Intent.RiskIncreasing {
		return nil
	}
	if !o.Available {
		return []string{ReasonOracleUnavailable}
	}
	var out []string
	if in.Now.Sub(o.ObservedAt) > time.Duration(p.MaxOracleAgeMS)*time.Millisecond || o.ObservedAt.After(in.Now) {
		out = append(out, ReasonOracleStale)
	}
	div := o.DivergenceBPS
	if div < 0 {
		div = -div
	}
	if p.MaxOracleDivergenceBPS > 0 && div > p.MaxOracleDivergenceBPS {
		out = append(out, ReasonOracleDivergence)
	}
	return out
}

func checkVenue(_ AdvancedPolicy, in AdvancedInput) []string {
	if !in.Venue.Healthy && in.Intent.RiskIncreasing {
		return []string{ReasonVenueStatus}
	}
	return nil
}

func checkChain(_ AdvancedPolicy, in AdvancedInput) []string {
	if !in.Chain.Healthy {
		return []string{ReasonChainUnhealthy}
	}
	return nil
}

func checkSettlement(_ AdvancedPolicy, in AdvancedInput) []string {
	if in.Settlement.UnknownEffects > 0 {
		return []string{ReasonSettlementUnknown}
	}
	return nil
}

func checkVelocity(p AdvancedPolicy, in AdvancedInput) []string {
	v := in.Velocity
	var out []string
	if p.VelocityMaxCount > 0 && v.CountInWindow+1 > p.VelocityMaxCount {
		out = append(out, ReasonVelocity)
	}
	if !p.VelocityMaxNotional.IsZero() {
		after, err := v.NotionalInWindow.Add(in.Intent.NotionalUSD)
		if err != nil || after.Cmp(p.VelocityMaxNotional) > 0 {
			out = append(out, ReasonVelocity)
		}
	}
	return dedupe(out)
}

func checkDestination(p AdvancedPolicy, in AdvancedInput) []string {
	dst := in.Destination
	if !dst.Allowlisted {
		return []string{ReasonDestinationNotAllowed}
	}
	if p.MinDestinationAgeMS > 0 && in.Now.Sub(dst.AddedAt) < time.Duration(p.MinDestinationAgeMS)*time.Millisecond {
		return []string{ReasonDestinationTooNew}
	}
	return nil
}

func checkStrategy(_ AdvancedPolicy, in AdvancedInput) []string {
	var out []string
	if !in.Strategy.Approved {
		out = append(out, ReasonStrategyNotApproved)
	}
	if !in.Strategy.ManifestPermits {
		out = append(out, ReasonStrategyPermission)
	}
	return out
}

func checkAuthority(_ AdvancedPolicy, in AdvancedInput) []string {
	a := in.Authority
	if !a.Present {
		return []string{ReasonAuthorityMissing}
	}
	var out []string
	if a.Expired {
		out = append(out, ReasonAuthorityExpired)
	}
	if a.Revoked {
		out = append(out, ReasonAuthorityRevoked)
	}
	if !a.WithinLimits {
		out = append(out, ReasonAuthorityLimit)
	}
	if !a.InScope {
		out = append(out, ReasonAuthorityScope)
	}
	return out
}

func checkSandbox(_ AdvancedPolicy, in AdvancedInput) []string {
	var out []string
	if !in.Sandbox.ArtifactVerified {
		out = append(out, ReasonSandboxArtifactUnverified)
	}
	if in.Sandbox.Violations > 0 {
		out = append(out, ReasonSandboxViolation)
	}
	return out
}

func checkReservation(_ AdvancedPolicy, in AdvancedInput) []string {
	if !in.Reservation.Held {
		return []string{ReasonReservationMissing}
	}
	return nil
}

func checkCircuitBreakers(_ AdvancedPolicy, in AdvancedInput) []string {
	if len(in.CircuitBreakers) > 0 && in.Intent.RiskIncreasing {
		return []string{ReasonCircuitBreaker}
	}
	return nil
}

// shareExceedsUSD reports whether part/whole exceeds limit (in BPS), exactly.
func shareExceedsUSD(part, whole money.USD, limit money.BPS) bool {
	lhs := money.QuantityFromInt64(part.Minor()).Mul(money.QuantityFromInt64(10_000))
	rhs := money.QuantityFromInt64(whole.Minor()).Mul(money.QuantityFromInt64(int64(limit)))
	return lhs.Cmp(rhs) > 0
}

func dedupe(in []string) []string {
	sort.Strings(in)
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// RecordAdvancedDecision persists a V2 decision exactly as EvaluateAdvanced
// produced it. The hash is re-derived first: a decision edited after
// evaluation cannot be stored.
func (s *Store) RecordAdvancedDecision(ctx context.Context, tx pgx.Tx, in AdvancedInput, d AdvancedDecision, correlationID string) (DecisionID, error) {
	if d.Hash == "" || d.Hash != d.ComputeHash() {
		return DecisionID{}, errs.New(errs.CodeValidationFailed, "risk: advanced decision hash mismatch; decisions must come unmodified from EvaluateAdvanced")
	}
	if !d.Stage.Valid() {
		return DecisionID{}, errs.Newf(errs.CodeValidationFailed, "risk: stage %q cannot be persisted", d.Stage)
	}
	in = in.normalized()
	intentID, err := optionalUUID("intent_id", in.Intent.ID)
	if err != nil {
		return DecisionID{}, err
	}
	accountID, err := optionalUUID("account_id", in.Intent.AccountID)
	if err != nil {
		return DecisionID{}, err
	}
	inputJSON, err := canonicalJSON(in)
	if err != nil {
		return DecisionID{}, errs.Wrap(err, errs.CodeValidationFailed, "risk: advanced input cannot be rendered")
	}
	decisionJSON, err := canonicalJSON(d)
	if err != nil {
		return DecisionID{}, errs.Wrap(err, errs.CodeValidationFailed, "risk: advanced decision cannot be rendered")
	}
	codes := d.ReasonCodes
	if codes == nil {
		codes = []string{}
	}
	decisionID := id.New[decisionKind]()
	_, err = tx.Exec(ctx, `INSERT INTO advanced_risk_decisions
		(id, intent_id, account_id, intent_kind, stage, verdict, reason_codes, input_hash, policy_version, policy_hash,
		 evaluator_version, decision_hash, input, decision, correlation_id, evaluated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,NULLIF($15,''),$16)`,
		decisionID, intentID, accountID, d.IntentKind, string(d.Stage), string(d.Verdict), codes, d.InputHash,
		d.PolicyVersion, d.PolicyHash, d.EvaluatorVersion, d.Hash, inputJSON, decisionJSON, correlationID, d.EvaluatedAt.UTC())
	if err != nil {
		return DecisionID{}, fmt.Errorf("risk: record advanced decision: %w", err)
	}
	return decisionID, nil
}

// RejectionError renders a REJECT decision as the error a service returns:
// RISK_REJECTED, with the stable reason codes in the fields.
func (d AdvancedDecision) RejectionError() error {
	if d.Verdict == Allow {
		return nil
	}
	return errs.Newf(errs.CodeRiskRejected, "risk kernel rejected %s: %v", d.IntentKind, d.ReasonCodes).
		WithField("reason_codes", d.ReasonCodes).
		WithField("missing_sections", d.MissingSections).
		WithField("decision_hash", d.Hash)
}
