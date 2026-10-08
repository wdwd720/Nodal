//go:build integration

package transfers

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/security"
)

// TestIntegration_AReversalCompensatesAndNeverEditsHistory: an operator's
// reversal is a new COMPENSATION journal with reversal_of and a reason code;
// the original journal is byte-for-byte unchanged; balances, the per-party
// identity and Σ WALLET are restored; the effect is COMPENSATED; a second
// reversal is the same fact.
func TestIntegration_AReversalCompensatesAndNeverEditsHistory(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customer(), f.customer()
	f.fund(alice.account, 10_000_000)
	res, err := f.send(alice.p, f.req(alice, bob, 4_000_000))
	require.NoError(t, err)
	tr := res.Transfer
	original := *tr.JournalTransactionID
	fingerprint := f.journalFingerprint(original)
	sum := f.walletSum(alice.account, bob.account)

	req := ReverseRequest{TransferID: tr.ID, ReasonCode: "FRAUD_CONFIRMED", Reason: "sender reported account takeover"}
	// Only an operator holding ledger:post_correction reverses.
	_, err = f.reverse(alice.p, req)
	requireCode(t, err, errs.CodeForbidden)
	support := operator()
	support.Roles = []security.Role{security.RoleSupportReadOnly}
	_, err = f.reverse(support, req)
	requireCode(t, err, errs.CodeForbidden)
	bad := req
	bad.ReasonCode = "fraud"
	_, err = f.reverse(operator(), bad)
	requireCode(t, err, errs.CodeValidationFailed)

	op := operator()
	rev, err := f.reverse(op, req)
	require.NoError(t, err)
	assert.Equal(t, StatusReversed, rev.Status)
	assert.Equal(t, "FRAUD_CONFIRMED", rev.ReversalReasonCode)
	assert.Equal(t, op.SubjectID, rev.ReversedByActorID)
	require.NotNil(t, rev.ReversalJournalTransactionID)

	// History: the original is untouched, the compensation is new and linked.
	assert.Equal(t, fingerprint, f.journalFingerprint(original), "the original journal is never edited")
	j, err := f.ledger.Transaction(f.ctx, testDB, *rev.ReversalJournalTransactionID)
	require.NoError(t, err)
	assert.Equal(t, ledger.KindCompensation, j.Kind)
	require.NotNil(t, j.ReversalOf)
	assert.Equal(t, original, *j.ReversalOf)
	assert.Equal(t, "FRAUD_CONFIRMED", j.ReasonCode)
	assert.Len(t, j.Entries, 4)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM journal_transactions WHERE reversal_of = $1`, original))

	// Balances restored; the identities hold.
	assert.Equal(t, "10000000", f.balance(alice.account, ledger.CodeWallet).String())
	assert.True(t, f.balance(bob.account, ledger.CodeWallet).IsZero())
	assert.True(t, f.balance(alice.account, ledger.CodeTransferOut).IsZero())
	assert.True(t, f.balance(bob.account, ledger.CodeTransferIn).IsZero())
	assert.True(t, sum.Equal(f.walletSum(alice.account, bob.account)))
	f.requirePartyIdentity(alice.account)
	f.requirePartyIdentity(bob.account)

	eff, err := f.deps.Effects.Get(f.ctx, testDB, *tr.EffectID)
	require.NoError(t, err)
	assert.Equal(t, econ.EffectCompensated, eff.Status)
	assert.Equal(t, *rev.ReversalJournalTransactionID, *eff.CompensationJournalTransactionID)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM internal_transfer_transitions WHERE transfer_id = $1 AND from_status = 'COMPLETED' AND to_status = 'REVERSED' AND actor_type = 'OPERATOR'`, tr.ID))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM notifications WHERE user_id = $1 AND resource_id = $2 AND kind = 'TRANSFER_REVERSED'`, bob.user, tr.ID.String()))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM provider_evidence WHERE key = $1`, "advanced/p2p/"+tr.ID.String()+"/reversal.json"))

	// Again: the same fact, nothing new.
	again, err := f.reverse(operator(), req)
	require.NoError(t, err)
	assert.Equal(t, *rev.ReversalJournalTransactionID, *again.ReversalJournalTransactionID)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM journal_transactions WHERE reversal_of = $1`, original))

	// The application role cannot edit the posted journal either.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE journal_entries SET quantity = 1 WHERE transaction_id = $1`, original)
		return err
	})
	require.Error(t, err)
	f.requireReconciled()
}

// TestIntegration_AReversalFailsClosedWhenTheFundsAreGone: a reversal that
// would take the recipient below zero (or into funds reserved for something
// else) is refused and changes nothing; it succeeds once the funds are back.
// A refused transfer cannot be reversed at all.
func TestIntegration_AReversalFailsClosedWhenTheFundsAreGone(t *testing.T) {
	f := newFixture(t)
	alice, bob, carol := f.customer(), f.customer(), f.customer()
	f.fund(alice.account, 10_000_000)
	res, err := f.send(alice.p, f.req(alice, bob, 4_000_000))
	require.NoError(t, err)
	tr := res.Transfer
	_, err = f.send(bob.p, f.req(bob, carol, 3_000_000))
	require.NoError(t, err)

	req := ReverseRequest{TransferID: tr.ID, ReasonCode: "DISPUTE_UPHELD", Reason: "itest"}
	_, err = f.reverse(operator(), req)
	requireCode(t, err, errs.CodeConflict)
	assert.Equal(t, "TRANSFER_REVERSAL_FUNDS_UNAVAILABLE", fieldOf(t, err, "rejection_code"))
	got, err := f.svc.Get(f.ctx, testDB, alice.p, tr.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, got.Status)
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM journal_transactions WHERE reversal_of = $1`, *tr.JournalTransactionID))
	assert.Equal(t, "1000000", f.balance(bob.account, ledger.CodeWallet).String())

	// Funds reserved for something else are not available to a reversal.
	_, err = f.send(carol.p, f.req(carol, bob, 3_000_000))
	require.NoError(t, err)
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.cap.Reserve(ctx, tx, reserveFor(bob, f.asset.ID, 2_000_000))
		return err
	}))
	_, err = f.reverse(operator(), req)
	requireCode(t, err, errs.CodeConflict)

	// With 4 available again the reversal goes through.
	_, err = f.send(alice.p, f.req(alice, bob, 2_000_000))
	require.NoError(t, err)
	_, err = f.reverse(operator(), req)
	require.NoError(t, err)
	for _, c := range []customer{alice, bob, carol} {
		f.requirePartyIdentity(c.account)
	}

	broke := f.customer()
	refused, err := f.send(broke.p, f.req(broke, bob, 1))
	require.Error(t, err)
	_, err = f.reverse(operator(), ReverseRequest{TransferID: refused.Transfer.ID, ReasonCode: "DISPUTE_UPHELD", Reason: "x"})
	requireCode(t, err, errs.CodeInvalidStateTransition)
	f.requireReconciled()
}
