//go:build integration

package derivatives

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
)

func (f *fixture) reconcile() ClearingReport {
	f.t.Helper()
	var rep ClearingReport
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rep, err = f.svc.ReconcileClearing(ctx, tx, f.usd)
		return err
	}))
	return rep
}

// TestIntegration_TheClearingHouseNetsToZeroForAMatchedBook: two customers
// trade 5 contracts with each other at the same prices (matched book fills),
// through a variation-margin settlement, a funding interval and the close.
// Every flow one receives the other pays, so the clearing house's
// CLEARING_RESULT nets to exactly zero and reconciliation kind CLEARING
// matches. Then value is moved through the clearing house outside the
// register -- a posting the database accepts, because it balances per party --
// and reconciliation finds it: a material CLEARING MISMATCH, refreshed rather
// than duplicated on the next pass, that stops new derivative risk.
func TestIntegration_TheClearingHouseNetsToZeroForAMatchedBook(t *testing.T) {
	f := newFixture(t)
	perp := f.perp()
	a, b := f.account(), f.account()
	f.fund(a, usdUnits(100_000))
	f.fund(b, usdUnits(100_000))
	m := f.mark(perp, 65_000, 65_000)
	f.bookFill(a, perp, Buy, 5, dollars(65_000), m, FillOpens)
	f.bookFill(b, perp, Sell, 5, dollars(65_000), m, FillOpens)
	rep := f.reconcile()
	assert.True(t, rep.InternallyMatched)
	assert.True(t, rep.Matched)
	assert.True(t, rep.ClearingResult.IsZero())

	// Variation margin at $66 000: a +$50, b −$50.
	f.clk.Set(start.Add(time.Hour))
	f.mark(perp, 66_000, 65_000)
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		r, err := f.svc.RunVariationMargin(ctx, tx, RunRequest{InstrumentID: perp, IntervalStart: start})
		assert.Equal(t, "50000000", r.Received.String())
		assert.Equal(t, "50000000", r.Paid.String())
		return err
	}))
	// Funding for 08:00-16:00: premium (0 + 1/65)/2 clamped to 0.75% of $660.
	f.clk.Set(fundingWindow.Add(8 * time.Hour))
	r, err := f.runFunding(perp, fundingWindow)
	require.NoError(t, err)
	assert.True(t, r.Clamped)
	assert.Equal(t, "4950000", r.PerContract.String())
	assert.Equal(t, r.Paid.String(), r.Received.String())

	// Both close at $64 000 against each other.
	f.clk.Advance(time.Minute)
	m = f.mark(perp, 64_000, 64_000)
	closeA := f.bookFill(a, perp, Sell, 5, dollars(64_000), m, FillReduces)
	closeB := f.bookFill(b, perp, Buy, 5, dollars(64_000), m, FillReduces)
	assert.Equal(t, "-100000000", closeA.Fill.CashSettled.String())
	assert.Equal(t, "100000000", closeB.Fill.CashSettled.String())
	assert.Equal(t, PositionClosed, closeA.Position.Status)

	assert.True(t, f.clearingBalance(ledger.CodeClearingResult).IsZero(), "a matched book nets to zero at the clearing house")
	assert.Equal(t, f.clearingBalance(ledger.CodeClearingCapital).String(), f.clearingBalance(ledger.CodeClearingCash).String())
	rep = f.reconcile()
	assert.True(t, rep.Matched, "%+v", rep)
	assert.True(t, rep.InternallyMatched)
	assert.True(t, rep.RegisterNet.IsZero())
	assert.Equal(t, int64(6), rep.Flows, "variation margin, funding and the close, both sides each")
	// Each customer's whole life: a −$100 + $50 − $24.75 = −$74.75, b the mirror.
	assert.Equal(t, "-74750000", f.balance(a, ledger.CodeRealizedPnL).String())
	assert.Equal(t, "74750000", f.balance(b, ledger.CodeRealizedPnL).String())

	// Inject: $0.000777 through the clearing house with no register row.
	ch := f.clearingHouse()
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := rawJournal(ctx, f, tx, ledger.KindVariationMargin, []rawEntry{
			{ledger.CustomerAccount(a, ledger.CodeWallet, f.usd), ledger.Debit, 777},
			{ledger.CustomerAccount(a, ledger.CodeRealizedPnL, f.usd), ledger.Credit, 777},
			{ch.Account(ledger.CodeClearingResult, f.usd), ledger.Debit, 777},
			{ch.Account(ledger.CodeClearingCash, f.usd), ledger.Credit, 777},
		})
		return err
	}), "the database cannot see this: it balances per party")
	// Six passes at once open one record between them.
	var wg sync.WaitGroup
	reports := make(chan ClearingReport, 6)
	failures := make(chan error, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
				r, err := f.svc.ReconcileClearing(ctx, tx, f.usd)
				if err == nil {
					reports <- r
				}
				return err
			})
			if err != nil {
				failures <- err
			}
		}()
	}
	wg.Wait()
	close(reports)
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	for r := range reports {
		rep = r
		assert.False(t, r.Matched)
	}
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM reconciliation_records WHERE kind = 'CLEARING' AND scope_id = $1`,
		clearingScopeID(ch.ID, f.usd)))
	assert.False(t, rep.Matched)
	assert.Equal(t, "-777", rep.Difference.String())
	require.NotEmpty(t, rep.RecordID)
	recID, err := reconciliation.ParseRecordID(rep.RecordID)
	require.NoError(t, err)
	rec, err := f.recon.Get(f.ctx, testDB, recID)
	require.NoError(t, err)
	assert.Equal(t, reconciliation.KindClearing, rec.Kind)
	assert.Equal(t, reconciliation.StatusMismatch, rec.Status)
	assert.True(t, rec.Material)
	assert.Equal(t, ClearingScopeType, rec.ScopeType)

	again := f.reconcile()
	assert.Equal(t, rep.RecordID, again.RecordID, "the open record is refreshed, not duplicated")
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM reconciliation_records WHERE kind = 'CLEARING' AND scope_id = $1`,
		clearingScopeID(ch.ID, f.usd)))

	// While the clearing house is unreconciled, new derivative risk stops.
	f.mark(perp, 64_000, 64_000)
	_, err = f.tryOpen(OpenRequest{IntentRequest: f.req(a, perp), Side: Long, Quantity: q(1)})
	ref := requireRefusal(t, err, errs.CodeRiskRejected, risk.ReasonCircuitBreaker)
	assert.NotNil(t, ref.DecisionID)
}
