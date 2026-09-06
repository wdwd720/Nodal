package intent

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/idempotency"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

type intentKind struct{}

// IntentID identifies a trade_intents row.
type IntentID = id.ID[intentKind]

// NewIntentID returns a fresh intent id.
func NewIntentID() IntentID { return id.New[intentKind]() }

// ParseIntentID parses the canonical form.
func ParseIntentID(s string) (IntentID, error) { return id.Parse[intentKind](s) }

// Action is what the intent asks for (PART 35).
type Action string

// Actions. BUY_EVENT_OUTCOME is declared so the type system can represent it
// (PART 4 keeps the interface open) but Validate rejects it as UNSUPPORTED
// until prediction markets are enabled by a capability gate.
const (
	ActionAcquireNotional Action = "ACQUIRE_NOTIONAL"
	ActionReduceNotional  Action = "REDUCE_NOTIONAL"
	ActionClosePosition   Action = "CLOSE_POSITION"
	ActionTargetExposure  Action = "TARGET_EXPOSURE"
	ActionBuyEventOutcome Action = "BUY_EVENT_OUTCOME"
)

var enabledActions = []Action{ActionAcquireNotional, ActionReduceNotional, ActionClosePosition, ActionTargetExposure}

// Actions returns the V1 actions Validate accepts, in declaration order.
func Actions() []Action { return append([]Action(nil), enabledActions...) }

// Declared reports whether a is a known action, enabled or not.
func (a Action) Declared() bool { return a.Enabled() || a == ActionBuyEventOutcome }

// Enabled reports whether a may be submitted in this deployment.
func (a Action) Enabled() bool {
	for _, e := range enabledActions {
		if a == e {
			return true
		}
	}
	return false
}

// String returns the wire form.
func (a Action) String() string { return string(a) }

// Mode is the performance mode stored with every intent (PART 159). It is
// never inferred from the UI or from the account; the submitter states it.
type Mode string

// Modes, in promotion order.
const (
	ModeBacktest Mode = "BACKTEST"
	ModePaper    Mode = "PAPER"
	ModeShadow   Mode = "SHADOW"
	ModeCanary   Mode = "CANARY"
	ModeLimited  Mode = "LIMITED"
	ModeLive     Mode = "LIVE"
)

var allModes = []Mode{ModeBacktest, ModePaper, ModeShadow, ModeCanary, ModeLimited, ModeLive}

// Modes returns every mode in promotion order.
func Modes() []Mode { return append([]Mode(nil), allModes...) }

// Valid reports whether m is declared.
func (m Mode) Valid() bool {
	for _, x := range allModes {
		if m == x {
			return true
		}
	}
	return false
}

// String returns the wire form.
func (m Mode) String() string { return string(m) }

// Status is the intent lifecycle state.
type Status string

// Statuses. The main path is RECEIVED → ELIGIBILITY_CHECKED → RISK_CHECKED →
// RESERVED → PLANNED → EXECUTING → COMPLETED; the others are terminal side
// states reachable per Transitions.
const (
	StatusReceived           Status = "RECEIVED"
	StatusEligibilityChecked Status = "ELIGIBILITY_CHECKED"
	StatusRiskChecked        Status = "RISK_CHECKED"
	StatusReserved           Status = "RESERVED"
	StatusPlanned            Status = "PLANNED"
	StatusExecuting          Status = "EXECUTING"
	StatusCompleted          Status = "COMPLETED"
	StatusRejected           Status = "REJECTED"
	StatusExpired            Status = "EXPIRED"
	StatusCancelled          Status = "CANCELLED"
	StatusFailed             Status = "FAILED"
	StatusNoValidPlan        Status = "NO_VALID_PLAN"
)

var allStatuses = []Status{
	StatusReceived, StatusEligibilityChecked, StatusRiskChecked, StatusReserved, StatusPlanned, StatusExecuting,
	StatusCompleted, StatusRejected, StatusExpired, StatusCancelled, StatusFailed, StatusNoValidPlan,
}

var terminalStatuses = []Status{
	StatusCompleted, StatusRejected, StatusExpired, StatusCancelled, StatusFailed, StatusNoValidPlan,
}

// Statuses returns every status in declaration order.
func Statuses() []Status { return append([]Status(nil), allStatuses...) }

// TerminalStatuses returns the statuses with no outgoing transition.
func TerminalStatuses() []Status { return append([]Status(nil), terminalStatuses...) }

// Valid reports whether s is declared.
func (s Status) Valid() bool {
	for _, x := range allStatuses {
		if s == x {
			return true
		}
	}
	return false
}

// IsTerminal reports whether s has no outgoing transition.
func (s Status) IsTerminal() bool {
	for _, x := range terminalStatuses {
		if s == x {
			return true
		}
	}
	return false
}

// String returns the wire form.
func (s Status) String() string { return string(s) }

// Constraints are the submitter's hard limits on execution (PART 35). Zero
// values mean "no limit" for the bps fields, MinReceive, MaxPrice,
// AllowedVenues, QuoteFreshness and ExecutionDeadline; the settlement compiler
// takes the strictest of these and the risk kernel's resulting constraints.
type Constraints struct {
	MaxSlippageBPS    money.BPS
	MaxFeeBPS         money.BPS
	MaxPriceImpactBPS money.BPS
	MaxPrice          *money.Price
	MinReceive        *money.Quantity
	AllowedVenues     []string
	QuoteFreshness    time.Duration // persisted as whole milliseconds
	ExecutionDeadline time.Time
}

// Links are the decision, reservation, plan and order identifiers attached to
// an intent as it moves through the pipeline. They are uuid text; "" means
// not linked.
type Links struct {
	EligibilityDecisionID string `json:"eligibility_decision_id,omitempty"`
	RiskDecisionID        string `json:"risk_decision_id,omitempty"`
	ReservationID         string `json:"reservation_id,omitempty"`
	PlanID                string `json:"plan_id,omitempty"`
	OrderID               string `json:"order_id,omitempty"`
}

// TradeIntent mirrors one trade_intents row. The first block is the request
// as submitted and is what Validate and Canonical cover; the second block is
// server-owned lifecycle state that the Repository maintains.
type TradeIntent struct {
	ID                IntentID
	AccountID         string
	ActorType         security.ActorType
	ActorID           string
	AgentID           *string
	StrategyVersionID *string
	PredictionID      *string
	Action            Action
	InstrumentID      instruments.InstrumentID
	NotionalUSD       *money.USD
	TargetExposureUSD *money.USD
	Quantity          *money.Quantity
	Constraints       Constraints
	Deadline          time.Time
	RequestedAt       time.Time
	IdempotencyKey    string
	CorrelationID     string
	Mode              Mode

	// Lifecycle state, assigned by the Repository.
	Status        Status
	RejectionCode string
	Links         Links
	ReceivedAt    time.Time
	TerminalAt    *time.Time
	ContentHash   []byte
	CreatedAt     time.Time
	UpdatedAt     time.Time
	// Existing is true when Create or Submit returned an intent that was
	// already persisted by an earlier command with the same idempotency key.
	Existing bool
}

// Limits enforced by Validate.
const (
	MaxActorIDLength       = 256
	MaxCorrelationIDLength = 256
	MaxVenueCodeLength     = 64
	MaxAllowedVenues       = 32
	MaxBPS                 = money.OneHundredPercent
)

// submittableActorTypes mirrors the trade_intents.actor_type CHECK.
var submittableActorTypes = []security.ActorType{security.ActorUser, security.ActorAgent, security.ActorOperator}

// CanSubmit reports whether actor type a may own an intent (USER, AGENT or
// OPERATOR). SERVICE and SYSTEM actors transition intents but never submit.
func CanSubmit(a security.ActorType) bool {
	for _, x := range submittableActorTypes {
		if a == x {
			return true
		}
	}
	return false
}

// Validate checks the request block of t. It returns nil, a
// VALIDATION_FAILED *errs.Error whose Fields map every offending field to a
// message, or UNSUPPORTED for the declared-but-disabled BUY_EVENT_OUTCOME
// action. It never panics and never consults the database or a clock.
//
// Action/field rules (stricter than the table CHECK, which is the last line
// of defense):
//
//	ACQUIRE_NOTIONAL  NotionalUSD > 0; no TargetExposureUSD, no Quantity
//	REDUCE_NOTIONAL   exactly one of NotionalUSD > 0 or Quantity > 0; no TargetExposureUSD
//	CLOSE_POSITION    no amount at all (the whole position closes)
//	TARGET_EXPOSURE   TargetExposureUSD >= 0; no NotionalUSD, no Quantity
func (t TradeIntent) Validate() error {
	if !t.Action.Declared() {
		return errs.New(errs.CodeValidationFailed, "intent: invalid").WithField("action", "unknown action")
	}
	if !t.Action.Enabled() {
		return errs.Newf(errs.CodeUnsupported, "intent: action %s is not enabled in this deployment", t.Action).
			WithField("action", string(t.Action))
	}
	fields := map[string]any{}
	fail := func(k, msg string) {
		if _, dup := fields[k]; !dup {
			fields[k] = msg
		}
	}
	t.validateIdentity(fail)
	t.validateActor(fail)
	t.validateAmounts(fail)
	t.validateTimes(fail)
	t.Constraints.validate(fail, t.RequestedAt, t.Deadline)
	if len(fields) == 0 {
		return nil
	}
	return errs.New(errs.CodeValidationFailed, "intent: invalid").WithFields(fields)
}

func (t TradeIntent) validateIdentity(fail func(k, msg string)) {
	if t.ID.IsZero() {
		fail("id", "required")
	}
	if _, err := accounts.ParseAccountID(t.AccountID); err != nil {
		fail("account_id", "must be a canonical uuid")
	}
	if t.InstrumentID.IsZero() {
		fail("instrument_id", "required")
	}
	if !t.Mode.Valid() {
		fail("mode", "must be one of BACKTEST, PAPER, SHADOW, CANARY, LIMITED, LIVE")
	}
	checkText(fail, "idempotency_key", t.IdempotencyKey, idempotency.MaxKeyLength)
	checkText(fail, "correlation_id", t.CorrelationID, MaxCorrelationIDLength)
}

func (t TradeIntent) validateActor(fail func(k, msg string)) {
	if !CanSubmit(t.ActorType) {
		fail("actor_type", "must be USER, AGENT or OPERATOR")
	}
	checkText(fail, "actor_id", t.ActorID, MaxActorIDLength)
	linkage := []struct {
		name  string
		value *string
	}{
		{"agent_id", t.AgentID},
		{"strategy_version_id", t.StrategyVersionID},
		{"prediction_id", t.PredictionID},
	}
	for _, l := range linkage {
		switch {
		case t.ActorType == security.ActorAgent && l.value == nil:
			fail(l.name, "required for AGENT intents")
		case t.ActorType != security.ActorAgent && l.value != nil:
			fail(l.name, "only AGENT intents carry it")
		case l.value != nil:
			if _, err := id.ParseAny(*l.value); err != nil {
				fail(l.name, "must be a canonical uuid")
			}
		}
	}
}

func (t TradeIntent) validateAmounts(fail func(k, msg string)) {
	if t.NotionalUSD != nil && !t.NotionalUSD.IsPositive() {
		fail("notional_usd", "must be positive")
	}
	if t.TargetExposureUSD != nil && t.TargetExposureUSD.IsNegative() {
		fail("target_exposure_usd", "must not be negative")
	}
	if t.Quantity != nil && !t.Quantity.IsPositive() {
		fail("quantity", "must be positive")
	}
	forbid := func(name string, present bool) {
		if present {
			fail(name, "not allowed for "+string(t.Action))
		}
	}
	switch t.Action {
	case ActionAcquireNotional:
		if t.NotionalUSD == nil {
			fail("notional_usd", "required for ACQUIRE_NOTIONAL")
		}
		forbid("target_exposure_usd", t.TargetExposureUSD != nil)
		forbid("quantity", t.Quantity != nil)
	case ActionReduceNotional:
		switch {
		case t.NotionalUSD == nil && t.Quantity == nil:
			fail("notional_usd", "REDUCE_NOTIONAL needs notional_usd or quantity")
		case t.NotionalUSD != nil && t.Quantity != nil:
			fail("quantity", "REDUCE_NOTIONAL takes notional_usd or quantity, not both")
		}
		forbid("target_exposure_usd", t.TargetExposureUSD != nil)
	case ActionClosePosition:
		forbid("notional_usd", t.NotionalUSD != nil)
		forbid("target_exposure_usd", t.TargetExposureUSD != nil)
		forbid("quantity", t.Quantity != nil)
	case ActionTargetExposure:
		if t.TargetExposureUSD == nil {
			fail("target_exposure_usd", "required for TARGET_EXPOSURE")
		}
		forbid("notional_usd", t.NotionalUSD != nil)
		forbid("quantity", t.Quantity != nil)
	}
}

func (t TradeIntent) validateTimes(fail func(k, msg string)) {
	if t.RequestedAt.IsZero() {
		fail("requested_at", "required")
		return
	}
	if !t.Deadline.IsZero() && !t.Deadline.After(t.RequestedAt) {
		fail("deadline", "must be after requested_at")
	}
}

func (c Constraints) validate(fail func(k, msg string), requestedAt, deadline time.Time) {
	for _, b := range []struct {
		name  string
		value money.BPS
	}{
		{"constraints.max_slippage_bps", c.MaxSlippageBPS},
		{"constraints.max_fee_bps", c.MaxFeeBPS},
		{"constraints.max_price_impact_bps", c.MaxPriceImpactBPS},
	} {
		if b.value < 0 || b.value > MaxBPS {
			fail(b.name, "must be within 0..10000")
		}
	}
	if c.MaxPrice != nil {
		switch {
		case c.MaxPrice.Validate() != nil:
			fail("constraints.max_price", "invalid price")
		case !c.MaxPrice.Mantissa.IsPositive():
			fail("constraints.max_price", "must be positive")
		}
	}
	if c.MinReceive != nil && !c.MinReceive.IsPositive() {
		fail("constraints.min_receive", "must be positive")
	}
	if len(c.AllowedVenues) > MaxAllowedVenues {
		fail("constraints.allowed_venues", "too many venues")
	}
	seen := make(map[string]struct{}, len(c.AllowedVenues))
	for _, v := range c.AllowedVenues {
		switch {
		case strings.TrimSpace(v) == "" || v != strings.TrimSpace(v):
			fail("constraints.allowed_venues", "venue codes must be non-empty and trimmed")
		case len(v) > MaxVenueCodeLength || !isClean(v):
			fail("constraints.allowed_venues", "invalid venue code")
		}
		if _, dup := seen[v]; dup {
			fail("constraints.allowed_venues", "duplicate venue code")
		}
		seen[v] = struct{}{}
	}
	switch {
	case c.QuoteFreshness < 0:
		fail("constraints.quote_freshness", "must not be negative")
	case c.QuoteFreshness%time.Millisecond != 0:
		fail("constraints.quote_freshness", "must be a whole number of milliseconds")
	}
	if !c.ExecutionDeadline.IsZero() && !requestedAt.IsZero() {
		switch {
		case !c.ExecutionDeadline.After(requestedAt):
			fail("constraints.execution_deadline", "must be after requested_at")
		case !deadline.IsZero() && c.ExecutionDeadline.After(deadline):
			fail("constraints.execution_deadline", "must not be after deadline")
		}
	}
}

// checkText requires a non-blank, bounded, clean string.
func checkText(fail func(k, msg string), name, v string, maxLen int) {
	switch {
	case strings.TrimSpace(v) == "":
		fail(name, "required")
	case len(v) > maxLen:
		fail(name, "too long")
	case !isClean(v):
		fail(name, "must be valid utf-8 without control characters")
	}
}

// isClean reports valid UTF-8 without control characters.
func isClean(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
