//go:build integration

package bridge

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/security"
)

// TestIntegration_ABridgeMovesValueOnceAndReconciles: a leg moves units from
// the source chain to the destination exactly once -- in the books, on both
// chains and in the facility's conservation identity -- through every state,
// with each posting made only after the transaction it records is final.
func TestIntegration_ABridgeMovesValueOnceAndReconciles(t *testing.T) {
	f := newFixture(t)
	r := f.route
	c := f.customer()
	f.fund(r, c, 1_000_000)

	leg := f.mustBridge(r, c, 400_000)

	assert.Equal(t, "600000", f.wallet(c, r.SourceAssetID))
	assert.Equal(t, "0", f.inFlight(r, c))
	assert.Equal(t, "400000", f.wallet(c, r.DestinationAssetID))
	assert.Equal(t, "400000", f.facility(r, ledger.CodeBridgeLocked, r.SourceAssetID))
	assert.Equal(t, "400000", f.facility(r, ledger.CodeBridgeOutstanding, r.DestinationAssetID))
	assert.Equal(t, uint64(600_000), f.chainBalance(r.SourceChain, c.holder, r.SourceMint))
	assert.Equal(t, uint64(400_000), f.escrow(r))
	assert.Equal(t, uint64(400_000), f.supply(r.DestinationChain, r.DestinationMint))
	assert.Equal(t, uint64(400_000), f.chainBalance(r.DestinationChain, c.holder, r.DestinationMint))

	assert.Equal(t, 1, f.journals(leg, ledger.KindBridgeSourceFinalized))
	assert.Equal(t, 1, f.journals(leg, ledger.KindBridgeDestinationFinalized))
	assert.Equal(t, 0, f.journals(leg, ledger.KindBridgeRefund))
	assert.Equal(t, 1, f.submissions(leg.LockTxID), "the lock was submitted once")
	atts := f.attempts(leg.ID, AttemptRelease)
	require.Len(t, atts, 1)
	assert.Equal(t, 1, f.submissions(atts[0].TxID), "the release was submitted once")
	assert.Equal(t, econ.EffectApplied, f.effect(leg.SourceEffectID).Status)
	assert.Equal(t, econ.EffectApplied, f.effect(leg.DestinationEffectID).Status)
	assert.Equal(t, econ.StatusCompleted, f.intent(leg).Status)
	assert.Equal(t, capital.ReservationConsumed, f.reservation(leg).Status)

	var path []string
	rows, err := testDB.Query(f.ctx, `SELECT to_status FROM bridge_leg_transitions WHERE leg_id = $1 ORDER BY occurred_at, id`, leg.ID)
	require.NoError(t, err)
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		path = append(path, s)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"AVAILABLE", "RESERVED", "BRIDGE_REQUESTED", "SOURCE_FINALIZED", "MESSAGE_IN_FLIGHT",
		"DESTINATION_FINALIZED", "RECONCILED"}, path)
	assert.Equal(t, 2, f.count(`SELECT count(*) FROM provider_evidence WHERE key LIKE $1`, "advanced/bridge/%/"+leg.ID.String()+".json"))
	assert.NotEmpty(t, f.effect(leg.SourceEffectID).EvidenceRef)
	assert.NotEmpty(t, f.effect(leg.DestinationEffectID).EvidenceRef)

	rec := f.reconcile(r)
	assert.True(t, rec.Matched(), "%+v", rec)
	assert.NotEmpty(t, rec.RecordID, "a periodic match is recorded")
	assert.Equal(t, RouteActive, f.routeNow(r).Status)
}

// TestIntegration_ADuplicateRequestIsIdempotent: the same request twice is one
// leg, one lock, one posting of each kind; a different request with the same
// key is refused.
func TestIntegration_ADuplicateRequestIsIdempotent(t *testing.T) {
	f := newFixture(t)
	r := f.route
	c := f.customer()
	f.fund(r, c, 1_000_000)
	req := f.request(r, c, 250_000, "dup-"+uuid.NewString(), gates.ModeSandbox)

	first, err := f.svc.Bridge(c.ctx, req)
	require.NoError(t, err)
	require.Equal(t, StateReconciled, first.Leg.Status)
	again, err := f.svc.Bridge(c.ctx, req)
	require.NoError(t, err)
	assert.True(t, again.Replay)
	assert.Equal(t, first.Leg.ID, again.Leg.ID)
	assert.Equal(t, StateReconciled, again.Leg.Status)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM bridge_legs WHERE account_id = $1`, c.id))
	assert.Equal(t, 1, f.submissions(first.Leg.LockTxID))
	assert.Equal(t, 1, f.journals(first.Leg, ledger.KindBridgeSourceFinalized))
	assert.Equal(t, 1, f.journals(first.Leg, ledger.KindBridgeDestinationFinalized))
	assert.Equal(t, "750000", f.wallet(c, r.SourceAssetID))

	changed := req
	changed.Quantity = devnet.QuantityOf(1)
	_, err = f.svc.Bridge(c.ctx, changed)
	require.Error(t, err)
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, errs.CodeOf(err))
}

// TestIntegration_ADuplicateReleaseMintsNothingTwice: once a lock is released,
// relaying it again -- the same bytes, or a fresh release of the same lock --
// mints nothing, and settling the leg again posts nothing.
func TestIntegration_ADuplicateReleaseMintsNothingTwice(t *testing.T) {
	f := newFixture(t)
	r := f.route
	c := f.customer()
	f.fund(r, c, 1_000_000)
	leg := f.mustBridge(r, c, 300_000)
	atts := f.attempts(leg.ID, AttemptRelease)
	require.Len(t, atts, 1)

	// The same bytes again: the chain already holds that transaction.
	_, err := f.host.Submit(f.ctx, r.DestinationChain, atts[0].Raw)
	if err == nil {
		f.produce(r.DestinationChain, 1+itDepth)
	}
	// A fresh release of the same lock, properly signed by the attester.
	dup := f.outOfBandReleaseAllowFailure(r, leg.LockTxID)
	st := f.status(r.DestinationChain, dup)
	assert.Equal(t, devnet.TxFailed, st.State)
	assert.Equal(t, devnet.CodeBridgeAlreadyReleased, st.Err)
	assert.Equal(t, uint64(300_000), f.supply(r.DestinationChain, r.DestinationMint), "nothing was minted twice")

	// Settling the leg again changes nothing.
	for i := 0; i < 2; i++ {
		res, err := f.svc.Advance(f.ctx, leg.ID)
		require.NoError(t, err)
		assert.Equal(t, StateReconciled, res.Leg.Status)
		require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := f.svc.SettleDestination(ctx, tx, leg.ID); err != nil {
				return err
			}
			_, err := f.svc.SettleSource(ctx, tx, leg.ID)
			return err
		}))
	}
	assert.Equal(t, 1, f.journals(leg, ledger.KindBridgeDestinationFinalized))
	assert.Equal(t, 1, f.journals(leg, ledger.KindBridgeSourceFinalized))
	assert.Equal(t, "300000", f.wallet(c, r.DestinationAssetID))
	assert.Len(t, f.attempts(leg.ID, AttemptRelease), 1, "a released leg is never relayed again")

	// While a release can still land, no second one is relayed.
	other := f.customer()
	f.fund(r, other, 100_000)
	res, err := f.bridge(r, other, 100_000, gates.ModeDevnet)
	require.NoError(t, err)
	f.produce(r.SourceChain, 1+itDepth)
	first := f.observe(res.Leg.ID)
	require.Equal(t, StateMessageInFlight, first.Status)
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		a, err := f.svc.PrepareRelease(ctx, tx, first.ID)
		assert.Nil(t, a, "the first release is pending: never race it")
		return err
	}))
	assert.Len(t, f.attempts(first.ID, AttemptRelease), 1)
}

// outOfBandReleaseAllowFailure relays a lock again with a fresh release and
// returns its id, whatever its outcome.
func (f *fixture) outOfBandReleaseAllowFailure(r Route, lockTx devnet.TxID) devnet.TxID {
	f.t.Helper()
	lt, err := f.host.LockedTransfer(f.ctx, r.SourceChain, lockTx)
	require.NoError(f.t, err)
	tip, err := f.host.Tip(f.ctx, r.DestinationChain)
	require.NoError(f.t, err)
	msg, err := devnet.BuildRelease(lt, devnet.ReleaseParams{DestMint: r.DestinationMint, Authority: f.attester.PublicKey(),
		FeePayer: f.attester.PublicKey(), RecentBlockhash: tip.Hash})
	require.NoError(f.t, err)
	exp := ReleaseExpectation{Chain: r.DestinationChain, Mint: r.DestinationMint, ToOwner: lt.Lock.DestOwner, Amount: lt.Lock.Amount,
		SourceChain: r.SourceChain, SourceTxID: lockTx, Nonce: lt.Lock.Nonce}
	_, raw, err := f.attester.SignRelease(exp, msg)
	require.NoError(f.t, err)
	require.NoError(f.t, f.host.EnsureNativeBalance(f.ctx, r.DestinationChain, f.attester.PublicKey(), 1_000_000, 100_000_000))
	id, err := f.host.Submit(f.ctx, r.DestinationChain, raw)
	require.NoError(f.t, err)
	f.produce(r.DestinationChain, 1+itDepth)
	return id
}

// TestIntegration_AFailedLockRestoresTheSourceState: a lock that never lands
// fails the leg once it can never land, releases the reservation, posts
// nothing and is never resubmitted; the account is exactly as before.
func TestIntegration_AFailedLockRestoresTheSourceState(t *testing.T) {
	f := newFixture(t)
	r := f.route
	c := f.customer()
	f.fund(r, c, 1_000_000)
	before := f.available(c, r.SourceAssetID)
	require.NoError(t, f.host.DropNext(f.ctx, r.SourceChain, 1))

	res, err := f.bridge(r, c, 400_000, gates.ModeSandbox)
	require.NoError(t, err)
	leg := res.Leg
	assert.Equal(t, StateFailed, leg.Status)
	assert.Equal(t, "SUBMISSION_EXPIRED", leg.FailureCode)
	assert.Equal(t, 1, f.submissions(leg.LockTxID), "an expired lock is never resubmitted")
	assert.Equal(t, 0, f.journals(leg, ledger.KindBridgeSourceFinalized))
	assert.Equal(t, capital.ReservationReleased, f.reservation(leg).Status)
	assert.Equal(t, econ.EffectFailed, f.effect(leg.SourceEffectID).Status)
	assert.Equal(t, econ.StatusFailed, f.intent(leg).Status)
	assert.Equal(t, before, f.available(c, r.SourceAssetID))
	assert.Equal(t, "1000000", f.wallet(c, r.SourceAssetID))
	assert.Equal(t, uint64(1_000_000), f.chainBalance(r.SourceChain, c.holder, r.SourceMint))
	assert.Equal(t, uint64(0), f.escrow(r))
	assert.True(t, f.reconcile(r).Matched())

	// A lock the chain refuses outright fails at once, the same way.
	tooMuch, err := f.bridge(r, c, 1_000_001, gates.ModeSandbox)
	require.Error(t, err, "the ledger refuses to reserve what the account does not have")
	assert.Nil(t, tooMuch.Leg.ReservationID)
}

// TestIntegration_AFailedReleaseIsRefundedAndRestoresTheSourceState: when the
// destination never releases, the leg goes to manual review with its value in
// flight (counted nowhere as spendable); an operator's refund returns it to
// the source wallet -- in the books and on the chain -- and the books still
// reconcile, with the escrowed originals retired.
func TestIntegration_AFailedReleaseIsRefundedAndRestoresTheSourceState(t *testing.T) {
	f := newFixture(t)
	r := f.route
	c := f.customer()
	f.fund(r, c, 1_000_000)
	require.NoError(t, f.host.SetMintFrozen(f.ctx, r.DestinationChain, r.DestinationMint, true))

	res, err := f.bridge(r, c, 400_000, gates.ModeSandbox)
	require.NoError(t, err)
	leg := res.Leg
	require.Equal(t, StateManualReview, leg.Status)
	atts := f.attempts(leg.ID, AttemptRelease)
	require.Len(t, atts, itAttempts, "release attempts are bounded")
	for _, a := range atts {
		st := f.status(r.DestinationChain, a.TxID)
		assert.Equal(t, devnet.CodeMintFrozen, st.Err)
		assert.Equal(t, 1, f.submissions(a.TxID))
	}
	assert.Equal(t, "600000", f.wallet(c, r.SourceAssetID))
	assert.Equal(t, "400000", f.inFlight(r, c))
	assert.Equal(t, "0", f.wallet(c, r.DestinationAssetID))
	bp, err := f.svc.BuyingPower(f.ctx, testDB, c.id, HaircutPolicy{Version: "generous", InFlightBPS: 10_000, ObservedBPS: 10_000})
	require.NoError(t, err)
	require.Len(t, bp.Holdings, 1)
	assert.Equal(t, "0.00%", bp.Holdings[0].FactorBPS.String(), "a leg in review counts nothing, whatever the policy")

	// Only an operator refunds; an agent never moves capital across chains.
	_, err = f.svc.Refund(f.ctx, leg.ID, c.actor, "please")
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	_, err = f.svc.Refund(f.ctx, leg.ID, econ.Actor{Type: security.ActorAgent, ID: "agent-1"}, "please")
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	out, err := f.svc.Refund(f.ctx, leg.ID, operator, "the destination mint is frozen")
	require.NoError(t, err)
	leg = out.Leg
	assert.Equal(t, StateFailed, leg.Status)
	assert.Equal(t, "REFUNDED", leg.FailureCode)
	assert.Equal(t, "1000000", f.wallet(c, r.SourceAssetID))
	assert.Equal(t, "0", f.inFlight(r, c))
	assert.Equal(t, "0", f.facility(r, ledger.CodeBridgeLocked, r.SourceAssetID))
	assert.Equal(t, uint64(1_000_000), f.chainBalance(r.SourceChain, c.holder, r.SourceMint))
	assert.Equal(t, uint64(400_000), f.escrow(r), "the escrowed originals stay retired")
	assert.Equal(t, uint64(0), f.supply(r.DestinationChain, r.DestinationMint))
	assert.Equal(t, 1, f.journals(leg, ledger.KindBridgeRefund))
	assert.Equal(t, econ.EffectCompensated, f.effect(leg.SourceEffectID).Status)
	assert.Equal(t, econ.EffectFailed, f.effect(leg.DestinationEffectID).Status)
	assert.Equal(t, econ.EffectApplied, f.effect(leg.RefundEffectID).Status)
	assert.Equal(t, econ.StatusFailed, f.intent(leg).Status)
	rec := f.reconcile(r)
	assert.True(t, rec.Matched(), "%+v", rec)
	assert.Equal(t, "400000", rec.Refunded.String())

	// The same decision again changes nothing; a refunded leg never relays.
	again, err := f.svc.Refund(f.ctx, leg.ID, operator, "again")
	require.NoError(t, err)
	assert.Equal(t, StateFailed, again.Leg.Status)
	assert.Equal(t, 1, f.journals(leg, ledger.KindBridgeRefund))
	require.NoError(t, f.host.SetMintFrozen(f.ctx, r.DestinationChain, r.DestinationMint, false))
	err = testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.ResumeRelay(ctx, tx, leg.ID, operator, "retry")
		return err
	})
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))
}

// TestIntegration_AResumedRelayCompletesAfterTheDestinationRecovers: a leg in
// manual review because its destination was frozen completes, once, after an
// operator resumes it.
func TestIntegration_AResumedRelayCompletesAfterTheDestinationRecovers(t *testing.T) {
	f := newFixture(t)
	r := f.route
	c := f.customer()
	f.fund(r, c, 1_000_000)
	require.NoError(t, f.host.SetMintFrozen(f.ctx, r.DestinationChain, r.DestinationMint, true))
	res, err := f.bridge(r, c, 400_000, gates.ModeSandbox)
	require.NoError(t, err)
	require.Equal(t, StateManualReview, res.Leg.Status)

	require.NoError(t, f.host.SetMintFrozen(f.ctx, r.DestinationChain, r.DestinationMint, false))
	err = testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.ResumeRelay(ctx, tx, res.Leg.ID, c.actor, "mine")
		return err
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "only an operator resumes")
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.ResumeRelay(ctx, tx, res.Leg.ID, operator, "destination unfrozen")
		return err
	}))
	done, err := f.svc.Advance(f.ctx, res.Leg.ID)
	require.NoError(t, err)
	assert.Equal(t, StateReconciled, done.Leg.Status)
	assert.Len(t, f.attempts(res.Leg.ID, AttemptRelease), itAttempts+1)
	assert.Equal(t, "400000", f.wallet(c, r.DestinationAssetID))
	assert.Equal(t, uint64(400_000), f.supply(r.DestinationChain, r.DestinationMint))
	assert.Equal(t, econ.StatusCompleted, f.intent(done.Leg).Status)
	assert.True(t, f.reconcile(r).Matched())
}

// TestIntegration_AnUnknownLockSubmissionIsReconciledNotRetried: a lock whose
// submission timed out is never resubmitted: if it landed, the leg completes
// (the intent RECONCILED); if it did not, the leg fails once it can never
// land.
func TestIntegration_AnUnknownLockSubmissionIsReconciledNotRetried(t *testing.T) {
	f := newFixture(t)
	r := f.route
	c := f.customer()
	f.fund(r, c, 1_000_000)

	require.NoError(t, f.host.TimeoutButLand(f.ctx, r.SourceChain))
	res, err := f.bridge(r, c, 100_000, gates.ModeSandbox)
	require.Error(t, err)
	assert.Equal(t, errs.CodeSubmissionStateUnknown, errs.CodeOf(err))
	landed := res.Leg
	assert.Equal(t, StateBridgeRequested, landed.Status)
	assert.Equal(t, econ.EffectUnknown, f.effect(landed.SourceEffectID).Status)
	assert.Equal(t, econ.StatusUnknown, f.intent(landed).Status)
	done, err := f.svc.Advance(f.ctx, landed.ID)
	require.NoError(t, err)
	assert.Equal(t, StateReconciled, done.Leg.Status)
	assert.Equal(t, econ.StatusReconciled, f.intent(landed).Status)
	assert.Equal(t, econ.EffectApplied, f.effect(landed.SourceEffectID).Status)
	assert.Equal(t, 1, f.submissions(landed.LockTxID))

	require.NoError(t, f.host.TimeoutAndDrop(f.ctx, r.SourceChain))
	res, err = f.bridge(r, c, 100_000, gates.ModeSandbox)
	require.Error(t, err)
	dropped := res.Leg
	observed, err := f.svc.ObservePending(f.ctx, 10)
	require.NoError(t, err)
	assert.Positive(t, observed.Examined)
	assert.Empty(t, observed.Failures)
	assert.Equal(t, StateBridgeRequested, f.leg(dropped.ID).Status, "observation alone never fails a lock that may still land")
	failed, err := f.svc.Advance(f.ctx, dropped.ID)
	require.NoError(t, err)
	assert.Equal(t, StateFailed, failed.Leg.Status)
	assert.Equal(t, econ.StatusFailed, f.intent(dropped).Status)
	assert.Equal(t, econ.EffectFailed, f.effect(dropped.SourceEffectID).Status)
	assert.Equal(t, 1, f.submissions(dropped.LockTxID), "the timed-out lock was never resubmitted")
	assert.Equal(t, "900000", f.wallet(c, r.SourceAssetID))
	assert.Equal(t, "100000", f.wallet(c, r.DestinationAssetID))
	assert.True(t, f.reconcile(r).Matched())
}

// TestIntegration_RefusalsAreRecordedAndMoveNothing: an agent, an account
// that does not own the wallet, a paused route and a chain balance that does
// not cover the lock are refused before anything is reserved or signed.
func TestIntegration_RefusalsAreRecordedAndMoveNothing(t *testing.T) {
	f := newFixture(t)
	r := f.route
	c := f.customer()
	f.fund(r, c, 1_000_000)

	req := f.request(r, c, 1_000, "agent-"+uuid.NewString(), gates.ModeSandbox)
	req.Actor = econ.Actor{Type: security.ActorAgent, ID: "agent-1"}
	_, err := f.svc.Bridge(f.ctx, req)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	stranger := f.customer()
	_, err = f.svc.Bridge(stranger.ctx, f.request(r, c, 1_000, "theft-"+uuid.NewString(), gates.ModeSandbox))
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	// The ledger says 1,000,000 but the chain wallet holds less (units moved
	// out of band): the lock is refused before anything is reserved.
	poor := f.customer()
	f.fund(r, poor, 1_000)
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxFunding, IdempotencyKey: "sbx-fund:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(poor.id, ledger.CodeWallet, r.SourceAssetID), Side: ledger.Debit, Quantity: devnet.QuantityOf(9_000)},
				{Account: ledger.CustomerAccount(poor.id, ledger.CodeCapital, r.SourceAssetID), Side: ledger.Credit, Quantity: devnet.QuantityOf(9_000)},
			},
		})
		return err
	}))
	_, err = f.bridge(r, poor, 5_000, gates.ModeSandbox)
	require.Error(t, err)
	assert.Equal(t, errs.CodeInsufficientBuyingPower, errs.CodeOf(err))
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM bridge_legs WHERE account_id = $1`, poor.id))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM economic_intents WHERE account_id = $1 AND status = 'REJECTED'
		AND rejection_code = $2`, poor.id, RefusalChainBalance))

	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.TransitionRoute(ctx, tx, r.ID, RoutePaused, operator, "maintenance")
		return err
	}))
	_, err = f.bridge(r, c, 1_000, gates.ModeSandbox)
	assert.Equal(t, errs.CodeAssetRestricted, errs.CodeOf(err))
	assert.Equal(t, "1000000", f.wallet(c, r.SourceAssetID))
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM bridge_legs WHERE account_id = $1`, c.id))
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM asset_reservations WHERE account_id = $1`, c.id.String()))
}
