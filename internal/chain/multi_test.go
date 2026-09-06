package chain_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/chain/chaintest"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/provider"
)

type multiFixture struct {
	clk     *clock.Fake
	ch      *chaintest.Chain
	primary *chaintest.Fake
	second  *chaintest.Fake
	ptr     *provider.Tracker
	str     *provider.Tracker
	multi   *chain.MultiObserver
	tx      chain.TxObservation
}

func newMultiFixture(t *testing.T) *multiFixture {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	ch := chaintest.NewChain(clk)
	primary := ch.NewObserver("helius", 0)
	second := ch.NewObserver("rpc-fallback", 0)
	th := provider.DefaultThresholds()
	th.MinSamples = 3
	th.RecoverStreak = 2
	th.MaxStaleness = 0
	ptr, err := provider.NewTracker("helius", th, clk.Now())
	require.NoError(t, err)
	str, err := provider.NewTracker("rpc-fallback", th, clk.Now())
	require.NoError(t, err)
	multi, err := chain.NewMultiObserver(primary, second, chain.MultiObserverOptions{
		// 500ms, not 50ms: the fakes answer from memory, so no successful call in
		// this file needs a deadline at all, but several tests inject FaultTimeout,
		// which blocks until this deadline fires. 50ms left only a 50ms budget for
		// the success paths too, which is thin under a contended -race run.
		PerCallTimeout: 500 * time.Millisecond, PrimaryTracker: ptr, SecondaryTracker: str, Clock: clk,
	})
	require.NoError(t, err)
	tx := ch.Land(ch.Swap("W", "USDC", "BONK", q(1_000_000), q(5_000_000), q(5000)))
	ch.Advance(40) // finalized for both
	return &multiFixture{clk: clk, ch: ch, primary: primary, second: second, ptr: ptr, str: str, multi: multi, tx: tx}
}

func TestNewMultiObserver_Validation(t *testing.T) {
	t.Parallel()
	a := chaintest.NewFake("a", nil)
	_, err := chain.NewMultiObserver(a, nil, chain.MultiObserverOptions{})
	require.Error(t, err)
	_, err = chain.NewMultiObserver(a, chaintest.NewFake("a", nil), chain.MultiObserverOptions{})
	require.Error(t, err, "observers must be independent vendors")
	bad := chain.DefaultPolicy()
	bad.OnDisagreement = "x"
	_, err = chain.NewMultiObserver(a, chaintest.NewFake("b", nil), chain.MultiObserverOptions{Policy: bad})
	require.Error(t, err)
	m, err := chain.NewMultiObserver(a, chaintest.NewFake("b", nil), chain.MultiObserverOptions{})
	require.NoError(t, err)
	require.Equal(t, chain.DefaultPolicy(), m.Policy())
	require.Equal(t, "a", m.Primary().Name())
	require.Equal(t, "b", m.Secondary().Name())
	_, err = m.Observe(context.Background(), "s", "BOGUS")
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestMultiObserver_BothHealthyAgree(t *testing.T) {
	t.Parallel()
	f := newMultiFixture(t)
	res, err := f.multi.Observe(context.Background(), f.tx.Signature, chain.FinalityFinalized)
	require.NoError(t, err)
	require.Equal(t, chain.Agreed, res.State)
	require.Equal(t, chain.FinalityFinalized, res.Finality)
	require.True(t, res.Satisfied)
	require.False(t, res.Degraded)
	require.Equal(t, 1, f.primary.Calls(chaintest.MethodGetTransaction))
	require.Equal(t, 1, f.second.Calls(chaintest.MethodGetTransaction))
	require.Equal(t, 1, f.ptr.Snapshot(f.clk.Now()).Samples)
	require.Equal(t, 1, f.str.Snapshot(f.clk.Now()).Samples)
}

func TestMultiObserver_LaggingSecondaryIsDisagreement(t *testing.T) {
	t.Parallel()
	f := newMultiFixture(t)
	f.second.SetLag(100) // sees the chain before the transaction landed
	res, err := f.multi.Observe(context.Background(), f.tx.Signature, chain.FinalityConfirmed)
	require.NoError(t, err)
	require.Equal(t, chain.Disagreed, res.State)
	require.True(t, res.BlockDependent)
	require.Contains(t, res.Detail, "found by helius but not by rpc-fallback")
}

func TestMultiObserver_ConfirmedVsFinalized(t *testing.T) {
	t.Parallel()
	f := newMultiFixture(t)
	f.second.SetLag(35) // secondary still sees it as confirmed
	res, err := f.multi.Observe(context.Background(), f.tx.Signature, chain.FinalityFinalized)
	require.NoError(t, err)
	require.Equal(t, chain.Agreed, res.State)
	require.Equal(t, chain.FinalityConfirmed, res.Finality, "never the optimistic level")
	require.False(t, res.Satisfied)
	require.Equal(t, "rpc-fallback", res.Observation.Source)
}

func TestMultiObserver_SecondaryTimeout_PrimaryOnlyCapped(t *testing.T) {
	t.Parallel()
	f := newMultiFixture(t)
	f.second.Fault(chaintest.MethodGetTransaction, chaintest.Fault{Kind: chaintest.FaultTimeout})
	res, err := f.multi.Observe(context.Background(), f.tx.Signature, chain.FinalityFinalized)
	require.NoError(t, err)
	require.Equal(t, chain.PrimaryOnly, res.State)
	require.Equal(t, chain.FinalityConfirmed, res.Finality)
	require.True(t, res.Degraded)
	require.False(t, res.Satisfied)
	require.True(t, res.BlockDependent)
	require.Contains(t, res.Detail, "rpc-fallback unavailable (PROVIDER_UNAVAILABLE)")
	require.Contains(t, res.Detail, "capped at CONFIRMED")
	snap := f.str.Snapshot(f.clk.Now())
	require.Equal(t, 1, snap.Samples)
	require.Equal(t, int64(10_000), snap.ErrorRateBPS)

	res, err = f.multi.Observe(context.Background(), f.tx.Signature, chain.FinalityConfirmed)
	require.NoError(t, err)
	require.True(t, res.Satisfied, "CONFIRMED is still achievable with one observer")
	require.False(t, res.BlockDependent)
}

func TestMultiObserver_DisabledObserverIsNotQueried(t *testing.T) {
	t.Parallel()
	f := newMultiFixture(t)
	f.primary.SetHealth(provider.Disabled)
	res, err := f.multi.Observe(context.Background(), f.tx.Signature, chain.FinalityConfirmed)
	require.NoError(t, err)
	require.Equal(t, chain.SecondaryOnly, res.State)
	require.Equal(t, 0, f.primary.Calls(chaintest.MethodGetTransaction), "DISABLED means not consulted")
	require.Contains(t, res.Detail, "helius is DISABLED")
	require.Equal(t, 0, f.ptr.Snapshot(f.clk.Now()).Samples)
}

func TestMultiObserver_UnhealthyObserverStillObservedButCannotRaise(t *testing.T) {
	t.Parallel()
	f := newMultiFixture(t)
	f.second.SetHealth(provider.Unhealthy)

	// Unhealthy secondary agrees: still degraded, capped at CONFIRMED.
	res, err := f.multi.Observe(context.Background(), f.tx.Signature, chain.FinalityFinalized)
	require.NoError(t, err)
	require.Equal(t, chain.PrimaryOnly, res.State)
	require.Equal(t, chain.FinalityConfirmed, res.Finality)
	require.Equal(t, 1, f.second.Calls(chaintest.MethodGetTransaction), "UNHEALTHY observers are still read (PART 107)")
	require.NotNil(t, res.Secondary)

	// Unhealthy secondary lagging (not found): staleness explains it, ignored.
	f.second.SetLag(100)
	res, err = f.multi.Observe(context.Background(), f.tx.Signature, chain.FinalityConfirmed)
	require.NoError(t, err)
	require.Equal(t, chain.PrimaryOnly, res.State)
	require.True(t, res.Satisfied)

	// Unhealthy secondary found with different deltas: hard conflict.
	f.second.SetLag(0).Fault(chaintest.MethodGetTransaction, chaintest.Fault{Kind: chaintest.FaultWrongDeltas})
	res, err = f.multi.Observe(context.Background(), f.tx.Signature, chain.FinalityConfirmed)
	require.NoError(t, err)
	require.Equal(t, chain.Disagreed, res.State)
	require.True(t, res.BlockDependent)
	require.True(t, res.Degraded)
	require.Contains(t, res.Detail, "UNHEALTHY but its answer conflicts")

	// Unhealthy secondary found while healthy primary does not: disagreement.
	f.second.ClearFaults()
	f.primary.SetLag(100)
	res, err = f.multi.Observe(context.Background(), f.tx.Signature, chain.FinalityConfirmed)
	require.NoError(t, err)
	require.Equal(t, chain.Disagreed, res.State)
}

func TestMultiObserver_BothUnhealthy(t *testing.T) {
	t.Parallel()
	f := newMultiFixture(t)
	f.primary.SetHealth(provider.Unhealthy)
	f.second.SetHealth(provider.Unhealthy)
	res, err := f.multi.Observe(context.Background(), f.tx.Signature, chain.FinalityFinalized)
	require.NoError(t, err)
	require.Equal(t, chain.Agreed, res.State)
	require.Equal(t, chain.FinalityConfirmed, res.Finality, "two unhealthy observers never finalize")
	require.True(t, res.Degraded)
	require.False(t, res.Satisfied)
	require.True(t, res.BlockDependent)
}

func TestMultiObserver_BothUnavailable(t *testing.T) {
	t.Parallel()
	f := newMultiFixture(t)
	f.primary.Fault(chaintest.MethodAll, chaintest.Fault{Kind: chaintest.FaultError})
	f.second.SetHealth(provider.Disabled)
	_, err := f.multi.Observe(context.Background(), f.tx.Signature, chain.FinalityConfirmed)
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	require.Contains(t, err.Error(), "rpc-fallback is DISABLED")
	_, err = f.multi.Balances(context.Background(), "W", nil)
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	_, err = f.multi.BlockHeight(context.Background())
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
}

func TestMultiObserver_HealthSamplingTransitions(t *testing.T) {
	t.Parallel()
	f := newMultiFixture(t)
	f.second.Fault(chaintest.MethodGetTransaction, chaintest.Fault{Kind: chaintest.FaultTimeout, Remaining: 3})
	for i := 0; i < 3; i++ {
		_, err := f.multi.Observe(context.Background(), f.tx.Signature, chain.FinalityConfirmed)
		require.NoError(t, err)
		f.clk.Advance(time.Second)
	}
	require.Equal(t, provider.Unhealthy, f.str.Health(), "three timeouts out of three samples")
	require.Equal(t, provider.Healthy, f.ptr.Health())
	for i := 0; i < 6; i++ {
		_, err := f.multi.Observe(context.Background(), f.tx.Signature, chain.FinalityConfirmed)
		require.NoError(t, err)
		f.clk.Advance(time.Second)
	}
	require.NotEqual(t, provider.Unhealthy, f.str.Health(), "successes recover with hysteresis")
}

func TestMultiObserver_ContextCancelled(t *testing.T) {
	t.Parallel()
	f := newMultiFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.primary.Fault(chaintest.MethodAll, chaintest.Fault{Kind: chaintest.FaultTimeout})
	f.second.Fault(chaintest.MethodAll, chaintest.Fault{Kind: chaintest.FaultTimeout})
	_, err := f.multi.Observe(ctx, f.tx.Signature, chain.FinalityConfirmed)
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
}

func TestMultiObserver_Balances(t *testing.T) {
	t.Parallel()
	f := newMultiFixture(t)
	res, err := f.multi.Balances(context.Background(), "W", []string{"USDC", "BONK"})
	require.NoError(t, err)
	require.Equal(t, chain.Agreed, res.State)
	require.Len(t, res.Balances, 2)
	require.Equal(t, "5000000", res.Balances[0].Amount.String())

	f.second.Fault(chaintest.MethodGetBalances, chaintest.Fault{Kind: chaintest.FaultWrongDeltas})
	res, err = f.multi.Balances(context.Background(), "W", []string{"USDC", "BONK"})
	require.NoError(t, err)
	require.Equal(t, chain.Disagreed, res.State)
	require.True(t, res.BlockDependent)

	f.second.ClearFaults().SetHealth(provider.Disabled)
	res, err = f.multi.Balances(context.Background(), "W", nil)
	require.NoError(t, err)
	require.Equal(t, chain.PrimaryOnly, res.State)
	require.True(t, res.Degraded)

	f.second.SetHealth(provider.Unhealthy)
	res, err = f.multi.Balances(context.Background(), "W", nil)
	require.NoError(t, err)
	require.Equal(t, chain.PrimaryOnly, res.State)

	f.primary.SetHealth(provider.Unhealthy)
	res, err = f.multi.Balances(context.Background(), "W", nil)
	require.NoError(t, err)
	require.Equal(t, chain.Agreed, res.State)
	require.True(t, res.Degraded)
}

func TestMultiObserver_BlockHeightAndProvenAbsent(t *testing.T) {
	t.Parallel()
	f := newMultiFixture(t)
	f.second.SetLag(5)
	h, err := f.multi.BlockHeight(context.Background())
	require.NoError(t, err)
	require.Equal(t, f.ch.Height()-5, h.Height, "conservative minimum")
	require.Equal(t, uint64(5), h.Skew)
	require.False(t, h.Degraded)

	f.second.SetLag(0)
	res, err := f.multi.Observe(context.Background(), "unknown-sig", chain.FinalityConfirmed)
	require.NoError(t, err)
	require.Equal(t, chain.NotFound, res.State)
	h, err = f.multi.BlockHeight(context.Background())
	require.NoError(t, err)
	require.True(t, chain.ProvenAbsent(res, h, h.Height-10, 5))
	require.False(t, chain.ProvenAbsent(res, h, h.Height-3, 5), "inside the margin is not proof")
	require.False(t, chain.ProvenAbsent(res, h, h.Height, 0))

	// Degraded absence is never proof.
	f.second.SetHealth(provider.Disabled)
	res, err = f.multi.Observe(context.Background(), "unknown-sig", chain.FinalityConfirmed)
	require.NoError(t, err)
	require.Equal(t, chain.NotFound, res.State)
	require.True(t, res.BlockDependent)
	h, err = f.multi.BlockHeight(context.Background())
	require.NoError(t, err)
	require.True(t, h.Degraded)
	require.False(t, chain.ProvenAbsent(res, h, 0, 0))
}
