package devnet

import (
	"bytes"
)

// Program identifies an instruction's program on the wire.
type Program uint8

// The seven programs. Values are part of the wire format and never change.
const (
	ProgramSystemTransfer Program = iota + 1
	ProgramTokenTransfer
	ProgramTokenMintTo
	ProgramTokenBurn
	ProgramMemo
	ProgramBridgeLock
	ProgramBridgeRelease
)

// String returns the program's name, e.g. "TOKEN_MINT_TO".
func (p Program) String() string {
	switch p {
	case ProgramSystemTransfer:
		return "SYSTEM_TRANSFER"
	case ProgramTokenTransfer:
		return "TOKEN_TRANSFER"
	case ProgramTokenMintTo:
		return "TOKEN_MINT_TO"
	case ProgramTokenBurn:
		return "TOKEN_BURN"
	case ProgramMemo:
		return "MEMO"
	case ProgramBridgeLock:
		return "BRIDGE_LOCK"
	case ProgramBridgeRelease:
		return "BRIDGE_RELEASE"
	default:
		return "UNKNOWN"
	}
}

// Instruction is one typed program call. The set is closed: only the seven
// types in this package implement it, and Decode returns them as values.
type Instruction interface {
	// Program returns the instruction's program.
	Program() Program
	// Signers returns the authorities whose signatures the instruction needs
	// besides the fee payer, in a fixed order.
	Signers() []PublicKey
	// encode appends the program data.
	encode(w *writer) error
	// validate checks the rules that do not depend on the enclosing message.
	validate() error
}

// Fixed program-data sizes.
const (
	systemTransferSize = 2*PublicKeySize + 8
	tokenTransferSize  = 3*PublicKeySize + 8
	tokenMintToSize    = 3*PublicKeySize + 8
	tokenBurnSize      = 2*PublicKeySize + 8
)

// SystemTransfer moves native units (lamports) between accounts. Data:
// from[32] | to[32] | lamports u64. Signer: From.
type SystemTransfer struct {
	From     PublicKey
	To       PublicKey
	Lamports uint64
}

// Program returns ProgramSystemTransfer.
func (i SystemTransfer) Program() Program { return ProgramSystemTransfer }

// Signers returns From.
func (i SystemTransfer) Signers() []PublicKey { return []PublicKey{i.From} }

func (i SystemTransfer) encode(w *writer) error {
	w.key(i.From)
	w.key(i.To)
	w.u64(i.Lamports)
	return nil
}

func (i SystemTransfer) validate() error {
	return firstErr(nonZero(i.From, "from"), nonZero(i.To, "to"), positive(i.Lamports, "lamports"))
}

// TokenTransfer moves Amount base units of Mint from FromOwner to ToOwner.
// Data: mint[32] | from_owner[32] | to_owner[32] | amount u64. Signer:
// FromOwner.
type TokenTransfer struct {
	Mint      PublicKey
	FromOwner PublicKey
	ToOwner   PublicKey
	Amount    uint64
}

// Program returns ProgramTokenTransfer.
func (i TokenTransfer) Program() Program { return ProgramTokenTransfer }

// Signers returns FromOwner.
func (i TokenTransfer) Signers() []PublicKey { return []PublicKey{i.FromOwner} }

func (i TokenTransfer) encode(w *writer) error {
	w.key(i.Mint)
	w.key(i.FromOwner)
	w.key(i.ToOwner)
	w.u64(i.Amount)
	return nil
}

func (i TokenTransfer) validate() error {
	return firstErr(nonZero(i.Mint, "mint"), nonZero(i.FromOwner, "from_owner"),
		nonZero(i.ToOwner, "to_owner"), positive(i.Amount, "amount"))
}

// TokenMintTo issues Amount new base units of Mint to ToOwner. Authority
// names the signer; execution fails with AUTHORITY_SIGNATURE_MISSING unless
// it is the mint's authority. Data: mint[32] | to_owner[32] | amount u64 |
// authority[32]. Signer: Authority.
type TokenMintTo struct {
	Mint      PublicKey
	ToOwner   PublicKey
	Amount    uint64
	Authority PublicKey
}

// Program returns ProgramTokenMintTo.
func (i TokenMintTo) Program() Program { return ProgramTokenMintTo }

// Signers returns Authority.
func (i TokenMintTo) Signers() []PublicKey { return []PublicKey{i.Authority} }

func (i TokenMintTo) encode(w *writer) error {
	w.key(i.Mint)
	w.key(i.ToOwner)
	w.u64(i.Amount)
	w.key(i.Authority)
	return nil
}

func (i TokenMintTo) validate() error {
	return firstErr(nonZero(i.Mint, "mint"), nonZero(i.ToOwner, "to_owner"),
		positive(i.Amount, "amount"), nonZero(i.Authority, "authority"))
}

// TokenBurn destroys Amount base units of Mint held by Owner, reducing
// supply. Data: mint[32] | owner[32] | amount u64. Signer: Owner.
type TokenBurn struct {
	Mint   PublicKey
	Owner  PublicKey
	Amount uint64
}

// Program returns ProgramTokenBurn.
func (i TokenBurn) Program() Program { return ProgramTokenBurn }

// Signers returns Owner.
func (i TokenBurn) Signers() []PublicKey { return []PublicKey{i.Owner} }

func (i TokenBurn) encode(w *writer) error {
	w.key(i.Mint)
	w.key(i.Owner)
	w.u64(i.Amount)
	return nil
}

func (i TokenBurn) validate() error {
	return firstErr(nonZero(i.Mint, "mint"), nonZero(i.Owner, "owner"), positive(i.Amount, "amount"))
}

// Memo records 1..MaxMemoSize opaque bytes in the transaction, typically a
// settlement-plan hash the transaction is bound to. It changes no state and
// needs no signer beyond the fee payer. Data: the bytes themselves.
type Memo struct {
	Data []byte
}

// Program returns ProgramMemo.
func (i Memo) Program() Program { return ProgramMemo }

// Signers returns nothing: the fee payer's signature covers the memo.
func (i Memo) Signers() []PublicKey { return nil }

func (i Memo) encode(w *writer) error {
	if err := i.validate(); err != nil {
		return err
	}
	w.raw(i.Data)
	return nil
}

func (i Memo) validate() error {
	if len(i.Data) == 0 || len(i.Data) > MaxMemoSize {
		return invalid(CodeInvalidMessage, "memo must be 1 to 256 bytes")
	}
	return nil
}

// BridgeLock moves Amount of Mint from Owner into the chain's bridge escrow
// for release to DestOwner on DestChain, consuming Nonce on this chain. A
// message may carry at most one. Data: mint[32] | owner[32] | amount u64 |
// u8 len | dest_chain | dest_owner[32] | nonce u64. Signer: Owner.
type BridgeLock struct {
	Mint      PublicKey
	Owner     PublicKey
	Amount    uint64
	DestChain ChainID
	DestOwner PublicKey
	Nonce     uint64
}

// Program returns ProgramBridgeLock.
func (i BridgeLock) Program() Program { return ProgramBridgeLock }

// Signers returns Owner.
func (i BridgeLock) Signers() []PublicKey { return []PublicKey{i.Owner} }

func (i BridgeLock) encode(w *writer) error {
	w.key(i.Mint)
	w.key(i.Owner)
	w.u64(i.Amount)
	if err := w.chainID(i.DestChain); err != nil {
		return err
	}
	w.key(i.DestOwner)
	w.u64(i.Nonce)
	return nil
}

func (i BridgeLock) validate() error {
	return firstErr(nonZero(i.Mint, "mint"), nonZero(i.Owner, "owner"), positive(i.Amount, "amount"),
		i.DestChain.Validate(), nonZero(i.DestOwner, "dest_owner"))
}

// BridgeRelease mints Amount of Mint to ToOwner on the destination chain for
// the lock SourceTxID (nonce Nonce) on SourceChain. Authority names the
// signer; execution fails with AUTHORITY_SIGNATURE_MISSING unless it is the
// chain's configured bridge authority. Data: mint[32] | to_owner[32] |
// amount u64 | u8 len | source_chain | source_tx_id[64] | nonce u64 |
// authority[32]. Signer: Authority.
type BridgeRelease struct {
	Mint        PublicKey
	ToOwner     PublicKey
	Amount      uint64
	SourceChain ChainID
	SourceTxID  TxID
	Nonce       uint64
	Authority   PublicKey
}

// Program returns ProgramBridgeRelease.
func (i BridgeRelease) Program() Program { return ProgramBridgeRelease }

// Signers returns Authority.
func (i BridgeRelease) Signers() []PublicKey { return []PublicKey{i.Authority} }

func (i BridgeRelease) encode(w *writer) error {
	src, err := i.SourceTxID.Signature()
	if err != nil {
		return err
	}
	w.key(i.Mint)
	w.key(i.ToOwner)
	w.u64(i.Amount)
	if err := w.chainID(i.SourceChain); err != nil {
		return err
	}
	w.signature(src)
	w.u64(i.Nonce)
	w.key(i.Authority)
	return nil
}

func (i BridgeRelease) validate() error {
	_, txErr := i.SourceTxID.Signature()
	return firstErr(nonZero(i.Mint, "mint"), nonZero(i.ToOwner, "to_owner"), positive(i.Amount, "amount"),
		i.SourceChain.Validate(), txErr, nonZero(i.Authority, "authority"))
}

// decodeInstruction decodes one instruction's program data, which it must
// consume exactly. It copies everything it keeps, so the result never aliases
// data.
func decodeInstruction(p Program, data []byte) (Instruction, error) {
	r := &reader{buf: data}
	var ins Instruction
	switch p {
	case ProgramSystemTransfer:
		if len(data) != systemTransferSize {
			return nil, malformed("SYSTEM_TRANSFER data has the wrong length")
		}
		var i SystemTransfer
		i.From = r.key("from")
		i.To = r.key("to")
		i.Lamports = r.u64("lamports")
		ins = i
	case ProgramTokenTransfer:
		if len(data) != tokenTransferSize {
			return nil, malformed("TOKEN_TRANSFER data has the wrong length")
		}
		var i TokenTransfer
		i.Mint = r.key("mint")
		i.FromOwner = r.key("from_owner")
		i.ToOwner = r.key("to_owner")
		i.Amount = r.u64("amount")
		ins = i
	case ProgramTokenMintTo:
		if len(data) != tokenMintToSize {
			return nil, malformed("TOKEN_MINT_TO data has the wrong length")
		}
		var i TokenMintTo
		i.Mint = r.key("mint")
		i.ToOwner = r.key("to_owner")
		i.Amount = r.u64("amount")
		i.Authority = r.key("authority")
		ins = i
	case ProgramTokenBurn:
		if len(data) != tokenBurnSize {
			return nil, malformed("TOKEN_BURN data has the wrong length")
		}
		var i TokenBurn
		i.Mint = r.key("mint")
		i.Owner = r.key("owner")
		i.Amount = r.u64("amount")
		ins = i
	case ProgramMemo:
		if len(data) == 0 || len(data) > MaxMemoSize {
			return nil, malformed("MEMO must carry 1 to 256 bytes")
		}
		b, _ := r.take(len(data), "memo")
		ins = Memo{Data: bytes.Clone(b)}
	case ProgramBridgeLock:
		var i BridgeLock
		i.Mint = r.key("mint")
		i.Owner = r.key("owner")
		i.Amount = r.u64("amount")
		i.DestChain = r.chainID("dest_chain")
		i.DestOwner = r.key("dest_owner")
		i.Nonce = r.u64("nonce")
		ins = i
	case ProgramBridgeRelease:
		var i BridgeRelease
		i.Mint = r.key("mint")
		i.ToOwner = r.key("to_owner")
		i.Amount = r.u64("amount")
		i.SourceChain = r.chainID("source_chain")
		i.SourceTxID = TxIDFromSignature(r.signature("source_tx_id"))
		i.Nonce = r.u64("nonce")
		i.Authority = r.key("authority")
		ins = i
	default:
		return nil, malformed("unknown program")
	}
	if err := r.finish(p.String() + " data"); err != nil {
		return nil, err
	}
	return ins, nil
}

// canonical returns ins as a value type, dereferencing a pointer to one of
// the seven instruction types; ok is false for a nil interface or nil
// pointer. Messages built in Go may use either form and encode identically.
func canonical(ins Instruction) (Instruction, bool) {
	switch v := ins.(type) {
	case nil:
		return nil, false
	case *SystemTransfer:
		return deref(v)
	case *TokenTransfer:
		return deref(v)
	case *TokenMintTo:
		return deref(v)
	case *TokenBurn:
		return deref(v)
	case *Memo:
		return deref(v)
	case *BridgeLock:
		return deref(v)
	case *BridgeRelease:
		return deref(v)
	default:
		return ins, true
	}
}

// deref returns *p as an Instruction, or false for a nil pointer.
func deref[T Instruction](p *T) (Instruction, bool) {
	if p == nil {
		return nil, false
	}
	return *p, true
}

// nonZero rejects the zero key.
func nonZero(k PublicKey, field string) error {
	if k.IsZero() {
		return invalid(CodeInvalidMessage, field+" is the zero key")
	}
	return nil
}

// positive rejects a zero amount.
func positive(v uint64, field string) error {
	if v == 0 {
		return invalid(CodeInvalidMessage, field+" must be greater than zero")
	}
	return nil
}

// firstErr returns the first non-nil error.
func firstErr(errList ...error) error {
	for _, err := range errList {
		if err != nil {
			return err
		}
	}
	return nil
}
