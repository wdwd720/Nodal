//go:build integration

package advancedmetrics

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
)

// histogramCounts returns how many measurements a histogram holds per value
// of its outcome attribute.
func (f *fixture) histogramCounts(name string) map[string]int64 {
	f.t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(f.t, f.reader.Collect(f.ctx, &rm))
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[int64])
			require.True(f.t, ok, "%s is not an int64 histogram", name)
			for _, dp := range h.DataPoints {
				v, _ := dp.Attributes.Value(attribute.Key("outcome"))
				out[v.AsString()] += int64(dp.Count) // #nosec G115 -- a test's handful of measurements
			}
		}
	}
	return out
}

// TestIntegration_TheOrderPathRecordsEachSubmitAndCancelLatencyOnce: a
// pipeline built with the order latency observer records each customer
// submit once -- one carried out, one refused before any order was recorded
// -- and each cancel once, with its outcome; a pipeline built without one
// times nothing.
func TestIntegration_TheOrderPathRecordsEachSubmitAndCancelLatencyOnce(t *testing.T) {
	f := newFixture(t)
	latency, err := NewOrderLatency(f.metrics)
	require.NoError(t, err)
	pipe, err := orders.NewPipeline(f.orders, testDB, orders.WithObserver(latency))
	require.NoError(t, err)
	inst, eth := f.spotBook()
	maker := f.customer()
	f.fund(maker.account, eth.ID, units(2, eth.Decimals))

	resting, err := pipe.Submit(f.ctx, f.limit(maker, inst, eth, orders.SideSell, "2500.00", 1))
	require.NoError(t, err)
	require.Equal(t, orders.StatusOpen, resting.Order.Status)
	_, err = pipe.Submit(f.ctx, f.limit(maker, inst, eth, orders.SideSell, "2500.001", 1))
	require.Error(t, err)
	assert.Equal(t, orders.ReasonPricePrecision, orders.ReasonOf(err), "refused before any order is recorded")
	cancelled, err := pipe.Cancel(f.ctx, orders.CancelRequest{OrderID: resting.Order.ID, AccountID: maker.account,
		ActorType: security.ActorUser, ActorID: maker.user.String()})
	require.NoError(t, err)
	require.Equal(t, orders.StatusCancelled, cancelled.Order.Status)

	assert.Equal(t, map[string]int64{OutcomeOK: 1, OutcomeRefused: 1}, f.histogramCounts("advanced_order_latency"))
	assert.Equal(t, map[string]int64{OutcomeOK: 1}, f.histogramCounts("advanced_cancel_latency"))

	_, err = f.pipeline.Submit(f.ctx, f.limit(maker, inst, eth, orders.SideSell, "2600.00", 1))
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{OutcomeOK: 1, OutcomeRefused: 1}, f.histogramCounts("advanced_order_latency"),
		"a pipeline without the observer records nothing")
}

// TestIntegration_FillAndRejectionRatesCountEachBookOrderOnce: every book
// order is counted once when it is recorded, once when it first fills --
// maker and taker alike, and never again on a later fill -- and once when it
// is rejected; a request refused before any order existed is in neither
// pair. Both pairs share their denominator.
func TestIntegration_FillAndRejectionRatesCountEachBookOrderOnce(t *testing.T) {
	f := newFixture(t)
	inst, eth := f.spotBook()
	maker, taker, again, broke := f.customer(), f.customer(), f.customer(), f.customer()
	f.fund(maker.account, eth.ID, units(2, eth.Decimals))
	f.fund(taker.account, f.usd.ID, units(5_000, 6))
	f.fund(again.account, f.usd.ID, units(5_000, 6))
	c := f.collector()
	require.Nil(t, f.collect(c).Counted)

	// A resting sell of 2, a buy of 1 against it (both orders' first fill),
	// a buy its account cannot pay for (rejected), and a price the book
	// cannot express (refused before any order).
	_, err := f.pipeline.Submit(f.ctx, f.limit(maker, inst, eth, orders.SideSell, "2500.00", 2))
	require.NoError(t, err)
	out, err := f.pipeline.Submit(f.ctx, f.limit(taker, inst, eth, orders.SideBuy, "2500.00", 1))
	require.NoError(t, err)
	require.Len(t, out.Trades, 1)
	refused, err := f.pipeline.Submit(f.ctx, f.limit(broke, inst, eth, orders.SideBuy, "2500.00", 1))
	require.Error(t, err)
	require.Equal(t, orders.StatusRejected, refused.Order.Status)
	_, err = f.pipeline.Submit(f.ctx, f.limit(taker, inst, eth, orders.SideBuy, "2500.001", 1))
	require.Error(t, err)

	s1 := f.collect(c)
	require.NotNil(t, s1.Counted)
	customer := string(orders.OwnerCustomer)
	assert.Equal(t, map[string]int64{customer: 3}, s1.Counted.BookOrders, "the maker, the taker and the rejected order")
	assert.Equal(t, map[string]int64{customer: 2}, s1.Counted.BookOrdersFilled, "the maker's and the taker's first fills")
	assert.Equal(t, map[string]int64{customer: 1}, s1.Counted.BookOrdersRejected)

	// The maker's second fill is not a first fill; the new taker's is.
	out, err = f.pipeline.Submit(f.ctx, f.limit(again, inst, eth, orders.SideBuy, "2500.00", 1))
	require.NoError(t, err)
	require.Len(t, out.Trades, 1)
	s2 := f.collect(c)
	require.NotNil(t, s2.Counted)
	assert.Equal(t, map[string]int64{customer: 1}, s2.Counted.BookOrders)
	assert.Equal(t, map[string]int64{customer: 1}, s2.Counted.BookOrdersFilled, "only the new taker's first fill")
	assert.Empty(t, s2.Counted.BookOrdersRejected)

	s3 := f.collect(c)
	require.NotNil(t, s3.Counted)
	assert.Empty(t, s3.Counted.BookOrders, "nothing new: nothing counted again")
	assert.Empty(t, s3.Counted.BookOrdersFilled)

	for name, want := range map[string]int64{
		"advanced_fill_rate_orders": 4, "advanced_fill_rate_filled": 3,
		"advanced_rejection_rate_orders": 4, "advanced_rejection_rate_rejected": 1,
	} {
		assert.Equal(t, want, f.counter(name, "owner_type", customer), name)
	}
}

// TestIntegration_ExternalRouteVolumeIsThePaperVenuesNotional: a fill on the
// order book's paper venue -- the stand-in for an external venue -- counts its
// reference-mark notional, valued as the internal match volume is (the
// sandbox dollar at par), once.
func TestIntegration_ExternalRouteVolumeIsThePaperVenuesNotional(t *testing.T) {
	f := newFixture(t)
	inst, eth := f.spotBook()
	venue, err := orders.NewPaperVenue(orders.PaperConfig{
		Clock: f.clk, Ledger: f.ledger, Capital: f.capital, Intents: f.intents, Effects: f.effects, Authorizer: f.authz,
		Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(), Router: f.router, Instruments: f.instr, Assets: f.assets,
		Prices: f.prices, KillSwitches: f.kills, FeeLedger: f.fees,
	})
	require.NoError(t, err)
	schedule := fees.ZeroSchedule(f.usd.ID, 6)
	schedule.Version = "advancedmetrics-paper-zero"
	f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := venue.EnsureSchedule(ctx, tx, inst, schedule, "advancedmetrics integration test",
			econ.Actor{Type: security.ActorOperator, ID: uuid.NewString()})
		return err
	})
	buyer := f.customer()
	f.fund(buyer.account, f.usd.ID, units(10_000, 6))
	c := f.collector()
	require.Nil(t, f.collect(c).Counted)

	filled, err := venue.Submit(f.ctx, testDB, orders.PaperOrderRequest{InstrumentID: inst, AccountID: buyer.account,
		Side: orders.SideBuy, Quantity: units(2, eth.Decimals), IdempotencyKey: "advancedmetrics-paper-" + uuid.NewString(),
		ActorType: security.ActorUser, ActorID: buyer.user.String()})
	require.NoError(t, err)
	require.NotNil(t, filled.Fill)
	require.Equal(t, "5000000000", filled.Fill.NotionalAtReference.String(), "2 units at the 2500.00 mark, in sandbox dollar base units")

	s1 := f.collect(c)
	require.NotNil(t, s1.Counted)
	assert.Equal(t, int64(5000_00), s1.Counted.ExternalRouteVolume, "the reference notional at par, in USD minor units")
	assert.Zero(t, s1.Counted.UnvaluedRouteFills)
	assert.Zero(t, s1.Counted.InternalMatchVolume, "the internal book matched nothing")
	assert.Empty(t, s1.Counted.BookOrders, "a paper fill is no book order")
	s2 := f.collect(c)
	require.NotNil(t, s2.Counted)
	assert.Zero(t, s2.Counted.ExternalRouteVolume, "counted once")
	assert.Equal(t, int64(5000_00), f.counter("advanced_external_route_volume", "", ""))
	assert.Zero(t, f.counter("advanced_unvalued_route_fills", "", ""))
}
