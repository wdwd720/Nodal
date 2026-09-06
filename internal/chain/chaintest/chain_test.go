package chaintest_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/chain/chaintest"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

func q(n int64) money.Quantity { return money.QuantityFromInt64(n) }

func TestChain_CommitmentProgressionAndLag(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	ch := chaintest.NewChain(clk)
	fast := ch.NewObserver("fast", 0)
	slow := ch.NewObserver("slow", 3)
	ctx := context.Background()

	tx := ch.Land(ch.Swap("W", "USDC", "BONK", q(100), q(200), q(5000)))
	require.True(t, tx.Found)
	require.Equal(t, uint64(0), tx.Slot)

	// Processed: invisible to getTransaction, visible to statuses.
	obs, err := fast.GetTransaction(ctx, tx.Signature)
	require.NoError(t, err)
	require.False(t, obs.Found)
	st, err := fast.GetSignatureStatuses(ctx, []string{tx.Signature, "nope"})
	require.NoError(t, err)
	require.True(t, st[0].Found)
	require.Equal(t, chain.CommitmentProcessed, st[0].Commitment)
	require.False(t, st[1].Found)

	ch.Advance(1)
	obs, err = fast.GetTransaction(ctx, tx.Signature)
	require.NoError(t, err)
	require.True(t, obs.Found)
	require.Equal(t, chain.CommitmentConfirmed, obs.Commitment)
	require.Equal(t, "fast", obs.Source)
	obs, err = slow.GetTransaction(ctx, tx.Signature)
	require.NoError(t, err)
	require.False(t, obs.Found, "lagging observer has not seen it yet")

	ch.Advance(31)
	obs, err = fast.GetTransaction(ctx, tx.Signature)
	require.NoError(t, err)
	require.Equal(t, chain.CommitmentFinalized, obs.Commitment)
	obs, err = slow.GetTransaction(ctx, tx.Signature)
	require.NoError(t, err)
	require.Equal(t, chain.CommitmentConfirmed, obs.Commitment)
	st, err = fast.GetSignatureStatuses(ctx, []string{tx.Signature})
	require.NoError(t, err)
	require.Nil(t, st[0].Confirmations, "finalized has no confirmation count")

	ch.Drop(tx.Signature)
	obs, err = fast.GetTransaction(ctx, tx.Signature)
	require.NoError(t, err)
	require.False(t, obs.Found)
}

func TestChain_HeightBlockhashAndBalances(t *testing.T) {
	t.Parallel()
	ch := chaintest.NewChain(nil)
	ch.MaxBlockhashAge = 3
	obs := ch.NewObserver("o", 0)
	ctx := context.Background()

	bh, err := obs.GetLatestBlockhash(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(3), bh.LastValidBlockHeight)
	ok, err := obs.IsBlockhashValid(ctx, bh.Blockhash)
	require.NoError(t, err)
	require.True(t, ok)

	ch.Advance(2)
	ch.Skip(10) // skipped slots do not raise the height
	h, err := obs.GetBlockHeight(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(2), h)
	require.Equal(t, uint64(12), ch.Slot())
	ok, err = obs.IsBlockhashValid(ctx, bh.Blockhash)
	require.NoError(t, err)
	require.True(t, ok)
	ch.Advance(2)
	ok, err = obs.IsBlockhashValid(ctx, bh.Blockhash)
	require.NoError(t, err)
	require.False(t, ok, "expired once height > lastValidBlockHeight")
	ok, err = obs.IsBlockhashValid(ctx, "unknown")
	require.NoError(t, err)
	require.False(t, ok)

	ch.SetBalance(chain.BalanceObservation{Owner: "W", Mint: "USDC", TokenAccount: "W-ata-USDC", Amount: q(1000), Decimals: 6})
	ch.SetBalance(chain.BalanceObservation{Owner: "W", Mint: chain.MintNativeSOL, Amount: q(10_000)})
	ch.Land(ch.Swap("W", "USDC", "BONK", q(400), q(7), q(100)))
	ch.Advance(1)
	bs, err := obs.GetBalances(ctx, "W", nil)
	require.NoError(t, err)
	require.Len(t, bs, 3)
	byMint := map[string]string{}
	for _, b := range bs {
		byMint[b.Mint] = b.Amount.String()
		require.Equal(t, ch.Slot(), b.Slot)
	}
	require.Equal(t, "600", byMint["USDC"])
	require.Equal(t, "7", byMint["BONK"])
	require.Equal(t, "9900", byMint[chain.MintNativeSOL])
	bs, err = obs.GetBalances(ctx, "W", []string{"BONK"})
	require.NoError(t, err)
	require.Len(t, bs, 1)

	acts, err := obs.SearchWalletActivity(ctx, "W", time.Time{}, 10)
	require.NoError(t, err)
	require.Len(t, acts, 1)
	acts, err = obs.SearchWalletActivity(ctx, "someone-else", time.Time{}, 10)
	require.NoError(t, err)
	require.Empty(t, acts)
}

func TestFake_ScriptedAndFaults(t *testing.T) {
	t.Parallel()
	f := chaintest.NewFake("f", nil)
	ctx := context.Background()
	f.SetTransaction(chain.TxObservation{Signature: "s", Slot: 9, TokenBalanceDeltas: []chain.TokenDelta{{Owner: "W", Mint: "M", TokenAccount: "T", Pre: q(1), Post: q(2)}}})
	obs, err := f.GetTransaction(ctx, "s")
	require.NoError(t, err)
	require.True(t, obs.Found)
	require.Equal(t, chain.CommitmentConfirmed, obs.Commitment, "unscripted commitment defaults to confirmed")

	f.Fault(chaintest.MethodGetTransaction, chaintest.Fault{Kind: chaintest.FaultWrongDeltas, Remaining: 1})
	obs, err = f.GetTransaction(ctx, "s")
	require.NoError(t, err)
	require.Equal(t, "3", obs.TokenBalanceDeltas[0].Post.String())
	obs, err = f.GetTransaction(ctx, "s")
	require.NoError(t, err)
	require.Equal(t, "2", obs.TokenBalanceDeltas[0].Post.String(), "fault expired")

	f.Fault(chaintest.MethodGetTransaction, chaintest.Fault{Kind: chaintest.FaultNotFound})
	obs, err = f.GetTransaction(ctx, "s")
	require.NoError(t, err)
	require.False(t, obs.Found)

	f.Fault(chaintest.MethodAll, chaintest.Fault{Kind: chaintest.FaultError, Code: errs.CodeRateLimited})
	f.ClearFaults()
	f.Fault(chaintest.MethodAll, chaintest.Fault{Kind: chaintest.FaultError, Code: errs.CodeRateLimited})
	_, err = f.GetBlockHeight(ctx)
	require.Equal(t, errs.CodeRateLimited, errs.CodeOf(err))
	f.ClearFaults()

	tctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	f.Fault(chaintest.MethodGetBlockHeight, chaintest.Fault{Kind: chaintest.FaultTimeout})
	_, err = f.GetBlockHeight(tctx)
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	f.ClearFaults()

	f.Fault(chaintest.MethodGetBlockHeight, chaintest.Fault{Kind: chaintest.FaultLatency, Delay: time.Millisecond})
	f.SetBlockHeight(77)
	h, err := f.GetBlockHeight(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(77), h)
	require.Equal(t, 3, f.Calls(chaintest.MethodGetBlockHeight))

	f.SetSimulation(chain.SimulationResult{OK: false, Err: "x"}, nil)
	sim, err := f.Simulate(ctx, []byte("tx"), chain.SimulateOptions{})
	require.NoError(t, err)
	require.False(t, sim.OK)
	require.Equal(t, "f", sim.Source)

	f.SetBlockhash(chain.Blockhash{Blockhash: "h", LastValidBlockHeight: 5})
	bh, err := f.GetLatestBlockhash(ctx)
	require.NoError(t, err)
	require.Equal(t, "h", bh.Blockhash)
	ok, err := f.IsBlockhashValid(ctx, "h")
	require.NoError(t, err)
	require.True(t, ok)
	f.SetBlockhashValid("h", false)
	ok, err = f.IsBlockhashValid(ctx, "h")
	require.NoError(t, err)
	require.False(t, ok)

	f.SetStatus(chain.SignatureStatus{Signature: "z", Found: true, Commitment: chain.CommitmentFinalized})
	st, err := f.GetSignatureStatuses(ctx, []string{"z", "s", "none"})
	require.NoError(t, err)
	require.Equal(t, chain.CommitmentFinalized, st[0].Commitment)
	require.True(t, st[1].Found, "derived from the scripted transaction")
	require.False(t, st[2].Found)

	bt := time.Now()
	f.SetActivity("W", chain.TxObservation{Signature: "a1", Slot: 1, BlockTime: &bt}, chain.TxObservation{Signature: "a2", Slot: 2})
	acts, err := f.SearchWalletActivity(ctx, "W", time.Time{}, 1)
	require.NoError(t, err)
	require.Len(t, acts, 1)
	require.Equal(t, "a2", acts[0].Signature)
	f.SetBalances("W", chain.BalanceObservation{Mint: "M", TokenAccount: "T", Amount: q(1)})
	bs, err := f.GetBalances(ctx, "W", []string{"M"})
	require.NoError(t, err)
	require.Len(t, bs, 1)
	require.Equal(t, "f", bs[0].Source)
}

func TestFake_StreamFromChain(t *testing.T) {
	t.Parallel()
	ch := chaintest.NewChain(nil)
	obs := ch.NewObserver("helius", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sink, events := chaintest.Collect(2)
	done := make(chan error, 1)
	go func() { done <- obs.StreamWalletEvents(ctx, []string{"W"}, sink) }()

	// Land a transaction for W and one for someone else; only W's is delivered.
	ch.Land(ch.Swap("OTHER", "USDC", "BONK", q(1), q(1), q(1)))
	ch.Land(ch.Swap("W", "USDC", "BONK", q(1), q(1), q(1)))
	ch.Advance(1)
	require.ErrorIs(t, <-done, chaintest.ErrStop)
	evs := events()
	require.Len(t, evs, 2)
	require.Equal(t, chain.EventReconnect, evs[0].Kind)
	require.Equal(t, chain.EventTransaction, evs[1].Kind)
	require.Equal(t, chain.OriginStream, evs[1].Origin)
	require.Equal(t, "W", evs[1].Wallet)
	require.True(t, evs[1].Observation.Found)
	require.NotNil(t, evs[1].Sequence)

	// Streaming faults propagate.
	f := chaintest.NewFake("x", nil).Fault(chaintest.MethodStreamWalletEvents, chaintest.Fault{Kind: chaintest.FaultError})
	require.Error(t, f.StreamWalletEvents(ctx, nil, sink))
	// Context cancellation ends the stream.
	cctx, ccancel := context.WithCancel(ctx)
	ccancel()
	require.ErrorIs(t, chaintest.NewFake("y", nil).StreamWalletEvents(cctx, nil, func(chain.WalletEvent) error { return nil }), context.Canceled)
}

func TestMemArchive(t *testing.T) {
	t.Parallel()
	a := chaintest.NewMemArchive()
	ref, err := a.Store(context.Background(), chain.RawObject{Provider: "p", EventType: "getTransaction", Body: []byte("{}")})
	require.NoError(t, err)
	got, ok := a.Get(ref)
	require.True(t, ok)
	require.Equal(t, "{}", string(got.Body))
	require.Equal(t, 1, a.Len())
	require.Equal(t, []string{ref}, a.Refs())
	a.Err = errs.New(errs.CodeInternal, "down")
	_, err = a.Store(context.Background(), chain.RawObject{})
	require.Error(t, err)
}
