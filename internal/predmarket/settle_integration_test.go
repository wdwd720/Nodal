//go:build integration

package predmarket

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/security"
)

// TestIntegration_ScenarioH_CreateTradeCloseResolveSettle is Scenario H end to
// end through the real database: a market is created and opened, complete
// sets are issued, shares change hands in paper trades, the market closes,
// the oracle resolves it, it settles, and conservation is proven from the
// books at every step.
func TestIntegration_ScenarioH_CreateTradeCloseResolveSettle(t *testing.T) {
	f := newFixture(t)
	a, b, c := f.customer(100), f.customer(100), f.customer(100)
	m := f.openMarket("YES", "NO")
	yes, no := m.Outcomes[0].ShareAssetID, m.Outcomes[1].ShareAssetID

	// The market and its instruments.
	assert.Equal(t, assets.StatusActive, f.instrumentStatus(m.InstrumentID))
	for _, o := range m.Outcomes {
		assert.Equal(t, assets.StatusActive, f.instrumentStatus(o.InstrumentID))
	}
	fac, err := econ.NewRegistry().Facility(f.ctx, testDB, m.FacilityID)
	require.NoError(t, err)
	assert.Equal(t, ledger.FacilityPredictionMarket, fac.Kind)

	// Complete sets: A buys 10, B buys 5.
	f.issue(m, a, 10)
	f.issue(m, b, 5)
	f.requireConserved(m, money.Quantity{})
	assert.Equal(t, "15000000", f.books(m).Escrow.String())

	// Paper trades redistribute the shares.
	f.trade(a, c, yes, 4_000_000, 2_400_000) // A sells 4 YES to C at 0.60
	f.trade(b, a, no, 5_000_000, 1_500_000)  // B sells 5 NO to A at 0.30
	f.trade(c, b, yes, 1_000_000, 650_000)   // C sells 1 YES to B at 0.65
	f.requireConserved(m, money.Quantity{})
	positions, err := f.svc.Positions(f.ctx, testDB, m.ID)
	require.NoError(t, err)
	held := map[accounts.AccountID]map[string]string{}
	for _, p := range positions {
		if held[p.AccountID] == nil {
			held[p.AccountID] = map[string]string{}
		}
		held[p.AccountID][p.OutcomeSymbol] = p.Quantity.String()
	}
	assert.Equal(t, map[string]string{"YES": "6000000", "NO": "15000000"}, held[a])
	assert.Equal(t, map[string]string{"YES": "6000000"}, held[b])
	assert.Equal(t, map[string]string{"YES": "3000000"}, held[c])

	// Close: trading halts on every path.
	m = f.closeMarket(m)
	for _, o := range m.Outcomes {
		assert.Equal(t, assets.StatusHalted, f.instrumentStatus(o.InstrumentID))
	}
	assert.Equal(t, assets.StatusCloseOnly, f.instrumentStatus(m.InstrumentID))

	// Resolve and settle.
	res := f.resolve(m, "YES")
	assert.Equal(t, ResolutionOracle, res.Resolution.Kind)
	out, err := f.trySettle(m, f.third)
	require.NoError(t, err)
	assert.Equal(t, StatusSettled, out.Market.Status)
	assert.Equal(t, "15000000", out.Settlement.EscrowBefore.String())
	assert.Equal(t, "15000000", out.Settlement.TotalPaid.String(), "Σ payouts = escrow, exactly")
	assert.True(t, out.Settlement.Residual.IsZero())
	assert.Equal(t, 3, out.Settlement.HolderCount)
	assert.Equal(t, RoundingExact, out.Settlement.RoundingPolicy)

	// Every holder was paid one unit per YES share and nothing for NO.
	assert.Equal(t, "96900000", f.wallet(a, f.collateral).String())  // 90 - 1.5 + 2.4 + 6
	assert.Equal(t, "101850000", f.wallet(b, f.collateral).String()) // 95 + 1.5 - 0.65 + 6
	assert.Equal(t, "101250000", f.wallet(c, f.collateral).String()) // 100 - 2.4 + 0.65 + 3
	total := f.wallet(a, f.collateral).Add(f.wallet(b, f.collateral)).Add(f.wallet(c, f.collateral))
	assert.Equal(t, units(300).String(), total.String(), "collateral is conserved across the customers")
	for _, acct := range []accounts.AccountID{a, b, c} {
		assert.True(t, f.wallet(acct, yes).IsZero())
		assert.True(t, f.wallet(acct, no).IsZero())
	}

	// The books are empty and reconcile; the record says so.
	after := f.requireConserved(m, money.Quantity{})
	assert.True(t, after.Escrow.IsZero())
	for i := range after.SharesOutstanding {
		assert.True(t, after.SharesOutstanding[i].IsZero())
		assert.True(t, after.SharesIssued[i].IsZero())
	}
	assert.Equal(t, assets.StatusHalted, f.instrumentStatus(m.InstrumentID))
	settled, err := f.svc.ListByStatus(f.ctx, testDB, StatusSettled, 500)
	require.NoError(t, err)
	found := false
	for _, s := range settled {
		found = found || s.ID == m.ID
	}
	assert.True(t, found, "the market is listed as SETTLED")
	var rec reconciliation.Record
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rec, err = f.svc.ReconcileEscrow(ctx, tx, ReconcileRequest{MarketID: m.ID})
		return err
	}))
	assert.Equal(t, reconciliation.StatusMatched, rec.Status)
	drift, err := ledger.VerifyBalances(f.ctx, testDB)
	require.NoError(t, err)
	assert.Empty(t, drift, "no ledger balance drifted from its entries")

	// Every economic action went through the kernel: one completed intent and
	// one applied effect per issuance and per payout, each with evidence.
	assert.Equal(t, 5, f.count(`SELECT count(*) FROM economic_effects e JOIN economic_intents i ON i.id = e.intent_id
		WHERE e.subsystem = 'PREDICTION_MARKETS' AND e.status = 'APPLIED' AND i.status = 'COMPLETED' AND e.evidence_ref IS NOT NULL
		  AND i.instrument_id = $1`, m.InstrumentID))
	assert.Equal(t, 5, f.count(`SELECT count(*) FROM advanced_risk_decisions d JOIN economic_intents i ON i.id = d.intent_id
		WHERE d.verdict = 'ALLOW' AND i.instrument_id = $1`, m.InstrumentID))
	assert.Equal(t, 6, f.count(`SELECT count(*) FROM prediction_market_transitions WHERE market_id = $1`, m.ID),
		"NONE->DRAFT->OPEN->CLOSED->RESOLVING->RESOLVED->SETTLED")
}

// TestIntegration_AWinningSharePaysOneUnitAndALosingSharePaysNothing on a
// three-outcome market: a holder of only winners, only losers, and both.
func TestIntegration_AWinningSharePaysOneUnitAndALosingSharePaysNothing(t *testing.T) {
	f := newFixture(t)
	winner, loser, mixed := f.customer(50), f.customer(50), f.customer(50)
	m := f.openMarket("RED", "GREEN", "BLUE")
	red, green, blue := m.Outcomes[0].ShareAssetID, m.Outcomes[1].ShareAssetID, m.Outcomes[2].ShareAssetID
	f.issue(m, winner, 4)
	f.issue(m, loser, 4)
	f.issue(m, mixed, 2)
	// The winner gives away every loser it holds; the loser gives away its winners.
	f.trade(winner, mixed, green, 4_000_000, 0)
	f.trade(winner, mixed, blue, 4_000_000, 0)
	f.trade(loser, mixed, red, 4_000_000, 0)
	// winner: 4 RED; loser: 4 GREEN, 4 BLUE; mixed: 6 RED, 6 GREEN, 6 BLUE.
	assert.Equal(t, "4000000", f.wallet(winner, red).String())
	assert.Equal(t, "6000000", f.wallet(mixed, red).String())
	assert.Equal(t, "0", f.wallet(loser, red).String())

	f.resolve(m, "RED")
	before := map[accounts.AccountID]money.Quantity{
		winner: f.wallet(winner, f.collateral), loser: f.wallet(loser, f.collateral), mixed: f.wallet(mixed, f.collateral),
	}
	out, err := f.trySettle(m, f.third)
	require.NoError(t, err)
	paid := func(a accounts.AccountID) string { return f.wallet(a, f.collateral).Sub(before[a]).String() }
	assert.Equal(t, "4000000", paid(winner), "four winning shares pay exactly four units")
	assert.Equal(t, "0", paid(loser), "losing shares pay nothing")
	assert.Equal(t, "6000000", paid(mixed), "only the winning shares of a mixed holding pay")
	assert.Equal(t, "10000000", out.Settlement.TotalPaid.String())
	for _, a := range []accounts.AccountID{winner, loser, mixed} {
		for _, o := range m.Outcomes {
			assert.True(t, f.wallet(a, o.ShareAssetID).IsZero(), "every share of every outcome is retired")
		}
	}
	assert.Len(t, out.Lines, 3, "the loser's shares are retired by its own zero payout")
	for _, l := range out.Lines {
		assert.Equal(t, econ.StatusCompleted, l.Intent.Status)
		assert.Equal(t, econ.KindResolvePredictionPosition, l.Intent.Kind)
		assert.Equal(t, econ.EffectApplied, l.Effect.Status)
	}
}

// TestIntegration_VoidRefundsABinaryMarketExactly: each share refunds half a
// unit; with whole-share holdings the refunds are the escrow exactly.
func TestIntegration_VoidRefundsABinaryMarketExactly(t *testing.T) {
	f := newFixture(t)
	a, b := f.customer(20), f.customer(20)
	m := f.openMarket("YES", "NO")
	f.issue(m, a, 6)
	f.issue(m, b, 2)
	f.trade(a, b, m.Outcomes[0].ShareAssetID, 5_000_000, 3_000_000)
	f.closeMarket(m)
	before := f.wallet(a, f.collateral).Add(f.wallet(b, f.collateral))

	out, err := f.tryVoid(f.get(m), f.resolver)
	require.NoError(t, err)
	assert.Equal(t, StatusVoided, out.Market.Status)
	st := out.Settlement
	assert.Equal(t, SettlementRefund, st.Kind)
	require.NotNil(t, st.RefundRatePerShare)
	assert.Equal(t, "500000", st.RefundRatePerShare.String())
	assert.Equal(t, "8000000", st.EscrowBefore.String())
	assert.Equal(t, "8000000", st.TotalPaid.String(), "a binary void refunds the escrow exactly")
	assert.True(t, st.Residual.IsZero())
	assert.Equal(t, RoundingFloorPerHolder, st.RoundingPolicy)
	// a held 1 YES + 6 NO = 7 shares -> 3.5 units; b held 7 YES + 2 NO = 9 -> 4.5.
	after := f.wallet(a, f.collateral).Add(f.wallet(b, f.collateral))
	assert.Equal(t, "8000000", after.Sub(before).String())
	books := f.requireConserved(m, money.Quantity{})
	assert.True(t, books.Escrow.IsZero())
	for _, o := range m.Outcomes {
		assert.Equal(t, assets.StatusHalted, f.instrumentStatus(o.InstrumentID))
	}
	assert.Equal(t, assets.StatusHalted, f.instrumentStatus(m.InstrumentID))
}

// TestIntegration_VoidRefundsAMultiOutcomeMarketAndDisclosesTheResidual: with
// three outcomes a share refunds floor(1,000,000 / 3) = 333,333; each complete
// set leaves 1 base unit behind, plus less than one base unit per holder of a
// fractional holding. Conservation includes the residual, which stays on the
// facility's books and in the refund record.
func TestIntegration_VoidRefundsAMultiOutcomeMarketAndDisclosesTheResidual(t *testing.T) {
	f := newFixture(t)
	a, b, c := f.customer(20), f.customer(20), f.customer(20)
	m := f.openMarket("A", "B", "C")
	f.issue(m, a, 10)
	// One outcome per holder, plus a fractional sliver.
	f.trade(a, b, m.Outcomes[1].ShareAssetID, 10_000_000, 0)
	f.trade(a, c, m.Outcomes[2].ShareAssetID, 9_500_001, 0)
	before := map[accounts.AccountID]money.Quantity{a: f.wallet(a, f.collateral), b: f.wallet(b, f.collateral), c: f.wallet(c, f.collateral)}

	out, err := f.tryVoid(f.get(m), f.resolver)
	require.NoError(t, err)
	st := out.Settlement
	assert.Equal(t, "333333", st.RefundRatePerShare.String())
	// a: 10 A + 0.499999 C = 10.499999 shares -> floor(10,499,999 × 333,333 / 1e6) = 3,499,996
	// b: 10 B -> 3,333,330;  c: 9.500001 C -> floor(9,500,001 × 333,333 / 1e6) = 3,166,663
	refunded := func(acct accounts.AccountID) string { return f.wallet(acct, f.collateral).Sub(before[acct]).String() }
	assert.Equal(t, "3499996", refunded(a))
	assert.Equal(t, "3333330", refunded(b))
	assert.Equal(t, "3166663", refunded(c))
	assert.Equal(t, "9999989", st.TotalPaid.String())
	assert.Equal(t, "11", st.Residual.String(), "10 sets × 1 base unit, plus the fractional holdings' rounding")
	assert.Equal(t, st.EscrowBefore.String(), st.TotalPaid.Add(st.Residual).String(), "escrow = refunds + residual")

	// The residual is disclosed and stays on the books: the identity holds
	// with it, and the reconciliation knows it.
	b2 := f.requireConserved(m, st.Residual)
	assert.Equal(t, "11", b2.Escrow.String())
	assert.Equal(t, "11", b2.Collateral.String())
	var rec reconciliation.Record
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rec, err = f.svc.ReconcileEscrow(ctx, tx, ReconcileRequest{MarketID: m.ID})
		return err
	}))
	assert.Equal(t, reconciliation.StatusMatched, rec.Status)
	var expected map[string]any
	require.NoError(t, json.Unmarshal(rec.Expected, &expected))
	assert.Equal(t, "11", expected["residual"], "the reconciliation expects the disclosed residual")
}

// TestIntegration_VoidBeforeCloseHaltsTradingAndRefunds: an OPEN market can
// be voided; trading halts on every outcome at once.
func TestIntegration_VoidBeforeCloseHaltsTradingAndRefunds(t *testing.T) {
	f := newFixture(t)
	a := f.customer(10)
	m := f.openMarket("YES", "NO")
	f.issue(m, a, 4)
	out, err := f.tryVoid(m, f.resolver)
	require.NoError(t, err)
	assert.Equal(t, "4000000", out.Settlement.TotalPaid.String())
	assert.Equal(t, units(10).String(), f.wallet(a, f.collateral).String(), "every unit came back")
	for _, o := range m.Outcomes {
		assert.Equal(t, assets.StatusHalted, f.instrumentStatus(o.InstrumentID))
	}
	assert.Equal(t, assets.StatusHalted, f.instrumentStatus(m.InstrumentID))
	_, err = f.tryIssue(f.positionRequest(m, a, 1))
	require.Error(t, err, "a voided market issues nothing")

	// Voiding again changes nothing.
	again, err := f.tryVoid(f.get(m), f.resolver)
	require.NoError(t, err)
	assert.True(t, again.Replay)
	assert.Equal(t, out.Settlement.ID, again.Settlement.ID)

	// The creator may not void (dual control), and a settled market cannot be voided.
	m2 := f.openMarket("YES", "NO")
	_, err = f.tryVoid(m2, f.creator)
	requireCode(t, err, errs.CodeForbidden)
	f.resolve(m2, "NO")
	_, err = f.trySettle(m2, f.third)
	require.NoError(t, err)
	_, err = f.tryVoid(f.get(m2), f.resolver)
	requireCode(t, err, errs.CodeInvalidStateTransition)
}

// TestIntegration_ConcurrentSettlementsSettleOnce: N settlement attempts in
// separate transactions pay every holder exactly once.
func TestIntegration_ConcurrentSettlementsSettleOnce(t *testing.T) {
	f := newFixture(t)
	holders := []accounts.AccountID{f.customer(10), f.customer(10), f.customer(10)}
	m := f.openMarket("YES", "NO")
	for i, h := range holders {
		f.issue(m, h, int64(i+1))
	}
	f.resolve(m, "NO")
	before := make([]money.Quantity, len(holders))
	for i, h := range holders {
		before[i] = f.wallet(h, f.collateral)
	}

	const n = 8
	var wg sync.WaitGroup
	results := make(chan error, n)
	fresh := make(chan bool, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := f.trySettle(m, f.third)
			results <- err
			fresh <- err == nil && !out.Replay
		}()
	}
	wg.Wait()
	close(results)
	close(fresh)
	for err := range results {
		require.NoError(t, err)
	}
	settledOnce := 0
	for ok := range fresh {
		if ok {
			settledOnce++
		}
	}
	assert.Equal(t, 1, settledOnce, "exactly one attempt settles; the rest find it settled")
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM prediction_settlements WHERE market_id = $1`, m.ID))
	for i, h := range holders {
		assert.Equal(t, units(int64(i+1)).String(), f.wallet(h, f.collateral).Sub(before[i]).String(), "holder %d paid once", i)
		assert.Equal(t, 1, f.count(`SELECT count(*) FROM economic_intents WHERE account_id = $1 AND kind = 'RESOLVE_PREDICTION_POSITION'`, h))
	}
	assert.True(t, f.books(m).Escrow.IsZero())
}

// TestIntegration_ACustomerCannotBlockSettlementByClaimingAnIntentKey:
// economic intents are unique per (account, idempotency key) across every
// subsystem, so a payout keyed only by market and account could be blocked by
// a customer who used that key first anywhere. Payout intents are keyed by
// the settlement attempt; exactly once rests on the effect key.
func TestIntegration_ACustomerCannotBlockSettlementByClaimingAnIntentKey(t *testing.T) {
	f := newFixture(t)
	a := f.customer(10)
	m := f.openMarket("YES", "NO")
	f.issue(m, a, 2)
	f.resolve(m, "YES")

	other := econ.NewIntentStore(f.clk, event.NewOutbox(f.clk), audit.NewWriterWithBuildVersion("another-lane"))
	qty := money.QuantityFromInt64(1)
	for _, key := range []string{"predmarket:payout:" + m.ID.String() + ":" + a.String(), "predmarket:payout:" + m.ID.String()} {
		require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, _, err := other.Submit(ctx, tx, econ.Intent{
				AccountID: a, Kind: econ.KindTransferValue, Mode: gates.ModeSandbox, Capability: gates.P2PTransfers,
				ActorType: security.ActorUser, ActorID: "user", Quantity: &qty, IdempotencyKey: key,
			})
			return err
		}))
	}
	before := f.wallet(a, f.collateral)
	out, err := f.trySettle(m, f.third)
	require.NoError(t, err)
	assert.Equal(t, units(2).String(), f.wallet(a, f.collateral).Sub(before).String())
	require.Len(t, out.Lines, 1)
	assert.Contains(t, out.Lines[0].Intent.IdempotencyKey, out.Settlement.ID.String(), "the payout intent names its settlement")
	assert.Equal(t, "predmarket:payout:"+m.ID.String()+":"+a.String(), out.Lines[0].Effect.Key, "the effect key is what makes it exactly once")
}

// TestIntegration_ConcurrentIssuancesCannotOverspendCollateral: the capital
// reservation serializes issuances on one account's collateral.
func TestIntegration_ConcurrentIssuancesCannotOverspendCollateral(t *testing.T) {
	f := newFixture(t)
	a := f.customer(10)
	m := f.openMarket("YES", "NO")
	const n = 8
	var wg sync.WaitGroup
	errsCh := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.tryIssue(f.positionRequest(m, a, 3))
			errsCh <- err
		}()
	}
	wg.Wait()
	close(errsCh)
	ok := 0
	for err := range errsCh {
		if err == nil {
			ok++
			continue
		}
		code := errs.CodeOf(err)
		assert.Contains(t, []errs.Code{errs.CodeInsufficientBuyingPower, errs.CodeRiskRejected}, code, "%v", err)
	}
	assert.Equal(t, 3, ok, "10 units buy three lots of 3 sets and no more")
	assert.Equal(t, "1000000", f.wallet(a, f.collateral).String())
	assert.Equal(t, "9000000", f.books(m).Escrow.String())
	f.requireConserved(m, money.Quantity{})
}

// TestIntegration_IssuanceIsExactlyOnce: a replay returns the first issuance
// and posts nothing; the same key for a different request is refused.
func TestIntegration_IssuanceIsExactlyOnce(t *testing.T) {
	f := newFixture(t)
	a := f.customer(10)
	m := f.openMarket("YES", "NO")
	req := f.positionRequest(m, a, 2)
	first, err := f.tryIssue(req)
	require.NoError(t, err)
	again, err := f.tryIssue(req)
	require.NoError(t, err)
	assert.True(t, again.Replay)
	assert.Equal(t, first.Intent.ID, again.Intent.ID)
	assert.Equal(t, first.JournalTransactionID, again.JournalTransactionID)
	assert.Equal(t, "2000000", f.books(m).Escrow.String(), "one issuance, however often it is asked for")
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM journal_transactions WHERE kind = 'PREDICTION_SET_ISSUANCE' AND reference_id = $1`, first.Intent.ID.String()))

	changed := req
	changed.Quantity = units(3)
	_, err = f.tryIssue(changed)
	requireCode(t, err, errs.CodeInvalidIdempotencyReuse)
}

// TestIntegration_CompleteSetRedemptionReturnsCollateral: N sets back for N
// units while the market is open, and never more than the account holds.
func TestIntegration_CompleteSetRedemptionReturnsCollateral(t *testing.T) {
	f := newFixture(t)
	a, b := f.customer(10), f.customer(10)
	m := f.openMarket("YES", "NO", "VOID_TIE")
	f.issue(m, a, 5)
	res, err := f.redeem(m, a, 2)
	require.NoError(t, err)
	assert.Equal(t, econ.KindResolvePredictionPosition, res.Intent.Kind)
	assert.Equal(t, units(7).String(), f.wallet(a, f.collateral).String())
	for _, o := range m.Outcomes {
		assert.Equal(t, units(3).String(), f.wallet(a, o.ShareAssetID).String())
	}
	f.requireConserved(m, money.Quantity{})

	// Without a complete set there is nothing to redeem.
	f.trade(a, b, m.Outcomes[2].ShareAssetID, 2_000_000, 0)
	_, err = f.redeem(m, a, 2)
	require.Error(t, err)
	assert.Contains(t, []errs.Code{errs.CodeRiskRejected, errs.CodeInsufficientBuyingPower}, errs.CodeOf(err))
	_, err = f.redeem(m, a, 1)
	require.NoError(t, err, "one complete set remains")
	f.requireConserved(m, money.Quantity{})
}

// TestProp_SettlementPaysExactlyTheEscrow is the payout conservation property
// against the real database: after random issuances, redemptions and paper
// trades through the service, settlement pays Σ payouts = escrow exactly,
// each holder exactly its winning shares, and leaves the escrow and every
// outcome's outstanding shares at zero.
func TestProp_SettlementPaysExactlyTheEscrow(t *testing.T) {
	requireEnv(t)
	rapid.Check(t, func(rt *rapid.T) {
		f := newFixture(t)
		n := rapid.IntRange(2, 4).Draw(rt, "outcomes")
		names := []string{"W", "X", "Y", "Z"}[:n]
		m := f.openMarket(names...)
		holders := make([]accounts.AccountID, rapid.IntRange(1, 4).Draw(rt, "holders"))
		for i := range holders {
			holders[i] = f.customer(1000)
		}
		steps := rapid.IntRange(1, 10).Draw(rt, "steps")
		for i := 0; i < steps; i++ {
			h := holders[rapid.IntRange(0, len(holders)-1).Draw(rt, "holder")]
			switch rapid.IntRange(0, 2).Draw(rt, "op") {
			case 0:
				f.issue(m, h, rapid.Int64Range(1, 20).Draw(rt, "sets"))
			case 1:
				_, _ = f.redeem(m, h, rapid.Int64Range(1, 5).Draw(rt, "redeem"))
			default:
				to := holders[rapid.IntRange(0, len(holders)-1).Draw(rt, "to")]
				o := m.Outcomes[rapid.IntRange(0, n-1).Draw(rt, "outcome")]
				have := f.wallet(h, o.ShareAssetID)
				if to == h || !have.IsPositive() {
					continue
				}
				amt, _ := have.Int64()
				amt = rapid.Int64Range(1, amt).Draw(rt, "amount")
				f.trade(h, to, o.ShareAssetID, amt, rapid.Int64Range(0, 1_000_000).Draw(rt, "price"))
			}
			if v := f.books(m).Violations(money.Quantity{}); len(v) > 0 {
				rt.Fatalf("identity broken mid-run: %v", v)
			}
		}
		winning := rapid.IntRange(0, n-1).Draw(rt, "winning")
		escrow := f.books(m).Escrow
		winners := map[accounts.AccountID]money.Quantity{}
		before := map[accounts.AccountID]money.Quantity{}
		for _, h := range holders {
			winners[h] = f.wallet(h, m.Outcomes[winning].ShareAssetID)
			before[h] = f.wallet(h, f.collateral)
		}
		f.resolve(m, names[winning])
		out, err := f.trySettle(m, f.third)
		if err != nil {
			rt.Fatalf("settle: %v", err)
		}
		if out.Settlement.TotalPaid.Cmp(escrow) != 0 {
			rt.Fatalf("paid %s of escrow %s", out.Settlement.TotalPaid, escrow)
		}
		for _, h := range holders {
			if got := f.wallet(h, f.collateral).Sub(before[h]); got.Cmp(winners[h]) != 0 {
				rt.Fatalf("holder paid %s for %s winning shares", got, winners[h])
			}
		}
		b := f.books(m)
		if !b.Escrow.IsZero() || len(b.Violations(money.Quantity{})) > 0 {
			rt.Fatalf("books after settlement: escrow %s, violations %v", b.Escrow, b.Violations(money.Quantity{}))
		}
		for i := range b.SharesOutstanding {
			if !b.SharesOutstanding[i].IsZero() {
				rt.Fatalf("outcome %d still has %s outstanding", i, b.SharesOutstanding[i])
			}
		}
	})
}

// TestProp_PositionsNeverSpendWhatTheAccountDoesNotHold: over any sequence of
// issuances and redemptions, an issuance succeeds exactly when the account
// holds the collateral and a redemption exactly when it holds the complete
// sets; balances follow an exact model and never go negative, and the escrow
// identity holds after every step.
func TestProp_PositionsNeverSpendWhatTheAccountDoesNotHold(t *testing.T) {
	f := newFixture(t)
	rapid.Check(t, func(rt *rapid.T) {
		fundedUnits := rapid.Int64Range(0, 12).Draw(rt, "funded_units")
		a := f.customer(fundedUnits)
		m := f.openMarket("YES", "NO", "MAYBE")
		cash, sets := fundedUnits, int64(0)
		for i, steps := 0, rapid.IntRange(1, 8).Draw(rt, "steps"); i < steps; i++ {
			n := rapid.Int64Range(1, 6).Draw(rt, "sets")
			if rapid.Bool().Draw(rt, "issue") {
				_, err := f.tryIssue(f.positionRequest(m, a, n))
				if (err == nil) != (n <= cash) {
					rt.Fatalf("issue %d sets with %d units: err=%v", n, cash, err)
				}
				if err == nil {
					cash, sets = cash-n, sets+n
				}
			} else {
				_, err := f.redeem(m, a, n)
				if (err == nil) != (n <= sets) {
					rt.Fatalf("redeem %d sets holding %d: err=%v", n, sets, err)
				}
				if err == nil {
					cash, sets = cash+n, sets-n
				}
			}
			if got := f.wallet(a, f.collateral); got.Cmp(units(cash)) != 0 {
				rt.Fatalf("collateral %s, model %d units", got, cash)
			}
			for _, o := range m.Outcomes {
				if got := f.wallet(a, o.ShareAssetID); got.Cmp(units(sets)) != 0 {
					rt.Fatalf("outcome %s: %s shares, model %d sets", o.Symbol, got, sets)
				}
			}
			if v := f.books(m).Violations(money.Quantity{}); len(v) > 0 {
				rt.Fatalf("identity broken: %v", v)
			}
		}
	})
}
