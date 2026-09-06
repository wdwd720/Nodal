package oidc

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"

	"github.com/nodal/controlplane/internal/auth"
)

// Verification errors. All wrap ErrInvalidIDToken; callers map that to
// UNAUTHENTICATED and log the specific reason.
var (
	ErrInvalidIDToken   = errors.New("oidc: invalid id token")
	ErrUnsupportedAlg   = fmt.Errorf("%w: unsupported signing algorithm", ErrInvalidIDToken)
	ErrUnknownKey       = fmt.Errorf("%w: unknown signing key", ErrInvalidIDToken)
	ErrBadSignature     = fmt.Errorf("%w: signature verification failed", ErrInvalidIDToken)
	ErrIssuerMismatch   = fmt.Errorf("%w: issuer mismatch", ErrInvalidIDToken)
	ErrAudienceMismatch = fmt.Errorf("%w: audience mismatch", ErrInvalidIDToken)
	ErrNonceMismatch    = fmt.Errorf("%w: nonce mismatch", ErrInvalidIDToken)
	ErrTokenExpired     = fmt.Errorf("%w: expired", ErrInvalidIDToken)
	ErrTokenNotYetValid = fmt.Errorf("%w: not yet valid", ErrInvalidIDToken)
	ErrMissingSubject   = fmt.Errorf("%w: missing subject", ErrInvalidIDToken)
)

// DefaultSigningAlgs are the asymmetric JWS algorithms accepted by default.
// HMAC algorithms are never accepted: a shared secret would let any party
// holding it mint identities.
var DefaultSigningAlgs = []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512"}

// supportedSigningAlgs are the algorithms go-oidc can verify; Config.SigningAlgs
// must be drawn from this set.
var supportedSigningAlgs = map[string]bool{
	gooidc.RS256: true, gooidc.RS384: true, gooidc.RS512: true,
	gooidc.PS256: true, gooidc.PS384: true, gooidc.PS512: true,
	gooidc.ES256: true, gooidc.ES384: true, gooidc.ES512: true,
	gooidc.EdDSA: true,
}

// unixTime accepts a NumericDate as a JSON number or a numeric string.
type unixTime struct {
	t   time.Time
	set bool
}

func (u *unixTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" || s == "" {
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("numeric date: %w", err)
	}
	sec, frac := int64(f), f-float64(int64(f))
	u.t = time.Unix(sec, int64(frac*1e9)).UTC()
	u.set = true
	return nil
}

// flexBool accepts true/false or the strings "true"/"false" (some IdPs
// emit email_verified as a string).
type flexBool bool

func (fb *flexBool) UnmarshalJSON(b []byte) error {
	switch strings.Trim(string(b), `"`) {
	case "true":
		*fb = true
	case "false", "null", "":
		*fb = false
	default:
		return fmt.Errorf("boolean: %q", string(b))
	}
	return nil
}

// idClaims are the claims read from a verified ID token beyond those
// gooidc.IDToken already exposes (iss, sub, aud, exp, iat, nonce).
type idClaims struct {
	AuthorizedParty string   `json:"azp"`
	IssuedAt        unixTime `json:"iat"`
	NotBefore       unixTime `json:"nbf"`
	Nonce           string   `json:"nonce"`
	Email           string   `json:"email"`
	EmailVerified   flexBool `json:"email_verified"`
	AMR             []string `json:"amr"`
	ACR             string   `json:"acr"`
	AuthTime        unixTime `json:"auth_time"`
}

// verifyIDToken hands raw to go-oidc (algorithm allow-list, signature, iss,
// aud, exp/nbf), then applies the checks go-oidc leaves to the caller and
// binds the token to expectedNonce. Nothing from the payload is read before
// go-oidc has verified the signature.
func (p *Provider) verifyIDToken(ctx context.Context, raw, expectedNonce string) (auth.Identity, error) {
	outcome := &signatureOutcome{}
	idt, err := p.verifier.Verify(context.WithValue(ctx, signatureOutcomeKey{}, outcome), raw)
	if err != nil {
		return auth.Identity{}, translate(err, outcome)
	}
	var c idClaims
	if err := idt.Claims(&c); err != nil {
		return auth.Identity{}, fmt.Errorf("%w: payload json: %w", ErrInvalidIDToken, err)
	}
	var all map[string]any
	if err := idt.Claims(&all); err != nil {
		return auth.Identity{}, fmt.Errorf("%w: payload json: %w", ErrInvalidIDToken, err)
	}
	if err := p.checkClaims(idt, c, expectedNonce); err != nil {
		return auth.Identity{}, err
	}
	return auth.Identity{
		Subject:       idt.Subject,
		Email:         c.Email,
		EmailVerified: bool(c.EmailVerified),
		AMR:           append([]string(nil), c.AMR...),
		AuthTime:      c.AuthTime.t,
		ACR:           c.ACR,
		Claims:        all,
	}, nil
}

// checkClaims holds the explicit checks: go-oidc verifies that aud contains
// the client id, that exp has not passed (with the configured skew, see
// New) and that nbf is not more than five minutes ahead; everything below
// is ours.
func (p *Provider) checkClaims(idt *gooidc.IDToken, c idClaims, expectedNonce string) error {
	// OIDC Core 3.1.3.7 (4)-(5): multi-audience tokens must name us as azp,
	// and an azp that names someone else is never ours.
	if len(idt.Audience) > 1 && c.AuthorizedParty != p.cfg.ClientID {
		return fmt.Errorf("%w: azp %q", ErrAudienceMismatch, c.AuthorizedParty)
	}
	if c.AuthorizedParty != "" && c.AuthorizedParty != p.cfg.ClientID {
		return fmt.Errorf("%w: azp %q", ErrAudienceMismatch, c.AuthorizedParty)
	}
	// go-oidc's nbf leeway is fixed at five minutes and it does not look at
	// iat; both are held to the configured skew here.
	now := p.cfg.Now()
	if c.NotBefore.set && now.Add(p.cfg.ClockSkew).Before(c.NotBefore.t) {
		return fmt.Errorf("%w: nbf %s", ErrTokenNotYetValid, c.NotBefore.t.Format(time.RFC3339))
	}
	if c.IssuedAt.set && now.Add(p.cfg.ClockSkew).Before(c.IssuedAt.t) {
		return fmt.Errorf("%w: iat %s", ErrTokenNotYetValid, c.IssuedAt.t.Format(time.RFC3339))
	}
	if expectedNonce == "" || subtle.ConstantTimeCompare([]byte(c.Nonce), []byte(expectedNonce)) != 1 {
		return ErrNonceMismatch
	}
	if idt.Subject == "" {
		return ErrMissingSubject
	}
	return nil
}

// translate maps a go-oidc verification failure onto this package's
// sentinel errors. The verdict (reject) is go-oidc's and is never changed
// here; translation only names the reason. go-oidc types the expiry error
// and this package's key set records its own typed verdict; the remaining
// reasons are recognized from go-oidc v3.21 / go-jose v4 message fragments,
// each pinned by a negative test, and anything unrecognized is reported as
// a plain ErrInvalidIDToken.
func translate(err error, outcome *signatureOutcome) error {
	var expired *gooidc.TokenExpiredError
	if errors.As(err, &expired) {
		if expired.Expiry.IsZero() {
			return fmt.Errorf("%w: missing exp", ErrInvalidIDToken)
		}
		return fmt.Errorf("%w: exp %s", ErrTokenExpired, expired.Expiry.UTC().Format(time.RFC3339))
	}
	if outcome.err != nil {
		return outcome.err
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unexpected signature algorithm"):
		return fmt.Errorf("%w: %w", ErrUnsupportedAlg, err)
	case strings.Contains(msg, "malformed jwt"), strings.Contains(msg, "not signed"), strings.Contains(msg, "multiple signatures"):
		return fmt.Errorf("%w: malformed: %w", ErrInvalidIDToken, err)
	case strings.Contains(msg, "issued by a different provider"):
		return fmt.Errorf("%w: %w", ErrIssuerMismatch, err)
	case strings.Contains(msg, "expected audience"):
		return fmt.Errorf("%w: %w", ErrAudienceMismatch, err)
	case strings.Contains(msg, "before the nbf"):
		return fmt.Errorf("%w: %w", ErrTokenNotYetValid, err)
	default:
		return fmt.Errorf("%w: %w", ErrInvalidIDToken, err)
	}
}
