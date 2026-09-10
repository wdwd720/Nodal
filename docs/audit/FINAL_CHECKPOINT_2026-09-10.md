# FINAL CHECKPOINT — 2026-09-10

The seventeen-item final output the mega-goal §43 requires, written against
observed evidence rather than against the code's intent. Every claim below
carries its §2 evidence class. Where a class is `UNKNOWN` or `BLOCKED_EXTERNAL`
it says so instead of rounding up to PASS.

**The headline first, because it is the answer to the question the goal asks:**
`SOFTWARE_COMPLETE` is **false**, and this checkpoint does not make it true.
Four named software items remain, each with a reason that is a decision or a
deployment change rather than unwritten code. The other four launch flags are
false for reasons no amount of engineering can clear.

---

## 1 · Final HEAD

**`d483b07a17e1d8867dd9bc9f8e1d8ac0ce782b01`** — the commit carrying this
document. Written in after the commit, because a hash cannot be in the object it
hashes; `git log -1` is the check.

The three commits that end the session:

| Commit | What |
|---|---|
| `9e9278c` | `archive: five spellings of one key is five objects` — F-126 |
| `8c5d8a9` | `docs: two blockers were resolved before this file said so` — B-13, B-14 |
| `d483b07` | `docs: the seventeen-item final checkpoint` — this file |

Sixteen commits since `05ec7f3`, the session's starting point.

**Evidence:** `LIVE_OBSERVED` (`git rev-parse HEAD`, `git log`).

---

## 2 · Findings — opened, closed, remaining

`docs/audit/AUDIT_FINDINGS.md` is the register: **126 findings**, of which
**116 are fixed, 5 are open and 5 are partial.**

Opened this session: **F-100 through F-126 — 27 findings, 16 P1, 7 P2, 4 P3.**
Twenty-four are fixed, two are partial (F-105, F-118) and one is open (F-125).
Twenty-five came from eleven parallel read-only audits whose claims were
re-verified before anything was changed; **two the fuzz tier found on its own**
(F-123, F-126).

### What remains, and the exact reason each remains

| Finding | Sev | State | Exact reason it remains |
|---|---|---|---|
| **F-42** | P2 | open | The AU001 audit binding trusts a transaction-local setting any caller can set. F-109 closed the four tables whose columns are money by revoking column-level UPDATE from `cp_app`; the general case needs the same privilege work on every state column, which is a schema-wide change, not a fix. |
| **F-47** | P2 | open | Two deliberate statements about who may read encrypted PII contradict each other. **This is a policy decision, not a defect** — the architecture does not derive which statement wins, so §42's "the decision can be derived from the architecture" does not apply. |
| **F-65** | P2 | part | Two kill-switch kinds reach nothing. The reachable half is fixed; the remainder needs the kill-switch to own surfaces it does not currently own. |
| **F-69** | P2 | open | An inventory row, not a defect: it names what six audits found and has been drawn down as each was closed. |
| **F-84** | P3 | part | Request validation precedes authentication. **Confirmed LIVE today** — `POST /v1/payouts` with no session answers `400 "Header parameter Idempotency-Key is required"`, not 401. Authentication still holds: the same request *with* an idempotency key answers 401. Fixing it means authorising on the route pattern before the generated wrapper, which wants its own design. |
| **F-93** | P1 | open | An inventory row across six provider audits. Its constituent items are individually tracked; several are deployment changes (`CP_DATABASE_MIGRATE_URL` service-conditional, a Neon idle timeout, an advisory lock on replica count) and one is an external fact (a mainnet settlement mint). |
| **F-95** | P3 | part | 121 enum CHECKs have no Go counterpart. Most have no Go list to compare against, by their nature. Nine more were paired this session; three were deliberately left unpaired. |
| **F-105** | P1 | part | The unauthenticated write amplification is fixed. The larger half — `security_events` is bounded per minute and still unprunable — needs partitioning per ADR-0020. **On a 500 MB ceiling, any steady rate eventually fills a database nothing can remove a row from.** |
| **F-118** | P2 | part | Alerts now log at ERROR/WARN and both roots construct the real OTel instruments. What remains is a destination and something on a timer — half deployment decision. |
| **F-125** | P3 | open | The race detector cannot link on this host: this GCC's path contains a space and binutils splits the linker-script argument on it. It is a host change, not a repository one. **Every race claim in this repository rests on CI.** |

**Evidence:** `STATIC_PROOF` for the register; `LIVE_OBSERVED` for F-84's live
behaviour and F-125's link failure.

---

## 3 · Major defects found and fixed — the P1s

No P0. Sixteen P1s, every one **observed failing before it was believed.**
The ones that would have cost real money or real authority:

- **F-100** — a refund parked a funding for review, the next webhook un-parked
  it, and Credits were minted for money that had been returned. There was no
  operator path out of review at all.
- **F-101** — one transition row licensed a second, unrelated edge, because the
  previous audit's own migration put its `|` and `>` delimiters in band.
- **F-102** — one ADMIN session could cancel any customer's intent and move any
  seller's product, past a guard that matched one helper name of four.
- **F-103** — five configuration rules permitted what the deployment cannot
  survive, including live payment credentials in DEV.
- **F-105** — an unauthenticated caller chose how many permanent, undeletable
  rows the service wrote, on a deployment whose database ceiling halts every
  financial action.
- **F-106** — three money tables handed one account's record to another on a
  reused idempotency key.
- **F-107** — the seller set the platform's own commission, and a payout the
  provider may already have paid could be cancelled by its owner.
- **F-108** — two failed RPCs were read as proof a transaction never happened.
- **F-109** — the application role could rewrite an amount, a destination, or
  the definition of what counts as money.
- **F-112** — a shipped configuration removed the cookie prefix a takeover fix
  depends on.
- **F-114** — a revoked agent could return to live capital with no approval.
- **F-115** — the branch whose comment reads "do not resubmit" was the one that
  resubmitted.
- **F-121** — dual control itself could be forged at INSERT, which is the
  missing half of a fully-evidenced agent promotion to LIVE.
- **F-124** — the webhook path the published contract documents is not the one
  the service registers, so a delivery to it answers 404 and Stripe eventually
  gives up.

### Three things this session got wrong and corrected

Recorded because a checkpoint that lists only successes is the kind of document
this audit exists to distrust.

1. **An integration run was reported as passing when it had failed.** A
   `| tail -20` pipe masked the exit status; `internal/httpapi` and
   `internal/signing` had both failed. Corrected in writing to the user, and
   every gate since has been run to a log file with the exit status appended.
2. **One reported finding was not true.** The capacity negative-ceiling loop IS
   reachable; only its test passed for the wrong reason. F-104 records the
   narrower fact rather than the reported one.
3. **The birth controls took three attempts.** Two CHECK constraints asserted an
   ordering the money path does not have and were dropped with the reasoning
   written into `00738`; the replacement was keyed on the code's happy path
   rather than on the property being protected, and `00739` restates it against
   the lines the schema already draws.

### The transferable finding

**Six fixtures encoded the defect they were meant to guard against**, and every
one was found by closing a control rather than by reading the fixture:
fourteen providers on live credentials in `prodEnv()`; `CP_AUTH_COOKIE_DOMAIN`
set to a registrable domain in the same fixture; a seller-set platform fee in
the HTTP commerce journey; outcomes resolved before their horizons closed; an
as-of price read answering with unreceived rows; and an `admin_actions` row born
APPROVED. **A suite's known-good fixture is an assertion about what is safe, and
nothing was checking it.**

**Evidence:** `REAL_DB_INTEGRATION` for every migration-level fix (each
reproduced against a real PostgreSQL 16 before and after);
`LOCAL_EXTERNAL_STACK` for the provider-facing ones; `LIVE_OBSERVED` for F-124's
404s.

---

## 4 · Current schema

| | |
|---|---|
| Migration head | **`00739_a_birth_control_keyed_on_the_money_line.sql`** |
| Migration files | **71** |
| Tables | **119** |
| CHECK constraints | **462** |

Applied migrations are never edited — a correction is a new file, and `00738`
exists solely to drop two constraints `00736`/`00737` got wrong, with the
reasoning kept in the file rather than in a commit message.

**Evidence:** `REAL_DB_INTEGRATION` — counted from a live PostgreSQL 16 at
version 739, not from the files.

---

## 5 · Test evidence

Every tier below was run at the archive fix (`9e9278c`). The only commit after
it changes two markdown files, and `./test/docs` and `./test/infra` pass at that
commit too.

| Command | Result |
|---|---|
| `go build ./...` | pass |
| `go test ./...` | **140 packages, 0 failures** |
| `go run ./scripts/inttest` | **51 packages, one fresh database each, all passed, 15m31s** |
| `go run ./scripts/fuzzall -fuzztime=10s` | **29 targets, 0 failed** |
| `go test ./internal/archive/ -fuzz FuzzParseKey -fuzztime=45s` | pass, 68,139 execs, no new failures |
| `go run ./scripts/restoredrill` | **OK, 11.7s** |
| `go run ./scripts/fmtcheck .` | ok |
| `go run ./scripts/tool golangci-lint run` | 0 issues |
| `go run ./scripts/lintfin` | 0 findings |
| `go run ./scripts/configcheck -service api .env.example` | 244 variables, valid |
| `terraform validate` × dev, staging, prod | Success, all three |
| `terraform fmt -check -recursive` | clean |

**The one tier that cannot run here:** `go test -race`. It fails to LINK for
every package, because this GCC's install path contains a space. Recorded as
F-125. Both race tiers run in CI on Linux, so the coverage exists — but
**no race claim in this repository can be checked from this host.**

**Evidence:** `LIVE_OBSERVED` for every row above, each run to a log file with
its exit status appended. `REAL_DB_INTEGRATION` for the 51 integration packages
and the restore drill. `BLOCKED_EXTERNAL` — on the host, not a provider — for
the race tier.

---

## 6 · Live staging evidence

Observed against `https://api-nodal.actorvia.xyz` on 2026-09-10.

| Probe | Result |
|---|---|
| `GET /v1/healthz` | 200 |
| `GET /v1/readyz` | 200 |
| `GET /v1/version` | 200 · `{"build_version":"dev","config_hash":"e1ad81b6…","environment":"STAGING"}` |
| `GET /v1/auth/login` | **302 to the ZITADEL authorize endpoint**, carrying an S256 PKCE challenge, a nonce, and the deployment's own redirect URI |
| `POST /v1/payouts` with an idempotency key, no session | **401 UNAUTHENTICATED** |
| `POST /v1/payouts` with no idempotency key, no session | 400 — F-84, validation before authentication |
| `POST /v1/withdrawals`, `GET /v1/credits/pricing` | 401 without a session |
| `POST /v1/webhooks/stripe` (the path the contract used to document) | 404 — this is F-124, now corrected in the contract |

Security headers present on every response:

```
strict-transport-security: max-age=31536000; includeSubDomains
content-security-policy: frame-ancestors 'none'
x-frame-options: DENY
x-content-type-options: nosniff
referrer-policy: strict-origin-when-cross-origin
permissions-policy: camera=(), microphone=(), geolocation=(), payment=(), usb=(), interest-cohort=()
ratelimit-limit: 600
```

**`build_version` is `dev`.** The deployed binary does not carry a commit
identity, so **the deployment cannot be pinned to a commit from the outside.**
It is recorded here rather than smoothed over: nothing above proves the
deployment is at `9e9278c`, and it almost certainly is not.

**Evidence:** `LIVE_OBSERVED` throughout. Nothing here is "production
approved" — this is STAGING, on a free tier, and the §2 rule that a deployed
endpoint is not a production approval is why that sentence exists.

---

## 7 · Provider evidence, classified

**Fourteen providers, every one in `sandbox` mode. Zero in `live`. Zero fake.**

| Class | Which |
|---|---|
| `LIVE_OBSERVED` | ZITADEL OIDC — the deployment performs a real authorization request against the live issuer, and the issuer's discovery document answers 200. |
| `LOCAL_EXTERNAL_STACK` | Stripe sandbox (credit purchase), S3 via MinIO including the archive layout, Redpanda for the event bus. Real protocols, real wire formats, not production accounts. |
| `REAL_DB_INTEGRATION` | Every provider port's persistence half — `provider_events`, the inbox, the evidence archive keys — exercised against real PostgreSQL 16 in the 51-package integration run. |
| `TEST_DOUBLE_ONLY` | Payout, wallet, signing, execution, market data, chain observer (and its fallback), model, workflow, notification. Each has a faithful sandbox including the inconvenient behaviours, and **a faithful sandbox is still a sandbox.** |
| `BLOCKED_EXTERNAL` | Stripe crypto onramp (EB-003), Privy delegated wallets (EB-005), Helius (EB-010), Jupiter (EB-011), S3 Object Lock itself (EB-012). |

**No provider path in this system has been observed end-to-end against a
production provider account.** That sentence is the honest summary of this
section, and §2 forbids the shorter one.

---

## 8 · Financial invariants

| Invariant | Where it is proved | Class |
|---|---|---|
| Every posting balances | `TestProp_PostingBalanced`, `TestProp_SwapPostingBalanced` | `REAL_DB_INTEGRATION` |
| Capital is conserved | `TestProp_CapitalConserved` | `REAL_DB_INTEGRATION` |
| Reservations never oversubscribe | `TestProp_ReservationsNeverOversubscribe` | `REAL_DB_INTEGRATION` |
| A funding reversal cannot leave a deficit | `TestProp_FundingReversalDeficit` | `REAL_DB_INTEGRATION` |
| Content hashes do not depend on ordering | `TestProp_ContentHashOrderInvariant` | `REAL_DB_INTEGRATION` |
| No floating point anywhere in the financial packages | `scripts/lintfin` rule 1, over `money`, `ledger`, `capital`, `risk`, `positions`, `valuation`, `quote`, `settlement` | `STATIC_PROOF` |
| Production never imports a test double | `scripts/lintfin` rule 2 | `STATIC_PROOF` |
| Agent-facing code cannot reach signing, wallets, admin, capital or risk policy | `scripts/lintfin` rule 3 | `STATIC_PROOF` |
| A restored database has zero balance drift and identical journal hashes | `scripts/restoredrill` | `REAL_DB_INTEGRATION` |

**Closed this session, at the schema level rather than in Go**, so that a future
code path cannot reopen them: money columns are out of the application role's
reach (F-109); nothing is born finished (F-122, three migrations); an approval
is not born approved (F-121); a revoked agent stays revoked (F-114); a state
name cannot forge a transition edge (F-101).

---

## 9 · Security

| Control | State | Class |
|---|---|---|
| Authentication on every money endpoint | Holds. 401 without a session, observed live. | `LIVE_OBSERVED` |
| Account scoping across the read grades | Fixed (F-102). The read-grade list is now **derived by scanning for `security.RequireAccount(`**, so it cannot go stale. | `REAL_DB_INTEGRATION` |
| Dual control on capability activation | Cannot be forged at INSERT (F-121). Approver must be distinct from proposer; activator distinct from approver; step-up within 15 minutes. | `REAL_DB_INTEGRATION` |
| Database privilege separation | `cp_migrate` / `cp_app` / `cp_readonly` / `cp_ops`. `cp_app` holds no UPDATE on money columns and no DELETE on `login_attempts`. | `REAL_DB_INTEGRATION` |
| Secret redaction in logs | `SecretRef` implements `slog.LogValuer` (F-104). | `STATIC_PROOF` |
| Cookie host-only, `__Host-` prefix preserved | Config rule added (F-112). | `STATIC_PROOF` |
| Request body limits | Adversarial suite added (F-85). | `REAL_DB_INTEGRATION` |
| Idempotency keys scoped by owner | Fixed across three money tables (F-106). | `REAL_DB_INTEGRATION` |
| Security headers, HSTS, rate limiting | Present, observed live. | `LIVE_OBSERVED` |
| Independent penetration test | **Not done. This audit is not one and does not claim to be.** | `BLOCKED_EXTERNAL` |
| Race detector | Cannot link on this host. Rests entirely on CI. | See F-125 |
| `security_events` retention | Unprunable (F-105 residual). | Open |

Eighteen files under `test/security/` cover authority boundaries, dual control,
idempotency abuse and break scanning, and run as part of the 51-package
integration tier.

---

## 10 · Restore drill

Run at `9e9278c`:

```
restoredrill: backup: 682455 bytes sha256=7b9ab2f634dbe578
restoredrill: boot: version source=739 restored=739 verify=ok
restoredrill: reconciliation dry-run: tables=119 rowcounts_match=true
              balance_drift_accounts=0 journal_hash_match=true
restoredrill: OK (11.712s)
```

A restored database reaches the same schema version, the same table count, the
same row counts, **zero balance drift and identical journal hashes.** Report at
`dist/restore-drill.json`.

**Evidence:** `REAL_DB_INTEGRATION`. This is a drill against a local
PostgreSQL 16, not against the Neon backup — **the deployed database's own
restore has not been drilled**, and that is `UNKNOWN`, not PASS.

---

## 11 · The $0 launch tier

Render free web service, Neon free Postgres, Cloudflare free, ZITADEL free.
**No workers, no cron, no fixed monthly cost.** Nothing paid was activated and
no paid infrastructure was applied.

Every provider is `sandbox`. `CP_AUTH_MODE` is `oidc`. `CP_ENV` is `STAGING`.

### Fail-closed ceilings, as deployed

| Ceiling | Value | What it protects |
|---|---|---|
| `CP_CAPACITY_MAX_ACCOUNTS` | 50 | the cohort the tier was sized for |
| `CP_CAPACITY_MAX_PURCHASES_PER_DAY` | 200 | ~1 MB/day of ledger, funding and evidence rows |
| `CP_CAPACITY_MAX_AT_RISK_MINOR` | 200,000 | money promised or taken but not yet decided |
| `CP_CAPACITY_MAX_DATABASE_BYTES` | 524,288,000 | the Neon free quota |
| Database headroom | 0.67 | refuses at two-thirds, leaving a third free |

**These refuse rather than degrade.** `Guard.Admit` returns `ErrAtCapacity` and
the action does not happen. The money-at-risk ceiling sums the `credit_fundings`
states that are neither terminal nor SETTLED — including CAPTURED, which is
money already taken from the payer and one transition from minting Credit.

### Two periodic jobs that would otherwise never run

This deployment has one process. `runCreditSettlement` (F-90) and
`runLoginAttemptRetention` (F-105) both live inside `cmd/api` for that reason,
each sweeping once at startup because a process that wakes, serves a login and
spins down would otherwise never sweep at all. Retention **warns at WARN naming
the consequence** when unconfigured rather than refusing to boot — a web
service's job is serving requests — but it is never silent.

### Migration triggers

Any ceiling reaching its headroom is the signal to move. The database ceiling
is the binding one: `security_events` is unprunable (F-105), so **on this tier
the 500 MB quota is a countdown, not a steady state.**

---

## 12 · AWS scale-up readiness

**66 Terraform files** across twelve modules — network, RDS, Redis, S3 with
Object Lock, KMS, Secrets Manager, the ECS cluster and services, the ALB with
WAF, observability and the GitHub OIDC deploy role.

| Check | Result |
|---|---|
| `terraform validate` — dev | Success |
| `terraform validate` — staging | Success |
| `terraform validate` — prod | Success |
| `terraform fmt -check -recursive` | clean |
| Applied resources | **zero** |

The only state file on disk is a `.terraform/` backend-configuration cache with
zero resources, and it is gitignored. **Nothing was applied, and no fixed
monthly cost was incurred.**

The account exists (`049286562577`). `aws sts get-caller-identity` fails with
`Your session has expired` — B-12, which blocks the scale-up path and **not the
launch**, because the launch tier does not run on AWS.

Before the Terraform is ever applied, one thing in it should be read again: its
five application alarms set `treat_missing_data = "notBreaching"`, so **a metric
that never arrives reads OK.**

---

## 13 · Capability state, and `CREDIT_PURCHASE` in particular

`CP_API_ENABLED_CAPABILITIES` is `CREDIT_PURCHASE` on the deployment. **That
line does not activate anything.**

`gates.Evaluate` is a conjunction, and it fails closed at every step:

- `configEnabled` false → `CONFIG_DISABLED`.
- No gate row → **`NO_GATE_ROW`, not active.** The absence of a row is a refusal,
  not a default-allow.
- State not `ACTIVE` → refused.

The local database has **zero rows in `capability_gates`**, so every capability
there is inactive by absence. The default persisted row is `DISABLED`.

To reach `ACTIVE`, a high-risk capability needs **all four** evidence references
— `legal_review_ref`, `provider_contract_ref`, `risk_approval_ref`,
`security_approval_ref` — and **three distinct principals**: a proposer, an
approver who is not the proposer, and an activator who is not the approver, each
with a step-up within 15 minutes.

**The gate was not bypassed and no approval reference was fabricated.** F-93
records the honest limit of the control: three principals is three `users.id`
values, which is not the same as three people.

---

## 14 · `BLOCKED_EXTERNAL`, and who can clear each

Twelve remain. Two were cleared before this checkpoint and the document had not
caught up — recorded in this session's second commit.

| | Blocker | Who clears it |
|---|---|---|
| B-01 | No payout provider contract | A signed agreement. People. |
| B-02 | Whether Credits may be paid out at all | Counsel. |
| B-03 | Pre-KYC participation in native markets | Counsel. |
| B-04 | Credit purchase production credentials | Stripe, after B-09. |
| B-05 | Hosted partner rail: no provider selected | A commercial decision. |
| B-06 | Financial identity verification provider | A commercial decision. |
| B-07 | Age and jurisdiction matrix | Counsel. |
| B-08 | Independent security review and pentest | A third party. |
| B-09 | Stripe restricted-business review | Stripe. **Not submitted, and this session did not submit it.** |
| B-10 | Stripe stablecoin payout private preview | Stripe. |
| B-11 | Stripe Connect platform profile | Stripe. |
| B-12 | No authenticated non-root AWS role | The account owner. Blocks scale-up, not launch. |
| ~~B-13~~ | ~~No OIDC identity provider~~ | **RESOLVED** — ZITADEL, verified live. |
| ~~B-14~~ | ~~Hostname and TLS certificate~~ | **RESOLVED** — `api-nodal.actorvia.xyz`, certificate verifies. |

---

## 15 · The five launch flags

Stated explicitly, and none of them optimistically.

| Flag | Value | Why |
|---|---|---|
| `SOFTWARE_COMPLETE` | **false** | Four named items remain: `security_events` partitioning (F-105), an alert destination (F-118), the PII-read policy contradiction (F-47), and the AU001 privilege work (F-42). Three are decisions or deployment changes; one is schema work. |
| `STRIPE_PRODUCTION_APPROVED` | **false** | Stripe's own review of a real business. Not submitted. `BLOCKED_EXTERNAL`. |
| `LEGAL_APPROVED` | **false** | Counsel. `BLOCKED_EXTERNAL`. **No legal approval is claimed anywhere.** |
| `PENTEST_COMPLETE` | **false** | An independent third party. **This audit is not one and does not claim to be.** `BLOCKED_EXTERNAL`. |
| `LIVE_READY` | **false** | The conjunction of all four, plus the capability gate activated by three principals against four approval references. |

**Twenty-seven findings closed this session moved none of these**, and that is
the honest headline. Not one of the four independent reasons `LIVE_READY` is
false was a thing this audit could fix.

---

## 16 · Remaining human actions

1. **Decide the PII-read policy** (F-47). Two deliberate statements contradict
   each other and the architecture does not derive which wins.
2. **Choose an alert destination** (F-118). The instruments exist and the alerts
   log; nothing has anywhere to go.
3. **Engage counsel** for B-02, B-03 and B-07.
4. **Submit the Stripe restricted-business review** (B-09). Deliberately not
   done here.
5. **Commission an independent penetration test** (B-08).
6. **Sign a payout provider contract** (B-01), and select the partner rail
   (B-05) and identity verification provider (B-06).
7. **Authenticate a non-root AWS role** (B-12), when scale-up is wanted.
8. **Activate `CREDIT_PURCHASE`** — three distinct principals, four approval
   references, step-up within 15 minutes. Not fabricable, and fabricating it
   would defeat the control.
9. **Give the deployed binary a commit identity.** `build_version` is `dev`, so
   the deployment cannot be pinned to a commit from outside.
10. **Install a GCC whose path has no space**, if race claims are ever to be
    checkable off CI (F-125).

---

## 17 · Resume here

**Start with `docs/build/MASTER_BUILD_STATE.md`.** It is the continuity
document; this file is the checkpoint that points into it.

The next four pieces of software work, in the order they are worth doing:

1. **`security_events` partitioning per ADR-0020** (F-105). The largest
   remaining software item, and the one that makes the 500 MB ceiling a steady
   state instead of a countdown.
2. **An alert destination and something on a timer** (F-118). Everything up to
   the destination is built.
3. **Birth control for `wallets`, `assets` and `instruments`** (F-122 residual).
   These are named in an assertion that fails when one is closed, so the list
   cannot go stale.
4. **An agent can still be born SHADOW without its promotion evidence** (F-122
   residual). A provenance gap, not a money one — closing it is a decision about
   how the suite seeds agents, and `00739` records the reasoning in full.

Then F-42's privilege work and F-93's inventory rows.

**The thing worth carrying forward is not on any of these lists.** Six fixtures
this session encoded the exact defect they were meant to guard against, and
every one surfaced only when a control was closed around them. A green suite
proves the assertions ran. It does not prove the fixture they ran against was
ever safe.
