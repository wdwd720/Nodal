package authtest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Grant describes the ID token the fake token endpoint will issue for an
// authorization code. Zero fields take sensible defaults at signing time;
// the override fields exist for negative tests.
type Grant struct {
	Subject       string
	Email         string
	EmailVerified bool
	Nonce         string
	AMR           []string  // nil omits the claim
	ACR           string    // empty omits the claim
	AuthTime      time.Time // zero omits the claim

	// CodeChallenge, when set, makes the token endpoint verify the PKCE
	// code_verifier (S256) and fail with invalid_grant otherwise.
	CodeChallenge string

	// Overrides.
	Audience []string        // default: [ClientID]
	Issuer   string          // default: the server's issuer
	IssuedAt time.Time       // default: now
	Expiry   time.Time       // default: now + 5m
	Alg      string          // default: RS256; "none" emits an unsigned token
	KeyID    string          // default: the server's current kid
	SignWith *rsa.PrivateKey // default: the server's key; another key yields a bad signature
	Extra    map[string]any  // merged into the payload last

	// RawIDToken, when set, is returned verbatim as the id_token instead of
	// a token built from the fields above (malformed-token and fuzz tests).
	RawIDToken string
	// OmitIDToken drops id_token from the token response entirely.
	OmitIDToken bool
}

// OIDCServer is an httptest-backed OpenID Connect provider for tests.
type OIDCServer struct {
	*httptest.Server
	Issuer       string
	ClientID     string
	ClientSecret string
	// Now is the clock used for default iat/exp; tests may replace it.
	Now func() time.Time
	// DiscoveryIssuer, when set, is advertised in the discovery document
	// instead of Issuer (to test issuer-mismatch handling).
	DiscoveryIssuer string

	// DiscoveryOverrides is merged over the discovery document. A nil value
	// removes the key entirely.
	DiscoveryOverrides map[string]any

	mu        sync.Mutex
	key       *rsa.PrivateKey
	kid       string
	grants    map[string]Grant
	autoCodes int
	tokenHits int
	jwksHits  int
	// lastAuthorize records the query of the last /authorize request.
	lastAuthorize url.Values
}

// NewOIDCServer starts a fake provider with a fresh 2048-bit RSA key. It is
// closed when the test ends.
func NewOIDCServer(t testing.TB, clientID, clientSecret string) *OIDCServer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	s := &OIDCServer{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Now:          func() time.Time { return time.Now().UTC() },
		key:          key,
		kid:          "test-key-1",
		grants:       map[string]Grant{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", s.discovery)
	mux.HandleFunc("/jwks", s.jwks)
	mux.HandleFunc("/authorize", s.authorize)
	mux.HandleFunc("/token", s.token)
	s.Server = httptest.NewServer(mux)
	s.Issuer = s.URL
	t.Cleanup(s.Close)
	return s
}

// AddGrant registers an authorization code. Codes are single use.
func (s *OIDCServer) AddGrant(code string, g Grant) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants[code] = g
}

// RotateKey replaces the signing key and kid, so previously issued tokens
// reference a key no longer in the JWKS.
func (s *OIDCServer) RotateKey(t testing.TB, kid string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.key = key
	s.kid = kid
}

// Key returns the current signing key (for building deliberately bad
// tokens in tests).
func (s *OIDCServer) Key() *rsa.PrivateKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.key
}

// KeyID returns the kid advertised for the current signing key.
func (s *OIDCServer) KeyID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kid
}

// Hits returns how many token and JWKS requests were served.
func (s *OIDCServer) Hits() (token, jwks int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokenHits, s.jwksHits
}

// LastAuthorize returns the query parameters of the most recent /authorize
// request (nil if none).
func (s *OIDCServer) LastAuthorize() url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAuthorize
}

func (s *OIDCServer) discovery(w http.ResponseWriter, r *http.Request) {
	iss := s.Issuer
	if s.DiscoveryIssuer != "" {
		iss = s.DiscoveryIssuer
	}
	doc := map[string]any{
		"issuer":                                iss,
		"authorization_endpoint":                s.Issuer + "/authorize",
		"token_endpoint":                        s.Issuer + "/token",
		"jwks_uri":                              s.Issuer + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
		"scopes_supported":                      []string{"openid", "email", "profile"},
		"claims_supported":                      []string{"sub", "email", "email_verified", "amr", "acr", "auth_time"},
		"acr_values_supported":                  []string{"phr"},
	}
	// DiscoveryOverrides lets a test say what a real issuer would say. A nil
	// value deletes the key, which is how an issuer that publishes nothing
	// about a capability is distinguished from one that publishes a list
	// without us in it -- a distinction the provider treats differently and
	// which could not otherwise be exercised.
	for k, v := range s.DiscoveryOverrides {
		if v == nil {
			delete(doc, k)
			continue
		}
		doc[k] = v
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *OIDCServer) jwks(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.jwksHits++
	pub := s.key.PublicKey
	kid := s.kid
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA",
			"use": "sig",
			"alg": "RS256",
			"kid": kid,
			"n":   b64(pub.N.Bytes()),
			"e":   b64(big.NewInt(int64(pub.E)).Bytes()),
		}},
	})
}

// authorize simulates a user approving the login: it registers a grant
// bound to the request's nonce and PKCE challenge and redirects back with a
// code. Tests that exercise the full round trip follow the redirect
// themselves.
func (s *OIDCServer) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	s.lastAuthorize = q
	s.autoCodes++
	code := fmt.Sprintf("auto-code-%d", s.autoCodes)
	amr := []string{"pwd"}
	acr := ""
	if q.Get("prompt") == "login" || q.Get("acr_values") != "" {
		amr = []string{"pwd", "mfa"}
		acr = q.Get("acr_values")
	}
	s.grants[code] = Grant{
		Subject:       "auto-subject",
		Email:         "auto@example.test",
		EmailVerified: true,
		Nonce:         q.Get("nonce"),
		CodeChallenge: q.Get("code_challenge"),
		AMR:           amr,
		ACR:           acr,
		AuthTime:      s.Now(),
	}
	s.mu.Unlock()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || redirect.String() == "" {
		http.Error(w, "missing redirect_uri", http.StatusBadRequest)
		return
	}
	rq := redirect.Query()
	rq.Set("code", code)
	rq.Set("state", q.Get("state"))
	redirect.RawQuery = rq.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound) // #nosec G710 -- test-only fake IdP: a fake authorize endpoint must redirect to the redirect_uri its own test client supplied; depguard forbids authtest in production code
}

func (s *OIDCServer) token(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.tokenHits++
	s.mu.Unlock()
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "invalid_request"})
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != s.ClientID || secret != s.ClientSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_client"})
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported_grant_type"})
		return
	}
	code := r.PostForm.Get("code")
	s.mu.Lock()
	g, found := s.grants[code]
	if found {
		delete(s.grants, code)
	}
	s.mu.Unlock()
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
		return
	}
	if g.CodeChallenge != "" && S256Challenge(r.PostForm.Get("code_verifier")) != g.CodeChallenge {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "pkce verification failed"})
		return
	}
	body := map[string]any{
		"access_token": "access-" + code,
		"token_type":   "Bearer",
		"expires_in":   3600,
	}
	if !g.OmitIDToken {
		body["id_token"] = s.SignIDToken(g)
	}
	writeJSON(w, http.StatusOK, body)
}

// SignIDToken builds and signs an ID token for g, applying defaults. A
// RawIDToken override is returned as is.
func (s *OIDCServer) SignIDToken(g Grant) string {
	if g.RawIDToken != "" {
		return g.RawIDToken
	}
	now := s.Now()
	s.mu.Lock()
	key, kid := s.key, s.kid
	s.mu.Unlock()
	if g.SignWith != nil {
		key = g.SignWith
	}
	if g.KeyID != "" {
		kid = g.KeyID
	}
	alg := g.Alg
	if alg == "" {
		alg = "RS256"
	}
	iss := g.Issuer
	if iss == "" {
		iss = s.Issuer
	}
	aud := g.Audience
	if aud == nil {
		aud = []string{s.ClientID}
	}
	iat := g.IssuedAt
	if iat.IsZero() {
		iat = now
	}
	exp := g.Expiry
	if exp.IsZero() {
		exp = now.Add(5 * time.Minute)
	}
	claims := map[string]any{
		"iss": iss,
		"sub": g.Subject,
		"iat": iat.Unix(),
		"exp": exp.Unix(),
	}
	if len(aud) == 1 {
		claims["aud"] = aud[0]
	} else {
		claims["aud"] = aud
	}
	if g.Nonce != "" {
		claims["nonce"] = g.Nonce
	}
	if g.Email != "" {
		claims["email"] = g.Email
		claims["email_verified"] = g.EmailVerified
	}
	if g.AMR != nil {
		claims["amr"] = g.AMR
	}
	if g.ACR != "" {
		claims["acr"] = g.ACR
	}
	if !g.AuthTime.IsZero() {
		claims["auth_time"] = g.AuthTime.Unix()
	}
	for k, v := range g.Extra {
		claims[k] = v
	}
	header := map[string]any{"alg": alg, "typ": "JWT"}
	if alg != "none" {
		header["kid"] = kid
	}
	hb, _ := json.Marshal(header)
	pb, _ := json.Marshal(claims)
	signingInput := b64(hb) + "." + b64(pb)
	if alg == "none" {
		return signingInput + "."
	}
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		panic("authtest: sign id token: " + err.Error())
	}
	return signingInput + "." + b64(sig)
}

// S256Challenge computes the PKCE S256 challenge for verifier.
func S256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return b64(sum[:])
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ParseUnixClaim converts a numeric claim value to time (test helper).
func ParseUnixClaim(v any) (time.Time, bool) {
	switch n := v.(type) {
	case float64:
		return time.Unix(int64(n), 0).UTC(), true
	case int64:
		return time.Unix(n, 0).UTC(), true
	case json.Number:
		i, err := strconv.ParseInt(string(n), 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		return time.Unix(i, 0).UTC(), true
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		return time.Unix(i, 0).UTC(), true
	}
	return time.Time{}, false
}
