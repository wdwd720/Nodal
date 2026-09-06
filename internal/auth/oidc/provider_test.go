package oidc_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/auth/authtest"
	"github.com/nodal/controlplane/internal/auth/oidc"
	"github.com/nodal/controlplane/internal/security"
)

var t0 = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

type env struct {
	srv *authtest.OIDCServer
	clk *authtest.Clock
	p   *oidc.Provider
}

func setup(t *testing.T, mutate func(*oidc.Config)) env {
	t.Helper()
	srv := authtest.NewOIDCServer(t, "cp-web", "cp-secret")
	clk := authtest.NewClock(t0)
	srv.Now = clk.Now
	cfg := oidc.Config{
		Issuer:         srv.Issuer,
		ClientID:       srv.ClientID,
		ClientSecret:   srv.ClientSecret,
		RedirectURL:    "http://127.0.0.1:3000/auth/callback",
		HTTPClient:     srv.Client(),
		Now:            clk.Now,
		JWKSMinRefresh: time.Nanosecond,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := oidc.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("oidc.New: %v", err)
	}
	return env{srv: srv, clk: clk, p: p}
}

func goodGrant(nonce, challenge string) authtest.Grant {
	return authtest.Grant{
		Subject: "user-1", Email: "user1@example.test", EmailVerified: true,
		Nonce: nonce, AMR: []string{"pwd", "mfa"}, ACR: "phr", AuthTime: t0.Add(-time.Second),
		CodeChallenge: challenge,
	}
}

func mustIs(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("got %v, want errors.Is(%v)", err, target)
	}
}

func TestNew_Validation(t *testing.T) {
	srv := authtest.NewOIDCServer(t, "cp-web", "cp-secret")
	base := oidc.Config{Issuer: srv.Issuer, ClientID: "cp-web", RedirectURL: "http://127.0.0.1/cb", HTTPClient: srv.Client()}
	cases := map[string]func(*oidc.Config){
		"missing issuer":        func(c *oidc.Config) { c.Issuer = "" },
		"missing client id":     func(c *oidc.Config) { c.ClientID = "" },
		"missing redirect":      func(c *oidc.Config) { c.RedirectURL = "" },
		"plain http issuer":     func(c *oidc.Config) { c.Issuer = "http://idp.example.com" },
		"unsupported scheme":    func(c *oidc.Config) { c.Issuer = "ftp://idp.example.com" },
		"hmac algorithm":        func(c *oidc.Config) { c.SigningAlgs = []string{"HS256"} },
		"alg not advertised":    func(c *oidc.Config) { c.SigningAlgs = []string{"ES256"} },
		"negative skew":         func(c *oidc.Config) { c.ClockSkew = -time.Second },
		"issuer not discovered": func(c *oidc.Config) { c.Issuer = srv.Issuer + "/tenant" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := base
			mutate(&cfg)
			if _, err := oidc.New(context.Background(), cfg); err == nil {
				t.Fatal("New accepted invalid config")
			}
		})
	}
	t.Run("discovery issuer mismatch", func(t *testing.T) {
		srv2 := authtest.NewOIDCServer(t, "cp-web", "s")
		srv2.DiscoveryIssuer = "https://evil.example"
		cfg := base
		cfg.Issuer = srv2.Issuer
		cfg.HTTPClient = srv2.Client()
		if _, err := oidc.New(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("issuer mismatch accepted: %v", err)
		}
	})
	t.Run("happy", func(t *testing.T) {
		p, err := oidc.New(context.Background(), base)
		if err != nil {
			t.Fatal(err)
		}
		d := p.Discovery()
		if d.Issuer != srv.Issuer || d.TokenEndpoint != srv.Issuer+"/token" || d.JWKSURI != srv.Issuer+"/jwks" {
			t.Fatalf("discovery: %+v", d)
		}
		if p.Name() != "oidc" {
			t.Fatalf("name %q", p.Name())
		}
		cfg := base
		cfg.Name = "okta-prod"
		p2, _ := oidc.New(context.Background(), cfg)
		if p2.Name() != "okta-prod" {
			t.Fatal("name override ignored")
		}
	})
}

func TestAuthCodeURL(t *testing.T) {
	e := setup(t, nil)
	_, challenge := oidc.GeneratePKCE()
	for _, stepUp := range []bool{false, true} {
		u, err := url.Parse(e.p.AuthCodeURL("st4te", "n0nce", challenge, stepUp))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(u.String(), e.srv.Issuer+"/authorize?") {
			t.Fatalf("wrong endpoint: %s", u)
		}
		q := u.Query()
		want := map[string]string{
			"response_type": "code", "client_id": "cp-web", "redirect_uri": "http://127.0.0.1:3000/auth/callback",
			"state": "st4te", "nonce": "n0nce", "code_challenge": challenge, "code_challenge_method": "S256",
		}
		for k, v := range want {
			if q.Get(k) != v {
				t.Fatalf("stepUp=%v: %s = %q, want %q", stepUp, k, q.Get(k), v)
			}
		}
		if !strings.Contains(" "+q.Get("scope")+" ", " openid ") {
			t.Fatalf("scope %q lacks openid", q.Get("scope"))
		}
		if stepUp {
			if q.Get("prompt") != "login" || q.Get("acr_values") != "phr" || q.Get("max_age") != "0" {
				t.Fatalf("step-up params missing: %v", q)
			}
		} else if q.Has("prompt") || q.Has("acr_values") || q.Has("max_age") {
			t.Fatalf("step-up params present without stepUp: %v", q)
		}
	}
	e2 := setup(t, func(c *oidc.Config) { c.StepUpACRValues = []string{"phrh", "phr"}; c.Scopes = []string{"email"} })
	u, _ := url.Parse(e2.p.AuthCodeURL("s", "n", "c", true))
	if u.Query().Get("acr_values") != "phrh phr" || u.Query().Get("scope") != "openid email" {
		t.Fatalf("config not applied: %v", u.Query())
	}
}

func TestExchange_HappyPath(t *testing.T) {
	e := setup(t, nil)
	verifier, challenge := oidc.GeneratePKCE()
	e.srv.AddGrant("code-1", goodGrant("n1", challenge))
	id, err := e.p.Exchange(context.Background(), "code-1", verifier, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "user-1" || id.Email != "user1@example.test" || !id.EmailVerified || id.ACR != "phr" {
		t.Fatalf("identity: %+v", id)
	}
	if !security.HasStrongAMR(id.AMR) || !id.AuthTime.Equal(t0.Add(-time.Second)) {
		t.Fatalf("amr/auth_time: %+v", id)
	}
	if id.Claims["sub"] != "user-1" || id.Claims["iss"] != e.srv.Issuer {
		t.Fatalf("claims: %v", id.Claims)
	}
	if _, err := e.p.Exchange(context.Background(), "code-1", verifier, "n1"); err == nil {
		t.Fatal("authorization code was accepted twice")
	}
}

func TestExchange_PKCEMismatch(t *testing.T) {
	e := setup(t, nil)
	_, challenge := oidc.GeneratePKCE()
	other, _ := oidc.GeneratePKCE()
	e.srv.AddGrant("code-1", goodGrant("n1", challenge))
	if _, err := e.p.Exchange(context.Background(), "code-1", other, "n1"); err == nil || !strings.Contains(err.Error(), "token exchange") {
		t.Fatalf("wrong verifier accepted: %v", err)
	}
}

func TestExchange_BadNonce(t *testing.T) {
	e := setup(t, nil)
	verifier, challenge := oidc.GeneratePKCE()
	e.srv.AddGrant("code-1", goodGrant("n1", challenge))
	_, err := e.p.Exchange(context.Background(), "code-1", verifier, "n2")
	mustIs(t, err, oidc.ErrNonceMismatch)
	mustIs(t, err, oidc.ErrInvalidIDToken)

	e.srv.AddGrant("code-2", goodGrant("", challenge))
	_, err = e.p.Exchange(context.Background(), "code-2", verifier, "n1")
	mustIs(t, err, oidc.ErrNonceMismatch)
}

func TestExchange_WrongAudience(t *testing.T) {
	e := setup(t, nil)
	verifier, challenge := oidc.GeneratePKCE()
	cases := []struct {
		name string
		aud  []string
		azp  string
		ok   bool
	}{
		{"other client", []string{"other-client"}, "", false},
		{"multi without azp", []string{"cp-web", "other"}, "", false},
		{"multi with foreign azp", []string{"cp-web", "other"}, "other", false},
		{"multi with our azp", []string{"cp-web", "other"}, "cp-web", true},
		{"single with foreign azp", []string{"cp-web"}, "other", false},
		{"single ok", []string{"cp-web"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := goodGrant("n1", challenge)
			g.Audience = tc.aud
			if tc.azp != "" {
				g.Extra = map[string]any{"azp": tc.azp}
			}
			e.srv.AddGrant("code", g)
			_, err := e.p.Exchange(context.Background(), "code", verifier, "n1")
			if tc.ok {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			mustIs(t, err, oidc.ErrAudienceMismatch)
		})
	}
}

func TestExchange_Expiry(t *testing.T) {
	e := setup(t, nil)
	verifier, challenge := oidc.GeneratePKCE()

	g := goodGrant("n1", challenge)
	g.Expiry = t0.Add(-10 * time.Minute)
	e.srv.AddGrant("expired", g)
	_, err := e.p.Exchange(context.Background(), "expired", verifier, "n1")
	mustIs(t, err, oidc.ErrTokenExpired)

	g = goodGrant("n1", challenge)
	g.Expiry = t0.Add(-time.Minute) // within the 2m default skew
	e.srv.AddGrant("skew", g)
	if _, err := e.p.Exchange(context.Background(), "skew", verifier, "n1"); err != nil {
		t.Fatalf("token within skew rejected: %v", err)
	}

	g = goodGrant("n1", challenge)
	g.IssuedAt = t0.Add(10 * time.Minute)
	e.srv.AddGrant("future", g)
	_, err = e.p.Exchange(context.Background(), "future", verifier, "n1")
	mustIs(t, err, oidc.ErrTokenNotYetValid)

	g = goodGrant("n1", challenge)
	g.Extra = map[string]any{"nbf": t0.Add(10 * time.Minute).Unix()}
	e.srv.AddGrant("nbf", g)
	_, err = e.p.Exchange(context.Background(), "nbf", verifier, "n1")
	mustIs(t, err, oidc.ErrTokenNotYetValid)
}

func TestExchange_IssuerMismatch(t *testing.T) {
	e := setup(t, nil)
	verifier, challenge := oidc.GeneratePKCE()
	g := goodGrant("n1", challenge)
	g.Issuer = "https://evil.example"
	e.srv.AddGrant("code", g)
	_, err := e.p.Exchange(context.Background(), "code", verifier, "n1")
	mustIs(t, err, oidc.ErrIssuerMismatch)
}

func TestExchange_BadSignature(t *testing.T) {
	e := setup(t, nil)
	verifier, challenge := oidc.GeneratePKCE()
	other := authtest.NewOIDCServer(t, "x", "y") // just a source of another RSA key
	g := goodGrant("n1", challenge)
	g.SignWith = other.Key()
	e.srv.AddGrant("code", g)
	_, err := e.p.Exchange(context.Background(), "code", verifier, "n1")
	mustIs(t, err, oidc.ErrBadSignature)
}

func TestExchange_AlgNoneRejected(t *testing.T) {
	e := setup(t, nil)
	verifier, challenge := oidc.GeneratePKCE()
	g := goodGrant("n1", challenge)
	g.Alg = "none"
	e.srv.AddGrant("code", g)
	_, err := e.p.Exchange(context.Background(), "code", verifier, "n1")
	mustIs(t, err, oidc.ErrUnsupportedAlg)
}

func TestExchange_UnknownKidAndRotation(t *testing.T) {
	e := setup(t, nil)
	verifier, challenge := oidc.GeneratePKCE()

	g := goodGrant("n1", challenge)
	g.KeyID = "no-such-key"
	e.srv.AddGrant("code", g)
	_, err := e.p.Exchange(context.Background(), "code", verifier, "n1")
	mustIs(t, err, oidc.ErrUnknownKey)
	if _, jwks := e.srv.Hits(); jwks != 1 {
		t.Fatalf("jwks fetched %d times, want 1 (refresh rate-limited)", jwks)
	}

	// The provider rotates its key; after the refresh window the verifier
	// refetches and accepts tokens under the new kid.
	e.srv.RotateKey(t, "test-key-2")
	e.clk.Advance(time.Second)
	e.srv.AddGrant("code2", goodGrant("n1", challenge))
	if _, err := e.p.Exchange(context.Background(), "code2", verifier, "n1"); err != nil {
		t.Fatalf("after rotation: %v", err)
	}
	if _, jwks := e.srv.Hits(); jwks != 2 {
		t.Fatalf("jwks fetched %d times, want 2", jwks)
	}
	// Cached now: no further fetch for a known kid.
	e.srv.AddGrant("code3", goodGrant("n1", challenge))
	if _, err := e.p.Exchange(context.Background(), "code3", verifier, "n1"); err != nil {
		t.Fatal(err)
	}
	if _, jwks := e.srv.Hits(); jwks != 2 {
		t.Fatalf("jwks refetched for a cached kid")
	}
}

func TestExchangeStepUp_MissingAMR(t *testing.T) {
	e := setup(t, nil)
	verifier, challenge := oidc.GeneratePKCE()

	g := goodGrant("n1", challenge)
	g.AMR = nil
	g.ACR = ""
	e.srv.AddGrant("no-amr", g)
	_, err := e.p.ExchangeStepUp(context.Background(), "no-amr", verifier, "n1")
	mustIs(t, err, auth.ErrStepUpNotSatisfied)
	mustIs(t, err, security.ErrStepUpRequired)

	g = goodGrant("n1", challenge)
	g.AMR = []string{"pwd"}
	e.srv.AddGrant("pwd-only", g)
	_, err = e.p.ExchangeStepUp(context.Background(), "pwd-only", verifier, "n1")
	mustIs(t, err, auth.ErrStepUpNotSatisfied)

	// acr alone is not enough: the session AMR must carry the strong method.
	g = goodGrant("n1", challenge)
	g.AMR = nil
	g.ACR = "phr"
	e.srv.AddGrant("acr-only", g)
	_, err = e.p.ExchangeStepUp(context.Background(), "acr-only", verifier, "n1")
	mustIs(t, err, auth.ErrStepUpNotSatisfied)

	// A plain Exchange of the same token succeeds, and the resulting
	// principal then fails RequireStepUp downstream.
	g = goodGrant("n1", challenge)
	g.AMR = nil
	e.srv.AddGrant("plain", g)
	id, err := e.p.Exchange(context.Background(), "plain", verifier, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if len(id.AMR) != 0 {
		t.Fatalf("amr fabricated: %v", id.AMR)
	}
	p := security.Principal{SubjectID: id.Subject, ActorType: security.ActorUser, AuthTime: id.AuthTime, AMR: id.AMR}
	if err := security.RequireStepUp(security.WithPrincipal(context.Background(), p), time.Hour, e.clk.Now); !errors.Is(err, security.ErrStepUpRequired) {
		t.Fatalf("downstream step-up check passed without amr: %v", err)
	}

	e.srv.AddGrant("mfa", goodGrant("n1", challenge))
	if _, err := e.p.ExchangeStepUp(context.Background(), "mfa", verifier, "n1"); err != nil {
		t.Fatal(err)
	}
}

func TestExchange_MissingSubjectAndArgs(t *testing.T) {
	e := setup(t, nil)
	verifier, challenge := oidc.GeneratePKCE()
	g := goodGrant("n1", challenge)
	g.Subject = ""
	e.srv.AddGrant("code", g)
	_, err := e.p.Exchange(context.Background(), "code", verifier, "n1")
	mustIs(t, err, oidc.ErrMissingSubject)

	for _, args := range [][3]string{{"", verifier, "n1"}, {"code", "", "n1"}, {"code", verifier, ""}} {
		if _, err := e.p.Exchange(context.Background(), args[0], args[1], args[2]); err == nil {
			t.Fatalf("empty argument accepted: %q", args)
		}
	}
	tokenHits, _ := e.srv.Hits()
	if tokenHits != 1 {
		t.Fatalf("token endpoint called %d times; empty-argument calls must not reach it", tokenHits)
	}
}

func TestExchange_NoIDTokenInResponse(t *testing.T) {
	e := setup(t, nil)
	verifier, challenge := oidc.GeneratePKCE()
	g := goodGrant("n1", challenge)
	g.OmitIDToken = true
	e.srv.AddGrant("code", g)
	_, err := e.p.Exchange(context.Background(), "code", verifier, "n1")
	mustIs(t, err, oidc.ErrInvalidIDToken)
	if !strings.Contains(err.Error(), "no id_token") {
		t.Fatalf("missing id_token not reported: %v", err)
	}
}

func TestFullRoundTrip(t *testing.T) {
	e := setup(t, nil)
	verifier, challenge := oidc.GeneratePKCE()
	state, _ := oidc.GenerateState()
	nonce, _ := oidc.GenerateNonce()

	client := *e.srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Get(e.p.AuthCodeURL(state, nonce, challenge, true))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize status %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if loc.Query().Get("state") != state || !strings.HasPrefix(loc.String(), "http://127.0.0.1:3000/auth/callback?") {
		t.Fatalf("callback %s", loc)
	}
	id, err := e.p.ExchangeStepUp(context.Background(), loc.Query().Get("code"), verifier, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "auto-subject" || !security.HasStrongAMR(id.AMR) || id.ACR != "phr" {
		t.Fatalf("identity %+v", id)
	}
	if got := e.srv.LastAuthorize(); got.Get("prompt") != "login" || got.Get("code_challenge") != challenge {
		t.Fatalf("authorize request: %v", got)
	}
}

func TestGenerators(t *testing.T) {
	v, c := oidc.GeneratePKCE()
	if v == "" || c == "" || c != authtest.S256Challenge(v) {
		t.Fatal("pkce pair inconsistent")
	}
	s1, _ := oidc.GenerateState()
	s2, _ := oidc.GenerateState()
	n1, _ := oidc.GenerateNonce()
	if s1 == s2 || len(s1) != 43 || len(n1) != 43 {
		t.Fatal("state/nonce generation weak")
	}
}
