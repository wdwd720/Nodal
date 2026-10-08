//go:build integration

package bridge

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// noPhantomCapital checks, at one instant, that a customer's value is counted
// exactly once across the source wallet, the bridge and the destination
// wallet, that nothing not final on the destination is available there, and
// that in-flight value adds no buying power under the default policy.
func (f *fixture) noPhantomCapital(r Route, c customer, funded uint64, stage string) {
	f.t.Helper()
	q := func(s string) money.Quantity {
		v, err := money.ParseQuantity(s)
		require.NoError(f.t, err)
		return v
	}
	src, inFlight, dst := q(f.wallet(c, r.SourceAssetID)), q(f.inFlight(r, c)), q(f.wallet(c, r.DestinationAssetID))
	require.Zero(f.t, src.Add(inFlight).Add(dst).Cmp(devnet.QuantityOf(funded)), "%s: value counted twice or not at all", stage)
	require.Equal(f.t, dst.String(), f.available(c, r.DestinationAssetID), "%s: only final destination value is available", stage)

	var pending money.Quantity
	legs, err := listLegs(f.ctx, testDB, `account_id = $1 AND status = 'BRIDGE_REQUESTED'`, c.id)
	require.NoError(f.t, err)
	for _, l := range legs {
		pending = pending.Add(l.Quantity)
	}
	avail := q(f.available(c, r.SourceAssetID))
	require.Zero(f.t, avail.Add(pending).Add(inFlight).Add(dst).Cmp(devnet.QuantityOf(funded)),
		"%s: available %s + pending %s + in flight %s + destination %s", stage, avail, pending, inFlight, dst)

	bp, err := f.svc.BuyingPower(f.ctx, testDB, c.id, DefaultHaircutPolicy())
	require.NoError(f.t, err, stage)
	var held money.Quantity
	for _, h := range bp.Holdings {
		held = held.Add(h.Quantity)
	}
	require.Zero(f.t, held.Cmp(inFlight), "%s: the bridge's holdings are the in-flight balance", stage)
	v2, err := buyingpower.EvaluateV2(buyingpower.V2Input{OwnerType: "CUSTOMER", OwnerID: c.id.String(), Holdings: bp.Holdings,
		Policy: buyingpower.DefaultV2Policy(), AsOf: f.clk.Now()})
	require.NoError(f.t, err)
	require.True(f.t, v2.EligibleCollateral.IsZero(), "%s: in-flight value is never eligible by default", stage)
}

// TestIntegration_ADelayedBridgeCreatesNoPhantomCapital: while a release is
// delayed, then confirmed but not final, the destination shows nothing the
// ledger has not booked, the source never shows the units as spendable, and
// buying power counts the value once, at zero; only finality moves it.
func TestIntegration_ADelayedBridgeCreatesNoPhantomCapital(t *testing.T) {
	f := newFixture(t)
	r := f.route
	c := f.customer()
	f.fund(r, c, 1_000_000)

	res, err := f.bridge(r, c, 400_000, gates.ModeDevnet)
	require.NoError(t, err)
	lid := res.Leg.ID
	require.Equal(t, StateBridgeRequested, res.Leg.Status, "DEVNET legs are observed, never driven")
	f.noPhantomCapital(r, c, 1_000_000, "lock pending")
	f.produce(r.SourceChain, 1)
	require.Equal(t, StateBridgeRequested, f.observe(lid).Status, "a confirmed lock is not final")
	f.noPhantomCapital(r, c, 1_000_000, "lock confirmed")
	assert.True(t, f.reconcile(r).Matched(), "a landed, unposted lock is the honest difference")

	require.NoError(t, f.host.DelayInclusion(f.ctx, r.DestinationChain, 5))
	f.produce(r.SourceChain, itDepth)
	leg := f.observe(lid)
	require.Equal(t, StateMessageInFlight, leg.Status)
	f.noPhantomCapital(r, c, 1_000_000, "release delayed")
	release := f.attempts(lid, AttemptRelease)[0]

	delayed := 0
	for ; delayed < 10 && f.status(r.DestinationChain, release.TxID).State == devnet.TxPending; delayed++ {
		require.Equal(t, StateMessageInFlight, f.observe(lid).Status)
		f.noPhantomCapital(r, c, 1_000_000, "release delayed")
		f.produce(r.DestinationChain, 1)
	}
	require.GreaterOrEqual(t, delayed, 3, "the release was held back")
	require.Equal(t, devnet.TxConfirmed, f.status(r.DestinationChain, release.TxID).State)
	require.Equal(t, StateDestinationObserved, f.observe(lid).Status)
	assert.Equal(t, uint64(400_000), f.chainBalance(r.DestinationChain, c.holder, r.DestinationMint), "the chain shows it")
	f.noPhantomCapital(r, c, 1_000_000, "release confirmed, not final")
	assert.Equal(t, "0", f.wallet(c, r.DestinationAssetID), "the books do not, until it is final")
	assert.True(t, f.reconcile(r).Matched(), "a landed, unposted release is the honest difference")

	f.produce(r.DestinationChain, itDepth)
	leg = f.observe(lid)
	require.Equal(t, StateReconciled, leg.Status)
	f.noPhantomCapital(r, c, 1_000_000, "final")
	assert.Equal(t, "400000", f.wallet(c, r.DestinationAssetID))
	assert.Equal(t, "400000", f.available(c, r.DestinationAssetID))
	assert.Len(t, f.attempts(lid, AttemptRelease), 1, "a delayed release is waited for, never raced")
}

// TestIntegration_AReorgNeverRevertsFinalityOrDoubleCounts: a lock reorged
// away before finality is never posted and fails only once it can never
// land; one returned to the mempool finalizes and posts once; a release
// reorged away returns the leg to MESSAGE_IN_FLIGHT, and a new release is
// relayed only after the first can never land; nothing finalized is ever
// reverted, and the destination mints the leg's units exactly once.
func TestIntegration_AReorgNeverRevertsFinalityOrDoubleCounts(t *testing.T) {
	f := newFixture(t)
	r := f.route
	c := f.customer()
	f.fund(r, c, 1_000_000)

	// A lock confirmed, then reorged away and forgotten.
	res, err := f.bridge(r, c, 300_000, gates.ModeDevnet)
	require.NoError(t, err)
	dropped := res.Leg
	f.produce(r.SourceChain, 1)
	require.Equal(t, devnet.TxConfirmed, f.status(r.SourceChain, dropped.LockTxID).State)
	_, err = f.host.Reorg(f.ctx, r.SourceChain, 1, devnet.ReorgDrop)
	require.NoError(t, err)
	require.Equal(t, devnet.TxNotFound, f.status(r.SourceChain, dropped.LockTxID).State)
	require.Equal(t, StateBridgeRequested, f.observe(dropped.ID).Status, "it might still land while its blockhash is valid")
	assert.Equal(t, 0, f.journals(dropped, ledger.KindBridgeSourceFinalized))
	f.noPhantomCapital(r, c, 1_000_000, "lock reorged away")
	f.produce(r.SourceChain, itWindow+itDepth+1)
	dropped = f.observe(dropped.ID)
	require.Equal(t, StateFailed, dropped.Status)
	assert.Equal(t, "SUBMISSION_EXPIRED", dropped.FailureCode)
	assert.Equal(t, 1, f.submissions(dropped.LockTxID), "never resubmitted")
	assert.Equal(t, capital.ReservationReleased, f.reservation(dropped).Status)
	assert.Equal(t, uint64(1_000_000), f.chainBalance(r.SourceChain, c.holder, r.SourceMint))
	f.noPhantomCapital(r, c, 1_000_000, "lock expired")

	// A lock confirmed, reorged back into the mempool, then finalized.
	res, err = f.bridge(r, c, 200_000, gates.ModeDevnet)
	require.NoError(t, err)
	lid := res.Leg.ID
	f.produce(r.SourceChain, 1)
	_, err = f.host.Reorg(f.ctx, r.SourceChain, 1, devnet.ReorgRequeue)
	require.NoError(t, err)
	require.Equal(t, devnet.TxPending, f.status(r.SourceChain, res.Leg.LockTxID).State)
	require.Equal(t, StateBridgeRequested, f.observe(lid).Status)
	f.produce(r.SourceChain, 1+itDepth)
	leg := f.observe(lid)
	require.Equal(t, StateMessageInFlight, leg.Status)
	assert.Equal(t, 1, f.journals(leg, ledger.KindBridgeSourceFinalized))
	assert.Equal(t, econ.EffectApplied, f.effect(leg.SourceEffectID).Status)

	// The release confirmed, then reorged away and forgotten.
	f.produce(r.DestinationChain, 1)
	require.Equal(t, StateDestinationObserved, f.observe(lid).Status)
	assert.True(t, f.reconcile(r).Matched())
	_, err = f.host.Reorg(f.ctx, r.DestinationChain, 1, devnet.ReorgDrop)
	require.NoError(t, err)
	leg = f.observe(lid)
	require.Equal(t, StateMessageInFlight, leg.Status, "an observed release that left the chain is in flight again")
	require.Len(t, f.attempts(lid, AttemptRelease), 1, "while it might still land, nothing else is relayed")
	assert.Equal(t, 0, f.journals(leg, ledger.KindBridgeDestinationFinalized))
	assert.Equal(t, uint64(0), f.supply(r.DestinationChain, r.DestinationMint))
	f.noPhantomCapital(r, c, 1_000_000, "release reorged away")
	assert.True(t, f.reconcile(r).Matched())

	f.produce(r.DestinationChain, itWindow+itDepth+1)
	leg = f.observe(lid)
	atts := f.attempts(lid, AttemptRelease)
	require.Len(t, atts, 2, "once the first can never land, a second is relayed")
	require.Equal(t, StateMessageInFlight, leg.Status)
	f.produce(r.DestinationChain, 1+itDepth)
	leg = f.observe(lid)
	require.Equal(t, StateReconciled, leg.Status)
	assert.Equal(t, 1, f.journals(leg, ledger.KindBridgeDestinationFinalized))
	assert.Equal(t, uint64(200_000), f.supply(r.DestinationChain, r.DestinationMint), "minted exactly once")
	assert.Equal(t, "200000", f.wallet(c, r.DestinationAssetID))
	f.noPhantomCapital(r, c, 1_000_000, "final")

	// Finality never reverts: a deep reorg removes no finalized block.
	_, _ = f.host.Reorg(f.ctx, r.DestinationChain, 64, devnet.ReorgDrop)
	_, _ = f.host.Reorg(f.ctx, r.SourceChain, 64, devnet.ReorgDrop)
	assert.Equal(t, devnet.TxFinalized, f.status(r.DestinationChain, atts[1].TxID).State)
	assert.Equal(t, devnet.TxFinalized, f.status(r.SourceChain, leg.LockTxID).State)
	res2, err := f.svc.Advance(f.ctx, lid)
	require.NoError(t, err)
	assert.Equal(t, StateReconciled, res2.Leg.Status)
	assert.Equal(t, uint64(200_000), f.supply(r.DestinationChain, r.DestinationMint))
	assert.True(t, f.reconcile(r).Matched())
}
