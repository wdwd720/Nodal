//go:build integration

package equities

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
)

// TestIntegration_BuyIsPaidAtTradeDateAndSharesSettleTPlusOne: a market buy
// of 2.5 shares at a 150.00 mark fills at 150.15 (10 bps disclosed
// slippage). On the trade date the buyer pays the notional, the 20 bps
// spread, the 10 bps fee and the 3 bps venue pass-through out of WALLET and
// the shares wait in UNSETTLED_RECEIVABLE; on the next trading day
// EQUITY_SETTLEMENT moves them into WALLET and the intent completes.
func TestIntegration_BuyIsPaidAtTradeDateAndSharesSettleTPlusOne(t *testing.T) {
	w := newWorld(t)
	acct := w.account()
	l := w.list(1, 1)
	w.fund(acct, w.cash.ID, 10_000_000_000)
	w.mark(l, 15_000)

	o := w.trade(acct, l, market(SideBuy, 2_500_000))
	require.Equal(t, OrderFilled, o.Status)
	assert.Equal(t, "15166", o.ProtectionPrice.String(), "150.15 indicative moved 1% against the buyer, up to the cent")
	f, ok, err := w.svc.FillForOrder(w.ctx, testDB, o.ID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "15015", f.Price.String())
	assert.Equal(t, money.BPS(10), f.SlippageBPS)
	assert.Equal(t, "15000", f.Mark.Mantissa.String())
	assert.Equal(t, NewDate(2026, 10, 7), f.TradeDate)
	assert.Equal(t, NewDate(2026, 10, 8), f.SettlementDate)
	// 2.5 × 150.15 = 375.375000; spread 20 bps = 0.750750; fee 10 bps =
	// 0.375375; venue 3 bps = 0.1126125, down to 0.112612.
	assert.Equal(t, "375375000", f.Charges.Notional.String())
	assert.Equal(t, "750750", f.Charges.Spread.String())
	assert.Equal(t, "375375", f.Charges.PlatformFees.String())
	assert.Equal(t, "112612", f.Charges.VenueFees.String())
	assert.Equal(t, "376613737", f.Charges.Total.String())

	assert.Equal(t, "9623386263", w.cust(acct, ledger.CodeWallet, w.cash.ID).String(), "the buyer pays on the trade date")
	assert.Equal(t, "375375000", w.cust(acct, ledger.CodeTradingOutflow, w.cash.ID).String(), "the notional leaves to the venue")
	assert.Equal(t, "750750", w.cust(acct, ledger.CodeFeesSpread, w.cash.ID).String())
	assert.Equal(t, "375375", w.cust(acct, ledger.CodeFeesPlatform, w.cash.ID).String())
	assert.Equal(t, "112612", w.cust(acct, ledger.CodeFeesVenue, w.cash.ID).String())
	assert.Equal(t, "2500000", w.cust(acct, ledger.CodeUnsettledReceivable, l.ShareAsset.ID).String())
	assert.Equal(t, "2500000", w.cust(acct, ledger.CodeTradingInflow, l.ShareAsset.ID).String())
	assert.True(t, w.cust(acct, ledger.CodeWallet, l.ShareAsset.ID).IsZero(), "unsettled shares are not in WALLET")
	assert.Equal(t, "750750", w.platform(ledger.CodeSpreadRevenue).String())
	assert.Equal(t, "375375", w.platform(ledger.CodePlatformFeeRevenue).String())
	assert.Equal(t, "1126125", w.platform(ledger.CodePlatformFeeReceivable).String())

	res := w.reservation(o)
	assert.Equal(t, capital.ReservationConsumed, res.Status)
	assert.Equal(t, "376613737", res.ConsumedQuantity.String())
	assert.Equal(t, "382296945", res.Quantity.String(), "sized at the 151.66 protection price, slippage reserve included")
	assert.Equal(t, econ.StatusExecuting, w.intent(o.IntentID).Status)

	tx, err := w.ledger.Transaction(w.ctx, testDB, f.JournalTransactionID)
	require.NoError(t, err)
	assert.Equal(t, ledger.KindSimTrade, tx.Kind)
	effect, ok, err := w.effects.GetByKey(w.ctx, testDB, tradeKey(o.ID))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, econ.EffectApplied, effect.Status)
	assert.Equal(t, f.JournalTransactionID, *effect.JournalTransactionID)
	assert.NotEmpty(t, effect.EvidenceRef)

	w.settleDue()
	st, ok, err := w.svc.SettlementForFill(w.ctx, testDB, f.ID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.False(t, st.Settled(), "nothing settles on the trade date")
	assert.Equal(t, NewDate(2026, 10, 8), st.DueDate)
	w.at(2026, 10, 8, 9, 0)
	settled := w.settleDue()
	mine := 0
	for _, s := range settled {
		if s.FillID == f.ID {
			mine++
			assert.True(t, s.Settled())
		}
	}
	assert.Equal(t, 1, mine)
	assert.Equal(t, "2500000", w.cust(acct, ledger.CodeWallet, l.ShareAsset.ID).String())
	assert.True(t, w.cust(acct, ledger.CodeUnsettledReceivable, l.ShareAsset.ID).IsZero())
	assert.Equal(t, econ.StatusCompleted, w.intent(o.IntentID).Status)
	st, ok, err = w.svc.SettlementForFill(w.ctx, testDB, f.ID)
	require.NoError(t, err)
	require.True(t, ok)
	stx, err := w.ledger.Transaction(w.ctx, testDB, *st.JournalTransactionID)
	require.NoError(t, err)
	assert.Equal(t, ledger.KindEquitySettlement, stx.Kind)
	for _, s := range w.settleDue() {
		assert.NotEqual(t, f.ID, s.FillID, "a settlement settles once")
	}
	assert.Equal(t, "2500000", w.cust(acct, ledger.CodeWallet, l.ShareAsset.ID).String())
}

// TestIntegration_FractionalSharesTradeAtSixDecimalsAndBelowLotIsRefused:
// orders and positions are exact base units at the listing's six decimals;
// a quantity that is not a whole number of lots, or below the minimum, or
// too small to be worth the minimum notional, is refused and recorded.
func TestIntegration_FractionalSharesTradeAtSixDecimalsAndBelowLotIsRefused(t *testing.T) {
	w := newWorld(t)
	acct := w.account()
	w.fund(acct, w.cash.ID, 10_000_000_000)
	frac := w.list(1, 1)
	w.mark(frac, 15_000)

	o := w.trade(acct, frac, limit(SideBuy, 123_457, 15_100))
	require.Equal(t, OrderFilled, o.Status)
	f, _, err := w.svc.FillForOrder(w.ctx, testDB, o.ID)
	require.NoError(t, err)
	assert.Equal(t, "123457", f.Quantity.String(), "0.123457 shares")
	// 0.123457 × 150.15 = 18.53706855, rounded down for the buyer.
	assert.Equal(t, "18537068", f.Charges.Notional.String())
	w.at(2026, 10, 8, 10, 0)
	w.settleDue()
	assert.Equal(t, "123457", w.cust(acct, ledger.CodeWallet, frac.ShareAsset.ID).String())

	w.mark(frac, 15_000)
	sell := w.trade(acct, frac, market(SideSell, 23_457))
	require.Equal(t, OrderFilled, sell.Status)
	assert.Equal(t, "100000", w.cust(acct, ledger.CodeWallet, frac.ShareAsset.ID).String(), "0.1 share left, exactly")

	refused := func(l Listing, s orderSpec, code RejectionCode) {
		t.Helper()
		in := w.submit(w.request(acct, l, s))
		o := w.accept(in.ID)
		require.Equal(t, OrderRejected, o.Status)
		assert.Equal(t, code, o.RejectionCode)
		assert.Nil(t, o.ReservationID, "nothing is reserved for a refused order")
		got := w.intent(in.ID)
		assert.Equal(t, econ.StatusRejected, got.Status)
		assert.Equal(t, string(code), got.RejectionCode)
		ee, ok := errs.As(o.Refusal())
		require.True(t, ok)
		assert.Equal(t, string(code), ee.Fields["rejection_code"])
	}
	coarse := w.listKind(instruments.TypeETF, 10_000, 10_000) // an ETF in 0.01-share lots
	assert.Equal(t, instruments.TypeETF, coarse.Instrument.Type)
	assert.Equal(t, uint8(6), coarse.ShareAsset.Decimals)
	assert.Equal(t, assets.KindSimulated, coarse.ShareAsset.Kind)
	w.mark(coarse, 15_000)
	refused(coarse, limit(SideBuy, 5_000, 15_100), RejectNotLotMultiple)
	refused(coarse, limit(SideBuy, 15_000, 15_100), RejectNotLotMultiple)
	etf := w.trade(acct, coarse, limit(SideBuy, 20_000, 15_100))
	require.Equal(t, OrderFilled, etf.Status, "two lots of the ETF trade")
	assert.Equal(t, "20000", w.cust(acct, ledger.CodeUnsettledReceivable, coarse.ShareAsset.ID).String())
	minimum := w.list(1_000, 100_000)
	w.mark(minimum, 15_000)
	refused(minimum, limit(SideBuy, 50_000, 15_100), RejectBelowMinimum)
	// One millionth of a share at 151.66 is 0.000151 dollars: below the
	// one-dollar minimum notional.
	refused(frac, market(SideBuy, 1), RejectNotionalTooSmall)
	// A limit price with more decimals than the instrument quotes.
	s := limit(SideBuy, 1_000_000, 0)
	req := w.request(acct, frac, s)
	req.LimitPrice = &PricePoint{Mantissa: q(150_005), Scale: 3}
	o = w.accept(w.submit(req).ID)
	require.Equal(t, OrderRejected, o.Status)
	assert.Equal(t, RejectLimitPriceInvalid, o.RejectionCode)
}

// TestIntegration_TradingHaltBlocksNewOrders: an INSTRUMENT_HALT kill switch
// and an instrument status of HALTED each refuse every new order on the
// instrument, buys and sells, through the risk kernel, and the refusal is
// recorded with its decision.
func TestIntegration_TradingHaltBlocksNewOrders(t *testing.T) {
	w := newWorld(t)
	acct := w.account()
	w.fund(acct, w.cash.ID, 10_000_000_000)

	halted := w.list(1, 1)
	w.mark(halted, 15_000)
	w.fund(acct, halted.ShareAsset.ID, 5_000_000)
	activateHalt(t, w, halted.Instrument.ID)
	for _, s := range []orderSpec{market(SideBuy, 1_000_000), market(SideSell, 1_000_000), limit(SideSell, 1_000_000, 16_000)} {
		in := w.submit(w.request(acct, halted, s))
		o := w.accept(in.ID)
		require.Equal(t, OrderRejected, o.Status, "%s %s", s.side, s.typ)
		assert.Equal(t, RejectRisk, o.RejectionCode)
		assert.Contains(t, o.RejectionReasons, risk.ReasonKillSwitch)
		require.NotNil(t, o.RiskDecisionID)
		assert.Equal(t, 1, w.count(`SELECT count(*) FROM advanced_risk_decisions WHERE intent_id = $1 AND verdict = 'REJECT'`, in.ID))
	}

	status := w.list(1, 1)
	w.mark(status, 15_000)
	w.fund(acct, status.ShareAsset.ID, 5_000_000)
	resting := w.trade(acct, status, limit(SideBuy, 1_000_000, 14_900))
	require.Equal(t, OrderOpen, resting.Status)
	transitionInstrument(t, w, status.Instrument.ID, assets.StatusHalted)
	// Nothing executes during a halt, even a resting order the mark crosses.
	w.at(2026, 10, 7, 11, 0)
	w.mark(status, 14_000)
	assert.Equal(t, OrderOpen, w.poll(resting).Status)
	for _, s := range []orderSpec{market(SideBuy, 1_000_000), market(SideSell, 1_000_000)} {
		o := w.accept(w.submit(w.request(acct, status, s)).ID)
		require.Equal(t, OrderRejected, o.Status)
		assert.Contains(t, o.RejectionReasons, risk.ReasonInstrumentNotTradable)
	}
	// Lifting the halt lets orders through again, and the resting one fills.
	transitionInstrument(t, w, status.Instrument.ID, assets.StatusActive)
	o := w.trade(acct, status, market(SideBuy, 1_000_000))
	assert.Equal(t, OrderFilled, o.Status)
	assert.Equal(t, OrderFilled, w.poll(resting).Status)
}

// TestIntegration_AVenueRejectionReleasesTheReservation: a market order whose
// market moved beyond its protection price before it reached the venue is
// refused there; the order is REJECTED with the venue's reason, nothing is
// posted, the reservation is handed back and the intent is rejected.
func TestIntegration_AVenueRejectionReleasesTheReservation(t *testing.T) {
	w := newWorld(t)
	acct := w.account()
	w.fund(acct, w.cash.ID, 10_000_000_000)
	l := w.list(1, 1)
	w.mark(l, 15_000)
	o := w.accept(w.submit(w.request(acct, l, market(SideBuy, 1_000_000))).ID)
	require.Equal(t, OrderAccepted, o.Status)
	w.at(2026, 10, 7, 10, 1)
	w.mark(l, 16_000)
	o = w.place(o)
	require.Equal(t, OrderRejected, o.Status)
	assert.Equal(t, RejectVenue, o.RejectionCode)
	assert.Equal(t, []string{VenueReasonPriceProtection}, o.RejectionReasons)
	assert.NotEmpty(t, o.BrokerOrderID)
	assert.Equal(t, capital.ReservationReleased, w.reservation(o).Status)
	assert.Equal(t, econ.StatusRejected, w.intent(o.IntentID).Status)
	assert.Equal(t, string(RejectVenue), w.intent(o.IntentID).RejectionCode)
	effect, _, err := w.effects.GetByKey(w.ctx, testDB, submitKey(o.ID))
	require.NoError(t, err)
	assert.Equal(t, econ.EffectFailed, effect.Status)
	assert.Equal(t, "10000000000", w.cust(acct, ledger.CodeWallet, w.cash.ID).String(), "nothing was paid")
	assert.Zero(t, w.count(`SELECT count(*) FROM equity_fills WHERE order_id = $1`, o.ID))
	err = w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return w.svc.MarkSubmissionUnknown(ctx, tx, o.ID, "too late")
	})
	require.Error(t, err, "a definitive answer is not unknown")
}

// TestIntegration_ARestingBuyDoesNotExecuteOnceTheInstrumentOnlyCloses: an
// order already at the venue is held to what a new order would face: once
// the instrument is CLOSE_ONLY a resting buy does not execute even when the
// mark crosses it (and can still be cancelled), while a resting sell does.
func TestIntegration_ARestingBuyDoesNotExecuteOnceTheInstrumentOnlyCloses(t *testing.T) {
	w := newWorld(t)
	acct := w.account()
	w.fund(acct, w.cash.ID, 10_000_000_000)
	l := w.list(1, 1)
	w.fund(acct, l.ShareAsset.ID, 2_000_000)
	w.mark(l, 15_000)
	buy := w.trade(acct, l, limit(SideBuy, 1_000_000, 14_900))
	sell := w.trade(acct, l, limit(SideSell, 1_000_000, 15_100))
	require.Equal(t, OrderOpen, buy.Status)
	require.Equal(t, OrderOpen, sell.Status)
	transitionInstrument(t, w, l.Instrument.ID, assets.StatusCloseOnly)

	w.at(2026, 10, 7, 11, 0)
	w.mark(l, 14_000)
	assert.Equal(t, OrderOpen, w.poll(buy).Status, "a buy does not execute on a close-only instrument")
	w.at(2026, 10, 7, 11, 1)
	w.mark(l, 16_000)
	assert.Equal(t, OrderFilled, w.poll(sell).Status, "a sell reduces exposure and executes")
	r, err := w.svc.CancelAtVenue(w.ctx, testDB, buy.ID)
	require.NoError(t, err)
	buy, err = w.apply(buy.ID, r)
	require.NoError(t, err)
	assert.Equal(t, OrderCancelled, buy.Status, "a cancel is never blocked")
	assert.Equal(t, capital.ReservationReleased, w.reservation(buy).Status)
}

type nopKillAudit struct{}

func (nopKillAudit) Append(context.Context, pgx.Tx, killswitch.AuditEvent) error { return nil }

type noApprovals struct{}

func (noApprovals) VerifyApproved(context.Context, db.Querier, string, string, string) (killswitch.Approval, error) {
	return killswitch.Approval{}, errs.New(errs.CodeForbidden, "no approvals in this test")
}

func activateHalt(t *testing.T, w *world, iid instruments.InstrumentID) {
	t.Helper()
	ctrl, err := killswitch.NewController(w.clk, nopKillAudit{}, noApprovals{})
	require.NoError(t, err)
	op := security.WithPrincipal(context.Background(), security.Principal{SubjectID: "equities-itest-operator",
		ActorType: security.ActorOperator, Roles: []security.Role{security.RoleOperations}, AuthTime: w.clk.Now(), AMR: []string{"mfa"}})
	require.NoError(t, testDB.InTx(op, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ctrl.Activate(ctx, tx, killswitch.InstrumentHalt, iid.String(), "equities itest halt")
		return err
	}))
}

func transitionInstrument(t *testing.T, w *world, iid instruments.InstrumentID, to assets.Status) {
	t.Helper()
	require.NoError(t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := instruments.NewRepository().TransitionStatus(ctx, tx, iid, instruments.StatusChange{
			To: to, ActorType: "OPERATOR", ActorID: "equities-itest-operator", Reason: "itest"})
		return err
	}))
}

// TestIntegration_SessionRulesAreEnforced: market orders are accepted only
// in the regular session and refused (never queued) outside it; limit
// orders trade pre-market and after-hours only with extended hours;
// overnight, weekends and recorded holidays refuse everything.
func TestIntegration_SessionRulesAreEnforced(t *testing.T) {
	w := newWorld(t)
	acct := w.account()
	w.fund(acct, w.cash.ID, 100_000_000_000)
	l := w.list(1, 1)
	require.NoError(t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if err := w.svc.AddHoliday(ctx, tx, CalendarXNYS, Holiday{Date: thanksgiving2026, Name: "Thanksgiving Day"}, w.sys); err != nil {
			return err
		}
		// Recording it again is a replay; another name for the date is not.
		if err := w.svc.AddHoliday(ctx, tx, CalendarXNYS, Holiday{Date: thanksgiving2026, Name: "Thanksgiving Day"}, w.sys); err != nil {
			return err
		}
		return nil
	}))
	err := w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return w.svc.AddHoliday(ctx, tx, CalendarXNYS, Holiday{Date: thanksgiving2026, Name: "Turkey Day"}, w.sys)
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))

	ext := func(s orderSpec) orderSpec { s.extended = true; return s }
	cases := []struct {
		name            string
		y               int
		m               time.Month
		d, hh, mm       int
		spec            orderSpec
		accepted        bool
		expectedSession Session
	}{
		{"market regular", 2026, 10, 7, 10, 0, market(SideBuy, 100_000), true, SessionRegular},
		{"market pre-market", 2026, 10, 7, 8, 0, market(SideBuy, 100_000), false, SessionPreMarket},
		{"market after-hours", 2026, 10, 7, 17, 0, market(SideBuy, 100_000), false, SessionAfterHours},
		{"limit regular-only pre-market", 2026, 10, 7, 8, 0, limit(SideBuy, 100_000, 15_100), false, SessionPreMarket},
		{"limit regular-only after-hours", 2026, 10, 7, 17, 0, limit(SideBuy, 100_000, 15_100), false, SessionAfterHours},
		{"extended limit pre-market", 2026, 10, 7, 8, 0, ext(limit(SideBuy, 100_000, 15_100)), true, SessionPreMarket},
		{"extended limit after-hours", 2026, 10, 7, 17, 0, ext(limit(SideBuy, 100_000, 15_100)), true, SessionAfterHours},
		{"extended limit overnight", 2026, 10, 7, 21, 0, ext(limit(SideBuy, 100_000, 15_100)), false, SessionClosed},
		{"market on a Saturday", 2026, 10, 10, 11, 0, market(SideBuy, 100_000), false, SessionClosed},
		{"extended limit on a Sunday", 2026, 10, 11, 11, 0, ext(limit(SideBuy, 100_000, 15_100)), false, SessionClosed},
		{"market on a holiday", 2026, 11, 26, 11, 0, market(SideBuy, 100_000), false, SessionClosed},
		{"extended limit on a holiday", 2026, 11, 26, 8, 0, ext(limit(SideBuy, 100_000, 15_100)), false, SessionClosed},
		{"market the day after the holiday", 2026, 11, 27, 9, 30, market(SideBuy, 100_000), true, SessionRegular},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w.at(tc.y, tc.m, tc.d, tc.hh, tc.mm)
			w.mark(l, 15_000)
			o := w.accept(w.submit(w.request(acct, l, tc.spec)).ID)
			assert.Equal(t, tc.expectedSession, o.Session)
			if !tc.accepted {
				require.Equal(t, OrderRejected, o.Status)
				assert.Equal(t, RejectRisk, o.RejectionCode)
				assert.Contains(t, o.RejectionReasons, risk.ReasonInstrumentSessionClosed)
				return
			}
			require.Equal(t, OrderAccepted, o.Status)
			assert.Equal(t, OrderFilled, w.place(o).Status, "the venue honours the same session")
		})
	}
}

// TestIntegration_LimitOrderRestsThenFillsOnlyWhenTheMarkCrosses: a buy
// limit below the mark rests OPEN with its capital reserved at the limit,
// survives a poll at a mark still above the limit, and fills when the mark
// crosses -- at the slipped mark, capped by the limit.
func TestIntegration_LimitOrderRestsThenFillsOnlyWhenTheMarkCrosses(t *testing.T) {
	w := newWorld(t)
	acct := w.account()
	w.fund(acct, w.cash.ID, 10_000_000_000)
	l := w.list(1, 1)
	w.mark(l, 15_000)

	o := w.trade(acct, l, limit(SideBuy, 1_000_000, 14_900))
	require.Equal(t, OrderOpen, o.Status)
	assert.NotEmpty(t, o.BrokerOrderID)
	assert.Equal(t, econ.StatusExecuting, w.intent(o.IntentID).Status)
	res := w.reservation(o)
	assert.Equal(t, capital.ReservationActive, res.Status)
	assert.Equal(t, o.ID.String(), res.LockedByOrderID, "an open order's reservation is locked against expiry")
	// 149.00 + 20 bps spread + 10 bps fee + 3 bps venue + 50 bps reserve.
	assert.Equal(t, "150236700", res.Quantity.String())
	assert.Equal(t, "10000000000", w.cust(acct, ledger.CodeWallet, w.cash.ID).String(), "nothing is paid before the fill")

	w.at(2026, 10, 7, 11, 0)
	w.mark(l, 14_950)
	assert.Equal(t, OrderOpen, w.poll(o).Status)

	w.at(2026, 10, 7, 12, 0)
	w.mark(l, 14_880)
	o = w.poll(o)
	require.Equal(t, OrderFilled, o.Status)
	f, _, err := w.svc.FillForOrder(w.ctx, testDB, o.ID)
	require.NoError(t, err)
	assert.Equal(t, "14895", f.Price.String(), "148.80 slipped 10 bps is 148.9488, up to 148.95, under the 149.00 limit")
	res = w.reservation(o)
	assert.Equal(t, capital.ReservationConsumed, res.Status)
	assert.Equal(t, "149441535", res.ConsumedQuantity.String())
	assert.Equal(t, "9850558465", w.cust(acct, ledger.CodeWallet, w.cash.ID).String())
	assert.Equal(t, econ.StatusExecuting, w.intent(o.IntentID).Status)
}

// TestIntegration_CancelAndExpiryReleaseTheReservation: a cancelled order
// and an expired one hand their reserved capital back, and their intents end.
func TestIntegration_CancelAndExpiryReleaseTheReservation(t *testing.T) {
	w := newWorld(t)
	acct := w.account()
	w.fund(acct, w.cash.ID, 10_000_000_000)
	l := w.list(1, 1)
	w.mark(l, 15_000)

	cancelled := w.trade(acct, l, limit(SideBuy, 1_000_000, 14_000))
	require.Equal(t, OrderOpen, cancelled.Status)
	r, err := w.svc.CancelAtVenue(w.ctx, testDB, cancelled.ID)
	require.NoError(t, err)
	cancelled, err = w.apply(cancelled.ID, r)
	require.NoError(t, err)
	assert.Equal(t, OrderCancelled, cancelled.Status)
	assert.Equal(t, capital.ReservationReleased, w.reservation(cancelled).Status)
	assert.Equal(t, econ.StatusFailed, w.intent(cancelled.IntentID).Status)

	resting := w.trade(acct, l, limit(SideBuy, 1_000_000, 14_000))
	unsent := w.accept(w.submit(w.request(acct, l, limit(SideBuy, 1_000_000, 14_000))).ID)
	require.Equal(t, OrderAccepted, unsent.Status)
	w.at(2026, 10, 7, 16, 0)
	w.mark(l, 15_000)
	resting = w.poll(resting)
	assert.Equal(t, OrderExpired, resting.Status)
	assert.Equal(t, econ.StatusFailed, w.intent(resting.IntentID).Status)
	unsent = w.poll(unsent)
	assert.Equal(t, OrderExpired, unsent.Status)
	assert.Equal(t, econ.StatusCancelled, w.intent(unsent.IntentID).Status)
	effect, _, err := w.effects.GetByKey(w.ctx, testDB, submitKey(unsent.ID))
	require.NoError(t, err)
	assert.Equal(t, econ.EffectFailed, effect.Status, "an order that never reached the venue never was submitted")
	for _, o := range []Order{resting, unsent} {
		assert.Equal(t, capital.ReservationReleased, w.reservation(o).Status)
	}
	av, err := w.capital.Availability(w.ctx, testDB, acct.String(), w.cash.ID, w.clk.Now())
	require.NoError(t, err)
	assert.True(t, av.Reserved.IsZero(), "every reservation was handed back")
	assert.Equal(t, "10000000000", av.Available.String())
}

// TestIntegration_UnsettledProceedsAreNotWithdrawable: a sale's proceeds sit
// in UNSETTLED_RECEIVABLE until the settlement date. Buying power V2 sees
// them as equity but never as withdrawable or spendable cash; after
// EQUITY_SETTLEMENT they are. A buy pays at the trade date, so the cash
// stops being withdrawable at once and the unsettled shares are not either.
func TestIntegration_UnsettledProceedsAreNotWithdrawable(t *testing.T) {
	w := newWorld(t)
	acct := w.account()
	l := w.list(1, 1)
	w.fund(acct, w.cash.ID, 1_000_000_000)    // $1,000.00
	w.fund(acct, l.ShareAsset.ID, 10_000_000) // 10 shares, settled
	w.mark(l, 15_000)
	loader, err := buyingpower.NewV2Loader(buyingpower.V2LoaderConfig{Clock: w.clk, Prices: w.prices, QuoteAssetID: w.cash.ID,
		MaxPriceAge: time.Minute})
	require.NoError(t, err)
	bp := func() buyingpower.V2 {
		t.Helper()
		v, err := loader.Compute(w.ctx, testDB, "CUSTOMER", acct.String())
		require.NoError(t, err)
		return v
	}
	assert.Equal(t, money.USDFromMinor(1_000_00), bp().Withdrawable)

	sell := w.trade(acct, l, market(SideSell, 4_000_000))
	require.Equal(t, OrderFilled, sell.Status)
	// 4 × 149.85 = 599.40 less 1.1988 spread, 0.5994 fee, 0.17982 venue.
	assert.Equal(t, "597421980", w.cust(acct, ledger.CodeUnsettledReceivable, w.cash.ID).String())
	assert.Equal(t, "1000000000", w.cust(acct, ledger.CodeWallet, w.cash.ID).String(), "proceeds are not in WALLET yet")
	assert.Equal(t, "6000000", w.cust(acct, ledger.CodeWallet, l.ShareAsset.ID).String(), "the shares left at the trade date")

	mid := bp()
	assert.Equal(t, money.USDFromMinor(1_000_00), mid.Withdrawable, "unsettled proceeds are not withdrawable")
	assert.Equal(t, money.USDFromMinor(1_000_00), mid.TradeBuyingPower, "nor spendable under the default policy")
	assert.Equal(t, money.USDFromMinor(2_497_42), mid.GrossEquity, "but they are the customer's: 1000 + 597.42 + 6 × 150")
	found := false
	for _, r := range mid.Reasons {
		if r.Code == buyingpower.V2ReasonUnsettled {
			found = true
			assert.Equal(t, money.USDFromMinor(597_42), r.Amount)
		}
	}
	assert.True(t, found, "the answer says why: %+v", mid.Reasons)

	w.at(2026, 10, 8, 9, 0)
	w.mark(l, 15_000)
	w.settleDue()
	assert.Equal(t, "1597421980", w.cust(acct, ledger.CodeWallet, w.cash.ID).String())
	assert.True(t, w.cust(acct, ledger.CodeUnsettledReceivable, w.cash.ID).IsZero())
	after := bp()
	assert.Equal(t, money.USDFromMinor(1_597_42), after.Withdrawable, "settled proceeds are withdrawable")

	w.at(2026, 10, 8, 10, 0)
	w.mark(l, 15_000)
	buy := w.trade(acct, l, market(SideBuy, 1_000_000))
	require.Equal(t, OrderFilled, buy.Status)
	paid, _, err := w.svc.FillForOrder(w.ctx, testDB, buy.ID)
	require.NoError(t, err)
	wantCash, err := money.QuoteQuantityToUSD(q(1_597_421_980).Sub(paid.Charges.Total), 6, money.RoundDown)
	require.NoError(t, err)
	assert.Equal(t, wantCash, bp().Withdrawable, "the buyer's cash left on the trade date")
	assert.Equal(t, "1000000", w.cust(acct, ledger.CodeUnsettledReceivable, l.ShareAsset.ID).String())
}

// TestIntegration_AllChargedSpreadIsDiscoverableFromTheStoredDisclosure:
// for every fill the stored schedule and disclosure re-verify with
// fees.VerifyDisclosure, the journal books exactly the disclosed spread and
// fees, and the platform's whole spread revenue is the sum of the disclosed
// spreads -- nothing was charged that a record does not account for.
func TestIntegration_AllChargedSpreadIsDiscoverableFromTheStoredDisclosure(t *testing.T) {
	w := newWorld(t)
	acct := w.account()
	l := w.list(1, 1)
	w.fund(acct, w.cash.ID, 10_000_000_000)
	w.fund(acct, l.ShareAsset.ID, 10_000_000)
	w.mark(l, 15_000)
	orders := []Order{
		w.trade(acct, l, market(SideBuy, 2_500_000)),
		w.trade(acct, l, market(SideSell, 4_000_000)),
		w.trade(acct, l, limit(SideBuy, 1_234_567, 15_100)),
	}
	wantHash, err := w.schedule.Hash()
	require.NoError(t, err)
	var spread, platformFees money.Quantity
	for _, o := range orders {
		require.Equal(t, OrderFilled, o.Status)
		d, err := w.svc.VerifyFill(w.ctx, testDB, o.ID)
		require.NoError(t, err)
		assert.Equal(t, wantHash, d.ScheduleHash)
		assert.True(t, d.Spread.Amount.IsPositive())
		spread = spread.Add(d.Spread.Amount)
		f, _, err := w.svc.FillForOrder(w.ctx, testDB, o.ID)
		require.NoError(t, err)
		platformFees = platformFees.Add(f.Charges.PlatformFees)
	}
	assert.Equal(t, spread.String(), w.platform(ledger.CodeSpreadRevenue).String())
	assert.Equal(t, spread.String(), w.cust(acct, ledger.CodeFeesSpread, w.cash.ID).String())
	assert.Equal(t, platformFees.String(), w.platform(ledger.CodePlatformFeeRevenue).String())
}

// TestIntegration_UnknownSubmissionIsReconciledNotRetried: when the venue's
// answer to a submission is lost the submission effect becomes UNKNOWN and
// sits on the reconciliation worklist; nothing re-sends the order, and the
// outcome observed by polling the venue resolves it.
func TestIntegration_UnknownSubmissionIsReconciledNotRetried(t *testing.T) {
	w := newWorld(t)
	acct := w.account()
	w.fund(acct, w.cash.ID, 10_000_000_000)
	l := w.list(1, 1)
	w.mark(l, 15_000)
	in := w.submit(w.request(acct, l, market(SideBuy, 1_000_000)))
	o := w.accept(in.ID)
	require.Equal(t, OrderAccepted, o.Status)
	require.NoError(t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return w.svc.MarkSubmissionUnknown(ctx, tx, o.ID, "venue call timed out")
	}))
	effect, _, err := w.effects.GetByKey(w.ctx, testDB, submitKey(o.ID))
	require.NoError(t, err)
	assert.Equal(t, econ.EffectUnknown, effect.Status)
	unresolved, err := w.effects.ListUnresolved(w.ctx, testDB, econ.SubsystemEquities, time.Now().Add(time.Hour), 1000)
	require.NoError(t, err)
	listed := false
	for _, u := range unresolved {
		listed = listed || u.ID == effect.ID
	}
	assert.True(t, listed, "an unknown submission is on the reconciliation worklist")
	assert.Equal(t, o.ID, w.accept(in.ID).ID, "replaying the acceptance changes nothing")

	o = w.poll(o)
	require.Equal(t, OrderFilled, o.Status)
	effect, _, err = w.effects.GetByKey(w.ctx, testDB, submitKey(o.ID))
	require.NoError(t, err)
	assert.Equal(t, econ.EffectApplied, effect.Status, "UNKNOWN resolves to APPLIED once the outcome is observed")
}

type fakeKills struct{ snap killswitch.Snapshot }

func (f fakeKills) Snapshot(context.Context, db.Querier) (killswitch.Snapshot, error) {
	return f.snap, nil
}

// TestIntegration_TheCapabilityGateAndKillSwitchRefuseOrders: a
// CAPABILITY_DISABLE switch for EQUITIES, or a deployment whose EQUITIES gate
// is not ACTIVE (STAGING), refuses every order with the Authorizer's code.
func TestIntegration_TheCapabilityGateAndKillSwitchRefuseOrders(t *testing.T) {
	killed := newWorld(t, withKillSwitches(fakeKills{snap: killswitch.Snapshot{Switches: []killswitch.Switch{
		{Kind: killswitch.CapabilityDisable, ScopeID: string(gates.Equities), Active: true}}}}))
	acct := killed.account()
	l := killed.list(1, 1)
	killed.mark(l, 15_000)
	killed.fund(acct, killed.cash.ID, 10_000_000_000)
	o := killed.accept(killed.submit(killed.request(acct, l, market(SideBuy, 1_000_000))).ID)
	require.Equal(t, OrderRejected, o.Status)
	assert.Equal(t, RejectKillSwitch, o.RejectionCode)
	assert.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(o.Refusal()))

	staging := newWorld(t, func(c *Config) {
		checker, err := gates.NewChecker("STAGING", func(gates.Capability) bool { return true }, c.Clock)
		require.NoError(t, err)
		c.Authorizer = econ.NewAuthorizer(checker, c.KillSwitches)
	})
	acct2 := staging.account()
	l2 := staging.list(1, 1)
	staging.mark(l2, 15_000)
	o = staging.accept(staging.submit(staging.request(acct2, l2, market(SideBuy, 1_000_000))).ID)
	require.Equal(t, OrderRejected, o.Status)
	assert.Equal(t, RejectCapabilityNotApproved, o.RejectionCode)
	assert.Contains(t, o.RejectionReasons, gates.ReasonGateNotActive)
}

// TestIntegration_AnAgentOrderWithoutAuthorityIsRefused: an agent may
// propose an equity order, but the risk kernel requires its delegated
// authority, and without one the order is refused -- fail closed.
func TestIntegration_AnAgentOrderWithoutAuthorityIsRefused(t *testing.T) {
	w := newWorld(t)
	acct := w.account()
	w.fund(acct, w.cash.ID, 10_000_000_000)
	l := w.list(1, 1)
	w.mark(l, 15_000)
	agent := uuid.NewString()
	req := w.request(acct, l, market(SideBuy, 1_000_000))
	req.ActorType, req.ActorID, req.AgentID, req.AgentAuthority = security.ActorAgent, agent, agent, agentauthority.LevelUserApprovedRule
	in := w.submit(req)
	require.Equal(t, econ.StatusRouted, in.Status)
	o := w.accept(in.ID)
	require.Equal(t, OrderRejected, o.Status)
	assert.Contains(t, o.RejectionReasons, risk.ReasonInputInvalid)
	var missing []string
	require.NoError(t, testDB.QueryRow(w.ctx, `SELECT ARRAY(SELECT jsonb_array_elements_text(decision->'missing_sections'))
		FROM advanced_risk_decisions WHERE intent_id = $1`, in.ID).Scan(&missing))
	assert.Contains(t, missing, risk.SecAuthority)

	// An agent never applies anything itself.
	err := w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return w.svc.AddHoliday(ctx, tx, CalendarXNYS, Holiday{Date: NewDate(2027, 7, 5), Name: "x"}, econ.Actor{Type: security.ActorAgent, ID: agent})
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
}

// TestIntegration_LiveCannotBeRequested: no equity order can be LIVE; the
// request is refused before anything is written.
func TestIntegration_LiveCannotBeRequested(t *testing.T) {
	w := newWorld(t)
	acct := w.account()
	l := w.list(1, 1)
	req := w.request(acct, l, market(SideBuy, 1_000_000))
	req.Mode = gates.ModeLive
	err := w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.svc.SubmitOrderIntent(ctx, tx, req)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	assert.Zero(t, w.count(`SELECT count(*) FROM economic_intents WHERE account_id = $1`, acct))
}
