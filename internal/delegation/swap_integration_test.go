//go:build integration

package delegation

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
)

// The whole path, once: Agent -> Intent -> Risk V2 -> Reservation -> Plan ->
// Inspector -> Signing Policy -> Provider -> devnet -> finality ->
// reconciliation -> SIM_TRADE -> effect -> evidence.
func TestIntegration_ADelegatedSwapSettlesThroughEveryStep(t *testing.T) {
	f := newFixture(t)
	c := f.customer(10_000_000) // $10
	capability := f.grant(c, nil)
	sw := f.mustSwap(c, capability.ID, 1, 2_000_000) // pay $2 for 0.02 token less spread
	require.Equal(t, SwapAdmitted, sw.Status)
	assert.Equal(t, "19900000", sw.AmountOut.String())
	assert.Equal(t, "19800000", sw.MinAmountOut.String())
	assert.Equal(t, int64(200), sw.NotionalUSD.Minor())
	assert.Equal(t, c.wallet, sw.Wallet)
	in := f.intentOf(sw.IntentID)
	assert.Equal(t, econ.StatusReserved, in.Status)
	assert.Equal(t, econ.KindAcquireExposure, in.Kind)
	assert.Equal(t, capability.ID.String(), in.DelegationID)
	assert.Equal(t, c.agentID, in.AgentID)
	assert.Equal(t, capital.ReservationActive, f.reservationOf(sw.ReservationID).Status)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM advanced_risk_decisions WHERE intent_id = $1 AND verdict = 'ALLOW'`, sw.IntentID))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM delegated_capability_usage WHERE subject_id = $1 AND notional_minor = 200`, sw.ID))

	res, err := f.svc.ExecuteSwap(f.ctx, sw.ID)
	require.NoError(t, err)
	require.True(t, res.Settled)
	sw = res.Swap
	require.Equal(t, SwapSettled, sw.Status, "failure %q", sw.FailureCode)

	// The books: the customer paid exactly $2 and received exactly the quote;
	// the house the opposite; the reservation is consumed.
	assert.Equal(t, "8000000", f.balance(c.account, ledger.CodeWallet, f.usd).String())
	assert.Equal(t, "19900000", f.balance(c.account, ledger.CodeWallet, f.tok).String())
	assert.Equal(t, "2000000", f.balance(c.account, ledger.CodeTradingOutflow, f.usd).String())
	assert.Equal(t, capital.ReservationConsumed, f.reservationOf(sw.ReservationID).Status)
	// The chain says the same thing.
	assert.Equal(t, uint64(8_000_000), f.chainBalance(c.wallet, f.usd))
	assert.Equal(t, uint64(19_900_000), f.chainBalance(c.wallet, f.tok))
	assert.Equal(t, uint64(1_000_000_000+2_000_000), f.chainBalance(f.venueKey.PublicKey(), f.usd))
	// One decision, approved, over the message it signed; the effect applied
	// with its journal and evidence; the intent completed.
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM delegated_signing_decisions WHERE swap_id = $1 AND verdict = 'APPROVED'`, sw.ID))
	e, err := f.effects.Get(f.ctx, testDB, *sw.EffectID)
	require.NoError(t, err)
	assert.Equal(t, econ.EffectApplied, e.Status)
	assert.Equal(t, *sw.JournalTransactionID, *e.JournalTransactionID)
	assert.NotEmpty(t, e.EvidenceRef)
	assert.Equal(t, econ.StatusCompleted, f.intentOf(sw.IntentID).Status)
	j, err := f.ledger.Transaction(f.ctx, testDB, *sw.JournalTransactionID)
	require.NoError(t, err)
	assert.Equal(t, ledger.KindSimTrade, j.Kind)
	st, err := f.host.Status(f.ctx, devnethost.ChainSolana, sw.TxID)
	require.NoError(t, err)
	assert.Equal(t, devnet.TxFinalized, st.State)
	// Every step left a transition row.
	assert.Equal(t, 4, f.count(`SELECT count(*) FROM delegated_swap_transitions WHERE swap_id = $1`, sw.ID),
		"NONE->ADMITTED, ADMITTED->SIGNED, SIGNED->SUBMITTED, SUBMITTED->SETTLED")
}

// TestIntegration_ExpiredTokenRejected: an expired capability admits
// nothing, and a swap admitted before the expiry is not signed after it.
func TestIntegration_ExpiredTokenRejected(t *testing.T) {
	f := newFixture(t)
	c := f.customer(10_000_000)
	short := f.grant(c, func(r *GrantRequest) { r.ExpiresAt = f.clk.Now().Add(time.Hour) })

	admitted := f.mustSwap(c, short.ID, 1, 1_000_000)
	f.advance(2 * time.Hour)

	// A new request under the expired capability: recorded and refused by
	// Risk V2's authority section.
	_, err := f.requestSwap(f.agent(c), f.swapRequest(c, short.ID, 2, 1_000_000))
	requireCode(t, err, errs.CodeRiskRejected)
	requireReason(t, err, risk.ReasonAuthorityExpired)
	assert.Contains(t, err.Error(), ReasonCapabilityExpired)

	// The swap admitted while it was valid is not signed after it expired.
	_, err = f.svc.ExecuteSwap(f.ctx, admitted.ID)
	requireCode(t, err, errs.CodeSigningRejected)
	got := f.swap(admitted.ID)
	assert.Equal(t, SwapRejected, got.Status)
	assert.Equal(t, ReasonCapabilityExpired, got.FailureCode)
	assert.Equal(t, capital.ReservationReleased, f.reservationOf(got.ReservationID).Status)
	assert.Equal(t, econ.StatusRejected, f.intentOf(got.IntentID).Status)
	assert.Empty(t, got.TxID, "nothing was signed")
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM delegated_signing_decisions WHERE swap_id = $1`, got.ID))
}

// TestIntegration_RevokedTokenRejected: a revoked capability admits
// nothing; a swap admitted before the revocation is not signed; a swap signed
// before it is not submitted.
func TestIntegration_RevokedTokenRejected(t *testing.T) {
	f := newFixture(t)
	c := f.customer(10_000_000)
	capability := f.grant(c, nil)
	admitted := f.mustSwap(c, capability.ID, 1, 1_000_000)
	signed := f.mustSwap(c, capability.ID, 2, 1_000_000)
	require.NoError(t, f.host.EnsureNativeBalance(f.ctx, devnethost.ChainSolana, c.wallet, 1_000_000, 100_000_000))
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		out, err := f.svc.SignSwap(ctx, tx, signed.ID)
		require.True(t, out.Fresh)
		return err
	}))

	revoked, err := f.revoke(f.owner(c), capability.ID)
	require.NoError(t, err)
	require.Equal(t, CapabilityRevoked, revoked.Status)

	_, err = f.requestSwap(f.agent(c), f.swapRequest(c, capability.ID, 3, 1_000_000))
	requireCode(t, err, errs.CodeRiskRejected)
	requireReason(t, err, risk.ReasonAuthorityRevoked)

	_, err = f.svc.ExecuteSwap(f.ctx, admitted.ID)
	requireCode(t, err, errs.CodeSigningRejected)
	assert.Equal(t, SwapRejected, f.swap(admitted.ID).Status)
	assert.Equal(t, ReasonCapabilityRevoked, f.swap(admitted.ID).FailureCode)

	// Signed before the revocation: stopped at the gate before submission.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.GateSwapSubmission(ctx, tx, signed.ID)
		if reasonOf(err) == ReasonBlockedBeforeSubmission {
			return nil
		}
		return err
	})
	require.NoError(t, err)
	got := f.swap(signed.ID)
	assert.Equal(t, SwapFailed, got.Status)
	assert.Equal(t, ReasonBlockedBeforeSubmission+":"+ReasonCapabilityRevoked, got.FailureCode)
	assert.Equal(t, capital.ReservationReleased, f.reservationOf(got.ReservationID).Status)
	st, err := f.host.Status(f.ctx, devnethost.ChainSolana, got.TxID)
	require.NoError(t, err)
	assert.Equal(t, devnet.TxNotFound, st.State, "never submitted")
	e, err := f.effects.Get(f.ctx, testDB, *got.EffectID)
	require.NoError(t, err)
	assert.Equal(t, econ.EffectFailed, e.Status)

	// Revoking twice changes nothing; an agent never revokes.
	again, err := f.revoke(f.owner(c), capability.ID)
	require.NoError(t, err)
	assert.Equal(t, revoked.RevokedAt, again.RevokedAt)
	_, err = f.revoke(f.agent(c), capability.ID)
	requireCode(t, err, errs.CodeForbidden)
}

// TestIntegration_AmountOverCapRejected: the single-trade, daily and
// lifetime limits each refuse, through Risk V2's authority section.
func TestIntegration_AmountOverCapRejected(t *testing.T) {
	f := newFixture(t)
	c := f.customer(100_000_000) // $100
	capability := f.grant(c, func(r *GrantRequest) {
		r.MaxSingleTrade, r.MaxDailyNotional, r.MaxNotional = money.USDFromMinor(500), money.USDFromMinor(800), money.USDFromMinor(1_000)
		r.ExpiresAt = f.clk.Now().Add(72 * time.Hour)
	})
	_, err := f.requestSwap(f.agent(c), f.swapRequest(c, capability.ID, 1, 5_000_001)) // $5.000001 -> 501 cents
	requireCode(t, err, errs.CodeRiskRejected)
	requireReason(t, err, risk.ReasonAuthorityLimit)
	assert.Contains(t, err.Error(), ReasonSingleLimitExceeded)

	f.mustSwap(c, capability.ID, 2, 5_000_000) // $5.00
	_, err = f.requestSwap(f.agent(c), f.swapRequest(c, capability.ID, 3, 3_000_001))
	requireCode(t, err, errs.CodeRiskRejected)
	assert.Contains(t, err.Error(), ReasonDailyLimitExceeded)

	f.mustSwap(c, capability.ID, 4, 3_000_000) // $8.00 today, the daily limit
	f.advance(24 * time.Hour)
	f.mustSwap(c, capability.ID, 5, 2_000_000) // a new day: $10.00 in all, the lifetime limit
	_, err = f.requestSwap(f.agent(c), f.swapRequest(c, capability.ID, 6, 10_000))
	requireCode(t, err, errs.CodeRiskRejected)
	assert.Contains(t, err.Error(), ReasonLifetimeLimitExceeded)
	assert.Equal(t, 1_000, f.count(`SELECT coalesce(sum(notional_minor), 0)::int FROM delegated_capability_usage WHERE capability_id = $1`, capability.ID))
}

// TestIntegration_DestinationMismatchRejected: a venue outside the
// capability's destination allowlist is refused, and so is a transaction
// whose payment goes anywhere but the planned venue.
func TestIntegration_DestinationMismatchRejected(t *testing.T) {
	f := newFixture(t)
	c := f.customer(10_000_000)
	elsewhere := f.grant(c, func(r *GrantRequest) { r.DestinationAllowlist = []devnet.PublicKey{ukey("somewhere-else")} })
	_, err := f.requestSwap(f.agent(c), f.swapRequest(c, elsewhere.ID, 1, 1_000_000))
	requireCode(t, err, errs.CodeRiskRejected)
	requireReason(t, err, risk.ReasonAuthorityScope)
	assert.Contains(t, err.Error(), ReasonDestinationNotGranted)

	// A transaction paying another address than the plan's venue is refused
	// by the provider, which records its one decision as REJECTED.
	capability := f.grant(c, nil)
	sw := f.mustSwap(c, capability.ID, 1, 1_000_000)
	decision := f.offerModified(sw, func(m *devnet.Message) {
		p := m.Instructions[0].(devnet.TokenTransfer)
		p.ToOwner = ukey("thief")
		m.Instructions[0] = p
	})
	assert.Equal(t, VerdictRejected, decision.Verdict)
	assert.Contains(t, decision.ReasonCodes, InspectDestinationMismatch)
	assert.Contains(t, decision.ReasonCodes, ReasonMessageNotPersisted)
}

// offerModified compiles a swap as SignSwap does, then offers the provider a
// modified message instead of the compiled one, and commits the decision.
func (f *fixture) offerModified(sw Swap, mutate func(*devnet.Message)) SigningDecision {
	f.t.Helper()
	var d SigningDecision
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		locked, err := getSwap(ctx, tx, sw.ID, true)
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
		if _, err := f.svc.setSwapColumns(ctx, tx, locked, subjectSet{message: compiled, recentBlockhash: &tip.Hash, blockhashHeight: &height}); err != nil {
			return err
		}
		modified := msg
		modified.Instructions = append([]devnet.Instruction(nil), msg.Instructions...)
		mutate(&modified)
		b, err := modified.Bytes()
		if err != nil {
			return err
		}
		d, err = f.signer.Sign(ctx, tx, SignRequest{Kind: SubjectSwap, SubjectID: sw.ID.String(), Message: b, MessageSHA256: sha256Of(b)})
		return err
	}))
	return d
}

// TestIntegration_ModifiedTransactionRejected: the provider refuses any
// message that is not the persisted compiled one, records that refusal as the
// swap's one decision, and never signs that swap afterwards.
func TestIntegration_ModifiedTransactionRejected(t *testing.T) {
	f := newFixture(t)
	c := f.customer(10_000_000)
	capability := f.grant(c, nil)
	sw := f.mustSwap(c, capability.ID, 1, 1_000_000)
	decision := f.offerModified(sw, func(m *devnet.Message) {
		p := m.Instructions[0].(devnet.TokenTransfer)
		p.Amount *= 2
		m.Instructions[0] = p
	})
	require.Equal(t, VerdictRejected, decision.Verdict)
	assert.Contains(t, decision.ReasonCodes, InspectAmountMismatch)
	assert.Equal(t, devnet.Signature{}, decision.Signature, "a refusal carries no signature")
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM delegated_signing_decisions WHERE swap_id = $1 AND verdict = 'REJECTED'`, sw.ID))

	// The boundary decided once: the swap can never be signed now, not even
	// with the honest bytes.
	_, err := f.svc.ExecuteSwap(f.ctx, sw.ID)
	requireCode(t, err, errs.CodeSigningRejected)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM delegated_signing_decisions WHERE swap_id = $1`, sw.ID))
	refusedSwap := f.swap(sw.ID)
	assert.Equal(t, SwapRejected, refusedSwap.Status)
	assert.Empty(t, refusedSwap.TxID)
	assert.Equal(t, capital.ReservationReleased, f.reservationOf(refusedSwap.ReservationID).Status)

	// A signed transaction altered after signing fails the final inspection.
	sw2 := f.mustSwap(c, capability.ID, 2, 1_000_000)
	res, err := f.svc.ExecuteSwap(f.ctx, sw2.ID)
	require.NoError(t, err)
	require.Equal(t, SwapSettled, res.Swap.Status)
	tampered := append([]byte(nil), res.Swap.SignedTx...)
	tampered[len(tampered)-1] ^= 0xFF
	exp := SwapExpectation{Plan: res.Swap.Plan, PlanHash: res.Swap.PlanHash, Blockhash: res.Swap.RecentBlockhash,
		Wallet: res.Swap.Wallet, AllowedChains: capability.Terms.ChainAllowlist, AllowedDestinations: capability.Terms.DestinationAllowlist,
		ChainFee: itFee, MaxFee: DefaultSigningPolicy().MaxFeePerTx}
	_, err = InspectSwapTransaction(exp, tampered)
	requireCode(t, err, errs.CodeSigningRejected)
	_, err = InspectSwapTransaction(exp, res.Swap.SignedTx)
	require.NoError(t, err)
}

// TestIntegration_ReplayRejected: a request nonce is used once for ever, a
// replay of the same request is the same swap, a reuse with other content is
// refused, and nothing is signed, posted or applied twice.
func TestIntegration_ReplayRejected(t *testing.T) {
	f := newFixture(t)
	c := f.customer(10_000_000)
	capability := f.grant(c, nil)
	req := f.swapRequest(c, capability.ID, 7, 1_000_000)
	first, err := f.requestSwap(f.agent(c), req)
	require.NoError(t, err)
	second, err := f.requestSwap(f.agent(c), req)
	require.NoError(t, err)
	assert.True(t, second.Replay)
	assert.Equal(t, first.Swap.ID, second.Swap.ID)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM delegated_capability_usage WHERE capability_id = $1`, capability.ID))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM asset_reservations WHERE economic_intent_id = $1`, first.Swap.IntentID))

	reuse := req
	reuse.AmountIn = money.QuantityFromInt64(1_000_001)
	_, err = f.requestSwap(f.agent(c), reuse)
	requireCode(t, err, errs.CodeInvalidIdempotencyReuse)

	res, err := f.svc.ExecuteSwap(f.ctx, first.Swap.ID)
	require.NoError(t, err)
	require.Equal(t, SwapSettled, res.Swap.Status)
	again, err := f.svc.ExecuteSwap(f.ctx, first.Swap.ID)
	require.NoError(t, err)
	assert.True(t, again.Replay)
	// The signed bytes replayed to the chain change nothing either.
	_, err = f.host.Submit(f.ctx, devnethost.ChainSolana, res.Swap.SignedTx)
	require.NoError(t, err, "the chain acknowledges a transaction it already holds")
	_, err = f.svc.ReconcilePending(f.ctx, 100, true)
	require.NoError(t, err)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM journal_transactions WHERE reference_type = 'delegated_swap' AND reference_id = $1`, first.Swap.ID.String()))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM economic_effects WHERE effect_key = $1`, "delegation:swap:"+first.Swap.ID.String()))
	assert.Equal(t, uint64(9_000_000), f.chainBalance(c.wallet, f.usd), "paid once")
	// A replay of a refused request is the same refusal.
	big := f.swapRequest(c, capability.ID, 8, 100_000_000)
	_, err = f.requestSwap(f.agent(c), big)
	require.Error(t, err)
	replayed, err := f.requestSwap(f.agent(c), big)
	require.Error(t, err)
	assert.True(t, replayed.Replay || replayed.Swap.ID.IsZero())
}

func sha256Of(b []byte) [32]byte { return sha256.Sum256(b) }
