package agents

import (
	"sort"
	"time"

	"github.com/nodal/controlplane/internal/agent"
	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
)

type grantKind struct{}

// GrantID identifies one agent_grants row.
type GrantID = id.ID[grantKind]

// NewGrantID returns a fresh grant id.
func NewGrantID() GrantID { return id.New[grantKind]() }

// ParseGrantID parses the canonical form.
func ParseGrantID(s string) (GrantID, error) { return id.Parse[grantKind](s) }

// ScheduleKind is how often an agent may look. It mirrors the
// agent_grants.schedule_kind CHECK.
type ScheduleKind string

// The two schedule kinds.
const (
	// ScheduleManual means the agent evaluates only when a person asks it to.
	// It is the default, because "runs by itself" is a decision and a default
	// that made it for the user would be exactly the wording goal §17 forbids.
	ScheduleManual ScheduleKind = "MANUAL"
	// ScheduleInterval means the agent evaluates every IntervalMinutes.
	ScheduleInterval ScheduleKind = "INTERVAL"
)

var allScheduleKinds = []ScheduleKind{ScheduleManual, ScheduleInterval}

// ScheduleKinds returns every declared schedule kind, for the enum agreement
// test and for rendering.
func ScheduleKinds() []ScheduleKind { return append([]ScheduleKind(nil), allScheduleKinds...) }

// Valid reports whether k is declared.
func (k ScheduleKind) Valid() bool {
	for _, v := range allScheduleKinds {
		if v == k {
			return true
		}
	}
	return false
}

// String renders the kind.
func (k ScheduleKind) String() string { return string(k) }

// Schedule bounds (agent_grants.schedule_interval_minutes CHECK).
const (
	MinIntervalMinutes = 5
	MaxIntervalMinutes = 10080 // one week
	// MaxAllowedAssets mirrors the cardinality CHECK. A universe wider than
	// this is not a universe, it is an absence of one.
	MaxAllowedAssets = 64
)

// Schedule is how often the agent may evaluate.
type Schedule struct {
	Kind ScheduleKind
	// IntervalMinutes is set exactly when Kind is INTERVAL.
	IntervalMinutes int
}

// Validate mirrors the table's CHECKs so a bad schedule is a typed
// VALIDATION_FAILED rather than a SQLSTATE 23514.
func (s Schedule) Validate() map[string]any {
	fields := map[string]any{}
	if !s.Kind.Valid() {
		fields["schedule.kind"] = "must be MANUAL or INTERVAL"
		return fields
	}
	switch s.Kind {
	case ScheduleManual:
		if s.IntervalMinutes != 0 {
			fields["schedule.interval_minutes"] = "a MANUAL schedule has no interval"
		}
	case ScheduleInterval:
		if s.IntervalMinutes < MinIntervalMinutes || s.IntervalMinutes > MaxIntervalMinutes {
			fields["schedule.interval_minutes"] = "must be between 5 and 10080 minutes"
		}
	}
	return fields
}

// Limits is the bound the owner put on an agent, in Credits and basis points.
//
// Every money field is a money.Quantity: exact base units backed by big.Int.
// Nothing here is ever a float, and nothing here is a currency amount — Credits
// are a closed-loop internal unit and these are ceilings on how many of them
// may be at risk.
type Limits struct {
	// BudgetCredits is the hard ceiling on Credits at risk. It is a LIMIT, not
	// a reservation: no ledger row is written for it (D-076).
	BudgetCredits money.Quantity
	// PerTradeCapCredits bounds a single trade.
	PerTradeCapCredits money.Quantity
	// DailyLossStopCredits stops the agent for the day once realised losses
	// reach it.
	DailyLossStopCredits money.Quantity
	// MaxPositionShareBPS is the largest share of the budget one position may
	// take, in basis points (10000 = the whole budget).
	MaxPositionShareBPS money.BPS
	// AllowedAssets is the universe. Never empty: an agent whose universe is
	// unstated is an agent whose universe is whatever the strategy names.
	AllowedAssets []assets.AssetID
	Schedule      Schedule
}

// Validate reports every structural reason the limits are not grantable.
func (l Limits) Validate() error {
	fields := map[string]any{}
	positive := func(name string, q money.Quantity) bool {
		if !q.IsPositive() {
			fields[name] = "must be a positive whole number of Credits"
			return false
		}
		return true
	}
	budgetOK := positive("budget_credits", l.BudgetCredits)
	perTradeOK := positive("per_trade_cap_credits", l.PerTradeCapCredits)
	lossOK := positive("daily_loss_stop_credits", l.DailyLossStopCredits)
	if budgetOK && perTradeOK && l.PerTradeCapCredits.Cmp(l.BudgetCredits) > 0 {
		fields["per_trade_cap_credits"] = "cannot exceed the budget it caps"
	}
	if budgetOK && lossOK && l.DailyLossStopCredits.Cmp(l.BudgetCredits) > 0 {
		fields["daily_loss_stop_credits"] = "cannot exceed the budget it stops"
	}
	if l.MaxPositionShareBPS < 1 || l.MaxPositionShareBPS > money.OneHundredPercent {
		fields["max_position_share_bps"] = "must be between 1 and 10000 basis points"
	}
	switch {
	case len(l.AllowedAssets) == 0:
		fields["allowed_assets"] = "name at least one asset the agent may trade"
	case len(l.AllowedAssets) > MaxAllowedAssets:
		fields["allowed_assets"] = "at most 64 assets"
	default:
		seen := map[string]struct{}{}
		for _, a := range l.AllowedAssets {
			if a.IsZero() {
				fields["allowed_assets"] = "every asset must be a canonical UUID"
				break
			}
			if _, dup := seen[a.String()]; dup {
				fields["allowed_assets"] = "the same asset is listed twice"
				break
			}
			seen[a.String()] = struct{}{}
		}
	}
	for k, v := range l.Schedule.Validate() {
		fields[k] = v
	}
	if len(fields) == 0 {
		return nil
	}
	return errs.New(errs.CodeValidationFailed, "agents: the limits on this agent are not grantable").WithFields(fields)
}

// AllowedAssetStrings renders the universe for SQL, sorted so two grants with
// the same universe store it identically.
func (l Limits) AllowedAssetStrings() []string {
	out := make([]string, 0, len(l.AllowedAssets))
	for _, a := range l.AllowedAssets {
		out = append(out, a.String())
	}
	sort.Strings(out)
	return out
}

// Grant is one agent_grants row: what a person authorised, when, and inside
// what bounds.
type Grant struct {
	ID                GrantID
	AgentID           agent.AgentID
	AccountID         string
	StrategyVersionID string
	Level             agentauthority.Level
	Limits            Limits
	GrantedByUserID   string
	GrantedAt         time.Time
	ArchivedAt        *time.Time
}

// Archived reports whether the grant has been archived.
func (g Grant) Archived() bool { return g.ArchivedAt != nil }

// Validate mirrors the table's constraints.
func (g Grant) Validate() error {
	fields := map[string]any{}
	if g.ID.IsZero() {
		fields["id"] = "required"
	}
	if g.AgentID.IsZero() {
		fields["agent_id"] = "required"
	}
	if g.AccountID == "" {
		fields["account_id"] = "required"
	}
	if g.StrategyVersionID == "" {
		fields["strategy_version_id"] = "required: an agent is created from a compiled strategy version"
	}
	if g.GrantedByUserID == "" {
		fields["granted_by_user_id"] = "required: a grant is something a person made"
	}
	if !g.Level.SupportedInThisBuild() {
		fields["authority_level"] = "not enabled in this build"
	}
	if len(fields) > 0 {
		return errs.New(errs.CodeValidationFailed, "agents: grant is invalid").WithFields(fields)
	}
	return g.Limits.Validate()
}
