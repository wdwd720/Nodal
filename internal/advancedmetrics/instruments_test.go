package advancedmetrics

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/nodal/controlplane/internal/advancedjobs"
)

func newTestMeter(t *testing.T) (metric.Meter, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	return mp.Meter("test"), reader
}

func collectMetrics(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

// gaugeValues returns a gauge's last value per value of one attribute ("" for
// a gauge recorded without it).
func gaugeValues(t *testing.T, m metricdata.Metrics, key string) map[string]int64 {
	t.Helper()
	g, ok := m.Data.(metricdata.Gauge[int64])
	require.True(t, ok, "%s is not an int64 gauge", m.Name)
	out := map[string]int64{}
	for _, dp := range g.DataPoints {
		v, _ := dp.Attributes.Value(attribute.Key(key))
		out[v.AsString()] = dp.Value
	}
	return out
}

// TestJobHealth_RecordsTheSchedulersHealthPerJob: the three job-health
// instruments carry the scheduler's own figures, attribute job, and a job
// that has never succeeded has no last-success point rather than a zero.
func TestJobHealth_RecordsTheSchedulersHealthPerJob(t *testing.T) {
	t.Parallel()
	meter, reader := newTestMeter(t)
	jh, err := NewJobHealth(meter)
	require.NoError(t, err)
	ok := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	jh.ObserveRun(context.Background(), advancedjobs.Health{Job: "margin.interest", LastSuccess: ok, LastDuration: 1500 * time.Millisecond})
	jh.ObserveRun(context.Background(), advancedjobs.Health{Job: "capital.reservation_expiry", ConsecutiveFailures: 3, LastDuration: 20 * time.Millisecond})

	got := collectMetrics(t, reader)
	last := gaugeValues(t, got["advanced_job_last_success"], "job")
	assert.Equal(t, map[string]int64{"margin.interest": ok.Unix()}, last, "a job that never succeeded reports no last success")
	fails := gaugeValues(t, got["advanced_job_consecutive_failures"], "job")
	assert.Equal(t, int64(3), fails["capital.reservation_expiry"])
	assert.Equal(t, int64(0), fails["margin.interest"])
	h, isHist := got["advanced_job_run_duration"].Data.(metricdata.Histogram[int64])
	require.True(t, isHist)
	sums := map[string]int64{}
	for _, dp := range h.DataPoints {
		v, _ := dp.Attributes.Value("job")
		sums[v.AsString()] = dp.Sum
	}
	assert.Equal(t, int64(1500), sums["margin.interest"], "milliseconds")
	assert.Equal(t, int64(20), sums["capital.reservation_expiry"])
}

// TestInstruments_AreNamespacedAndDescribed: the collector's own instruments
// sit under advanced_ beside the ones they qualify.
func TestInstruments_AreNamespacedAndDescribed(t *testing.T) {
	t.Parallel()
	meter, reader := newTestMeter(t)
	in, err := NewInstruments(meter)
	require.NoError(t, err)
	ctx := context.Background()
	in.OpenReconciliationRecords.Record(ctx, 1)
	in.UnvaluedHoldings.Record(ctx, 1)
	in.UnvaluedMatchTrades.Add(ctx, 1)
	in.UnvaluedRouteFills.Add(ctx, 1)
	got := collectMetrics(t, reader)
	for _, name := range []string{"advanced_open_reconciliation_records", "advanced_unvalued_holdings", "advanced_unvalued_match_trades",
		"advanced_unvalued_route_fills"} {
		require.Contains(t, got, name)
		assert.NotEmpty(t, got[name].Description, name)
	}
	_, err = NewInstruments(nil)
	assert.Error(t, err)
	_, err = NewJobHealth(nil)
	assert.Error(t, err)
}
