package inspect

import (
	"crypto/sha256"
	"errors"
	"fmt"

	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
)

// Version is the transaction message version.
type Version string

// Supported message versions. v1 (SIMD-0385) is decoded by solana-go but
// deliberately rejected here until its semantics are documented and verified.
const (
	VersionLegacy Version = "legacy"
	VersionV0     Version = "v0"
)

// Header mirrors the Solana message header.
type Header struct {
	NumRequiredSignatures       uint8
	NumReadonlySignedAccounts   uint8
	NumReadonlyUnsignedAccounts uint8
}

// CompiledInstruction is one instruction with account indexes into the
// message's (static + loaded) account list.
type CompiledInstruction struct {
	ProgramIDIndex uint16
	Accounts       []uint16
	Data           []byte
}

// LookupTableRef is one address-table lookup of a v0 message.
type LookupTableRef struct {
	Table           solana.PublicKey
	WritableIndexes []uint8
	ReadonlyIndexes []uint8
}

// DecodedTransaction is the structural view of an unsigned transaction. Keys
// from lookup tables are not resolved here; Inspect resolves them from
// Expectations.LookupTables.
type DecodedTransaction struct {
	Version         Version
	NumSignatures   int
	Header          Header
	StaticKeys      []solana.PublicKey
	RecentBlockhash solana.Hash
	Instructions    []CompiledInstruction
	LookupTables    []LookupTableRef
	// Hash is sha256(raw): the inspected transaction hash recorded on the
	// signing decision.
	Hash [32]byte
	// Size is len(raw).
	Size int
}

// ErrDecode wraps every decoding failure.
var ErrDecode = errors.New("inspect: transaction decode failed")

// TxHash returns sha256(raw), the hash the signing service records and
// compares with the caller's expected hash.
func TxHash(raw []byte) [32]byte { return sha256.Sum256(raw) }

// Decode decodes a legacy or v0 transaction. It never panics: any panic in
// the underlying decoder is converted into an error. Trailing bytes, v1
// messages, size above MaxTransactionSize, header/key inconsistencies and
// out-of-range instruction indexes are all decode errors.
func Decode(raw []byte) (tx DecodedTransaction, err error) {
	defer func() {
		if r := recover(); r != nil {
			tx = DecodedTransaction{}
			err = fmt.Errorf("%w: decoder panic: %v", ErrDecode, r)
		}
	}()
	if len(raw) == 0 {
		return DecodedTransaction{}, fmt.Errorf("%w: empty input", ErrDecode)
	}
	if len(raw) > MaxTransactionSize {
		return DecodedTransaction{}, fmt.Errorf("%w: %d bytes exceeds the %d byte packet limit", ErrDecode, len(raw), MaxTransactionSize)
	}
	dec := bin.NewBinDecoder(raw)
	var stx solana.Transaction
	if err := stx.UnmarshalWithDecoder(dec); err != nil {
		return DecodedTransaction{}, fmt.Errorf("%w: %v", ErrDecode, err)
	}
	if dec.Remaining() != 0 {
		return DecodedTransaction{}, fmt.Errorf("%w: %d trailing bytes after the message", ErrDecode, dec.Remaining())
	}
	msg := &stx.Message
	var version Version
	switch msg.GetVersion() {
	case solana.MessageVersionLegacy:
		version = VersionLegacy
	case solana.MessageVersionV0:
		version = VersionV0
	default:
		return DecodedTransaction{}, fmt.Errorf("%w: unsupported message version %d", ErrDecode, msg.GetVersion())
	}
	if version == VersionLegacy && len(msg.AddressTableLookups) > 0 {
		return DecodedTransaction{}, fmt.Errorf("%w: legacy message carries address table lookups", ErrDecode)
	}
	h := msg.Header
	nStatic := len(msg.AccountKeys)
	if nStatic == 0 {
		return DecodedTransaction{}, fmt.Errorf("%w: no account keys", ErrDecode)
	}
	if int(h.NumRequiredSignatures) == 0 || int(h.NumRequiredSignatures) > nStatic {
		return DecodedTransaction{}, fmt.Errorf("%w: header requires %d signatures for %d static keys", ErrDecode, h.NumRequiredSignatures, nStatic)
	}
	if int(h.NumReadonlySignedAccounts) > int(h.NumRequiredSignatures) {
		return DecodedTransaction{}, fmt.Errorf("%w: readonly signed accounts exceed required signatures", ErrDecode)
	}
	if int(h.NumReadonlyUnsignedAccounts) > nStatic-int(h.NumRequiredSignatures) {
		return DecodedTransaction{}, fmt.Errorf("%w: readonly unsigned accounts exceed unsigned keys", ErrDecode)
	}
	if len(stx.Signatures) != int(h.NumRequiredSignatures) {
		return DecodedTransaction{}, fmt.Errorf("%w: %d signature slots for %d required signatures", ErrDecode, len(stx.Signatures), h.NumRequiredSignatures)
	}
	if msg.HasDuplicates() {
		return DecodedTransaction{}, fmt.Errorf("%w: duplicate account keys", ErrDecode)
	}
	if len(msg.Instructions) == 0 {
		return DecodedTransaction{}, fmt.Errorf("%w: no instructions", ErrDecode)
	}
	lookups := make([]LookupTableRef, 0, len(msg.AddressTableLookups))
	nLoaded := 0
	for i, l := range msg.AddressTableLookups {
		if l.AccountKey.IsZero() {
			return DecodedTransaction{}, fmt.Errorf("%w: lookup %d has a zero table address", ErrDecode, i)
		}
		if len(l.WritableIndexes)+len(l.ReadonlyIndexes) == 0 {
			return DecodedTransaction{}, fmt.Errorf("%w: lookup %d loads no addresses", ErrDecode, i)
		}
		nLoaded += len(l.WritableIndexes) + len(l.ReadonlyIndexes)
		lookups = append(lookups, LookupTableRef{
			Table:           l.AccountKey,
			WritableIndexes: append([]uint8(nil), l.WritableIndexes...),
			ReadonlyIndexes: append([]uint8(nil), l.ReadonlyIndexes...),
		})
	}
	total := nStatic + nLoaded
	if total > 256 {
		return DecodedTransaction{}, fmt.Errorf("%w: %d account keys exceed the u8 index space", ErrDecode, total)
	}
	ixs := make([]CompiledInstruction, 0, len(msg.Instructions))
	for i, ci := range msg.Instructions {
		if int(ci.ProgramIDIndex) >= total {
			return DecodedTransaction{}, fmt.Errorf("%w: instruction %d program index %d out of range", ErrDecode, i, ci.ProgramIDIndex)
		}
		if int(ci.ProgramIDIndex) >= nStatic {
			// The runtime requires program ids to be static keys.
			return DecodedTransaction{}, fmt.Errorf("%w: instruction %d program id loaded from a lookup table", ErrDecode, i)
		}
		accts := make([]uint16, 0, len(ci.Accounts))
		for _, a := range ci.Accounts {
			if int(a) >= total {
				return DecodedTransaction{}, fmt.Errorf("%w: instruction %d account index %d out of range", ErrDecode, i, a)
			}
			accts = append(accts, a)
		}
		ixs = append(ixs, CompiledInstruction{ProgramIDIndex: ci.ProgramIDIndex, Accounts: accts, Data: append([]byte(nil), ci.Data...)})
	}
	keys := make([]solana.PublicKey, len(msg.AccountKeys))
	copy(keys, msg.AccountKeys)
	return DecodedTransaction{
		Version:       version,
		NumSignatures: len(stx.Signatures),
		Header: Header{
			NumRequiredSignatures:       h.NumRequiredSignatures,
			NumReadonlySignedAccounts:   h.NumReadonlySignedAccounts,
			NumReadonlyUnsignedAccounts: h.NumReadonlyUnsignedAccounts,
		},
		StaticKeys:      keys,
		RecentBlockhash: msg.RecentBlockhash,
		Instructions:    ixs,
		LookupTables:    lookups,
		Hash:            sha256.Sum256(raw),
		Size:            len(raw),
	}, nil
}

// account is one resolved account of the message with its access flags.
type account struct {
	key        solana.PublicKey
	signer     bool
	writable   bool
	fromLookup bool
}

// resolvedIx is one instruction with its accounts resolved.
type resolvedIx struct {
	index    int
	program  solana.PublicKey
	accounts []account
	data     []byte
}

// resolvedTx is the fully resolved account/instruction view Inspect works on.
type resolvedTx struct {
	accounts []account
	ixs      []resolvedIx
}

// resolve builds the full account list (static keys, then every writable
// loaded address in lookup order, then every readonly loaded address) and
// resolves each instruction. Missing tables or out-of-range indexes are
// errors: an account the inspector cannot see is an account it cannot judge.
func resolve(tx DecodedTransaction, tables map[solana.PublicKey][]solana.PublicKey) (*resolvedTx, error) {
	h := tx.Header
	nStatic := len(tx.StaticKeys)
	accts := make([]account, 0, nStatic)
	for i, k := range tx.StaticKeys {
		signer := i < int(h.NumRequiredSignatures)
		var writable bool
		if signer {
			writable = i < int(h.NumRequiredSignatures)-int(h.NumReadonlySignedAccounts)
		} else {
			writable = i < nStatic-int(h.NumReadonlyUnsignedAccounts)
		}
		accts = append(accts, account{key: k, signer: signer, writable: writable})
	}
	var writableLoaded, readonlyLoaded []account
	for _, l := range tx.LookupTables {
		table, ok := tables[l.Table]
		if !ok {
			return nil, fmt.Errorf("lookup table %s not supplied", l.Table)
		}
		for _, idx := range l.WritableIndexes {
			if int(idx) >= len(table) {
				return nil, fmt.Errorf("lookup table %s index %d out of range (%d entries)", l.Table, idx, len(table))
			}
			writableLoaded = append(writableLoaded, account{key: table[idx], writable: true, fromLookup: true})
		}
		for _, idx := range l.ReadonlyIndexes {
			if int(idx) >= len(table) {
				return nil, fmt.Errorf("lookup table %s index %d out of range (%d entries)", l.Table, idx, len(table))
			}
			readonlyLoaded = append(readonlyLoaded, account{key: table[idx], fromLookup: true})
		}
	}
	accts = append(accts, writableLoaded...)
	accts = append(accts, readonlyLoaded...)
	seen := make(map[solana.PublicKey]struct{}, len(accts))
	for _, a := range accts {
		// The zero key is the System program id and is a legitimate account.
		if _, dup := seen[a.key]; dup {
			return nil, fmt.Errorf("duplicate account key %s after lookup resolution", a.key)
		}
		seen[a.key] = struct{}{}
	}
	out := &resolvedTx{accounts: accts}
	for i, ci := range tx.Instructions {
		if int(ci.ProgramIDIndex) >= len(accts) {
			return nil, fmt.Errorf("instruction %d program index out of range", i)
		}
		rix := resolvedIx{index: i, program: accts[ci.ProgramIDIndex].key, data: ci.Data}
		for _, a := range ci.Accounts {
			if int(a) >= len(accts) {
				return nil, fmt.Errorf("instruction %d account index out of range", i)
			}
			rix.accounts = append(rix.accounts, accts[a])
		}
		out.ixs = append(out.ixs, rix)
	}
	return out, nil
}
