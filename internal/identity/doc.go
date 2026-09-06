// Package identity is the login flow (PART 90, 192): it drives the OIDC
// authorization-code + PKCE exchange through auth.IdentityProvider, maps the
// provider subject to a platform user (creating the user and their first
// CUSTOMER account on first login), decides roles from the operator directory
// (never from provider claims), and issues a server-side session.
//
// Login state (state, nonce, PKCE verifier) is persisted in login_attempts
// and consumed exactly once, so a replayed or forged callback never yields a
// session. Every login and logout is a security event and an audit event.
//
// This package must never: accept a session without a verified ID token,
// take roles from identity-provider claims, or let an AGENT obtain a session.
package identity
