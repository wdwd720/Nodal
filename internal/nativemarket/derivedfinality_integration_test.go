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
// the sandbox tier produces: a trader holding the demo seeder's PROMOTIONAL,
// UNFUNDED Credits buys into a market and sells back out, and the
// MARKET_TRADING_PROCEEDS they end up with clear the payout engine under
// SandboxPolicy at PAYOUT_KYC.
//
// It asserts the eligibility DECISION rather than driving payout.Service.Create,
// because Create adds a provider, a destination and a quote to the fixture and
// none of them is what this finding is about; the decision is exactly what
// Create consults, and internal/payout's own suite drives the rest.
func TestIntegration_ASandboxTraderEarnsProceedsThatCanReachAPayout(t *testing.T) {
	f := newFixture(t)

	// A trader holding what a seeded sandbox deployment gives somebody, and
	// nothing else: a grant, unfunded, which the payout policy refuses on
	// ORIGIN and which nothing can reverse. The fixture's own trader starts
	// with SETTLED purchased Credits, which would be spent first and would make
	// this prove nothing.
	trader := newAccount(t)
	grant := f.fundAs(trader, valuedomain.OriginPromotional, valuedomain.FinalityUnfunded, 40_000_000_000)

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
	assert.Equal(t, valuedomain.FinalityUnfunded, proceeds.Finality,
		"proceeds funded by an unfunded grant are unfunded: nothing external can claw them back")
	assert.True(t, proceeds.Finality.PayoutEligible())

	// And the payout engine agrees, under the policy the sandbox tier runs.
	policy := valuedomain.SandboxPolicy()
	require.True(t, policy.Rule(valuedomain.OriginMarketTradingProceeds).PayoutAllowed,
		"fixture check: the sandbox policy permits this origin")
	decision, err := payout.NewEngine(f.credits).Evaluate(f.ctx, testDB, payout.EligibilityInput{
		AccountID:  trader,
		Requested:  proceeds.Remaining,
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
	for _, lot := range decision.Lots {
		assert.NotEqual(t, valuedomain.OriginPromotional, lot.Origin,
			"the grant itself never leaves; it is the PROCEEDS that do")
	}
}

// TestIntegration_ProceedsOfAReversiblePurchaseAreReversible is the other
// direction, and it is the one that keeps the fix honest: a trader spending
// Credits a card issuer can still take back does not get payout-eligible
// proceeds by selling them into a market and out again.
func TestIntegration_ProceedsOfAReversiblePurchaseAreReversible(t *testing.T) {
	f := newFixture(t)
	// A trader of their own, holding nothing but the reversible purchase: the
	// fixture's own trader is funded with SETTLED Credits, and consumption
	// order would spend those first and prove nothing.
	trader := newAccount(t)
	f.fundAs(trader, valuedomain.OriginPurchased, valuedomain.FinalityReversible, 40_000_000_000)

	buy, err := f.buy(trader, 1_000_000_000, money.Quantity{})
	require.NoError(t, err)
	_, err = f.sell(trader, buy.Fill.AssetsOut, money.Quantity{})
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
