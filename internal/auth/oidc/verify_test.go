package oidc_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/nodal/controlplane/internal/auth/authtest"
	"github.com/nodal/controlplane/internal/auth/oidc"
)

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// signRaw signs arbitrary claims with the fake provider's current key so
// tests can present payload shapes Grant cannot express (absent claims).
func signRaw(t testing.TB, srv *authtest.OIDCServer, claims map[string]any) string {
	t.Helper()
	key := &jose.JSONWebKey{Key: srv.Key(), KeyID: srv.KeyID()}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// exchangeRaw runs raw through the token endpoint and Exchange.
func exchangeRaw(t *testing.T, e env, verifier, challenge, raw string) error {
	t.Helper()
	g := goodGrant("n1", challenge)
	g.RawIDToken = raw
	e.srv.AddGrant("raw", g)
	_, err := e.p.Exchange(context.Background(), "raw", verifier, "n1")
	return err
}

func TestExchange_MalformedIDToken(t *testing.T) {
	e := setup(t, nil)
	verifier, challenge := oidc.GeneratePKCE()
	valid := e.srv.SignIDToken(goodGrant("n1", challenge))
	parts := strings.Split(valid, ".")
	if err := exchangeRaw(t, e, verifier, challenge, valid); err != nil {
		t.Fatalf("untouched token rejected: %v", err)
	}
	cases := map[string]string{
		"one part":           "abc",
		"two parts":          "abc.def",
		"four parts":         valid + ".x",
		"bad header b64":     "!!." + parts[1] + "." + parts[2],
		"header not json":    b64("nope") + "." + parts[1] + "." + parts[2],
		"empty signature":    parts[0] + "." + parts[1] + ".",
		"bad payload b64":    parts[0] + ".!!." + parts[2],
		"payload not json":   parts[0] + "." + b64("nope") + "." + parts[2],
		"json serialization": `{"payload":"` + parts[1] + `","signatures":[]}`,
		"signature swapped":  parts[0] + "." + parts[1] + "." + strings.Split(e.srv.SignIDToken(goodGrant("n2", challenge)), ".")[2],
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			err := exchangeRaw(t, e, verifier, challenge, raw)
			mustIs(t, err, oidc.ErrInvalidIDToken)
			if errors.Is(err, oidc.ErrNonceMismatch) || errors.Is(err, oidc.ErrMissingSubject) {
				t.Fatalf("claims were inspected on a token that never verified: %v", err)
			}
		})
	}
}

func TestExchange_ClaimShapes(t *testing.T) {
	e := setup(t, nil)
	verifier, challenge := oidc.GeneratePKCE()

	// Providers that emit booleans and NumericDates as strings.
	g := goodGrant("n1", challenge)
	g.Extra = map[string]any{
		"email_verified": "true",
		"auth_time":      "1780000000",
		"exp":            strconv.FormatInt(t0.Add(5*time.Minute).Unix(), 10),
	}
	e.srv.AddGrant("strings", g)
	id, err := e.p.Exchange(context.Background(), "strings", verifier, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if !id.EmailVerified || !id.AuthTime.Equal(time.Unix(1780000000, 0).UTC()) {
		t.Fatalf("flexible claims not parsed: %+v", id)
	}

	bad := map[string]map[string]any{
		"exp null":               {"exp": nil},
		"exp not a number":       {"exp": "not-a-number"},
		"email_verified garbage": {"email_verified": "maybe"},
		"auth_time garbage":      {"auth_time": "soon"},
	}
	for name, extra := range bad {
		t.Run(name, func(t *testing.T) {
			g := goodGrant("n1", challenge)
			g.Extra = extra
			e.srv.AddGrant("bad", g)
			_, err := e.p.Exchange(context.Background(), "bad", verifier, "n1")
			mustIs(t, err, oidc.ErrInvalidIDToken)
			if errors.Is(err, oidc.ErrTokenExpired) {
				t.Fatalf("unparseable exp reported as expiry: %v", err)
			}
		})
	}

	// An absent exp is invalid, not "expired at the zero time".
	err = exchangeRaw(t, e, verifier, challenge, signRaw(t, e.srv, map[string]any{
		"iss": e.srv.Issuer, "sub": "user-1", "aud": "cp-web", "iat": t0.Unix(), "nonce": "n1",
	}))
	mustIs(t, err, oidc.ErrInvalidIDToken)
	if errors.Is(err, oidc.ErrTokenExpired) || !strings.Contains(err.Error(), "missing exp") {
		t.Fatalf("absent exp: %v", err)
	}

	// The same signed payload with exp present is accepted (control).
	err = exchangeRaw(t, e, verifier, challenge, signRaw(t, e.srv, map[string]any{
		"iss": e.srv.Issuer, "sub": "user-1", "aud": "cp-web", "iat": t0.Unix(), "exp": t0.Add(time.Minute).Unix(), "nonce": "n1",
	}))
	if err != nil {
		t.Fatalf("control token rejected: %v", err)
	}
}

func TestExchange_AlgAllowList(t *testing.T) {
	// The fake provider advertises RS256 only, so every other algorithm is
	// refused at parse time regardless of what the signature would verify
	// as; the token endpoint signs RS-style whatever the header claims.
	e := setup(t, nil)
	verifier, challenge := oidc.GeneratePKCE()
	for _, alg := range []string{"ES256", "PS256", "HS256", "EdDSA", "RS512"} {
		t.Run(alg, func(t *testing.T) {
			g := goodGrant("n1", challenge)
			g.Alg = alg
			e.srv.AddGrant("alg", g)
			_, err := e.p.Exchange(context.Background(), "alg", verifier, "n1")
			mustIs(t, err, oidc.ErrUnsupportedAlg)
		})
	}
}

func TestExchange_NoKidTriesEveryKey(t *testing.T) {
	e := setup(t, nil)
	verifier, challenge := oidc.GeneratePKCE()
	key := &jose.JSONWebKey{Key: e.srv.Key()} // no kid header
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{
		"iss": e.srv.Issuer, "sub": "user-1", "aud": "cp-web", "iat": t0.Unix(), "exp": t0.Add(time.Minute).Unix(), "nonce": "n1",
	})
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := jws.CompactSerialize()
	if err := exchangeRaw(t, e, verifier, challenge, raw); err != nil {
		t.Fatalf("token without kid rejected: %v", err)
	}
}

func discoveryServer(t *testing.T, padding int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issuer := "http://" + r.Host
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"issuer":%q,"authorization_endpoint":%q,"token_endpoint":%q,"jwks_uri":%q,"padding":%q}`,
			issuer, issuer+"/authorize", issuer+"/token", issuer+"/jwks", strings.Repeat("x", padding))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestNew_DiscoveryDocumentBounded(t *testing.T) {
	small := discoveryServer(t, 1024)
	cfg := oidc.Config{Issuer: small.URL, ClientID: "cp-web", RedirectURL: "http://127.0.0.1/cb", HTTPClient: small.Client()}
	if _, err := oidc.New(context.Background(), cfg); err != nil {
		t.Fatalf("small discovery document rejected: %v", err)
	}
	large := discoveryServer(t, oidc.MaxDocumentBytes)
	cfg.Issuer, cfg.HTTPClient = large.URL, large.Client()
	_, err := oidc.New(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized discovery document accepted: %v", err)
	}
}

func FuzzVerifyNeverPanics(f *testing.F) {
	srv := authtest.NewOIDCServer(f, "cp-web", "cp-secret")
	clk := authtest.NewClock(t0)
	srv.Now = clk.Now
	p, err := oidc.New(context.Background(), oidc.Config{
		Issuer: srv.Issuer, ClientID: srv.ClientID, ClientSecret: srv.ClientSecret,
		RedirectURL: "http://127.0.0.1:3000/auth/callback", HTTPClient: srv.Client(), Now: clk.Now,
		JWKSMinRefresh: time.Nanosecond,
	})
	if err != nil {
		f.Fatal(err)
	}
	verifier, challenge := oidc.GeneratePKCE()
	valid := srv.SignIDToken(goodGrant("n1", challenge))
	parts := strings.Split(valid, ".")
	f.Add(valid)
	f.Add(parts[0] + "." + parts[1] + ".AAAA")
	f.Add(parts[0] + ".e30." + parts[2])
	f.Add("eyJhbGciOiJub25lIn0.e30.")
	f.Add("...")
	f.Add("{}")
	f.Add(`{"payload":"e30","signatures":[{"protected":"eyJhbGciOiJSUzI1NiJ9","signature":"AA"}]}`)
	var seq atomic.Int64
	f.Fuzz(func(t *testing.T, raw string) {
		// An empty override makes the fake sign the grant normally, which is
		// the only way a token for user-1 can be accepted.
		code := "fuzz-" + strconv.FormatInt(seq.Add(1), 10)
		g := goodGrant("n1", challenge)
		g.RawIDToken = raw
		srv.AddGrant(code, g)
		id, err := p.Exchange(context.Background(), code, verifier, "n1")
		if err == nil && id.Subject != "user-1" {
			t.Fatalf("accepted a token for %q", id.Subject)
		}
	})
}
