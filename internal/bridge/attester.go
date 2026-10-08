package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/errs"
)

const (
	planVersion    = "nodal-sandbox/bridge-plan/v1"
	planHashDomain = "nodal-sandbox/bridge-plan/v1\x00"
)

// BridgePlan is the canonical description of one leg's lock: what moves,
// from whom, to whom, on which chains, under which single-use nonce. Its hash
// is the lock transaction's MEMO, binding the holder's signature to exactly
// this plan.
type BridgePlan struct {
	Version          string    `json:"version"`
	LegID            string    `json:"leg_id"`
	RouteID          string    `json:"route_id"`
	AccountID        string    `json:"account_id"`
	SourceChain      string    `json:"source_chain"`
	DestinationChain string    `json:"destination_chain"`
	SourceMint       string    `json:"source_mint"`
	DestinationMint  string    `json:"destination_mint"`
	SourceOwner      string    `json:"source_owner"`
	DestinationOwner string    `json:"destination_owner"`
	Amount           string    `json:"amount"`
	Nonce            string    `json:"nonce"`
	RecentBlockhash  string    `json:"recent_blockhash"`
	BlockhashHeight  string    `json:"blockhash_height"`
	QuoteExpiresAt   time.Time `json:"quote_expires_at"`
}

// Hash returns the plan hash.
func (p BridgePlan) Hash() ([32]byte, error) {
	b, err := audit.CanonicalJSON(p)
	if err != nil {
		return [32]byte{}, errs.Wrap(err, errs.CodeInternal, "bridge: plan is not canonically encodable")
	}
	return sha256.Sum256(append([]byte(planHashDomain), b...)), nil
}

// Lock returns the BRIDGE_LOCK the plan describes.
func (p BridgePlan) Lock() (devnet.BridgeLock, error) {
	mint, err := devnet.ParsePublicKey(p.SourceMint)
	if err != nil {
		return devnet.BridgeLock{}, err
	}
	owner, err := devnet.ParsePublicKey(p.SourceOwner)
	if err != nil {
		return devnet.BridgeLock{}, err
	}
	dest, err := devnet.ParsePublicKey(p.DestinationOwner)
	if err != nil {
		return devnet.BridgeLock{}, err
	}
	amount, err := strconv.ParseUint(p.Amount, 10, 64)
	if err != nil || amount == 0 {
		return devnet.BridgeLock{}, errs.New(errs.CodeValidationFailed, "bridge: plan amount is not a positive u64")
	}
	nonce, err := strconv.ParseUint(p.Nonce, 10, 64)
	if err != nil {
		return devnet.BridgeLock{}, errs.New(errs.CodeValidationFailed, "bridge: plan nonce is not a u64")
	}
	return devnet.BridgeLock{Mint: mint, Owner: owner, Amount: amount, DestChain: devnet.ChainID(p.DestinationChain),
		DestOwner: dest, Nonce: nonce}, nil
}

// LockMessage builds [BRIDGE_LOCK, MEMO(plan hash)] paid by the holder.
func (p BridgePlan) LockMessage() (devnet.Message, devnet.BridgeLock, [32]byte, error) {
	lock, err := p.Lock()
	if err != nil {
		return devnet.Message{}, devnet.BridgeLock{}, [32]byte{}, err
	}
	bh, err := devnet.ParseHash(p.RecentBlockhash)
	if err != nil {
		return devnet.Message{}, devnet.BridgeLock{}, [32]byte{}, err
	}
	h, err := p.Hash()
	if err != nil {
		return devnet.Message{}, devnet.BridgeLock{}, [32]byte{}, err
	}
	return devnet.Message{
		ChainID: devnet.ChainID(p.SourceChain), RecentBlockhash: bh, FeePayer: lock.Owner,
		Instructions: []devnet.Instruction{lock, devnet.Memo{Data: []byte(hex.EncodeToString(h[:]))}},
	}, lock, h, nil
}

// Release inspection reason codes.
const (
	InspectMalformed         = "INSPECT_MALFORMED"
	InspectChainMismatch     = "INSPECT_CHAIN_MISMATCH"
	InspectFeePayerMismatch  = "INSPECT_FEE_PAYER_MISMATCH"
	InspectInstructionCount  = "INSPECT_INSTRUCTION_COUNT"
	InspectReleaseMismatch   = "INSPECT_RELEASE_MISMATCH"
	InspectSignerSetMismatch = "INSPECT_SIGNER_SET_MISMATCH"
	InspectSignatureInvalid  = "INSPECT_SIGNATURE_INVALID"
)

func rejected(codes []string) error {
	sort.Strings(codes)
	return errs.New(errs.CodeSigningRejected, "bridge: release inspection refused the transaction").WithField("reason_codes", codes)
}

// ReleaseExpectation is the release the platform decided to relay: the lock
// it relays (source chain, tx id, nonce), the units it mints (mint, owner,
// amount) and the chain it executes on.
type ReleaseExpectation struct {
	Chain       devnet.ChainID
	Mint        devnet.PublicKey
	ToOwner     devnet.PublicKey
	Amount      uint64
	SourceChain devnet.ChainID
	SourceTxID  devnet.TxID
	Nonce       uint64
}

func (e ReleaseExpectation) release(authority devnet.PublicKey) devnet.BridgeRelease {
	return devnet.BridgeRelease{Mint: e.Mint, ToOwner: e.ToOwner, Amount: e.Amount, SourceChain: e.SourceChain,
		SourceTxID: e.SourceTxID, Nonce: e.Nonce, Authority: authority}
}

func inspectRelease(attester devnet.PublicKey, e ReleaseExpectation, m devnet.Message) []string {
	var codes []string
	if m.ChainID != e.Chain {
		codes = append(codes, InspectChainMismatch)
	}
	if m.FeePayer != attester {
		codes = append(codes, InspectFeePayerMismatch)
	}
	if len(m.Instructions) != 1 {
		return append(codes, InspectInstructionCount)
	}
	if got, ok := m.Instructions[0].(devnet.BridgeRelease); !ok || got != e.release(attester) {
		codes = append(codes, InspectReleaseMismatch)
	}
	signers := m.RequiredSigners()
	if len(signers) != 1 || signers[0] != attester {
		codes = append(codes, InspectSignerSetMismatch)
	}
	return codes
}

// InspectRelease verifies a signed release against the expectation: exactly
// one BRIDGE_RELEASE equal to it, authorised and paid by the attester alone,
// with valid signatures. It returns the TxID.
func InspectRelease(attester devnet.PublicKey, e ReleaseExpectation, raw []byte) (devnet.TxID, error) {
	tx, err := devnet.Decode(raw)
	if err != nil {
		return "", rejected([]string{InspectMalformed})
	}
	if codes := inspectRelease(attester, e, tx.Message); len(codes) > 0 {
		return "", rejected(codes)
	}
	if err := tx.VerifySignatures(); err != nil {
		return "", rejected([]string{InspectSignatureInvalid})
	}
	id, err := tx.ID()
	if err != nil {
		return "", rejected([]string{InspectSignatureInvalid})
	}
	return id, nil
}

// Attester holds the bridge authority key (the key a destination chain's
// BridgeConfig names). It signs one thing -- a BRIDGE_RELEASE equal to a
// release the platform decided to relay -- and only after inspection. Its key
// lives only in a closure: no getter, no logging, every formatting verb
// prints the public key.
type Attester struct {
	public devnet.PublicKey
	sign   func(devnet.Message) (devnet.Transaction, error)
}

// NewAttester derives the attester from seed. The caller must not keep the
// seed.
func NewAttester(seed [devnet.SeedSize]byte) *Attester {
	kp := devnet.NewKeypairFromSeed(seed)
	return &Attester{public: kp.PublicKey(), sign: func(m devnet.Message) (devnet.Transaction, error) { return devnet.Sign(m, kp) }}
}

// PublicKey returns the bridge authority's address.
func (a *Attester) PublicKey() devnet.PublicKey { return a.public }

// SignRelease inspects msg against e and, only if it passes, signs it as the
// bridge authority and fee payer.
func (a *Attester) SignRelease(e ReleaseExpectation, msg devnet.Message) (devnet.TxID, []byte, error) {
	raw, err := devnet.Transaction{Message: msg}.Encode()
	if err != nil {
		return "", nil, rejected([]string{InspectMalformed})
	}
	decoded, err := devnet.Decode(raw)
	if err != nil {
		return "", nil, rejected([]string{InspectMalformed})
	}
	if codes := inspectRelease(a.public, e, decoded.Message); len(codes) > 0 {
		return "", nil, rejected(codes)
	}
	tx, err := a.sign(decoded.Message)
	if err != nil {
		return "", nil, errs.Wrap(err, errs.CodeSigningRejected, "bridge: the attester could not sign")
	}
	signed, err := tx.Encode()
	if err != nil {
		return "", nil, rejected([]string{InspectMalformed})
	}
	id, err := InspectRelease(a.public, e, signed)
	if err != nil {
		return "", nil, err
	}
	return id, signed, nil
}

// String returns "Attester(<public key>)".
func (a *Attester) String() string { return "Attester(" + a.public.String() + ")" }

// Format prints String for every verb.
func (a *Attester) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, a.String()) }
