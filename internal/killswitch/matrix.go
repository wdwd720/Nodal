package killswitch

import (
	"sort"
	"time"

	"github.com/nodal/controlplane/internal/errs"
)

// Switch is the current state of one (kind, scope) control.
type Switch struct {
	ID                SwitchID
	Kind              Kind
	ScopeID           string
	Active            bool
	Severity          Severity
	Reason            string
	ActivatedBy       string
	ActivatedAt       *time.Time
	ReleasedBy        string
	ReleasedAt        *time.Time
	ReleaseApprovalID string
	ReleaseReason     string
	Version           int64
	UpdatedAt         time.Time
}

// Action describes a guarded operation. Class is mandatory; the identifiers
// name the dimensions the operation touches and are matched against switch
// scopes (an empty identifier matches nothing). Funding marks a new funding
// session so that FUNDING_DISABLE stops funding without halting trading.
type Action struct {
	Class             ActionClass
	AccountID         string
	AgentID           string
	StrategyVersionID string
	Venue             string
	InstrumentID      string
	Chain             string
	Provider          string
	ModelID           string
	Funding           bool
}

// Validate fails with VALIDATION_FAILED for an unknown class.
func (a Action) Validate() error {
	if !a.Class.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "unknown action class %q", a.Class).WithField("action_class", string(a.Class))
	}
	return nil
}

// Policy holds the few operator-tunable blocking choices of the matrix.
// The zero value is the documented default.
type Policy struct {
	// AccountFreezeBlocksRiskReduction makes ACCOUNT_FREEZE block REDUCE_RISK
	// too. Default false: a frozen account may still close positions.
	AccountFreezeBlocksRiskReduction bool
}

// Matches reports whether the switch's scope applies to the action's
// dimensions, independent of the action class. Global-scope kinds always
// match; id-scoped kinds match when the action carries the same id;
// MODEL_DISABLE with scope "*" matches any action driven by a model.
func Matches(sw Switch, a Action) bool {
	switch sw.Kind {
	case GlobalNewRiskKill, FundingDisable, WithdrawalsDisable:
		return true
	case AccountFreeze:
		return a.AccountID != "" && a.AccountID == sw.ScopeID
	case AgentPause:
		return a.AgentID != "" && a.AgentID == sw.ScopeID
	case StrategyVersionDisable:
		return a.StrategyVersionID != "" && a.StrategyVersionID == sw.ScopeID
	case VenueDisable:
		return a.Venue != "" && a.Venue == sw.ScopeID
	case InstrumentCloseOnly, InstrumentHalt:
		return a.InstrumentID != "" && a.InstrumentID == sw.ScopeID
	case ChainDisableNewActions:
		return a.Chain != "" && a.Chain == sw.ScopeID
	case ProviderDisableNewActions:
		return a.Provider != "" && a.Provider == sw.ScopeID
	case ModelDisable:
		return a.ModelID != "" && (sw.ScopeID == GlobalScope || a.ModelID == sw.ScopeID)
	}
	return false
}

// Blocks is the blocking matrix of POLICY_AUTHORITY §2 for one switch: it
// reports whether an active switch of this kind and scope blocks the
// action. Inactive switches never block.
func Blocks(sw Switch, a Action, p Policy) bool {
	if !sw.Active || a.Class.NeverBlocked() {
		return false
	}
	switch a.Class {
	case NewRisk:
		switch sw.Kind {
		case WithdrawalsDisable:
			return false // withdrawals are not new risk
		case FundingDisable:
			return a.Funding
		}
		return Matches(sw, a)
	case ReduceRisk:
		switch sw.Kind {
		case InstrumentHalt, ChainDisableNewActions, ProviderDisableNewActions:
			return Matches(sw, a)
		case AccountFreeze:
			return p.AccountFreezeBlocksRiskReduction && Matches(sw, a)
		}
		return false
	case Withdraw:
		switch sw.Kind {
		case WithdrawalsDisable, GlobalNewRiskKill, AccountFreeze:
			return Matches(sw, a)
		}
		return false
	}
	// Unknown class: fail closed only if something is active at all.
	return true
}

// sortSwitches orders by (kind, scope) so decisions are deterministic.
func sortSwitches(s []Switch) {
	sort.SliceStable(s, func(i, j int) bool {
		if s[i].Kind != s[j].Kind {
			return s[i].Kind < s[j].Kind
		}
		return s[i].ScopeID < s[j].ScopeID
	})
}

// Blocking returns the first active switch, in (kind, scope) order, that
// blocks the action, if any. It is a pure function: no clock, no I/O.
func Blocking(active []Switch, a Action, p Policy) (Switch, bool) {
	if a.Class.NeverBlocked() {
		return Switch{}, false
	}
	sorted := append([]Switch(nil), active...)
	sortSwitches(sorted)
	for _, sw := range sorted {
		if Blocks(sw, a, p) {
			return sw, true
		}
	}
	return Switch{}, false
}

// BlockedError renders the KILL_SWITCH_ACTIVE rejection for a blocking
// switch, with fields "switch" (kind) and "scope".
func BlockedError(sw Switch, a Action) error {
	return errs.Newf(errs.CodeKillSwitchActive, "kill switch %s is active", sw.Kind).
		WithField("switch", string(sw.Kind)).
		WithField("scope", sw.ScopeID).
		WithField("action_class", string(a.Class))
}

// Snapshot is the list of active switches at one instant, sorted by (kind,
// scope). It is the kill-switch summary the risk kernel takes as input.
type Snapshot struct {
	Switches []Switch
}

// Has reports whether an active switch (kind, scope) is in the snapshot.
func (s Snapshot) Has(kind Kind, scope string) bool {
	for _, sw := range s.Switches {
		if sw.Kind == kind && sw.ScopeID == scope {
			return true
		}
	}
	return false
}

// Global reports whether GLOBAL_NEW_RISK_KILL is active.
func (s Snapshot) Global() bool { return s.Has(GlobalNewRiskKill, GlobalScope) }

// Blocking evaluates the matrix against the snapshot.
func (s Snapshot) Blocking(a Action, p Policy) (Switch, bool) {
	return Blocking(s.Switches, a, p)
}
