// Package compliance stores the per-user compliance profile the eligibility
// engine consumes (PART 56, 57, 121): identity verification state, age
// verification, jurisdiction and residency, sanctions state, provider
// references, and account-level restrictions.
//
// The profile is written only by non-agent actors — identity-provider
// webhooks (SYSTEM), operators, or compliance staff — and every change is
// recorded as an audit event. Raw PII stays in identity_pii under separate
// encryption; this package handles states and codes only.
//
// # What this package owns, and what internal/verification owns
//
// It owns the ATTRIBUTE half of the profile: age verified, jurisdiction,
// residency, sanctions state, provider references, policy version and
// restrictions.
//
// It does NOT own `identity_state`, `verified_at` or `expires_at`. Those are
// the financial verification state machine of goal §20, and migration 00761
// made a transition row the only way any of them moves: a profile is born
// UNVERIFIED, the application role has no UPDATE privilege on the three
// columns, and `internal/verification` records the edges. `Upsert` therefore
// ignores those fields on the way in and returns whatever the row actually
// says on the way out.
//
// This package must never: decide eligibility itself (that is the
// eligibility engine's pure function), accept AGENT actors, store
// documents, or write a verification state.
package compliance
