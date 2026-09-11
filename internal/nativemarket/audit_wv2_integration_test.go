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

// Reproductions for the SECOND round of the withdrawal-verification audit
// (goal §54, "repeat until findings flatten"). Every test in this file is
// expected to FAIL on 92280c6; each asserts the invariant the goal and the
// product documents state, so a fix makes it pass rather than making it moot.
//
// Nothing here changes product code.

// proceedsOf returns the account's MARKET_TRADING_PROCEEDS lot, of which the
// tests below expect exactly one.
func proceedsOf(t *testing.T, f *fixture, account accounts.AccountID) credit.Lot {
	t.Helper()
	lots, err := f.credits.Lots(f.ctx, testDB, account)
	require.NoError(t, err)
	var out []credit.Lot
	for _, l := range lots {
		if l.Origin == valuedomain.OriginMarketTradingProceeds {
			out = append(out, l)
		}
	}
	require.Len(t, out, 1, "fixture check: exactly one proceeds lot")
	return out[0]
}

// ---------------------------------------------------------------------------
// F-wv2-4 — the pooled reserve is drawn down in ARRIVAL order, so a sale's
// proceeds inherit the finality of whoever paid into the market FIRST, not of
// what the seller paid in.
//
// D-124 states the rule the whole finality model exists for: "Payout-eligible
// by default is the laundering route the whole finality model exists to close:
// buy with card-funded Credits, sell them into a market and out again,
// withdraw, charge back." The shipped test
// TestIntegration_ProceedsOfAReversiblePurchaseAreReversible asserts it in as
// many words -- "a sale cannot launder a reversible purchase into
// payout-eligible value" -- but it drives a market whose ONLY contributor is
// the reversible trader, so FIFO hands them back their own lot.
//
// Add one earlier contributor whose Credits are settled -- which every real
// market has, and which a seeded deployment has before anybody signs up -- and
// the claim inverts: the reversible trader draws the settled contribution and
// is minted payout-eligible proceeds at birth. Their own reversible Credits
// stay in the pool, to be handed to whoever sells next.
// ---------------------------------------------------------------------------

func TestAuditWV2_TheFirstContributorsFinalityIsHandedToTheNextSeller(t *testing.T) {
	f := newFixture(t)

	// An earlier, ordinary trader whose Credits are settled: a card payment
	// out of its dispute window, which is what most of a live pool is.
	settledTrader := newAccount(t)
	settled := f.fundAs(settledTrader, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 40_000_000_000)
	_, err := f.buy(settledTrader, 1_000_000_000, money.Quantity{})
	require.NoError(t, err)

	// The attacker, holding nothing but a card payment a issuer can still take
	// back. They buy, and then sell back LESS than the settled trader put in,
	// so the draw-down is covered by the earlier contribution alone.
	attacker := newAccount(t)
	f.fundAs(attacker, valuedomain.OriginPurchased, valuedomain.FinalityReversible, 40_000_000_000)
	buy, err := f.buy(attacker, 1_000_000_000, money.Quantity{})
	require.NoError(t, err)
	half, err := buy.Fill.AssetsOut.MulDiv(
		money.QuantityFromInt64(1), money.QuantityFromInt64(2), money.RoundDown,
	)
	require.NoError(t, err)
	sell, err := f.sell(attacker, half, money.Quantity{})
	require.NoError(t, err)
	require.True(t, sell.Fill.CreditsOut.IsPositive(), "fixture check: the sale returned Credits")

	proceeds := proceedsOf(t, f, attacker)
	parents, err := f.credits.ParentsOf(f.ctx, testDB, proceeds.ID)
	require.NoError(t, err)
	require.NotEmpty(t, parents, "fixture check: the sale named the lots that funded it")

	for _, p := range parents {
		assert.NotEqual(t, settled.ID, p.LotID,
			"F-wv2-4: the seller's proceeds were funded by ANOTHER trader's settled contribution, "+
				"because native_market_credit_sources is drawn down in arrival order")
	}
	assert.Equal(t, valuedomain.FinalityReversible, proceeds.Finality,
		"F-wv2-4: a sale laundered a reversible purchase into payout-eligible value -- "+
			"the exact route D-124 says the finality model exists to close")
	assert.False(t, proceeds.Finality.PayoutEligible(),
		"F-wv2-4: proceeds of a card payment inside its dispute window are payout-eligible at birth")
}

// ---------------------------------------------------------------------------
// F-wv2-3 — a grant the payout policy forbids becomes withdrawable value by
// being traded, which is goal §23's forbidden pattern in as many words.
//
// Goal §23:
//
//	Do not allow:
//	  nonwithdrawable source -> trade -> magically payout-eligible balance
//	  unless the eventual external/legal/provider policy explicitly allows it.
//
// valuedomain.SandboxPolicy IS the policy, and it says the opposite of
// allowing it: OriginPromotional is `closed`, with the comment "a promotional
// grant that could leave the system would be the first rule somebody copied".
// docs/product/VERIFICATION_AND_WITHDRAWAL.md §7 says the requirement
// "therefore holds by construction rather than by a check somebody could
// forget", on the strength of an argument about lot SELECTION that says
// nothing about the transformation this test performs.
//
// Before D-124 the pattern was unreachable by accident: proceeds were minted
// REVERSIBLE for ever, so PayoutEligible() was false whatever the origin.
// D-124 made a derived lot inherit its parents' FINALITY and nothing else, so
// an UNFUNDED grant now yields UNFUNDED -- payout-eligible -- proceeds of an
// origin the policy permits.
// ---------------------------------------------------------------------------

func TestAuditWV2_AGrantThePolicyForbidsCannotBeTradedIntoWithdrawableValue(t *testing.T) {
	f := newFixture(t)

	// A trader holding what a seeded sandbox deployment hands somebody, and
	// nothing else. SandboxPolicy refuses this origin at every verification
	// level, and nothing external can reverse it.
	trader := newAccount(t)
	grant := f.fundAs(trader, valuedomain.OriginPromotional, valuedomain.FinalityUnfunded, 40_000_000_000)

	policy := valuedomain.SandboxPolicy()
	require.False(t, policy.Rule(valuedomain.OriginPromotional).PayoutAllowed,
		"fixture check: the policy forbids the grant leaving")
	require.True(t, policy.Rule(valuedomain.OriginMarketTradingProceeds).PayoutAllowed,
		"fixture check: the policy permits trading proceeds leaving")

	buy, err := f.buy(trader, 1_000_000_000, money.Quantity{})
	require.NoError(t, err)
	sell, err := f.sell(trader, buy.Fill.AssetsOut, money.Quantity{})
	require.NoError(t, err)
	require.True(t, sell.Fill.CreditsOut.IsPositive(), "fixture check: the sale returned Credits")

	proceeds := proceedsOf(t, f, trader)
	parents, err := f.credits.ParentsOf(f.ctx, testDB, proceeds.ID)
	require.NoError(t, err)
	require.NotEmpty(t, parents)
	require.Equal(t, grant.ID, parents[0].LotID,
		"fixture check: the Credits that came out of the pool are the grant the trader put in")

	// INVERTED, and the inversion is the decision (D-131, F-261).
	//
	// As written this asserted `!proceeds.Finality.PayoutEligible()` -- that
	// the proceeds of an UNFUNDED grant must not be payout-eligible BY
	// FINALITY. Making that true would mean minting them REVERSIBLE, and
	// nothing could ever move them out of it: a grant has no credit_fundings
	// row, so SettleFunding cannot reach it and SettleDerived would wait for a
	// parent that is already as final as it will ever be. That is F-230
	// restored, which is what D-124 exists to have fixed.
	//
	// The finality was never the right dimension for this finding. UNFUNDED
	// value IS final -- nobody can claw back a gift -- and what makes a grant
	// unwithdrawable is its ORIGIN, which the policy closes and which D-124 did
	// not carry across the trade. So the assertion now reads the dimension the
	// fix works in: the proceeds are as final as the grant (UNFUNDED, and that
	// is correct), and their ORIGIN FLOOR is the grant, which no policy in this
	// build releases.
	assert.Equal(t, valuedomain.FinalityUnfunded, proceeds.Finality,
		"proceeds of an unfunded grant are unfunded; nothing external can reverse either of them")
	assert.Equal(t, valuedomain.OriginPromotional, proceeds.OriginFloor,
		"F-wv2-3: value whose every parent is a grant the policy forbids did not inherit the "+
			"grant's origin floor, so a market round trip laundered it (goal §23)")
	assert.False(t, policy.Rule(proceeds.OriginFloor).PayoutAllowed,
		"F-wv2-3: the floor a round trip produced is one the policy releases")

	// And the engine agrees to let it go, which is the half a person sees.
	decision, err := payout.NewEngine(f.credits).Evaluate(f.ctx, testDB, payout.EligibilityInput{
		AccountID:             trader,
		Requested:             proceeds.Remaining,
		Policy:                policy,
		Verified:              valuedomain.VerificationPayoutKYC,
		ActiveCaps:            map[valuedomain.CapabilityKey]bool{valuedomain.CapPayoutReserve: true},
		Now:                   f.clk.Now(),
		DestinationVerified:   true,
		ProviderSupports:      true,
		SanctionsState:        "CLEAR",
		JurisdictionSupported: true,
	})
	require.NoError(t, err)
	assert.False(t, decision.Sufficient(),
		"F-wv2-3: the payout engine approved the whole of a grant that had been round-tripped "+
			"through a market -- nonwithdrawable source -> trade -> payout-eligible balance")
}

// ---------------------------------------------------------------------------
// F-wv2-8 — credit_lot_parents is INSERTable by cp_app at any time, and
// SettleDerived promotes any lot that has a parent row, so one INSERT promotes
// a card-funded REVERSIBLE lot to payout-eligible without touching the
// finality column the migration protects.
//
// 00809 grants `SELECT, INSERT ON credit_lot_parents TO cp_app` because the
// rows are written at mint. Nothing binds an INSERT to the transaction that
// created the lot, nothing requires the parent to belong to the same account,
// and SettleDerived's candidate query is `finality IN ('REVERSIBLE','SETTLED')
// AND EXISTS (a parent row)` -- it never asks whether the lot is a DERIVED one
// or whether it has a credit_fundings row of its own.
// ---------------------------------------------------------------------------

func TestAuditWV2_AParentRowCannotPromoteAPurchaseThatHasNotSettled(t *testing.T) {
	f := newFixture(t)

	buyer := newAccount(t)
	// A card payment still inside its dispute window.
	reversible := f.fundAs(buyer, valuedomain.OriginPurchased, valuedomain.FinalityReversible, 1_000_000_000)
	// Anybody's settled lot. It does not have to be the same person's.
	stranger := newAccount(t)
	settled := f.fundAs(stranger, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 1_000_000_000)

	// One INSERT, as cp_app, with no state change and no finality written.
	_, err := testDB.Exec(f.ctx,
		`INSERT INTO credit_lot_parents (lot_id, parent_lot_id, quantity) VALUES ($1,$2,1)`,
		reversible.ID, settled.ID)
	assert.Error(t, err,
		"F-wv2-8: cp_app invented the provenance of a lot months after it was minted")

	var res credit.SettleDerivedResult
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var serr error
			res, serr = f.credits.SettleDerived(ctx, tx, 100)
			return serr
		}))
	assert.Zero(t, res.Promoted, "F-wv2-8: SettleDerived promoted a lot that funds nothing")

	var finality string
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT finality FROM credit_lot_state WHERE lot_id = $1`, reversible.ID).Scan(&finality))
	assert.Equal(t, string(valuedomain.FinalityReversible), finality,
		"F-wv2-8: a card payment inside its dispute window became payout-eligible "+
			"because one row claimed it was derived from somebody else's settled Credits")
}

// derivedLot mints a lot out of other lots, the way a sale or a marketplace
// order does: RecordLot takes the least final parent, so the finality asked
// for here is not the one it gets.
func (f *fixture) derivedLot(account accounts.AccountID, parents []credit.LotParent, qty int64) credit.Lot {
	f.t.Helper()
	var lot credit.Lot
	require.NoError(f.t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			lot, err = f.credits.Issue(ctx, tx, credit.IssueRequest{
				AccountID: account, Quantity: money.QuantityFromInt64(qty),
				Origin:         valuedomain.OriginMarketTradingProceeds,
				Finality:       valuedomain.FinalityReversible,
				Parents:        parents,
				Reference:      credit.Reference{Type: "audit2_derived", ID: uuid.NewString()},
				IdempotencyKey: "audit2-derived-" + uuid.NewString(),
				Reason:         "withdrawal audit round two: an earning", EffectiveAt: f.clk.Now(),
			})
			return err
		}))
	return lot
}

func (f *fixture) finalityOf(id credit.LotID) valuedomain.FundingFinality {
	f.t.Helper()
	var out valuedomain.FundingFinality
	require.NoError(f.t, testDB.QueryRow(f.ctx,
		`SELECT finality FROM credit_lot_state WHERE lot_id = $1`, id).Scan(&out))
	return out
}

func (f *fixture) settleDerived(limit int) credit.SettleDerivedResult {
	f.t.Helper()
	var res credit.SettleDerivedResult
	require.NoError(f.t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			res, err = f.credits.SettleDerived(ctx, tx, limit)
			return err
		}))
	return res
}

// ---------------------------------------------------------------------------
// F-wv2-2 — SettleDerived's candidate set never shrinks, so the sweep starves
// after `settleBatch` derived lots and F-230 comes back for every earning
// minted after the hundredth.
//
// The query is
//
//	SELECT st.lot_id, st.finality FROM credit_lot_state st
//	 WHERE st.finality IN ('REVERSIBLE','SETTLED')
//	   AND EXISTS (SELECT 1 FROM credit_lot_parents p WHERE p.lot_id = st.lot_id)
//	 ORDER BY st.lot_id LIMIT $1
//
// A lot this pass PROMOTES goes REVERSIBLE -> SETTLED and stays in the
// predicate; a lot that was payout-eligible at birth was never out of it. So
// the candidate set only ever grows, and `ORDER BY st.lot_id` on UUIDv7s is
// chronological -- the sweep returns the OLDEST `limit` derived lots on every
// pass, for ever. cmd/api runs it at settleBatch = 100, so the hundred-and-
// first derived lot a deployment ever mints is never examined again, whatever
// happens to its parents.
//
// That is exactly F-230's outcome -- "no earned Credit could ever reach a
// payout-eligible funding finality" -- restored by the sweep that was written
// to fix it, and it arrives quietly at a volume any real deployment passes in
// its first week.
// ---------------------------------------------------------------------------

func TestAuditWV2_TheDerivedSweepReachesEveryLotAndNotOnlyTheOldestBatch(t *testing.T) {
	f := newFixture(t)
	holder := newAccount(t)

	// Two earnings that were payout-eligible at birth, which is what a sandbox
	// tier and a settled purchase both produce. They are the oldest derived
	// lots in the system and they never leave the candidate set.
	settledParent := f.fundAs(holder, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 10_000_000)
	for i := 0; i < 2; i++ {
		lot := f.derivedLot(holder, []credit.LotParent{{
			LotID: settledParent.ID, Quantity: money.QuantityFromInt64(1_000), Finality: settledParent.Finality,
		}}, 1_000)
		require.Equal(t, valuedomain.FinalitySettled, f.finalityOf(lot.ID),
			"fixture check: an earning funded by settled Credits is payout-eligible at birth")
	}

	// A later earning, funded by a card payment still inside its window. This
	// is the lot the sweep exists for.
	reversibleParent := f.fundAs(holder, valuedomain.OriginPurchased, valuedomain.FinalityReversible, 10_000_000)
	waiting := f.derivedLot(holder, []credit.LotParent{{
		LotID: reversibleParent.ID, Quantity: money.QuantityFromInt64(1_000), Finality: reversibleParent.Finality,
	}}, 1_000)
	require.Equal(t, valuedomain.FinalityReversible, f.finalityOf(waiting.ID))

	// The card payment clears, which is the event the sweep is supposed to act
	// on.
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			return f.credits.SetFinality(ctx, tx, reversibleParent.ID, valuedomain.FinalitySettled,
				credit.Reference{Type: "audit2_settlement", ID: uuid.NewString()},
				"the chargeback window closed")
		}))

	// Five passes, with a batch smaller than the number of derived lots --
	// which is cmd/api's position the moment a deployment has more than
	// settleBatch of them.
	for i := 0; i < 5; i++ {
		f.settleDerived(2)
	}

	assert.Equal(t, valuedomain.FinalitySettled, f.finalityOf(waiting.ID),
		"F-wv2-2: the sweep returned the same two already-settled derived lots on every pass "+
			"and never reached this one, so an earning whose funding has cleared is stranded "+
			"at REVERSIBLE -- which is F-230, restored by the sweep written to fix it")
}
