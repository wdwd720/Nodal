# BACKUP AND RESTORE

Status: local drill implemented and passing (2026-09-06); production procedure designed, not yet exercised (no AWS environment exists, see BLOCKERS EB-012). A backup that was never restored is not a proven backup (PART 141); this document separates what has been proven from what is planned.

## 1. What is proven today (LOCAL)

`make restore-drill` (`scripts/restoredrill`) runs against the docker-compose Postgres:

1. provisions a fresh, fully migrated database and seeds a balanced ledger fixture as the application role;
2. backs it up with `pg_dump -Fc` through `docker exec`, recording size and SHA-256;
3. restores the archive into a new, empty database with `pg_restore --exit-on-error`;
4. "boots" the restored copy: `migrate verify` (embedded-file checksums vs applied rows) and version equality with the source;
5. reconciliation dry-run: recomputes every ledger balance from journal entries on the restored copy (expects zero drift), compares row counts of every table with the source, and compares a deterministic hash of all journal transactions and entries on both sides;
6. writes `dist/restore-drill.json` and exits non-zero on any mismatch.

Latest local run (2026-09-10, after 00805): 154 tables, row counts identical, 0 accounts with balance drift, journal hashes equal, version 805 on both sides, 15.5 s, and one real audited state transition driven on the restored database.

That last step is new and exists because the three before it all compare data. None of them proves the restored database can still be USED — and since 00741 that is no longer implied, because every state change on seventeen tables is checked against a keyed tag computed from a single row in `cp_transition_key`. A restore that brought back every row but lost that one table would have passed every other assertion here and then refused every state change in the system (F-129). `CP_DRILL_BREAK=lose_the_transition_key` empties the table so the probe can be watched firing; a run with it set is expected to fail. The table count jumped from 119 because 00740 partitioned `security_events` into thirteen months and a default — a partition is a table, and the drill counts what it would have to restore. That it comes back with matching row counts is the point: the first partitioned table in this schema restores as a partitioned table, not as an empty parent. The fixture is a live internal economy driven through the real services, so the Domain A tables carry rows rather than comparing zero with zero — including the risk policy a trade was evaluated against and the decision it produced. `dist/restore-drill.json` holds the numbers from the last run.

## Running the race detector on a Windows host

`go test -race` needs an external linker, and it fails to LINK if the GCC
toolchain's resolved path contains a space — the linker-script argument is split
on it and `ld` reports the fragment as a script that "appears multiple times".
The WinGet default install path contains one.

Copy the toolchain somewhere without a space and point `CC` at the copy:

    robocopy "<WinGet packages>\...\mingw64" C:	oolchain\mingw64 /E
    CC=C:	oolchain\mingw64in\gcc.exe CXX=C:	oolchain\mingw64in\g++.exe make race

A **copy**, not a junction and not an 8.3 short path. Both of those resolve back
to the original location, and GCC reports its resolved path, so the space
returns. The property that matters is not "a path without spaces" but "a path
GCC resolves to without spaces" (F-125).

This is not wired into the Makefile because `CC` is host-specific, and CI runs
on Linux where none of this applies.

This line said 89 tables and version 604 until F-54. That was a hundred and eleven migrations of schema ago: the drill it reported had never seen the ledger's chart-parity CHECK, the frozen-economics constraints, or a single Domain A table. A restore drill is evidence that the schema you would actually restore comes back — so a stale one is not a weaker claim, it is a claim about a different database. `TestDocs_CountsMatchTheCode` now holds the version cited here against the newest migration in the tree, and fails when migrations land without the drill being re-run.

The drill runs only against `127.0.0.1`/`localhost` and refuses any other host (`internal/testkit/localdb`).

## 2. Production design (AWS, pending Terraform)

| Concern | Design | Evidence required before claiming |
|---|---|---|
| Primary database | RDS PostgreSQL Multi-AZ, encryption at rest (KMS), automated backups with PITR (retention ≥ 35 days), deletion protection | Terraform plan/apply output; RDS console screenshot in evidence archive |
| Committed-transaction durability (RPO) | synchronous standby in Multi-AZ; design objective RPO 0 for primary failure; PITR covers broader disaster recovery | failover drill report |
| Restore drill (staging, quarterly) | restore the latest automated snapshot **and** a PITR point into a new instance; run `cmd/migrate verify`; point a staging API/reconciliation-worker at it; run `reconciliation.RunFull` in dry-run against the staging chain observers; compare with the source | signed drill report with timings (RTO measurement) |
| Ledger integrity after restore | `ledger.VerifyBalances`, `positions.VerifyAgainstLedger`, `capital.VerifyReservationTotals`, `audit.VerifyAll` must all report zero drift on the restored copy | job output archived |
| Evidence archive | S3 with Object Lock (compliance mode) for audit archives; versioning + replication for raw evidence; restore drill includes fetching and re-hashing a sample of archived objects | archive verification job output |
| Secrets | restored instance receives new credentials from Secrets Manager; old credentials rotated | rotation log |

RTO/RPO are **not** claimed until a staging drill has been run and measured (PART 205).

## 3. Runbook: restoring production (procedure, unexercised)

1. Declare the incident; activate `GLOBAL_NEW_RISK_KILL` (new risk stops; reconciliation and settlement continue against the surviving database or are paused if the database is the failure).
2. Choose the restore point: latest snapshot or PITR timestamp just before corruption. Record the choice and rationale in the incident log.
3. Restore into a **new** RDS instance (never overwrite the original); apply the production parameter group and security groups; do not expose publicly.
4. Run `cmd/migrate verify` against the restored instance with the migration role. A checksum mismatch means the restore point predates a deployed migration: stop and escalate.
5. Point a single reconciliation-worker at the restored instance in read-only mode and run full reconciliation against external truth (chain observers, providers). Every unknown external transaction since the restore point becomes a reconciliation record.
6. Compare the reconciliation outcome with the incident timeline; obtain dual approval (FINANCE + SECURITY) to promote the restored instance.
7. Rotate database credentials, repoint services, release the kill switch under the documented re-enable approval, and keep the original instance for forensics until the incident is closed.

## 4. Related

`scripts/restoredrill`, `internal/testkit/localdb`, `docs/operations/DISASTER_RECOVERY.md` (pending), `docs/runbooks/database-corruption.md` (pending), BLOCKERS EB-012.
