package devnet

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

func TestFault_DropNext(t *testing.T) {
	w := newWorld(t)
	w.c.DropNext(2)

	// A rejected submission does not consume a drop.
	_, err := w.c.Submit([]byte("garbage"))
	require.Error(t, err)

	a := signed(t, w.msg(memo("a")), w.payer)
	b := signed(t, w.msg(memo("b")), w.payer)
	c := signed(t, w.msg(memo("c")), w.payer)
	idA, idB, idC := w.submit(a), w.submit(b), w.submit(c)
	require.Equal(t, TxNotFound, w.c.Status(idA).State, "accepted, but dropped before the mempool")
	require.Equal(t, TxNotFound, w.c.Status(idB).State)
	require.Equal(t, TxPending, w.c.Status(idC).State)

	// An idempotent resubmission does not consume a drop either.
	w.c.DropNext(1)
	require.Equal(t, idC, w.submit(c))
	blk := w.c.ProduceBlock()
	require.Equal(t, []TxID{idC}, blk.TxIDs)
	require.Equal(t, TxNotFound, w.c.Status(idA).State)

	// A dropped transaction was never seen, so resending it works (the one
	// armed drop takes the first resend).
	require.Equal(t, idA, w.submit(a))
	require.Equal(t, TxNotFound, w.c.Status(idA).State)
	require.Equal(t, idA, w.submit(a))
	w.c.ProduceBlock()
	require.Equal(t, TxConfirmed, w.c.Status(idA).State)
	require.NoError(t, w.c.audit())
}

func TestFault_TimeoutButLand_IsObservableThroughStatus(t *testing.T) {
	w := newWorld(t)
	w.c.TimeoutButLand()
	raw := signed(t, w.msg(SystemTransfer{From: w.alice.PublicKey(), To: w.bob.PublicKey(), Lamports: 9}), w.payer, w.alice)

	id, err := w.c.Submit(raw)
	requireReason(t, err, errs.CodeSubmissionStateUnknown, CodeSubmissionTimeout)
	tx, decodeErr := Decode(raw)
	require.NoError(t, decodeErr)
	wantID, idErr := tx.ID()
	require.NoError(t, idErr)
	require.Equal(t, wantID, id, "the TxID comes back with the timeout, as a client knows its signature")
	e, ok := errs.As(err)
	require.True(t, ok)
	require.Equal(t, string(id), e.Fields["tx_id"])

	// The caller cannot tell from Submit; Status tells it the truth.
	require.Equal(t, TxPending, w.c.Status(id).State)
	w.c.ProduceBlock()
	require.Equal(t, TxConfirmed, w.c.Status(id).State)
	require.Equal(t, uint64(testFunds+9), w.nativeOf(w.bob))

	// The careful client resubmits; that is idempotent and the fault is gone.
	again, err := w.c.Submit(raw)
	require.NoError(t, err)
	require.Equal(t, id, again)
	w.c.ProduceBlock()
	require.Equal(t, uint64(testFunds+9), w.nativeOf(w.bob), "landed exactly once")
}

func TestFault_TimeoutAndDrop_CanNeverLandOnceTheBlockhashExpires(t *testing.T) {
	w := newWorld(t)
	w.c.TimeoutAndDrop()
	m := w.msg(memo("lost"))
	raw := signed(t, m, w.payer)

	id, err := w.c.Submit(raw)
	requireReason(t, err, errs.CodeSubmissionStateUnknown, CodeSubmissionTimeout)
	require.NotEmpty(t, id)
	require.Equal(t, TxNotFound, w.c.Status(id).State)

	// NOT_FOUND alone proves nothing while the blockhash is valid...
	w.c.ProduceBlocks(testWindow - 1)
	require.Equal(t, TxNotFound, w.c.Status(id).State)
	require.True(t, w.c.IsBlockhashValid(m.RecentBlockhash))
	// ...once it has expired, the transaction can never land.
	w.c.ProduceBlock()
	require.False(t, w.c.IsBlockhashValid(m.RecentBlockhash))
	_, err = w.c.Submit(raw)
	requireReason(t, err, errs.CodeValidationFailed, CodeBlockhashNotFound)
}

func TestFault_ArmedFaultsAreConsumedInOrder(t *testing.T) {
	w := newWorld(t)
	w.c.DropNext(1)
	w.c.TimeoutButLand()
	w.c.TimeoutAndDrop()
	w.c.DropNext(0) // no-op

	ids := make([]TxID, 4)
	errsSeen := make([]error, 4)
	for i, note := range []string{"one", "two", "three", "four"} {
		ids[i], errsSeen[i] = w.c.Submit(signed(t, w.msg(memo(note)), w.payer))
	}
	require.NoError(t, errsSeen[0])
	require.Equal(t, CodeSubmissionTimeout, ReasonOf(errsSeen[1]))
	require.Equal(t, CodeSubmissionTimeout, ReasonOf(errsSeen[2]))
	require.NoError(t, errsSeen[3])
	w.c.ProduceBlock()
	require.Equal(t, []TxState{TxNotFound, TxConfirmed, TxNotFound, TxConfirmed},
		[]TxState{w.c.Status(ids[0]).State, w.c.Status(ids[1]).State, w.c.Status(ids[2]).State, w.c.Status(ids[3]).State})

	w.c.DropNext(5)
	w.c.DelayInclusion(3)
	w.c.ClearFaults()
	id := w.submit(signed(t, w.msg(memo("clear")), w.payer))
	w.c.ProduceBlock()
	require.Equal(t, TxConfirmed, w.c.Status(id).State)
}

func TestFault_DelayInclusion(t *testing.T) {
	w := newWorld(t)
	w.c.DelayInclusion(3)
	id := w.submit(signed(t, w.msg(memo("slow")), w.payer))
	for i := 0; i < 3; i++ {
		b := w.c.ProduceBlock()
		require.Empty(t, b.TxIDs)
		require.Equal(t, TxPending, w.c.Status(id).State)
	}
	b := w.c.ProduceBlock()
	require.Equal(t, []TxID{id}, b.TxIDs)

	// Pending transactions keep the delay they were accepted with.
	w.c.DelayInclusion(2)
	delayed := w.submit(signed(t, w.msg(memo("delayed")), w.payer))
	w.c.DelayInclusion(0)
	prompt := w.submit(signed(t, w.msg(memo("prompt")), w.payer))
	b = w.c.ProduceBlock()
	require.Equal(t, []TxID{prompt}, b.TxIDs, "a later, undelayed transaction may overtake a delayed one")
	b = w.c.ProduceBlock()
	require.Empty(t, b.TxIDs)
	b = w.c.ProduceBlock()
	require.Equal(t, []TxID{delayed}, b.TxIDs)
}

func TestFault_DelayBeyondTheWindowMeansNeverLanding(t *testing.T) {
	w := newWorld(t)
	w.c.DelayInclusion(testWindow)
	m := w.msg(memo("too slow"))
	id := w.submit(signed(t, m, w.payer))
	w.c.ProduceBlocks(testWindow)
	require.Equal(t, TxPending, w.c.Status(id).State, "still pending: expiry is applied as the next block is produced")
	w.c.ProduceBlock()
	require.Equal(t, TxNotFound, w.c.Status(id).State, "expired in the mempool")
	require.False(t, w.c.IsBlockhashValid(m.RecentBlockhash))
	require.Zero(t, w.c.PendingCount())
	require.NoError(t, w.c.audit())
}

func TestFault_ReorgRequeuesConfirmedTransactions(t *testing.T) {
	w := newWorld(t)
	w.mintUSD(w.alice.PublicKey(), 100)
	w.c.ProduceBlocks(testDepth) // the mint is final; nothing else is pending
	hashBefore := w.c.StateHash()
	alice, bob, payer := w.usdOf(w.alice), w.usdOf(w.bob), w.nativeOf(w.payer)

	id := w.land([]Keypair{w.alice}, TokenTransfer{Mint: w.usd, FromOwner: w.alice.PublicKey(), ToOwner: w.bob.PublicKey(), Amount: 25})
	orphan := w.c.Tip()
	require.Equal(t, TxConfirmed, w.c.Status(id).State)

	res, err := w.c.Reorg(1, ReorgRequeue)
	require.NoError(t, err)
	require.Equal(t, uint64(1), res.Removed)
	require.Equal(t, []TxID{id}, res.TxIDs)
	require.Equal(t, orphan.Parent, res.Tip.Hash)
	require.Equal(t, TxPending, w.c.Status(id).State, "finality downgrade: CONFIRMED back to PENDING")
	require.Equal(t, alice, w.usdOf(w.alice))
	require.Equal(t, bob, w.usdOf(w.bob))
	require.Equal(t, payer, w.nativeOf(w.payer), "the fee is reverted too")
	require.Equal(t, hashBefore, w.c.StateHash(), "the reorg restored the exact pre-block state")
	require.False(t, w.c.IsBlockhashValid(orphan.Hash), "an orphaned block's hash leaves the window")
	require.NoError(t, w.c.audit())

	b := w.c.ProduceBlock()
	require.Equal(t, []TxID{id}, b.TxIDs)
	require.Equal(t, orphan.Height, b.Height)
	require.Greater(t, b.Slot, orphan.Slot, "the new fork does not reuse the orphaned slot")
	require.NotEqual(t, orphan.Hash, b.Hash)
	require.Equal(t, TxConfirmed, w.c.Status(id).State)
	require.Equal(t, alice-25, w.usdOf(w.alice))
	require.NoError(t, w.c.audit())
}

func TestFault_ReorgDropForgetsTransactions(t *testing.T) {
	w := newWorld(t)
	w.mintUSD(w.alice.PublicKey(), 100)
	raw := signed(t, w.msg(TokenTransfer{Mint: w.usd, FromOwner: w.alice.PublicKey(), ToOwner: w.bob.PublicKey(), Amount: 60}), w.payer, w.alice)
	id := w.submit(raw)
	w.c.ProduceBlock()
	failing := w.land([]Keypair{w.alice}, TokenBurn{Mint: w.usd, Owner: w.alice.PublicKey(), Amount: 41})
	w.requireFailed(failing, CodeInsufficientTokenBalance)
	payer := w.nativeOf(w.payer)

	res, err := w.c.Reorg(2, ReorgDrop)
	require.NoError(t, err)
	require.Equal(t, uint64(2), res.Removed)
	require.Equal(t, []TxID{id, failing}, res.TxIDs)
	require.Equal(t, TxNotFound, w.c.Status(id).State)
	require.Equal(t, TxNotFound, w.c.Status(failing).State, "a FAILED transaction that was not final is downgraded too")
	require.Equal(t, uint64(100), w.usdOf(w.alice))
	require.Equal(t, payer+2*testFee, w.nativeOf(w.payer), "both fees are refunded")
	require.Zero(t, w.c.PendingCount())

	// Forgotten, so it may be sent again while its blockhash is canonical.
	require.Equal(t, id, w.submit(raw))
	w.c.ProduceBlock()
	require.Equal(t, TxConfirmed, w.c.Status(id).State)
	require.Equal(t, uint64(40), w.usdOf(w.alice))
	require.NoError(t, w.c.audit())
}

func TestFault_FinalizedNeverReverts(t *testing.T) {
	w := newWorld(t)
	final := w.mintUSD(w.alice.PublicKey(), 100)
	w.c.ProduceBlocks(testDepth)
	require.Equal(t, TxFinalized, w.c.Status(final).State)
	finalStatus := w.c.Status(final)
	// Anchor the burn to a block the reorg cannot remove, so it can land again.
	m := w.msg(TokenBurn{Mint: w.usd, Owner: w.alice.PublicKey(), Amount: 10})
	anchor, err := w.c.Block(w.c.FinalizedHeight())
	require.NoError(t, err)
	m.RecentBlockhash = anchor.Hash
	confirmed := w.submit(signed(t, m, w.payer, w.alice))
	w.c.ProduceBlock()
	require.Equal(t, TxConfirmed, w.c.Status(confirmed).State)
	tipBefore := w.c.Tip().Height
	finalized := w.c.FinalizedHeight()

	res, err := w.c.Reorg(1_000, ReorgRequeue)
	require.NoError(t, err)
	require.Equal(t, tipBefore-finalized, res.Removed, "only the non-finalized blocks can go")
	require.Equal(t, uint64(testDepth), res.Removed)
	require.Equal(t, finalized, res.Tip.Height)
	require.Equal(t, finalStatus, w.c.Status(final))
	require.Equal(t, TxPending, w.c.Status(confirmed).State)
	require.Equal(t, uint64(100), w.usdSupply())

	again, err := w.c.Reorg(1_000, ReorgDrop)
	require.NoError(t, err)
	require.Zero(t, again.Removed, "nothing above the finalized height is left")
	require.Equal(t, finalStatus, w.c.Status(final))
	require.Equal(t, finalized, w.c.FinalizedHeight(), "finality never moves backwards")
	require.NoError(t, w.c.audit())

	// After the reorg, new blocks finalize from where finality stood.
	w.c.ProduceBlocks(testDepth + 1)
	require.Equal(t, TxFinalized, w.c.Status(confirmed).State)
	require.Equal(t, uint64(90), w.usdSupply())
}

func TestFault_ReorgedBlockhashExpiresRequeuedTransactions(t *testing.T) {
	w := newWorld(t)
	w.c.ProduceBlock()
	m := w.msg(memo("anchored to the soon-orphaned tip"))
	anchor := m.RecentBlockhash
	id := w.submit(signed(t, m, w.payer))
	w.c.ProduceBlock()
	require.Equal(t, TxConfirmed, w.c.Status(id).State)

	_, err := w.c.Reorg(2, ReorgRequeue) // removes the anchor block and the including block
	require.NoError(t, err)
	require.False(t, w.c.IsBlockhashValid(anchor))
	require.Equal(t, TxPending, w.c.Status(id).State)
	w.c.ProduceBlock()
	require.Equal(t, TxNotFound, w.c.Status(id).State, "its blockhash is not on the new fork")
	require.NoError(t, w.c.audit())
}

func TestFault_ReorgCommutesWithOperatorAirdrops(t *testing.T) {
	w := newWorld(t)
	id := w.land([]Keypair{w.alice}, SystemTransfer{From: w.alice.PublicKey(), To: w.bob.PublicKey(), Lamports: testFunds})
	require.Equal(t, TxConfirmed, w.c.Status(id).State)
	require.NoError(t, w.c.Airdrop(w.bob.PublicKey(), 5)) // after the block, outside any block
	_, err := w.c.Reorg(1, ReorgDrop)
	require.NoError(t, err)
	require.Equal(t, uint64(testFunds+5), w.nativeOf(w.bob), "the airdrop survives; the transfer is reverted")
	require.Equal(t, uint64(testFunds), w.nativeOf(w.alice))
	require.NoError(t, w.c.audit())
}

func TestFault_ReorgArguments(t *testing.T) {
	w := newWorld(t)
	_, err := w.c.Reorg(1, ReorgMode(0))
	requireReason(t, err, errs.CodeValidationFailed, CodeInvalidArgument)
	_, err = w.c.Reorg(1, ReorgMode(3))
	requireReason(t, err, errs.CodeValidationFailed, CodeInvalidArgument)
	w.c.ProduceBlock()
	res, err := w.c.Reorg(0, ReorgRequeue)
	require.NoError(t, err)
	require.Zero(t, res.Removed)
	require.Equal(t, w.c.Tip(), res.Tip)
}

func TestFault_ConsecutiveArmsOfOneKindAccumulate(t *testing.T) {
	w := newWorld(t)
	w.c.DropNext(1)
	w.c.DropNext(2) // merges with the run above: three drops in all
	for i, note := range []string{"a", "b", "c", "d"} {
		id := w.submit(signed(t, w.msg(memo("drop-"+note)), w.payer))
		w.c.ProduceBlock()
		want := TxNotFound
		if i == 3 {
			want = TxConfirmed
		}
		require.Equal(t, want, w.c.Status(id).State, "submission %d", i)
	}
}

// TestReorg_RefusesAtomicallyWhenTheBooksDisagree corrupts the state behind
// the journal's back (white box) and requires Reorg to fail closed: an
// INTERNAL error and no change at all, rather than a half-reverted chain.
func TestReorg_RefusesAtomicallyWhenTheBooksDisagree(t *testing.T) {
	w := newWorld(t)
	id := w.land([]Keypair{w.alice}, SystemTransfer{From: w.alice.PublicKey(), To: w.bob.PublicKey(), Lamports: 100})
	w.c.mu.Lock()
	delete(w.c.state.native, w.bob.PublicKey()) // bob's credit vanishes outside any block
	w.c.mu.Unlock()
	tip := w.c.Tip()
	alice, payer := w.nativeOf(w.alice), w.nativeOf(w.payer)

	_, err := w.c.Reorg(1, ReorgRequeue)
	requireReason(t, err, errs.CodeInternal, CodeInvariantViolated)
	require.Equal(t, tip, w.c.Tip(), "no block was removed")
	require.Equal(t, TxConfirmed, w.c.Status(id).State)
	require.Equal(t, alice, w.nativeOf(w.alice), "no effect was half-reverted")
	require.Equal(t, payer, w.nativeOf(w.payer))
}
