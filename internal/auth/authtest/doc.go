// Package authtest holds test doubles for the auth packages: an in-memory
// SessionStore, a controllable clock, and an httptest-backed OpenID
// Connect server that serves discovery, a JWKS with a key generated at test
// time, an authorization endpoint and a token endpoint issuing signed ID
// tokens with caller-chosen claims (so negative cases — wrong audience, bad
// nonce, expired, tampered signature, alg=none, unknown kid, an arbitrary
// raw id_token, no id_token at all — are one Grant field away).
//
// This package must never:
//
//   - be imported by production wiring: MemorySessionStore is not durable,
//     not shared between processes and rejected by config validation in
//     PROD;
//   - be used as an identity provider outside tests — the fake server
//     signs whatever it is told to sign.
package authtest
