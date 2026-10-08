//go:build integration

package stableasset

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
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// TestIntegration_AMintIsPostedOnlyAfterFinalityAndReconciles walks the whole
// issuance workflow: the reserve payment moves to the issuer facility and the
// stable units to the customer in one STABLE_ISSUANCE that exists only once
// the devnet mint is FINALIZED; books, wallets and chain agree; the effect,
// the intent, the risk decision, the evidence and the transition history are
// all where they belong.
func TestIntegration_AMintIsPostedOnlyAfterFinalityAndReconciles(t *testing.T) {
	f := newFixture(t)
	a := f.register()
	c := f.customer()
	f.fund(c, f.cash, 5_000_000)

	rec := f.mustMint(c, a, 1_000_000)

	assert.Equal(t, "4000000", f.wallet(c, f.cash), "the reserve payment left the customer")
	assert.Equal(t, "1000000", f.wallet(c, a.AssetID))
	assert.Equal(t, "1000000", f.balance(a.account(ledger.CodeStableReserve, f.cash)), "and reached the issuer's reserve")
	assert.Equal(t, "1000000", f.balance(a.account(ledger.CodeStableOutstanding, a.AssetID)))
	assert.Equal(t, uint64(1_000_000), f.chainBalance(c, a))
	assert.Equal(t, uint64(1_000_000), f.chainSupply(a))

	// The posting is STABLE_ISSUANCE, on the request's own key, after finality.
	var kind string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT kind FROM journal_transactions WHERE id = $1`, rec.JournalTransactionID).Scan(&kind))
	assert.Equal(t, string(ledger.KindStableIssuance), kind)
	st, err := f.host.Status(f.ctx, a.Chain, rec.TxID)
	require.NoError(t, err)
	assert.Equal(t, devnet.TxFinalized, st.State)
	_, err = InspectMintTransaction(rec.Plan, rec.SignedTx)
	require.NoError(t, err, "the stored transaction is exactly its plan")

	effect, err := f.effects.Get(f.ctx, testDB, rec.EffectID)
	require.NoError(t, err)
	assert.Equal(t, econ.EffectApplied, effect.Status)
	assert.Equal(t, rec.JournalTransactionID.String(), effect.JournalTransactionID.String())
	assert.NotEmpty(t, effect.EvidenceRef, "the issuance evidence is archived")
	in, err := f.intents.Get(f.ctx, testDB, rec.IntentID)
	require.NoError(t, err)
	assert.Equal(t, econ.StatusCompleted, in.Status)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM advanced_risk_decisions WHERE intent_id = $1 AND verdict = 'ALLOW'`, rec.IntentID))
	assert.Equal(t, 1, f.submissions(rec.TxID))
	var path []string
	rows, err := testDB.Query(f.ctx, `SELECT from_status || '>' || to_status FROM stable_mint_request_transitions
		WHERE request_id = $1 ORDER BY occurred_at, to_status`, rec.ID)
	require.NoError(t, err)
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		path = append(path, s)
	}
	assert.ElementsMatch(t, []string{"NONE>SUBMITTING", "SUBMITTING>SUBMITTED", "SUBMITTED>ISSUED"}, path)

	rl := f.reserveLedger(a)
	assert.True(t, rl.PendingMint.IsZero())
	assert.Equal(t, money.OneHundredPercent, rl.CoverageBPS)
	r := f.reconcile(a)
	assert.True(t, r.Matched(), "%+v", r)
	assert.NotEmpty(t, r.RecordID, "a periodic match is recorded")
}

// TestIntegration_ReserveCannotBeDoubleCounted: a customer's reserve funds
// count once -- as the receivable of their own pending mint, not also as the
// issuer's reserve, and not for a second mint; after the posting they count
// only as issuer reserve.
func TestIntegration_ReserveCannotBeDoubleCounted(t *testing.T) {
	f := newFixture(t)
	a := f.register()
	c := f.customer()
	f.fund(c, f.cash, 1_500_000)

	pending, err := f.svc.Mint(c.ctx, f.mintRequest(c, a, 1_000_000, "pending-"+uuid.NewString(), gates.ModeDevnet))
	require.NoError(t, err)
	require.Equal(t, MintSubmitted, pending.Request.Status, "DEVNET mode leaves the request to the chain's own producer")

	rl := f.reserveLedger(a)
	assert.Equal(t, "1000000", rl.PendingMint.String())
	assert.Equal(t, "1000000", sumQuantity(rl.Receivable, f.cash).String())
	assert.True(t, rl.cashHolding(f.cash).IsZero(), "the payment is not issuer reserve until the mint posts")
	assert.True(t, rl.IssuedSupply.IsZero())

	_, err = f.mint(c, a, 1_000_000)
	require.Error(t, err, "the same funds cannot pay for a second mint")
	assert.Equal(t, errs.CodeRiskRejected, errs.CodeOf(err))
	e, _ := errs.As(err)
	assert.Contains(t, e.Fields["reason_codes"], "RISK_INSUFFICIENT_BALANCE", "the risk kernel sees the reserved funds as spoken for")
	// And underneath the kernel, the reservation itself refuses them.
	err = testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.capital.Reserve(ctx, tx, capital.ReserveRequest{AccountID: c.id.String(), AssetID: f.cash,
			Quantity: money.QuantityFromInt64(1_000_000), ActorType: c.actor.Type, ActorID: c.actor.ID,
			IdempotencyKey: "double-" + uuid.NewString(), TTL: time.Hour, Reason: "double count probe"})
		return err
	})
	assert.Equal(t, errs.CodeInsufficientBuyingPower, errs.CodeOf(err))

	_, err = f.host.ProduceBlocks(f.ctx, a.Chain, 1+itDepth)
	require.NoError(t, err)
	rep, err := f.svc.ObservePending(f.ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Issued)

	rl = f.reserveLedger(a)
	assert.Equal(t, "1000000", rl.cashHolding(f.cash).String(), "counted exactly once, now as reserve")
	assert.True(t, rl.PendingMint.IsZero())
	assert.Empty(t, rl.Receivable)
	assert.Equal(t, 0, rl.EligibleReserve.Cmp(rl.SupplyValue), "exactly covered: nothing counted twice")
	assert.Equal(t, money.OneHundredPercent, rl.CoverageBPS)
}

// TestIntegration_ADuplicateMintRequestIsIdempotent: replaying an idempotency
// key -- sequentially or concurrently -- returns the first request and
// submits, posts and issues nothing again; reusing it for a different
// request is refused.
func TestIntegration_ADuplicateMintRequestIsIdempotent(t *testing.T) {
	f := newFixture(t)
	a := f.register()
	c := f.customer()
	f.fund(c, f.cash, 10_000_000)

	key := "dup-" + uuid.NewString()
	first, err := f.svc.Mint(c.ctx, f.mintRequest(c, a, 700_000, key, gates.ModeSandbox))
	require.NoError(t, err)
	require.Equal(t, MintIssued, first.Request.Status)
	again, err := f.svc.Mint(c.ctx, f.mintRequest(c, a, 700_000, key, gates.ModeSandbox))
	require.NoError(t, err)
	assert.True(t, again.Replay)
	assert.Equal(t, first.Request.ID, again.Request.ID)
	assert.Equal(t, 1, f.submissions(first.Request.TxID))
	assert.Equal(t, uint64(700_000), f.chainSupply(a))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM journal_transactions WHERE reference_id = $1`, first.Request.ID.String()))

	_, err = f.svc.Mint(c.ctx, f.mintRequest(c, a, 800_000, key, gates.ModeSandbox))
	require.Error(t, err)
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, errs.CodeOf(err))

	// Eight concurrent deliveries of one new request.
	key = "dup-concurrent-" + uuid.NewString()
	var wg sync.WaitGroup
	results := make(chan MintResult, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := f.svc.Mint(c.ctx, f.mintRequest(c, a, 300_000, key, gates.ModeSandbox))
			assert.NoError(t, err)
			results <- res
		}()
	}
	wg.Wait()
	close(results)
	ids := map[MintRequestID]bool{}
	fresh := 0
	for r := range results {
		ids[r.Request.ID] = true
		if !r.Replay {
			fresh++
		}
	}
	assert.Len(t, ids, 1, "every delivery resolves to one request")
	assert.Equal(t, 1, fresh, "exactly one delivery did the work")
	var rid MintRequestID
	for k := range ids {
		rid = k
	}
	rec, err := f.svc.GetMint(f.ctx, testDB, rid)
	require.NoError(t, err)
	assert.Equal(t, MintIssued, rec.Status)
	assert.Equal(t, 1, f.submissions(rec.TxID))
	assert.Equal(t, uint64(1_000_000), f.chainSupply(a))
	assert.True(t, f.reconcile(a).Matched())
}

// TestIntegration_SupplyCannotExceedThePolicyCap: admission refuses a mint
// that would cross the cap (counting pending mints), and the database
// refuses an issuance journal that crosses it even when the service is
// bypassed (SQLSTATE SV003).
func TestIntegration_SupplyCannotExceedThePolicyCap(t *testing.T) {
	f := newFixture(t)
	a := f.register(func(d *Definition) { d.IssuanceCap = money.QuantityFromInt64(1_500_000) })
	c := f.customer()
	f.fund(c, f.cash, 10_000_000)

	f.mustMint(c, a, 1_000_000)
	_, err := f.mint(c, a, 600_000)
	require.Error(t, err)
	assert.Equal(t, errs.CodeRiskMaxPosition, errs.CodeOf(err))
	e, _ := errs.As(err)
	assert.Equal(t, RefusalCapExceeded, e.Fields["reason"])
	f.mustMint(c, a, 500_000) // exactly at the cap
	assert.Equal(t, uint64(1_500_000), f.chainSupply(a))

	// Around the service: a fully backed issuance of one more unit.
	err = f.postIssuance(a, c, 1, 1)
	require.Error(t, err)
	assert.Equal(t, "SV003", db.SQLState(err), "%v", err)
}

// postIssuance posts a STABLE_ISSUANCE directly through the ledger, around
// the stable asset service, so only the database stands in its way.
func (f *fixture) postIssuance(a PlatformStableAsset, c customer, q, r int64) error {
	return testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		rec := MintRecord{ID: id.New[mintRequestKind](), AccountID: c.id, Quantity: money.QuantityFromInt64(q),
			ReserveAssetID: f.cash, ReserveQuantity: money.QuantityFromInt64(r)}
		p := issuancePosting(a, rec, f.clk.Now())
		p.IdempotencyKey = "bypass:" + ref
		_, err := f.ledger.Post(ctx, tx, p)
		return err
	})
}

// postRedemption posts a STABLE_REDEMPTION directly through the ledger.
func (f *fixture) postRedemption(a PlatformStableAsset, c customer, q, payout int64) error {
	return testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		rec := BurnRecord{ID: id.New[burnRequestKind](), AccountID: c.id, Quantity: money.QuantityFromInt64(q),
			PayoutAssetID: f.cash, PayoutQuantity: money.QuantityFromInt64(payout)}
		p := redemptionPosting(a, rec, f.clk.Now())
		p.IdempotencyKey = "bypass:" + uuid.NewString()
		_, err := f.ledger.Post(ctx, tx, p)
		return err
	})
}

// TestIntegration_TheDatabaseRefusesUnderReservedIssuanceAndOverpayingRedemption:
// with the service bypassed, an issuance carrying less than its full backing
// (SV004) and a redemption paying more than the peg value of what it retires
// (SV005) cannot commit.
func TestIntegration_TheDatabaseRefusesUnderReservedIssuanceAndOverpayingRedemption(t *testing.T) {
	f := newFixture(t)
	a := f.register()
	c := f.customer()
	f.fund(c, f.cash, 10_000_000)
	f.mustMint(c, a, 1_000_000)

	err := f.postIssuance(a, c, 1_000, 999)
	require.Error(t, err)
	assert.Equal(t, "SV004", db.SQLState(err), "%v", err)
	require.NoError(t, f.postIssuance(a, c, 1_000, 1_000), "a fully backed issuance is fine")

	err = f.postRedemption(a, c, 1_000, 1_001)
	require.Error(t, err)
	assert.Equal(t, "SV005", db.SQLState(err), "%v", err)
	require.NoError(t, f.postRedemption(a, c, 1_000, 1_000))
}

// TestIntegration_AFailedSettlementCannotPostASuccessfulIssuance: a mint that
// fails on chain releases the reserve payment and posts nothing; nothing --
// not a second settlement, not a raw update -- can turn it into an issuance.
func TestIntegration_AFailedSettlementCannotPostASuccessfulIssuance(t *testing.T) {
	f := newFixture(t)
	a := f.register()
	c := f.customer()
	f.fund(c, f.cash, 2_000_000)
	require.NoError(t, f.host.SetMintFrozen(f.ctx, a.Chain, a.Mint, true))

	res, err := f.mint(c, a, 1_000_000)
	require.NoError(t, err)
	rec := res.Request
	require.Equal(t, MintFailed, rec.Status)
	assert.Equal(t, "CHAIN_FAILED:"+string(devnet.CodeMintFrozen), rec.FailureCode)
	assert.Nil(t, rec.JournalTransactionID)
	assert.Equal(t, "2000000", f.wallet(c, f.cash))
	assert.Equal(t, "0", f.wallet(c, a.AssetID))
	avail, err := f.capital.Availability(f.ctx, testDB, c.id.String(), f.cash, f.clk.Now())
	require.NoError(t, err)
	assert.Equal(t, "2000000", avail.Available.String(), "the reservation was released")
	effect, err := f.effects.Get(f.ctx, testDB, rec.EffectID)
	require.NoError(t, err)
	assert.Equal(t, econ.EffectFailed, effect.Status)
	assert.Zero(t, f.count(`SELECT count(*) FROM journal_transactions WHERE kind = 'STABLE_ISSUANCE' AND reference_id = $1`, rec.ID.String()))

	// Settling again is a no-op.
	_, err = f.svc.settleMintTx(f.ctx, rec.ID)
	require.NoError(t, err)
	again, err := f.svc.GetMint(f.ctx, testDB, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, MintFailed, again.Status)

	// Around Go: a licensing transition row and a bare update are refused.
	err = testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO stable_mint_request_transitions (id, request_id, from_status, to_status, actor_type, actor_id, reason)
			VALUES ($1,$2,'FAILED','ISSUED','SYSTEM','x','bypass')`, uuid.New(), rec.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE stable_mint_requests SET status = 'ISSUED', failure_code = NULL WHERE id = $1`, rec.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "SV001", db.SQLState(err), "%v", err)

	require.NoError(t, f.host.SetMintFrozen(f.ctx, a.Chain, a.Mint, false))
	f.mustMint(c, a, 1_000_000)
}

// TestIntegration_AnUnknownSubmissionIsReconciledNotRetried: a submission
// that timed out but landed is recorded UNKNOWN, blocks the account's new
// mints, is never resubmitted, and is posted by ReconcileUnknown once the
// chain says FINALIZED.
func TestIntegration_AnUnknownSubmissionIsReconciledNotRetried(t *testing.T) {
	f := newFixture(t)
	a := f.register()
	c := f.customer()
	f.fund(c, f.cash, 5_000_000)
	require.NoError(t, f.host.TimeoutButLand(f.ctx, a.Chain))

	res, err := f.mint(c, a, 1_000_000)
	require.Error(t, err)
	assert.Equal(t, errs.CodeSubmissionStateUnknown, errs.CodeOf(err))
	rec := res.Request
	require.Equal(t, MintSubmissionUnknown, rec.Status)
	effect, err := f.effects.Get(f.ctx, testDB, rec.EffectID)
	require.NoError(t, err)
	assert.Equal(t, econ.EffectUnknown, effect.Status)

	_, err = f.mint(c, a, 1_000)
	require.Error(t, err, "an unknown settlement blocks new mints of the account")
	assert.Equal(t, errs.CodeRiskRejected, errs.CodeOf(err))

	rep, err := f.svc.ReconcileUnknown(f.ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Waiting, "still in the mempool: wait")

	_, err = f.host.ProduceBlocks(f.ctx, a.Chain, 1+itDepth)
	require.NoError(t, err)
	rep, err = f.svc.ReconcileUnknown(f.ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Issued)

	rec, err = f.svc.GetMint(f.ctx, testDB, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, MintIssued, rec.Status)
	in, err := f.intents.Get(f.ctx, testDB, rec.IntentID)
	require.NoError(t, err)
	assert.Equal(t, econ.StatusReconciled, in.Status)
	assert.Equal(t, 1, f.submissions(rec.TxID), "submitted exactly once, never again")
	assert.Equal(t, uint64(1_000_000), f.chainSupply(a))
	assert.True(t, f.reconcile(a).Matched())
}

// TestIntegration_ADroppedSubmissionExpiresWithoutResubmission: a submission
// the chain accepted and dropped is never resubmitted; once its blockhash is
// a window below finality it fails and the reserve payment is released.
func TestIntegration_ADroppedSubmissionExpiresWithoutResubmission(t *testing.T) {
	f := newFixture(t)
	a := f.register()
	c := f.customer()
	f.fund(c, f.cash, 5_000_000)
	require.NoError(t, f.host.DropNext(f.ctx, a.Chain, 1))

	res, err := f.mint(c, a, 1_000_000)
	require.NoError(t, err)
	rec := res.Request
	require.Equal(t, MintFailed, rec.Status)
	assert.Equal(t, "SUBMISSION_EXPIRED", rec.FailureCode)
	assert.Equal(t, 1, f.submissions(rec.TxID), "the drop was observed, not retried")
	assert.Zero(t, f.chainSupply(a))
	assert.Equal(t, "5000000", f.wallet(c, f.cash))
	avail, err := f.capital.Availability(f.ctx, testDB, c.id.String(), f.cash, f.clk.Now())
	require.NoError(t, err)
	assert.Equal(t, "5000000", avail.Available.String())
}

// TestIntegration_ATimedOutSubmissionThatNeverLandedFailsAfterExpiry: the
// other half of the unknown case: timed out and not accepted. Reconciliation
// waits while the transaction could still land and fails it only after.
func TestIntegration_ATimedOutSubmissionThatNeverLandedFailsAfterExpiry(t *testing.T) {
	f := newFixture(t)
	a := f.register()
	c := f.customer()
	f.fund(c, f.cash, 5_000_000)
	require.NoError(t, f.host.TimeoutAndDrop(f.ctx, a.Chain))

	res, err := f.mint(c, a, 1_000_000)
	require.Error(t, err)
	rec := res.Request
	require.Equal(t, MintSubmissionUnknown, rec.Status)
	rep, err := f.svc.ReconcileUnknown(f.ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Waiting, "NOT_FOUND alone proves nothing while the blockhash can still land")

	_, err = f.host.ProduceBlocks(f.ctx, a.Chain, itWindow+itDepth+1)
	require.NoError(t, err)
	rep, err = f.svc.ReconcileUnknown(f.ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Failed)
	rec, err = f.svc.GetMint(f.ctx, testDB, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, MintFailed, rec.Status)
	assert.Equal(t, "SUBMISSION_EXPIRED", rec.FailureCode)
	assert.Equal(t, 1, f.submissions(rec.TxID))
	avail, err := f.capital.Availability(f.ctx, testDB, c.id.String(), f.cash, f.clk.Now())
	require.NoError(t, err)
	assert.Equal(t, "5000000", avail.Available.String())
}
