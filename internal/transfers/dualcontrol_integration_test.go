//go:build integration

package transfers

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/security"
)

// completedTransfer funds alice (unless fund is 0) and sends bob qty,
// returning the COMPLETED transfer.
func (f *fixture) completedTransfer(alice, bob customer, fund, qty int64) Transfer {
	f.t.Helper()
	if fund > 0 {
		f.fund(alice.account, fund)
	}
	res, err := f.send(alice.p, f.req(alice, bob, qty))
	require.NoError(f.t, err)
	require.Equal(f.t, StatusCompleted, res.Transfer.Status)
	return res.Transfer
}

// requireNotReversed proves a transfer is still COMPLETED, with no
// compensation journal and its effect still APPLIED.
func (f *fixture) requireNotReversed(tr Transfer) {
	f.t.Helper()
	got, err := getTransfer(f.ctx, testDB, tr.ID, false)
	require.NoError(f.t, err)
	require.Equal(f.t, StatusCompleted, got.Status)
	require.Nil(f.t, got.ReversalProposalID)
	require.Equal(f.t, 0, f.reversalJournals(*tr.JournalTransactionID))
}

// TestIntegration_AProposerCannotApproveTheirOwnReversal: the operator who
// proposed a reversal is refused FORBIDDEN when approving it and nothing is
// written; a second operator's approval executes it.
func TestIntegration_AProposerCannotApproveTheirOwnReversal(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customer(), f.customer()
	tr := f.completedTransfer(alice, bob, 10_000_000, 3_000_000)
	proposer := f.op()
	prop, err := f.propose(proposer, tr.ID, "FRAUD_CONFIRMED", "takeover reported")
	require.NoError(t, err)

	_, err = f.approve(proposer, prop.Proposal.ID)
	requireCode(t, err, errs.CodeForbidden)
	assert.True(t, errors.Is(err, security.ErrSelfApproval), "%v", err)
	assert.Equal(t, rejectionSelfApproval, fieldOf(t, err, "rejection_code"))
	assert.Equal(t, prop.Proposal.ID.String(), fieldOf(t, err, "proposal_id"))
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM transfer_reversal_decisions WHERE proposal_id = $1`, prop.Proposal.ID))
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM audit_events WHERE resource_id = $1 AND action <> 'transfers.reversal.proposed'`,
		prop.Proposal.ID.String()))
	f.requireNotReversed(tr)
	assert.Equal(t, "7000000", f.balance(alice.account, ledger.CodeWallet).String())

	// The same human under another session is the same subject: still refused.
	again := proposer
	again.SessionID = "another-session"
	_, err = f.approve(again, prop.Proposal.ID)
	requireCode(t, err, errs.CodeForbidden)

	res, err := f.approve(f.op(), prop.Proposal.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusReversed, res.Transfer.Status)
	assert.NotEqual(t, proposer.SubjectID, res.Transfer.ReversedByActorID)
	f.requireReconciled()
}

// TestIntegration_TwoOperatorsReverseExactlyOnceUnderConcurrency: many
// distinct second operators approve the same proposal at once, each in its
// own transaction. Exactly one approval executes the reversal; every other
// one is a CONFLICT; there is one decision, one COMPENSATION journal, the
// balances are restored exactly once and Reconcile is clean.
func TestIntegration_TwoOperatorsReverseExactlyOnceUnderConcurrency(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customer(), f.customer()
	tr := f.completedTransfer(alice, bob, 10_000_000, 4_000_000)
	// Bob holds more than one reversal's worth, so a double execution would
	// not be stopped by funds: only the dual control can stop it.
	_, err := f.send(alice.p, f.req(alice, bob, 4_000_000))
	require.NoError(t, err)
	prop, err := f.propose(f.op(), tr.ID, "DISPUTE_UPHELD", "card dispute upheld")
	require.NoError(t, err)

	const n = 12
	approvers := make([]security.Principal, n)
	for i := range approvers {
		approvers[i] = f.op()
	}
	var wg sync.WaitGroup
	results := make(chan error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := f.approve(approvers[i], prop.Proposal.ID)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	succeeded, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errs.CodeOf(err) == errs.CodeConflict:
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 1, succeeded, "exactly one approval executes")
	assert.Equal(t, n-1, conflicts)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM transfer_reversal_decisions WHERE proposal_id = $1`, prop.Proposal.ID))
	assert.Equal(t, 1, f.reversalJournals(*tr.JournalTransactionID), "one COMPENSATION journal")
	got, err := getTransfer(f.ctx, testDB, tr.ID, false)
	require.NoError(t, err)
	assert.Equal(t, StatusReversed, got.Status)
	require.NotNil(t, got.ReversalProposalID)
	assert.Equal(t, prop.Proposal.ID, *got.ReversalProposalID)
	// Alice sent 8 and got 4 back exactly once; bob holds the other 4.
	assert.Equal(t, "6000000", f.balance(alice.account, ledger.CodeWallet).String())
	assert.Equal(t, "4000000", f.balance(bob.account, ledger.CodeWallet).String())
	f.requirePartyIdentity(alice.account)
	f.requirePartyIdentity(bob.account)
	f.requireReconciled()
}

// TestIntegration_ConcurrentProposalsLeaveOnePending: distinct operators
// proposing a reversal of the same transfer at once leave exactly one open
// proposal; every other attempt is a CONFLICT and writes nothing.
func TestIntegration_ConcurrentProposalsLeaveOnePending(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customer(), f.customer()
	tr := f.completedTransfer(alice, bob, 10_000_000, 1_000_000)

	const n = 10
	ops := make([]security.Principal, n)
	for i := range ops {
		ops[i] = f.op()
	}
	var wg sync.WaitGroup
	results := make(chan error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := f.propose(ops[i], tr.ID, "FRAUD_CONFIRMED", "concurrent proposal")
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	succeeded, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errs.CodeOf(err) == errs.CodeConflict:
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 1, succeeded)
	assert.Equal(t, n-1, conflicts)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM transfer_reversal_proposals WHERE transfer_id = $1`, tr.ID))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM transfer_reversal_proposals WHERE transfer_id = $1 AND seq = 1`, tr.ID))
	f.requireNotReversed(tr)
}

// TestIntegration_ARejectedProposalExecutesNothing: a rejected proposal can
// never be approved; nothing moves; the rejection is final (the same
// operator rejecting again is a replay); and a fresh proposal, the next in
// sequence, can still correct the transfer.
func TestIntegration_ARejectedProposalExecutesNothing(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customer(), f.customer()
	tr := f.completedTransfer(alice, bob, 10_000_000, 2_000_000)
	proposer, reviewer := f.op(), f.op()
	prop, err := f.propose(proposer, tr.ID, "FRAUD_SUSPECTED", "unusual pattern")
	require.NoError(t, err)

	// A rejection states its reason.
	_, err = f.reject(reviewer, prop.Proposal.ID, "")
	requireCode(t, err, errs.CodeValidationFailed)

	rej, err := f.reject(reviewer, prop.Proposal.ID, "the sender confirmed the payment")
	require.NoError(t, err)
	assert.Equal(t, ProposalRejected, rej.Proposal.Status())
	assert.Equal(t, "the sender confirmed the payment", rej.Proposal.DecisionReason)
	assert.Equal(t, StatusCompleted, rej.Transfer.Status, "a rejection leaves the transfer as it is")
	again, err := f.reject(reviewer, prop.Proposal.ID, "the sender confirmed the payment")
	require.NoError(t, err)
	assert.True(t, again.Replayed)

	for name, p := range map[string]security.Principal{"a third operator": f.op(), "the reviewer": reviewer} {
		_, err = f.approve(p, prop.Proposal.ID)
		requireCode(t, err, errs.CodeConflict)
		assert.Equal(t, "REJECTED", fieldOf(t, err, "decision"), name)
	}
	_, err = f.reject(f.op(), prop.Proposal.ID, "a second opinion")
	requireCode(t, err, errs.CodeConflict)
	f.requireNotReversed(tr)
	assert.Equal(t, "8000000", f.balance(alice.account, ledger.CodeWallet).String())
	assert.Equal(t, "2000000", f.balance(bob.account, ledger.CodeWallet).String())

	// The proposer may withdraw their own proposal: a rejection by them.
	second, err := f.propose(proposer, tr.ID, "FRAUD_CONFIRMED", "confirmed after all")
	require.NoError(t, err)
	assert.Equal(t, 2, second.Proposal.Seq)
	withdrawn, err := f.reject(proposer, second.Proposal.ID, "withdrawn by the proposer")
	require.NoError(t, err)
	assert.Equal(t, ProposalRejected, withdrawn.Proposal.Status())
	f.requireNotReversed(tr)

	// A third proposal, approved by a second operator, does correct it.
	third, err := f.propose(proposer, tr.ID, "FRAUD_CONFIRMED", "confirmed by the bank")
	require.NoError(t, err)
	assert.Equal(t, 3, third.Proposal.Seq)
	done, err := f.approve(reviewer, third.Proposal.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusReversed, done.Transfer.Status)
	assert.Equal(t, third.Proposal.ID, *done.Transfer.ReversalProposalID)
	f.requireReconciled()
}

// TestIntegration_AConsumedProposalExecutesNothingAgain: once approved, a
// proposal is consumed: another approval, a rejection, an approval by the
// proposer and a new proposal are all refused, and there is still exactly one
// COMPENSATION journal.
func TestIntegration_AConsumedProposalExecutesNothingAgain(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customer(), f.customer()
	tr := f.completedTransfer(alice, bob, 10_000_000, 2_000_000)
	_, err := f.send(alice.p, f.req(alice, bob, 2_000_000))
	require.NoError(t, err)
	proposer, approver := f.op(), f.op()
	tr2, err := f.reverse(proposer, approver, tr.ID, "DUPLICATE_PAYMENT", "paid twice")
	require.NoError(t, err)
	require.Equal(t, StatusReversed, tr2.Status)
	pid := *tr2.ReversalProposalID

	_, err = f.approve(f.op(), pid)
	requireCode(t, err, errs.CodeConflict)
	assert.Equal(t, "APPROVED", fieldOf(t, err, "decision"))
	_, err = f.approve(proposer, pid)
	requireCode(t, err, errs.CodeConflict)
	_, err = f.reject(approver, pid, "changed my mind")
	requireCode(t, err, errs.CodeConflict)
	_, err = f.propose(f.op(), tr.ID, "DUPLICATE_PAYMENT", "paid twice")
	requireCode(t, err, errs.CodeInvalidStateTransition)
	assert.Equal(t, 1, f.reversalJournals(*tr.JournalTransactionID))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM transfer_reversal_proposals WHERE transfer_id = $1`, tr.ID))
	assert.Equal(t, "8000000", f.balance(alice.account, ledger.CodeWallet).String())
	f.requireReconciled()
}

// TestIntegration_ReversalNeedsPermissionAndStepUp: a customer, an agent, an
// operator without ledger:post_correction, and an operator whose strong
// authentication is stale or absent are refused at every step; reading
// proposals is for operators who may read any account.
func TestIntegration_ReversalNeedsPermissionAndStepUp(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customer(), f.customer()
	tr := f.completedTransfer(alice, bob, 10_000_000, 1_000_000)
	open, err := f.propose(f.op(), tr.ID, "FRAUD_CONFIRMED", "x")
	require.NoError(t, err)

	opsRole := f.op()
	opsRole.Roles = []security.Role{security.RoleOperations}
	noMFA := f.op()
	noMFA.AMR = []string{"pwd"}
	stale := f.op()
	stale.AuthTime = f.clk.Now().Add(-DefaultPolicy().StepUpMaxAge() - time.Second)
	agent := security.AgentPrincipal("agent-1", alice.account.String())
	for name, c := range map[string]struct {
		p    security.Principal
		code errs.Code
	}{
		"the customer":                    {alice.p, errs.CodeForbidden},
		"an agent":                        {agent, errs.CodeForbidden},
		"an operator without authority":   {opsRole, errs.CodeForbidden},
		"no strong authentication":        {noMFA, errs.CodeStepUpRequired},
		"a strong authentication too old": {stale, errs.CodeStepUpRequired},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.propose(c.p, tr.ID, "FRAUD_CONFIRMED", "y")
			requireCode(t, err, c.code)
			_, err = f.approve(c.p, open.Proposal.ID)
			requireCode(t, err, c.code)
			_, err = f.reject(c.p, open.Proposal.ID, "z")
			requireCode(t, err, c.code)
		})
	}
	f.requireNotReversed(tr)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM transfer_reversal_proposals WHERE transfer_id = $1`, tr.ID))
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM transfer_reversal_decisions WHERE proposal_id = $1`, open.Proposal.ID))

	// Reads: an operator who reads every account sees it, anyone else is told
	// it does not exist (or may not list).
	got, err := f.svc.ReversalProposal(f.ctx, testDB, opsRole, open.Proposal.ID)
	require.NoError(t, err)
	assert.Equal(t, open.Proposal.ID, got.ID)
	_, err = f.svc.ReversalProposal(f.ctx, testDB, alice.p, open.Proposal.ID)
	requireCode(t, err, errs.CodeNotFound)
	_, err = f.svc.ReversalProposal(f.ctx, testDB, opsRole, NewReversalProposalID())
	requireCode(t, err, errs.CodeNotFound)
	_, _, err = f.svc.ListReversalProposals(f.ctx, testDB, alice.p, ReversalProposalFilter{})
	requireCode(t, err, errs.CodeForbidden)
	_, _, err = f.svc.ListReversalProposals(f.ctx, testDB, agent, ReversalProposalFilter{})
	requireCode(t, err, errs.CodeForbidden)

	// An unknown proposal is NOT_FOUND to a decider who may decide.
	_, err = f.approve(f.op(), NewReversalProposalID())
	requireCode(t, err, errs.CodeNotFound)
	// An unknown transfer is NOT_FOUND to a proposer.
	_, err = f.propose(f.op(), NewTransferID(), "FRAUD_CONFIRMED", "x")
	requireCode(t, err, errs.CodeNotFound)
}

// TestIntegration_ProposingTheSameReversalAgainIsAReplay: the proposer
// proposing the same reversal again gets the open proposal back; a
// different proposal while it is open is a CONFLICT naming it.
func TestIntegration_ProposingTheSameReversalAgainIsAReplay(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customer(), f.customer()
	tr := f.completedTransfer(alice, bob, 10_000_000, 1_000_000)
	proposer := f.op()
	first, err := f.propose(proposer, tr.ID, "FRAUD_CONFIRMED", "same words")
	require.NoError(t, err)
	again, err := f.propose(proposer, tr.ID, "FRAUD_CONFIRMED", "same words")
	require.NoError(t, err)
	assert.True(t, again.Replayed)
	assert.Equal(t, first.Proposal, again.Proposal)

	for name, c := range map[string]struct {
		p          security.Principal
		code, text string
	}{
		"another code":     {proposer, "DISPUTE_UPHELD", "same words"},
		"another reason":   {proposer, "FRAUD_CONFIRMED", "other words"},
		"another operator": {f.op(), "FRAUD_CONFIRMED", "same words"},
	} {
		_, err := f.propose(c.p, tr.ID, c.code, c.text)
		requireCode(t, err, errs.CodeConflict)
		assert.Equal(t, first.Proposal.ID.String(), fieldOf(t, err, "proposal_id"), name)
	}
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM transfer_reversal_proposals WHERE transfer_id = $1`, tr.ID))
}

// TestIntegration_ReversalProposalsAreListedNewestFirst: the operator's list,
// by status and by transfer, paged by cursor.
func TestIntegration_ReversalProposalsAreListedNewestFirst(t *testing.T) {
	f := newFixture(t)
	alice := f.customer()
	f.fund(alice.account, 50_000_000)
	reader := f.op()
	var transfers []Transfer
	for range 3 {
		bob := f.customer()
		res, err := f.send(alice.p, f.req(alice, bob, 1_000_000))
		require.NoError(t, err)
		transfers = append(transfers, res.Transfer)
	}
	var props []ReversalProposalID
	for i, tr := range transfers {
		f.clk.Advance(time.Second)
		p, err := f.propose(f.op(), tr.ID, "FRAUD_CONFIRMED", "listing "+string(rune('a'+i)))
		require.NoError(t, err)
		props = append(props, p.Proposal.ID)
	}
	_, err := f.reject(f.op(), props[0], "not fraud")
	require.NoError(t, err)
	_, err = f.approve(f.op(), props[1])
	require.NoError(t, err)

	ids := func(ps []ReversalProposal) []ReversalProposalID {
		out := []ReversalProposalID{}
		for _, p := range ps {
			if p.TransferID == transfers[0].ID || p.TransferID == transfers[1].ID || p.TransferID == transfers[2].ID {
				out = append(out, p.ID)
			}
		}
		return out
	}
	pending, _, err := f.svc.ListReversalProposals(f.ctx, testDB, reader, ReversalProposalFilter{Status: ProposalPending, Limit: 200})
	require.NoError(t, err)
	assert.Equal(t, []ReversalProposalID{props[2]}, ids(pending))
	approved, _, err := f.svc.ListReversalProposals(f.ctx, testDB, reader, ReversalProposalFilter{Status: ProposalApproved, Limit: 200})
	require.NoError(t, err)
	assert.Equal(t, []ReversalProposalID{props[1]}, ids(approved))
	rejected, _, err := f.svc.ListReversalProposals(f.ctx, testDB, reader, ReversalProposalFilter{Status: ProposalRejected, Limit: 200})
	require.NoError(t, err)
	assert.Equal(t, []ReversalProposalID{props[0]}, ids(rejected))
	one, _, err := f.svc.ListReversalProposals(f.ctx, testDB, reader, ReversalProposalFilter{TransferID: &transfers[1].ID})
	require.NoError(t, err)
	require.Len(t, one, 1)
	assert.Equal(t, props[1], one[0].ID)
	assert.Equal(t, ProposalApproved, one[0].Status())

	// Newest first, two at a time, for one transfer each, across all three:
	// filter by transfer to keep the test independent of other tests' rows.
	var seen []ReversalProposalID
	cursor := ""
	for {
		page, next, err := f.svc.ListReversalProposals(f.ctx, testDB, reader, ReversalProposalFilter{Cursor: cursor, Limit: 2})
		require.NoError(t, err)
		seen = append(seen, ids(page)...)
		if next == "" {
			break
		}
		cursor = next
	}
	assert.Equal(t, []ReversalProposalID{props[2], props[1], props[0]}, seen)

	_, _, err = f.svc.ListReversalProposals(f.ctx, testDB, reader, ReversalProposalFilter{Status: "CONSUMED"})
	requireCode(t, err, errs.CodeValidationFailed)
	_, _, err = f.svc.ListReversalProposals(f.ctx, testDB, reader, ReversalProposalFilter{Cursor: "not-an-id"})
	requireCode(t, err, errs.CodeValidationFailed)
}
