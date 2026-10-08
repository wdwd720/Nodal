package devnet

import (
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstruction_SystemTransfer(t *testing.T) {
	w := newWorld(t)
	collector := FeeCollectorAddress(testChainID)

	id := w.land([]Keypair{w.alice}, SystemTransfer{From: w.alice.PublicKey(), To: w.bob.PublicKey(), Lamports: 250})
	require.Equal(t, TxConfirmed, w.c.Status(id).State)
	require.Equal(t, uint64(testFunds-250), w.nativeOf(w.alice))
	require.Equal(t, uint64(testFunds+250), w.nativeOf(w.bob))
	require.Equal(t, uint64(testFunds-testFee), w.nativeOf(w.payer))
	require.Equal(t, uint64(testFee), w.c.Balance(collector))

	failed := w.land([]Keypair{w.alice}, SystemTransfer{From: w.alice.PublicKey(), To: w.bob.PublicKey(), Lamports: testFunds})
	w.requireFailed(failed, CodeInsufficientFunds)
	require.Equal(t, uint64(testFunds-250), w.nativeOf(w.alice), "a failed transfer moves nothing")
	require.Equal(t, uint64(testFunds-2*testFee), w.nativeOf(w.payer), "but the fee is still charged")
	require.Equal(t, uint64(2*testFee), w.c.Balance(collector))

	rec, err := w.c.GetTransaction(failed)
	require.NoError(t, err)
	require.Equal(t, uint64(testFee), rec.Fee)
	require.Equal(t, uint64(4*testFunds), w.c.NativeSupply(), "transfers and fees conserve native supply")
	require.NoError(t, w.c.audit())
}

func TestInstruction_TokenTransfer(t *testing.T) {
	w := newWorld(t)
	w.mintUSD(w.alice.PublicKey(), 1_000)

	id := w.land([]Keypair{w.alice}, TokenTransfer{Mint: w.usd, FromOwner: w.alice.PublicKey(), ToOwner: w.bob.PublicKey(), Amount: 400})
	require.Equal(t, TxConfirmed, w.c.Status(id).State)
	require.Equal(t, uint64(600), w.usdOf(w.alice))
	require.Equal(t, uint64(400), w.usdOf(w.bob))
	require.Equal(t, uint64(1_000), w.usdSupply(), "a transfer leaves supply alone")

	over := w.land([]Keypair{w.alice}, TokenTransfer{Mint: w.usd, FromOwner: w.alice.PublicKey(), ToOwner: w.bob.PublicKey(), Amount: 601})
	w.requireFailed(over, CodeInsufficientTokenBalance)

	unknown := w.land([]Keypair{w.alice}, TokenTransfer{Mint: testAddr("nope"), FromOwner: w.alice.PublicKey(), ToOwner: w.bob.PublicKey(), Amount: 1})
	w.requireFailed(unknown, CodeMintNotFound)

	self := w.land([]Keypair{w.alice}, TokenTransfer{Mint: w.usd, FromOwner: w.alice.PublicKey(), ToOwner: w.alice.PublicKey(), Amount: 600})
	require.Equal(t, TxConfirmed, w.c.Status(self).State)
	require.Equal(t, uint64(600), w.usdOf(w.alice), "a self-transfer is a no-op")

	require.NoError(t, w.c.SetMintFrozen(w.usd, true))
	frozen := w.land([]Keypair{w.alice}, TokenTransfer{Mint: w.usd, FromOwner: w.alice.PublicKey(), ToOwner: w.bob.PublicKey(), Amount: 1})
	w.requireFailed(frozen, CodeMintFrozen)
	require.NoError(t, w.c.SetMintFrozen(w.usd, false))
	thawed := w.land([]Keypair{w.alice}, TokenTransfer{Mint: w.usd, FromOwner: w.alice.PublicKey(), ToOwner: w.bob.PublicKey(), Amount: 1})
	require.Equal(t, TxConfirmed, w.c.Status(thawed).State)
	require.Equal(t, uint64(599), w.usdOf(w.alice))
	require.NoError(t, w.c.audit())
}

func TestInstruction_TokenMintTo(t *testing.T) {
	w := newWorld(t)
	w.mintUSD(w.bob.PublicKey(), 500)
	require.Equal(t, uint64(500), w.usdOf(w.bob))
	require.Equal(t, uint64(500), w.usdSupply())

	// Signed, but by a key that is not the mint's authority.
	impostor := w.land([]Keypair{w.carol},
		TokenMintTo{Mint: w.usd, ToOwner: w.carol.PublicKey(), Amount: 1, Authority: w.carol.PublicKey()})
	w.requireFailed(impostor, CodeAuthoritySignatureMissing)
	require.Zero(t, w.usdOf(w.carol))

	unknown := w.land([]Keypair{w.issuer},
		TokenMintTo{Mint: testAddr("nope"), ToOwner: w.bob.PublicKey(), Amount: 1, Authority: w.issuer.PublicKey()})
	w.requireFailed(unknown, CodeMintNotFound)

	// Supply is capped at the uint64 range.
	rest := ^uint64(0) - 500
	w.mintUSD(w.carol.PublicKey(), rest)
	require.Equal(t, ^uint64(0), w.usdSupply())
	overflow := w.land([]Keypair{w.issuer},
		TokenMintTo{Mint: w.usd, ToOwner: w.bob.PublicKey(), Amount: 1, Authority: w.issuer.PublicKey()})
	w.requireFailed(overflow, CodeArithmeticOverflow)
	require.Equal(t, ^uint64(0), w.usdSupply())

	require.NoError(t, w.c.SetMintFrozen(w.usd, true))
	frozen := w.land([]Keypair{w.issuer},
		TokenMintTo{Mint: w.usd, ToOwner: w.bob.PublicKey(), Amount: 1, Authority: w.issuer.PublicKey()})
	w.requireFailed(frozen, CodeMintFrozen)
	require.NoError(t, w.c.audit())
}

func TestInstruction_TokenBurn(t *testing.T) {
	w := newWorld(t)
	w.mintUSD(w.alice.PublicKey(), 300)

	id := w.land([]Keypair{w.alice}, TokenBurn{Mint: w.usd, Owner: w.alice.PublicKey(), Amount: 120})
	require.Equal(t, TxConfirmed, w.c.Status(id).State)
	require.Equal(t, uint64(180), w.usdOf(w.alice))
	require.Equal(t, uint64(180), w.usdSupply(), "a burn reduces supply")

	over := w.land([]Keypair{w.alice}, TokenBurn{Mint: w.usd, Owner: w.alice.PublicKey(), Amount: 181})
	w.requireFailed(over, CodeInsufficientTokenBalance)
	require.Equal(t, uint64(180), w.usdSupply())

	require.NoError(t, w.c.SetMintFrozen(w.usd, true))
	frozen := w.land([]Keypair{w.alice}, TokenBurn{Mint: w.usd, Owner: w.alice.PublicKey(), Amount: 1})
	w.requireFailed(frozen, CodeMintFrozen)
	require.NoError(t, w.c.audit())
}

// TestInstruction_MemoBindsAPlanHash: a MEMO carries a settlement-plan hash
// inside the signed message, so the binding is covered by every signature
// and readable back from the chain.
func TestInstruction_MemoBindsAPlanHash(t *testing.T) {
	w := newWorld(t)
	w.mintUSD(w.alice.PublicKey(), 50)
	plan := sha256.Sum256([]byte("settlement plan v7"))
	before := w.c.StateHash()

	id := w.land([]Keypair{w.alice},
		TokenTransfer{Mint: w.usd, FromOwner: w.alice.PublicKey(), ToOwner: w.bob.PublicKey(), Amount: 50},
		Memo{Data: plan[:]})
	rec, err := w.c.GetTransaction(id)
	require.NoError(t, err)
	require.Equal(t, TxConfirmed, rec.Status.State)
	require.Len(t, rec.Transaction.Message.Instructions, 2)
	m, ok := rec.Transaction.Message.Instructions[1].(Memo)
	require.True(t, ok)
	require.Equal(t, plan[:], m.Data)
	require.NotEqual(t, before, w.c.StateHash())

	// A memo alone changes nothing but the fee.
	payerBefore := w.nativeOf(w.payer)
	only := w.land(nil, Memo{Data: plan[:]})
	require.Equal(t, TxConfirmed, w.c.Status(only).State)
	require.Equal(t, payerBefore-testFee, w.nativeOf(w.payer))

	// The record is a copy: mutating it does not reach the chain.
	m.Data[0] ^= 0xFF
	again, err := w.c.GetTransaction(id)
	require.NoError(t, err)
	m2, ok := again.Transaction.Message.Instructions[1].(Memo)
	require.True(t, ok)
	require.Equal(t, plan[:], m2.Data)
}

func TestInstruction_BridgeLock(t *testing.T) {
	bridged := func(cfg *ChainConfig) { cfg.Bridge = &BridgeConfig{Authority: testKey("attester").PublicKey()} }
	w := newWorld(t, bridged)
	w.mintUSD(w.alice.PublicKey(), 100)
	escrow := BridgeEscrowAddress(testChainID)
	lock := func(amount, nonce uint64) BridgeLock {
		return BridgeLock{Mint: w.usd, Owner: w.alice.PublicKey(), Amount: amount, DestChain: "sbx-ethereum", DestOwner: w.bob.PublicKey(), Nonce: nonce}
	}

	id := w.land([]Keypair{w.alice}, lock(40, 1))
	require.Equal(t, TxConfirmed, w.c.Status(id).State)
	require.Equal(t, uint64(60), w.usdOf(w.alice))
	require.Equal(t, uint64(40), w.c.TokenBalance(escrow, w.usd))
	require.Equal(t, uint64(100), w.usdSupply(), "locking moves tokens into escrow; supply is unchanged")

	reused := w.land([]Keypair{w.alice}, lock(1, 1))
	w.requireFailed(reused, CodeBridgeNonceUsed)

	over := w.land([]Keypair{w.alice}, lock(61, 2))
	w.requireFailed(over, CodeInsufficientTokenBalance)
	require.Equal(t, uint64(40), w.c.TokenBalance(escrow, w.usd))

	// The failed lock did not consume nonce 2.
	ok := w.land([]Keypair{w.alice}, lock(60, 2))
	require.Equal(t, TxConfirmed, w.c.Status(ok).State)
	require.Equal(t, uint64(100), w.c.TokenBalance(escrow, w.usd))

	plain := newWorld(t)
	plain.mintUSD(plain.alice.PublicKey(), 10)
	off := plain.land([]Keypair{plain.alice}, BridgeLock{Mint: plain.usd, Owner: plain.alice.PublicKey(), Amount: 1, DestChain: "sbx-ethereum", DestOwner: plain.bob.PublicKey(), Nonce: 1})
	plain.requireFailed(off, CodeBridgeNotConfigured)
	require.NoError(t, w.c.audit())
	require.NoError(t, plain.c.audit())
}

func TestExecution_FailedTransactionIsAtomic(t *testing.T) {
	w := newWorld(t)
	w.mintUSD(w.alice.PublicKey(), 100)
	collector := FeeCollectorAddress(testChainID)
	supply, payer, coll := w.usdSupply(), w.nativeOf(w.payer), w.c.Balance(collector)
	before := map[string]uint64{"alice": w.usdOf(w.alice), "bob": w.usdOf(w.bob), "carol": w.usdOf(w.carol)}
	nativeAlice := w.nativeOf(w.alice)

	// Three instructions succeed, the fourth fails: none of them may stick.
	id := w.land([]Keypair{w.alice},
		TokenTransfer{Mint: w.usd, FromOwner: w.alice.PublicKey(), ToOwner: w.bob.PublicKey(), Amount: 30},
		SystemTransfer{From: w.alice.PublicKey(), To: w.carol.PublicKey(), Lamports: 77},
		TokenBurn{Mint: w.usd, Owner: w.alice.PublicKey(), Amount: 20},
		TokenTransfer{Mint: w.usd, FromOwner: w.alice.PublicKey(), ToOwner: w.carol.PublicKey(), Amount: 51},
	)
	w.requireFailed(id, CodeInsufficientTokenBalance)
	require.Equal(t, before["alice"], w.usdOf(w.alice))
	require.Equal(t, before["bob"], w.usdOf(w.bob))
	require.Equal(t, before["carol"], w.usdOf(w.carol))
	require.Equal(t, nativeAlice, w.nativeOf(w.alice))
	require.Equal(t, supply, w.usdSupply())
	require.Equal(t, payer-testFee, w.nativeOf(w.payer), "the fee is the only thing that moved")
	require.Equal(t, coll+testFee, w.c.Balance(collector))
	require.NoError(t, w.c.audit())
}

func TestExecution_FeePayerWhoCannotPayMovesNothing(t *testing.T) {
	w := newWorld(t)
	broke := testKey("broke")
	require.NoError(t, w.c.Airdrop(broke.PublicKey(), testFee-1))
	collector := w.c.Balance(FeeCollectorAddress(testChainID))

	m := w.msg(SystemTransfer{From: w.alice.PublicKey(), To: w.bob.PublicKey(), Lamports: 5})
	m.FeePayer = broke.PublicKey()
	id := w.submit(signed(t, m, broke, w.alice))
	w.c.ProduceBlock()
	w.requireFailed(id, CodeInsufficientFundsForFee)
	rec, err := w.c.GetTransaction(id)
	require.NoError(t, err)
	require.Zero(t, rec.Fee)
	require.Equal(t, uint64(testFee-1), w.c.Balance(broke.PublicKey()))
	require.Equal(t, collector, w.c.Balance(FeeCollectorAddress(testChainID)))
	require.Equal(t, uint64(testFunds), w.nativeOf(w.alice))
	require.NoError(t, w.c.audit())
}

func TestExecution_FreeChainChargesNoFee(t *testing.T) {
	w := newWorld(t, func(cfg *ChainConfig) { cfg.FeePerTx = 0 })
	id := w.land([]Keypair{w.alice}, SystemTransfer{From: w.alice.PublicKey(), To: w.bob.PublicKey(), Lamports: 1})
	require.Equal(t, TxConfirmed, w.c.Status(id).State)
	require.Equal(t, uint64(testFunds), w.nativeOf(w.payer))
	rec, err := w.c.GetTransaction(id)
	require.NoError(t, err)
	require.Zero(t, rec.Fee)
}

func TestOperator_ControlsValidateTheirArguments(t *testing.T) {
	w := newWorld(t)
	requireReason(t, w.c.Airdrop(PublicKey{}, 1), "VALIDATION_FAILED", CodeInvalidArgument)
	requireReason(t, w.c.Airdrop(w.alice.PublicKey(), 0), "VALIDATION_FAILED", CodeInvalidArgument)
	requireReason(t, w.c.Airdrop(w.alice.PublicKey(), ^uint64(0)), "OVERFLOW", CodeArithmeticOverflow)

	requireReason(t, w.c.CreateMint(MintConfig{Address: w.usd, Authority: w.issuer.PublicKey()}), "CONFLICT", CodeMintExists)
	requireReason(t, w.c.CreateMint(MintConfig{Authority: w.issuer.PublicKey()}), "VALIDATION_FAILED", CodeInvalidArgument)
	requireReason(t, w.c.CreateMint(MintConfig{Address: testAddr("m2")}), "VALIDATION_FAILED", CodeInvalidArgument)
	requireReason(t, w.c.CreateMint(MintConfig{Address: testAddr("m2"), Authority: w.issuer.PublicKey(), Decimals: MaxDecimals + 1}), "VALIDATION_FAILED", CodeInvalidArgument)
	requireReason(t, w.c.SetMintFrozen(testAddr("nope"), true), "NOT_FOUND", CodeMintNotFound)

	_, err := w.c.Supply(testAddr("nope"))
	requireReason(t, err, "NOT_FOUND", CodeMintNotFound)
	info, err := w.c.Mint(w.usd)
	require.NoError(t, err)
	require.Equal(t, MintInfo{MintConfig: MintConfig{Address: w.usd, Decimals: 6, Authority: w.issuer.PublicKey()}}, info)
}
