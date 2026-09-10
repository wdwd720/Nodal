package observability

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Units follow UCUM as recommended by OpenTelemetry.
const (
	UnitMilliseconds = "ms"
	UnitSeconds      = "s"
	UnitBasisPoints  = "{bps}"
	UnitUSDMinor     = "{usd_minor}"
	UnitCount        = "1"
)

// Histogram bucket boundaries. These are telemetry parameters, not
// financial values; recorded measurements are always int64.
var (
	latencyBucketsMS = []float64{1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000}
	bpsBuckets       = []float64{1, 5, 10, 25, 50, 100, 200, 500, 1000, 2500}
	usdMinorBuckets  = []float64{1, 10, 100, 1000, 10000, 100000, 1000000}
)

// ErrNilMeter is returned by the metric constructors when meter is nil.
var ErrNilMeter = errors.New("observability: meter is nil")

// FinancialMetrics holds the instruments of goal PART 132. Every field is
// named exactly after its instrument.
type FinancialMetrics struct {
	LedgerPostingErrors        metric.Int64Counter
	ReservationConflicts       metric.Int64Counter
	UnknownSubmissions         metric.Int64Counter
	ReconciliationMismatches   metric.Int64Counter
	OldestUnresolvedMismatch   metric.Int64Gauge // seconds
	ProviderDuplicateEvents    metric.Int64Counter
	DuplicateCommandRejections metric.Int64Counter
	NegativeDeficitAccounts    metric.Int64Gauge
	RiskRejections             metric.Int64Counter
	CapabilityGateRejections   metric.Int64Counter
	// VerificationPasses is the heartbeat: one per completed internal
	// verification pass. Every counter above is silent when nothing is wrong
	// AND silent when it was never constructed, and an alarm over a counter
	// cannot tell those apart. This one is meant to arrive every pass, so an
	// alarm that treats its absence as breaching can (F-118).
	VerificationPasses metric.Int64Counter
}

// NewFinancialMetrics creates the PART 132 instruments on meter.
func NewFinancialMetrics(meter metric.Meter) (*FinancialMetrics, error) {
	b := newBuilder(meter)
	if b == nil {
		return nil, ErrNilMeter
	}
	m := &FinancialMetrics{
		LedgerPostingErrors:        b.counter("ledger_posting_errors", "Ledger postings rejected or failed."),
		ReservationConflicts:       b.counter("reservation_conflicts", "Buying-power reservation conflicts."),
		UnknownSubmissions:         b.counter("unknown_submissions", "Submissions whose outcome is unknown after timeout."),
		ReconciliationMismatches:   b.counter("reconciliation_mismatches", "Reconciliation mismatches detected."),
		OldestUnresolvedMismatch:   b.gauge("oldest_unresolved_mismatch", "Age in seconds of the oldest unresolved reconciliation mismatch.", UnitSeconds),
		ProviderDuplicateEvents:    b.counter("provider_duplicate_events", "Inbound provider events rejected as duplicates."),
		DuplicateCommandRejections: b.counter("duplicate_command_rejections", "Commands rejected by idempotency checks."),
		NegativeDeficitAccounts:    b.gauge("negative_deficit_accounts", "Accounts currently in deficit.", UnitCount),
		RiskRejections:             b.counter("risk_rejections", "Intents rejected by risk policy."),
		CapabilityGateRejections:   b.counter("capability_gate_rejections", "Actions rejected by a capability gate."),
		VerificationPasses:         b.counter("verification_passes", "Completed internal verification passes; the alerting heartbeat."),
	}
	return m, b.err()
}

// RatePair is a numerator/denominator counter pair from which a rate is
// derived at query time; counters, unlike a ratio gauge, aggregate correctly.
type RatePair struct {
	Denominator metric.Int64Counter
	Numerator   metric.Int64Counter
}

// Observe increments the denominator and, when hit, the numerator.
func (p RatePair) Observe(ctx context.Context, hit bool, opts ...metric.AddOption) {
	if p.Denominator != nil {
		p.Denominator.Add(ctx, 1, opts...)
	}
	if hit && p.Numerator != nil {
		p.Numerator.Add(ctx, 1, opts...)
	}
}

// ExecutionMetrics holds the instruments of goal PART 133. Latencies are
// int64 milliseconds; slippage and price impact are int64 basis points.
type ExecutionMetrics struct {
	QuoteLatency        metric.Int64Histogram
	QuoteAgeAtSubmit    metric.Int64Histogram
	BuildLatency        metric.Int64Histogram
	SimulationLatency   metric.Int64Histogram
	SignLatency         metric.Int64Histogram
	SubmitLatency       metric.Int64Histogram
	ConfirmationLatency metric.Int64Histogram
	FinalityLatency     metric.Int64Histogram
	Slippage            metric.Int64Histogram
	PriceImpact         metric.Int64Histogram
	// ExecutionFailureRate is execution_failure_rate_attempts /
	// execution_failure_rate_failures.
	ExecutionFailureRate RatePair
	// UnknownSubmissionRate is unknown_submission_rate_submissions /
	// unknown_submission_rate_unknown.
	UnknownSubmissionRate RatePair
}

// NewExecutionMetrics creates the PART 133 instruments on meter.
func NewExecutionMetrics(meter metric.Meter) (*ExecutionMetrics, error) {
	b := newBuilder(meter)
	if b == nil {
		return nil, ErrNilMeter
	}
	m := &ExecutionMetrics{
		QuoteLatency:        b.histogram("quote_latency", "Time to obtain a quote.", UnitMilliseconds, latencyBucketsMS),
		QuoteAgeAtSubmit:    b.histogram("quote_age_at_submit", "Age of the quote when the order was submitted.", UnitMilliseconds, latencyBucketsMS),
		BuildLatency:        b.histogram("build_latency", "Time to build the transaction.", UnitMilliseconds, latencyBucketsMS),
		SimulationLatency:   b.histogram("simulation_latency", "Time to simulate the transaction.", UnitMilliseconds, latencyBucketsMS),
		SignLatency:         b.histogram("sign_latency", "Time to sign the transaction.", UnitMilliseconds, latencyBucketsMS),
		SubmitLatency:       b.histogram("submit_latency", "Time to submit the transaction.", UnitMilliseconds, latencyBucketsMS),
		ConfirmationLatency: b.histogram("confirmation_latency", "Time from submit to confirmation.", UnitMilliseconds, latencyBucketsMS),
		FinalityLatency:     b.histogram("finality_latency", "Time from submit to finality.", UnitMilliseconds, latencyBucketsMS),
		Slippage:            b.histogram("slippage", "Realized slippage versus quote, basis points.", UnitBasisPoints, bpsBuckets),
		PriceImpact:         b.histogram("price_impact", "Quoted price impact, basis points.", UnitBasisPoints, bpsBuckets),
		ExecutionFailureRate: RatePair{
			Denominator: b.counter("execution_failure_rate_attempts", "Execution attempts (denominator of execution_failure_rate)."),
			Numerator:   b.counter("execution_failure_rate_failures", "Execution failures (numerator of execution_failure_rate)."),
		},
		UnknownSubmissionRate: RatePair{
			Denominator: b.counter("unknown_submission_rate_submissions", "Submissions (denominator of unknown_submission_rate)."),
			Numerator:   b.counter("unknown_submission_rate_unknown", "Submissions with unknown outcome (numerator of unknown_submission_rate)."),
		},
	}
	return m, b.err()
}

// AgentMetrics holds the instruments of goal PART 134. Costs are int64 USD
// minor units; freshness and latency are int64 milliseconds.
type AgentMetrics struct {
	StrategyEvaluations metric.Int64Counter
	IntentProposals     metric.Int64Counter
	RiskRejects         metric.Int64Counter
	PredictionCount     metric.Int64Counter
	ModelCost           metric.Int64Histogram
	ToolCost            metric.Int64Histogram
	DataFreshness       metric.Int64Histogram
	DecisionLatency     metric.Int64Histogram
	AgentPauses         metric.Int64Counter
	PolicyViolations    metric.Int64Counter
}

// NewAgentMetrics creates the PART 134 instruments on meter.
func NewAgentMetrics(meter metric.Meter) (*AgentMetrics, error) {
	b := newBuilder(meter)
	if b == nil {
		return nil, ErrNilMeter
	}
	m := &AgentMetrics{
		StrategyEvaluations: b.counter("strategy_evaluations", "Strategy evaluations performed."),
		IntentProposals:     b.counter("intent_proposals", "Intents proposed by agents."),
		RiskRejects:         b.counter("risk_rejects", "Agent proposals rejected by risk."),
		PredictionCount:     b.counter("prediction_count", "Predictions recorded."),
		ModelCost:           b.histogram("model_cost", "Model call cost in USD minor units.", UnitUSDMinor, usdMinorBuckets),
		ToolCost:            b.histogram("tool_cost", "Tool call cost in USD minor units.", UnitUSDMinor, usdMinorBuckets),
		DataFreshness:       b.histogram("data_freshness", "Age of the freshest input data used for a decision.", UnitMilliseconds, latencyBucketsMS),
		DecisionLatency:     b.histogram("decision_latency", "Time to reach a decision.", UnitMilliseconds, latencyBucketsMS),
		AgentPauses:         b.counter("agent_pauses", "Agent pauses (manual or automatic)."),
		PolicyViolations:    b.counter("policy_violations", "Agent policy violations detected."),
	}
	return m, b.err()
}

// ---- attribute hygiene ----------------------------------------------------

// highCardinalityKeys are never allowed on metrics (goal PART 131).
var highCardinalityKeys = map[string]struct{}{
	"account_id": {}, "user_id": {}, "intent_id": {}, "order_id": {}, "wallet": {}, "signature": {},
	"request_id": {}, "correlation_id": {}, "workflow_id": {}, "session_id": {}, "subject_id": {},
	"trace_id": {}, "span_id": {}, "email": {}, "address": {}, "wallet_address": {}, "tx_signature": {},
	"tx_hash": {}, "idempotency_key": {}, "ip": {}, "client_ip": {},
}

// IsHighCardinalityKey reports whether key must be kept off metrics.
func IsHighCardinalityKey(key string) bool {
	norm := strings.ToLower(strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(strings.TrimSpace(key)))
	_, bad := highCardinalityKeys[norm]
	return bad
}

// SafeAttrs returns attrs without high-cardinality keys, preserving order.
func SafeAttrs(attrs ...attribute.KeyValue) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, kv := range attrs {
		if IsHighCardinalityKey(string(kv.Key)) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// SafeAttrSet is SafeAttrs as an attribute.Set.
func SafeAttrSet(attrs ...attribute.KeyValue) attribute.Set {
	return attribute.NewSet(SafeAttrs(attrs...)...)
}

// WithSafeAttrs is a measurement option carrying only safe attributes.
func WithSafeAttrs(attrs ...attribute.KeyValue) metric.MeasurementOption {
	return metric.WithAttributeSet(SafeAttrSet(attrs...))
}

// ---- builder ---------------------------------------------------------------

type builder struct {
	m    metric.Meter
	errs []error
}

func newBuilder(m metric.Meter) *builder {
	if m == nil {
		return nil
	}
	return &builder{m: m}
}

func (b *builder) fail(name string, err error) {
	if err != nil {
		b.errs = append(b.errs, fmt.Errorf("observability: instrument %s: %w", name, err))
	}
}

func (b *builder) err() error { return errors.Join(b.errs...) }

func (b *builder) counter(name, desc string) metric.Int64Counter {
	c, err := b.m.Int64Counter(name, metric.WithDescription(desc), metric.WithUnit(UnitCount))
	b.fail(name, err)
	return c
}

func (b *builder) gauge(name, desc, unit string) metric.Int64Gauge {
	g, err := b.m.Int64Gauge(name, metric.WithDescription(desc), metric.WithUnit(unit))
	b.fail(name, err)
	return g
}

func (b *builder) histogram(name, desc, unit string, bounds []float64) metric.Int64Histogram {
	h, err := b.m.Int64Histogram(name, metric.WithDescription(desc), metric.WithUnit(unit), metric.WithExplicitBucketBoundaries(bounds...))
	b.fail(name, err)
	return h
}
