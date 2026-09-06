// Package eligibility is the deterministic eligibility engine (goal PARTS 56,
// 57; POLICY_AUTHORITY §3). It answers one question: may this identity, in
// this jurisdiction, with this account, perform this kind of action on this
// product through this venue and provider right now?
//
// # Responsibilities
//
//   - Policy is a versioned, typed rules document (eligibility_policies.rules)
//     parsed with ParsePolicy. Unknown keys, malformed values and unknown enum
//     members are rejected: an operator cannot accidentally ship a rule the
//     engine does not understand.
//   - Evaluate(policy, input) is a pure function. It returns every failing
//     dimension as a sorted, de-duplicated list of reason codes (never just
//     the first), the policy version, the evaluation time taken from the
//     input, a hash of the evaluated context and a hash of the decision.
//   - Store persists policies and decisions in the caller's transaction and
//     loads the identity/account part of an Input from compliance_profiles
//     and accounts.
//
// # Fail closed
//
// A missing policy yields ELIGIBILITY_POLICY_MISSING; an unknown jurisdiction
// yields ELIGIBILITY_JURISDICTION_UNKNOWN; an unknown identity, sanctions or
// account state, a context the policy has no rule for, or an incomplete
// input yields ELIGIBILITY_POLICY_UNKNOWN. Allowlists are explicit: an empty
// allowlist allows nothing. DefaultPolicyJSON ships with an empty country
// allowlist, so a deployment that has not recorded an operator policy makes
// everyone ineligible.
//
// # What this package must never do
//
//   - Call a model, an LLM, or any network service to decide eligibility.
//   - Read a clock: EvaluatedAt is Input.Now, supplied by the caller.
//   - Depend on map iteration order, floating point or randomness; every list
//     in the output is sorted and hashed canonically.
//   - Hardcode a jurisdiction assumption such as "all U.S. users can trade".
//   - Let an AGENT actor record a policy (application check plus the
//     created_by_actor_type CHECK constraint).
//   - Import capital, killswitch, reconciliation or any agent-facing package;
//     it consumes typed input snapshots only.
package eligibility
