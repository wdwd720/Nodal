package devnet

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

func TestLifecycle_PendingConfirmedFinalized(t *testing.T) {
	w := newWorld(t)
	id := w.send([]Keypair{w.alice}, SystemTransfer{From: w.alice.PublicKey(), To: w.bob.PublicKey(), Lamports: 1})
	require.Equal(t, TxStatus{State: TxPending}, w.c.Status(id))
	require.Equal(t, 1, w.c.PendingCount())

	b := w.c.ProduceBlock()
	require.Equal(t, []TxID{id}, b.TxIDs)
	require.Zero(t, w.c.PendingCount())
	want := TxStatus{State: TxConfirmed, Slot: b.Slot, Height: b.Height}
	require.Equal(t, want, w.c.Status(id))

	// CONFIRMED for FinalityDepth-1 further blocks, FINALIZED at the depth.
	for i := 1; i < testDepth; i++ {
		w.c.ProduceBlock()
		require.Equal(t, want, w.c.Status(id), "after %d further blocks", i)
		blk, err := w.c.Block(b.Height)
		require.NoError(t, err)
		require.False(t, blk.Final)
	}
	tip := w.c.ProduceBlock()
	require.Equal(t, b.Height+testDepth, tip.Height)
	require.Equal(t, TxStatus{State: TxFinalized, Slot: b.Slot, Height: b.Height, Final: true}, w.c.Status(id))
	require.Equal(t, b.Height, w.c.FinalizedHeight())
	blk, err := w.c.Block(b.Height)
	require.NoError(t, err)
	require.True(t, blk.Final)
	require.Equal(t, b.Hash, blk.Hash)
	require.NoError(t, w.c.audit())
}

func TestLifecycle_FailedTransactionsFinalizeToo(t *testing.T) {
	w := newWorld(t)
	id := w.land([]Keypair{w.alice}, TokenBurn{Mint: w.usd, Owner: w.alice.PublicKey(), Amount: 1})
	st := w.c.Status(id)
	require.Equal(t, TxFailed, st.State)
	require.Equal(t, CodeInsufficientTokenBalance, st.Err)
	require.False(t, st.Final)
	w.c.ProduceBlocks(testDepth)
	st = w.c.Status(id)
	require.Equal(t, TxFailed, st.State)
	require.True(t, st.Final)
}

func TestLifecycle_InclusionFollowsSubmissionOrder(t *testing.T) {
	w := newWorld(t)
	w.mintUSD(w.alice.PublicKey(), 10)
	// The second transfer only succeeds if the first ran before it.
	first := w.send([]Keypair{w.alice}, TokenTransfer{Mint: w.usd, FromOwner: w.alice.PublicKey(), ToOwner: w.bob.PublicKey(), Amount: 10})
	second := w.send([]Keypair{w.bob}, TokenTransfer{Mint: w.usd, FromOwner: w.bob.PublicKey(), ToOwner: w.carol.PublicKey(), Amount: 10})
	third := w.send(nil, memo("third"))
	b := w.c.ProduceBlock()
	require.Equal(t, []TxID{first, second, third}, b.TxIDs)
	require.Equal(t, TxConfirmed, w.c.Status(second).State)
	require.Equal(t, uint64(10), w.usdOf(w.carol))
}

func TestLifecycle_BlockhashWindow(t *testing.T) {
	w := newWorld(t)
	old := w.c.LatestBlockhash() // genesis, height 0
	raw := signed(t, w.msg(memo("late")), w.payer)

	// Valid while its block is among the testWindow most recent blocks.
	w.c.ProduceBlocks(testWindow - 1)
	require.True(t, w.c.IsBlockhashValid(old))
	stillOK := signed(t, Message{ChainID: testChainID, RecentBlockhash: old, FeePayer: w.payer.PublicKey(), Instructions: []Instruction{memo("edge")}}, w.payer)
	edge := w.submit(stillOK)

	w.c.ProduceBlock() // includes edge; old is now testWindow blocks old
	require.Equal(t, TxConfirmed, w.c.Status(edge).State)
	require.False(t, w.c.IsBlockhashValid(old))
	_, err := w.c.Submit(raw)
	requireReason(t, err, errs.CodeValidationFailed, CodeBlockhashNotFound)

	// A hash that was never produced is not found either.
	m := w.msg(memo("fabricated"))
	m.RecentBlockhash = Hash{0xEE}
	_, err = w.c.Submit(signed(t, m, w.payer))
	requireReason(t, err, errs.CodeValidationFailed, CodeBlockhashNotFound)
	require.False(t, w.c.IsBlockhashValid(Hash{0xEE}))
}

func TestLifecycle_DuplicateSubmissionIsIdempotent(t *testing.T) {
	w := newWorld(t)
	w.mintUSD(w.alice.PublicKey(), 100)
	raw := signed(t, w.msg(TokenTransfer{Mint: w.usd, FromOwner: w.alice.PublicKey(), ToOwner: w.bob.PublicKey(), Amount: 30}), w.payer, w.alice)

	id1, err := w.c.Submit(raw)
	require.NoError(t, err)
	id2, err := w.c.Submit(raw)
	require.NoError(t, err)
	require.Equal(t, id1, id2)
	require.Equal(t, 1, w.c.PendingCount(), "a resubmission does not queue a second copy")

	b := w.c.ProduceBlock()
	require.Equal(t, []TxID{id1}, b.TxIDs)
	status := w.c.Status(id1)

	id3, err := w.c.Submit(raw)
	require.NoError(t, err)
	require.Equal(t, id1, id3)
	require.Equal(t, status, w.c.Status(id1), "resubmission returns the existing status unchanged")
	require.Zero(t, w.c.PendingCount())

	// Even after its blockhash expires and it is final, the known
	// transaction is acknowledged, never re-applied.
	w.c.ProduceBlocks(testWindow + testDepth)
	id4, err := w.c.Submit(raw)
	require.NoError(t, err)
	require.Equal(t, id1, id4)
	require.Equal(t, TxFinalized, w.c.Status(id1).State)
	w.c.ProduceBlock()
	require.Equal(t, uint64(70), w.usdOf(w.alice), "applied exactly once")
	require.Equal(t, uint64(30), w.usdOf(w.bob))
	require.NoError(t, w.c.audit())
}

func TestLifecycle_GetTransactionAndQueries(t *testing.T) {
	w := newWorld(t)
	m := w.msg(SystemTransfer{From: w.alice.PublicKey(), To: w.bob.PublicKey(), Lamports: 3})
	tx, err := Sign(m, w.payer, w.alice)
	require.NoError(t, err)
	id := w.submit(encode(t, tx))

	pending, err := w.c.GetTransaction(id)
	require.NoError(t, err)
	require.Equal(t, TxPending, pending.Status.State)
	require.Equal(t, tx, pending.Transaction)
	require.Zero(t, pending.Fee)

	w.c.ProduceBlock()
	rec, err := w.c.GetTransaction(id)
	require.NoError(t, err)
	require.Equal(t, id, rec.ID)
	require.Equal(t, TxConfirmed, rec.Status.State)
	require.Equal(t, uint64(testFee), rec.Fee)

	_, err = w.c.GetTransaction(TxIDFromSignature(Signature{1, 2, 3}))
	requireReason(t, err, errs.CodeNotFound, CodeTransactionNotFound)
	_, err = w.c.GetTransaction("not base58!")
	requireReason(t, err, errs.CodeValidationFailed, CodeInvalidBase58)
	require.Equal(t, TxStatus{State: TxNotFound}, w.c.Status("not base58!"))

	tip := w.c.Tip()
	require.Equal(t, uint64(1), tip.Height)
	require.Equal(t, uint64(1), tip.Slot)
	require.Equal(t, tip.Hash, w.c.LatestBlockhash())
	genesis, err := w.c.Block(0)
	require.NoError(t, err)
	require.Equal(t, genesis.Hash, tip.Parent)
	require.True(t, genesis.Final)
	_, err = w.c.Block(2)
	requireReason(t, err, errs.CodeNotFound, CodeBlockNotFound)
	require.Equal(t, w.c.Tip(), w.c.ProduceBlocks(0))
}

func TestStateHash_IsDeterministic(t *testing.T) {
	run := func() (*world, []Hash) {
		w := newWorld(t)
		var hashes []Hash
		hashes = append(hashes, w.c.StateHash())
		w.mintUSD(w.alice.PublicKey(), 500)
		hashes = append(hashes, w.c.StateHash())
		w.land([]Keypair{w.alice}, TokenTransfer{Mint: w.usd, FromOwner: w.alice.PublicKey(), ToOwner: w.bob.PublicKey(), Amount: 7})
		hashes = append(hashes, w.c.StateHash())
		w.c.ProduceBlocks(testDepth)
		hashes = append(hashes, w.c.StateHash())
		return w, hashes
	}
	a, ha := run()
	b, hb := run()
	require.Equal(t, ha, hb, "identical operation sequences give identical hashes")
	for i := 1; i < len(ha); i++ {
		require.NotEqual(t, ha[i-1], ha[i], "step %d changed state", i)
	}
	for i := 0; i < 20; i++ {
		require.Equal(t, a.c.StateHash(), b.c.StateHash(), "independent of map iteration order")
	}
	require.Equal(t, a.c.Tip(), b.c.Tip())

	// A fresh world starts from the same hash; an operator airdrop, which
	// belongs to no block, still changes it.
	c := newWorld(t)
	require.Equal(t, ha[0], c.c.StateHash())
	require.NoError(t, c.c.Airdrop(c.alice.PublicKey(), 1))
	require.NotEqual(t, ha[0], c.c.StateHash())
}
