package agent

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// BudgetKind names one of the independent budgets of AGENT_RUNTIME.md §6.
// Each is checked on its own; none relies on another to fail first.
type BudgetKind string

// The budgets. CAPITAL is enforced by the reservation path, not here; it is
// named so a refusal can be attributed consistently.
const (
	BudgetData      BudgetKind = "DATA"
	BudgetModel     BudgetKind = "MODEL"
	BudgetOrderRate BudgetKind = "ORDER_RATE"
	BudgetCapital   BudgetKind = "CAPITAL"
)

// String renders the kind.
func (k BudgetKind) String() string { return string(k) }

// BudgetLimit is the intersected cap for one budget. A zero value in any
// field means "not capped by this budget"; the other budgets and the risk
// kernel still apply.
type BudgetLimit struct {
	Kind            BudgetKind
	CallsPerRun     int
	CallsPerDay     int
	SpendPerDay     money.USD
	MaxInputTokens  int64
	MaxOutputTokens int64
}

// canonical renders the limit for the authority fingerprint.
func (l BudgetLimit) canonical() string {
	var b strings.Builder
	b.WriteString(string(l.Kind))
	b.WriteByte('|')
	b.WriteString(strconv.Itoa(l.CallsPerRun))
	b.WriteByte('|')
	b.WriteString(strconv.Itoa(l.CallsPerDay))
	b.WriteByte('|')
	b.WriteString(strconv.FormatInt(l.SpendPerDay.Minor(), 10))
	b.WriteByte('|')
	b.WriteString(strconv.FormatInt(l.MaxInputTokens, 10))
	b.WriteByte('|')
	b.WriteString(strconv.FormatInt(l.MaxOutputTokens, 10))
	return b.String()
}

// BudgetUsage is what has already been consumed, read from persisted
// counters. Redis may pre-check; Postgres decides (PART 66 §3).
type BudgetUsage struct {
	CallsThisRun int
	CallsToday   int
	SpentToday   money.USD
}

// BudgetSnapshot is the state of every budget for one agent at one instant.
// It is read inside the broker's transaction immediately before a call, so a
// concurrent run cannot spend the same allowance twice.
type BudgetSnapshot struct {
	AgentID AgentID
	RunID   RunID
	// Day is the UTC day the daily counters cover.
	Day time.Time
	// HourStart is the start of the rolling window the order-rate counter
	// covers.
	HourStart time.Time

	Data      BudgetLimit
	DataUsed  BudgetUsage
	Model     BudgetLimit
	ModelUsed BudgetUsage

	IntentsPerHour  int
	IntentsThisHour int
}

// budgetError is the single refusal shape. The run records skip_reason
// BUDGET_EXHAUSTED and the field names which budget ran out, so an operator
// never has to guess.
func budgetError(kind BudgetKind, detail string) *errs.Error {
	return errs.Newf(errs.CodeBudgetExhausted, "agent: %s budget exhausted: %s", kind, detail).
		WithField("budget", kind.String())
}

// CheckTool reports whether one more tool call costing cost may be made. It
// is called before the adapter is constructed, so a refusal means zero
// provider dials.
func (s BudgetSnapshot) CheckTool(cost money.USD) error {
	return checkOne(BudgetData, s.Data, s.DataUsed, cost)
}

// CheckModel reports whether one more model call costing at most cost may be
// made.
func (s BudgetSnapshot) CheckModel(cost money.USD) error {
	return checkOne(BudgetModel, s.Model, s.ModelUsed, cost)
}

// CheckIntent reports whether one more agent intent may be created in the
// current hour.
func (s BudgetSnapshot) CheckIntent() error {
	if s.IntentsPerHour > 0 && s.IntentsThisHour >= s.IntentsPerHour {
		return errs.Newf(errs.CodeRateLimited, "agent: %d intents already created this hour (limit %d)",
			s.IntentsThisHour, s.IntentsPerHour).WithField("budget", BudgetOrderRate.String())
	}
	return nil
}

// checkOne applies the three independent caps of a limit in a fixed order:
// per-run calls, per-day calls, then per-day spend.
func checkOne(kind BudgetKind, l BudgetLimit, used BudgetUsage, cost money.USD) error {
	if cost.IsNegative() {
		return errs.New(errs.CodeValidationFailed, "agent: negative call cost")
	}
	if l.CallsPerRun > 0 && used.CallsThisRun >= l.CallsPerRun {
		return budgetError(kind, "calls per run: "+strconv.Itoa(used.CallsThisRun)+" of "+strconv.Itoa(l.CallsPerRun))
	}
	if l.CallsPerDay > 0 && used.CallsToday >= l.CallsPerDay {
		return budgetError(kind, "calls per day: "+strconv.Itoa(used.CallsToday)+" of "+strconv.Itoa(l.CallsPerDay))
	}
	if !l.SpendPerDay.IsZero() {
		after, err := used.SpentToday.Add(cost)
		if err != nil {
			return errs.Wrap(err, errs.CodeOverflow, "agent: budget arithmetic overflowed")
		}
		if after.Cmp(l.SpendPerDay) > 0 {
			return budgetError(kind, "spend per day: "+after.String()+" would exceed "+l.SpendPerDay.String())
		}
	}
	return nil
}

// RemainingToolCalls is how many more tool calls this run may make, or -1
// when neither cap applies.
func (s BudgetSnapshot) RemainingToolCalls() int {
	return remaining(s.Data, s.DataUsed)
}

// RemainingModelCalls is how many more model calls this run may make, or -1
// when neither cap applies.
func (s BudgetSnapshot) RemainingModelCalls() int {
	return remaining(s.Model, s.ModelUsed)
}

func remaining(l BudgetLimit, used BudgetUsage) int {
	out := -1
	if l.CallsPerRun > 0 {
		out = l.CallsPerRun - used.CallsThisRun
	}
	if l.CallsPerDay > 0 {
		d := l.CallsPerDay - used.CallsToday
		if out < 0 || d < out {
			out = d
		}
	}
	if out < 0 && (l.CallsPerRun > 0 || l.CallsPerDay > 0) {
		return 0
	}
	return out
}

// BudgetReader reads the persisted counters the budgets are enforced from.
type BudgetReader interface {
	Snapshot(ctx context.Context, q db.Querier, a Authority, runID RunID, now time.Time) (BudgetSnapshot, error)
}

// PGBudgetReader reads tool_invocations, model_calls and trade_intents. Only
// successful and rate-limited calls consume the call counters; a call the
// broker itself refused for budget reasons is recorded with budget_refused
// and does not consume anything, so a refusal can never cascade.
type PGBudgetReader struct{}

var _ BudgetReader = PGBudgetReader{}

// NewBudgetReader returns the PostgreSQL budget reader.
func NewBudgetReader() PGBudgetReader { return PGBudgetReader{} }

const selectDataUsageSQL = `
SELECT
    count(*) FILTER (WHERE agent_run_id = $2 AND NOT budget_refused),
    count(*) FILTER (WHERE created_at >= $3 AND created_at < $4 AND NOT budget_refused),
    coalesce(sum(cost_usd_minor) FILTER (WHERE created_at >= $3 AND created_at < $4), 0)
  FROM tool_invocations
 WHERE agent_id = $1 AND effect <> 'CALL_MODEL'`

const selectModelUsageSQL = `
SELECT
    count(*) FILTER (WHERE mc.agent_run_id = $2),
    count(*) FILTER (WHERE mc.request_at >= $3 AND mc.request_at < $4),
    coalesce(sum(mc.cost_usd_minor) FILTER (WHERE mc.request_at >= $3 AND mc.request_at < $4), 0)
  FROM model_calls mc
  JOIN agent_runs r ON r.id = mc.agent_run_id
 WHERE r.agent_id = $1 AND mc.purpose = 'RUNTIME'`

const selectIntentRateSQL = `
SELECT count(*) FROM trade_intents WHERE agent_id = $1 AND requested_at >= $2`

// Snapshot reads every counter in one round of queries against q. Pass the
// broker's transaction so the counters and the invocation row that follows
// commit together.
func (PGBudgetReader) Snapshot(ctx context.Context, q db.Querier, a Authority, runID RunID, now time.Time) (BudgetSnapshot, error) {
	utc := now.UTC()
	dayStart := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour)
	hourStart := utc.Add(-time.Hour)

	s := BudgetSnapshot{
		AgentID:        a.AgentID(),
		RunID:          runID,
		Day:            dayStart,
		HourStart:      hourStart,
		Data:           a.DataBudget(),
		Model:          a.ModelBudget(),
		IntentsPerHour: a.IntentsPerHour(),
	}
	var dataSpend, modelSpend int64
	if err := q.QueryRow(ctx, selectDataUsageSQL, a.AgentID(), runID, dayStart, dayEnd).
		Scan(&s.DataUsed.CallsThisRun, &s.DataUsed.CallsToday, &dataSpend); err != nil {
		return BudgetSnapshot{}, errs.Wrap(err, errs.CodeInternal, "agent: read data budget counters")
	}
	if err := q.QueryRow(ctx, selectModelUsageSQL, a.AgentID(), runID, dayStart, dayEnd).
		Scan(&s.ModelUsed.CallsThisRun, &s.ModelUsed.CallsToday, &modelSpend); err != nil {
		return BudgetSnapshot{}, errs.Wrap(err, errs.CodeInternal, "agent: read model budget counters")
	}
	if err := q.QueryRow(ctx, selectIntentRateSQL, a.AgentID(), hourStart).Scan(&s.IntentsThisHour); err != nil {
		return BudgetSnapshot{}, errs.Wrap(err, errs.CodeInternal, "agent: read intent rate counter")
	}
	s.DataUsed.SpentToday = money.USDFromMinor(dataSpend)
	s.ModelUsed.SpentToday = money.USDFromMinor(modelSpend)
	return s, nil
}
