package main

import (
	"log/slog"

	"go.opentelemetry.io/otel"

	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/reconciliation"
)

// financialMetrics builds the reconciliation instruments, or the in-process
// ones if the meter refuses.
//
// Both composition roots passed reconciliation.NoopMetrics(), so
// observability.NewFinancialMetrics -- whose instruments the 29 designed
// CloudWatch alarms are bound to -- had no caller outside its own tests. That
// was one of three independent reasons those alarms would have read green over
// a system emitting nothing (F-118).
//
// otel.Meter returns the global provider's meter, which observability.Setup
// leaves as a no-op while CP_TELEMETRY_OTLP_ENDPOINT is unset. That is
// deliberate rather than a workaround: this removes the reason that is
// SOFTWARE, and leaves the one that is a deployment decision, rather than
// leaving both and calling the result instrumented.
//
// A failure to build instruments is not a reason to refuse to start: the
// alternative to a metric is not a stopped service. It is logged and the
// in-process counters are used, which is what Raise falls back to anyway.
func financialMetrics(log *slog.Logger) *reconciliation.Metrics {
	fm, err := observability.NewFinancialMetrics(otel.Meter("controlplane/financial"))
	if err != nil {
		log.Warn("observability: financial instruments unavailable; alerts are logged and counted in process only",
			"error", err.Error())
		return reconciliation.NoopMetrics()
	}
	return reconciliation.NewMetrics(fm)
}
