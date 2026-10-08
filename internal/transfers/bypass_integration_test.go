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
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// The tests in this file go around the Go code on purpose and prove the
// database refuses the states internal/transfers depends on never existing.

type rawEntry struct {
	ref  ledger.AccountRef
	side ledger.Side
	qty  int64
}

// rawJournal inserts a journal bypassing internal/ledger (the econ test
// helper, with the reference chosen by the caller).
func (f *fixture) rawJournal(ctx context.Context, tx pgx.Tx, kind ledger.Kind, refType, refID string, entries []rawEntry) (ledger.TransactionID, error) {
	txID := ledger.NewTransactionID()
	sum := sha256.Sum256([]byte(txID.String()))
	if _, err := tx.Exec(ctx, `INSERT INTO journal_transactions (id, kind, idempotency_key, reference_type, reference_id,
		effective_at, posted_by_actor_type, posted_by_actor_id, content_hash) VALUES ($1,$2,$3,$4,$5,now(),'SYSTEM','raw',$6)`,
		txID, kind, "raw:"+txID.String(), refType, refID, sum[:]); err != nil {
		return ledger.TransactionID{}, err
	}
	for i, e := range entries {
		acct, err := f.ledger.EnsureAccount(ctx, tx, e.ref)
		if err != nil {
			return ledger.TransactionID{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO journal_entries (id, transaction_id, seq, ledger_account_id, asset_id, side, quantity)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`, uuid.New(), txID, i, acct.ID, e.ref.AssetID, e.side, e.qty); err != nil {
			return ledger.TransactionID{}, err
		}
	}
	return txID, nil
}

// rawPending writes an economic intent and a PENDING transfer row directly.
func (f *fixture) rawPending(ctx context.Context, tx pgx.Tx, from, to customer, qty int64, senderUser string) (TransferID, econ.IntentID, error) {
	q := money.QuantityFromInt64(qty)
	assetID := f.asset.ID
	in, _, err := f.deps.Intents.Submit(ctx, tx, econ.Intent{
		AccountID: from.account, Kind: econ.KindTransferValue, Mode: gates.ModeSandbox, Capability: gates.P2PTransfers,
		ActorType: security.ActorUser, ActorID: from.user.String(), AssetID: &assetID, Quantity: &q,
		IdempotencyKey: "raw-" + uuid.NewString(),
	})
	if err != nil {
		return TransferID{}, econ.IntentID{}, err
	}
	tid := NewTransferID()
	sum := sha256.Sum256([]byte(tid.String()))
	_, err = tx.Exec(ctx, `INSERT INTO internal_transfers (id, intent_id, sender_account_id, sender_user_id, recipient_account_id,
		asset_id, quantity, notional_usd_minor, mode, idempotency_key, request_hash, policy_version, policy_hash, status, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,0,'SANDBOX',$8,$9,'raw',$10,'PENDING',now())`,
		tid, in.ID, from.account, senderUser, to.account, f.asset.ID, qty, "raw-"+uuid.NewString(), sum[:], strings.Repeat("a", 64))
	return tid, in.ID, err
}

// TestIntegration_ATransferStatusChangeWithoutItsTransitionRowIsRefused:
// AU001, under the edge binding -- a bare status change, and one licensed by
// a row describing a different change, are both refused at COMMIT.
func TestIntegration_ATransferStatusChangeWithoutItsTransitionRowIsRefused(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customer(), f.customer()
	reject := `UPDATE internal_transfers SET status = 'REJECTED', rejection_code = 'RAW', reason_codes = '{RAW}', decided_at = now() WHERE id = $1`

	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		tid, _, err := f.rawPending(ctx, tx, alice, bob, 10, alice.user.String())
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, reject, tid)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "AU001", db.SQLState(err), "%v", err)

	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		tid, _, err := f.rawPending(ctx, tx, alice, bob, 10, alice.user.String())
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO internal_transfer_transitions (id, transfer_id, from_status, to_status, actor_type, actor_id, reason)
			VALUES ($1,$2,'PENDING','COMPLETED','SYSTEM','raw','a different change')`, uuid.New(), tid); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, reject, tid)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "AU001", db.SQLState(err), "%v", err)
}

// TestIntegration_AP2PJournalThatDoesNotBalancePerPartyIsRefused: SM002 --
// balanced per asset is not enough; each party's books must balance.
func TestIntegration_AP2PJournalThatDoesNotBalancePerPartyIsRefused(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customer(), f.customer()
	f.fund(alice.account, 1_000)
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.rawJournal(ctx, tx, ledger.KindP2PTransfer, refTypeTransfer, uuid.NewString(), []rawEntry{
			{ledger.CustomerAccount(alice.account, ledger.CodeWallet, f.asset.ID), ledger.Credit, 100},
			{ledger.CustomerAccount(bob.account, ledger.CodeWallet, f.asset.ID), ledger.Debit, 100},
		})
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "SM002", db.SQLState(err), "%v", err)
	assert.True(t, f.balance(bob.account, ledger.CodeWallet).IsZero())
}

// TestIntegration_TheTransferRegisterRefusesInvalidStates: born PENDING
// (PT003), initiated by the owner (PT001), identity and decisions frozen
// (PT002 / 42501), legal edges only (PT004), and a settled transfer must
// match its journal and effect (PT005).
func TestIntegration_TheTransferRegisterRefusesInvalidStates(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customer(), f.customer()
	f.fund(alice.account, 10_000_000)

	// PT001: someone else's user cannot initiate alice's transfer.
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := f.rawPending(ctx, tx, alice, bob, 10, bob.user.String())
		return err
	})
	assert.Equal(t, "PT001", db.SQLState(err), "%v", err)

	// PT003: a transfer cannot be born COMPLETED.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		tid, _, err := f.rawPending(ctx, tx, alice, bob, 10, alice.user.String())
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO internal_transfers (id, intent_id, sender_account_id, sender_user_id, recipient_account_id,
			asset_id, quantity, notional_usd_minor, mode, idempotency_key, request_hash, policy_version, policy_hash, status,
			decided_at, created_at)
			SELECT $2, intent_id, sender_account_id, sender_user_id, recipient_account_id, asset_id, quantity, 0, mode, 'born-done',
			       request_hash, policy_version, policy_hash, 'COMPLETED', now(), now() FROM internal_transfers WHERE id = $1`, tid, uuid.New())
		return err
	})
	assert.Equal(t, "PT003", db.SQLState(err), "%v", err)

	done, err := f.send(alice.p, f.req(alice, bob, 1_000))
	require.NoError(t, err)
	tr := done.Transfer

	// PT004: COMPLETED -> PENDING is not an edge, transition row or not.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO internal_transfer_transitions (id, transfer_id, from_status, to_status, actor_type, actor_id, reason)
			VALUES ($1,$2,'COMPLETED','PENDING','SYSTEM','raw','undo')`, uuid.New(), tr.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE internal_transfers SET status = 'PENDING' WHERE id = $1`, tr.ID)
		return err
	})
	assert.Equal(t, "PT004", db.SQLState(err), "%v", err)

	// PT002: a decision is written once. The application role may update the
	// decision columns, but only by leaving PENDING.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE internal_transfers SET evidence_ref = 'pg://forged' WHERE id = $1`, tr.ID)
		return err
	})
	assert.Equal(t, "PT002", db.SQLState(err), "%v", err)

	// Identity: the application role cannot even name the column, and the
	// migration owner is stopped by the trigger.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE internal_transfers SET quantity = quantity + 1 WHERE id = $1`, tr.ID)
		return err
	})
	assert.Equal(t, "42501", db.SQLState(err), "%v", err)
	owner, err := pgx.Connect(f.ctx, testMigrateURL)
	require.NoError(t, err)
	defer owner.Close(f.ctx)
	_, err = owner.Exec(f.ctx, `UPDATE internal_transfers SET quantity = quantity + 1 WHERE id = $1`, tr.ID)
	assert.Equal(t, "PT002", db.SQLState(err), "%v", err)
	_, err = owner.Exec(f.ctx, `DELETE FROM internal_transfer_transitions WHERE transfer_id = $1`, tr.ID)
	require.Error(t, err, "transition history is append-only")

	// PT005: a transfer cannot be completed by a journal that is not its
	// own -- here a P2P_TRANSFER of a different amount, with an applied effect
	// and a correct transition row, so the journal match is the only thing
	// wrong.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		tid, iid, err := f.rawPending(ctx, tx, alice, bob, 10, alice.user.String())
		if err != nil {
			return err
		}
		journal, err := f.rawJournal(ctx, tx, ledger.KindP2PTransfer, refTypeTransfer, tid.String(), []rawEntry{
			{ledger.CustomerAccount(alice.account, ledger.CodeTransferOut, f.asset.ID), ledger.Debit, 11},
			{ledger.CustomerAccount(alice.account, ledger.CodeWallet, f.asset.ID), ledger.Credit, 11},
			{ledger.CustomerAccount(bob.account, ledger.CodeWallet, f.asset.ID), ledger.Debit, 11},
			{ledger.CustomerAccount(bob.account, ledger.CodeTransferIn, f.asset.ID), ledger.Credit, 11},
		})
		if err != nil {
			return err
		}
		eff, _, err := f.deps.Effects.Begin(ctx, tx, econ.BeginRequest{Key: "p2p:" + tid.String(), Subsystem: econ.SubsystemTransfers,
			Kind: effectKind, IntentID: &iid, AccountID: alice.account.String(), Request: map[string]string{"raw": "1"},
			PostsLedger: true, Actor: serviceActor})
		if err != nil {
			return err
		}
		if _, err := f.deps.Effects.Apply(ctx, tx, eff.ID, econ.ApplyRequest{JournalTransactionID: &journal, Actor: serviceActor}); err != nil {
			return err
		}
		riskID := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO advanced_risk_decisions (id, intent_kind, stage, verdict, reason_codes, input_hash,
			policy_version, policy_hash, evaluator_version, decision_hash, input, decision, evaluated_at)
			VALUES ($1,'TRANSFER_VALUE','FINAL','ALLOW','{}','x','x','x','x','x','{}','{}',now())`, riskID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO internal_transfer_transitions (id, transfer_id, from_status, to_status, actor_type, actor_id, reason)
			VALUES ($1,$2,'PENDING','COMPLETED','SYSTEM','raw','forged')`, uuid.New(), tid); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE internal_transfers SET status = 'COMPLETED', journal_transaction_id = $2, effect_id = $3,
			risk_decision_id = $4, evidence_ref = 'pg://x', decided_at = now() WHERE id = $1`, tid, journal, eff.ID, riskID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "PT005", db.SQLState(err), "%v", err)
	f.requireReconciled()
}

// TestIntegration_SQLAndGoAgreeOnTransferTransitions is the parity test of
// CanTransition and cp_internal_transfer_transition_legal.
func TestIntegration_SQLAndGoAgreeOnTransferTransitions(t *testing.T) {
	requireEnv(t)
	for _, from := range AllStatuses() {
		for _, to := range AllStatuses() {
			var legal bool
			require.NoError(t, testDB.QueryRow(context.Background(), `SELECT cp_internal_transfer_transition_legal($1,$2)`, from, to).Scan(&legal))
			assert.Equal(t, CanTransition(from, to), legal, "%s -> %s", from, to)
		}
	}
}

// TestIntegration_ReconcileDetectsAJournalNoTransferExplains: a P2P_TRANSFER
// posted around the service balances per party and passes the ledger, but
// Reconcile reports it -- the journal is an orphan and the ledger's
// TRANSFER_OUT / TRANSFER_IN no longer match the register.
func TestIntegration_ReconcileDetectsAJournalNoTransferExplains(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.customer(), f.customer()
	f.fund(alice.account, 10_000_000)
	_, err := f.send(alice.p, f.req(alice, bob, 1_000))
	require.NoError(t, err)
	f.requireReconciled()

	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindP2PTransfer, IdempotencyKey: "itest-orphan:" + ref,
			Reference: ledger.FinancialEventReference{Type: refTypeTransfer, ID: ref}, EffectiveAt: f.clk.Now(),
			Entries: TransferEntries(alice.account, bob.account, f.asset.ID, money.QuantityFromInt64(500)),
		})
		return err
	}))
	rep, err := f.svc.Reconcile(f.ctx, testDB, f.asset.ID)
	require.NoError(t, err)
	assert.False(t, rep.OK())
	codes := map[string]bool{}
	for _, d := range rep.Discrepancies {
		codes[d.Code] = true
	}
	assert.True(t, codes[DiscOrphanJournal], "%+v", rep.Discrepancies)
	assert.True(t, codes[DiscRegisterLedger], "%+v", rep.Discrepancies)
	assert.True(t, codes[DiscClearingImbalance], "%+v", rep.Discrepancies)
	assert.False(t, codes[DiscWalletNotConserved], "the orphan still moved ownership only")
}
