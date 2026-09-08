// Package agentauthority is the model of how much an agent may do on a user's
// behalf (gola.md PART XXVIII).
//
// The seven levels are a ladder from "reads and reasons" to "allocates capital
// across strategies", and the distance between adjacent rungs is a legal
// distance as much as a technical one. Level 3 executes a rule the user read
// and approved; level 4 lets the agent choose among options; level 5 lets it
// choose investments. In most jurisdictions the second of those steps is where
// discretionary investment management begins, which is why levels 4 and above
// are capability-gated and disabled by default rather than merely unimplemented.
//
// # What this package is
//
// A leaf: types, a permission matrix, and pure functions. It reads no clock, no
// database and no configuration. It is deliberately separate from
// internal/agent — that package is the runtime, and mixing "what an agent may
// be permitted to do" with "how an agent runs" is how the first quietly becomes
// a property of the second.
//
// # What it must never do
//
//   - Let an agent raise its own level. The level is a property of the
//     user-approved grant, and Permits takes it as an input.
//   - Report a level as usable because it is declared. Declared and enabled are
//     different, and RequiresCapability says which levels need what.
//   - Treat "the user approved a strategy" as "the user approved every action
//     the strategy might take". Level 3's whole point is that the approved
//     thing is a specific deterministic rule.
package agentauthority

import (
	"sort"
	"strconv"
	"strings"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Level is how much authority a user has granted an agent.
type Level int

// Authority levels (PART XXVIII).
const (
	// LevelResearchOnly reads approved data and produces analysis. It cannot
	// propose an action.
	LevelResearchOnly Level = 0
	// LevelRecommendation proposes; the user decides each time.
	LevelRecommendation Level = 1
	// LevelPrepareTransaction prepares a transaction for the user to confirm
	// or sign. Nothing executes without that confirmation.
	LevelPrepareTransaction Level = 2
	// LevelUserApprovedRule executes a deterministic rule the user read and
	// activated, within the bounds they approved. The user approved the RULE,
	// not the agent's judgement.
	LevelUserApprovedRule Level = 3
	// LevelBoundedDiscretion lets the agent choose among options the user
	// pre-approved. This is where the agent's judgement starts to matter.
	LevelBoundedDiscretion Level = 4
	// LevelAutonomousSelection lets the agent choose investments itself.
	LevelAutonomousSelection Level = 5
	// LevelAutonomousPortfolio lets the agent allocate across strategies and
	// assets.
	LevelAutonomousPortfolio Level = 6
)

var allLevels = []Level{
	LevelResearchOnly, LevelRecommendation, LevelPrepareTransaction, LevelUserApprovedRule,
	LevelBoundedDiscretion, LevelAutonomousSelection, LevelAutonomousPortfolio,
}

// AllLevels returns every declared level in ascending order (a copy).
func AllLevels() []Level { return append([]Level(nil), allLevels...) }

// MaxSupportedLevel is the highest level this build implements as product
// architecture. Levels above it are declared, disabled, and require their own
// evidence-backed capability before they could ever be reached.
const MaxSupportedLevel = LevelUserApprovedRule

// Valid reports whether l is a declared level.
func (l Level) Valid() bool { return l >= LevelResearchOnly && l <= LevelAutonomousPortfolio }

// String renders the level as "3 USER_APPROVED_RULE".
func (l Level) String() string {
	if !l.Valid() {
		return strconv.Itoa(int(l)) + " UNKNOWN"
	}
	return strconv.Itoa(int(l)) + " " + levelNames[l]
}

// Name is the bare name of the level.
func (l Level) Name() string {
	if !l.Valid() {
		return "UNKNOWN"
	}
	return levelNames[l]
}

var levelNames = map[Level]string{
	LevelResearchOnly:        "RESEARCH_ONLY",
	LevelRecommendation:      "RECOMMENDATION",
	LevelPrepareTransaction:  "PREPARE_TRANSACTION",
	LevelUserApprovedRule:    "USER_APPROVED_RULE",
	LevelBoundedDiscretion:   "BOUNDED_DISCRETION",
	LevelAutonomousSelection: "AUTONOMOUS_SELECTION",
	LevelAutonomousPortfolio: "AUTONOMOUS_PORTFOLIO",
}

// ParseLevel parses "3" or "USER_APPROVED_RULE" or "3 USER_APPROVED_RULE".
func ParseLevel(s string) (Level, error) {
	t := strings.TrimSpace(strings.ToUpper(s))
	if t == "" {
		return 0, errs.New(errs.CodeValidationFailed, "agent authority level is required")
	}
	if i := strings.IndexByte(t, ' '); i > 0 {
		t = t[i+1:]
	}
	if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
		l := Level(n)
		if !l.Valid() {
			return 0, errs.Newf(errs.CodeValidationFailed, "unknown agent authority level %d", n)
		}
		return l, nil
	}
	for l, name := range levelNames {
		if name == t {
			return l, nil
		}
	}
	return 0, errs.Newf(errs.CodeValidationFailed, "unknown agent authority level %q", s)
}

// SupportedInThisBuild reports whether the level is product architecture here.
//
// Levels above MaxSupportedLevel are not removed and not stubbed: they are
// declared so that adding them later is a capability activation rather than a
// redesign, and refused now so that declaring them does not make them usable.
func (l Level) SupportedInThisBuild() bool { return l.Valid() && l <= MaxSupportedLevel }

// RequiresCapability names the capability gate a level needs before it can be
// granted at all. Levels at or below MaxSupportedLevel need none; above it,
// each has its own, because "we may let agents pick among your chosen options"
// and "we may let agents pick your investments" are different approvals.
func (l Level) RequiresCapability() valuedomain.CapabilityKey {
	switch l {
	case LevelBoundedDiscretion:
		return "AGENT_BOUNDED_DISCRETION"
	case LevelAutonomousSelection:
		return "AGENT_AUTONOMOUS_SELECTION"
	case LevelAutonomousPortfolio:
		return "AGENT_AUTONOMOUS_PORTFOLIO"
	}
	return ""
}

// Action is something an agent might attempt.
type Action string

// Actions, in roughly increasing order of consequence.
const (
	// ActionReadData reads approved data sources.
	ActionReadData Action = "READ_DATA"
	// ActionRequestInference calls a model.
	ActionRequestInference Action = "REQUEST_INFERENCE"
	// ActionCommitPrediction writes to the Prediction Ledger before an
	// outcome is known.
	ActionCommitPrediction Action = "COMMIT_PREDICTION"
	// ActionProposeIntent produces a typed FinancialIntent for a human to
	// look at. It moves nothing.
	ActionProposeIntent Action = "PROPOSE_INTENT"
	// ActionPrepareTransaction builds an unsigned transaction or an
	// unactivated plan.
	ActionPrepareTransaction Action = "PREPARE_TRANSACTION"
	// ActionExecuteApprovedRule executes within an activated, user-approved
	// Strategy IR.
	ActionExecuteApprovedRule Action = "EXECUTE_APPROVED_RULE"
	// ActionSelectAmongApproved chooses between options the user pre-approved.
	ActionSelectAmongApproved Action = "SELECT_AMONG_APPROVED"
	// ActionSelectInstrument chooses an instrument the user did not name.
	ActionSelectInstrument Action = "SELECT_INSTRUMENT"
	// ActionAllocatePortfolio moves capital between strategies.
	ActionAllocatePortfolio Action = "ALLOCATE_PORTFOLIO"

	// The forbidden set. These are never permitted at ANY level, which is why
	// they are listed here rather than omitted: an action that is absent from
	// a matrix is an oversight, and an action that is present and always false
	// is a decision.
	ActionWithdraw           Action = "WITHDRAW"
	ActionTransferValue      Action = "TRANSFER_VALUE"
	ActionChangeOwnLimits    Action = "CHANGE_OWN_LIMITS"
	ActionChangeOwnAuthority Action = "CHANGE_OWN_AUTHORITY"
	ActionEditPolicy         Action = "EDIT_POLICY"
	ActionAccessSecrets      Action = "ACCESS_SECRETS"
	ActionSignArbitrary      Action = "SIGN_ARBITRARY"
	ActionBypassRisk         Action = "BYPASS_RISK"
	ActionCallArbitraryHost  Action = "CALL_ARBITRARY_HOST"
)

var allActions = []Action{
	ActionReadData, ActionRequestInference, ActionCommitPrediction,
	ActionProposeIntent, ActionPrepareTransaction, ActionExecuteApprovedRule,
	ActionSelectAmongApproved, ActionSelectInstrument, ActionAllocatePortfolio,
	ActionWithdraw, ActionTransferValue, ActionChangeOwnLimits, ActionChangeOwnAuthority,
	ActionEditPolicy, ActionAccessSecrets, ActionSignArbitrary, ActionBypassRisk,
	ActionCallArbitraryHost,
}

// AllActions returns every declared action in declaration order (a copy).
func AllActions() []Action { return append([]Action(nil), allActions...) }

// Valid reports whether a is declared.
func (a Action) Valid() bool {
	for _, x := range allActions {
		if x == a {
			return true
		}
	}
	return false
}

// forbiddenAlways is PART XXXI's list: things an agent may never do, at any
// level, with any approval, in any jurisdiction. They are compiled in because
// they are the boundary of the whole agent architecture, and a boundary a
// config flag can move is not a boundary.
// #nosec G101 -- these are ACTION NAMES and the reasons an agent may never
// take them. "AccessSecrets" trips a word-list looking for credentials; the
// map contains no credential and is compiled in precisely so that nothing can
// supply one.
var forbiddenAlways = map[Action]string{
	ActionWithdraw:           "an agent may never withdraw value; a payout is a user action with its own verification",
	ActionTransferValue:      "an agent may never transfer value to an arbitrary destination",
	ActionChangeOwnLimits:    "an agent may never change its own capital limits",
	ActionChangeOwnAuthority: "an agent may never change its own authority level",
	ActionEditPolicy:         "an agent may never edit production or jurisdiction policy",
	ActionAccessSecrets:      "an agent may never reach provider or signing secrets",
	ActionSignArbitrary:      "an agent may never sign an uninspected transaction",
	ActionBypassRisk:         "an agent may never bypass the risk kernel, compliance or reservations",
	ActionCallArbitraryHost:  "an agent may never call an arbitrary network destination",
}

// ForbiddenAlways reports whether the action is permanently forbidden, and why.
func ForbiddenAlways(a Action) (bool, string) {
	why, ok := forbiddenAlways[a]
	return ok, why
}

// minLevel is the lowest level at which each permitted action becomes
// available. An action absent from this map and absent from forbiddenAlways
// would be undecided, which Validate refuses to allow.
var minLevel = map[Action]Level{
	ActionReadData:            LevelResearchOnly,
	ActionRequestInference:    LevelResearchOnly,
	ActionCommitPrediction:    LevelResearchOnly,
	ActionProposeIntent:       LevelRecommendation,
	ActionPrepareTransaction:  LevelPrepareTransaction,
	ActionExecuteApprovedRule: LevelUserApprovedRule,
	ActionSelectAmongApproved: LevelBoundedDiscretion,
	ActionSelectInstrument:    LevelAutonomousSelection,
	ActionAllocatePortfolio:   LevelAutonomousPortfolio,
}

// MinimumLevel returns the lowest level at which an action is available, and
// whether the action is available at any level at all.
func MinimumLevel(a Action) (Level, bool) {
	l, ok := minLevel[a]
	return l, ok
}

// Reason is a machine-readable explanation of a refusal.
type Reason string

// Refusal reasons.
const (
	ReasonUnknownAction     Reason = "UNKNOWN_ACTION"
	ReasonUnknownLevel      Reason = "UNKNOWN_LEVEL"
	ReasonForbiddenAlways   Reason = "FORBIDDEN_AT_EVERY_LEVEL"
	ReasonLevelTooLow       Reason = "AUTHORITY_LEVEL_TOO_LOW"
	ReasonLevelNotSupported Reason = "AUTHORITY_LEVEL_NOT_SUPPORTED_IN_THIS_BUILD"
	ReasonCapabilityOff     Reason = "AUTHORITY_LEVEL_CAPABILITY_NOT_ACTIVE"
)

// Decision is the result of a permission check.
type Decision struct {
	Allowed bool
	Reasons []Reason
	// Detail is operator- and user-facing text. It is present on every refusal,
	// because "denied" without a reason is not an answer anyone can act on.
	Detail string
	// RequiredLevel is the level that would permit the action, when one exists.
	RequiredLevel Level
	// RequiredCapability is the gate that would have to be ACTIVE.
	RequiredCapability valuedomain.CapabilityKey
}

// Permits reports whether an agent at the given level may take an action.
//
// activeCaps is the set of capability keys currently ACTIVE; a nil map means
// nothing is active, which is the correct reading for a fresh deployment.
//
// The order of the checks is deliberate. A permanently forbidden action is
// refused before the level is even consulted, so no combination of level and
// capability can reach it and no future edit to the level matrix can
// accidentally open it.
func Permits(level Level, action Action, activeCaps map[valuedomain.CapabilityKey]bool) Decision {
	if !action.Valid() {
		return Decision{Reasons: []Reason{ReasonUnknownAction},
			Detail: "unknown agent action " + string(action)}
	}
	if forbidden, why := ForbiddenAlways(action); forbidden {
		return Decision{Reasons: []Reason{ReasonForbiddenAlways}, Detail: why}
	}
	if !level.Valid() {
		return Decision{Reasons: []Reason{ReasonUnknownLevel},
			Detail: "unknown agent authority level"}
	}

	need, ok := MinimumLevel(action)
	if !ok {
		// Neither permitted at a level nor explicitly forbidden. Refusing is
		// the only safe reading, and Validate makes this state impossible to
		// reach in a build that has been tested.
		return Decision{Reasons: []Reason{ReasonUnknownAction},
			Detail: "action " + string(action) + " has no declared authority level"}
	}

	d := Decision{RequiredLevel: need, RequiredCapability: need.RequiresCapability()}
	if level < need {
		d.Reasons = append(d.Reasons, ReasonLevelTooLow)
		d.Detail = "this agent is at " + level.String() + "; " + string(action) + " needs " + need.String()
		return d
	}
	if !level.SupportedInThisBuild() {
		d.Reasons = append(d.Reasons, ReasonLevelNotSupported)
		d.Detail = level.String() + " is declared but not enabled in this build"
	}
	if cap := level.RequiresCapability(); cap != "" && !activeCaps[cap] {
		d.Reasons = append(d.Reasons, ReasonCapabilityOff)
		d.RequiredCapability = cap
		if d.Detail == "" {
			d.Detail = level.String() + " requires capability " + string(cap) + " to be ACTIVE"
		}
	}
	if len(d.Reasons) > 0 {
		return d
	}
	d.Allowed = true
	return d
}

// Validate checks the package's own tables for completeness. It is called by a
// test rather than at init, because a build that fails this is broken and
// should fail loudly in CI rather than at a customer's first request.
func Validate() error {
	var problems []string
	for _, a := range allActions {
		_, permitted := minLevel[a]
		forbidden, _ := ForbiddenAlways(a)
		switch {
		case permitted && forbidden:
			problems = append(problems, "action "+string(a)+" is both permitted at a level and forbidden always")
		case !permitted && !forbidden:
			problems = append(problems, "action "+string(a)+" is neither permitted at any level nor forbidden")
		}
	}
	for a, l := range minLevel {
		if !a.Valid() {
			problems = append(problems, "minimum level declared for undeclared action "+string(a))
		}
		if !l.Valid() {
			problems = append(problems, "action "+string(a)+" needs an undeclared level")
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return errs.New(errs.CodeInternal, "agent authority matrix is incomplete").
			WithField("problems", problems)
	}
	return nil
}

// Describe renders the whole matrix as operator-facing text, so what a
// reviewer reads is generated from the table the code enforces rather than a
// prose copy of it that can drift.
func Describe() string {
	var b strings.Builder
	b.WriteString("AGENT AUTHORITY MATRIX\n\n")
	for _, l := range allLevels {
		b.WriteString(l.String())
		if !l.SupportedInThisBuild() {
			b.WriteString("   [DISABLED IN THIS BUILD")
			if c := l.RequiresCapability(); c != "" {
				b.WriteString("; requires " + string(c))
			}
			b.WriteString("]")
		}
		b.WriteString("\n")
		for _, a := range allActions {
			if need, ok := MinimumLevel(a); ok && need == l {
				b.WriteString("    + " + string(a) + "\n")
			}
		}
	}
	b.WriteString("\nNever permitted at any level:\n")
	names := make([]string, 0, len(forbiddenAlways))
	for a := range forbiddenAlways {
		names = append(names, string(a))
	}
	sort.Strings(names)
	for _, n := range names {
		b.WriteString("  - " + n + ": " + forbiddenAlways[Action(n)] + "\n")
	}
	return b.String()
}
