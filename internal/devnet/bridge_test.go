package devnet

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

const (
	srcChain ChainID = "sbx-solana"
	dstChain ChainID = "sbx-ethereum"
)

// bridgeWorld is two bridged chains: alice holds usdA on the source, bob
// receives usdB on the destination, and the attester is the bridge authority
// on both.
type bridgeWorld struct {
	t                testing.TB
	net              *Network
	a, b             *Chain
	attester         Keypair
	payer, relayer   Keypair // fee payers on a and b
	issuerA, issuerB Keypair
	usdA, usdB       PublicKey
	alice, bob       Keypair
}

func newBridgeWorld(t testing.TB) *bridgeWorld {
	t.Helper()
	bw := &bridgeWorld{
		t: t, net: NewNetwork(),
		attester: testKey("attester"), payer: testKey("payer"), relayer: testKey("relayer"),
		issuerA: testKey("issuer-a"), issuerB: testKey("issuer-b"),
		usdA: testAddr("usd-a"), usdB: testAddr("usd-b"),
		alice: testKey("alice"), bob: testKey("bob"),
	}
	bridged := func(id ChainID) ChainConfig {
		cfg := testConfig(id)
		cfg.Bridge = &BridgeConfig{Authority: bw.attester.PublicKey()}
		return cfg
	}
	var err error
	bw.a, err = bw.net.AddChain(bridged(srcChain))
	require.NoError(t, err)
	bw.b, err = bw.net.AddChain(bridged(dstChain))
	require.NoError(t, err)
	require.NoError(t, bw.a.Airdrop(bw.payer.PublicKey(), testFunds))
	require.NoError(t, bw.b.Airdrop(bw.relayer.PublicKey(), testFunds))
	require.NoError(t, bw.a.CreateMint(MintConfig{Address: bw.usdA, Decimals: 6, Authority: bw.issuerA.PublicKey()}))
	require.NoError(t, bw.b.CreateMint(MintConfig{Address: bw.usdB, Decimals: 6, Authority: bw.issuerB.PublicKey()}))

	mint := Message{
		ChainID: srcChain, RecentBlockhash: bw.a.LatestBlockhash(), FeePayer: bw.payer.PublicKey(),
		Instructions: []Instruction{TokenMintTo{Mint: bw.usdA, ToOwner: bw.alice.PublicKey(), Amount: 1_000, Authority: bw.issuerA.PublicKey()}},
	}
	id, err := bw.a.Submit(signed(t, mint, bw.payer, bw.issuerA))
	require.NoError(t, err)
	bw.a.ProduceBlock()
	require.Equal(t, TxConfirmed, bw.a.Status(id).State)
	return bw
}

// lock submits (without producing a block) alice's lock of amount to bob.
func (bw *bridgeWorld) lock(amount, nonce uint64) TxID {
	bw.t.Helper()
	m := Message{
		ChainID: srcChain, RecentBlockhash: bw.a.LatestBlockhash(), FeePayer: bw.payer.PublicKey(),
		Instructions: []Instruction{BridgeLock{
			Mint: bw.usdA, Owner: bw.alice.PublicKey(), Amount: amount,
			DestChain: dstChain, DestOwner: bw.bob.PublicKey(), Nonce: nonce,
		}},
	}
	id, err := bw.a.Submit(signed(bw.t, m, bw.payer, bw.alice))
	require.NoError(bw.t, err)
	return id
}

// finalizeA produces enough blocks on the source to finalize everything
// included so far.
func (bw *bridgeWorld) finalizeA() { bw.a.ProduceBlocks(1 + testDepth) }

// releaseMsg reads the lock on the source and builds its release.
func (bw *bridgeWorld) releaseMsg(lockID TxID) (Message, error) {
	bw.t.Helper()
	lt, err := bw.a.LockedTransfer(lockID)
	require.NoError(bw.t, err)
	return BuildRelease(lt, ReleaseParams{
		DestMint: bw.usdB, Authority: bw.attester.PublicKey(),
		FeePayer: bw.relayer.PublicKey(), RecentBlockhash: bw.b.LatestBlockhash(),
	})
}

// relay builds, signs and encodes the release for lockID.
func (bw *bridgeWorld) relay(lockID TxID) []byte {
	bw.t.Helper()
	m, err := bw.releaseMsg(lockID)
	require.NoError(bw.t, err)
	return signed(bw.t, m, bw.relayer, bw.attester)
}

func (bw *bridgeWorld) submitB(raw []byte) TxID {
	bw.t.Helper()
	id, err := bw.b.Submit(raw)
	require.NoError(bw.t, err)
	return id
}

func (bw *bridgeWorld) supplyB() uint64 {
	bw.t.Helper()
	s, err := bw.b.Supply(bw.usdB)
	require.NoError(bw.t, err)
	return s
}

func TestBridge_LockFinalizeRelease(t *testing.T) {
	bw := newBridgeWorld(t)
	lockID := bw.lock(250, 1)
	bw.a.ProduceBlock()
	require.Equal(t, TxConfirmed, bw.a.Status(lockID).State)
	escrow := BridgeEscrowAddress(srcChain)
	require.Equal(t, uint64(250), bw.a.TokenBalance(escrow, bw.usdA))
	require.Equal(t, uint64(750), bw.a.TokenBalance(bw.alice.PublicKey(), bw.usdA))

	// A confirmed lock can still be reorged away: no release for it yet.
	_, err := bw.releaseMsg(lockID)
	requireReason(t, err, errs.CodeInvalidStateTransition, CodeLockNotFinalized)

	bw.a.ProduceBlocks(testDepth)
	lt, err := bw.a.LockedTransfer(lockID)
	require.NoError(t, err)
	require.Equal(t, TxFinalized, lt.Status.State)
	m, err := bw.releaseMsg(lockID)
	require.NoError(t, err)
	require.Equal(t, dstChain, m.ChainID)
	require.Equal(t, []Instruction{BridgeRelease{
		Mint: bw.usdB, ToOwner: bw.bob.PublicKey(), Amount: 250, SourceChain: srcChain,
		SourceTxID: lockID, Nonce: 1, Authority: bw.attester.PublicKey(),
	}}, m.Instructions)
	require.Equal(t, []PublicKey{bw.relayer.PublicKey(), bw.attester.PublicKey()}, m.RequiredSigners())

	releaseID := bw.submitB(signed(t, m, bw.relayer, bw.attester))
	bw.b.ProduceBlock()
	require.Equal(t, TxConfirmed, bw.b.Status(releaseID).State)
	require.Equal(t, uint64(250), bw.b.TokenBalance(bw.bob.PublicKey(), bw.usdB))
	require.Equal(t, uint64(250), bw.supplyB(), "the release mints on the destination")
	sA, err := bw.a.Supply(bw.usdA)
	require.NoError(t, err)
	require.Equal(t, uint64(1_000), sA, "the source supply is unchanged; the tokens sit in escrow")

	bw.b.ProduceBlocks(testDepth)
	require.Equal(t, TxFinalized, bw.b.Status(releaseID).State)
	require.NoError(t, bw.a.audit())
	require.NoError(t, bw.b.audit())
}

func TestBridge_DuplicateReleaseIsRejected(t *testing.T) {
	bw := newBridgeWorld(t)
	lockID := bw.lock(100, 7)
	bw.finalizeA()
	raw := bw.relay(lockID)
	first := bw.submitB(raw)
	bw.b.ProduceBlock()
	require.Equal(t, TxConfirmed, bw.b.Status(first).State)

	// The same relay message again: idempotent, nothing new.
	require.Equal(t, first, bw.submitB(raw))
	require.Zero(t, bw.b.PendingCount())

	// A fresh relay of the same lock (new blockhash, so a new message and a
	// new TxID) lands as a FAILED transaction and mints nothing.
	dup := bw.submitB(bw.relay(lockID))
	require.NotEqual(t, first, dup)
	bw.b.ProduceBlock()
	st := bw.b.Status(dup)
	require.Equal(t, TxFailed, st.State)
	require.Equal(t, CodeBridgeAlreadyReleased, st.Err)
	require.Equal(t, uint64(100), bw.supplyB())

	// A relayer bug that reuses the nonce under another source tx id, or the
	// source tx id under another nonce, is caught either way.
	crafted := func(sourceTx TxID, nonce uint64) TxID {
		m := Message{
			ChainID: dstChain, RecentBlockhash: bw.b.LatestBlockhash(), FeePayer: bw.relayer.PublicKey(),
			Instructions: []Instruction{BridgeRelease{
				Mint: bw.usdB, ToOwner: bw.bob.PublicKey(), Amount: 100, SourceChain: srcChain,
				SourceTxID: sourceTx, Nonce: nonce, Authority: bw.attester.PublicKey(),
			}},
		}
		return bw.submitB(signed(t, m, bw.relayer, bw.attester))
	}
	sameNonce := crafted(TxIDFromSignature(Signature{0xAB}), 7)
	sameSource := crafted(lockID, 8)
	bw.b.ProduceBlock()
	for _, id := range []TxID{sameNonce, sameSource} {
		st := bw.b.Status(id)
		require.Equal(t, TxFailed, st.State)
		require.Equal(t, CodeBridgeAlreadyReleased, st.Err)
	}
	require.Equal(t, uint64(100), bw.supplyB(), "nothing is minted twice")
	require.NoError(t, bw.b.audit())
}

func TestBridge_TwoRelaysInOneBlockMintOnce(t *testing.T) {
	bw := newBridgeWorld(t)
	lockID := bw.lock(40, 1)
	bw.finalizeA()
	one := bw.submitB(bw.relay(lockID))
	m, err := bw.releaseMsg(lockID)
	require.NoError(t, err)
	m.FeePayer = bw.attester.PublicKey() // a second relayer: same lock, different message
	require.NoError(t, bw.b.Airdrop(bw.attester.PublicKey(), testFunds))
	two := bw.submitB(signed(t, m, bw.attester))
	bw.b.ProduceBlock()
	require.Equal(t, TxConfirmed, bw.b.Status(one).State)
	require.Equal(t, CodeBridgeAlreadyReleased, bw.b.Status(two).Err)
	require.Equal(t, uint64(40), bw.supplyB())
}

func TestBridge_ReleaseNeedsTheBridgeAuthority(t *testing.T) {
	bw := newBridgeWorld(t)
	lockID := bw.lock(60, 3)
	bw.finalizeA()
	lt, err := bw.a.LockedTransfer(lockID)
	require.NoError(t, err)

	impostor := testKey("impostor")
	m, err := BuildRelease(lt, ReleaseParams{
		DestMint: bw.usdB, Authority: impostor.PublicKey(),
		FeePayer: bw.relayer.PublicKey(), RecentBlockhash: bw.b.LatestBlockhash(),
	})
	require.NoError(t, err, "BuildRelease is pure; the destination decides who may attest")
	forged := bw.submitB(signed(t, m, bw.relayer, impostor))
	bw.b.ProduceBlock()
	st := bw.b.Status(forged)
	require.Equal(t, TxFailed, st.State)
	require.Equal(t, CodeAuthoritySignatureMissing, st.Err)
	require.Zero(t, bw.supplyB())

	// The forgery consumed nothing: the honest relay still goes through.
	honest := bw.submitB(bw.relay(lockID))
	bw.b.ProduceBlock()
	require.Equal(t, TxConfirmed, bw.b.Status(honest).State)
	require.Equal(t, uint64(60), bw.supplyB())
}

func TestBridge_ReorgedReleaseCanBeRelayedAgain(t *testing.T) {
	bw := newBridgeWorld(t)
	lockID := bw.lock(80, 9)
	bw.finalizeA()
	first := bw.submitB(bw.relay(lockID))
	bw.b.ProduceBlock()
	require.Equal(t, uint64(80), bw.supplyB())

	res, err := bw.b.Reorg(1, ReorgDrop)
	require.NoError(t, err)
	require.Equal(t, []TxID{first}, res.TxIDs)
	require.Equal(t, TxNotFound, bw.b.Status(first).State)
	require.Zero(t, bw.supplyB(), "the reorged release minted nothing")

	again := bw.submitB(bw.relay(lockID))
	bw.b.ProduceBlocks(1 + testDepth)
	require.Equal(t, TxFinalized, bw.b.Status(again).State)
	require.Equal(t, uint64(80), bw.supplyB(), "released exactly once")
	require.Equal(t, uint64(80), bw.b.TokenBalance(bw.bob.PublicKey(), bw.usdB))
	require.NoError(t, bw.b.audit())
}

func TestBridge_ReleaseIntoAFrozenMintFailsWithoutConsumingTheLock(t *testing.T) {
	bw := newBridgeWorld(t)
	lockID := bw.lock(10, 4)
	bw.finalizeA()
	require.NoError(t, bw.b.SetMintFrozen(bw.usdB, true))
	frozen := bw.submitB(bw.relay(lockID))
	bw.b.ProduceBlock()
	require.Equal(t, CodeMintFrozen, bw.b.Status(frozen).Err)
	require.NoError(t, bw.b.SetMintFrozen(bw.usdB, false))
	ok := bw.submitB(bw.relay(lockID))
	bw.b.ProduceBlock()
	require.Equal(t, TxConfirmed, bw.b.Status(ok).State)
	require.Equal(t, uint64(10), bw.supplyB())
}

func TestBridge_RefusesLocksThatAreNotFinalizedSuccesses(t *testing.T) {
	bw := newBridgeWorld(t)
	pending := bw.lock(1, 1)
	_, err := bw.releaseMsg(pending)
	requireReason(t, err, errs.CodeInvalidStateTransition, CodeLockNotFinalized)

	failed := bw.lock(5_000, 2) // more than alice holds
	bw.finalizeA()
	require.Equal(t, TxFinalized, bw.a.Status(pending).State)
	st := bw.a.Status(failed)
	require.Equal(t, TxFailed, st.State)
	require.True(t, st.Final)
	_, err = bw.releaseMsg(failed)
	requireReason(t, err, errs.CodeInvalidStateTransition, CodeLockNotFinalized)
}

func TestBridge_LockedTransferLookups(t *testing.T) {
	bw := newBridgeWorld(t)
	_, err := bw.a.LockedTransfer(TxIDFromSignature(Signature{9}))
	requireReason(t, err, errs.CodeNotFound, CodeTransactionNotFound)
	_, err = bw.a.LockedTransfer("bad id")
	requireReason(t, err, errs.CodeValidationFailed, CodeInvalidBase58)

	m := Message{
		ChainID: srcChain, RecentBlockhash: bw.a.LatestBlockhash(), FeePayer: bw.payer.PublicKey(),
		Instructions: []Instruction{memo("not a lock")},
	}
	id, err := bw.a.Submit(signed(t, m, bw.payer))
	require.NoError(t, err)
	_, err = bw.a.LockedTransfer(id)
	requireReason(t, err, errs.CodeValidationFailed, CodeNotABridgeLock)
}

func TestBridge_ReleaseOnAnUnbridgedChain(t *testing.T) {
	w := newWorld(t) // no BridgeConfig
	id := w.land([]Keypair{w.issuer}, BridgeRelease{
		Mint: w.usd, ToOwner: w.bob.PublicKey(), Amount: 1, SourceChain: dstChain,
		SourceTxID: TxIDFromSignature(Signature{1}), Nonce: 1, Authority: w.issuer.PublicKey(),
	})
	w.requireFailed(id, CodeBridgeNotConfigured)
	require.Zero(t, w.usdSupply())
}

func TestBridge_BuildReleaseValidatesWhatItBuilds(t *testing.T) {
	bw := newBridgeWorld(t)
	lockID := bw.lock(5, 1)
	bw.finalizeA()
	lt, err := bw.a.LockedTransfer(lockID)
	require.NoError(t, err)
	_, err = BuildRelease(lt, ReleaseParams{Authority: bw.attester.PublicKey(), FeePayer: bw.relayer.PublicKey()})
	requireReason(t, err, errs.CodeValidationFailed, CodeInvalidMessage) // zero destination mint
}

func TestBridge_LockOfAFrozenMintFailsWithoutConsumingTheNonce(t *testing.T) {
	bw := newBridgeWorld(t)
	require.NoError(t, bw.a.SetMintFrozen(bw.usdA, true))
	frozen := bw.lock(5, 1)
	bw.a.ProduceBlock()
	require.Equal(t, CodeMintFrozen, bw.a.Status(frozen).Err)
	require.NoError(t, bw.a.SetMintFrozen(bw.usdA, false))
	again := bw.lock(5, 1)
	bw.a.ProduceBlock()
	require.Equal(t, TxConfirmed, bw.a.Status(again).State)
}

func TestBridge_ReleaseCannotOverflowTheDestinationSupply(t *testing.T) {
	bw := newBridgeWorld(t)
	lockID := bw.lock(10, 1)
	bw.finalizeA()
	issue := Message{
		ChainID: dstChain, RecentBlockhash: bw.b.LatestBlockhash(), FeePayer: bw.relayer.PublicKey(),
		Instructions: []Instruction{TokenMintTo{
			Mint: bw.usdB, ToOwner: bw.bob.PublicKey(), Amount: ^uint64(0) - 5, Authority: bw.issuerB.PublicKey(),
		}},
	}
	bw.submitB(signed(t, issue, bw.relayer, bw.issuerB))
	bw.b.ProduceBlock()
	release := bw.submitB(bw.relay(lockID))
	bw.b.ProduceBlock()
	require.Equal(t, CodeArithmeticOverflow, bw.b.Status(release).Err)
	require.Equal(t, ^uint64(0)-5, bw.supplyB())
	require.NoError(t, bw.b.audit())
}
