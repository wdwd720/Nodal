package agent

import (
	"time"

	"github.com/nodal/controlplane/internal/errs"
)

// Stage is a position on the promotion ladder (PART 68). It is the furthest
// point an agent has reached and the position a pause returns to.
type Stage string

// The eight ladder positions, in order. Nothing may be skipped.
const (
	StageDraft            Stage = "DRAFT"
	StageCompiled         Stage = "COMPILED"
	StageValidated        Stage = "VALIDATED"
	StageBacktestEligible Stage = "BACKTEST_ELIGIBLE"
	StageShadow           Stage = "SHADOW"
	StageCanary           Stage = "CANARY"
	StageLimited          Stage = "LIMITED"
	StageLive             Stage = "LIVE"
)

// ladder is the ordered promotion path; the index is the rung number.
var ladder = []Stage{
	StageDraft, StageCompiled, StageValidated, StageBacktestEligible,
	StageShadow, StageCanary, StageLimited, StageLive,
}

// Stages returns the ladder in order.
func Stages() []Stage { return append([]Stage(nil), ladder...) }

// Valid reports whether s is a declared ladder position.
func (s Stage) Valid() bool { return s.rung() >= 0 }

// String renders the stage.
func (s Stage) String() string { return string(s) }

// State returns the active state that corresponds to the stage.
func (s Stage) State() State { return State(s) }

// rung is the ladder index of s, or -1.
func (s Stage) rung() int {
	for i, v := range ladder {
		if v == s {
			return i
		}
	}
	return -1
}

// RealCapital reports whether the stage deploys capital that can move on a
// venue. CANARY is platform capital, LIMITED and LIVE are customer capital;
// all three require a bound envelope (enforced by the agents CHECK) and a
// dual-controlled approval to enter.
func (s Stage) RealCapital() bool {
	return s == StageCanary || s == StageLimited || s == StageLive
}

// State is the agent's current state: a ladder position while active, or one
// of the four side states.
type State string

// Active states mirror the ladder; side states do not.
const (
	StateDraft            = State(StageDraft)
	StateCompiled         = State(StageCompiled)
	StateValidated        = State(StageValidated)
	StateBacktestEligible = State(StageBacktestEligible)
	StateShadow           = State(StageShadow)
	StateCanary           = State(StageCanary)
	StateLimited          = State(StageLimited)
	StateLive             = State(StageLive)

	// StatePaused suspends new work; the agent returns to its stage on resume.
	StatePaused State = "PAUSED"
	// StateFailed is an unrecoverable evaluator, strategy or envelope failure.
	StateFailed State = "FAILED"
	// StateRevoked is terminal: an operator or security decision.
	StateRevoked State = "REVOKED"
	// StateSuperseded is terminal: a newer agent replaced this one.
	StateSuperseded State = "SUPERSEDED"
)

// sideStates are the four non-ladder states the agents CHECK allows.
var sideStates = []State{StatePaused, StateFailed, StateRevoked, StateSuperseded}

// States returns every declared state: the ladder, then the side states.
func States() []State {
	out := make([]State, 0, len(ladder)+len(sideStates))
	for _, s := range ladder {
		out = append(out, s.State())
	}
	return append(out, sideStates...)
}

// Valid reports whether s is a declared state.
func (s State) Valid() bool {
	if Stage(s).Valid() {
		return true
	}
	for _, v := range sideStates {
		if v == s {
			return true
		}
	}
	return false
}

// String renders the state.
func (s State) String() string { return string(s) }

// IsStage reports whether s is a ladder position rather than a side state.
func (s State) IsStage() bool { return Stage(s).Valid() }

// IsSide reports whether s is one of PAUSED, FAILED, REVOKED, SUPERSEDED.
func (s State) IsSide() bool { return s.Valid() && !s.IsStage() }

// IsTerminal reports whether no transition leaves s. REVOKED and SUPERSEDED
// are terminal; FAILED is not terminal, but its only exits are REVOKED and
// SUPERSEDED because AGENT_RUNTIME.md §1 lists no recovery transition.
func (s State) IsTerminal() bool { return s == StateRevoked || s == StateSuperseded }

// Runs reports whether an agent in this state may open runs. Stages below
// BACKTEST_ELIGIBLE never run, and no side state runs.
func (s State) Runs() bool {
	return s.IsStage() && Stage(s).rung() >= StageBacktestEligible.rung()
}

// CanTransition reports whether from -> to is a legal state change.
//
// Rules, exactly as PART 68 and AGENT_RUNTIME.md §1 state them:
//
//   - ladder -> ladder: only the next rung. Stages are never skipped and
//     never walked backwards (reducing authority is PAUSE or REVOKE).
//   - ladder -> PAUSED / FAILED / REVOKED / SUPERSEDED: always allowed.
//   - PAUSED -> a ladder position: resume. CanTransition is stateless, so it
//     admits any rung here; Lifecycle.Resume additionally requires the target
//     to equal the agent's recorded stage.
//   - PAUSED -> FAILED / REVOKED / SUPERSEDED: allowed.
//   - FAILED -> REVOKED / SUPERSEDED: allowed. Nothing else leaves FAILED.
//   - REVOKED, SUPERSEDED: terminal.
//   - A state never transitions to itself.
func CanTransition(from, to State) bool {
	if !from.Valid() || !to.Valid() || from == to || from.IsTerminal() {
		return false
	}
	if to == StateRevoked || to == StateSuperseded {
		return true
	}
	switch {
	case from.IsStage():
		if to == StatePaused || to == StateFailed {
			return true
		}
		return to.IsStage() && Stage(to).rung() == Stage(from).rung()+1
	case from == StatePaused:
		return to.IsStage() || to == StateFailed
	default: // FAILED: only the two terminal exits, handled above.
		return false
	}
}

// Mode is the performance mode a run, prediction and intent carry (PART 159).
// It is copied from the agent at run creation and never inferred.
type Mode string

// The six modes.
const (
	ModeBacktest Mode = "BACKTEST"
	ModePaper    Mode = "PAPER"
	ModeShadow   Mode = "SHADOW"
	ModeCanary   Mode = "CANARY"
	ModeLimited  Mode = "LIMITED"
	ModeLive     Mode = "LIVE"
)

var allModes = []Mode{ModeBacktest, ModePaper, ModeShadow, ModeCanary, ModeLimited, ModeLive}

// Modes returns every declared mode.
func Modes() []Mode { return append([]Mode(nil), allModes...) }

// Valid reports whether m is declared.
func (m Mode) Valid() bool {
	for _, v := range allModes {
		if v == m {
			return true
		}
	}
	return false
}

// String renders the mode.
func (m Mode) String() string { return string(m) }

// RealCapital reports whether orders in this mode reach a venue with money
// behind them.
func (m Mode) RealCapital() bool {
	return m == ModeCanary || m == ModeLimited || m == ModeLive
}

// stageModes mirrors the agents CHECK: which modes each stage admits.
// DRAFT, COMPILED and VALIDATED carry no mode at all.
var stageModes = map[Stage][]Mode{
	StageDraft:            nil,
	StageCompiled:         nil,
	StageValidated:        nil,
	StageBacktestEligible: {ModeBacktest, ModePaper},
	StageShadow:           {ModeShadow},
	StageCanary:           {ModeCanary},
	StageLimited:          {ModeLimited},
	StageLive:             {ModeLive},
}

// ModesForStage returns the modes a stage admits; empty means the stage
// carries no mode (the column is NULL).
func ModesForStage(s Stage) []Mode { return append([]Mode(nil), stageModes[s]...) }

// ModeAllowed reports whether mode m is legal at stage s. A stage that
// carries no mode accepts only the empty mode.
func ModeAllowed(s Stage, m Mode) bool {
	allowed := stageModes[s]
	if len(allowed) == 0 {
		return m == ""
	}
	for _, v := range allowed {
		if v == m {
			return true
		}
	}
	return false
}

// DefaultModeForStage is the mode assigned on promotion when the caller does
// not name one. BACKTEST_ELIGIBLE defaults to PAPER, the safer of its two.
func DefaultModeForStage(s Stage) Mode {
	if s == StageBacktestEligible {
		return ModePaper
	}
	if ms := stageModes[s]; len(ms) == 1 {
		return ms[0]
	}
	return ""
}

// Agent is one agents row: the deployable binding of an account, a compiled
// strategy version, a capital envelope and a mode.
type Agent struct {
	ID                  AgentID
	AccountID           string
	StrategyID          string
	StrategyVersionID   string // empty only while DRAFT
	Name                string
	Stage               Stage
	State               State
	Mode                Mode   // empty for DRAFT, COMPILED, VALIDATED
	EnvelopeID          string // required from CANARY upwards
	RiskPolicyVersion   string
	SupersededByAgentID string
	FailureReason       string
	Version             int64 // bumped on rebind; recorded on runs and predictions
	CreatedByActorType  string
	CreatedByActorID    string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// MaxNameLength mirrors the agents.name CHECK.
const MaxNameLength = 120

// Validate reports the structural reasons a is not a legal agents row. It
// mirrors every CHECK on the table so a violation surfaces as a typed
// VALIDATION_FAILED rather than a SQLSTATE 23514.
func (a Agent) Validate() error {
	fields := map[string]any{}
	fail := func(k, msg string) {
		if _, dup := fields[k]; !dup {
			fields[k] = msg
		}
	}
	if a.ID.IsZero() {
		fail("id", "required")
	}
	if a.AccountID == "" {
		fail("account_id", "required")
	}
	if a.StrategyID == "" {
		fail("strategy_id", "required")
	}
	if l := len(a.Name); l < 1 || l > MaxNameLength {
		fail("name", "must be 1..120 characters")
	}
	if !a.Stage.Valid() {
		fail("stage", "unknown stage")
	}
	if !a.State.Valid() {
		fail("state", "unknown state")
	}
	if a.Stage.Valid() && a.State.Valid() && a.State != a.Stage.State() && !a.State.IsSide() {
		fail("state", "must equal stage unless PAUSED, FAILED, REVOKED or SUPERSEDED")
	}
	if a.Stage != StageDraft && a.StrategyVersionID == "" {
		fail("strategy_version_id", "required from COMPILED onwards")
	}
	if a.Stage.RealCapital() && a.EnvelopeID == "" {
		fail("envelope_id", "required for CANARY, LIMITED and LIVE")
	}
	if a.Stage.Valid() && !ModeAllowed(a.Stage, a.Mode) {
		fail("mode", "not permitted at stage "+a.Stage.String())
	}
	if a.Version < 1 {
		fail("version", "must be >= 1")
	}
	if a.CreatedByActorType == "AGENT" {
		fail("created_by_actor_type", "an agent can never create an agent")
	}
	if len(fields) == 0 {
		return nil
	}
	return errs.New(errs.CodeValidationFailed, "agent: invalid").WithFields(fields)
}

// Paused reports whether the agent's state is PAUSED.
func (a Agent) Paused() bool { return a.State == StatePaused }

// Runnable reports whether the dispatcher may open runs for the agent.
func (a Agent) Runnable() bool { return a.State.Runs() && a.Mode.Valid() }
