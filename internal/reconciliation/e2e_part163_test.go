//go:build integration

package reconciliation

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// PART 163: internal expects 100 USDC, external observes 99.99 USDC.
const (
	internalExpected int64 = 100_000_000 // 100.00 USDC
	externalObserved int64 = 99_990_000  // 99.99 USDC
	shortfall        int64 = 10_000      // 0.01 USDC
)

// TestIntegration_Part163_ReconciliationE2E is the PART 163 end-to-end flow:
//
//   - a mismatch is created,
//   - unsafe new action is blocked according to threshold policy,
//   - an operator can inspect the evidence,
//   - resolution records a reason,
//   - a compensating journal posts where necessary,
//   - the mismatch resolves,
//   - and there is audit proof.
func TestIntegration_Part163_ReconciliationE2E(t *testing.T) {
	d := openTestDB(t)
	f := newFixture(t, d)

	// Internal accounting truth: 100 USDC.
	f.fund(f.usdc, money.QuantityFromInt64(internalExpected), 10_000)

	// External truth: the chain holds 99.99 USDC. Both observers agree.
	f.sim.SetBalance(chain.BalanceObservation{
		Owner: f.walletAddr, Mint: f.usdc.MintAddress, TokenAccount: "ata-usdc-" + f.suffix,
		Amount: money.QuantityFromInt64(externalObserved), Decimals: 6, DecimalsKnown: true,
	})

	// The threshold policy decides materiality: a one-cent difference is
	// material under a one-cent threshold (PART 163 "according to
	// threshold/policy").
	policy := DefaultPolicy()
	policy.MaterialThresholdUSDMinor = 1
	metrics := NoopMetrics()
	engine := f.newEngine(Config{Policy: policy, Metrics: metrics})

	// --- a mismatch is created -------------------------------------------
	recs, err := engine.RunFull(f.ctx, f.account)
	require.NoError(t, err)
	rec := findRecord(t, recs, KindWalletBalance)
	assert.Equal(t, StatusMismatch, rec.Status)
	assert.True(t, rec.Material, "0.01 USDC is material under a 0.01 threshold")
	assert.True(t, rec.BlocksNewRisk)
	assert.Equal(t, ModeFull, rec.Mode)
	assert.Equal(t, f.account, rec.AccountID)
	assert.Equal(t, f.usdc.ID, rec.AssetID)
	assert.Positive(t, metrics.SEV1Count(), "a material mismatch pages somebody")

	// The difference is recorded exactly, as a decimal string: no float ever
	// touches a financial value.
	diff := decodeDoc(t, rec.Difference)
	assert.Equal(t, "-10000", diff["quantity"])
	assert.Equal(t, "-0.010000", diff["decimal"])
	assert.EqualValues(t, -1, diff["usd_minor"])

	// The observation was persisted as evidence in its own right.
	obs, err := f.records.LatestBalanceObservations(f.ctx, f.d, f.walletID, f.usdc.ID)
	require.NoError(t, err)
	require.NotEmpty(t, obs)
	for _, o := range obs {
		assert.True(t, o.Quantity.Equal(money.QuantityFromInt64(externalObserved)))
	}

	// --- unsafe new action is blocked ------------------------------------
	blocks := engine.Blocks()
	blocked, blocking, err := blocks.BlocksNewRisk(f.ctx, f.d, f.account)
	require.NoError(t, err)
	assert.True(t, blocked)
	require.Len(t, blocking, 1)
	assert.Equal(t, rec.ID, blocking[0].ID)

	err = blocks.RequireNoBlock(f.ctx, f.d, f.account)
	require.Error(t, err)
	assert.Equal(t, errs.CodeReconciliationRequired, errs.CodeOf(err), "a new intent is refused with RECONCILIATION_REQUIRED")

	count, err := blocks.CountBlocking(f.ctx, f.d, f.account)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "the risk kernel reads this as UnresolvedMaterialMismatches")

	oldest, err := blocks.OldestUnresolvedMaterial(f.ctx, f.d)
	require.NoError(t, err)
	require.NotNil(t, oldest)

	// --- an operator can inspect the evidence -----------------------------
	opID := f.newOperator("resolver")
	approverID := f.newOperator("approver")
	opCtx := f.operatorCtx(opID, security.RoleFinance)

	inspected, err := f.records.Get(opCtx, f.d, rec.ID)
	require.NoError(t, err)
	expectedDoc := decodeDoc(t, inspected.Expected)
	observedDoc := decodeDoc(t, inspected.Observed)
	assert.Equal(t, "ledger.WALLET", expectedDoc["source"])
	assert.Equal(t, "100000000", expectedDoc["quantity"])
	assert.Equal(t, "99990000", observedDoc["quantity"])
	assert.Equal(t, string(chain.Agreed), observedDoc["state"], "both observers agreed on what the chain holds")

	var investigating Record
	require.NoError(t, d.InTx(opCtx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		investigating, err = engine.Investigate(ctx, tx, rec.ID, "operator is inspecting the 0.01 USDC shortfall")
		return err
	}))
	assert.Equal(t, StatusInvestigating, investigating.Status)

	// --- a material resolution needs dual control -------------------------
	repairIn := BalanceRepairInputs{
		RecordID: rec.ID, AccountID: f.account, AssetID: f.usdc.ID,
		Difference: money.QuantityFromInt64(-shortfall), ReasonCode: "CHAIN_SHORTFALL",
		Description: "chain holds 0.01 USDC less than the ledger", EffectiveAt: f.clk.Now(),
		CorrelationID: rec.CorrelationID,
	}
	repair, err := BalanceRepair(repairIn)
	require.NoError(t, err)
	repair, err = repair.WithPositionRepair(repairIn, "reconciliation:part163")
	require.NoError(t, err)

	resolution := ManualResolution{
		Operator:    Actor{Type: security.ActorOperator, ID: opID},
		Reason:      "chain credited 0.01 USDC less than the ledger recorded; compensating adjustment posted",
		EvidenceRef: "incident://part163/" + f.suffix,
		Compensation: func() *Repair {
			r := repair
			return &r
		}(),
	}

	err = d.InTx(opCtx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := engine.ResolveManual(ctx, tx, rec.ID, resolution)
		return err
	})
	require.Error(t, err, "a material record cannot be resolved without an approval")
	assert.Equal(t, errs.CodeStepUpRequired, errs.CodeOf(err))

	// Propose and approve the admin action (two distinct humans).
	params, err := json.Marshal(map[string]any{
		"record_id": rec.ID.String(), "difference": "-10000", "reason_code": "CHAIN_SHORTFALL",
	})
	require.NoError(t, err)
	var action admin.Action
	require.NoError(t, d.InTx(opCtx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		action, err = f.adminSvc.Propose(ctx, tx, admin.Proposal{
			Kind: admin.KindReconciliationResolveMaterial, TargetType: "reconciliation_record",
			TargetID: rec.ID.String(), Params: params,
			Reason: "resolve the 0.01 USDC chain shortfall with a compensating adjustment",
		})
		return err
	}))
	approverCtx := f.breakGlassCtx(approverID)
	require.NoError(t, d.InTx(approverCtx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.adminSvc.Approve(ctx, tx, action.ID.String(), "evidence reviewed; adjustment agreed")
		return err
	}))

	// --- resolution records a reason and posts the compensating journal ---
	resolution.ApprovalID = action.ID.String()
	var resolved Record
	require.NoError(t, d.InTx(opCtx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		resolved, err = engine.ResolveManual(ctx, tx, rec.ID, resolution)
		return err
	}))

	// --- the mismatch resolves -------------------------------------------
	assert.Equal(t, StatusResolvedManual, resolved.Status)
	assert.Equal(t, security.ActorOperator, resolved.ResolvedByActorType)
	assert.Equal(t, opID, resolved.ResolvedByActorID)
	assert.Equal(t, resolution.Reason, resolved.ResolutionReason)
	assert.Equal(t, resolution.EvidenceRef, resolved.ResolutionEvidenceRef)
	assert.Equal(t, action.ID.String(), resolved.ApprovalID)
	require.NotEmpty(t, resolved.CompensatingJournalTxID)
	assert.False(t, resolved.BlocksNewRisk, "the block is lifted with the resolution")
	require.NotNil(t, resolved.ResolvedAt)

	// The compensating journal transaction is a new RECONCILIATION_ADJUSTMENT
	// referencing the record; no balance was edited.
	assert.Equal(t, 1, f.countRows(
		`SELECT count(*) FROM journal_transactions WHERE kind = 'RECONCILIATION_ADJUSTMENT' AND reference_id = $1`,
		rec.ID.String()))
	assert.Equal(t, 1, f.countRows(
		`SELECT count(*) FROM journal_transactions WHERE id = $1::uuid AND reason_code = 'CHAIN_SHORTFALL'`,
		resolved.CompensatingJournalTxID))

	// Internal truth now equals external truth, exactly.
	assert.True(t, f.balance(ledger.CodeWallet, f.usdc.ID).Equal(money.QuantityFromInt64(externalObserved)))
	assert.True(t, f.balance(ledger.CodeReconciliationAdjustment, f.usdc.ID).Equal(money.QuantityFromInt64(-shortfall)),
		"the adjustment account carries the correction, signed")

	// Positions followed the ledger through a disposal, not an overwrite.
	drifts, err := f.lots.VerifyAgainstLedger(f.ctx, f.d, f.account)
	require.NoError(t, err)
	assert.Empty(t, drifts)
	assert.Equal(t, 1, f.countRows(
		`SELECT count(*) FROM lot_dispositions WHERE account_id = $1 AND asset_id = $2`, f.account, f.usdc.ID))

	// --- the block is lifted ---------------------------------------------
	require.NoError(t, blocks.RequireNoBlock(f.ctx, f.d, f.account))
	count, err = blocks.CountBlocking(f.ctx, f.d, f.account)
	require.NoError(t, err)
	assert.Zero(t, count)

	// --- audit proof ------------------------------------------------------
	trs := f.transitions(rec.ID)
	statuses := make([]Status, 0, len(trs))
	for _, tr := range trs {
		statuses = append(statuses, tr.To)
	}
	assert.Equal(t, []Status{StatusOpen, StatusMismatch, StatusInvestigating, StatusResolvedManual}, statuses)
	assert.Equal(t, security.ActorOperator, trs[len(trs)-1].ActorType)
	assert.Equal(t, opID, trs[len(trs)-1].ActorID)
	assert.Equal(t, resolution.EvidenceRef, trs[len(trs)-1].EvidenceRef)

	actions := f.auditActions(rec.ID)
	assert.Contains(t, actions, AuditRecordOpened)
	assert.Contains(t, actions, AuditRepairPosted)
	assert.Contains(t, actions, AuditRecordTransitioned)

	repairPayload := f.auditPayload(rec.ID, AuditRepairPosted)
	assert.Equal(t, resolved.CompensatingJournalTxID, repairPayload["journal_transaction_id"])
	assert.Equal(t, "CHAIN_SHORTFALL", repairPayload["reason_code"])

	f.requireAuditChainIntact()

	// Re-running the comparison now finds no difference. The resolved record
	// is returned untouched — it is history — and no second record is opened
	// for the same scope, however often the hourly FULL pass runs.
	before := f.countRows(`SELECT count(*) FROM reconciliation_records WHERE scope_type = $1 AND scope_id = $2`,
		rec.ScopeType, rec.ScopeID)
	for i := 0; i < 3; i++ {
		after, err := engine.RunFull(f.ctx, f.account)
		require.NoError(t, err)
		later := findRecord(t, after, KindWalletBalance)
		assert.Equal(t, rec.ID, later.ID)
		assert.Equal(t, StatusResolvedManual, later.Status, "a resolved record is never reopened or rewritten")
		assert.False(t, later.BlocksNewRisk)
	}
	assert.Equal(t, before, f.countRows(
		`SELECT count(*) FROM reconciliation_records WHERE scope_type = $1 AND scope_id = $2`, rec.ScopeType, rec.ScopeID))
	assert.Equal(t, 1, f.countRows(
		`SELECT count(*) FROM journal_transactions WHERE kind = 'RECONCILIATION_ADJUSTMENT' AND reference_id = $1`,
		rec.ID.String()), "no second compensating posting")
	require.NoError(t, blocks.RequireNoBlock(f.ctx, f.d, f.account))
}

func findRecord(t *testing.T, recs []Record, kind Kind) Record {
	t.Helper()
	for _, r := range recs {
		if r.Kind == kind {
			return r
		}
	}
	t.Fatalf("no %s record among %d records", kind, len(recs))
	return Record{}
}

func decodeDoc(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	out := map[string]any{}
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}
