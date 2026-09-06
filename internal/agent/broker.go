package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// ToolCall is one request from the evaluator to the broker. The evaluator
// supplies a declared dependency and its parameters; it never supplies a URL,
// a host, a credential or prompt text.
type ToolCall struct {
	RunID      RunID
	Dependency ir.Dependency
	Params     map[string]string
	Effect     ir.Effect
	Deadline   time.Time
}

// ToolBroker is the only path from agent code to the outside world.
type ToolBroker interface {
	Invoke(ctx context.Context, call ToolCall) (Observation, error)
	Budgets(ctx context.Context, q db.Querier, now time.Time) (BudgetSnapshot, error)
}

// AdapterRequest is what an adapter receives. It carries the declared
// parameters, the deadline, the mode, and the egress-guarded HTTP client the
// adapter must use. It carries no principal, no envelope, no secret of the
// platform's and no free-text prompt.
type AdapterRequest struct {
	ToolCode    string
	ToolVersion int
	Dependency  string
	Params      map[string]string
	Mode        Mode
	Deadline    time.Time
	// HTTPClient refuses every host outside tools.egress_hosts.
	HTTPClient *http.Client
}

// AdapterResponse is one tool's typed answer.
type AdapterResponse struct {
	// Payload is the typed value, already conforming to the tool's declared
	// output schema.
	Payload json.RawMessage
	// Untrusted is external free text the payload referenced. Adapters pass
	// it here rather than embedding it in Payload so it stays quarantined.
	Untrusted []Untrusted
	// Source names the data source or provider that answered.
	Source string
	// SourceEventAt and ProviderPublishedAt are the provider's own times.
	SourceEventAt       time.Time
	ProviderPublishedAt *time.Time
	// FeatureAvailableAt is when a derived feature became computable; zero
	// means "as soon as it was normalized".
	FeatureAvailableAt time.Time
	// CostUSD overrides tools.cost_per_call_usd_minor when the provider
	// charges per response; zero means the registered per-call cost.
	CostUSD money.USD
	// OutputRef is the archive URI of the raw response, written by the
	// adapter before it interpreted anything.
	OutputRef string
}

// ToolAdapter performs one call to one registered tool version.
type ToolAdapter interface {
	Code() string
	Version() int
	Fetch(ctx context.Context, req AdapterRequest) (AdapterResponse, error)
}

// PauseChecker reports whether an agent currently has an open pause. The
// broker consults it before every call so a pause is effective immediately,
// including for a run already in flight (PART 71).
type PauseChecker interface {
	OpenPause(ctx context.Context, q db.Querier, agentID AgentID) (Pause, bool, error)
}

// InvocationRecorder writes the provenance row for one call.
type InvocationRecorder interface {
	Record(ctx context.Context, tx pgx.Tx, inv Invocation) error
}

// BrokerDeps are the broker's collaborators.
type BrokerDeps struct {
	DB       *db.DB
	Clock    clock.Clock
	Registry ToolRegistry
	Budgets  BudgetReader
	Pauses   PauseChecker
	Recorder InvocationRecorder
	// ModelCalls records model_calls provenance rows; required for CallModel.
	ModelCalls ModelCallRecorder
	Adapters   map[string]ToolAdapter // keyed by ToolKey(code, version)
	BaseRound  http.RoundTripper      // nil means http.DefaultTransport
	// Environment is the deployment environment code; a tool not registered
	// for it is refused.
	Environment string
}

// Broker is bound to one run's frozen Authority. It enforces, in this order:
//
//  1. pause      — an open pause refuses every call, immediately;
//  2. permission — the effect must be in the run's declared set, the tool
//     must have been declared by a dependency of the compiled strategy, the
//     registered tool's own effect must match, and the tool must be ACTIVE
//     and registered for this environment;
//  3. budgets    — per-run and per-period call counts and spend, read from
//     persisted counters before the adapter is constructed;
//  4. rate limit — per tool per minute and per run, from the same counters;
//  5. egress     — the adapter receives a client that can only reach the
//     tool's declared hosts;
//  6. provenance — exactly one tool_invocations row per call, refusals
//     included.
//
// Steps 1 to 4, the dial and step 6 all run inside one transaction that holds
// a per-agent advisory lock, so two concurrent runs of the same agent cannot
// spend the same allowance twice. The lock is held across the provider call,
// which is bounded by tools.timeout_ms; this is the deliberate cost of
// enforcing a spend limit exactly rather than approximately.
type Broker struct {
	deps      BrokerDeps
	authority Authority
	runID     RunID
}

var _ ToolBroker = (*Broker)(nil)

// NewBroker binds a broker to one run.
func NewBroker(deps BrokerDeps, authority Authority, runID RunID) (*Broker, error) {
	switch {
	case deps.DB == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: broker requires a database")
	case deps.Clock == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: broker requires a clock")
	case deps.Registry == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: broker requires a tool registry")
	case deps.Budgets == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: broker requires a budget reader")
	case deps.Pauses == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: broker requires a pause checker")
	case deps.Recorder == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: broker requires an invocation recorder")
	case runID.IsZero():
		return nil, errs.New(errs.CodeValidationFailed, "agent: broker requires a run id")
	case authority.AgentID().IsZero():
		return nil, errs.New(errs.CodeValidationFailed, "agent: broker requires a frozen authority")
	}
	if deps.Adapters == nil {
		deps.Adapters = map[string]ToolAdapter{}
	}
	return &Broker{deps: deps, authority: authority, runID: runID}, nil
}

// Authority returns the frozen basis this broker enforces. It is a copy: no
// caller, and no content, can change what the broker permits.
func (b *Broker) Authority() Authority { return b.authority }

// Budgets returns the current budget snapshot without making a call.
func (b *Broker) Budgets(ctx context.Context, q db.Querier, now time.Time) (BudgetSnapshot, error) {
	return b.deps.Budgets.Snapshot(ctx, q, b.authority, b.runID, now)
}

// refusal describes why a call was refused, for the invocation row.
type refusal struct {
	code        errs.Code
	err         error
	rateLimited bool
	budget      bool
}

// Invoke performs one tool call under the full enforcement chain. A refusal
// is a first-class outcome: it returns a typed error AND writes an
// unsuccessful tool_invocations row. It never returns a zero Observation with
// a nil error, and it never silently skips.
func (b *Broker) Invoke(ctx context.Context, call ToolCall) (Observation, error) {
	if call.RunID != b.runID {
		return Observation{}, errs.New(errs.CodeForbidden, "agent: tool call belongs to a different run").
			WithField("run_id", call.RunID.String())
	}
	if call.Dependency.ToolCode == "" || call.Dependency.ToolVersion < 1 {
		return Observation{}, errs.New(errs.CodeValidationFailed, "agent: tool call names no tool")
	}

	// A refusal is recorded, so the transaction that wrote the refusal row must
	// commit even though the caller receives an error. Only a genuine
	// infrastructure failure rolls back: refused is a decision, not a fault.
	var (
		obs     Observation
		refused error
	)
	err := b.deps.DB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 0},
		func(ctx context.Context, tx pgx.Tx) error {
			o, rec, err := b.invokeInTx(ctx, tx, call)
			obs, refused = o, nil
			if rec {
				// The refusal row is part of this transaction; commit it and
				// hand the error back to the caller afterwards.
				refused = err
				return nil
			}
			return err
		})
	if err != nil {
		return Observation{}, err
	}
	if refused != nil {
		return Observation{}, refused
	}
	return obs, nil
}

func (b *Broker) invokeInTx(ctx context.Context, tx pgx.Tx, call ToolCall) (Observation, bool, error) {
	now := b.deps.Clock.Now()
	key := ToolKey(call.Dependency.ToolCode, call.Dependency.ToolVersion)

	// Serialize this agent's calls so the persisted counters are exact.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "agent:"+b.authority.AgentID().String()); err != nil {
		return Observation{}, false, errs.Wrap(err, errs.CodeInternal, "agent: lock agent for tool call")
	}

	// (1) Pause. An open pause refuses every call, in flight or not.
	if _, paused, err := b.deps.Pauses.OpenPause(ctx, tx, b.authority.AgentID()); err != nil {
		return Observation{}, false, err
	} else if paused {
		return b.refuse(ctx, tx, call, Tool{Code: call.Dependency.ToolCode, Version: call.Dependency.ToolVersion}, now, refusal{
			code: errs.CodeKillSwitchActive,
			err: errs.New(errs.CodeKillSwitchActive, "agent: paused; no tool call is made").
				WithField("agent_id", b.authority.AgentID().String()),
		})
	}

	// (2) Permission. Every check reads the frozen authority, never content.
	declaredEffect, declared := b.authority.ToolEffect(call.Dependency.ToolCode, call.Dependency.ToolVersion)
	if !declared {
		return b.refuse(ctx, tx, call, Tool{Code: call.Dependency.ToolCode, Version: call.Dependency.ToolVersion}, now, refusal{
			code: errs.CodeEffectForbidden,
			err: errs.Newf(errs.CodeEffectForbidden, "agent: tool %s was not declared by the compiled strategy", key).
				WithField("tool", key),
		})
	}
	if call.Effect != "" && call.Effect != declaredEffect {
		return b.refuse(ctx, tx, call, Tool{Code: call.Dependency.ToolCode, Version: call.Dependency.ToolVersion}, now, refusal{
			code: errs.CodeEffectForbidden,
			err: errs.Newf(errs.CodeEffectForbidden, "agent: call claims effect %s but dependency %s carries %s",
				call.Effect, call.Dependency.Name, declaredEffect).WithField("effect", string(call.Effect)),
		})
	}
	if !b.authority.AllowsEffect(declaredEffect) {
		return b.refuse(ctx, tx, call, Tool{Code: call.Dependency.ToolCode, Version: call.Dependency.ToolVersion}, now, refusal{
			code: errs.CodeEffectForbidden,
			err: errs.Newf(errs.CodeEffectForbidden, "agent: effect %s is not in the run's effect set", declaredEffect).
				WithField("effect", declaredEffect.String()),
		})
	}

	tool, err := b.deps.Registry.Lookup(ctx, tx, call.Dependency.ToolCode, call.Dependency.ToolVersion)
	if err != nil {
		return Observation{}, false, err
	}
	if tool.Effect != declaredEffect {
		return b.refuse(ctx, tx, call, tool, now, refusal{
			code: errs.CodeEffectForbidden,
			err: errs.Newf(errs.CodeEffectForbidden, "agent: tool %s now declares effect %s, not %s", key, tool.Effect, declaredEffect).
				WithField("tool", key).WithField("effect", tool.Effect.String()),
		})
	}
	if !tool.Status.Invocable() {
		return b.refuse(ctx, tx, call, tool, now, refusal{
			code: errs.CodeProviderUnavailable,
			err: errs.Newf(errs.CodeProviderUnavailable, "agent: tool %s is %s", key, tool.Status).
				WithField("tool", key).WithField("status", tool.Status.String()),
		})
	}
	if b.deps.Environment != "" && !tool.AllowsEnvironment(b.deps.Environment) {
		return b.refuse(ctx, tx, call, tool, now, refusal{
			code: errs.CodeUnsupported,
			err: errs.Newf(errs.CodeUnsupported, "agent: tool %s is not registered for environment %s", key, b.deps.Environment).
				WithField("tool", key),
		})
	}

	// (3) Budgets, from persisted counters, before the adapter exists.
	snap, err := b.deps.Budgets.Snapshot(ctx, tx, b.authority, b.runID, now)
	if err != nil {
		return Observation{}, false, err
	}
	cost := tool.CostPerCall
	if err := snap.CheckTool(cost); err != nil {
		return b.refuse(ctx, tx, call, tool, now, refusal{code: errs.CodeBudgetExhausted, err: err, budget: true})
	}

	// (4) Rate limits: per run and per tool per minute.
	if err := b.checkRateLimits(ctx, tx, tool, now); err != nil {
		return b.refuse(ctx, tx, call, tool, now, refusal{code: errs.CodeRateLimited, err: err, rateLimited: true})
	}

	adapter, ok := b.deps.Adapters[key]
	if !ok {
		return b.refuse(ctx, tx, call, tool, now, refusal{
			code: errs.CodeProviderUnavailable,
			err: errs.Newf(errs.CodeProviderUnavailable, "agent: no adapter is wired for tool %s", key).
				WithField("tool", key),
		})
	}

	// (5) Egress. The adapter only ever gets this client.
	deadline := call.Deadline
	if deadline.IsZero() || deadline.After(now.Add(tool.Timeout)) {
		deadline = now.Add(tool.Timeout)
	}
	callCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	requestHash := hashRequest(key, call.Dependency.Name.String(), call.Params)
	start := b.deps.Clock.Now()
	resp, ferr := adapter.Fetch(callCtx, AdapterRequest{
		ToolCode:    tool.Code,
		ToolVersion: tool.Version,
		Dependency:  call.Dependency.Name.String(),
		Params:      copyParams(call.Params),
		Mode:        b.authority.Mode(),
		Deadline:    deadline,
		HTTPClient:  NewToolHTTPClient(tool, b.deps.BaseRound),
	})
	received := b.deps.Clock.Now()
	latency := received.Sub(start)
	if latency < 0 {
		latency = 0
	}

	if ferr != nil {
		code := errs.CodeOf(ferr)
		if IsEgressRefused(ferr) {
			ferr = egressRefusal(ferr)
			code = errs.CodeForbidden
		}
		if code == errs.CodeInternal {
			ferr = errs.Wrap(ferr, errs.CodeProviderUnavailable, "agent: tool call failed")
			code = errs.CodeProviderUnavailable
		}
		return b.refuseWithCost(ctx, tx, call, tool, now, requestHash, latency, cost, refusal{code: code, err: ferr})
	}

	// (6) Provenance and the typed observation.
	if resp.Source == "" {
		resp.Source = tool.Provider
	}
	if !resp.CostUSD.IsZero() {
		cost = resp.CostUSD
	}
	payload := resp.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	sum := sha256.Sum256(payload)
	outputHash := sum[:]

	normalized := received
	featureAvailable := resp.FeatureAvailableAt
	if featureAvailable.IsZero() || featureAvailable.Before(normalized) {
		featureAvailable = normalized
	}
	decisionAvailable := featureAvailable.Add(tool.PipelineLatency)

	invID := NewInvocationID()
	inv := Invocation{
		ID:                  invID,
		ToolID:              tool.ID,
		ToolCode:            tool.Code,
		ToolVersion:         tool.Version,
		Effect:              tool.Effect,
		AgentID:             b.authority.AgentID(),
		RunID:               b.runID,
		StrategyVersionID:   b.authority.StrategyVersionID(),
		DependencyName:      call.Dependency.Name.String(),
		Mode:                b.authority.Mode(),
		RequestHash:         requestHash,
		OutputHash:          outputHash,
		OutputRef:           resp.OutputRef,
		Source:              resp.Source,
		SourceEventAt:       nilTime(resp.SourceEventAt),
		ProviderPublishedAt: resp.ProviderPublishedAt,
		ReceivedAt:          received,
		DecisionAvailableAt: &decisionAvailable,
		CostUSD:             cost,
		Latency:             latency,
		Success:             true,
	}
	if err := b.deps.Recorder.Record(ctx, tx, inv); err != nil {
		return Observation{}, false, err
	}

	untrusted := make([]Untrusted, 0, len(resp.Untrusted))
	for _, u := range resp.Untrusted {
		if u.ProvenanceRef == "" {
			u.ProvenanceRef = invID.String()
		}
		untrusted = append(untrusted, u)
	}

	return Observation{
		Dependency:          call.Dependency.Name.String(),
		ToolCode:            tool.Code,
		ToolVersion:         tool.Version,
		Effect:              tool.Effect,
		InvocationID:        invID,
		Source:              resp.Source,
		Mode:                b.authority.Mode(),
		SourceEventAt:       resp.SourceEventAt,
		ProviderPublishedAt: resp.ProviderPublishedAt,
		PlatformReceivedAt:  received,
		NormalizedAt:        normalized,
		FeatureAvailableAt:  featureAvailable,
		DecisionAvailableAt: decisionAvailable,
		Payload:             payload,
		OutputHash:          outputHash,
		OutputRef:           resp.OutputRef,
		Untrusted:           untrusted,
		CostUSD:             cost,
		Latency:             latency,
	}, false, nil
}

const selectToolMinuteRateSQL = `
SELECT
    count(*) FILTER (WHERE created_at >= $3),
    count(*) FILTER (WHERE agent_run_id = $2)
  FROM tool_invocations
 WHERE tool_id = $1 AND NOT budget_refused`

// checkRateLimits applies tools.max_calls_per_minute (across the platform for
// that tool) and tools.max_calls_per_run.
func (b *Broker) checkRateLimits(ctx context.Context, tx pgx.Tx, tool Tool, now time.Time) error {
	var perMinute, perRun int
	if err := tx.QueryRow(ctx, selectToolMinuteRateSQL, tool.ID, b.runID, now.Add(-time.Minute)).
		Scan(&perMinute, &perRun); err != nil {
		return errs.Wrap(err, errs.CodeInternal, "agent: read tool rate counters")
	}
	if tool.MaxCallsPerRun > 0 && perRun >= tool.MaxCallsPerRun {
		return errs.Newf(errs.CodeRateLimited, "agent: tool %s already called %d times in this run (limit %d)",
			tool.Key(), perRun, tool.MaxCallsPerRun).WithField("tool", tool.Key())
	}
	if tool.MaxCallsPerMinute > 0 && perMinute >= tool.MaxCallsPerMinute {
		return errs.Newf(errs.CodeRateLimited, "agent: tool %s already called %d times this minute (limit %d)",
			tool.Key(), perMinute, tool.MaxCallsPerMinute).WithField("tool", tool.Key())
	}
	return nil
}

// refuse records an unsuccessful invocation and returns the typed error. A
// refusal is never silent and never free of evidence.
func (b *Broker) refuse(ctx context.Context, tx pgx.Tx, call ToolCall, tool Tool, now time.Time, r refusal) (Observation, bool, error) {
	hash := hashRequest(ToolKey(call.Dependency.ToolCode, call.Dependency.ToolVersion), call.Dependency.Name.String(), call.Params)
	return b.refuseWithCost(ctx, tx, call, tool, now, hash, 0, money.USD{}, r)
}

func (b *Broker) refuseWithCost(ctx context.Context, tx pgx.Tx, call ToolCall, tool Tool, now time.Time,
	requestHash []byte, latency time.Duration, cost money.USD, r refusal,
) (Observation, bool, error) {
	// A call the broker itself refused costs nothing: it was never dialed.
	if r.budget || r.rateLimited || latency == 0 {
		cost = money.USD{}
	}
	inv := Invocation{
		ID:                NewInvocationID(),
		ToolID:            tool.ID,
		ToolCode:          call.Dependency.ToolCode,
		ToolVersion:       call.Dependency.ToolVersion,
		Effect:            effectOr(tool.Effect, call.Effect),
		AgentID:           b.authority.AgentID(),
		RunID:             b.runID,
		StrategyVersionID: b.authority.StrategyVersionID(),
		DependencyName:    call.Dependency.Name.String(),
		Mode:              b.authority.Mode(),
		RequestHash:       requestHash,
		Source:            sourceOr(tool.Provider),
		ReceivedAt:        now,
		CostUSD:           cost,
		Latency:           latency,
		Success:           false,
		ErrorCode:         string(r.code),
		RateLimited:       r.rateLimited,
		BudgetRefused:     r.budget,
	}
	if tool.ID.IsZero() {
		// The tool was never resolved (undeclared or unregistered): there is
		// no tools row to reference, so the refusal is recorded on the run
		// instead of in tool_invocations, which requires a valid tool_id.
		return Observation{}, false, r.err
	}
	if err := b.deps.Recorder.Record(ctx, tx, inv); err != nil {
		return Observation{}, false, err
	}
	// The refusal row belongs to this transaction and must commit with it.
	return Observation{}, true, r.err
}

func effectOr(a, b ir.Effect) ir.Effect {
	if a != "" {
		return a
	}
	if b != "" {
		return b
	}
	return ir.EffectReadMarketData
}

func sourceOr(provider string) string {
	if provider != "" {
		return provider
	}
	return "UNRESOLVED"
}

// hashRequest is the deterministic digest of what was asked for. Parameters
// are sorted so the same request always hashes identically.
func hashRequest(toolKey, dependency string, params map[string]string) []byte {
	h := sha256.New()
	h.Write([]byte(toolKey))
	h.Write([]byte{0})
	h.Write([]byte(dependency))
	h.Write([]byte{0})
	for _, k := range sortedKeys(params) {
		h.Write([]byte(k))
		h.Write([]byte{1})
		h.Write([]byte(params[k]))
		h.Write([]byte{0})
	}
	return h.Sum(nil)
}

func copyParams(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func nilTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	c := t
	return &c
}
