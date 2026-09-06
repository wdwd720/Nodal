package proof

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
)

func testDigest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func newLocalSigner(t *testing.T) (*LocalECDSASigner, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := GenerateLocalKey()
	require.NoError(t, err)
	s, err := NewLocalECDSASigner(config.EnvTest, key)
	require.NoError(t, err)
	return s, key
}

func TestLocalSigner_RoundTrip(t *testing.T) {
	ctx := context.Background()
	s, key := newLocalSigner(t)
	digest := testDigest("checkpoint")
	sig, keyID, alg, err := s.Sign(ctx, digest)
	require.NoError(t, err)
	assert.Equal(t, AlgorithmECDSASHA256, alg)
	assert.Equal(t, LocalKeyID(&key.PublicKey), keyID)
	assert.True(t, strings.HasPrefix(keyID, "local-test:"))
	assert.Equal(t, SignerLocalTest, s.Kind())
	assert.NoError(t, s.Verify(ctx, digest, sig, keyID))
	// DER signatures verify with the standard library too (KMS-compatible encoding).
	assert.True(t, ecdsa.VerifyASN1(&key.PublicKey, digest, sig))

	assert.ErrorIs(t, s.Verify(ctx, testDigest("other"), sig, keyID), ErrSignatureInvalid, "different digest")
	tampered := append([]byte(nil), sig...)
	tampered[len(tampered)-1] ^= 0x01
	assert.ErrorIs(t, s.Verify(ctx, digest, tampered, keyID), ErrSignatureInvalid, "flipped signature bit")
	assert.ErrorIs(t, s.Verify(ctx, digest, sig, "local-test:someone-else"), ErrKeyMismatch, "foreign key id")
	other, _ := newLocalSigner(t)
	assert.ErrorIs(t, other.Verify(ctx, digest, sig, keyID), ErrKeyMismatch, "another verifier's key")
	assert.ErrorIs(t, other.Verify(ctx, digest, sig, other.KeyID()), ErrSignatureInvalid, "another key under its own id")
}

func TestLocalSigner_SignsOnlyDigests(t *testing.T) {
	ctx := context.Background()
	s, _ := newLocalSigner(t)
	for _, bad := range [][]byte{nil, []byte("short"), make([]byte, 31), make([]byte, 33), []byte(strings.Repeat("x", 64))} {
		_, _, _, err := s.Sign(ctx, bad)
		assert.ErrorIs(t, err, ErrDigestSize, "len %d", len(bad))
		assert.ErrorIs(t, s.Verify(ctx, bad, []byte{1}, s.KeyID()), ErrDigestSize, "len %d", len(bad))
	}
}

func TestLocalSigner_RefusedInProductionLikeEnvironments(t *testing.T) {
	key, err := GenerateLocalKey()
	require.NoError(t, err)
	for _, env := range []config.Environment{config.EnvStaging, config.EnvProd} {
		s, err := NewLocalECDSASigner(env, key)
		assert.Nil(t, s)
		require.Error(t, err, env)
		assert.Contains(t, err.Error(), "KMS")
	}
	for _, env := range []config.Environment{config.EnvLocal, config.EnvTest, config.EnvDev} {
		_, err := NewLocalECDSASigner(env, key)
		assert.NoError(t, err, env)
	}
}

func TestLocalSigner_RequiresP256(t *testing.T) {
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	_, err = NewLocalECDSASigner(config.EnvTest, p384)
	assert.Error(t, err)
	_, err = NewLocalECDSASigner(config.EnvTest, nil)
	assert.Error(t, err)
}

func TestLocalKey_PEMRoundTrip(t *testing.T) {
	key, err := GenerateLocalKey()
	require.NoError(t, err)
	pkcs8, err := MarshalLocalKeyPEM(key)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(pkcs8, "-----BEGIN PRIVATE KEY-----"))
	parsed, err := ParseLocalKeyPEM(pkcs8)
	require.NoError(t, err)
	assert.True(t, key.Equal(parsed))
	assert.Equal(t, LocalKeyID(&key.PublicKey), LocalKeyID(&parsed.PublicKey), "key id is a function of the public key only")

	sec1, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	parsedSEC1, err := ParseLocalKeyPEM(string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1})))
	require.NoError(t, err)
	assert.True(t, key.Equal(parsedSEC1))

	// A verifier built from the PEM verifies signatures of the original.
	signer, err := NewLocalECDSASigner(config.EnvTest, key)
	require.NoError(t, err)
	verifier, err := NewLocalECDSASigner(config.EnvTest, parsed)
	require.NoError(t, err)
	sig, keyID, _, err := signer.Sign(context.Background(), testDigest("d"))
	require.NoError(t, err)
	assert.NoError(t, verifier.Verify(context.Background(), testDigest("d"), sig, keyID))
}

func TestLocalKey_PEMRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"empty":       "",
		"garbage":     "not pem at all",
		"wrong block": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1, 2, 3}})),
		"bad der":     string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte{1, 2, 3}})),
		"bad pkcs8":   string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1, 2, 3}})),
	}
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(p384)
	require.NoError(t, err)
	cases["p384"] = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	for name, in := range cases {
		_, err := ParseLocalKeyPEM(in)
		assert.Error(t, err, name)
		if err != nil && in != "" {
			assert.NotContains(t, err.Error(), in, "%s: error must not echo key material", name)
			assert.NotContains(t, err.Error(), "MIG", "%s: error must not echo key material", name)
		}
	}
}

func TestSignerKind_Valid(t *testing.T) {
	assert.True(t, SignerKMS.Valid())
	assert.True(t, SignerLocalTest.Valid())
	assert.False(t, SignerKind("").Valid())
	assert.False(t, SignerKind("hsm").Valid())
}
