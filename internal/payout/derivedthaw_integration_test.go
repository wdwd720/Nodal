//go:build integration

package payout_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// The thaw half of the derived sweep (D-140, F-278).
//
// D-124 froze a derived lot when a lot it was derived from was disputed or
// reversed, and D-094 built the mirror for FUNDED value -- `UnfreezeFunding`
// returns a funding to its window when the dispute is resolved in the
// platform's favour. Derived value had no mirror: `SettleDerived`'s promotion
// clause opened `st.finality = 'REVERSIBLE'` and a frozen lot is not, its
// freeze clause opened "not already frozen" and a frozen lot is, and no other
// writer can reach a lot with no `credit_fundings` row. A dispute the platform
// WON therefore unfroze the card and stranded every earning behind it.
//
// These are the product's own tests of the third clause. The auditor's
// reproduction of the defect is in audit_wv4_integration_test.go.

// settleDerivedPass runs one pass at `limit` and returns what it did.
func settleDerivedPass(t *testing.T, f *auditFixture, limit int) credit.SettleDerivedResult {
	t.Helper()
	var res credit.SettleDerivedResult
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			res, err = f.credits.SettleDerived(ctx, tx, limit)
			return err
		}))
	return res
}

// setFinality moves a lot the way an external event does.
func setFinality(t *testing.T, f *auditFixture, lot credit.LotID, to valuedomain.FundingFinality, why string) {
	t.Helper()
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			return f.credits.SetFinality(ctx, tx, lot, to,
				credit.Reference{Type: "derived_thaw_test", ID: uuid.NewString()}, why)
		}))
}

// A dispute resolved in the platform's favour thaws everything the freeze
// reached, one level per pass, through the ordinary funding paths.
func TestIntegration_ADisputeWonThawsTheEarningsDerivedFromIt(t *testing.T) {
	f := newAuditFixture(t)

	fundingID, purchase := f.fundedPurchase(t, 600_000_000)
	earning := f.derive(valuedomain.OriginMarketTradingProceeds, 300_000_000,
		[]credit.LotParent{{
			LotID: purchase, Quantity: money.QuantityFromInt64(300_000_000),
			Finality: valuedomain.FinalityReversible,
		}})
	grand := f.derive(valuedomain.OriginMarketCreatorEarning, 100_000_000,
		[]credit.LotParent{{
			LotID: earning.ID, Quantity: money.QuantityFromInt64(100_000_000),
			Finality: valuedomain.FinalityReversible,
		}})

	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			return f.credits.DisputeFunding(ctx, tx, fundingID, "the cardholder disputed the charge")
		}))
	f.sweepDerived(t, 3)
	require.Equal(t, valuedomain.FinalityDisputed, f.finalityOf(earning.ID))
	require.Equal(t, valuedomain.FinalityDisputed, f.finalityOf(grand.ID))

	// The dispute is resolved in the platform's favour: D-094's mirror returns
	// the funding to its window, and the window then closes.
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			return f.credits.UnfreezeFunding(ctx, tx, fundingID, "the dispute was resolved in our favour")
		}))

	// One pass while the funding is REVERSIBLE again: the earning comes back to
	// REVERSIBLE rather than to SETTLED, because what funded it can still be
	// taken back. The value is spendable again and still may not leave.
	first := settleDerivedPass(t, f, 100)
	assert.Equal(t, 1, first.Thawed, "the child of the unfrozen funding thaws on the first pass")
	assert.Equal(t, valuedomain.FinalityReversible, f.finalityOf(earning.ID),
		"a thaw answers what the parents say NOW: one of them is inside a dispute window again")
	assert.True(t, valuedomain.FinalityReversible.Spendable())
	assert.False(t, valuedomain.FinalityReversible.PayoutEligible())

	second := settleDerivedPass(t, f, 100)
	assert.Equal(t, 1, second.Thawed, "and the lot derived from the earning thaws one pass later")
	assert.Equal(t, valuedomain.FinalityReversible, f.finalityOf(grand.ID))

	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			return f.credits.SettleFunding(ctx, tx, fundingID, "and then the window closed")
		}))
	require.Equal(t, valuedomain.FinalitySettled, f.finalityOf(purchase))

	// From here the ordinary promotion clause finishes the job.
	f.sweepDerived(t, 4)
	assert.Equal(t, valuedomain.FinalitySettled, f.finalityOf(earning.ID))
	assert.Equal(t, valuedomain.FinalitySettled, f.finalityOf(grand.ID))

	// And what the holder is told. FUNDING_NOT_SETTLED's own declaration says
	// "waiting fixes it"; that sentence is true again (F-230, F-278).
	bal, err := f.credits.Balances(f.ctx, testDB, credit.BalanceRequest{
		AccountID: f.account, Policy: valuedomain.SandboxPolicy(),
		Verified:   valuedomain.VerificationPayoutKYC,
		ActiveCaps: map[valuedomain.CapabilityKey]bool{valuedomain.CapPayoutReserve: true},
		Now:        f.clk.Now(),
	})
	require.NoError(t, err)
	assert.Equal(t, "0", bal.Frozen.String(), "nothing is frozen once the dispute is won")
	assert.Zero(t, bal.IneligibleReasons[valuedomain.ReasonFundingNotFinal])

	// The sweep has nothing left to do, which is the property F-260 is about:
	// every direction takes a lot OUT of the candidate set.
	idle := settleDerivedPass(t, f, 100)
	assert.Zero(t, idle.Thawed+idle.Promoted+idle.Frozen,
		"a sweep that keeps returning work it has already done is the defect, not the fix")
}

// A lot frozen by a parent whose funding was REVERSED stays frozen for ever,
// and that is the design: the money behind it was actually taken back.
func TestIntegration_ALotFrozenByAReversedParentIsNeverThawed(t *testing.T) {
	f := newAuditFixture(t)
	parent := f.issue(valuedomain.OriginPurchased, valuedomain.FinalityReversible, 600_000_000)
	earning := f.derive(valuedomain.OriginMarketTradingProceeds, 300_000_000,
		[]credit.LotParent{{
			LotID: parent.ID, Quantity: money.QuantityFromInt64(300_000_000),
			Finality: valuedomain.FinalityReversible,
		}})

	setFinality(t, f, parent.ID, valuedomain.FinalityDisputed, "the payer disputed the payment")
	require.Equal(t, 1, settleDerivedPass(t, f, 100).Frozen)
	require.Equal(t, valuedomain.FinalityDisputed, f.finalityOf(earning.ID))

	// The dispute is lost: the card network took the money back.
	setFinality(t, f, parent.ID, valuedomain.FinalityReversed, "the issuer decided for the cardholder")

	for i := 0; i < 10; i++ {
		res := settleDerivedPass(t, f, 100)
		require.Zero(t, res.Thawed+res.Promoted+res.Frozen, "pass %d moved something", i+1)
	}
	assert.Equal(t, valuedomain.FinalityDisputed, f.finalityOf(earning.ID),
		"D-140: REVERSED is terminal, so a parent that was reversed is frozen for ever and the "+
			"thaw clause -- which requires that NO parent is still frozen -- never selects its "+
			"children. Thawing them would be the ledger releasing value it has already lost")
	assert.False(t, valuedomain.FinalityDisputed.PayoutEligible())
	assert.False(t, valuedomain.FinalityDisputed.Spendable())
}

// A frozen lot with two parents, one thawed and one still disputed, stays
// frozen: the clause is about ALL of them.
func TestIntegration_AFrozenLotWithOneParentStillFrozenIsNotThawed(t *testing.T) {
	f := newAuditFixture(t)
	first := f.issue(valuedomain.OriginPurchased, valuedomain.FinalityReversible, 400_000_000)
	second := f.issue(valuedomain.OriginPurchased, valuedomain.FinalityReversible, 400_000_000)
	earning := f.derive(valuedomain.OriginMarketTradingProceeds, 200_000_000,
		[]credit.LotParent{
			{
				LotID: first.ID, Quantity: money.QuantityFromInt64(100_000_000),
				Finality: valuedomain.FinalityReversible,
			},
			{
				LotID: second.ID, Quantity: money.QuantityFromInt64(100_000_000),
				Finality: valuedomain.FinalityReversible,
			},
		})

	setFinality(t, f, first.ID, valuedomain.FinalityDisputed, "the first payer disputed")
	setFinality(t, f, second.ID, valuedomain.FinalityDisputed, "the second payer disputed")
	require.Equal(t, 1, settleDerivedPass(t, f, 100).Frozen)

	setFinality(t, f, first.ID, valuedomain.FinalitySettled, "the first dispute was resolved in our favour")
	for i := 0; i < 3; i++ {
		require.Zero(t, settleDerivedPass(t, f, 100).Thawed,
			"one parent is still disputed, so there is nothing to thaw")
	}
	require.Equal(t, valuedomain.FinalityDisputed, f.finalityOf(earning.ID))

	setFinality(t, f, second.ID, valuedomain.FinalitySettled, "and so was the second")
	assert.Equal(t, 1, settleDerivedPass(t, f, 100).Thawed)
	assert.Equal(t, valuedomain.FinalitySettled, f.finalityOf(earning.ID),
		"every parent is payout-eligible, so the thaw lands on SETTLED rather than REVERSIBLE")
}

// The thaw reaches past the batch and then stops, which is F-260's property
// applied to the new clause: a lot leaves the candidate set the moment it moves.
func TestIntegration_TheThawReachesMoreLotsThanItsBatchAndThenStops(t *testing.T) {
	f := newAuditFixture(t)
	const batch = 100

	parent := f.issue(valuedomain.OriginPurchased, valuedomain.FinalityReversible, 10_000_000_000)
	children := make([]credit.LotID, 0, batch+1)
	for i := 0; i < batch+1; i++ {
		lot := f.derive(valuedomain.OriginMarketTradingProceeds, 1_000,
			[]credit.LotParent{{
				LotID: parent.ID, Quantity: money.QuantityFromInt64(1_000),
				Finality: valuedomain.FinalityReversible,
			}})
		children = append(children, lot.ID)
	}

	setFinality(t, f, parent.ID, valuedomain.FinalityDisputed, "the payer disputed the payment")
	assert.Equal(t, batch, settleDerivedPass(t, f, batch).Frozen)
	assert.Equal(t, 1, settleDerivedPass(t, f, batch).Frozen,
		"the freeze direction already reached past the batch (F-260)")

	setFinality(t, f, parent.ID, valuedomain.FinalitySettled, "the dispute was resolved in our favour")

	firstPass := settleDerivedPass(t, f, batch)
	assert.Equal(t, batch, firstPass.Thawed, "the first pass thaws a full batch")
	lastPass := settleDerivedPass(t, f, batch)
	assert.Equal(t, 1, lastPass.Thawed,
		"F-260: the hundred-and-first frozen lot must be examined too. A clause that kept "+
			"selecting lots it had already moved would return the oldest batch for ever")
	idle := settleDerivedPass(t, f, batch)
	assert.Zero(t, idle.Thawed+idle.Promoted+idle.Frozen,
		"and the candidate set is empty: a thawed lot is SETTLED, which the promotion clause "+
			"does not open and the thaw clause does not either")

	for i, id := range children {
		assert.Equalf(t, valuedomain.FinalitySettled, f.finalityOf(id),
			"derived lot %d of %d is still frozen after the dispute was won", i+1, len(children))
	}
}

// The thaw is expressed as a legal edge, not as a special case: the finalities
// the clause opens are the frozen ones the transition table can leave, which is
// DISPUTED and never REVERSED.
func TestIntegration_TheThawClauseOpensOnlyTheEdgesTheTableHas(t *testing.T) {
	for _, f := range valuedomain.FrozenFinalities() {
		settled := valuedomain.CanTransitionFinality(f, valuedomain.FinalitySettled)
		reversible := valuedomain.CanTransitionFinality(f, valuedomain.FinalityReversible)
		switch f {
		case valuedomain.FinalityDisputed:
			assert.True(t, settled && reversible,
				"DISPUTED is the finality the thaw leaves, in both directions")
		case valuedomain.FinalityReversed:
			assert.False(t, settled || reversible,
				"REVERSED has no outgoing edge and must not acquire one: a lot whose funding was "+
					"taken back is frozen for ever, by design (D-140)")
		}
	}
}
