//go:build integration

package delegation

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
)

// defaultPolicy auto-approves up to $50 to a destination at least an hour
// old, caps a withdrawal at $100 and a day at $200, and wants a strong
// authentication within five minutes for a human decision.
func (f *fixture) policy(c customer, mutate func(*WithdrawalPolicy)) WithdrawalPolicy {
	f.t.Helper()
	p := WithdrawalPolicy{
		AccountID: c.account, AutoApproveEnabled: true, AutoApproveMax: money.USDFromMinor(5_000), AutoApproveMaxRiskScore: 30,
		MaxRiskScore: 80, PerWithdrawalLimit: money.USDFromMinor(10_000), DailyLimit: money.USDFromMinor(20_000),
		VelocityMaxCount: 10, MinDestinationAge: time.Hour, CoolingPeriod: 0, StrongAuthRequired: true, StrongAuthMaxAge: 5 * time.Minute,
	}
	if mutate != nil {
		mutate(&p)
	}
	var out WithdrawalPolicy
	require.NoError(f.t, f.inTx(func(_ context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.SetWithdrawalPolicy(f.owner(c), tx, p)
		return err
	}))
	return out
}

func (f *fixture) destination(c customer) Destination {
	f.t.Helper()
	var d Destination
	require.NoError(f.t, f.inTx(func(_ context.Context, tx pgx.Tx) error {
		var err error
		d, err = f.svc.AddDestination(f.owner(c), tx, DestinationRequest{AccountID: c.account, Chain: devnethost.ChainSolana,
			Address: devnet.NewKeypairFromSeed(seedOf("dest-" + uuid.NewString())).PublicKey(), Label: "cold wallet"})
		return err
	}))
	return d
}

// withdrawalSetup is a funded customer with a policy, an aged destination and
// a capability that allows it: everything an auto-approval needs.
type withdrawalSetup struct {
	c    customer
	dest Destination
	cap  Capability
	pol  WithdrawalPolicy
}

func (f *fixture) withdrawalSetup(mutatePolicy func(*WithdrawalPolicy), mutateGrant func(*GrantRequest)) withdrawalSetup {
	f.t.Helper()
	c := f.customer(100_000_000) // $100
	pol := f.policy(c, mutatePolicy)
	dest := f.destination(c)
	capability := f.grant(c, mutateGrant, dest.Address)
	f.advance(2 * time.Hour)
	return withdrawalSetup{c: c, dest: dest, cap: capability, pol: pol}
}

func (f *fixture) proposal(s withdrawalSetup, nonce uint64, amount int64) ProposalRequest {
	return ProposalRequest{CapabilityID: s.cap.ID, AccountID: s.c.account, Nonce: nonce, DestinationID: s.dest.ID,
		AssetID: f.usd.ID, Amount: money.QuantityFromInt64(amount), Jurisdiction: "US-CA", CorrelationID: "corr-" + uuid.NewString()}
}

// propose runs ProposeWithdrawal as the agent; a recorded refusal is
// committed and returned as the error.
func (f *fixture) propose(ctx context.Context, req ProposalRequest) (WithdrawalTicket, error) {
	var ticket WithdrawalTicket
	err := f.inTx(func(_ context.Context, tx pgx.Tx) error {
		var err error
		ticket, err = f.svc.ProposeWithdrawal(ctx, tx, req)
		return err
	})
	if err != nil {
		return WithdrawalTicket{}, err
	}
	return ticket, ticket.Refusal
}

func (f *fixture) mustPropose(s withdrawalSetup, nonce uint64, amount int64) Withdrawal {
	f.t.Helper()
	ticket, err := f.propose(f.agent(s.c), f.proposal(s, nonce, amount))
	require.NoError(f.t, err)
	return ticket.Withdrawal
}

func (f *fixture) decideWithdrawal(ctx context.Context, d WithdrawalDecision) (Withdrawal, error) {
	var out Withdrawal
	err := f.inTx(func(_ context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.DecideWithdrawal(ctx, tx, d)
		return err
	})
	return out, err
}

func (f *fixture) withdrawal(wid WithdrawalID) Withdrawal {
	f.t.Helper()
	w, err := f.svc.GetWithdrawal(f.ctx, testDB, wid)
	require.NoError(f.t, err)
	return w
}

// offerModifiedWithdrawal compiles a withdrawal as SignWithdrawal does, then
// offers the provider a modified message, and commits the decision.
func (f *fixture) offerModifiedWithdrawal(w Withdrawal, mutate func(*devnet.Message)) SigningDecision {
	f.t.Helper()
	var d SigningDecision
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		locked, err := getWithdrawal(ctx, tx, w.ID, true)
		if err != nil {
			return err
		}
		tip, err := tipOf(ctx, f.host, tx, locked.Chain)
		if err != nil {
			return err
		}
		msg, err := locked.Plan.Message(tip.Hash)
		if err != nil {
			return err
		}
		compiled, err := msg.Bytes()
		if err != nil {
			return err
		}
		height := tip.Height
		if _, err := f.svc.setWithdrawalColumns(ctx, tx, locked, subjectSet{message: compiled, recentBlockhash: &tip.Hash, blockhashHeight: &height}); err != nil {
			return err
		}
		modified := msg
		modified.Instructions = append([]devnet.Instruction(nil), msg.Instructions...)
		mutate(&modified)
		b, err := modified.Bytes()
		if err != nil {
			return err
		}
		d, err = f.signer.Sign(ctx, tx, SignRequest{Kind: SubjectWithdrawal, SubjectID: w.ID.String(), Message: b, MessageSHA256: sha256Of(b)})
		return err
	}))
	return d
}

// TestIntegration_AnAutoApprovedWithdrawalSettlesInTestMode: proposal ->
// reservation -> policy evaluation (auto, low-risk test mode) -> compile ->
// inspection -> bounded signing -> submit -> finality -> reconciliation ->
// SANDBOX_WITHDRAWAL -> evidence.
func TestIntegration_AnAutoApprovedWithdrawalSettlesInTestMode(t *testing.T) {
	f := newFixture(t)
	s := f.withdrawalSetup(nil, nil)
	w := f.mustPropose(s, 1, 5_000_000) // $5
	require.Equal(t, WithdrawalApproved, w.Status)
	assert.Equal(t, OutcomeAutoApprove, w.EvaluationOutcome)
	assert.Equal(t, s.dest.Address, w.DestinationAddress)
	in := f.intentOf(w.IntentID)
	assert.Equal(t, econ.KindWithdraw, in.Kind)
	assert.Equal(t, security.ActorService, in.ActorType, "the WITHDRAW intent is the service's, never the agent's")
	assert.Empty(t, in.AgentID)
	assert.Equal(t, s.cap.ID.String(), in.DelegationID)
	assert.Equal(t, econ.StatusReserved, in.Status)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM delegated_withdrawal_approvals WHERE withdrawal_id = $1
		AND approver_actor_type = 'SERVICE' AND decision = 'APPROVE'`, w.ID))
	assert.Equal(t, 2, f.count(`SELECT count(*) FROM delegated_withdrawal_transitions WHERE withdrawal_id = $1`, w.ID),
		"NONE->PROPOSED, PROPOSED->APPROVED: every step leaves its transition row")

	res, err := f.svc.ExecuteWithdrawal(f.ctx, w.ID)
	require.NoError(t, err)
	require.Equal(t, WithdrawalSettled, res.Withdrawal.Status, "failure %q", res.Withdrawal.FailureCode)
	w = res.Withdrawal
	assert.Equal(t, "5000000", f.balance(s.c.account, ledger.CodeWithdrawn, f.usd).String())
	assert.Equal(t, "95000000", f.balance(s.c.account, ledger.CodeWallet, f.usd).String())
	assert.Equal(t, uint64(5_000_000), f.chainBalance(s.dest.Address, f.usd))
	assert.Equal(t, uint64(95_000_000), f.chainBalance(s.c.wallet, f.usd))
	assert.Equal(t, capital.ReservationConsumed, f.reservationOf(w.ReservationID).Status)
	assert.Equal(t, econ.StatusCompleted, f.intentOf(w.IntentID).Status)
	e, err := f.effects.Get(f.ctx, testDB, *w.EffectID)
	require.NoError(t, err)
	assert.Equal(t, econ.EffectApplied, e.Status)
	assert.Equal(t, econ.SubsystemWithdrawals, e.Subsystem)
	assert.NotEmpty(t, e.EvidenceRef)
	j, err := f.ledger.Transaction(f.ctx, testDB, *w.JournalTransactionID)
	require.NoError(t, err)
	assert.Equal(t, ledger.KindSandboxWithdrawal, j.Kind)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM delegated_signing_decisions WHERE withdrawal_id = $1 AND verdict = 'APPROVED'`, w.ID))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM advanced_risk_decisions WHERE intent_id = $1`, w.IntentID))
}

// TestIntegration_AHumanDecidesEverythingElse: a proposal outside low-risk
// test mode waits for the owner, who must be strongly authenticated, must
// decide the exact plan, and whose decline is itself an anomaly afterwards.
func TestIntegration_AHumanDecidesEverythingElse(t *testing.T) {
	f := newFixture(t)
	s := f.withdrawalSetup(nil, nil)
	big := f.mustPropose(s, 1, 60_000_000) // $60: above the $50 auto limit
	require.Equal(t, WithdrawalAwaitingApproval, big.Status)
	assert.Contains(t, big.Evaluation.ReasonCodes, ReasonHumanAmountAboveAuto)

	_, err := f.decideWithdrawal(f.agent(s.c), WithdrawalDecision{WithdrawalID: big.ID, PlanHash: big.PlanHash, Approve: true, Reason: "self"})
	requireCode(t, err, errs.CodeForbidden)
	stale := security.WithPrincipal(f.ctx, security.Principal{SubjectID: s.c.user.String(), ActorType: security.ActorUser,
		Roles: []security.Role{security.RoleCustomer}, AccountIDs: []string{s.c.account.String()},
		AuthTime: f.clk.Now().Add(-10 * time.Minute), AMR: []string{"mfa"}})
	_, err = f.decideWithdrawal(stale, WithdrawalDecision{WithdrawalID: big.ID, PlanHash: big.PlanHash, Approve: true, Reason: "stale"})
	requireCode(t, err, errs.CodeStepUpRequired)
	_, err = f.decideWithdrawal(f.owner(s.c), WithdrawalDecision{WithdrawalID: big.ID, PlanHash: [32]byte{7}, Approve: true, Reason: "other"})
	requireCode(t, err, errs.CodeConflict)

	approved, err := f.decideWithdrawal(f.owner(s.c), WithdrawalDecision{WithdrawalID: big.ID, PlanHash: big.PlanHash, Approve: true, Reason: "mine"})
	require.NoError(t, err)
	require.Equal(t, WithdrawalApproved, approved.Status)
	res, err := f.svc.ExecuteWithdrawal(f.ctx, big.ID)
	require.NoError(t, err)
	assert.Equal(t, WithdrawalSettled, res.Withdrawal.Status)

	// A young destination needs a person too.
	young := f.destination(s.c)
	grant2 := f.grant(s.c, nil, young.Address)
	req := f.proposal(withdrawalSetup{c: s.c, dest: young, cap: grant2}, 1, 1_000_000)
	ticket, err := f.propose(f.agent(s.c), req)
	require.NoError(t, err)
	assert.Equal(t, WithdrawalAwaitingApproval, ticket.Withdrawal.Status)
	assert.Contains(t, ticket.Withdrawal.Evaluation.ReasonCodes, ReasonHumanDestinationTooNew)

	// A decline returns the funds, and the next proposal is anomalous.
	declined, err := f.decideWithdrawal(f.owner(s.c), WithdrawalDecision{WithdrawalID: ticket.Withdrawal.ID,
		PlanHash: ticket.Withdrawal.PlanHash, Approve: false, Reason: "not mine"})
	require.NoError(t, err)
	assert.Equal(t, WithdrawalDeclined, declined.Status)
	assert.Equal(t, capital.ReservationReleased, f.reservationOf(declined.ReservationID).Status)
	assert.Equal(t, econ.StatusCancelled, f.intentOf(declined.IntentID).Status)
	small := f.mustPropose(s, 2, 1_000_000)
	assert.Equal(t, WithdrawalAwaitingApproval, small.Status)
	assert.Contains(t, small.Evaluation.Anomalies, string(AnomalyRecentDecline))
}

// TestIntegration_NoRawAddressSubstitutionAfterApproval: the approved
// destination is frozen in the plan whose hash the approval binds. No role
// can change it, the destination row cannot be edited, and the provider
// refuses a transaction to any other address -- even one the agent's
// capability allows.
func TestIntegration_NoRawAddressSubstitutionAfterApproval(t *testing.T) {
	f := newFixture(t)
	other := devnet.NewKeypairFromSeed(seedOf("other-" + uuid.NewString())).PublicKey()
	s := f.withdrawalSetup(nil, func(r *GrantRequest) {
		r.DestinationAllowlist = append(r.DestinationAllowlist, other)
	})
	w := f.mustPropose(s, 1, 2_000_000)
	require.Equal(t, WithdrawalApproved, w.Status)

	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE delegated_withdrawals SET destination_address = $2 WHERE id = $1`, w.ID, other.String())
		return err
	})
	requireSQLState(t, err, "42501")
	owner := ownerConn(t)
	for _, sql := range []string{
		`UPDATE delegated_withdrawals SET destination_address = $2 WHERE id = $1`,
		`UPDATE delegated_withdrawals SET plan = jsonb_set(plan, '{destination_address}', to_jsonb($2::text)) WHERE id = $1`,
	} {
		_, err := owner.Exec(f.ctx, sql, w.ID, other.String())
		requireSQLState(t, err, "DW002")
	}
	_, err = owner.Exec(f.ctx, `UPDATE delegated_withdrawal_destinations SET address = $2 WHERE id = $1`, s.dest.ID, other.String())
	requireSQLState(t, err, "DW002")

	d := f.offerModifiedWithdrawal(w, func(m *devnet.Message) {
		p := m.Instructions[0].(devnet.TokenTransfer)
		p.ToOwner = other
		m.Instructions[0] = p
	})
	require.Equal(t, VerdictRejected, d.Verdict)
	assert.Contains(t, d.ReasonCodes, InspectDestinationMismatch)
	_, err = f.svc.ExecuteWithdrawal(f.ctx, w.ID)
	requireCode(t, err, errs.CodeSigningRejected)
	refusedW := f.withdrawal(w.ID)
	assert.Equal(t, WithdrawalRejected, refusedW.Status, "the one decision was a refusal: this withdrawal is never signed")
	assert.Equal(t, capital.ReservationReleased, f.reservationOf(refusedW.ReservationID).Status)
	assert.Equal(t, uint64(0), f.chainBalance(other, f.usd))

	// The honest path pays only the approved destination.
	w2 := f.mustPropose(s, 2, 2_000_000)
	res, err := f.svc.ExecuteWithdrawal(f.ctx, w2.ID)
	require.NoError(t, err)
	require.Equal(t, WithdrawalSettled, res.Withdrawal.Status)
	assert.Equal(t, uint64(2_000_000), f.chainBalance(s.dest.Address, f.usd))
	assert.Equal(t, uint64(0), f.chainBalance(other, f.usd))
}

// TestIntegration_NoAmountMutationAfterApproval.
func TestIntegration_NoAmountMutationAfterApproval(t *testing.T) {
	f := newFixture(t)
	s := f.withdrawalSetup(nil, nil)
	w := f.mustPropose(s, 1, 60_000_000) // needs the owner
	approved, err := f.decideWithdrawal(f.owner(s.c), WithdrawalDecision{WithdrawalID: w.ID, PlanHash: w.PlanHash, Approve: true, Reason: "ok"})
	require.NoError(t, err)
	require.Equal(t, WithdrawalApproved, approved.Status)

	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE delegated_withdrawals SET amount = amount * 10 WHERE id = $1`, w.ID)
		return err
	})
	requireSQLState(t, err, "42501")
	owner := ownerConn(t)
	_, err = owner.Exec(f.ctx, `UPDATE delegated_withdrawals SET amount = amount * 10, notional_usd_minor = notional_usd_minor * 10 WHERE id = $1`, w.ID)
	requireSQLState(t, err, "DW002")
	// A second approval, of another plan, cannot be recorded.
	forged := sha256Of([]byte("a bigger plan"))
	err = rawTx(f.ctx, owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(f.ctx, `INSERT INTO delegated_withdrawal_approvals (id, withdrawal_id, decision, plan_hash, approver_actor_type,
			approver_actor_id, authenticated_at, decided_at, reason) VALUES ($1, $2, 'APPROVE', $3, 'USER', $4, now(), now(), 'forged')`,
			uuid.New(), w.ID, forged[:], s.c.user.String())
		return err
	})
	require.Error(t, err)

	d := f.offerModifiedWithdrawal(w, func(m *devnet.Message) {
		p := m.Instructions[0].(devnet.TokenTransfer)
		p.Amount *= 10
		m.Instructions[0] = p
	})
	require.Equal(t, VerdictRejected, d.Verdict)
	assert.Contains(t, d.ReasonCodes, InspectAmountMismatch)
	assert.Equal(t, "100000000", f.balance(s.c.account, ledger.CodeWallet, f.usd).String(), "nothing moved")
}

// TestIntegration_WithdrawalReplayIsRefused.
func TestIntegration_WithdrawalReplayIsRefused(t *testing.T) {
	f := newFixture(t)
	s := f.withdrawalSetup(nil, nil)
	req := f.proposal(s, 9, 1_000_000)
	first, err := f.propose(f.agent(s.c), req)
	require.NoError(t, err)
	again, err := f.propose(f.agent(s.c), req)
	require.NoError(t, err)
	assert.True(t, again.Replay)
	assert.Equal(t, first.Withdrawal.ID, again.Withdrawal.ID)
	reuse := req
	reuse.Amount = money.QuantityFromInt64(1_000_001)
	_, err = f.propose(f.agent(s.c), reuse)
	requireCode(t, err, errs.CodeInvalidIdempotencyReuse)

	res, err := f.svc.ExecuteWithdrawal(f.ctx, first.Withdrawal.ID)
	require.NoError(t, err)
	require.Equal(t, WithdrawalSettled, res.Withdrawal.Status)
	replay, err := f.svc.ExecuteWithdrawal(f.ctx, first.Withdrawal.ID)
	require.NoError(t, err)
	assert.True(t, replay.Replay)
	_, err = f.host.Submit(f.ctx, devnethost.ChainSolana, res.Withdrawal.SignedTx)
	require.NoError(t, err)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM journal_transactions WHERE reference_type = 'delegated_withdrawal' AND reference_id = $1`,
		first.Withdrawal.ID.String()))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM delegated_capability_usage WHERE subject_id = $1`, first.Withdrawal.ID))
	assert.Equal(t, uint64(1_000_000), f.chainBalance(s.dest.Address, f.usd), "paid out once")
}

// TestIntegration_UnknownWithdrawalSubmissionIsReconciled.
func TestIntegration_UnknownWithdrawalSubmissionIsReconciled(t *testing.T) {
	f := newFixture(t)
	s := f.withdrawalSetup(nil, nil)
	landed := f.mustPropose(s, 1, 1_000_000)
	dropped := f.mustPropose(s, 2, 1_000_000)
	require.NoError(t, f.host.TimeoutButLand(f.ctx, devnethost.ChainSolana))
	_, err := f.svc.ExecuteWithdrawal(f.ctx, landed.ID)
	requireCode(t, err, errs.CodeSubmissionStateUnknown)
	require.Equal(t, WithdrawalUnknown, f.withdrawal(landed.ID).Status)
	require.NoError(t, f.host.TimeoutAndDrop(f.ctx, devnethost.ChainSolana))
	_, err = f.svc.ExecuteWithdrawal(f.ctx, dropped.ID)
	requireCode(t, err, errs.CodeSubmissionStateUnknown)

	_, err = f.svc.ReconcilePending(f.ctx, 100, true)
	require.NoError(t, err)
	got := f.withdrawal(landed.ID)
	assert.Equal(t, WithdrawalSettled, got.Status)
	assert.Equal(t, econ.StatusReconciled, f.intentOf(got.IntentID).Status)
	gone := f.withdrawal(dropped.ID)
	assert.Equal(t, WithdrawalFailed, gone.Status)
	assert.Equal(t, capital.ReservationReleased, f.reservationOf(gone.ReservationID).Status)
	for _, w := range []Withdrawal{got, gone} {
		assert.Equal(t, 1, f.count(`SELECT count(*) FROM devnet_events WHERE network = $1 AND kind = 'SUBMIT' AND tx_id = $2`,
			"dg-"+f.tag, string(w.TxID)), "submitted exactly once")
	}
	assert.Equal(t, uint64(1_000_000), f.chainBalance(s.dest.Address, f.usd))
	assert.Equal(t, "1000000", f.balance(s.c.account, ledger.CodeWithdrawn, f.usd).String())
}

// TestIntegration_WithdrawalDailyLimitHoldsUnderConcurrency: concurrent
// proposals never together exceed the policy's daily limit.
func TestIntegration_WithdrawalDailyLimitHoldsUnderConcurrency(t *testing.T) {
	f := newFixture(t)
	s := f.withdrawalSetup(func(p *WithdrawalPolicy) {
		p.PerWithdrawalLimit, p.DailyLimit, p.AutoApproveMax = money.USDFromMinor(1_000), money.USDFromMinor(1_000), money.USDFromMinor(1_000)
		p.VelocityMaxCount = 100
		p.MaxRiskScore, p.AutoApproveMaxRiskScore = 100, 100
	}, func(r *GrantRequest) { r.ExpiresAt = f.clk.Now().Add(72 * time.Hour) })
	const n = 9 // $2 each; the $10 day admits exactly 5
	var wg sync.WaitGroup
	errsOut := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errsOut[i] = f.propose(f.agent(s.c), f.proposal(s, uint64(i+1), 2_000_000))
		}(i)
	}
	wg.Wait()
	admitted := 0
	for _, err := range errsOut {
		if err == nil {
			admitted++
			continue
		}
		requireReason(t, err, ReasonWithdrawalDailyLimit)
	}
	assert.Equal(t, 5, admitted)
	assert.Equal(t, 1_000, f.count(`SELECT coalesce(sum(notional_usd_minor),0)::int FROM delegated_withdrawals WHERE account_id = $1`, s.c.account))
	// A new day starts afresh.
	f.advance(24 * time.Hour)
	f.mustPropose(s, 100, 2_000_000)
}

// TestIntegration_WithdrawalAllowlistIsEnforced: only an ACTIVE destination
// on the account's own allowlist, and on the agent's capability, can receive
// a withdrawal, and removing it stops one not yet signed.
func TestIntegration_WithdrawalAllowlistIsEnforced(t *testing.T) {
	f := newFixture(t)
	s := f.withdrawalSetup(nil, nil)
	stranger := f.withdrawalSetup(nil, nil)

	// Another account's destination is not this account's to use.
	req := f.proposal(s, 1, 1_000_000)
	req.DestinationID = stranger.dest.ID
	_, err := f.propose(f.agent(s.c), req)
	requireCode(t, err, errs.CodeNotFound)

	// A destination the customer allowlisted but the capability does not.
	unscoped := f.destination(s.c)
	f.advance(2 * time.Hour)
	req = f.proposal(s, 2, 1_000_000)
	req.DestinationID = unscoped.ID
	_, err = f.propose(f.agent(s.c), req)
	requireCode(t, err, errs.CodeRiskRejected)
	requireReason(t, err, risk.ReasonDestinationNotAllowed)

	// Approved, then removed from the allowlist before signing: stopped.
	w := f.mustPropose(s, 3, 1_000_000)
	require.Equal(t, WithdrawalApproved, w.Status)
	require.NoError(t, f.inTx(func(_ context.Context, tx pgx.Tx) error {
		_, err := f.svc.RemoveDestination(f.owner(s.c), tx, s.dest.ID, "compromised")
		return err
	}))
	_, err = f.svc.ExecuteWithdrawal(f.ctx, w.ID)
	requireCode(t, err, errs.CodeSigningRejected)
	got := f.withdrawal(w.ID)
	assert.Equal(t, WithdrawalRejected, got.Status)
	assert.Equal(t, ReasonDestinationRemoved, got.FailureCode)
	assert.Equal(t, capital.ReservationReleased, f.reservationOf(got.ReservationID).Status)
	// And no new proposal can name it.
	_, err = f.propose(f.agent(s.c), f.proposal(s, 4, 1_000_000))
	requireCode(t, err, errs.CodeRiskRejected)
}

// TestIntegration_AFrozenAccountStopsWithdrawalsAtEveryStep: a freeze --
// the account's status or an ACCOUNT_FREEZE switch -- stops a withdrawal at
// the proposal, at the approval, at the signing and at the submission, and
// nothing ever reaches the chain.
func TestIntegration_AFrozenAccountStopsWithdrawalsAtEveryStep(t *testing.T) {
	f := newFixture(t)

	// 1. Before the proposal: refused and recorded.
	s1 := f.withdrawalSetup(nil, nil)
	f.freezeAccount(s1.c)
	_, err := f.propose(f.agent(s1.c), f.proposal(s1, 1, 1_000_000))
	requireCode(t, err, errs.CodeAccountFrozen)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM economic_intents WHERE account_id = $1 AND status = 'REJECTED'
		AND rejection_code = $2`, s1.c.account, RefusalAccountFrozen))

	// 2. Before the approval: the owner cannot approve (Go and SQL).
	s2 := f.withdrawalSetup(nil, nil)
	w2 := f.mustPropose(s2, 1, 60_000_000)
	require.Equal(t, WithdrawalAwaitingApproval, w2.Status)
	f.activateKill(killswitch.AccountFreeze, s2.c.account.String())
	_, err = f.decideWithdrawal(f.owner(s2.c), WithdrawalDecision{WithdrawalID: w2.ID, PlanHash: w2.PlanHash, Approve: true, Reason: "go"})
	requireCode(t, err, errs.CodeKillSwitchActive)
	assert.Equal(t, WithdrawalAwaitingApproval, f.withdrawal(w2.ID).Status)
	owner := ownerConn(t)
	err = rawTx(f.ctx, owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(f.ctx, `INSERT INTO delegated_withdrawal_approvals (id, withdrawal_id, decision, plan_hash, approver_actor_type,
			approver_actor_id, authenticated_at, decided_at, reason) VALUES ($1, $2, 'APPROVE', $3, 'USER', $4, $5, $5, 'raw')`,
			uuid.New(), w2.ID, w2.PlanHash[:], s2.c.user.String(), f.clk.Now())
		return err
	})
	requireSQLState(t, err, "DW005")

	// 3. Before the signing.
	s3 := f.withdrawalSetup(nil, nil)
	w3 := f.mustPropose(s3, 1, 1_000_000)
	require.Equal(t, WithdrawalApproved, w3.Status)
	f.freezeAccount(s3.c)
	_, err = f.svc.ExecuteWithdrawal(f.ctx, w3.ID)
	requireCode(t, err, errs.CodeSigningRejected)
	got3 := f.withdrawal(w3.ID)
	assert.Equal(t, WithdrawalRejected, got3.Status)
	assert.Equal(t, RefusalAccountFrozen, got3.FailureCode)
	assert.Empty(t, got3.TxID)

	// 4. After the signing, before the submission.
	s4 := f.withdrawalSetup(nil, nil)
	w4 := f.mustPropose(s4, 1, 1_000_000)
	require.NoError(t, f.host.EnsureNativeBalance(f.ctx, devnethost.ChainSolana, s4.c.wallet, 1_000_000, 100_000_000))
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		out, err := f.svc.SignWithdrawal(ctx, tx, w4.ID)
		require.True(t, out.Fresh)
		return err
	}))
	f.freezeAccount(s4.c)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.GateWithdrawalSubmission(ctx, tx, w4.ID)
		if reasonOf(err) == ReasonBlockedBeforeSubmission {
			return nil
		}
		return err
	})
	require.NoError(t, err)
	got4 := f.withdrawal(w4.ID)
	assert.Equal(t, WithdrawalFailed, got4.Status)
	assert.Equal(t, ReasonBlockedBeforeSubmission+":"+RefusalAccountFrozen, got4.FailureCode)
	st, err := f.host.Status(f.ctx, devnethost.ChainSolana, got4.TxID)
	require.NoError(t, err)
	assert.Equal(t, devnet.TxNotFound, st.State, "never submitted")
	assert.Equal(t, capital.ReservationReleased, f.reservationOf(got4.ReservationID).Status)

	// 5. A switch that stops every withdrawal stops a signature in SQL too
	// (rolled back, so it never touches another test).
	s5 := f.withdrawalSetup(nil, nil)
	w5 := f.mustPropose(s5, 1, 1_000_000)
	err = rawTx(f.ctx, owner, func(tx pgx.Tx) error {
		sw := uuid.New()
		if _, err := tx.Exec(f.ctx, `INSERT INTO kill_switches (id, kind, scope_id, active, severity, reason, activated_by_actor_id, activated_at)
			VALUES ($1, 'WITHDRAWALS_DISABLE', '*', true, 'SEVERE', 'itest', 'itest', now())
			ON CONFLICT (kind, scope_id) DO UPDATE SET active = true`, sw); err != nil {
			return err
		}
		_, err := tx.Exec(f.ctx, `UPDATE delegated_withdrawals SET status = 'SIGNED', message = '\x01', signed_at = $2 WHERE id = $1`,
			w5.ID, f.clk.Now())
		return err
	})
	requireSQLState(t, err, "DW005")
	assert.Equal(t, uint64(0), f.chainBalance(s1.dest.Address, f.usd)+f.chainBalance(s2.dest.Address, f.usd)+
		f.chainBalance(s3.dest.Address, f.usd)+f.chainBalance(s4.dest.Address, f.usd))
}

// TestIntegration_TheDatabaseRefusesUnlicensedWithdrawalStates.
func TestIntegration_TheDatabaseRefusesUnlicensedWithdrawalStates(t *testing.T) {
	f := newFixture(t)
	s := f.withdrawalSetup(func(p *WithdrawalPolicy) { p.CoolingPeriod = time.Hour }, nil)
	owner := ownerConn(t)
	ctx := f.ctx

	t.Run("signed without an approved decision, inside its cooling period (DW006)", func(t *testing.T) {
		w := f.mustPropose(s, 1, 1_000_000)
		require.Equal(t, WithdrawalApproved, w.Status)
		err := rawTx(ctx, owner, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO delegated_withdrawal_transitions (id, withdrawal_id, from_status, to_status, actor_type, actor_id, reason)
				VALUES ($1, $2, 'APPROVED', 'SIGNED', 'SYSTEM', 'forger', 'bypass')`, uuid.New(), w.ID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE delegated_withdrawals SET status = 'SIGNED', message = '\x01', recent_blockhash = 'x',
				blockhash_height = 1, signed_at = $2, tx_id = $3, signed_tx = '\x01' WHERE id = $1`, w.ID, f.clk.Now(), uuid.NewString())
			return err
		})
		requireSQLState(t, err, "DW006")
	})

	t.Run("the policy approving what needed a person (DW003)", func(t *testing.T) {
		w := f.mustPropose(s, 2, 60_000_000)
		require.Equal(t, WithdrawalAwaitingApproval, w.Status)
		err := rawTx(ctx, owner, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO delegated_withdrawal_approvals (id, withdrawal_id, decision, plan_hash, approver_actor_type,
				approver_actor_id, decided_at, reason) VALUES ($1, $2, 'APPROVE', $3, 'SERVICE', 'delegation-withdrawal-policy', $4, 'auto')`,
				uuid.New(), w.ID, w.PlanHash[:], f.clk.Now())
			return err
		})
		requireSQLState(t, err, "DW003")
		// The owner without the strong authentication the policy demands.
		err = rawTx(ctx, owner, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO delegated_withdrawal_approvals (id, withdrawal_id, decision, plan_hash, approver_actor_type,
				approver_actor_id, authenticated_at, decided_at, reason) VALUES ($1, $2, 'APPROVE', $3, 'USER', $4, $5, $6, 'weak')`,
				uuid.New(), w.ID, w.PlanHash[:], s.c.user.String(), f.clk.Now().Add(-time.Hour), f.clk.Now())
			return err
		})
		requireSQLState(t, err, "DW004")
	})

	t.Run("policies are immutable and versioned by the owner (P0001, DW003)", func(t *testing.T) {
		_, err := owner.Exec(ctx, `UPDATE delegated_withdrawal_policies SET daily_limit_minor = 999999999 WHERE id = $1`, s.pol.ID)
		requireSQLState(t, err, "P0001")
		err = rawTx(ctx, owner, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO delegated_withdrawal_policies (id, account_id, version, auto_approve_enabled, auto_approve_max_minor,
				auto_approve_max_risk_score, max_risk_score, per_withdrawal_limit_minor, daily_limit_minor, velocity_max_count,
				min_destination_age_seconds, min_account_age_seconds, cooling_period_seconds, strong_auth_required, strong_auth_max_age_seconds,
				policy_hash, created_by_actor_type, created_by_actor_id, effective_at)
				SELECT $1, account_id, version + 1, true, 999999, 100, 100, 999999, 999999, 1000, 0, 0, 0, false, 60, policy_hash, 'USER', $2, now()
				  FROM delegated_withdrawal_policies WHERE id = $3`, uuid.New(), s.c.agentID, s.pol.ID)
			return err
		})
		requireSQLState(t, err, "DW003")
	})

	t.Run("a settled withdrawal that is not its SANDBOX_WITHDRAWAL (DW007)", func(t *testing.T) {
		dv := f.withdrawalSetup(nil, func(r *GrantRequest) { r.Mode = gates.ModeDevnet })
		w := f.mustPropose(dv, 1, 1_000_000)
		res, err := f.svc.ExecuteWithdrawal(ctx, w.ID)
		require.NoError(t, err)
		require.Equal(t, WithdrawalSubmitted, res.Withdrawal.Status)
		var funding string
		require.NoError(t, testDB.QueryRow(ctx, `SELECT id::text FROM journal_transactions WHERE kind = 'SANDBOX_FUNDING'
			ORDER BY posted_at LIMIT 1`).Scan(&funding))
		err = rawTx(ctx, owner, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO delegated_withdrawal_transitions (id, withdrawal_id, from_status, to_status, actor_type, actor_id, reason)
				VALUES ($1, $2, 'SUBMITTED', 'SETTLED', 'SYSTEM', 'forger', 'bypass')`, uuid.New(), w.ID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE delegated_withdrawals SET status = 'SETTLED', journal_transaction_id = $2 WHERE id = $1`, w.ID, funding)
			return err
		})
		requireSQLState(t, err, "DW007")
	})
}

// TestIntegration_TheCoolingPeriodDelaysExecution: an approved withdrawal
// is not signed before its cooling period has elapsed, and nothing changes
// while it waits.
func TestIntegration_TheCoolingPeriodDelaysExecution(t *testing.T) {
	f := newFixture(t)
	s := f.withdrawalSetup(func(p *WithdrawalPolicy) { p.CoolingPeriod = time.Hour }, nil)
	w := f.mustPropose(s, 1, 1_000_000)
	require.Equal(t, WithdrawalApproved, w.Status)
	_, err := f.svc.ExecuteWithdrawal(f.ctx, w.ID)
	requireReason(t, err, ReasonCoolingPeriod)
	assert.Equal(t, WithdrawalApproved, f.withdrawal(w.ID).Status)
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM delegated_signing_decisions WHERE withdrawal_id = $1`, w.ID))
	f.advance(time.Hour)
	res, err := f.svc.ExecuteWithdrawal(f.ctx, w.ID)
	require.NoError(t, err)
	assert.Equal(t, WithdrawalSettled, res.Withdrawal.Status)
}

// TestIntegration_AWithdrawalWaitsOnlyUntilItsDeadline: an undecided
// proposal is rejected by the sweep once its approval window closes, and its
// funds return.
func TestIntegration_AWithdrawalWaitsOnlyUntilItsDeadline(t *testing.T) {
	f := newFixture(t)
	s := f.withdrawalSetup(nil, func(r *GrantRequest) { r.ExpiresAt = f.clk.Now().Add(72 * time.Hour) })
	w := f.mustPropose(s, 1, 60_000_000)
	require.Equal(t, WithdrawalAwaitingApproval, w.Status)
	f.advance(25 * time.Hour)
	_, err := f.decideWithdrawal(f.owner(s.c), WithdrawalDecision{WithdrawalID: w.ID, PlanHash: w.PlanHash, Approve: true, Reason: "late"})
	requireReason(t, err, ReasonApprovalExpired)
	_, err = f.svc.ReconcilePending(f.ctx, 100, true)
	require.NoError(t, err)
	got := f.withdrawal(w.ID)
	assert.Equal(t, WithdrawalRejected, got.Status)
	assert.Equal(t, ReasonApprovalExpired, got.FailureCode)
	assert.Equal(t, capital.ReservationReleased, f.reservationOf(got.ReservationID).Status)
}
