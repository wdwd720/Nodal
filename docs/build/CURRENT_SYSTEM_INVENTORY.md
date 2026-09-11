# CURRENT SYSTEM INVENTORY

> **2026-09-09.** The system is deployed. `https://api-nodal.actorvia.xyz` is a
> Render free web service, one instance, `CP_ENV=STAGING`, against Neon Postgres
> at migration 730, ZITADEL for identity and Stripe's sandbox for payments, at
> $0 fixed cost with no payment method on file at any provider. STAGING means
> every production rule with sandbox providers, so no real money can move
> through it.
>
> `CREDIT_PURCHASE` is **not** activated: `capability_gates` has no row for it,
> the verdict is `ReasonNoGateRow`, and activating it needs three distinct
> principals and four external approval references. That is the control working,
> and it is why no real Stripe PaymentIntent has been exercised through the
> deployed service.
>
> What the deployment runs, what it does not, and what was verified rather than
> claimed: `docs/operations/PROVIDER_ACTIVATION_CHECKPOINT.md` as corrected on
> 2026-09-09, and `docs/audit/AUDIT_FINDINGS.md` F-83 to F-93.
>
> Two things a reader should carry. The tier deploys **one** binary, `cmd/api`,
> so everything in the worker tier is code that exists and does not run there
> — the Credit settlement sweep is the one exception, and it moved into the
> API for the reason recorded in D-046. And the configuration hash changes with
> this batch (D-046, D-047), so the value printed in the checkpoint is
> superseded.
>
> Schema is at migration **731**. `docs/audit/LAUNCH_GATE_MATRIX.md` states which
> launch flags are true (none) and why.


Baseline frozen at git SHA **`b8da0c4ce73687e587b7d20c8abc00440afd694d`** (branch `main`, working tree
clean except the new goal document `gola.md`).

Produced for PART V of `gola.md` — "do not perform destructive rewrites before this inventory exists".
Every number below was measured on the frozen tree, not copied from a previous readiness document.

---

## 1. Baseline verification actually executed

| Check | Command | Result |
|---|---|---|
| Compile | `go build ./...` | exit 0 |
| Vet | `go vet ./...` | exit 0 |
| Unit tests (short) | `go test -short -count=1 ./internal/...` | exit 0 — 62 packages `ok`, 22 with no test files, 0 failures |
| Toolchain | `go version` | go1.27.0 windows/amd64; gcc 16.2.0 (race detector available) |
| Local infra | `docker ps` | 7 services healthy: postgres:5433, redis:6380, redpanda:19092, clickhouse:18123, temporal:7233, temporal-ui:8233, minio:9100 |

Integration, e2e, chaos, security, contract and migration suites are **not** included in that baseline
number; they need a provisioned database and are run separately during the audit (Stage 1).

## 2. Size

| Dimension | Count |
|---|---|
| Go files | 790 |
| Go lines | 209,041 |
| Go test files | 314 |
| SQL migrations | 41 (`00001` … `00701`, linear numbering) |
| TypeScript/TSX files (apps + packages, excl. node_modules) | 76 |
| OpenAPI paths | 41 |
| Service binaries (`cmd/`) | 9 |

## 3. Build system and stack

- **Backend:** Go 1.27, single module `github.com/nodal/controlplane`. Binaries: `api`, `agent-worker`,
  `audit-worker`, `execution-worker`, `market-ingest-worker`, `reconciliation-worker`, `relay-worker`,
  `workflow-worker`, `migrate`.
- **Database:** PostgreSQL, goose-style migrations embedded in `migrations/`, applied by `cmd/migrate`,
  with a `ProtectedVersion` guard that refuses destructive rollback of financial history.
- **Event transport:** Redpanda/Kafka with a transactional outbox (`internal/event`) and a relay worker.
- **Workflows:** Temporal (`internal/workflows`, `cmd/workflow-worker`).
- **Analytics:** ClickHouse. **Object storage:** S3/MinIO (`internal/archive`).
- **Cache:** Redis — used only by `internal/ratelimit` and `internal/reality/datasource`. **Not** a
  financial authority anywhere (verified by grep over `internal/`, `cmd/`).
- **Frontend:** `apps/web` (Vite/React + Playwright e2e), `apps/admin`, generated TS client in
  `packages/generated-client`.
- **Codegen:** sqlc, buf/protobuf (`proto/controlplane/signing/v1`), oapi-codegen (`internal/gen/api`).
- **Infra:** Terraform under `infra/`, container build under `build/`, CI in `.github/`.

## 4. Package inventory (non-test LOC / test LOC / test funcs)

Domain classification uses the `gola.md` value domains:
**B** = simulated capital, **C** = real capital, **X** = cross-cutting, **A** = Nodal-native economy.

| Package | LOC | Test LOC | Tests | Domain | What it is |
|---|---:|---:|---:|---|---|
| `internal/money` | 1,284 | 2,514 | 74 | X | Exact USD (integer minor units), `Quantity` (big.Int + scale), 7 rounding modes, BPS. Has a test that greps the package for float literals. |
| `internal/ledger` | 2,029 | 2,106 | 41 | X | Per-asset double-entry journal, canonical content hash, idempotent posting, chart of accounts (12 codes), DB-trigger-enforced balance/immutability. |
| `internal/capital` | 2,525 | 2,636 | 43 | X | Reservations (`ReserveRequest`, TTL ≤ 24 h), withdrawal holds, capital envelopes, envelope state machine, P&L. |
| `internal/capital/buyingpower` | 803 | 801 | 14 | X | Buying-power derivation with property tests. |
| `internal/assets` | 401 | 0 | 0 | X | Asset registry: chain/mint identity, kinds NATIVE/SPL_TOKEN/SPL_TOKEN_2022/FIAT, 6 statuses + transition table, risk classes. **No test file in-package.** |
| `internal/accounts` | 266 | 0 | 0 | X | Account registry. **No test file in-package.** |
| `internal/idempotency` | 465 | 653 | 15 | X | Idempotency store; request-hash comparison. |
| `internal/intent` | 1,814 | 1,548 | 28 | X | FinancialIntent type, canonicalisation, repository, transitions. |
| `internal/settlement` | 5,906 | 2,015 | 47 | X | Settlement Compiler: planner, executor, steps, sizing, plan repo, reason codes. |
| `internal/risk` | 1,944 | 1,553 | 25 | X | Deterministic risk kernel: policy, evaluate, canonical inputs hash, decision store. |
| `internal/eligibility` | 1,182 | 926 | 20 | X | Eligibility policy + decisions, separate from risk. |
| `internal/compliance` | 248 | 96 | 1 | X | Compliance profile only — thin. |
| `internal/gates` | 1,403 | 1,278 | 27 | X | ProductionCapabilityGate: 10 capabilities, 7 states, five-condition activation, dual control. |
| `internal/killswitch` | 1,080 | 1,039 | 22 | X | 12 kill-switch kinds, action-class matrix. |
| `internal/proof` | 2,973 | 2,314 | 49 | X | Evidence bundles, Merkle, hash-chained checkpoints, KMS signer, verifier. |
| `internal/audit` | 903 | 972 | 20 | X | Audit events, canonical serialisation. |
| `internal/event` | 1,810 | 1,863 | 51 | X | Envelope, topic registry, transactional outbox, inbox dedupe, relay ordering. |
| `internal/reconciliation` | 4,310 | 2,323 | 35 | X | Reconciliation engine and record lifecycle. |
| `internal/reality` | 4,105 | 1,879 | 36 | B | Reality Engine: point-in-time provenance, datasources. |
| `internal/prediction` | 2,077 | 1,116 | 37 | B | Prediction Ledger. |
| `internal/strategy` + `ir` | 4,199 | 3,323 | 84 | B | Typed Strategy IR, compiler, validation. |
| `internal/agent` | 6,311 | 3,053 | 97 | B | Agent runtime, tools, sandboxing. |
| `internal/positions`, `valuation` | 1,607 | 1,361 | 26 | X | FIFO lots, basis conservation, valuation. |
| `internal/funding` | 2,404 | 1,250 | 24 | C | Deposit funding lifecycle (Stripe-shaped). |
| `internal/withdrawal` | 801 | 702 | 12 | C | Withdrawal requests, destinations, velocity limits. |
| `internal/instruments` | 593 | 168 | 1 | C | External instrument registry, venues, listings. |
| `internal/execution` | 2,384 | 972 | 23 | C | Order execution. |
| `internal/quote` | 640 | 522 | 8 | C | Quote lifecycle. |
| `internal/signing` + `inspect` | 3,775 | 2,421 | 34 | C | Transaction signing, **independent Solana transaction decoding/inspection**. |
| `internal/chain` | 1,218 | 730 | 27 | C | Chain interaction, finality. |
| `internal/wallet` | 517 | 105 | 5 | C | Wallet records. |
| `internal/provider/*` | 9,295 | 4,833 | 112 | C | Jupiter, Solana RPC, Helius, Privy, Stripe adapters + provider health. |
| `internal/httpapi` | 6,524 | 4,862 | 90 | X | HTTP surface. |
| `internal/auth*`, `security` | 3,931 | 3,422 | 93 | X | Sessions, OIDC+PKCE, RBAC, tenant scoping, step-up. |
| `internal/admin`, `adminplane` | 2,696 | 3,020 | 62 | X | Admin workflows and dual control. |
| `internal/config`, `observability`, `db`, `errs`, `id`, `clock` | 5,412 | 4,553 | 171 | X | Foundations. |

## 5. Database schema (41 migrations)

Grouped:

- `00001`–`00003` extensions, outbox/inbox, idempotency
- `00010`–`00011` identity, accounts, sessions, break-glass
- `00100`–`00107` assets, ledger, capital, funding, positions, valuation, audit events, provider events
- `00150`–`00153` capability gates, kill switches, policies/decisions, admin actions
- `00200`–`00202` instruments, intents/quotes, plans/orders
- `00300`–`00301` wallets/execution, reconciliation
- `00500`–`00502` strategies, agents, predictions/tools
- `00600`–`00701` reality, backtests/performance, fiat asset kind, state-change binding, ledger grants,
  intent content hash, wallet status transitions, signing results, notifications, login attempts, outbox
  attempts, audit checkpoints, indexes, agent state binding, execution plan leases, gate state authority

Enforcement observed in SQL (not only in Go): balanced-transaction trigger, negative-balance rejection,
append-only triggers (`forbid_mutation`), role grants (`cp_app` / `cp_readonly` / `cp_ops`) with no
`DELETE` for the app role, and state-change binding requiring a transition row in the same transaction.

## 6. API surface

41 OpenAPI paths: auth/session, accounts (buying power, holdings, ledger, activity, export), assets,
instruments, quotes preview, intents, orders, funding deposits, withdrawals, event stream, provider
webhooks, admin (accounts, gates, kill switches, dual-control actions, reconciliation, instruments,
providers), health/ready/version.

**Not exposed over HTTP:** agents, strategies, backtests, predictions — despite `internal/agent` and
`internal/strategy` being two of the largest packages. That is a real gap against PART LII.

## 7. Documentation claims present in the repo

`docs/build/` contains `MASTER_BUILD_STATE.md` (94 KB), `REQUIREMENTS_TRACEABILITY.md` (368 KB),
`DECISION_REGISTER.md` (57 KB), `BLOCKERS.md`, `ADVERSARIAL_VALIDATION.md`.
`docs/` also holds `PRODUCTION_READINESS_REPORT.md`, architecture docs, 19+ ADRs, runbooks,
threat model, security docs, compliance gates.

These claim the previous goal document's Stages 0–19 are complete with "12 of 13 stopping criteria met".
Per PART VII of `gola.md`, **those claims are not accepted here**; they are audited in Stage 1 and the
results recorded in `docs/audit/INDEPENDENT_AUDIT.md`.

## 8. The decisive structural finding

The repository implements the **previous** goal: a USD-native Solana spot-trading and financial-agent
control plane. Measured against `gola.md`, that is Domain B (simulated capital) and Domain C (real
capital) only.

**Domain A — the Nodal-native economy — does not exist in any form.** Grep over `*.go`, `*.sql`, `*.ts`,
`*.tsx` for the load-bearing concepts of `gola.md`:

| Concept | Files containing it |
|---|---|
| `ValueDomain` | 0 |
| `CapitalRail` | 0 |
| `CreditOrigin` / Nodal Credit | 0 |
| `NativeAsset` (Nodal-native, creator-issued) | 0 (3 hits are Solana's *native* SOL asset kind — unrelated) |
| Bonding curve / native market engine | 0 |
| `PayoutEligibility` / provenance lots | 0 |
| `LegalCapabilityRouter` | 0 |
| `AgentAuthorityLevel` | 0 |
| `InternalCommerce` / creator economy | 0 |

Everything in §4 marked **A** is absent. This — not a defect list — is the dominant migration cost.
## 9. Productization additions: verification and the conversion request (2026-09-10)

Appended rather than folded into §5 and §6 above, because those sections are a
frozen baseline measurement and this is what was added after it. Architecture:
ADR-0025, ADR-0026, `docs/product/VERIFICATION_AND_WITHDRAWAL.md`.

### Tables and migrations

| Migration | Table | What it holds | State machine |
|---|---|---|---|
| 00761 | `compliance_profile_transitions` | every change of a person's verification state, with actor, reason, provider, provider reference and the session it came from | writes `compliance_profiles.identity_state`, `verified_at`, `expires_at`; `cp_app` holds UPDATE on the attribute columns only |
| 00761 | `compliance_profiles` (altered) | the state list grows to §20's ten; born UNVERIFIED | F-42 + edge binding + birth control |
| 00762 | `verification_sessions` | one attempt: purpose, provider, provider reference, status, jurisdiction, rule version, environment, sandbox flag, expiry. **No hosted URL** | 9 statuses, F-42 + edge binding + birth control; one open session per person (partial unique index) |
| 00762 | `verification_session_transitions` | the edges of the above | immutable |
| 00762 | `verification_checks` | the evidence: one append-only row per sub-check (IDENTITY_DOCUMENT, AGE, JURISDICTION, SANCTIONS, PEP) with outcome, provider, reference, rule version, environment and sandbox flag | append-only; `CHECK (NOT sandbox OR environment <> 'PROD')` |
| 00763 | `payout_destination_transitions` | the edges of a destination's usability | writes `payout_destinations.status` and `verified_at` |
| 00763 | `payout_destinations` (altered) | gains `country`, `masked_display`, `sandbox`; born UNVERIFIED | F-42 + edge binding + birth control; REJECTED and DISABLED terminal |
| 00764 | `payout_quotes` | the pre-commitment quote: gross/fee/net on both the Credit side and the money side, three rule versions, `minimum_ok` net of fees, expiry, consumed-once | no state column by design; immutable but for `consumed_at`; unique on `(account_id, idempotency_key)` |
| 00764 | `payout_requests` (altered) | gains `quote_id` | unchanged |

Custom SQLSTATE added: `PQ001` (quote immutability). `AD001` is reused for the
three birth controls.

### Routes

| Method | Path | Permission | Step-up | Notes |
|---|---|---|---|---|
| GET | `/v1/me/verification` | `account:read` | — | the §24 profile area: state, level, evidence, what is missing and the action for each. No PII. |
| POST | `/v1/me/verification/sessions` | `payout:create` or `withdrawal:create` | — | opens a provider-hosted session; the jurisdiction is supplied and never inferred |
| GET | `/v1/me/verification/sessions/{sessionId}` | `account:read` | — | polls the provider and ingests the decision; idempotent |
| POST | `/v1/me/verification/sandbox-outcome` | `payout:create` or `withdrawal:create` | — | **SANDBOX TIER ONLY**, refused three times over |
| GET | `/v1/me/eligibility` | `payout:read` or `credit:read` | — | withdrawal eligibility per origin bucket, with reasons |
| GET | `/v1/me/payout-destinations` | `payout:read` | — | includes disabled and rejected ones |
| POST | `/v1/me/payout-destinations` | `payout:create` | yes | takes a PROVIDER TOKEN; refuses anything that looks like an account number |
| DELETE | `/v1/me/payout-destinations/{destinationId}` | `payout:create` | yes | disables; never deletes |
| POST | `/v1/payouts/quote` | `payout:create` | — | gross, fee, net, expiry, and the provenance that would leave |

`POST /v1/payouts` gains an optional `quote_id`; `GET /v1/payouts/{id}` gains
`provenance` and `sandbox`. No new permission and no new capability were
declared: the customer role already holds `payout:create` and `payout:read`, and
verification exists to enable a payout.

### Packages

| Package | What it is |
|---|---|
| `internal/verification` | the financial verification state machine, the session and evidence model, the provider contract and registry, the composite resolver, the §24 snapshot, the sandbox control |
| `internal/verification/rules` | the versioned age, country and sanctions rule tables (`verification-rules-v1-us-only`) |
| `internal/provider/verifysandbox` | the rehearsal identity provider: refuses PROD, decides nothing on its own, has no default outcome |
| `internal/eligibility/withdrawal.go` | the pure per-origin withdrawal explanation composing policy, gates, verification, jurisdiction and provider |
| `internal/payout/destination.go`, `quote.go`, `provenance.go` | the destination lifecycle and token validation, the pre-commitment quote, the provenance read model |
| `internal/httpapi/handlers_verification.go`, `handlers_eligibility.go`, `handlers_payout_destinations.go`, `ports_verification.go`, `wiring_verification.go` | the HTTP surface and its adapters |

### What did NOT change

The value-domain isolation, the payout state machine's thirteen states, the
ledger paths, the fail-closed `valuedomain.DefaultPolicy`, the capability gates,
and the rule that no approval reference is fabricated. `internal/verification`
imports neither `internal/credit` nor `internal/ledger`, and a test counts the
Credit tables across a full verification to keep it that way.
