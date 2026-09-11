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
## Addendum — 2026-09-10: the Nodal-native market's product surface (M)

This section is an addendum rather than an edit: §§1–8 above are a dated
snapshot of a baseline audit and stay as they were written. What follows is what
the productization wave added for goal §§11–16, §35, §46, §47 and §51.

### Migrations added (schema is now at 775)

| Migration | Table / function | Who may write it | Why it exists |
|---|---|---|---|
| `00771` | `native_market_prints` | `cp_app` INSERT + SELECT; a BEFORE INSERT trigger recomputes every price from the fill and refuses a disagreement (NM001); `forbid_mutation` on UPDATE/DELETE | the price series a chart reads: one print per fill with both spot prices, the effective price and both volumes |
| `00772` | `native_positions` | **nobody but the triggers**: `cp_app` has SELECT only | per (account, asset) quantity, average cost basis, realised P&L and fees, maintained from the fills and the creator allocation. A table CHECK states `quantity = allocation + bought - sold` |
| `00772` | `cp_native_positions_unreconciled()` | read-only function, EXECUTE to `cp_app`/`cp_readonly`/`cp_ops` | compares every position to `ledger_balances`, the source the triggers do not write. It reports; it never repairs |
| `00773` | `native_market_safety_policies` | `cp_app` INSERT + SELECT; immutable | the versioned, hashed market-safety document (§47) |
| `00773` | `native_market_breaker_events` | `cp_app` INSERT + SELECT; immutable | the evidence behind a circuit-breaker pause: policy, window, both prices, the move |
| `00774` | `demo_seed_rows` | `cp_app` INSERT + SELECT; immutable; a CHECK refuses `environment = 'PROD'` | the idempotence key and the label for sandbox demo data |
| `00775` | indexes | — | discovery: a `simple` tsvector GIN over name+symbol+description, a `text_pattern_ops` btree on `upper(symbol)`, and `native_markets (created_at DESC, id DESC)` |

No table above holds a fact that is not derivable from a table that already
existed, which is why every one of them is either database-maintained or
database-validated.

### Routes added

| Method + path | Permission | Notes |
|---|---|---|
| `GET /v1/native-markets` | `native_asset:read` | the markets page: cursor pagination, status/creator/`q` filters, five sorts. The response says which orderings are stable under paging (only `NEWEST` is) |
| `GET /v1/native-markets/{marketId}/summary` | `native_asset:read` | the asset detail / trading screen: the same summary row the list returns, the **limits in force** (both the market-safety policy and the risk kernel's GLOBAL concentration limits), and holder concentration |
| `GET /v1/native-markets/{marketId}/candles` | `native_asset:read` | OHLCV over a bounded window (`1m`, `5m`, `15m`, `1h`, `1d`; ≤ 1,500 buckets). Empty buckets are absent, never filled forward |
| `GET /v1/native-markets/{marketId}/trades` | `native_asset:read` | the public tape. It carries **no account identity**, asserted over the type |
| `GET /v1/me/portfolio` | `credit:read` (+ per-request tenant scope) | the Credit balance breakdown from `internal/credit` unchanged, the native positions, the totals, an explicit `as_of` and a value temperature |
| `GET /v1/me/activity` | `account:read` / `account:read_any` (+ tenant scope) | the §16 timeline: seven kinds, each amount with its unit, origin and temperature, a reference and a server-built summary |

`GET /v1/accounts/{accountId}/activity` is unchanged and remains the hosted
rail's operational timeline (D-066).

### Packages added

- `internal/activity` — the unified timeline. Owns no table; the union is one
  compiled-in constant and the kind filter is a bound parameter.
- `internal/demo` — the sandbox demo seeder. Drives the domain services; refuses
  PROD in Go, in `cmd/api` and in SQL.
- `scripts/demodata` — the same seeder as a command.
- `scripts/marketsafety` — records a market-safety policy, and prints the
  compiled-in one.

### State machines

No new state column. The circuit breaker moves a market through the status
machine migration 00712 already enforces — `ACTIVE → CLOSE_ONLY` (and
`CLOSE_ONLY → ACTIVE` when an operator resumes) — by writing a
`native_market_transitions` row as the SYSTEM actor `market:circuit-breaker`,
so a pause is a recorded transition like any other and a second state column
cannot disagree with the first.
## 9. Product surfaces added during productization — profile, terms and account lifecycle

> Appended 2026-09-10. Sections 1–8 above are a snapshot taken at migration 731
> and are not restated here; this section records what the profile/account
> lifecycle work added, so the numbers in §5 and §6 are known to be that
> snapshot's rather than silently stale.

**Migrations 00756–00760.**

| Migration | Adds |
|---|---|
| `00756` | `user_profiles` — one row per user: display name, optional unique handle, locale, time zone, avatar seed, four onboarding timestamps. Two triggers: nothing is born onboarded (`PROFILE_BORN_ONBOARDED`), a step is stamped once (`PROFILE_STEP_RESTAMPED`). No personal data column, asserted against `information_schema` |
| `00757` | `user_status_transitions` + the F-42 pair for `users.status`: flag-edge on insert, a SECURITY DEFINER writer, a deferred constraint trigger binding the edge, `REVOKE UPDATE ON users FROM cp_app` with `GRANT UPDATE (email_hash)` back for the row lock |
| `00758` | `account_closure_requests` + `account_closure_request_transitions`: PENDING → CANCELLED \| REFUSED \| EFFECTED, one open request per user (partial unique index), a birth control, and a writer that refuses EFFECTED before `cooling_off_until` |
| `00759` | `terms_acceptances` — append-only, one row per (user, document, version, sha256 of the bytes shown), with the acting session, address and user agent |
| `00760` | `operator_roles.role` gains a CHECK: every declared role except BREAK_GLASS, paired with `operatorroles.Directory()` in `test/integration/enums` |

**Migrations 00798-00800** (the accounts-auth audit fixes, F-174 to F-181).

| Migration | Adds |
|---|---|
| `00798` | The closure cooling-off period is compared against `statement_timestamp()` rather than the caller-supplied `occurred_at`; `cp_transition_stamp_is_honest` bounds a transition stamp to two minutes either side of the database clock; the closure and user-status EDGE SETS become CHECK constraints on their transition tables, paired with `profile.ClosureEdges()` and `profile.UserStatusEdges()` in `test/integration/enums` |
| `00799` | `operator_role_transitions` + the F-42 pair for the operator directory: an append-only record of every revocation and expiry with its actor and reason, a SECURITY DEFINER writer, an immutability trigger over the provenance columns and a one-way `revoked_at`, `REVOKE UPDATE ON operator_roles FROM cp_app` with `GRANT UPDATE (reason)` back for the row lock |
| `00800` | `operator_roles.role` drops `CUSTOMER` from its CHECK: it is what a principal the directory says nothing about already is, and naming it issued an OPERATOR session with no operator permissions |

**Routes added (10).** All under `/v1`; permissions are the existing ones.

| Method | Path | Permission | Notes |
|---|---|---|---|
| GET | `/me` | `account:read` \| `account:read_any` | *extended*, additively: `profile`, `onboarding`, `break_glass_until` |
| POST | `/me/profile` | `account:read` \| `account:read_any` | mutating; display name, handle, locale, time zone |
| GET | `/me/terms-acceptances` | `account:read` \| `account:read_any` | serves the document text, version, hash and counsel-review flag |
| POST | `/me/terms-acceptances` | `account:read` \| `account:read_any` | mutating; idempotent per (document, version, bytes) |
| GET | `/me/account` | `account:read` \| `account:read_any` | status, restrictions written for the user, the closure request |
| POST | `/me/account/close` | `account:read` \| `account:read_any` | mutating, **step-up** |
| POST | `/me/account/close/cancel` | `account:read` \| `account:read_any` | mutating, deliberately **no** step-up (D-055) |
| GET | `/me/security` | `session:list_own` | derived from the session store and the session's claims |
| GET | `/admin/users/{userId}` | `account:read_any` | read-only support view (§38) |
| POST | `/admin/users/{userId}/closure` | `account:freeze` | mutating, **step-up**; CANCEL \| REFUSE \| EFFECT |

`Account` also gained a read-only `owner_user_id`, so a surface that can list
accounts can reach the person who holds one.

**Packages added.**

- `internal/profile` — the product record, the terms view, the account lifecycle
  and the operator support view. Imports no money, no ledger and no verification
  domain.
- `internal/terms` — the five legal documents as embedded Markdown with one
  version constant. A leaf: standard library only.
- `internal/operatorroles` — the declaration format, the roles the operator
  directory may name, and the production rule. A leaf: `internal/security` and
  `internal/errs` only, so `internal/config` can validate a deployment's
  declaration without importing the login path.

**State machines added.**

- `account_closure_requests.state`: `PENDING → {CANCELLED, REFUSED, EFFECTED}`;
  all three destinations terminal, no edge out of them, no self-edge. A new
  request after a decision restarts the cooling-off period.
- `users.status`: `ACTIVE ↔ SUSPENDED`, both → `CLOSED`, `CLOSED` terminal. The
  values existed since 00010; 00757 is what bound them.

**Configuration added.** One variable, `CP_AUTH_BOOTSTRAP_OPERATORS`
(ADR-0024). PROD accepts only an empty value or exactly one ADMIN.
---

## Productization wave — notifications, realtime and the customer's own audit trail (2026-09-10)

Appended rather than rewritten: the document above is a frozen baseline, and
this section records what the notification/realtime work added to it. See
ADR-0028 and D-069..D-072.

### Routes added (all under `/v1`, all `sessionCookie`, all `account:read`)

| Method + path | Operation | Permission | Mutating | Port | Scope |
|---|---|---|---|---|---|
| GET `/me/notifications` | `GetMeNotifications` | `account:read` | | `Notifications` | the caller's own; cursor, `unread`, `kinds` |
| GET `/me/notifications/unread-count` | `GetMeNotificationsUnreadCount` | `account:read` | | `Notifications` | the caller's own |
| POST `/me/notifications/{notificationId}/read` | `PostMeNotificationsNotificationIdRead` | `account:read` | M | `Notifications` | the caller's own |
| POST `/me/notifications/read-all` | `PostMeNotificationsReadAll` | `account:read` | M | `Notifications` | the caller's own |
| GET `/me/notification-preferences` | `GetMeNotificationPreferences` | `account:read` | | `Notifications` | the caller's own |
| PUT `/me/notification-preferences` | `PutMeNotificationPreferences` | `account:read` | M | `Notifications` | the caller's own |
| GET `/me/audit` | `GetMeAudit` | `account:read` | | `MeAudit` | the caller's own security + account history |

`account:read_any` appears on none of them, deliberately: an operator reads the
audit trail, not somebody's inbox (D-070). PUT is the surface's first, so PUT
joined the CORS preflight's allowed methods — the deployed topology is
cross-origin, and a method the router mounts and the preflight does not name is
a route that works from curl and fails from the browser the product ships.

`GET /v1/events/stream` is unchanged as a route and **changed as a fact**: it
carried heartbeats only since it was built, and now carries `notification.created`
and `data.changed` (D-071).

### Tables and migrations added

| Migration | Change |
|---|---|
| `00781_a_notification_is_a_fact_of_the_transaction_that_caused_it.sql` | `notifications`: kind CHECK widened to 21 values (13 product + 8 inherited from 00640), `sandbox boolean` added, `notifications_user_kind_idx`, the immutability guard widened to `severity`, `sandbox`, `resource_type`, `resource_id` |
| `00782_a_user_may_decline_a_kind_of_notification.sql` | `notification_preferences (user_id, kind, channel, enabled, updated_at)`, PK `(user_id, kind, channel)`, `channel` CHECK admits `IN_APP` and nothing else |
| `00783_a_follower_remembers_where_it_stopped.sql` | `notification_follower_cursors (source, last_at, last_id, pending_at, emitted, updated_at)` — the in-process follower's position per source; `pending_at` dropped again by 00803 |

Schema is at migration **783** after this batch.

### The audit fixes on top of it (2026-09-10)

| Migration | Change |
|---|---|
| `00801_a_notification_carries_the_instant_it_became_visible.sql` | `notifications.inserted_at timestamptz NOT NULL DEFAULT clock_timestamp()` (backfilled from `created_at`), `notifications_user_inserted_idx (user_id, inserted_at, id)`, the immutability guard widened to the new column. `Last-Event-ID` resume filters on it: `created_at` is the occurrence and was skipping every row the lap wrote (F-186, D-104) |
| `00802_an_agent_names_a_version_of_the_strategy_it_names.sql` | `strategy_versions` UNIQUE `(strategy_id, id)`; `agents` composite FK `(strategy_id, strategy_version_id)` → `strategy_versions (strategy_id, id)`, MATCH SIMPLE so a DRAFT agent with no version stays legal (F-187, D-105) |
| `00803_two_capabilities_the_grants_claimed_and_the_table_refuses.sql` | REVOKE SELECT and DELETE on `notifications` from `cp_readonly` and `cp_ops` (ADR-0021 §4; the DELETE was refused by the table's own guard anyway); `notification_follower_cursors.pending_at` dropped — written and cleared in one transaction, so no session could ever observe it (F-188, F-191, D-106) |

Schema is at migration **803** after the fixes.

### The agents-completion wave (2026-09-11)

| Migration | Change |
|---|---|
| `00811_a_structured_strategy_is_an_authoring_path.sql` | `STRUCTURED_SANDBOX` added to the `source_kind` CHECK on `strategies`, `strategy_versions` and `compile_attempts`. A document assembled field by field from a declared strategy is not natural language, and recording it as `NATURAL_LANGUAGE` would say a model read somebody's words when none ran (D-129). `ir.AllLineageSources()` is the Go half, paired in `test/integration/enums` |
| `00812_a_sandbox_compiled_version_says_so_and_cannot_exist_in_prod.sql` | `strategy_versions.sandbox boolean NOT NULL DEFAULT false` and `.environment text NOT NULL DEFAULT ''`; CHECK `source_kind <> 'STRUCTURED_SANDBOX' OR sandbox`; CHECK `NOT sandbox OR (environment <> 'PROD' AND environment <> '')`; both columns added to `strategy_versions_guard`'s immutable set, so `cp_app` — which holds UPDATE for acceptance — cannot clear the label on a row it wrote (ADR-0023, D-129) |
| `00813_the_constraints_a_person_declared_are_not_part_of_their_description.sql` | `strategies.constraints jsonb NOT NULL DEFAULT '{}'::jsonb`. They were concatenated onto the description, which made "what you wrote" untrue on every screen that shows one back, and left a compiler no way to claim it had not read the prose (D-129) |

Schema is at migration **813** after the wave.

**Routes added:** one.

| Route | Floor | Notes |
|---|---|---|
| `POST /v1/strategies/{strategyId}/versions/{version}/accept` | `strategy:write` + **StepUp** | the review step of goal §18, recorded. Owner-only (`NOT_FOUND` for a stranger), `COMPILED`-only, idempotent, and the body must echo the version's `ir_hash`. The only writer of `strategy_versions.status = 'ACCEPTED'` anywhere; before it, no agent could be created on any deployment (F-255, D-128) |

**Reads that changed:** `GET /v1/strategies` and `GET /v1/strategies/{id}` carry
`compiler: { name, sandbox, structured }` beside the existing
`compiler_configured`, so the create-agent page knows WHICH compiler answered and
whether what it produces is a rehearsal. `StrategyVersion` carries `sandbox`,
`environment`, `accepted_by_user_id` and `accepted_at`; `Agent` carries
`sandbox`, read from its strategy version rather than stored on the agent;
`CompileResult` carries `rationale`. `CreateStrategyRequest.constraints` is now a
declared `StructuredStrategy` schema rather than `additionalProperties: true`.

**Packages added:** one. `internal/provider/compilersandbox` — a sandbox tier's
structured strategy compiler, beside `payoutsandbox` and `verifysandbox`. It
compiles only a fully declared strategy, calls no model, reads no natural
language, and refuses PROD at construction (D-129).

**New failure code:** `STRUCTURED_CONSTRAINTS_REQUIRED`, recorded on a
`compile_attempts` row with outcome `REJECTED` and `stage_reached PROMPT` when
this deployment's compiler reads a declared strategy and the one it was handed
is absent or incomplete. Every field it needed is named in
`compile_attempts.clarifications` — a column 00500 created and nothing wrote
until now, along with `explanation`.

**New lineage source:** `STRUCTURED_SANDBOX`.

**Boot steps added (sandbox tier only, all refused in PROD):**
`priceToolAtBoot` registers the `sandbox_price_spot` READ_MARKET_DATA tool a
declared price dependency reads through — `tools` had no seeder at all, so every
compile would have answered `UNKNOWN_TOOL`; `sandboxVenuePolicyAtBoot` records a
GLOBAL risk policy version whose venue allowlist is the venues the registry
holds, because the compiled-in policy's allowlist is empty and permits none
(F-257).

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
| `internal/notifications` (new) | the product notification centre: `Producer.Emit` (in the caller's transaction, idempotent on user/kind/ref/occurrence), the read side, preferences, and the six-source `Follower` |
| `internal/stream` (extended) | `notification.created` and `data.changed`, per-user addressing, a Broadcast flag, time-encoded event ids, durable resume, a per-user stream cap |
| `internal/notification` (unchanged) | the 2026-09 package. Still has no production caller; kept because deleting it deletes its tests (D-070) |
| `cmd/api/notifications.go` (new) | the follower's ticker, the hub publisher, the durable resume hook |

### State machines

None added. The follower READS six existing transition tables and adds no state
column of its own; `read_at` is a nullable timestamp, not a state, and
`notification_preferences.enabled` is a person's answer rather than a machine's.
## 9. Addendum — the agent product surface (2026-09-10, productization)

§6 above said "**Not exposed over HTTP:** agents, strategies, backtests,
predictions". Half of that is no longer true and the half that is has not moved.

**Routes added** (nine; permission is the boundary floor, and tenant scoping is a
separate per-request check):

| Method + path | Permission | Notes |
|---|---|---|
| `POST /v1/strategies` | `strategy:write` | records a description; compiles nothing |
| `GET /v1/strategies` | `strategy:read` | carries `compiler_configured` |
| `GET /v1/strategies/{strategyId}` | `strategy:read` | the compiled version and its human-readable form |
| `POST /v1/strategies/{strategyId}/compile` | `strategy:write` | writes a `compile_attempts` row on every path |
| `POST /v1/agents` | `strategy:write` | from a compiled strategy version; levels 4-6 refused |
| `GET /v1/agents` | `strategy:read` | with the whole authority ladder |
| `GET /v1/agents/{agentId}` | `strategy:read` | limits, budget, last run, honest runtime |
| `POST /v1/agents/{agentId}/{action}` | `strategy:write` | enable, pause, resume, disable, archive |
| `GET /v1/admin/agents` | `account:read_any` | operator read model |
| `POST /v1/admin/agents/{agentId}/pause` | `kill:activate` (+ `agent:pause` and an OPERATOR actor in the domain) | the customer role holds `agent:pause`, so it cannot be the floor of an admin route |

**Tables added:** one. `agent_grants` (migration 00786) — what the OWNER granted
an agent: authority level, Credit ceilings, universe, frequency, who granted it
and when. Immutable but for `archived_at`, enforced by a guard trigger raising
SQLSTATE `AG005`. `cp_app` holds `SELECT, INSERT, UPDATE (archived_at)` and
nothing else.

**Packages added:** one. `internal/agents` — the management surface, deliberately
distinct from `internal/agent`, which stays an import-restricted agent tree
(ADR-0029, D-073).

**What still is not exposed over HTTP (2026-09-11):** backtests, predictions,
agent decision history, and agent performance. The first two have no product surface in this
wave; the third exists as immutable `agent_lifecycle_transitions` rows with no
read route yet; the fourth is not built at all (`internal/backtest` and
`internal/performance` do not exist).

**What did not change:** the agent runtime is still inert at all three layers
(no deployed worker, no evaluator inside the worker that exists, no production
caller for the runtime's own services), so F-65's deferral of the kill-switch
bridge is undisturbed and `test/security` still watches the premise.
## 9. The operator console (`apps/admin`), 2026-09-10

Added here because §3 named the app and nothing described it. Vanilla TypeScript,
no framework and no bundler: `build.mjs` uses Node's own
`module.stripTypeScriptTypes` to emit browser ES modules, and the only runtime
dependency is `openapi-fetch`, already pinned for `packages/generated-client`.
`server.mjs` is a loopback dev server that reverse-proxies `/v1` and `/auth` so
the console is same-origin with the API — which the `SameSite=Lax` session
cookie and the Fetch Metadata CSRF guard require. It is not a deployment.

| Surface | Route | Reads | Writes |
|---|---|---|---|
| Action queue | `#actions` | `GET /v1/admin/actions` | propose, approve, reject, execute (`cancel` has no HTTP route and says so) |
| Reconciliation | `#reconciliation` | `GET /v1/admin/reconciliation/records` | resolve (material resolutions demand the approved action id); no `compensation` form, deliberately |
| Kill switches | `#kill-switches` | `GET /v1/admin/kill-switches` | activate (one operator, no step-up), release (step-up; SEVERE needs an approved action) |
| Capability gates | `#gates` | `GET /v1/admin/gates`, `/v1/admin/gates/{capability}/history` | propose, approve, activate, suspend, resume, revoke, **sandbox, unsandbox** |
| Accounts | `#accounts` | `GET /v1/admin/accounts`, `/v1/admin/users/{userId}`, `/v1/admin/agents`, `/v1/accounts/{id}`, `/v1/accounts/{id}/activity`, `/v1/credits/balance`, plus the reconciliation, action, gate and kill-switch lists for one account | account status; decide a closure request the customer opened; pause one agent |
| Withdrawals, envelopes, agent promotion | `#withdrawals` `#envelopes` `#agent-promotion` | `GET /v1/admin/actions`, filtered by the surface's action kinds | none — each links into the one queue |
| Break-glass | `#break-glass` | `GET /v1/admin/actions` | propose `BREAK_GLASS_GRANT` |
| Providers | `#providers` | `GET /v1/admin/providers` | none |

What the console will not do, each for a reason on screen: fabricate an approval
reference (a high-risk proposal still requires all four); render a `SANDBOX` gate
as an approval (ADR-0023 — its own hue, a dashed pill, and the words "sandbox —
not an approval", in the verdict column and in the transition history alike);
show a balance-editing control (none exists: account-scoped writes go through
`RequireAccountOwner`, which has no operator override); originate a closure
request (no route closes an account nobody asked to close, so no control here
could); or leave a marker instead of a decision (`src/scan.test.ts`).

Two generated documents are its authority and neither is hand-written:
`src/generated/authority.json` (permissions, roles, action kinds, surfaces,
step-up windows, capabilities, kill-switch severities) and
`src/generated/decisions.json` (a decision vector per principal/action/verb/instant),
both produced by `internal/adminplane` and held equal to the Go tables by
`TestAuthorityGolden` and `TestDecisionVectorsGolden`. The console fetches them
at runtime from `dist/`, so `build.mjs` verifies its own copy and
`TestConsoleBuiltArtifactsMatchTheirSource` compares `dist/` to `src/` whenever
`dist/` exists.

**Closed, 2026-09-10 (later).** Every gap this section recorded has since been
closed by the backend, and the console uses each one rather than describing it:
`GET /v1/admin/gates/{capability}/history` names the operator or the SYSTEM actor
`config:CP_API_SANDBOX_GATES` that moved a gate, and every gate with a row now
shows its whole recorded history; `Account` carries `owner_user_id`, so
`GET /v1/admin/users/{userId}` renders the support view and
`POST /v1/admin/users/{userId}/closure` decides a closure request the customer
opened; `GET /v1/admin/agents` and `POST /v1/admin/agents/{agentId}/pause` fill
the agents panel; `providerCatalog` describes the Credit purchase and payout
slots, so `sandbox_payout` renders as sandbox; and the `Capability` enum names
all twenty, which retires the drift notice D-079 introduced — `scan.test.ts`
holds the enum and the authority document equal instead, so a future divergence
fails CI rather than reaching an operator.

What remains, stated in the views rather than worked around: the action queue has
no account filter, so one account's controlled actions are filtered client-side
over the newest page and the view says so; and `internal/adminplane` does not
export `gate.sandbox` / `gate.unsandbox` writes, so those two steps are gated on
`gate.propose` — the permission `gates.Admin.sandboxOp` actually requires.

---

## Addendum — 2026-09-10: the cross-domain wiring (W)

The seams the domain branches left for each other, closed. Nothing here is a new
product surface: it is the wiring that makes the surfaces already built reach
each other, plus the passes that make the clocks behind them run.

### Routes added

None. Every route in this addendum already existed; what changed is what the
existing ones answer with.

### Migrations added

None. Every table read here was created by another branch's migration; the
schema stays at 00786.

### Packages added

None. `internal/activity`, `internal/notifications`, `internal/agents`,
`internal/payout`, `internal/eligibility`, `internal/profile`,
`internal/verification` and `cmd/api` all gained code.

### What the existing surfaces now carry

| Surface | Before | Now |
|---|---|---|
| `GET /v1/me/activity` | 7 kinds: Credit purchases, reversals, native trades, asset creation, payouts and admin adjustments | 18. The 11 added are `VERIFICATION_UPDATED`, `PAYOUT_DESTINATION_ADDED`, `PAYOUT_DESTINATION_DISABLED`, `TERMS_ACCEPTED`, `ACCOUNT_CLOSURE_REQUESTED`, `ACCOUNT_CLOSURE_DECIDED`, `AGENT_CREATED`, `AGENT_PAUSED`, `AGENT_RESUMED`, `AGENT_DISABLED`, `NATIVE_MARKET_PAUSED` (D-081) |
| `GET /v1/me/notifications` | 6 follower sources; `VERIFICATION_UPDATED` and `AGENT_PAUSED` declared with no producer | 8 sources. `compliance_profile_transitions` (7 of the 10 states) and `agent_pauses` (a pause somebody other than the owner opened), plus a `cmd/api` publisher that emits the same `AGENT_PAUSED` immediately and dedups against the follower on the pause row (D-082) |
| `POST /v1/payouts/quote`, `POST /v1/payouts` | succeeded without the withdrawal disclosure | refuse with `TERMS_ACCEPTANCE_REQUIRED` (422), in the domain service, naming the document (D-083) |
| `GET /v1/me/eligibility` | six reasons | seven: `TERMS_NOT_ACCEPTED`, which lowers the verdict and changes no withdrawable figure (D-083) |

### Error codes added

`TERMS_ACCEPTANCE_REQUIRED` → 422. Deliberately not `VERIFICATION_REQUIRED`,
which would send somebody into an identity flow they may already have finished,
and not `FORBIDDEN`, which says the account may not do this at all.

### In-process passes added to `cmd/api`

The launch tier deploys one web service and no workers, so periodic work runs
here or nowhere — the answer D-046, F-118 and D-069 already gave.

| Pass | Cadence | What it does |
|---|---|---|
| `runVerificationExpiry` | 5 min | VERIFIED profiles past `expires_at` → EXPIRED through the real transition, under SYSTEM `verification:expiry-sweep`. Finishes D-061; the resolver already reported the base level without it (D-084) |
| `runPayoutSweeps` | 15 s | submit every reserved (VERIFIED) request to the single configured provider; apply the provider's answer to SUBMITTED / PROVIDER_PENDING / PAYOUT_STATUS_UNKNOWN through `Reconcile`. Neither had any caller in `cmd/` at all (D-085) |
| `runCreditSettlement` | 15 min, **20 s on a sandbox tier** | unchanged, except that a sandbox tier's window is 2 minutes rather than the configured 720-hour chargeback window — without which nothing bought on STAGING could ever be withdrawn (D-086) |

### Boot-time provisioning added

`creditAssetAtBoot` registers THE Credit asset through `assets.Repository.Create`
on a sandbox tier that has none. Nothing wrote one: `scripts/seedeconomy` refuses
to run anywhere but LOCAL, DEV and TEST, so D-068's demo seeder was a permanent
no-op on the one tier it was built for. PROD is refused (D-084).

A fresh sandbox database now boots to 1 Credit asset, 1 GLOBAL risk policy, 4
demo markets with 6 fills and 19 labelled `demo_seed_rows`, and 6 sandbox gates —
and boots to the same thing twice
(`TestIntegration_BootingTwiceCreatesEverythingOnceOnASandboxTier`).

### State machines

None added. Every state change in this addendum goes through a transition table
another branch built: `compliance_profile_transitions` (00761),
`payout_request_transitions` (00603), `agent_lifecycle_transitions` (00501,
00750), `credit_funding_transitions` (00743).

### Interfaces exposed for other domains

- `activity.Kind` — eleven new members; a nineteenth kind is a `const src…`
  branch, a `sources` entry, a `feedQuery` group and a summary template.
- `notifications.AgentPauseRef` / `AgentPauseCopy` — exported so any writer of an
  `AGENT_PAUSED` notification derives the same dedup key from the same row.
- `notifications.ScopeVerification` / `ScopeEligibility` / `ScopeAgent` — realtime
  invalidation scopes a client refetches on.
- `agents.Event.PauseID` — which `agent_pauses` row a pause event is about.
- `agents.Publisher` — wired, non-nil, in `cmd/api` for the first time.
- `profile.Service.Outstanding(ctx, q, userID, terms.Requirement)` — the documents
  a person still owes at one point in the journey.
- `payout.CreateRequest.DisclosureAccepted`, `payout.QuoteRequest.DisclosureAccepted`,
  `payout.RequiredDisclosure`, `payout.Service.AwaitingSubmission`.
- `eligibility.WithdrawalInput.DisclosureAccepted`, `eligibility.WithdrawalTermsNotAccepted`.
- `verification.Service.ExpireOverdue`, `verification.Repository.OverdueVerifications`,
  `verification.SweepBatch`.
- `httpapi.WithdrawalDeps.Terms` (`httpapi.TermsOutstanding`) — the legal registry
  reader the withdrawal surfaces consult.

---

## Addendum — 2026-09-10: the platform-hardening fixes (F-166 … F-173)

Nothing here is a new product surface. It is the transport, the follower, the
schema binding and the timers behind surfaces that already exist.

### Routes added

None.

### Migrations added

| Migration | What it adds |
|---|---|
| `00796_a_sanctions_screen_is_a_decision_not_an_attribute.sql` | `compliance_profile_transitions.from_sanctions_state` / `.to_sanctions_state` and their CHECKs; `cp_uuid_v7()`; `cp_compliance_profile_birth_screen()` and its trigger; a replaced `cp_compliance_apply_state_transition()`; the constraint trigger `compliance_profiles_require_sanctions_transition`; `sanctions_state` out of `cp_app`'s column grant |

Schema is at migration **796** after this batch. No table was added; no
migration ≤ 00755 was touched.

### Packages added

None. `internal/httpapi`, `internal/ratelimit`, `internal/errs`,
`internal/notifications`, `internal/compliance`, `internal/verification` and
`cmd/api` gained code.

### Interfaces other domains can use

- `ratelimit.NewMemoryStoreWithMax`, `ratelimit.DefaultMaxKeys`,
  `ratelimit.MemoryStore.Stats` / `MemoryStats` — what the store is holding, for
  a sweeper's log.
- `notifications.Follower.WithLogger` — the follower reports a pass that looks
  stalled to the binary's logger.
- `verification.Service.ExpireOverdueSessions`,
  `verification.Repository.OverdueSessions`, `verification.DueSession`,
  `verification.UnstartedSessionGrace` — closing an attempt that can no longer
  be decided.
- `errs.CodeBodyTooLarge` is in `errs.AllCodes()`, so anything that enumerates
  the code set now covers the one the transport returns on every route.
## Addendum — 2026-09-10: the credits-payments audit fixes (F-151 … F-159)

Nine findings against goal §54, one P0 and three P1. No new product surface: a
payment now issues the quantity it charged for, a chargeback takes the units it
reversed, and the two passes that end an unfinished purchase exist and run.

### Routes added

None. Three existing responses grew fields:

| Route | Field | Why |
|---|---|---|
| `GET /v1/credits/pricing` | `decimals`, `minor_units_per_major_unit`, `rounding` | the rate alone is not the conversion, so a page holding it had to assume the rest (F-151) |
| `GET /v1/credits/balance`, `GET /v1/me/portfolio` | `credit_decimals` required; `reversed` promoted to required | the scale travels with the figures, and a bucket nobody renders is a number that can be wrong forever (F-151, F-156) |
| `POST /v1/payments`, `GET /v1/payments/{id}` | `provider_mode` | the sandbox label is a fact about the payment, not about today's configuration (F-158, D-096) |

`POST /v1/payments` also bounds `amount_minor` against the maximum the pricing
endpoint publishes, before any capacity guard measures anything against it
(F-159).

### Migrations added (schema is now at 00793)

| Migration | What | Why |
|---|---|---|
| 00793 | `credit_fundings.provider_mode text`, CHECK `IN ('fake','sandbox','live')`, nullable, no backfill | the mode that opened a payment, written once at creation and write-once by privilege under 00743's grants. NULL is a funding that predates the column and renders as sandbox: an unrecorded mode cannot be asserted to be real money (D-096) |

### Packages added

None. `internal/credit`, `internal/capacity`, `internal/valuedomain`,
`internal/provider/stripecredit`, `internal/httpapi`, `cmd/api`,
`cmd/reconciliation-worker` and `apps/web` all gained code.

### State machines

`credit.FundingState` gains one edge: `DISPUTED → REVERSIBLE`, taken when a
dispute or an early-fraud-warning inquiry closes without taking the money. It is
not `DISPUTED → SETTLED`: settlement means the reversibility window closed, which
is a clock and a policy, and `SettleDue` is now the only thing in the binary that
writes SETTLED (D-094). `credit.PurchaseStatus` gains `DISPUTE_LIFTED`, so a
closed inquiry and a won dispute stay distinguishable in the transition rows.

### Passes that now run in `cmd/api`

| Pass | Cadence | What it ends |
|---|---|---|
| `SettleDue` (existing) | the credit ticker | a reversibility window that closed |
| `ReconcileDue` | the same ticker | a purchase in flight past 15 minutes whose provider event was swallowed (F-154) |
| `ExpireInFlight` | the same ticker | a pre-capture purchase open past a day, cancelled at the provider first (F-153) |

`cmd/reconciliation-worker` calls the same two methods, so the tiers cannot
drift; the sweep's own `stale()` query is gone, and with it the
`provider_reference IS NOT NULL` filter that excluded exactly the fundings the
sweep existed to recover.

### Interfaces that changed

- `credit.PurchaseProvider` gains `CancelPurchase(ctx, providerReference, idempotencyKey)`.
  Without it a purchase nobody finishes is permanent, so it is on the interface
  rather than an optional capability. `stripecredit.Client` implements it with
  `POST /v1/payment_intents/:id/cancel` and `cancellation_reason=abandoned`.
- `credit.NewPurchaseService(ctx, q, cfg)` takes a querier, to hold its pricing
  policy's scale against the registered CREDIT asset (D-093), and
  `cfg.ProviderMode`, to stamp on every funding it opens (D-096).
- `credit.ConsumeRequest.LotIDs` restricts consumption to named lots, which is
  what a clawback needs (F-152).
- `credit.Balances.CreditDecimals` carries the scale with the figures.
- `httpapi.Options.CreditPurchaseSandbox` is **removed**: there is no
  deployment-wide answer left to stamp on a past payment.

### What did NOT change

No new error code, no new capability gate, no new environment variable, no new
route, no change to the ledger, the lot event stream or the payout path. The
shipped pricing policy issues 10^6 times what it did, which is the fix; no
deployment has ever sold a Credit, so there are no fundings recorded under the
old arithmetic.
