# PRODUCTION READINESS REPORT

**Platform status: `NOT_READY`. Capital authority: `DISABLED`.**

Commit `a92b69d` · 2026-09-06 · Stage 19 of `ULTIMATE MASTER GOAL — Production Universal Financial Control Plane.md` (PART 242).

---

## 1. Executive conclusion

**This system is not authorized to touch real customer money, and must not be pointed at any live provider, live wallet, live chain or live card rail today.** Not in production, not in a "small" canary, not for one internal test trade. Nothing in this repository has ever moved real money, and three specific facts — not general caution — are why that must remain true: **there is no execution venue adapter of any kind** (the only implementation is a test fake, which is why quote requests correctly answer 503); **the Jupiter instruction layout the transaction inspector relies on was reproduced from memory and has never been checked against the published IDL or a real mainnet transaction** (SB-007); and **every provider integration has only ever been exercised against recorded fixtures on this machine — no request has ever left it.**

What follows is a large body of genuine engineering evidence. It is real, and section 10 records it precisely. It is also narrower than the words "all tests pass" imply, and the whole value of this document is in the distance between those two things. A reader who acts on this report and is later surprised means the report failed.

### 1.1 What IS authorized, stated precisely

These are not concessions. They are the things this build has actually earned.

| Authorized | Scope and limits |
|---|---|
| **Local development** (`CP_ENV=LOCAL`) against `docker compose` | Postgres 5433, Redis 6380, Redpanda 19092, ClickHouse 19000/18123, Temporal 7233, MinIO 9100. `make seed` writes fake USDC and dev identities; it refuses any non-local host and any environment other than LOCAL/DEV/TEST. |
| **The full local test suite** | Every tier in §10, including the destructive ones (chaos pauses containers, security plants forged sessions, migrations drops the schema) — each against a database provisioned by `scripts/testdb`, never a shared one. |
| **PAPER-mode intent submission** | `POST /v1/intents` accepts, deduplicates and persists an intent. Verified under 100-way concurrency: 100 callers sharing one `Idempotency-Key` produced exactly one intent row (§12). Eligibility, risk and capital reservation happen **downstream in the execution worker, which cannot start** — so no intent has ever been acted on, and "end to end" would be the wrong phrase. |
| **Every read path in the API and both web applications** | Buying power, holdings, ledger, activity, export, markets, instruments, admin console reads. 51 Playwright tests pass against the real API and a real database, plus a real OIDC round trip through the dev IdP. |
| **The admin plane's propose / approve decision paths** | Against a real database over real HTTP, with proposer≠approver proven by identity rather than by permission (§5), and refused independently by a database `CHECK`. **Execution of an approved action is not wired in this build** (§15.1b). |
| **CI on GitHub Actions** | `ci.yml` is green — all 18 jobs, run `34065141796`, commit `af28dd8`. **This supersedes the hollow run this report originally cited.** That earlier green run executed ZERO integration packages; the gap this report found was fixed in the same commit that first published the report. The current run executes all **40** integration-tagged packages, each on its own database, races the seven financial-core packages *with* the integration tag, and runs the chaos fault-injection tests that previously skipped silently — 0 skips, chaos 178s against 0.4s before. See §10.6. |
| **Terraform `validate` and `trivy config`** in dev, staging and prod | Configuration is syntactically and structurally sound and scans clean. Nothing has ever been planned or applied against AWS. |

Everything else — every capability that could cause an external financial effect — is off, and §9 shows the machinery that keeps it off.

### 1.2 What is NOT authorized, and why

| Not authorized | The specific reason |
|---|---|
| Any live trade, including a single canary | **SB-007** (unverified Jupiter layout) **and** no `ExecutionAdapter` implementation exists. `bindProviders` returns an error for every provider mode. |
| Any live funding (fiat → USDC) | **EB-003** Stripe commercial approval and production credentials. Adapter is code-complete and fixture-tested; no request has ever reached Stripe. |
| Any real signature over a real transaction | **EB-002** custody analysis, **EB-005** Privy signing semantics. `SignTransaction` idempotent replay is a documentation claim the vendor has not confirmed. |
| Any withdrawal | The route is live and answers `422 CAPABILITY_NOT_APPROVED` on every request — `withdrawal.Service.Request` calls `RequireActive(WITHDRAWALS)`, which fails at condition 1 before any query is issued. There is also no off-ramp adapter and no production caller of `Repository.Transition`, so a withdrawal could never leave `REQUESTED`. **EB-015**. |
| Any agent trading with customer capital | **EB-007** adviser/CTA analysis, plus everything above. |
| Any staging or production deployment | **EB-012** — no AWS account. Terraform has never been planned or applied; nothing AWS-side is verified. |
| Presenting §12's numbers as capacity | They are loopback measurements on one laptop against a one-customer dataset. |
| ~~Trusting a green CI run as coverage of the database-backed tier~~ **RESOLVED** | This was true when the report was written and is no longer: CI ran **zero** of the 40 integration-tagged packages, `make race` omitted `-tags=integration` so no database-backed concurrency test had ever been raced, and two chaos fault-injection tests skipped silently for want of two environment variables. All three are fixed and verified in run `34065141796`. The finding is kept rather than deleted, because *how* it was missed is the useful part: a job whose package list resolved to the empty set exited 0 in under a tenth of a second, and a green badge was read as evidence. |

### 1.3 What this report found that the build-state documents did not

Writing this report meant re-verifying rather than restating, and that produced eleven findings that are not in `MASTER_BUILD_STATE.md`, `ADVERSARIAL_VALIDATION.md` or the traceability document. None of them changes the answer in §1.1 — everything still fails closed — and every one of them is a launch blocker. They are listed here rather than buried because a reader who stops after section 1 should still know they exist.

| Finding | Where |
|---|---|
| **CI runs zero of the 40 database-backed integration packages.** The job's package list is empty and it exits 0 in under a tenth of a second | §10.6 |
| **CI's chaos job skips the two tests that inject real faults** — 0.4 s in CI against 28.5 s locally | §10.6 |
| **No concurrency test that needs a database has ever run under `-race`** — `make race` omits the build tag | §10.6 |
| **Branch protection cannot be enabled on this repository**, so a red commit can land on `main` | §10.6 |
| **A single `UPDATE` from the application database role can activate a capability gate**, contradicting the compliance document's explicit claim | §9.2b |
| **`gates.Bootstrap` has zero callers**, so a fresh deployment has no gate rows at all | §9.2a |
| **The agent worker's live-gate check ignores four of the five activation conditions** | §9.2c |
| **`LIVE_MANUAL_TRADING` has no enforcement point that has ever executed**, and the API accepts a client-declared `mode: LIVE` | §9.3 |
| **Dual control cannot complete for seven of its eight action kinds** — a severe kill switch can be activated and never released | §15.1b |
| **Every reconciliation SEV1 is discarded**, and 7 of 9 binaries emit no telemetry at all | §14 |
| **Seven of nine binaries default `CP_ENV` to `LOCAL`**, disabling every production validation rule | §15.3b |

Six documents also no longer describe the repository, in both directions (§17.4). The threat model in particular is scored against a boundary from before half the system existed.

### 1.4 Status values (PART 243)

| Field | Value | Why not higher |
|---|---|---|
| Platform | **`NOT_READY`** | `CODE_READY` would require, at minimum, an execution venue adapter to exist. It does not. |
| Capital authority | **`DISABLED`** | `CANARY_AUTHORIZED` requires SB-007 closed against real transactions, a venue adapter, live provider credentials, and the legal P0 gates (EB-001, EB-002, EB-006, EB-007) answered. None is satisfied. |

---

## 2. Architecture implemented

A modular monolith in Go (single module `github.com/nodal/controlplane`, D-001/D-003) with **nine binaries** split along trust boundaries, sharing `internal/` packages. PostgreSQL is the only financial source of truth.

| Binary | Role | State |
|---|---|---|
| `cmd/api` | `/v1` REST per the OpenAPI 3.1 document, problem+json, `Idempotency-Key`, SSE, deny-by-default authorization | runs; tested cross-process. **No admin action kind is executable over HTTP in this build** (§15.1b) |
| `cmd/execution-worker` | settlement compiler / resumable executor | **cannot start in any environment** — no provider binding exists |
| `cmd/reconciliation-worker` | event-driven, periodic and full reconciliation | runs; **only internal consistency verification works** — `full` always fails, no observers or adapters are wired (§15.1b) |
| `cmd/relay-worker` | transactional outbox → Redpanda, per-partition ordering | runs; integration-tested against live local Redpanda |
| `cmd/workflow-worker` | Temporal workflows (funding, reconciliation escalation) | runs; replay-tested against histories from the live local Temporal. Dials Temporal **without TLS** (§15.3b) |
| `cmd/audit-worker` | Merkle checkpoints over the audit chain, signing, WORM archive, `verify` | runs locally with a local ECDSA key and a filesystem archive. **`run` and `checkpoint` cannot start in STAGING or PROD** (§15.1b) |
| `cmd/market-ingest-worker` | raw archive → normalizer → ClickHouse | runs; smoke-tested against the live local stack |
| `cmd/agent-worker` | agent runtime dispatcher | runs; **no tests**, and its evaluator is a stub that hard-errors |
| `cmd/migrate` | migrations under a dedicated role, checksum-verified | runs; integration-tested. **Does not enforce TLS on its own connection** (§15.3b) |

The control path the system implements, in order: eligibility → deterministic risk kernel → atomic capital reservation → settlement compiler → quote → transaction inspection → bounded signing → submission → finality observation → reconciliation → ledger posting → audit evidence.

**The path is complete in code and broken in one place at runtime**: between "quote" and "submission" there is no venue. `internal/execution/adapter.go` declares the `ExecutionAdapter` contract (`Quote`, `ValidateQuote`, `Build`, `Submit`, `Status`, `Cancel`, `Reconcile`), `internal/settlement` drives it, and the only type that satisfies it anywhere in the tree is `internal/settlement/settlementtest.Adapter`, a scripted test double. `internal/provider/jupiter` is a complete Swap V2 client that nothing wraps into that interface.

Supporting infrastructure, all implemented and locally exercised: Temporal (D-009), Redpanda via franz-go (D-008/D-031), ClickHouse for point-in-time analytics, S3/MinIO with Object Lock for evidence, Redis for rate limiting only (never financial truth).

Documentation: 10 architecture documents, 21 ADRs, 19 incident runbooks, 4 operations documents (`DEPLOYMENT`, `BACKUP_RESTORE`, `DISASTER_RECOVERY`, `RECONCILIATION`), a security architecture and a STRIDE threat model. See §17.4 for two of these that are now stale.

---

## 3. Repository structure

```
cmd/          9 Go binaries (above)
internal/     50 domain packages — the modular monolith
migrations/   40 numbered SQL migrations (00001 … 00700) + embed.go
openapi/      OpenAPI 3.1 source of truth → internal/gen/api (strict chi server)
proto/        signing service contract (buf) → internal/gen/proto
packages/     generated-client (TS, from OpenAPI), strategy-sdk (TS, mirrors the IR)
apps/         web (customer), admin (operator console)
test/         contract, security, e2e, chaos, load, integration/migrations
infra/        terraform — 12 modules × 3 environments
scripts/      Go programs, not shell (D-013): testdb, seed, fuzzall, restoredrill,
              lintfin, tool, images, supplychain, devrun, maketargets
docs/         architecture, adr, runbooks, operations, security, threat-model, build
```

`docs/build/` is the persistent execution state required by PART 7: `MASTER_BUILD_STATE.md`, `REQUIREMENTS_TRACEABILITY.md` (389 rows), `BLOCKERS.md`, `DECISION_REGISTER.md` (D-001…D-042), and `ADVERSARIAL_VALIDATION.md` (the PART 238 matrix). **This report is a summary of those documents plus fresh verification; where they disagree with the repository, §17.4 says so.**

---

## 4. Financial model

Exactness, enforced structurally rather than by convention (D-007, PART 17):

- USD is `int64` minor units with checked arithmetic. Asset quantities are `big.Int`-backed values serialized as decimal strings, stored `NUMERIC(38,0)`. Prices carry `{Mantissa, Scale, QuoteAsset, Source, At}`. Every rounding call takes an explicit `RoundingMode`.
- **`scripts/lintfin` forbids floating point outright** in production files under `internal/{money,ledger,capital,risk,positions,valuation,quote,settlement}` — no `float32`/`float64` identifiers, no float literals, no `ParseFloat`/`FormatFloat`, no `big.Float`, no `.Float64()`. Verified clean at this commit (exit 0), and it runs in CI.
- Append-only, multi-asset, per-asset double-entry ledger with a canonical content hash and idempotent posting. `DEFICIT` is credit-normal (D-017 — the original design document was wrong and a property test found it).
- Capital reservations are transactional. The PART 23 torture test (100 × $500 against $10,000) yields exactly 20 accepted / 80 refused in every iteration, under both row-lock and `SERIALIZABLE`, for asset and envelope variants.
- Buying power is computed per request from the ledger. **There is no cache in front of it** — not in Redis, not anywhere. Redis holds rate-limit counters only.
- The database enforces what the application must not be trusted to enforce: `cp_app` holds **no** default table privileges (D-016), has `DELETE` nowhere and `UPDATE` on no append-only table; state changes to gates, kill switches, accounts, assets, instruments, deposits, withdrawals, intents, orders, reconciliation records, admin actions and agents each require a matching transition row in the same transaction (migrations 00603 / 00690, SQLSTATE `AU001`).

---

## 5. Security model

Authentication is OIDC authorization-code + PKCE with server-side sessions in Postgres and opaque cookie tokens (D-006). ID tokens are verified with `coreos/go-oidc` over a package-local key set that keeps a JWKS refresh bound and distinguishes unknown-kid from bad-signature; `azp`, `nbf`/`iat` skew, constant-time nonce, `amr`/`acr`/`auth_time` and step-up stay explicit (D-014). MFA and passkeys are delegated to the IdP. The dev IdP is refused programmatically in STAGING/PROD.

Authorization is deny-by-default with a **closed 42-permission matrix** pinned by a golden test (`TestGoldenMatrix_PermissionListClosed` fails if the count changes), tenant scoping through `security.RequireAccount`, and a route-coverage test so a new route cannot be added without an authorization decision. Dual-control permissions are held by **no standing role**.

The database is the second enforcement layer, not a convenience: `cp_app` has no default table privileges (D-016), no `DELETE` anywhere, no `UPDATE` on append-only tables, and 12 lifecycle tables refuse a bare state change without a same-transaction transition row.

**Evidence.** 39 security tests / 462 subtests, run **twice against one database with nothing cleaned in between** (D-029 — a suite that only passes on a fresh database hides defects). **[re-verified at HEAD:** 39/39, 462 subtests, green on both runs.**]** The API-level half starts the real `cmd/api` binary and drives it over HTTP as two tenants plus an admin. **All 16 declared breaks were individually shown to make the suite fail.**

The SQL-injection control deserves naming because it is not a grep: it is a constant-derivation analysis answering "could this statement string have come from anything but source text in this repository?" — through concatenation, package constants, locals, `strings.Join`, `strings.Builder`, printf verbs, constant map and slice lookups, and function parameters resolved through every call site in the package. Result: **368 Postgres call sites across 128 packages, zero findings.**

**Known open items:** the enumeration oracle in §10.5, and D-042's accepted 400-before-401 behaviour. **And the residual risk that matters most for a security reviewer is §9.3 — dual control is a property of the application write path, re-derived from data the application's own database role can write.**

---

## 6. Agent authority boundary

This is the system's defining security property and it is enforced structurally rather than by review.

**Every tool effect available to an agent is a READ or a model call. There is deliberately no write effect.** An agent's only route to action is proposing a trade intent that independently-owned code then validates, risk-checks, reserves capital for and executes. `RAW_SIGN` and `TRANSFER_VALUE` are not permissions — not permissions an agent lacks, permissions that do not exist.

Three independent guards, which are not interchangeable:

1. **`test/security/authority_boundary_test.go`** parses imports and asserts the agent tree never reaches the authority packages. The agent that built Stage 10 was told explicitly that this test was not its to weaken.
2. **`golangci-lint` depguard** forbids `internal/agent`, `internal/strategy`, `internal/prediction`, `internal/model` and `cmd/agent-worker` from importing `internal/capital` and `internal/risk/policy`. Verified to bite: a temporary `internal/capital` import in `internal/prediction` was rejected by name.
3. **`scripts/lintfin`** forbids agent-facing code from importing `internal/signing`, `internal/wallet`, `internal/admin`, `internal/capital` or `internal/risk/policy`.

The database refuses `AGENT` as an actor independently: `capital_envelopes` and `capital_envelope_changes` carry `CHECK (... <> 'AGENT')`, `risk_policies.created_by_actor_type IN ('OPERATOR','SYSTEM','USER')`, reconciliation refuses agent resolution in Go **and** in SQL, and ledger postings refuse `AGENT` principals outright. `TestProp_AgentNeverCrossesAccount` and `TestProp_AgentNeverGainsForbiddenPermissions` constrain the principal itself.

Building the agent runtime closed a real authority hole rather than confirming a designed one: `agents` was the one lifecycle table migration 00603 did not cover, so `cp_app` could have run `UPDATE agents SET state='LIVE'` and moved an agent onto customer capital with no transition row, no approval and no evidence. Migration 00690 closed it; `TestBareStateUpdateIsRefused` proves the bare update now raises `AU001`.

**The honest caveat.** `cmd/agent-worker` has **no tests**, `internal/model`'s Anthropic client has zero callers, nothing persists a strategy (§17.2), and the worker's live-gate check is weaker than the gate controller's (§9.3). The boundary is well constructed; the runtime behind it is the least-proven part of the tree.

---

## 7. Financial invariants

Each row names the property, the strongest evidence for it, and what that evidence does not reach.

| Invariant | Evidence | Limit of that evidence |
|---|---|---|
| **Money is exact; no float ever touches it** | `scripts/lintfin` (structural, in CI, clean at this commit); `internal/money` fuzzers at 2.4–2.5 M executions each | fuzz budget is 30 s per target |
| **Postings balance; the journal is append-only** | `TestProp_PostingBalanced`, `TestProp_SwapPostingBalanced`, immutability triggers, `cp_app` has no `DELETE`/`UPDATE` | — |
| **Capital is conserved** | `TestProp_CapitalConserved` — **integration-tagged**, so `make property` alone does not run it | needs the second invocation to mean anything |
| **Reservations never oversubscribe buying power** | `TestProp_ReservationsNeverOversubscribe` (integration-tagged) **and** the PART 23 torture test: 100 × $500 against $10,000 → exactly 20/80 in **every** iteration, under row-lock and `SERIALIZABLE`, asset and envelope variants | never load-tested — `reservation_contention` measured 0 reserved (§12) |
| **Exactly one economic effect per command** | Observed at the HTTP boundary under real concurrency: 100 callers on one `Idempotency-Key` → exactly one intent row. `TestE2E_IdempotencyAcrossProcesses`, `TestE2E_ConcurrentIdenticalCommands` across a real process boundary (16 callers, 15 saw `IDEMPOTENCY_IN_PROGRESS`) | PAPER intents only; no external effect exists to duplicate |
| **A timeout is uncertainty, never failure and never success** | `TestExecutor_SubmitTimeoutLanded_AdoptedNoDuplicate`, `..._ProvenAbsent_NewAttemptOnce`, `..._BudgetExhausted_...`, `..._Unresolved_ReconciliationRequired`, `TestExecutor_SubmitRejectedButLanded_IsUnknownNotFailure`; `TestIntegration_Worker_UnresolvedSubmissionIsPausedNotFailed` | against a scripted adapter, never a real venue |
| **Crash recovery never re-submits or double-posts** | `TestExecutor_ResumeAfterCrash_EveryStepBoundary` — **17 steps × 3 phases = 51 fault-injected runs**, each crashing at a different persistence boundary and asserting exactly one landed transaction, one submission, one fill, one ledger posting, one position update | simulated crashes in one process |
| **Reconciliation converges and never double-posts** | `TestProp_ReplayConvergesAndNeverDoublePosts`, `TestProp_BalanceComparisonConverges` (both integration-tagged) | reconciles against a fake chain observer; no real external truth has ever been compared |
| **A restore reproduces the ledger byte for byte** | `restoredrill`: journal hash identical, 0 balance drift, 97 tables (§13) | local `pg_dump` on a small dataset |
| **Backtest is not live; no look-ahead leakage** | `TestProp_ClickHouse_SnapshotNeverReturnsFutureKnowledge` — 100 rapid cases against live ClickHouse | **there is no backtester** (§17.2) |
| **The audit chain detects tampering and survives key rotation** | 11 tamper subtests each rejecting on its own distinct reason; 5 key-rotation subtests; three consecutive runs on one database | local ECDSA key, filesystem archive — never KMS, never Object Lock |
| **Per-partition event order survives concurrent relays** | `TestIntegration_ConcurrentRelaysPublishExactlyOnceAndInPartitionOrder` (4 instances, 150 events, 15 partitions), `..._OrderingSurvivesConcurrentRelaysAndAMidPartitionFailure`, and `TestIntegration_RelayNeverPublishesPastARowHeldByAnotherInstance`, which manufactures a genuinely held row and asserts the rows behind it were **not even attempted** (`attempts == 0`). Both concurrency tests also assert two instances demonstrably shared a partition, because a concurrency test that never contends proves nothing | an **in-memory bus**, one machine; the repeated "verified non-vacuous by stubbing" claim has no recorded artifact (§17.4) |

Two of these were bugs the tests found rather than confirmed, which is the reason to trust the tier at all. **D-036**: `Relay.claim` uses `FOR UPDATE SKIP LOCKED`, so a claimed batch is "the oldest rows nobody else holds", not a contiguous run; the order check consulted only each partition's oldest batch row, so with another instance holding a row in the *middle* of a partition, **a consumer would have seen an aggregate's 4th event before its 2nd.** Now checked per row. **D-017**: the ledger's own property test proved the design document's `DEFICIT` normal side was unreachable.

---

## 8. Provider integration status

Labels are the PART 208 vocabulary: `CODE_COMPLETE` → `CONTRACT_TESTED` → `SANDBOX_VERIFIED` → `CANARY_VERIFIED` → `LIVE_VERIFIED`. **No integration in this repository is above `CONTRACT_TESTED`, and `CONTRACT_TESTED` here means "parses recorded fixtures".**

| Provider | Role | Label | What is genuinely unknown |
|---|---|---|---|
| Jupiter | `ExecutionAdapter` (Solana) | `CODE_COMPLETE + CONTRACT_TESTED` → `BLOCKED_EXTERNAL` (EB-011) | The on-chain instruction layout (SB-007). Also: **nothing wraps this client into `execution.ExecutionAdapter`**, so it is not reachable from the executor even with a key. |
| Privy | `WalletProvider` / `SigningProvider` | `CODE_COMPLETE + CONTRACT_TESTED` → `BLOCKED_EXTERNAL` (EB-005) | Whether `signTransaction` really is idempotent on replay (a documentation claim); the policy engine cannot resolve address-lookup-table accounts, so `programId` allow-lists are the only enforceable policy. |
| Stripe | `FundingProvider` (crypto onramp) | `CODE_COMPLETE + CONTRACT_TESTED` → `BLOCKED_EXTERNAL` (EB-003) | Everything about the live product. Sandbox itself is application-gated. |
| Helius | `SolanaDataProvider` / `ChainObserver` | `CODE_COMPLETE + CONTRACT_TESTED` → `BLOCKED_EXTERNAL` (EB-010) | `transactionSubscribe` payload shape and error bodies are **assumed** in the fixtures, not documented. |
| Fallback Solana RPC | `ChainObserver` (secondary) | `CODE_COMPLETE + CONTRACT_TESTED` | `addressTableLookups` content is **assumed** in the fixtures. |
| Anthropic | `ModelProvider` (NL strategy compiler) | see §17.2 | — |
| Redpanda / Temporal / ClickHouse / S3 | infrastructure | implemented, exercised against local containers only | managed-service behaviour, EB-014 |

**All 53 contract tests replay recorded fixtures from a local `httptest` server. Re-verified at this commit: 53 top-level tests, 68 including subtests, 0 failures.** No request has ever left this machine. A fixture-backed contract test proves the adapter parses *what we believe the provider sends*; it cannot detect that the belief is wrong. Several fixture fields are explicitly labelled **assumed** in their own READMEs.

### 8.1 SB-007 — the single most important line in this report

`internal/signing/inspect/jupiter.go:35-36` says it about itself:

> *Source: the jup-ag/jupiter-cpi IDL for program `JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4` as reproduced here from memory of that IDL; it is NOT verified against a live fetch in this build.*

`internal/provider/jupiter/transaction.go:155,183` carries the same `UNVERIFIED (blocker SB-007)` marker over the account order and data layout.

Three consequences, none of which may be softened:

1. **`FuzzDecode`, `FuzzInspect`, `FuzzSummarizeTransaction`, all 21 Jupiter contract tests, the 57-case mutation table and every signing-boundary test in this repository prove only that the code is self-consistent with an unverified assumption.** None is evidence about a real Jupiter mainnet transaction.
2. **The test that should catch an error pins the same unverified assumption.** `TestLayout_JupiterDiscriminators` (`internal/signing/inspect/layouts_test.go:158`) asserts the discriminators against `sha256("global:"+name)` — a derivation that is correct if and only if the *names* are right — and then asserts two hard-coded byte values that came from the same memory. The account orders and Swap-variant payload sizes, which determine *which account is the customer's destination* and *where the minimum-output field sits in the byte stream*, are pinned against nothing external at all. A guard that encodes the assumption it is guarding reads as coverage and is worse than no guard.
3. **A previously believed corroboration was found to be circular and was closed on 2026-09-06.** The Jupiter fake's route builder mirrored the same layout the inspector parses, both derived from one unverified source, so the two agreeing looked like confirmation and proved nothing. Only the illusion was removed; nothing about the layout has been verified.

If the layout is wrong, the inspector either rejects valid swaps (fail closed — tolerable) or misparses a variant and approves a transaction whose real minimum-output or slippage differs from the one it checked (**not** tolerable). **No canary trade may occur until SB-007 is closed against the published IDL and real recorded mainnet transactions.**

---

## 9. Production capability status

**Every capability is off.** Ten capabilities are declared as a closed set in `internal/gates/capability.go:12-23` and mirrored by a `CHECK` constraint in `migrations/00150_capability_gates.sql`.

| Capability | State | Why it cannot be on |
|---|---|---|
| `LIVE_FUNDING` | **DISABLED** | EB-001, EB-003, EB-004 |
| `LIVE_MANUAL_TRADING` | **DISABLED** | EB-001, EB-002, EB-005, EB-006, EB-010, EB-011, **SB-007**, and no venue adapter |
| `LIVE_AGENT_TRADING` | **DISABLED** | everything above plus EB-007 |
| `WITHDRAWALS` | **DISABLED** | EB-015, EB-002 |
| `SOCIAL_DATA_PERSISTENCE` | **DISABLED** | EB-008 |
| `MARKETPLACE`, `CROSS_CHAIN`, `PREDICTION_MARKETS`, `SECURITIES`, `CEX_TRADING` | **DISABLED** | out of V1 scope |

### 9.1 What activation actually requires

`gates.Evaluate` (`internal/gates/gate.go:264`) checks five conditions in order and reports the first failure:

1. the capability is named in this deployment's `CP_API_ENABLED_CAPABILITIES`;
2. a persisted row exists for `(capability, environment)` and its state is `ACTIVE`;
3. the row is inside its validity window and not revoked;
4. for high-risk capabilities, all four evidence references are present (legal review, provider contract, risk approval, security approval);
5. a proposer exists, no approver equals the proposer, and there are **at least two distinct approvers**.

Activation is a **three-principal** operation, not two: `approveRule` forbids the proposer from approving, and `activateRule` additionally forbids the approving principal from activating — *"the approving principal cannot also activate; a distinct principal is required"*. Approve, Activate and Resume each additionally require `gate:approve` **and** a step-up within 15 minutes. Agents are rejected before any query is issued.

**"No single environment variable can enable live money" is true, and I verified it.** The live-funding path needs `CP_ENV`, `CP_PROVIDER_FUNDING_MODE=live`, a name, an API key reference resolving to an `sk_live_`/`rk_live_` key over https, a webhook secret reference, a settlement chain and mint, `CP_API_ENABLED_CAPABILITIES=LIVE_FUNDING` — **and then** all five conditions above. `TestOneEnvironmentVariableCannotEnableLiveMoney` (`cmd/api/main_test.go:189`) names every capability in that variable and asserts each is still inactive against a nil row, a `DISABLED` row and an `ACTIVE`-but-unapproved row. `TestEvaluate_ConfigAloneNeverActivates` and `TestRules_SamePrincipalCannotApproveAndActivate` cover the rest, and `TestIntegration_Lifecycle_ThreePrincipals` proves it against a real database.

### 9.2 Three findings about the gate machinery that I verified and that the documents get wrong

These do not change the answer — everything is off, and every path fails closed — but a reviewer relying on `docs/compliance-gates/PRODUCTION_GATES.md` would be relying on three claims that are not true of the code.

**(a) `gates.Bootstrap` has zero callers, so a fresh deployment has no gate rows at all.** `PRODUCTION_GATES.md` states *"Fresh deployments persist a `DISABLED` row for every capability at startup (`gates.Bootstrap`)"*. `Bootstrap` is defined at `internal/gates/store.go:240` and is called from nowhere in the repository, **including tests**. No migration inserts a row and `scripts/seed` never touches the table. The first row for a capability appears only when `Admin.Propose` lazily inserts it. **This still fails closed** — the verdict is `ReasonNoGateRow` — so the outcome is right and the documented mechanism does not exist. It also means there is no persistent, auditable record that a capability is deliberately off; absence is doing the work.

**(b) The claim that no single *database edit* can activate a capability is not true.** `Evaluate` re-derives conditions 2 through 5 — state, window, evidence refs, proposer and the `approvers` JSON array — **from the same row it is checking**, and `cp_app` holds `UPDATE` on `capability_gates` (`migrations/00150:47`). One `UPDATE` writing `state`, `effective_at`, the four `*_ref` columns, `proposed_by_user_id` and a forged `approvers` array satisfies all four. Migration 00603's constraint trigger forces a matching transition row in the same transaction — but that is a second `INSERT`, not a second principal, and the write leaves no audit-chain entry because it bypassed the service. **Dual control here is a property of the application's write path, re-derived from data the application's own database role can write.** It is real against an operator using the product and not against anyone holding `cp_app` credentials. That distinction belongs in the security section of any diligence pack, and closing it needs either a `SECURITY DEFINER` activation function, revocation of `cp_app`'s `UPDATE` on this table, or a signature over the approval chain.

**(c) The agent worker's gate check is materially weaker than the gate controller's.** `internal/agent/store.go:495` `LiveTradingEnabled` deliberately does not import `internal/gates` (correct — the agent tree may not reach the gate controller) and issues its own `SELECT state, effective_at, expires_at`. It then tests **only** `state != "ACTIVE"`; `effective_at` and `expires_at` are selected and never used. Conditions 1, 3, 4 and 5 are all skipped. Its own comment says *"no environment variable can turn it on"*, which is true and beside the point: one `UPDATE` setting `state='ACTIVE'` turns it on for the agent worker while the API's checker would still refuse. Contained today only because `cmd/agent-worker` constructs an evaluator that hard-errors, so a LIVE run can produce nothing.

### 9.3 Where the gates are actually enforced — thinner than it looks

**`RequireActive` has exactly two production call sites in the entire repository**: `internal/funding/service.go:420` (`LIVE_FUNDING`) and `internal/withdrawal/service.go:123` (`WITHDRAWALS`). That is the complete list.

**`LIVE_MANUAL_TRADING` is checked at no enforcement point that has ever executed.** `POST /v1/intents` takes `mode` **from the client request body** (`internal/httpapi/handlers_trading.go:112`), validates it only against the closed set of mode names, and passes it through; `internal/intent` does not import `internal/gates` at all. The OpenAPI document declares `mode: {enum: [PAPER, LIVE], description: "Manual trades are PAPER until LIVE_MANUAL_TRADING is ACTIVE"}` — **nothing implements that sentence at the API boundary.** The capability requirement genuinely exists, in the eligibility policy (`internal/eligibility/policy.go:427` requires `LIVE_MANUAL_TRADING` for the live-manual-trade context, and `evaluate.go:206` enforces required capabilities), but `internal/eligibility` is imported only by `internal/settlement` — the settlement compiler, which runs inside `cmd/execution-worker`, **which refuses to start**. So the design is layered correctly and the layer has never run.

Today that is harmless: a `LIVE`-mode intent can be recorded, and nothing can ever pick it up. It is not harmless the day a venue adapter lands. **A `LIVE`-mode intent should be refused at the API boundary while the gate is off, so the gate's state is checked before the row exists rather than after** — Phase A, item 8 of §18.

### 9.4 Kill switches

Twelve kinds (`internal/killswitch/kind.go`): `GLOBAL_NEW_RISK_KILL`, `ACCOUNT_FREEZE`, `AGENT_PAUSE`, `STRATEGY_VERSION_DISABLE`, `VENUE_DISABLE`, `INSTRUMENT_CLOSE_ONLY`, `INSTRUMENT_HALT`, `CHAIN_DISABLE_NEW_ACTIONS`, `PROVIDER_DISABLE_NEW_ACTIONS`, `FUNDING_DISABLE`, `WITHDRAWALS_DISABLE`, `MODEL_DISABLE`. Activation is fast and ungated; **release is gated and dual-controlled** — the asymmetry is the point. An action-class matrix decides what each switch stops, and the properties that matter are tested: `TestProp_GlobalKillNeverStopsRiskReduction`, `TestProp_NewRiskNeverAllowedUnderGlobalKill`, `TestProp_EveryActiveSwitchCombination`, and `TestIntegration_KillSwitchNeverStopsReconciliation`, which activates a global kill plus an account freeze and then asserts the full recovery still runs, the fill still posts and both switches are still active (PART 247's `kill switch stops reconciliation` anti-pattern). The engine cannot consult a switch even by accident: `internal/reconciliation` never imports `internal/killswitch`.

**But release does not work in the shipped build.** `kill:release` is one of the seven approve-side permissions held only by `RoleBreakGlass`, and no break-glass elevation can be granted (§15.1b(2)). A `GLOBAL_NEW_RISK_KILL` pressed today could not be lifted through the product.

---

## 10. Test evidence

Two sources. The PART 238 matrix in `docs/build/ADVERSARIAL_VALIDATION.md` was produced by commands run on this host at commit `bbc12f8`. Everything marked **[re-verified]** below was re-run by me at HEAD (`a92b69d`) for this report; where a figure changed, the change is stated.

### 10.1 Tiers

| Tier | Command | Result | Provenance |
|---|---|---|---|
| build | `go build ./...`, `go vet -tags=integration ./...`, `go vet -tags='integration e2e chaos' ./...` | **PASS**, exit 0 for all three | **[re-verified]** — every build-tagged test file in the tree compiles |
| unit | `go test -count=1 ./...` | **82 packages ok, 0 FAIL**, 32 with no test files | **[re-verified]** |
| race | `go test -count=1 -race ./...` | 82 packages, **0 data races**, 3 m 31 s | ADVERSARIAL §5 (one manual local run) |
| property | `-run 'Prop\|Property' ./internal/...` **plus** the integration-tagged ones | PASS. **Both invocations are required** — 4 database-backed properties (capital conservation, reservation oversubscription, reconciliation convergence, balance convergence) live behind `//go:build integration` and are silently skipped by the first | ADVERSARIAL §6 |
| fuzz | `go run ./scripts/fuzzall -fuzztime=30s` | 28 targets, 0 failed, **18,410,387 executions** | ADVERSARIAL §7 — see §10.3 |
| contract | `go test ./test/contract/...` | **53 tests, 68 with subtests, 0 FAIL** | **[re-verified]** |
| integration | per-package, own fresh database | 75 packages; **74 passed at `bbc12f8`, and the one failure is now fixed** — see §10.4 | **[re-verified]** |
| E2E (Go) | `-tags='integration e2e' ./test/e2e/` | **6 tests / 26 subtests**, cross-process against the real `cmd/api` binary | **[re-verified]** |
| E2E (browser) | `npx playwright test` | 51 tests, real Chrome, real OIDC login | ADVERSARIAL §10.2 + CI `web-e2e` job on Linux/Chromium |
| chaos | `-tags='integration chaos' ./test/chaos/` | **7 tests PASS**; containers verified `healthy` before and after | **[re-verified]** |
| security | `-tags=integration ./test/security/` | **39 tests / 462 subtests**, passed **twice on one database with nothing cleaned between runs** | **[re-verified]** |
| migration | own database, own invocation | 3 tests / 11 subtests | ADVERSARIAL §14.1 |
| restore drill | `go run ./scripts/restoredrill` | journal hash identical across restore, 0 balance drift | ADVERSARIAL §14.2, artifact `dist/restore-drill.json` |
| audit verification | `go run ./cmd/audit-worker verify` | 304 events, 1 signed checkpoint covering all 304 | ADVERSARIAL §14.3 |

Scale, measured at HEAD: **1,847 `func Test*`, 40 `func Fuzz*`, 312 `_test.go` files**.

### 10.2 Negative controls — the strongest evidence here

A green suite proves nothing on its own. **29 declared "breaks" were executed, and all 29 made their suite fail**: security 16/16, chaos 6/6, E2E 7/7. Each break deliberately violates the property one guard defends; the run is then expected to fail, and did. A catalog test in each suite keeps the list from rotting — a name a test reads must be declared, and a declared name must be read.

This is the difference between tests that pass and tests that *can* fail, and it is the single best reason to take §10.1 seriously.

### 10.3 The fuzzing is a smoke test, not a campaign

18.4 million executions is 30 seconds per target on one laptop. **It is evidence that no crashing input was found inside that budget. It is not evidence that none exists.** Coverage-guided fuzzing usually needs hours to leave the shallow neighbourhood of its seed corpus.

The weakest target is the worst possible one: **`internal/signing/inspect.FuzzInspect` managed 9,479 executions** — three orders of magnitude below the strongest — and that function is the last thing standing between a signed message and customer funds. That budget must be hours, not seconds, before any live signing.

Go's fuzzing coordinator also froze its execution counter partway through several targets on this host; those counts may understate. No number was adjusted. **CI runs fuzzing at 10 seconds per target**, which is a compile-and-smoke check, not a campaign either.

### 10.4 The integration tier is stitched together by hand

There is **no single command that runs all 75 integration packages against one database and passes.** Run in parallel, 11 packages fail; serialized with `-p 1`, 5 fail; only per-package fresh databases give a real answer. The cause is structural, not accidental: `internal/event`'s fixture executes `TRUNCATE outbox_events, inbox_messages` on every test while several other suites assert on global row counts. "Integration: green" is therefore a claim about ~75 separate runs assembled by a human — exactly the kind of assembly that hides a regression.

**One correction to the recorded matrix.** `ADVERSARIAL_VALIDATION.md` §9.4 and §15.8 record `internal/risk` `TestIntegration_CountOrders_FromPersistedIntents` as genuinely failing, and conclude that `risk.CountOrders` — the input to the order-rate limit — has no passing database-backed test. **That was fixed in commit `d697b7e` and is no longer true.** Re-verified at HEAD on two fresh databases: the named test passes, and the whole `internal/risk` integration package passes (`ok internal/risk 6.876s`). The fixture now supplies the `content_hash` column that migration 00605 made `NOT NULL`. §15.8 of that document is stale and should be struck.

### 10.5 A known defect the security suite reports rather than hides

`GET /v1/intents/{id}` answers **403** for an existing record belonging to another tenant and **404** for one that does not exist, so a customer can test whether an id names a real trade intent. The same shape exists in `GetOrdersOrderId`, `GetFundingDepositsDepositId` and `PostIntentsIntentIdCancel`. `TestIDOR_DEFECT_RecordLookupDistinguishesForeignFromAbsent` passes **because it asserts the currently observed wrong behaviour** and logs it as a finding. Low severity, real, and open. Do not read "security: 39/39 green" as "no known IDOR-class issues".

Separately, D-042 records an accepted behaviour: request binding runs before authorization, so an anonymous caller gets `400` rather than `401` for a malformed identifier in a UUID- or int-typed position (measured: 138 of 299 injection probes). Accepted deliberately — the fix would require a second source of truth about which routes are public, which is a worse failure mode than a status code that reveals only what the OpenAPI document already publishes.

### 10.6 What CI actually covers — read this before trusting "18 jobs green"

CI (`ci.yml`) is green: run **`34065141796`**, commit **`af28dd8`**, **all 18 jobs**. It took six runs to get there, and each red run surfaced defects that no amount of reading the workflow files would have found — a `make staticcheck` target that had never passed anywhere, a process assertion true on Windows and false on Linux, a test double ordered by Go's randomized map iteration in the most safety-critical test in `internal/settlement`, and a slippage fallback clamped only at the low end. **That is the argument for CI, and it is also the argument for reading what CI runs.**

| CI job | Covers | Does not cover |
|---|---|---|
| `go` | build, vet, gofumpt, staticcheck, golangci-lint, `lintfin`, **unit tier**, property tier, govulncheck, gosec | **`make race` is only 7 package trees** (`capital, ledger, execution, reconciliation, event, settlement, signing`), not the whole module |
| `fuzz` | all 28 targets | at **10 s** each |
| `contract`, `migrations`, `security-scans`, `web`, `openapi-drift`, `generated-code-drift`, `workspace-packages`, `scripts-crossplatform` (macOS + Windows) | as named | — |
| `e2e`, `web-e2e` | Go E2E and Playwright on Linux/Chromium, each on its own database | — |
| `chaos` | **all 7 chaos tests**, container-pausing included (178s, 0 skips) | single-node infrastructure only |
| `load-scripts` | `k6 inspect` only | no measurements are taken in CI |
| **`integration`** | **all 40 integration-tagged packages**, each on its own database, plus the seven financial-core trees under `-race -tags=integration`, the `test/security` suite and the restore drill (1,155s) | packages are run serially, so this job is the long pole |

**RESOLVED, and the history is the point.** When this report was first written the integration job ran **zero** packages: its loop was `go list -tags=integration ./test/integration/... | grep -v migrations`, that directory contains only the migration suite, so the list was empty and the job exited 0 in **under a tenth of a second** while reporting success. All 40 packages under `internal/` and `cmd/` carrying `//go:build integration` test files — ledger, capital, settlement, execution, reconciliation, signing, funding, withdrawal, event, httpapi, identity, proof, gates, killswitch, admin, adminplane and the rest — were never run by CI, so the entire database-backed tier for the financial core existed only as manual local runs.

It is fixed in commit `af28dd8` and verified in run `34065141796`: the job enumerates packages from the build tag itself (so a new one is picked up without editing the workflow), **fails loudly if the enumeration returns fewer than two** rather than silently succeeding on an empty set, and reports `integration packages: 40`.

**Keep the finding rather than deleting it, because how it was missed is the useful part.** A job whose package list resolves to the empty set passes. Passing is not the same as checking, and a green badge was read as evidence of a tier that was never executed. Anyone auditing a CI configuration should ask what each job *ran*, not what it is *named*.

Two of the three gaps below are likewise resolved; the rest stand.

Three further gaps, each verified directly:

- **RESOLVED — the race detector now sees database-backed concurrency tests.** `make race` is still `go test -race $(RACE_PKGS)` with **no `-tags=integration`**, but CI's integration job gained a step that races those same seven trees *with* the tag (capital 165s, ledger, settlement and the rest, no data races). Before that, so `TestIntegration_ConcurrentAppendsSameStream` (100 goroutines on one audit stream), `TestIntegration_ConcurrentActivationsOneRow`, `TestIntegration_ConcurrentRelaysPublishExactlyOnceAndInPartitionOrder` and `TestIntegration_ConcurrentApprovalHappensOnce` are excluded at compile time from every `-race` invocation, local and CI. The whole-module `-race` run in §10.1 has the same gap. "0 data races" is true of the untagged tree only.
- **RESOLVED — CI's chaos job now injects real faults.** `CP_TEST_REDPANDA_BROKERS` and `CP_TEST_ARCHIVE_ENDPOINT` are set, the job takes **178s with zero skips** where it previously took 0.427s. Before that: `ci.yml` sets none of `CP_TEST_REDPANDA_BROKERS`, `CP_TEST_ARCHIVE_ENDPOINT`, `CP_TEST_CLICKHOUSE_ADDR` or `CP_TEST_REDIS_URL`, and `broker_stall_test.go:30` and `archive_refused_test.go:30` skip without them — while `make infra-up` has already started the containers. The evidence is in the timing: the chaos job reports `ok test/chaos 0.427s` where the same suite takes **28.5 s** locally, the difference being exactly the two container-pausing tests (16.5 s + 10.9 s). **The skipped broker test is the one that found the 634-second silent-success defect.** Two of the six chaos negative controls therefore never fire in CI.
- **`release.yml` has never executed.** It triggers on `push: tags: ["v*"]` and `workflow_dispatch`; the repository has **zero tags** (verified). Image signing, SBOM attestation, SLSA provenance and the OIDC deploy path are authored and unproven.
- **Branch protection cannot be configured on this repository.** It is private on a plan that does not offer branch protection (`GET /branches/main/protection` → 403, "Upgrade to GitHub Pro or make this repository public"). The `ci` aggregate job exists specifically to be a required check, and there is nothing to require it. **A red commit can land on `main` today.** Dependabot is enabled and its `npm_and_yarn` run is currently failing.

---

## 11. Chaos evidence

**7 tests pass, all 6 declared negative controls fire, and the faults are genuinely injected** — the suite's own logs record `chaos: paused cp-minio` and `chaos: paused cp-redpanda`. **[re-verified at HEAD:** 7/7 PASS in 28.5 s; `cp-redpanda`, `cp-postgres` and `cp-minio` confirmed `running healthy` both before and after.**]**

The suite's discipline is worth naming. On its first attempt in the PART 238 run, `cp-redpanda` was already paused from earlier work; the suite **detected it and refused to run** rather than producing a meaningless green. A chaos test that will run against an already-broken stack is a chaos test that proves nothing.

**What it does not prove.** Six faults are injected: paused Postgres mid-transaction, paused Redpanda, paused MinIO, a clock jump, a duplicated delivery, and a relay dying mid-pass. **Every one is a fault a human imagined and encoded.** There is no network partition between two live nodes, no partial write, no disk-full, no corrupted page, no Byzantine provider, no slow-but-not-stopped dependency, no leader-election failure, no DNS failure, and no clock skew *between* two processes. It runs against single-node Postgres, a single-broker Redpanda and a single MinIO on one machine, **so no failure mode requiring more than one node of anything can even be expressed.** Multi-AZ failover, RDS promotion, cross-instance partition and managed-service degradation are entirely untested and untestable here.

**And CI only runs five of the seven.** The two that pause containers skip silently on the runner because `ci.yml` never exports `CP_TEST_REDPANDA_BROKERS` or `CP_TEST_ARCHIVE_ENDPOINT` (§10.6). So the fault injection this section describes happens **only when a human runs it locally**.

One real production defect this tier found — in one of the two tests CI skips, which is why the tier is worth having and why the skip matters: franz-go's `ProduceSync` **blocked for 634 seconds against a paused broker and then returned `nil`** — a completely stalled broker looked like a successful publish. Neither `RecordDeliveryTimeout` nor `ProduceRequestTimeout` bounds that case. Fixed by bounding `Publish` itself and reporting a timeout as **failure**, deliberately: an unknown outcome reported as failure costs a duplicate that `event_id` dedup already absorbs, while an unknown outcome reported as success costs the event permanently (D-034).

---

## 12. Load evidence

**Read this section as a characterisation of code paths, not as capacity.** Every figure was produced on one Ryzen 9 laptop talking to a Postgres container on the same machine over loopback, against a database seeded with **one** customer, **one** instrument and **one** ledger posting. There is no network, no TLS termination, no load balancer, no connection-pool exhaustion, no noisy neighbour, no realistic data volume and no index degradation at scale. **Do not present any number here as a capacity estimate.**

| Scenario | Load | Result (PART 238 run) |
|---|---|---|
| `public_surface` | 20 VUs, 30 s | 344,456 requests, **11,479 req/s**; 861,140 checks, 0 failed; liveness p95 **2.27 ms**, readiness p95 **6.17 ms** (includes a database round trip) |
| `portfolio_read` | ramp to 25 VUs, 3 m | 10,893 requests, **0.00 % failed**; buying power p95 **19.90 ms**, holdings p95 **24.77 ms**, ledger p95 **7.05 ms** |
| `sse_clients` | 200 concurrent, 1 m | **600 held connections, 100 % received a frame** |
| `quote_load` | — | **Blocked, and blocked correctly.** All 2,401 requests answered `503 PROVIDER_UNAVAILABLE — "no execution venue adapter is configured, so no quote can be produced"`. Blocked on provider credentials, not on code. |
| `reservation_contention` | 100 VUs, 400 iterations | **Half measured, half vacuous.** 301 × `202` and 99 × `409`, and the database held exactly 301 intents: 100 concurrent callers on one `Idempotency-Key` produced **exactly one** intent. But `reserved` stayed at `0.00` — reservation happens in the execution worker, which was not running. **The "reserved never exceeds buying power" half of this scenario has never been load-tested.** Its authoritative evidence is the PART 23 torture test and `TestProp_ReservationsNeverOversubscribe`, not this script. |

An earlier run recorded in `test/load/README.md` reports 16,527 req/s for `public_surface`; the table above is the PART 238 run. Both are loopback on the same laptop and neither is a capacity figure. The 3-minute `portfolio_read` run is also far too short to say anything about pool leaks, memory growth or GC behaviour.

The error contract held under load: every refusal was `application/problem+json` with a machine-readable code and a request id, and none leaked a DSN, SQL or a secret.

---

## 13. Backup / restore evidence

`scripts/restoredrill` runs `pg_dump` → `pg_restore` → `migrate verify` → reconciliation dry-run, and it passes:

```
dump 476,773 bytes  sha256 598dafbe911678610c681f47cd1d23055a457a3e03fbc4a1efb6333222574fe7
source_version 700  restored_version 700  migration_verify_ok true
row_counts_match true  ledger_balances_recomputed_match true  journal_hash_match true
source_journal_hash   6c5fd329e7019909e6b380d8806cb318f9fa9b0784f0ea0e0b4c33b849fe5f8b
restored_journal_hash 6c5fd329e7019909e6b380d8806cb318f9fa9b0784f0ea0e0b4c33b849fe5f8b
```

97 tables, row counts match, **0 balance drift**, journal hash byte-identical across the restore. Artifact at `dist/restore-drill.json`; the drill runs in CI's integration job and uploads the artifact.

**Limits, stated as the documents themselves state them.** This is local `pg_dump`/`pg_restore` on a small dataset. The production procedure — RDS snapshot or PITR, restore into a *new* instance, application repointed, reconciliation against external truth — has **never been executed**, because no AWS environment exists (EB-012). Every RTO and RPO figure in `docs/operations/DISASTER_RECOVERY.md` is explicitly a **design objective, not a measurement**, and that document says so in its own status line. A backup that was never restored in the environment that matters is not a proven backup.

---

## 14. Observability

Implemented: structured logging with a redaction layer, OpenTelemetry traces and metrics over OTLP/gRPC, request correlation, `/v1/healthz` and `/v1/readyz` (readiness includes a database round trip), build-version and config-hash stamping verified end to end — the running API reports the version its `-ldflags` stamped, which is what makes the stamp real rather than declared.

Redaction is enforced in two independent ways (D-041): a key denylist implementing PART 190 *and* a value rule, because the credential can travel under any key or embedded in a longer message. `postgres://cp_app:s3cr3t@db.internal:5432/controlplane` renders as `postgres://cp_app:[REDACTED]@db.internal:5432/controlplane`, keeping what an operator actually reads a connection log line for.

**Limits — and this section is weaker than the others, so read it carefully.**

- `CP_TELEMETRY_OTLP_ENDPOINT` defaults to empty, which installs **no-op tracer and meter providers**, and there is no OTLP collector in `docker-compose.yml`. **Telemetry export has never been exercised against a real collector.**
- **Only 2 of the 9 binaries call `observability.Setup` at all** — `cmd/api` and `cmd/relay-worker`. `reconciliation-worker`, `execution-worker`, `audit-worker`, `agent-worker`, `workflow-worker`, `market-ingest-worker` and `migrate` export **no traces and no metrics whatsoever**, in any environment.
- **`observability.NewFinancialMetrics` — the PART 132 financial instrument set — has zero callers outside `metrics_test.go`.** It is never constructed in any binary. `cmd/reconciliation-worker` passes `reconciliation.NoopMetrics()` explicitly. **Every reconciliation SEV1, including `ledger_integrity_violation` and observer disagreement, currently increments an in-process counter that nothing reads and is then discarded.** The alert exists as a code path and as a test assertion; it has no route to a human.
- Alarms, dashboards and SNS routing exist in Terraform and have never been applied.
- `internal/notification` has a complete implementation, repository and dispatcher, and **no non-test file in the repository imports it**, so no notification is ever produced.
- Only 4 of PART 130's 11 security-event kinds are emitted.

The instrumentation is well built. It is, today, almost entirely unwired.

---

## 15. Operational readiness

### 15.1 The controls that would catch a failure in production — and where each has actually run

This is the table an on-call engineer and a due-diligence reviewer should read together. Every control below is implemented and genuinely tested. **The right-hand column is the point: not one of them has ever run against anything but containers on a developer machine, and the database-backed tests that prove them are run by no CI job at all (§10.6).**

| Control | What it would catch | Proven by | Has only ever run against |
|---|---|---|---|
| **Reconciliation** (`internal/reconciliation`, `cmd/reconciliation-worker`) | provider truth diverging from our ledger; an unknown submission; internal drift between journal and balances | PART 49 crash recovery, PART 163 operator flow, `TestProp_ReplayConvergesAndNeverDoublePosts`, `TestIntegration_InternalDriftIsASEV1LedgerIntegrityViolation`; verified twice on one database | local Postgres + **fake chain observers**. **No external truth has ever been reconciled against.** With no venue adapter and no provider credentials, the only divergence it can currently detect is internal — journal vs. balances — which is real and valuable and is not the failure mode it exists for. |
| **Kill switches** (§9.4) | a runaway agent, a bad venue, a compromised provider, a market event | 12 kinds, action-class matrix, 4 properties incl. "global kill never stops risk reduction" and "never stops reconciliation" | local Postgres. **Never exercised in an incident, never in a multi-instance deployment.** |
| **Audit chain + rotating-key verification** (`internal/audit`, `internal/proof`, `cmd/audit-worker`) | tampering with the evidence that a loss happened | per-stream hash chain under an advisory lock, 100-goroutine same-stream append, Merkle checkpoints, 11 tamper subtests each rejecting on its own distinct reason, 5 key-rotation subtests, three consecutive runs on one database | **a local ECDSA key and a filesystem directory.** Never KMS, never an S3 Object Lock bucket. `audit-worker` prints a `WARN` saying exactly that on every run. The rotation semantics are the right ones — `active`/`retired`/`revoked` resolved by the key id the row names, with `checkpoint_key_unknown`, `checkpoint_key_revoked` and `checkpoint_signature` kept strictly separate — and were built because the suite passing once per database and failing on every later run exposed the real production failure: **the first scheduled KMS rotation would have made `verify-audit` claim tampering across all pre-rotation history** (D-029). |
| **Per-partition ordered outbox delivery** (`internal/event`, `cmd/relay-worker`, `internal/reality/redpandabus`) | a consumer seeing one aggregate's events out of order; an event silently lost | D-036's per-row blocked check, present in `relay.go` as both a SQL predicate over the whole claimed set and an in-pass carry-forward; two multi-instance ordering tests that assert later rows were **not even attempted** (`attempts == 0`) while a real second transaction holds the blocking row, each also asserting the instances demonstrably shared a partition; producer idempotent with `acks=all` and a bounded `Publish` that reports an unknown outcome as failure (D-034); consumer commits only the contiguous handled prefix per partition | **an in-memory bus, not Redpanda.** Every relay ordering test uses a test recorder; the only place a real broker meets a real relay is the chaos broker-stall test, **which skips in CI**. One single-broker container even locally: no partition leadership change, no multi-broker rebalance, no cross-AZ latency. The repeated claim that the guard was "verified non-vacuous by stubbing the blocked set" has **no recorded artifact** (§17.4). |
| **Dual control** (`internal/admin`, `internal/adminplane`) | one operator moving money, raising authority, or turning on a capability alone | proposer refused **while holding both the propose and the approve permission**, so identity is the only thing refusing — proven in Go, over HTTP, and through the real `cmd/api` binary; the database refuses it independently (`CHECK (approved_by_user_id <> proposed_by_user_id)`, migration 00153); an approval that cannot travel to another record or kind, tested N×N over every dual-control kind; elevation expiry at an hour, a second, and exactly at the deadline. Each with a positive control. | local Postgres over real HTTP — **and see §15.1b: it is not operable end to end in the shipped build.** Before it was tested with the real service, `internal/httpapi`'s admin tests used in-memory fakes for every admin port, and a fake happily answers "approved" to a proposer approving their own action. **The guarantee was untested until someone wired the real database in.** |
| **SEV1 alerting** | ledger integrity violation, unauthorized signing, duplicate execution | metric counters asserted by reconciliation tests | **no alert has ever been delivered anywhere, and today none can be** — the financial metric set is never constructed and the worker passes `NoopMetrics()` (§14). |

### 15.1b Four controls that cannot currently do their job in the shipped build

Each is implemented, tested and unreachable. None is dangerous today, because nothing can execute a trade; each becomes a launch blocker the moment something can.

1. **Reconciliation can only reconcile against itself.** `cmd/reconciliation-worker` wires `DB, Clock, Records, Policy, Orders, Attempts, Metrics, Logger` and nothing else — no `Ledger`, `Observers`, `Adapters`, `Reservations`, `Wallets` or `Valuer`. Consequences: `verify` (internal consistency — journal vs. balances, reservations vs. totals) **works fully and is genuinely valuable**; `full <account>` **always exits 1** on `"reconciliation: no ledger is configured"`; the periodic sweep resolves every attempt as `DispositionUncertain` because there is no observer to prove absence, so reservations are held and nothing is ever decided; and the funding sweep records `"no chain observer configured"` and then, after the 2-hour default settlement timeout, **converts every confirmed deposit into a material new-risk-blocking MISMATCH**. Running that worker as shipped, against real deposits, would manufacture false SEV incidents.
2. **Dual control cannot complete for seven of its eight action kinds.** The seven approve-side permissions are held by exactly one role — `RoleBreakGlass = sorted(dualControlPermissions)`, and `RoleAdmin = except(allPermissions, union(dualControlPermissions, agentOnlyPermissions))` deliberately excludes them. A break-glass elevation is the only way to hold them, `BREAK_GLASS_GRANT` is itself a dual-control action, and **no production code writes `break_glass_until`** — `admin.NewBreakGlass` and `admin.PrincipalWithBreakGlass` have no non-test callers. `cmd/api` closes the loop: `AdminExecutors: map[admin.Kind]admin.ExecFunc{}`, commented *"No admin action kind is executable over HTTP in this deployment."* **So no capability gate can be approved, no ledger correction approved, no material reconciliation mismatch resolved — and a `GLOBAL_NEW_RISK_KILL` can be activated by many principals and released by none.** Fail-closed in the safest direction, and a one-way door an operator would discover during an incident.
3. **`cmd/audit-worker run` and `checkpoint` cannot start in STAGING or PROD.** `buildArchive` returns either a filesystem `DirArchive` — which `proof.NewDirArchive` refuses outright when `env.IsProductionLike()` — or `nil`; **the S3 Object Lock archive is not wired to `proof.Archive` anywhere.** `checkpointer()` then fails with `"no archive configured: wire the Object Lock archive"`. Meanwhile Terraform deploys `audit-worker = ["run"]` in all three environments with the WORM bucket and `kms:Sign` already granted, and `config.Validate` *requires* `Archive.ObjectLockRequired` and `KMS.AuditSigningKeyID` there. The evidence chain would be unwritable in the only environment where it matters.
4. **`cmd/execution-worker` cannot start in any environment** (§17.1). Correctly tested — `TestWire_RefusesWithoutAProviderBinding` asserts it refuses *before touching the database*.

### 15.2 Operator tooling

Nine binaries with CLI subcommands (`relay-worker status`, `market-ingest-worker {schema,sources,gaps,verify}`, `audit-worker verify`, `migrate {up,verify,down-to,create}`), an admin console (`apps/admin`) with RBAC and dual control, `make` targets that CI calls identically to developers, `scripts/devrun` (refuses any environment but LOCAL/DEV), and `scripts/seed` (refuses non-local hosts and any environment but LOCAL/DEV/TEST).

**19 incident runbooks**, each with trigger, blast radius, first ten minutes, diagnosis, containment, what not to do, exit criteria and post-incident review, naming real admin routes, real kill-switch kinds and real read-only SQL. Unimplemented controls are marked `PENDING` inside them. **No runbook has ever been executed in an incident, because there has never been an incident, because there has never been a deployment.**

### 15.3 Fail-closed behaviour, which is the part that is genuinely production-shaped

The system prefers refusing to degrading, in places where degrading would have been easier:

- `cmd/execution-worker` **errors out before opening the database** when no provider binding exists, rather than starting with a partial wiring.
- `cmd/relay-worker` refuses to start rather than falling back to a loopback bus — a relay that appears to run and publishes nowhere is worse than one that will not start, because the outbox drains and the events vanish.
- `cmd/market-ingest-worker` refuses an unreachable broker (`event bus: PROVIDER_UNAVAILABLE: redpandabus: brokers are unreachable`) and refuses to run with a fake chain observer.
- `POST /v1/quotes/preview` answers 503 rather than inventing a quote.
- Fake providers, the dev IdP, seed data and debug auth are refused in STAGING and PROD by configuration validation, and `test/security/production_config_test.go` proves it.
- The signing service refuses to start in DEV/STAGING/PROD unless the wallet provider reports `DelegationVerified`.
- The web app is tested for the same honesty: *"a quote the backend cannot produce is reported, not faked"*, *"no dead controls anywhere in the application"*, *"no capability that is off is shown as a zero"*.

The production configuration rules behind that are a closed, named catalogue in `internal/config/validate.go` — TLS required on Postgres (`verify-ca`/`verify-full`, not merely `require`), Redis, Redpanda, ClickHouse and Temporal; no CORS wildcard; secure cookies; https public base URL; Object Lock required; KMS signing key required; no insecure OTLP in PROD; no plain-text or `file://` secrets. `config.Validate` runs inside `config.Load`, which every binary calls first, so each is a hard startup refusal.

### 15.3b Four gaps in the fail-closed story, all verified

1. **Seven of the nine binaries default `CP_ENV` to `LOCAL` with only a stderr warning.** `agent-worker`, `audit-worker`, `execution-worker`, `market-ingest-worker`, `reconciliation-worker`, `relay-worker` and `workflow-worker` all print `WARNING CP_ENV is not set; assuming LOCAL` and continue. `cmd/api` does not. In `LOCAL`, `AllowsDefaults()` is true and **every rule listed above is skipped**: provider modes default to `fake`, auth to `dev`, TLS requirements to false, plain secrets are accepted, and `agent-worker` reads the gate row for `environment='LOCAL'`. Terraform does set `CP_ENV` (`infra/terraform/modules/app-config`), so this is not live exposure — but **the only thing standing between a worker and a fully permissive configuration is a deployment variable, enforced nowhere in the code**, and no deployment has ever run. This is the most consequential hardening gap in the tree.
2. **`Archive.ObjectLockRequired` is validated and never enforced.** `archive.S3.ObjectLockEnabled` — whose own doc comment says *"The composition root calls it at startup"* — has exactly one caller in the repository, and it is a test. A production deployment pointed at a non-WORM audit bucket would start cleanly.
3. **`Temporal.RequireTLS` is validated and never applied.** `cmd/workflow-worker` dials `client.Dial(client.Options{HostPort, Namespace, Logger})` with no `ConnectionOptions.TLS`, and nothing outside `internal/config` reads the flag. Workflow gRPC would be plaintext in PROD while the rule passes.
4. **`cmd/migrate` never enforces TLS.** It reads `CP_DATABASE_MIGRATE_URL` directly and never calls `db.Open`, so the `sslmode` guard does not apply to the one connection that holds DDL rights.

### 15.4 What operational readiness is missing

No deployment has ever happened. No `terraform plan` has ever run against AWS. No alert has ever fired. No runbook has ever been used. No on-call rotation exists. No SLO has ever been measured. No production incident has ever been survived. **Every operational claim in this section is a claim about code and documents, not about operations.**

---

## 16. External blockers

Separated per PART 240. **No test result can close any row in this table.**

### Legal (P0 — PART 241)

| ID | Blocker | Blocks |
|---|---|---|
| EB-001 | U.S. and California licensing or exemption determination | every `LIVE_*` capability |
| EB-002 | Delegated-signing custody analysis (embedded-wallet control semantics) | all live trading |
| EB-006 | Permitted asset universe decision | instrument activation |
| EB-007 | Strategy / adviser / CTA regulatory implications | `LIVE_AGENT_TRADING`, any marketplace |
| EB-008 | Provider data-retention and redistribution rights | `SOCIAL_DATA_PERSISTENCE`, long-term raw archive |

### Provider

| ID | Blocker | Software state |
|---|---|---|
| EB-003 | Stripe onramp commercial approval + production credentials (sandbox is application-gated too) | **met** — genuinely `BLOCKED_EXTERNAL` |
| EB-005 | Privy production credentials + verified signing semantics | **met** — genuinely `BLOCKED_EXTERNAL` |
| EB-010 | Helius production credentials and plan | **met** — genuinely `BLOCKED_EXTERNAL` |
| EB-011 | Jupiter API key and commercial terms | **not met.** The client is complete; **wrapping it into `execution.ExecutionAdapter` and wiring the executor is still ours to do**, so this is not `BLOCKED_EXTERNAL` end to end. Recorded honestly in the traceability document, which deliberately withheld `BLOCKED_EXTERNAL` from that row. |
| EB-012 | AWS account, IAM bootstrap, GitHub OIDC trust | **met** — `validate` + `trivy` clean, never planned or applied |
| EB-013 | Anthropic production API key and usage terms | see §17.2 |
| EB-014 | Managed Temporal / Redpanda / ClickHouse accounts | local containers only |
| EB-015 | Fiat off-ramp / withdrawal partner and custody path | withdrawal domain + gate implemented, capability refused |
| EB-016 | Tax-reporting partner | lot-level records preserved; no filing claim made |
| EB-017 | Identity provider selection + production tenant | OIDC-generic adapter; dev IdP refused in STAGING/PROD |

### Business

| ID | Blocker |
|---|---|
| EB-004 | Stripe fraud/dispute responsibility allocation → funding reversibility policy values |
| EB-009 | Final brand clearance (`PUBLIC_PRODUCT_NAME` / trademark) |

### Software (ours)

| ID | Blocker | Status |
|---|---|---|
| **SB-007** | **Jupiter v6 instruction layout reproduced from memory, UNVERIFIED** | **OPEN — blocks any canary trade** |
| SB-005 | Solana decode library selection | RESOLVED (D-019: `solana-go` v1.23.0 pinned) |
| SB-001/2/3/4/6/8 | Toolchain, Docker, tools, git remote, cgo, uncommitted build | RESOLVED |

---

## 17. Remaining risks

Ordered by what would cost the most if acted on wrongly.

### 17.1 The risks that block live capital

1. **SB-007 — the unverified instruction layout, whose own guard is circular.** §8.1. This is the top risk in the system.
2. **No execution venue adapter exists.** The system cannot execute a trade today. `bindProviders` returns an error for every mode (`cmd/execution-worker/main.go:282`), and the executor's only adapter is a test fake. This is also why quotes answer 503, which is the correct behaviour: with no venue wired, the alternatives are inventing a quote or pretending success, and both are worse.
3. **Every provider belief is untested against the provider.** §8. Fixtures marked *assumed* include Helius' `transactionSubscribe` payload shape and error bodies and Solana RPC's `addressTableLookups` content.
4. **Nothing here has touched money.** Every path exercised in this report ran in `PAPER`/`LOCAL` mode against fakes. No live gate is `ACTIVE`, no real wallet was signed with, no transaction reached any chain, no fiat moved.

### 17.1b The authority machinery is thinner than the documents claim

Verified for this report; none of it changes the answer today, all of it must be fixed before a capability moves.

5. **Dual control can be defeated by one `UPDATE` from the application database role** (§9.2b). It is enforced in the application write path and re-derived from data `cp_app` can write. This is the residual of `THREAT_MODEL.md` §5.2 — "turn live money on without authority" — that migration 00603 partly closed for the bare state update and did not close for a complete forged row.
6. **`gates.Bootstrap` has zero callers**, so a fresh deployment carries no gate rows at all and "disabled" is expressed by absence rather than by an auditable record (§9.2a).
7. **The agent worker's live-gate check ignores four of the five activation conditions** (§9.2c).
8. **`LIVE_MANUAL_TRADING` has no enforcement point that has ever executed**, and the API accepts a client-declared `mode: LIVE` (§9.3). Harmless while nothing can execute; a defect the day a venue adapter lands.
9. **`RequireActive` is called from exactly two places in the entire repository** — funding and withdrawal. Seven of the ten capabilities have no enforcement point at all, because the systems they would gate are not yet reachable from a binary.
10. **Seven of nine binaries default `CP_ENV` to `LOCAL`, disabling every production rule** (§15.3b(1)).
11. **Three production configuration rules are validated and never applied** — Object Lock, Temporal TLS, and migrate-connection TLS (§15.3b).
12. **Dual control is not operable end to end**, so a severe kill switch can be activated and never released (§15.1b(2)).
13. **Every reconciliation SEV1 is discarded**, and 7 of 9 binaries emit no telemetry at all (§14).

### 17.2 Systems that are incomplete or absent

- **Stage 12 is unbuilt.** `internal/backtest` and `internal/performance` do not exist. Backtesting, historical replay, counterfactual and calibration reporting are absent. The point-in-time store (`internal/reality`) that a backtester would sit on *is* built and its look-ahead-leakage property test passes 100 rapid cases against live ClickHouse — but **no backtester exists to be protected by it.** Do not describe this platform as having backtesting.
- **Nothing persists a strategy.** `internal/strategy` has no database code, nothing writes `compile_attempts`, and the OpenAPI document has no strategy route. A compiled strategy cannot be saved, versioned or deployed.
- **`cmd/agent-worker` has no tests.**
- **`internal/notification` has no emitters**, so no notification is ever produced.
- **Only 4 of PART 130's 11 security-event kinds are emitted.**
- **There is no completeness sweep proving every fill eventually maps.**

### 17.3 Risks in the evidence itself

- **Bounded fuzzing** (§10.3), weakest exactly where it matters most.
- **Chaos covers six imagined faults against single-node everything** (§11).
- **Load numbers are loopback on one laptop** (§12).
- **CI does not run the database-backed tier at all** (§10.6), `release.yml` has never run, and branch protection cannot be enabled.
- **The integration tier has no single green invocation** (§10.4).
- **Every guard was written by the same author as the code it guards.** The 29 negative controls are the strongest available answer — they prove the assertions *can* fire — but they still only cover failures somebody imagined. **No external red team, no independent security audit and no adversary with an incentive has looked at this system.**

### 17.4 Documents that no longer describe the repository

Found while writing this report, and material because a reviewer would rely on them.

- **`docs/threat-model/THREAT_MODEL.md` is stale and understates the system.** Its own status line fixes the analysis at 2026-09-05, and its §2 "what exists today" table — *the boundary every STRIDE row and the entire top-ten residual-risk list is scored against* — lists `internal/{signing, wallet, execution, reconciliation, settlement, quote, instruments, intent, agent, strategy, model, prediction}` and every worker binary as **absent**. All of them now exist. Rows still say "no HTTP handler exists, so nothing calls `RequireAccount` yet", "`internal/risk` absent", "`internal/admin` absent", "`TestAgentImportBoundary` absent", "no Merkle/KMS/WORM", "verifier binary absent". Eight of its ten top residual risks have since been addressed in some measure. **The error is in the pessimistic direction, which is the safer one, but the effect is that a security engineer reading it gets a picture of a repository that no longer exists and will not find the current top risks — SB-007, the missing venue adapter, fixture-only contract tests, bounded fuzzing.** It needs a re-score against this commit.
- **`docs/security/SECURITY.md`** carries the same 2026-09-05 inventory date and the same class of staleness.
- **`docs/build/ADVERSARIAL_VALIDATION.md` §9.4 and §15.8** record a failing `internal/risk` integration test that has since been fixed (§10.4).
- **`docs/compliance-gates/PRODUCTION_GATES.md` contains two claims the code does not support**: that no single *database edit* can activate a capability alone (§9.2b), and that fresh deployments persist a `DISABLED` row via `gates.Bootstrap` (§9.2a). It also says propose requires step-up (the code requires it only for approve, activate and resume) and shows a `SUSPENDED → ACTIVE` transition the code does not allow — the code is stricter there than the document. `docs/architecture/POLICY_AUTHORITY.md` repeats the first two.
- **`POLICY_AUTHORITY.md`, `THREAT_MODEL.md` and `internal/gates/checker.go:20` all cite a `config.Capabilities.Enabled` field that does not exist.** The real mechanism is `CP_API_ENABLED_CAPABILITIES`, read directly in `cmd/api/wire.go`, **absent from `.env.example`**, set only by `infra/terraform/modules/app-config`, and consulted by `cmd/api` alone — no worker reads it.
- **`docs/build/MASTER_BUILD_STATE.md` §6–§12 are stale.** §6 "Architectural changes made: None yet"; §7 "Migrations applied: None yet" (40 exist and are applied); §9 "Unresolved defects: None yet"; §11 lists Redpanda, Temporal, S3 and ClickHouse as `NOT_STARTED` when all four are implemented and locally exercised; §12's test matrix predates chaos, E2E, load measurement and CI. The narrative sections (§3b onward) are current and detailed; the numbered summary tables at the end were never updated. A reader who trusts §10–§12 will be wrong in both directions.
- **`test/load/README.md`** still contains the line "**No numbers have been measured yet**: the API binary does not exist", immediately above a section reporting measurements.
- **`README.md`** says the web app is Next.js; `apps/web` is Vite + React Router — an undocumented deviation from D-011 that the traceability refresh caught and the decision register still does not record. It also lists 7 binaries where 9 exist.
- **Traceability rows R-104-1, R-142-1, R-186-1 and R-187-1** still say "workflows authored, never executed — no git remote (SB-004)". SB-004 is resolved and those jobs have run green. **R-143-1's "workflow never run" is still correct** — that one is `release.yml`.
- **`docs/operations/RECONCILIATION.md`** still says the reconciliation engine "is not yet implemented (Stage 7)" and marks the block reader `PENDING`. Both are false — the engine exists and the reader is real SQL wired into `cmd/api`. Understates, which is the safe direction, and still wrong.
- **Traceability row R-031-1 (VERIFIED) cites the wrong test.** It offers `TestIntegration_Redpanda_PerPartitionOrderingFollowsTheAggregateKey` as evidence for the relay's ordering guard; that test contains no relay and no outbox — it exercises Kafka's own key-to-partition guarantee. The real evidence is in `internal/event` and `cmd/relay-worker`, and those tests use an in-memory bus.
- **The D-036 non-vacuity claim has no recorded artifact.** `DECISION_REGISTER.md`, `MASTER_BUILD_STATE.md` and the traceability document all state the ordering guard was "verified non-vacuous by stubbing the blocked set to empty and watching it fail". Unlike the 29 negative controls in §10.2 — each of which has a recorded name, exit code and outcome — there is no transcript, no committed patch, and **no entry in the chaos suite's own closed six-entry break catalogue for an ordering break**, though the machinery to encode one exists. The tests themselves are structurally strong (they assert later rows were *not even attempted*, `attempts == 0`, with a real second transaction holding the blocking row), so this is a gap in the *evidence for the claim*, not evidence the guard is weak.
- **`.env.example` documents zero `CP_AUDIT_*` variables** (`CP_AUDIT_ARCHIVE_DIR`, `CP_AUDIT_LOCAL_SIGNING_KEY_REF`, the retired and revoked key id lists, the three intervals) and no `CP_API_ENABLED_CAPABILITIES`. They exist only in a worker's `usage()` output and in Terraform.

---

## 18. Exact launch checklist

Ordered. Each item names the blocker, the evidence that closes it, and who can produce that evidence. **Nothing below may be reordered to reach a trade sooner** — items 1–3 exist because the alternative is signing a transaction nobody has verified.

### Phase A — before any live provider credential is loaded anywhere

| # | Gate | Evidence that closes it | Who |
|---|---|---|---|
| 1 | **SB-007 — verify the Jupiter v6 layout** | Fetch the published `jup-ag/jupiter-cpi` IDL for `JUP6Lkb…` and diff it against `internal/signing/inspect/jupiter.go`: discriminators, account orders, Swap-variant payload sizes. Then capture **real mainnet transactions** for `route` and `shared_accounts_route`, commit them as golden fixtures, and assert the inspector parses each one's minimum-output and destination account correctly. **Replace `TestLayout_JupiterDiscriminators` with a test whose expected values come from the IDL file, not from the same source as the code.** Remove the `UNVERIFIED` markers only when both halves exist. | Engineering + Jupiter documentation. No approval needed; this is ours and it is first. |
| 2 | **Fuzz the inspector properly** | `FuzzInspect` and `FuzzDecode` at ≥ 4 hours each on a machine with a warm corpus, with the corpus committed. 9,479 executions is not a result. | Engineering |
| 3 | **Independent review of the signing boundary** | An external security review of `internal/signing`, `internal/signing/inspect` and the execution-worker↔signer process boundary, by someone who did not write it. Every guard in this repository was written by the code's own author. | External security firm |
| 4 | **CI must run the database-backed tier** | Extend the `integration` job to iterate the 40 `//go:build integration` packages under `internal/` and `cmd/`, each on its own database, exactly as `CONVENTIONS.md` tells developers to run them locally. A green run that exercises `ledger`, `capital`, `settlement`, `execution`, `reconciliation` and `signing` against Postgres. | Engineering |
| 5 | **Make CI enforceable** | Branch protection on `main` requiring the `ci` aggregate check. Requires a repository plan that offers it, or making the repository public. Until then a red commit can land on `main`. | Repository owner |
| 6 | **Prove `release.yml`** | Push a `v0.0.0-rc` tag; a green release run producing a signed image, an SBOM attestation and SLSA provenance. Currently zero tags exist. | Engineering |
| 7 | **Make dual control survive a database credential** | Either move gate activation behind a `SECURITY DEFINER` function, or revoke `cp_app`'s `UPDATE` on `capability_gates`, or sign the approval chain so a forged `approvers` array does not verify. Evidence: an integration test that attempts a full forged-row `UPDATE` as `cp_app` and asserts the capability is still inactive. Today that test would fail (§9.2b). | Engineering |
| 8 | **Fix the gate machinery gaps found writing this report** | Call `gates.Bootstrap` at startup so "disabled" is an auditable row rather than an absence (§9.2a); make `internal/agent`'s `LiveTradingEnabled` evaluate the validity window and the approval chain, or have the worker consult a gate verdict computed outside the agent tree (§9.2c); refuse a `LIVE`-mode intent at the API boundary while `LIVE_MANUAL_TRADING` is off, so the gate is checked before the row exists (§9.3); correct `PRODUCTION_GATES.md` and `POLICY_AUTHORITY.md` (§17.4). | Engineering |
| 9 | **Make dual control operable** | Wire at least the `BREAK_GLASS_GRANT` executor and a writer for `break_glass_until`, then prove the loop end to end: a second human grants an elevation, a third approves a gate, and a `GLOBAL_NEW_RISK_KILL` is released. **Today a severe kill switch can be activated and never released** (§15.1b(2)). No capability may be activated in any environment until this works, because activating one requires the very approval path that does not function. | Engineering |
| 10 | **Make `CP_ENV` mandatory in every binary** | Remove the "assuming LOCAL" default from all seven workers so an unset `CP_ENV` is a startup refusal, matching `cmd/api`. Then enforce the three rules that are validated and never applied: call `ObjectLockEnabled` at startup, pass TLS options to `client.Dial` in `workflow-worker`, and route `cmd/migrate` through the same `sslmode` guard (§15.3b). | Engineering |
| 11 | **Wire the observability the alerts depend on** | Construct `observability.NewFinancialMetrics` and call `observability.Setup` in every worker, and replace `reconciliation.NoopMetrics()`. **Until this lands, a `ledger_integrity_violation` raises nothing that reaches a human** (§14). | Engineering |
| 12 | **Run the database-backed concurrency tests under `-race`** | Add `-tags=integration` to a race invocation covering `internal/{audit,event,capital,ledger,admin,gates,killswitch}`, so the 100-goroutine append, the concurrent-relay ordering tests and the concurrent-approval test are actually raced (§10.6). | Engineering |
| 13 | **Give CI's chaos job its dependencies** | Export `CP_TEST_REDPANDA_BROKERS`, `CP_TEST_ARCHIVE_ENDPOINT`, `CP_TEST_CLICKHOUSE_ADDR` and `CP_TEST_REDIS_URL` in the chaos and integration jobs, so the two fault-injection tests stop skipping silently. Evidence: the chaos job takes ~30 s, not 0.4 s (§10.6). | Engineering |

### Phase B — legal, and it does not begin until Phase A is done

These are PART 241 P0 gates. **Do not invent answers.** Each records a `legal_review_ref` on the capability gate that consumes it.

| # | Gate | Evidence | Who |
|---|---|---|---|
| 14 | **EB-001** | Written U.S. and California licensing or exemption determination covering custody, money transmission and the trading model. | External counsel |
| 15 | **EB-002** | Delegated-signing custody analysis: does a bounded delegated signer over an embedded wallet constitute custody? | External counsel |
| 16 | **EB-006** | Permitted asset universe, written, with the policy reference the asset registry stores. Unlisted assets already fail closed. | Counsel + business |
| 17 | **EB-007** | Adviser / CTA implications of typed strategies and the agent runtime. Blocks `LIVE_AGENT_TRADING` specifically and can be deferred past manual trading. | External counsel |
| 18 | **EB-004** | Stripe fraud and dispute responsibility allocation → the concrete hold-policy values the funding reversal path takes as configuration. | Business + Stripe |
| 19 | **EB-008, EB-009** | Data-retention and redistribution rights; brand clearance. Block `SOCIAL_DATA_PERSISTENCE` and the public product name, not trading. | Counsel + business |

### Phase C — provider verification, per provider, before that provider's capability moves

| # | Gate | Evidence | Who |
|---|---|---|---|
| 20 | **EB-011 + the missing adapter** | A Jupiter API key **and** an `execution.ExecutionAdapter` implementation wrapping `internal/provider/jupiter`, wired into `bindProviders`. Then: `SANDBOX_VERIFIED` — every one of the 21 contract fixtures replaced by or diffed against a real response from `api.jup.ag`. Any fixture field that differs is a defect, not a test update. | Jupiter (key) + engineering (adapter) |
| 21 | **EB-005** | Privy production credentials, and a written or empirically demonstrated answer on `signTransaction` idempotent replay — today it is a documentation claim, which is why the code classifies it `UNKNOWN_EFFECT_WRITE`. Then `delegation_verified_at` may be set; production startup fails closed without it. | Privy + engineering |
| 22 | **EB-010** | Helius credentials, and the **assumed** fixture fields (`transactionSubscribe` payload shape, error bodies) confirmed against live responses. | Helius + engineering |
| 23 | **EB-003** | Stripe onramp approval and production credentials; sandbox is application-gated too. Then re-run the contract suite against sandbox. | Stripe |
| 24 | **EB-017, EB-013, EB-014** | Production OIDC issuer and tenant; Anthropic production key and usage terms; managed Temporal / Redpanda / ClickHouse. | Business + providers |

### Phase D — environment, before any capability is enabled

| # | Gate | Evidence | Who |
|---|---|---|---|
| 25 | **EB-012** | AWS account, IAM bootstrap, GitHub OIDC trust. Then `terraform plan` and `apply` in **staging**, which is the first time any of the 12 modules has been executed rather than validated. Expect deploy-day defects; four were already found by review alone. | Business (account) + engineering |
| 26 | **Real evidence storage — and it needs code, not just an account** | First **wire `archive.S3` to the `proof.Archive` interface**; today `buildArchive` can only return a filesystem directory or nil, so `audit-worker run` and `checkpoint` cannot start in STAGING or PROD at all (§15.1b(3)). Then verify the chain with a **KMS** key and an **S3 Object Lock (COMPLIANCE)** bucket, including a rotation: add the new key, keep the old trusted as `retired`, confirm `verify` still passes over pre-rotation history (D-029). Also fix the KMS verification path, which currently reports a live KMS API failure — a throttle, timeout or `AccessDenied` — as `checkpoint_signature`, i.e. **as tampering**; the signer layer distinguishes it and the verifier discards the distinction. An audit system that cries tamper on a throttle is the failure mode D-029 exists to prevent. | Engineering, on real AWS |
| 27 | **A staging restore drill** | RDS PITR restore into a new instance, application repointed, reconciliation against external truth, with **measured** RTO and RPO replacing the design objectives in `DISASTER_RECOVERY.md`. | SRE |
| 28 | **A real OTLP collector** | Traces and metrics observed end to end from **every** binary, after item 11 wires the seven workers that currently emit nothing. Today the default endpoint is empty and installs no-op providers, so export has never run anywhere. | SRE |
| 29 | **Load against staging** | Re-run every k6 scenario against a deployed environment with realistic data volume, TLS, a load balancer and a bounded connection pool — including `quote_load`, which is only measurable once item 13 lands, and `reservation_contention` **with `cmd/execution-worker` running**, so its namesake invariant is actually exercised. Replace §12 entirely. | SRE |

### Phase E — the first live capability

| # | Gate | Evidence | Who |
|---|---|---|---|
| 30 | **`LIVE_FUNDING` first, alone** | Funding is reversible, bounded and does not sign anything, which makes it the correct first live capability. Activate through the gate's own five-condition path with dual control and step-up, with `legal_review_ref` populated from items 14–16. Then a **single small real deposit**, reconciled to the ledger and the chain before a second is attempted. | Operations, under dual control |
| 31 | **Canary trading, only after items 1, 20 and 21** | A canary account with a strict maximum exposure, a strict asset allowlist and a hard notional cap. `CANARY_VERIFIED` is claimed only from a real filled transaction reconciled against two independent chain observers that **agree** — the agreement policy never picks the optimistic answer (D-025). | Operations |
| 32 | **`LIVE_MANUAL_TRADING`** | Only after a canary has run long enough to produce reconciliation history with no unexplained mismatch, and only with the order-rate and envelope limits set deliberately rather than inherited from local defaults. | Operations, under dual control |
| 33 | **`LIVE_AGENT_TRADING`** | Last. Requires item 17, a persisted-strategy path that does not yet exist (§17.2), tests for `cmd/agent-worker`, and the full security-event emitter set. | Operations |
| 34 | **`WITHDRAWALS`** | Requires EB-015. Refused unconditionally today. | Business + counsel |

---

## 19. How to reproduce this report

```bash
export PATH="/c/Dev/tools/go/bin:/c/Dev/tools/mingw64/bin:$HOME/go/bin:/c/Dev/Nodal/bin:$PATH"
export GOTOOLCHAIN=local CGO_ENABLED=1
docker compose up -d --wait

go build ./... && go vet -tags='integration e2e chaos' ./...
go test -count=1 ./...                       # unit: 82 packages
go test -count=1 ./test/contract/...          # 53 tests, recorded fixtures only
go run ./scripts/lintfin                      # no floating point in financial packages

# every database-backed suite gets its OWN database — never the shared one
eval "$(go run ./scripts/testdb -name mysuite -export)"
export CP_TEST_REDIS_URL=redis://127.0.0.1:6380 \
       CP_TEST_REDPANDA_BROKERS=127.0.0.1:19092 \
       CP_TEST_CLICKHOUSE_ADDR=127.0.0.1:19000 \
       CP_TEST_ARCHIVE_ENDPOINT=http://127.0.0.1:9100

go test -count=1 -tags=integration ./test/security/          # run it twice; it must pass both times
go test -count=1 -tags='integration e2e'   ./test/e2e/
go test -count=1 -tags='integration chaos' ./test/chaos/     # verify `rpk cluster health` afterwards
go test -count=1 -tags=integration ./internal/<pkg>/          # ONE package per fresh database
go run ./scripts/restoredrill
```

`docs/build/ADVERSARIAL_VALIDATION.md` §16 carries the full PART 238 reproduction, including the negative controls and the load setup.

---

**Nothing in this system has ever moved real money. Until every item in Phase A and the relevant items of Phases B through D are closed with the evidence named above, that must remain true.**
