package prediction

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"sort"
	"strconv"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

type (
	predictionKind struct{}
	outcomeKind    struct{}
	snapshotKind   struct{}
)

// Typed identifiers.
type (
	// PredictionID identifies one predictions row.
	PredictionID = id.ID[predictionKind]
	// OutcomeID identifies one prediction_outcomes row.
	OutcomeID = id.ID[outcomeKind]
	// SnapshotID identifies one calibration_snapshots row.
	SnapshotID = id.ID[snapshotKind]
)

// NewPredictionID returns a fresh prediction id.
func NewPredictionID() PredictionID { return id.New[predictionKind]() }

// NewOutcomeID returns a fresh prediction-outcome id.
func NewOutcomeID() OutcomeID { return id.New[outcomeKind]() }

// NewSnapshotID returns a fresh calibration-snapshot id.
func NewSnapshotID() SnapshotID { return id.New[snapshotKind]() }

// ParsePredictionID parses the canonical form.
func ParsePredictionID(s string) (PredictionID, error) { return id.Parse[predictionKind](s) }

// Mode mirrors the predictions.mode CHECK. It is declared here rather than
// imported from internal/agent so the ledger does not depend on the runtime;
// the runtime converts.
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

// Direction is a predicted or realized price direction.
type Direction string

// The three directions.
const (
	DirectionUp   Direction = "UP"
	DirectionDown Direction = "DOWN"
	DirectionFlat Direction = "FLAT"
)

// Valid reports whether d is declared.
func (d Direction) Valid() bool {
	return d == DirectionUp || d == DirectionDown || d == DirectionFlat
}

// String renders the direction.
func (d Direction) String() string { return string(d) }

// ProbabilityScale is the scale of every probability column: numeric(7,6).
const ProbabilityScale uint8 = 6

// ScoreScale is the scale of every score column: numeric(12,8).
const ScoreScale uint8 = 8

// Prediction is one predictions row: a structured forecast committed before
// the trade it justifies, and immutable afterwards (PART 72).
type Prediction struct {
	ID                PredictionID
	AgentID           string
	AgentVersion      int64
	RunID             string
	ActionName        string
	StrategyVersionID string
	AccountID         string
	Mode              Mode
	InstrumentID      instruments.InstrumentID
	Horizon           time.Duration

	// Direction and ProbabilityDirection are both present or both absent: a
	// probability without a direction says nothing, and the table CHECK
	// enforces the pairing.
	Direction            Direction
	ProbabilityDirection ir.Decimal
	ExpectedReturnBPS    money.BPS
	DownsideProbability  ir.Decimal
	MaxDownsideBPS       money.BPS
	Confidence           ir.Decimal

	// InformationSetHash is sha256 over the sorted (dependency,
	// tool_invocation_id, output_hash, decision_available_at) tuples of the
	// snapshot the decision was made from.
	InformationSetHash []byte
	// DecisionAvailableAt is the latest decision_available_at in that set: the
	// earliest instant this decision could lawfully have been made.
	DecisionAvailableAt time.Time
	CommittedAt         time.Time

	ModelCallID     string
	TemplateVersion string
	Rationale       json.RawMessage
	EvidenceRefs    json.RawMessage
	Signals         json.RawMessage
	CorrelationID   string
}

// HorizonEnd is when the prediction becomes resolvable.
func (p Prediction) HorizonEnd() time.Time { return p.CommittedAt.Add(p.Horizon) }

// Validate reports every structural reason p cannot be committed. It mirrors
// each predictions CHECK so a violation is a typed VALIDATION_FAILED rather
// than a SQLSTATE 23514, and it never consults a clock or the database.
func (p Prediction) Validate() error {
	fields := map[string]any{}
	fail := func(k, msg string) {
		if _, dup := fields[k]; !dup {
			fields[k] = msg
		}
	}
	if p.ID.IsZero() {
		fail("id", "required")
	}
	if p.AgentID == "" {
		fail("agent_id", "required")
	}
	if p.AgentVersion < 1 {
		fail("agent_version", "must be >= 1")
	}
	if p.RunID == "" {
		fail("agent_run_id", "required")
	}
	if p.ActionName == "" {
		fail("action_name", "required")
	}
	if p.StrategyVersionID == "" {
		fail("strategy_version_id", "required")
	}
	if p.AccountID == "" {
		fail("account_id", "required")
	}
	if !p.Mode.Valid() {
		fail("mode", "must be one of the six modes")
	}
	if p.InstrumentID.IsZero() {
		fail("instrument_id", "required")
	}
	if p.Horizon <= 0 {
		fail("horizon_ms", "must be > 0")
	}
	hasDirection := p.Direction != ""
	hasProbability := !isUnset(p.ProbabilityDirection)
	switch {
	case hasDirection && !p.Direction.Valid():
		fail("direction", "must be UP, DOWN or FLAT")
	case hasDirection != hasProbability:
		fail("direction", "direction and probability_direction are present together or not at all")
	}
	if hasProbability {
		checkProbability(fail, "probability_direction", p.ProbabilityDirection)
	}
	checkProbability(fail, "downside_probability", p.DownsideProbability)
	checkProbability(fail, "confidence", p.Confidence)
	if p.MaxDownsideBPS < 0 {
		fail("max_downside_bps", "must be >= 0")
	}
	if len(p.InformationSetHash) != sha256.Size {
		fail("information_set_hash", "must be a 32-byte sha256")
	}
	if p.DecisionAvailableAt.IsZero() {
		fail("decision_available_at", "required")
	}
	if p.CommittedAt.IsZero() {
		fail("committed_at", "required")
	}
	if !p.DecisionAvailableAt.IsZero() && !p.CommittedAt.IsZero() && p.DecisionAvailableAt.After(p.CommittedAt) {
		fail("decision_available_at", "must not be after committed_at: a decision cannot use data it did not have")
	}
	if len(fields) == 0 {
		return nil
	}
	return errs.New(errs.CodeValidationFailed, "prediction: invalid").WithFields(fields)
}

// checkProbability requires a decimal in [0, 1] that fits numeric(7,6). It
// does not use ir.Decimal.IsProbability, which pins the IR's own scale of 4:
// the ledger column is scale 6, so anything from scale 0 to 6 is accepted and
// NormalizeProbability widens it losslessly at commit time.
func checkProbability(fail func(k, msg string), field string, d ir.Decimal) {
	if isUnset(d) {
		fail(field, "required")
		return
	}
	if err := d.Validate(); err != nil {
		fail(field, "not a valid decimal")
		return
	}
	if d.Scale > ProbabilityScale {
		fail(field, "scale must be at most 6")
		return
	}
	if !inUnitInterval(d) {
		fail(field, "must be between 0 and 1 inclusive")
	}
}

// inUnitInterval reports whether 0 <= d <= 1 at any scale.
func inUnitInterval(d ir.Decimal) bool {
	v, err := d.Int()
	if err != nil {
		return false
	}
	if v.Sign() < 0 {
		return false
	}
	one := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(d.Scale)), nil)
	return v.Cmp(one) <= 0
}

// NormalizeProbability widens d to the ledger's scale of 6. Widening a
// fixed-point decimal is exact, so this never rounds and never loses a digit;
// a decimal already at scale 6 is returned unchanged.
func NormalizeProbability(d ir.Decimal) (ir.Decimal, error) {
	if isUnset(d) {
		return ir.Decimal{}, errs.New(errs.CodeValidationFailed, "prediction: probability is required")
	}
	if d.Scale == ProbabilityScale {
		if err := d.Validate(); err != nil {
			return ir.Decimal{}, errs.Wrap(err, errs.CodeValidationFailed, "prediction: invalid probability")
		}
		return d, nil
	}
	if d.Scale > ProbabilityScale {
		return ir.Decimal{}, errs.Newf(errs.CodeValidationFailed,
			"prediction: probability scale %d exceeds the ledger scale %d", d.Scale, ProbabilityScale)
	}
	out, err := d.Rescale(ProbabilityScale, money.RoundExact)
	if err != nil {
		return ir.Decimal{}, errs.Wrap(err, errs.CodeValidationFailed, "prediction: widen probability")
	}
	return out, nil
}

// isUnset reports whether a Decimal was never assigned.
func isUnset(d ir.Decimal) bool { return d.Mantissa == "" }

// InformationItem is one entry of the information set: what was read, from
// which invocation, with which output and from what instant it could be used.
type InformationItem struct {
	Dependency          string
	InvocationID        string
	OutputHash          []byte
	DecisionAvailableAt time.Time
}

// InformationSetHash is the sha256 over the sorted, length-prefixed tuples of
// the set. It pins exactly what the decision knew: change any output, any
// invocation or any availability time and the hash changes, so a prediction
// cannot later be claimed to have rested on different evidence.
func InformationSetHash(items []InformationItem) []byte {
	sorted := append([]InformationItem(nil), items...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Dependency != sorted[j].Dependency {
			return sorted[i].Dependency < sorted[j].Dependency
		}
		if sorted[i].InvocationID != sorted[j].InvocationID {
			return sorted[i].InvocationID < sorted[j].InvocationID
		}
		return hex.EncodeToString(sorted[i].OutputHash) < hex.EncodeToString(sorted[j].OutputHash)
	})
	h := sha256.New()
	write := func(s string) {
		h.Write([]byte(strconv.Itoa(len(s))))
		h.Write([]byte{':'})
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	for _, it := range sorted {
		write(it.Dependency)
		write(it.InvocationID)
		write(hex.EncodeToString(it.OutputHash))
		write(it.DecisionAvailableAt.UTC().Format(time.RFC3339Nano))
	}
	return h.Sum(nil)
}

// LatestAvailable is the maximum decision_available_at of a set: the instant
// from which the whole set could be used. It is what a prediction records as
// its own DecisionAvailableAt.
func LatestAvailable(items []InformationItem) time.Time {
	var out time.Time
	for _, it := range items {
		if it.DecisionAvailableAt.After(out) {
			out = it.DecisionAvailableAt
		}
	}
	return out
}

// UsableAt reports whether every item of the set was available at t. A
// backtest passes simulated time; a live run passes the run's decision time.
// A set that fails this must never be scored or acted on.
func UsableAt(items []InformationItem, t time.Time) bool {
	for _, it := range items {
		if it.DecisionAvailableAt.After(t) {
			return false
		}
	}
	return true
}
