package proof

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/proof/prooftest"
)

const (
	fakeKeyARN   = "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
	fakeKeyAlias = "alias/audit-signing"
)

func newFakeKMS(t *testing.T) (*prooftest.FakeKMS, *KMSSigner) {
	t.Helper()
	api, err := prooftest.NewFakeKMS(fakeKeyARN, fakeKeyAlias)
	require.NoError(t, err)
	s, err := NewKMSSigner(api, fakeKeyAlias)
	require.NoError(t, err)
	return api, s
}

func TestKMSSigner_SignUsesDigestAndECDSASHA256(t *testing.T) {
	ctx := context.Background()
	api, s := newFakeKMS(t)
	digest := testDigest("checkpoint")
	sig, keyID, alg, err := s.Sign(ctx, digest)
	require.NoError(t, err)
	assert.Equal(t, AlgorithmECDSASHA256, alg)
	assert.Equal(t, fakeKeyARN, keyID, "the persisted key id is the ARN KMS reports, not the alias")
	assert.Equal(t, SignerKMS, s.Kind())
	calls := api.SignCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, fakeKeyAlias, aws.ToString(calls[0].KeyId))
	assert.Equal(t, types.MessageTypeDigest, calls[0].MessageType)
	assert.Equal(t, types.SigningAlgorithmSpecEcdsaSha256, calls[0].SigningAlgorithm)
	assert.Equal(t, digest, calls[0].Message, "the digest is signed as-is, never re-hashed or wrapped")
	assert.True(t, ecdsa.VerifyASN1(api.PublicKey(), digest, sig), "DER signature over the digest")

	assert.NoError(t, s.Verify(ctx, digest, sig, keyID))
	vcalls := api.VerifyCalls()
	require.Len(t, vcalls, 1)
	assert.Equal(t, keyID, aws.ToString(vcalls[0].KeyId), "verification uses the row's key id")
	assert.Equal(t, types.MessageTypeDigest, vcalls[0].MessageType)
}

func TestKMSSigner_VerifyRejects(t *testing.T) {
	ctx := context.Background()
	api, s := newFakeKMS(t)
	digest := testDigest("checkpoint")
	sig, keyID, _, err := s.Sign(ctx, digest)
	require.NoError(t, err)

	tampered := append([]byte(nil), sig...)
	tampered[len(tampered)-1] ^= 0x01
	assert.ErrorIs(t, s.Verify(ctx, digest, tampered, keyID), ErrSignatureInvalid, "KMSInvalidSignatureException maps to ErrSignatureInvalid")
	assert.ErrorIs(t, s.Verify(ctx, testDigest("other"), sig, keyID), ErrSignatureInvalid)
	assert.Error(t, s.Verify(ctx, digest, sig, "arn:aws:kms:us-east-1:111122223333:key/other"), "unknown key is an error, never success")
	assert.Error(t, s.Verify(ctx, digest, sig, ""))
	assert.ErrorIs(t, s.Verify(ctx, []byte("short"), sig, keyID), ErrDigestSize)

	api.VerifyErr = errors.New("throttled")
	err = s.Verify(ctx, digest, sig, keyID)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrSignatureInvalid, "an API failure is not reported as an invalid signature")
	api.VerifyErr = nil

	// A fake reporting SignatureValid=false without an error is still a failure.
	falseAPI := &stubKMS{verify: &kms.VerifyOutput{SignatureValid: false}}
	fs, err := NewKMSSigner(falseAPI, fakeKeyARN)
	require.NoError(t, err)
	assert.ErrorIs(t, fs.Verify(ctx, digest, sig, keyID), ErrSignatureInvalid)
}

func TestKMSSigner_SignErrors(t *testing.T) {
	ctx := context.Background()
	api, s := newFakeKMS(t)
	_, _, _, err := s.Sign(ctx, []byte("not a digest"))
	assert.ErrorIs(t, err, ErrDigestSize)
	assert.Empty(t, api.SignCalls(), "nothing reaches KMS for a malformed digest")

	api.SignErr = errors.New("access denied")
	_, _, _, err = s.Sign(ctx, testDigest("d"))
	assert.ErrorContains(t, err, "access denied")

	empty := &stubKMS{sign: &kms.SignOutput{SigningAlgorithm: types.SigningAlgorithmSpecEcdsaSha256}}
	es, err := NewKMSSigner(empty, fakeKeyARN)
	require.NoError(t, err)
	_, _, _, err = es.Sign(ctx, testDigest("d"))
	assert.ErrorContains(t, err, "no signature")

	wrongAlg := &stubKMS{sign: &kms.SignOutput{Signature: []byte{1}, SigningAlgorithm: types.SigningAlgorithmSpecRsassaPssSha256, KeyId: aws.String(fakeKeyARN)}}
	ws, err := NewKMSSigner(wrongAlg, fakeKeyARN)
	require.NoError(t, err)
	_, _, _, err = ws.Sign(ctx, testDigest("d"))
	assert.ErrorContains(t, err, "ECDSA_SHA_256")

	noKey := &stubKMS{sign: &kms.SignOutput{Signature: []byte{1}, SigningAlgorithm: types.SigningAlgorithmSpecEcdsaSha256}}
	ns, err := NewKMSSigner(noKey, fakeKeyARN)
	require.NoError(t, err)
	_, keyID, _, err := ns.Sign(ctx, testDigest("d"))
	require.NoError(t, err)
	assert.Equal(t, fakeKeyARN, keyID, "falls back to the configured key id when KMS omits it")
}

func TestKMSSigner_Constructor(t *testing.T) {
	api, err := prooftest.NewFakeKMS(fakeKeyARN)
	require.NoError(t, err)
	_, err = NewKMSSigner(nil, fakeKeyARN)
	assert.Error(t, err)
	_, err = NewKMSSigner(api, "")
	assert.Error(t, err)
}

// stubKMS returns canned outputs.
type stubKMS struct {
	sign   *kms.SignOutput
	verify *kms.VerifyOutput
}

func (s *stubKMS) Sign(context.Context, *kms.SignInput, ...func(*kms.Options)) (*kms.SignOutput, error) {
	return s.sign, nil
}

func (s *stubKMS) Verify(context.Context, *kms.VerifyInput, ...func(*kms.Options)) (*kms.VerifyOutput, error) {
	return s.verify, nil
}
