# ADR-0007: S3 with Object Lock for tamper-evident evidence

## Status

Accepted — implementation tracked in docs/build/REQUIREMENTS_TRACEABILITY.md

## Date

2026-09-05

## Context

The platform must be able to prove, after the fact, what it knew, what it decided,
and what it did (PART 87, 88, 89). Raw provider requests and responses (PART 183),
audit events, risk decisions, signing requests, and reconciliation records are the
evidence. The failure modes:

- An insider or an attacker with administrative access deletes or edits audit
  records after an incident; a database-only audit log is mutable by whoever holds
  the database role.
- Log truncation by retention policy or ransomware removes the only record of a
  disputed fill.
- Serialization drift: the same logical record hashed from two JSON encoders with
  different field ordering yields different hashes, making verification fail or,
  worse, making tampering undetectable because verification is disabled.
- Unverifiable claims: "we have an audit trail" without a job that actually checks
  the chain is a claim, not evidence (PART 201).
- Evidence retained in violation of data-licensing or privacy obligations
  (PART 120–122).

## Decision

- Evidence objects are written to S3 with Object Lock enabled on the bucket, so a
  locked object cannot be deleted or overwritten for its retention period even by
  the bucket owner. Locally, MinIO with Object Lock provides the same interface
  (DECISION_REGISTER D-010).
- The proof pipeline follows PART 87: canonical record, deterministic canonical
  serialization, hash, previous-hash reference, KMS signature, append-only stream,
  periodic Merkle root, archive to the locked bucket. The object hash and the chain
  position are recorded in Postgres audit tables (PART 123).
- Canonical serialization is a defined byte encoding (sorted keys, fixed number
  formatting, no insignificant whitespace); reordering fields in the source struct
  does not change the hash.
- Raw provider request and response bodies are archived with provenance metadata
  (provider, endpoint, request ID, timestamps) under a partition layout that allows
  reconstruction (PART 124).
- IAM: the audit writer role may `PutObject` and read; it may not delete, shorten
  retention, or disable Object Lock. No application role has those permissions.
- A verification job (`make verify-audit` or the equivalent Go program) checks the
  hash chain, KMS signatures, Merkle membership where applicable, and object hashes.
  A tampered-record test must fail verification (PART 201).
- Retention classes (PART 122) decide what is archived and for how long; licensed
  data is archived only where rights permit (BLOCKERS EB-012).

### Explicitly not decided / deferred

- Object Lock mode (Compliance versus Governance) and retention durations; these
  depend on legal review (EB-007, EB-012).
- Cross-region replication of the archive.
- External anchoring of Merkle roots (for example publishing them to a third party
  or a public chain). The pipeline produces roots; anchoring is deferred.

## Consequences

### Positive

- Tamper evidence without blockchain infrastructure (PART 13, 87).
- Disputes and incidents can be reconstructed from raw evidence, not from logs.
- Verification is a runnable command, so the guarantee can be checked continuously.

### Negative

- Locked objects cannot be deleted for their retention period even when deletion
  would be legitimately desired (for example a wrongly archived personal record);
  retention classes and pre-archive filtering must be correct up front.
- Storage cost grows monotonically for the retention window.

### Operational

- The verification job runs on a schedule and its failure is a P1 alert.
- KMS key rotation must preserve the ability to verify old signatures.

## Alternatives considered

- Blockchain anchoring as the primary mechanism: PART 13 rejects blockchain audit
  "merely for marketing"; it adds infrastructure without improving the internal
  guarantee. Left open as an optional anchoring target.
- Database-only audit tables: mutable by anyone holding a privileged database role;
  retained as the index, not the authority.
- Glacier Vault Lock: appropriate for cold storage; Object Lock is chosen because
  verification needs read access on demand. Lifecycle to Glacier is possible later.
- Third-party log services: no immutability under the platform's own control.
- Amazon QLDB: discontinued by the provider.

## Related

- ADR-0001, ADR-0005, ADR-0006, ADR-0012, ADR-0013.
- Goal PART 12, 13, 75, 87, 88, 89, 120, 121, 122, 123, 124, 183, 201.
- DECISION_REGISTER D-010; BLOCKERS EB-007, EB-012.

## Evidence required before this ADR's guarantees can be claimed VERIFIED

- `make verify-audit` passing against a staging archive, with the report attached
  to the readiness report.
- Tampered-record test: modify one archived object or one chain row; verification
  fails and identifies the position.
- Property test that canonical serialization is invariant under struct field
  reordering and map iteration order, and that two semantically equal records hash
  equal.
- IAM policy test (Terraform plan assertion or a runtime probe in staging) that the
  writer role is denied `DeleteObject`, `PutObjectRetention` with a shorter date,
  and `PutObjectLegalHold` removal.
- Staging report showing Object Lock enabled on the bucket and a locked object
  resisting deletion.
- KMS signature verification test including a rotated key.
- Retention-class review record covering licensed and personal data (EB-012,
  PART 121, 122).
