package ir

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

// Validation codes. They are stable strings persisted in
// compile_attempts.failure_codes; STRUCTURAL_* and EFFECT_* are produced
// here, TYPE_* and RISK_* by internal/strategy.
const (
	CodeParseFailed = "PARSE_FAILED"

	CodeStructuralSchemaVersion       = "STRUCTURAL_SCHEMA_VERSION"
	CodeStructuralInvalidID           = "STRUCTURAL_INVALID_ID"
	CodeStructuralNoTrigger           = "STRUCTURAL_NO_TRIGGER"
	CodeStructuralNoAction            = "STRUCTURAL_NO_ACTION"
	CodeStructuralNoDependency        = "STRUCTURAL_NO_DEPENDENCY"
	CodeStructuralTooMany             = "STRUCTURAL_TOO_MANY"
	CodeStructuralInvalidRef          = "STRUCTURAL_INVALID_REF"
	CodeStructuralDuplicateRef        = "STRUCTURAL_DUPLICATE_REF"
	CodeStructuralUnresolvedRef       = "STRUCTURAL_UNRESOLVED_REF"
	CodeStructuralSignalCycle         = "STRUCTURAL_SIGNAL_CYCLE"
	CodeStructuralExprMalformed       = "STRUCTURAL_EXPR_MALFORMED"
	CodeStructuralExprDepth           = "STRUCTURAL_EXPR_DEPTH"
	CodeStructuralExprNodes           = "STRUCTURAL_EXPR_NODES"
	CodeStructuralInvalidOperator     = "STRUCTURAL_INVALID_OPERATOR"
	CodeStructuralInvalidTrigger      = "STRUCTURAL_INVALID_TRIGGER"
	CodeStructuralIntervalTooShort    = "STRUCTURAL_INTERVAL_TOO_SHORT"
	CodeStructuralDedupWindow         = "STRUCTURAL_DEDUP_WINDOW"
	CodeStructuralInvalidDependency   = "STRUCTURAL_INVALID_DEPENDENCY"
	CodeStructuralMaxAge              = "STRUCTURAL_MAX_AGE"
	CodeStructuralLookbackBudget      = "STRUCTURAL_LOOKBACK_EXCEEDS_BUDGET"
	CodeStructuralInvalidInstrument   = "STRUCTURAL_INVALID_INSTRUMENT"
	CodeStructuralInvalidAction       = "STRUCTURAL_INVALID_ACTION"
	CodeStructuralIntentNoPrediction  = "STRUCTURAL_INTENT_WITHOUT_PREDICTION"
	CodeStructuralRateLimit           = "STRUCTURAL_RATE_LIMIT"
	CodeStructuralModelBudget         = "STRUCTURAL_MODEL_BUDGET"
	CodeStructuralDataBudget          = "STRUCTURAL_DATA_BUDGET"
	CodeStructuralText                = "STRUCTURAL_TEXT"
	CodeStructuralInvalidPath         = "STRUCTURAL_INVALID_PATH"
	CodeStructuralParams              = "STRUCTURAL_PARAMS"
	CodeStructuralEnvelopeInstruments = "STRUCTURAL_ENVELOPE_INSTRUMENTS"

	CodeEffectForbidden = "EFFECT_FORBIDDEN"
	CodeEffectMismatch  = "EFFECT_MISMATCH"
)

// Issue is one validation finding. Field is a JSON-pointer-like path.
type Issue struct {
	Code   string
	Field  string
	Detail string
}

// Codes returns the sorted, unique codes of a finding list.
func Codes(issues []Issue) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(issues))
	for _, i := range issues {
		if _, ok := seen[i.Code]; ok {
			continue
		}
		seen[i.Code] = struct{}{}
		out = append(out, i.Code)
	}
	sort.Strings(out)
	return out
}

// Fields returns a map of field → detail (first finding per field wins;
// fields are sorted when rendered by the caller).
func Fields(issues []Issue) map[string]string {
	out := map[string]string{}
	for _, i := range issues {
		if _, ok := out[i.Field]; !ok {
			out[i.Field] = i.Code + ": " + i.Detail
		}
	}
	return out
}

// ValidationError carries the stage and findings of a failed check as an
// *errs.Error-compatible value.
type ValidationError struct {
	Stage  string
	Issues []Issue
}

// Error renders the stage and codes.
func (e *ValidationError) Error() string {
	return fmt.Sprintf("ir: %s validation failed: %s", e.Stage, strings.Join(Codes(e.Issues), ","))
}

// Codes returns the sorted codes.
func (e *ValidationError) Codes() []string { return Codes(e.Issues) }

// ToErr converts the finding into the API error contract: EFFECT_FORBIDDEN
// when the stage is EFFECT with a forbidden name, STRATEGY_REJECTED
// otherwise.
func (e *ValidationError) ToErr() *errs.Error {
	code := errs.CodeStrategyRejected
	for _, c := range Codes(e.Issues) {
		if c == CodeEffectForbidden {
			code = errs.CodeEffectForbidden
		}
	}
	fields := map[string]any{"stage": e.Stage, "codes": Codes(e.Issues)}
	for k, v := range Fields(e.Issues) {
		fields[k] = v
	}
	return errs.New(code, e.Error()).WithFields(fields)
}

// Decode strictly parses raw into an IR: size limit, JSON nesting limit,
// unknown fields rejected, trailing data rejected, then Normalize. It never
// panics on any input (FuzzParseIR). No structural checks are applied here;
// callers that need a trustworthy document use ParseIR.
func Decode(raw []byte) (*IR, error) {
	if len(raw) > MaxDocumentBytes {
		return nil, &ValidationError{Stage: "PARSE", Issues: []Issue{{Code: CodeParseFailed, Field: "", Detail: fmt.Sprintf("document exceeds %d bytes", MaxDocumentBytes)}}}
	}
	if !utf8.Valid(raw) {
		return nil, parseIssue("document is not valid UTF-8")
	}
	if err := checkDepth(raw); err != nil {
		return nil, parseIssue(err.Error())
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var doc IR
	if err := dec.Decode(&doc); err != nil {
		return nil, parseIssue(err.Error())
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, parseIssue("trailing data after document")
	}
	doc.Normalize()
	return &doc, nil
}

func parseIssue(detail string) error {
	return &ValidationError{Stage: "PARSE", Issues: []Issue{{Code: CodeParseFailed, Detail: detail}}}
}

// checkDepth rejects nesting bombs before the reflective decoder sees them.
func checkDepth(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	depth := 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
				if depth > MaxJSONDepth {
					return fmt.Errorf("json nesting exceeds %d", MaxJSONDepth)
				}
			case '}', ']':
				depth--
			}
		}
	}
}

// ParseIR is Decode followed by the checks a document must pass before any
// component trusts it: forbidden effects (EFFECT stage, terminal), the
// STRUCTURAL stage, and declared-versus-derived effect equality. The
// returned error is a *ValidationError.
func ParseIR(raw []byte) (*IR, error) {
	doc, err := Decode(raw)
	if err != nil {
		return nil, err
	}
	if err := Check(doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// Check runs the document-local stages (forbidden effects, STRUCTURAL,
// effect mismatch) on an already decoded IR.
func Check(doc *IR) error {
	if doc == nil {
		return ErrNilIR
	}
	if issues := ForbiddenEffectIssues(doc); len(issues) > 0 {
		return &ValidationError{Stage: "EFFECT", Issues: issues}
	}
	if issues := Structural(doc); len(issues) > 0 {
		return &ValidationError{Stage: "STRUCTURAL", Issues: issues}
	}
	if issues := EffectIssues(doc); len(issues) > 0 {
		return &ValidationError{Stage: "EFFECT", Issues: issues}
	}
	return nil
}

// ForbiddenEffectIssues reports every declared effect outside the allowed
// table. Unknown names are reported as forbidden (fail closed).
func ForbiddenEffectIssues(doc *IR) []Issue {
	var out []Issue
	for i, e := range doc.Effects {
		if !e.Allowed() {
			out = append(out, Issue{Code: CodeEffectForbidden, Field: fmt.Sprintf("effects[%d]", i), Detail: string(e)})
		}
	}
	return out
}

// EffectIssues reports forbidden names and a declared set that differs
// from the derived set.
func EffectIssues(doc *IR) []Issue {
	out := ForbiddenEffectIssues(doc)
	derived := DeriveEffects(doc)
	if !EffectsEqual(doc.Effects, derived) {
		out = append(out, Issue{Code: CodeEffectMismatch, Field: "effects", Detail: fmt.Sprintf("declared %v, derived %v", EffectStrings(doc.Effects), EffectStrings(derived))})
	}
	return out
}

// Structural runs the STRUCTURAL stage (STRATEGY_IR.md §4, §7): counts, ref
// grammar and uniqueness, every reference resolves, signals reference only
// earlier signals (no cycles), expression depth and node bounds, interval
// and dedup bounds, lookback within the data budget, matching action
// specs, intents preceded by predictions, rate and budget consistency.
// Pure: no clock, no I/O, deterministic order of findings.
func Structural(doc *IR) []Issue {
	if doc == nil {
		return []Issue{{Code: CodeStructuralSchemaVersion, Detail: "nil document"}}
	}
	s := &structCheck{doc: doc, deps: map[Ref]Dependency{}, signals: map[Ref]int{}, conds: map[Ref]struct{}{}, instruments: map[Ref]struct{}{}, actions: map[Ref]ActionKind{}}
	s.header()
	s.instrumentsCheck()
	s.dependencies()
	s.triggers()
	s.signalsCheck()
	s.conditions()
	s.actionsCheck()
	s.budgets()
	if s.nodes > MaxExprNodes {
		s.add(CodeStructuralExprNodes, "expr", fmt.Sprintf("%d expression nodes exceed %d", s.nodes, MaxExprNodes))
	}
	return s.issues
}

type structCheck struct {
	doc         *IR
	issues      []Issue
	deps        map[Ref]Dependency
	signals     map[Ref]int
	conds       map[Ref]struct{}
	instruments map[Ref]struct{}
	actions     map[Ref]ActionKind
	nodes       int
}

func (s *structCheck) add(code, field, detail string) {
	s.issues = append(s.issues, Issue{Code: code, Field: field, Detail: detail})
}

func (s *structCheck) header() {
	d := s.doc
	if d.SchemaVersion != SchemaVersion {
		s.add(CodeStructuralSchemaVersion, "schema_version", fmt.Sprintf("must be %d", SchemaVersion))
	}
	for _, f := range []struct{ name, v string }{{"strategy_id", d.StrategyID}, {"owner.account_id", d.Owner.AccountID}, {"owner.user_id", d.Owner.UserID}} {
		if _, err := id.ParseAny(f.v); err != nil {
			s.add(CodeStructuralInvalidID, f.name, "must be a canonical uuid")
		}
	}
	if len(d.Triggers) == 0 {
		s.add(CodeStructuralNoTrigger, "triggers", "at least one trigger")
	}
	if len(d.Actions) < MinActions {
		s.add(CodeStructuralNoAction, "actions", "at least one action")
	}
	if len(d.Dependencies) < MinDependencies {
		s.add(CodeStructuralNoDependency, "dependencies", "at least one dependency")
	}
	for _, c := range []struct {
		name string
		n    int
		lim  int
	}{
		{"triggers", len(d.Triggers), MaxTriggers},
		{"signals", len(d.Signals), MaxSignals},
		{"conditions", len(d.Conditions), MaxConditions},
		{"actions", len(d.Actions), MaxActions},
		{"dependencies", len(d.Dependencies), MaxDependencies},
		{"instruments", len(d.Instruments), MaxInstruments},
		{"model_budget.providers", len(d.ModelBudget.Providers), MaxProviders},
		{"envelope.venues", len(d.Envelope.Venues), MaxVenues},
		{"envelope.instruments", len(d.Envelope.Instruments), MaxInstruments},
		{"envelope.asset_classes", len(d.Envelope.AssetClasses), MaxVenues},
	} {
		if c.n > c.lim {
			s.add(CodeStructuralTooMany, c.name, fmt.Sprintf("%d exceeds %d", c.n, c.lim))
		}
	}
	for i, v := range d.Envelope.Venues {
		s.text(fmt.Sprintf("envelope.venues[%d]", i), v, MaxRefLength)
	}
	for i, v := range d.Envelope.AssetClasses {
		s.text(fmt.Sprintf("envelope.asset_classes[%d]", i), v, MaxRefLength)
	}
	for i, v := range d.ModelBudget.Providers {
		s.text(fmt.Sprintf("model_budget.providers[%d]", i), v, MaxRefLength)
	}
	s.text("risk_policy.version", d.RiskPolicy.Version, MaxTextLength)
}

func (s *structCheck) text(field, v string, maxLen int) {
	switch {
	case strings.TrimSpace(v) == "":
		s.add(CodeStructuralText, field, "required")
	case len(v) > maxLen:
		s.add(CodeStructuralText, field, fmt.Sprintf("longer than %d bytes", maxLen))
	case !isClean(v):
		s.add(CodeStructuralText, field, "must be valid utf-8 without control characters")
	}
}

func (s *structCheck) ref(field string, r Ref, seen map[Ref]struct{}) bool {
	if !r.Valid() {
		s.add(CodeStructuralInvalidRef, field, fmt.Sprintf("%q does not match ^[a-z][a-z0-9_]{0,63}$", r))
		return false
	}
	if _, dup := seen[r]; dup {
		s.add(CodeStructuralDuplicateRef, field, fmt.Sprintf("%q already declared", r))
		return false
	}
	seen[r] = struct{}{}
	return true
}

func (s *structCheck) instrumentsCheck() {
	envelope := map[string]struct{}{}
	for _, v := range s.doc.Envelope.Instruments {
		envelope[v] = struct{}{}
	}
	for i, decl := range s.doc.Instruments {
		field := fmt.Sprintf("instruments[%d]", i)
		s.ref(field+".name", decl.Name, s.instruments)
		if _, err := id.ParseAny(decl.InstrumentID); err != nil {
			s.add(CodeStructuralInvalidInstrument, field+".instrument_id", "must be a canonical uuid")
			continue
		}
		if _, ok := envelope[decl.InstrumentID]; !ok {
			s.add(CodeStructuralEnvelopeInstruments, field+".instrument_id", "must be listed in envelope.instruments")
		}
	}
}

func (s *structCheck) dependencies() {
	seen := map[Ref]struct{}{}
	for i, d := range s.doc.Dependencies {
		field := fmt.Sprintf("dependencies[%d]", i)
		if s.ref(field+".name", d.Name, seen) {
			s.deps[d.Name] = d
		}
		if EffectOfDependency(d.Kind) == "" {
			s.add(CodeStructuralInvalidDependency, field+".kind", fmt.Sprintf("unknown kind %q", d.Kind))
		}
		s.text(field+".tool_code", d.ToolCode, MaxRefLength)
		if d.ToolVersion < 1 {
			s.add(CodeStructuralInvalidDependency, field+".tool_version", "must be >= 1")
		}
		if d.DependencyVersion < 1 {
			s.add(CodeStructuralInvalidDependency, field+".dependency_version", "must be >= 1")
		}
		if d.MaxAgeMS <= 0 {
			s.add(CodeStructuralMaxAge, field+".max_age_ms", "must be > 0 (PART 174)")
		}
		if len(d.Params) > MaxParams {
			s.add(CodeStructuralParams, field+".params", fmt.Sprintf("more than %d params", MaxParams))
		}
		keys := make([]string, 0, len(d.Params))
		for k := range d.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !Ref(k).Valid() {
				s.add(CodeStructuralParams, field+".params."+k, "key must match the ref grammar")
			}
			if v := d.Params[k]; len(v) > MaxParamLength || !isClean(v) {
				s.add(CodeStructuralParams, field+".params."+k, "value must be clean and at most 256 bytes")
			}
		}
	}
}

func (s *structCheck) triggers() {
	seen := map[Ref]struct{}{}
	for i, t := range s.doc.Triggers {
		field := fmt.Sprintf("triggers[%d]", i)
		s.ref(field+".name", t.Name, seen)
		switch t.Kind {
		case TriggerOnEvent:
			s.text(field+".event_type", t.EventType, MaxEventTypeLength)
			if t.EveryMS != nil {
				s.add(CodeStructuralInvalidTrigger, field+".every_ms", "not allowed for ON_EVENT")
			}
			if t.Filter != nil {
				s.expr(field+".filter", t.Filter, 0, exprScope{trigger: t.Name})
			}
		case TriggerOnInterval:
			switch {
			case t.EveryMS == nil:
				s.add(CodeStructuralInvalidTrigger, field+".every_ms", "required for ON_INTERVAL")
			case *t.EveryMS < MinIntervalMS:
				s.add(CodeStructuralIntervalTooShort, field+".every_ms", fmt.Sprintf("must be >= %d ms (PART 198)", MinIntervalMS))
			case *t.EveryMS > MaxIntervalMS:
				s.add(CodeStructuralInvalidTrigger, field+".every_ms", "exceeds seven days")
			}
			if t.EventType != "" || t.Filter != nil {
				s.add(CodeStructuralInvalidTrigger, field, "event_type and filter are only for ON_EVENT")
			}
		default:
			s.add(CodeStructuralInvalidTrigger, field+".kind", fmt.Sprintf("unknown kind %q", t.Kind))
		}
		if t.DedupWindowMS < 0 || t.DedupWindowMS > MaxDedupWindowMS {
			s.add(CodeStructuralDedupWindow, field+".dedup_window_ms", "must be within 0..7 days")
		}
	}
}

func (s *structCheck) signalsCheck() {
	seen := map[Ref]struct{}{}
	for i, sig := range s.doc.Signals {
		field := fmt.Sprintf("signals[%d]", i)
		ok := s.ref(field+".name", sig.Name, seen)
		// Only signals declared before this one are visible to its expression.
		s.expr(field+".expr", &sig.Expr, 0, exprScope{})
		if ok {
			s.signals[sig.Name] = i
		}
		if sig.Scale > MaxScale {
			s.add(CodeStructuralInvalidOperator, field+".scale", "exceeds max scale")
		}
	}
}

func (s *structCheck) conditions() {
	seen := map[Ref]struct{}{}
	for i, c := range s.doc.Conditions {
		field := fmt.Sprintf("conditions[%d]", i)
		if s.ref(field+".name", c.Name, seen) {
			s.conds[c.Name] = struct{}{}
		}
		s.expr(field+".expr", &c.Expr, 0, exprScope{})
	}
}

func (s *structCheck) actionsCheck() {
	seen := map[Ref]struct{}{}
	modelCalls, intents := 0, 0
	for i, a := range s.doc.Actions {
		field := fmt.Sprintf("actions[%d]", i)
		named := s.ref(field+".name", a.Name, seen)
		if a.When != nil {
			if _, ok := s.conds[*a.When]; !ok {
				s.add(CodeStructuralUnresolvedRef, field+".when", fmt.Sprintf("condition %q not declared", *a.When))
			}
		}
		specs := 0
		for _, present := range []bool{a.Model != nil, a.Prediction != nil, a.Intent != nil} {
			if present {
				specs++
			}
		}
		if specs != 1 {
			s.add(CodeStructuralInvalidAction, field, "exactly one of model, prediction, intent must be set")
		}
		switch a.Kind {
		case ActionCallModel:
			modelCalls++
			if a.Model == nil {
				s.add(CodeStructuralInvalidAction, field+".model", "required for CALL_MODEL")
				break
			}
			s.modelCall(field+".model", a.Model)
		case ActionCommitPrediction:
			if a.Prediction == nil {
				s.add(CodeStructuralInvalidAction, field+".prediction", "required for COMMIT_PREDICTION")
				break
			}
			s.prediction(field+".prediction", a.Prediction)
		case ActionCreateTradeIntent:
			intents++
			if a.Intent == nil {
				s.add(CodeStructuralInvalidAction, field+".intent", "required for CREATE_TRADE_INTENT")
				break
			}
			s.intent(field+".intent", a.Intent)
		default:
			s.add(CodeStructuralInvalidAction, field+".kind", fmt.Sprintf("unknown kind %q", a.Kind))
		}
		if named {
			s.actions[a.Name] = a.Kind
		}
	}
	if modelCalls > MaxModelCalls {
		s.add(CodeStructuralTooMany, "actions", fmt.Sprintf("%d CALL_MODEL actions exceed %d", modelCalls, MaxModelCalls))
	}
	if modelCalls > 0 && s.doc.ModelBudget.MaxCallsPerRun < modelCalls {
		s.add(CodeStructuralModelBudget, "model_budget.max_calls_per_run", "smaller than the number of CALL_MODEL actions")
	}
	if intents > 0 && s.doc.Envelope.MaxIntentsPerHour < 1 {
		s.add(CodeStructuralRateLimit, "envelope.max_intents_per_hour", "must be >= 1 when the strategy creates intents")
	}
}

func (s *structCheck) modelCall(field string, m *ModelCall) {
	s.text(field+".template_version", m.TemplateVersion, MaxTextLength)
	dep, ok := s.deps[m.OutputSchema]
	switch {
	case !ok:
		s.add(CodeStructuralUnresolvedRef, field+".output_schema", fmt.Sprintf("dependency %q not declared", m.OutputSchema))
	case dep.Kind != DepModel:
		s.add(CodeStructuralInvalidAction, field+".output_schema", "must reference a MODEL dependency")
	}
	if len(m.Inputs) > MaxModelInputs {
		s.add(CodeStructuralTooMany, field+".inputs", fmt.Sprintf("more than %d inputs", MaxModelInputs))
	}
	for i, in := range m.Inputs {
		if _, ok := s.signals[in]; !ok {
			s.add(CodeStructuralUnresolvedRef, fmt.Sprintf("%s.inputs[%d]", field, i), fmt.Sprintf("signal %q not declared", in))
		}
	}
	if m.MaxOutputTokens <= 0 {
		s.add(CodeStructuralInvalidAction, field+".max_output_tokens", "must be > 0")
	}
	if s.doc.ModelBudget.MaxOutputTokens > 0 && m.MaxOutputTokens > s.doc.ModelBudget.MaxOutputTokens {
		s.add(CodeStructuralModelBudget, field+".max_output_tokens", "exceeds model_budget.max_output_tokens")
	}
}

func (s *structCheck) prediction(field string, p *PredictionSpec) {
	if _, ok := s.instruments[p.Instrument]; !ok {
		s.add(CodeStructuralUnresolvedRef, field+".instrument", fmt.Sprintf("instrument %q not declared", p.Instrument))
	}
	if p.HorizonMS <= 0 {
		s.add(CodeStructuralInvalidAction, field+".horizon_ms", "must be > 0")
	}
	switch p.Direction {
	case DirectionUp, DirectionDown, DirectionFlat:
	default:
		s.add(CodeStructuralInvalidAction, field+".direction", fmt.Sprintf("unknown direction %q", p.Direction))
	}
	for _, e := range []struct {
		name string
		expr *Expr
	}{{"probability", &p.Probability}, {"expected_return_bps", &p.ExpectedReturnBPS}, {"downside_probability", &p.DownsideProbability}, {"max_downside_bps", &p.MaxDownsideBPS}, {"confidence", &p.Confidence}} {
		s.expr(field+"."+e.name, e.expr, 0, exprScope{})
	}
}

func (s *structCheck) intent(field string, in *IntentSpec) {
	if _, ok := s.instruments[in.Instrument]; !ok {
		s.add(CodeStructuralUnresolvedRef, field+".instrument", fmt.Sprintf("instrument %q not declared", in.Instrument))
	}
	if !in.Action.Declared() {
		s.add(CodeStructuralInvalidAction, field+".action", fmt.Sprintf("unknown intent action %q", in.Action))
	}
	kind, ok := s.actions[in.Prediction]
	if !ok || kind != ActionCommitPrediction {
		s.add(CodeStructuralIntentNoPrediction, field+".prediction", "must name an earlier COMMIT_PREDICTION action")
	}
	if in.DeadlineMS <= 0 {
		s.add(CodeStructuralInvalidAction, field+".deadline_ms", "must be > 0")
	}
	switch in.Sizing.Kind {
	case SizingNone, SizingFixedNotional, SizingEnvelopeFractionBPS, SizingTargetExposure:
	default:
		s.add(CodeStructuralInvalidAction, field+".sizing.kind", fmt.Sprintf("unknown sizing kind %q", in.Sizing.Kind))
	}
	if len(in.Constraints.AllowedVenues) > MaxVenues {
		s.add(CodeStructuralTooMany, field+".constraints.allowed_venues", fmt.Sprintf("more than %d venues", MaxVenues))
	}
	for i, v := range in.Constraints.AllowedVenues {
		s.text(fmt.Sprintf("%s.constraints.allowed_venues[%d]", field, i), v, MaxRefLength)
	}
}

func (s *structCheck) budgets() {
	d := s.doc
	if d.Envelope.MaxRunsPerMinute < 1 || d.Envelope.MaxRunsPerMinute > MaxRunsPerMinute {
		s.add(CodeStructuralRateLimit, "envelope.max_runs_per_minute", fmt.Sprintf("must be within 1..%d", MaxRunsPerMinute))
	}
	if d.Envelope.MaxIntentsPerHour < 0 {
		s.add(CodeStructuralRateLimit, "envelope.max_intents_per_hour", "must be >= 0")
	}
	if d.DataBudget.MaxLookbackMS < 0 {
		s.add(CodeStructuralDataBudget, "data_budget.max_lookback_ms", "must be >= 0")
	}
	reads := 0
	for _, dep := range d.Dependencies {
		if dep.Kind != DepModel {
			reads++
		}
	}
	if d.DataBudget.MaxToolCallsPerRun < reads {
		s.add(CodeStructuralDataBudget, "data_budget.max_tool_calls_per_run", "smaller than the number of read dependencies")
	}
	if d.DataBudget.MaxToolCallsPerDay < d.DataBudget.MaxToolCallsPerRun {
		s.add(CodeStructuralDataBudget, "data_budget.max_tool_calls_per_day", "smaller than max_tool_calls_per_run")
	}
	if d.ModelBudget.MaxCallsPerDay < d.ModelBudget.MaxCallsPerRun {
		s.add(CodeStructuralModelBudget, "model_budget.max_calls_per_day", "smaller than max_calls_per_run")
	}
	for _, n := range []struct {
		name string
		v    int64
	}{{"model_budget.max_input_tokens", d.ModelBudget.MaxInputTokens}, {"model_budget.max_output_tokens", d.ModelBudget.MaxOutputTokens}} {
		if n.v < 0 {
			s.add(CodeStructuralModelBudget, n.name, "must be >= 0")
		}
	}
	for _, n := range []struct {
		name string
		v    int
	}{{"model_budget.max_calls_per_run", d.ModelBudget.MaxCallsPerRun}, {"model_budget.max_calls_per_day", d.ModelBudget.MaxCallsPerDay}, {"data_budget.max_tool_calls_per_run", d.DataBudget.MaxToolCallsPerRun}, {"data_budget.max_tool_calls_per_day", d.DataBudget.MaxToolCallsPerDay}} {
		if n.v < 0 {
			s.add(CodeStructuralDataBudget, n.name, "must be >= 0")
		}
	}
}

// exprScope carries what an expression may reference: inside a trigger
// filter, fields of the trigger's event (Dependency == trigger name).
type exprScope struct {
	trigger Ref
}

// expr walks one expression: exactly one member, depth and node bounds,
// operator enums, and resolvable references (signals only backwards).
func (s *structCheck) expr(field string, e *Expr, depth int, scope exprScope) {
	if e == nil {
		s.add(CodeStructuralExprMalformed, field, "missing expression")
		return
	}
	s.nodes++
	if depth >= MaxExprDepth {
		s.add(CodeStructuralExprDepth, field, fmt.Sprintf("nesting exceeds %d", MaxExprDepth))
		return
	}
	members := 0
	for _, present := range []bool{e.Const != nil, e.Field != nil, e.Signal != nil, e.Bin != nil, e.Window != nil, e.Cmp != nil, e.And != nil, e.Or != nil, e.Not != nil} {
		if present {
			members++
		}
	}
	if members != 1 {
		s.add(CodeStructuralExprMalformed, field, "exactly one of const, field, signal, bin, window, cmp, and, or, not")
		return
	}
	switch {
	case e.Field != nil:
		s.fieldRef(field+".field", e.Field, scope)
	case e.Signal != nil:
		idx, ok := s.signals[*e.Signal]
		switch {
		case !ok && s.declaredLater(*e.Signal):
			s.add(CodeStructuralSignalCycle, field+".signal", fmt.Sprintf("signal %q references itself or a later signal", *e.Signal))
		case !ok:
			s.add(CodeStructuralUnresolvedRef, field+".signal", fmt.Sprintf("signal %q not declared", *e.Signal))
		default:
			_ = idx
		}
	case e.Bin != nil:
		switch e.Bin.Op {
		case OpAdd, OpSub, OpMul, OpDiv, OpMin, OpMax:
		default:
			s.add(CodeStructuralInvalidOperator, field+".bin.op", fmt.Sprintf("unknown operator %q", e.Bin.Op))
		}
		if e.Bin.Scale > MaxScale {
			s.add(CodeStructuralInvalidOperator, field+".bin.scale", "exceeds max scale")
		}
		s.expr(field+".bin.l", e.Bin.L, depth+1, scope)
		s.expr(field+".bin.r", e.Bin.R, depth+1, scope)
	case e.Window != nil:
		s.window(field+".window", e.Window, scope)
	case e.Cmp != nil:
		switch e.Cmp.Op {
		case CmpLT, CmpLE, CmpGT, CmpGE, CmpEQ, CmpNE:
		default:
			s.add(CodeStructuralInvalidOperator, field+".cmp.op", fmt.Sprintf("unknown operator %q", e.Cmp.Op))
		}
		s.expr(field+".cmp.l", e.Cmp.L, depth+1, scope)
		s.expr(field+".cmp.r", e.Cmp.R, depth+1, scope)
	case e.And != nil:
		if len(e.And) == 0 {
			s.add(CodeStructuralExprMalformed, field+".and", "needs at least one operand")
		}
		for i, c := range e.And {
			s.expr(fmt.Sprintf("%s.and[%d]", field, i), c, depth+1, scope)
		}
	case e.Or != nil:
		if len(e.Or) == 0 {
			s.add(CodeStructuralExprMalformed, field+".or", "needs at least one operand")
		}
		for i, c := range e.Or {
			s.expr(fmt.Sprintf("%s.or[%d]", field, i), c, depth+1, scope)
		}
	case e.Not != nil:
		s.expr(field+".not", e.Not, depth+1, scope)
	}
}

func (s *structCheck) declaredLater(name Ref) bool {
	for _, sig := range s.doc.Signals {
		if sig.Name == name {
			return true
		}
	}
	return false
}

func (s *structCheck) fieldRef(field string, f *FieldRef, scope exprScope) {
	if scope.trigger != "" && f.Dependency == scope.trigger {
		s.path(field+".path", f.Path)
		return
	}
	dep, ok := s.deps[f.Dependency]
	if !ok {
		s.add(CodeStructuralUnresolvedRef, field+".dependency", fmt.Sprintf("dependency %q not declared", f.Dependency))
		return
	}
	_ = dep
	s.path(field+".path", f.Path)
	if f.Scale > MaxScale {
		s.add(CodeStructuralInvalidOperator, field+".scale", "exceeds max scale")
	}
}

func (s *structCheck) window(field string, w *WindowOp, scope exprScope) {
	switch w.Fn {
	case WinSMA, WinEMA, WinMax, WinMin, WinSum, WinCount, WinReturn, WinStddev:
	default:
		s.add(CodeStructuralInvalidOperator, field+".fn", fmt.Sprintf("unknown window function %q", w.Fn))
	}
	if scope.trigger != "" && w.Dependency == scope.trigger {
		s.add(CodeStructuralInvalidTrigger, field+".dependency", "windows cannot be evaluated over the triggering event")
	}
	if _, ok := s.deps[w.Dependency]; !ok {
		s.add(CodeStructuralUnresolvedRef, field+".dependency", fmt.Sprintf("dependency %q not declared", w.Dependency))
	}
	s.path(field+".path", w.Path)
	switch {
	case w.LookbackMS <= 0:
		s.add(CodeStructuralLookbackBudget, field+".lookback_ms", "must be > 0")
	case w.LookbackMS > s.doc.DataBudget.MaxLookbackMS:
		s.add(CodeStructuralLookbackBudget, field+".lookback_ms", "exceeds data_budget.max_lookback_ms")
	}
	if w.Scale > MaxScale {
		s.add(CodeStructuralInvalidOperator, field+".scale", "exceeds max scale")
	}
}

func (s *structCheck) path(field, p string) {
	if p == "" || len(p) > MaxPathLength {
		s.add(CodeStructuralInvalidPath, field, "path must be 1..128 bytes")
		return
	}
	for _, seg := range strings.Split(p, ".") {
		if !Ref(seg).Valid() {
			s.add(CodeStructuralInvalidPath, field, fmt.Sprintf("segment %q must match the ref grammar", seg))
			return
		}
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
