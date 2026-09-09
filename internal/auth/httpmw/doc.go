// Package httpmw provides chi-compatible middleware
// (func(http.Handler) http.Handler) for session loading, authentication
// and role/permission guards, CSRF protection and secure response headers,
// plus the session cookie helpers (PART 192).
//
//   - Session loads the session named by the cookie and attaches a
//     security.Principal (and the auth.Session) to the request context. No
//     cookie, or a cookie that is malformed/unknown/expired/revoked/idle,
//     yields an anonymous request (the stale cookie is cleared) — not an
//     error, so public routes keep working. A session-store outage is a
//     503: an outage must never demote requests to anonymous silently.
//   - RequireAuth, RequireRole, RequirePermission and RequireStepUp turn
//     the security package's decisions into 401/403 problem+json responses.
//     THE API DOES NOT MOUNT THEM. Authorization for every route the server
//     serves is decided in internal/httpapi/authz.go, per operation, from the
//     generated operation id -- which is the correct place for it, because a
//     route added to the OpenAPI document cannot then be left unguarded by
//     forgetting to wrap it. These six are reachable from this package's own
//     tests and from nowhere else (F-76). They are kept because they are the
//     right shape for a second server, and left documented as unused because a
//     guard that looks live is one somebody will mount beside the real
//     enforcement layer and end up with two authorization paths that disagree.
//   - CSRF rejects unsafe methods (POST/PUT/PATCH/DELETE and anything not
//     GET/HEAD/OPTIONS/TRACE) unless the browser proves the request is
//     first-party: Sec-Fetch-Site ∈ {same-origin, none}, or Origin exactly
//     matches an allow-listed origin. For browsers that send neither
//     header, the legacy fallback accepts X-Requested-With: XMLHttpRequest
//     together with an allow-listed Referer origin. The request's own Host
//     header is never used to derive "same origin" — the allow-list is the
//     authority.
//   - SecureHeaders sets X-Content-Type-Options, Referrer-Policy,
//     Permissions-Policy, Content-Security-Policy (frame-ancestors 'none'),
//     X-Frame-Options, Cache-Control: no-store by default, and HSTS when
//     serving over TLS.
//   - SetSessionCookie / ClearSessionCookie write the session cookie
//     HttpOnly, SameSite=Lax (Strict would drop the cookie on the OIDC
//     callback redirect), Secure when secure, and with the __Host- prefix
//     when secure and host-only (EffectiveCookieName).
//
// Rate limiting of authentication endpoints (RateLimitAuth) is out of
// scope for this package: it belongs with the shared rate limiter backed
// by Redis and is tracked there.
//
// Error bodies are application/problem+json with the stable codes
// UNAUTHENTICATED, FORBIDDEN, STEP_UP_REQUIRED and INTERNAL written by a
// local writer; the API layer's errs.ToProblem is the eventual single
// source of that format.
//
// This package must never:
//
//   - derive a principal from any request header, query parameter or body
//     (Authorization, X-User-Id, X-Roles and the like are ignored);
//   - log or echo the session cookie value;
//   - treat a session-store error as an anonymous request;
//   - accept an unsafe method on the word of a Referer alone, or on a
//     Host-derived origin;
//   - set the session cookie without HttpOnly, or without Secure when the
//     deployment is secure.
package httpmw
