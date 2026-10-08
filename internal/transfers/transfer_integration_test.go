//go:build integration

package transfers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// TestIntegration_ATransferChangesOwnershipNotLiabilities walks the whole
// flow once and checks every artefact it must leave: the four-leg
// P2P_TRANSFER journal, Σ WALLET unchanged, the per-party identity, the
// APPLIED effect naming the journal, the intent COMPLETED through every step,
// the consumed reservation, the risk decision, the evidence, the outbox and
// audit records, and the recipient's notification.
func TestIntegration_ATransferChangesOwnershipNotLiabilities(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customer(), f.customer()
	f.fund(alice.account, 100_000_000) // 100 SBX
	before := f.walletSum(alice.account, bob.account)

	req := f.req(alice, bob, 12_345_678)
	req.Memo = "rent for october"
	res, err := f.send(alice.p, req)
	require.NoError(t, err)
	tr := res.Transfer
	assert.False(t, res.Replayed)
	assert.Equal(t, StatusCompleted, tr.Status)
	assert.Equal(t, "rent for october", tr.Memo)
	assert.Equal(t, money.USDFromMinor(1235), tr.NotionalUSD, "12.345678 at par is $12.35 rounded up for the limits")

	assert.Equal(t, "87654322", f.balance(alice.account, ledger.CodeWallet).String())
	assert.Equal(t, "12345678", f.balance(bob.account, ledger.CodeWallet).String())
	assert.Equal(t, "12345678", f.balance(alice.account, ledger.CodeTransferOut).String())
	assert.Equal(t, "12345678", f.balance(bob.account, ledger.CodeTransferIn).String())
	assert.True(t, before.Equal(f.walletSum(alice.account, bob.account)), "Σ WALLET is conserved")
	f.requirePartyIdentity(alice.account)
	f.requirePartyIdentity(bob.account)

	// The journal: exactly the §5 template, and nothing a customer typed.
	require.NotNil(t, tr.JournalTransactionID)
	j, err := f.ledger.Transaction(f.ctx, testDB, *tr.JournalTransactionID)
	require.NoError(t, err)
	assert.Equal(t, ledger.KindP2PTransfer, j.Kind)
	assert.Len(t, j.Entries, 4)
	assert.NotContains(t, j.Description, "rent")
	assert.Empty(t, j.Metadata)
	assert.Equal(t, 1, f.journalsOfTransfer(tr.ID))

	// The effect names the journal; the intent went through every step.
	require.NotNil(t, tr.EffectID)
	eff, err := f.deps.Effects.Get(f.ctx, testDB, *tr.EffectID)
	require.NoError(t, err)
	assert.Equal(t, econ.EffectApplied, eff.Status)
	assert.Equal(t, *tr.JournalTransactionID, *eff.JournalTransactionID)
	assert.Equal(t, "p2p:"+tr.ID.String(), eff.Key)
	assert.Equal(t, econ.SubsystemTransfers, eff.Subsystem)
	assert.NotEmpty(t, eff.EvidenceRef)
	in, err := f.deps.Intents.Get(f.ctx, testDB, tr.IntentID)
	require.NoError(t, err)
	assert.Equal(t, econ.StatusCompleted, in.Status)
	assert.Equal(t, econ.KindTransferValue, in.Kind)
	assert.Equal(t, 5, f.count(`SELECT count(*) FROM economic_intent_transitions WHERE intent_id = $1`, tr.IntentID),
		"RECEIVED -> ROUTED -> RISK_APPROVED -> RESERVED -> EXECUTING -> COMPLETED, one row per step")
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM internal_transfer_transitions WHERE transfer_id = $1 AND from_status = 'PENDING' AND to_status = 'COMPLETED'`, tr.ID))

	// Reservation consumed, risk decision allowed, evidence archived.
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM asset_reservations WHERE id = $1::uuid AND status = 'CONSUMED'`, tr.ReservationID))
	require.NotNil(t, tr.RiskDecisionID)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM advanced_risk_decisions WHERE id = $1 AND verdict = 'ALLOW' AND intent_kind = 'TRANSFER_VALUE'`, *tr.RiskDecisionID))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM provider_evidence WHERE key = $1`, "advanced/p2p/"+tr.ID.String()+".json"))
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM provider_evidence WHERE key = $1 AND position('rent for october' in convert_from(body, 'UTF8')) > 0`,
		"advanced/p2p/"+tr.ID.String()+".json"), "the evidence carries the memo's digest, not the memo")

	// Outbox (intent + effect transitions), audit, notification.
	assert.Equal(t, 6, f.count(`SELECT count(*) FROM outbox_events WHERE topic = 'econ.intent.transitioned' AND aggregate_id = $1`, tr.IntentID.String()))
	assert.Equal(t, 2, f.count(`SELECT count(*) FROM outbox_events WHERE topic = 'econ.effect.transitioned' AND aggregate_id = $1`, tr.EffectID.String()))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM audit_events WHERE resource_type = 'internal_transfer' AND resource_id = $1 AND action = 'transfers.transfer.completed'`, tr.ID.String()))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM notifications WHERE user_id = $1 AND resource_id = $2 AND kind = 'TRANSFER_RECEIVED'`, bob.user, tr.ID.String()))
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM notifications WHERE resource_id = $1 AND (body LIKE '%rent%' OR data::text LIKE '%rent%')`, tr.ID.String()))

	// Both parties read it; a stranger is told it does not exist.
	got, err := f.svc.Get(f.ctx, testDB, bob.p, tr.ID)
	require.NoError(t, err)
	assert.Equal(t, tr.ID, got.ID)
	_, err = f.svc.Get(f.ctx, testDB, f.customer().p, tr.ID)
	requireCode(t, err, errs.CodeNotFound)
	page, next, err := f.svc.ListForAccount(f.ctx, testDB, alice.p, alice.account, "", 10)
	require.NoError(t, err)
	assert.Empty(t, next)
	require.Len(t, page, 1)

	f.requireReconciled()
}

// TestIntegration_ADuplicateTransferIsIdempotent: the same key and request is
// the same transfer -- one journal, one effect, one intent -- and the same key
// with a different request is refused.
func TestIntegration_ADuplicateTransferIsIdempotent(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customer(), f.customer()
	f.fund(alice.account, 10_000_000)
	req := f.req(alice, bob, 1_000_000)

	first, err := f.send(alice.p, req)
	require.NoError(t, err)
	second, err := f.send(alice.p, req)
	require.NoError(t, err)
	assert.True(t, second.Replayed)
	assert.Equal(t, first.Transfer.ID, second.Transfer.ID)
	assert.Equal(t, 1, f.journalsOfTransfer(first.Transfer.ID))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM economic_effects WHERE effect_key = $1`, "p2p:"+first.Transfer.ID.String()))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM economic_intents WHERE account_id = $1 AND idempotency_key = $2`, alice.account, "p2p:"+req.IdempotencyKey))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM notifications WHERE resource_id = $1`, first.Transfer.ID.String()))
	assert.Equal(t, "9000000", f.balance(alice.account, ledger.CodeWallet).String(), "moved once")

	changed := req
	changed.Quantity = money.QuantityFromInt64(2_000_000)
	_, err = f.send(alice.p, changed)
	requireCode(t, err, errs.CodeInvalidIdempotencyReuse)

	// A refused request replays as the same refusal.
	broke := f.customer()
	refusedReq := f.req(broke, bob, 5)
	_, err = f.send(broke.p, refusedReq)
	requireCode(t, err, errs.CodeInsufficientBuyingPower)
	again, err := f.send(broke.p, refusedReq)
	requireCode(t, err, errs.CodeInsufficientBuyingPower)
	assert.True(t, again.Replayed)
	assert.Equal(t, StatusRejected, again.Transfer.Status)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM internal_transfers WHERE sender_account_id = $1`, broke.account))
	f.requireReconciled()
}

// TestIntegration_TheSenderCannotGoNegative: a transfer larger than the
// sender's available balance is refused, recorded, and posts nothing.
func TestIntegration_TheSenderCannotGoNegative(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customer(), f.customer()
	f.fund(alice.account, 1_000_000)

	res, err := f.send(alice.p, f.req(alice, bob, 1_000_001))
	requireCode(t, err, errs.CodeInsufficientBuyingPower)
	tr := res.Transfer
	assert.Equal(t, StatusRejected, tr.Status)
	assert.Contains(t, []string{RejectInsufficientFunds, "RISK_INSUFFICIENT_BALANCE"}, tr.RejectionCode)
	assert.Nil(t, tr.JournalTransactionID)
	assert.Nil(t, tr.EffectID)
	assert.Equal(t, 0, f.journalsOfTransfer(tr.ID))
	assert.Equal(t, "1000000", f.balance(alice.account, ledger.CodeWallet).String())
	assert.True(t, f.balance(bob.account, ledger.CodeWallet).IsZero())
	in, err := f.deps.Intents.Get(f.ctx, testDB, tr.IntentID)
	require.NoError(t, err)
	assert.Equal(t, econ.StatusRejected, in.Status)
	assert.Equal(t, tr.RejectionCode, in.RejectionCode)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM provider_evidence WHERE key = $1`, "advanced/p2p/"+tr.ID.String()+".json"))

	// The whole balance is fine; one more base unit is not.
	_, err = f.send(alice.p, f.req(alice, bob, 1_000_000))
	require.NoError(t, err)
	_, err = f.send(alice.p, f.req(alice, bob, 1))
	requireCode(t, err, errs.CodeInsufficientBuyingPower)
	assert.True(t, f.balance(alice.account, ledger.CodeWallet).IsZero())
	f.requirePartyIdentity(alice.account)
	f.requireReconciled()
}
