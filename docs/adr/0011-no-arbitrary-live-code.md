# ADR-0011: No arbitrary user code in the live path

## Status

Accepted — implementation tracked in docs/build/REQUIREMENTS_TRACEABILITY.md

## Date

2026-09-05

## Context

Users and developers will ask to run their own Python, Node.js, or Rust strategies
with their own dependencies. Goal PART 4 excludes this from V1 and PART 13 lists
untrusted code execution as premature. The failure modes are severe and specific:

- Sandbox escape: any in-process or container-level sandbox that shares a host with
  a process holding secrets is one kernel or runtime bug away from key exfiltration.
- Dependency supply chain: `pip install` or `npm install` inside a strategy pulls
  arbitrary code from public registries at run time; typosquatting and
  maintainer-account compromise are routine (PART 104).
- Egress exfiltration: a strategy with network access can send wallet intelligence,
  positions, or credentials anywhere; a strategy with inbound access is a foothold.
- Resource exhaustion: unbounded CPU, memory, or wall-clock consumption starves the
  execution and reconciliation workers on the same infrastructure.
- Nondeterminism: user code with clocks, randomness, and I/O cannot be replayed,
  so its decisions cannot be audited or backtested (PART 224).
- Unprovable effect bounds: the platform cannot show a regulator or a customer what
  a strategy is incapable of doing (PART 63, BLOCKERS EB-011).

## Decision

- No arbitrary user code executes in any live path. Strategies exist only as typed
  Strategy IR (ADR-0010) evaluated by the platform's own evaluator.
- No package-manager dependency execution on behalf of a strategy; no user-supplied
  binaries, scripts, or WASM modules in V1.
- Strategies reach external data only through the tool broker (PART 66) with an
  allowlisted set of read tools; they have no socket access, no file access, and no
  ability to name an arbitrary URL.
- Python is confined to research and offline analysis with no production money
  credentials (PART 12). The TypeScript SDK runs at authoring time and emits IR; it
  does not run on the platform's servers.
- Interfaces are kept compatible with a future sandboxed executor per PART 232: the
  evaluator consumes IR through a boundary that a microVM-hosted executor could also
  satisfy (no secrets, no wallet access, restricted egress, resource quotas,
  capability proxy). This ADR reserves that shape; it does not build it.

### Explicitly not decided / deferred

- Whether and when arbitrary code is offered at all.
- Sandbox technology if it is ever offered (Firecracker per PART 232, WASM with
  capability imports, or other). No V1 complexity budget is spent here.
- The tool-broker allowlist contents for V1.

## Consequences

### Positive

- The strategy runtime has no interpreter to escape from.
- Effect bounds and reproducibility follow from the IR, not from sandbox strength.
- Supply-chain exposure is limited to the platform's own pinned dependencies.

### Negative

- Some sophisticated users will not be served in V1.
- The IR vocabulary must grow deliberately to cover legitimate needs.

### Operational

- Support must be able to explain the limitation honestly (PART 112).

## Alternatives considered

- Docker containers per strategy: shared kernel; weak isolation for a host that
  also runs money workers. Rejected.
- gVisor or similar user-space kernels: better, but still on the same infrastructure
  and still arbitrary code with arbitrary dependencies.
- Firecracker microVMs now: the PART 232 target shape, but PART 13 excludes spending
  V1 complexity on it.
- WASM with capability-based imports: promising for a later phase; still arbitrary
  code whose bounds depend on the host, and no determinism guarantee.
- "Trusted developer" allowlist for raw code: no technical enforcement; rejected.

## Related

- ADR-0002, ADR-0010, ADR-0012, ADR-0013.
- Goal PART 4, 9, 12, 13, 61, 63, 66, 67, 104, 112, 224, 232.
- BLOCKERS EB-011.

## Evidence required before this ADR's guarantees can be claimed VERIFIED

- `test/security` architectural test: the strategy evaluator and agent packages
  import no interpreter, `os/exec`, plugin loader, or WASM runtime.
- Compile-time negative tests: IR that references an external URL, a shell
  command, a file path, or a package dependency is rejected with a typed error.
- Runtime negative test: the agent worker, with the tool broker stubbed to deny,
  cannot reach any network destination (verified with an egress probe in staging
  and documented security-group rules).
- Import-boundary test from ADR-0003 (agent packages cannot import signing, wallet,
  admin, capital mutation, risk mutation).
- Threat-model entry (PART 156) for "malicious strategy author" with mitigations
  pointing at these tests.
