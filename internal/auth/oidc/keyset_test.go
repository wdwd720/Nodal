package oidc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

var t0 = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

func marshalJWK(t testing.TB, kid, use, alg string, key any) json.RawMessage {
	t.Helper()
	b, err := (&jose.JSONWebKey{Key: key, KeyID: kid, Use: use, Algorithm: alg}).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type testJWKS struct {
	rsa  *rsa.PrivateKey
	ec   *ecdsa.PrivateKey
	body []byte
}

// newTestJWKS builds a set with two usable keys (rsa1, ec1) surrounded by
// keys that must be ignored.
func newTestJWKS(t testing.TB) testJWKS {
	t.Helper()
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keys := []json.RawMessage{
		marshalJWK(t, "ec1", "", "", &ec.PublicKey),
		marshalJWK(t, "rsa1", "sig", "RS256", &rk.PublicKey),
		marshalJWK(t, "weak", "sig", "", &weak.PublicKey),
		marshalJWK(t, "enc", "enc", "", &rk.PublicKey),
		marshalJWK(t, "foreign-alg", "sig", "ES256K", &rk.PublicKey),
		marshalJWK(t, "private", "sig", "", rk),
		marshalJWK(t, "hmac", "sig", "", []byte("secret")),
		json.RawMessage(`{"kty":"EC","kid":"badpoint","crv":"P-256","x":"AQ","y":"Ag"}`),
		json.RawMessage(`{"kty":"XYZ","kid":"unknown-kty"}`),
		json.RawMessage(`{"kty":"OKP","kid":"ed448","crv":"Ed448","x":"AA"}`),
		json.RawMessage(`"not an object"`),
	}
	body, err := json.Marshal(map[string]any{"keys": keys})
	if err != nil {
		t.Fatal(err)
	}
	return testJWKS{rsa: rk, ec: ec, body: body}
}

func TestParseJWKS_Filtering(t *testing.T) {
	set := newTestJWKS(t)
	keys, err := parseJWKS(set.body, map[string]bool{"RS256": true, "ES256": true})
	if err != nil {
		t.Fatal(err)
	}
	var kids []string
	for _, k := range keys {
		kids = append(kids, k.KeyID)
	}
	if strings.Join(kids, ",") != "ec1,rsa1" {
		t.Fatalf("usable keys %v, want [ec1 rsa1]", kids)
	}
	if _, err := parseJWKS([]byte("nope"), nil); err == nil {
		t.Fatal("non-JSON key set accepted")
	}
	if keys, err := parseJWKS([]byte(`{"keys":[]}`), nil); err != nil || len(keys) != 0 {
		t.Fatalf("empty set: %v %v", keys, err)
	}
}

func serveJWKS(t testing.TB, body []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestKeySet_Candidates(t *testing.T) {
	set := newTestJWKS(t)
	srv, hits := serveJWKS(t, set.body)
	now := t0
	ks := newKeySet(srv.URL, srv.Client(), func() time.Time { return now }, time.Minute, []string{"RS256", "ES256"})
	ctx := context.Background()

	all, err := ks.candidates(ctx, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("no kid: %d keys, %v", len(all), err)
	}
	one, err := ks.candidates(ctx, "rsa1")
	if err != nil || len(one) != 1 || one[0].KeyID != "rsa1" {
		t.Fatalf("kid lookup: %v %v", one, err)
	}
	if hits.Load() != 1 {
		t.Fatalf("fetched %d times before any miss", hits.Load())
	}
	if _, err := ks.candidates(ctx, "nope"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown kid: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatal("refetched inside the refresh window")
	}
	now = now.Add(time.Minute)
	if _, err := ks.candidates(ctx, "nope"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown kid after refresh: %v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("expected one refetch after the window, got %d fetches", hits.Load())
	}
	// A set with no usable keys is refused rather than cached as empty.
	empty, _ := serveJWKS(t, []byte(`{"keys":[{"kty":"oct","k":"AA"}]}`))
	ks = newKeySet(empty.URL, empty.Client(), func() time.Time { return now }, time.Minute, []string{"RS256"})
	if _, err := ks.candidates(ctx, ""); err == nil || !strings.Contains(err.Error(), "no usable") {
		t.Fatalf("empty set: %v", err)
	}
	// A fetch failure is reported as such, not as an invalid token.
	down, _ := serveJWKS(t, nil)
	down.Close()
	ks = newKeySet(down.URL, down.Client(), func() time.Time { return now }, time.Minute, []string{"RS256"})
	if _, err := ks.candidates(ctx, ""); err == nil || errors.Is(err, ErrInvalidIDToken) {
		t.Fatalf("fetch failure: %v", err)
	}
}

func TestKeySet_VerifySignature(t *testing.T) {
	set := newTestJWKS(t)
	srv, _ := serveJWKS(t, set.body)
	ks := newKeySet(srv.URL, srv.Client(), func() time.Time { return t0 }, time.Minute, []string{"RS256", "ES256"})
	sign := func(alg jose.SignatureAlgorithm, key any, kid string) string {
		t.Helper()
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: &jose.JSONWebKey{Key: key, KeyID: kid}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		jws, err := signer.Sign([]byte(`{"sub":"user-1"}`))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := jws.CompactSerialize()
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	verify := func(raw string) (string, error) {
		outcome := &signatureOutcome{}
		payload, err := ks.VerifySignature(context.WithValue(context.Background(), signatureOutcomeKey{}, outcome), raw)
		if !errors.Is(outcome.err, err) && err != nil {
			t.Fatalf("outcome %v not recorded for %v", outcome.err, err)
		}
		return string(payload), err
	}
	if payload, err := verify(sign(jose.RS256, set.rsa, "rsa1")); err != nil || payload != `{"sub":"user-1"}` {
		t.Fatalf("rsa1: %q %v", payload, err)
	}
	if payload, err := verify(sign(jose.ES256, set.ec, "ec1")); err != nil || payload != `{"sub":"user-1"}` {
		t.Fatalf("ec1: %q %v", payload, err)
	}
	if _, err := verify(sign(jose.RS256, set.rsa, "")); err != nil {
		t.Fatalf("no kid must try every key: %v", err)
	}
	if _, err := verify(sign(jose.RS256, set.rsa, "ghost")); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown kid: %v", err)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verify(sign(jose.RS256, other, "rsa1")); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("foreign key under a known kid: %v", err)
	}
	if _, err := verify(sign(jose.ES256, set.ec, "rsa1")); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("alg/key family mismatch: %v", err)
	}
	if _, err := verify(sign(jose.PS256, set.rsa, "rsa1")); !errors.Is(err, ErrInvalidIDToken) || errors.Is(err, ErrBadSignature) {
		t.Fatalf("algorithm outside the allow-list must fail at parse time: %v", err)
	}
	if _, err := verify("not.a.jwt"); !errors.Is(err, ErrInvalidIDToken) {
		t.Fatalf("garbage: %v", err)
	}
}
