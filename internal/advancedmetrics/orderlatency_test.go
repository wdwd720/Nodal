package advancedmetrics

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/orders"
)

// histogramByOutcome returns a latency histogram's (count, sum) per outcome.
func histogramByOutcome(t *testing.T, m metricdata.Metrics) map[string][2]int64 {
	t.Helper()
	h, ok := m.Data.(metricdata.Histogram[int64])
	require.True(t, ok, "%s is not an int64 histogram", m.Name)
	out := map[string][2]int64{}
	for _, dp := range h.DataPoints {
		v, _ := dp.Attributes.Value("outcome")
		out[v.AsString()] = [2]int64{int64(dp.Count), dp.Sum} // #nosec G115 -- a test's handful of observations
	}
	return out
}

// TestOrderLatency_RecordsEachSubmitAndCancelOnceInMillisecondsByOutcome:
// a submit goes to the order latency and a cancel to the cancel latency, once
// each, in whole milliseconds rounded toward zero, with the outcome of what
// the command returned; a command the pipeline does not time has no
// instrument and is not recorded.
func TestOrderLatency_RecordsEachSubmitAndCancelOnceInMillisecondsByOutcome(t *testing.T) {
	t.Parallel()
	meter, reader := newTestMeter(t)
	m, err := observability.NewAdvancedMetrics(meter)
	require.NoError(t, err)
	l, err := NewOrderLatency(m)
	require.NoError(t, err)
	ctx := context.Background()
	l.ObserveCommand(ctx, orders.CommandTiming{Kind: orders.CommandNewOrder, Elapsed: 1999 * time.Microsecond})
	l.ObserveCommand(ctx, orders.CommandTiming{Kind: orders.CommandNewOrder, Elapsed: 40 * time.Millisecond,
		Err: errs.New(errs.CodeRiskRejected, "refused")})
	l.ObserveCommand(ctx, orders.CommandTiming{Kind: orders.CommandCancel, Elapsed: 7 * time.Millisecond, Err: errors.New("connection reset")})
	l.ObserveCommand(ctx, orders.CommandTiming{Kind: orders.CommandReplace, Elapsed: time.Second})

	got := collectMetrics(t, reader)
	assert.Equal(t, map[string][2]int64{OutcomeOK: {1, 1}, OutcomeRefused: {1, 40}}, histogramByOutcome(t, got["advanced_order_latency"]))
	assert.Equal(t, map[string][2]int64{OutcomeFailed: {1, 7}}, histogramByOutcome(t, got["advanced_cancel_latency"]))
	assert.Equal(t, observability.UnitMilliseconds, got["advanced_order_latency"].Unit)

	_, err = NewOrderLatency(nil)
	assert.Error(t, err)
	_, err = NewOrderLatency(&observability.AdvancedMetrics{})
	assert.Error(t, err)
}

// TestAdvancedVolumes_AreInUSDMinorUnits: the two volumes the collector adds
// in USD minor units say so, as the capital gauges do.
func TestAdvancedVolumes_AreInUSDMinorUnits(t *testing.T) {
	t.Parallel()
	meter, reader := newTestMeter(t)
	m, err := observability.NewAdvancedMetrics(meter)
	require.NoError(t, err)
	m.InternalMatchVolume.Add(context.Background(), 1)
	m.ExternalRouteVolume.Add(context.Background(), 1)
	got := collectMetrics(t, reader)
	assert.Equal(t, observability.UnitUSDMinor, got["advanced_internal_match_volume"].Unit)
	assert.Equal(t, observability.UnitUSDMinor, got["advanced_external_route_volume"].Unit)
}

// TestCommandOutcome_IsTheErrorsStatusClass: nothing is OK, a refusal a client
// is answered with a 4xx is REFUSED -- however it is wrapped -- and anything
// else, an uncoded error included, is FAILED.
func TestCommandOutcome_IsTheErrorsStatusClass(t *testing.T) {
	t.Parallel()
	assert.Equal(t, OutcomeOK, CommandOutcome(nil))
	for _, c := range []errs.Code{errs.CodeValidationFailed, errs.CodeForbidden, errs.CodeNotFound, errs.CodeConflict,
		errs.CodeRiskRejected, errs.CodeInsufficientBuyingPower, errs.CodeCapabilityNotApproved, errs.CodeKillSwitchActive,
		errs.CodeStaleMarketData} {
		assert.Equal(t, OutcomeRefused, CommandOutcome(errs.New(c, "refused")), c)
		assert.Equal(t, OutcomeRefused, CommandOutcome(fmt.Errorf("wrapped: %w", errs.New(c, "refused"))), c)
	}
	for _, err := range []error{errs.New(errs.CodeInternal, "broken"), errs.New(errs.CodeVenueUnavailable, "down"),
		errors.New("not coded"), context.Canceled} {
		assert.Equal(t, OutcomeFailed, CommandOutcome(err), "%v", err)
	}
}
