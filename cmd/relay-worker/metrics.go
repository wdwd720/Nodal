package main

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/observability"
)

// Instrument names. Relay lag is the signal that tells an operator the read
// models are stale, so outbox_depth and outbox_oldest_unpublished_age are
// gauges an alert can be written against directly rather than quantities a
// human has to derive from log lines.
const (
	metricPublished    = "outbox_events_published"
	metricFailures     = "outbox_publish_failures"
	metricRetries      = "outbox_publish_retries"
	metricLag          = "outbox_publish_lag"
	metricUnregistered = "outbox_unregistered_topic_events"
	metricDepth        = "outbox_depth"
	metricEligible     = "outbox_depth_eligible"
	metricOldestAge    = "outbox_oldest_unpublished_age"
	metricBlocked      = "outbox_blocked_partitions"
	metricPartitions   = "outbox_partitions"
	metricSampleErrors = "outbox_snapshot_errors"
)

// lagBucketsMS covers a healthy relay (single-digit milliseconds from commit
// to publish) through an outage measured in hours.
var lagBucketsMS = []float64{
	5, 10, 25, 50, 100, 250, 500, 1000, 5000, 15000, 60000, 300000, 1800000, 3600000, 21600000,
}

// unregisteredTopicLabel keeps the topic attribute bounded. A registered
// topic is one of a fixed, small set; anything else is collapsed to this
// value, because a row whose topic is not in the registry can carry an
// arbitrary string and metric labels must never take unbounded values. The
// exact name is not lost: it is on the WARN line and in `status`.
const unregisteredTopicLabel = "_unregistered"

// relayMetrics is the event.Observer of internal/event wired onto
// OpenTelemetry instruments, plus the gauges the sampler feeds.
//
// Every method is called from inside the relay's transaction, so nothing
// here may block or fail: counters and atomics only.
type relayMetrics struct {
	published    metric.Int64Counter
	failures     metric.Int64Counter
	retries      metric.Int64Counter
	lag          metric.Int64Histogram
	unregistered metric.Int64Counter
	depth        metric.Int64Gauge
	eligible     metric.Int64Gauge
	oldestAge    metric.Int64Gauge
	blocked      metric.Int64Gauge
	partitions   metric.Int64Gauge
	sampleErrors metric.Int64Counter

	// Process-lifetime tallies. The host loop reads the deltas to decide
	// how long to sleep, and the shutdown log line reports the totals.
	totalPublished    atomic.Int64
	totalFailed       atomic.Int64
	totalRetried      atomic.Int64
	totalUnregistered atomic.Int64
}

var _ event.Observer = (*relayMetrics)(nil)

// newRelayMetrics builds the instruments. A nil meter yields a metrics value
// whose instruments are nil; every use is guarded, so the worker still runs
// and still logs when no meter provider is configured.
func newRelayMetrics(meter metric.Meter) (*relayMetrics, error) {
	m := &relayMetrics{}
	if meter == nil {
		return m, nil
	}
	var errs []error
	counter := func(name, desc string) metric.Int64Counter {
		c, err := meter.Int64Counter(name, metric.WithDescription(desc), metric.WithUnit(observability.UnitCount))
		if err != nil {
			errs = append(errs, err)
		}
		return c
	}
	gauge := func(name, desc, unit string) metric.Int64Gauge {
		g, err := meter.Int64Gauge(name, metric.WithDescription(desc), metric.WithUnit(unit))
		if err != nil {
			errs = append(errs, err)
		}
		return g
	}
	m.published = counter(metricPublished, "Outbox rows the relay published and marked published.")
	m.failures = counter(metricFailures, "Outbox publish attempts the bus rejected; the row stays unpublished and retryable.")
	m.retries = counter(metricRetries, "Outbox publish attempts that were not the first attempt for their row.")
	m.unregistered = counter(metricUnregistered, "Outbox rows whose topic is not in the event registry.")
	m.sampleErrors = counter(metricSampleErrors, "Failures to read the outbox depth snapshot.")
	m.depth = gauge(metricDepth, "Committed outbox rows that are not published yet.", observability.UnitCount)
	m.eligible = gauge(metricEligible, "Unpublished outbox rows the relay may claim right now.", observability.UnitCount)
	m.oldestAge = gauge(metricOldestAge, "Age in seconds of the oldest unpublished outbox row: relay lag.", observability.UnitSeconds)
	m.blocked = gauge(metricBlocked, "Partitions whose oldest unpublished row has failed or is backed off.", observability.UnitCount)
	m.partitions = gauge(metricPartitions, "Partitions holding unpublished outbox rows.", observability.UnitCount)
	h, err := meter.Int64Histogram(metricLag,
		metric.WithDescription("Time from outbox recorded_at to successful publish."),
		metric.WithUnit(observability.UnitMilliseconds),
		metric.WithExplicitBucketBoundaries(lagBucketsMS...))
	if err != nil {
		errs = append(errs, err)
	}
	m.lag = h
	return m, errors.Join(errs...)
}

// topicAttr bounds the topic label to the registry.
func topicAttr(topic string) attribute.KeyValue {
	if _, ok := event.Lookup(event.Topic(topic)); !ok {
		return attribute.String("topic", unregisteredTopicLabel)
	}
	return attribute.String("topic", topic)
}

// OnPublished records a successful publish. attempts includes the successful
// attempt, so attempts > 1 means the row had failed before.
func (m *relayMetrics) OnPublished(topic string, attempts int, lag time.Duration) {
	ctx := context.Background()
	attrs := observability.WithSafeAttrs(topicAttr(topic))
	m.totalPublished.Add(1)
	if m.published != nil {
		m.published.Add(ctx, 1, attrs)
	}
	if attempts > 1 {
		m.totalRetried.Add(1)
		if m.retries != nil {
			m.retries.Add(ctx, 1, attrs)
		}
	}
	if m.lag != nil {
		m.lag.Record(ctx, int64(nonNegative(lag)/time.Millisecond), attrs)
	}
}

// OnPublishFailed records a rejected publish. The row is still unpublished
// and still retryable; this counter is what an alert on "the bus is
// unreachable" is written against.
func (m *relayMetrics) OnPublishFailed(topic string, attempts int, _ error) {
	ctx := context.Background()
	attrs := observability.WithSafeAttrs(topicAttr(topic))
	m.totalFailed.Add(1)
	if m.failures != nil {
		m.failures.Add(ctx, 1, attrs)
	}
	if attempts > 1 {
		m.totalRetried.Add(1)
		if m.retries != nil {
			m.retries.Add(ctx, 1, attrs)
		}
	}
}

// OnDuplicate is the inbox hook. This binary has no inbox: it produces, it
// does not consume.
func (m *relayMetrics) OnDuplicate(string) {}

// onUnregisteredTopic counts a row whose topic the registry does not know.
// The relay still publishes it; dropping an event to make the number look
// better would be the one unforgivable behavior here.
func (m *relayMetrics) onUnregisteredTopic(ctx context.Context, topic string) {
	m.totalUnregistered.Add(1)
	if m.unregistered != nil {
		// The raw topic is deliberately not a label; see
		// unregisteredTopicLabel.
		m.unregistered.Add(ctx, 1, observability.WithSafeAttrs(topicAttr(topic)))
	}
}

// recordSnapshot publishes the depth and lag gauges.
func (m *relayMetrics) recordSnapshot(ctx context.Context, s Snapshot) {
	if m.depth != nil {
		m.depth.Record(ctx, s.Unpublished)
	}
	if m.eligible != nil {
		m.eligible.Record(ctx, s.Eligible)
	}
	if m.oldestAge != nil {
		m.oldestAge.Record(ctx, int64(s.OldestAge()/time.Second))
	}
	if m.blocked != nil {
		m.blocked.Record(ctx, s.BlockedPartitions)
	}
	if m.partitions != nil {
		m.partitions.Record(ctx, s.Partitions)
	}
}

func (m *relayMetrics) recordSnapshotError(ctx context.Context) {
	if m.sampleErrors != nil {
		m.sampleErrors.Add(ctx, 1)
	}
}

// counters is the process-lifetime tally used by the host loop and by the
// shutdown log line.
type counters struct{ published, failed, retried, unregistered int64 }

func (m *relayMetrics) counters() counters {
	return counters{
		published:    m.totalPublished.Load(),
		failed:       m.totalFailed.Load(),
		retried:      m.totalRetried.Load(),
		unregistered: m.totalUnregistered.Load(),
	}
}
