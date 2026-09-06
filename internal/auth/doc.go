// Package auth is the authentication foundation (PART 90, PART 192): the
// IdentityProvider abstraction (OIDC authorization-code + PKCE, decision
// D-006), opaque server-side sessions, and the Manager that issues,
// validates, rotates and revokes them and turns a session into a
// security.Principal.
//
// Sessions are opaque: the browser holds a 32-byte random token
// (base64url, 43 characters) and the store is keyed by hex(sha256(token)).
// Nothing in this package persists, logs or returns the raw token except
// Issued.Token on the way to the cookie. Validity is decided by the Manager
// from four facts: revoked, absolute expiry, idle timeout, and structural
// validity of the stored row. Rotation on privilege change (login,
// step-up, role change, break-glass elevation) revokes the old session and
// issues a new token in the same transaction, preserving the absolute
// expiry so rotation cannot extend a session's lifetime.
//
// SessionStore takes pgx transaction/query handles so session rows are
// written in the same transaction as the audit record of the login (the
// Postgres implementation is a separate pass; authtest.MemorySessionStore is
// the in-memory one used by tests and never wired in PROD).
//
// This package must never:
//
//   - log, persist or compare raw session tokens (hash first; compare with
//     constant-time equality where a raw token is ever compared);
//   - accept a token that is revoked, expired, idle or malformed;
//   - extend a session's absolute expiry through rotation or touch;
//   - build a Principal with roles for an AGENT session;
//   - implement password hashing or its own federation protocol — identity
//     is delegated to the IdentityProvider;
//   - allow the dev identity provider outside LOCAL/TEST/DEV
//     (ErrDevIdPNotAllowed).
package auth
