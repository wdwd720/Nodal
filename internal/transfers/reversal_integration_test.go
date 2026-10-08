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

// TestIntegration_AReversalCompensatesAndNeverEditsHistory: a reversal one
// operator proposed and a second approved is a new COMPENSATION journal with
// reversal_of and the proposal's reason code; the original journal is
// byte-for-byte unchanged; balances, the per-party identity and Σ WALLET are
// restored; the effect is COMPENSATED; the transfer names its proposal and
// its approver; approving again is the same fact.
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

	// Only an operator holding ledger:post_correction proposes.
	_, err = f.propose(alice.p, tr.ID, "FRAUD_CONFIRMED", "sender reported account takeover")
	requireCode(t, err, errs.CodeForbidden)
	support := f.op()
	support.Roles = []security.Role{security.RoleSupportReadOnly}
	_, err = f.propose(support, tr.ID, "FRAUD_CONFIRMED", "sender reported account takeover")
	requireCode(t, err, errs.CodeForbidden)
	_, err = f.propose(f.op(), tr.ID, "fraud", "sender reported account takeover")
	requireCode(t, err, errs.CodeValidationFailed)
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM transfer_reversal_proposals WHERE transfer_id = $1`, tr.ID))

	proposer, approver := f.op(), f.op()
	prop, err := f.propose(proposer, tr.ID, "FRAUD_CONFIRMED", "sender reported account takeover")
	require.NoError(t, err)
	require.False(t, prop.Replayed)
	assert.Equal(t, ProposalPending, prop.Proposal.Status())
	assert.Equal(t, 1, prop.Proposal.Seq)
	assert.Equal(t, proposer.SubjectID, prop.Proposal.ProposedBy.ID)
	assert.Equal(t, security.ActorOperator, prop.Proposal.ProposedBy.Type)
	// A proposal moves nothing.
	got, err := f.svc.Get(f.ctx, testDB, alice.p, tr.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, got.Status)
	assert.Equal(t, 0, f.reversalJournals(original))
	assert.Equal(t, "6000000", f.balance(alice.account, ledger.CodeWallet).String())

	dec, err := f.approve(approver, prop.Proposal.ID)
	require.NoError(t, err)
	rev := dec.Transfer
	assert.False(t, dec.Replayed)
	assert.Equal(t, ProposalApproved, dec.Proposal.Status())
	require.NotNil(t, dec.Proposal.DecidedBy)
	assert.Equal(t, approver.SubjectID, dec.Proposal.DecidedBy.ID)
	assert.Equal(t, StatusReversed, rev.Status)
	assert.Equal(t, "FRAUD_CONFIRMED", rev.ReversalReasonCode)
	assert.Equal(t, approver.SubjectID, rev.ReversedByActorID, "the reversal is recorded as the approver's")
	require.NotNil(t, rev.ReversalProposalID)
	assert.Equal(t, prop.Proposal.ID, *rev.ReversalProposalID)
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
	assert.Equal(t, 1, f.reversalJournals(original))

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
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM internal_transfer_transitions WHERE transfer_id = $1 AND from_status = 'COMPLETED'
		AND to_status = 'REVERSED' AND actor_type = 'OPERATOR' AND actor_id = $2`, tr.ID, approver.SubjectID))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM notifications WHERE user_id = $1 AND resource_id = $2 AND kind = 'TRANSFER_REVERSED'`, bob.user, tr.ID.String()))
	key := "advanced/p2p/" + tr.ID.String() + "/reversal.json"
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM provider_evidence WHERE key = $1`, key))
	var schema, proposedBy, approvedBy, proposalID string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT d->>'schema', d->>'proposed_by', d->>'approved_by', d->>'proposal_id'
		FROM (SELECT convert_from(body, 'UTF8')::jsonb AS d FROM provider_evidence WHERE key = $1) e`, key).
		Scan(&schema, &proposedBy, &approvedBy, &proposalID))
	assert.Equal(t, "nodal.transfers.reversal/v2", schema)
	assert.Equal(t, proposer.SubjectID, proposedBy, "the evidence names both operators")
	assert.Equal(t, approver.SubjectID, approvedBy)
	assert.Equal(t, prop.Proposal.ID.String(), proposalID)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM audit_events WHERE action = 'transfers.reversal.proposed' AND resource_id = $1`, prop.Proposal.ID.String()))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM audit_events WHERE action = 'transfers.reversal.approved' AND resource_id = $1
		AND actor_id = $2`, prop.Proposal.ID.String(), approver.SubjectID))

	// Again, by the same approver: the same fact, nothing new.
	again, err := f.approve(approver, prop.Proposal.ID)
	require.NoError(t, err)
	assert.True(t, again.Replayed)
	assert.Equal(t, *rev.ReversalJournalTransactionID, *again.Transfer.ReversalJournalTransactionID)
	assert.Equal(t, 1, f.reversalJournals(original))

	// The application role cannot edit the posted journal either.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE journal_entries SET quantity = 1 WHERE transaction_id = $1`, original)
		return err
	})
	require.Error(t, err)
	f.requireReconciled()
}

// TestIntegration_AReversalFailsClosedWhenTheFundsAreGone: an approval that
// would take the recipient below zero (or into funds reserved for something
// else) is refused, changes nothing and leaves the proposal open; it executes
// once the funds are back. A refused transfer cannot be proposed at all.
func TestIntegration_AReversalFailsClosedWhenTheFundsAreGone(t *testing.T) {
	f := newFixture(t)
	alice, bob, carol := f.customer(), f.customer(), f.customer()
	f.fund(alice.account, 10_000_000)
	res, err := f.send(alice.p, f.req(alice, bob, 4_000_000))
	require.NoError(t, err)
	tr := res.Transfer
	_, err = f.send(bob.p, f.req(bob, carol, 3_000_000))
	require.NoError(t, err)

	proposer, approver := f.op(), f.op()
	prop, err := f.propose(proposer, tr.ID, "DISPUTE_UPHELD", "itest")
	require.NoError(t, err)
	_, err = f.approve(approver, prop.Proposal.ID)
	requireCode(t, err, errs.CodeConflict)
	assert.Equal(t, "TRANSFER_REVERSAL_FUNDS_UNAVAILABLE", fieldOf(t, err, "rejection_code"))
	assert.Equal(t, prop.Proposal.ID.String(), fieldOf(t, err, "proposal_id"))
	got, err := f.svc.Get(f.ctx, testDB, alice.p, tr.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, got.Status)
	assert.Equal(t, 0, f.reversalJournals(*tr.JournalTransactionID))
	assert.Equal(t, "1000000", f.balance(bob.account, ledger.CodeWallet).String())
	open, err := f.svc.ReversalProposal(f.ctx, testDB, approver, prop.Proposal.ID)
	require.NoError(t, err)
	assert.Equal(t, ProposalPending, open.Status(), "a refused approval writes no decision")
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM transfer_reversal_decisions WHERE proposal_id = $1`, prop.Proposal.ID))

	// Funds reserved for something else are not available to a reversal.
	_, err = f.send(carol.p, f.req(carol, bob, 3_000_000))
	require.NoError(t, err)
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.cap.Reserve(ctx, tx, reserveFor(bob, f.asset.ID, 2_000_000))
		return err
	}))
	_, err = f.approve(approver, prop.Proposal.ID)
	requireCode(t, err, errs.CodeConflict)

	// With 4 available again the same proposal executes.
	_, err = f.send(alice.p, f.req(alice, bob, 2_000_000))
	require.NoError(t, err)
	done, err := f.approve(approver, prop.Proposal.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusReversed, done.Transfer.Status)
	for _, c := range []customer{alice, bob, carol} {
		f.requirePartyIdentity(c.account)
	}

	broke := f.customer()
	refused, err := f.send(broke.p, f.req(broke, bob, 1))
	require.Error(t, err)
	_, err = f.propose(f.op(), refused.Transfer.ID, "DISPUTE_UPHELD", "x")
	requireCode(t, err, errs.CodeInvalidStateTransition)
	f.requireReconciled()
}
