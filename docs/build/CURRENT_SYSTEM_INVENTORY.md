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
| `00783_a_follower_remembers_where_it_stopped.sql` | `notification_follower_cursors (source, last_at, last_id, pending_at, emitted, updated_at)` — the in-process follower's position per source |

Schema is at migration **783** after this batch.

### Packages

| Package | What it is |
|---|---|
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

**What still is not exposed over HTTP:** backtests, predictions, agent decision
history, and agent performance. The first two have no product surface in this
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
| Capability gates | `#gates` | `GET /v1/admin/gates` | propose, approve, activate, suspend, resume, revoke, **sandbox, unsandbox** |
| Accounts | `#accounts` | `GET /v1/admin/accounts`, `/v1/accounts/{id}`, `/v1/accounts/{id}/activity`, `/v1/credits/balance`, plus the reconciliation, action, gate and kill-switch lists for one account | account status only |
| Withdrawals, envelopes, agent promotion | `#withdrawals` `#envelopes` `#agent-promotion` | `GET /v1/admin/actions`, filtered by the surface's action kinds | none — each links into the one queue |
| Break-glass | `#break-glass` | `GET /v1/admin/actions` | propose `BREAK_GLASS_GRANT` |
| Providers | `#providers` | `GET /v1/admin/providers` | none |

What the console will not do, each for a reason on screen: fabricate an approval
reference (a high-risk proposal still requires all four); render a `SANDBOX` gate
as an approval (ADR-0023 — its own hue, a dashed pill, and the words "sandbox —
not an approval"); show a balance-editing control (none exists: account-scoped
writes go through `RequireAccountOwner`, which has no operator override); or
leave a marker instead of a decision (`src/scan.test.ts`).

Two generated documents are its authority and neither is hand-written:
`src/generated/authority.json` (permissions, roles, action kinds, surfaces,
step-up windows, capabilities, kill-switch severities) and
`src/generated/decisions.json` (a decision vector per principal/action/verb/instant),
both produced by `internal/adminplane` and held equal to the Go tables by
`TestAuthorityGolden` and `TestDecisionVectorsGolden`. The console fetches them
at runtime from `dist/`, so `build.mjs` verifies its own copy and
`TestConsoleBuiltArtifactsMatchTheirSource` compares `dist/` to `src/` whenever
`dist/` exists.

Known gaps, stated in the views rather than worked around: no `/v1/admin` route
exposes `capability_gate_transitions`, so a SANDBOX row's actor (an operator, or
the SYSTEM actor `config:CP_API_SANDBOX_GATES`) cannot be named for a specific
row; `Account` carries no owner, so `GET /v1/admin/users/{userId}` has nothing to
be called with; no route lists an account's agents; `cmd/api`'s
`providerCatalog` omits the payout slot, so `sandbox_payout` never appears in
the providers view; and `openapi/openapi.yaml`'s `Capability` enum lists ten of
the twenty capabilities Go declares (D-079).
