package oidc

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/security"
)

// Defaults applied by New.
const (
	DefaultClockSkew      = 2 * time.Minute
	DefaultHTTPTimeout    = 10 * time.Second
	DefaultJWKSMinRefresh = time.Minute
	DefaultStepUpACR      = "phr" // phishing-resistant (OIDC acr value registry)
)

// Config configures a Provider. Issuer, ClientID and RedirectURL are
// required; the rest have defaults.
type Config struct {
	// Issuer is the exact issuer identifier; discovery must echo it.
	Issuer string
	// ClientID is the OAuth client id and the expected ID token audience.
	ClientID string
	// ClientSecret authenticates the token request (may be empty for public
	// clients; PKCE is always used).
	ClientSecret string
	// RedirectURL is the registered callback.
	RedirectURL string
	// Scopes default to openid, email, profile; openid is always added.
	Scopes []string
	// StepUpACRValues are sent as acr_values on step-up (default: phr).
	StepUpACRValues []string
	// SigningAlgs restricts accepted JWS algorithms (default
	// DefaultSigningAlgs); intersected with what discovery advertises.
	SigningAlgs []string
	// HTTPClient is used for discovery, JWKS and the token endpoint. New
	// works on a copy whose response bodies are capped at MaxDocumentBytes
	// and which, if HTTPClient has no Timeout, times out after
	// DefaultHTTPTimeout.
	HTTPClient *http.Client
	// Now is the injected clock for expiry checks.
	Now func() time.Time
	// ClockSkew tolerated on exp/nbf/iat (default 2m).
	ClockSkew time.Duration
	// JWKSMinRefresh bounds JWKS refetches on unknown kid (default 1m).
	JWKSMinRefresh time.Duration
	// AllowInsecureIssuer permits an http:// issuer and endpoints beyond
	// loopback addresses. Tests only; never set in STAGING/PROD.
	AllowInsecureIssuer bool
	// Name is reported by Name() (default "oidc").
	Name string
}

// Discovery is the subset of the provider metadata document we rely on.
type Discovery struct {
	Issuer                           string   `json:"issuer"`
	AuthorizationEndpoint            string   `json:"authorization_endpoint"`
	TokenEndpoint                    string   `json:"token_endpoint"`
	JWKSURI                          string   `json:"jwks_uri"`
	IDTokenSigningAlgValuesSupported []string `json:"id_token_signing_alg_values_supported"`
	CodeChallengeMethodsSupported    []string `json:"code_challenge_methods_supported"`
	ACRValuesSupported               []string `json:"acr_values_supported"`
}

// Provider implements auth.IdentityProvider for one OIDC issuer.
type Provider struct {
	cfg       Config
	disc      Discovery
	client    *http.Client
	oauth     oauth2.Config
	verifier  *gooidc.IDTokenVerifier
	stepUpACR string
}

var _ auth.IdentityProvider = (*Provider)(nil)

// New validates cfg, runs discovery and returns a ready Provider.
func New(ctx context.Context, cfg Config) (*Provider, error) {
	if cfg.Issuer == "" || cfg.ClientID == "" || cfg.RedirectURL == "" {
		return nil, errors.New("oidc: issuer, client id and redirect url are required")
	}
	if err := checkURL(cfg.Issuer, cfg.AllowInsecureIssuer); err != nil {
		return nil, fmt.Errorf("oidc: issuer: %w", err)
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.ClockSkew == 0 {
		cfg.ClockSkew = DefaultClockSkew
	}
	if cfg.ClockSkew < 0 {
		return nil, errors.New("oidc: negative clock skew")
	}
	if cfg.JWKSMinRefresh == 0 {
		cfg.JWKSMinRefresh = DefaultJWKSMinRefresh
	}
	if len(cfg.SigningAlgs) == 0 {
		cfg.SigningAlgs = append([]string(nil), DefaultSigningAlgs...)
	}
	if len(cfg.StepUpACRValues) == 0 {
		cfg.StepUpACRValues = []string{DefaultStepUpACR}
	}
	if cfg.Name == "" {
		cfg.Name = "oidc"
	}
	scopes := []string{"openid"}
	if len(cfg.Scopes) == 0 {
		scopes = append(scopes, "email", "profile")
	}
	for _, s := range cfg.Scopes {
		if s != "openid" {
			scopes = append(scopes, s)
		}
	}
	client := boundedClient(cfg.HTTPClient)

	// Discovery. go-oidc refuses a document whose issuer differs from the
	// configured one; InsecureIssuerURLContext is deliberately never used.
	provider, err := gooidc.NewProvider(gooidc.ClientContext(ctx, client), cfg.Issuer)
	if err != nil {
		var mismatch *gooidc.IssuerMismatchError
		if errors.As(err, &mismatch) {
			return nil, fmt.Errorf("oidc: discovery issuer does not match configured issuer: %w", err)
		}
		return nil, fmt.Errorf("oidc: discovery: %w", err)
	}
	var disc Discovery
	if err := provider.Claims(&disc); err != nil {
		return nil, fmt.Errorf("oidc: discovery: %w", err)
	}
	for name, u := range map[string]string{"authorization_endpoint": disc.AuthorizationEndpoint, "token_endpoint": disc.TokenEndpoint, "jwks_uri": disc.JWKSURI} {
		if u == "" {
			return nil, fmt.Errorf("oidc: discovery missing %s", name)
		}
		if err := checkURL(u, cfg.AllowInsecureIssuer); err != nil {
			return nil, fmt.Errorf("oidc: %s: %w", name, err)
		}
	}
	algs := map[string]bool{}
	for _, a := range cfg.SigningAlgs {
		if !supportedSigningAlgs[a] {
			return nil, fmt.Errorf("oidc: signing algorithm %q is not supported", a)
		}
		if len(disc.IDTokenSigningAlgValuesSupported) > 0 && !slices.Contains(disc.IDTokenSigningAlgValuesSupported, a) {
			continue
		}
		algs[a] = true
	}
	if len(algs) == 0 {
		return nil, errors.New("oidc: no accepted signing algorithm is advertised by the issuer")
	}
	algList := slices.Sorted(maps.Keys(algs))

	// PKCE is not optional in this implementation: AuthCodeURL always sends a
	// code_challenge with method S256. An issuer that does not support it
	// ignores both parameters and completes the flow anyway, so the
	// authorization code stops being bound to this client and nothing
	// anywhere reports that it happened. Refusing at construction is the only
	// place that difference is visible.
	//
	// The field is optional in discovery, so an issuer that omits it is not
	// refused -- only one that publishes a list without S256 in it, which is a
	// statement rather than a silence.
	if len(disc.CodeChallengeMethodsSupported) > 0 &&
		!slices.Contains(disc.CodeChallengeMethodsSupported, "S256") {
		return nil, fmt.Errorf(
			"oidc: issuer advertises code_challenge_methods_supported %v and not S256; this client always sends a PKCE challenge, and an issuer that ignores it leaves the authorization code unbound",
			disc.CodeChallengeMethodsSupported)
	}

	// The same reasoning for acr_values. Step-up sends acr_values and is
	// ultimately decided by the amr claim, not this -- but an issuer that has
	// published the acr values it supports, and does not list ours, will
	// silently ignore the request. A step-up that is quietly not performed is
	// the failure this whole path exists to prevent, so it is refused here
	// rather than discovered when somebody's break-glass elevation succeeds
	// without a second factor.
	//
	// acr_values is a preference list and not a conjunction: the issuer
	// satisfies ONE of the requested values, in order. So the check is that at
	// least one overlaps, not that all do -- asking for "phrh phr" against an
	// issuer that supports only "phr" is a correct request that will be
	// answered, and refusing it would be refusing the spec.
	if len(disc.ACRValuesSupported) > 0 && len(cfg.StepUpACRValues) > 0 {
		overlap := false
		for _, want := range cfg.StepUpACRValues {
			if slices.Contains(disc.ACRValuesSupported, want) {
				overlap = true
				break
			}
		}
		if !overlap {
			return nil, fmt.Errorf(
				"oidc: step-up asks for acr %v and the issuer advertises acr_values_supported %v, which share nothing; the request would be ignored",
				cfg.StepUpACRValues, disc.ACRValuesSupported)
		}
	}

	verifier := gooidc.NewVerifier(cfg.Issuer, newKeySet(disc.JWKSURI, client, cfg.Now, cfg.JWKSMinRefresh, algList), &gooidc.Config{
		ClientID:             cfg.ClientID,
		SupportedSigningAlgs: algList,
		// go-oidc compares exp against Now with no tolerance; a clock running
		// ClockSkew behind ours gives exp exactly the configured tolerance.
		Now: func() time.Time { return cfg.Now().Add(-cfg.ClockSkew) },
	})

	return &Provider{
		cfg:    cfg,
		disc:   disc,
		client: client,
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Scopes:       scopes,
			Endpoint: oauth2.Endpoint{
				AuthURL:   disc.AuthorizationEndpoint,
				TokenURL:  disc.TokenEndpoint,
				AuthStyle: oauth2.AuthStyleInHeader,
			},
		},
		verifier:  verifier,
		stepUpACR: strings.Join(cfg.StepUpACRValues, " "),
	}, nil
}

// Name implements auth.IdentityProvider.
func (p *Provider) Name() string { return p.cfg.Name }

// Discovery returns the metadata obtained at construction.
func (p *Provider) Discovery() Discovery { return p.disc }

// AuthCodeURL implements auth.IdentityProvider. It always requests PKCE
// S256 and binds the nonce; with stepUp it adds prompt=login, max_age=0 and
// acr_values so the provider re-authenticates with a strong method.
func (p *Provider) AuthCodeURL(state, nonce, codeChallenge string, stepUp bool) string {
	opts := []oauth2.AuthCodeOption{
		gooidc.Nonce(nonce),
		oauth2.SetAuthURLParam("code_challenge", codeChallenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	}
	if stepUp {
		opts = append(opts,
			oauth2.SetAuthURLParam("prompt", "login"),
			oauth2.SetAuthURLParam("max_age", "0"),
			oauth2.SetAuthURLParam("acr_values", p.stepUpACR),
		)
	}
	return p.oauth.AuthCodeURL(state, opts...)
}

// Exchange implements auth.IdentityProvider.
func (p *Provider) Exchange(ctx context.Context, code, codeVerifier, nonce string) (auth.Identity, error) {
	if code == "" || codeVerifier == "" || nonce == "" {
		return auth.Identity{}, errors.New("oidc: code, code verifier and nonce are required")
	}
	ctx = gooidc.ClientContext(ctx, p.client)
	tok, err := p.oauth.Exchange(ctx, code, oauth2.SetAuthURLParam("code_verifier", codeVerifier))
	if err != nil {
		return auth.Identity{}, fmt.Errorf("oidc: token exchange: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return auth.Identity{}, fmt.Errorf("%w: token response has no id_token", ErrInvalidIDToken)
	}
	id, err := p.verifyIDToken(ctx, raw, nonce)
	if err != nil {
		return auth.Identity{}, err
	}
	if err := id.Validate(); err != nil {
		return auth.Identity{}, fmt.Errorf("%w: %w", ErrInvalidIDToken, err)
	}
	return id, nil
}

// ExchangeStepUp is Exchange for a flow started with stepUp=true: the
// identity must carry a strong amr (security.StrongAMR) or the exchange
// fails with auth.ErrStepUpNotSatisfied. acr is recorded but not sufficient
// on its own, so the session's AMR remains the single source for
// security.RequireStepUp.
func (p *Provider) ExchangeStepUp(ctx context.Context, code, codeVerifier, nonce string) (auth.Identity, error) {
	id, err := p.Exchange(ctx, code, codeVerifier, nonce)
	if err != nil {
		return auth.Identity{}, err
	}
	if !security.HasStrongAMR(id.AMR) {
		return auth.Identity{}, fmt.Errorf("%w: amr %v", auth.ErrStepUpNotSatisfied, id.AMR)
	}
	return id, nil
}

// GeneratePKCE returns a fresh PKCE verifier and its S256 challenge.
func GeneratePKCE() (verifier, challenge string) {
	v := oauth2.GenerateVerifier()
	return v, oauth2.S256ChallengeFromVerifier(v)
}

// GenerateState returns a random, URL-safe state value (32 bytes).
func GenerateState() (string, error) { return randomURLSafe() }

// GenerateNonce returns a random, URL-safe nonce (32 bytes).
func GenerateNonce() (string, error) { return randomURLSafe() }

func randomURLSafe() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("oidc: random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// checkURL requires https, or http on a loopback host, unless insecure is
// explicitly allowed.
func checkURL(raw string, insecure bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if insecure || isLoopback(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("plain http is not allowed for %q", raw)
	default:
		return fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
