// Package devidp is a local-only auth.IdentityProvider that returns fixed
// identities (one per standing role, two customers) without any real
// authentication. It exists so LOCAL, TEST and DEV environments can
// exercise login, sessions, step-up and RBAC without an external issuer.
//
// The guard is in the constructor and nowhere else: New(env, cfg) returns
// auth.ErrDevIdPNotAllowed unless env is exactly "LOCAL", "TEST" or "DEV"
// (case-sensitive, no trimming — fail closed on anything unexpected). The
// package has no init function and registers nothing at import time, so
// importing it cannot make it active; the only path to a Provider is New,
// and config validation additionally refuses debug auth in PROD.
//
// Codes are "<identity>" or "<identity>:mfa"; the latter yields a strong
// amr so step-up flows can be tested. Roles and accounts ride along in
// Identity.Claims ("dev_roles", "dev_accounts") purely for the dev login
// handler to map; a real provider never dictates roles.
//
// This package must never:
//
//   - construct successfully in STAGING or PROD, or for an unknown env;
//   - carry an init function or package-level registration;
//   - be reachable from a production binary's wiring without the env gate;
//   - be mistaken for authentication: it verifies nothing.
package devidp
