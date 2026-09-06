package proof

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/proof/prooftest"
)

func TestKeySet_ResolvesByTheKeyIDTheRowNames(t *testing.T) {
	ctx := context.Background()
	old, _ := newLocalSigner(t)
	current, _ := newLocalSigner(t)
	digest := testDigest("checkpoint")

	oldSig, oldID, _, err := old.Sign(ctx, digest)
	require.NoError(t, err)
	newSig, newID, _, err := current.Sign(ctx, digest)
	require.NoError(t, err)
	require.NotEqual(t, oldID, newID)

	// After a rotation both keys are trusted: the retired one still verifies
	// what it signed, the active one verifies what it signs now.
	ks, err := NewKeySet(old.TrustedKey(KeyRetired), current.TrustedKey(KeyActive))
	require.NoError(t, err)
	assert.NoError(t, ks.Verify(ctx, digest, oldSig, oldID), "a retired key still verifies its own checkpoints")
	assert.NoError(t, ks.Verify(ctx, digest, newSig, newID))
	assert.Equal(t, 2, ks.Len())
	assert.Equal(t, []string{minStr(oldID, newID), maxStr(oldID, newID)}, ks.KeyIDs())
	st, ok := ks.Status(oldID)
	assert.True(t, ok)
	assert.Equal(t, KeyRetired, st)

	// Each signature is judged by the key it names, never by the other one.
	assert.ErrorIs(t, ks.Verify(ctx, digest, oldSig, newID), ErrSignatureInvalid, "old signature under the new key id")
	assert.ErrorIs(t, ks.Verify(ctx, digest, newSig, oldID), ErrSignatureInvalid)
	assert.ErrorIs(t, ks.Verify(ctx, testDigest("other"), newSig, newID), ErrSignatureInvalid)
}

func TestKeySet_UnknownRevokedAndInvalidAreDistinct(t *testing.T) {
	ctx := context.Background()
	trusted, _ := newLocalSigner(t)
	stranger, _ := newLocalSigner(t)
	digest := testDigest("checkpoint")
	sig, keyID, _, err := trusted.Sign(ctx, digest)
	require.NoError(t, err)
	strangerSig, strangerID, _, err := stranger.Sign(ctx, digest)
	require.NoError(t, err)

	ks, err := NewKeySet(trusted.TrustedKey(KeyActive))
	require.NoError(t, err)

	// Unknown: the id was never trusted. The signature is not judged at all.
	err = ks.Verify(ctx, digest, strangerSig, strangerID)
	assert.ErrorIs(t, err, ErrKeyUnknown)
	assert.NotErrorIs(t, err, ErrSignatureInvalid, "an untrusted key must never be reported as a bad signature")
	assert.NotErrorIs(t, err, ErrKeyRevoked)
	_, ok := ks.Status(strangerID)
	assert.False(t, ok)

	// Revoked: trusted at some point, deliberately withdrawn now. No key
	// material is needed and the rejection precedes any cryptography, so a
	// valid signature is still refused.
	revoked, err := NewKeySet(RevokedKey(keyID))
	require.NoError(t, err)
	err = revoked.Verify(ctx, digest, sig, keyID)
	assert.ErrorIs(t, err, ErrKeyRevoked)
	assert.NotErrorIs(t, err, ErrKeyUnknown)
	assert.NotErrorIs(t, err, ErrSignatureInvalid)
	st, ok := revoked.Status(keyID)
	assert.True(t, ok)
	assert.Equal(t, KeyRevoked, st)

	// Invalid: trusted key, signature does not verify. This one is tampering.
	tampered := append([]byte(nil), sig...)
	tampered[len(tampered)-1] ^= 0x01
	err = ks.Verify(ctx, digest, tampered, keyID)
	assert.ErrorIs(t, err, ErrSignatureInvalid)
	assert.NotErrorIs(t, err, ErrKeyUnknown)
	assert.NotErrorIs(t, err, ErrKeyRevoked)
}

func TestKeySet_EmptyAndNil(t *testing.T) {
	ctx := context.Background()
	empty, err := NewKeySet()
	require.NoError(t, err)
	assert.Zero(t, empty.Len())
	assert.Empty(t, empty.KeyIDs())
	assert.ErrorIs(t, empty.Verify(ctx, testDigest("d"), []byte{1}, "k"), ErrKeyUnknown)

	var nilSet *KeySet
	assert.Zero(t, nilSet.Len())
	assert.Nil(t, nilSet.KeyIDs())
	_, ok := nilSet.Status("k")
	assert.False(t, ok)
	assert.ErrorIs(t, nilSet.Verify(ctx, testDigest("d"), []byte{1}, "k"), ErrKeyUnknown)
}

func TestKeySet_RejectsUnusableEntries(t *testing.T) {
	s, _ := newLocalSigner(t)
	cases := map[string][]TrustedKey{
		"empty id":          {{KeyID: "", Status: KeyActive, Verifier: s}},
		"undeclared status": {{KeyID: "k", Status: "sometimes", Verifier: s}},
		"empty status":      {{KeyID: "k", Verifier: s}},
		"active without a verifier": {
			{KeyID: "k", Status: KeyActive},
		},
		"retired without a verifier": {
			{KeyID: "k", Status: KeyRetired},
		},
		"duplicate id": {s.TrustedKey(KeyActive), s.TrustedKey(KeyRetired)},
	}
	for name, keys := range cases {
		ks, err := NewKeySet(keys...)
		assert.Nil(t, ks, name)
		assert.Error(t, err, name)
	}
	// A revoked key needs no material: the material may be long gone.
	ks, err := NewKeySet(RevokedKey("k"))
	require.NoError(t, err)
	assert.Equal(t, 1, ks.Len())
}

func TestKeySet_KMSKeysAreTrustedByARN(t *testing.T) {
	ctx := context.Background()
	api, s := newFakeKMS(t)
	digest := testDigest("checkpoint")
	sig, keyID, _, err := s.Sign(ctx, digest)
	require.NoError(t, err)
	require.Equal(t, api.ARN(), keyID, "the row records the ARN")

	// Trusting the alias would not be a decision about a key: an alias can be
	// re-pointed. The set names the ARN the row carries.
	aliasOnly, err := NewKeySet(s.TrustedKey(fakeKeyAlias, KeyActive))
	require.NoError(t, err)
	assert.ErrorIs(t, aliasOnly.Verify(ctx, digest, sig, keyID), ErrKeyUnknown)

	byARN, err := NewKeySet(s.TrustedKey(keyID, KeyActive))
	require.NoError(t, err)
	assert.NoError(t, byARN.Verify(ctx, digest, sig, keyID))

	// A rotated-away KMS key stays verifiable through the same client.
	rotated, err := prooftest.NewFakeKMS("arn:aws:kms:us-east-1:111122223333:key/old")
	require.NoError(t, err)
	oldSigner, err := NewKMSSigner(rotated, rotated.ARN())
	require.NoError(t, err)
	oldSig, oldID, _, err := oldSigner.Sign(ctx, digest)
	require.NoError(t, err)
	mixed, err := NewKeySet(s.TrustedKey(keyID, KeyActive), oldSigner.TrustedKey(oldID, KeyRetired))
	require.NoError(t, err)
	assert.NoError(t, mixed.Verify(ctx, digest, sig, keyID))
	assert.NoError(t, mixed.Verify(ctx, digest, oldSig, oldID))
	assert.ErrorIs(t, mixed.Verify(ctx, digest, oldSig, keyID), ErrSignatureInvalid, "each key judges only its own signature")
}

func TestKeyStatus_Valid(t *testing.T) {
	for _, s := range []KeyStatus{KeyActive, KeyRetired, KeyRevoked} {
		assert.True(t, s.Valid(), s)
	}
	for _, s := range []KeyStatus{"", "expired", "ACTIVE"} {
		assert.False(t, s.Valid(), s)
	}
}

func TestLocalSigner_TrustedKeyUsesItsOwnID(t *testing.T) {
	s, key := newLocalSigner(t)
	tk := s.TrustedKey(KeyRetired)
	assert.Equal(t, LocalKeyID(&key.PublicKey), tk.KeyID)
	assert.Equal(t, KeyRetired, tk.Status)
	assert.Same(t, s, tk.Verifier)
	// The same key loaded from PEM in another process yields the same id, so
	// a key kept across restarts keeps verifying its checkpoints.
	pemText, err := MarshalLocalKeyPEM(key)
	require.NoError(t, err)
	reloaded, err := ParseLocalKeyPEM(pemText)
	require.NoError(t, err)
	again, err := NewLocalECDSASigner(config.EnvTest, reloaded)
	require.NoError(t, err)
	assert.Equal(t, tk.KeyID, again.TrustedKey(KeyActive).KeyID)
}

func minStr(a, b string) string {
	if a < b {
		return a
	}
	return b
}

func maxStr(a, b string) string {
	if a < b {
		return b
	}
	return a
}
