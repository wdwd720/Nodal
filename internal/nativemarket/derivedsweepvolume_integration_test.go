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
	"github.com/nodal/controlplane/internal/valuedomain"
)

// The derived sweep at the volume a deployment reaches in its first week
// (F-260, D-124's sweep).
//
// TestAuditWV2_TheDerivedSweepReachesEveryLotAndNotOnlyTheOldestBatch is the
// auditor's reproduction and drives a batch of two. These two drive the number
// that matters: cmd/api runs SettleDerived at settleBatch = 100, and the
// original predicate returned the OLDEST 100 derived lots on every pass for
// ever, because a lot it promoted stayed in the candidate set and a lot that
// was payout-eligible at birth was never out of it.
//
// So the hundred-and-first derived lot a deployment ever minted was never
// examined again, in either direction: an earning whose funding had cleared
// stayed REVERSIBLE, and an earning whose funding had been charged back was
// never frozen. The first is F-230 restored by the sweep written to fix it; the
// second is worse, because it is value the platform has already lost sitting in
// a spendable, withdrawable state.

// settleBatch is cmd/api's batch size. It is repeated here rather than
// imported because cmd/api is a main package; the number is asserted against
// the deployment's own constant by the cmd/api suite, and what these tests need
// is a batch SMALLER than the population, which is the condition every real
// deployment is in.
const settleBatch = 100

// derivedFrom mints `count` derived lots, each funded by `parent`, and returns
// their ids in creation order.
func derivedFrom(t *testing.T, f *fixture, holder accounts.AccountID, parent credit.Lot, count int) []credit.LotID {
	t.Helper()
	out := make([]credit.LotID, 0, count)
	for i := 0; i < count; i++ {
		lot := f.derivedLot(holder, []credit.LotParent{{
			LotID:    parent.ID,
			Quantity: money.QuantityFromInt64(1_000),
			Finality: parent.Finality,
		}}, 1_000)
		out = append(out, lot.ID)
	}
	return out
}

// TestIntegration_TheDerivedSweepPromotesMoreLotsThanItsBatchAcrossPasses: a
// deployment with settleBatch + 1 promotable lots promotes all of them.
func TestIntegration_TheDerivedSweepPromotesMoreLotsThanItsBatchAcrossPasses(t *testing.T) {
	f := newFixture(t)
	holder := newAccount(t)

	// One card payment inside its dispute window, and 101 earnings funded by
	// it. Every one of them is REVERSIBLE and none can move yet.
	parent := f.fundAs(holder, valuedomain.OriginPurchased, valuedomain.FinalityReversible, 10_000_000_000)
	lots := derivedFrom(t, f, holder, parent, settleBatch+1)
	require.Len(t, lots, settleBatch+1)
	for _, id := range lots {
		require.Equal(t, valuedomain.FinalityReversible, f.finalityOf(id))
	}

	// Nothing to do while the funding can still be taken back. This is also the
	// first half of the predicate: a REVERSIBLE lot with a REVERSIBLE parent is
	// not a candidate at all, so a pass over a hundred and one of them costs
	// one query and moves nothing.
	assert.Zero(t, f.settleDerived(settleBatch).Promoted,
		"a lot whose parent can still be reversed is not promotable and must not be a candidate")

	// The chargeback window closes.
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			return f.credits.SetFinality(ctx, tx, parent.ID, valuedomain.FinalitySettled,
				credit.Reference{Type: "test_settlement", ID: uuid.NewString()},
				"the chargeback window closed")
		}))

	first := f.settleDerived(settleBatch)
	assert.Equal(t, settleBatch, first.Promoted, "the first pass promotes a full batch")
	second := f.settleDerived(settleBatch)
	assert.Equal(t, 1, second.Promoted,
		"F-260: the hundred-and-first derived lot was never examined again, because the "+
			"candidate set only ever grew and ORDER BY a UUIDv7 is chronological")
	assert.Zero(t, f.settleDerived(settleBatch).Promoted,
		"and the set is empty afterwards: a sweep that keeps returning work it has already "+
			"done is the defect, not the fix")

	for i, id := range lots {
		assert.Equalf(t, valuedomain.FinalitySettled, f.finalityOf(id),
			"derived lot %d of %d is still REVERSIBLE after its funding settled", i+1, len(lots))
	}
}

// TestIntegration_TheDerivedSweepFreezesBeyondItsBatchToo: the freeze direction
// reaches the hundred-and-first lot as well, which is the half that costs money
// rather than the half that withholds it.
func TestIntegration_TheDerivedSweepFreezesBeyondItsBatchToo(t *testing.T) {
	f := newFixture(t)
	holder := newAccount(t)

	// A hundred earnings that were payout-eligible at birth. They are the
	// oldest derived lots in the system and, under the old predicate, the ones
	// the sweep returned on every pass for ever.
	settledParent := f.fundAs(holder, valuedomain.OriginPurchased, valuedomain.FinalitySettled, 10_000_000_000)
	eligible := derivedFrom(t, f, holder, settledParent, settleBatch)
	for _, id := range eligible {
		require.Equal(t, valuedomain.FinalitySettled, f.finalityOf(id))
	}

	// The hundred-and-first, funded by a card payment that is about to be
	// charged back.
	disputedParent := f.fundAs(holder, valuedomain.OriginPurchased, valuedomain.FinalityReversible, 10_000_000_000)
	waiting := f.derivedLot(holder, []credit.LotParent{{
		LotID:    disputedParent.ID,
		Quantity: money.QuantityFromInt64(1_000),
		Finality: disputedParent.Finality,
	}}, 1_000)
	require.Equal(t, valuedomain.FinalityReversible, f.finalityOf(waiting.ID))

	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			return f.credits.SetFinality(ctx, tx, disputedParent.ID, valuedomain.FinalityDisputed,
				credit.Reference{Type: "test_dispute", ID: uuid.NewString()},
				"the payer disputed the payment")
		}))

	// One pass, at the deployment's own batch size, with a hundred
	// already-settled derived lots sitting in front of this one.
	res := f.settleDerived(settleBatch)
	assert.Equal(t, 1, res.Frozen,
		"F-260: the freeze direction never reached the hundred-and-first lot, so value the "+
			"platform has already lost stayed spendable and payout-eligible")
	assert.Equal(t, valuedomain.FinalityDisputed, f.finalityOf(waiting.ID))
	assert.False(t, valuedomain.FinalityDisputed.PayoutEligible())

	// The hundred that were fine are still fine, and the sweep does not keep
	// re-finding them.
	for _, id := range eligible {
		assert.Equal(t, valuedomain.FinalitySettled, f.finalityOf(id))
	}
	after := f.settleDerived(settleBatch)
	assert.Zero(t, after.Frozen)
	assert.Zero(t, after.Promoted)
}
