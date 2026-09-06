// Package e2e drives the real `cmd/api` binary over real HTTP against a real
// PostgreSQL database, across process boundaries (STAGE 18).
//
// Nothing here is in-process. Every test builds cmd/api, starts it with
// os/exec on a port taken from a 127.0.0.1:0 probe, waits for GET /v1/healthz,
// and then speaks HTTP to it exactly as a browser or an SDK would: real
// cookies, real CSRF headers, real Idempotency-Key headers, real
// application/problem+json bodies. The in-process fakes of
// internal/httpapi/harness_test.go answer a different question — whether the
// handlers behave — and cannot answer this one, which is whether the shipped
// binary, its configuration, its middleware order, its session store and its
// database agree.
//
// What this package asserts, and nothing else is interesting:
//
//   - the customer path produces the right *state*, not merely the right
//     status code: seeded capital is visible as buying power to the cent, the
//     seed posting is visible in the journal, and a revoked session is
//     actually refused;
//   - a command executes exactly once per Idempotency-Key even when the
//     callers are concurrent, verified by counting rows in the database
//     rather than by trusting the API's own answer;
//   - every refusal is a problem+json document with a stable code and leaks
//     no internals — no DSN, no driver name, no SQL, no stack frame, no
//     table name;
//   - the SSE endpoint really streams to a client that reads incrementally,
//     with the headers such a client can observe.
//
// Rules this package holds itself to:
//
//   - It provisions its OWN database (`go run ./scripts/testdb -name e2e
//     -export`) and refuses to run against the shared controlplane_test or
//     controlplane. TestMain fails loudly rather than skipping if pointed at
//     either.
//   - It must pass twice in a row against ONE database with nothing cleaned
//     in between. Every fixture a test creates is namespaced by a per-run
//     token, and no assertion depends on a table being empty.
//   - Every guard has a named negative control (CP_E2E_BREAK, see
//     break_test.go). A test that cannot fail is worse than no test.
//   - It never kills a process it did not start, and never touches a port it
//     did not take from the kernel itself.
package e2e
