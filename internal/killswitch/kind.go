package killswitch

import (
	"strings"
	"unicode"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

// Kind is an emergency control (PART 53). The set is closed and mirrors the
// CHECK constraint on kill_switches.kind.
type Kind string

// Switch kinds, with their scope in parentheses.
const (
	GlobalNewRiskKill         Kind = "GLOBAL_NEW_RISK_KILL"         // (*)
	AccountFreeze             Kind = "ACCOUNT_FREEZE"               // (account_id)
	AgentPause                Kind = "AGENT_PAUSE"                  // (agent_id)
	StrategyVersionDisable    Kind = "STRATEGY_VERSION_DISABLE"     // (strategy_version_id)
	VenueDisable              Kind = "VENUE_DISABLE"                // (venue)
	InstrumentCloseOnly       Kind = "INSTRUMENT_CLOSE_ONLY"        // (instrument_id)
	InstrumentHalt            Kind = "INSTRUMENT_HALT"              // (instrument_id)
	ChainDisableNewActions    Kind = "CHAIN_DISABLE_NEW_ACTIONS"    // (chain)
	ProviderDisableNewActions Kind = "PROVIDER_DISABLE_NEW_ACTIONS" // (provider)
	FundingDisable            Kind = "FUNDING_DISABLE"              // (*)
	WithdrawalsDisable        Kind = "WITHDRAWALS_DISABLE"          // (*)
	ModelDisable              Kind = "MODEL_DISABLE"                // (model_id|*)
)

var allKinds = []Kind{
	GlobalNewRiskKill, AccountFreeze, AgentPause, StrategyVersionDisable, VenueDisable,
	InstrumentCloseOnly, InstrumentHalt, ChainDisableNewActions, ProviderDisableNewActions,
	FundingDisable, WithdrawalsDisable, ModelDisable,
}

// AllKinds returns every kind in declaration order.
func AllKinds() []Kind { return append([]Kind(nil), allKinds...) }

// Valid reports whether k is a declared kind.
func (k Kind) Valid() bool {
	for _, x := range allKinds {
		if x == k {
			return true
		}
	}
	return false
}

// Severity classifies how a switch is released (PART 53: "re-enabling after
// severe/global kill should require stronger approval").
type Severity string

// Severities.
const (
	// SeverityStandard: release needs kill:release and a step-up.
	SeverityStandard Severity = "STANDARD"
	// SeveritySevere: release additionally needs an approved, dual-controlled
	// KILL_SWITCH_RELEASE admin action.
	SeveritySevere Severity = "SEVERE"
)

// Severity returns the release severity of the kind. Unknown kinds are
// SEVERE (fail closed).
func (k Kind) Severity() Severity {
	switch k {
	case GlobalNewRiskKill, ChainDisableNewActions, ProviderDisableNewActions, FundingDisable, WithdrawalsDisable:
		return SeveritySevere
	case AccountFreeze, AgentPause, StrategyVersionDisable, VenueDisable, InstrumentCloseOnly, InstrumentHalt, ModelDisable:
		return SeverityStandard
	}
	return SeveritySevere
}

// GlobalScope is the scope of switches that apply to everything.
const GlobalScope = "*"

// maxScopeLen bounds scope identifiers (operator input).
const maxScopeLen = 200

type scopeRule int

const (
	scopeGlobalOnly scopeRule = iota // must be "*"
	scopeIDOnly                      // must be a specific id
	scopeIDOrGlobal                  // either
)

func (k Kind) scopeRule() scopeRule {
	switch k {
	case GlobalNewRiskKill, FundingDisable, WithdrawalsDisable:
		return scopeGlobalOnly
	case ModelDisable:
		return scopeIDOrGlobal
	default:
		return scopeIDOnly
	}
}

// NormalizeScope trims and validates a scope for the kind. An empty scope
// is accepted as "*" only for kinds whose scope rule allows the global
// scope. It fails with VALIDATION_FAILED.
func (k Kind) NormalizeScope(scope string) (string, error) {
	if !k.Valid() {
		return "", errs.Newf(errs.CodeValidationFailed, "unknown kill switch kind %q", k).WithField("kind", string(k))
	}
	scope = strings.TrimSpace(scope)
	if scope == "" {
		scope = GlobalScope
	}
	if len(scope) > maxScopeLen {
		return "", errs.New(errs.CodeValidationFailed, "scope is too long").WithField("scope", "too_long")
	}
	for _, r := range scope {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", errs.New(errs.CodeValidationFailed, "scope must not contain whitespace or control characters").WithField("scope", "invalid")
		}
	}
	switch k.scopeRule() {
	case scopeGlobalOnly:
		if scope != GlobalScope {
			return "", errs.Newf(errs.CodeValidationFailed, "%s is global; scope must be %q", k, GlobalScope).WithField("scope", "must_be_global")
		}
	case scopeIDOnly:
		if scope == GlobalScope {
			return "", errs.Newf(errs.CodeValidationFailed, "%s requires a specific scope id", k).WithField("scope", "required")
		}
	}
	return scope, nil
}

// ActionClass classifies a guarded operation (POLICY_AUTHORITY §2).
type ActionClass string

// Action classes.
const (
	// NewRisk opens or increases exposure, starts a funding session, or
	// creates a new agent intent. Blocked by any matching switch.
	NewRisk ActionClass = "NEW_RISK"
	// ReduceRisk closes or reduces a position. Blocked only by
	// INSTRUMENT_HALT, CHAIN_DISABLE_NEW_ACTIONS, PROVIDER_DISABLE_NEW_ACTIONS
	// and, if Policy.AccountFreezeBlocksRiskReduction, ACCOUNT_FREEZE.
	ReduceRisk ActionClass = "REDUCE_RISK"
	// Withdraw moves value out. Blocked by WITHDRAWALS_DISABLE,
	// ACCOUNT_FREEZE and GLOBAL_NEW_RISK_KILL.
	Withdraw ActionClass = "WITHDRAW"
	// Observe reads external state. Never blocked.
	Observe ActionClass = "OBSERVE"
	// Settle processes already-received fills and settlement. Never blocked.
	Settle ActionClass = "SETTLE"
	// Reconcile compares internal and external state. Never blocked.
	Reconcile ActionClass = "RECONCILE"
	// LedgerPost posts journal entries. Never blocked.
	LedgerPost ActionClass = "LEDGER_POST"
	// Cancel cancels an open order or session (risk-reducing cleanup).
	// Never blocked.
	Cancel ActionClass = "CANCEL"
)

var allActionClasses = []ActionClass{NewRisk, ReduceRisk, Withdraw, Observe, Settle, Reconcile, LedgerPost, Cancel}

// AllActionClasses returns every class in declaration order.
func AllActionClasses() []ActionClass { return append([]ActionClass(nil), allActionClasses...) }

// Valid reports whether c is a declared class.
func (c ActionClass) Valid() bool {
	for _, x := range allActionClasses {
		if x == c {
			return true
		}
	}
	return false
}

// NeverBlocked reports whether the class is exempt from every switch
// (PART 52): observation, settlement, reconciliation, ledger posting and
// cancellation must keep running during any emergency.
func (c ActionClass) NeverBlocked() bool {
	switch c {
	case Observe, Settle, Reconcile, LedgerPost, Cancel:
		return true
	}
	return false
}

type (
	switchKind     struct{}
	transitionKind struct{}
)

// SwitchID identifies a kill_switches row.
type SwitchID = id.ID[switchKind]

// TransitionID identifies a kill_switch_transitions row.
type TransitionID = id.ID[transitionKind]

// NewSwitchID mints a switch id.
func NewSwitchID() SwitchID { return id.New[switchKind]() }

// NewTransitionID mints a transition id.
func NewTransitionID() TransitionID { return id.New[transitionKind]() }
