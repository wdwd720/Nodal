# ADR-0003: Modular monolith with security-motivated process boundaries

## Status

Accepted — implementation tracked in docs/build/REQUIREMENTS_TRACEABILITY.md

## Date

2026-09-05

## Context

A financial control plane has two pressures that pull in opposite directions.
Correctness wants a single transaction boundary: reservation, intent state, ledger
posting, and outbox event must commit or fail together. Security wants blast-radius
separation: the process that evaluates strategies must not be the process that
holds signing credentials (PART 9, 100).

Failure modes of the naive answers:

- Microservices: "capital" and "ledger" as separate services need distributed
  transactions or sagas for every trade. Partial commits, compensations that
  themselves fail, version skew between services, N authentication surfaces, and
  network partitions between services that share one truth. This is the
  distributed-monolith pattern.
- Single process for everything: the agent worker, which ingests untrusted external
  content (PART 67), would run in the same address space and IAM role as wallet
  signing. A prompt-injection or dependency compromise there reaches the keys.
- Shared database credentials across roles: an `api` container compromise exposes
  break-glass privileges if every binary uses the same role.

## Decision

- One Go module; domain packages under `internal/<domain>` are the module
  boundaries. All financial state mutations across domains happen in-process inside
  one Postgres transaction (`db.InTx` / `db.Serializable`).
- Multiple binaries built from that module (DECISION_REGISTER D-003): `api`,
  `execution-worker`, `reconciliation-worker`, `market-ingest-worker`,
  `agent-worker`, `workflow-worker`, `audit-worker`, `migrate`. Each binary runs
  under its own ECS task role, its own database role, and its own secret set
  (PART 100, 101). A binary can only load the secrets its role permits.
- A process boundary with Protobuf/gRPC exists only where security requires it:
  `execution-worker` to the signing service (ADR-0012). Everything else is an
  in-process call.
- Import boundaries are enforced, not just documented: agent-facing packages may
  import only read tools, prediction, and intent creation; they never import
  `signing`, `wallet`, `admin`, capital mutation, or risk-policy mutation
  (CONVENTIONS non-negotiable 9). A `test/security` test fails the build on
  violation.
- Deployment on ECS/Fargate behind a load balancer with Terraform (PART 137);
  no Kubernetes, no service mesh (PART 13).

### Explicitly not decided / deferred

- Whether the signing service is a separate deployable in a separate VPC/account or
  a separately credentialed task in the same cluster. Both satisfy this ADR; the
  choice depends on the wallet provider outcome (BLOCKERS EB-003) and threat-model
  review.
- Extraction of `market-ingest-worker` into an independently scaled service if
  ingest volume warrants it. The interface (`EventBus`) already allows it.
- Any additional binaries beyond the D-003 list.

## Consequences

### Positive

- Refactoring across domains is a compiler-checked change, not an API migration.
- One transaction boundary for money; no sagas inside the core.
- IAM and database-role separation per binary limits what a compromised container
  can reach.

### Negative

- All binaries share a release cadence; a bug in one package ships in all images.
- Discipline is required to keep package boundaries from eroding; enforcement is a
  test, not a network.

### Operational

- One image per binary; each has its own task definition, scaling policy, and
  alarms (PART 131–136).
- Rollout ordering: `migrate` first, then workers, then `api` (PART 140).

## Alternatives considered

- Microservices per domain: rejected for V1 because every trade would span services
  and require distributed transaction handling; cost and latency with no scaling
  need at V1 volumes.
- One binary, one role: rejected because it collapses the signing/agent blast-radius
  separation that PART 9 and PART 100 require.
- Kubernetes with a service mesh: PART 13 lists both as premature; ECS/Fargate with
  Terraform covers the deployment requirements at lower operational cost.
- Serverless functions: cold starts, connection-pool exhaustion against Postgres, and
  poor fit for long-running reconciliation and workflow workers.

## Related

- ADR-0001, ADR-0002, ADR-0004, ADR-0005, ADR-0012, ADR-0016.
- Goal PART 9, 12, 13, 14, 100, 101, 102, 103, 131, 137, 140.
- DECISION_REGISTER D-001, D-003; BLOCKERS EB-003, EB-007.

## Evidence required before this ADR's guarantees can be claimed VERIFIED

- `test/security` import-boundary test: agent packages cannot import the forbidden
  packages; the test enumerates the forbidden set and fails on any new edge.
- Terraform review record showing one task role per binary and a least-privilege
  policy per role (PART 100), with the agent-worker role denied KMS signing and
  wallet secrets.
- Integration test that each binary's database role has exactly the grants listed
  for it (PART 101) and that posted-ledger tables reject `UPDATE`/`DELETE` from the
  application role.
- Startup test: a binary configured with a secret reference outside its allowed set
  fails closed.
- Chaos test killing any single worker binary; the trade path degrades to a typed
  error, never to an inconsistent ledger.
- Threat-model entry (PART 156) covering compromise of each binary individually.
