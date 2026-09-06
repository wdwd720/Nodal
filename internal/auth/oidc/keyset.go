package oidc

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
)

const minRSABits = 2048

// signatureOutcomeKey carries a *signatureOutcome through the context that
// gooidc.IDTokenVerifier.Verify passes on to KeySet.VerifySignature.
type signatureOutcomeKey struct{}

// signatureOutcome records the key set's typed verdict. go-oidc flattens
// KeySet errors into a string ("failed to verify signature: ..."), so the
// verifier reads the verdict from here to report ErrUnknownKey versus
// ErrBadSignature.
type signatureOutcome struct{ err error }

// keySet implements gooidc.KeySet: it fetches the issuer's JWKS through the
// bounded client, caches it, refreshes at most once per minRefresh when a
// token names an unknown kid (OIDC Core 10.1.1), and verifies signatures with
// go-jose. Keys that are not public asymmetric signing keys for an allowed
// algorithm, or RSA keys under 2048 bits, are ignored rather than failing the
// whole set (RFC 7517 section 5).
type keySet struct {
	url        string
	client     *http.Client
	now        func() time.Time
	minRefresh time.Duration
	algs       []jose.SignatureAlgorithm
	allowed    map[string]bool

	mu        sync.Mutex
	keys      []jose.JSONWebKey
	fetchedAt time.Time
}

var _ gooidc.KeySet = (*keySet)(nil)

func newKeySet(url string, client *http.Client, now func() time.Time, minRefresh time.Duration, algs []string) *keySet {
	k := &keySet{url: url, client: client, now: now, minRefresh: minRefresh, allowed: map[string]bool{}}
	for _, a := range algs {
		k.algs = append(k.algs, jose.SignatureAlgorithm(a))
		k.allowed[a] = true
	}
	return k
}

// VerifySignature implements gooidc.KeySet. The verifier has already
// enforced the algorithm allow-list; parsing again with the same list keeps
// this method safe if it is ever called on its own.
func (k *keySet) VerifySignature(ctx context.Context, jwt string) ([]byte, error) {
	payload, err := k.verify(ctx, jwt)
	if outcome, ok := ctx.Value(signatureOutcomeKey{}).(*signatureOutcome); ok {
		outcome.err = err
	}
	return payload, err
}

func (k *keySet) verify(ctx context.Context, jwt string) ([]byte, error) {
	jws, err := jose.ParseSigned(jwt, k.algs)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidIDToken, err)
	}
	if len(jws.Signatures) != 1 {
		return nil, fmt.Errorf("%w: expected exactly one signature, got %d", ErrInvalidIDToken, len(jws.Signatures))
	}
	candidates, err := k.candidates(ctx, jws.Signatures[0].Header.KeyID)
	if err != nil {
		return nil, err
	}
	for i := range candidates {
		if payload, err := jws.Verify(candidates[i].Key); err == nil {
			return payload, nil
		}
	}
	return nil, ErrBadSignature
}

// candidates returns the keys a token with the given kid may be verified
// against. With a kid: that key (refreshing once if unknown). Without: every
// key, so a provider that omits kid still works.
func (k *keySet) candidates(ctx context.Context, kid string) ([]jose.JSONWebKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.fetchedAt.IsZero() {
		if err := k.refreshLocked(ctx); err != nil {
			return nil, err
		}
	}
	if keys := k.lookupLocked(kid); len(keys) > 0 {
		return keys, nil
	}
	if k.now().Sub(k.fetchedAt) < k.minRefresh {
		return nil, fmt.Errorf("%w: kid %q", ErrUnknownKey, kid)
	}
	if err := k.refreshLocked(ctx); err != nil {
		return nil, err
	}
	if keys := k.lookupLocked(kid); len(keys) > 0 {
		return keys, nil
	}
	return nil, fmt.Errorf("%w: kid %q", ErrUnknownKey, kid)
}

func (k *keySet) lookupLocked(kid string) []jose.JSONWebKey {
	if kid == "" {
		return k.keys
	}
	var out []jose.JSONWebKey
	for _, key := range k.keys {
		if key.KeyID == kid {
			out = append(out, key)
		}
	}
	return out
}

func (k *keySet) refreshLocked(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return fmt.Errorf("oidc: fetch jwks: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := k.client.Do(req)
	if err != nil {
		return fmt.Errorf("oidc: fetch jwks: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body) // bounded by the client's transport
	if err != nil {
		return fmt.Errorf("oidc: fetch jwks: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oidc: fetch jwks: unexpected status %d", resp.StatusCode)
	}
	keys, err := parseJWKS(body, k.allowed)
	if err != nil {
		return fmt.Errorf("oidc: fetch jwks: %w", err)
	}
	if len(keys) == 0 {
		return errors.New("oidc: jwks contains no usable signing keys")
	}
	k.keys = keys
	k.fetchedAt = k.now()
	return nil
}

// parseJWKS decodes a JWK Set, keeping only public asymmetric keys usable
// for signatures under an allowed algorithm. Keys go-jose cannot represent
// (unknown kty, unsupported curve, malformed point) are skipped.
func parseJWKS(body []byte, allowed map[string]bool) ([]jose.JSONWebKey, error) {
	var raw struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	var keys []jose.JSONWebKey
	for _, r := range raw.Keys {
		var key jose.JSONWebKey
		if err := key.UnmarshalJSON(r); err != nil {
			continue
		}
		if usableSigningKey(&key, allowed) {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

func usableSigningKey(key *jose.JSONWebKey, allowed map[string]bool) bool {
	if !key.Valid() || !key.IsPublic() {
		return false
	}
	if key.Use != "" && key.Use != "sig" {
		return false
	}
	if key.Algorithm != "" && !allowed[key.Algorithm] {
		return false
	}
	if pub, ok := key.Key.(*rsa.PublicKey); ok && pub.N.BitLen() < minRSABits {
		return false
	}
	return true
}
