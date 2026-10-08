//go:build integration

package transfers

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/security"
)

// The tests in this file go around the Go code on purpose and prove the
// database (migration 00758) refuses every reversal state the dual control
// depends on never existing.

// rawProposal inserts a proposal row directly.
func rawProposal(ctx context.Context, tx pgx.Tx, tid TransferID, seq int, by string) (ReversalProposalID, error) {
	pid := NewReversalProposalID()
	_, err := tx.Exec(ctx, `INSERT INTO transfer_reversal_proposals (id, transfer_id, seq, reason_code, reason,
		proposed_by_actor_type, proposed_by_actor_id, proposed_at) VALUES ($1,$2,$3,'RAW_PROPOSAL','raw','OPERATOR',$4,now())`,
		pid, tid, seq, by)
	return pid, err
}

// rawDecision inserts a decision row directly.
func rawDecision(ctx context.Context, tx pgx.Tx, pid ReversalProposalID, decision ReversalDecision, by string) error {
	_, err := tx.Exec(ctx, `INSERT INTO transfer_reversal_decisions (id, proposal_id, decision, reason, decided_by_actor_type,
		decided_by_actor_id, decided_at) VALUES ($1,$2,$3,'raw','OPERATOR',$4,now())`, uuid.New(), pid, decision, by)
	return err
}

func operatorActor(p security.Principal) econ.Actor {
	return econ.Actor{Type: security.ActorOperator, ID: p.SubjectID}
}

// forceReversal drives the genuine reversal (reservation, COMPENSATION
// journal, compensated effect, evidence, REVERSED row naming prop) through
// the package's internals, skipping the dual control that should license it.
func (f *fixture) forceReversal(ctx context.Context, tx pgx.Tx, tid TransferID, prop ReversalProposal, actor econ.Actor) error {
	t, err := getTransfer(ctx, tx, tid, true)
	if err != nil {
		return err
	}
	_, err = f.svc.executeReversal(ctx, tx, actor, t, prop, "", "", f.clk.Now().UTC())
	return err
}

// TestIntegration_TheDatabaseRefusesASelfApprovedReversal: PT006, whatever
// the Go code does. A rejection by the proposer (a withdrawal) is allowed.
func TestIntegration_TheDatabaseRefusesASelfApprovedReversal(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customer(), f.customer()
	tr := f.completedTransfer(alice, bob, 10_000_000, 1_000_000)
	proposer := f.op()
	prop, err := f.propose(proposer, tr.ID, "FRAUD_CONFIRMED", "x")
	require.NoError(t, err)

	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return rawDecision(ctx, tx, prop.Proposal.ID, ReversalApproved, proposer.SubjectID)
	})
	require.Error(t, err)
	assert.Equal(t, "PT006", db.SQLState(err), "%v", err)
	f.requireNotReversed(tr)

	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return rawDecision(ctx, tx, prop.Proposal.ID, ReversalRejected, proposer.SubjectID)
	}), "the proposer may withdraw their own proposal")
	// Decided once: a second decision collides on UNIQUE (proposal_id).
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return rawDecision(ctx, tx, prop.Proposal.ID, ReversalRejected, f.op().SubjectID)
	})
	assert.Equal(t, db.SQLStateUniqueViolation, db.SQLState(err), "%v", err)
}

// TestIntegration_TheDatabaseRefusesAReversalWithoutAnApprovedTwoOperatorProposal:
// a REVERSED transfer must name a proposal (CHECK 23514), and the proposal it
// names must be APPROVED, of that transfer, with its reason code, by an
// operator other than the proposer who is the operator recorded as reversing
// it (PT007 at COMMIT) -- even when everything else about the reversal is the
// genuine article.
func TestIntegration_TheDatabaseRefusesAReversalWithoutAnApprovedTwoOperatorProposal(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customer(), f.customer()
	tr := f.completedTransfer(alice, bob, 20_000_000, 2_000_000)
	other, err := f.send(alice.p, f.req(alice, bob, 1_000_000))
	require.NoError(t, err)
	op1, op2, op3 := f.op(), f.op(), f.op()

	// No proposal at all: the immediate CHECK.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO internal_transfer_transitions (id, transfer_id, from_status, to_status, actor_type, actor_id, reason)
			VALUES ($1,$2,'COMPLETED','REVERSED','OPERATOR',$3,'raw reversal')`, uuid.New(), tr.ID, op1.SubjectID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE internal_transfers SET status = 'REVERSED', reversal_journal_transaction_id = journal_transaction_id,
			reversal_reason_code = 'RAW_REVERSAL', reversed_by_actor_id = $2, reversal_evidence_ref = 'pg://raw', reversed_at = now()
			WHERE id = $1`, tr.ID, op1.SubjectID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, db.SQLStateCheckViolation, db.SQLState(err), "%v", err)
	assert.Equal(t, "internal_transfers_reversal_proposed", db.ConstraintName(err))
	f.requireNotReversed(tr)

	// An undecided proposal: the genuine reversal, refused at COMMIT.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		prop := ReversalProposal{ID: NewReversalProposalID(), TransferID: tr.ID, Seq: 1, ReasonCode: "FRAUD_CONFIRMED",
			Reason: "undecided", ProposedBy: operatorActor(op1), ProposedAt: f.clk.Now().UTC()}
		if err := insertProposal(ctx, tx, prop); err != nil {
			return err
		}
		return f.forceReversal(ctx, tx, tr.ID, prop, operatorActor(op2))
	})
	require.Error(t, err)
	assert.Equal(t, "PT007", db.SQLState(err), "%v", err)
	f.requireNotReversed(tr)

	// A rejected proposal.
	rejected, err := f.propose(op1, tr.ID, "FRAUD_CONFIRMED", "to be rejected")
	require.NoError(t, err)
	_, err = f.reject(op2, rejected.Proposal.ID, "no")
	require.NoError(t, err)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return f.forceReversal(ctx, tx, tr.ID, rejected.Proposal, operatorActor(op2))
	})
	require.Error(t, err)
	assert.Equal(t, "PT007", db.SQLState(err), "%v", err)
	f.requireNotReversed(tr)

	open, err := f.propose(op1, tr.ID, "FRAUD_CONFIRMED", "the real one")
	require.NoError(t, err)
	for name, force := range map[string]func(ctx context.Context, tx pgx.Tx) error{
		// Approved by op2, but the reversal is recorded as op3's.
		"an approver who is not the reverser": func(ctx context.Context, tx pgx.Tx) error {
			if err := rawDecision(ctx, tx, open.Proposal.ID, ReversalApproved, op2.SubjectID); err != nil {
				return err
			}
			return f.forceReversal(ctx, tx, tr.ID, open.Proposal, operatorActor(op3))
		},
		// Approved, but the reversal carries a different reason code.
		"a different reason code": func(ctx context.Context, tx pgx.Tx) error {
			if err := rawDecision(ctx, tx, open.Proposal.ID, ReversalApproved, op2.SubjectID); err != nil {
				return err
			}
			changed := open.Proposal
			changed.ReasonCode = "SOMETHING_ELSE"
			return f.forceReversal(ctx, tx, tr.ID, changed, operatorActor(op2))
		},
		// An approved proposal of ANOTHER transfer.
		"another transfer's proposal": func(ctx context.Context, tx pgx.Tx) error {
			foreign := ReversalProposal{ID: NewReversalProposalID(), TransferID: other.Transfer.ID, Seq: 1, ReasonCode: "FRAUD_CONFIRMED",
				Reason: "foreign", ProposedBy: operatorActor(op1), ProposedAt: f.clk.Now().UTC()}
			if err := insertProposal(ctx, tx, foreign); err != nil {
				return err
			}
			if err := rawDecision(ctx, tx, foreign.ID, ReversalApproved, op2.SubjectID); err != nil {
				return err
			}
			return f.forceReversal(ctx, tx, tr.ID, foreign, operatorActor(op2))
		},
	} {
		err := f.inTx(force)
		require.Error(t, err, name)
		assert.Equal(t, "PT007", db.SQLState(err), "%s: %v", name, err)
		f.requireNotReversed(tr)
	}

	// The real thing still works, through the service.
	done, err := f.approve(op2, open.Proposal.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusReversed, done.Transfer.Status)
	f.requireReconciled()
}

// TestIntegration_TheDatabaseRefusesAnApprovalThatDidNotReverse: an APPROVED
// decision whose transaction does not reverse the transfer is refused at
// COMMIT (PT007): an approval cannot be banked for later.
func TestIntegration_TheDatabaseRefusesAnApprovalThatDidNotReverse(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customer(), f.customer()
	tr := f.completedTransfer(alice, bob, 10_000_000, 1_000_000)
	prop, err := f.propose(f.op(), tr.ID, "FRAUD_CONFIRMED", "x")
	require.NoError(t, err)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return rawDecision(ctx, tx, prop.Proposal.ID, ReversalApproved, f.op().SubjectID)
	})
	require.Error(t, err)
	assert.Equal(t, "PT007", db.SQLState(err), "%v", err)
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM transfer_reversal_decisions WHERE proposal_id = $1`, prop.Proposal.ID))
	f.requireNotReversed(tr)
}

// TestIntegration_TheDatabaseRefusesProposalsOutOfTurn: PT008 -- a proposal
// for a transfer that is not COMPLETED, one made while another is undecided,
// and one out of sequence.
func TestIntegration_TheDatabaseRefusesProposalsOutOfTurn(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customer(), f.customer()
	tr := f.completedTransfer(alice, bob, 10_000_000, 1_000_000)
	by := f.op().SubjectID
	insert := func(tid TransferID, seq int) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := rawProposal(ctx, tx, tid, seq, by)
			return err
		})
	}

	// A REJECTED transfer, and a PENDING one, are not open to a reversal.
	broke := f.customer()
	refused, err := f.send(broke.p, f.req(broke, bob, 1))
	require.Error(t, err)
	assert.Equal(t, "PT008", db.SQLState(insert(refused.Transfer.ID, 1)))
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		tid, _, err := f.rawPending(ctx, tx, alice, bob, 10, alice.user.String())
		if err != nil {
			return err
		}
		_, err = rawProposal(ctx, tx, tid, 1, by)
		return err
	})
	assert.Equal(t, "PT008", db.SQLState(err), "%v", err)
	assert.Equal(t, "PT008", db.SQLState(insert(NewTransferID(), 1)), "a transfer that does not exist")

	// Out of sequence, then a second one while the first is open.
	assert.Equal(t, "PT008", db.SQLState(insert(tr.ID, 2)))
	require.NoError(t, insert(tr.ID, 1))
	assert.Equal(t, "PT008", db.SQLState(insert(tr.ID, 2)), "proposal 1 is undecided")
	first, _, err := f.svc.ListReversalProposals(f.ctx, testDB, f.op(), ReversalProposalFilter{TransferID: &tr.ID})
	require.NoError(t, err)
	require.Len(t, first, 1)
	_, err = f.reject(f.op(), first[0].ID, "raw proposal")
	require.NoError(t, err)
	assert.Equal(t, "PT008", db.SQLState(insert(tr.ID, 1)), "the number is taken")
	assert.Equal(t, "PT008", db.SQLState(insert(tr.ID, 3)), "the next is 2")
	require.NoError(t, insert(tr.ID, 2))

	// A REVERSED transfer is closed to proposals for good.
	second, _, err := f.svc.ListReversalProposals(f.ctx, testDB, f.op(), ReversalProposalFilter{TransferID: &tr.ID, Status: ProposalPending})
	require.NoError(t, err)
	require.Len(t, second, 1)
	_, err = f.approve(f.op(), second[0].ID)
	require.NoError(t, err)
	assert.Equal(t, "PT008", db.SQLState(insert(tr.ID, 3)))
}

// TestIntegration_ReversalHistoryIsAppendOnly: proposals and decisions are
// never edited or deleted (no privilege for the application role, the
// immutability trigger for the owner); the transfer's proposal is written
// once, by COMPLETED -> REVERSED (PT002), and never at birth (PT003).
func TestIntegration_ReversalHistoryIsAppendOnly(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customer(), f.customer()
	tr := f.completedTransfer(alice, bob, 10_000_000, 1_000_000)
	untouched := f.completedTransfer(alice, bob, 0, 1_000_000)
	reversed, err := f.reverse(f.op(), f.op(), tr.ID, "FRAUD_CONFIRMED", "append-only")
	require.NoError(t, err)
	pid := *reversed.ReversalProposalID

	for _, stmt := range []string{
		`UPDATE transfer_reversal_proposals SET reason = 'edited' WHERE id = $1`,
		`DELETE FROM transfer_reversal_proposals WHERE id = $1`,
		`UPDATE transfer_reversal_decisions SET decision = 'REJECTED' WHERE proposal_id = $1`,
		`DELETE FROM transfer_reversal_decisions WHERE proposal_id = $1`,
	} {
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt, pid)
			return err
		})
		assert.Equal(t, db.SQLStateInsufficientPrivilege, db.SQLState(err), "%s: %v", stmt, err)
	}
	owner, err := pgx.Connect(f.ctx, testMigrateURL)
	require.NoError(t, err)
	defer owner.Close(f.ctx)
	for _, stmt := range []string{
		`UPDATE transfer_reversal_proposals SET reason = 'edited' WHERE id = $1`,
		`DELETE FROM transfer_reversal_proposals WHERE id = $1`,
		`UPDATE transfer_reversal_decisions SET decision = 'REJECTED' WHERE proposal_id = $1`,
		`DELETE FROM transfer_reversal_decisions WHERE proposal_id = $1`,
	} {
		_, err := owner.Exec(f.ctx, stmt, pid)
		assert.Equal(t, db.SQLStateRaiseException, db.SQLState(err), "%s: %v", stmt, err)
	}

	// The proposal a reversal names is frozen once written, and cannot be
	// attached to a transfer that was not reversed by it.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE internal_transfers SET reversal_proposal_id = NULL WHERE id = $1`, tr.ID)
		return err
	})
	assert.Equal(t, "PT002", db.SQLState(err), "%v", err)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE internal_transfers SET reversal_proposal_id = $2 WHERE id = $1`, untouched.ID, pid)
		return err
	})
	assert.Equal(t, "PT002", db.SQLState(err), "%v", err)

	// A transfer is never born naming a reversal proposal.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		tid, iid, err := f.rawPending(ctx, tx, alice, bob, 10, alice.user.String())
		if err != nil {
			return err
		}
		sum := sha256.Sum256([]byte(tid.String() + "-again"))
		_, err = tx.Exec(ctx, `INSERT INTO internal_transfers (id, intent_id, sender_account_id, sender_user_id, recipient_account_id,
			asset_id, quantity, notional_usd_minor, mode, idempotency_key, request_hash, policy_version, policy_hash, status,
			reversal_proposal_id, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,10,0,'SANDBOX',$7,$8,'raw',$9,'PENDING',$10,now())`,
			NewTransferID(), iid, alice.account, alice.user, bob.account, f.asset.ID, "raw-"+uuid.NewString(), sum[:],
			strings.Repeat("a", 64), pid)
		return err
	})
	assert.Equal(t, "PT003", db.SQLState(err), "%v", err)
	f.requireReconciled()
}
