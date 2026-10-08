# BACKUP AND RESTORE

Status: local drill implemented and passing (2026-09-06); production procedure designed, not yet exercised (no AWS environment exists, see BLOCKERS EB-012). A backup that was never restored is not a proven backup (PART 141); this document separates what has been proven from what is planned.

## 1. What is proven today (LOCAL)

`make restore-drill` (`scripts/restoredrill`) runs against the docker-compose Postgres:

1. provisions a fresh, fully migrated database and seeds it as the application role: a balanced ledger fixture, then, through the real services, the internal (Domain A) economy and the advanced books (below);
2. backs it up with `pg_dump -Fc` through `docker exec`, recording size and SHA-256;
3. restores the archive into a new, empty database with `pg_restore --exit-on-error`;
4. "boots" the restored copy: `migrate verify` (embedded-file checksums vs applied rows) and version equality with the source;
5. reconciliation dry-run: recomputes every ledger balance from journal entries on the restored copy (expects zero drift), compares row counts of every table with the source, compares a deterministic hash of all journal transactions and entries on both sides, and reports every advanced area's rows on both sides, failing when an area the seed writes holds no row in the source;
6. writes `dist/restore-drill.json` and exits non-zero on any mismatch.

Latest local run (2026-10-07, macOS host without Docker, `go run ./scripts/restoredrill -pg-bin <PostgreSQL 16 client bin>` against a local PostgreSQL 16.15): 269 tables, row counts identical, 0 accounts with balance drift, journal hashes equal, 145 rows in the 12 advanced areas the fixture seeds and none of them empty, version 763 on both sides, 2.9 s (2026-10-08, with the advanced books in the fixture). Earlier runs reached version 763 with 269 tables in 2.6 s and no advanced book, versions 762, 761 and 760 with 269 tables, version 759 with 268 and 263 tables, version 758 with 263 tables, version 756 with 261 and 259 tables and version 754 with 254, 239 and 220 tables and versions 751 with 217, 209 and 181 tables, 749 (152), 744 (134) and 732 (128); the one before (2026-09-09, docker-compose) version 730 with 119 tables. The fixture is a live internal economy driven through the real services, so the Domain A tables carry rows rather than comparing zero with zero — including the risk policy a trade was evaluated against and the decision it produced — and since 2026-10-08 it carries the advanced books as well (next paragraph). `dist/restore-drill.json` holds the numbers from the last run.

The advanced books are seeded through the services the advanced demo drives (`scripts/internal/sandboxworld`: LOCAL, every value SIMULATED). The faucet funds Alice (1,000 SBX-USD and 1 SBX-ETH) and Bob (1,000 SBX-USD); Alice sends Bob 250 SBX-USD; on an internal PAPER book for SBX-ETH/SBX-USD, Alice's resting limit sell of 0.2 SBX-ETH at 2,500.00 is filled by Bob's crossing limit buy (one trade of 500 SBX-USD notional, with a maker fee of 0.50 and a taker fee of 1.00 SBX-USD booked through the fee ledger); Bob opens a CROSS paper margin account and posts 100 SBX-USD of collateral; Alice mints 100 SBX-DRILL, a stable asset pegged to and fully reserved in SBX-USD, on the simulated chain (sbx-solana), posted only once the mint is final; and both buy complete sets of a SANDBOX prediction market (Alice 5, Bob 10, so 15 SBX-USD in escrow). Each step is checked by what its service returned. The drill then reports twenty advanced areas, which between them hold every table migrations 00730 to 00763 create, with their rows on both sides (`advanced_areas` in the report), and fails when an area the seed writes holds no row in the source or when any table restores with a different count — so empty advanced books can no longer pass as zero equal to zero. Rows in the source in the latest run, each area restored with the same count: evidence archive 14, economic intents and effects 71, facilities and house accounts 4, canonical instruments 6, sandbox funding 3, transfers 2, order books and trades 18, fee ledger 6, margin 3, stable assets 7, simulated chain 6, prediction markets 5. The eight areas the seed does not write (derivatives, equities, treasury, bridges, the strategy marketplace, the strategy sandbox, delegation and scheduled upkeep) held no rows and still compare zero with zero, and in the seeded areas only what that opening activity writes carries rows: no liquidation, redemption, resolution or settlement is seeded.

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
