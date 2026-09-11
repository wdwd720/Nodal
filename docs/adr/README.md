# Architecture Decision Records

Architecture decisions for the platform, one file per decision. The process, the
template, and the status vocabulary are in [ADR-0000](0000-adr-process.md).

Every ADR below describes intended architecture. An `Accepted` status means the
decision has been made; it does not mean the decision has been implemented or
proven. Implementation and verification state is recorded in
`docs/build/REQUIREMENTS_TRACEABILITY.md` using the goal's verification labels
(`CODE_COMPLETE`, `CONTRACT_TESTED`, `SANDBOX_VERIFIED`, `CANARY_VERIFIED`,
`LIVE_VERIFIED`, `BLOCKED_EXTERNAL`). Each ADR names the tests and reports that
must exist before its guarantees can be claimed verified.

The set of ADRs is the one required by goal PART 210.

## Index

| ADR | Title | Status | Goal PARTs | Register / Blockers |
|---|---|---|---|---|
| [0000](0000-adr-process.md) | ADR process, template, status vocabulary | Accepted | 7, 209, 210, 237, 239, 243 | — |
| [0001](0001-postgres-ledger.md) | PostgreSQL is the sole financial ledger of record | Accepted | 11, 12, 13, 19, 21, 22, 23, 101, 129, 138–141 | D-001, D-004, D-005, D-007, D-008 |
| [0002](0002-go-core.md) | Go for the financial core | Accepted | 12, 13, 14, 17, 104, 143, 151, 152, 214–216, 224 | D-002, D-012, D-013; SB-005 |
| [0003](0003-modular-monolith.md) | Modular monolith with security-motivated process boundaries | Accepted | 9, 12, 13, 14, 100, 101, 102, 137 | D-003; EB-003, EB-007 |
| [0004](0004-temporal-workflows.md) | Temporal for durable multi-step processes only | Accepted | 11, 12, 28, 46, 69, 70, 94, 115, 116 | D-009, D-010; EB-008 |
| [0005](0005-redpanda-event-stream.md) | Redpanda event streaming with transactional outbox and inbox | Accepted | 11, 12, 31, 36, 46, 77, 117, 199 | D-008, D-010; EB-008 |
| [0006](0006-clickhouse-analytics.md) | ClickHouse for analytics, never for balances | Accepted | 11, 12, 74–76, 80–82, 110, 118, 226 | D-010; EB-008 |
| [0007](0007-s3-worm-evidence.md) | S3 with Object Lock for tamper-evident evidence | Accepted | 12, 13, 75, 87–89, 122–124, 183, 201 | D-010; EB-007, EB-012 |
| [0008](0008-jupiter-execution-router.md) | Jupiter as the V1 Solana execution router, behind an adapter | Accepted | 34, 41–46, 48, 79, 105–107, 152, 183, 207, 208 | EB-006 |
| [0009](0009-helius-solana-data.md) | Helius as primary Solana data provider, with independent fallback | Accepted | 11, 45, 48, 50, 78, 79, 174, 175, 196, 199, 221 | EB-005 |
| [0010](0010-typed-strategy-ir.md) | Typed, hashed Strategy IR as the only executable strategy form | Accepted | 9, 10, 60–65, 68–70, 170, 176, 224, 225 | EB-011, EB-015 |
| [0011](0011-no-arbitrary-live-code.md) | No arbitrary user code in the live path | Accepted | 4, 9, 12, 13, 61, 63, 66, 67, 104, 232 | EB-011 |
| [0012](0012-no-ai-signing.md) | No AI, agent, or strategy may sign; bounded signing behind an isolated service | Accepted | 4, 9, 10, 34, 44, 63, 67, 94–96, 100, 152, 155, 221 | D-003; EB-003, EB-004 |
| [0013](0013-deterministic-risk-kernel.md) | Deterministic, versioned Risk Kernel with no model in the loop | Accepted | 10, 24, 52, 53, 58, 59, 60, 128, 174, 224 | D-007 |
| [0014](0014-settlement-compiler.md) | Deterministic Settlement Compiler as the only route from intent to execution | Accepted | 3, 10, 35, 38–41, 46–49, 170, 224, 225, 228 | D-007 |
| [0015](0015-universal-buying-power.md) | Universal Buying-Power Engine as a dedicated domain | Accepted | 3, 4, 11, 20–27, 33, 110, 119, 174 | D-007 |
| [0016](0016-production-capability-gating.md) | Production capabilities are software-enforced, evidence-backed gates | Accepted | 9, 54, 55, 91, 93, 94, 128, 164, 167, 208, 240–244 | EB-001–EB-016 |
| [0017](0017-single-primary-region-v1.md) | Single primary write region for V1; no active-active money writes | Accepted | 12, 13, 137–139, 141, 172, 205, 219 | EB-007, EB-008 |
| [0018](0018-no-proprietary-stablecoin.md) | No proprietary stablecoin or platform-issued dollar token | Accepted | 4, 11, 13, 20, 26, 27, 32, 33, 94 | EB-002, EB-009, EB-010 |
| [0019](0019-no-internal-crossing.md) | No internal order matching, crossing, or principal trading | Accepted | 3, 4, 10, 41, 46, 50, 163, 182 | EB-009, EB-011 |
| [0020](0020-retention-against-append-only-tables.md) | Retention on an append-only table is partition detachment, never row deletion under a disabled trigger | Accepted | 122 | — |
| [0021](0021-who-may-read-personal-data.md) | Personal data is encrypted in the application or not stored, and a role with no use for it cannot read it | Accepted | 121 | — |
| [0022](0022-one-identity-source-of-truth.md) | One identity source of truth: ZITADEL authenticates, Neon owns the Nodal user; no second authentication system | Accepted | 4 (product goal §3) | — |
| [0023](0023-the-sandbox-tier.md) | The sandbox tier: one declaration, refused in PROD, lets a non-production deployment exercise every gated surface without approving anything | Accepted | 4 (product goal §9) | — |
| [0025](0025-verification-is-provider-hosted-and-evidence-based.md) | Verification is provider-hosted, evidence-based, and never touches a Credit | Accepted | 4 (product goal §20, §21, §24) | D-057, D-058, D-059, D-061; B-02, B-06 |
| [0026](0026-the-conversion-request-is-payout-requests.md) | The conversion request is `payout_requests`, with a destination, a quote and its provenance | Accepted | 4 (product goal §19, §22, §23, §25) | D-060, D-062; B-01, B-05, B-09 |

"Accepted" in the table abbreviates the full status line used in each file:
`Accepted — implementation tracked in docs/build/REQUIREMENTS_TRACEABILITY.md`.

## How the ADRs relate

- Truth and storage: 0001 (ledger) is the authority; 0005, 0006, 0007 are derived
  streams, analytics, and evidence; 0004 orchestrates without holding truth; 0017
  bounds where the authority lives.
- Language and shape: 0002 and 0003 define the codebase; 0003 also creates the one
  process boundary that 0012 depends on.
- The control path (goal PART 10): 0010 and 0011 bound what can propose; 0015
  values, 0013 permits, 0014 plans, 0008 and 0009 execute and observe, 0012 signs
  within bounds, 0007 records.
- Scope exclusions that are also safety boundaries: 0011, 0018, 0019; all enforced
  through 0016.

## Adding or changing an ADR

Follow [ADR-0000](0000-adr-process.md). A change to an accepted decision is a new
ADR that supersedes the old one, plus an entry in
`docs/build/DECISION_REGISTER.md`. Update this index in the same change.
