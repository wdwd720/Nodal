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

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// --- Kill switches -----------------------------------------------------------------

type itestKillAudit struct{}

func (itestKillAudit) Append(context.Context, pgx.Tx, killswitch.AuditEvent) error { return nil }

type itestNoApprovals struct{}

func (itestNoApprovals) VerifyApproved(context.Context, db.Querier, string, string, string) (killswitch.Approval, error) {
	return killswitch.Approval{}, errs.New(errs.CodeForbidden, "no approvals in this test")
}

// activateKill turns a switch on through the real controller and turns it
// off again (as the migration owner, with its transition row) when the test
// ends, so a global switch never leaks into the next test.
func (f *fixture) activateKill(kind killswitch.Kind, scope string) {
	f.t.Helper()
	ctrl, err := killswitch.NewController(f.clk, itestKillAudit{}, itestNoApprovals{})
	require.NoError(f.t, err)
	var sw killswitch.Switch
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		sw, err = ctrl.Activate(f.operatorCtx(), tx, kind, scope, "delegation itest")
		return err
	}))
	f.t.Cleanup(func() {
		conn, err := pgx.Connect(context.Background(), testMigrateURL)
		if err != nil {
			f.t.Errorf("release %s: %v", kind, err)
			return
		}
		defer conn.Close(context.Background())
		if err := rawTx(context.Background(), conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(context.Background(), `INSERT INTO kill_switch_transitions (id, switch_id, kind, scope_id, to_active, actor_type, actor_id, reason)
				VALUES ($1, $2, $3, $4, false, 'OPERATOR', 'itest', 'test cleanup')`, uuid.New(), sw.ID, string(kind), sw.ScopeID); err != nil {
				return err
			}
			_, err := tx.Exec(context.Background(), `UPDATE kill_switches SET active = false WHERE id = $1`, sw.ID)
			return err
		}); err != nil {
			f.t.Errorf("release %s: %v", kind, err)
		}
	})
}

func (f *fixture) freezeAccount(c customer) {
	f.t.Helper()
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := accounts.NewRepository().Transition(ctx, tx, c.account, accounts.StatusChange{To: accounts.StatusFrozen,
			ActorType: "OPERATOR", ActorID: "itest", Reason: "freeze for the test"}, f.clk.Now())
		return err
	}))
}

// TestIntegration_StrategyCannotSelfExpandCapability: an agent (or the
// strategy driving it) can never create, widen, extend or re-target a
// capability -- not through the service, not through the application role,
// not even as the migration owner -- and cannot use one beyond its terms.
func TestIntegration_StrategyCannotSelfExpandCapability(t *testing.T) {
	f := newFixture(t)
	c := f.customer(10_000_000)
	capability := f.grant(c, func(r *GrantRequest) {
		r.ActionClasses = []ActionClass{ActionAcquireExposure}
		r.InstrumentClasses = []InstrumentClass{ClassStableValue, ClassSpotToken}
		r.MaxSingleTrade = money.USDFromMinor(1_000)
	})

	// 1. The service: an agent principal never grants or revokes.
	req := f.grantRequest(c)
	req.MaxSingleTrade = money.USDFromMinor(100_000_000)
	err := f.inTx(func(_ context.Context, tx pgx.Tx) error {
		_, _, err := f.svc.GrantCapability(f.agent(c), tx, req)
		return err
	})
	requireCode(t, err, errs.CodeForbidden)
	_, err = f.revoke(f.agent(c), capability.ID)
	requireCode(t, err, errs.CodeForbidden)

	// 2. The application role holds no UPDATE privilege on any term column.
	for _, sql := range []string{
		`UPDATE delegated_capabilities SET max_single_trade_minor = 99999999 WHERE id = $1`,
		`UPDATE delegated_capabilities SET expires_at = expires_at + interval '30 days' WHERE id = $1`,
		`UPDATE delegated_capabilities SET action_classes = ARRAY['ACQUIRE_EXPOSURE','PROPOSE_WITHDRAWAL'] WHERE id = $1`,
		`UPDATE delegated_capabilities SET destination_allowlist = destination_allowlist || ARRAY['` + ukey("thief").String() + `'] WHERE id = $1`,
		`UPDATE delegated_capabilities SET agent_id = agent_id WHERE id = $1`,
	} {
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, sql, capability.ID); return err })
		requireSQLState(t, err, "42501")
	}
	// 3. Even the migration owner, who holds every privilege, is stopped by
	// the trigger.
	owner := ownerConn(t)
	forgedHash := sha256Of([]byte(uuid.NewString()))
	_, err = owner.Exec(f.ctx, `UPDATE delegated_capabilities SET max_daily_notional_minor = max_notional_minor WHERE id = $1`, capability.ID)
	requireSQLState(t, err, "DG002")
	_, err = owner.Exec(f.ctx, `UPDATE delegated_capabilities SET expires_at = expires_at + interval '1 day' WHERE id = $1`, capability.ID)
	requireSQLState(t, err, "DG002")
	// 4. A grant forged in SQL in the agent's name is refused: a grant is the
	// account owner's.
	err = rawTx(f.ctx, owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(f.ctx, `INSERT INTO delegated_capabilities (id, account_id, agent_id, strategy_id, environment, mode,
			instrument_classes, action_classes, max_notional_minor, max_daily_notional_minor, max_single_trade_minor,
			destination_allowlist, chain_allowlist, granted_at, expires_at, nonce, requires_human_approval, content_hash,
			idempotency_key, status, granted_by_actor_type, granted_by_actor_id)
			SELECT $1, account_id, agent_id, strategy_id, environment, mode, instrument_classes, action_classes, 99999999, 99999999,
			       99999999, destination_allowlist, chain_allowlist, granted_at, expires_at, $2, false, $3, $4, 'ACTIVE', 'USER', $5
			  FROM delegated_capabilities WHERE id = $6`,
			uuid.New(), "ab"+capability.Nonce[2:], forgedHash[:], "forged-"+uuid.NewString(), c.agentID, capability.ID)
		return err
	})
	requireSQLState(t, err, "DG003")
	// 5. Use beyond the terms is refused: another action, another venue,
	// more than the single limit.
	reduce := f.swapRequest(c, capability.ID, 1, 1_000_000)
	reduce.Action, reduce.PayAssetID, reduce.ReceiveAssetID = ActionReduceExposure, f.tok.ID, f.usd.ID
	f.fundCustomer(c, f.tok, 1_000_000_000)
	_, err = f.requestSwap(f.agent(c), reduce)
	requireCode(t, err, errs.CodeRiskRejected)
	assert.Contains(t, err.Error(), ReasonActionNotGranted)
	_, err = f.requestSwap(f.agent(c), f.swapRequest(c, capability.ID, 2, 10_000_001))
	requireCode(t, err, errs.CodeRiskRejected)
	assert.Contains(t, err.Error(), ReasonSingleLimitExceeded)

	got, err := f.svc.GetCapability(f.ctx, testDB, capability.ID)
	require.NoError(t, err)
	assert.Equal(t, capability.ContentHash, got.ContentHash, "the capability is exactly what was granted")
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM delegated_capabilities WHERE account_id = $1`, c.account))
}

// TestIntegration_CapabilityCannotCrossAccountBoundary: a capability acts on
// its own account, for its own agent, signed by its own account's key, and
// nowhere else.
func TestIntegration_CapabilityCannotCrossAccountBoundary(t *testing.T) {
	f := newFixture(t)
	alice := f.customer(10_000_000)
	bob := f.customer(10_000_000)
	aliceCap := f.grant(alice, nil)
	bobCap := f.grant(bob, nil)

	// Alice's agent with Bob's capability, on Alice's account.
	_, err := f.requestSwap(f.agent(alice), f.swapRequest(alice, bobCap.ID, 1, 1_000_000))
	requireCode(t, err, errs.CodeForbidden)
	// Alice's agent on Bob's account, with either capability.
	_, err = f.requestSwap(f.agent(alice), f.swapRequest(bob, bobCap.ID, 1, 1_000_000))
	requireCode(t, err, errs.CodeForbidden)
	_, err = f.requestSwap(f.agent(alice), f.swapRequest(bob, aliceCap.ID, 1, 1_000_000))
	requireCode(t, err, errs.CodeForbidden)
	// Alice granting on Bob's account, or to Bob's agent on her own.
	err = f.inTx(func(_ context.Context, tx pgx.Tx) error {
		_, _, err := f.svc.GrantCapability(f.owner(alice), tx, f.grantRequest(bob))
		return err
	})
	requireCode(t, err, errs.CodeForbidden)
	toBobsAgent := f.grantRequest(alice)
	toBobsAgent.AgentID = bob.agentID
	err = f.inTx(func(_ context.Context, tx pgx.Tx) error {
		_, _, err := f.svc.GrantCapability(f.owner(alice), tx, toBobsAgent)
		return err
	})
	requireCode(t, err, errs.CodeForbidden)
	// Alice revoking Bob's capability.
	_, err = f.revoke(f.owner(alice), bobCap.ID)
	requireCode(t, err, errs.CodeForbidden)
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM economic_intents WHERE delegation_id = $1`, bobCap.ID))

	// The database refuses the same crossings when Go is not the caller.
	owner := ownerConn(t)
	err = rawTx(f.ctx, owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(f.ctx, `INSERT INTO delegated_capability_usage (id, capability_id, account_id, agent_id, subject_kind, subject_id,
			action_class, notional_minor, used_at, usage_day) VALUES ($1, $2, $3, $4, 'SWAP', $5, 'ACQUIRE_EXPOSURE', 1, $6, $7::date)`,
			uuid.New(), bobCap.ID, alice.account, bob.agentID, uuid.New(), f.clk.Now(), f.clk.Now().Format(time.DateOnly))
		return err
	})
	requireSQLState(t, err, "DG003")
	err = rawTx(f.ctx, owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(f.ctx, `UPDATE delegated_capabilities SET status = 'REVOKED', revoked_by_actor_type = 'USER',
			revoked_by_actor_id = $2, revocation_reason = 'cross', revoked_at = now() WHERE id = $1`, bobCap.ID, alice.user.String())
		return err
	})
	requireSQLState(t, err, "DG003")

	// Each account's swaps are signed by its own derived key only.
	sw := f.mustSwap(alice, aliceCap.ID, 1, 1_000_000)
	res, err := f.svc.ExecuteSwap(f.ctx, sw.ID)
	require.NoError(t, err)
	require.Equal(t, SwapSettled, res.Swap.Status)
	tx, err := devnet.Decode(res.Swap.SignedTx)
	require.NoError(t, err)
	assert.Equal(t, alice.wallet, tx.Message.FeePayer)
	assert.NotEqual(t, bob.wallet, tx.Message.FeePayer)
	assert.Equal(t, uint64(10_000_000), f.chainBalance(bob.wallet, f.usd), "Bob's wallet is untouched")
}

// TestIntegration_ConcurrentSigningsCannotExceedDailyNotional: N agents'
// requests race under one capability; together they never exceed its daily
// notional, in the books or in the capability usage.
func TestIntegration_ConcurrentSigningsCannotExceedDailyNotional(t *testing.T) {
	f := newFixture(t)
	c := f.customer(100_000_000)
	capability := f.grant(c, func(r *GrantRequest) {
		r.MaxSingleTrade, r.MaxDailyNotional, r.MaxNotional = money.USDFromMinor(100), money.USDFromMinor(500), money.USDFromMinor(10_000)
	})
	const n = 12 // each $1.00; the daily limit admits exactly 5
	var wg sync.WaitGroup
	errsOut := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errsOut[i] = f.requestSwap(f.agent(c), f.swapRequest(c, capability.ID, uint64(i+1), 1_000_000))
		}(i)
	}
	wg.Wait()
	admitted := 0
	for _, err := range errsOut {
		if err == nil {
			admitted++
			continue
		}
		requireCode(t, err, errs.CodeRiskRejected)
		assert.Contains(t, err.Error(), ReasonDailyLimitExceeded)
	}
	assert.Equal(t, 5, admitted)
	assert.Equal(t, 500, f.count(`SELECT coalesce(sum(notional_minor),0)::int FROM delegated_capability_usage WHERE capability_id = $1`, capability.ID))
	assert.Equal(t, 5, f.count(`SELECT count(*) FROM asset_reservations r JOIN delegated_swaps s ON s.reservation_id = r.id
		WHERE s.capability_id = $1 AND r.status = 'ACTIVE'`, capability.ID))
}

// TestIntegration_ExactlyOneSignaturePerSwap: many executors race on one
// swap; the provider decides once, signs once, the chain sees one
// transaction, the books one posting.
func TestIntegration_ExactlyOneSignaturePerSwap(t *testing.T) {
	f := newFixture(t)
	c := f.customer(10_000_000)
	capability := f.grant(c, nil)
	sw := f.mustSwap(c, capability.ID, 1, 2_000_000)
	const n = 10
	var wg sync.WaitGroup
	results := make([]SwapResult, n)
	errsOut := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errsOut[i] = f.svc.ExecuteSwap(f.ctx, sw.ID)
		}(i)
	}
	wg.Wait()
	for _, err := range errsOut {
		require.NoError(t, err)
	}
	fresh := 0
	for _, r := range results {
		if !r.Replay {
			fresh++
		}
	}
	assert.Equal(t, 1, fresh, "exactly one executor signed")
	// Settle whatever the replays only observed.
	_, err := f.svc.ReconcilePending(f.ctx, 100, true)
	require.NoError(t, err)
	got := f.swap(sw.ID)
	require.Equal(t, SwapSettled, got.Status)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM delegated_signing_decisions WHERE swap_id = $1`, sw.ID))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM economic_effects WHERE effect_key = $1`, "delegation:swap:"+sw.ID.String()))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM journal_transactions WHERE reference_type = 'delegated_swap' AND reference_id = $1`, sw.ID.String()))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM devnet_events WHERE network = $1 AND kind = 'SUBMIT' AND tx_id = $2`, "dg-"+f.tag, string(got.TxID)))
	assert.Equal(t, uint64(8_000_000), f.chainBalance(c.wallet, f.usd), "paid once")
}

// TestIntegration_HumanApprovalBindsThePlan: a capability that requires a
// person sends every swap to its owner, whose decision binds the exact plan.
func TestIntegration_HumanApprovalBindsThePlan(t *testing.T) {
	f := newFixture(t)
	c := f.customer(10_000_000)
	capability := f.grant(c, func(r *GrantRequest) { r.RequiresHumanApproval = true })
	sw := f.mustSwap(c, capability.ID, 1, 1_000_000)
	require.Equal(t, SwapAwaitingApproval, sw.Status)

	// Not signable before the decision.
	out, err := f.svc.ExecuteSwap(f.ctx, sw.ID)
	require.NoError(t, err)
	assert.Equal(t, SwapAwaitingApproval, out.Swap.Status)
	assert.Empty(t, out.Swap.TxID)

	decide := func(ctx context.Context, d SwapDecision) (Swap, error) {
		var out Swap
		err := f.inTx(func(_ context.Context, tx pgx.Tx) error {
			var err error
			out, err = f.svc.DecideSwap(ctx, tx, d)
			return err
		})
		return out, err
	}
	// The agent cannot approve its own swap; a decision on another plan is
	// refused; a stale authentication is refused.
	_, err = decide(f.agent(c), SwapDecision{SwapID: sw.ID, PlanHash: sw.PlanHash, Approve: true, Reason: "self"})
	requireCode(t, err, errs.CodeForbidden)
	_, err = decide(f.owner(c), SwapDecision{SwapID: sw.ID, PlanHash: [32]byte{1}, Approve: true, Reason: "other plan"})
	requireCode(t, err, errs.CodeConflict)
	stale := security.WithPrincipal(f.ctx, security.Principal{SubjectID: c.user.String(), ActorType: security.ActorUser,
		Roles: []security.Role{security.RoleCustomer}, AccountIDs: []string{c.account.String()},
		AuthTime: f.clk.Now().Add(-time.Hour), AMR: []string{"mfa"}})
	_, err = decide(stale, SwapDecision{SwapID: sw.ID, PlanHash: sw.PlanHash, Approve: true, Reason: "stale"})
	requireCode(t, err, errs.CodeStepUpRequired)

	approved, err := decide(f.owner(c), SwapDecision{SwapID: sw.ID, PlanHash: sw.PlanHash, Approve: true, Reason: "looks right"})
	require.NoError(t, err)
	require.Equal(t, SwapApproved, approved.Status)
	res, err := f.svc.ExecuteSwap(f.ctx, sw.ID)
	require.NoError(t, err)
	assert.Equal(t, SwapSettled, res.Swap.Status)

	// A decline cancels and returns the funds.
	sw2 := f.mustSwap(c, capability.ID, 2, 1_000_000)
	declined, err := decide(f.owner(c), SwapDecision{SwapID: sw2.ID, PlanHash: sw2.PlanHash, Approve: false, Reason: "no"})
	require.NoError(t, err)
	assert.Equal(t, SwapCancelled, declined.Status)
	assert.Equal(t, capital.ReservationReleased, f.reservationOf(sw2.ReservationID).Status)
	assert.Equal(t, econ.StatusCancelled, f.intentOf(sw2.IntentID).Status)
}

// TestIntegration_UnknownSubmissionIsReconciledNotRetried: a submission
// whose outcome is unknown is marked UNKNOWN and settled by observing the
// chain by transaction id -- landed or never landed -- and is never
// submitted again.
func TestIntegration_UnknownSubmissionIsReconciledNotRetried(t *testing.T) {
	f := newFixture(t)
	c := f.customer(10_000_000)
	capability := f.grant(c, nil)

	landed := f.mustSwap(c, capability.ID, 1, 1_000_000)
	dropped := f.mustSwap(c, capability.ID, 2, 1_000_000)
	require.NoError(t, f.host.TimeoutButLand(f.ctx, devnethost.ChainSolana))
	_, err := f.svc.ExecuteSwap(f.ctx, landed.ID)
	requireCode(t, err, errs.CodeSubmissionStateUnknown)
	got := f.swap(landed.ID)
	require.Equal(t, SwapUnknown, got.Status)
	assert.Equal(t, econ.StatusUnknown, f.intentOf(got.IntentID).Status)
	e, err := f.effects.Get(f.ctx, testDB, *got.EffectID)
	require.NoError(t, err)
	assert.Equal(t, econ.EffectUnknown, e.Status)

	// While an outcome is unknown the account takes no new delegated risk.
	_, err = f.requestSwap(f.agent(c), f.swapRequest(c, capability.ID, 3, 1_000_000))
	requireCode(t, err, errs.CodeRiskRejected)
	requireReason(t, err, "RISK_SETTLEMENT_UNKNOWN")

	require.NoError(t, f.host.TimeoutAndDrop(f.ctx, devnethost.ChainSolana))
	_, err = f.svc.ExecuteSwap(f.ctx, dropped.ID)
	requireCode(t, err, errs.CodeSubmissionStateUnknown)

	// Reconciliation observes and produces blocks; it never resubmits.
	_, err = f.svc.ReconcilePending(f.ctx, 100, true)
	require.NoError(t, err)
	got = f.swap(landed.ID)
	assert.Equal(t, SwapSettled, got.Status)
	assert.Equal(t, econ.StatusReconciled, f.intentOf(got.IntentID).Status)
	gone := f.swap(dropped.ID)
	assert.Equal(t, SwapFailed, gone.Status)
	assert.Equal(t, RefusalSubmissionExpired, gone.FailureCode)
	assert.Equal(t, capital.ReservationReleased, f.reservationOf(gone.ReservationID).Status)
	assert.Equal(t, econ.StatusFailed, f.intentOf(gone.IntentID).Status)
	for _, sw := range []Swap{got, gone} {
		assert.Equal(t, 1, f.count(`SELECT count(*) FROM devnet_events WHERE network = $1 AND kind = 'SUBMIT' AND tx_id = $2`,
			"dg-"+f.tag, string(sw.TxID)), "submitted exactly once")
	}
	assert.Equal(t, uint64(9_000_000), f.chainBalance(c.wallet, f.usd), "only the landed swap paid")
	assert.Equal(t, "9000000", f.balance(c.account, "WALLET", f.usd).String())
}

// TestIntegration_AFailedChainTransactionPostsNothing: a swap the chain
// executes and fails (the venue cannot deliver on chain) posts nothing and
// returns the reserved funds.
func TestIntegration_AFailedChainTransactionPostsNothing(t *testing.T) {
	f := newFixture(t)
	c := f.customer(10_000_000)
	capability := f.grant(c, nil)
	// Freeze the token's mint on chain so the venue's leg fails.
	mint, err := devnet.ParsePublicKey(f.tok.MintAddress)
	require.NoError(t, err)
	require.NoError(t, f.host.SetMintFrozen(f.ctx, devnethost.ChainSolana, mint, true))
	sw := f.mustSwap(c, capability.ID, 1, 1_000_000)
	res, err := f.svc.ExecuteSwap(f.ctx, sw.ID)
	require.NoError(t, err)
	assert.Equal(t, SwapFailed, res.Swap.Status)
	assert.Contains(t, res.Swap.FailureCode, RefusalChainFailed)
	assert.Nil(t, res.Swap.JournalTransactionID)
	assert.Equal(t, capital.ReservationReleased, f.reservationOf(sw.ReservationID).Status)
	assert.Equal(t, "10000000", f.balance(c.account, "WALLET", f.usd).String())
	assert.Equal(t, uint64(10_000_000), f.chainBalance(c.wallet, f.usd))
	require.NoError(t, f.host.SetMintFrozen(f.ctx, devnethost.ChainSolana, mint, false))
}

// TestIntegration_KeysSurviveARestart: a provider restarted with the same
// master seed signs for the same wallets; one with another seed refuses to
// claim them. Only public keys are stored.
func TestIntegration_KeysSurviveARestart(t *testing.T) {
	f := newFixture(t)
	c := f.customer(10_000_000)
	capability := f.grant(c, nil)
	f.mustSwap(c, capability.ID, 1, 1_000_000) // registers the key

	restarted, err := NewDelegatedSigner(seedOf("master-"+f.tag), SignerConfig{Environment: config.EnvTest, Clock: f.clk,
		Policy: DefaultSigningPolicy(), Fees: HostFees(f.host)})
	require.NoError(t, err)
	f.rebuild(func(d *Deps) { d.Signer = restarted })
	sw := f.mustSwap(c, capability.ID, 2, 1_000_000)
	res, err := f.svc.ExecuteSwap(f.ctx, sw.ID)
	require.NoError(t, err)
	assert.Equal(t, SwapSettled, res.Swap.Status)

	stranger, err := NewDelegatedSigner(seedOf("another-master"), SignerConfig{Environment: config.EnvTest, Clock: f.clk,
		Policy: DefaultSigningPolicy(), Fees: HostFees(f.host)})
	require.NoError(t, err)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := stranger.EnsureKey(ctx, tx, c.account, devnethost.ChainSolana)
		return err
	})
	requireCode(t, err, errs.CodeConflict)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM delegated_signer_keys WHERE account_id = $1`, c.account))
	var cols []string
	rows, err := testDB.Query(f.ctx, `SELECT column_name FROM information_schema.columns WHERE table_name = 'delegated_signer_keys' ORDER BY 1`)
	require.NoError(t, err)
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		cols = append(cols, s)
	}
	rows.Close()
	assert.Equal(t, []string{"account_id", "chain_id", "created_at", "derivation", "id", "public_key"}, cols, "no secret column")
}

// mismatchHost reports a different transaction under every id: what a
// broken or dishonest chain would say.
type mismatchHost struct{ ChainHost }

func (m mismatchHost) GetTransaction(ctx context.Context, chain devnet.ChainID, txID devnet.TxID) (devnet.TxRecord, error) {
	rec, err := m.ChainHost.GetTransaction(ctx, chain, txID)
	if err != nil {
		return rec, err
	}
	rec.Transaction.Message.RecentBlockhash = devnet.Hash{9}
	return rec, nil
}

// TestIntegration_AChainMismatchOpensReconciliationAndPostsNothing.
func TestIntegration_AChainMismatchOpensReconciliationAndPostsNothing(t *testing.T) {
	f := newFixture(t)
	c := f.customer(10_000_000)
	capability := f.grant(c, nil)
	f.rebuild(func(d *Deps) { d.Host = mismatchHost{ChainHost: f.host} })
	sw := f.mustSwap(c, capability.ID, 1, 1_000_000)
	_, err := f.svc.ExecuteSwap(f.ctx, sw.ID)
	requireCode(t, err, errs.CodeReconciliationRequired)
	got := f.swap(sw.ID)
	assert.Equal(t, SwapSubmitted, got.Status, "nothing posted, nothing settled")
	assert.Nil(t, got.JournalTransactionID)
	rec, found, err := f.recon.FindUnresolvedByScope(f.ctx, testDB, reconciliation.KindDevnetChain, ScopeDelegatedTransaction, sw.ID.String())
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, rec.Material)
	assert.True(t, rec.BlocksNewRisk)
}

// TestIntegration_KillSwitchesStopDelegatedSigning: an agent pause, an
// account freeze and a venue disable each stop new delegated swaps, and a
// frozen account stops a swap already admitted.
func TestIntegration_KillSwitchesStopDelegatedSigning(t *testing.T) {
	f := newFixture(t)
	c := f.customer(10_000_000)
	capability := f.grant(c, nil)
	admitted := f.mustSwap(c, capability.ID, 1, 1_000_000)

	f.activateKill(killswitch.AgentPause, c.agentID)
	_, err := f.requestSwap(f.agent(c), f.swapRequest(c, capability.ID, 2, 1_000_000))
	requireCode(t, err, errs.CodeKillSwitchActive)
	_, err = f.svc.ExecuteSwap(f.ctx, admitted.ID)
	requireCode(t, err, errs.CodeSigningRejected)
	assert.Equal(t, RefusalKillSwitch, f.swap(admitted.ID).FailureCode)

	other := f.customer(10_000_000)
	otherCap := f.grant(other, nil)
	f.activateKill(killswitch.VenueDisable, f.venue.ID.String())
	_, err = f.requestSwap(f.agent(other), f.swapRequest(other, otherCap.ID, 1, 1_000_000))
	requireCode(t, err, errs.CodeKillSwitchActive)
}

// TestIntegration_AReduceExposureSwapSettles: selling the token for dollars
// under REDUCE_EXPOSURE settles the mirror exchange.
func TestIntegration_AReduceExposureSwapSettles(t *testing.T) {
	f := newFixture(t)
	c := f.customer(0)
	f.fundCustomer(c, f.tok, 50_000_000) // 0.05 token
	capability := f.grant(c, nil)
	req := f.swapRequest(c, capability.ID, 1, 20_000_000) // 0.02 token at 100 = $2 less 50 bp
	req.Action, req.PayAssetID, req.ReceiveAssetID = ActionReduceExposure, f.tok.ID, f.usd.ID
	ticket, err := f.requestSwap(f.agent(c), req)
	require.NoError(t, err)
	assert.Equal(t, "1990000", ticket.Swap.AmountOut.String())
	assert.Equal(t, econ.KindReduceExposure, f.intentOf(ticket.Swap.IntentID).Kind)
	res, err := f.svc.ExecuteSwap(f.ctx, ticket.Swap.ID)
	require.NoError(t, err)
	require.Equal(t, SwapSettled, res.Swap.Status)
	assert.Equal(t, "30000000", f.balance(c.account, "WALLET", f.tok).String())
	assert.Equal(t, "1990000", f.balance(c.account, "WALLET", f.usd).String())
	assert.Equal(t, uint64(1_990_000), f.chainBalance(c.wallet, f.usd))
}

// TestIntegration_DevnetModeIsObservedNotDriven: in DEVNET mode the service
// signs and submits, but blocks come from the chain's own producer; the
// reconciler settles once they have.
func TestIntegration_DevnetModeIsObservedNotDriven(t *testing.T) {
	f := newFixture(t)
	c := f.customer(10_000_000)
	capability := f.grant(c, func(r *GrantRequest) { r.Mode = gates.ModeDevnet })
	sw := f.mustSwap(c, capability.ID, 1, 1_000_000)
	assert.Equal(t, gates.ModeDevnet, sw.Mode)
	res, err := f.svc.ExecuteSwap(f.ctx, sw.ID)
	require.NoError(t, err)
	assert.Equal(t, SwapSubmitted, res.Swap.Status, "nothing produced the blocks finality needs")
	_, err = f.host.ProduceBlocks(f.ctx, devnethost.ChainSolana, 1+itDepth)
	require.NoError(t, err)
	_, err = f.svc.ReconcilePending(f.ctx, 100, false)
	require.NoError(t, err)
	assert.Equal(t, SwapSettled, f.swap(sw.ID).Status)
}

// TestIntegration_AnAgentCanNeverWithdrawDirectly: the settlement compiler
// refuses an agent's WITHDRAW at the highest supported authority level --
// which is why a delegated withdrawal is the withdrawal service's intent on
// an agent's proposal.
func TestIntegration_AnAgentCanNeverWithdrawDirectly(t *testing.T) {
	f := newFixture(t)
	c := f.customer(0)
	q := money.QuantityFromInt64(1_000_000)
	in := econ.Intent{ID: econ.NewIntentID(), AccountID: c.account, Kind: econ.KindWithdraw, Mode: gates.ModeSandbox,
		Capability: gates.AgentWithdrawals, ActorType: security.ActorAgent, ActorID: c.agentID, AgentID: c.agentID,
		AssetID: &f.usd.ID, Quantity: &q, IdempotencyKey: "direct-" + uuid.NewString()}
	router, err := legalrouterConservative()
	require.NoError(t, err)
	rt, err := econ.Route(econ.RouteRequest{Intent: in, Subject: settlement.Subject{Type: settlement.SubjectDestination,
		ID: id.New[id.Any]().String(), AssetID: f.usd.ID.String()}, AgentAuthority: agentauthority.LevelUserApprovedRule,
		Verification: valuedomain.VerificationNone}, router, f.clk.Now())
	require.NoError(t, err)
	assert.False(t, rt.MayExecuteNow())
	assert.True(t, rt.HasReason(settlement.ReasonAgentActionForbidden), "%v", rt.Reasons)
}
