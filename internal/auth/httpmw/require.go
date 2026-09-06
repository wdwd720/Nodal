package httpmw

import (
	"errors"
	"net/http"
	"time"

	"github.com/nodal/controlplane/internal/security"
)

func wallClock() time.Time { return time.Now().UTC() }

// RequireAuth rejects anonymous requests with 401 UNAUTHENTICATED.
func RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := security.PrincipalFrom(r.Context()); !ok {
			writeProblem(w, r, http.StatusUnauthorized, CodeUnauthenticated, "authentication required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireRole allows the request when the principal holds at least one of
// roles: 401 when anonymous, 403 otherwise. Agents never hold roles and
// are always refused. RoleBreakGlass counts only while the elevation is
// live (wall clock; see RequireRoleAt). Prefer RequirePermission for
// authorization decisions — roles are for coarse routing.
func RequireRole(roles ...security.Role) func(http.Handler) http.Handler {
	return RequireRoleAt(wallClock, roles...)
}

// RequireRoleAt is RequireRole with an injected clock.
func RequireRoleAt(now func() time.Time, roles ...security.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := security.PrincipalFrom(r.Context())
			if !ok {
				writeProblem(w, r, http.StatusUnauthorized, CodeUnauthenticated, "authentication required")
				return
			}
			if p.Validate() != nil || p.IsAgent() || !holdsAny(p, roles, now()) {
				writeProblem(w, r, http.StatusForbidden, CodeForbidden, "insufficient role")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func holdsAny(p security.Principal, roles []security.Role, now time.Time) bool {
	for _, role := range roles {
		if !p.HasRole(role) {
			continue
		}
		if role == security.RoleBreakGlass && !p.BreakGlassActive(now) {
			continue
		}
		return true
	}
	return false
}

// RequirePermission enforces security.Require: 401 when anonymous, 403
// otherwise. Tenant scoping (security.RequireAccount) stays in handlers,
// where the account id is known.
func RequirePermission(perm security.Permission) func(http.Handler) http.Handler {
	return RequirePermissionAt(perm, wallClock)
}

// RequirePermissionAt is RequirePermission with an injected clock.
func RequirePermissionAt(perm security.Permission, now func() time.Time) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := security.RequireAt(r.Context(), perm, now); err != nil {
				writeSecurityError(w, r, err)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireStepUp enforces security.RequireStepUp with maxAge: 401 when
// anonymous, 403 STEP_UP_REQUIRED when the authentication is too old or
// not strong. The client should start a step-up login and retry.
func RequireStepUp(maxAge time.Duration, now func() time.Time) func(http.Handler) http.Handler {
	if now == nil {
		now = wallClock
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := security.RequireStepUp(r.Context(), maxAge, now); err != nil {
				writeSecurityError(w, r, err)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// writeSecurityError maps the security sentinels onto problem responses.
func writeSecurityError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, security.ErrUnauthenticated):
		writeProblem(w, r, http.StatusUnauthorized, CodeUnauthenticated, "authentication required")
	case errors.Is(err, security.ErrStepUpRequired):
		writeProblem(w, r, http.StatusForbidden, CodeStepUpRequired, "recent strong authentication required")
	default:
		writeProblem(w, r, http.StatusForbidden, CodeForbidden, "insufficient permission")
	}
}
