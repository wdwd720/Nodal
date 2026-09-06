package proof

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"

	"github.com/nodal/controlplane/internal/config"
)

// AlgorithmECDSASHA256 is the only signature algorithm checkpoints use. It
// is the KMS SigningAlgorithmSpec name and is stored verbatim in
// audit_checkpoints.signature_algorithm.
const AlgorithmECDSASHA256 = "ECDSA_SHA_256"

// SignerKind names the signer implementation stored in
// audit_checkpoints.signer.
type SignerKind string

// Signer kinds.
const (
	SignerKMS       SignerKind = "kms"
	SignerLocalTest SignerKind = "local-test"
)

// Valid reports whether k is a declared kind.
func (k SignerKind) Valid() bool { return k == SignerKMS || k == SignerLocalTest }

// Signer errors.
var (
	ErrDigestSize       = errors.New("proof: digest must be exactly 32 bytes")
	ErrSignatureInvalid = errors.New("proof: signature does not verify")
	ErrKeyMismatch      = errors.New("proof: signature was made with a different key")
)

// Signer signs and verifies checkpoint digests. Sign receives exactly the
// 32-byte sha256 checkpoint digest (never a message) and returns the DER
// encoded ECDSA signature, the key identifier to persist and the algorithm
// name. Verify checks sig over digest for the persisted keyID, returning
// ErrSignatureInvalid (wrapped) when it does not verify.
type Signer interface {
	Sign(ctx context.Context, digest []byte) (sig []byte, keyID, algorithm string, err error)
	Verify(ctx context.Context, digest, sig []byte, keyID string) error
	Kind() SignerKind
}

// LocalECDSASigner signs with an in-process P-256 key. It is for LOCAL, TEST
// and DEV only: NewLocalECDSASigner refuses STAGING and PROD, where the
// signing key must live in KMS.
type LocalECDSASigner struct {
	key   *ecdsa.PrivateKey
	keyID string
}

var _ Signer = (*LocalECDSASigner)(nil)

// NewLocalECDSASigner wraps key. The key must be P-256.
func NewLocalECDSASigner(env config.Environment, key *ecdsa.PrivateKey) (*LocalECDSASigner, error) {
	if env.IsProductionLike() {
		return nil, fmt.Errorf("proof: local signing keys are not permitted in %s; configure the KMS audit signing key", env)
	}
	if key == nil || key.Curve != elliptic.P256() {
		return nil, errors.New("proof: local signer requires a P-256 private key")
	}
	return &LocalECDSASigner{key: key, keyID: LocalKeyID(&key.PublicKey)}, nil
}

// GenerateLocalKey returns a fresh P-256 key.
func GenerateLocalKey() (*ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("proof: generate key: %w", err)
	}
	return key, nil
}

// ParseLocalKeyPEM decodes a PEM "EC PRIVATE KEY" (SEC 1) or "PRIVATE KEY"
// (PKCS #8) block holding a P-256 key. The input is secret material: the
// returned errors never echo it and callers must never log it.
func ParseLocalKeyPEM(pemText string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("proof: no PEM block in local signing key")
	}
	var key *ecdsa.PrivateKey
	switch block.Type {
	case "EC PRIVATE KEY":
		k, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("proof: local signing key is not a valid EC private key")
		}
		key = k
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("proof: local signing key is not a valid PKCS#8 private key")
		}
		ec, ok := k.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("proof: local signing key is not an ECDSA key")
		}
		key = ec
	default:
		return nil, fmt.Errorf("proof: unsupported PEM block %q for local signing key", block.Type)
	}
	if key.Curve != elliptic.P256() {
		return nil, errors.New("proof: local signing key must use P-256")
	}
	return key, nil
}

// MarshalLocalKeyPEM encodes key as a PKCS #8 "PRIVATE KEY" PEM block, for
// writing a development key file. The result is secret material.
func MarshalLocalKeyPEM(key *ecdsa.PrivateKey) (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", fmt.Errorf("proof: marshal key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

// LocalKeyID derives the persisted key identifier of a local key:
// "local-test:" followed by 32 hex digits of sha256(SPKI DER). It depends
// only on the public key, so a verifier holding the same key file
// recognizes its own signatures.
func LocalKeyID(pub *ecdsa.PublicKey) string {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "local-test:invalid"
	}
	sum := sha256.Sum256(der)
	return "local-test:" + hex.EncodeToString(sum[:16])
}

// KeyID returns the signer's key identifier.
func (s *LocalECDSASigner) KeyID() string { return s.keyID }

// Kind implements Signer.
func (s *LocalECDSASigner) Kind() SignerKind { return SignerLocalTest }

// Sign implements Signer with a DER (ASN.1) encoded signature, the same
// encoding KMS returns.
func (s *LocalECDSASigner) Sign(_ context.Context, digest []byte) ([]byte, string, string, error) {
	if len(digest) != HashSize {
		return nil, "", "", ErrDigestSize
	}
	sig, err := ecdsa.SignASN1(rand.Reader, s.key, digest)
	if err != nil {
		return nil, "", "", fmt.Errorf("proof: local sign: %w", err)
	}
	return sig, s.keyID, AlgorithmECDSASHA256, nil
}

// Verify implements Signer.
func (s *LocalECDSASigner) Verify(_ context.Context, digest, sig []byte, keyID string) error {
	if len(digest) != HashSize {
		return ErrDigestSize
	}
	if keyID != s.keyID {
		return fmt.Errorf("%w: row names %q, verifier holds %q", ErrKeyMismatch, keyID, s.keyID)
	}
	if !ecdsa.VerifyASN1(&s.key.PublicKey, digest, sig) {
		return ErrSignatureInvalid
	}
	return nil
}
