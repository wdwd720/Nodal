// Package withdrawal is the withdrawal boundary (goal PART 94): a separate
// domain from funding even though no fiat off-ramp is enabled in V1. A
// withdrawal moves value off the platform path and therefore requires, in
// this order, a human actor, the withdrawal:create permission on the
// account, recent step-up authentication, the WITHDRAWALS capability gate
// (DISABLED in every environment today: docs/compliance-gates/PRODUCTION_GATES.md,
// external blockers EB-015 and EB-002), no blocking kill switch, an ACTIVE
// account, a validated destination that is not the source wallet, and the
// account's velocity policy. Every request and transition is audited.
//
// No agent path exists, and that is enforced three ways: Service.Request
// takes a HumanActor, a type only HumanFrom can build and HumanFrom refuses
// AGENT (and SERVICE/SYSTEM) principals, so an agent principal cannot even
// be expressed as a caller; Request re-checks the actor type before any
// other work; and the withdrawal_transitions table refuses actor_type AGENT
// at the database. A test in this package parses every Go file under
// internal/agent, internal/strategy, internal/model and internal/prediction
// and fails if any imports this package.
//
// This package must never:
//   - accept a request from, or on behalf of, an AGENT principal, whatever
//     roles or headers accompany it;
//   - skip the WITHDRAWALS gate because the environment is LOCAL or the
//     provider is fake: the gate is consulted unconditionally;
//   - move money: when the gate is one day ACTIVE, settlement goes through
//     the signing and execution packages under a human-approved plan; this
//     package records intent and authorization only;
//   - treat "funds can trade" as "funds can withdraw" (funding's
//     withdrawal_eligible and capital's withdrawal holds are consulted at
//     approval time, never inferred);
//   - accept an INTERNAL_ACCOUNT destination (customer-to-customer transfer
//     is excluded from V1; the enum value is reserved and rejected).
package withdrawal
