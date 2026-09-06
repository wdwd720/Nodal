package httpmw

import (
	"net/http"
	"strings"
	"time"
)

// HostPrefix is the cookie-name prefix that makes browsers enforce Secure,
// Path=/ and no Domain attribute, binding the cookie to exactly one host.
const HostPrefix = "__Host-"

// EffectiveCookieName returns the name SetSessionCookie will use: name
// with the __Host- prefix when the cookie is secure and host-only (no
// domain), otherwise name unchanged. Configure the Session middleware
// with this value.
func EffectiveCookieName(name, domain string, secure bool) string {
	if secure && domain == "" && !strings.HasPrefix(name, HostPrefix) {
		return HostPrefix + name
	}
	return name
}

// SetSessionCookie writes the session cookie: HttpOnly always; SameSite=Lax
// (Strict would drop the cookie on the OIDC callback redirect and break
// login); Secure when secure; Path=/; Domain only when given and not
// secure-host-only; Max-Age from ttl when positive (otherwise a browser
// session cookie). The token is written verbatim and never logged.
func SetSessionCookie(w http.ResponseWriter, name, token, domain string, secure bool, ttl time.Duration) {
	c := &http.Cookie{ //nolint:gosec // Secure comes from config; config.Validate forces CookieSecure=true outside LOCAL/TEST
		Name:     EffectiveCookieName(name, domain, secure),
		Value:    token,
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

// ClearSessionCookie expires the session cookie with attributes matching
// SetSessionCookie so the browser actually removes it.
func ClearSessionCookie(w http.ResponseWriter, name, domain string, secure bool) {
	c := &http.Cookie{ //nolint:gosec // Secure comes from config; config.Validate forces CookieSecure=true outside LOCAL/TEST
		Name:     EffectiveCookieName(name, domain, secure),
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	}
	if !strings.HasPrefix(c.Name, HostPrefix) {
		c.Domain = domain
	}
	http.SetCookie(w, c)
}
