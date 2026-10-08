//go:build integration

package equities

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
)

func (w *world) announce(spec CorporateActionSpec) (CorporateAction, error) {
	var a CorporateAction
	spec.Actor = w.sys
	err := w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		a, err = w.svc.AnnounceCorporateAction(ctx, tx, spec)
		return err
	})
	return a, err
}

func (w *world) applyAction(aid CorporateActionID) (CorporateActionResult, error) {
	var r CorporateActionResult
	err := w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = w.svc.ApplyCorporateAction(ctx, tx, aid, w.sys)
		return err
	})
	return r, err
}

func applicationOf(t *testing.T, apps []Application, acct accounts.AccountID) Application {
	t.Helper()
	for _, a := range apps {
		if a.AccountID == acct {
			return a
		}
	}
	t.Fatalf("no application for %s", acct)
	return Application{}
}

// valueAt is quantity × price exactly, in cash micro-units (six decimals on
// both sides, price at scale 2), as a rational over 100.
func valueAt(qty money.Quantity, cents int64) money.Quantity { return qty.Mul(q(cents)) }

// TestIntegration_SplitAndReverseSplitPreserveValueWithCashInLieu: a 2-for-1
// split doubles every settled holding with CORPORATE_ACTION postings; a
// 1-for-3 reverse split divides it, rounds each holding down to a whole
// base unit and pays the fraction in cash at the stated price. Value is
// preserved exactly: shares × price before = shares × price after + cash.
func TestIntegration_SplitAndReverseSplitPreserveValueWithCashInLieu(t *testing.T) {
	w := newWorld(t)
	a, b := w.account(), w.account()
	l := w.list(1, 1)
	w.fund(a, l.ShareAsset.ID, 10_000_000)
	w.fund(b, l.ShareAsset.ID, 3_333_334)
	w.fund(b, w.cash.ID, 1_000_000_000)
	w.mark(l, 15_000)

	split, err := w.announce(CorporateActionSpec{InstrumentID: l.Instrument.ID, Type: ActionSplit, EffectiveDate: NewDate(2026, 10, 8),
		Ratio: &Ratio{Numerator: 2, Denominator: 1}, Reason: "2-for-1 split"})
	require.NoError(t, err)
	assert.Equal(t, ActionAnnounced, split.Status)
	again, err := w.announce(CorporateActionSpec{InstrumentID: l.Instrument.ID, Type: ActionSplit, EffectiveDate: NewDate(2026, 10, 8),
		Ratio: &Ratio{Numerator: 2, Denominator: 1}, Reason: "2-for-1 split"})
	require.NoError(t, err)
	assert.Equal(t, split.ID, again.ID, "announcing the same action twice is a replay")
	_, err = w.announce(CorporateActionSpec{InstrumentID: l.Instrument.ID, Type: ActionSplit, EffectiveDate: NewDate(2026, 10, 8),
		Ratio: &Ratio{Numerator: 3, Denominator: 1}, Reason: "3-for-1 split"})
	require.Error(t, err)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))

	_, err = w.applyAction(split.ID)
	require.Error(t, err, "not before its effective date")

	w.at(2026, 10, 8, 10, 0)
	w.mark(l, 7_500)
	blocked := w.accept(w.submit(w.request(b, l, market(SideBuy, 1_000_000))).ID)
	require.Equal(t, OrderRejected, blocked.Status)
	assert.Equal(t, RejectCorporateActionPending, blocked.RejectionCode, "no trading while a split is effective and unapplied")

	res, err := w.applyAction(split.ID)
	require.NoError(t, err)
	require.Len(t, res.Applications, 2)
	assert.Equal(t, ActionApplied, res.Action.Status)
	assert.NotNil(t, res.Action.AppliedAt)
	appA, appB := applicationOf(t, res.Applications, a), applicationOf(t, res.Applications, b)
	assert.Equal(t, "10000000", appA.SharesDelta.String())
	assert.Equal(t, "3333334", appB.SharesDelta.String())
	assert.True(t, appA.CashAmount.IsZero())
	require.NotNil(t, appA.JournalTransactionID)
	jt, err := w.ledger.Transaction(w.ctx, testDB, *appA.JournalTransactionID)
	require.NoError(t, err)
	assert.Equal(t, ledger.KindCorporateAction, jt.Kind)
	assert.Equal(t, "20000000", w.cust(a, ledger.CodeWallet, l.ShareAsset.ID).String())
	assert.Equal(t, "6666668", w.cust(b, ledger.CodeWallet, l.ShareAsset.ID).String())
	assert.Equal(t, "10000000", w.cust(a, ledger.CodeCorporateActions, l.ShareAsset.ID).String())
	// 10 × 150.00 = 20 × 75.00, exactly.
	assert.Equal(t, valueAt(q(10_000_000), 15_000).String(), valueAt(w.cust(a, ledger.CodeWallet, l.ShareAsset.ID), 7_500).String())
	assert.Equal(t, OrderFilled, w.trade(b, l, market(SideBuy, 1_000_000)).Status, "trading resumes once applied")
	w.at(2026, 10, 9, 8, 0)
	w.settleDue()
	require.True(t, w.cust(b, ledger.CodeUnsettledReceivable, l.ShareAsset.ID).IsZero())

	rev, err := w.announce(CorporateActionSpec{InstrumentID: l.Instrument.ID, Type: ActionReverseSplit, EffectiveDate: NewDate(2026, 10, 9),
		Ratio: &Ratio{Numerator: 1, Denominator: 3}, CashInLieuPrice: &PricePoint{Mantissa: q(22_500), Scale: 2},
		Reason: "1-for-3 reverse split"})
	require.NoError(t, err)
	_, err = w.announce(CorporateActionSpec{InstrumentID: l.Instrument.ID, Type: ActionReverseSplit, EffectiveDate: NewDate(2026, 10, 12),
		Ratio: &Ratio{Numerator: 1, Denominator: 3}, Reason: "no cash-in-lieu price"})
	require.Error(t, err, "a ratio that can leave fractions needs a cash-in-lieu price")

	beforeA := w.cust(a, ledger.CodeWallet, l.ShareAsset.ID)
	beforeB := w.cust(b, ledger.CodeWallet, l.ShareAsset.ID)
	cashB := w.cust(b, ledger.CodeWallet, w.cash.ID)
	res, err = w.applyAction(rev.ID)
	require.NoError(t, err)
	require.Len(t, res.Applications, 2)
	for _, h := range []struct {
		acct           accounts.AccountID
		before         money.Quantity
		after, cash    string
		cashBeforeHand money.Quantity
	}{
		// 20 shares / 3 = 6.666666 and 2/3 of a millionth, paid at 225.00: 150 micro-dollars.
		{a, beforeA, "6666666", "150", q(0)},
		// 7.666668 / 3 = 2.555556 exactly: nothing in lieu.
		{b, beforeB, "2555556", "0", cashB},
	} {
		app := applicationOf(t, res.Applications, h.acct)
		assert.Equal(t, h.after, w.cust(h.acct, ledger.CodeWallet, l.ShareAsset.ID).String())
		assert.Equal(t, h.cash, app.CashAmount.String())
		assert.Equal(t, h.cashBeforeHand.Add(app.CashAmount).String(), w.cust(h.acct, ledger.CodeWallet, w.cash.ID).String())
		// Value preserved exactly: before × 75.00 = after × 225.00 + cash × 100
		// (cash is in micro-dollars, value in micro-dollars × 100).
		before := valueAt(h.before, 7_500)
		after := valueAt(w.cust(h.acct, ledger.CodeWallet, l.ShareAsset.ID), 22_500)
		exactCash, err := app.ExactCashNumerator.MulDiv(q(100), app.ExactCashDenominator, money.RoundExact)
		require.NoError(t, err)
		assert.Equal(t, before.String(), after.Add(exactCash).String(), "value preserved for %s", h.acct)
		assert.Equal(t, app.CashAmount.Mul(q(100)).String(), exactCash.String(), "this cash-in-lieu needed no rounding")
	}
	assert.Equal(t, "150", w.cust(a, ledger.CodeCorporateActions, w.cash.ID).String())
}

// TestIntegration_DividendPaysHoldersOfRecordOnlyExactAmounts: record date
// Friday 2026-10-09, so under T+1 the ex-date is that Friday. A holder from
// before, a buyer on Thursday (settles on the record date) and a seller on
// the ex-date (still the holder of record) are paid exactly 0.24 per share
// on the pay date; a buyer on the ex-date is not.
func TestIntegration_DividendPaysHoldersOfRecordOnlyExactAmounts(t *testing.T) {
	w := newWorld(t)
	holder, seller, early, late := w.account(), w.account(), w.account(), w.account()
	l := w.list(1, 1)
	w.fund(holder, l.ShareAsset.ID, 10_000_000)
	w.fund(seller, l.ShareAsset.ID, 5_000_000)
	w.fund(early, w.cash.ID, 1_000_000_000)
	w.fund(late, w.cash.ID, 1_000_000_000)
	w.mark(l, 15_000)

	spec := CorporateActionSpec{InstrumentID: l.Instrument.ID, Type: ActionCashDividend, ExDate: NewDate(2026, 10, 8),
		RecordDate: NewDate(2026, 10, 9), PayDate: NewDate(2026, 10, 14), CashPerShare: &PricePoint{Mantissa: q(24), Scale: 2},
		Reason: "quarterly dividend"}
	_, err := w.announce(spec)
	require.Error(t, err, "under T+1 the ex-date is the record date")
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	spec.ExDate = NewDate(2026, 10, 9)
	div, err := w.announce(spec)
	require.NoError(t, err)
	assert.Equal(t, NewDate(2026, 10, 9), div.EffectiveDate)

	w.at(2026, 10, 8, 10, 0)
	w.mark(l, 15_000)
	require.Equal(t, OrderFilled, w.trade(early, l, market(SideBuy, 3_000_000)).Status)
	w.at(2026, 10, 9, 10, 0)
	w.mark(l, 15_000)
	require.Equal(t, OrderFilled, w.trade(late, l, market(SideBuy, 2_000_000)).Status)
	require.Equal(t, OrderFilled, w.trade(seller, l, market(SideSell, 5_000_000)).Status)

	w.at(2026, 10, 13, 10, 0)
	_, err = w.applyAction(div.ID)
	require.Error(t, err, "not before the pay date")
	w.at(2026, 10, 14, 9, 0)
	before := map[accounts.AccountID]money.Quantity{}
	for _, acct := range []accounts.AccountID{holder, seller, early, late} {
		before[acct] = w.cust(acct, ledger.CodeWallet, w.cash.ID)
	}
	res, err := w.applyAction(div.ID)
	require.NoError(t, err)
	require.Len(t, res.Applications, 3, "three holders of record; the ex-date buyer is not one")
	for _, h := range []struct {
		acct            accounts.AccountID
		entitled, cents string
	}{
		{holder, "10000000", "2400000"}, // 10 × 0.24
		{seller, "5000000", "1200000"},  // sold on the ex-date: still the holder of record
		{early, "3000000", "720000"},    // bought the day before: settles on the record date
	} {
		app := applicationOf(t, res.Applications, h.acct)
		assert.Equal(t, h.entitled, app.EntitledQuantity.String())
		assert.Equal(t, h.cents, app.CashAmount.String())
		assert.True(t, app.SharesDelta.IsZero())
		assert.Equal(t, h.cents, w.cust(h.acct, ledger.CodeDividends, w.cash.ID).String())
		assert.Equal(t, h.cents, w.cust(h.acct, ledger.CodeWallet, w.cash.ID).Sub(before[h.acct]).String())
	}
	assert.True(t, w.cust(late, ledger.CodeDividends, w.cash.ID).IsZero(), "the ex-date buyer is not paid")
	assert.Equal(t, before[late].String(), w.cust(late, ledger.CodeWallet, w.cash.ID).String())
}

// TestIntegration_CorporateActionReplayIsIdempotent: applying an applied
// action posts nothing and returns the recorded applications, sequentially
// and when many workers race to apply it.
func TestIntegration_CorporateActionReplayIsIdempotent(t *testing.T) {
	w := newWorld(t)
	holders := []accounts.AccountID{w.account(), w.account(), w.account()}
	l := w.list(1, 1)
	for i, h := range holders {
		w.fund(h, l.ShareAsset.ID, int64(i+1)*1_000_000)
	}
	countJournals := func(aid CorporateActionID) int {
		return w.count(`SELECT count(*) FROM journal_transactions t JOIN corporate_action_applications a
			ON a.journal_transaction_id = t.id WHERE a.action_id = $1`, aid)
	}
	div, err := w.announce(CorporateActionSpec{InstrumentID: l.Instrument.ID, Type: ActionCashDividend,
		ExDate: NewDate(2026, 10, 8), RecordDate: NewDate(2026, 10, 8), PayDate: NewDate(2026, 10, 9),
		CashPerShare: &PricePoint{Mantissa: q(1_005), Scale: 3}, Reason: "special dividend"})
	require.NoError(t, err)
	w.at(2026, 10, 9, 9, 0)
	first, err := w.applyAction(div.ID)
	require.NoError(t, err)
	require.False(t, first.Replayed)
	require.Len(t, first.Applications, 3)
	second, err := w.applyAction(div.ID)
	require.NoError(t, err)
	assert.True(t, second.Replayed)
	assert.ElementsMatch(t, first.Applications, second.Applications)
	assert.Equal(t, 3, countJournals(div.ID), "one posting per account, ever")
	assert.Equal(t, "2010000", w.cust(holders[1], ledger.CodeDividends, w.cash.ID).String(), "2 × 1.005")

	split, err := w.announce(CorporateActionSpec{InstrumentID: l.Instrument.ID, Type: ActionSplit, EffectiveDate: NewDate(2026, 10, 9),
		Ratio: &Ratio{Numerator: 3, Denominator: 2}, CashInLieuPrice: &PricePoint{Mantissa: q(10_000), Scale: 2}, Reason: "3-for-2 split"})
	require.NoError(t, err)
	const n = 8
	var wg sync.WaitGroup
	results := make(chan CorporateActionResult, n)
	errsCh := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var r CorporateActionResult
			errsCh <- inTxRetrying(w.ctx, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				r, err = w.svc.ApplyCorporateAction(ctx, tx, split.ID, w.sys)
				return err
			})
			results <- r
		}()
	}
	wg.Wait()
	close(results)
	close(errsCh)
	for err := range errsCh {
		require.NoError(t, err)
	}
	fresh := 0
	for r := range results {
		if !r.Replayed {
			fresh++
		}
		assert.Len(t, r.Applications, 3)
	}
	assert.Equal(t, 1, fresh, "exactly one worker applied the split")
	assert.Equal(t, 3, countJournals(split.ID))
	assert.Equal(t, "3000000", w.cust(holders[1], ledger.CodeWallet, l.ShareAsset.ID).String(), "2 shares × 3/2")
}

// TestIntegration_SymbolChangeIsATermsAmendmentWithoutPosting: the new
// symbol is a new instrument terms version; nothing is posted and nobody's
// holding changes; orders afterwards carry the new symbol.
func TestIntegration_SymbolChangeIsATermsAmendmentWithoutPosting(t *testing.T) {
	w := newWorld(t)
	acct := w.account()
	l := w.list(1, 1)
	w.fund(acct, l.ShareAsset.ID, 2_000_000)
	w.fund(acct, w.cash.ID, 1_000_000_000)
	w.mark(l, 15_000)
	_, err := w.announce(CorporateActionSpec{InstrumentID: l.Instrument.ID, Type: ActionSymbolChange, EffectiveDate: NewDate(2026, 10, 7),
		NewSymbol: l.Terms.Terms.Symbol, Reason: "no change"})
	require.Error(t, err)
	newSymbol := uniqueSymbol("RN")
	sc, err := w.announce(CorporateActionSpec{InstrumentID: l.Instrument.ID, Type: ActionSymbolChange, EffectiveDate: NewDate(2026, 10, 7),
		NewSymbol: newSymbol, Reason: "rebrand"})
	require.NoError(t, err)
	res, err := w.applyAction(sc.ID)
	require.NoError(t, err)
	require.NotNil(t, res.TermsVersion)
	assert.Equal(t, 2, res.TermsVersion.Version)
	assert.Equal(t, newSymbol, res.TermsVersion.Terms.Symbol)
	assert.Empty(t, res.Applications)
	assert.Zero(t, w.count(`SELECT count(*) FROM corporate_action_applications WHERE action_id = $1`, sc.ID))
	assert.Equal(t, "2000000", w.cust(acct, ledger.CodeWallet, l.ShareAsset.ID).String())
	hist, err := instruments.NewRepository().TermsHistory(w.ctx, testDB, l.Instrument.ID)
	require.NoError(t, err)
	require.Len(t, hist, 2)
	assert.Equal(t, l.Terms.Terms.Symbol, hist[0].Terms.Symbol, "history keeps the old symbol")
	o := w.trade(acct, l, market(SideBuy, 1_000_000))
	require.Equal(t, OrderFilled, o.Status)
	assert.Equal(t, newSymbol, o.Symbol)
	assert.Equal(t, 2, o.TermsVersion)
}

// TestIntegration_DelistingBlocksBuysThenEverything: from the effective date
// the instrument is DELISTING -- buys are refused and holders may still
// sell -- and from the final date it is DELISTED and nothing trades.
func TestIntegration_DelistingBlocksBuysThenEverything(t *testing.T) {
	w := newWorld(t)
	holder, buyer := w.account(), w.account()
	l := w.list(1, 1)
	w.fund(holder, l.ShareAsset.ID, 5_000_000)
	w.fund(buyer, w.cash.ID, 10_000_000_000)
	w.mark(l, 15_000)
	d, err := w.announce(CorporateActionSpec{InstrumentID: l.Instrument.ID, Type: ActionDelisting, EffectiveDate: NewDate(2026, 10, 8),
		FinalDate: NewDate(2026, 10, 12), Reason: "issuer delisting"})
	require.NoError(t, err)
	require.Equal(t, OrderFilled, w.trade(buyer, l, market(SideBuy, 1_000_000)).Status, "trading is normal before it begins")

	w.at(2026, 10, 8, 10, 0)
	w.mark(l, 15_000)
	pending := w.accept(w.submit(w.request(buyer, l, market(SideBuy, 1_000_000))).ID)
	assert.Equal(t, RejectCorporateActionPending, pending.RejectionCode, "an effective delisting stops buys even before it is applied")
	res, err := w.applyAction(d.ID)
	require.NoError(t, err)
	assert.Equal(t, ActionApplying, res.Action.Status)
	require.NotNil(t, res.Instrument)
	assert.Equal(t, assets.StatusDelisting, res.Instrument.Status)
	buy := w.accept(w.submit(w.request(buyer, l, market(SideBuy, 1_000_000))).ID)
	require.Equal(t, OrderRejected, buy.Status)
	assert.Contains(t, buy.RejectionReasons, risk.ReasonInstrumentCloseOnly)
	assert.Equal(t, OrderFilled, w.trade(holder, l, market(SideSell, 2_000_000)).Status, "holders may sell while delisting")
	_, err = w.applyAction(d.ID)
	require.Error(t, err, "not DELISTED before the final date")

	w.at(2026, 10, 12, 10, 0)
	w.mark(l, 15_000)
	res, err = w.applyAction(d.ID)
	require.NoError(t, err)
	assert.Equal(t, ActionApplied, res.Action.Status)
	assert.Equal(t, assets.StatusDelisted, res.Instrument.Status)
	sell := w.accept(w.submit(w.request(holder, l, market(SideSell, 1_000_000))).ID)
	require.Equal(t, OrderRejected, sell.Status)
	assert.Contains(t, sell.RejectionReasons, risk.ReasonInstrumentNotTradable)
	again, err := w.applyAction(d.ID)
	require.NoError(t, err)
	assert.True(t, again.Replayed)
}

// TestIntegration_CorporateActionsRefuseWhilePositionsCanMove: a split is
// refused while an order on the instrument is live or shares bought are not
// settled, and applies once they are; a cancelled action cannot be applied.
func TestIntegration_CorporateActionsRefuseWhilePositionsCanMove(t *testing.T) {
	w := newWorld(t)
	acct := w.account()
	l := w.list(1, 1)
	w.fund(acct, w.cash.ID, 10_000_000_000)
	w.mark(l, 15_000)
	resting := w.trade(acct, l, limit(SideBuy, 1_000_000, 14_000))
	require.Equal(t, OrderOpen, resting.Status)
	require.Equal(t, OrderFilled, w.trade(acct, l, market(SideBuy, 1_000_000)).Status)
	split, err := w.announce(CorporateActionSpec{InstrumentID: l.Instrument.ID, Type: ActionSplit, EffectiveDate: NewDate(2026, 10, 7),
		Ratio: &Ratio{Numerator: 2, Denominator: 1}, Reason: "2-for-1"})
	require.NoError(t, err)

	_, err = w.applyAction(split.ID)
	require.Error(t, err)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err), "a live order")
	r, err := w.svc.CancelAtVenue(w.ctx, testDB, resting.ID)
	require.NoError(t, err)
	_, err = w.apply(resting.ID, r)
	require.NoError(t, err)
	_, err = w.applyAction(split.ID)
	require.Error(t, err)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err), "unsettled shares")

	w.at(2026, 10, 8, 8, 0)
	w.settleDue()
	res, err := w.applyAction(split.ID)
	require.NoError(t, err)
	assert.Equal(t, "2000000", w.cust(acct, ledger.CodeWallet, l.ShareAsset.ID).String())
	assert.Equal(t, "1000000", applicationOf(t, res.Applications, acct).SharesDelta.String())

	cancelled, err := w.announce(CorporateActionSpec{InstrumentID: l.Instrument.ID, Type: ActionSplit, EffectiveDate: NewDate(2026, 10, 9),
		Ratio: &Ratio{Numerator: 2, Denominator: 1}, Reason: "withdrawn"})
	require.NoError(t, err)
	require.NoError(t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.svc.CancelCorporateAction(ctx, tx, cancelled.ID, w.sys, "board withdrew it")
		return err
	}))
	w.at(2026, 10, 9, 8, 0)
	_, err = w.applyAction(cancelled.ID)
	require.Error(t, err)
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))
	// A cancelled split no longer stops trading.
	w.at(2026, 10, 9, 10, 0)
	w.mark(l, 7_500)
	assert.Equal(t, OrderFilled, w.trade(acct, l, market(SideBuy, 1_000_000)).Status)
}
