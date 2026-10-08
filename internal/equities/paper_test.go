package equities

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

func q(n int64) money.Quantity { return money.QuantityFromInt64(n) }

func qp(n int64) *money.Quantity { v := q(n); return &v }

func testVenue(t testing.TB, slippage money.BPS) *PaperVenue {
	t.Helper()
	v, err := NewPaperVenue(config.ProviderModeFake, PaperConfig{Name: "nodal-paper-equities", Calendar: testCalendar(t), SlippageBPS: slippage})
	require.NoError(t, err)
	return v
}

// mark returns a mark of mantissa at scale observed a second before at.
func mark(mantissa int64, scale int32, at time.Time) *Mark {
	return &Mark{Mantissa: q(mantissa), Scale: scale, Source: "test-feed", ObservedAt: at.Add(-time.Second)}
}

func marketOrder(side Side, protection int64, expires time.Time) BrokerOrder {
	return BrokerOrder{ClientOrderID: "01920000-0000-7000-8000-000000000001", Symbol: "SBXA", ExchangeMIC: "XNAS", Side: side,
		Type: OrderMarket, Quantity: q(2_500_000), QuantityDecimals: 6, PriceScale: 2, TickSize: q(1),
		ProtectionPrice: qp(protection), ExpiresAt: expires}
}

func limitOrder(side Side, limit int64, extended bool, expires time.Time) BrokerOrder {
	return BrokerOrder{ClientOrderID: "01920000-0000-7000-8000-000000000002", Symbol: "SBXA", ExchangeMIC: "XNAS", Side: side,
		Type: OrderLimit, Quantity: q(1_000_000), QuantityDecimals: 6, PriceScale: 2, TickSize: q(1),
		LimitPrice: qp(limit), ExtendedHours: extended, ExpiresAt: expires}
}

// TestPaper_MarketOrdersFillAtTheMarkPlusDisclosedSlippage: 10 bps on a mark
// of 150.00 is 150.15 for a buy (rounded up onto the cent grid) and 149.85
// for a sell (rounded down); the fill discloses the mark and the slippage.
func TestPaper_MarketOrdersFillAtTheMarkPlusDisclosedSlippage(t *testing.T) {
	v := testVenue(t, 10)
	ctx := context.Background()
	at := ny(t, 2026, 10, 7, 10, 0, 0)
	exp := ny(t, 2026, 10, 7, 16, 0, 0)
	m := mark(15_000, 2, at)

	r, err := v.PlaceOrder(ctx, PlaceOrderRequest{Order: marketOrder(SideBuy, 15_200, exp), Mark: m, At: at})
	require.NoError(t, err)
	require.Equal(t, ReportFilled, r.Status)
	assert.Equal(t, "15015", r.Fill.Price.String())
	assert.Equal(t, money.BPS(10), r.Fill.SlippageBPS)
	assert.Equal(t, "15000", r.Fill.Mark.Mantissa.String())
	assert.Equal(t, "2500000", r.Fill.Quantity.String(), "a market order fills in full")

	r, err = v.PlaceOrder(ctx, PlaceOrderRequest{Order: marketOrder(SideSell, 14_800, exp), Mark: m, At: at})
	require.NoError(t, err)
	require.Equal(t, ReportFilled, r.Status)
	assert.Equal(t, "14985", r.Fill.Price.String())

	// A mark with more decimals than the grid rounds against the customer.
	r, err = v.PlaceOrder(ctx, PlaceOrderRequest{Order: marketOrder(SideBuy, 15_200, exp), Mark: mark(1_500_001, 4, at), At: at})
	require.NoError(t, err)
	assert.Equal(t, "15016", r.Fill.Price.String(), "150.0001 x 1.001 = 150.1501001, up to 150.16")
}

func TestPaper_ProtectionPriceBoundsMarketOrders(t *testing.T) {
	v := testVenue(t, 10)
	at := ny(t, 2026, 10, 7, 10, 0, 0)
	exp := ny(t, 2026, 10, 7, 16, 0, 0)
	r, err := v.PlaceOrder(context.Background(), PlaceOrderRequest{Order: marketOrder(SideBuy, 15_014, exp), Mark: mark(15_000, 2, at), At: at})
	require.NoError(t, err)
	assert.Equal(t, ReportRejected, r.Status)
	assert.Equal(t, VenueReasonPriceProtection, r.Reason)
	r, err = v.PlaceOrder(context.Background(), PlaceOrderRequest{Order: marketOrder(SideSell, 14_986, exp), Mark: mark(15_000, 2, at), At: at})
	require.NoError(t, err)
	assert.Equal(t, ReportRejected, r.Status)
}

// TestPaper_LimitOrdersFillOnlyWhenTheMarkCrosses: a buy limit at 149.00
// rests while the mark is 150.00 and fills when the mark falls to 148.50, at
// the slipped price (148.65) which is below the limit; a sell is symmetric
// and the limit caps the slipped price.
func TestPaper_LimitOrdersFillOnlyWhenTheMarkCrosses(t *testing.T) {
	v := testVenue(t, 10)
	ctx := context.Background()
	at := ny(t, 2026, 10, 7, 10, 0, 0)
	exp := ny(t, 2026, 10, 7, 16, 0, 0)
	buy := limitOrder(SideBuy, 14_900, false, exp)

	r, err := v.PlaceOrder(ctx, PlaceOrderRequest{Order: buy, Mark: mark(15_000, 2, at), At: at})
	require.NoError(t, err)
	assert.Equal(t, ReportOpen, r.Status)
	assert.Nil(t, r.Fill)

	later := at.Add(time.Hour)
	r, err = v.OrderStatus(ctx, OrderStatusRequest{BrokerOrderID: r.BrokerOrderID, Order: buy, Mark: mark(14_850, 2, later), At: later})
	require.NoError(t, err)
	require.Equal(t, ReportFilled, r.Status)
	assert.Equal(t, "14865", r.Fill.Price.String())

	// Touching the limit crosses; the slipped price is capped by the limit.
	r, err = v.OrderStatus(ctx, OrderStatusRequest{Order: buy, Mark: mark(14_900, 2, later), At: later})
	require.NoError(t, err)
	require.Equal(t, ReportFilled, r.Status)
	assert.Equal(t, "14900", r.Fill.Price.String(), "149.00 slipped is 149.15, capped at the 149.00 limit")

	sell := limitOrder(SideSell, 15_100, false, exp)
	r, err = v.PlaceOrder(ctx, PlaceOrderRequest{Order: sell, Mark: mark(15_000, 2, at), At: at})
	require.NoError(t, err)
	assert.Equal(t, ReportOpen, r.Status)
	r, err = v.OrderStatus(ctx, OrderStatusRequest{Order: sell, Mark: mark(15_200, 2, later), At: later})
	require.NoError(t, err)
	require.Equal(t, ReportFilled, r.Status)
	assert.Equal(t, "15184", r.Fill.Price.String(), "152.00 less 10 bps is 151.848, down to 151.84")

	// No fresh mark: a limit order rests, a market order is refused.
	r, err = v.OrderStatus(ctx, OrderStatusRequest{Order: sell, At: later})
	require.NoError(t, err)
	assert.Equal(t, ReportOpen, r.Status)
	r, err = v.PlaceOrder(ctx, PlaceOrderRequest{Order: marketOrder(SideBuy, 15_200, exp), At: at})
	require.NoError(t, err)
	assert.Equal(t, ReportRejected, r.Status)
	assert.Equal(t, VenueReasonNoMark, r.Reason)
}

// TestPaper_SessionRulesAndExpiry: the venue refuses an order placed outside
// the session it may trade in, honours extended hours for limit orders, and
// expires every DAY order at its expiry.
func TestPaper_SessionRulesAndExpiry(t *testing.T) {
	v := testVenue(t, 0)
	ctx := context.Background()
	pre := ny(t, 2026, 10, 7, 8, 0, 0)
	m := mark(15_000, 2, pre)
	exp := ny(t, 2026, 10, 7, 16, 0, 0)
	r, err := v.PlaceOrder(ctx, PlaceOrderRequest{Order: marketOrder(SideBuy, 15_200, exp), Mark: m, At: pre})
	require.NoError(t, err)
	assert.Equal(t, ReportRejected, r.Status)
	assert.Equal(t, VenueReasonSessionClosed, r.Reason)
	assert.Equal(t, SessionPreMarket, r.Session)

	r, err = v.PlaceOrder(ctx, PlaceOrderRequest{Order: limitOrder(SideBuy, 15_100, false, exp), Mark: m, At: pre})
	require.NoError(t, err)
	assert.Equal(t, ReportRejected, r.Status, "a regular-hours limit order is not accepted pre-market")

	ext := limitOrder(SideBuy, 15_100, true, ny(t, 2026, 10, 7, 20, 0, 0))
	r, err = v.PlaceOrder(ctx, PlaceOrderRequest{Order: ext, Mark: m, At: pre})
	require.NoError(t, err)
	assert.Equal(t, ReportFilled, r.Status, "an extended-hours limit order trades pre-market")

	weekend := ny(t, 2026, 10, 10, 10, 0, 0)
	r, err = v.PlaceOrder(ctx, PlaceOrderRequest{Order: limitOrder(SideBuy, 15_100, true, ny(t, 2026, 10, 10, 20, 0, 0)),
		Mark: mark(15_000, 2, weekend), At: weekend})
	require.NoError(t, err)
	assert.Equal(t, ReportRejected, r.Status)

	at := ny(t, 2026, 10, 7, 16, 0, 0)
	r, err = v.OrderStatus(ctx, OrderStatusRequest{Order: limitOrder(SideBuy, 14_000, false, exp), Mark: mark(15_000, 2, at), At: at})
	require.NoError(t, err)
	assert.Equal(t, ReportExpired, r.Status)
	assert.Equal(t, VenueReasonDayOrderExpired, r.Reason)
}

func TestPaper_CancelRestingOrFillsFirst(t *testing.T) {
	v := testVenue(t, 0)
	ctx := context.Background()
	at := ny(t, 2026, 10, 7, 11, 0, 0)
	exp := ny(t, 2026, 10, 7, 16, 0, 0)
	o := limitOrder(SideBuy, 14_900, false, exp)
	r, err := v.Cancel(ctx, CancelRequest{Order: o, Mark: mark(15_000, 2, at), At: at})
	require.NoError(t, err)
	assert.Equal(t, ReportCancelled, r.Status)
	r, err = v.Cancel(ctx, CancelRequest{Order: o, Mark: mark(14_800, 2, at), At: at})
	require.NoError(t, err)
	assert.Equal(t, ReportFilled, r.Status, "the venue fills a crossing order before it sees the cancel")
	_, err = v.Cancel(ctx, CancelRequest{BrokerOrderID: "someone-else", Order: o, Mark: mark(15_000, 2, at), At: at})
	require.Error(t, err)
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
}

func TestPaper_RefusesMalformedOrdersWithAnError(t *testing.T) {
	v := testVenue(t, 0)
	at := ny(t, 2026, 10, 7, 11, 0, 0)
	exp := ny(t, 2026, 10, 7, 16, 0, 0)
	for name, mutate := range map[string]func(*BrokerOrder){
		"no client id":          func(o *BrokerOrder) { o.ClientOrderID = "" },
		"bad mic":               func(o *BrokerOrder) { o.ExchangeMIC = "nasdaq" },
		"zero quantity":         func(o *BrokerOrder) { o.Quantity = q(0) },
		"limit off grid":        func(o *BrokerOrder) { o.TickSize = q(5); o.LimitPrice = qp(14_901) },
		"market with limit":     func(o *BrokerOrder) { o.Type = OrderMarket },
		"no expiry":             func(o *BrokerOrder) { o.ExpiresAt = time.Time{} },
		"limit with protection": func(o *BrokerOrder) { o.ProtectionPrice = qp(1) },
	} {
		t.Run(name, func(t *testing.T) {
			o := limitOrder(SideBuy, 14_900, false, exp)
			mutate(&o)
			_, err := v.PlaceOrder(context.Background(), PlaceOrderRequest{Order: o, Mark: mark(15_000, 2, at), At: at})
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

// TestProviderMode_LiveIsRefusedUnconditionally: live retail execution does
// not exist in this build, in any environment; fake and sandbox run only in
// LOCAL, TEST and DEV.
func TestProviderMode_LiveIsRefusedUnconditionally(t *testing.T) {
	cfg := PaperConfig{Name: "nodal-paper-equities", Calendar: testCalendar(t), SlippageBPS: 5}
	for _, env := range append(config.Environments(), config.Environment("MOON")) {
		_, err := NewExecutionAdapter(env, config.ProviderModeLive, cfg)
		require.Error(t, err, env)
		assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err), env)
		ee, _ := errs.As(err)
		assert.Equal(t, ReasonLiveExecutionUnsupported, ee.Fields["reason"], env)
	}
	for _, mode := range []config.ProviderMode{config.ProviderModeFake, config.ProviderModeSandbox} {
		for _, env := range config.Environments() {
			a, err := NewExecutionAdapter(env, mode, cfg)
			if env.AllowsFakeProviders() {
				require.NoError(t, err, "%s %s", env, mode)
				assert.Equal(t, mode, a.Mode())
				continue
			}
			require.Error(t, err, "%s %s", env, mode)
			ee, _ := errs.As(err)
			assert.Equal(t, ReasonProviderModeNotAllowed, ee.Fields["reason"])
		}
	}
	_, err := NewExecutionAdapter(config.EnvLocal, "carrier-pigeon", cfg)
	require.Error(t, err)
	_, err = NewPaperVenue(config.ProviderModeLive, cfg)
	require.Error(t, err)
	assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
	_, err = NewPaperVenue(config.ProviderModeFake, PaperConfig{Name: "x", Calendar: testCalendar(t), SlippageBPS: MaxPaperSlippageBPS + 1})
	require.Error(t, err)
	_, err = NewPaperVenue(config.ProviderModeFake, PaperConfig{Name: "x"})
	require.Error(t, err)
}

// TestProp_PaperVenueIsDeterministicAndNeverFillsThroughALimit: the same
// request always gets the same answer, a fill is always the whole order on
// the tick grid, a market fill never passes its protection price and a limit
// fill never passes its limit.
func TestProp_PaperVenueIsDeterministicAndNeverFillsThroughALimit(t *testing.T) {
	v := testVenue(t, 25)
	ctx := context.Background()
	exp := ny(t, 2026, 10, 7, 20, 0, 0)
	rapid.Check(t, func(rt *rapid.T) {
		at := ny(t, 2026, 10, 7, 4, 0, 0).Add(time.Duration(rapid.IntRange(0, 16*60-1).Draw(rt, "minute")) * time.Minute)
		side := rapid.SampledFrom(AllSides()).Draw(rt, "side")
		tick := q(rapid.Int64Range(1, 5).Draw(rt, "tick"))
		m := mark(rapid.Int64Range(1, 10_000_000).Draw(rt, "mark"), int32(rapid.IntRange(0, 6).Draw(rt, "markScale")), at)
		var o BrokerOrder
		bound := q(rapid.Int64Range(1, 2_000).Draw(rt, "ticks")).Mul(tick)
		if rapid.Bool().Draw(rt, "market") {
			o = marketOrder(side, 0, ny(t, 2026, 10, 7, 16, 0, 0))
			o.ProtectionPrice = &bound
		} else {
			o = limitOrder(side, 0, rapid.Bool().Draw(rt, "extended"), exp)
			o.LimitPrice = &bound
		}
		o.TickSize = tick
		req := PlaceOrderRequest{Order: o, Mark: m, At: at}
		a, errA := v.PlaceOrder(ctx, req)
		b, errB := v.PlaceOrder(ctx, req)
		if (errA == nil) != (errB == nil) || (errA == nil && !sameReport(a, b)) {
			rt.Fatalf("the same request got two answers: %+v / %+v", a, b)
		}
		if errA != nil || a.Status != ReportFilled {
			return
		}
		f := a.Fill
		if !f.Quantity.Equal(o.Quantity) || !onGrid(f.Price, tick) || !f.Price.IsPositive() {
			rt.Fatalf("bad fill %+v", f)
		}
		if (side == SideBuy && f.Price.Cmp(bound) > 0) || (side == SideSell && f.Price.Cmp(bound) < 0) {
			rt.Fatalf("%s %s filled at %s through %s", side, o.Type, f.Price, bound)
		}
	})
}

func sameReport(a, b OrderReport) bool {
	if a.Status != b.Status || a.Reason != b.Reason || a.Session != b.Session || !a.At.Equal(b.At) || (a.Fill == nil) != (b.Fill == nil) {
		return false
	}
	if a.Fill == nil {
		return true
	}
	return a.Fill.Price.Equal(b.Fill.Price) && a.Fill.Quantity.Equal(b.Fill.Quantity) && a.Fill.VenueFillID == b.Fill.VenueFillID
}

// TestProp_SlippedPriceIsTheMarkMovedAgainstTheCustomerByLessThanATick: the
// buy price is at least the exact slipped value and less than one tick
// above it; the sell price at most and less than one tick below.
func TestProp_SlippedPriceIsTheMarkMovedAgainstTheCustomerByLessThanATick(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		m := Mark{Mantissa: q(rapid.Int64Range(1, 1_000_000_000).Draw(rt, "m")), Scale: int32(rapid.IntRange(0, 8).Draw(rt, "s")),
			Source: "x", ObservedAt: time.Unix(1, 0)}
		bps := money.BPS(rapid.Int64Range(0, 1_000).Draw(rt, "bps"))
		scale := int32(rapid.IntRange(0, 6).Draw(rt, "ps"))
		tick := q(rapid.Int64Range(1, 100).Draw(rt, "tick"))
		for _, side := range AllSides() {
			p, err := slippedPrice(m, side, bps, scale, tick)
			if err != nil {
				rt.Fatal(err)
			}
			factor := int64(money.OneHundredPercent + bps)
			if side == SideSell {
				factor = int64(money.OneHundredPercent - bps)
			}
			// exact = m * factor * 10^scale / (10_000 * 10^m.Scale); compare p*den with num.
			num := m.Mantissa.Mul(q(factor)).Mul(pow10(int(scale)))
			den := bpsDivisor.Mul(pow10(int(m.Scale)))
			pd := p.Mul(den)
			tickDen := tick.Mul(den)
			switch side {
			case SideBuy:
				if pd.Cmp(num) < 0 || pd.Sub(num).Cmp(tickDen) >= 0 {
					rt.Fatalf("buy %s not within one tick above exact", p)
				}
			case SideSell:
				if pd.Cmp(num) > 0 || num.Sub(pd).Cmp(tickDen) >= 0 {
					rt.Fatalf("sell %s not within one tick below exact", p)
				}
			}
		}
	})
}

func TestRescalePrice(t *testing.T) {
	p, err := rescalePrice(q(15), 1, 2)
	require.NoError(t, err)
	assert.Equal(t, "150", p.String())
	p, err = rescalePrice(q(15000), 3, 2)
	require.NoError(t, err)
	assert.Equal(t, "1500", p.String())
	_, err = rescalePrice(q(15001), 3, 2)
	require.Error(t, err, "a limit with more decimals than the instrument quotes is refused, not rounded")
}
