// Package prooftest provides in-memory doubles for internal/proof: a
// write-once MemArchive and a FakeKMS that signs with a real P-256 key
// through the KMS request/response shapes. It is test-only and is never
// wired into production (the generic "<pkg>/<pkg>test" rule is enforced by
// scripts/lintfin). It deliberately does not import internal/proof, so the
// proof package's own tests can use it.
package prooftest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
)

// MemArchive is an in-memory write-once object store addressed by mem://
// URIs. It records retention per object and lets tests tamper with stored
// bytes to simulate a corrupted or altered archived object.
type MemArchive struct {
	mu        sync.Mutex
	objects   map[string][]byte
	retention map[string]*time.Duration
	puts      int
	// FailPut, when set, makes every Put fail with it.
	FailPut error
	// FailGet, when set, makes every Get fail with it.
	FailGet error
}

// NewMemArchive returns an empty archive.
func NewMemArchive() *MemArchive {
	return &MemArchive{objects: map[string][]byte{}, retention: map[string]*time.Duration{}}
}

const memScheme = "mem://audit/"

// ErrObjectExists and ErrObjectExistsIdentical mirror the sentinels of the
// same names in internal/proof.
//
// They are separate values rather than the real ones because this package
// cannot import internal/proof: package proof's own tests import this one, and
// that would be a cycle. Two sets of sentinels is a thing that can drift, so
// internal/proof's TestMemArchiveRefusesTheSameWayTheRealArchivesDo asserts
// this fake refuses in the same shape the real implementations do.
var (
	ErrObjectExists          = errors.New("prooftest: archive object already exists")
	ErrObjectExistsIdentical = fmt.Errorf("%w (identical bytes: a replay of a Put that already succeeded)", ErrObjectExists)
)

// Put stores body under key and refuses to overwrite.
func (a *MemArchive) Put(_ context.Context, key string, body []byte, retention *time.Duration) (string, []byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.FailPut != nil {
		return "", nil, a.FailPut
	}
	if key == "" {
		return "", nil, errors.New("prooftest: empty key")
	}
	uri := memScheme + key
	if stored, exists := a.objects[uri]; exists {
		// Refuse, but say which refusal it is, the way the real archives do.
		// A fake that collapses the two teaches a caller that identical bytes
		// and a rewritten object are the same event, and they are not.
		have := sha256.Sum256(stored)
		if have == sha256.Sum256(body) {
			return uri, have[:], fmt.Errorf("%w: %s", ErrObjectExistsIdentical, key)
		}
		return uri, have[:], fmt.Errorf("%w: %s (a different object is already stored under this key)", ErrObjectExists, key)
	}
	cp := append([]byte(nil), body...)
	a.objects[uri] = cp
	if retention != nil {
		d := *retention
		a.retention[uri] = &d
	} else {
		a.retention[uri] = nil
	}
	a.puts++
	sum := sha256.Sum256(cp)
	return uri, sum[:], nil
}

// Get returns a copy of the stored bytes.
func (a *MemArchive) Get(_ context.Context, uri string) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.FailGet != nil {
		return nil, a.FailGet
	}
	b, ok := a.objects[uri]
	if !ok {
		return nil, fmt.Errorf("prooftest: object %s not found", uri)
	}
	return append([]byte(nil), b...), nil
}

// Tamper replaces the stored bytes of uri with fn(current). It reports false
// when the object does not exist.
func (a *MemArchive) Tamper(uri string, fn func([]byte) []byte) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, ok := a.objects[uri]
	if !ok {
		return false
	}
	a.objects[uri] = fn(append([]byte(nil), b...))
	return true
}

// Delete removes an object (simulates a lost object).
func (a *MemArchive) Delete(uri string) { a.mu.Lock(); defer a.mu.Unlock(); delete(a.objects, uri) }

// Retention returns the retention recorded for uri and whether it exists.
func (a *MemArchive) Retention(uri string) (*time.Duration, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.retention[uri]
	return r, ok
}

// URIs lists every stored URI in sorted order.
func (a *MemArchive) URIs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.objects))
	for u := range a.objects {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// Puts returns how many successful Put calls happened.
func (a *MemArchive) Puts() int { a.mu.Lock(); defer a.mu.Unlock(); return a.puts }

// FakeKMS implements the Sign/Verify subset of the KMS API with a real
// P-256 key. It enforces the request shape proof.KMSSigner must send:
// MessageType DIGEST, ECDSA_SHA_256, a 32-byte message and the configured
// key id or alias. Verify returns KMSInvalidSignatureException for a bad
// signature, like the real service.
type FakeKMS struct {
	mu   sync.Mutex
	key  *ecdsa.PrivateKey
	arn  string
	aka  map[string]bool
	sign []*kms.SignInput
	ver  []*kms.VerifyInput
	// SignErr / VerifyErr, when set, are returned by the respective call.
	SignErr   error
	VerifyErr error
}

// NewFakeKMS creates a fake with a fresh key reachable as arn and any of
// aliases.
func NewFakeKMS(arn string, aliases ...string) (*FakeKMS, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	f := &FakeKMS{key: key, arn: arn, aka: map[string]bool{arn: true}}
	for _, a := range aliases {
		f.aka[a] = true
	}
	return f, nil
}

// PublicKey exposes the verifying key for out-of-band checks.
func (f *FakeKMS) PublicKey() *ecdsa.PublicKey { return &f.key.PublicKey }

// ARN returns the key ARN the fake reports in responses.
func (f *FakeKMS) ARN() string { return f.arn }

// SignCalls returns copies of every SignInput received.
func (f *FakeKMS) SignCalls() []*kms.SignInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*kms.SignInput(nil), f.sign...)
}

// VerifyCalls returns copies of every VerifyInput received.
func (f *FakeKMS) VerifyCalls() []*kms.VerifyInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*kms.VerifyInput(nil), f.ver...)
}

func (f *FakeKMS) check(keyID *string, msg []byte, mt types.MessageType, alg types.SigningAlgorithmSpec) error {
	if keyID == nil || !f.aka[*keyID] {
		return &types.NotFoundException{Message: aws.String("key not found: " + aws.ToString(keyID))}
	}
	if mt != types.MessageTypeDigest {
		return &types.InvalidKeyUsageException{Message: aws.String("fake kms only accepts MessageType DIGEST")}
	}
	if alg != types.SigningAlgorithmSpecEcdsaSha256 {
		return &types.InvalidKeyUsageException{Message: aws.String("algorithm not supported by this key: " + string(alg))}
	}
	if len(msg) != sha256.Size {
		return &types.InvalidKeyUsageException{Message: aws.String("digest must be 32 bytes")}
	}
	return nil
}

// Sign implements the KMS Sign operation.
func (f *FakeKMS) Sign(_ context.Context, in *kms.SignInput, _ ...func(*kms.Options)) (*kms.SignOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sign = append(f.sign, in)
	if f.SignErr != nil {
		return nil, f.SignErr
	}
	if err := f.check(in.KeyId, in.Message, in.MessageType, in.SigningAlgorithm); err != nil {
		return nil, err
	}
	sig, err := ecdsa.SignASN1(rand.Reader, f.key, in.Message)
	if err != nil {
		return nil, err
	}
	return &kms.SignOutput{KeyId: aws.String(f.arn), Signature: sig, SigningAlgorithm: types.SigningAlgorithmSpecEcdsaSha256}, nil
}

// Verify implements the KMS Verify operation.
func (f *FakeKMS) Verify(_ context.Context, in *kms.VerifyInput, _ ...func(*kms.Options)) (*kms.VerifyOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ver = append(f.ver, in)
	if f.VerifyErr != nil {
		return nil, f.VerifyErr
	}
	if err := f.check(in.KeyId, in.Message, in.MessageType, in.SigningAlgorithm); err != nil {
		return nil, err
	}
	if !ecdsa.VerifyASN1(&f.key.PublicKey, in.Message, in.Signature) {
		return nil, &types.KMSInvalidSignatureException{Message: aws.String("signature is invalid")}
	}
	return &kms.VerifyOutput{KeyId: aws.String(f.arn), SignatureValid: true, SigningAlgorithm: types.SigningAlgorithmSpecEcdsaSha256}, nil
}

// IsMemURI reports whether uri was minted by a MemArchive.
func IsMemURI(uri string) bool { return strings.HasPrefix(uri, memScheme) }
