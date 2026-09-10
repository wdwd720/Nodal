# ADR-0020 — Retention against append-only tables

Status: **Accepted** (2026-09-09)

Supersedes nothing. Constrains any future implementation of
`CP_RETENTION_SOCIAL_DATA_DAYS`, `CP_RETENTION_MODEL_IO_DAYS` and
`CP_RETENTION_OPERATIONAL_LOG_DAYS`.

## Context

`config.RetentionConfig` declares six retention classes and `internal/reality`
names them: FINANCIAL_RECORD, SECURITY_AUDIT, RAW_MARKET_DATA, SOCIAL_DATA,
MODEL_IO, OPERATIONAL_LOG. Two of the six are enforced today, and both are
enforced by something outside PostgreSQL:

- **SECURITY_AUDIT** sets the Object Lock retain-until date on archived audit
  objects (`cmd/audit-worker`).
- **RAW_MARKET_DATA** sets the ClickHouse TTL on `normalized_events`
  (`cmd/market-ingest-worker`).

The other four are read by nothing. FINANCIAL_RECORD is deliberately so — a
financial record is kept, not aged out — which leaves three classes that name a
policy the system does not have: SOCIAL_DATA, MODEL_IO and OPERATIONAL_LOG.

The obvious implementation is a `DELETE ... WHERE created_at < now() - interval`
on a ticker, as F-79 did for `login_attempts`. It does not work here, and the
reason is a control rather than an obstacle. The tables these classes would
cover carry `forbid_mutation` triggers, which refuse **UPDATE and DELETE
outright, for every role including the table owner**:

| Class | Tables it would cover | Refuses DELETE? |
|---|---|---|
| MODEL_IO | `model_calls`, `tool_invocations`, `compile_attempts` | yes, all three |
| SOCIAL_DATA | `raw_archive_objects` (rows classified SOCIAL_DATA), the ClickHouse side | yes |
| OPERATIONAL_LOG | `provider_health_samples`, `provider_events`, `login_attempts`, `security_events` | `provider_health_samples` and `provider_events` yes; `login_attempts` no (F-79 purges it); `security_events` no |

Fifty-three tables in the schema carry `forbid_mutation`. That immutability is
not incidental: it is what makes `internal/proof`'s hash chain verifiable, what
makes `provider_events` evidence (F-56), and what several findings in this
register exist to have restored. Any retention scheme that turns it off, even
briefly and even for one table, has removed the property the rest of the system
is built on.

There is a second constraint that is easy to miss. A row is not the only copy.
`raw_archive_objects` rows point at archived objects; `audit_events` are covered
by Merkle checkpoints; `model_calls` are referenced by `agent_runs`. Deleting a
row that something else proves the existence of turns a verifiable chain into a
broken one, which reads as tampering.

## Decision

**1. Retention never disables a trigger and never grants DELETE on an
append-only table.** `ALTER TABLE ... DISABLE TRIGGER`, `session_replication_role
= replica`, and a migration that drops `forbid_mutation` "for the retention job"
are all out of scope permanently. If a scheme requires one of them it is the
wrong scheme.

**2. Retention on an append-only table is expressed as partition detachment,
not as row deletion.** The table becomes range-partitioned on its time column;
retention detaches and drops whole partitions. A partition drop is a DDL
operation on a table, not a DML operation on rows, so it does not pass through
`forbid_mutation` — and, more to the point, it cannot delete *some* rows and
leave others, which is what makes it auditable. What was dropped is a stated
range, not a set somebody chose.

**3. Nothing is dropped that something else still proves.** Before a partition
is detached, the retention pass must establish that no surviving artefact
references what is in it: no unexpired Merkle checkpoint covering those audit
events, no `agent_runs` row referencing those `model_calls`, no
`raw_archive_objects` row whose object is still under an Object Lock retain-until
date. A pass that cannot establish this refuses, in the same way
`internal/capacity` refuses when it cannot measure.

**4. Where the payload is the sensitive part and the row is not, redact rather
than delete.** `login_attempts` is the counter-example that proves the shape:
what must not persist is the plaintext nonce and PKCE verifier, not the fact
that a login was attempted from an address at a time. For an append-only table
the same split applies — a class whose sensitivity is in one column is served by
writing that column to a separate, deletable table from the start, and never by
retrospectively editing an immutable row.

**5. A retention class that is declared must be enforced or must say it is
not.** The present state — three classes configured, validated, and read by
nothing — is the failure this register keeps finding under other names. Until a
class is enforced, `config.Validate` should not accept it silently; the variable
should carry, in the table, the fact that nothing reads it yet.

## Consequences

- Implementing SOCIAL_DATA, MODEL_IO or OPERATIONAL_LOG retention starts with a
  migration that partitions the target table, not with a worker. That migration
  is not free: partitioning an existing table means creating the partitioned
  parent, moving rows, and swapping — on tables that refuse DELETE, which means
  the move is a `INSERT INTO ... SELECT` into the new parent and a rename, done
  as the owner, in a migration.
- The retention worker becomes a partition manager: it creates the next
  partition ahead of time and detaches expired ones. Both are DDL, so it needs a
  role that can do DDL — `cp_migrate`, not `cp_ops` and certainly not `cp_app`.
  That is a stronger credential than F-79's purge needed, and it is the reason
  this ADR exists rather than a commit.
- Retention becomes visible in the schema: a reader can see which tables are
  partitioned by time and infer which classes are enforced, rather than having
  to find the worker.
- `login_attempts` stays a plain DELETE (F-79). It is not append-only, it holds
  no chain, and nothing proves its rows exist. Decision 2 is about the tables
  that are, not about every table with a timestamp.

## Alternatives rejected

**Delete rows under a temporarily disabled trigger.** Rejected by decision 1.
It is the only approach that needs no schema change, and it makes every
immutability guarantee in the system conditional on a worker's good behaviour.
The audit chain would remain verifiable only because the deleting process chose
not to touch it.

**Grant `cp_ops` DELETE on the append-only tables and rely on privilege
separation.** Rejected. `forbid_mutation` is deliberately stronger than
privilege: it refuses the owner too, precisely so that a compromise of an
administrative credential cannot rewrite history. Reintroducing a role that may
delete recreates what the trigger was written to prevent.

**Archive to object storage and then delete.** Rejected as the primary scheme,
though it is a legitimate part of one. It has the same problem: the delete still
has to happen through the trigger. It also moves the retention obligation rather
than discharging it, since the archived copy has its own retention.

**Do nothing and let the tables grow.** This is the current state, and it is not
neutral: the launch tier's database ceiling is 500 MB and the capacity guard
refuses new financial actions at 67% of it (`internal/capacity`). Unbounded
growth in `model_calls` or `provider_health_samples` eventually stops the product
by a path that looks like a capacity refusal rather than a retention failure.

## Open, and deliberately

The retention **periods** are not decided here. The variables exist and carry
defaults; whether 30 days of MODEL_IO is right is a product and compliance
question. This ADR decides only how a period, once chosen, may be enforced.
