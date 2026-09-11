// Package compilersandbox is the strategy compiler of a sandbox tier: it
// assembles a typed Strategy IR document from a strategy the user specified
// FIELD BY FIELD, it calls no model, and it infers nothing.
//
// # Why it exists
//
// ADR-0029 declares the compiler a seam and says a deployment without one
// reports COMPILER_UNAVAILABLE rather than guessing. That is still the truth on
// every tier that has no model provider. But it left goal §17/§18's whole
// journey — describe, compile, review, accept, create an agent, pause, resume,
// disable — unreachable on every deployment that exists, so the review step
// nobody could reach was also the review step nobody could test.
//
// A sandbox tier is where a rehearsal happens (ADR-0023). This compiler makes
// the journey reachable there without weakening the thing the review step
// protects, because it takes no natural language at all.
//
// # The line it does not cross
//
// The `description` a person writes is recorded, rendered back to them, and
// NEVER read by this package. What it compiles is `strategies.constraints`: a
// declared, versioned object (`StructuredStrategy`) whose every field the
// person filled in — the instrument, the venue, the comparator, the threshold,
// the limits, the interval. Each element of the IR is traceable to one of those
// fields, to the registry, or to the risk policy in force, and the rationale
// it returns says which. There is no path by which a sentence becomes
// authority: an unspecified strategy compiles to a refusal that names the
// missing fields, not to a default.
//
// It follows that this compiler cannot produce a "creative" strategy, and that
// is the point. It produces exactly the strategy a person typed, or nothing.
//
// # Three refusals of PROD, like the other sandbox providers
//
//  1. New refuses config.EnvProd outright, so a production binary that somehow
//     reached the constructor gets an error rather than a compiler.
//  2. cmd/api constructs it only when cfg.SandboxTier(), which is
//     CP_API_LEGAL_POLICY=SANDBOX — a value config.Validate already refuses in
//     PROD (RULE SANDBOX_TIER_NOT_IN_PROD). There is no separate switch to
//     forget, and no new environment variable.
//  3. Every version it produces carries lineage source STRUCTURED_SANDBOX and
//     the `sandbox` flag, and migration 00812's CHECK refuses that pair in any
//     PROD database.
//
// # What it is not
//
// It is not a test double. Production wiring may not import one (scripts/lintfin),
// and a compiler that refuses to exist in PROD has to say so somewhere PROD
// compiles — the same two reasons payoutsandbox and verifysandbox are first
// class providers. It is also not an approval: it returns a COMPILED version
// and nothing else, because ACCEPTED is the record of a person reading the
// document, and this package has no way to make one.
package compilersandbox
