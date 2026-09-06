package agent

import (
	"crypto/sha256"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// Authority is the frozen decision basis of one run: which effects the agent
// holds, which tools it may reach, what it may spend, which stage and mode it
// is in, and which envelope it is bound to.
//
// It is computed once, at run creation, from the agents row, the compiled
// strategy version's IR and the bound capital envelope, all of which are
// platform state. It is then immutable: every field is unexported, every
// method has a value receiver, and the package exposes no setter. Nothing
// read through a tool can reach it (PART 67). A test can therefore assert
// that Fingerprint is unchanged after an adversarial run, which is exactly
// the claim "content cannot change the agent's effect set, budgets, tools or
// lifecycle stage".
type Authority struct {
	agentID           AgentID
	agentVersion      int64
	accountID         string
	strategyVersionID string
	stage             Stage
	mode              Mode
	envelopeID        string
	riskPolicyVersion string

	effects []ir.Effect          // sorted, deduplicated
	tools   map[string]toolGrant // "code@version" -> the dependency that declared it

	data           BudgetLimit
	model          BudgetLimit
	intentsPerHour int
	modelRequired  bool

	fingerprint [32]byte
}

// toolGrant records why a tool is reachable: the dependency that named it and
// the effect that dependency carries. A tool not present here is refused
// even if it is registered, ACTIVE and within budget.
type toolGrant struct {
	dependency string
	effect     ir.Effect
	required   bool
	maxAge     time.Duration
}

// AuthorityInput is everything Authority is derived from. Every field is
// platform state read before any tool is dialed.
type AuthorityInput struct {
	AgentID           AgentID
	AgentVersion      int64
	AccountID         string
	StrategyVersionID string
	Stage             Stage
	Mode              Mode
	EnvelopeID        string
	RiskPolicyVersion string
	// IR is the compiled strategy document. Its declared effects and
	// dependencies define the closed sets; nothing else can extend them.
	IR *ir.IR
	// Envelope is the bound capital envelope's read-only snapshot. Its spend
	// caps intersect with the IR's, strictest wins. The zero value means no
	// envelope is bound (stages below CANARY), in which case the IR's own
	// budgets stand alone.
	Envelope EnvelopeSnapshot
}

// NewAuthority derives the frozen basis. It fails closed: an IR that declares
// an effect outside the allowed set, a dependency whose effect the IR does
// not declare, or a stage/mode mismatch is refused here rather than at the
// first tool call.
func NewAuthority(in AuthorityInput) (Authority, error) {
	if in.IR == nil {
		return Authority{}, errs.New(errs.CodeValidationFailed, "agent: authority requires a compiled IR")
	}
	if in.AgentID.IsZero() {
		return Authority{}, errs.New(errs.CodeValidationFailed, "agent: authority requires an agent id")
	}
	if !in.Stage.Valid() {
		return Authority{}, errs.Newf(errs.CodeValidationFailed, "agent: unknown stage %q", in.Stage)
	}
	if !ModeAllowed(in.Stage, in.Mode) {
		return Authority{}, errs.Newf(errs.CodeValidationFailed, "agent: mode %q is not permitted at stage %s", in.Mode, in.Stage)
	}
	if in.Stage.RealCapital() && in.EnvelopeID == "" {
		return Authority{}, errs.New(errs.CodeValidationFailed, "agent: CANARY, LIMITED and LIVE require a bound envelope")
	}

	effects := ir.SortEffects(in.IR.Effects)
	for _, e := range effects {
		if !e.Allowed() {
			return Authority{}, errs.Newf(errs.CodeEffectForbidden, "agent: strategy declares forbidden effect %s", e).
				WithField("effect", e.String())
		}
	}
	declared := map[ir.Effect]struct{}{}
	for _, e := range effects {
		declared[e] = struct{}{}
	}

	tools := map[string]toolGrant{}
	for _, d := range in.IR.Dependencies {
		eff := ir.EffectOfDependency(d.Kind)
		if eff == "" {
			continue // FEATURE dependencies are computed, not fetched
		}
		if _, ok := declared[eff]; !ok {
			return Authority{}, errs.Newf(errs.CodeEffectForbidden,
				"agent: dependency %s needs effect %s which the strategy does not declare", d.Name, eff).
				WithField("dependency", d.Name.String()).WithField("effect", eff.String())
		}
		tools[ToolKey(d.ToolCode, d.ToolVersion)] = toolGrant{
			dependency: d.Name.String(),
			effect:     eff,
			required:   d.Required,
			maxAge:     time.Duration(d.MaxAgeMS) * time.Millisecond,
		}
	}

	a := Authority{
		agentID:           in.AgentID,
		agentVersion:      in.AgentVersion,
		accountID:         in.AccountID,
		strategyVersionID: in.StrategyVersionID,
		stage:             in.Stage,
		mode:              in.Mode,
		envelopeID:        in.EnvelopeID,
		riskPolicyVersion: in.RiskPolicyVersion,
		effects:           effects,
		tools:             tools,
		data:              dataLimit(in.IR.DataBudget, in.Envelope),
		model:             modelLimit(in.IR.ModelBudget, in.Envelope),
		intentsPerHour:    minPositive(in.IR.Envelope.MaxIntentsPerHour, in.Envelope.MaxOrderRatePerHour),
		modelRequired:     in.IR.ModelBudget.Required,
	}
	a.fingerprint = a.computeFingerprint()
	return a, nil
}

// dataLimit intersects the IR's data budget with the envelope's data cap.
func dataLimit(b ir.DataBudget, e EnvelopeSnapshot) BudgetLimit {
	return BudgetLimit{
		Kind:        BudgetData,
		CallsPerRun: b.MaxToolCallsPerRun,
		CallsPerDay: b.MaxToolCallsPerDay,
		SpendPerDay: minUSD(b.MaxSpendPerDay, e.MaxDataSpendPerDay),
	}
}

// modelLimit intersects the IR's model budget with the envelope's model cap.
func modelLimit(b ir.ModelBudget, e EnvelopeSnapshot) BudgetLimit {
	return BudgetLimit{
		Kind:            BudgetModel,
		CallsPerRun:     b.MaxCallsPerRun,
		CallsPerDay:     b.MaxCallsPerDay,
		SpendPerDay:     minUSD(b.MaxSpendPerDay, e.MaxModelSpendPerDay),
		MaxOutputTokens: b.MaxOutputTokens,
		MaxInputTokens:  b.MaxInputTokens,
	}
}

// minUSD is the strictest of two caps. A zero cap on the envelope side means
// "the envelope sets no cap" and leaves the IR's limit standing; a zero cap
// on the IR side likewise defers to the envelope. When both are zero there is
// no spend allowance at all, which is the correct default for a strategy that
// declared no data budget.
func minUSD(a, b money.USD) money.USD {
	switch {
	case a.IsZero():
		return b
	case b.IsZero():
		return a
	case a.Cmp(b) <= 0:
		return a
	default:
		return b
	}
}

// minPositive is minUSD for counts: zero means "not capped here".
func minPositive(a, b int) int {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

// Accessors. Every one returns a copy; Authority is never handed out by
// pointer and holds no reference a caller can mutate.

// AgentID is the agent this authority belongs to.
func (a Authority) AgentID() AgentID { return a.agentID }

// AgentVersion is the agents.version recorded on runs and predictions.
func (a Authority) AgentVersion() int64 { return a.agentVersion }

// AccountID is the single account the agent is bound to.
func (a Authority) AccountID() string { return a.accountID }

// StrategyVersionID is the compiled version the agent runs.
func (a Authority) StrategyVersionID() string { return a.strategyVersionID }

// Stage is the ladder position at run creation.
func (a Authority) Stage() Stage { return a.stage }

// Mode is the performance mode copied onto the run.
func (a Authority) Mode() Mode { return a.mode }

// EnvelopeID is the bound capital envelope, or "".
func (a Authority) EnvelopeID() string { return a.envelopeID }

// RiskPolicyVersion is the policy the strategy was validated against.
func (a Authority) RiskPolicyVersion() string { return a.riskPolicyVersion }

// ModelRequired reports whether the strategy cannot proceed without a model
// answer (PART 177).
func (a Authority) ModelRequired() bool { return a.modelRequired }

// Effects returns the declared effect set, sorted.
func (a Authority) Effects() []ir.Effect { return append([]ir.Effect(nil), a.effects...) }

// AllowsEffect reports whether e is in the declared set. It is the first of
// the broker's five checks and it consults nothing but this frozen value.
func (a Authority) AllowsEffect(e ir.Effect) bool {
	for _, v := range a.effects {
		if v == e {
			return true
		}
	}
	return false
}

// AllowsTool reports whether the (code, version) pair was declared by a
// dependency of the compiled strategy.
func (a Authority) AllowsTool(code string, version int) bool {
	_, ok := a.tools[ToolKey(code, version)]
	return ok
}

// ToolEffect returns the effect the declaring dependency carries, and whether
// the tool is declared at all.
func (a Authority) ToolEffect(code string, version int) (ir.Effect, bool) {
	g, ok := a.tools[ToolKey(code, version)]
	return g.effect, ok
}

// Tools returns the sorted set of declared "code@version" keys.
func (a Authority) Tools() []string {
	out := make([]string, 0, len(a.tools))
	for k := range a.tools {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// DataBudget is the intersected data limit.
func (a Authority) DataBudget() BudgetLimit { return a.data }

// ModelBudget is the intersected model limit.
func (a Authority) ModelBudget() BudgetLimit { return a.model }

// IntentsPerHour is the intersected order-rate cap; 0 means uncapped here
// (the risk kernel still applies RISK_ORDER_RATE).
func (a Authority) IntentsPerHour() int { return a.intentsPerHour }

// Fingerprint is the sha256 of the canonical encoding of everything above.
// Two Authority values with the same fingerprint permit exactly the same
// effects, tools, budgets, stage and mode.
func (a Authority) Fingerprint() []byte {
	out := make([]byte, len(a.fingerprint))
	copy(out, a.fingerprint[:])
	return out
}

// computeFingerprint canonicalizes the authority basis. Field order is fixed,
// sets are sorted, and every value is length-prefixed so no two different
// bases can encode identically.
func (a Authority) computeFingerprint() [32]byte {
	var b strings.Builder
	put := func(k, v string) {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(strconv.Itoa(len(v)))
		b.WriteByte(':')
		b.WriteString(v)
		b.WriteByte('\n')
	}
	put("agent_id", a.agentID.String())
	put("agent_version", strconv.FormatInt(a.agentVersion, 10))
	put("account_id", a.accountID)
	put("strategy_version_id", a.strategyVersionID)
	put("stage", a.stage.String())
	put("mode", a.mode.String())
	put("envelope_id", a.envelopeID)
	put("risk_policy_version", a.riskPolicyVersion)
	put("effects", strings.Join(ir.EffectStrings(a.effects), ","))
	keys := a.Tools()
	for _, k := range keys {
		g := a.tools[k]
		put("tool:"+k, g.dependency+"|"+g.effect.String()+"|"+strconv.FormatBool(g.required)+"|"+strconv.FormatInt(g.maxAge.Milliseconds(), 10))
	}
	put("data_budget", a.data.canonical())
	put("model_budget", a.model.canonical())
	put("intents_per_hour", strconv.Itoa(a.intentsPerHour))
	put("model_required", strconv.FormatBool(a.modelRequired))
	return sha256.Sum256([]byte(b.String()))
}
