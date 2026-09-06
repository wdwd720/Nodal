package agent

import (
	"encoding/json"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// RunStatus mirrors the agent_runs.status CHECK.
type RunStatus string

// The run statuses, in the order a successful run walks them.
const (
	RunStarted       RunStatus = "STARTED"
	RunGathering     RunStatus = "GATHERING"
	RunEvaluated     RunStatus = "EVALUATED"
	RunSkipped       RunStatus = "SKIPPED"
	RunPredicted     RunStatus = "PREDICTED"
	RunIntentCreated RunStatus = "INTENT_CREATED"
	RunFailed        RunStatus = "FAILED"
)

var allRunStatuses = []RunStatus{
	RunStarted, RunGathering, RunEvaluated, RunSkipped, RunPredicted, RunIntentCreated, RunFailed,
}

// RunStatuses returns every declared status.
func RunStatuses() []RunStatus { return append([]RunStatus(nil), allRunStatuses...) }

// Valid reports whether s is declared.
func (s RunStatus) Valid() bool {
	for _, v := range allRunStatuses {
		if v == s {
			return true
		}
	}
	return false
}

// String renders the status.
func (s RunStatus) String() string { return string(s) }

// Terminal reports whether the run is finished.
func (s RunStatus) Terminal() bool {
	return s == RunSkipped || s == RunIntentCreated || s == RunFailed
}

// SkipReason mirrors the agent_runs.skip_reason CHECK. A skipped run always
// carries one: "nothing happened" is never an acceptable record.
type SkipReason string

// The declared skip reasons.
const (
	SkipStaleData           SkipReason = "STALE_DATA"
	SkipMissingDependency   SkipReason = "MISSING_DEPENDENCY"
	SkipBudgetExhausted     SkipReason = "BUDGET_EXHAUSTED"
	SkipModelUnavailable    SkipReason = "MODEL_UNAVAILABLE"
	SkipRateLimited         SkipReason = "RATE_LIMITED"
	SkipConditionFalse      SkipReason = "CONDITION_FALSE"
	SkipAgentPaused         SkipReason = "AGENT_PAUSED"
	SkipKillSwitch          SkipReason = "KILL_SWITCH"
	SkipEffectMismatch      SkipReason = "EFFECT_MISMATCH"
	SkipEnvelopeUnavailable SkipReason = "ENVELOPE_UNAVAILABLE"
)

var allSkipReasons = []SkipReason{
	SkipStaleData, SkipMissingDependency, SkipBudgetExhausted, SkipModelUnavailable,
	SkipRateLimited, SkipConditionFalse, SkipAgentPaused, SkipKillSwitch,
	SkipEffectMismatch, SkipEnvelopeUnavailable,
}

// SkipReasons returns every declared skip reason.
func SkipReasons() []SkipReason { return append([]SkipReason(nil), allSkipReasons...) }

// Valid reports whether r is declared.
func (r SkipReason) Valid() bool {
	for _, v := range allSkipReasons {
		if v == r {
			return true
		}
	}
	return false
}

// String renders the reason.
func (r SkipReason) String() string { return string(r) }

// SkipReasonFor maps a typed platform error onto the skip reason the run
// records. Anything unrecognized is MISSING_DEPENDENCY rather than a guess,
// and an error that is not a skip at all returns "" so the caller fails the
// run instead of pretending it was skipped.
func SkipReasonFor(err error) SkipReason {
	switch errs.CodeOf(err) {
	case errs.CodeBudgetExhausted:
		return SkipBudgetExhausted
	case errs.CodeRateLimited:
		return SkipRateLimited
	case errs.CodeModelUnavailable:
		return SkipModelUnavailable
	case errs.CodeStaleMarketData:
		return SkipStaleData
	case errs.CodeKillSwitchActive:
		return SkipKillSwitch
	case errs.CodeEffectForbidden:
		return SkipEffectMismatch
	case errs.CodeProviderUnavailable, errs.CodeNotFound:
		return SkipMissingDependency
	default:
		return ""
	}
}

// TriggerKind mirrors the agent_runs.trigger_kind CHECK.
type TriggerKind string

// The two trigger kinds.
const (
	TriggerOnEvent    TriggerKind = "ON_EVENT"
	TriggerOnInterval TriggerKind = "ON_INTERVAL"
)

// Valid reports whether k is declared.
func (k TriggerKind) Valid() bool { return k == TriggerOnEvent || k == TriggerOnInterval }

// String renders the kind.
func (k TriggerKind) String() string { return string(k) }

// Run is one agent_runs row: one bounded evaluation. Its identity, mode and
// linkage are immutable once inserted (trigger agent_runs_guard, AG004), so a
// run can never be re-attributed to another agent or another mode after the
// fact.
type Run struct {
	ID                   RunID
	AgentID              AgentID
	AgentVersion         int64
	StrategyVersionID    string
	AccountID            string
	EnvelopeID           string
	Mode                 Mode
	TriggerName          string
	TriggerKind          TriggerKind
	TriggerDedupKey      []byte
	TriggerEventID       string
	TriggerSourceEventAt *time.Time
	DecisionTime         time.Time
	Status               RunStatus
	SkipReason           SkipReason
	EvaluatorVersion     string
	EvalInputHash        []byte
	EvalOutputHash       []byte
	InformationSetHash   []byte
	Signals              json.RawMessage
	ConditionResults     json.RawMessage
	Rationale            json.RawMessage
	Evidence             json.RawMessage
	ToolCalls            int
	ModelCalls           int
	DataCostUSD          money.USD
	ModelCostUSD         money.USD
	PredictionID         string
	IntentID             string
	Error                string
	StartedAt            time.Time
	FinishedAt           *time.Time
	CorrelationID        string
	BuildVersion         string
	UpdatedAt            time.Time
}

// Validate mirrors the agent_runs CHECKs.
func (r Run) Validate() error {
	fields := map[string]any{}
	fail := func(k, msg string) {
		if _, dup := fields[k]; !dup {
			fields[k] = msg
		}
	}
	if r.ID.IsZero() {
		fail("id", "required")
	}
	if r.AgentID.IsZero() {
		fail("agent_id", "required")
	}
	if r.AgentVersion < 1 {
		fail("agent_version", "must be >= 1")
	}
	if r.StrategyVersionID == "" {
		fail("strategy_version_id", "required")
	}
	if r.AccountID == "" {
		fail("account_id", "required")
	}
	if !r.Mode.Valid() {
		fail("mode", "must be one of the six modes")
	}
	if r.TriggerName == "" {
		fail("trigger_name", "required")
	}
	if !r.TriggerKind.Valid() {
		fail("trigger_kind", "must be ON_EVENT or ON_INTERVAL")
	}
	if len(r.TriggerDedupKey) == 0 {
		fail("trigger_dedup_key", "required")
	}
	if r.DecisionTime.IsZero() {
		fail("decision_time", "required")
	}
	if !r.Status.Valid() {
		fail("status", "unknown run status")
	}
	if r.Status == RunSkipped && !r.SkipReason.Valid() {
		fail("skip_reason", "a skipped run must name a declared reason")
	}
	if r.SkipReason != "" && !r.SkipReason.Valid() {
		fail("skip_reason", "unknown skip reason")
	}
	if r.Status == RunIntentCreated && (r.IntentID == "" || r.PredictionID == "") {
		fail("intent_id", "INTENT_CREATED requires both a prediction and an intent")
	}
	if r.CorrelationID == "" {
		fail("correlation_id", "required")
	}
	if r.ToolCalls < 0 || r.ModelCalls < 0 {
		fail("tool_calls", "must be >= 0")
	}
	if r.DataCostUSD.IsNegative() || r.ModelCostUSD.IsNegative() {
		fail("data_cost_usd_minor", "must be >= 0")
	}
	if len(fields) == 0 {
		return nil
	}
	return errs.New(errs.CodeValidationFailed, "agent: invalid run").WithFields(fields)
}

// CanAdvance reports whether a run may move from status from to status to.
// Runs move forward only; a terminal status is final. The database enforces
// immutability of identity, not of status order, so this is where order is
// enforced.
func CanAdvance(from, to RunStatus) bool {
	if !from.Valid() || !to.Valid() || from == to || from.Terminal() {
		return false
	}
	// FAILED and SKIPPED are reachable from any non-terminal status.
	if to == RunFailed || to == RunSkipped {
		return true
	}
	order := map[RunStatus]int{
		RunStarted: 0, RunGathering: 1, RunEvaluated: 2, RunPredicted: 3, RunIntentCreated: 4,
	}
	f, okF := order[from]
	t, okT := order[to]
	return okF && okT && t > f
}
