package devidp

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/security"
)

// Name is reported by Provider.Name.
const Name = "devidp"

// DefaultLoginURL is where AuthCodeURL points when Config.LoginURL is
// empty: a dev-only page that lets the developer pick an identity and
// posts back to the app's callback with code=<identity>[:mfa].
const DefaultLoginURL = "/auth/dev/login"

// MFASuffix on a code yields a strong amr ("mfa") and acr "phr".
const MFASuffix = "mfa"

// ErrUnknownIdentity is returned by Exchange for a code that names no
// fixed identity.
var ErrUnknownIdentity = errors.New("devidp: unknown identity")

// Allowed reports whether env permits the dev identity provider. The match
// is exact and case-sensitive; anything else is refused.
func Allowed(env string) bool {
	switch env {
	case "LOCAL", "TEST", "DEV":
		return true
	}
	return false
}

// Config configures a Provider.
type Config struct {
	// LoginURL is the dev identity picker (default DefaultLoginURL).
	LoginURL string
	// Now is the injected clock for auth_time (default UTC wall clock).
	Now func() time.Time
}

// Provider is the dev identity provider.
type Provider struct {
	env      string
	loginURL string
	now      func() time.Time
}

var _ auth.IdentityProvider = (*Provider)(nil)

// New returns a Provider for LOCAL, TEST or DEV and auth.ErrDevIdPNotAllowed
// for every other env value.
func New(env string, cfg Config) (*Provider, error) {
	if !Allowed(env) {
		return nil, fmt.Errorf("%w: env %q", auth.ErrDevIdPNotAllowed, env)
	}
	if cfg.LoginURL == "" {
		cfg.LoginURL = DefaultLoginURL
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Provider{env: env, loginURL: cfg.LoginURL, now: cfg.Now}, nil
}

type fixedIdentity struct {
	name     string
	roles    []security.Role
	accounts []string
}

// identities is the closed, read-only table of dev identities.
var identities = []fixedIdentity{
	{"customer-a", []security.Role{security.RoleCustomer}, []string{"acct-dev-a"}},
	{"customer-b", []security.Role{security.RoleCustomer}, []string{"acct-dev-b"}},
	{"support", []security.Role{security.RoleSupportReadOnly}, nil},
	{"operations", []security.Role{security.RoleOperations}, nil},
	{"risk", []security.Role{security.RoleRisk}, nil},
	{"compliance", []security.Role{security.RoleCompliance}, nil},
	{"finance", []security.Role{security.RoleFinance}, nil},
	{"security", []security.Role{security.RoleSecurity}, nil},
	{"admin", []security.Role{security.RoleAdmin}, nil},
}

// Identities lists the fixed identity names.
func Identities() []string {
	out := make([]string, 0, len(identities))
	for _, id := range identities {
		out = append(out, id.name)
	}
	return out
}

// Env returns the environment the provider was constructed for.
func (p *Provider) Env() string { return p.env }

// Name implements auth.IdentityProvider.
func (p *Provider) Name() string { return Name }

// AuthCodeURL implements auth.IdentityProvider: it sends the browser to
// the dev picker with the same parameters a real provider would receive.
func (p *Provider) AuthCodeURL(state, nonce, codeChallenge string, stepUp bool) string {
	q := url.Values{}
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	if stepUp {
		q.Set("prompt", "login")
		q.Set("acr_values", "phr")
	}
	sep := "?"
	if strings.Contains(p.loginURL, "?") {
		sep = "&"
	}
	return p.loginURL + sep + q.Encode()
}

// Exchange implements auth.IdentityProvider. code is "<identity>" or
// "<identity>:mfa". codeVerifier and nonce must be present so callers
// exercise the same contract as with a real provider; the nonce is echoed
// in Claims["nonce"].
func (p *Provider) Exchange(_ context.Context, code, codeVerifier, nonce string) (auth.Identity, error) {
	if code == "" || codeVerifier == "" || nonce == "" {
		return auth.Identity{}, errors.New("devidp: code, code verifier and nonce are required")
	}
	name, suffix, _ := strings.Cut(code, ":")
	if suffix != "" && suffix != MFASuffix {
		return auth.Identity{}, fmt.Errorf("%w: %q", ErrUnknownIdentity, code)
	}
	var fixed *fixedIdentity
	for i := range identities {
		if identities[i].name == name {
			fixed = &identities[i]
			break
		}
	}
	if fixed == nil {
		return auth.Identity{}, fmt.Errorf("%w: %q", ErrUnknownIdentity, name)
	}
	amr := []string{"pwd"}
	acr := ""
	if suffix == MFASuffix {
		amr = append(amr, "mfa")
		acr = "phr"
	}
	roles := make([]string, 0, len(fixed.roles))
	for _, r := range fixed.roles {
		roles = append(roles, string(r))
	}
	subject := "dev:" + name
	email := name + "@dev.invalid"
	return auth.Identity{
		Subject:       subject,
		Email:         email,
		EmailVerified: true,
		AMR:           amr,
		AuthTime:      p.now().UTC(),
		ACR:           acr,
		Claims: map[string]any{
			"iss":          Name,
			"sub":          subject,
			"email":        email,
			"nonce":        nonce,
			"dev_roles":    roles,
			"dev_accounts": append([]string(nil), fixed.accounts...),
		},
	}, nil
}

// Roles returns the roles a dev identity carries in its claims (empty for
// identities not produced by this package).
func Roles(id auth.Identity) []security.Role {
	raw, _ := id.Claims["dev_roles"].([]string)
	out := make([]security.Role, 0, len(raw))
	for _, r := range raw {
		if role := security.Role(r); role.Valid() {
			out = append(out, role)
		}
	}
	return out
}

// AccountIDs returns the accounts a dev identity carries in its claims.
func AccountIDs(id auth.Identity) []string {
	raw, _ := id.Claims["dev_accounts"].([]string)
	return append([]string(nil), raw...)
}
