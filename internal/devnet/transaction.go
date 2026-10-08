package devnet

import (
	"crypto/sha256"
	"strconv"

	"github.com/nodal/controlplane/internal/errs"
)

// Message is the signed part of a transaction: the chain it is for, the
// recent blockhash that bounds its lifetime, the fee payer and the
// instructions.
type Message struct {
	ChainID         ChainID
	RecentBlockhash Hash
	FeePayer        PublicKey
	Instructions    []Instruction
}

// Bytes returns the canonical encoding of m: the exact bytes every required
// signer signs. It fails when m cannot be encoded (bad chain id, no or too
// many instructions, a nil instruction, an oversize memo or message).
func (m Message) Bytes() ([]byte, error) {
	n := len(m.Instructions)
	if n == 0 || n > MaxInstructions {
		return nil, invalid(CodeInvalidMessage, "a message carries 1 to 16 instructions")
	}
	w := &writer{buf: make([]byte, 0, 128)}
	w.str(msgMagic)
	w.u8(WireVersion)
	if err := w.chainID(m.ChainID); err != nil {
		return nil, err
	}
	w.hash(m.RecentBlockhash)
	w.key(m.FeePayer)
	w.count(n)
	for idx, raw := range m.Instructions {
		ins, ok := canonical(raw)
		if !ok {
			return nil, invalid(CodeInvalidMessage, "instruction "+strconv.Itoa(idx)+" is nil")
		}
		data := &writer{}
		if err := ins.encode(data); err != nil {
			return nil, err
		}
		if len(data.buf) > maxMessageSize {
			return nil, invalid(CodeInvalidMessage, "instruction "+strconv.Itoa(idx)+" is too large")
		}
		w.u8(uint8(ins.Program()))
		w.u16(uint16(len(data.buf))) // #nosec G115 -- bounded above by maxMessageSize (4088)
		w.raw(data.buf)
	}
	if len(w.buf) > maxMessageSize {
		return nil, invalid(CodeInvalidMessage, "message exceeds the transaction size limit")
	}
	return w.buf, nil
}

// Validate checks every rule a chain applies to a message before it looks at
// state: everything Bytes checks, plus no zero keys, no zero amounts, at most
// one BRIDGE_LOCK, and no bridge from or to the message's own chain.
func (m Message) Validate() error {
	if err := m.ChainID.Validate(); err != nil {
		return err
	}
	if m.FeePayer.IsZero() {
		return invalid(CodeInvalidMessage, "fee payer is the zero key")
	}
	locks := 0
	for idx, raw := range m.Instructions {
		ins, ok := canonical(raw)
		if !ok {
			return invalid(CodeInvalidMessage, "instruction "+strconv.Itoa(idx)+" is nil")
		}
		if err := ins.validate(); err != nil {
			return err
		}
		switch v := ins.(type) {
		case BridgeLock:
			locks++
			if v.DestChain == m.ChainID {
				return invalid(CodeInvalidMessage, "a bridge lock cannot target its own chain")
			}
		case BridgeRelease:
			if v.SourceChain == m.ChainID {
				return invalid(CodeInvalidMessage, "a bridge release cannot come from its own chain")
			}
		}
	}
	if locks > 1 {
		return invalid(CodeInvalidMessage, "a message carries at most one BRIDGE_LOCK")
	}
	_, err := m.Bytes()
	return err
}

// RequiredSigners returns the keys that must sign m, in signature order: the
// fee payer, then each instruction's signers, de-duplicated in order of first
// appearance.
func (m Message) RequiredSigners() []PublicKey {
	out := []PublicKey{m.FeePayer}
	seen := map[PublicKey]struct{}{m.FeePayer: {}}
	for _, raw := range m.Instructions {
		ins, ok := canonical(raw)
		if !ok {
			continue
		}
		for _, s := range ins.Signers() {
			if _, dup := seen[s]; dup {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

// Transaction is a message plus its signatures. Signatures are positional:
// Signatures[i] is by Message.RequiredSigners()[i].
type Transaction struct {
	Message    Message
	Signatures []Signature
}

// ID returns the TxID: the base58 form of the first signature.
func (t Transaction) ID() (TxID, error) {
	if len(t.Signatures) == 0 {
		return "", invalid(CodeSignatureMissing, "transaction has no signatures")
	}
	return TxIDFromSignature(t.Signatures[0]), nil
}

// Encode returns the canonical wire encoding of t.
func (t Transaction) Encode() ([]byte, error) {
	msg, err := t.Message.Bytes()
	if err != nil {
		return nil, err
	}
	if len(t.Signatures) > MaxSignatures {
		return nil, invalid(CodeSignatureExtra, "more than 32 signatures")
	}
	w := &writer{buf: make([]byte, 0, txOverhead+len(msg)+len(t.Signatures)*SignatureSize)}
	w.str(txMagic)
	w.u8(WireVersion)
	w.u16(uint16(len(msg))) // #nosec G115 -- Bytes bounds a message to maxMessageSize (4088)
	w.raw(msg)
	w.count(len(t.Signatures))
	for _, s := range t.Signatures {
		w.signature(s)
	}
	if len(w.buf) > MaxTransactionSize {
		return nil, invalid(CodeInvalidMessage, "transaction exceeds 4096 bytes")
	}
	return w.buf, nil
}

// Decode parses a canonical version-1 transaction. It rejects oversize input
// before reading it, and unknown magic or versions, unknown programs, wrong
// data lengths, invalid chain ids, more than MaxSignatures signatures and
// trailing bytes, all with VALIDATION_FAILED / MALFORMED_TRANSACTION. It
// never panics, and Encode of its result reproduces raw exactly. The result
// never aliases raw.
//
// Decode checks structure only; Validate and VerifySignatures check the
// rules and the signatures.
func Decode(raw []byte) (Transaction, error) {
	if len(raw) > MaxTransactionSize {
		return Transaction{}, malformed("larger than 4096 bytes")
	}
	r := &reader{buf: raw}
	r.magic(txMagic, "transaction magic")
	if v := r.u8("transaction version"); r.err == nil && v != WireVersion {
		return Transaction{}, malformed("unknown transaction version " + strconv.Itoa(int(v)))
	}
	msgLen := int(r.u16("message length"))
	msgBytes, ok := r.take(msgLen, "message")
	if !ok {
		return Transaction{}, r.err
	}
	msg, err := decodeMessage(msgBytes)
	if err != nil {
		return Transaction{}, err
	}
	n := int(r.u8("signature count"))
	if r.err == nil && n > MaxSignatures {
		return Transaction{}, malformed("more than 32 signatures")
	}
	sigs := make([]Signature, 0, n)
	for i := 0; i < n && r.err == nil; i++ {
		sigs = append(sigs, r.signature("signature"))
	}
	if err := r.finish("transaction"); err != nil {
		return Transaction{}, err
	}
	return Transaction{Message: msg, Signatures: sigs}, nil
}

// decodeMessage parses canonical message bytes, which it must consume
// exactly.
func decodeMessage(b []byte) (Message, error) {
	r := &reader{buf: b}
	r.magic(msgMagic, "message magic")
	if v := r.u8("message version"); r.err == nil && v != WireVersion {
		return Message{}, malformed("unknown message version " + strconv.Itoa(int(v)))
	}
	var m Message
	m.ChainID = r.chainID("chain id")
	m.RecentBlockhash = r.hash("recent blockhash")
	m.FeePayer = r.key("fee payer")
	n := int(r.u8("instruction count"))
	if r.err == nil && (n == 0 || n > MaxInstructions) {
		return Message{}, malformed("a message carries 1 to 16 instructions")
	}
	m.Instructions = make([]Instruction, 0, n)
	for i := 0; i < n && r.err == nil; i++ {
		p := Program(r.u8("program"))
		dataLen := int(r.u16("instruction data length"))
		data, ok := r.take(dataLen, "instruction data")
		if !ok {
			break
		}
		ins, err := decodeInstruction(p, data)
		if err != nil {
			return Message{}, err
		}
		m.Instructions = append(m.Instructions, ins)
	}
	if err := r.finish("message"); err != nil {
		return Message{}, err
	}
	return m, nil
}

// Sign signs msg with signers and returns the transaction with its
// signatures in RequiredSigners order. Every required signer must be offered
// (SIGNATURE_MISSING) and nothing else may be (SIGNATURE_EXTRA). It is a
// convenience over Keypair.Sign and holds on to nothing.
func Sign(msg Message, signers ...Keypair) (Transaction, error) {
	b, err := msg.Bytes()
	if err != nil {
		return Transaction{}, err
	}
	offered := make(map[PublicKey]Keypair, len(signers))
	for _, kp := range signers {
		if _, dup := offered[kp.PublicKey()]; dup {
			return Transaction{}, invalid(CodeSignatureExtra, "keypair "+kp.PublicKey().String()+" offered twice")
		}
		offered[kp.PublicKey()] = kp
	}
	required := msg.RequiredSigners()
	sigs := make([]Signature, 0, len(required))
	for _, k := range required {
		kp, ok := offered[k]
		if !ok {
			return Transaction{}, invalid(CodeSignatureMissing, "no keypair for required signer "+k.String())
		}
		delete(offered, k)
		sig, err := kp.Sign(b)
		if err != nil {
			return Transaction{}, err
		}
		sigs = append(sigs, sig)
	}
	for _, kp := range signers { // in the caller's order, so the error is deterministic
		if _, extra := offered[kp.PublicKey()]; extra {
			return Transaction{}, invalid(CodeSignatureExtra, "keypair "+kp.PublicKey().String()+" is not a required signer")
		}
	}
	return Transaction{Message: msg, Signatures: sigs}, nil
}

// VerifySignatures checks that t carries exactly one valid signature per
// required signer, in order, over the exact message bytes.
func (t Transaction) VerifySignatures() error {
	b, err := t.Message.Bytes()
	if err != nil {
		return err
	}
	required := t.Message.RequiredSigners()
	switch {
	case len(t.Signatures) < len(required):
		return invalid(CodeSignatureMissing, "missing signature of required signer "+
			required[len(t.Signatures)].String()).WithField("signer", required[len(t.Signatures)].String())
	case len(t.Signatures) > len(required):
		return invalid(CodeSignatureExtra, strconv.Itoa(len(t.Signatures))+" signatures for "+
			strconv.Itoa(len(required))+" required signers")
	}
	for i, k := range required {
		if !Verify(k, b, t.Signatures[i]) {
			return invalid(CodeSignatureInvalid, "signature "+strconv.Itoa(i)+" does not verify for "+
				k.String()).WithField("signer", k.String())
		}
	}
	return nil
}

// messageHash is SHA-256 of the canonical message bytes: the key under which
// the chain refuses a second signature over the same message.
func messageHash(msgBytes []byte) Hash {
	return Hash(sha256.Sum256(msgBytes))
}

// checkedMessage is a decoded, validated, signature-verified transaction and
// the values Submit derives from it.
type checkedMessage struct {
	tx      Transaction
	id      Signature
	msgHash Hash
}

// check runs every stateless Submit check for chain: decode, chain id, rules
// and signatures.
func check(chain ChainID, raw []byte) (checkedMessage, error) {
	tx, err := Decode(raw)
	if err != nil {
		return checkedMessage{}, err
	}
	if tx.Message.ChainID != chain {
		return checkedMessage{}, invalid(CodeWrongChain, "message is for chain "+string(tx.Message.ChainID)+
			", not "+string(chain))
	}
	if err := tx.Message.Validate(); err != nil {
		return checkedMessage{}, err
	}
	if err := tx.VerifySignatures(); err != nil {
		return checkedMessage{}, err
	}
	msgBytes, err := tx.Message.Bytes()
	if err != nil {
		return checkedMessage{}, errs.Wrap(err, errs.CodeInternal, "devnet: re-encoding a verified message failed")
	}
	return checkedMessage{tx: tx, id: tx.Signatures[0], msgHash: messageHash(msgBytes)}, nil
}
