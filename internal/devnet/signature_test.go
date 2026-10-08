package devnet

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

// transferTx is a USD transfer alice -> bob paid by w.payer, signed by both.
func (w *world) transferTx(amount uint64, note string) Transaction {
	w.t.Helper()
	tx, err := Sign(w.msg(
		TokenTransfer{Mint: w.usd, FromOwner: w.alice.PublicKey(), ToOwner: w.bob.PublicKey(), Amount: amount},
		memo(note),
	), w.payer, w.alice)
	require.NoError(w.t, err)
	return tx
}

func encode(t require.TestingT, tx Transaction) []byte {
	if h, ok := t.(interface{ Helper() }); ok {
		h.Helper() // *testing.T has Helper; *rapid.T does not
	}
	raw, err := tx.Encode()
	require.NoError(t, err)
	return raw
}

func TestSubmit_SignatureFailures(t *testing.T) {
	w := newWorld(t)
	w.mintUSD(w.alice.PublicKey(), 100)
	good := w.transferTx(10, "sig")
	msgBytes, err := good.Message.Bytes()
	require.NoError(t, err)

	t.Run("missing signer", func(t *testing.T) {
		tx := good
		tx.Signatures = tx.Signatures[:1] // the payer signed, alice did not
		_, err := w.c.Submit(encode(t, tx))
		requireReason(t, err, errs.CodeValidationFailed, CodeSignatureMissing)
		e, ok := errs.As(err)
		require.True(t, ok)
		require.Equal(t, w.alice.PublicKey().String(), e.Fields["signer"])
	})
	t.Run("no signatures at all", func(t *testing.T) {
		tx := good
		tx.Signatures = nil
		_, err := w.c.Submit(encode(t, tx))
		requireReason(t, err, errs.CodeValidationFailed, CodeSignatureMissing)
	})
	t.Run("signature over a different message", func(t *testing.T) {
		other := w.transferTx(11, "sig") // same signers, different amount
		tx := good
		tx.Signatures = other.Signatures
		_, err := w.c.Submit(encode(t, tx))
		requireReason(t, err, errs.CodeValidationFailed, CodeSignatureInvalid)
	})
	t.Run("signature over the transaction bytes instead of the message", func(t *testing.T) {
		raw := encode(t, good)
		sig, err := w.payer.Sign(raw)
		require.NoError(t, err)
		tx := good
		tx.Signatures = []Signature{sig, good.Signatures[1]}
		_, err = w.c.Submit(encode(t, tx))
		requireReason(t, err, errs.CodeValidationFailed, CodeSignatureInvalid)
	})
	t.Run("extra signature", func(t *testing.T) {
		extra, err := w.carol.Sign(msgBytes)
		require.NoError(t, err)
		tx := good
		tx.Signatures = append(append([]Signature(nil), good.Signatures...), extra)
		_, err = w.c.Submit(encode(t, tx))
		requireReason(t, err, errs.CodeValidationFailed, CodeSignatureExtra)
	})
	t.Run("right count, wrong signer in a position", func(t *testing.T) {
		bobSig, err := w.bob.Sign(msgBytes)
		require.NoError(t, err)
		tx := good
		tx.Signatures = []Signature{good.Signatures[0], bobSig}
		_, err = w.c.Submit(encode(t, tx))
		requireReason(t, err, errs.CodeValidationFailed, CodeSignatureInvalid)
	})
	t.Run("signatures in the wrong order", func(t *testing.T) {
		tx := good
		tx.Signatures = []Signature{good.Signatures[1], good.Signatures[0]}
		_, err := w.c.Submit(encode(t, tx))
		requireReason(t, err, errs.CodeValidationFailed, CodeSignatureInvalid)
	})
	t.Run("a flipped bit in the message", func(t *testing.T) {
		raw := encode(t, good)
		idx := txOverhead - 1 + len(msgBytes) - 1 // the last byte of the memo
		raw[idx] ^= 1
		_, err := w.c.Submit(raw)
		requireReason(t, err, errs.CodeValidationFailed, CodeSignatureInvalid)
	})

	// None of the rejected submissions reached the mempool or moved value,
	// and the honest transaction still goes through afterwards.
	require.Zero(t, w.c.PendingCount())
	id := w.submit(encode(t, good))
	w.c.ProduceBlock()
	require.Equal(t, TxConfirmed, w.c.Status(id).State)
	require.Equal(t, uint64(90), w.usdOf(w.alice))
	require.NoError(t, w.c.audit())
}

func TestSubmit_RejectsStructuralAndRuleViolations(t *testing.T) {
	w := newWorld(t)

	_, err := w.c.Submit([]byte("garbage"))
	requireReason(t, err, errs.CodeValidationFailed, CodeMalformedTransaction)

	other := w.msg(memo("x"))
	other.ChainID = "sbx-ethereum"
	_, err = w.c.Submit(signed(t, other, w.payer))
	requireReason(t, err, errs.CodeValidationFailed, CodeWrongChain)

	zero := w.msg(SystemTransfer{From: w.alice.PublicKey(), To: w.bob.PublicKey()}) // zero lamports
	tx, err := Sign(zero, w.payer, w.alice)
	require.NoError(t, err)
	_, err = w.c.Submit(encode(t, tx))
	requireReason(t, err, errs.CodeValidationFailed, CodeInvalidMessage)
	require.Zero(t, w.c.PendingCount())
}

// weakKey is the Ed25519 identity point. crypto/ed25519 verifies
// cofactorless and does not reject small-order public keys, so under this
// key every message has more than one valid signature. A real chain must
// not let that turn into a replay, which is why replay protection is keyed
// by message as well as by signature.
func weakKey() PublicKey { return PublicKey{1} }

// weakSignatures returns two different signatures that verify for every
// message under weakKey: (R = identity, S = 0) and (R = base point, S = 1).
func weakSignatures() (Signature, Signature) {
	a := Signature{1}
	var b Signature
	b[0] = 0x58
	for i := 1; i < 32; i++ {
		b[i] = 0x66
	}
	b[32] = 1
	return a, b
}

func TestSubmit_ReplayProtectionIsKeyedByMessageAndTxID(t *testing.T) {
	w := newWorld(t)
	sigA, sigB := weakSignatures()
	weak := weakKey()
	probe := []byte("any message at all")
	if !Verify(weak, probe, sigA) || !Verify(weak, probe, sigB) {
		t.Skip("crypto/ed25519 now rejects small-order keys; a second signature over one message cannot be produced")
	}
	require.NoError(t, w.c.Airdrop(weak, testFunds))

	m := w.msg(memo("weak payer"))
	m.FeePayer = weak
	first := Transaction{Message: m, Signatures: []Signature{sigA}}
	id := w.submit(encode(t, first))

	t.Run("second signature over the same message", func(t *testing.T) {
		_, err := w.c.Submit(encode(t, Transaction{Message: m, Signatures: []Signature{sigB}}))
		requireReason(t, err, errs.CodeConflict, CodeDuplicateMessage)
		e, ok := errs.As(err)
		require.True(t, ok)
		require.Equal(t, string(id), e.Fields["existing_tx_id"])
	})
	t.Run("same TxID over a different message", func(t *testing.T) {
		m2 := m
		m2.Instructions = []Instruction{memo("a different message")}
		_, err := w.c.Submit(encode(t, Transaction{Message: m2, Signatures: []Signature{sigA}}))
		requireReason(t, err, errs.CodeConflict, CodeTxIDConflict)
	})

	w.c.ProduceBlocks(1 + testDepth) // the including block, then the depth
	require.Equal(t, TxFinalized, w.c.Status(id).State)
	feeBefore := w.c.Balance(weak)
	_, err := w.c.Submit(encode(t, Transaction{Message: m, Signatures: []Signature{sigB}}))
	requireReason(t, err, errs.CodeConflict, CodeDuplicateMessage)
	w.c.ProduceBlock()
	require.Equal(t, feeBefore, w.c.Balance(weak), "the message applied exactly once")
	require.NoError(t, w.c.audit())
}
