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
// This package must never: decide eligibility itself (that is the
// eligibility engine's pure function), accept AGENT actors, or store
// documents.
package compliance
