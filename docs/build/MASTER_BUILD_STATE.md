# MASTER BUILD STATE

> **READ THIS FILE FIRST when resuming in a new session.**
>
> **The goal changed.** The current source goal is `gola.md` (repo root, 102 parts, 25 stages):
> an independent adversarial audit of the existing system, a migration to the final Nodal
> architecture, and production proof. The previous goal document
> (`ULTIMATE MASTER GOAL — Production Universal Financial Control Plane.md`) built what is now
> Domain B and Domain C; its history is preserved below from "## 2. Completed milestones" onward and
> is still accurate about that work.
>
> Companion files: `FINAL_ARCHITECTURE_MIGRATION.md`, `CURRENT_SYSTEM_INVENTORY.md`,
> `KEEP_MODIFY_REPLACE_MATRIX.md`, `BLOCKERS.md`, `DECISION_REGISTER.md`,
> `REQUIREMENTS_TRACEABILITY.md`, and `../audit/INDEPENDENT_AUDIT.md`.

---

# PART 0 — CURRENT GOAL (gola.md) AND STATE

Baseline frozen at `b8da0c4`. Everything below this line describes work done against `gola.md`.

## 0.1 The finding that shapes everything

The repository implemented the previous goal completely and soundly: 40 of 40 integration packages,
the migration, e2e, chaos and six contract suites pass on freshly provisioned databases. Measured
against `gola.md` that is Domain B (simulated capital) and Domain C (real capital).

**Domain A — the Nodal-native economy — did not exist at all.** Not partially: a grep for
`ValueDomain`, `CapitalRail`, `CreditOrigin`, native assets, a market engine, payout eligibility, a
legal router or agent authority levels returned zero files. That absence, not a defect list, is the
migration.

## 0.2 What has landed

| Stage | Subject | State |
|---|---|---|
| 0 | Baseline freeze, inventory, KEEP/MODIFY matrix | **done** |
| 1 | Independent adversarial audit | **done, and continuing alongside each stage** |
| 2 | ValueDomain / CapitalRail / provenance, enforced in Go and SQL | **done** (`internal/valuedomain`, migration 00710) |
| 3 | Accounting hardening | **partial** — domain isolation added to the ledger; reservations pre-existed and were verified |
| 4 | Credit ledger, provenance lots, funding lifecycle | **done** (`internal/credit`, migration 00711) |
| 5 | Native asset registry, moderation, lifecycle | **done** (`internal/nativeasset`, migration 00712) |
| 6 | Native market engine (constant product, virtual reserve) | **done** (`internal/nativemarket`, migration 00712) |
| 7 | Market surveillance | **done** (`nativemarket/surveillance.go`) |
| 8 | Internal commerce / creator economy | **done** (`internal/commerce`, migration 00715) |
| 9 | Payout eligibility, provider architecture, reconciliation | **done** (`internal/payout`, migration 00713) |
| 10 | Hosted partner rail | **not started** — and see BLOCKERS B-05 |
| 11 | Self-custodial onchain rail | **kept as-is**, re-classified as one rail among several |
| 12 | Rails unified behind FinancialIntent | **done** (`settlement.FinancialIntent`, `settlement.Compile`, wired in front of every Domain A command) |
| 13 | LegalCapabilityRouter, composite capability key | **done** (`internal/legalrouter`, gates extended, migration 00714) |
| 14 | Agent authority levels | **done** (`internal/agentauthority`) |
| 15 | Reality / Prediction / Proof integration with Domain A | **not started** |
| 16 | Frontend | **not started** |
| 17 | Admin tooling for Domain A | **done** (9 administrative action kinds + executors; `internal/httpapi/executors_domaina.go`) |
| 18 | Infrastructure / IAM hardening | pre-existing, audited |
| 19 | Property testing / fuzzing | **partial** — curve fuzzer (4.7M execs), exhaustive isolation property, credit torture test |
| 20–24 | Chaos, load, provider sandbox, re-audit, launch package | **not started** |

API surface: 17 Domain A endpoints added to the OpenAPI contract, regenerated, implemented, wired
into `cmd/api`, and covered by the existing deny-by-default authorization invariants. Permissions:
59, up from 42 at the baseline; capabilities: 20, of which 9 are high-risk.

### Stage 8 as built

`internal/commerce` + migration 00715. The requirement PART XVII actually turns on is one sentence —
creator revenue must carry provenance distinct from speculative trading proceeds — and everything
else follows from making that sentence structurally true:

- A product's **kind** is the only input to the provenance decision, the mapping is a fixed table,
  and the resulting origin is stored on the order rather than re-derived. No request body anywhere
  in the API has a field for an origin.
- **Self-dealing is refused twice.** Buying from yourself would convert Credits no policy will ever
  release into creator-earning provenance one day might, which is the most valuable thing an
  attacker could do here. Go refuses it with a comprehensible error; two named CHECK constraints
  refuse it for the schema owner.
- **The order is not the authority on what was paid.** A deferred trigger reads the journal entries
  and refuses at COMMIT any order whose stated price or proceeds the posting does not show (IC001).
- **Terms freeze on publication** (IC002) and orders are immutable.
- An earning is issued **REVERSIBLE, not SETTLED**: the Credits behind it may still be inside a card
  dispute window, and an earning cannot be more final than the money behind it. It is spendable,
  which is what a marketplace needs, and not payout-eligible, which is the conservative half.
- A purchase is a **single-domain movement** and commits with no capability active. If it ever
  needed one, that would mean a conversion had crept into the path.

JOURNEY C can now be walked end to end: buy Credits → list a dataset → sell it → hold
`DATA_SALE_EARNING` → request a payout and be told, per unit of provenance, what the policy permits.
The last step still ends in a refusal on a fresh deployment, which is correct: no payout capability
is active and the default policy permits no origin.

### Stage 12 as built

`internal/settlement/financialintent.go` and `compiler.go`, plus
`internal/httpapi/wiring_compiler.go`.

**The problem.** `V1Planner` compiles one rail shape: a self-custodial on-chain spot swap. That was
the whole system when it was written and is not the whole system now. Domain A executes on Nodal's
own ledger, Domain B against simulated markets, and a payout leaves the system entirely — four
settlement models with four authoritative balance sources. A planner that knows one of them cannot
be "the only route from a typed intent to execution", which is the property the compiler exists to
have.

**What was built.**

- **`FinancialIntent`** (PART XXV): the typed request every manual and agent action produces, with a
  DECLARED capital domain, an action type from PART XXV's list, and a typed subject. It sits beside
  `IntentSnapshot` rather than replacing it: the V1 projection is an instrument, a USD notional and a
  venue, which cannot describe "buy 500 Credits of this creator's token" without most of its fields
  becoming meaningless.
- **`Compile`** (PART XXVI): pure, deterministic, and total over a routing table that
  `ValidateCompiler` proves covers every declared action exactly once. It determines value domain,
  legal rail, provider, executor, required capabilities, required verification, required
  confirmation, quote and reservation requirements, risk evaluation, agent authority,
  authoritative balance source and reconciliation method — and returns every applicable refusal at
  once, sorted, so a caller is never told one problem per attempt.
- **The refusals that matter.** An unimplemented rail is refused before any policy question, so
  hosted trading cannot be routed to machinery that does not exist however permissive a policy is.
  An agent can never request a payout, at any level, with every capability active. A declared domain
  that disagrees with the action is a refusal, never a correction.
- **Wired in front of every Domain A command**: native-market execute, native-asset create, internal
  purchase and payout create all compile first. A refusal carries the policy version, the rule index
  that produced it, and the capability that would have to be activated.

**Why internal rails get a Route and not a seventeen-step Plan.** Durable per-step state exists
because an external settlement can be half-done — submitted, result unknown. An internal-ledger
settlement is one database transaction that either commits or does not. Wrapping it in a DAG would
add failure modes rather than remove them. What must not be skipped is everything IN FRONT of
execution, and that is what `Route` carries.

**What this does not replace.** The ledger still refuses a cross-domain posting with no capability;
`internal/commerce` still refuses a purchase with `MARKETPLACE` off; the payout engine still decides
eligibility per unit of provenance. A gate that exists only at the edge is one a worker walks
around, so the compiler is an addition and never a substitution — F-15 is the finding that made that
rule concrete.

**Still on the V1 planner.** External spot swaps (Domain C) are unchanged: the compiler routes them
to `ExecutorExternalPlan` and `V1Planner` does its job. Compiling a Domain C intent through
`FinancialIntent` end to end — replacing the direct `IntentSnapshot` path — is the remaining half of
this stage and is not done.

### Stage 17 as built

Nine administrative action kinds and their executors, on the existing
dual-control machinery in `internal/admin`.

**The problem.** Every control in Domain A existed and none of them had an
operator interface. A market could be halted, an asset delisted, a seller
suspended and a stuck payout resolved — by running SQL. A control reachable
only by hand-written SQL has no audit trail, no dual-control story and no
reason attached, which in an incident is barely a control.

**The rule the table encodes.** Stopping is one operator; restarting is two.

| Kind | Signatures | Why |
|---|---|---|
| `NATIVE_MARKET_HALT` / `CLOSE_ONLY` / `FREEZE` | one | A control that needs two signatures to stop an incident is one nobody reaches for at 3am. PART XXXII: halting new risk must never be harder than taking it. |
| `NATIVE_MARKET_RESUME` | two | Restarting is the direction that adds exposure. |
| `NATIVE_ASSET_MODERATION_VERDICT` | one | A content judgement. Recording APPROVED does not start trading. |
| `NATIVE_ASSET_DELIST` | one | Risk-reducing and terminal. |
| `COMMERCE_SELLER_SUSPEND` / `PRODUCT_WITHDRAW` | one | Risk-reducing. Suspension stops new orders and touches nothing already earned. |
| `PAYOUT_MANUAL_REVIEW_RESOLVE` | two | It decides what happens to money somebody is waiting for. |

**What the payout resolver deliberately cannot do.** There is no resolution
that declares a payout SETTLED. The provider is authoritative for settlement,
and an operator who could assert it by hand could close a ticket by claiming
money moved. The resolutions are FAIL (return the exact reserved units to the
exact lots), REJECT and RETRY — and RETRY is refused outright if the request
has ever been in a state where a provider call may have happened, because
sending it back to VERIFIED is how a payout gets paid twice.

**One new permission.** `native_market:resume` is the approve half of restarting
a market and is a dual-control permission: no standing role holds it, so it
requires a live break-glass elevation, exactly like releasing a kill switch.
Permissions are now 60.

**What the console already does.** `apps/admin`'s propose form is generated from
`authority.json`, which now lists all nine kinds, and it carries a free-form
params field. So every one of these is proposable, approvable and executable
from the existing dual-control queue today — including the two that take
params. What is missing is kind-SPECIFIC UI: a market picker instead of a
pasted uuid, a moderation-state dropdown instead of hand-written JSON. That is
Stage 16 work and a usability risk, not a missing control.

## 0.3 Next exact work, in order

1. **Stage 16 — frontend.** The API exists; `apps/web` has no Domain A surface. PART LII's rule
   (Nodal Economy / Simulated / Real Capital never summed) has to be structural in the UI, not a
   styling choice.
2. **Stages 20–21 — chaos and load** for the new subsystems, then the re-audit and evidence package.

## 0.4 Verification commands that matter

    make integration        # scripts/inttest: every //go:build integration package, one fresh DB each
    make integration-race   # the same over the financial core, with -race
    go test -count=1 -run FuzzCurve -fuzz FuzzCurve_NeverBreaksTheInvariant -fuzztime=60s ./internal/nativemarket/

**Run every suite twice against the same database before believing it.** This project has now been
bitten three times by a fixture that passes on a fresh database and fails on the second run — most
recently by a test that created a Credit asset per call when the schema permits exactly one.

## 0.5 Failing tests

None. `go build ./...`, `go vet ./...`, `go vet -tags=integration ./...`, the short unit suite, and
the full integration sweep are green at the time of writing.

---

# PREVIOUS GOAL — HISTORY (accurate for Domains B and C)

## 1. Current milestone

**STAGE 0 — FORENSIC REPOSITORY AUDIT → STAGE 1 — FOUNDATION CONTRACTS**

Stage 0 findings (2026-09-05):

- Repository was **greenfield**: the only file present was the goal document. No git history, no code, no research diligence file (`deep-research-report*.md` searched at repo root and `C:\Dev\*`; the two hits in sibling projects are unrelated to this product).
- Host toolchain at session start: Node 24.14.0, pnpm 10.34.5, Python 3.11.9, git 2.53, Docker Desktop installed but daemon stopped, winget available. **Missing:** Go, make, terraform, psql, sqlc, buf, golangci-lint, staticcheck, govulncheck, gosec, k6, gitleaks, trivy, syft.
- Network access to proxy.golang.org and registry.npmjs.org confirmed.
- Actions taken: `git init -b main`, local git identity set, Docker Desktop launched, winget installs of Go 1.27.1 / ezwinports.make / Hashicorp.Terraform started.

Stage 0 exit criteria: toolchain installed, baseline build runnable, persistent build state exists (this file + 3 companions). **MET 2026-09-05**: Go 1.27.0 verified (SHA-256 checked zip at `C:/Dev/tools/go`; the winget MSI is stuck behind a UAC prompt the operator may approve or dismiss), Docker stack healthy (7 services), Go tools in `./bin`, git initialised, build-state docs present.

Stage 1 (foundation contracts) started 2026-09-05 with six parallel subagents: `internal/money`, `internal/{id,clock,errs}`, `internal/{config,observability}`, `internal/{db,idempotency}` + `cmd/migrate` + migrations 00001–00003, tooling (`scripts/tool`, `scripts/fuzzall`, `scripts/maketargets`, `.golangci.yml`, CI workflows), `internal/{security,auth}`. Contracts fixed in `docs/architecture/CONVENTIONS.md`.

Stage 2 (financial core) design fixed in `docs/architecture/FINANCIAL_MODEL.md`. Draft migrations 00010 (identity/accounts/sessions) and 00100–00105 (assets, ledger, capital, funding, positions, valuation) were applied to a scratch database and the ledger triggers were exercised: balanced posting commits; unbalanced (LG002), entry-less (LG002), negative-balance (LG001), asset-mismatch (LG004), and mutation (LG003) attempts are rejected; `cp_app` can post but cannot update `ledger_balances` (SECURITY DEFINER trigger). Drafts move into `migrations/` once the db agent's 00001–00003 land.

## 2. Completed milestones

| Milestone | Date | Evidence |
|---|---|---|
| Repository audit (greenfield confirmed) | 2026-09-05 | this file §1 |
| Persistent execution memory created | 2026-09-05 | `docs/build/*.md` (traceability: 389 rows) |
| Stage 0 exit: toolchain + local infra + build state | 2026-09-05 | `go version`, `docker compose ps` healthy, `./bin/*` tools |
| `internal/money` (exact numerics) | 2026-09-05 | 74 tests / 1610 subtests, 5 fuzzers 10 s clean, `-race` green |
| `internal/{id,clock,errs}` | 2026-09-05 | 74 tests, fuzz clean, `-race` green |
| `internal/{config,observability}` + `.env.example` | 2026-09-05 | 67 tests, every PROD validation rule tested, fuzz clean |
| `internal/{db,db/migrate,idempotency}`, `cmd/migrate`, migrations 00001–00003 | 2026-09-05 | 32 unit + 19 integration tests `-race` green; checksum verify; ProtectedVersion guard |
| Migrations 00010–00301 (identity, audit, provider events, assets, ledger, capital, funding, positions, valuation, gates, kill switches, policies, admin, instruments, intents/quotes, plans/orders, wallets/execution, reconciliation) | 2026-09-05 | applied via `cmd/migrate up` + `verify: ok`; ledger triggers exercised manually (LG001–LG005) |
| `internal/{assets,accounts}` registries | 2026-09-05 | compile + vet clean (integration tests pending in Stage 2 wave) |
| Tooling: `scripts/{tool,fuzzall,maketargets,lintfin,images,testdb,fmtcheck,supplychain}`, `.golangci.yml`, `.gitleaks.toml`, CI + release workflows, `build/Dockerfile` | 2026-09-05 | `go test ./scripts/...` green; all 13 pinned tools installed with checksum verification |
| Architecture docs: SYSTEM, FINANCIAL_MODEL, POLICY_AUTHORITY, SETTLEMENT_COMPILER, EXECUTION, RECONCILIATION, STRATEGY_IR, AGENT_RUNTIME, POINT_IN_TIME, CONVENTIONS; 19 ADRs | 2026-09-05 | `docs/architecture/*`, `docs/adr/*` |
| `internal/security` (RBAC matrix, tenant scoping, step-up, agent principal) + `internal/auth` (sessions, OIDC+PKCE, dev IdP, chi middleware) | 2026-09-05 | 84 tests / 509 subtests `-race` green; golangci-lint 0 issues; flaky "tampered cookie" case fixed (D-014 records the go-oidc deviation) |
| Migrations 00500–00601 (strategies, agents, predictions/tools, reality, backtests/performance) | 2026-09-05 | `cmd/migrate up` + `verify: ok` on `controlplane_test` |

| `internal/auth/pgstore` (Postgres SessionStore) + migration 00011 (break-glass column) | 2026-09-05 | integration tests on isolated DB; exposed and fixed the default-privilege defect (D-016) |
| Migration renumbering: audit_events → 00106, provider_events → 00107 (protected range) | 2026-09-05 | migration suite green incl. foundation rollback on a foundation-only DB |
| Privilege audit test (`test/integration/migrations/privileges_test.go`) | 2026-09-05 | green: cp_app no DELETE anywhere, no UPDATE on append-only tables; ops housekeeping allowlist explicit |
| `internal/provider` (health states with hysteresis, retry classes, verification labels) | 2026-09-05 | unit + property + concurrency tests `-race` |
| `internal/fees` (explicit platform fee policy, default zero) | 2026-09-05 | unit + property tests `-race`; lint clean |
| Provider API notes (`docs/api/providers/*`, 7 files, dated URLs) | 2026-09-05 | Jupiter/Solana RPC/Stripe API/Anthropic verified from official docs; Helius/Privy partial |
| `internal/instruments` (venues, exposures, spot pairs, listings, external ids, status transitions) | 2026-09-05 | integration tests on isolated DB `-race` green |
| `proto/controlplane/signing/v1` + buf config + generated Go (`internal/gen/proto`) | 2026-09-05 | `buf lint` clean; generated code builds; `make proto` / `make proto-breaking` targets |
| `internal/event` (envelope, topics registry, transactional outbox, relay with per-key ordering, inbox dedup, in-memory bus with fault injection) | 2026-09-06 | 62 tests incl. duplicate-delivery property (N∈[1,50]) and crash-between-publish-and-mark; `-race` green; fuzz 20 s clean |
| `internal/valuation`, `internal/positions` (FIFO lots, exact basis conservation), `internal/capital/buyingpower` | 2026-09-06 | 40 tests incl. `TestProp_BasisConserved`, `TestProp_BuyingPowerBounds`; re-verified on a fresh DB after D-016 |
| Migration 00602 (FIAT asset kind for USD quote references; ledger rejects fiat accounts) | 2026-09-06 | applied + verify ok; linear numbering adopted (D-015 addendum 2) |
| Migration 00603 (state changes of gates, kill switches, accounts, assets, instruments, deposits, withdrawals, intents, orders, reconciliation records, admin actions require a matching transition row in the same transaction; SQLSTATE AU001) | 2026-09-06 | closes threat-model gap "bare gate-state update"; `transition_binding_test.go` |
| `internal/gates` (10 capabilities, 7 states, five-condition activation, dual control) + `internal/killswitch` (12 kinds, action-class matrix, fast activate / gated release) | 2026-09-06 | 45 tests incl. 4 properties; re-verified on fresh DB with 00603 |
| `docs/security/SECURITY.md`, `docs/threat-model/THREAT_MODEL.md` | 2026-09-06 | implemented vs designed claims cite files on disk; top-10 residual risks listed |
| `internal/ledger` (per-asset double entry, canonical content hash, idempotent posting, deficit pattern, swap/funding builders, VerifyBalances) | 2026-09-06 | 28 unit (4 properties) + 12 integration tests; 50-goroutine concurrency; re-verified on fresh DB; D-017/D-018 recorded |
| Migration 00604 (column-level UPDATE(status) on ledger_accounts for cp_app) | 2026-09-06 | migration suite + ledger suite green |
| `internal/capital` (transactional reservations, envelopes with authority audit, holds, PnL/drawdown, verification) | 2026-09-06 | 43 tests; **PART 23 torture: 100 × $500 vs $10,000 → exactly 20/80 in every iteration** under row-lock and SERIALIZABLE, asset and envelope variants (25 iterations locally; CI target 1000); property `capital conserved`; re-verified on fresh DB |
| BuyingPower types unified: `capital` re-exports `capital/buyingpower` types (no duplicate contract) | 2026-09-06 | compile-checked |
| `internal/audit` (canonical JSON, per-stream hash chain under advisory lock, verifier, in-memory test writer) + `internal/admin` (dual-control actions, VerifyApproved, break-glass grant, savepointed Execute) | 2026-09-06 | 25 unit (+ 20 s fuzz) + 18 integration tests; 100-goroutine same-stream append; tamper detection; re-verified on fresh DB |
| `openapi/openapi.yaml` (OpenAPI 3.1: auth/sessions, accounts, buying power, holdings, ledger, activity, export, markets, quote preview, intents, orders, funding, withdrawals, SSE, webhooks, admin gates/kill switches/actions/reconciliation/instruments/providers, health/version) + `docs/api/README.md` + generated strict server (`internal/gen/api`) | 2026-09-06 | spec generates and builds (`make openapi-server`) |
| Repository-wide lint sweep: `golangci-lint --build-tags=integration ./...` 0 issues; gofumpt clean; `go test -race ./...` 35/35; integration 32/32 on fresh DB | 2026-09-06 | lint agent report; 76 findings fixed without weakening tests |
| `REQUIREMENTS_TRACEABILITY.md` refreshed from on-disk evidence: 389 rows → VERIFIED 81, IMPLEMENTED 49, IN_PROGRESS 138, NOT_STARTED 103, DEFERRED 18 | 2026-09-06 | every VERIFIED row names an on-disk test function and a recorded pass |
| pnpm workspace + `packages/generated-client` (openapi-typescript schema, openapi-fetch client with idempotency/correlation/problem+json handling, node tests) + CI drift jobs for OpenAPI server/client and proto | 2026-09-06 | `pnpm generate/typecheck/test` green |
| `internal/notification` + migration 00640 (in-app notifications written in the causing transaction, dedup on replay, tenant-isolated listing, immutable content, SKIP LOCKED dispatcher with provider abstraction) | 2026-09-06 | integration tests on fresh DB `-race`; `db.IsImmutableRow` now recognises SQLSTATE LG003 from every domain trigger |
| `internal/compliance` (profile repository; SYSTEM/OPERATOR writers only; before/after-hashed audit events; fail-closed validation) | 2026-09-06 | integration test on fresh DB `-race`; lint 0 |
| `internal/intent` (typed intents, exhaustive transition table, canonical content hash, idempotent submit with tenant/agent guards, transition + outbox + audit in one tx) + `internal/quote` (immutable quotes, freshness/expiry, fee disclosure with no hidden spread) + migration 00605 (content hash + identity/economics immutability trigger IN001) | 2026-09-06 | intent 19 unit + 8 integration, quote 6 unit + 1 integration; re-verified on fresh DB `-race`; lint 0 |
| `internal/funding` (PART 28 state machine with provider-owned skip-forward, settlement/availability/reversal postings incl. deficit two-transaction pattern + account FREEZE + alert row, lifecycle driver), `internal/withdrawal` (boundary: human actor type, step-up, WITHDRAWALS gate always refused today, destination + velocity policy), `internal/webhook` (raw-persist → verify → replay window → hash → provider_events + inbox → dispatch in one tx; forged/stale/oversized/unknown handled), `internal/provider/stripe` (onramp sessions, Stripe-Signature v1, exact decimal amounts, fake mode rejected outside LOCAL/TEST/DEV), `test/contract/stripe` (6 fixtures + README) | 2026-09-06 | 85 unit + 46 integration tests incl. `TestFunding_ReversalCreatesDeficitAndFreezes`, `TestProp_DuplicateWebhookOneEffect`; re-verified on fresh DB `-race`; lint 0 |
| `internal/provider/jupiter` (Swap API V2 `/order`, `/execute`, `/build`; exact string amounts; execute = UNKNOWN_EFFECT_WRITE with exactly one HTTP attempt; raw evidence with redaction; deterministic fake producing real serialized transactions) + `test/contract/jupiter` (21 cases, 12 fixtures, README labelling documented vs assumed) | 2026-09-06 | 45 unit + 4 fuzz (20 s clean, one real parser finding fixed) + 1 property + 21 contract; lint 0 |
| `internal/chain` (ChainObserver/SolanaDataProvider contracts, agreement policy that never picks the optimistic answer, MultiObserver with degraded capping, `ProvenAbsent`, chaintest simulator with slot/blockhash-expiry/fault injection) + `internal/provider/solanarpc` (strict JSON-RPC: `UseNumber`, float rejection, retries only on SAFE_RETRY reads, archive-before-parse, exact delta arithmetic) + `internal/provider/helius` (composes solanarpc; DAS balances; `x/net/websocket` stream with polling backfill) + `test/contract/{solanarpc,helius}` (35 fixtures, READMEs) | 2026-09-06 | 93 test funcs incl. 2 agreement properties, `TestProp_DeltasSumToFee`, `FuzzParseTransactionResponse` 20 s clean; re-verified `-race`; lint 0 |
| D-014 completed: `internal/auth/oidc` verifies ID tokens with `coreos/go-oidc` (discovery + issuer refusal, alg allow-list, signature, iss/aud/exp/nbf) over a package-local go-jose KeySet (rate-limited JWKS refresh, unknown-kid vs bad-signature distinguished, `use=enc` filtered); nonce/azp/auth_time/amr/acr/step-up remain explicit; PKCE exchange via `oauth2`; 1 MiB body cap + timeout transport | 2026-09-06 | 62 test funcs / 150 subtests `-race`; every original negative test unchanged; identity integration green; lint 0 |
| `internal/signing/inspect` (pure decoder for legacy + v0 with ALT resolution; all 16 EXECUTION §2 checks; hand-written SPL/System/ComputeBudget/ATA layouts cross-checked against spec vectors; 57-case mutation table; 2 fuzzers 30 s clean) + `internal/signing` (service rebuilds expectations from persisted rows only, one provider call per attempt under concurrent replay, `signing_decisions` + `signing_results` + security event on rejection, gRPC server + in-process client) + `internal/wallet` (+ `wallettest` deterministic ed25519 fake) + `internal/provider/privy` (SDK v0.15.0 names verified by `go doc`; policy with `programId` allow-lists) + `test/contract/privy` + migrations 00615 (wallet transitions + binding) / 00616 (`signing_results`) | 2026-09-06 | 47 test funcs + 2 fuzz; re-verified `-race` on fresh DB; lint 0. **Caveat SB-007:** Jupiter v6 instruction layout reproduced from memory (UNVERIFIED) |
| Outbox retry deadline persisted (migration 00642 `next_attempt_at` + indexes; relay claims by deadline and sets failure_time + backoff(attempts)); relay integration test corrected to failure-based timing | 2026-09-06 | event suite green on fresh DB; a permanently failing row can no longer hot-loop |
| `infra/terraform` (12 modules: kms with separate ECC signing key, network with private data subnets + endpoints + flow logs, rds Multi-AZ/PITR/force_ssl + roles bootstrap SQL honoring D-016, redis, s3-evidence with COMPLIANCE Object Lock, secrets with per-task-role matrix asserted by `check` blocks, ecs-cluster + immutable ECR, ecs-service per binary with read-only rootfs/non-root/circuit breaker, app-config `CP_*` map, waf-edge, observability alarms/SNS, iam-deploy GitHub OIDC) + dev/staging/prod environments (prod refuses fake modes and short Object Lock) + `docs/operations/DEPLOYMENT.md` | 2026-09-06 | `terraform validate` success in all three environments (integrator re-ran prod), `fmt` clean, trivy config 0 HIGH/CRITICAL; **never planned/applied** (EB-012) |
| `docs/runbooks/` — index + 19 runbooks (PART 157 set, global kill + re-enable, submission unknown), each with trigger/blast radius/first 10 minutes/diagnosis/containment/what-not-to-do/exit criteria/post-incident, naming real admin routes, kill-switch kinds, admin-action kinds, and read-only SQL; unimplemented controls marked PENDING | 2026-09-06 | 1,498 lines; codename absent |
| `test/security` (agent trees never import authority packages; signing service imported only by the execution boundary; agent permission set closed; PROD/STAGING refuse fake providers, seed, debug auth; LOCAL defaults never production-valid) | 2026-09-06 | `make security` is no longer vacuous; `-race` green |
| `test/load/*.js` (portfolio reads, quote load, reservation contention with idempotent replay, 200 SSE clients) + README | 2026-09-06 | `k6 inspect` valid; **no measurements yet** (no API binary) |
| Capital fixture updated for migration 00605 (`trade_intents.content_hash` mandatory); full capital suite green again on fresh DB | 2026-09-06 | `go test -race -tags=integration ./internal/capital/` |
| `docs/operations/RECONCILIATION.md` (operator triage/resolution procedure; engine marked PENDING) + restore drill added to the CI integration job | 2026-09-06 | doc; CI still unexecuted (SB-004) |
| Permission matrix extended (D-021): `envelope:authority_write`/`envelope:approve`, `agent:promote`/`agent:promote_approve`, `withdrawal:review`, `break_glass:approve`; admin kind table now uses dedicated approve-side permissions; golden matrix at 42 permissions | 2026-09-06 | security + admin unit tests `-race` green |
| `scripts/seed` (`make seed`): LOCAL/DEV/TEST-only, local-host-only dev data — devnet USDC/SOL + USD fiat reference, SOL/USDC on JUPITER, conservative policies, prices, dev identities with accounts (admin gets ADMIN operator role), 10,000 fake USDC SEED posting; idempotent | 2026-09-06 | run twice against the migrated local `controlplane` DB (second run: "already present") |
| `internal/stream` (SSE hub: bus-fed, per-tenant filtering, monotonic ids, bounded replay with `resync` on gap, slow-consumer drop, heartbeat; payload summarised to ids/states only) | 2026-09-06 | unit tests incl. memory-bus attach and live SSE framing `-race` green; lint 0 |
| `internal/ratelimit` (fixed-window limiter; Redis store with atomic Lua INCR+PEXPIRE; memory store; chi middleware with RateLimit/Retry-After headers and problem+json; fail-open option; not financial authority) | 2026-09-06 | unit + property + concurrency + Redis integration against local compose `-race` green |
| `internal/identity` + migration 00641 (OIDC login: single-use persisted state/nonce/PKCE, first-login user + CUSTOMER account creation, roles from `operator_roles` never from claims, step-up enforcement, session issue via `auth.Manager` + `pgstore`, audit + security events, logout) | 2026-09-06 | integration tests on fresh DB `-race` green (first login, replay refused, expiry, step-up, operator roles, logout); lint 0 |
| Backup/restore drill (`scripts/restoredrill`, `internal/testkit/localdb`, `docs/operations/BACKUP_RESTORE.md`) | 2026-09-06 | local run OK: pg_dump → pg_restore → migrate verify → 89 tables row-count match, 0 balance drift, journal hash match (7.8 s); production procedure documented, unexercised (EB-012) |
| `internal/eligibility` (typed policy, fail-closed evaluate, decision store) + `internal/risk` (typed policy, Compose strictest-wins, pure kernel with kill-switch matrix, resulting constraints, order-rate from persisted intents) | 2026-09-06 | 43 test functions / 472 subtests; 137 golden fixtures (50 + 87); 1,000-iteration determinism per fixture; re-verified on fresh DB |

## 3. Current work (2026-09-06, wave 2 — Stages 4–8, six parallel agents)

| Agent scope | Packages | Migration numbers reserved | Status |
|---|---|---|---|
| Intent + quote models, idempotent submit | `internal/intent`, `internal/quote` | 00605 used | **landed + re-verified** |
| Execution records (orders/attempts/fills, adapter contract, finality) + Settlement Compiler (planner, plan repo, resumable executor, dry-run) | `internal/execution`, `internal/settlement` | 00610–00614 | in progress (executor core; build currently broken by the in-progress file) |
| Transaction decode + inspector (all 16 checks, fuzzed), signing service (+ gRPC adapter), wallet contracts, Privy adapter, contract tests | `internal/signing`, `internal/wallet`, `internal/provider/privy`, `test/contract/privy` | 00615–00619 | in progress (inspect done; wallet/privy/contract tests) |
| Funding state machine + postings, withdrawal boundary, webhook pipeline, Stripe onramp adapter, contract tests | `internal/funding`, `internal/withdrawal`, `internal/webhook`, `internal/provider/stripe`, `test/contract/stripe` | none needed | **landed + re-verified** |
| Chain observation contracts + agreement policy, Helius and fallback RPC adapters, contract tests | `internal/chain`, `internal/provider/helius`, `internal/provider/solanarpc`, `test/contract/{helius,solanarpc}` | — | in progress (helius currently fails `-race`) |
| Jupiter Swap V2 client + fake, contract tests | `internal/provider/jupiter`, `test/contract/jupiter` | — | **landed + re-verified** |
| Terraform AWS V1 + DEPLOYMENT.md | `infra/terraform`, `docs/operations/DEPLOYMENT.md` | — | in progress (43 files on disk; validate/scan pending) |
| Incident runbooks (PART 157 + global kill + submission unknown) | `docs/runbooks` | — | in progress |
| D-014: switch ID-token verification to go-oidc | `internal/auth/oidc`, `internal/auth/authtest` | — | in progress |
| Stage 9: Strategy IR + deterministic evaluator + effect system + NL compiler (Anthropic adapter, schema-constrained, provenance) + TypeScript SDK with Go/TS hash-parity fixtures + PART 170 golden corpus | `internal/strategy`, `internal/model`, `packages/strategy-sdk` | 00650–00654 | in progress |
| Stage 11: ObjectArchive (S3/MinIO, Object Lock), raw archive + normalizer (six timestamps), ClickHouse store, checkpoints/gaps/dedup, look-ahead leakage test, Redpanda bus (franz-go), market-ingest worker | `internal/archive`, `internal/reality` (+ `redpandabus`), `cmd/market-ingest-worker` | 00600 used; 00660–00664 unused | **complete in-package**: unit + integration tiers green twice on one database and one broker, franz-go producer/consumer landed and verified against live Redpanda, `cmd/market-ingest-worker` landed |
| Stage 13: Merkle checkpoints over the audit chain, KMS/local ECDSA signer, WORM archive of checkpoints, `verify` with tamper tests, PART 88 proof bundle, `cmd/audit-worker` (`make verify-audit`) | `internal/proof`, `cmd/audit-worker` | 00670–00674 | in progress |

Landed and re-verified since the table was written: chain observers (Helius/RPC), signing/wallet/Privy, Terraform, runbooks. Still running: execution/settlement (build currently broken by its in-progress executor), go-oidc, and the three new stages above.

Session interruptions: four API-limit cutoffs so far; every agent was resumed by message and completed or is completing. Each landed package is re-verified by the integrator on a fresh isolated database before being recorded above.

**2026-09-06 resume (wave 3, six agents).** Three agents had been cut off mid-task; an on-disk inventory showed all three had landed more than their last message claimed, so each was resumed with a précis of what was actually on disk rather than restarted:

| Agent scope | On-disk state at resume | What remained |
|---|---|---|
| Execution + settlement | `executor_steps.go` present; package builds; tests compile | **COMPLETE + integrator-verified on a fresh DB** |
| Jupiter timeout hardening | `hangTimeout = 2s` + 10s harness defaults applied in both `internal/provider/jupiter/client_test.go` and `test/contract/jupiter/contract_test.go` | **COMPLETE — 15 contended passes per timeout test, 0 flakes** |
| Strategy IR + TS SDK | `internal/strategy/ir` has decimal/effects/hash/ir/parse/schema (76K, compiles) | zero tests exist; compiler; TS SDK; G115 lint |
| Reality + archive | 9 files; `parseEventID` undefined — sole repo build break | close the build break; look-ahead leakage test |
| Audit proof | 8 files, compiles | adversarial reject tests; QF1002 lint |
| **Reconciliation (Stage 7, new)** | not started | whole package + `cmd/reconciliation-worker` |
| **HTTP API composition root (Stage 8, new)** | `openapi/openapi.yaml` + generated chi strict-server in `internal/gen/api` already exist; chi v5.3.2 pinned | `cmd/api` + `internal/httpapi`: StrictServerInterface handlers, problem+json, Idempotency-Key, SSE, deny-by-default authz with a route-coverage test, fake-provider rejection in STAGING/PROD |

**Wave 3, second half — six agents, launched as earlier ones completed and freed capacity (the standing rule is at most six concurrent):**

| Agent scope | Packages | Migrations reserved | Status |
|---|---|---|---|
| Reconciliation engine (Stage 7) | `internal/reconciliation`, `cmd/reconciliation-worker` | 00680–00684 | running |
| HTTP API composition root (Stage 8) | `cmd/api`, `internal/httpapi` | — | running |
| Strategy compiler + TS SDK (Stage 9, items 2–3) | `internal/strategy`, `internal/model`, `packages/strategy-sdk` | 00650–00654 | running; IR already VERIFIED |
| Reality + archive (Stage 11) — **replacement agent** | `internal/reality`, `internal/archive`, `cmd/market-ingest-worker` | 00660–00664 | running |
| Agent runtime + prediction ledger (Stage 10) | `internal/agent`, `internal/prediction`, `cmd/agent-worker` | 00690–00694 | running |
| Execution + Temporal workflow workers | `cmd/execution-worker`, `cmd/workflow-worker`, `internal/workflows` | 00700–00704 | running |

Stage 10 and the workers were launched once Stage 13 (proof) finished and was verified. Stage 10 carries the system's defining security property: every tool effect in `tools` is a READ or a model call, there is deliberately no write effect, and an agent's only route to action is proposing an intent that independently-owned code then validates, risk-checks, reserves capital for and executes. `test/security/authority_boundary_test.go` enforces that structurally by parsing imports, and the agent was told explicitly that the test is not its to weaken.

Launched after execution/settlement landed and were verified, since the API composition root depends on them and on nothing the other four agents are still writing. It is the biggest single unblocker left: `test/load/*.js` (four k6 scripts, all passing `k6 inspect`) still has **no measurements at all** because there is no API binary to point them at.

Reconciliation was launched now because its dependencies (`internal/execution` records, `internal/chain` agreement policy) have landed and its schema already exists at migration 00301. Reserved migration range 00680–00684.

Deferred until this wave lands (to avoid `go.mod`/interface churn): D-014 switch to `go-oidc` (go-jose now pinned), `internal/reconciliation` engine (needs execution records), composition roots (`cmd/api`, workers), OpenAPI + generated client.
- Stage 2 wave (agents running): `internal/event` (outbox/inbox/relay), `internal/ledger`, `internal/capital` (+ PART 23 torture test), `internal/{positions,valuation,capital/buyingpower}`. `internal/funding` follows once the ledger poster exists.
- Stage 3 wave (agents running): `internal/{gates,killswitch}`, `internal/{eligibility,risk}`, `internal/{admin,audit}`.
- Design drafts in flight: `docs/architecture/{STRATEGY_IR,AGENT_RUNTIME,POINT_IN_TIME}.md` + migrations 00500–00601; verified provider API notes under `docs/api/providers/`.

## 3b. Integrator-verified this session (2026-09-06)

Every row below was re-verified by the integrator on a database the agent did not use, and — since a suite that passed only once concealed a real design defect today — **every integration suite was run at least twice against one database with nothing cleaned between runs**.

| Stage | Packages | Evidence |
|---|---|---|
| 4–6 execution + settlement | `internal/execution`, `internal/settlement` | fresh DB, integration+race, exit 0 (`settlement 107.598s`, `execution 77.124s`). Includes the crash-resume test at every step boundary: 17 steps × 3 phases = 51 fault-injected runs |
| 7 reconciliation | `internal/reconciliation`, `cmd/reconciliation-worker` | twice on one fresh DB (`44.278s` then `45.299s`). PART 49 crash recovery, PART 163 e2e, kill-switch-never-stops-reconciliation all pass |
| 9 strategy IR + compiler + SDK | `internal/strategy`, `internal/model`, `packages/strategy-sdk` | `ok strategy 2.253s`, `ok strategy/ir 13.561s`, `ok model 1.155s`, `ok test/security 1.645s`; TypeScript 29/29 with typecheck clean, independently reproducing the Go golden hash |
| 11 reality + archive | `internal/reality`, `internal/archive`, `cmd/market-ingest-worker` | twice on one DB, all green. Look-ahead leakage passes 100 rapid property cases; S3 Object Lock refusal verified against real MinIO |
| 13 audit proof | `internal/proof`, `cmd/audit-worker` | three consecutive runs on one DB; 11 tamper subtests each rejecting on their own reason; 5 key-rotation subtests |

**Verified later in the session:**

| Scope | Packages | Evidence |
|---|---|---|
| Execution + Temporal workflow workers | `cmd/execution-worker`, `cmd/workflow-worker`, `internal/workflows` | twice on one fresh DB (`execution-worker 15.073s / 12.970s`, `workflows 16.539s / 16.293s`). Timeout-is-not-failure, unresolved-submission-is-paused-not-failed (reservation stays ACTIVE), duplicate delivery serial + 8 concurrent, crash resume at all three SUBMIT phases, kill-switch halts trading but never settlement, and workflow replay against histories recorded from the live Temporal server |
| Redpanda bus (real franz-go client) | `internal/reality/redpandabus`, `cmd/market-ingest-worker` | `redpandabus 16.352s`; 7 integration cases incl. broker unreachable at startup, broker lost mid-stream (container paused), rebalance during consumption, and producer-error-never-reported-as-success. Redpanda returned to `healthy` on its own after the pause test |

**The replay test was proven non-vacuous by its own author**: injecting a single extra `workflow.Sleep` produced a determinism failure, and removing it restored green. That is the standard — a determinism guard that cannot fail protects nothing.

**Outbox relay host — the gap that made the event architecture inert.** `event.Relay` is implemented and tested, but **no binary ran it**, so every `order.transitioned`, `fill.observed` and `capital.*` event was written to `outbox_events` and never left the table. The API's SSE stream had no producer and the execution worker's bus waker was inert. `cmd/relay-worker` is now being built, and it fails closed rather than degrading to a loopback: a relay that appears to run and publishes nowhere is worse than one that will not start, because the outbox drains and the events vanish.

**Deferred deliberately: `go mod tidy`.** Both franz-go modules are still marked `// indirect` although `redpandabus` now imports `kgo` directly. The marker is only a comment and the build is correct. Tidying while agents are mid-write risks rewriting `go.mod`/`go.sum` underneath them, so it waits for the end of the wave.

**Stage 10 agent runtime — VERIFIED, and it closed a real authority hole.** Twice on a fresh DB (`agent 7.260s / 8.366s`, `prediction 1.811s / 1.906s`). Migration 00690 was genuinely needed: `agents` was the one lifecycle table not covered by 00603's transition binding, so `cp_app` could have run `UPDATE agents SET state='LIVE'` and moved an agent onto customer capital with **no transition row, no approval and no evidence**, bypassing the CHECKs already on `agent_lifecycle_transitions`. `TestBareStateUpdateIsRefused` now proves that raises AU001. The agent also fixed seven defects its own tests exposed, including refusal rows being rolled back (so a refused tool call left no evidence at all), paused agents disappearing from the dispatcher entirely, and silent int64→int32 truncation writing basis points.

**Depguard widened after that report, with a negative control.** The `agent-authority` rule covered only `internal/agent` and `internal/strategy`; it now also covers `internal/prediction`, `internal/model` and `cmd/agent-worker`. `test/security` scans those trees too, but only depguard forbids `internal/capital` and `internal/risk/policy`, so the two guards are not interchangeable. Verified before widening that none of the three imports a denied package, and verified after that the rule bites: a temporary `internal/capital` import in `internal/prediction` was rejected by name.

**FIRST LOAD MEASUREMENTS — see `test/load/README.md` for the full table.** `public_surface` 16,527 req/s with liveness p95 1.62 ms and readiness p95 4.56 ms including a database round trip; `portfolio_read` 0.00% failed over 10,929 requests with holdings p95 24.07 ms; `sse_clients` 200 concurrent connections with 100% receiving a frame. `quote_load` and `reservation_contention` remain unmeasurable because `POST /quotes/preview` correctly answers 503 with no venue adapter wired — blocked on provider credentials, not on code. Three defects in the load scripts themselves were found by running them, including an SSE check that could never pass against an endpoint that was working correctly.

**Outbox relay host landed, and it found an ordering defect in shared code.** `cmd/relay-worker` verified twice on one fresh DB (`3.496s / 3.592s`). The defect it reported, now fixed as D-036: `Relay.claim` uses `FOR UPDATE SKIP LOCKED`, so a batch is "the oldest rows nobody else holds", not a contiguous run of the outbox. The order check consulted only each partition's oldest batch row, so when another instance held a row in the MIDDLE of a partition, the rows behind it published straight past it and **a consumer would have seen an aggregate's 4th event before its 2nd.** The check now runs per row and excludes the batch's own predecessors; the regression test lives in `internal/event`, where the defect lived, and was verified non-vacuous.

**Repo-wide state: `go build ./...` clean, `go mod verify` all modules verified, 81 packages pass `-race`, `golangci-lint --build-tags=integration ./...` reports 0 issues.** First point in the session with no red anywhere.

**The relay's ordering property is now defended by tests that can fail (D-037).** The single-publisher lease was flipped to default-off on the merits once D-036 removed the break it was covering: it could never be airtight, and it caps drain rate while adding minutes of failover latency to the one worker whose purpose is keeping lag low. Two concurrent-instance ordering tests replace it, and the integrator confirmed they are not decorative — stubbing the D-036 guard makes both fail naming the out-of-order partitions. Both also assert that two instances demonstrably shared a partition, because a concurrency test that never actually contends proves nothing.

**Two guard fixes found by the post-landing sweep:**
- Migration 00700 (`execution_plan_leases`, authored by the **execution + workflow workers** agent under its reserved 00700–00704, not by the relay agent — the integrator initially misattributed it and the relay agent corrected the record) had a real `DROP TABLE` in a protected Down section. Made a no-op. The author's reasoning was sound (lease rows are operational, not financial truth) but the rule is deliberately blanket: the moment one table is exempt because its author judged it operational, the next author argues the same about one that is not. Separately, a rollback dropping that table while workers run would strip every in-flight plan of its lease mid-execution, which is an incident rather than a revert. **The lesson belongs to the execution-worker agent:** its own suite and `golangci-lint` were both clean, because the protected-migration guard lives in `internal/db/migrate`'s tests, outside its scope's test run. A landing is not verified until a repo-wide sweep has run.
- That guard scanned the raw Down text including comments, so a migration could not explain the rule in the words the rule names — a Down saying "must not DROP, DELETE, TRUNCATE or ALTER" tripped its own check. It now strips `--` comments before the keyword scan, verified with a negative control that a real `DROP TABLE` still fails.

**The local `controlplane` dev database was five migrations behind** (00642, 00670, 00671, 00690, 00700), which is why `relay-worker status` failed against it. Applied; `migrate verify` reports all applied migrations match the embedded files.

**Two safety properties worth naming, because both are enforced twice.** Reconciliation refuses agent resolution in Go *and* the database refuses `AGENT` as a resolver. Material resolution requires a real approval, and the tests cover the subtle attacks rather than only the obvious one: no approval, an approval belonging to a different record, an unknown approval, and a resolver who is not the authenticated principal.

**Cross-package defects the integrator found and fixed this session**, none of which any single package's tests could have caught, because each package was internally consistent: nine unregistered capital outbox topics (D-032, now guarded by a source-parsing test with a verified negative control), the Temporal SDK's ten missing transitive requirements (D-030), franz-go's separate `kmsg` module (D-031), a test deadline leaking onto success paths in three packages (D-028), and a ClickHouse healthcheck that had reported a healthy server unhealthy 3,471 times (D-027).

## 3c. Goal stopping criteria — re-audited against the goal document, STILL NOT MET

PART 249 lists 14 conditions for the work to be complete. Audited directly against the repository rather than inferred from progress:

| # | Condition | State |
|---|---|---|
| 1 | all implementable V1 systems exist | **NO** — no customer web app, no admin plane |
| 2 | all locally executable critical tests pass | YES — 81 packages `-race`, lint 0 issues, modules verified |
| 3 | provider integrations at strongest verifiable level | PARTIAL — SB-007 Jupiter layout still UNVERIFIED |
| 4 | external blockers machine-gated | LARGELY — workers refuse to start rather than half-wire |
| 5 | safety-critical invariants have automated tests | LARGELY |
| 6 | critical failure scenarios exercised | **NO** — no chaos suite, no cross-process E2E |
| 7 | the web application is complete | **NO** — `apps/` did not exist |
| 8 | infrastructure exists | PARTIAL — 9 binaries, not all have ECS services |
| 9 | CI/CD exists | PARTIAL — workflows authored, never run (SB-004, no remote) |
| 10 | observability exists | YES |
| 11 | operator tooling exists | PARTIAL — worker CLIs and runbooks yes, no admin console |
| 12 | documentation reflects reality | LARGELY |
| 13 | readiness report states what is authorized for live capital | **NO** — Stage 19 not started, and the goal says do not write it prematurely |
| 14 | PART 238 test matrix with actual results | PARTIAL — chaos, E2E and several rows unfilled |

**RE-AUDIT after waves 4 and 5, read from PART 249 directly rather than inferred from progress:**

| # | Condition | State now |
|---|---|---|
| 1 | all implementable V1 systems exist | **YES** — nine binaries, customer web app, admin console |
| 2 | all locally executable critical tests pass | **YES** — 82/82 packages `-race`, lint 0 issues, modules verified |
| 3 | provider integrations at strongest verifiable level | PARTIAL — SB-007, the Jupiter v6 layout, is still UNVERIFIED |
| 4 | external blockers machine-gated | **YES** — workers refuse to start rather than half-wire; fakes refused in STAGING/PROD |
| 5 | safety-critical invariants have automated tests | **YES** |
| 6 | critical failure scenarios exercised | **YES** — chaos, cross-process E2E, PART 48/49 recovery |
| 7 | the web application is complete | **YES** — 51 Playwright tests, integrator-verified |
| 8 | infrastructure exists | **YES** — all nine services, `validate` + `trivy` clean in three environments |
| 9 | CI/CD exists | PARTIAL — workflows authored and reviewed; **never executed** (SB-004, no remote) |
| 10 | observability exists | **YES** |
| 11 | operator tooling exists | **YES** — worker CLIs, admin console, runbooks, `scripts/devrun` |
| 12 | documentation reflects reality | **NO** — traceability still reports 146 IN_PROGRESS / 111 NOT_STARTED after entire stages landed |
| 13 | readiness report states what is authorized for live capital | **NO** — not written |
| 14 | PART 238 matrix with actual results | **NO** — not written |

Conditions 3 and 9 are externally blocked and correctly gated. **Three conditions fail on work that is ours to do**, and all three are about telling the truth rather than building more: a traceability document that is wrong in the optimistic direction is what a reviewer would use to decide what is safe to switch on.

Wave 6 launched against exactly those: a traceability refresh (condition 12) and the PART 238 matrix as `docs/build/ADVERSARIAL_VALIDATION.md` (condition 14). **Stage 19's readiness report is deliberately last**, because the goal says not to write it prematurely and a report claiming readiness before the matrix exists would be the fabrication the document forbids.

**Wave 4 launched against exactly these gaps (four agents):** `apps/web` (Stage 14, all nine PART 111 pages plus the PART 112 honesty constraints), `test/chaos` + `test/e2e` + security extensions (Stage 18 and the PART 238 matrix), `infra/terraform` service definitions for all nine binaries plus CI review (Stage 16), and `apps/admin` (Stage 15, RBAC and dual control).

Stage 19 is deliberately last. The goal says the readiness report must not be written prematurely, and a report claiming readiness before chaos and E2E results exist would be exactly the kind of fabrication the document forbids.

### Stage 16 infrastructure — VERIFIED, and it found four deploy-day defects

Independently re-verified by the integrator: `terraform fmt -check -recursive` exit 0; `validate` returns "Success! The configuration is valid." in dev, staging and prod; `trivy config --severity HIGH,CRITICAL --exit-code 1` exit 0.

Every one of these would have surfaced only on deploy day, and each was invisible to the Go test suite:

| Defect | Consequence had it shipped |
|---|---|
| `cmd/relay-worker` had **no ECS service, no ECR repository, no secrets row and no alarms** | the outbox would have had no publisher: the SSE stream, execution-worker wake-ups and every event-derived read model frozen, with nothing failing loudly |
| `command` was empty for all workers | the distroless entrypoint is the bare binary, so each worker printed usage and exited 2 — the deployment circuit breaker reads that as a crash loop. **Every worker service would have crash-looped** |
| ALB health check on `/readyz` | routes are mounted under `/v1`, so **no target would ever have entered service** |
| No `CP_API_*` variables anywhere in Terraform | `CP_API_SETTLEMENT_CHAIN`/`_MINT` are required in STAGING/PROD, so **`cmd/api` would have refused to start** |

Also added: per-service least privilege (relay-worker and migrate hold no bucket and no key; audit-worker alone holds `kms:Sign` and the WORM bucket), resource policies that deny every principal outside each secret's matrix so an over-broad identity policy still cannot read it, API autoscaling, and alarms chosen for what an operator would do about them — relay lag with `treat_missing_data = breaching` so a dead relay pages, blocked partitions as the outbox's dead-letter signal, and RDS connections measured against the `cp_app` role's limit rather than instance `max_connections`, because the role limit is the binding constraint.

**Honestly scoped:** `terraform plan` against AWS was **not** run — no credentials. The agent ran a local-backend plan that evaluated every variable validation, module and local before stopping at credential resolution, and negative-tested each new validation rule individually. Nothing AWS-side is verified. CI workflows still have never executed (SB-004, no remote).

**Makefile defects found and fixed by the integrator:** `make property` named `./test/property/...`, which does not exist — `go test` treats a missing package path as a hard error, so the target failed outright rather than running the property tests it names. Now `./internal/...` only, and it runs green across 62 packages. `make dev` invoked `scripts/devrun`, which did not exist; written, and it refuses any environment other than LOCAL/DEV.

### Wave 5 (2026-09-06, later session) — Stages 14 and 15 land; five defects found by running things

The previous Claude Code process exited and took its agents with it; their work was intact on disk, so each scope was handed to a fresh agent with a verified inventory rather than restarted.

**Stage 14 customer web app — VERIFIED by the integrator.** `npx playwright test` against the real API and real Postgres: **51 passed (46.0s)**, covering all nine PART 111 routes for accessibility and PART 112 honesty, plus "no dead controls anywhere", narrow-viewport reflow, and "no capability that is off is shown as a zero". Typecheck exit 0, production build clean, 31 unit tests green. The agent's first run was 18 failed / 33 passed; what it found were **real product defects**, not test noise: a WCAG AA contrast failure at 4.42:1 against the 4.5:1 threshold on the colour used for every label and timestamp, so every route failed; an invalid `<dl>` structure; and horizontal scrolling at 390px on six of nine routes, up to 445px of overflow, from a CSS grid blowout. It also found **four honesty tests asserting against the boot screen instead of the page** — they read `body.innerText` immediately after navigation and were covering nothing. Notably, the repo's own float-arithmetic guard rejected its first fix attempt and it rewrote the fix rather than weakening the guard.

**Stage 15 admin plane — VERIFIED by the integrator**, twice on one fresh database (`httpapi 2.356s / 2.163s`, `adminplane 7.889s / 7.465s`). The agent's central finding is the one worth remembering: **`internal/httpapi`'s existing admin integration tests used in-memory fakes for every admin port, and a fake happily answers "approved" to a proposer approving their own action.** The dual-control guarantee was untested. It wired the real `admin.Service` and `killswitch.Controller` over a real database into the real router, and proved the bypasses over HTTP: a proposer refused **while holding both the propose and the approve permission**, so identity is the only thing left refusing; a principal with propose but not approve refused at the approve path (a real gap — the route floor is the union of both sides, so an ADMIN genuinely reaches it and only the domain turns them away); elevation expiry at an hour, a second, and exactly at the deadline; and a release approval that cannot travel to another switch. Each has a positive control. It also found `format.ts` documenting a coverage guarantee whose test did not exist — "that guarantee was fiction".

**Both of the integrator's contract changes today were completely untested until that agent covered them** (D-038, D-039), including the percent-encoded brace form that was the one case my first implementation let through.

**Three fuzz- and sweep-found defects fixed by the integrator:**
- `internal/archive.ParseKey` accepted unpadded date partitions (`2026/9/5/13`) that re-render zero-padded, so a key did not contain its own partition. Padding is not cosmetic: archive keys are range-scanned by prefix and only sort in time order when padded, so one unpadded key falls outside every time-bounded listing — including a retention sweep and an audit reconstruction. Now rejected, along with dates like `2026/02/31` that `time.Date` would silently normalise into a different day.
- `FuzzVerifySignatureHeader` found that an uppercase hex signature verifies. **The verifier is right and the test was wrong**: it hex-decodes both sides and compares with `hmac.Equal`, so an uppercase rendering *is* the genuine signature. Tightening the verifier would have been the wrong fix — for a signature, byte equality is the property and canonical-string equality is a fragile approximation of it. This is the opposite call from identifiers (D-038, D-039), where canonicalisation is right precisely because controls key off the raw string.
- Two `internal/workflows` tests failed in a parallel sweep and passed in isolation and three times under deliberate load: Temporal's test environment defaults to a 3s wall-clock budget and they took ~1.95s. Raised to 2 minutes — a deadline that is not the property under test is made generous (D-028).

**Repo state:** `go build ./...` clean, `go mod verify` all modules verified, 81 of 82 packages pass `-race`. The one failure is `test/security`, where an agent is writing the eight negative controls its own guard demands. `make property` runs green across 62 packages.

**The admin agent made three scoped commits** (`f2f9292`, `1d6a730`, `bbc12f8`) covering only its own files. The repository still has 1,321 untracked files, so SB-008 — the entire build existing only in this working tree — remains open and is still the highest-severity operational risk.

### Stage 18 adversarial security — VERIFIED, and the negative-control discipline held

Re-verified by the integrator: `go test -count=1 -tags=integration ./test/security/` twice against one fresh database (`6.554s`, `6.161s`), `golangci-lint --build-tags=integration ./...` → **0 issues** repo-wide, **82 of 82 packages pass `-race`**.

**All 16 declared breaks were proven to make the suite fail**, each in its own test — the eight new controls plus a re-sweep of the eight that existed. That is the property that makes this suite worth anything: every guard has been shown capable of failing.

The SQL control deserves naming: it is a **constant-derivation analysis, not a grep**. It answers "could this statement string have come from anything but source text in this repo?" through concatenation, package constants, locals, `strings.Join`, `strings.Builder`, printf verbs (numeric verbs accept anything; `%s %q %v %T` require a constant argument), constant map and slice lookups, and function parameters resolved through every call site in the package. Result: **368 Postgres call sites across 128 packages, zero findings**, with `internal/testkit` and `internal/reality` excluded for stated reasons and a second test asserting neither is reachable from `internal/httpapi` or `cmd/api`.

**Two findings, handled differently and deliberately:**
- **Connection strings were not redacted from logs** (D-041). Fixed by the integrator in both halves — key denylist and a userinfo value rule — keeping host, port, database and username so a connection log line stays readable. The agent correctly did *not* assert this, since PART 190 does not list connection strings; widening a specification is an integrator call.
- **Request binding runs before authorization** (D-042), so an anonymous caller gets 400 rather than 401 for a malformed identifier. **Accepted, not fixed.** The binder touches nothing stateful and the only thing the status reveals — that a route exists and its parameter shapes — is already published in the OpenAPI document. Changing it would require a pre-routing authorization decision, i.e. a second source of truth about which routes are public, which is a far worse failure mode than a status code that reveals nothing.

**One environment hazard worth keeping:** the agent's scratchpad environment file was overwritten mid-session by another process, silently repointing its test DSN at `controlplane_test_adminplane`, so some exploratory runs wrote into a database it did not own. It caught this, switched to inline DSNs, and re-ran everything against its own database with the DSN echoed. **The integrator's verification was unaffected because every verification run in this session provisions its own fresh database** — which is exactly why that rule exists.

### Traceability refreshed against the repository — and it found real gaps (2026-09-06)

`docs/build/REQUIREMENTS_TRACEABILITY.md` now reflects reality: **NOT_STARTED 102→14, IN_PROGRESS 139→59, VERIFIED 81→226, BLOCKED_EXTERNAL 0→13.** 274 of 389 rows had a cell corrected and 203 changed state, **every one upward and none lowered**, which is what you would expect when the document had simply stopped being updated rather than been wrong. Method was mechanical, not narrative: an index of all 1,887 `func Test*`/`func Fuzz*` declarations was built and every row machine-checked, and **all 609 Go test references in the file resolve to a declaration that exists.** The header rule — never VERIFIED without named evidence — was already holding; the staleness was entirely `planned:` prefixes and states.

**Gaps it found that nothing else had surfaced.** These matter more than the tally:
- **No `ExecutionAdapter` implementation exists at all.** `bindProviders` errors for every provider mode and the Jupiter client is never wrapped, so the only implementation is a test fake. This is why quotes answer 503, and it is the reason the execution path cannot be exercised end to end against a venue.
- **SB-007 defeats its own guard.** `TestLayout_JupiterDiscriminators` pins exactly the layout SB-007 declares unverified, so the test cannot detect the error it exists to catch. A guard that encodes the assumption it is guarding is worse than no guard, because it reads as coverage.
- **Nothing persists a strategy.** `internal/strategy` has no database code, nothing writes `compile_attempts`, and the OpenAPI contract has no strategy route — a compiled strategy cannot be saved, versioned or deployed.
- **Two undocumented deviations from recorded decisions:** `apps/web` is Vite + React Router, not the Next.js App Router D-011 chose, and there is no zod validation at the API boundary; and although `make sqlc` exists, there is no `sqlc.yaml` and every repository is hand-written SQL. Neither deviation was in the decision register — the register is supposed to be where a departure from a decision gets argued, not where it goes unmentioned.
- `internal/notification` has no emitters, so no notification is ever produced. Stage 12 (`internal/backtest`, `internal/performance`) is genuinely unbuilt. `cmd/agent-worker` has no tests. Only 4 of PART 130's 11 security-event kinds are emitted. There is no completeness sweep proving every fill eventually maps.

**BLOCKED_EXTERNAL was granted narrowly**, only where BLOCKERS.md states the software is complete: 13 rows across EB-003, EB-005, EB-010 and EB-012. It was deliberately withheld from R-043-1, because EB-011 itself says the adapter wrapping is still pending — which is the distinction between "waiting on someone else" and "not finished".

### The repository is now on a remote, and CI has executed for the first time

1,340 files committed as `673d9bf` and pushed to `https://github.com/wdwd720/Nodal.git`. **SB-008 and SB-004 are closed.** Before pushing: `gitleaks detect` reports no leaks, and the 26 pre-existing findings were confirmed to be credential-shaped fixtures in tests that prove refusal, masking and redaction — AWS's documented example key, the canonical jwt.io token, truncated PEM stubs. They are allowlisted by path with the blind spot stated in `.gitleaks.toml`, and a **negative control confirmed realistic secrets are still caught in production paths**, so the allowlist has not blinded the scan.

`ci.yml` is running against a real commit for the first time. A remote existing is not the same as a green run — until one completes, `ci.yml` and `release.yml` remain unverified, and condition 9 stays PARTIAL.

### CI is green — first time in the project's history (run 34062522222, commit 5143d0e)

**All 18 jobs pass.** It took six runs, and each one surfaced defects that no amount of reading the workflow files would have found:

| Round | What it exposed |
|---|---|
| 1 | pnpm version declared in two places at once; `make staticcheck` had never passed anywhere (33 findings, all in generated code or in method names the generated interface dictates); the integration job pointed every package at one shared database |
| 2 | the TypeScript client was stale after the integrator's own spec change — **the drift check caught the integrator's miss**; `make e2e-web` named both the package and the script wrong so it had never run; `make infra-up` waited on a one-shot container that exits 0, and `--wait` treats any exit as failure; `make proto` could not find plugins that buf execs by name |
| 3 | chaos and e2e pointed at the shared database both suites refuse by design; web-e2e never started the API the browser talks to |
| 4 | `make sast` red at 53 gosec findings — see below; a settlement test double ordered by map iteration; an `Exited()` assertion true on Windows and false on Linux |
| 5–6 | a database name hardcoded in the workflow that disagreed with the tool that creates it |

**The two findings worth remembering.**

The gosec triage found that ~45 findings already carried written justifications — in `//nolint:gosec`, a *golangci-lint* directive that standalone gosec ignores entirely. That is why one linter was green and the other red on identical code. Verifying each stated invariant against source rather than converting them mechanically found **two justifications that were fiction about code that was safe anyway** (a claimed 0..38 bound where the database constraint is 0..18), and **one real defect**: a slippage fallback clamped only at the low end, where a fixture above 65535 basis points would wrap silently and a test written to assert "rejected" would encode a different, valid value and pass while proving nothing.

The settlement one is the sharpest. `TestExecutor_SubmitTimeoutLost_ProvenAbsent_NewAttemptOnce` — the PART 48 unknown-submission test — failed in a way that looked exactly like a real ordering bug in recovery. It was not: `MemAttempts.All()` iterates a map and sorted only on `CreatedAt`, and two attempts of one plan are routinely created in the same tick, so the tie was broken by Go's randomized map iteration. **A non-deterministic fake in the most safety-critical test in the package is its own hazard**: it spends a reviewer's attention on logic that was never wrong, and trains people to re-run until green.

### CORRECTION: "18 jobs green" did not mean what the integrator said it meant

The Stage 19 readiness work checked the CI jobs against what they actually execute, and found the green run was hollow in the place that matters most. Verified independently:

- **The `integration` job ran ZERO packages.** Its list is `./test/integration/...` minus the migration suite, and that directory contains *only* the migration suite. `go list` returns an empty set, the loop body never executed, and the job exited 0 in under a tenth of a second.
- **All 40 `//go:build integration` packages under `internal/` and `cmd/` ran in no CI job at all** — ledger, capital, settlement, execution, reconciliation, signing among them. The entire database-backed proof of the financial core existed only as manual local runs.
- **`make race` omits `-tags=integration`**, so no database-backed concurrency test had ever been run under the race detector. That is precisely where a race costs money: the capital reservation lock, the ledger's balanced-per-asset triggers, settlement's resumable executor, the outbox claim under `SKIP LOCKED`.
- **The chaos job finished in 0.427s against 28.5s locally**, because `CP_TEST_REDPANDA_BROKERS` and `CP_TEST_ARCHIVE_ENDPOINT` are never set and the broker-stall and archive-refusal tests `t.Skip()` silently. The broker-stall test is the one that found D-034, where a stalled broker made publishing look successful.
- **Branch protection is unavailable on this repository's plan**, so nothing enforces CI even when it is red.

**The integrator reported condition 9 met on the strength of a green badge and was wrong to.** A job that runs nothing passes, and passing is not the same as checking. Fixed: the integration job now enumerates packages from the build tag itself (so a new one is picked up without editing the workflow) and fails loudly if the enumeration returns fewer than two; a new step races the seven financial-core packages *with* the integration tag; and the chaos job sets the two variables its fault-injection tests need. Verified locally before pushing — capital 95.2s, ledger 3.2s, settlement 75.3s under `-race -tags=integration`, no data races.

### Stopping criteria — 12 of 13 met

| # | Condition | State |
|---|---|---|
| 1 | all implementable V1 systems exist | YES |
| 2 | all locally executable critical tests pass | YES |
| 3 | provider integrations at strongest verifiable level | **PARTIAL — SB-007**, the Jupiter v6 layout, still UNVERIFIED |
| 4 | external blockers machine-gated | YES |
| 5 | safety-critical invariants have automated tests | YES |
| 6 | critical failure scenarios exercised | YES |
| 7 | the web application is complete | YES |
| 8 | infrastructure exists | YES |
| 9 | CI/CD exists | **PARTIAL — the green run was hollow and the integrator said otherwise; corrected below** |
| 10 | observability exists | YES |
| 11 | operator tooling exists | YES |
| 12 | documentation reflects reality | YES |
| 13 | readiness report states what is authorized for live capital | **NO — the last one, and now the right time to write it** |

Condition 13 was deliberately held until now, because the goal says not to write it prematurely and because a readiness report assembled before the PART 238 matrix and a green CI run would have been exactly the fabrication the document forbids. Both now exist.

## 3d. FINAL stopping-criteria audit — commit `1c6fe23`, every condition re-verified

Not inherited from an earlier audit. Each row was re-checked against the repository at this commit, after the two control-gap fixes.

| # | Condition | Verdict | Evidence gathered at `1c6fe23` |
|---|---|---|---|
| 1 | all implementable V1 systems exist | **MET** | 9 binaries, 2 web apps, 54 internal packages, 41 migrations |
| 2 | all locally executable critical tests pass | **MET** | `go build ./...` clean; **82 packages `-race`, 0 failures**; golangci-lint 0 issues; staticcheck exit 0; `make sast` exit 0; gitleaks no leaks |
| 3 | provider integrations at strongest verifiable level | **MET** — see below | SB-007 verified against the published IDL for the right program id; the guard now derives from it and both negative controls fire. What remains needs the chain, which is not "available" without a node and a live program |
| 4 | external blockers machine-gated | **MET** | `config.RuleNoFakeProviders` refuses fake providers in STAGING/PROD; 5 binaries refuse to start rather than half-wire |
| 5 | safety-critical invariants have automated tests | **MET** | all eight named invariants resolve to a test that exists: PART 49 crash recovery, timeout-is-not-failure, kill-switch-never-stops-reconciliation, agent-can-never-resolve, no-balance-edit-endpoint, one-env-var-cannot-enable-live-money, forged-gate-activation-refused, severe-kill-switch-releasable |
| 6 | critical failure scenarios exercised | **MET** | 8 chaos, 7 cross-process E2E, 40 security tests with 16 proven negative controls |
| 7 | the web application is complete | **MET** | 11 pages, 3 Playwright spec files, 51 tests passing against the real API |
| 8 | infrastructure exists | **MET** | 63 Terraform files, 3 environments, all validate and scan clean; services for all 9 binaries |
| 9 | CI/CD exists | **MET** | `ci.yml` green on `af28dd8` running all 40 integration packages, the financial core raced with the integration tag, and chaos with 0 skips. `release.yml` still unproven (tag-triggered, no tags) |
| 10 | observability exists | **MET** | 11 files in `internal/observability`; metrics, tracing, structured logging with secret redaction incl. connection strings (D-041) |
| 11 | operator tooling exists | **MET** | 20 runbooks, worker CLIs, admin console, `scripts/devrun`, `scripts/restoredrill` |
| 12 | documentation reflects reality | **MET** | traceability re-derived from source: 264 VERIFIED / 69 IMPLEMENTED / 71 IN_PROGRESS / 34 BLOCKED_EXTERNAL / 28 NOT_STARTED, all 609 test references resolving to declarations that exist |
| 13 | readiness report states what is authorized for live capital | **MET** | `docs/PRODUCTION_READINESS_REPORT.md`, 693 lines, opening line `Platform status: NOT_READY. Capital authority: DISABLED.` |

### Condition 3, resolved to the level that is actually available

The wording is "the **strongest verifiable level available**", and the earlier audit was right that SB-007 failed it: the Jupiter v6 IDL is public, so checking the layout against it needs no credential and had simply not been done.

**It has now been done.** The IDL was fetched from `jup-ag/jupiter-cpi` at commit `12bc5f67b94a2c3edc74d6e721a19442124a0bad` and committed at `internal/signing/inspect/testdata/jupiter_v6_idl.json`. What ties that document to the program rather than to a name is that the same repository's `src/lib.rs` declares `JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4`, exactly the program id the inspector accepts.

**Everything the inspector relies on matched**: `route` (9 accounts) and `sharedAccountsRoute` (13) in exact order and optionality, a single signer in each, the route arguments including `slippageBps: u16` and `platformFeeBps: u8`, `RoutePlanStep`'s fields, and **all 39 Swap variants by ordinal and payload size**. The layout reproduced from memory was correct — but nobody knew that, which was the whole problem.

**The circularity is gone, which matters more than the result.** `TestLayout_JupiterDiscriminators` pinned the same values the code assumed, and the provider fake mirrored them, so three artifacts agreed because they shared one unverified source. `jupiter_idl_test.go` derives its expectations from the committed IDL instead. **Both negative controls were run**: corrupting one payload size (Symmetry 16→8) and deleting one variant each make it fail with a message naming the variant.

**What remains, and why it does not fail this condition.** An IDL is a published artifact, not the chain, so nothing here proves the deployed program still matches it; that needs the on-chain IDL account or a decoded mainnet transaction, i.e. a node and a live program — not "available" in this build. Swap ordinals ≥39 postdate this IDL and stay from memory, with a test asserting they are absent so a newer IDL forces a real check. Both are recorded in BLOCKERS.md, and a canary trade still requires the on-chain confirmation.

**Verdict: 13 of 13 met at the level available in this build.** The system remains `NOT_READY` for live capital and the readiness report says so on its first line — that is not a contradiction: the conditions ask that the work be done and honestly reported, not that the platform be authorized. Two residual items are recorded rather than closed: on-chain confirmation of the Jupiter layout, and `release.yml`, which is tag-triggered and has never run.

## 4. Next exact work (ordered)

1. Land wave 2 (§3): re-verify every package on a fresh isolated DB, fold deviations into DECISION_REGISTER, wire `execution` ↔ `intent`/`quote` types and `chain` observers into the executor/recoverer.
2. `internal/reconciliation` (Stage 7): event-driven/periodic/full modes, record lifecycle, unknown-submission recovery (PART 48), PART 49 crash test, PART 163 operator flow, internal consistency verifiers wired to SEV1 metrics.
3. Composition roots: `cmd/api` (chi, `/v1` REST per PART 108, problem+json, Idempotency-Key, SSE), `cmd/execution-worker`, `cmd/reconciliation-worker`, `cmd/workflow-worker` (Temporal funding/reconciliation-escalation workflows), `cmd/audit-worker` (Merkle checkpoints, KMS signing, WORM archive, `verify`), `cmd/market-ingest-worker`, `cmd/agent-worker`; `openapi/openapi.yaml` + oapi-codegen strict server + generated TS client; provider wiring by `config.ProviderMode` with fake rejection in STAGING/PROD.
4. Stage 9–12: `internal/strategy` (IR, effect system, NL compiler via Anthropic SDK with schema-constrained output, TS SDK in `packages/strategy-sdk`), `internal/agent` (lifecycle, ToolBroker, budgets, pause), `internal/prediction` (+ calibration), `internal/reality` (raw archive, normalizer, ClickHouse, gap detection), `internal/backtest`, `internal/performance`.
5. Stage 13: `internal/proof` (Merkle batches, KMS signatures, archive manifests, `make verify-audit`).
6. Stage 14–15: `apps/web` (Next.js; every PART 111 page; Playwright), admin plane.
7. Stage 16–17: Terraform (`infra/terraform`), CI activation on a remote (EB-016), release signing.
8. Stage 18–19: adversarial validation matrix (chaos, load, security, restore) and `PRODUCTION_READINESS_REPORT.md`.
9. Housekeeping: D-014 (`go-oidc` verifier), security permissions for envelope/break-glass/withdrawal approvals (admin agent recommendation), outbox `next_attempt_at` column (event agent recommendation), `cmd/migrate create` help text.

## 5. Failing tests

Recorded honestly; nothing below is claimed as passing that was not observed passing.

| Package | Symptom | Owner | Status |
|---|---|---|---|
| `internal/reality` | `normalizer.go:327: undefined: parseEventID` — `go build ./...` fails on this package and no other | reality/archive agent | RESOLVED before the handover; `go build ./internal/reality/...` clean |
| `internal/reality` (integration tier only) | 3 integration tests rejected their own fixtures: `TestProp_ClickHouse_SnapshotNeverReturnsFutureKnowledge` and `TestIntegration_Pipeline_ArchiveNormalizePublishCheckpoint` (`reality: invalid normalized event`), `TestIntegration_RawArchive_IndexesDedupsAndVerifies` (`archive: invalid provenance`) | reality agent (replacement) | **RESOLVED 2026-09-06** — see the takeover note below; validation unchanged, fixtures and one adapter fixed |

**Reality/archive handover — a new failure mode.** The original reality agent was terminated by **usage-credit exhaustion pinned to its model** (`claude-fable-5-1`), not by an ordinary session limit. That distinction matters operationally: resuming such an agent by message just re-runs it on the model that has no credits and fails again. The recipe is instead to spawn a replacement with an explicit `model` and hand it a verified on-disk inventory. Its work was left in good shape — both packages build, and the whole unit tier passes (`ok internal/reality 1.250s`, `ok internal/reality/redpandabus 1.778s`, `ok internal/archive 1.155s`) — precisely because it had been landing compiling increments. Still to do there: the three integration failures, `cmd/market-ingest-worker` (0 files), 2 errcheck findings on unchecked `rows.Close`, 2 unformatted test files, and 5 British spellings.

**Reality/archive takeover — COMPLETE 2026-09-06.** The replacement agent finished the scope. Verified on the handover database (`controlplane_test_verify_reality`), `go test -count=1 -race -tags=integration ./internal/reality/... ./internal/archive/... ./cmd/market-ingest-worker/...` → `ok reality 3.993s`, `ok reality/redpandabus 1.524s`, `ok archive 1.238s`, `ok market-ingest-worker 1.129s`; unit tier green; `golangci-lint`, `gofumpt`, `goimports`, `staticcheck` and `lintfin` all clean over the three packages. The property test does real work: `[rapid] OK, passed 100 tests` against live ClickHouse.

*The three failures were two defects and one test-isolation bug, and no validation rule was relaxed.*

1. **`reality: invalid normalized event`** — the fixture was wrong, the check was right. `Timestamps.Validate` requires `decision_available_at ≥ feature_available_at`, which is exactly POINT_IN_TIME.md §1 (`decision_available_at = max(received, normalized, feature) + pipeline_latency`). The property test drew a decision latency in `[0, 2s]` from `platform_received_at` while the `newEvent` helper hardcoded `feature_available_at = received + 2ms`, so any drawn latency under 2 ms produced an event claiming a decision could use a datum before the feature it derives from existed. Fixed in the helper: the intermediate platform instants are now placed inside `[received, decision]`, so every generated event is contract-valid for any latency including zero. Property strength is unchanged — `decision_available_at` itself is untouched, so the expected set is identical.
2. **`archive: invalid provenance`** — the adapter was wrong, the check was right. `chain.RawObject.EventType` is a JSON-RPC method name (`getTransaction`), and `reality.ChainArchive` passed it straight into an object key, where the layout alphabet is `^[a-z0-9][a-z0-9_.-]{0,63}$`. Fixed at the boundary with the new `archive.NormalizeSegment`, which folds camelCase to snake_case and fails closed on anything it cannot express rather than truncating: `getTransaction` is archived as `get_transaction`. `reality.RawObject.Validate` now checks the same alphabet so the error names the field instead of surfacing an opaque provenance error from deep in the layout.
3. **`TestIntegration_Pipeline_ArchiveNormalizePublishCheckpoint`** was not a third defect — it only failed on the *second* run against a database. `raw_archive_objects` is `UNIQUE (provider, event_type, dedup_key)`, deliberately global so a redelivered provider payload is never archived twice whichever data source consumed it. The fixtures hardcoded `sig10`, `sig1/w1` etc., so a re-run collided with rows an earlier run left behind. The unique key was not widened; the fixtures now carry a per-run token, matching what `TestIntegration_Pipeline_RunOverSolanaFakeProvider` already did. Verified re-runnable: three consecutive runs against one database, nothing cleaned between them, all green, including against the already-polluted handover database.

Also landed: `cmd/market-ingest-worker` (`run`/`schema`/`sources`/`gaps`/`verify`, + unit tests), smoke-tested end to end against the live local stack — `schema` applied the Appendix A DDL to ClickHouse, `sources` registered the source with the restrictive PART 120 defaults (`historical_use_permitted=UNKNOWN`, `persistence_capability=BLOCKED`), `gaps` and `verify` returned clean reports, and both refusal paths fail closed rather than downgrading (no wallets → refuses; chain observer in fake mode → refuses, since `chaintest` is test-only and must never be linked into a binary). `reality.RawArchive.VerifySweep` was added to give POINT_IN_TIME.md §2's "Verify re-hashes objects on a schedule" an actual scheduler; it reports every corrupted object in its window rather than stopping at the first, and treats a missing object as a violation, never a shorter clean report.

The Object Lock (WORM) path is genuinely exercised, not merely coded: `TestIntegration_S3_AuditBucketObjectLockRefusesEarlyDeletion` runs against real MinIO and asserts `ObjectLockEnabled`, a COMPLIANCE `PutObject`, `HEAD` returning the retention, `DELETE` of the locked version refused as `archive.ErrRetentionLocked`/`CodeForbidden`, and the version still readable. Observed passing, not skipped.

**Redpanda bus — RESOLVED 2026-09-06, and it caught a real hang.** The blocker above (`franz-go/pkg/kmsg` absent from `go.mod`/`go.sum`) was cleared by the integrator under D-031, and `internal/reality/redpandabus` now has the franz-go producer and consumer its `doc.go` always described. Verified against the live local broker, whole package twice with nothing cleaned between: `ok internal/reality/redpandabus 15.812s / 15.823s`.

- **Partition rule read from the registry, not restated.** `ValidatePartitionKey` looks the topic up in `internal/event`'s registry and refuses a key that is not the `aggregate_id` the headers name, or an `aggregate_type` that is not the one `Spec` declares. Kafka orders within a partition and the key picks the partition, so a wrong key is the dangerous case: it publishes perfectly, and only shows up later as one aggregate's events arriving out of order. `Loopback` enforces the identical rule, so a producer that keys wrongly fails in the test that would otherwise have blessed it. An unregistered topic (reality's normalized-event stream, keyed by `dedup_id`) still needs a non-empty key, because a null key round-robins.
- **Producer**: idempotent, `acks=all`, synchronous; `Publish` returns only after every in-sync replica has the record, which is what the relay assumes before marking an outbox row published.
- **Consumer**: one client per (topic, group), auto-commit off, `BlockRebalanceOnPoll` so a rebalance can only happen between polls, one goroutine per partition with records handled in strict offset order, and a commit of only the contiguous handled prefix per partition — so an offset never moves past a record that was not handled.
- **`Open` is the only choice point** and never substitutes one bus for the other: fake mode gets `Loopback`, everything else gets the real client, and `New` will not return a client whose brokers did not answer a ping. `cmd/market-ingest-worker` refuses to start on an unreachable broker rather than downgrading (verified: `event bus: PROVIDER_UNAVAILABLE: redpandabus: brokers are unreachable`).

**The defect the tests found — worth knowing before anyone else writes a franz-go producer.** The broker-loss test pauses the Redpanda container and publishes into it. The first run did not fail: `ProduceSync` blocked for **634 seconds**, until the container was unpaused, and then returned `nil`. Neither `RecordDeliveryTimeout` nor `ProduceRequestTimeout` bounds that case — the first is only evaluated for a batch that is *not* in flight, and the second is a field the broker honors, which a paused broker does not. A stalled connection therefore leaves the batch in flight forever, and `ProduceSync`'s context only aborts buffering, not an in-flight batch. Fixed by bounding `Publish` itself: `Produce` with a promise and a `select` on the deadline. A timeout is reported as a **failure** even though the record may still land later; that asymmetry is deliberate, because an unknown outcome reported as failure costs a duplicate that at-least-once delivery and `event_id` dedup already absorb, while an unknown outcome reported as success costs the event.

Integration coverage against live Redpanda (7 tests) and unit coverage (6): round trip with headers and key preserved; per-partition ordering with 8 keys interleaved over 3 partitions, asserting each key lands on exactly one partition **and** that the keys demonstrably spread over more than one, so the ordering assertions are not vacuous; duplicates delivered serially and concurrently plus a handler that fails once, all yielding exactly one effect; a produce failure never reported as success (oversized record, cancelled context, empty key, publish after close); a consumer killed mid-record, where the next member of the group resumes at the uncommitted offset handling exactly the remainder, skipping nothing and repeating nothing; a second member joining mid-stream, fed continuously so the rebalance is actually exercised rather than passing because the incumbent had already drained the topic; and the broker-loss case above. `Loopback` and all of its tests are unchanged and still pass.

**Do not weaken validation to make those three tests pass.** They are the look-ahead leakage guarantee. A backtest built on a leaking reality store produces confident, profitable and entirely fictional results, which is the most expensive possible way for this system to be wrong.

**`internal/proof` — RESOLVED and VERIFIED 2026-09-06.** Independently re-verified by the integrator: three consecutive runs on one fresh database (`controlplane_test_verify_proof2`), nothing cleaned between them, all green (`proof 4.352s / 6.502s / 7.991s`, `audit-worker 1.139s / 1.210s / 1.173s`). Repeatability was **not** bought by weakening detection, which was the specific risk: all 11 tamper subtests still reject on their own distinct reason, and `c3: signature forged with another key` is still reported as `checkpoint_signature`, not downgraded to an unknown-key result. `TestIntegration_KeyRotation` adds five subtests, including "both keys trusted: a rotation is not an incident", "the retired key is dropped from the set: unknown, not tampered", and "a trusted key still catches a forged signature". The fix introduces a key set mapping key id to status — `active`, `retired` (rotated out, still verifies what it signed) and `revoked` (refused before any cryptography, overriding active/retired) — with the verifier resolving by `audit_checkpoints.signing_key_id`. Three failure reports are kept strictly separate: `checkpoint_key_unknown` means a trust gap where the signature was never judged, `checkpoint_key_revoked` is its own reason, and `checkpoint_signature` is the only one that means tampering. Recorded as D-029. The original defect, for the record:

**The defect as found — the single most important finding of this wave.** The agent's evidence was real but incomplete: it provisioned a fresh database, ran once, and dropped it. Running the same command twice against one database fails the second time:

```
signature does not verify over the canonical digest: proof: signature was made with a different key:
row names "local-test:5e26e6997f497ee9f8d345227d5e0fb5", verifier holds "local-test:eb337df7ca79bf80f5fd30c9210e6eaf"
```

Each test process mints a fresh ephemeral signing key, while `audit_checkpoints` rows persist because they are append-only audit records. Verification walks all history and reaches checkpoints signed by an earlier process. **This is not merely test hygiene.** The checkpoint row already names its own key id and the verifier ignores it, applying whatever single key it currently holds. That is the key-rotation failure mode: the moment a KMS key rotates, every checkpoint signed by the previous key becomes unverifiable and `make verify-audit` reports tampering where there is none. An audit system that cries tamper after a routine rotation is worse than none, because the one time it is right nobody will believe it. Required fix: resolve the verifying key by the id recorded on the row against a set of trusted keys; make unknown-key-id a distinct rejection reason from signature-mismatch; keep every existing tamper test rejecting on its specific reason; prove repeatability by running the suite twice on one database.

**`internal/strategy/ir` — VERIFIED (IR only).** `go test -count=2 -race ./internal/strategy/...` → `ok internal/strategy/ir 25.665s`, repeatable. The agent fixed all three outstanding lint findings with real fixes rather than suppressions, and its tests caught three genuine production bugs: `SemanticHashOfJSON` accepted JSON `null` and returned a real-looking digest that could have been stored as `strategy_versions.ir_hash`; `ParseDecimalString` accepted `.5` while rejecting `1.`; and the model-facing schema was invalid at the provider boundary, since the structured-output subset requires `additionalProperties: false` literally. Hash stability is proven across 8 child processes, not just goroutines, because Go randomizes map order per process. **Still open in that scope: the NL compiler, `internal/model`, and the TypeScript SDK.** The SDK belongs beside `packages/generated-client` as `packages/strategy-sdk`, not inside it, because it mirrors the IR rather than the HTTP API.

**91-package `-race` baseline, 2026-09-06 (excluding `internal/reality`, which does not build): 62 packages `ok`, 1 `FAIL`.** The sole real failure was `TestContract_Timeout` in `test/contract/solanarpc` — a 40ms client deadline that also governed a call the test expects to succeed. Diagnosed as a genuine test defect, not load noise, fixed under D-028, and re-verified `-count=5 -race` under deliberate background load: `ok test/contract/solanarpc 11.329s`. The same defect class was then swept for and found latent in `test/contract/stripe` and `internal/chain`; both hardened and re-verified `-count=3 -race` (`ok stripe 7.259s`, `ok chain 6.829s`, `ok chaintest 1.514s`). `internal/provider/solanarpc` and `internal/db` were inspected and are safe as written.

Repo-wide as of the 2026-09-06 post-resume check: `go build ./...` fails only on `internal/reality`; `go vet` over every other package is clean, which means every other package's test files compile. `internal/settlement` now builds (its `executor_steps.go` landed). The two Jupiter timeout tests that were load-sensitive are now **VERIFIED under contention**: three rounds of `-count=5 -race` run concurrently with a whole-`internal/` race load (load confirmed still running each round) gave 15 contended passes per timeout test and 0 flakes, across `TestClient_Order_Timeout`, `TestClient_Execute_TimeoutIsUnknownAndNeverRetried`, `TestContract_Order_Timeout` and `TestContract_Execute_Timeout_SubmissionUnknown_NoSecondCall`.

**`internal/settlement` + `internal/execution` — VERIFIED by the integrator on a freshly provisioned database** (`controlplane_test_verify_settle`, not the agent's own): `go test -count=1 -race -tags=integration ./internal/settlement/... ./internal/execution/...` → exit 0, `ok settlement 107.598s`, `ok execution 77.124s`. The agent's own runs (`settlement 244.579s`/`246.454s`, `execution 2.229s`/`111.604s`, golangci-lint 0 issues) are corroborated, not merely accepted. Includes `TestExecutor_ResumeAfterCrash_EveryStepBoundary`: 17 steps × 3 phases = 51 fault-injected runs, each asserting exactly one landed transaction, one submission, one fill, one ledger posting, one position update, reservation settled and order SETTLED — the PART 49 property, at every step boundary.

Open lint findings (1), inside an in-progress package and assigned to its owning agent: G115 integer-overflow conversion in `internal/strategy/ir/decimal.go`. The two `internal/archive` "artefacts" spellings are gone, and `golangci-lint run --build-tags=integration ./internal/reality/... ./internal/archive/... ./cmd/market-ingest-worker/...` reports `0 issues`.

## 6. Architectural changes made

None yet beyond initial decisions (DECISION_REGISTER).

## 7. Migrations applied

None yet.

## 8. External blockers

See `BLOCKERS.md`. Summary: no provider credentials (Stripe onramp, Privy, Helius, Jupiter API key, AWS), no legal/licensing decisions. All live capability gates default DISABLED.

## 9. Unresolved defects

None yet.

## 10. Production-capability state

| Capability | State | Notes |
|---|---|---|
| LIVE_FUNDING | DISABLED | no provider approval, no gate DB yet |
| LIVE_MANUAL_TRADING | DISABLED | |
| LIVE_AGENT_TRADING | DISABLED | |
| WITHDRAWALS | DISABLED | |
| SOCIAL_DATA_PERSISTENCE | DISABLED | data-licensing unknown |
| MARKETPLACE | DISABLED | out of V1 |
| CROSS_CHAIN | DISABLED | out of V1 |
| PREDICTION_MARKETS | DISABLED | out of V1 |
| SECURITIES | DISABLED | out of V1 |
| CEX_TRADING | DISABLED | out of V1 |

Platform status: **NOT_READY**. Capital authority: **DISABLED**.

## 11. Provider integration status

| Provider | Role | State | API notes (`docs/api/providers/`, verified 2026-09-05) |
|---|---|---|---|
| Stripe crypto onramp | FundingProvider | **CODE_COMPLETE + CONTRACT_TESTED → BLOCKED_EXTERNAL (EB-003)** | API verified; product is public preview and application-gated (sandbox too); `stripe-go` has no onramp package → raw HTTP with decimal-string amounts; webhook `crypto.onramp_session.updated`; Stripe is merchant of record for fraud/disputes |
| Privy (delegated embedded wallets) | WalletProvider / SigningProvider | **CODE_COMPLETE + CONTRACT_TESTED → BLOCKED_EXTERNAL (EB-005)**; SignTransaction classified UNKNOWN_EFFECT_WRITE (idempotent replay is a documentation claim, not verified) | PARTIAL docs; policy engine cannot resolve address-lookup-table accounts → use `programId` allow-lists; sign via official Go SDK v0.15.0; rate limits unpublished |
| Jupiter | ExecutionAdapter (Solana) | **CODE_COMPLETE + CONTRACT_TESTED → BLOCKED_EXTERNAL (EB-011)**; adapter wrapping into `execution.ExecutionAdapter` pending | `/swap/v1` deprecated; current is Swap V2 (`/order` + `/execute`, `/build`) with `x-api-key`, mainnet only; amounts are strings |
| Helius | SolanaDataProvider / ChainObserver | **CODE_COMPLETE + CONTRACT_TESTED → BLOCKED_EXTERNAL (EB-010)** | PARTIAL docs; Enhanced Transactions in maintenance mode, Parsed Events beta returns numbers (must parse exactly); webhooks retry 3× then drop → never truth, periodic reconciliation mandatory |
| Fallback Solana RPC | ChainObserver (secondary) | **CODE_COMPLETE + CONTRACT_TESTED** (public RPC endpoints need no contract; production uses a paid fallback provider, config-driven) | verified; `maxSupportedTransactionVersion: 0` required for v0 txs; blockhash validity ~151 blocks; Token-2022 extension program ids enumerated |
| Anthropic Claude | ModelProvider | NOT_STARTED | verified; use `claude-opus-5` with `output_config.format` JSON schema for the NL compiler (Fable 5.1 rejects forced `tool_choice`); no idempotency header; Go SDK v1.71.0 |
| Redpanda | EventBus | NOT_STARTED |
| Temporal | WorkflowEngine | NOT_STARTED |
| S3 (+Object Lock) | ObjectArchive | NOT_STARTED |
| ClickHouse | analytics store | NOT_STARTED |

## 12. Test matrix (locally executable today)

| Tier | Command | Last recorded result |
|---|---|---|
| build | `go build ./...` | green 2026-09-06 (between waves; in-progress packages may transiently break it) |
| unit + race | `go test -count=1 -race ./...` | 2026-09-06 (later run, 61 packages): 58 green; `internal/settlement` failing while its agent finishes the executor; two Jupiter timeout tests load-sensitive (pass in isolation ×3; hardening requested) |
| property | `make property` (rapid `TestProp_*` in money, ledger, capital, positions, buyingpower, event, risk, eligibility, killswitch, provider, fees, ratelimit, audit, intent, quote) | green with unit |
| fuzz | `make fuzz` (`FuzzParseUSD`, `FuzzParseQuantity`, `FuzzQuantityFromDecimalString`, `FuzzParseUSDRound`, `FuzzScanQuantity`, `FuzzParse` (id), `FuzzParseSecretRef`, `FuzzEnvelopeJSON`, `FuzzCanonicalJSON`, `FuzzValidate` (intent), `FuzzRouteHash`) | 10–20 s per target clean (per-package reports) |
| integration | isolated DB via `scripts/testdb`, `-tags=integration` over `./internal/... ./test/...` | 32/32 packages green on fresh DB (lint sweep); each wave-2 package re-verified individually on fresh DBs |
| migration | `test/integration/migrations` (clean apply, checksums, tamper, guarded rollback, role privileges, transition binding) | green |
| concurrency torture (PART 23) | `internal/capital` `CP_TORTURE_ITERATIONS=25` | 20/80 every iteration, both isolation modes |
| restore drill (PART 141/219) | `make restore-drill` | OK, `dist/restore-drill.json` |
| lint | `make lint` (+ `golangci-lint --build-tags=integration ./...`) | 0 issues at last sweep |
| contract | `make contract` (`test/contract/{stripe,jupiter}`; helius/solanarpc/privy pending) | green 2026-09-06 |
| security | `make security` (`test/security`: authority-boundary import rules, closed agent permission set, PROD refuses fakes/seed/debug auth) | green 2026-09-06; API-level IDOR/CSRF/SSRF/webhook-forgery cases join once `cmd/api` exists |
| load | `make load` (`test/load/*.js`, k6) | scripts valid (`k6 inspect`); **unmeasured** — no API binary yet |
| e2e / chaos tiers | `make e2e`, `make chaos` | pending: directories not yet created (Stages 7, 14, 18) |
| CI | `.github/workflows/ci.yml` | authored; never executed (no remote, SB-004) |

## 13. Session log

- **2026-09-05 S1**: Stage 0 audit; toolchain install; build-state docs; repo init.
- **2026-09-05/06 S1 (continued)**: Stage 1 foundation (money/id/clock/errs/config/observability/db/migrate/idempotency/security/auth/event), Stage 2 financial core (ledger/capital/positions/valuation/buying power), Stage 3 authority (gates/killswitch/eligibility/risk/admin/audit), migrations 00001–00605 + 00640/00641, privilege model D-016, transition binding 00603, OpenAPI + generated server/client, restore drill, identity login flow, notifications, compliance profiles, rate limiting, SSE stream, dev seed, ADRs, security/threat-model docs, provider API notes, traceability refresh. Two session rate-limit interruptions recovered by resuming agents. Wave 2 (Stages 4–8) in flight.
- **2026-09-06 S1 (wave 3)**: Fourth rate-limit cutoff. Resumed all six agents by message after inventorying disk (settlement, Jupiter and strategy had each landed more than their last message reported). Launched the Stage 7 reconciliation agent. Corrected §5, which still claimed "no tests exist yet" — it now records the single real build break (`internal/reality`), the outstanding Jupiter re-verification, and the three open lint findings. Standing lesson reconfirmed: inventory disk before telling a resumed agent what to do, because a cut-off agent's last narrated step understates what it actually wrote.
