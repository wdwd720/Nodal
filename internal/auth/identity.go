package auth

import (
	"context"
	"errors"
	"time"
)

// IdentityProvider is the OIDC authorization-code + PKCE boundary. The
// control plane never sees a password: it redirects the browser to
// AuthCodeURL and later exchanges the returned code. A production provider
// lives in internal/auth/oidc; a dev-only one in internal/auth/devidp.
type IdentityProvider interface {
	// AuthCodeURL builds the redirect URL for the provider's authorization
	// endpoint. state binds the callback to the browser session, nonce
	// binds the ID token to this login, codeChallenge is the PKCE S256
	// challenge. When stepUp is true the provider is asked to re-prompt
	// with a strong method (acr_values, prompt=login).
	AuthCodeURL(state, nonce, codeChallenge string, stepUp bool) string

	// Exchange redeems the authorization code with the PKCE verifier and
	// returns the verified identity. It must verify the ID token's
	// signature, issuer, audience, expiry and that its nonce equals nonce.
	Exchange(ctx context.Context, code, codeVerifier, nonce string) (Identity, error)

	// Name identifies the provider in logs and audit records.
	Name() string
}

// Identity is what an IdentityProvider asserts about the person who just
// authenticated. Subject is the provider-stable identifier used as
// Session.SubjectID; email is informational and never an identity key.
type Identity struct {
	Subject       string
	Email         string
	EmailVerified bool
	AMR           []string
	AuthTime      time.Time
	ACR           string
	Claims        map[string]any
}

// Validate checks that the identity can back a session.
func (i Identity) Validate() error {
	if i.Subject == "" {
		return errors.New("auth: identity has empty subject")
	}
	return nil
}
