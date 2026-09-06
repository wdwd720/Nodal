// Package workflows holds the Temporal workflow and activity definitions of
// the control plane (goal PARTS 51, 115, 116, 163; RECONCILIATION.md).
// cmd/workflow-worker is a thin composition root that registers what is
// defined here; every unit of durable orchestration lives in this package so
// it can be exercised with the Temporal test framework and replayed.
//
// # Responsibilities
//
//   - FundingWorkflow drives one deposit through the PART 28 state machine by
//     repeatedly calling internal/funding's Lifecycle driver, waking early on
//     a provider signal and escalating to an operator when a deposit sits in
//     a non-terminal state past its review deadline.
//   - ReconciliationEscalationWorkflow carries one reconciliation record
//     through a fixed ladder of notification tiers, marks it INVESTIGATING and
//     then ESCALATED, and — only for a material record that blocks new risk —
//     asks for containment, which halts new trading and never anything else.
//   - The activities are the only place I/O happens. Each one is a small,
//     idempotent step against Postgres or a notifier, so a Temporal retry of
//     an activity that already completed externally is safe (PART 116).
//
// # Determinism
//
// Workflow functions are replayed from history, so identical code must
// produce identical commands. Inside a workflow function this package never:
// reads the wall clock (workflow.Now only), uses randomness, iterates a map,
// performs I/O of any kind, starts a goroutine outside workflow.Go, or reads
// package-level mutable state. Every decision is driven by the workflow's
// input, its activity results and its signals, all of which the history
// replays exactly. determinism_test.go enforces the mechanical half of that
// by parsing the workflow source, and replay_test.go enforces the rest by
// replaying recorded histories.
//
// # What this package must never do
//
//   - Treat workflow state as financial truth. Postgres records balances,
//     deposits, reservations and reconciliation records; a workflow only
//     decides what to do next and when (PART 116).
//   - Move money, sign anything, or call a provider directly from workflow
//     code. Those belong to internal/funding, internal/signing and the
//     execution path, reached only through activities.
//   - Halt reconciliation. Containment raised by an escalation stops NEW RISK
//     only; the workflow keeps polling the record afterwards, because a
//     system that goes blind during an incident is worse than one that keeps
//     trading (PART 52).
//   - Assume an activity ran at most once. Temporal retries at least once, so
//     every activity must be idempotent and is written that way.
package workflows
