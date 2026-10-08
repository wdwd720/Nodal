package stableasset

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"io"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/errs"
)

// SignedTransaction is a signed devnet transaction ready to submit.
type SignedTransaction struct {
	TxID devnet.TxID
	Raw  []byte
}

// Key custody. Every signer below keeps its private key only inside a
// closure, exactly as devnet.Keypair does: a struct field can be read by
// reflection and is printed raw by fmt for a bad verb such as %p, a captured
// variable is reachable by neither. There is no getter for any private key or
// seed, nothing here logs, and every signer formats as its public key for
// every verb. Callers must not retain the seeds they pass in.

// IssuerSigner holds a stable asset's devnet mint authority. It signs one
// thing -- a TOKEN_MINT_TO bound by MEMO to a mint plan -- and only after the
// transaction inspector has proved the transaction is exactly what the plan
// says.
type IssuerSigner struct {
	public devnet.PublicKey
	sign   func(devnet.Message) (devnet.Transaction, error)
}

// NewIssuerSigner derives the mint authority from seed.
func NewIssuerSigner(seed [devnet.SeedSize]byte) *IssuerSigner {
	kp := devnet.NewKeypairFromSeed(seed)
	return &IssuerSigner{
		public: kp.PublicKey(),
		sign:   func(m devnet.Message) (devnet.Transaction, error) { return devnet.Sign(m, kp) },
	}
}

// PublicKey returns the mint authority's address.
func (s *IssuerSigner) PublicKey() devnet.PublicKey { return s.public }

// SignMint inspects msg against plan and, only if it passes, signs it as the
// mint authority and fee payer. The signed bytes are inspected again
// (signatures included) before they are returned.
func (s *IssuerSigner) SignMint(plan MintPlan, msg devnet.Message) (SignedTransaction, error) {
	if plan.MintAuthority != s.public.String() {
		return SignedTransaction{}, rejected([]string{InspectSignerNotHeld})
	}
	m, err := canonicalMessage(msg)
	if err != nil {
		return SignedTransaction{}, err
	}
	if codes := inspectMint(plan, m); len(codes) > 0 {
		return SignedTransaction{}, rejected(codes)
	}
	tx, err := s.sign(m)
	if err != nil {
		return SignedTransaction{}, errs.Wrap(err, errs.CodeSigningRejected, "stableasset: the mint authority could not sign")
	}
	raw, err := tx.Encode()
	if err != nil {
		return SignedTransaction{}, rejected([]string{InspectMalformed})
	}
	id, err := InspectMintTransaction(plan, raw)
	if err != nil {
		return SignedTransaction{}, err
	}
	return SignedTransaction{TxID: id, Raw: raw}, nil
}

// String returns "IssuerSigner(<public key>)".
func (s *IssuerSigner) String() string { return "IssuerSigner(" + s.public.String() + ")" }

// Format prints String for every verb.
func (s *IssuerSigner) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, s.String()) }

// CustodialWalletSigner custodies the sandbox devnet wallets of customers.
// Each account's key is derived from one master seed (HMAC-SHA256 over the
// account id), so nothing is stored and a restart recovers every wallet. It
// signs only the two holder-authorised programs the platform uses -- a
// TOKEN_BURN bound to a burn plan and a BRIDGE_LOCK bound to a bridge plan --
// and only after inspection.
type CustodialWalletSigner struct {
	derive func(account accounts.AccountID) devnet.Keypair
}

const custodialDomain = "nodal-sandbox/custodial-wallet/v1\x00"

// NewCustodialWalletSigner returns the custodian for masterSeed.
func NewCustodialWalletSigner(masterSeed [32]byte) *CustodialWalletSigner {
	seed := masterSeed
	return &CustodialWalletSigner{derive: func(account accounts.AccountID) devnet.Keypair {
		mac := hmac.New(sha256.New, seed[:])
		_, _ = mac.Write([]byte(custodialDomain + account.String()))
		var s [devnet.SeedSize]byte
		copy(s[:], mac.Sum(nil))
		return devnet.NewKeypairFromSeed(s)
	}}
}

// HolderAddress returns the account's sandbox wallet address (the same on
// every simulated chain).
func (c *CustodialWalletSigner) HolderAddress(account accounts.AccountID) devnet.PublicKey {
	return c.derive(account).PublicKey()
}

// SignHolderBurn inspects msg against a burn plan and signs it as the
// account's wallet, which must be the plan's holder.
func (c *CustodialWalletSigner) SignHolderBurn(account accounts.AccountID, plan BurnPlan, msg devnet.Message) (SignedTransaction, error) {
	kp := c.derive(account)
	if plan.Holder != kp.PublicKey().String() {
		return SignedTransaction{}, rejected([]string{InspectSignerNotHeld})
	}
	m, err := canonicalMessage(msg)
	if err != nil {
		return SignedTransaction{}, err
	}
	if codes := inspectBurn(plan, m); len(codes) > 0 {
		return SignedTransaction{}, rejected(codes)
	}
	tx, err := devnet.Sign(m, kp)
	if err != nil {
		return SignedTransaction{}, errs.Wrap(err, errs.CodeSigningRejected, "stableasset: the holder wallet could not sign")
	}
	raw, err := tx.Encode()
	if err != nil {
		return SignedTransaction{}, rejected([]string{InspectMalformed})
	}
	id, err := InspectBurnTransaction(plan, raw)
	if err != nil {
		return SignedTransaction{}, err
	}
	return SignedTransaction{TxID: id, Raw: raw}, nil
}

// SignHolderLock inspects msg as the BRIDGE_LOCK lock bound to planHash and
// signs it as the account's wallet, which must own the lock and pay the fee.
// It is the holder signature internal/bridge needs.
func (c *CustodialWalletSigner) SignHolderLock(account accounts.AccountID, planHash [32]byte, lock devnet.BridgeLock, msg devnet.Message) (devnet.TxID, []byte, error) {
	kp := c.derive(account)
	m, err := canonicalMessage(msg)
	if err != nil {
		return "", nil, err
	}
	inspect := func(m devnet.Message) []string { return inspectLock(kp.PublicKey(), planHash, lock, m) }
	if codes := inspect(m); len(codes) > 0 {
		return "", nil, rejected(codes)
	}
	tx, err := devnet.Sign(m, kp)
	if err != nil {
		return "", nil, errs.Wrap(err, errs.CodeSigningRejected, "stableasset: the holder wallet could not sign")
	}
	raw, err := tx.Encode()
	if err != nil {
		return "", nil, rejected([]string{InspectMalformed})
	}
	id, err := inspectSigned(raw, inspect)
	if err != nil {
		return "", nil, err
	}
	return id, raw, nil
}

// String returns "CustodialWalletSigner".
func (c *CustodialWalletSigner) String() string { return "CustodialWalletSigner" }

// Format prints String for every verb.
func (c *CustodialWalletSigner) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

// SnapshotSigner signs reserve snapshots with an Ed25519 key (crypto/ed25519).
// The key is injected; the signer exposes only the public half.
type SnapshotSigner struct {
	public ed25519.PublicKey
	sign   func(msg []byte) []byte
}

// NewSnapshotSigner derives the snapshot key from seed.
func NewSnapshotSigner(seed [ed25519.SeedSize]byte) *SnapshotSigner {
	private := ed25519.NewKeyFromSeed(seed[:])
	public := append(ed25519.PublicKey(nil), private.Public().(ed25519.PublicKey)...)
	return &SnapshotSigner{public: public, sign: func(msg []byte) []byte { return ed25519.Sign(private, msg) }}
}

// PublicKey returns a copy of the verification key.
func (s *SnapshotSigner) PublicKey() ed25519.PublicKey {
	return append(ed25519.PublicKey(nil), s.public...)
}

// String returns "SnapshotSigner(<public key hex>)".
func (s *SnapshotSigner) String() string { return fmt.Sprintf("SnapshotSigner(%x)", []byte(s.public)) }

// Format prints String for every verb.
func (s *SnapshotSigner) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, s.String()) }
