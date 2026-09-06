package httpmw

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// CSRFOptions configures CSRF. AllowedOrigins are absolute origins
// ("https://app.example.com", "http://localhost:3000") compared exactly
// after normalisation of scheme and host case; the application's own
// public origin must be listed. An empty list means only requests the
// browser marks Sec-Fetch-Site: same-origin/none are accepted.
type CSRFOptions struct {
	AllowedOrigins []string
}

// Validate reports malformed allow-list entries.
func (o CSRFOptions) Validate() error {
	for _, raw := range o.AllowedOrigins {
		if _, err := normalizeOrigin(raw); err != nil {
			return fmt.Errorf("httpmw: csrf allowed origin %q: %w", raw, err)
		}
	}
	return nil
}

// CSRF rejects state-changing requests that the browser does not prove to
// be first-party. Decision order for an unsafe method:
//
//  1. Sec-Fetch-Site is "same-origin" or "none" (typed URL, bookmark) —
//     allow.
//  2. Origin header present — allow only if it is in AllowedOrigins.
//     "null" is never allowed.
//  3. Sec-Fetch-Site present with another value (cross-site, same-site)
//     and no acceptable Origin — reject.
//  4. Neither header present (legacy browsers): allow only if
//     X-Requested-With: XMLHttpRequest is set AND the Referer's origin is
//     in AllowedOrigins. Otherwise reject.
//
// Safe methods (GET, HEAD, OPTIONS, TRACE) pass through untouched; they
// must not change state. The request Host header is never consulted.
// Malformed allow-list entries are dropped (fewer origins, fail closed);
// use CSRFOptions.Validate at configuration time to catch them.
func CSRF(opts CSRFOptions) func(http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, raw := range opts.AllowedOrigins {
		if o, err := normalizeOrigin(raw); err == nil {
			allowed[o] = true
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isSafeMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			if csrfAllowed(r, allowed) {
				next.ServeHTTP(w, r)
				return
			}
			writeProblem(w, r, http.StatusForbidden, CodeForbidden, "cross-site request refused")
		})
	}
}

func isSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}

func csrfAllowed(r *http.Request, allowed map[string]bool) bool {
	switch sfs := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); sfs {
	case "same-origin", "none":
		return true
	default:
		if origin := r.Header.Get("Origin"); origin != "" {
			o, err := normalizeOrigin(origin)
			return err == nil && allowed[o]
		}
		if sfs != "" {
			return false
		}
	}
	// Legacy fallback: no Sec-Fetch-Site and no Origin.
	if !strings.EqualFold(r.Header.Get("X-Requested-With"), "XMLHttpRequest") {
		return false
	}
	ref := r.Referer()
	if ref == "" {
		return false
	}
	o, err := normalizeOrigin(ref)
	return err == nil && allowed[o]
}

// normalizeOrigin reduces an absolute URL or origin to "scheme://host[:port]"
// in lower case. Anything without an http(s) scheme and host is an error.
func normalizeOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("origin must be http or https")
	}
	host := strings.ToLower(u.Host)
	if host == "" || u.User != nil {
		return "", errors.New("origin must have a host and no credentials")
	}
	return scheme + "://" + host, nil
}
