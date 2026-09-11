// Package security holds repository-wide security invariants (PART 155) that
// cut across packages: authority-boundary import rules, production refusal of
// fake providers, and the closed agent permission set. Package-level tests
// live next to their packages; this suite proves the rules that no single
// package can prove about the others.
//
// The API-level tests joined when cmd/api landed: IDOR and cross-tenant HTTP
// reads and writes over every account-scoped route the router declares, replay
// of authorization codes, state and session cookies, idempotency-key abuse,
// SQL-source and injection scans, the PII and value-domain boundaries, and
// dual control over the admin surface. Still not here, and named as planned in
// SECURITY.md §13: CSRF end-to-end against the mounted router, SSRF through the
// ToolBroker egress allow-list, webhook forgery at the route, and a dependency
// compromise simulation.
package security
