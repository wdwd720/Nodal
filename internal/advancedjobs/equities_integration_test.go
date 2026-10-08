//go:build integration

package advancedjobs

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/equities"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// listEquity lists a fresh equity settling in the sandbox dollar, with six
// share decimals and whole-cent prices.
func (w *world) listEquity() equities.Listing {
	w.t.Helper()
	var l equities.Listing
	require.NoError(w.t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		l, err = w.equities.ListEquity(ctx, tx, equities.ListingSpec{
			Kind: instruments.TypeEquity, Symbol: "AJEQ" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:6]),
			Name: "Advanced Worker Example Corp", ExchangeMIC: "XNAS", CashAssetID: w.usd.ID, ShareDecimals: 6,
			PriceScale: 2, TickSize: money.QuantityFromInt64(1), LotSize: money.QuantityFromInt64(1), MinQuantity: money.QuantityFromInt64(1),
			OracleMaxAgeMS: 60_000, OracleSources: []string{"advancedjobs-itest"}, RiskClass: assets.RiskStandard,
			Actor: w.operator("lister"),
		})
		return err
	}))
	return l
}

// buyShares runs a market buy through the whole equities pipeline -- intent,
// acceptance, the paper venue, the venue's report -- and returns the order.
func (w *world) buyShares(c customer, l equities.Listing, shares int64) equities.Order {
	w.t.Helper()
	req := equities.OrderRequest{
		AccountID: c.account, InstrumentID: l.Instrument.ID, Side: equities.SideBuy, Type: equities.OrderMarket,
		Quantity: units(shares, l.ShareAsset.Decimals), Mode: gates.ModePaper, IdempotencyKey: "advancedjobs-" + uuid.NewString(),
		ActorType: security.ActorUser, ActorID: c.user.String(),
	}
	var o equities.Order
	require.NoError(w.t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		in, err := w.equities.SubmitOrderIntent(ctx, tx, req)
		if err != nil {
			return err
		}
		o, err = w.equities.AcceptOrder(ctx, tx, in.ID)
		return err
	}))
	require.Equal(w.t, equities.OrderAccepted, o.Status, "%s %v", o.RejectionCode, o.RejectionReasons)
	report, err := w.equities.PlaceAtVenue(w.ctx, testDB, o.ID)
	require.NoError(w.t, err)
	require.NoError(w.t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		o, err = w.equities.ApplyOrderReport(ctx, tx, o.ID, report)
		return err
	}))
	require.Equal(w.t, equities.OrderFilled, o.Status)
	return o
}

// TestIntegration_AnEquityFillSettlesAtTPlusOneAndNotBefore: a fill on
// Wednesday is still unsettled after the job runs on Wednesday, settles when
// the job runs on Thursday (T+1 on the New York calendar), and settles once.
func TestIntegration_AnEquityFillSettlesAtTPlusOneAndNotBefore(t *testing.T) {
	w := newWorld(t)
	l := w.listEquity()
	c := w.customer("equities-buyer")
	w.fund(c.account, w.usd.ID, w.usdUnits(10_000))
	w.mark(l.ShareAsset.ID, "100.00")
	o := w.buyShares(c, l, 10)
	shares := func(code ledger.Code) money.Quantity {
		return w.balance(ledger.CustomerAccount(c.account, code, l.ShareAsset.ID))
	}
	require.Equal(t, units(10, 6).String(), shares(ledger.CodeUnsettledReceivable).String(), "bought, awaiting settlement")
	job := NewEquitiesSettlement(testDB, w.equities, Settings{})

	w.clk.Advance(5 * time.Hour) // still Wednesday in New York
	run(t, w.ctx, job)
	assert.Equal(t, units(10, 6).String(), shares(ledger.CodeUnsettledReceivable).String(), "T+0: not settled")
	assert.True(t, shares(ledger.CodeWallet).IsZero())

	w.clk.Set(time.Date(2026, 10, 8, 13, 30, 0, 0, time.UTC)) // Thursday 09:30 New York
	rep := run(t, w.ctx, job)
	assert.True(t, rep.OK(), "%+v", rep.Failures)
	assert.True(t, shares(ledger.CodeUnsettledReceivable).IsZero(), "T+1: settled")
	assert.Equal(t, units(10, 6).String(), shares(ledger.CodeWallet).String())

	run(t, w.ctx, job)
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM journal_transactions WHERE kind = 'EQUITY_SETTLEMENT'
		AND reference_id IN (SELECT id::text FROM equity_settlements WHERE order_id = $1)`, o.ID), "settled once")
	assert.Equal(t, units(10, 6).String(), shares(ledger.CodeWallet).String())
}

// TestIntegration_ACorporateActionIsAppliedOnItsDateOnce: a symbol change
// announced for Thursday is not applied on Wednesday, is applied by the job on
// Thursday, and only once.
func TestIntegration_ACorporateActionIsAppliedOnItsDateOnce(t *testing.T) {
	w := newWorld(t)
	l := w.listEquity()
	newSymbol := "AJNEW" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:6])
	var a equities.CorporateAction
	require.NoError(t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		a, err = w.equities.AnnounceCorporateAction(ctx, tx, equities.CorporateActionSpec{
			InstrumentID: l.Instrument.ID, Type: equities.ActionSymbolChange, EffectiveDate: equities.NewDate(2026, 10, 8),
			NewSymbol: newSymbol, Reason: "advanced worker integration test", Actor: w.operator("corporate"),
		})
		return err
	}))
	job := NewEquitiesCorporateActions(testDB, w.equities, w.clk, Settings{})
	status := func() string {
		var s string
		require.NoError(t, testDB.QueryRow(w.ctx, `SELECT status FROM equity_corporate_actions WHERE id = $1`, a.ID).Scan(&s))
		return s
	}

	rep := run(t, w.ctx, job)
	assert.False(t, failedUnit(rep, a.ID.String()), "%+v", rep.Failures)
	assert.Equal(t, "ANNOUNCED", status(), "not before its effective date")

	w.clk.Set(time.Date(2026, 10, 8, 13, 0, 0, 0, time.UTC)) // Thursday 09:00 New York
	rep = run(t, w.ctx, job)
	assert.False(t, failedUnit(rep, a.ID.String()), "%+v", rep.Failures)
	assert.Equal(t, "APPLIED", status())
	v, err := w.instr.CurrentTerms(w.ctx, testDB, l.Instrument.ID)
	require.NoError(t, err)
	assert.Equal(t, newSymbol, v.Terms.Symbol)

	run(t, w.ctx, job)
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM equity_corporate_action_transitions WHERE action_id = $1 AND to_status = 'APPLIED'`, a.ID),
		"applied once")
}

// acceptOnly takes a market buy as far as ACCEPTED and stops, as a request
// whose process died before it reached the venue leaves it.
func (w *world) acceptOnly(svc *equities.Service, c customer, l equities.Listing, shares int64) equities.Order {
	w.t.Helper()
	req := equities.OrderRequest{
		AccountID: c.account, InstrumentID: l.Instrument.ID, Side: equities.SideBuy, Type: equities.OrderMarket,
		Quantity: units(shares, l.ShareAsset.Decimals), Mode: gates.ModePaper, IdempotencyKey: "advancedjobs-" + uuid.NewString(),
		ActorType: security.ActorUser, ActorID: c.user.String(),
	}
	var o equities.Order
	require.NoError(w.t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		in, err := svc.SubmitOrderIntent(ctx, tx, req)
		if err != nil {
			return err
		}
		o, err = svc.AcceptOrder(ctx, tx, in.ID)
		return err
	}))
	require.Equal(w.t, equities.OrderAccepted, o.Status, "%s %v", o.RejectionCode, o.RejectionReasons)
	return o
}

func (w *world) equityOrder(svc *equities.Service, oid equities.OrderID) equities.Order {
	w.t.Helper()
	o, err := svc.Order(w.ctx, testDB, oid)
	require.NoError(w.t, err)
	return o
}

// unknownVenue is the paper venue except that it never answers a placement:
// every PlaceOrder ends SUBMISSION_STATE_UNKNOWN, as a timeout would. It
// counts the placements of each order.
type unknownVenue struct {
	equities.EquitiesExecutionAdapter
	mu         sync.Mutex
	placements map[string]int
}

func (v *unknownVenue) PlaceOrder(_ context.Context, req equities.PlaceOrderRequest) (equities.OrderReport, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.placements[req.Order.ClientOrderID]++
	return equities.OrderReport{}, errs.New(errs.CodeSubmissionStateUnknown, "the venue did not answer in time")
}

func (v *unknownVenue) placed(oid equities.OrderID) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.placements[oid.String()]
}

// TestIntegration_AnAcceptedEquityOrderIsPlacedOnceAfterTheGrace: an order a
// request accepted but never placed is left alone while the request may
// still be placing it, is placed by the job once the grace has passed --
// filled at the paper venue -- and is never placed again.
func TestIntegration_AnAcceptedEquityOrderIsPlacedOnceAfterTheGrace(t *testing.T) {
	w := newWorld(t)
	l := w.listEquity()
	c := w.customer("venue-buyer")
	w.fund(c.account, w.usd.ID, w.usdUnits(10_000))
	w.mark(l.ShareAsset.ID, "100.00")
	o := w.acceptOnly(w.equities, c, l, 5)
	job := NewEquitiesVenue(testDB, w.equities, w.clk, Settings{})

	rep := run(t, w.ctx, job)
	assert.False(t, failedUnit(rep, o.ID.String()), "%+v", rep.Failures)
	assert.Equal(t, equities.OrderAccepted, w.equityOrder(w.equities, o.ID).Status, "within the grace a request may still be placing it")

	w.clk.Advance(PlacementGrace)
	rep = run(t, w.ctx, job)
	assert.False(t, failedUnit(rep, o.ID.String()), "%+v", rep.Failures)
	assert.Equal(t, equities.OrderFilled, w.equityOrder(w.equities, o.ID).Status, "placed and filled at the paper venue")

	run(t, w.ctx, job)
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM equity_fills WHERE order_id = $1`, o.ID), "filled once")
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM equity_settlements WHERE order_id = $1`, o.ID))
}

// TestIntegration_AnUnknownEquityPlacementIsPolledNeverPlacedAgain: a
// placement whose answer never came is recorded as unknown -- the order
// stays ACCEPTED and the service refuses to send it again -- and the next run
// resolves it by polling the venue, which filled it. The venue saw exactly
// one placement.
func TestIntegration_AnUnknownEquityPlacementIsPolledNeverPlacedAgain(t *testing.T) {
	w := newWorld(t)
	venue := &unknownVenue{EquitiesExecutionAdapter: w.equities.Adapter(), placements: map[string]int{}}
	svc, err := newEquitiesOver(w.clk, w, w.audit, venue)
	require.NoError(t, err)
	l := w.listEquity()
	c := w.customer("venue-unknown")
	w.fund(c.account, w.usd.ID, w.usdUnits(10_000))
	w.mark(l.ShareAsset.ID, "100.00")
	o := w.acceptOnly(svc, c, l, 5)
	w.clk.Advance(PlacementGrace)
	job := NewEquitiesVenue(testDB, svc, w.clk, Settings{})

	rep := run(t, w.ctx, job)
	assert.False(t, failedUnit(rep, o.ID.String()), "%+v", rep.Failures)
	assert.Positive(t, rep.Deferred)
	assert.Equal(t, equities.OrderAccepted, w.equityOrder(svc, o.ID).Status)
	_, err = svc.PlaceAtVenue(w.ctx, testDB, o.ID)
	require.Error(t, err)
	assert.Equal(t, errs.CodeReconciliationRequired, errs.CodeOf(err), "an unknown submission is never sent again")

	rep = run(t, w.ctx, job)
	assert.False(t, failedUnit(rep, o.ID.String()), "%+v", rep.Failures)
	assert.Equal(t, equities.OrderFilled, w.equityOrder(svc, o.ID).Status, "resolved by polling the venue")
	run(t, w.ctx, job)
	assert.Equal(t, 1, venue.placed(o.ID), "placed exactly once")
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM equity_fills WHERE order_id = $1`, o.ID))
}
