// Package oidc implements auth.IdentityProvider for any OpenID Connect
// provider using the authorization-code flow with PKCE (S256), nonce
// binding and step-up requests (PART 90, decisions D-006 and D-014).
//
// New runs discovery through github.com/coreos/go-oidc (oidc.NewProvider)
// and refuses an issuer mismatch. AuthCodeURL adds nonce,
// code_challenge(+method=S256) and, for step-up, prompt=login, max_age=0 and
// acr_values. Exchange redeems the code with the PKCE verifier through
// golang.org/x/oauth2, then hands the ID token to go-oidc's
// IDTokenVerifier, which parses the JWS against the algorithm allow-list
// (never "none", never HMAC), verifies the signature against the issuer's
// JWKS, and checks iss, aud and exp/nbf. Explicit checks that go-oidc leaves
// to the caller follow: azp for multi-audience tokens, nbf/iat against the
// configured clock skew, nonce in constant time, a non-empty sub, and the
// amr/acr/auth_time claims that feed security.RequireStepUp. ExchangeStepUp
// additionally requires a strong amr (security.StrongAMR).
//
// The JWKS is served to go-oidc through this package's own oidc.KeySet
// (keyset.go, built on go-jose) rather than oidc.RemoteKeySet, so that an
// unknown kid triggers at most one refetch per JWKSMinRefresh, keys with
// use=enc, a foreign alg or fewer than 2048 RSA bits are ignored, and
// "unknown key" is distinguishable from "bad signature" in logs. Every
// response body read from the issuer (discovery, JWKS, token endpoint) is
// capped at MaxDocumentBytes and the HTTP client carries a timeout.
//
// This package must never:
//
//   - accept an ID token without verifying its signature against the
//     issuer's JWKS, or accept alg=none / HS* algorithms;
//   - trust iss, aud, nonce, exp or sub before signature verification;
//   - skip the nonce check, or compare nonces non-constant-time;
//   - use oidc.InsecureIssuerURLContext or the Skip*/Insecure* fields of
//     oidc.Config: the configured issuer must be the one discovery
//     advertises and the one that signs tokens;
//   - talk to an issuer or endpoint over plain HTTP outside loopback
//     unless explicitly configured (tests only);
//   - log the authorization code, code verifier, client secret, access
//     token or raw ID token;
//   - assign roles: the identity provider asserts who, never what they
//     may do.
package oidc
