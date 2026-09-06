package httpmw

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/security"
)

type sessionKey struct{}

// SessionFrom returns the auth.Session attached by the Session middleware
// (for logout, rotation and session-listing handlers).
func SessionFrom(ctx context.Context) (auth.Session, bool) {
	s, ok := ctx.Value(sessionKey{}).(auth.Session)
	return s, ok
}

// Session loads the session named by cookieName through mgr and attaches
// the resulting security.Principal and auth.Session to the context.
//
//   - no cookie: the request continues anonymously;
//   - malformed, unknown, expired, revoked or idle token: the cookie is
//     cleared and the request continues anonymously;
//   - session store failure: 503 INTERNAL — never anonymous.
//
// q is the query handle for the store (the pool in production; nil with
// the in-memory store). cookieName must be the EffectiveCookieName.
func Session(mgr *auth.Manager, q auth.Querier, cookieName string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := r.Cookie(cookieName)
			if err != nil || c.Value == "" {
				next.ServeHTTP(w, r)
				return
			}
			s, err := mgr.Authenticate(r.Context(), q, c.Value)
			if err != nil {
				if errors.Is(err, security.ErrUnauthenticated) {
					clearByName(w, cookieName)
					next.ServeHTTP(w, r)
					return
				}
				writeProblem(w, r, http.StatusServiceUnavailable, CodeInternal, "session store unavailable")
				return
			}
			p, err := s.Principal()
			if err != nil {
				clearByName(w, cookieName)
				next.ServeHTTP(w, r)
				return
			}
			ctx := security.WithPrincipal(r.Context(), p)
			ctx = context.WithValue(ctx, sessionKey{}, s)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// clearByName expires a cookie knowing only its name. A __Host- cookie is
// fully determined by its name; for other names the host-only, Path=/
// form is written, which matches cookies set without a Domain attribute.
func clearByName(w http.ResponseWriter, name string) {
	secure := strings.HasPrefix(name, HostPrefix)
	ClearSessionCookie(w, name, "", secure)
}
