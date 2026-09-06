package proof

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// KeyStatus is the trust state of one signing key id. Rotation is a normal,
// scheduled operation, so a checkpoint signed years ago by a key that is no
// longer used must keep verifying; only a deliberate revocation changes that.
type KeyStatus string

// Key statuses.
const (
	// KeyActive signs new checkpoints and verifies old ones.
	KeyActive KeyStatus = "active"
	// KeyRetired no longer signs (it was rotated out) but still verifies
	// every checkpoint it signed. Retiring a key is not a statement about
	// the signatures it already made.
	KeyRetired KeyStatus = "retired"
	// KeyRevoked is withdrawn: the key is believed compromised or the
	// signatures it made are no longer accepted as evidence. Checkpoints
	// naming it fail verification with their own distinct reason, never as
	// "signature does not verify", so an operator can tell a withdrawn key
	// from a forged signature. A revoked entry needs no key material: the
	// rejection happens before any cryptography.
	KeyRevoked KeyStatus = "revoked"
)

// Valid reports whether s is a declared status.
func (s KeyStatus) Valid() bool { return s == KeyActive || s == KeyRetired || s == KeyRevoked }

// Key-trust errors. They are verification outcomes, not configuration
// failures, and each maps to its own FailureKind.
var (
	// ErrKeyUnknown: the checkpoint names a key id that is not in the
	// trusted set. This is what a rotation gap looks like (the new or old
	// key id was never added to the trusted set), and it is NOT evidence of
	// tampering: nothing about the signature has been judged.
	ErrKeyUnknown = errors.New("proof: checkpoint names a signing key that is not in the trusted key set")
	// ErrKeyRevoked: the checkpoint names a key whose signatures are no
	// longer accepted.
	ErrKeyRevoked = errors.New("proof: checkpoint was signed with a revoked key")
)

// SignatureVerifier verifies a signature made by the key named keyID. Both
// LocalECDSASigner and KMSSigner satisfy it.
type SignatureVerifier interface {
	Verify(ctx context.Context, digest, sig []byte, keyID string) error
}

// TrustedKey is one entry of a KeySet: a key id, what trusting it means, and
// how to verify a signature made with it. Verifier is required unless Status
// is KeyRevoked.
type TrustedKey struct {
	KeyID    string
	Status   KeyStatus
	Verifier SignatureVerifier
}

// KeySet is the set of signing keys a verifier accepts, addressed by the key
// id recorded on the checkpoint row. It is the whole of the trust decision:
// a checkpoint is verified with the key it names, and only if an operator
// put that key id in the set. Resolving by id is what makes rotation safe
// (several keys are trusted at once, each verifying its own checkpoints) and
// what stops a forged row from choosing its own verifying key.
//
// A KeySet is immutable after construction and safe for concurrent use.
type KeySet struct {
	mu      sync.RWMutex
	entries map[string]TrustedKey
}

// NewKeySet builds a key set. It rejects an empty key id, a duplicate id, an
// undeclared status, and a missing verifier on a key that is not revoked: a
// trusted key that cannot verify anything would turn every checkpoint it
// signed into a false tampering report.
func NewKeySet(keys ...TrustedKey) (*KeySet, error) {
	ks := &KeySet{entries: make(map[string]TrustedKey, len(keys))}
	for _, k := range keys {
		switch {
		case k.KeyID == "":
			return nil, errors.New("proof: trusted key requires a key id")
		case !k.Status.Valid():
			return nil, fmt.Errorf("proof: trusted key %q has undeclared status %q", k.KeyID, k.Status)
		case k.Verifier == nil && k.Status != KeyRevoked:
			return nil, fmt.Errorf("proof: trusted key %q (%s) requires a verifier", k.KeyID, k.Status)
		}
		if _, dup := ks.entries[k.KeyID]; dup {
			return nil, fmt.Errorf("proof: trusted key %q is listed twice", k.KeyID)
		}
		ks.entries[k.KeyID] = k
	}
	return ks, nil
}

// Len returns the number of trusted key ids (revoked ones included).
func (ks *KeySet) Len() int {
	if ks == nil {
		return 0
	}
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	return len(ks.entries)
}

// KeyIDs returns every key id in the set, sorted.
func (ks *KeySet) KeyIDs() []string {
	if ks == nil {
		return nil
	}
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	out := make([]string, 0, len(ks.entries))
	for id := range ks.entries {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Status returns the status of keyID and whether it is in the set.
func (ks *KeySet) Status(keyID string) (KeyStatus, bool) {
	if ks == nil {
		return "", false
	}
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	e, ok := ks.entries[keyID]
	return e.Status, ok
}

// Verify checks sig over digest with the key the caller names, which is the
// key id persisted on the checkpoint row. It returns ErrKeyUnknown when the
// id is not trusted, ErrKeyRevoked when it has been withdrawn, and whatever
// the key's verifier returns otherwise (ErrSignatureInvalid for a signature
// that does not verify).
func (ks *KeySet) Verify(ctx context.Context, digest, sig []byte, keyID string) error {
	if ks == nil || ks.Len() == 0 {
		return fmt.Errorf("%w: %q (the trusted key set is empty)", ErrKeyUnknown, keyID)
	}
	ks.mu.RLock()
	e, ok := ks.entries[keyID]
	ks.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %q (trusted: %v)", ErrKeyUnknown, keyID, ks.KeyIDs())
	}
	if e.Status == KeyRevoked {
		return fmt.Errorf("%w: %q", ErrKeyRevoked, keyID)
	}
	return e.Verifier.Verify(ctx, digest, sig, keyID)
}

// TrustedKey returns the entry that trusts this local signer's own key with
// the given status, for building a KeySet in tests and development.
func (s *LocalECDSASigner) TrustedKey(status KeyStatus) TrustedKey {
	return TrustedKey{KeyID: s.keyID, Status: status, Verifier: s}
}

// TrustedKey returns an entry that trusts keyID and verifies it through this
// KMS client. keyID must be the identifier KMS reports for the key (its
// ARN), which is what Sign persists on the checkpoint row: an alias can be
// re-pointed at another key, so trusting an alias would not be a decision
// about a key at all.
func (s *KMSSigner) TrustedKey(keyID string, status KeyStatus) TrustedKey {
	return TrustedKey{KeyID: keyID, Status: status, Verifier: s}
}

// RevokedKey returns an entry that rejects every checkpoint naming keyID. No
// key material is needed to revoke a key.
func RevokedKey(keyID string) TrustedKey {
	return TrustedKey{KeyID: keyID, Status: KeyRevoked}
}
