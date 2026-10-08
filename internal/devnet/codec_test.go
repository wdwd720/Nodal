package devnet

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

// sampleInstructions returns one valid instance of every program.
func sampleInstructions() []Instruction {
	a, b, auth := testKey("alice").PublicKey(), testKey("bob").PublicKey(), testKey("issuer").PublicKey()
	mint := testAddr("usd")
	src, _ := testKey("payer").Sign([]byte("a lock transaction"))
	return []Instruction{
		SystemTransfer{From: a, To: b, Lamports: 1},
		TokenTransfer{Mint: mint, FromOwner: a, ToOwner: b, Amount: 2},
		TokenMintTo{Mint: mint, ToOwner: a, Amount: 3, Authority: auth},
		TokenBurn{Mint: mint, Owner: a, Amount: 4},
		Memo{Data: []byte("plan:0123456789abcdef")},
		BridgeLock{Mint: mint, Owner: a, Amount: 5, DestChain: "sbx-ethereum", DestOwner: b, Nonce: 6},
		BridgeRelease{
			Mint: mint, ToOwner: b, Amount: 7, SourceChain: "sbx-ethereum",
			SourceTxID: TxIDFromSignature(src), Nonce: 8, Authority: auth,
		},
	}
}

// sampleSigners are the keypairs that can sign any sampleInstructions mix.
func sampleSigners() map[PublicKey]Keypair {
	out := map[PublicKey]Keypair{}
	for _, l := range []string{"payer", "alice", "bob", "issuer"} {
		out[testKey(l).PublicKey()] = testKey(l)
	}
	return out
}

// sampleMessage wraps ixs in a message for testChainID paid by "payer".
func sampleMessage(ixs ...Instruction) Message {
	return Message{
		ChainID:         testChainID,
		RecentBlockhash: genesisHash(testChainID),
		FeePayer:        testKey("payer").PublicKey(),
		Instructions:    ixs,
	}
}

// signAll signs m with whichever sample keypairs it requires.
func signAll(t testing.TB, m Message) Transaction {
	t.Helper()
	keys := sampleSigners()
	var signers []Keypair
	for _, k := range m.RequiredSigners() {
		kp, ok := keys[k]
		require.True(t, ok, "no sample keypair for %s", k)
		signers = append(signers, kp)
	}
	tx, err := Sign(m, signers...)
	require.NoError(t, err)
	return tx
}

// validEncodings returns canonical encodings covering every program, a mixed
// message and an unsigned transaction. FuzzDecode seeds its corpus with them.
func validEncodings(t testing.TB) [][]byte {
	t.Helper()
	var out [][]byte
	ixs := sampleInstructions()
	for _, ins := range ixs {
		raw, err := signAll(t, sampleMessage(ins)).Encode()
		require.NoError(t, err)
		out = append(out, raw)
	}
	var mixed []Instruction
	for _, ins := range ixs {
		if _, isRelease := ins.(BridgeRelease); !isRelease { // a release from sbx-ethereum is fine; keep the mix varied
			mixed = append(mixed, ins)
		}
	}
	raw, err := signAll(t, sampleMessage(mixed...)).Encode()
	require.NoError(t, err)
	out = append(out, raw)
	unsigned, err := Transaction{Message: sampleMessage(memo("unsigned"))}.Encode()
	require.NoError(t, err)
	return append(out, unsigned)
}

func TestEncodeDecode_EveryProgramRoundTrips(t *testing.T) {
	for _, ins := range sampleInstructions() {
		t.Run(ins.Program().String(), func(t *testing.T) {
			tx := signAll(t, sampleMessage(ins))
			raw, err := tx.Encode()
			require.NoError(t, err)
			got, err := Decode(raw)
			require.NoError(t, err)
			require.Equal(t, tx, got)
			again, err := got.Encode()
			require.NoError(t, err)
			require.Equal(t, raw, again)
			require.NoError(t, got.Message.Validate())
			require.NoError(t, got.VerifySignatures())
		})
	}
}

func TestDecode_ResultDoesNotAliasInput(t *testing.T) {
	raw, err := signAll(t, sampleMessage(memo("alias"))).Encode()
	require.NoError(t, err)
	tx, err := Decode(raw)
	require.NoError(t, err)
	for i := range raw {
		raw[i] = 0xFF
	}
	m, ok := tx.Message.Instructions[0].(Memo)
	require.True(t, ok)
	require.Equal(t, "alias", string(m.Data))
}

// TestWireFormat_MatchesTheDocumentedLayout builds a transaction byte by
// byte from the layout in doc.go and requires Encode to agree.
func TestWireFormat_MatchesTheDocumentedLayout(t *testing.T) {
	payer, alice, bob := testKey("payer"), testKey("alice"), testKey("bob")
	bh := Hash{9, 9, 9}
	m := Message{
		ChainID:         "sbx-solana",
		RecentBlockhash: bh,
		FeePayer:        payer.PublicKey(),
		Instructions: []Instruction{
			SystemTransfer{From: alice.PublicKey(), To: bob.PublicKey(), Lamports: 0x0102030405060708},
			Memo{Data: []byte("hi")},
		},
	}
	tx, err := Sign(m, payer, alice)
	require.NoError(t, err)

	var msg []byte
	msg = append(msg, "ndvm"...)
	msg = append(msg, 1)
	msg = append(msg, byte(len("sbx-solana")))
	msg = append(msg, "sbx-solana"...)
	msg = append(msg, bh[:]...)
	pk := payer.PublicKey()
	msg = append(msg, pk[:]...)
	msg = append(msg, 2) // instruction count
	msg = append(msg, byte(ProgramSystemTransfer))
	msg = binary.LittleEndian.AppendUint16(msg, 72)
	from, to := alice.PublicKey(), bob.PublicKey()
	msg = append(msg, from[:]...)
	msg = append(msg, to[:]...)
	msg = append(msg, 8, 7, 6, 5, 4, 3, 2, 1) // little-endian lamports
	msg = append(msg, byte(ProgramMemo))
	msg = binary.LittleEndian.AppendUint16(msg, 2)
	msg = append(msg, "hi"...)

	gotMsg, err := m.Bytes()
	require.NoError(t, err)
	require.Equal(t, msg, gotMsg)

	var want []byte
	want = append(want, "ndvt"...)
	want = append(want, 1)
	want = binary.LittleEndian.AppendUint16(want, uint16(len(msg))) // #nosec G115 -- small test message
	want = append(want, msg...)
	want = append(want, 2) // signature count: payer, alice
	want = append(want, tx.Signatures[0][:]...)
	want = append(want, tx.Signatures[1][:]...)
	raw, err := tx.Encode()
	require.NoError(t, err)
	require.Equal(t, want, raw)
	require.True(t, Verify(payer.PublicKey(), msg, tx.Signatures[0]), "signers sign the exact message bytes")
}

func TestDecode_RejectsMalformedInput(t *testing.T) {
	valid := validEncodings(t)[0] // a SYSTEM_TRANSFER
	tx, err := Decode(valid)
	require.NoError(t, err)
	msg, err := tx.Message.Bytes()
	require.NoError(t, err)

	// frame re-wraps message bytes in a transaction with the given signatures.
	frame := func(msgBytes []byte, sigs int) []byte {
		out := append([]byte("ndvt"), 1)
		out = binary.LittleEndian.AppendUint16(out, uint16(len(msgBytes))) // #nosec G115 -- test sizes
		out = append(out, msgBytes...)
		out = append(out, byte(sigs))
		return append(out, make([]byte, sigs*SignatureSize)...)
	}
	// patch returns a copy of b with b[i] = v.
	patch := func(b []byte, i int, v byte) []byte {
		c := bytes.Clone(b)
		c[i] = v
		return c
	}
	// Offsets inside the message.
	const (
		msgVersionAt = 4
		chainLenAt   = 5
		chainAt      = 6
		countAt      = chainAt + len(testChainID) + 2*HashSize
		programAt    = countAt + 1
		dataLenAt    = programAt + 1
	)
	withData := func(program Program, data []byte) []byte {
		m := bytes.Clone(msg[:programAt])
		m = append(m, byte(program))
		m = binary.LittleEndian.AppendUint16(m, uint16(len(data))) // #nosec G115 -- test sizes
		return append(m, data...)
	}
	lockData := func(destChain string) []byte {
		var d []byte
		d = append(d, make([]byte, 2*PublicKeySize+8)...)
		d = append(d, byte(len(destChain)))
		d = append(d, destChain...)
		return append(d, make([]byte, PublicKeySize+8)...)
	}

	cases := map[string][]byte{
		"empty":                           nil,
		"short magic":                     []byte("nd"),
		"wrong magic":                     append([]byte("xxxx"), valid[4:]...),
		"unknown tx version":              patch(valid, 4, 2),
		"truncated length":                valid[:6],
		"message shorter than its length": valid[:40],
		"oversize":                        append(bytes.Clone(valid), make([]byte, MaxTransactionSize)...),
		"trailing byte":                   append(bytes.Clone(valid), 0),
		"truncated signature":             valid[:len(valid)-1],
		"too many signatures":             frame(msg, MaxSignatures+1),
		"unknown message version":         frame(patch(msg, msgVersionAt, 9), 1),
		"wrong message magic":             frame(patch(msg, 0, 'x'), 1),
		"uppercase chain id":              frame(patch(msg, chainAt, 'S'), 1),
		"empty chain id":                  frame(append(append(bytes.Clone(msg[:chainLenAt]), 0), msg[chainAt+len(testChainID):]...), 1),
		"zero instructions":               frame(patch(msg[:countAt+1], countAt, 0), 1),
		"seventeen instructions":          frame(patch(msg, countAt, MaxInstructions+1), 1),
		"count above body":                frame(patch(msg, countAt, 2), 1),
		"program zero":                    frame(patch(msg, programAt, 0), 1),
		"program eight":                   frame(patch(msg, programAt, 8), 1),
		"program 255":                     frame(patch(msg, programAt, 255), 1),
		"short fixed data":                frame(withData(ProgramSystemTransfer, make([]byte, systemTransferSize-1)), 1),
		"long fixed data":                 frame(withData(ProgramTokenTransfer, make([]byte, tokenTransferSize+1)), 1),
		"empty memo":                      frame(withData(ProgramMemo, nil), 1),
		"oversize memo":                   frame(withData(ProgramMemo, make([]byte, MaxMemoSize+1)), 1),
		"lock with bad chain":             frame(withData(ProgramBridgeLock, lockData("Bad")), 1),
		"lock with trailing data":         frame(withData(ProgramBridgeLock, append(lockData("sbx-ethereum"), 0)), 1),
		"lock truncated":                  frame(withData(ProgramBridgeLock, lockData("sbx-ethereum")[:50]), 1),
		"release truncated":               frame(withData(ProgramBridgeRelease, make([]byte, 60)), 1),
		"data length past end":            frame(patch(msg, dataLenAt, 0xFF), 1),
		"message with trailing bytes":     frame(append(bytes.Clone(msg), 0), 1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Decode(raw)
			requireReason(t, err, errs.CodeValidationFailed, CodeMalformedTransaction)
		})
	}

	// The control: the helpers produce a decodable transaction when nothing
	// is broken, so every case above fails for its stated reason.
	_, err = Decode(frame(msg, 1))
	require.NoError(t, err)
	_, err = Decode(frame(withData(ProgramBridgeLock, lockData("sbx-ethereum")), 1))
	require.NoError(t, err)
}

func TestEncode_RejectsWhatDecodeWouldReject(t *testing.T) {
	ok := sampleMessage(memo("x"))
	cases := map[string]Message{
		"bad chain id":     func() Message { m := ok; m.ChainID = "Bad"; return m }(),
		"no instructions":  func() Message { m := ok; m.Instructions = nil; return m }(),
		"too many":         func() Message { m := ok; m.Instructions = make([]Instruction, MaxInstructions+1); return m }(),
		"nil instruction":  func() Message { m := ok; m.Instructions = []Instruction{nil}; return m }(),
		"nil pointer":      func() Message { m := ok; m.Instructions = []Instruction{(*Memo)(nil)}; return m }(),
		"empty memo":       sampleMessage(Memo{}),
		"oversize memo":    sampleMessage(Memo{Data: make([]byte, MaxMemoSize+1)}),
		"lock bad chain":   sampleMessage(BridgeLock{DestChain: "NOPE"}),
		"release bad txid": sampleMessage(BridgeRelease{SourceChain: "sbx-ethereum", SourceTxID: "x"}),
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := m.Bytes()
			require.Error(t, err)
			require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
			_, err = Transaction{Message: m}.Encode()
			require.Error(t, err)
		})
	}

	// Sixteen maximal memos exceed the 4 KiB transaction limit.
	huge := make([]Instruction, MaxInstructions)
	for i := range huge {
		huge[i] = Memo{Data: bytes.Repeat([]byte{byte(i)}, MaxMemoSize)}
	}
	_, err := sampleMessage(huge...).Bytes()
	requireReason(t, err, errs.CodeValidationFailed, CodeInvalidMessage)

	// A message that fits alone can still overflow with its signatures.
	fit := make([]Instruction, 15)
	for i := range fit {
		fit[i] = Memo{Data: bytes.Repeat([]byte{byte(i)}, MaxMemoSize)}
	}
	m := sampleMessage(fit...)
	_, err = m.Bytes()
	require.NoError(t, err)
	_, err = Transaction{Message: m, Signatures: make([]Signature, MaxSignatures)}.Encode()
	requireReason(t, err, errs.CodeValidationFailed, CodeInvalidMessage)
	_, err = Transaction{Message: m, Signatures: make([]Signature, MaxSignatures+1)}.Encode()
	requireReason(t, err, errs.CodeValidationFailed, CodeSignatureExtra)
}

func TestMessage_PointerAndValueInstructionsEncodeIdentically(t *testing.T) {
	v := TokenTransfer{Mint: testAddr("usd"), FromOwner: testKey("alice").PublicKey(), ToOwner: testKey("bob").PublicKey(), Amount: 9}
	byValue, err := sampleMessage(v).Bytes()
	require.NoError(t, err)
	byPointer, err := sampleMessage(&v).Bytes()
	require.NoError(t, err)
	require.Equal(t, byValue, byPointer)
	require.Equal(t, sampleMessage(v).RequiredSigners(), sampleMessage(&v).RequiredSigners())
}

func TestMessage_ValidateRules(t *testing.T) {
	a, b := testKey("alice").PublicKey(), testKey("bob").PublicKey()
	mint := testAddr("usd")
	lock := BridgeLock{Mint: mint, Owner: a, Amount: 1, DestChain: "sbx-ethereum", DestOwner: b, Nonce: 1}
	src := TxIDFromSignature(Signature{7})
	cases := map[string]Message{
		"zero fee payer":         func() Message { m := sampleMessage(memo("x")); m.FeePayer = PublicKey{}; return m }(),
		"zero lamports":          sampleMessage(SystemTransfer{From: a, To: b}),
		"zero recipient":         sampleMessage(SystemTransfer{From: a, Lamports: 1}),
		"zero token amount":      sampleMessage(TokenTransfer{Mint: mint, FromOwner: a, ToOwner: b}),
		"zero mint":              sampleMessage(TokenBurn{Owner: a, Amount: 1}),
		"zero authority":         sampleMessage(TokenMintTo{Mint: mint, ToOwner: a, Amount: 1}),
		"two bridge locks":       sampleMessage(lock, func() BridgeLock { l := lock; l.Nonce = 2; return l }()),
		"lock to own chain":      sampleMessage(func() BridgeLock { l := lock; l.DestChain = testChainID; return l }()),
		"lock zero dest":         sampleMessage(func() BridgeLock { l := lock; l.DestOwner = PublicKey{}; return l }()),
		"release from own chain": sampleMessage(BridgeRelease{Mint: mint, ToOwner: b, Amount: 1, SourceChain: testChainID, SourceTxID: src, Authority: a}),
		"release bad txid":       sampleMessage(BridgeRelease{Mint: mint, ToOwner: b, Amount: 1, SourceChain: "sbx-ethereum", SourceTxID: "nope", Authority: a}),
		"nil instruction":        sampleMessage(nil),
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			err := m.Validate()
			require.Error(t, err)
			require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "%v", err)
		})
	}
	require.NoError(t, sampleMessage(lock).Validate())
	require.NoError(t, sampleMessage(SystemTransfer{From: a, To: a, Lamports: 1}).Validate(), "a self-transfer is a legal no-op")
}

func TestMessage_RequiredSignersOrderAndDedup(t *testing.T) {
	payer, a, b, auth := testKey("payer").PublicKey(), testKey("alice").PublicKey(), testKey("bob").PublicKey(), testKey("issuer").PublicKey()
	mint := testAddr("usd")
	m := sampleMessage(
		TokenTransfer{Mint: mint, FromOwner: b, ToOwner: a, Amount: 1},
		memo("no signer"),
		SystemTransfer{From: payer, To: a, Lamports: 1},
		TokenMintTo{Mint: mint, ToOwner: b, Amount: 1, Authority: auth},
		TokenBurn{Mint: mint, Owner: b, Amount: 1},
	)
	require.Equal(t, []PublicKey{payer, b, auth}, m.RequiredSigners())
}

func TestSign_DemandsExactlyTheRequiredKeypairs(t *testing.T) {
	m := sampleMessage(TokenTransfer{Mint: testAddr("usd"), FromOwner: testKey("alice").PublicKey(), ToOwner: testKey("bob").PublicKey(), Amount: 1})
	_, err := Sign(m, testKey("payer"))
	requireReason(t, err, errs.CodeValidationFailed, CodeSignatureMissing)
	_, err = Sign(m, testKey("payer"), testKey("alice"), testKey("carol"))
	requireReason(t, err, errs.CodeValidationFailed, CodeSignatureExtra)
	_, err = Sign(m, testKey("payer"), testKey("alice"), testKey("alice"))
	requireReason(t, err, errs.CodeValidationFailed, CodeSignatureExtra)
	_, err = Sign(m, testKey("payer"), Keypair{})
	require.Error(t, err)
	tx, err := Sign(m, testKey("alice"), testKey("payer")) // offered order does not matter
	require.NoError(t, err)
	require.NoError(t, tx.VerifySignatures())
	id, err := tx.ID()
	require.NoError(t, err)
	require.Equal(t, TxIDFromSignature(tx.Signatures[0]), id)

	_, err = Transaction{Message: m}.ID()
	requireReason(t, err, errs.CodeValidationFailed, CodeSignatureMissing)
}

func TestProgram_Names(t *testing.T) {
	names := map[Program]string{
		ProgramSystemTransfer: "SYSTEM_TRANSFER", ProgramTokenTransfer: "TOKEN_TRANSFER",
		ProgramTokenMintTo: "TOKEN_MINT_TO", ProgramTokenBurn: "TOKEN_BURN", ProgramMemo: "MEMO",
		ProgramBridgeLock: "BRIDGE_LOCK", ProgramBridgeRelease: "BRIDGE_RELEASE", Program(0): "UNKNOWN",
	}
	for p, n := range names {
		require.Equal(t, n, p.String())
	}
	for _, ins := range sampleInstructions() {
		require.NotEqual(t, "UNKNOWN", ins.Program().String())
	}
}

func TestDecode_RejectsWrongLengthsForEveryFixedProgram(t *testing.T) {
	for _, tc := range []struct {
		p    Program
		size int
	}{
		{ProgramSystemTransfer, systemTransferSize},
		{ProgramTokenTransfer, tokenTransferSize},
		{ProgramTokenMintTo, tokenMintToSize},
		{ProgramTokenBurn, tokenBurnSize},
	} {
		for _, n := range []int{0, tc.size - 1, tc.size + 1} {
			_, err := decodeInstruction(tc.p, make([]byte, n))
			requireReason(t, err, errs.CodeValidationFailed, CodeMalformedTransaction)
		}
		_, err := decodeInstruction(tc.p, make([]byte, tc.size))
		require.NoError(t, err, "%s at its exact size decodes", tc.p)
	}
}

func TestMessage_EveryPointerFormEncodesLikeItsValue(t *testing.T) {
	for _, v := range sampleInstructions() {
		var ptr Instruction
		switch x := v.(type) {
		case SystemTransfer:
			ptr = &x
		case TokenTransfer:
			ptr = &x
		case TokenMintTo:
			ptr = &x
		case TokenBurn:
			ptr = &x
		case Memo:
			ptr = &x
		case BridgeLock:
			ptr = &x
		case BridgeRelease:
			ptr = &x
		}
		byValue, err := sampleMessage(v).Bytes()
		require.NoError(t, err)
		byPointer, err := sampleMessage(ptr).Bytes()
		require.NoError(t, err)
		require.Equal(t, byValue, byPointer, "%s", v.Program())
	}
	var nilLock *BridgeLock
	_, err := sampleMessage(nilLock).Bytes()
	requireReason(t, err, errs.CodeValidationFailed, CodeInvalidMessage)
	require.Equal(t, []PublicKey{testKey("payer").PublicKey()}, sampleMessage(nilLock).RequiredSigners(),
		"a nil instruction contributes no signer and does not panic")
}

func TestSigningHelpers_RefuseUnencodableMessages(t *testing.T) {
	bad := sampleMessage(memo("x"))
	bad.ChainID = "Not Valid"
	_, err := Sign(bad, testKey("payer"))
	requireReason(t, err, errs.CodeValidationFailed, CodeInvalidChainID)
	require.Error(t, Transaction{Message: bad, Signatures: []Signature{{}}}.VerifySignatures())
	requireReason(t, bad.Validate(), errs.CodeValidationFailed, CodeInvalidChainID)

	// A message whose only required signer is the zero key can be offered
	// the zero Keypair, which then refuses to sign.
	zeroPayer := sampleMessage(memo("x"))
	zeroPayer.FeePayer = PublicKey{}
	_, err = Sign(zeroPayer, Keypair{})
	requireReason(t, err, errs.CodeValidationFailed, CodeKeypairUninitialized)
}

func TestHash_TextAndZero(t *testing.T) {
	require.True(t, Hash{}.IsZero())
	require.False(t, genesisHash(testChainID).IsZero())
	_, err := ParseHash("0OIl")
	requireReason(t, err, errs.CodeValidationFailed, CodeInvalidBase58)
	_, err = ParseHash(testKey("k").PublicKey().String() + "1")
	requireReason(t, err, errs.CodeValidationFailed, CodeInvalidBase58)
}
