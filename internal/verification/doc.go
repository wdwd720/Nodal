// Package verification is the financial verification state machine (goal §20),
// the age/jurisdiction/sanctions gates behind it (§21), and the evidence that
// justifies a verification level (§24).
//
// # What it is for, and what it is deliberately not for
//
// It answers one question: how thoroughly has the person behind this account
// been identified, and by whom. It never answers "may this money leave" — the
// value-domain policy, the capability gates, the legal router and the payout
// engine answer that, each independently.
//
// The distinction is the product's most important one. From
// PROVIDER_BOUNDARY.md §2:
//
//	Verification never mutates Credits. A Role B decision changes the
//	customer's financial profile; it does not touch a lot, a balance or a
//	value domain.
//
// So this package imports internal/credit, internal/ledger and
// internal/valuedomain's isolation rules NOT AT ALL. It reads and writes
// compliance_profiles, verification_sessions and verification_checks, and it
// reports a valuedomain.VerificationLevel. Nothing else. A future reader
// looking for the place where "verified" turns Credits into cash will not find
// one, because there is not one.
//
// # Provider-hosted, and what that means for what is stored
//
// §20 says: prefer provider-hosted KYC; the provider owns the SSN, the document
// capture, the AML and sanctions screening; Nodal stores a provider identifier,
// a state, timestamps, safe reason codes and eligibility metadata. This package
// stores exactly that list and refuses the rest. There is no document column,
// no date of birth, no government identifier, and the hosted URL a provider
// returns is handed to the browser that asked for it and never written down —
// those links are single-use credentials for resuming somebody's identity
// check.
//
// # Levels are earned from evidence, not asserted
//
// A profile in state VERIFIED is not sufficient to report PAYOUT_KYC. The
// resolver also requires the four sub-checks §21 names — identity document,
// age, jurisdiction, sanctions — to have PASSED, with PEP either passed or not
// applicable. A state without evidence is somebody's assertion; a state with
// evidence is a decision that can be shown to a regulator. Fail closed: missing
// evidence reports the lower level.
//
// # The sandbox rule
//
// A sandbox outcome (ADR-0023) exists so a non-production deployment can drive
// the whole journey without fabricating an approval. Three properties hold
// everywhere:
//
//  1. It can only be written when the deployment declared itself a sandbox
//     tier, and the row carries `sandbox = true` and its environment; a CHECK
//     refuses that combination in PROD (migration 00762).
//  2. It is labelled sandbox in every response that mentions it.
//  3. There is no default outcome. A sandbox session that nobody has answered
//     stays PENDING_USER_ACTION forever. "Approved unless told otherwise" is
//     the exact shape of a fabricated approval.
//
// # What this package must never do
//
//   - Write a Credit lot, a balance, a ledger row or a value domain.
//   - Report a level higher than the evidence supports.
//   - Store a document, an SSN, a full date of birth or a hosted session URL.
//   - Accept a verification result from a USER or AGENT actor: a customer
//     never attests their own compliance state.
//   - Infer a jurisdiction from an IP address. That is a legal determination
//     wearing a network header's clothes.
package verification
