package strategy

import (
	"fmt"
	"sort"
	"time"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// Stages of the compile pipeline, in order (STRATEGY_IR.md §4). They are
// persisted in compile_attempts.stage_reached.
const (
	StagePrompt     = "PROMPT"
	StageResponse   = "RESPONSE"
	StageParse      = "PARSE"
	StageStructural = "STRUCTURAL"
	StageType       = "TYPE"
	StageEffect     = "EFFECT"
	StageRiskCompat = "RISK_COMPAT"
	StageRender     = "RENDER"
	StageAccepted   = "ACCEPTED"
)

// TYPE and RISK_COMPAT failure codes. PARSE, STRUCTURAL and EFFECT codes
// live in the ir package, next to the checks that produce them.
const (
	CodeTypeScaleMismatch    = "TYPE_SCALE_MISMATCH"
	CodeTypeNotBoolean       = "TYPE_NOT_BOOLEAN"
	CodeTypeNotDecimal       = "TYPE_NOT_DECIMAL"
	CodeTypeProbabilityRange = "TYPE_PROBABILITY_RANGE"
	CodeTypeNegativeMoney    = "TYPE_NEGATIVE_MONEY"
	CodeTypeBadRounding      = "TYPE_BAD_ROUNDING"
	CodeTypeBadSizing        = "TYPE_BAD_SIZING"
	CodeTypeFractionRange    = "TYPE_FRACTION_RANGE"
	CodeTypeBadConstant      = "TYPE_BAD_CONSTANT"

	CodeUnsupportedAsset = "UNSUPPORTED_ASSET"
	CodeUnsupportedVenue = "UNSUPPORTED_VENUE"
	CodeUnknownTool      = "UNKNOWN_TOOL"

	CodeRiskIncompatible  = "RISK_INCOMPATIBLE"
	CodeRiskPolicyMissing = "RISK_POLICY_MISSING"
)

// Tool is the subset of a tools registry row (migration 00502) the compiler
// needs. The caller loads it; this package never queries.
type Tool struct {
	Code    string
	Version int
	Effect  ir.Effect
	Status  string
	// Environments the tool may be used in; empty means unrestricted.
	Environments []string
}

// Active reports whether the tool may back a new strategy version.
func (t Tool) Active() bool { return t.Status == "ACTIVE" }

// ValidationRefs is the registry snapshot the TYPE and RISK_COMPAT stages
// check against. It is loaded by the caller and passed in, so Validate
// stays a pure function that the golden corpus can run without a database.
type ValidationRefs struct {
	// Instruments is keyed by instrument id string.
	Instruments map[string]instruments.Instrument
	// Venues is keyed by venue code.
	Venues map[string]instruments.Venue
	// Tools is keyed by "code@version".
	Tools map[string]Tool
	// Policy is the composed risk policy the strategy must fit inside.
	Policy risk.Policy
	// PolicyHash is the hex rules hash of that policy, compared with the
	// IR's pinned RiskPolicy.Hash.
	PolicyHash string
	// AssetClasses that may appear in envelope requirements.
	AssetClasses map[string]struct{}
	// Now is the instant instrument activity windows are evaluated at. It is
	// supplied rather than read, so validation stays deterministic.
	Now time.Time
}

// ToolKey is the map key for a tool code and version.
func ToolKey(code string, version int) string { return fmt.Sprintf("%s@%d", code, version) }

// Report is the outcome of one validation pass.
type Report struct {
	Stage  string
	Issues []ir.Issue
}

// OK reports whether nothing was found.
func (r Report) OK() bool { return len(r.Issues) == 0 }

// Codes returns the sorted, unique failure codes.
func (r Report) Codes() []string { return ir.Codes(r.Issues) }

// Fields maps each offending field to its finding.
func (r Report) Fields() map[string]string { return ir.Fields(r.Issues) }

// Err converts a failing report into the typed error, or nil when it passed.
func (r Report) Err() error {
	if r.OK() {
		return nil
	}
	return &ir.ValidationError{Stage: r.Stage, Issues: r.Issues}
}

// Terminal reports whether the failure must not be retried. A forbidden
// effect and a risk incompatibility are decisions about what the platform
// permits: feeding them back to a model to try again would be asking it to
// negotiate with the policy.
func (r Report) Terminal() bool {
	for _, c := range r.Codes() {
		switch c {
		case ir.CodeEffectForbidden, ir.CodeEffectMismatch, CodeRiskIncompatible, CodeRiskPolicyMissing:
			return true
		}
	}
	return false
}

// Validate runs the document-local stages and then the registry-dependent
// ones, stopping at the first stage that fails. It never reads a clock,
// never iterates a map without sorting and never calls a model, so the same
// call is made at compile time, at agent promotion and by the corpus.
func Validate(doc *ir.IR, refs ValidationRefs) Report {
	if doc == nil {
		return Report{Stage: StageParse, Issues: []ir.Issue{{Code: ir.CodeParseFailed, Detail: "nil document"}}}
	}
	if issues := ir.ForbiddenEffectIssues(doc); len(issues) > 0 {
		return Report{Stage: StageEffect, Issues: issues}
	}
	if issues := ir.Structural(doc); len(issues) > 0 {
		return Report{Stage: StageStructural, Issues: issues}
	}
	if issues := typeCheck(doc, refs); len(issues) > 0 {
		return Report{Stage: StageType, Issues: issues}
	}
	if issues := ir.EffectIssues(doc); len(issues) > 0 {
		return Report{Stage: StageEffect, Issues: issues}
	}
	if issues := riskCompat(doc, refs); len(issues) > 0 {
		return Report{Stage: StageRiskCompat, Issues: issues}
	}
	return Report{Stage: StageAccepted}
}

// typeCheck is the TYPE stage: expressions type to a decimal or a bool with
// agreeing scales, probabilities are in range, money is non-negative, and
// every instrument, venue and tool exists and is usable.
func typeCheck(doc *ir.IR, refs ValidationRefs) []ir.Issue {
	c := &typeChecker{doc: doc, refs: refs, signals: map[ir.Ref]uint8{}}
	c.registries()
	c.signalTypes()
	c.conditionTypes()
	c.actionTypes()
	c.budgets()
	sort.SliceStable(c.issues, func(i, j int) bool { return c.issues[i].Field < c.issues[j].Field })
	return c.issues
}

type typeChecker struct {
	doc     *ir.IR
	refs    ValidationRefs
	issues  []ir.Issue
	signals map[ir.Ref]uint8
}

func (c *typeChecker) add(code, field, detail string) {
	c.issues = append(c.issues, ir.Issue{Code: code, Field: field, Detail: detail})
}

// registries checks that everything the document names exists and is
// active. An instrument that is halted or delisted, or a venue outside the
// allowlist, is rejected here rather than at execution time.
func (c *typeChecker) registries() {
	for i, decl := range c.doc.Instruments {
		field := fmt.Sprintf("instruments[%d].instrument_id", i)
		inst, ok := c.refs.Instruments[decl.InstrumentID]
		switch {
		case !ok:
			c.add(CodeUnsupportedAsset, field, fmt.Sprintf("instrument %s is not in the registry", decl.InstrumentID))
		case inst.Status != assets.StatusActive:
			c.add(CodeUnsupportedAsset, field, fmt.Sprintf("instrument %s has status %s, not ACTIVE", decl.InstrumentID, inst.Status))
		case !inst.IsActiveAt(c.refs.Now):
			c.add(CodeUnsupportedAsset, field, fmt.Sprintf("instrument %s is outside its activity window", decl.InstrumentID))
		}
	}
	for i, dep := range c.doc.Dependencies {
		field := fmt.Sprintf("dependencies[%d]", i)
		tool, ok := c.refs.Tools[ToolKey(dep.ToolCode, dep.ToolVersion)]
		switch {
		case !ok:
			c.add(CodeUnknownTool, field+".tool_code", fmt.Sprintf("tool %s@%d is not registered", dep.ToolCode, dep.ToolVersion))
		case !tool.Active():
			c.add(CodeUnknownTool, field+".tool_code", fmt.Sprintf("tool %s@%d has status %s", dep.ToolCode, dep.ToolVersion, tool.Status))
		case tool.Effect != ir.EffectOfDependency(dep.Kind):
			// The registry decides what a tool may do. A dependency that
			// claims a kind whose effect differs from the tool's own is an
			// attempt to read through a capability the tool does not carry.
			c.add(CodeUnknownTool, field+".kind",
				fmt.Sprintf("tool %s@%d carries effect %s, but kind %s requires %s", dep.ToolCode, dep.ToolVersion, tool.Effect, dep.Kind, ir.EffectOfDependency(dep.Kind)))
		}
	}
	c.venues()
}

func (c *typeChecker) venues() {
	check := func(field, code string) {
		venue, ok := c.refs.Venues[code]
		switch {
		case !ok:
			c.add(CodeUnsupportedVenue, field, fmt.Sprintf("venue %q is not in the registry", code))
		case !venue.Status.AllowsNewActions():
			c.add(CodeUnsupportedVenue, field, fmt.Sprintf("venue %q has status %s", code, venue.Status))
		}
	}
	for i, v := range c.doc.Envelope.Venues {
		check(fmt.Sprintf("envelope.venues[%d]", i), v)
	}
	for i, a := range c.doc.Actions {
		if a.Intent == nil {
			continue
		}
		for j, v := range a.Intent.Constraints.AllowedVenues {
			check(fmt.Sprintf("actions[%d].intent.constraints.allowed_venues[%d]", i, j), v)
		}
	}
	if len(c.refs.AssetClasses) > 0 {
		for i, ac := range c.doc.Envelope.AssetClasses {
			if _, ok := c.refs.AssetClasses[ac]; !ok {
				c.add(CodeUnsupportedAsset, fmt.Sprintf("envelope.asset_classes[%d]", i), fmt.Sprintf("asset class %q is not supported", ac))
			}
		}
	}
}

func (c *typeChecker) signalTypes() {
	for i, s := range c.doc.Signals {
		field := fmt.Sprintf("signals[%d]", i)
		c.rounding(field+".rounding", s.Rounding)
		kind, scale := c.exprType(field+".expr", &s.Expr)
		if kind == kindBool {
			c.add(CodeTypeNotDecimal, field+".expr", "a signal must be a decimal, not a boolean")
		}
		if kind == kindDecimal && scale != s.Scale {
			c.add(CodeTypeScaleMismatch, field+".scale", fmt.Sprintf("expression yields scale %d, declared %d", scale, s.Scale))
		}
		c.signals[s.Name] = s.Scale
	}
}

func (c *typeChecker) conditionTypes() {
	for i, cond := range c.doc.Conditions {
		field := fmt.Sprintf("conditions[%d].expr", i)
		if kind, _ := c.exprType(field, &cond.Expr); kind != kindBool {
			c.add(CodeTypeNotBoolean, field, "a condition must type to a boolean")
		}
	}
	for i, t := range c.doc.Triggers {
		if t.Filter == nil {
			continue
		}
		field := fmt.Sprintf("triggers[%d].filter", i)
		if kind, _ := c.exprType(field, t.Filter); kind != kindBool {
			c.add(CodeTypeNotBoolean, field, "a trigger filter must type to a boolean")
		}
	}
}

type exprKind int

const (
	kindInvalid exprKind = iota
	kindDecimal
	kindBool
)

// exprType returns the type and, for a decimal, its scale. Unknown shapes
// report kindInvalid without a second finding: the STRUCTURAL stage has
// already reported them, and repeating it here would double-count.
func (c *typeChecker) exprType(field string, e *ir.Expr) (exprKind, uint8) {
	switch {
	case e == nil:
		return kindInvalid, 0
	case e.Const != nil:
		if err := e.Const.Validate(); err != nil {
			c.add(CodeTypeBadConstant, field, err.Error())
			return kindInvalid, 0
		}
		return kindDecimal, e.Const.Scale
	case e.Field != nil:
		return kindDecimal, e.Field.Scale
	case e.Signal != nil:
		scale, ok := c.signals[*e.Signal]
		if !ok {
			return kindInvalid, 0
		}
		return kindDecimal, scale
	case e.Window != nil:
		c.rounding(field+".window.rounding", e.Window.Rounding)
		return kindDecimal, e.Window.Scale
	case e.Bin != nil:
		c.rounding(field+".bin.rounding", e.Bin.Rounding)
		lk, ls := c.exprType(field+".bin.l", e.Bin.L)
		rk, rs := c.exprType(field+".bin.r", e.Bin.R)
		if lk == kindBool || rk == kindBool {
			c.add(CodeTypeNotDecimal, field+".bin", "arithmetic operands must be decimals")
			return kindInvalid, 0
		}
		// MIN and MAX compare their operands, so both sides must already
		// agree; ADD and the rest state an explicit result scale.
		if (e.Bin.Op == ir.OpMin || e.Bin.Op == ir.OpMax) && lk == kindDecimal && rk == kindDecimal && ls != rs {
			c.add(CodeTypeScaleMismatch, field+".bin", fmt.Sprintf("%s operands have scales %d and %d", e.Bin.Op, ls, rs))
		}
		return kindDecimal, e.Bin.Scale
	case e.Cmp != nil:
		lk, ls := c.exprType(field+".cmp.l", e.Cmp.L)
		rk, rs := c.exprType(field+".cmp.r", e.Cmp.R)
		if lk == kindBool || rk == kindBool {
			c.add(CodeTypeNotDecimal, field+".cmp", "comparison operands must be decimals")
			return kindBool, 0
		}
		// Comparing two decimals of different scales is a type error, never
		// an implicit coercion: 1.00 and 1.000 would otherwise compare
		// unequal or equal depending on which side got rescaled.
		if lk == kindDecimal && rk == kindDecimal && ls != rs {
			c.add(CodeTypeScaleMismatch, field+".cmp", fmt.Sprintf("operands have scales %d and %d; no implicit coercion", ls, rs))
		}
		return kindBool, 0
	case e.And != nil, e.Or != nil:
		operands := e.And
		label := ".and"
		if e.Or != nil {
			operands, label = e.Or, ".or"
		}
		for i, op := range operands {
			if k, _ := c.exprType(fmt.Sprintf("%s%s[%d]", field, label, i), op); k != kindBool {
				c.add(CodeTypeNotBoolean, fmt.Sprintf("%s%s[%d]", field, label, i), "boolean operator needs boolean operands")
			}
		}
		return kindBool, 0
	case e.Not != nil:
		if k, _ := c.exprType(field+".not", e.Not); k != kindBool {
			c.add(CodeTypeNotBoolean, field+".not", "negation needs a boolean operand")
		}
		return kindBool, 0
	}
	return kindInvalid, 0
}

func (c *typeChecker) rounding(field, name string) {
	if _, err := ir.ParseRounding(name); err != nil {
		c.add(CodeTypeBadRounding, field, fmt.Sprintf("%q is not a rounding mode", name))
	}
}

func (c *typeChecker) actionTypes() {
	for i, a := range c.doc.Actions {
		field := fmt.Sprintf("actions[%d]", i)
		if a.Prediction != nil {
			c.predictionTypes(field+".prediction", a.Prediction)
		}
		if a.Intent != nil {
			c.intentTypes(field+".intent", a.Intent)
		}
	}
}

// predictionTypes enforces the prediction ledger's column types: the two
// probability fields and confidence are scale-4 decimals in [0,1], and the
// bps fields are whole basis points.
func (c *typeChecker) predictionTypes(field string, p *ir.PredictionSpec) {
	probabilities := []struct {
		name string
		expr *ir.Expr
	}{
		{"probability", &p.Probability},
		{"downside_probability", &p.DownsideProbability},
		{"confidence", &p.Confidence},
	}
	for _, pr := range probabilities {
		kind, scale := c.exprType(field+"."+pr.name, pr.expr)
		if kind != kindDecimal {
			c.add(CodeTypeNotDecimal, field+"."+pr.name, "must be a decimal")
			continue
		}
		if scale != ir.ProbabilityScale {
			c.add(CodeTypeProbabilityRange, field+"."+pr.name,
				fmt.Sprintf("probabilities are scale %d decimals, got scale %d", ir.ProbabilityScale, scale))
		}
		// A literal outside [0,1] is decidable now; a computed one is
		// checked again by prediction.Validate before it is committed.
		if pr.expr.Const != nil && !pr.expr.Const.IsProbability() {
			c.add(CodeTypeProbabilityRange, field+"."+pr.name, fmt.Sprintf("%s is outside [0, 1]", pr.expr.Const))
		}
	}
	for _, b := range []struct {
		name string
		expr *ir.Expr
	}{{"expected_return_bps", &p.ExpectedReturnBPS}, {"max_downside_bps", &p.MaxDownsideBPS}} {
		kind, scale := c.exprType(field+"."+b.name, b.expr)
		if kind != kindDecimal {
			c.add(CodeTypeNotDecimal, field+"."+b.name, "must be a decimal")
			continue
		}
		if scale != 0 {
			c.add(CodeTypeScaleMismatch, field+"."+b.name, fmt.Sprintf("basis points are whole numbers (scale 0), got scale %d", scale))
		}
	}
	if p.MaxDownsideBPS.Const != nil && p.MaxDownsideBPS.Const.Sign() < 0 {
		c.add(CodeTypeNegativeMoney, field+".max_downside_bps", "max downside must not be negative")
	}
}

func (c *typeChecker) intentTypes(field string, in *ir.IntentSpec) {
	s := in.Sizing
	// Exactly the field the sizing kind names must be present. A sizing that
	// carries two amounts has no single meaning.
	set := 0
	for _, present := range []bool{s.NotionalUSD != nil, s.FractionBPS != nil, s.TargetUSD != nil} {
		if present {
			set++
		}
	}
	switch s.Kind {
	case ir.SizingFixedNotional:
		switch {
		case s.NotionalUSD == nil:
			c.add(CodeTypeBadSizing, field+".sizing.notional_usd", "required for FIXED_NOTIONAL")
		case !s.NotionalUSD.IsPositive():
			c.add(CodeTypeNegativeMoney, field+".sizing.notional_usd", "must be positive")
		}
		if set != 1 {
			c.add(CodeTypeBadSizing, field+".sizing", "FIXED_NOTIONAL takes only notional_usd")
		}
	case ir.SizingEnvelopeFractionBPS:
		switch {
		case s.FractionBPS == nil:
			c.add(CodeTypeBadSizing, field+".sizing.fraction_bps", "required for ENVELOPE_FRACTION_BPS")
		case *s.FractionBPS <= 0:
			c.add(CodeTypeFractionRange, field+".sizing.fraction_bps", "must be positive")
		case *s.FractionBPS > money.OneHundredPercent:
			// "Use all available funds" arrives here: a fraction above 100%
			// of the envelope is not a large trade, it is an unbounded one.
			c.add(CodeTypeFractionRange, field+".sizing.fraction_bps",
				fmt.Sprintf("%d bps exceeds 100%% of the envelope", *s.FractionBPS))
		}
		if set != 1 {
			c.add(CodeTypeBadSizing, field+".sizing", "ENVELOPE_FRACTION_BPS takes only fraction_bps")
		}
	case ir.SizingTargetExposure:
		switch {
		case s.TargetUSD == nil:
			c.add(CodeTypeBadSizing, field+".sizing.target_usd", "required for TARGET_EXPOSURE")
		case s.TargetUSD.IsNegative():
			c.add(CodeTypeNegativeMoney, field+".sizing.target_usd", "must not be negative")
		}
		if set != 1 {
			c.add(CodeTypeBadSizing, field+".sizing", "TARGET_EXPOSURE takes only target_usd")
		}
	case ir.SizingNone:
		if set != 0 {
			c.add(CodeTypeBadSizing, field+".sizing", "NONE takes no amount")
		}
	}
	for _, b := range []struct {
		name string
		v    money.BPS
	}{
		{"max_slippage_bps", in.Constraints.MaxSlippageBPS},
		{"max_fee_bps", in.Constraints.MaxFeeBPS},
		{"max_price_impact_bps", in.Constraints.MaxPriceImpactBPS},
	} {
		if b.v < 0 || b.v > money.OneHundredPercent {
			c.add(CodeTypeFractionRange, field+".constraints."+b.name, "must be within 0..10000 bps")
		}
	}
	if in.Constraints.QuoteFreshnessMS < 0 {
		c.add(CodeTypeBadSizing, field+".constraints.quote_freshness_ms", "must not be negative")
	}
}

func (c *typeChecker) budgets() {
	for _, m := range []struct {
		field string
		v     money.USD
	}{
		{"model_budget.max_spend_per_day", c.doc.ModelBudget.MaxSpendPerDay},
		{"data_budget.max_spend_per_day", c.doc.DataBudget.MaxSpendPerDay},
		{"envelope.min_allocation", c.doc.Envelope.MinAllocation},
		{"envelope.max_single_trade", c.doc.Envelope.MaxSingleTrade},
		{"envelope.max_position", c.doc.Envelope.MaxPosition},
		{"envelope.max_daily_loss", c.doc.Envelope.MaxDailyLoss},
	} {
		if m.v.IsNegative() {
			c.add(CodeTypeNegativeMoney, m.field, "must not be negative")
		}
	}
}

// riskCompat is the RISK_COMPAT stage: the strategy's own limits must fit
// inside the policy's. The comparison is one-directional — a strategy may
// be stricter than the policy, never looser — so a compiled strategy can
// never be the reason a policy limit is exceeded.
func riskCompat(doc *ir.IR, refs ValidationRefs) []ir.Issue {
	var issues []ir.Issue
	add := func(code, field, detail string) {
		issues = append(issues, ir.Issue{Code: code, Field: field, Detail: detail})
	}
	policy := refs.Policy
	if policy.Missing() {
		add(CodeRiskPolicyMissing, "risk_policy", "no risk policy is in force for this account")
		return issues
	}
	if missing := policy.MissingLimits(); len(missing) > 0 {
		add(CodeRiskPolicyMissing, "risk_policy", fmt.Sprintf("the composed policy leaves %v unset", missing))
		return issues
	}
	// The IR pins the exact policy it was validated against. A hash that no
	// longer matches means the policy changed underneath the strategy, and
	// the strategy has to be revalidated rather than trusted.
	if refs.PolicyHash != "" && doc.RiskPolicy.Hash.String() != refs.PolicyHash {
		add(CodeRiskIncompatible, "risk_policy.hash",
			fmt.Sprintf("pinned policy hash %s does not match the policy in force (%s)", doc.RiskPolicy.Hash, refs.PolicyHash))
	}
	if doc.RiskPolicy.Version != "" && policy.Version != "" && doc.RiskPolicy.Version != policy.Version {
		add(CodeRiskIncompatible, "risk_policy.version",
			fmt.Sprintf("pinned policy version %q does not match %q", doc.RiskPolicy.Version, policy.Version))
	}

	env := doc.Envelope
	cmpUSD := func(field string, have money.USD, limit *money.USD, what string) {
		if limit != nil && have.Cmp(*limit) > 0 {
			add(CodeRiskIncompatible, field, fmt.Sprintf("%s %s exceeds the policy limit %s", what, have, *limit))
		}
	}
	cmpUSD("envelope.max_single_trade", env.MaxSingleTrade, policy.MaxSingleTradeUSD, "max single trade")
	cmpUSD("envelope.max_position", env.MaxPosition, policy.MaxPositionUSD, "max position")
	cmpUSD("envelope.max_daily_loss", env.MaxDailyLoss, policy.MaxDailyLossUSD, "max daily loss")

	if policy.MaxOrdersPerHour != nil && env.MaxIntentsPerHour > *policy.MaxOrdersPerHour {
		add(CodeRiskIncompatible, "envelope.max_intents_per_hour",
			fmt.Sprintf("%d intents per hour exceeds the policy order rate %d", env.MaxIntentsPerHour, *policy.MaxOrdersPerHour))
	}

	// Every sized action is checked against the single-trade limit too: an
	// envelope within the limit does not help if one action asks for more.
	for i, a := range doc.Actions {
		if a.Intent == nil {
			continue
		}
		field := fmt.Sprintf("actions[%d].intent.sizing", i)
		if n := a.Intent.Sizing.NotionalUSD; n != nil {
			cmpUSD(field+".notional_usd", *n, policy.MaxSingleTradeUSD, "trade notional")
			cmpUSD(field+".notional_usd", *n, &env.MaxSingleTrade, "trade notional")
		}
		if tgt := a.Intent.Sizing.TargetUSD; tgt != nil {
			cmpUSD(field+".target_usd", *tgt, policy.MaxPositionUSD, "target exposure")
		}
		cons := a.Intent.Constraints
		cmpBPS := func(name string, have money.BPS, limit *money.BPS) {
			if limit != nil && have > *limit {
				add(CodeRiskIncompatible, fmt.Sprintf("actions[%d].intent.constraints.%s", i, name),
					fmt.Sprintf("%d bps is looser than the policy limit %d bps", have, *limit))
			}
		}
		cmpBPS("max_slippage_bps", cons.MaxSlippageBPS, policy.MaxSlippageBPS)
		cmpBPS("max_fee_bps", cons.MaxFeeBPS, policy.MaxFeeBPS)
		cmpBPS("max_price_impact_bps", cons.MaxPriceImpactBPS, policy.MaxPriceImpactBPS)

		if policy.MaxQuoteAgeMS != nil && cons.QuoteFreshnessMS > *policy.MaxQuoteAgeMS {
			add(CodeRiskIncompatible, fmt.Sprintf("actions[%d].intent.constraints.quote_freshness_ms", i),
				fmt.Sprintf("%d ms is staler than the policy limit %d ms", cons.QuoteFreshnessMS, *policy.MaxQuoteAgeMS))
		}
		for j, v := range cons.AllowedVenues {
			if policy.AllowedVenues != nil && !contains(policy.AllowedVenues, v) {
				add(CodeRiskIncompatible, fmt.Sprintf("actions[%d].intent.constraints.allowed_venues[%d]", i, j),
					fmt.Sprintf("venue %q is not in the policy allowlist", v))
			}
		}
	}

	for i, v := range env.Venues {
		if policy.AllowedVenues != nil && !contains(policy.AllowedVenues, v) {
			add(CodeRiskIncompatible, fmt.Sprintf("envelope.venues[%d]", i),
				fmt.Sprintf("venue %q is not in the policy allowlist", v))
		}
	}

	// Declared staleness bounds must be at least as strict as the policy's
	// per-kind caps (PART 174).
	for i, dep := range doc.Dependencies {
		kind := dataAgeKey(dep.Kind)
		limit, ok := policy.MaxDataAgeMS[kind]
		if ok && dep.MaxAgeMS > limit {
			add(CodeRiskIncompatible, fmt.Sprintf("dependencies[%d].max_age_ms", i),
				fmt.Sprintf("%d ms is staler than the policy cap %d ms for %s data", dep.MaxAgeMS, limit, kind))
		}
	}
	sort.SliceStable(issues, func(i, j int) bool { return issues[i].Field < issues[j].Field })
	return issues
}

// dataAgeKey maps a dependency kind onto the policy's max_data_age_ms key.
//
// Each kind gets its own key rather than being folded into the nearest
// neighbor. A derived feature (a 24-hour volume, say) is legitimately far
// older than the spot price it was computed from, so charging it against
// the price bound would reject honest strategies; and a policy that does
// not name a kind leaves it unconstrained, which is the policy's decision
// to make rather than this function's.
func dataAgeKey(k ir.DependencyKind) string {
	switch k {
	case ir.DepPrice:
		return "price"
	case ir.DepFeature:
		return "feature"
	case ir.DepWalletEvent:
		return "wallet_event"
	case ir.DepOnchain:
		return "onchain"
	case ir.DepSocial:
		return "social"
	case ir.DepWalletIntelligence:
		return "wallet_intelligence"
	case ir.DepModel:
		return "model"
	}
	return string(k)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
