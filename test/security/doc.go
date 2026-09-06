// Package security holds repository-wide security invariants (PART 155) that
// cut across packages: authority-boundary import rules, production refusal of
// fake providers, and the closed agent permission set. Package-level tests
// live next to their packages; this suite proves the rules that no single
// package can prove about the others.
//
// API-level tests (IDOR, cross-tenant HTTP reads/writes, CSRF, SSRF, webhook
// forgery, replay, JWT/cookie tampering) join this package once cmd/api exists.
package security
