//go:build integration

package reconciliation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
)

// TestProp_ReplayConvergesAndNeverDoublePosts is the required property:
// replaying the same external observation any number of times converges to
// the same state and never double-posts.
//
// For N ∈ [1, 5] deliveries of one chain transaction the engine must end with
// exactly one fill, one journal transaction, one acquisition lot, one
// disposition and one reconciliation record — and the record must be the same
// record every time.
func TestProp_ReplayConvergesAndNeverDoublePosts(t *testing.T) {
	d := openTestDB(t)
	f := newFixture(t, d)
	f.seedChainBalances()
	// Enough capital that every iteration can hold its own reservation.
	f.fund(f.usdc, money.QuantityFromInt64(100_000_000_000), 100_000_000)

	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 5).Draw(rt, "deliveries")
		// Each iteration needs its own order, reservation and signature.
		// rapid replays a draw when it shrinks, so the identity comes from a
		// fresh id rather than from the draw.
		key := id.New[id.Any]().String()

		res := f.reserveWithKey(money.QuantityFromInt64(reservedUSDC), 5_000, "prop-"+key)
		sig := "SIG-prop-" + key
		ord, att := f.submittingOrderWithIntent(t, res, sig, 100)
		f.land(sig, money.QuantityFromInt64(orderInput), money.QuantityFromInt64(filledOutput), money.QuantityFromInt64(networkFee))

		var recordID RecordID
		var fillID string
		for i := 0; i < n; i++ {
			out, err := f.engine.RecoverAttempt(f.ctx, att.ID)
			require.NoError(rt, err)
			require.NotNil(rt, out.Fill, "delivery %d discovered no fill", i)
			if i == 0 {
				recordID, fillID = out.Record.ID, out.Fill.ID.String()
				continue
			}
			assert.Equal(rt, recordID, out.Record.ID, "delivery %d opened a second record", i)
			assert.Equal(rt, fillID, out.Fill.ID.String(), "delivery %d created a second fill", i)
		}

		// One fill.
		assert.Equal(rt, 1, f.countRows(`SELECT count(*) FROM fills WHERE order_id = $1`, ord.ID))
		// One set of ledger entries.
		assert.Equal(rt, 1, f.countRows(
			`SELECT count(*) FROM journal_transactions WHERE kind = 'TRADE_FILL' AND reference_id = $1`, fillID,
		))
		// One position change per asset.
		assert.Equal(rt, 1, f.countRows(
			`SELECT count(*) FROM position_lots WHERE account_id = $1 AND acquisition_ref_id = $2`, f.account, fillID,
		))
		assert.Equal(rt, 1, f.countRows(
			`SELECT count(*) FROM lot_dispositions WHERE account_id = $1 AND disposition_ref_id = $2`, f.account, fillID,
		))
		// One record, in one status.
		assert.Equal(rt, 1, f.countRows(
			`SELECT count(*) FROM reconciliation_records WHERE scope_type = $1 AND scope_id = $2`,
			ScopeAttempt, att.ID.String(),
		))
		rec, err := f.records.Get(f.ctx, f.d, recordID)
		require.NoError(rt, err)
		assert.Equal(rt, StatusMatched, rec.Status)
		// One reservation, finalized exactly once.
		assert.Equal(rt, "CONSUMED", string(f.reservation(res.ID).Status))
		// And never a submission.
		assert.Zero(rt, f.adapter.submits)
	})
}

// TestProp_BalanceComparisonConverges is the same property for the FULL mode:
// re-comparing an unchanged wallet balance never opens a second record and
// never changes the answer.
func TestProp_BalanceComparisonConverges(t *testing.T) {
	d := openTestDB(t)
	f := newFixture(t, d)
	f.fund(f.usdc, money.QuantityFromInt64(fundedUSDC), 10_000)
	f.seedChainBalances()

	rapid.Check(t, func(rt *rapid.T) {
		passes := rapid.IntRange(1, 4).Draw(rt, "passes")
		var first Record
		for i := 0; i < passes; i++ {
			recs, err := f.engine.RunFull(f.ctx, f.account)
			require.NoError(rt, err)
			rec := findRecord(t, recs, KindWalletBalance)
			if i == 0 {
				first = rec
				continue
			}
			assert.Equal(rt, first.ID, rec.ID, "pass %d opened a second record", i)
			assert.Equal(rt, first.Status, rec.Status, "pass %d changed the answer", i)
		}
		assert.Equal(rt, 1, f.countRows(
			`SELECT count(*) FROM reconciliation_records WHERE kind = $1 AND scope_type = $2 AND scope_id = $3`,
			KindWalletBalance, ScopeWallet, f.walletID+":"+f.usdc.ID.String(),
		))
	})
}
