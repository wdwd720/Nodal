package observability

import (
	"context"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func newTestMeter(t *testing.T) (metric.Meter, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	return mp.Meter("test"), reader
}

func collect(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
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

func sortedKeys(m map[string]metricdata.Metrics) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sumValue(t *testing.T, m metricdata.Metrics) int64 {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok, "%s is not an int64 sum", m.Name)
	var total int64
	for _, dp := range sum.DataPoints {
		total += dp.Value
	}
	return total
}

func TestFinancialMetrics_InstrumentNames(t *testing.T) {
	t.Parallel()
	meter, reader := newTestMeter(t)
	fm, err := NewFinancialMetrics(meter)
	require.NoError(t, err)
	ctx := context.Background()
	fm.LedgerPostingErrors.Add(ctx, 1)
	fm.ReservationConflicts.Add(ctx, 1)
	fm.UnknownSubmissions.Add(ctx, 1)
	fm.ReconciliationMismatches.Add(ctx, 1)
	fm.OldestUnresolvedMismatch.Record(ctx, 42)
	fm.ProviderDuplicateEvents.Add(ctx, 1)
	fm.DuplicateCommandRejections.Add(ctx, 1)
	fm.NegativeDeficitAccounts.Record(ctx, 3)
	fm.RiskRejections.Add(ctx, 1)
	fm.CapabilityGateRejections.Add(ctx, 1)
	fm.VerificationPasses.Add(ctx, 1)

	got := collect(t, reader)
	assert.Equal(t, []string{
		"capability_gate_rejections", "duplicate_command_rejections", "ledger_posting_errors",
		"negative_deficit_accounts", "oldest_unresolved_mismatch", "provider_duplicate_events",
		"reconciliation_mismatches", "reservation_conflicts", "risk_rejections", "unknown_submissions",
		"verification_passes",
	}, sortedKeys(got))
	assert.Equal(t, UnitSeconds, got["oldest_unresolved_mismatch"].Unit)
	g, ok := got["oldest_unresolved_mismatch"].Data.(metricdata.Gauge[int64])
	require.True(t, ok)
	assert.Equal(t, int64(42), g.DataPoints[0].Value)
	_, ok = got["negative_deficit_accounts"].Data.(metricdata.Gauge[int64])
	assert.True(t, ok)
	assert.Equal(t, int64(1), sumValue(t, got["ledger_posting_errors"]))
	assert.NotEmpty(t, got["risk_rejections"].Description)
}

func TestExecutionMetrics_InstrumentNames(t *testing.T) {
	t.Parallel()
	meter, reader := newTestMeter(t)
	em, err := NewExecutionMetrics(meter)
	require.NoError(t, err)
	ctx := context.Background()
	for _, h := range []metric.Int64Histogram{
		em.QuoteLatency, em.QuoteAgeAtSubmit, em.BuildLatency, em.SimulationLatency, em.SignLatency,
		em.SubmitLatency, em.ConfirmationLatency, em.FinalityLatency, em.Slippage, em.PriceImpact,
	} {
		h.Record(ctx, 7)
	}
	em.ExecutionFailureRate.Observe(ctx, true)
	em.ExecutionFailureRate.Observe(ctx, false)
	em.ExecutionFailureRate.Observe(ctx, false)
	em.UnknownSubmissionRate.Observe(ctx, false)
	em.UnknownSubmissionRate.Observe(ctx, true)

	got := collect(t, reader)
	assert.Equal(t, []string{
		"build_latency", "confirmation_latency", "execution_failure_rate_attempts", "execution_failure_rate_failures",
		"finality_latency", "price_impact", "quote_age_at_submit", "quote_latency", "sign_latency",
		"simulation_latency", "slippage", "submit_latency", "unknown_submission_rate_submissions",
		"unknown_submission_rate_unknown",
	}, sortedKeys(got))
	assert.Equal(t, UnitMilliseconds, got["quote_latency"].Unit)
	assert.Equal(t, UnitBasisPoints, got["slippage"].Unit)
	assert.Equal(t, UnitBasisPoints, got["price_impact"].Unit)
	h, ok := got["slippage"].Data.(metricdata.Histogram[int64])
	require.True(t, ok)
	assert.Equal(t, int64(7), h.DataPoints[0].Sum)
	assert.Equal(t, int64(3), sumValue(t, got["execution_failure_rate_attempts"]))
	assert.Equal(t, int64(1), sumValue(t, got["execution_failure_rate_failures"]))
	assert.Equal(t, int64(2), sumValue(t, got["unknown_submission_rate_submissions"]))
	assert.Equal(t, int64(1), sumValue(t, got["unknown_submission_rate_unknown"]))

	assert.NotPanics(t, func() { RatePair{}.Observe(ctx, true) }, "zero value is inert")
}

func TestAgentMetrics_InstrumentNames(t *testing.T) {
	t.Parallel()
	meter, reader := newTestMeter(t)
	am, err := NewAgentMetrics(meter)
	require.NoError(t, err)
	ctx := context.Background()
	am.StrategyEvaluations.Add(ctx, 1)
	am.IntentProposals.Add(ctx, 1)
	am.RiskRejects.Add(ctx, 1)
	am.PredictionCount.Add(ctx, 1)
	am.ModelCost.Record(ctx, 125) // USD minor units: $1.25
	am.ToolCost.Record(ctx, 3)
	am.DataFreshness.Record(ctx, 900)
	am.DecisionLatency.Record(ctx, 40)
	am.AgentPauses.Add(ctx, 1)
	am.PolicyViolations.Add(ctx, 1)

	got := collect(t, reader)
	assert.Equal(t, []string{
		"agent_pauses", "data_freshness", "decision_latency", "intent_proposals", "model_cost",
		"policy_violations", "prediction_count", "risk_rejects", "strategy_evaluations", "tool_cost",
	}, sortedKeys(got))
	assert.Equal(t, UnitUSDMinor, got["model_cost"].Unit)
	assert.Equal(t, UnitUSDMinor, got["tool_cost"].Unit)
	assert.Equal(t, UnitMilliseconds, got["data_freshness"].Unit)
	assert.Equal(t, UnitMilliseconds, got["decision_latency"].Unit)
	h, ok := got["model_cost"].Data.(metricdata.Histogram[int64])
	require.True(t, ok, "costs are int64 histograms, never floats")
	assert.Equal(t, int64(125), h.DataPoints[0].Sum)
}

func TestMetrics_NilMeter(t *testing.T) {
	t.Parallel()
	_, err := NewFinancialMetrics(nil)
	assert.ErrorIs(t, err, ErrNilMeter)
	_, err = NewExecutionMetrics(nil)
	assert.ErrorIs(t, err, ErrNilMeter)
	_, err = NewAgentMetrics(nil)
	assert.ErrorIs(t, err, ErrNilMeter)
}

func TestSafeAttrs(t *testing.T) {
	t.Parallel()
	in := []attribute.KeyValue{
		attribute.String("provider", "acme"),
		attribute.String("account_id", "acc-1"),
		attribute.String("user_id", "u-1"),
		attribute.String("venue", "dex"),
		attribute.String("intent_id", "i-1"),
		attribute.String("order_id", "o-1"),
		attribute.String("wallet", "9WzD..."),
		attribute.String("signature", "5x..."),
		attribute.String("Account-ID", "acc-2"),
		attribute.String("request_id", "r-1"),
		attribute.String("wallet.address", "x"),
		attribute.Int("attempt", 2),
		attribute.String("reason", "risk"),
	}
	got := SafeAttrs(in...)
	assert.Equal(t, []attribute.KeyValue{
		attribute.String("provider", "acme"),
		attribute.String("venue", "dex"),
		attribute.Int("attempt", 2),
		attribute.String("reason", "risk"),
	}, got, "high-cardinality keys dropped, order preserved")
	set := SafeAttrSet(in...)
	assert.Equal(t, 4, set.Len())
	assert.Empty(t, SafeAttrs())

	for _, k := range []string{"account_id", "user_id", "intent_id", "order_id", "wallet", "signature", "ACCOUNT_ID", "trace_id", "email"} {
		assert.True(t, IsHighCardinalityKey(k), k)
	}
	for _, k := range []string{"provider", "venue", "chain", "reason", "status", "strategy"} {
		assert.False(t, IsHighCardinalityKey(k), k)
	}

	// WithSafeAttrs applies the filter at record time.
	meter, reader := newTestMeter(t)
	fm, err := NewFinancialMetrics(meter)
	require.NoError(t, err)
	fm.RiskRejections.Add(context.Background(), 1, WithSafeAttrs(attribute.String("account_id", "acc"), attribute.String("reason", "limit")))
	m := collect(t, reader)["risk_rejections"]
	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok, "%T", m.Data)
	require.Len(t, sum.DataPoints, 1)
	keys := []string{}
	for _, kv := range sum.DataPoints[0].Attributes.ToSlice() {
		keys = append(keys, string(kv.Key))
	}
	assert.Equal(t, []string{"reason"}, keys)
}
