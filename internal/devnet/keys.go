package devnet

import (
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"
	"io"

	"github.com/mr-tron/base58"
)

// Sizes of the fixed-width values on the wire.
const (
	// PublicKeySize is the size of an Ed25519 public key, i.e. an address.
	PublicKeySize = ed25519.PublicKeySize
	// SignatureSize is the size of an Ed25519 signature.
	SignatureSize = ed25519.SignatureSize
	// HashSize is the size of a block hash and of a StateHash.
	HashSize = sha256.Size
	// SeedSize is the size of the seed NewKeypairFromSeed expands.
	SeedSize = ed25519.SeedSize
)

// Upper bounds on base58 text, checked before decoding so a hostile string
// cannot make the quadratic decoder do unbounded work: 32 bytes encode to at
// most 44 characters and 64 bytes to at most 88.
const (
	maxBase58Len32 = 44
	maxBase58Len64 = 88
)

// PublicKey is a 32-byte Ed25519 public key. It addresses an account, a mint
// or an authority. Its text form is base58.
type PublicKey [PublicKeySize]byte

// String returns the base58 form.
func (k PublicKey) String() string { return base58.Encode(k[:]) }

// IsZero reports whether k is all zero bytes: the "unset" value, which no
// fee payer and no instruction field may use.
func (k PublicKey) IsZero() bool { return k == PublicKey{} }

// ParsePublicKey parses the canonical base58 form of a 32-byte key.
func ParsePublicKey(s string) (PublicKey, error) {
	var k PublicKey
	if err := decodeBase58(s, k[:], maxBase58Len32, "public key"); err != nil {
		return PublicKey{}, err
	}
	return k, nil
}

// Signature is a 64-byte Ed25519 signature. Its text form is base58.
type Signature [SignatureSize]byte

// String returns the base58 form.
func (s Signature) String() string { return base58.Encode(s[:]) }

// ParseSignature parses the canonical base58 form of a 64-byte signature.
func ParseSignature(s string) (Signature, error) {
	var sig Signature
	if err := decodeBase58(s, sig[:], maxBase58Len64, "signature"); err != nil {
		return Signature{}, err
	}
	return sig, nil
}

// Hash is a 32-byte SHA-256 digest: a block hash (the recent blockhash a
// transaction references) or a StateHash. Its text form is base58.
type Hash [HashSize]byte

// String returns the base58 form.
func (h Hash) String() string { return base58.Encode(h[:]) }

// IsZero reports whether h is all zero bytes.
func (h Hash) IsZero() bool { return h == Hash{} }

// ParseHash parses the canonical base58 form of a 32-byte hash.
func ParseHash(s string) (Hash, error) {
	var h Hash
	if err := decodeBase58(s, h[:], maxBase58Len32, "hash"); err != nil {
		return Hash{}, err
	}
	return h, nil
}

// decodeBase58 decodes s into dst, which it must fill exactly, and insists s
// is the canonical encoding so every value has exactly one text form.
func decodeBase58(s string, dst []byte, maxLen int, what string) error {
	if s == "" || len(s) > maxLen {
		return invalid(CodeInvalidBase58, what+" is not canonical base58 of the expected size")
	}
	b, err := base58.Decode(s)
	if err != nil || len(b) != len(dst) || base58.Encode(b) != s {
		return invalid(CodeInvalidBase58, what+" is not canonical base58 of the expected size")
	}
	copy(dst, b)
	return nil
}

// Keypair is an Ed25519 key pair derived from a caller-supplied seed. It is a
// crypto helper only: this package never stores, selects or authorises keys.
//
// The private key lives only inside a signing closure. A struct field could
// be read by reflection, and fmt prints raw fields for a bad verb such as %p
// without consulting Format; a captured variable is reachable by neither, so
// no formatter, logger or diff can print it. A Keypair formats as its public
// key.
//
// The zero Keypair is unusable: Sign refuses it.
type Keypair struct {
	public PublicKey
	sign   func(msg []byte) Signature
}

// NewKeypairFromSeed derives the RFC 8032 key pair for seed. The same seed
// always yields the same key pair, which is what deterministic tests need.
func NewKeypairFromSeed(seed [SeedSize]byte) Keypair {
	private := ed25519.NewKeyFromSeed(seed[:])
	var public PublicKey
	// crypto/ed25519 lays a private key out as seed || public key.
	copy(public[:], private[SeedSize:])
	return Keypair{public: public, sign: func(msg []byte) Signature {
		var sig Signature
		copy(sig[:], ed25519.Sign(private, msg))
		return sig
	}}
}

// PublicKey returns the public half.
func (k Keypair) PublicKey() PublicKey { return k.public }

// Sign signs msg with RFC 8032 Ed25519. Signing is deterministic: the same
// key and message always give the same signature.
func (k Keypair) Sign(msg []byte) (Signature, error) {
	if k.sign == nil {
		return Signature{}, invalid(CodeKeypairUninitialized, "keypair is not initialized")
	}
	return k.sign(msg), nil
}

// String returns "Keypair(<public key>)".
func (k Keypair) String() string { return "Keypair(" + k.public.String() + ")" }

// Format prints String for every verb, so %v, %+v, %#v, %x and %s all
// redact the private key.
func (k Keypair) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, k.String())
}

// Verify reports whether sig is a valid Ed25519 signature of msg by pub, as
// crypto/ed25519 defines it (RFC 8032, canonical S required).
func Verify(pub PublicKey, msg []byte, sig Signature) bool {
	return ed25519.Verify(pub[:], msg, sig[:])
}
