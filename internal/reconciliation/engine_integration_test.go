//go:build integration

package reconciliation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/chain/chaintest"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// killswitchAudit adapts internal/audit to killswitch.AuditAppender.
type killswitchAudit struct{ w audit.Writer }

func (k killswitchAudit) Append(ctx context.Context, tx pgx.Tx, e killswitch.AuditEvent) error {
	_, err := k.w.Append(ctx, tx, audit.Event{
		Stream: e.Stream, ActorType: e.ActorType, ActorID: e.ActorID, Action: e.Action,
		ResourceType: e.ResourceType, ResourceID: e.ResourceID, Reason: e.Reason,
		EvidenceRef: e.EvidenceRef, PolicyVersion: e.PolicyVersion, Payload: e.Payload, OccurredAt: e.OccurredAt,
	})
	return err
}

// noApprovals satisfies killswitch.ApprovalVerifier; activation never needs it.
type noApprovals struct{}

func (noApprovals) VerifyApproved(context.Context, db.Querier, string, string, string) (killswitch.Approval, error) {
	return killswitch.Approval{}, errs.New(errs.CodeForbidden, "no approvals in this test")
}

// TestIntegration_KillSwitchNeverStopsReconciliation is the PART 52 property:
// kill switches halt new risk; they never halt reconciliation, observation,
// settlement, ledger posting or cancellation.
//
// The global kill and an account freeze are both active for the whole test.
// Recovery still adopts the external fill, still posts to the ledger, still
// moves the position, still finalizes the reservation, and the record still
// resolves.
func TestIntegration_KillSwitchNeverStopsReconciliation(t *testing.T) {
	d := openTestDB(t)
	f := newFixture(t, d)

	// Activate GLOBAL_NEW_RISK_KILL and ACCOUNT_FREEZE.
	ctrl, err := killswitch.NewController(f.clk, killswitchAudit{w: audit.NewWriter()}, noApprovals{})
	require.NoError(t, err)
	opCtx := f.operatorCtx(f.newOperator("killer"), security.RoleSecurity)
	require.NoError(t, d.InTx(opCtx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := ctrl.Activate(ctx, tx, killswitch.GlobalNewRiskKill, "*", "itest: halt all new risk"); err != nil {
			return err
		}
		return nil
	}))
	require.NoError(t, d.InTx(opCtx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ctrl.Activate(ctx, tx, killswitch.AccountFreeze, f.account.String(), "itest: freeze this account")
		return err
	}))

	// The matrix itself says so: the un-killable classes are never blocked,
	// while NEW_RISK is.
	checker := killswitch.NewChecker(killswitch.Policy{})
	for _, class := range []killswitch.ActionClass{
		killswitch.Reconcile, killswitch.Observe, killswitch.Settle, killswitch.LedgerPost, killswitch.Cancel,
	} {
		require.NoError(t, checker.Check(f.ctx, d, killswitch.Action{Class: class, AccountID: f.account.String()}),
			"%s must never be blocked by a kill switch (PART 52)", class)
	}
	err = checker.Check(f.ctx, d, killswitch.Action{Class: killswitch.NewRisk, AccountID: f.account.String()})
	require.Error(t, err, "new risk is blocked while the kill switch is active")
	assert.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(err))

	// And the engine behaves the same way: the whole recovery runs.
	f.seedChainBalances()
	f.fund(f.usdc, money.QuantityFromInt64(fundedUSDC), 10_000)
	res := f.reserve(money.QuantityFromInt64(reservedUSDC), 5_000)
	sig := "SIG-kill-" + f.suffix
	ord, att := f.submittingOrder(res,
		money.QuantityFromInt64(orderInput), money.QuantityFromInt64(orderMinOut), sig, 100)
	f.land(sig, money.QuantityFromInt64(orderInput), money.QuantityFromInt64(filledOutput), money.QuantityFromInt64(networkFee))

	out, err := f.engine.RecoverAttempt(f.ctx, att.ID)
	require.NoError(t, err, "reconciliation must run with every kill switch active")
	assert.Equal(t, DispositionAdopted, out.Disposition)
	fills := f.fills(ord.ID)
	require.Len(t, fills, 1, "a fill already received is still processed (PART 52)")
	assert.Equal(t, 1, f.countRows(
		`SELECT count(*) FROM journal_transactions WHERE kind = 'TRADE_FILL' AND reference_id = $1`, fills[0].ID),
		"ledger posting is not blocked")
	assert.Equal(t, "CONSUMED", string(f.reservation(res.ID).Status))

	// Internal verification also runs, and so does a FULL balance pass.
	_, err = f.engine.VerifyInternal(f.ctx)
	require.NoError(t, err)
	_, err = f.engine.RunFull(f.ctx, f.account)
	require.NoError(t, err)

	// The switches are still active: nothing in reconciliation released them.
	active, err := ctrl.Active(f.ctx, d)
	require.NoError(t, err)
	kinds := map[killswitch.Kind]bool{}
	for _, s := range active {
		kinds[s.Kind] = true
	}
	assert.True(t, kinds[killswitch.GlobalNewRiskKill])
	assert.True(t, kinds[killswitch.AccountFreeze])
}

// TestIntegration_AgentCanNeverResolve is the PART 9 / PART 51 rule, enforced
// in Go and in the database.
func TestIntegration_AgentCanNeverResolve(t *testing.T) {
	d := openTestDB(t)
	f := newFixture(t, d)
	rec := f.openMismatch(t, false)
	agentCtx := f.agentCtx("agent-" + f.suffix)

	t.Run("investigate", func(t *testing.T) {
		err := d.InTx(agentCtx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.engine.Investigate(ctx, tx, rec.ID, "an agent trying to investigate")
			return err
		})
		require.Error(t, err)
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	})

	t.Run("resolve manual", func(t *testing.T) {
		err := d.InTx(agentCtx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.engine.ResolveManual(ctx, tx, rec.ID, ManualResolution{
				Operator:    Actor{Type: security.ActorAgent, ID: "agent-" + f.suffix},
				Reason:      "an agent trying to resolve a mismatch",
				EvidenceRef: "none",
			})
			return err
		})
		require.Error(t, err)
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	})

	t.Run("escalate", func(t *testing.T) {
		err := d.InTx(agentCtx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.records.Transition(ctx, tx, rec.ID, StatusEscalated, TransitionEvidence{
				Actor: Actor{Type: security.ActorAgent, ID: "agent-" + f.suffix}, Reason: "agent escalation",
			})
			return err
		})
		require.Error(t, err)
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	})

	t.Run("database refuses AGENT as resolver", func(t *testing.T) {
		// Even if every Go guard were bypassed, the CHECK constraint on
		// reconciliation_records.resolved_by_actor_type refuses it.
		_, err := d.Exec(f.ctx, `UPDATE reconciliation_records SET resolved_by_actor_type = 'AGENT' WHERE id = $1`, rec.ID)
		require.Error(t, err)
		assert.True(t, db.IsCheckViolation(err), "expected a CHECK violation, got %v", err)
	})

	// The record is untouched.
	after, err := f.records.Get(f.ctx, d, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusMismatch, after.Status)
	assert.Empty(t, after.ResolvedByActorID)
}

// TestIntegration_MaterialResolutionNeedsARealApproval covers every way a
// material resolution can fail dual control.
func TestIntegration_MaterialResolutionNeedsARealApproval(t *testing.T) {
	d := openTestDB(t)
	f := newFixture(t, d)
	rec := f.openMismatch(t, true)
	other := f.openMismatch(t, true)

	opID := f.newOperator("resolver")
	opCtx := f.operatorCtx(opID, security.RoleOperations)
	investigate(opCtx, t, f, rec.ID)
	investigate(opCtx, t, f, other.ID)

	base := ManualResolution{
		Operator:    Actor{Type: security.ActorOperator, ID: opID},
		Reason:      "operator resolving a material mismatch in a test",
		EvidenceRef: "incident://approvals/" + f.suffix,
	}

	t.Run("no approval", func(t *testing.T) {
		err := d.InTx(opCtx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.engine.ResolveManual(ctx, tx, rec.ID, base)
			return err
		})
		require.Error(t, err)
		assert.Equal(t, errs.CodeStepUpRequired, errs.CodeOf(err))
	})

	t.Run("approval for another record", func(t *testing.T) {
		action := f.approvedAction(opCtx, t, other.ID)
		res := base
		res.ApprovalID = action
		err := d.InTx(opCtx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.engine.ResolveManual(ctx, tx, rec.ID, res)
			return err
		})
		require.Error(t, err)
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	})

	t.Run("unknown approval", func(t *testing.T) {
		res := base
		res.ApprovalID = "not-a-uuid"
		err := d.InTx(opCtx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.engine.ResolveManual(ctx, tx, rec.ID, res)
			return err
		})
		require.Error(t, err)
		assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
	})

	t.Run("resolver is not the authenticated principal", func(t *testing.T) {
		res := base
		res.Operator = Actor{Type: security.ActorOperator, ID: f.newOperator("impostor")}
		res.ApprovalID = f.approvedAction(opCtx, t, rec.ID)
		err := d.InTx(opCtx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.engine.ResolveManual(ctx, tx, rec.ID, res)
			return err
		})
		require.Error(t, err)
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	})

	// Still unresolved, still blocking.
	after, err := f.records.Get(f.ctx, d, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusInvestigating, after.Status)
	assert.True(t, after.BlocksNewRisk)
}

// TestIntegration_AutomaticResolutionIsNarrow proves that the automatic path
// only fires for enumerated causes and never for a material record, and that
// a dust adjustment posts exactly one journal transaction however often it is
// retried.
func TestIntegration_AutomaticResolutionIsNarrow(t *testing.T) {
	d := openTestDB(t)
	f := newFixture(t, d)
	f.fund(f.usdc, money.QuantityFromInt64(fundedUSDC), 10_000)

	// A one-lamport-scale dust difference: the chain holds 500 base units
	// less than the ledger, and the policy calls that dust.
	policy := DefaultPolicy()
	policy.DustQuantity[f.usdc.ID] = money.QuantityFromInt64(1_000)
	policy.AutoPostDustAdjustment = true
	policy.MaterialThresholdUSDMinor = 100
	engine := f.newEngine(Config{Policy: policy})

	f.sim.SetBalance(chain.BalanceObservation{
		Owner: f.walletAddr, Mint: f.usdc.MintAddress, TokenAccount: "ata-usdc-" + f.suffix,
		Amount: money.QuantityFromInt64(fundedUSDC - 500), Decimals: 6, DecimalsKnown: true,
	})
	recs, err := engine.RunFull(f.ctx, f.account)
	require.NoError(t, err)
	rec := findRecord(t, recs, KindWalletBalance)
	require.Equal(t, StatusMismatch, rec.Status)
	require.False(t, rec.Material, "a dust difference below the USD threshold is not material")

	t.Run("an unenumerated cause is refused", func(t *testing.T) {
		err := d.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := engine.ResolveAutomatic(ctx, tx, rec.ID, AutoCause("BECAUSE"))
			return err
		})
		require.Error(t, err)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	})

	var resolved Record
	require.NoError(t, d.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		resolved, err = engine.ResolveAutomatic(ctx, tx, rec.ID, AutoCauseFeeDust)
		return err
	}))
	assert.Equal(t, StatusResolvedAutomatic, resolved.Status)
	assert.Equal(t, security.ActorSystem, resolved.ResolvedByActorType)
	require.NotEmpty(t, resolved.CompensatingJournalTxID)

	// Exactly one adjustment, and the ledger now equals the chain.
	assert.Equal(t, 1, f.countRows(
		`SELECT count(*) FROM journal_transactions WHERE kind = 'RECONCILIATION_ADJUSTMENT' AND reference_id = $1`,
		rec.ID.String()))
	assert.True(t, f.balance(ledger.CodeWallet, f.usdc.ID).Equal(money.QuantityFromInt64(fundedUSDC-500)))

	// A material record can never resolve automatically.
	material := f.openMismatch(t, true)
	err = d.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := engine.ResolveAutomatic(ctx, tx, material.ID, AutoCauseFeeDust)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
}

// TestIntegration_ObserverDisagreementBlocks is PART 196: when the two
// observers disagree the platform never picks the optimistic answer; it
// records a mismatch and blocks dependent activity.
func TestIntegration_ObserverDisagreementBlocks(t *testing.T) {
	d := openTestDB(t)
	f := newFixture(t, d)
	f.fund(f.usdc, money.QuantityFromInt64(fundedUSDC), 10_000)
	res := f.reserve(money.QuantityFromInt64(reservedUSDC), 5_000)
	sig := "SIG-disagree-" + f.suffix
	_, att := f.submittingOrder(res,
		money.QuantityFromInt64(orderInput), money.QuantityFromInt64(orderMinOut), sig, 100)
	f.land(sig, money.QuantityFromInt64(orderInput), money.QuantityFromInt64(filledOutput), money.QuantityFromInt64(networkFee))

	// The secondary observer reports different economics for the same
	// signature.
	f.secondary.Fault(chaintest.MethodGetTransaction, chaintest.Fault{Kind: chaintest.FaultWrongDeltas})

	metrics := NoopMetrics()
	engine := f.newEngine(Config{Metrics: metrics})
	out, err := engine.RecoverAttempt(f.ctx, att.ID)
	require.NoError(t, err)

	assert.Equal(t, chain.Disagreed, out.Resolution.State)
	assert.Equal(t, DispositionUncertain, out.Disposition)
	assert.Equal(t, StatusMismatch, out.Record.Status)
	assert.True(t, out.Record.Material)
	assert.True(t, out.Record.BlocksNewRisk)
	assert.True(t, out.ReservationKept, "PART 48: the reservation is kept while uncertainty remains")
	assert.Positive(t, metrics.AlertCount(AlertObserverDisagreement))
	assert.Positive(t, metrics.SEV1Count())

	// Nothing was adopted and no money moved.
	assert.Zero(t, f.countRows(`SELECT count(*) FROM fills WHERE order_id = $1`, out.OrderID))
	assert.True(t, f.balance(ledger.CodeWallet, f.usdc.ID).Equal(money.QuantityFromInt64(fundedUSDC)),
		"no posting reached the ledger")
	assert.True(t, f.balance(ledger.CodeWallet, f.sol.ID).IsZero())
	assert.Equal(t, "ACTIVE", string(f.reservation(res.ID).Status))
	require.Error(t, engine.Blocks().RequireNoBlock(f.ctx, d, f.account))
}

// TestIntegration_ProvenAbsentReleasesUncertainty covers the PART 48 branch
// where both observers agree the transaction never landed and the chain has
// moved past the last valid block height.
func TestIntegration_ProvenAbsentReleasesUncertainty(t *testing.T) {
	d := openTestDB(t)
	f := newFixture(t, d)
	f.fund(f.usdc, money.QuantityFromInt64(fundedUSDC), 10_000)
	res := f.reserve(money.QuantityFromInt64(reservedUSDC), 5_000)
	sig := "SIG-absent-" + f.suffix
	ord, att := f.submittingOrder(res,
		money.QuantityFromInt64(orderInput), money.QuantityFromInt64(orderMinOut), sig, 10)

	// Nothing landed, and the chain is far past the attempt's deadline.
	f.sim.Advance(500)

	out, err := f.engine.RecoverAttempt(f.ctx, att.ID)
	require.NoError(t, err)
	assert.Equal(t, DispositionProvenAbsent, out.Disposition)
	assert.True(t, out.RetryAllowed, "PART 48: proven absence permits a retry under the same semantic context")
	assert.Equal(t, StatusMatched, out.Record.Status, "a definite negative answer is not a mismatch")
	assert.False(t, out.Record.BlocksNewRisk)

	// The attempt is EXPIRED; the reservation is untouched because the order
	// is not terminal — the executor owns that decision.
	after, err := f.attempts.Get(f.ctx, d, att.ID)
	require.NoError(t, err)
	assert.Equal(t, execution.AttemptExpired, after.Status)
	assert.Equal(t, "ACTIVE", string(f.reservation(res.ID).Status))
	assert.Zero(t, f.countRows(`SELECT count(*) FROM fills WHERE order_id = $1`, ord.ID))

	// A single observer can never prove absence: with the secondary gone the
	// same situation is uncertainty, not a proven negative.
	sig2 := "SIG-absent2-" + f.suffix
	res2 := f.reserveWithKey(money.QuantityFromInt64(reservedUSDC), 5_000, "res2-"+f.suffix)
	_, att2 := f.submittingOrderWithIntent(t, res2, sig2, 10)
	degraded := f.newEngine(Config{Observers: Observers{Primary: f.primary, Policy: chain.DefaultPolicy()}})
	out2, err := degraded.RecoverAttempt(f.ctx, att2.ID)
	require.NoError(t, err)
	assert.Equal(t, DispositionUncertain, out2.Disposition)
	assert.False(t, out2.RetryAllowed)
	assert.True(t, out2.Record.BlocksNewRisk)
}

// TestIntegration_UnknownWalletActivityOpensARecord is PART 48's "inspect
// wallet activity": a transaction touching the wallet that belongs to no
// attempt blocks new risk and needs a human classification.
func TestIntegration_UnknownWalletActivityOpensARecord(t *testing.T) {
	d := openTestDB(t)
	f := newFixture(t, d)
	f.fund(f.usdc, money.QuantityFromInt64(fundedUSDC), 10_000)
	res := f.reserve(money.QuantityFromInt64(reservedUSDC), 5_000)

	// An attempt with no signature at all: the executor never got one back.
	ord, att := f.submittingOrder(res,
		money.QuantityFromInt64(orderInput), money.QuantityFromInt64(orderMinOut), "SIG-never-sent-"+f.suffix, 100)
	_, err := d.Exec(f.ctx, `UPDATE execution_attempts SET tx_signature = NULL WHERE id = $1`, att.ID)
	require.NoError(t, err)
	att, err = f.attempts.Get(f.ctx, d, att.ID)
	require.NoError(t, err)
	require.Empty(t, att.TxSignature)

	// Wallet activity shows a transaction the platform did not initiate: it
	// moves an unrelated asset pair, so it cannot be this order's fill.
	stranger := f.sim.Swap(f.walletAddr, f.sol.MintAddress, f.usdc.MintAddress,
		money.QuantityFromInt64(1_000_000), money.QuantityFromInt64(90_000), money.QuantityFromInt64(5_000))
	stranger.Signature = "SIG-stranger-" + f.suffix
	f.sim.Land(stranger)
	f.sim.Advance(10)

	metrics := NoopMetrics()
	engine := f.newEngine(Config{Metrics: metrics})
	out, err := engine.RecoverAttempt(f.ctx, att.ID)
	require.NoError(t, err)
	assert.Equal(t, DispositionUncertain, out.Disposition)

	unknown, found, err := f.records.FindUnresolvedByScope(f.ctx, d, KindSubmissionUnknown, ScopeSignature, stranger.Signature)
	require.NoError(t, err)
	require.True(t, found, "wallet activity matching no attempt must open its own record")
	assert.True(t, unknown.Material)
	assert.True(t, unknown.BlocksNewRisk)
	assert.Positive(t, metrics.AlertCount(AlertUnknownTransaction))

	diff := decodeDoc(t, unknown.Difference)
	require.Contains(t, diff, "classification_required")

	// No fill was invented for the order.
	assert.Zero(t, f.countRows(`SELECT count(*) FROM fills WHERE order_id = $1`, ord.ID))
	require.Error(t, engine.Blocks().RequireNoBlock(f.ctx, d, f.account))
}

// TestIntegration_InternalDriftIsASEV1LedgerIntegrityViolation injects a
// direct ledger_balances change as the migrate role — the only role that can
// — and proves VerifyInternal notices.
func TestIntegration_InternalDriftIsASEV1LedgerIntegrityViolation(t *testing.T) {
	migrateURL := os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	if migrateURL == "" {
		t.Skip("CP_TEST_MIGRATE_DATABASE_URL not set")
	}
	d := openTestDB(t)
	f := newFixture(t, d)
	f.fund(f.usdc, money.QuantityFromInt64(fundedUSDC), 10_000)

	metrics := NoopMetrics()
	engine := f.newEngine(Config{Metrics: metrics})

	clean, err := engine.VerifyInternal(f.ctx)
	require.NoError(t, err)
	require.Empty(t, clean, "a freshly seeded account has no drift")

	// Corrupt the projection behind the ledger's back.
	ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
	defer cancel()
	md, err := db.Open(ctx, db.Config{URL: migrateURL, AppName: "reconciliation-itest-migrate", MaxConns: 2})
	require.NoError(t, err)
	defer md.Close()
	tag, err := md.Exec(ctx, `UPDATE ledger_balances SET balance = balance - 1
		WHERE ledger_account_id = (SELECT id FROM ledger_accounts
			WHERE owner_type = 'CUSTOMER' AND owner_id = $1 AND code = 'WALLET' AND asset_id = $2)`,
		f.account, f.usdc.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected())

	drift, err := engine.VerifyInternal(f.ctx)
	require.NoError(t, err)
	require.NotEmpty(t, drift, "a balance changed outside a posting must be detected")

	rec := findRecord(t, drift, KindLedgerInternal)
	assert.Equal(t, StatusMismatch, rec.Status)
	assert.True(t, rec.Material, "internal drift is always material")
	assert.True(t, rec.BlocksNewRisk)
	assert.Positive(t, metrics.AlertCount(AlertLedgerIntegrity), "SEV1 ledger_integrity_violation")
	assert.Positive(t, metrics.SEV1Count())

	diff := decodeDoc(t, rec.Difference)
	assert.Equal(t, "-1", diff["balance"])
	require.Error(t, engine.Blocks().RequireNoBlock(f.ctx, d, f.account))

	// Re-running converges on the same record rather than piling up.
	again, err := engine.VerifyInternal(f.ctx)
	require.NoError(t, err)
	require.NotEmpty(t, again)
	assert.Equal(t, rec.ID, findRecord(t, again, KindLedgerInternal).ID)

	// Put it back so the account is clean for any later assertion.
	_, err = md.Exec(ctx, `UPDATE ledger_balances SET balance = balance + 1
		WHERE ledger_account_id = (SELECT id FROM ledger_accounts
			WHERE owner_type = 'CUSTOMER' AND owner_id = $1 AND code = 'WALLET' AND asset_id = $2)`,
		f.account, f.usdc.ID)
	require.NoError(t, err)
}

// TestIntegration_PositionLedgerDriftIsDetected is the other half of
// RECONCILIATION.md §6.
func TestIntegration_PositionLedgerDriftIsDetected(t *testing.T) {
	d := openTestDB(t)
	f := newFixture(t, d)

	// A ledger balance with no lot behind it: Σ open lots ≠ WALLET balance.
	require.NoError(t, d.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.ledgerSvc.Post(ctx, tx, ledger.Posting{
			Kind:           ledger.KindSeed,
			IdempotencyKey: "seed-nolot:" + f.account.String(),
			Reference:      ledger.FinancialEventReference{Type: "seed", ID: f.account.String()},
			EffectiveAt:    f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(f.account, ledger.CodeWallet, f.usdc.ID), Side: ledger.Debit, Quantity: money.QuantityFromInt64(1_000_000)},
				{Account: ledger.CustomerAccount(f.account, ledger.CodeCapital, f.usdc.ID), Side: ledger.Credit, Quantity: money.QuantityFromInt64(1_000_000)},
			},
		})
		return err
	}))

	metrics := NoopMetrics()
	engine := f.newEngine(Config{Metrics: metrics})
	recs, err := engine.RunFull(f.ctx, f.account)
	require.NoError(t, err)
	rec := findRecord(t, recs, KindPositionLedger)
	assert.Equal(t, StatusMismatch, rec.Status)
	assert.True(t, rec.Material)
	assert.True(t, rec.BlocksNewRisk)
	assert.Positive(t, metrics.AlertCount(AlertLedgerIntegrity))

	diff := decodeDoc(t, rec.Difference)
	assert.Equal(t, "-1000000", diff["quantity"], "lots are 1 USDC short of the ledger")
}

// TestIntegration_OneStatusChangePerTransaction proves the rule the deferred
// binding trigger of migration 00603 imposes is caught in Go with a clear
// error instead of at COMMIT with SQLSTATE AU001.
func TestIntegration_OneStatusChangePerTransaction(t *testing.T) {
	d := openTestDB(t)
	f := newFixture(t, d)
	rec := f.openMismatch(t, false)

	err := d.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := f.records.Transition(ctx, tx, rec.ID, StatusEscalated, TransitionEvidence{
			Actor: SystemActor(), Reason: "first change in this transaction",
		}); err != nil {
			return err
		}
		_, err := f.records.Transition(ctx, tx, rec.ID, StatusInvestigating, TransitionEvidence{
			Actor: SystemActor(), Reason: "second change in the same transaction",
		})
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))

	// The transaction rolled back: the record is still MISMATCH.
	after, err := f.records.Get(f.ctx, d, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusMismatch, after.Status)

	// Every transition is refused when it is not in the PART 51 table.
	err = d.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.records.Transition(ctx, tx, rec.ID, StatusMatched, TransitionEvidence{
			Actor: SystemActor(), Reason: "MISMATCH -> MATCHED is not in the table",
		})
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))
}

// TestIntegration_TransitionsAreImmutable proves reconciliation_transitions
// cannot be rewritten by the application role.
func TestIntegration_TransitionsAreImmutable(t *testing.T) {
	d := openTestDB(t)
	f := newFixture(t, d)
	rec := f.openMismatch(t, false)
	trs := f.transitions(rec.ID)
	require.NotEmpty(t, trs)

	_, err := d.Exec(f.ctx, `UPDATE reconciliation_transitions SET reason = 'rewritten' WHERE id = $1`, trs[0].ID)
	require.Error(t, err)
	assert.True(t, db.IsMutationForbidden(err), "expected an immutability error, got %v", err)

	_, err = d.Exec(f.ctx, `DELETE FROM reconciliation_transitions WHERE id = $1`, trs[0].ID)
	require.Error(t, err)
	assert.True(t, db.IsMutationForbidden(err), "expected an immutability error, got %v", err)
}

// TestIntegration_FundingReconciliation covers RECONCILIATION.md §5: a
// deposit's expected credit against the observed chain receipt.
func TestIntegration_FundingReconciliation(t *testing.T) {
	d := openTestDB(t)
	f := newFixture(t, d)

	sig := "SIG-deposit-" + f.suffix
	depositID := f.newDeposit(t, "PROVIDER_CONFIRMED", money.QuantityFromInt64(10_000_000), sig)

	// No receipt yet: the record stays OPEN, not MATCHED. A provider
	// "success" alone never credits money (PART 162).
	rec, err := f.engine.ReconcileDeposit(f.ctx, depositID)
	require.NoError(t, err)
	assert.Equal(t, StatusOpen, rec.Status)
	assert.Equal(t, KindFunding, rec.Kind)

	// The chain credits exactly what was expected.
	obs := f.sim.Swap(f.walletAddr, f.sol.MintAddress, f.usdc.MintAddress,
		money.QuantityFromInt64(1), money.QuantityFromInt64(10_000_000), money.QuantityFromInt64(0))
	obs.Signature = sig
	f.sim.Land(obs)
	f.sim.Advance(10)

	matched, err := f.engine.ReconcileDeposit(f.ctx, depositID)
	require.NoError(t, err)
	assert.Equal(t, rec.ID, matched.ID, "the same record is classified, not a second one")
	assert.Equal(t, StatusMatched, matched.Status)

	observed := decodeDoc(t, matched.Observed)
	assert.Equal(t, "10000000", observed["quantity"])
	assert.Equal(t, true, observed["receipt_found"])
}

// --- shared helpers ---------------------------------------------------------

// openMismatch opens a WALLET_BALANCE mismatch directly, for tests about the
// lifecycle rather than about detection.
func (f *fixture) openMismatch(t *testing.T, material bool) Record {
	t.Helper()
	var rec Record
	require.NoError(t, f.d.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rec, err = f.records.Open(ctx, tx, OpenRequest{
			Kind: KindWalletBalance, Mode: ModeFull, ScopeType: ScopeWallet,
			ScopeID:   f.walletID + ":" + f.usdc.ID.String() + ":" + id.New[id.Any]().String(),
			AccountID: f.account, AssetID: f.usdc.ID,
			Expected:      map[string]any{"quantity": "100000000"},
			Observed:      map[string]any{"quantity": "99990000"},
			Difference:    map[string]any{"quantity": "-10000", "decimal": "-0.010000"},
			Status:        StatusMismatch,
			Material:      material,
			BlocksNewRisk: true,
			Actor:         SystemActor(), Reason: "itest mismatch",
		})
		return err
	}))
	return rec
}

func investigate(ctx context.Context, t *testing.T, f *fixture, recordID RecordID) {
	t.Helper()
	require.NoError(t, f.d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.engine.Investigate(ctx, tx, recordID, "operator picked this up in a test")
		return err
	}))
}

// approvedAction proposes and approves a RECONCILIATION_RESOLVE_MATERIAL
// action for a record and returns its id.
func (f *fixture) approvedAction(proposerCtx context.Context, t *testing.T, recordID RecordID) string {
	t.Helper()
	params, err := json.Marshal(map[string]any{"record_id": recordID.String()})
	require.NoError(t, err)
	var actionID string
	require.NoError(t, f.d.InTx(proposerCtx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		a, err := f.adminSvc.Propose(ctx, tx, admin.Proposal{
			Kind: admin.KindReconciliationResolveMaterial, TargetType: "reconciliation_record",
			TargetID: recordID.String(), Params: params,
			Reason: "resolve a material reconciliation mismatch in a test",
		})
		if err != nil {
			return err
		}
		actionID = a.ID.String()
		return nil
	}))
	approverCtx := f.breakGlassCtx(f.newOperator("approver-" + recordID.String()[:8]))
	require.NoError(t, f.d.InTx(approverCtx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.adminSvc.Approve(ctx, tx, actionID, "approved in a test")
		return err
	}))
	return actionID
}

// newDeposit inserts a deposit row in the given status.
func (f *fixture) newDeposit(t *testing.T, status string, expected money.Quantity, sig string) string {
	t.Helper()
	depositID := id.New[id.Any]().String()
	_, err := f.d.Exec(f.ctx, `INSERT INTO deposits
			(id, account_id, provider, provider_session_id, status, expected_asset_id, expected_quantity,
			 destination_wallet_id, tx_signature, idempotency_key, correlation_id, created_at)
		VALUES ($1,$2,'stripe',$3,$4,$5,$6::numeric,$7::uuid,$8,$9,$10,$11)`,
		depositID, f.account, "sess-"+depositID, status, f.usdc.ID, expected.String(),
		f.walletID, sig, "dep-"+depositID, "corr-"+f.suffix, f.clk.Now())
	require.NoError(t, err)
	return depositID
}

// submittingOrderWithIntent creates a second intent, plan, quote and order so
// a test can have two live orders on one account.
func (f *fixture) submittingOrderWithIntent(t *testing.T, res capital.Reservation, sig string, lastValid int64) (execution.Order, execution.Attempt) {
	t.Helper()
	hash := sha256.Sum256([]byte(sig))
	intentID := id.New[id.Any]().String()
	_, err := f.d.Exec(f.ctx, `INSERT INTO trade_intents (id, account_id, actor_type, actor_id, action, instrument_id, notional_usd_minor,
			constraints, requested_at, idempotency_key, correlation_id, mode, status, content_hash)
		VALUES ($1,$2,'USER',$3,'ACQUIRE_NOTIONAL',$4,5000,'{}',now(),$5,$6,'LIVE','PLANNED',$7)`,
		intentID, f.account, f.userID, f.instrument.ID, "idem-"+intentID, "corr-"+f.suffix, hash[:])
	require.NoError(t, err)
	planID := id.New[id.Any]().String()
	_, err = f.d.Exec(f.ctx, `INSERT INTO execution_plans (id, intent_id, version, planner_version, status, hard_constraints, plan_hash)
		VALUES ($1,$2,1,'itest','APPROVED','{}',$3)`, planID, intentID, hash[:])
	require.NoError(t, err)
	quoteID := id.New[id.Any]().String()
	_, err = f.d.Exec(f.ctx, `INSERT INTO quotes (id, intent_id, provider, instrument_id, venue_listing_id, side, input_asset_id, input_quantity,
			output_asset_id, expected_output, minimum_output, effective_price_mantissa, effective_price_scale,
			price_impact_bps, slippage_bps, received_at, expires_at, route_hash, raw_response_hash)
		VALUES ($1,$2,$3,$4,$5,'BUY',$6,49500000,$7,250000000,240000000,150,0,5,50,now(),now() + interval '30 seconds',$8,$8)`,
		quoteID, intentID, venueName, f.instrument.ID, f.listing.ID, f.usdc.ID, f.sol.ID, hash[:])
	require.NoError(t, err)

	saveIntent, savePlan, saveQuote := f.intentID, f.planID, f.quoteID
	f.intentID, f.planID, f.quoteID = intentID, planID, quoteID
	defer func() { f.intentID, f.planID, f.quoteID = saveIntent, savePlan, saveQuote }()
	return f.submittingOrder(res, money.QuantityFromInt64(orderInput), money.QuantityFromInt64(orderMinOut), sig, lastValid)
}
