# ADR-0002: Go for the financial core

## Status

Accepted — implementation tracked in docs/build/REQUIREMENTS_TRACEABILITY.md

## Date

2026-09-05

## Context

The core services (ledger, capital, risk, settlement compilation, execution,
reconciliation, signing boundary, audit) must be predictable under concurrency,
deterministic in arithmetic, and easy to audit line by line. The failure modes that
drive the language choice:

- Floating-point money by default. Languages whose numeric literal is a double make
  `0.1 + 0.2` errors the path of least resistance; exact minor-unit and big-integer
  arithmetic must be the only convenient option (PART 17).
- Data races in capital paths. A reservation service that is racy under load is a
  double-spend generator. A first-class race detector that runs in CI is required
  (PART 216).
- Hidden nondeterminism: dynamic dispatch, monkey patching, and implicit global state
  make "same inputs, same decision" unprovable (PART 224).
- Dependency sprawl: large transitive dependency trees widen the supply-chain attack
  surface for a process that constructs transactions (PART 104).
- Build portability: the primary development host lacks a C toolchain (BLOCKERS
  SB-005); a core that requires cgo would fork the build across platforms.
- Research code reaching production: Python is required for offline research but
  must not control money (PART 12).

## Decision

- The core is written in Go (1.27 at the time of writing), one module, cgo-free,
  formatted with `gofmt`, linted with `go vet`, `golangci-lint`, `staticcheck`, and
  scanned with `govulncheck` and `gosec` in CI.
- Money types are the exact types in CONVENTIONS (`money.USD`, `money.BPS`,
  `money.Quantity`, `money.Price`); floating point is forbidden in financial paths.
- `-race` runs on `internal/capital`, `internal/ledger`, `internal/execution`,
  `internal/reconciliation`, and `internal/event` in CI (D-012).
- Native Go fuzzing is used for every parser of external input (PART 151, 152).
- Python is confined to research and offline analysis; it has no credentials to
  production money APIs. TypeScript is used for the web app and the strategy SDK,
  which emits Strategy IR only (ADR-0010) and never executes money operations.
- Operational scripts are Go programs, not shell (D-013), so behaviour is identical
  on Windows, macOS, Linux, and CI.

### Explicitly not decided / deferred

- Whether a future performance-critical component (for example a transaction
  inspector for very high throughput) could be a separately audited library in
  another language. Not in V1; no process boundary is reserved for it.
- Go version upgrade cadence.

## Consequences

### Positive

- Static binaries, fast startup on ECS/Fargate, small images (PART 103).
- Mature first-party clients for the chosen infrastructure: pgx, franz-go, the
  Temporal Go SDK, OpenTelemetry.
- Race detector, fuzzing, and reproducible builds are standard toolchain features.

### Negative

- No native arbitrary-precision decimal type; the `money` package must be written
  and tested carefully rather than imported.
- Verbose error handling; mitigated by the `errs` package conventions.
- Generics are limited compared with some alternatives; typed IDs and money types
  must be designed within those limits.

### Operational

- Toolchain pinning via `GOTOOLCHAIN=local` and a checked-in version; CI fails on
  drift (PART 214).
- SBOM and provenance generated per release (PART 143).

## Alternatives considered

- Rust: strong guarantees, but PART 13 lists a Rust financial backend as premature.
  The dominant risks here are logic and boundary errors, not memory safety; the
  client ecosystem for Temporal and Redpanda is less mature; and build and hiring
  costs are higher for no V1 benefit.
- TypeScript/Node core: doubles by default, single-threaded event loop for
  CPU-bound compilation and inspection, larger dependency trees.
- Python core: excluded by PART 12 from controlling production money.
- JVM (Kotlin/Java): viable, but heavier runtime, GC tuning, slower cold start, and
  the team's tooling for cgo-free static builds is weaker.
- Elixir/Erlang: good concurrency model, but weaker static typing for money types
  and a thin ecosystem for the chosen infrastructure.

## Related

- ADR-0003, ADR-0010, ADR-0011, ADR-0013, ADR-0014.
- Goal PART 12, 13, 14, 17, 104, 143, 151, 152, 214, 215, 216, 224.
- DECISION_REGISTER D-001, D-002, D-004, D-007, D-012, D-013; BLOCKERS SB-001, SB-005.

## Evidence required before this ADR's guarantees can be claimed VERIFIED

- CI report showing `go vet`, `golangci-lint`, `staticcheck`, `govulncheck`, and
  `gosec` clean on the main branch.
- CI report showing `go test -race` passing on the packages listed above.
- A `test/security` lint test that fails if `float32`/`float64` appear in
  `internal/money`, `internal/ledger`, `internal/capital`, `internal/risk`, or
  `internal/execution` outside explicitly annotated display helpers.
- Fuzz targets (`Fuzz*`) with committed corpora for every external-input parser.
- A build-reproducibility check and SBOM attached to each release (PART 143).
- A test proving no production binary links cgo (`go version -m` inspection in CI).
