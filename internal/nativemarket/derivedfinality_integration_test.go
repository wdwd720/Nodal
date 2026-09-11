//go:build integration

package nativemarket

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Proceeds are as final as what paid for them (D-124, F-230, F-e2e-1).
//
// The finding: every earning this system minted was REVERSIBLE unconditionally,
// and the only writer that promotes a lot out of REVERSIBLE reads
// `credit_fundings.lot_id`, which an earning never has. So five of the six
// origins `valuedomain.SandboxPolicy` marks withdrawable could never be
// withdrawn on any deployment, and the browser audit saw the same thing from
// the outside: a verified customer with a tokenised destination, holding a
// permitted origin, held at FUNDING_NOT_SETTLED with nothing able to settle it.
//
// This drives the case the seeded sandbox tier actually produces.

// fundAs mints a lot of a stated origin and finality, which `fund` does not:
// the seeded demo deployment grants PROMOTIONAL, UNFUNDED Credits, and that is
// what a sandbox trader trades with.
func (f *fixture) fundAs(account accounts.AccountID, origin valuedomain.CreditOrigin, fin valuedomain.FundingFinality, amount int64) credit.Lot {
	f.t.Helper()
	var lot credit.Lot
	require.NoError(f.t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			lot, err = f.credits.Issue(ctx, tx, credit.IssueRequest{
				AccountID: account, Quantity: q(amount),
				Origin: origin, Finality: fin,
				Reference:      credit.Reference{Type: "test_fund", ID: uuid.NewString()},
				IdempotencyKey: "fund-" + uuid.NewString(),
				Reason:         "test funding", EffectiveAt: f.clk.Now(),
			})
			return err
		}))
	return lot
}

// TestIntegration_ASandboxTraderEarnsProceedsThatCanReachAPayout is the journey
// a sandbox tier produces: a trader buys into a market and sells back out, and
// the MARKET_TRADING_PROCEEDS they end up with clear the payout engine under
// SandboxPolicy at PAYOUT_KYC.
//
// It asserts the eligibility DECISION rather than driving payout.Service.Create,
// because Create adds a provider, a destination and a quote to the fixture and
// none of them is what this finding is about; the decision is exactly what
// Create consults, and internal/payout's own suite drives the rest.
//
// # Why the trader is funded with a PURCHASE and not a grant (D-131, F-261)
//
// This test used to fund the trader with the demo seeder's PROMOTIONAL,
// UNFUNDED Credits, on the reasoning that the grant is what a sandbox
// deployment actually hands somebody. The proceeds of that round trip are
// exactly goal section 23's forbidden pattern -- nonwithdrawable source, trade,
// payout-eligible balance -- and D-131 closes it: a derived lot inherits the
// most restricted ORIGIN among its parents as well as their finality, so a
// grant round-tripped through a market is still a grant and still cannot leave.
// TestAuditWV2_AGrantThePolicyForbidsCannotBeTradedIntoWithdrawableValue
// asserts that directly.
//
// What F-230 is about survives the change: an EARNING funded by value the
// policy permits must be able to reach a payout, and before D-124 none could,
// because every earning was minted REVERSIBLE for ever. So the trader is funded
// with a settled purchase -- what a person who bought Credits and whose card
// payment has cleared holds -- and the assertion is unchanged.
func TestIntegration_ASandboxTraderEarnsProceedsThatCanReachAPayout(t *testing.T) {
	f := newFixture(t)

	// A trader of their own, holding one settled purchase and nothing else. The
	// fixture's own trader is funded the same way, but a dedicated account
	// makes the consumption order irrelevant to what this proves.
	//
	// They are funded with more than they spend because the market's
	// concentration limit refuses an order that puts a whole balance into one
	// creator's assets; the request below is therefore for the WHOLE remaining
	// balance, so the decision has to reach the earning rather than covering
	// itself out of the purchase that PURCHASED's consumption rank puts first.
	trader := newAccount(t)
	grant := f.fundAs(trader, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 40_000_000_000)

	buy, err := f.buy(trader, 1_000_000_000, money.Quantity{})
	require.NoError(t, err)
	sell, err := f.sell(trader, buy.Fill.AssetsOut, money.Quantity{})
	require.NoError(t, err)
	require.True(t, sell.Fill.CreditsOut.IsPositive(), "fixture check: the sale returned Credits")

	lots, err := f.credits.Lots(f.ctx, testDB, trader)
	require.NoError(t, err)
	var proceeds *credit.Lot
	for i := range lots {
		if lots[i].Origin == valuedomain.OriginMarketTradingProceeds {
			proceeds = &lots[i]
			break
		}
	}
	require.NotNil(t, proceeds, "the sale minted MARKET_TRADING_PROCEEDS")

	// The pool paid it, and the pool's record says what the pool was paid with.
	parents, err := f.credits.ParentsOf(f.ctx, testDB, proceeds.ID)
	require.NoError(t, err)
	require.NotEmpty(t, parents,
		"F-230: proceeds recorded nothing about what funded them, so nothing could ever promote them")
	assert.Equal(t, grant.ID, parents[0].LotID,
		"the Credits that came out of the pool are the ones the trader put in")
	assert.Equal(t, valuedomain.FinalitySettled, proceeds.Finality,
		"proceeds funded by a settled purchase are settled: nothing external can claw them back")
	assert.True(t, proceeds.Finality.PayoutEligible())
	assert.Equal(t, valuedomain.OriginPurchased, proceeds.OriginFloor,
		"and the origin floor is the purchase, which the sandbox policy releases (D-131)")

	// And the payout engine agrees, under the policy the sandbox tier runs.
	policy := valuedomain.SandboxPolicy()
	require.True(t, policy.Rule(valuedomain.OriginMarketTradingProceeds).PayoutAllowed,
		"fixture check: the sandbox policy permits this origin")
	whole := money.Quantity{}
	for _, l := range lots {
		whole = whole.Add(l.Remaining)
	}
	require.True(t, whole.Cmp(proceeds.Remaining) > 0, "fixture check: the purchase left a remainder")
	decision, err := payout.NewEngine(f.credits).Evaluate(f.ctx, testDB, payout.EligibilityInput{
		AccountID:  trader,
		Requested:  whole,
		Policy:     policy,
		Verified:   valuedomain.VerificationPayoutKYC,
		ActiveCaps: map[valuedomain.CapabilityKey]bool{valuedomain.CapPayoutReserve: true},
		Now:        f.clk.Now(),
		// The facts the conversion adapter reads: a verified destination, a
		// provider that can pay it, an account in good standing (D-120).
		DestinationVerified:   true,
		ProviderSupports:      true,
		SanctionsState:        "CLEAR",
		JurisdictionSupported: true,
	})
	require.NoError(t, err)
	assert.True(t, decision.Sufficient(),
		"F-230: an earning of a permitted origin could not reach the payout path at all: %v",
		decision.Reasons)
	// The trader kept most of their purchase, so the decision draws on that as
	// well as on the proceeds; what matters is that the EARNING is among what
	// may leave, which is the thing F-230 made impossible, and that every lot
	// in the decision has a floor the policy releases (D-131).
	sawProceeds := false
	for _, lot := range decision.Lots {
		if lot.ID == proceeds.ID {
			sawProceeds = true
		}
		assert.Truef(t, policy.Rule(lot.OriginFloor).PayoutAllowed,
			"a lot with floor %s reached the decision and the policy forbids that floor", lot.OriginFloor)
	}
	assert.True(t, sawProceeds, "F-230: the earning itself could not reach the payout path")
}

// TestIntegration_ProceedsOfAReversiblePurchaseAreReversible is the other
// direction, and it is the one that keeps the fix honest: a trader spending
// Credits a card issuer can still take back does not get payout-eligible
// proceeds by selling them into a market and out again.
//
// It is driven against a market with TWO contributors, because the
// one-contributor version proved less than it looked (F-262, D-132). With only
// the reversible trader in the pool, arrival order hands them back their own
// lot and the assertion holds for a reason that has nothing to do with the
// rule. Add an earlier contributor whose Credits are settled -- which every
// real market has, and which a seeded deployment has before anybody signs up --
// and arrival order hands the reversible trader the SETTLED contribution
// instead, minting them payout-eligible proceeds at birth and leaving their own
// reversible Credits in the pool for whoever sells next.
//
// D-132 draws the pool down WORST first, so a seller is never handed provenance
// better than the pool's worst outstanding contribution.
func TestIntegration_ProceedsOfAReversiblePurchaseAreReversible(t *testing.T) {
	f := newFixture(t)

	// The earlier, ordinary contributor: a card payment out of its dispute
	// window, which is what most of a live pool is.
	settledTrader := newAccount(t)
	settled := f.fundAs(settledTrader, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 40_000_000_000)
	_, err := f.buy(settledTrader, 1_000_000_000, money.Quantity{})
	require.NoError(t, err)

	// The trader whose money can still be taken back. They sell back LESS than
	// the settled trader put in, so an arrival-ordered draw-down would be
	// covered by the earlier contribution alone.
	trader := newAccount(t)
	f.fundAs(trader, valuedomain.OriginPurchased, valuedomain.FinalityReversible, 40_000_000_000)
	buy, err := f.buy(trader, 1_000_000_000, money.Quantity{})
	require.NoError(t, err)
	half, err := buy.Fill.AssetsOut.MulDiv(
		money.QuantityFromInt64(1), money.QuantityFromInt64(2), money.RoundDown,
	)
	require.NoError(t, err)
	_, err = f.sell(trader, half, money.Quantity{})
	require.NoError(t, err)

	lots, err := f.credits.Lots(f.ctx, testDB, trader)
	require.NoError(t, err)
	found := false
	for _, lot := range lots {
		if lot.Origin != valuedomain.OriginMarketTradingProceeds {
			continue
		}
		found = true
		assert.Equal(t, valuedomain.FinalityReversible, lot.Finality,
			"a sale cannot launder a reversible purchase into payout-eligible value")
		assert.False(t, lot.Finality.PayoutEligible())
		parents, perr := f.credits.ParentsOf(f.ctx, testDB, lot.ID)
		require.NoError(t, perr)
		require.NotEmpty(t, parents)
		for _, p := range parents {
			assert.NotEqual(t, settled.ID, p.LotID,
				"the seller was handed another trader's settled contribution (F-262)")
		}
	}
	require.True(t, found, "the sale minted MARKET_TRADING_PROCEEDS")

	// Nothing promotes it while the purchase can still be taken back.
	var res credit.SettleDerivedResult
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var serr error
			res, serr = f.credits.SettleDerived(ctx, tx, 100)
			return serr
		}))
	assert.Zero(t, res.Promoted)
}

// TestIntegration_AnHonestSellerWaitsForTheWorstContributionToSettle is the
// mirror, and it is what D-132 costs.
//
// A contributes REVERSIBLE Credits and does not sell. B contributes SETTLED
// Credits and sells. B's proceeds are REVERSIBLE, because the pool's worst
// outstanding contribution is A's and B is drawn against it -- so an honest
// seller waits for somebody else's card payment to clear.
//
// That is the direction fail-closed errs in, and it is not permanent: when A's
// funding settles, credit.Service.SettleDerived promotes B's proceeds on the
// next pass. The alternative -- handing B the best provenance in the pool -- is
// F-262, where the pool laundered A's money for whoever sold next.
func TestIntegration_AnHonestSellerWaitsForTheWorstContributionToSettle(t *testing.T) {
	f := newFixture(t)

	reversibleTrader := newAccount(t)
	reversible := f.fundAs(reversibleTrader, valuedomain.OriginPurchased,
		valuedomain.FinalityReversible, 40_000_000_000)
	_, err := f.buy(reversibleTrader, 1_000_000_000, money.Quantity{})
	require.NoError(t, err)

	seller := newAccount(t)
	f.fundAs(seller, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 40_000_000_000)
	buy, err := f.buy(seller, 1_000_000_000, money.Quantity{})
	require.NoError(t, err)
	half, err := buy.Fill.AssetsOut.MulDiv(
		money.QuantityFromInt64(1), money.QuantityFromInt64(2), money.RoundDown,
	)
	require.NoError(t, err)
	_, err = f.sell(seller, half, money.Quantity{})
	require.NoError(t, err)

	proceeds := proceedsOf(t, f, seller)
	require.Equal(t, valuedomain.FinalityReversible, f.finalityOf(proceeds.ID),
		"D-132: the seller is drawn against the pool's worst outstanding contribution, "+
			"which is somebody else's reversible purchase")

	// The chargeback window closes on the OTHER trader's payment.
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			return f.credits.SetFinality(ctx, tx, reversible.ID, valuedomain.FinalitySettled,
				credit.Reference{Type: "test_settlement", ID: uuid.NewString()},
				"the chargeback window closed")
		}))
	res := f.settleDerived(100)
	assert.Positive(t, res.Promoted, "the sweep exists to promote exactly this")
	assert.Equal(t, valuedomain.FinalitySettled, f.finalityOf(proceeds.ID),
		"an honest seller's proceeds are promoted once the contribution they were drawn "+
			"against settles; the wait is the cost D-132 states, not a permanent refusal")
}
