// Package observability wires OpenTelemetry, structured logging and the
// financial/execution/agent metric instruments for every binary.
//
// Responsibility
//
//   - Setup configures the global OpenTelemetry tracer and meter providers
//     with OTLP/gRPC exporters, service.name / service.version /
//     deployment.environment resource attributes and W3C trace-context
//     propagation, and returns a shutdown function. With no OTLP endpoint it
//     installs no-op providers so code paths are identical in every
//     environment (goal PART 131).
//   - NewLogger returns a *slog.Logger (JSON; text in LOCAL) whose handler
//     chain always contains the redaction handler: attributes whose key
//     matches the denylist (private_key, seed, mnemonic, secret, token,
//     authorization, cookie, password, card, pan, cvv, ssn, api_key, ...)
//     are replaced by "[REDACTED]" at any nesting depth, and string values
//     that look like bearer tokens, Solana private keys or PEM blocks are
//     masked (goal PART 190). Secret is a slog.LogValuer that never renders.
//   - Context helpers carry request_id, correlation_id, intent_id, order_id
//     and workflow_id; ContextAttrs exposes them (plus trace_id/span_id) and
//     the context handler injects them into every record.
//   - FinancialMetrics, ExecutionMetrics and AgentMetrics create the
//     instruments named exactly as in goal PARTs 132 to 134. SafeAttrs drops
//     high-cardinality keys (account_id, user_id, intent_id, order_id, wallet,
//     signature, ...) from metric attributes.
//   - HTTPMiddleware wraps handlers with otelhttp and request-id propagation
//     (X-Request-Id generated when absent and always echoed).
//
// This package must never
//
//   - Log or export a secret. The redaction handler is not optional and the
//     fallback logger returned by LoggerFrom is also redacting.
//   - Export telemetry without TLS in PROD (Setup refuses OTLPInsecure there).
//   - Put per-account, per-user, per-order or wallet identifiers on metrics.
//   - Represent money as floating point. Cost histograms take int64 USD minor
//     units; latency histograms take int64 milliseconds. The only float64 in
//     the package is the trace sampling probability and histogram bucket
//     boundaries, which are telemetry parameters, not financial values.
//   - Import internal/errs, internal/clock or internal/id (foundation
//     packages are wired by the integrator).
package observability
