//go:build integration

package reconciliation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// The PART 49 numbers, in exact base units.
const (
	fundedUSDC   int64 = 100_000_000 // $100.00 (USDC, 6 decimals)
	reservedUSDC int64 = 50_000_000  // $50.00 reserved
	orderInput   int64 = 49_500_000  // $49.50 of it is what the order spends
	orderMinOut  int64 = 240_000_000 // 0.24 SOL minimum
	filledOutput int64 = 250_000_000 // 0.25 SOL actually received
	networkFee   int64 = 5_000       // 5000 lamports
)

// TestIntegration_Part49_CrashRecovery is the PART 49 crash test, automated.
//
//  1. user has $100                          → seeded WALLET:USDC
//  2. reserve $50                            → capital.Reserve
//  3. the transaction submits successfully   → the swap lands on chain
//  4. crash before local success persistence → the attempt is left SUBMITTING,
//     nothing about the fill is stored
//  5. restart                                → a fresh Engine over the same database
//  6. the state is uncertainty               → SUBMISSION_UNKNOWN, order and attempt
//  7. no duplicate transaction is sent       → the adapter fails the test on Submit
//  8. external truth is discovered           → both observers agree on the transaction
//  9. exactly one fill is persisted
//  10. exactly one position change occurs
//  11. correct ledger entries post
//  12. the unused reservation releases
//  13. the audit chain contains complete recovery evidence
func TestIntegration_Part49_CrashRecovery(t *testing.T) {
	d := openTestDB(t)
	f := newFixture(t, d)

	// 1. user has $100.
	f.fund(f.usdc, money.QuantityFromInt64(fundedUSDC), 10_000)
	require.True(t, f.balance(ledger.CodeWallet, f.usdc.ID).Equal(money.QuantityFromInt64(fundedUSDC)))

	// 2. reserve $50.
	res := f.reserve(money.QuantityFromInt64(reservedUSDC), 5_000)
	require.Equal(t, "ACTIVE", string(res.Status))
	require.True(t, f.reservedTotal().Equal(money.QuantityFromInt64(reservedUSDC)))

	sig := "SIG-part49-" + f.suffix
	ord, att := f.submittingOrder(res,
		money.QuantityFromInt64(orderInput), money.QuantityFromInt64(orderMinOut), sig, 100)

	// 3. the transaction submits successfully externally. 4. the process
	// crashes before any of that reaches the database: nothing below this
	// line was written by the executor.
	f.land(sig, money.QuantityFromInt64(orderInput), money.QuantityFromInt64(filledOutput), money.QuantityFromInt64(networkFee))
	require.Equal(t, execution.AttemptSubmitting, att.Status)
	require.Equal(t, execution.OrderSubmitting, ord.Status)
	require.Empty(t, f.fills(ord.ID), "the crashed process persisted no fill")

	// 5. restart: a brand new engine over the same database, nothing carried
	// over in memory.
	engine := f.newEngine(Config{})

	// 6-8. recovery.
	out, err := engine.RecoverAttempt(f.ctx, att.ID)
	require.NoError(t, err)

	// 6. the state was uncertainty before it was resolved.
	assert.Equal(t, DispositionAdopted, out.Disposition)
	unknownSeen := false
	for _, tr := range f.attemptTransitions(att.ID) {
		if tr == string(execution.AttemptSubmissionUnknown) {
			unknownSeen = true
		}
	}
	assert.True(t, unknownSeen, "the interrupted attempt passed through SUBMISSION_UNKNOWN (PART 48)")
	orderUnknownSeen := false
	for _, tr := range f.orderTransitions(ord.ID) {
		if tr == string(execution.OrderSubmissionUnknown) {
			orderUnknownSeen = true
		}
	}
	assert.True(t, orderUnknownSeen, "the order passed through SUBMISSION_UNKNOWN")

	// 7. no duplicate transaction is sent. noSubmitAdapter.Submit fails the
	// test outright; this asserts the counter as well so the guarantee is
	// visible in the assertions and not only in the fake.
	assert.Zero(t, f.adapter.submits, "PART 48: the recovery path must never submit")

	// 8. external truth is discovered.
	require.NotNil(t, out.Fill)
	assert.Equal(t, sig, out.Fill.TxSignature)
	assert.Equal(t, execution.FillFromReconciliation, out.Fill.Source)

	// 9. exactly one fill is persisted.
	fills := f.fills(ord.ID)
	require.Len(t, fills, 1)
	assert.True(t, fills[0].InputQuantity.Equal(money.QuantityFromInt64(orderInput)))
	assert.True(t, fills[0].OutputQuantity.Equal(money.QuantityFromInt64(filledOutput)))
	assert.True(t, fills[0].NetworkFeeQuantity.Equal(money.QuantityFromInt64(networkFee)))

	// 10. exactly one position change occurs: one SOL lot acquired, one USDC
	// disposal recorded. (The seed lot is the only other lot.)
	assert.Equal(t, 1, f.countRows(
		`SELECT count(*) FROM position_lots WHERE account_id = $1 AND asset_id = $2`, f.account, f.sol.ID))
	assert.Equal(t, 1, f.countRows(
		`SELECT count(*) FROM lot_dispositions WHERE account_id = $1 AND asset_id = $2`, f.account, f.usdc.ID))

	// 11. correct ledger entries post: exactly one TRADE_FILL transaction,
	// and the balances it produced.
	assert.Equal(t, 1, f.countRows(
		`SELECT count(*) FROM journal_transactions WHERE kind = 'TRADE_FILL' AND reference_id = $1`, fills[0].ID))
	assert.True(t, f.balance(ledger.CodeWallet, f.usdc.ID).Equal(money.QuantityFromInt64(fundedUSDC-orderInput)),
		"WALLET:USDC = 100 - 49.5")
	assert.True(t, f.balance(ledger.CodeWallet, f.sol.ID).Equal(money.QuantityFromInt64(filledOutput-networkFee)),
		"WALLET:SOL = received - network fee")
	assert.True(t, f.balance(ledger.CodeTradingOutflow, f.usdc.ID).Equal(money.QuantityFromInt64(orderInput)))
	assert.True(t, f.balance(ledger.CodeTradingInflow, f.sol.ID).Equal(money.QuantityFromInt64(filledOutput)))
	assert.True(t, f.balance(ledger.CodeFeesNetwork, f.sol.ID).Equal(money.QuantityFromInt64(networkFee)))

	// Positions and ledger agree, so the recovery left no drift behind.
	drifts, err := f.lots.VerifyAgainstLedger(f.ctx, f.d, f.account)
	require.NoError(t, err)
	assert.Empty(t, drifts, "Σ open lots == WALLET balance for every asset")

	// 12. the unused reservation releases: $50 was held, $49.50 consumed,
	// $0.50 returned to the account's available quantity.
	settled := f.reservation(res.ID)
	assert.Equal(t, "CONSUMED", string(settled.Status))
	assert.True(t, settled.ConsumedQuantity.Equal(money.QuantityFromInt64(orderInput)))
	assert.True(t, f.reservedTotal().IsZero(), "nothing stays reserved once the order is filled")

	// The order itself is FILLED.
	finalOrder, err := f.orders.Get(f.ctx, f.d, ord.ID)
	require.NoError(t, err)
	assert.Equal(t, execution.OrderFilled, finalOrder.Status)

	// 13. the audit chain contains complete recovery evidence.
	require.False(t, out.Record.ID.IsZero())
	assert.Equal(t, StatusMatched, out.Record.Status)
	assert.Equal(t, KindExecution, out.Record.Kind)
	assert.False(t, out.Record.BlocksNewRisk)

	actions := f.auditActions(out.Record.ID)
	assert.Contains(t, actions, AuditRecordOpened)
	assert.Contains(t, actions, AuditRecoveryAdopted)

	trs := f.transitions(out.Record.ID)
	require.Len(t, trs, 2)
	assert.Equal(t, StatusNone, trs[0].From)
	assert.Equal(t, StatusOpen, trs[0].To)
	assert.Equal(t, StatusOpen, trs[1].From)
	assert.Equal(t, StatusMatched, trs[1].To)

	// The recovery evidence names the transaction, the observers and the fact
	// that no duplicate was sent.
	payload := f.auditPayload(out.Record.ID, AuditRecoveryAdopted)
	assert.Equal(t, sig, payload["signature"])
	assert.Equal(t, false, payload["duplicate_submission_sent"])
	assert.Equal(t, string(DispositionAdopted), payload["disposition"])
	assert.Equal(t, "AGREED", payload["agreement_state"])

	// The whole account audit chain still verifies.
	f.requireAuditChainIntact()

	// Replaying the recovery converges: no second fill, no second posting, no
	// second position change, no second record.
	for i := 0; i < 3; i++ {
		again, err := engine.RecoverAttempt(f.ctx, att.ID)
		require.NoError(t, err)
		assert.Equal(t, out.Record.ID, again.Record.ID, "the same record is reused")
	}
	assert.Len(t, f.fills(ord.ID), 1)
	assert.Equal(t, 1, f.countRows(
		`SELECT count(*) FROM journal_transactions WHERE kind = 'TRADE_FILL' AND reference_id = $1`, fills[0].ID))
	assert.Equal(t, 1, f.countRows(
		`SELECT count(*) FROM position_lots WHERE account_id = $1 AND asset_id = $2`, f.account, f.sol.ID))
	assert.Equal(t, 1, f.countRows(
		`SELECT count(*) FROM reconciliation_records WHERE scope_type = $1 AND scope_id = $2`, ScopeAttempt, att.ID.String()))
	assert.Zero(t, f.adapter.submits)
}
