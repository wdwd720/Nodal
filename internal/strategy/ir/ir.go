package ir

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/money"
)

// SchemaVersion is the only IR schema this build understands.
const SchemaVersion = 1

// Structural bounds (STRATEGY_IR.md §2 and §7). They are the loop-safety
// contract: a document inside these bounds evaluates in bounded time.
const (
	MaxTriggers        = 8
	MaxSignals         = 64
	MaxConditions      = 64
	MaxActions         = 16
	MinActions         = 1
	MaxDependencies    = 32
	MinDependencies    = 1
	MaxInstruments     = 16
	MaxExprDepth       = 8
	MaxExprNodes       = 256
	MinIntervalMS      = 1000
	MaxIntervalMS      = 7 * 24 * 3600 * 1000
	MaxDedupWindowMS   = 7 * 24 * 3600 * 1000
	MaxDocumentBytes   = 256 * 1024
	MaxJSONDepth       = 48
	MaxParams          = 32
	MaxParamLength     = 256
	MaxPathLength      = 128
	MaxProviders       = 8
	MaxVenues          = 32
	MaxModelInputs     = 32
	MaxEventTypeLength = 64
	MaxTextLength      = 256
	MaxRefLength       = 64
	MaxRunsPerMinute   = 60
	MaxModelCalls      = 8
)

// Ref names a trigger, dependency, instrument, signal, condition or action.
// Grammar ^[a-z][a-z0-9_]{0,63}$; unique within its namespace.
type Ref string

// Valid reports whether r matches the Ref grammar.
func (r Ref) Valid() bool {
	if r == "" || len(r) > MaxRefLength {
		return false
	}
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9', c == '_':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// String returns the wire form.
func (r Ref) String() string { return string(r) }

// Hex is a byte slice rendered as lowercase hex in JSON so the document is
// identical in Go and TypeScript.
type Hex []byte

// MarshalJSON renders lowercase hex.
func (h Hex) MarshalJSON() ([]byte, error) { return json.Marshal(hex.EncodeToString(h)) }

// UnmarshalJSON accepts a lowercase or uppercase hex string.
func (h *Hex) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*h = Hex{}
		return nil
	}
	v, err := hex.DecodeString(s)
	if err != nil {
		return fmt.Errorf("ir: hex field: %w", err)
	}
	*h = v
	return nil
}

// String renders lowercase hex.
func (h Hex) String() string { return hex.EncodeToString(h) }

// IR is the compiled strategy document (goal PART 62). Field order follows
// STRATEGY_IR.md §2; JSON keys are snake_case and shared with the SDK.
type IR struct {
	SchemaVersion int                  `json:"schema_version"`
	StrategyID    string               `json:"strategy_id"`
	Version       int                  `json:"version"`
	Hash          Hex                  `json:"hash"`
	Owner         Owner                `json:"owner"`
	Instruments   []InstrumentDecl     `json:"instruments"`
	Triggers      []Trigger            `json:"triggers"`
	Dependencies  []Dependency         `json:"dependencies"`
	Signals       []Signal             `json:"signals"`
	Conditions    []Condition          `json:"conditions"`
	Actions       []Action             `json:"actions"`
	RiskPolicy    RiskPolicyRef        `json:"risk_policy"`
	ModelBudget   ModelBudget          `json:"model_budget"`
	DataBudget    DataBudget           `json:"data_budget"`
	Envelope      EnvelopeRequirements `json:"envelope"`
	Effects       []Effect             `json:"effects"`
	Lineage       Lineage              `json:"lineage"`
	BuiltAt       time.Time            `json:"built_at"`
}

// Owner identifies the account and user that own the strategy.
type Owner struct {
	AccountID string `json:"account_id"`
	UserID    string `json:"user_id"`
}

// InstrumentDecl binds a Ref to an instruments.InstrumentID so actions refer
// to instruments by name and the TYPE stage checks the registry once.
type InstrumentDecl struct {
	Name         Ref    `json:"name"`
	InstrumentID string `json:"instrument_id"`
}

// TriggerKind is ON_EVENT or ON_INTERVAL.
type TriggerKind string

// Trigger kinds.
const (
	TriggerOnEvent    TriggerKind = "ON_EVENT"
	TriggerOnInterval TriggerKind = "ON_INTERVAL"
)

// Trigger opens a run. ON_EVENT fires on a normalized event whose type
// matches and whose typed fields satisfy Filter; ON_INTERVAL fires every
// EveryMS milliseconds, epoch-aligned so replay is deterministic.
type Trigger struct {
	Name          Ref         `json:"name"`
	Kind          TriggerKind `json:"kind"`
	EventType     string      `json:"event_type,omitempty"`
	Filter        *Expr       `json:"filter,omitempty"`
	EveryMS       *int64      `json:"every_ms,omitempty"`
	DedupWindowMS int64       `json:"dedup_window_ms"`
}

// DependencyKind is the data class a dependency reads.
type DependencyKind string

// Dependency kinds (each maps to exactly one allowed effect).
const (
	DepPrice              DependencyKind = "PRICE"
	DepOnchain            DependencyKind = "ONCHAIN"
	DepWalletEvent        DependencyKind = "WALLET_EVENT"
	DepSocial             DependencyKind = "SOCIAL"
	DepWalletIntelligence DependencyKind = "WALLET_INTELLIGENCE"
	DepModel              DependencyKind = "MODEL"
	DepFeature            DependencyKind = "FEATURE"
)

// Dependency is a declared read through the tools registry (PARTS 174, 176).
type Dependency struct {
	Name              Ref               `json:"name"`
	Kind              DependencyKind    `json:"kind"`
	ToolCode          string            `json:"tool_code"`
	ToolVersion       int               `json:"tool_version"`
	DependencyVersion int               `json:"dependency_version"`
	Params            map[string]string `json:"params"`
	MaxAgeMS          int64             `json:"max_age_ms"`
	Required          bool              `json:"required"`
}

// Signal is a named Decimal computed once per run in listed order.
type Signal struct {
	Name     Ref    `json:"name"`
	Expr     Expr   `json:"expr"`
	Scale    uint8  `json:"scale"`
	Rounding string `json:"rounding"`
}

// Expr is a tagged union: exactly one member is set. Depth ≤ MaxExprDepth,
// ≤ MaxExprNodes per document. There is no recursion in the grammar beyond
// nesting: a Signal reference may only point to an earlier signal.
type Expr struct {
	Const  *Decimal  `json:"const,omitempty"`
	Field  *FieldRef `json:"field,omitempty"`
	Signal *Ref      `json:"signal,omitempty"`
	Bin    *BinOp    `json:"bin,omitempty"`
	Window *WindowOp `json:"window,omitempty"`
	Cmp    *Cmp      `json:"cmp,omitempty"`
	And    []*Expr   `json:"and,omitempty"`
	Or     []*Expr   `json:"or,omitempty"`
	Not    *Expr     `json:"not,omitempty"`
}

// FieldRef reads a typed decimal field of a dependency's output (or, inside
// a trigger filter, of the triggering event) at the declared scale.
type FieldRef struct {
	Dependency Ref    `json:"dependency"`
	Path       string `json:"path"`
	Scale      uint8  `json:"scale"`
}

// BinOpKind is an arithmetic operator.
type BinOpKind string

// Arithmetic operators.
const (
	OpAdd BinOpKind = "ADD"
	OpSub BinOpKind = "SUB"
	OpMul BinOpKind = "MUL"
	OpDiv BinOpKind = "DIV"
	OpMin BinOpKind = "MIN"
	OpMax BinOpKind = "MAX"
)

// BinOp combines two Decimal expressions at an explicit scale and rounding.
type BinOp struct {
	Op       BinOpKind `json:"op"`
	L        *Expr     `json:"l"`
	R        *Expr     `json:"r"`
	Scale    uint8     `json:"scale"`
	Rounding string    `json:"rounding"`
}

// WindowFn is a window aggregate.
type WindowFn string

// Window aggregates.
const (
	WinSMA    WindowFn = "SMA"
	WinEMA    WindowFn = "EMA"
	WinMax    WindowFn = "MAX"
	WinMin    WindowFn = "MIN"
	WinSum    WindowFn = "SUM"
	WinCount  WindowFn = "COUNT"
	WinReturn WindowFn = "RETURN"
	WinStddev WindowFn = "STDDEV"
)

// WindowOp aggregates a dependency's field over a lookback window.
type WindowOp struct {
	Fn         WindowFn `json:"fn"`
	Dependency Ref      `json:"dependency"`
	Path       string   `json:"path"`
	LookbackMS int64    `json:"lookback_ms"`
	Scale      uint8    `json:"scale"`
	Rounding   string   `json:"rounding"`
}

// CmpOp is a comparison operator.
type CmpOp string

// Comparison operators.
const (
	CmpLT CmpOp = "LT"
	CmpLE CmpOp = "LE"
	CmpGT CmpOp = "GT"
	CmpGE CmpOp = "GE"
	CmpEQ CmpOp = "EQ"
	CmpNE CmpOp = "NE"
)

// Cmp compares two Decimal expressions of equal scale and yields a bool.
type Cmp struct {
	Op CmpOp `json:"op"`
	L  *Expr `json:"l"`
	R  *Expr `json:"r"`
}

// Condition is a named boolean expression.
type Condition struct {
	Name Ref  `json:"name"`
	Expr Expr `json:"expr"`
}

// ActionKind is what an action does.
type ActionKind string

// Action kinds.
const (
	ActionCallModel         ActionKind = "CALL_MODEL"
	ActionCommitPrediction  ActionKind = "COMMIT_PREDICTION"
	ActionCreateTradeIntent ActionKind = "CREATE_TRADE_INTENT"
)

// Action is evaluated once per run in listed order when its condition holds.
type Action struct {
	Name       Ref             `json:"name"`
	Kind       ActionKind      `json:"kind"`
	When       *Ref            `json:"when,omitempty"`
	Model      *ModelCall      `json:"model,omitempty"`
	Prediction *PredictionSpec `json:"prediction,omitempty"`
	Intent     *IntentSpec     `json:"intent,omitempty"`
}

// ModelCall requests one schema-constrained inference through the broker.
// OutputSchema names a MODEL dependency whose tool output schema constrains
// the response; Inputs are signal names passed as TOOL RESULTS data.
type ModelCall struct {
	TemplateVersion string `json:"template_version"`
	OutputSchema    Ref    `json:"output_schema"`
	Inputs          []Ref  `json:"inputs"`
	MaxOutputTokens int64  `json:"max_output_tokens"`
	Required        bool   `json:"required"`
}

// Direction is a predicted price direction.
type Direction string

// Directions.
const (
	DirectionUp   Direction = "UP"
	DirectionDown Direction = "DOWN"
	DirectionFlat Direction = "FLAT"
)

// PredictionSpec computes a prediction ledger row. Probability fields are
// scale-4 Decimals in [0, 1]; bps fields are scale-0 Decimals.
type PredictionSpec struct {
	Instrument          Ref       `json:"instrument"`
	HorizonMS           int64     `json:"horizon_ms"`
	Direction           Direction `json:"direction"`
	Probability         Expr      `json:"probability"`
	ExpectedReturnBPS   Expr      `json:"expected_return_bps"`
	DownsideProbability Expr      `json:"downside_probability"`
	MaxDownsideBPS      Expr      `json:"max_downside_bps"`
	Confidence          Expr      `json:"confidence"`
}

// IntentSpec compiles to intent.TradeIntent fields. Prediction names the
// COMMIT_PREDICTION action that must precede it (prediction predates
// execution, PART 72).
type IntentSpec struct {
	Action      intent.Action     `json:"action"`
	Instrument  Ref               `json:"instrument"`
	Sizing      Sizing            `json:"sizing"`
	Constraints IntentConstraints `json:"constraints"`
	DeadlineMS  int64             `json:"deadline_ms"`
	Prediction  Ref               `json:"prediction"`
}

// SizingKind is how an intent is sized.
type SizingKind string

// Sizing kinds. NONE is only valid for CLOSE_POSITION.
const (
	SizingNone                SizingKind = "NONE"
	SizingFixedNotional       SizingKind = "FIXED_NOTIONAL"
	SizingEnvelopeFractionBPS SizingKind = "ENVELOPE_FRACTION_BPS"
	SizingTargetExposure      SizingKind = "TARGET_EXPOSURE"
)

// Sizing carries exactly the field its Kind needs.
type Sizing struct {
	Kind        SizingKind `json:"kind"`
	NotionalUSD *money.USD `json:"notional_usd,omitempty"`
	FractionBPS *money.BPS `json:"fraction_bps,omitempty"`
	TargetUSD   *money.USD `json:"target_usd,omitempty"`
}

// IntentConstraints are the hard execution limits copied onto the intent.
type IntentConstraints struct {
	MaxSlippageBPS    money.BPS `json:"max_slippage_bps"`
	MaxFeeBPS         money.BPS `json:"max_fee_bps"`
	MaxPriceImpactBPS money.BPS `json:"max_price_impact_bps"`
	QuoteFreshnessMS  int64     `json:"quote_freshness_ms"`
	AllowedVenues     []string  `json:"allowed_venues"`
}

// RiskPolicyRef pins the risk_policies row the IR was validated against.
type RiskPolicyRef struct {
	Version string `json:"version"`
	Hash    Hex    `json:"hash"`
}

// ModelBudget caps model use (AGENT_RUNTIME.md §6).
type ModelBudget struct {
	Required        bool      `json:"required"`
	Providers       []string  `json:"providers"`
	MaxCallsPerRun  int       `json:"max_calls_per_run"`
	MaxCallsPerDay  int       `json:"max_calls_per_day"`
	MaxInputTokens  int64     `json:"max_input_tokens"`
	MaxOutputTokens int64     `json:"max_output_tokens"`
	MaxSpendPerDay  money.USD `json:"max_spend_per_day"`
}

// DataBudget caps tool use and window lookback.
type DataBudget struct {
	MaxToolCallsPerRun int       `json:"max_tool_calls_per_run"`
	MaxToolCallsPerDay int       `json:"max_tool_calls_per_day"`
	MaxSpendPerDay     money.USD `json:"max_spend_per_day"`
	MaxLookbackMS      int64     `json:"max_lookback_ms"`
}

// EnvelopeRequirements is what the strategy needs from its capital envelope
// and the per-strategy rate limits (PART 198). MaxDailyLoss is the strategy's
// own stop-for-the-day; the envelope and policy limits still bind.
type EnvelopeRequirements struct {
	MinAllocation     money.USD `json:"min_allocation"`
	MaxSingleTrade    money.USD `json:"max_single_trade"`
	MaxPosition       money.USD `json:"max_position"`
	MaxDailyLoss      money.USD `json:"max_daily_loss"`
	Instruments       []string  `json:"instruments"`
	AssetClasses      []string  `json:"asset_classes"`
	Venues            []string  `json:"venues"`
	MaxIntentsPerHour int       `json:"max_intents_per_hour"`
	MaxRunsPerMinute  int       `json:"max_runs_per_minute"`
}

// LineageSource records which authoring path produced a version.
type LineageSource string

// Lineage sources (strategies.source_kind).
const (
	SourceNaturalLanguage LineageSource = "NATURAL_LANGUAGE"
	SourceTypeScriptSDK   LineageSource = "TYPESCRIPT_SDK"
	SourceClone           LineageSource = "CLONE"
	// SourceStructuredSandbox is a document assembled field by field from a
	// structured strategy the user declared, by a compiler that reads no
	// natural language and calls no model. It exists only on a sandbox tier:
	// migration 00812 pairs it with `sandbox` and refuses the pair in PROD.
	SourceStructuredSandbox LineageSource = "STRUCTURED_SANDBOX"
)

var allLineageSources = []LineageSource{
	SourceNaturalLanguage, SourceTypeScriptSDK, SourceClone, SourceStructuredSandbox,
}

// AllLineageSources returns every declared authoring path (a copy). It is the
// Go half of the source_kind CHECK on strategies, strategy_versions and
// compile_attempts; test/integration/enums holds the two together.
func AllLineageSources() []LineageSource {
	return append([]LineageSource(nil), allLineageSources...)
}

// Valid reports whether s is a declared authoring path.
func (s LineageSource) Valid() bool {
	for _, v := range allLineageSources {
		if v == s {
			return true
		}
	}
	return false
}

// String returns the wire form.
func (s LineageSource) String() string { return string(s) }

// Lineage is provenance; it never influences evaluation and is excluded
// from the semantic hash.
type Lineage struct {
	Source           LineageSource `json:"source"`
	SourceHash       Hex           `json:"source_hash"`
	CompileAttemptID string        `json:"compile_attempt_id"`
	ParentVersionID  string        `json:"parent_version_id"`
	CompilerVersion  string        `json:"compiler_version"`
	SDKVersion       string        `json:"sdk_version"`
}

// ErrNilIR is returned by functions handed a nil document.
var ErrNilIR = errors.New("ir: nil document")

// Normalize makes the document canonical without changing its meaning:
// nil lists become empty lists, nil maps become empty maps, list-valued
// sets (effects, envelope allowlists, providers, venues) are sorted and
// deduplicated, and BuiltAt is forced to UTC. Parsing and hashing both
// normalize, so a Go-built and a JSON-decoded document hash identically.
func (ir *IR) Normalize() {
	if ir == nil {
		return
	}
	if ir.Instruments == nil {
		ir.Instruments = []InstrumentDecl{}
	}
	if ir.Triggers == nil {
		ir.Triggers = []Trigger{}
	}
	if ir.Dependencies == nil {
		ir.Dependencies = []Dependency{}
	}
	for i := range ir.Dependencies {
		if ir.Dependencies[i].Params == nil {
			ir.Dependencies[i].Params = map[string]string{}
		}
	}
	if ir.Signals == nil {
		ir.Signals = []Signal{}
	}
	if ir.Conditions == nil {
		ir.Conditions = []Condition{}
	}
	if ir.Actions == nil {
		ir.Actions = []Action{}
	}
	for i := range ir.Actions {
		a := &ir.Actions[i]
		if a.Model != nil && a.Model.Inputs == nil {
			a.Model.Inputs = []Ref{}
		}
		if a.Intent != nil && a.Intent.Constraints.AllowedVenues == nil {
			a.Intent.Constraints.AllowedVenues = []string{}
		}
		if a.Intent != nil {
			a.Intent.Constraints.AllowedVenues = sortedUnique(a.Intent.Constraints.AllowedVenues)
		}
	}
	ir.ModelBudget.Providers = sortedUnique(ir.ModelBudget.Providers)
	ir.Envelope.Instruments = sortedUnique(ir.Envelope.Instruments)
	ir.Envelope.AssetClasses = sortedUnique(ir.Envelope.AssetClasses)
	ir.Envelope.Venues = sortedUnique(ir.Envelope.Venues)
	ir.Effects = SortEffects(ir.Effects)
	if ir.Hash == nil {
		ir.Hash = Hex{}
	}
	if ir.RiskPolicy.Hash == nil {
		ir.RiskPolicy.Hash = Hex{}
	}
	if ir.Lineage.SourceHash == nil {
		ir.Lineage.SourceHash = Hex{}
	}
	ir.BuiltAt = ir.BuiltAt.UTC()
}

// Clone returns a deep copy.
func (ir *IR) Clone() (*IR, error) {
	if ir == nil {
		return nil, ErrNilIR
	}
	b, err := json.Marshal(ir)
	if err != nil {
		return nil, fmt.Errorf("ir: clone: %w", err)
	}
	var out IR
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("ir: clone: %w", err)
	}
	out.Normalize()
	return &out, nil
}

// Dependency returns the named dependency.
func (ir *IR) Dependency(name Ref) (Dependency, bool) {
	for _, d := range ir.Dependencies {
		if d.Name == name {
			return d, true
		}
	}
	return Dependency{}, false
}

// Instrument returns the named instrument declaration.
func (ir *IR) Instrument(name Ref) (InstrumentDecl, bool) {
	for _, d := range ir.Instruments {
		if d.Name == name {
			return d, true
		}
	}
	return InstrumentDecl{}, false
}

// Trigger returns the named trigger.
func (ir *IR) Trigger(name Ref) (Trigger, bool) {
	for _, t := range ir.Triggers {
		if t.Name == name {
			return t, true
		}
	}
	return Trigger{}, false
}

// Action returns the named action.
func (ir *IR) Action(name Ref) (Action, bool) {
	for _, a := range ir.Actions {
		if a.Name == name {
			return a, true
		}
	}
	return Action{}, false
}

func sortedUnique(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
