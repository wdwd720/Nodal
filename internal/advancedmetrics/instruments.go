package advancedmetrics

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/nodal/controlplane/internal/advancedjobs"
	"github.com/nodal/controlplane/internal/observability"
)

// Instruments are the collector's own instruments, beside
// observability.AdvancedMetrics: the figures that keep the advanced ones
// honest.
type Instruments struct {
	// OpenReconciliationRecords is the open records per kind.
	OpenReconciliationRecords metric.Int64Gauge
	// UnvaluedHoldings is how many SIMULATED assets with a holding could
	// not be valued, and are therefore missing from the capital gauges.
	UnvaluedHoldings metric.Int64Gauge
	// UnvaluedMatchTrades counts internal trades whose quote asset could not
	// be valued, and are therefore missing from the match volume.
	UnvaluedMatchTrades metric.Int64Counter
	// UnvaluedRouteFills counts paper venue fills whose quote asset could not
	// be valued, and are therefore missing from the external route volume.
	UnvaluedRouteFills metric.Int64Counter
}

// NewInstruments creates the collector's instruments on meter.
func NewInstruments(meter metric.Meter) (*Instruments, error) {
	if meter == nil {
		return nil, observability.ErrNilMeter
	}
	var errs []error
	gauge := func(name, desc, unit string) metric.Int64Gauge {
		g, err := meter.Int64Gauge(name, metric.WithDescription(desc), metric.WithUnit(unit))
		if err != nil {
			errs = append(errs, fmt.Errorf("advancedmetrics: instrument %s: %w", name, err))
		}
		return g
	}
	in := &Instruments{
		OpenReconciliationRecords: gauge("advanced_open_reconciliation_records",
			"Reconciliation records not yet resolved, by kind.", observability.UnitCount),
		UnvaluedHoldings: gauge("advanced_unvalued_holdings",
			"Simulated assets with a holding that could not be valued and is missing from the capital gauges.", observability.UnitCount),
	}
	counter := func(name, desc string) metric.Int64Counter {
		c, err := meter.Int64Counter(name, metric.WithDescription(desc), metric.WithUnit(observability.UnitCount))
		if err != nil {
			errs = append(errs, fmt.Errorf("advancedmetrics: instrument %s: %w", name, err))
		}
		return c
	}
	in.UnvaluedMatchTrades = counter("advanced_unvalued_match_trades",
		"Internal book trades whose quote asset could not be valued and are missing from the match volume.")
	in.UnvaluedRouteFills = counter("advanced_unvalued_route_fills",
		"Paper venue fills whose quote asset could not be valued and are missing from the external route volume.")
	return in, errors.Join(errs...)
}

// JobHealth records the advanced worker's job health. It implements
// advancedjobs.Observer.
type JobHealth struct {
	lastSuccess         metric.Int64Gauge
	consecutiveFailures metric.Int64Gauge
	duration            metric.Int64Histogram
}

var _ advancedjobs.Observer = (*JobHealth)(nil)

// NewJobHealth creates the job-health instruments on meter.
func NewJobHealth(meter metric.Meter) (*JobHealth, error) {
	if meter == nil {
		return nil, observability.ErrNilMeter
	}
	ls, err1 := meter.Int64Gauge("advanced_job_last_success",
		metric.WithDescription("When an advanced worker job last processed every unit it examined, unix seconds."),
		metric.WithUnit(observability.UnitSeconds))
	cf, err2 := meter.Int64Gauge("advanced_job_consecutive_failures",
		metric.WithDescription("Failed runs of an advanced worker job since its last success."),
		metric.WithUnit(observability.UnitCount))
	d, err3 := meter.Int64Histogram("advanced_job_run_duration",
		metric.WithDescription("Duration of an advanced worker job's run."),
		metric.WithUnit(observability.UnitMilliseconds))
	if err := errors.Join(err1, err2, err3); err != nil {
		return nil, fmt.Errorf("advancedmetrics: job health instruments: %w", err)
	}
	return &JobHealth{lastSuccess: ls, consecutiveFailures: cf, duration: d}, nil
}

// ObserveRun implements advancedjobs.Observer.
func (j *JobHealth) ObserveRun(ctx context.Context, h advancedjobs.Health) {
	attrs := observability.WithSafeAttrs(attribute.String("job", h.Job))
	if !h.LastSuccess.IsZero() {
		j.lastSuccess.Record(ctx, h.LastSuccess.Unix(), attrs)
	}
	j.consecutiveFailures.Record(ctx, h.ConsecutiveFailures, attrs)
	j.duration.Record(ctx, h.LastDuration.Milliseconds(), attrs)
}
