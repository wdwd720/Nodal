//go:build integration

package orders

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// TestIntegration_ScenarioD_TwoSandboxUsersTradeWithAnAuditProof is Scenario
// D: two sandbox users submit compatible limit orders, they trade, balances
// and positions move exactly by the trade and its disclosed fees, and the
// audit trail -- journal transaction, effect, evidence, book state hash --
// proves it.
func TestIntegration_ScenarioD_TwoSandboxUsersTradeWithAnAuditProof(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{makerBPS: 10, takerBPS: 20})
	alice, bob := f.account(), f.account()
	f.fund(alice, m.base.ID, 10_000_000)  // 10 base units
	f.fund(bob, m.quote.ID, 2000_000_000) // 2000 quote units

	sell := f.submit(f.limit(m, alice, SideSell, "100.00", units(2)))
	require.Equal(t, StatusOpen, sell.Order.Status, "the first order rests")
	require.Equal(t, econ.StatusExecuting, sell.Intent.Status, "a resting order's intent is executing")

	buy := f.submit(f.limit(m, bob, SideBuy, "100.00", units(2)))
	require.Equal(t, StatusFilled, buy.Order.Status)
	require.Equal(t, econ.StatusCompleted, buy.Intent.Status)
	require.Len(t, buy.Trades, 1)
	trade := buy.Trades[0]

	// Balances: 2 units at 100.00 is 200 quote units; the maker pays 10 bps
	// (0.2), the taker 20 bps (0.4), both to PLATFORM_FEE_REVENUE.
	assert.Equal(t, "8000000", f.wallet(alice, m.base.ID).String())
	assert.Equal(t, "199800000", f.wallet(alice, m.quote.ID).String())
	assert.Equal(t, "2000000", f.wallet(bob, m.base.ID).String(), "bob's position is 2 base units")
	assert.Equal(t, "1799600000", f.wallet(bob, m.quote.ID).String())
	assert.Equal(t, "200000", trade.MakerFee.String())
	assert.Equal(t, "400000", trade.TakerFee.String())
	assert.Equal(t, "600000", f.balance(ledger.PlatformAccount(ledger.CodePlatformFeeRevenue, m.quote.ID)).String())
	assert.Equal(t, "200000000", f.balance(ledger.CustomerAccount(alice, ledger.CodeTradingInflow, m.quote.ID)).String())
	assert.Equal(t, "2000000", f.balance(ledger.CustomerAccount(bob, ledger.CodeTradingInflow, m.base.ID)).String())

	// The maker's order and intent were finished by the taker's command.
	aliceOrder, err := f.svc.Order(f.ctx, testDB, sell.Order.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusFilled, aliceOrder.Status)
	in, err := f.intents.Get(f.ctx, testDB, *aliceOrder.IntentID)
	require.NoError(t, err)
	assert.Equal(t, econ.StatusCompleted, in.Status)

	// Both reservations are used up and closed.
	for _, rid := range []string{aliceOrder.ReservationID, buy.Order.ReservationID} {
		var status string
		require.NoError(t, testDB.QueryRow(f.ctx, `SELECT status FROM asset_reservations WHERE id = $1`, rid).Scan(&status))
		assert.Equal(t, "CONSUMED", status)
	}

	// Audit proof: the trade names its journal transaction (a SIM_TRADE that
	// balances per party, checked by the database), its APPLIED effect, which
	// names the same journal and the archived evidence, and the book's state
	// hash replays from the journal.
	var kind string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT kind FROM journal_transactions WHERE id = $1`, trade.JournalTransactionID).Scan(&kind))
	assert.Equal(t, string(ledger.KindSimTrade), kind)
	eff, err := f.effects.Get(f.ctx, testDB, trade.EffectID)
	require.NoError(t, err)
	assert.Equal(t, econ.EffectApplied, eff.Status)
	require.NotNil(t, eff.JournalTransactionID)
	assert.Equal(t, trade.JournalTransactionID, *eff.JournalTransactionID)
	assert.Equal(t, trade.EvidenceRef, eff.EvidenceRef)
	assert.NotEmpty(t, trade.EvidenceRef)
	f.requireArchived(trade.EvidenceRef, trade.EvidenceSHA256)
	assert.Equal(t, 2, f.count(`SELECT count(*) FROM advanced_risk_decisions WHERE intent_id IN ($1, $2) AND verdict = 'ALLOW'`,
		*aliceOrder.IntentID, *buy.Order.IntentID))

	proof, err := f.svc.VerifyJournal(f.ctx, testDB, m.inst)
	require.NoError(t, err)
	assert.Equal(t, proof.StateHash, proof.FullReplayHash)
	assert.Equal(t, proof.StateHash, proof.SnapshotReplayHash)
	assert.Equal(t, buy.StateHash, proof.StateHash, "the hash the taker's command recorded is the book's")
	assert.EqualValues(t, 1, proof.Trades)
	t.Logf("scenario D proof: trade %d journal %s effect %s evidence %s book state %s",
		trade.TradeSeq, trade.JournalTransactionID, trade.EffectID, trade.EvidenceRef, hex.EncodeToString(proof.StateHash[:]))

	// Nothing moved that the trade does not explain: quote and base are
	// conserved across the two customers and the platform.
	totalQuote := f.wallet(alice, m.quote.ID).Add(f.wallet(bob, m.quote.ID)).
		Add(f.balance(ledger.PlatformAccount(ledger.CodePlatformFeeRevenue, m.quote.ID)))
	assert.Equal(t, money.QuantityFromInt64(2000_000_000).String(), totalQuote.String())
}
