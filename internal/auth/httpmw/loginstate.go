package httpmw

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
	"time"
)

// LoginStateCookieName is the cookie that binds an authorization-code flow to
// the browser that began it. EffectiveCookieName applies the __Host- prefix to
// it exactly as it does to the session cookie.
const LoginStateCookieName = "nodal_login_state"

// SetLoginState writes the login-state cookie.
//
// # Why this exists
//
// `state` was persisted server-side in login_attempts and consumed exactly
// once, which stops a callback being REPLAYED. It does not stop a callback
// being PLANTED, and those are different attacks (F-87):
//
//  1. the attacker calls /v1/auth/login themselves and authenticates at the
//     identity provider as themselves, capturing code=C&state=S from the
//     redirect without following it;
//  2. the attacker induces the victim's browser to make a top-level navigation
//     to /v1/auth/callback?code=C&state=S;
//  3. the server finds the attempt by S, exchanges C, and sets a session
//     cookie -- in the VICTIM's browser, for the ATTACKER's subject.
//
// The victim is then signed in as the attacker and everything they do lands in
// the attacker's account: a Credit purchase paid on the victim's card, identity
// documents, a payout destination. The audit trail attributes all of it to the
// attacker's subject, which is exactly backwards.
//
// What closes it is the property `state` has in OAuth 2.0 and did not have
// here: it must be known to the user agent that STARTED the flow. So the value
// goes to the browser as well as to the database, and the callback requires
// them to agree.
//
// # Why the digest rather than the value
//
// The cookie holds base64url(sha256(state)), not state. A cookie is readable
// by anything that can read the browser's storage for this host, and the raw
// state is the key to a pending attempt row. The digest is enough to compare
// and useless to present.
//
// SameSite is Lax for the same reason the session cookie is: the callback is a
// top-level cross-site GET from the identity provider, and Strict would not
// send the cookie at all -- which would break every login rather than only the
// planted ones.
func SetLoginState(w http.ResponseWriter, state, domain string, secure bool, ttl time.Duration) {
	c := &http.Cookie{ // #nosec G124 -- HttpOnly and SameSite are set below; Secure comes from Auth.CookieSecure, which config.Validate requires true in STAGING/PROD
		Name:     EffectiveCookieName(LoginStateCookieName, domain, secure),
		Value:    LoginStateDigest(state),
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
	if !strings.HasPrefix(c.Name, HostPrefix) {
		c.Domain = domain
	}
	if ttl > 0 {
		c.MaxAge = int(ttl / time.Second)
		if c.MaxAge == 0 {
			c.MaxAge = 1
		}
	}
	http.SetCookie(w, c)
}

// ClearLoginState expires the login-state cookie with attributes matching
// SetLoginState, so a completed or abandoned flow leaves nothing behind.
func ClearLoginState(w http.ResponseWriter, domain string, secure bool) {
	c := &http.Cookie{ // #nosec G124 -- HttpOnly and SameSite are set below; Secure comes from Auth.CookieSecure, which config.Validate requires true in STAGING/PROD
		Name:     EffectiveCookieName(LoginStateCookieName, domain, secure),
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	}
	if !strings.HasPrefix(c.Name, HostPrefix) {
		c.Domain = domain
	}
	http.SetCookie(w, c)
}

// LoginStateDigest is the value stored in the cookie for a given state.
func LoginStateDigest(state string) string {
	sum := sha256.Sum256([]byte(state))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// LoginStateMatches reports whether the request carries a login-state cookie
// for state. It is false when the cookie is absent, which is the point: a
// callback that did not begin in this browser has nothing to present.
//
// The comparison is constant time. The value is a digest of a secret an
// attacker is trying to guess only in the narrow sense, but a timing oracle on
// "how much of my cookie matches" is free to avoid.
func LoginStateMatches(r *http.Request, state, domain string, secure bool) bool {
	if state == "" {
		return false
	}
	c, err := r.Cookie(EffectiveCookieName(LoginStateCookieName, domain, secure))
	if err != nil || c.Value == "" {
		return false
	}
	want := LoginStateDigest(state)
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(want)) == 1
}
