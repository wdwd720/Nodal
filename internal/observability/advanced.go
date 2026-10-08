package observability

import (
	"go.opentelemetry.io/otel/metric"
)

// AdvancedMetrics holds the instruments of the advanced capabilities (advanced
// goal §29; ADR-0020). Every field is named after its instrument, every value
// is an int64 (USD in minor units, ratios in basis points, latencies in ms),
// and every instrument is prefixed "advanced_" so a dashboard can never mix
// simulated-domain figures with real-capital ones.
type AdvancedMetrics struct {
	// Capital (gauges, recorded by the periodic capital collector).
	GrossAssets         metric.Int64Gauge
	CustomerLiabilities metric.Int64Gauge
	HouseAssets         metric.Int64Gauge
	ReserveCoverage     metric.Int64Gauge // bps of outstanding supply covered by eligible reserve
	MarginExposure      metric.Int64Gauge
	Leverage            metric.Int64Gauge // bps
	LiquidationBuffer   metric.Int64Gauge

	// Execution. The latencies are recorded on the order book's request path
	// (attribute outcome); everything else from the database.
	OrderLatency        metric.Int64Histogram
	CancelLatency       metric.Int64Histogram
	FillRate            RatePair            // attribute: owner_type
	RejectionRate       RatePair            // attribute: owner_type
	InternalMatchVolume metric.Int64Counter // usd minor
	ExternalRouteVolume metric.Int64Counter // usd minor

	// Risk.
	RiskDenies           metric.Int64Counter // attribute: reason
	StaleOracleEvents    metric.Int64Counter
	MarginCalls          metric.Int64Counter
	LiquidationEvents    metric.Int64Counter
	CircuitBreakerEvents metric.Int64Counter // attributes: kind, reason

	// Agent.
	IntentProposals     metric.Int64Counter
	DeniedIntents       metric.Int64Counter
	SigningRequests     metric.Int64Counter
	WithdrawalProposals metric.Int64Counter
	AuthorityViolations metric.Int64Counter // attributes: kind, stage

	// Sandbox.
	SandboxRuns              metric.Int64Counter
	SandboxKilledRuns        metric.Int64Counter // attribute: reason
	SandboxCPULimitHits      metric.Int64Counter
	SandboxMemoryLimitHits   metric.Int64Counter
	SandboxNetworkViolations metric.Int64Counter

	// Reconciliation (gauges from the periodic collector). ProviderMismatches
	// is recorded by nothing: no advanced reconciliation compares against an
	// external provider.
	UnmatchedEconomicEffects metric.Int64Gauge
	ProviderMismatches       metric.Int64Gauge
	ChainMismatches          metric.Int64Gauge
	ReserveMismatches        metric.Int64Gauge
}

// NewAdvancedMetrics creates the advanced-capability instruments on meter.
func NewAdvancedMetrics(meter metric.Meter) (*AdvancedMetrics, error) {
	b := newBuilder(meter)
	if b == nil {
		return nil, ErrNilMeter
	}
	m := &AdvancedMetrics{
		GrossAssets:         b.gauge("advanced_gross_assets", "Simulated-domain assets held across all books, USD minor units.", UnitUSDMinor),
		CustomerLiabilities: b.gauge("advanced_customer_liabilities", "Simulated-domain value owed to customers, USD minor units.", UnitUSDMinor),
		HouseAssets:         b.gauge("advanced_house_assets", "Simulated-domain value held by house books, USD minor units.", UnitUSDMinor),
		ReserveCoverage:     b.gauge("advanced_reserve_coverage", "Stable-asset eligible reserve over outstanding supply, basis points.", UnitBasisPoints),
		MarginExposure:      b.gauge("advanced_margin_exposure", "Gross exposure of margin accounts, USD minor units.", UnitUSDMinor),
		Leverage:            b.gauge("advanced_leverage", "Aggregate gross exposure over equity of margin accounts, basis points.", UnitBasisPoints),
		LiquidationBuffer:   b.gauge("advanced_liquidation_buffer", "Equity above maintenance across margin accounts, USD minor units.", UnitUSDMinor),

		OrderLatency: b.histogram("advanced_order_latency", "Customer order submit latency on the order book pipeline, every stage, by outcome.",
			UnitMilliseconds, latencyBucketsMS),
		CancelLatency: b.histogram("advanced_cancel_latency", "Customer order cancel latency on the order book pipeline, by outcome.",
			UnitMilliseconds, latencyBucketsMS),
		FillRate: RatePair{
			Denominator: b.counter("advanced_fill_rate_orders", "Book orders recorded (denominator of advanced_fill_rate)."),
			Numerator:   b.counter("advanced_fill_rate_filled", "Book orders with at least one fill, counted at the first (numerator of advanced_fill_rate)."),
		},
		RejectionRate: RatePair{
			Denominator: b.counter("advanced_rejection_rate_orders", "Book orders recorded (denominator of advanced_rejection_rate)."),
			Numerator:   b.counter("advanced_rejection_rate_rejected", "Book orders rejected (numerator of advanced_rejection_rate)."),
		},
		InternalMatchVolume: b.counterIn("advanced_internal_match_volume", "Notional matched on the internal book, USD minor units.", UnitUSDMinor),
		ExternalRouteVolume: b.counterIn("advanced_external_route_volume", "Notional filled against the paper venue, USD minor units.", UnitUSDMinor),

		RiskDenies:           b.counter("advanced_risk_denies", "Risk Kernel V2 rejections, by reason."),
		StaleOracleEvents:    b.counter("advanced_stale_oracle_events", "Risk-increasing actions refused for a stale or missing mark."),
		MarginCalls:          b.counter("advanced_margin_calls", "Margin calls issued."),
		LiquidationEvents:    b.counter("advanced_liquidation_events", "Liquidation steps executed."),
		CircuitBreakerEvents: b.counter("advanced_circuit_breaker_events", "Circuit breakers tripped, by kind and reason."),

		IntentProposals:     b.counter("advanced_intent_proposals", "Economic intents proposed, by actor type."),
		DeniedIntents:       b.counter("advanced_denied_intents", "Economic intents rejected, by stage."),
		SigningRequests:     b.counter("advanced_signing_requests", "Delegated signing requests, by outcome."),
		WithdrawalProposals: b.counter("advanced_withdrawal_proposals", "Delegated withdrawal proposals, by outcome."),
		AuthorityViolations: b.counter("advanced_authority_violations", "Delegated requests refused for exceeding the owner's grant, by kind and stage."),

		SandboxRuns:              b.counter("advanced_sandbox_runs", "Strategy sandbox runs."),
		SandboxKilledRuns:        b.counter("advanced_sandbox_killed_runs", "Strategy sandbox runs terminated, by reason."),
		SandboxCPULimitHits:      b.counter("advanced_sandbox_cpu_limit", "Runs stopped by the wall-time/CPU limit."),
		SandboxMemoryLimitHits:   b.counter("advanced_sandbox_memory_limit", "Runs stopped by the memory limit."),
		SandboxNetworkViolations: b.counter("advanced_sandbox_network_violations", "Strategy versions refused at scan time for importing the network or another process."),

		UnmatchedEconomicEffects: b.gauge("advanced_unmatched_economic_effects", "Economic effects PENDING or UNKNOWN past the reconciliation horizon.", UnitCount),
		ProviderMismatches: b.gauge("advanced_provider_mismatches",
			"Open reconciliation records against an external provider; recorded by nothing while no advanced reconciliation compares against one.", UnitCount),
		ChainMismatches:   b.gauge("advanced_chain_mismatches", "Open reconciliation records against a simulated chain.", UnitCount),
		ReserveMismatches: b.gauge("advanced_reserve_mismatches", "Open stable-reserve reconciliation records.", UnitCount),
	}
	return m, b.err()
}

// counterIn is a counter whose values are in unit rather than a count.
func (b *builder) counterIn(name, desc, unit string) metric.Int64Counter {
	c, err := b.m.Int64Counter(name, metric.WithDescription(desc), metric.WithUnit(unit))
	b.fail(name, err)
	return c
}
