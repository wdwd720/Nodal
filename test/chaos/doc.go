// Package chaos holds fault-injection tests that break real dependencies —
// the Postgres, Redpanda and MinIO containers of the local stack — and assert
// that the system stays *correct*, not merely alive (STAGE 18).
//
// Every test in this package asserts one of three invariants, and nothing
// else is interesting:
//
//   - no duplicate financial effect: a fault never multiplies a posting, a
//     reservation, a fill or a position change;
//   - no lost financial event: an outbox row is marked published only when
//     the bus actually accepted it, so a fault costs a retry, never an event;
//   - no silent divergence: after the fault the recorded state either matches
//     what happened or is explicitly marked unknown. Never a confident wrong
//     answer.
//
// Rules this package holds itself to:
//
//   - Shared infrastructure is restored. Every test that pauses a container
//     registers an idempotent unpause in t.Cleanup and waits for the
//     container to report healthy again before returning.
//   - The database is provisioned per run by `go run ./scripts/testdb -name
//     chaos`; the shared controlplane_test is never touched.
//   - Every guard carries a negative control. `chaosbreak` (see break.go)
//     names the guards a run may deliberately disable so the suite can be
//     observed failing; a guard that has never been seen to fail is recorded
//     as unproven in docs/build/ADVERSARIAL_VALIDATION.md.
//
// Build tags: integration,chaos (`make chaos`).
package chaos
