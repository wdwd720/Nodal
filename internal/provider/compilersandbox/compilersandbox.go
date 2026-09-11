package compilersandbox

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/agents"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/model"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/strategy"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// Name is the compiler name the API reports.
const Name = "structured_sandbox"

// PreferredPriceTool is the tool this compiler looks for first when it needs a
// spot price. `cmd/api` registers it on a sandbox tier; if a deployment
// registers a different READ_MARKET_DATA tool, the compiler uses that one
// instead, and if the registry holds none at all the document it builds names
// this code and the TYPE stage refuses it with UNKNOWN_TOOL — which says
// exactly what is missing.
const PreferredPriceTool = "sandbox_price_spot"

// PriceFieldPath is the field of the price tool's output the rules read. It is
// the mid price, which is the only price that is not a side's opinion.
const PriceFieldPath = "mid"

// usdScale is the scale every USD decimal in the compiled document carries:
// two, because a USD amount is exact in cents and nothing here is a float.
const usdScale = 2

// Ref names the compiler assigns. They are fixed rather than derived from the
// user's words, except the instrument's, which is a slug of the canonical name
// so the rendered strategy reads like the thing it is about.
const (
	refTrigger    ir.Ref = "tick"
	refPrice      ir.Ref = "price"
	refEntryRule  ir.Ref = "entry_rule"
	refExitRule   ir.Ref = "exit_rule"
	refNoForecast ir.Ref = "no_forecast"
	refEnter      ir.Ref = "enter"
	refLeave      ir.Ref = "leave"
)

// Compiler is the sandbox tier's structured compiler.
//
// It holds a clock and a version string and nothing else: no provider, no
// credential, no database. It cannot reach a model because it has nothing to
// reach one with, which is a stronger statement than a promise not to.
type Compiler struct {
	env     config.Environment
	clk     clock.Clock
	version string
}

// Interface checks. The compiler is wired through the seam internal/agents
// declares, and nothing else.
var _ agents.StructuredCompilerBackend = (*Compiler)(nil)

// New returns the compiler, or refuses when the environment is PROD.
//
// The refusal is the provider's own and does not depend on the wiring being
// right: a production binary that somehow asked for this gets an error, not a
// compiler that produces strategies labelled as rehearsals in a database that
// has no rehearsals in it.
func New(env config.Environment, clk clock.Clock, compilerVersion string) (*Compiler, error) {
	if env == config.EnvProd {
		return nil, errs.New(errs.CodeForbidden,
			"compilersandbox: the sandbox structured compiler cannot exist in PROD; a production strategy is compiled by a contracted model provider or by nobody")
	}
	if clk == nil {
		clk = clock.System()
	}
	v := strings.TrimSpace(compilerVersion)
	if v == "" {
		v = "unspecified"
	}
	return &Compiler{env: env, clk: clk, version: Name + "/" + v}, nil
}

// CompilerName identifies this compiler in the API.
func (c *Compiler) CompilerName() string { return Name }

// SandboxCompiler reports that everything this compiler produces is a
// rehearsal. It is the marker `cmd/api` and the API surface key the label off,
// and it is a method rather than a field so no caller can construct a value
// that answers false.
func (c *Compiler) SandboxCompiler() bool { return true }

// CompileStructured turns a declared strategy into a validated version, or
// records honestly why it did not.
//
// It never reads req's description, because req has no description: the
// service passes strategies.constraints and nothing else. Every element of the
// document below is traceable to one of three places, and the rationale names
// which for each: a field the caller stated, the registry, or the risk policy
// in force.
func (c *Compiler) CompileStructured(ctx context.Context, req agents.StructuredCompileRequest) (strategy.Result, error) {
	_ = ctx // no I/O: this compiler reads only what it was handed.
	if req.StrategyID.IsZero() {
		return strategy.Result{}, errs.New(errs.CodeValidationFailed, "compilersandbox: compile request needs a strategy id")
	}
	if req.Version < 1 {
		return strategy.Result{}, errs.New(errs.CodeValidationFailed, "compilersandbox: compile request needs a version number")
	}
	attemptNo := req.AttemptNo
	if attemptNo < 1 {
		attemptNo = 1
	}

	// The input hash is over the CONSTRAINTS. That is the whole input this
	// compiler read, so the recorded hash is a claim anybody can check: the
	// description cannot have influenced the document, because it was not part
	// of what was hashed.
	inputSum := sha256.Sum256(canonicalInput(req.Constraints))
	now := c.clk.Now().UTC()

	attempt := strategy.Attempt{
		ID:         strategy.NewAttemptID(),
		StrategyID: req.StrategyID,
		RequestID:  req.RequestID,
		AttemptNo:  attemptNo,
		SourceKind: ir.SourceStructuredSandbox,
		InputHash:  inputSum[:],
		CreatedAt:  now,
		Provenance: model.Provenance{
			// No template, no provider, no model id, no tokens, no cost: none
			// of them happened, and a placeholder in any of these columns would
			// read as a model call that did.
			Purpose:     model.PurposeCompile,
			ParseResult: model.ParseNotAttempted,
			RequestedAt: now,
			Usage:       model.Usage{Cost: money.USDFromMinor(0)},
		},
	}

	declared, missing := Decode(req.Constraints)
	if len(missing) == 0 {
		missing = declared.Validate()
	}
	if len(missing) == 0 {
		missing = resolvable(declared, req.Registry, req.Refs)
	}
	if len(missing) > 0 {
		attempt.StageReached = strategy.StagePrompt
		attempt.Outcome = strategy.OutcomeRejected
		attempt.FailureCodes = []string{agents.StructuredConstraintsRequired}
		attempt.Fields = fieldsOf(missing)
		return strategy.Result{
			Attempts:       []strategy.Attempt{attempt},
			Outcome:        strategy.OutcomeRejected,
			Codes:          []string{agents.StructuredConstraintsRequired},
			Clarifications: sentences(missing),
			Rationale: strategy.Rationale{
				Summary: "Nothing was compiled. This compiler assembles a strategy from the fields you state and " +
					"reads no natural language, so an unstated field is a question it cannot answer and does not guess at.",
				Assumptions: []string{"No assumption was made: every field below is missing or unusable, and none was filled in for you."},
			},
		}, nil
	}

	doc, notes, err := c.build(declared, req, attempt.ID, inputSum[:], now)
	if err != nil {
		attempt.StageReached = strategy.StageParse
		attempt.Outcome = strategy.OutcomeRejected
		attempt.FailureCodes = []string{ir.CodeParseFailed}
		attempt.Fields = map[string]string{"constraints": err.Error()}
		return strategy.Result{
			Attempts: []strategy.Attempt{attempt}, Outcome: strategy.OutcomeRejected,
			Codes: []string{ir.CodeParseFailed},
		}, nil
	}

	// The same Validate the model path runs, with the same refs. A structured
	// document gets no additional trust for having been typed into a form.
	report := strategy.Validate(doc, req.Refs)
	attempt.StageReached = report.Stage
	if !report.OK() {
		attempt.Outcome = strategy.OutcomeRejected
		attempt.FailureCodes = report.Codes()
		attempt.Fields = report.Fields()
		if body, merr := json.Marshal(doc); merr == nil {
			attempt.Structured = body
		}
		return strategy.Result{
			Attempts: []strategy.Attempt{attempt}, Outcome: strategy.OutcomeRejected, Codes: report.Codes(),
			Rationale: strategy.Rationale{
				Summary:     "A document was assembled from the fields you stated and the platform's rules refused it. Nothing was changed to make it pass.",
				Assumptions: notes,
			},
		}, nil
	}

	hash, err := ir.SemanticHash(doc)
	if err != nil {
		return strategy.Result{}, errs.Wrap(err, errs.CodeInternal, "compilersandbox: hash the compiled document")
	}
	doc.Hash = hash
	human, err := strategy.Render(doc)
	if err != nil {
		return strategy.Result{}, errs.Wrap(err, errs.CodeInternal, "compilersandbox: render the compiled document")
	}

	policyHash, err := decodeHex(req.Refs.PolicyHash)
	if err != nil {
		return strategy.Result{}, errs.Wrap(err, errs.CodeInternal, "compilersandbox: the risk policy hash is malformed")
	}

	version := &strategy.Version{
		ID:            strategy.NewVersionID(),
		StrategyID:    req.StrategyID,
		Version:       req.Version,
		SchemaVersion: ir.SchemaVersion,
		IR:            doc,
		IRHash:        hash,
		EffectSet:     ir.EffectStrings(doc.Effects),
		// COMPILED, never ACCEPTED. Acceptance is the record of a person
		// reading this document, and this package has no way to make one.
		Status:          strategy.StatusCompiled,
		SourceKind:      ir.SourceStructuredSandbox,
		SourceHash:      inputSum[:],
		CompilerVersion: c.version,
		AttemptID:       &attempt.ID,
		RiskPolicy:      doc.RiskPolicy.Version,
		RiskPolicyHash:  policyHash,
		HumanReadable:   human,
		BuiltAt:         doc.BuiltAt,
	}

	attempt.Outcome = strategy.OutcomeSuccess
	attempt.VersionID = &version.ID
	if body, merr := json.Marshal(doc); merr == nil {
		attempt.Structured = body
	}

	return strategy.Result{
		Version:  version,
		Attempts: []strategy.Attempt{attempt},
		Outcome:  strategy.OutcomeSuccess,
		Rationale: strategy.Rationale{
			Summary: fmt.Sprintf(
				"Every element of this strategy came from a field you stated, from this deployment's registry, or from the "+
					"risk policy in force. No model was called, your description was not read, and nothing was inferred. "+
					"It trades %s on %s, evaluates every %d minutes, and is compiled for %s only.",
				declared.Universe.Instrument, declared.Universe.Venue, declared.Frequency.IntervalMinutes, ModePaper,
			),
			Assumptions: notes,
		},
	}, nil
}

// resolvable reports the declared fields that name something this deployment
// does not have. It is separate from Validate because these are not defects in
// the form: the shape is right and the registry disagrees, and the sentence has
// to say which.
func resolvable(s StructuredStrategy, reg agents.StructuredRegistry, refs strategy.ValidationRefs) []Missing {
	var out []Missing
	instrumentID, ok := reg.InstrumentIDsByCanonicalName[s.Universe.Instrument]
	if !ok {
		return []Missing{{
			Field: "universe.instrument",
			Detail: fmt.Sprintf("%q is not an instrument this deployment lists; name one from the instruments registry",
				s.Universe.Instrument),
		}}
	}
	if inst, found := refs.Instruments[instrumentID]; !found || inst.Status != assets.StatusActive {
		out = append(out, Missing{
			Field:  "universe.instrument",
			Detail: fmt.Sprintf("%q is in the registry but is not ACTIVE, so it cannot back a new strategy", s.Universe.Instrument),
		})
	}
	venue, found := refs.Venues[s.Universe.Venue]
	switch {
	case !found:
		out = append(out, Missing{
			Field:  "universe.venue",
			Detail: fmt.Sprintf("%q is not a venue this deployment lists", s.Universe.Venue),
		})
	case !venue.Status.AllowsNewActions():
		out = append(out, Missing{
			Field:  "universe.venue",
			Detail: fmt.Sprintf("venue %q has status %s and may not take new actions", s.Universe.Venue, venue.Status),
		})
	default:
		listed := false
		for _, code := range reg.VenueCodesByInstrumentID[instrumentID] {
			if code == s.Universe.Venue {
				listed = true
				break
			}
		}
		if !listed {
			out = append(out, Missing{
				Field: "universe.venue",
				Detail: fmt.Sprintf("venue %q does not list %s; a strategy cannot name a venue that does not trade its instrument",
					s.Universe.Venue, s.Universe.Instrument),
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Field < out[j].Field })
	return out
}

// build assembles the document. Every assignment below is either a field the
// caller stated, a registry value, or a risk-policy value, and `notes` records
// which for each so the rationale can say so in words.
func (c *Compiler) build(s StructuredStrategy, req agents.StructuredCompileRequest,
	attemptID strategy.AttemptID, inputHash []byte, now time.Time,
) (*ir.IR, []string, error) {
	instrumentID := req.Registry.InstrumentIDsByCanonicalName[s.Universe.Instrument]
	name := slugRef(s.Universe.Instrument)
	policy := req.Refs.Policy
	intervalMS := int64(s.Frequency.IntervalMinutes) * 60_000

	maxSingleTrade, err := usdOf(s.RiskLimits.MaxSingleTradeUSD)
	if err != nil {
		return nil, nil, err
	}
	maxPosition, err := usdOf(s.RiskLimits.MaxPositionUSD)
	if err != nil {
		return nil, nil, err
	}
	maxDailyLoss, err := usdOf(s.RiskLimits.MaxDailyLossUSD)
	if err != nil {
		return nil, nil, err
	}
	minAllocation, err := usdOf(s.CapitalLimit.MinAllocationUSD)
	if err != nil {
		return nil, nil, err
	}

	policyHash, err := hexBytes(req.Refs.PolicyHash)
	if err != nil {
		return nil, nil, err
	}

	constraints := ir.IntentConstraints{
		MaxSlippageBPS:    bpsOrZero(policy.MaxSlippageBPS),
		MaxFeeBPS:         bpsOrZero(policy.MaxFeeBPS),
		MaxPriceImpactBPS: bpsOrZero(policy.MaxPriceImpactBPS),
		QuoteFreshnessMS:  int64OrZero(policy.MaxQuoteAgeMS),
		AllowedVenues:     []string{s.Universe.Venue},
	}

	doc := &ir.IR{
		SchemaVersion: ir.SchemaVersion,
		StrategyID:    req.StrategyID.String(),
		Version:       req.Version,
		Owner:         ir.Owner{AccountID: req.OwnerAccountID, UserID: req.OwnerUserID},
		Instruments:   []ir.InstrumentDecl{{Name: name, InstrumentID: instrumentID}},
		Triggers: []ir.Trigger{{
			Name: refTrigger, Kind: ir.TriggerOnInterval,
			EveryMS: &intervalMS, DedupWindowMS: intervalMS,
		}},
		Dependencies: []ir.Dependency{{
			Name: refPrice, Kind: ir.DepPrice,
			ToolCode: priceTool(req.Refs.Tools), ToolVersion: priceToolVersion(req.Refs.Tools),
			DependencyVersion: 1,
			Params:            map[string]string{"instrument_id": instrumentID},
			MaxAgeMS:          priceMaxAge(policy),
			Required:          true,
		}},
		Signals:    []ir.Signal{},
		Conditions: []ir.Condition{},
		RiskPolicy: ir.RiskPolicyRef{Version: policy.Version, Hash: policyHash},
		// No model, declared as such: this compiler calls none and the document
		// it produces must not require one of a runtime either.
		ModelBudget: ir.ModelBudget{Required: false, Providers: []string{}, MaxSpendPerDay: money.USDFromMinor(0)},
		DataBudget: ir.DataBudget{
			MaxToolCallsPerRun: 1,
			MaxToolCallsPerDay: runsPerDay(s.Frequency.IntervalMinutes),
			MaxSpendPerDay:     money.USDFromMinor(0),
			MaxLookbackMS:      0,
		},
		Envelope: ir.EnvelopeRequirements{
			MinAllocation: minAllocation, MaxSingleTrade: maxSingleTrade,
			MaxPosition: maxPosition, MaxDailyLoss: maxDailyLoss,
			Instruments: []string{instrumentID}, AssetClasses: []string{}, Venues: []string{s.Universe.Venue},
			MaxIntentsPerHour: s.Frequency.MaxIntentsPerHour,
			MaxRunsPerMinute:  1,
		},
		Lineage: ir.Lineage{
			Source:           ir.SourceStructuredSandbox,
			SourceHash:       inputHash,
			CompileAttemptID: attemptID.String(),
			CompilerVersion:  c.version,
		},
		BuiltAt: now,
	}

	// The prediction that claims nothing.
	//
	// The IR requires a committed prediction before a trade intent (PART 72,
	// STRUCTURAL_INTENT_WITHOUT_PREDICTION) because on this platform a forecast
	// predates the execution it justifies. You did not state a forecast and this
	// compiler will not invent one, so the numbers below are the ones that
	// cannot flatter the strategy: no direction, no probability of it, no
	// expected gain, a loss not ruled out, and a maximum downside of the whole
	// position. A zero downside would have been the comfortable choice and it
	// would have been a claim.
	doc.Actions = []ir.Action{{
		Name: refNoForecast, Kind: ir.ActionCommitPrediction,
		Prediction: &ir.PredictionSpec{
			Instrument: name, HorizonMS: intervalMS, Direction: ir.DirectionFlat,
			Probability:         constExpr("0", ir.ProbabilityScale),
			ExpectedReturnBPS:   constExpr("0", 0),
			DownsideProbability: constExpr(pow10String(ir.ProbabilityScale), ir.ProbabilityScale),
			MaxDownsideBPS:      constExpr(fmt.Sprintf("%d", money.OneHundredPercent), 0),
			Confidence:          constExpr("0", ir.ProbabilityScale),
		},
	}}

	entryWhen, entryCond := rule(s.Entry, refEntryRule)
	if entryCond != nil {
		doc.Conditions = append(doc.Conditions, *entryCond)
	}
	doc.Actions = append(doc.Actions, ir.Action{
		Name: refEnter, Kind: ir.ActionCreateTradeIntent, When: entryWhen,
		Intent: &ir.IntentSpec{
			Action: intent.ActionAcquireNotional, Instrument: name,
			Sizing:      ir.Sizing{Kind: ir.SizingFixedNotional, NotionalUSD: &maxSingleTrade},
			Constraints: constraints,
			DeadlineMS:  intervalMS,
			Prediction:  refNoForecast,
		},
	})

	exitWhen, exitCond := rule(s.Exit, refExitRule)
	if exitCond != nil {
		doc.Conditions = append(doc.Conditions, *exitCond)
	}
	doc.Actions = append(doc.Actions, ir.Action{
		Name: refLeave, Kind: ir.ActionCreateTradeIntent, When: exitWhen,
		Intent: &ir.IntentSpec{
			Action: intent.ActionClosePosition, Instrument: name,
			Sizing:      ir.Sizing{Kind: ir.SizingNone},
			Constraints: constraints,
			DeadlineMS:  intervalMS,
			Prediction:  refNoForecast,
		},
	})

	// Derived by the ir package's own rules from the dependencies and actions
	// above, never declared: a declared set that differed from the derived one
	// is exactly what EFFECT_MISMATCH exists to catch.
	doc.Effects = ir.DeriveEffects(doc)
	doc.Normalize()

	return doc, c.notes(s, instrumentID, policy), nil
}

// notes is the per-element provenance the rationale carries. One line per
// element of the document, saying where it came from.
func (c *Compiler) notes(s StructuredStrategy, instrumentID string, policy risk.Policy) []string {
	out := []string{
		fmt.Sprintf("Instrument: you stated universe.instrument = %s; the registry resolved it to %s.", s.Universe.Instrument, instrumentID),
		fmt.Sprintf("Venue: you stated universe.venue = %s; the registry confirms it lists that instrument.", s.Universe.Venue),
		fmt.Sprintf("Trigger: you stated frequency.interval_minutes = %d, so it evaluates every %d minutes and no faster.", s.Frequency.IntervalMinutes, s.Frequency.IntervalMinutes),
		ruleNote("Entry", s.Entry),
		ruleNote("Exit", s.Exit),
		fmt.Sprintf("Trade size: you stated risk_limits.max_single_trade_usd = %s minor units, and that is the size of every trade it proposes.", s.RiskLimits.MaxSingleTradeUSD),
		fmt.Sprintf("Limits: you stated max_position_usd = %s and max_daily_loss_usd = %s minor units; capital_limit.min_allocation_usd = %s is the smallest envelope it will run in.",
			s.RiskLimits.MaxPositionUSD, s.RiskLimits.MaxDailyLossUSD, s.CapitalLimit.MinAllocationUSD),
		fmt.Sprintf("Rate: you stated frequency.max_intents_per_hour = %d.", s.Frequency.MaxIntentsPerHour),
		fmt.Sprintf("Execution constraints (slippage, fee, price impact, quote freshness) are risk policy %s's own limits. You did not state them and this compiler does not invent one.", policy.Version),
		"Prediction: you stated no forecast, so the committed prediction claims none — no direction, probability zero, expected return zero, a loss not ruled out, and a maximum downside of the whole position.",
		"Model budget: zero, with no providers. This compiler calls no model and the document it produced does not require a runtime to call one.",
		fmt.Sprintf("Mode: you stated %s, which is the only mode this build compiles.", s.Mode),
		"Your description was recorded and is shown back to you unchanged. It was not read by this compiler and no part of the document above came from it.",
	}
	return out
}

func ruleNote(label string, r Rule) string {
	if r.Kind == RuleEveryInterval {
		return fmt.Sprintf("%s: you stated EVERY_INTERVAL, so the rule carries no condition and acts on every evaluation.", label)
	}
	return fmt.Sprintf("%s: you stated %s %s %s minor units, which became a comparison of the instrument's mid price against that number.",
		label, r.Kind, r.Comparator, r.PriceUSD)
}

// rule turns a declared rule into the condition that guards its action, or
// into no condition at all for EVERY_INTERVAL — an action with no `when` fires
// on every run, which is precisely what "rebalance every N minutes" means.
func rule(r Rule, name ir.Ref) (*ir.Ref, *ir.Condition) {
	if r.Kind != RulePriceThreshold {
		return nil, nil
	}
	minor, err := ParseMinorUSD(r.PriceUSD)
	if err != nil {
		return nil, nil // unreachable: Validate refused this input already.
	}
	cond := ir.Condition{
		Name: name,
		Expr: ir.Expr{Cmp: &ir.Cmp{
			Op: cmpOp(r.Comparator),
			L:  &ir.Expr{Field: &ir.FieldRef{Dependency: refPrice, Path: PriceFieldPath, Scale: usdScale}},
			R:  &ir.Expr{Const: decimalPtr(ir.NewDecimal(big.NewInt(minor), usdScale))},
		}},
	}
	ref := name
	return &ref, &cond
}

func cmpOp(c Comparator) ir.CmpOp {
	switch c {
	case CmpLT:
		return ir.CmpLT
	case CmpLTE:
		return ir.CmpLE
	case CmpGT:
		return ir.CmpGT
	default:
		return ir.CmpGE
	}
}

// priceTool picks the READ_MARKET_DATA tool the price dependency reads through.
//
// It prefers the sandbox tier's own, then takes the lexicographically first
// ACTIVE one so the choice is deterministic and a corpus run is reproducible.
// When the registry holds none it still names PreferredPriceTool, so the TYPE
// stage refuses the document with UNKNOWN_TOOL and the refusal says which tool
// this deployment is missing — which is more useful than a compiler that says
// it could not start.
func priceTool(tools map[string]strategy.Tool) string {
	code, _ := choosePriceTool(tools)
	return code
}

func priceToolVersion(tools map[string]strategy.Tool) int {
	_, version := choosePriceTool(tools)
	return version
}

func choosePriceTool(tools map[string]strategy.Tool) (string, int) {
	candidates := make([]strategy.Tool, 0, len(tools))
	for _, t := range tools {
		if t.Effect == ir.EffectReadMarketData && t.Active() {
			candidates = append(candidates, t)
		}
	}
	if len(candidates) == 0 {
		return PreferredPriceTool, 1
	}
	// Deterministic: the sandbox tier's own tool first, then by code, then the
	// highest version of that code. A compiler whose choice depended on map
	// iteration order would produce a different ir_hash for the same input.
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if (a.Code == PreferredPriceTool) != (b.Code == PreferredPriceTool) {
			return a.Code == PreferredPriceTool
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		return a.Version > b.Version
	})
	return candidates[0].Code, candidates[0].Version
}

// priceMaxAge is the staleness bound on the price read. It is the risk policy's
// own cap for price data when the policy sets one, which is the strictest value
// that can validate; a policy that sets none leaves it at half a second, the
// bound STRATEGY_IR.md names for a spot price.
func priceMaxAge(policy risk.Policy) int64 {
	if v, ok := policy.MaxDataAgeMS["price"]; ok && v > 0 {
		return v
	}
	return 500
}

// runsPerDay is the tool-call-per-day budget: one read per evaluation, and the
// evaluations come from the interval the caller stated.
func runsPerDay(intervalMinutes int) int {
	if intervalMinutes <= 0 {
		return 1
	}
	n := (24 * 60) / intervalMinutes
	if n < 1 {
		return 1
	}
	return n
}

func bpsOrZero(v *money.BPS) money.BPS {
	if v == nil {
		return 0
	}
	return *v
}

func int64OrZero(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func usdOf(minorString string) (money.USD, error) {
	minor, err := ParseMinorUSD(minorString)
	if err != nil {
		return money.USD{}, err
	}
	return money.USDFromMinor(minor), nil
}

func constExpr(mantissa string, scale uint8) ir.Expr {
	return ir.Expr{Const: decimalPtr(ir.Decimal{Mantissa: mantissa, Scale: scale})}
}

func decimalPtr(d ir.Decimal) *ir.Decimal { return &d }

func pow10String(scale uint8) string {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil).String()
}

// slugRef turns a canonical instrument name into a valid IR ref, so the
// rendered strategy names the instrument the way a person wrote it rather than
// calling it "instrument_1".
func slugRef(canonical string) ir.Ref {
	var b strings.Builder
	for _, r := range strings.ToLower(canonical) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := strings.Trim(b.String(), "_")
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		s = "i_" + s
	}
	if len(s) > ir.MaxRefLength {
		s = s[:ir.MaxRefLength]
	}
	return ir.Ref(strings.Trim(s, "_"))
}

// canonicalInput is what the input hash is taken over: the constraints with
// insignificant JSON whitespace removed, so the same declared strategy hashes
// identically however the client formatted it.
func canonicalInput(raw json.RawMessage) []byte {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}

func fieldsOf(missing []Missing) map[string]string {
	out := make(map[string]string, len(missing))
	for _, m := range missing {
		if _, ok := out[m.Field]; !ok {
			out[m.Field] = m.Detail
		}
	}
	return out
}

func sentences(missing []Missing) []string {
	out := make([]string, 0, len(missing))
	for _, m := range missing {
		out = append(out, m.String())
	}
	return out
}

func hexBytes(s string) (ir.Hex, error) {
	b, err := decodeHex(s)
	if err != nil {
		return nil, err
	}
	return ir.Hex(b), nil
}

func decodeHex(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("compilersandbox: %q is not hex", s)
	}
	out := make([]byte, len(s)/2)
	for i := range out {
		hi, err := nibble(s[2*i])
		if err != nil {
			return nil, err
		}
		lo, err := nibble(s[2*i+1])
		if err != nil {
			return nil, err
		}
		out[i] = hi<<4 | lo
	}
	return out, nil
}

func nibble(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, nil
	}
	return 0, fmt.Errorf("compilersandbox: %q is not a hex digit", string(rune(c)))
}
