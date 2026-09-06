package proof

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
)

// KMSAPI is the subset of the KMS client the signer needs. *kms.Client
// satisfies it; tests supply a fake.
type KMSAPI interface {
	Sign(ctx context.Context, in *kms.SignInput, optFns ...func(*kms.Options)) (*kms.SignOutput, error)
	Verify(ctx context.Context, in *kms.VerifyInput, optFns ...func(*kms.Options)) (*kms.VerifyOutput, error)
}

var _ KMSAPI = (*kms.Client)(nil)

// KMSSigner signs checkpoint digests with an asymmetric KMS key
// (ECDSA_SHA_256, MessageType DIGEST) and verifies through the KMS Verify
// API, so the private key never leaves KMS and verification uses the key
// the row names.
type KMSSigner struct {
	api   KMSAPI
	keyID string
}

var _ Signer = (*KMSSigner)(nil)

// NewKMSSigner returns a signer for keyID (key id, ARN, alias name or alias
// ARN). The key identifier persisted with each checkpoint is the one KMS
// reports back (the key ARN), not the alias, so verification stays bound to
// the exact key after an alias is re-pointed.
func NewKMSSigner(api KMSAPI, keyID string) (*KMSSigner, error) {
	if api == nil {
		return nil, errors.New("proof: KMS signer requires a client")
	}
	if keyID == "" {
		return nil, errors.New("proof: KMS signer requires a key id")
	}
	return &KMSSigner{api: api, keyID: keyID}, nil
}

// Kind implements Signer.
func (s *KMSSigner) Kind() SignerKind { return SignerKMS }

// Sign implements Signer.
func (s *KMSSigner) Sign(ctx context.Context, digest []byte) ([]byte, string, string, error) {
	if len(digest) != HashSize {
		return nil, "", "", ErrDigestSize
	}
	out, err := s.api.Sign(ctx, &kms.SignInput{
		KeyId:            aws.String(s.keyID),
		Message:          digest,
		MessageType:      types.MessageTypeDigest,
		SigningAlgorithm: types.SigningAlgorithmSpecEcdsaSha256,
	})
	if err != nil {
		return nil, "", "", fmt.Errorf("proof: kms sign: %w", err)
	}
	if out == nil || len(out.Signature) == 0 {
		return nil, "", "", errors.New("proof: kms sign returned no signature")
	}
	if out.SigningAlgorithm != types.SigningAlgorithmSpecEcdsaSha256 {
		return nil, "", "", fmt.Errorf("proof: kms signed with %q, want %s", out.SigningAlgorithm, AlgorithmECDSASHA256)
	}
	keyID := aws.ToString(out.KeyId)
	if keyID == "" {
		keyID = s.keyID
	}
	return out.Signature, keyID, AlgorithmECDSASHA256, nil
}

// Verify implements Signer. A KMSInvalidSignatureException or a false
// SignatureValid is ErrSignatureInvalid; any other KMS error is returned as
// a verification error too, never as success.
func (s *KMSSigner) Verify(ctx context.Context, digest, sig []byte, keyID string) error {
	if len(digest) != HashSize {
		return ErrDigestSize
	}
	if keyID == "" {
		return errors.New("proof: kms verify requires the row's key id")
	}
	out, err := s.api.Verify(ctx, &kms.VerifyInput{
		KeyId:            aws.String(keyID),
		Message:          digest,
		MessageType:      types.MessageTypeDigest,
		Signature:        sig,
		SigningAlgorithm: types.SigningAlgorithmSpecEcdsaSha256,
	})
	if err != nil {
		var invalid *types.KMSInvalidSignatureException
		if errors.As(err, &invalid) {
			return fmt.Errorf("%w: key %s", ErrSignatureInvalid, keyID)
		}
		return fmt.Errorf("proof: kms verify: %w", err)
	}
	if out == nil || !out.SignatureValid {
		return fmt.Errorf("%w: key %s", ErrSignatureInvalid, keyID)
	}
	return nil
}
