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

**Run `git log -1` for the value.** This document has been rewritten by later
commits more than once, and a hash written into the object it hashes is wrong
the moment anything lands after it. The session's own commits are listed below;
`git log --oneline 05ec7f3..HEAD` gives the rest. Every
commit after it edits this document only, and a hash cannot be inside the object
it hashes, so the value below is the one a reader should check the tree against.
`git log --oneline 05ec7f3..HEAD` gives the rest.

The commits that end the session:

| Commit | What |
|---|---|
| `9e9278c` | `archive: five spellings of one key is five objects` — F-126 |
| `8c5d8a9` | `docs: two blockers were resolved before this file said so` — B-13, B-14 |
| `d483b07` | `docs: the seventeen-item final checkpoint` — this file |
| `6853caa` | `security_events: a trail that can be bounded without being rewritten` — F-105, F-127 |
| `952b9fd` | `audit: the flag can only be set by inserting a transition row` — F-42, F-128 |
| `057680b` | `docs: the checkpoint after two more findings closed` |
| `2a27ee8` | `docs: the gate is green at 741, and one failure is recorded not dropped` |
| `07d490e` | `audit: the other half of 00603's claim now has a test` |
| `02f8bdf` | `restore: prove the restored database works, not only that it matches` — F-129 |
| `54ed595` | `credit: a funding state is not the application's to write` — F-42's remedy, table 1 |
| `652dd67` | `accounts: an account status is not the application's to write` — F-42's remedy, table 2 |
| `0947e63` | `docs: the register did to itself what it keeps finding` — F-130 |
| `1370194` | `F-42: four more tables, and the race detector runs on this host` — F-125 closed |
| `6ebfdf4` | `F-42: trade_intents and agents, and a mirror that is checked` — F-131 |
| `609eef4` | `F-42: reconciliation_records, and the two actors that are not the same one` |
| `6226281` | `F-42: deposits and kill_switches, and the remedy is complete` |
| `5b48a39` | `F-65: the deferral is now watched, because it rested on an unwatched fact` |
| `cb4e1b4` | `httpapi: a public-route assertion that a 404 satisfied` — F-132 |
| `798d274` | `F-47: the contradiction is resolvable even though the decision is not` |
| `07f8c94` | `config: the web service is not given the credential that can turn off every trigger` |
| `68536a6` | `api: the one-instance assumption is now an assertion` |
| `cc7d7f3` | `db: a dial that outlives its request, and a connection that outlives its server` |

**Thirty-nine commits since `05ec7f3`**, the session's starting point.

**Evidence:** `LIVE_OBSERVED` (`git rev-parse HEAD`, `git log`).

---

## 2 · Findings — opened, closed, remaining

`docs/audit/AUDIT_FINDINGS.md` is the register: **132 findings**, of which
**125 are fixed, 3 are open and 4 are partial.**

The seven that are not fixed, and what each is now:

| Finding | State | What it is |
|---|---|---|
| **F-47** | fixed | **Closed this session, the way its own updates said it would be.** `internal/pii` is the encryption (AES-256-GCM, ciphertext bound to row, column and key version, a versioned keyring in one SecretRef); the login path writes the verified e-mail sealed; and with the columns ciphertext under a key the database never holds, 00754 does what 00010 meant — neither role reads `identity_pii`, `cp_readonly` does not read `sessions`, `cp_ops` reads exactly `sessions.expires_at`. Checking what `cp_ops` did with the table found F-133. |
| **F-65** | part | Two kill switches reach nothing, and the bridge is deliberately unbuilt because the agent runtime is inert. **The premise is now watched**: a test fails when the runtime acquires a production caller, and says what becomes owed. |
| **F-69** | open | An inventory row across six audits, drawn down as each item closed. Not a defect. |
| **F-84** | part | Request validation precedes authentication, so two endpoints answer 400 where 401 would be truthful. Fixing it means authorising on the chi route pattern before the generated wrapper, which is a change to the boundary's structure. |
| **F-93** | open | An inventory row across six provider audits. **Five of its rows closed this session** — the schema-owner credential, the replica assumption, the login-attempt window, and both Neon pool knobs. What remains is decisions (Stripe-account availability, the identity model, whether an unverified account may pay in), one external fact (a mainnet settlement mint), and two small items. |
| **F-95** | part | 121 enum CHECKs have no Go counterpart. Most have no Go list to compare against, by their nature. |
| **F-118** | fixed | **Closed this session.** A webhook destination in the shape each destination accepts, refused-when-empty in STAGING/PROD; the verification pass on a ticker in `cmd/api`; a heartbeat alarm that breaches on missing data; a drift in the database reaching a webhook end to end under test. What is not code is the URL, and the next deploy does not go live until it is set. |

Opened this session: **F-100 through F-132 — 33 findings, 17 P1, 10 P2, 6 P3.**
Every one is fixed. Twenty-five came from eleven parallel read-only audits whose
claims were re-verified before anything was changed; **two the fuzz tier found on
its own** (F-123, F-126); **two were found by attacking this session's own fixes
before writing them** (F-127, F-128); **one was caused by a fix in this session
and found by the restore drill within the hour** (F-129); and **three were found
by re-reading this repository's own checks rather than its code** — the register
disagreeing with itself (F-130), a fixture writing rows the system cannot read
(F-131), and a public-route assertion a 404 satisfied (F-132).

**Nothing opened this session remains open.** F-125 closed when the race
detector was made to link on this host.

**No P1 in the register is unfixed.** The last one not marked fixed is F-93, an
inventory row across six provider audits whose constituent items are tracked
individually.

### What remains, and the exact reason each remains

| Finding | Sev | State | Exact reason it remains |
|---|---|---|---|
| **F-47** | P2 | fixed | Two deliberate statements about who may read encrypted PII contradicted each other. Once the encryption existed the architecture did derive which wins: a grant with no use on ciphertext is a grant waiting for a key leak. `internal/pii`, migration 00754, ADR-0021. |
| **F-65** | P2 | part | Two kill-switch kinds reach nothing. The reachable half is fixed; the remainder needs the kill-switch to own surfaces it does not currently own. |
| **F-69** | P2 | open | An inventory row, not a defect: it names what six audits found and has been drawn down as each was closed. |
| **F-84** | P3 | part | Request validation precedes authentication. **Confirmed LIVE today** — `POST /v1/payouts` with no session answers `400 "Header parameter Idempotency-Key is required"`, not 401. Authentication still holds: the same request *with* an idempotency key answers 401. Fixing it means authorising on the route pattern before the generated wrapper, which wants its own design. |
| **F-93** | P1 | open | An inventory row across six provider audits. Its constituent items are individually tracked; several are deployment changes (`CP_DATABASE_MIGRATE_URL` service-conditional, a Neon idle timeout, an advisory lock on replica count) and one is an external fact (a mainnet settlement mint). |
| **F-95** | P3 | part | 121 enum CHECKs have no Go counterpart. Most have no Go list to compare against, by their nature. Nine more were paired this session; three were deliberately left unpaired. |
| **F-118** | P2 | fixed | Alerts log, both roots construct the real instruments, and now: `internal/alert` delivers to a webhook (Slack, Discord, ntfy or generic, chosen from the host), `cmd/api` runs `VerifyInternal` + `SweepEscalations` every five minutes, `verification_passes` is a heartbeat with a breaching alarm, and STAGING/PROD refuse to start without a destination. |
| **F-125** | P3 | open | The race detector cannot link on this host: this GCC's path contains a space and binutils splits the linker-script argument on it. It is a host change, not a repository one. **Every race claim in this repository rests on CI.** |

Closed after this checkpoint was first written, and listed because their absence
from the table above is the change: **F-105** (`security_events` is now
partitioned by month and prunable by detachment, 00740) and **F-42** (the
transition flag is now a keyed tag the application cannot forge, 00741, after
four sessions open and three fixes tried and rejected).

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
- **F-128** — the application role could mint a transition flag by attaching the
  real setter to a temp table of its own, forging a state change on any of
  seventeen audited tables with no audit row.

### Three found by attacking this session's own fixes

Two were found before the fix they concern was written. The third was caused by
one of those fixes and found by the restore drill within the hour. None would
have been found by reading the code.

- **F-127** — ADR-0020 decides how an unprunable table must be pruned, and
  records that `security_events` does not refuse DELETE. It does. That was the
  one fact the choice of remedy rested on, and an implementer following the
  table would have written a DELETE that fails at runtime, or reached for the
  trigger, which the same ADR forbids permanently.
- **F-128** — rated P1 above F-42 not because the outcome differs but because of
  what it does to the fix. The obvious repair for F-42 is to make the flag's
  *value* unforgeable; this route forges the value using the real setter, so
  that repair would have looked complete, passed every test written for F-42,
  and been bypassed in three lines.

- **F-129** — closing F-42 made every state change on seventeen tables depend on
  one row. A restore that brought back every row but lost that one table passed
  the drill's row counts, ledger balances and journal hashes, and then refused
  every state change with an error blaming the caller for not writing a
  transition row it had written. Comparing data does not prove a database can be
  used.

**And one thing a test found that a migration had missed.** The transition key
table arrived readable by two lower-privilege roles with no `GRANT` written
anywhere, because `ALTER DEFAULT PRIVILEGES` grants SELECT on every table the
migration role creates. **Writing no GRANT is not the same as granting nothing.**

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
| Migration head | **`00753_a_kill_switch_is_released_by_its_transition.sql`** |
| Migration files | **85** |
| Bound tables the application may still UPDATE | **0 of 17** |
| Tables | **120**, plus **14 partitions** of `security_events` |
| CHECK constraints | 462 declared on parents |

`security_events` is the **first partitioned table in this schema** (00740). A
partition is a table, so a freshly migrated database now reports 134 relations
where it reported 119; the restore drill counts them and they come back.

Applied migrations are never edited — a correction is a new file, and `00738`
exists solely to drop two constraints `00736`/`00737` got wrong, with the
reasoning kept in the file rather than in a commit message.

**Evidence:** `REAL_DB_INTEGRATION` — counted from a live PostgreSQL 16 at
version 739, not from the files.

---

## 5 · Test evidence

Every tier below was re-run at the session's last code-bearing commit.

**The race tier now runs here.** F-125 recorded that `go test -race` could not
link on this host; it can, and the fix is a plain copy of the toolchain to a
path without a space. A junction and an 8.3 short path both fail for the same
reason — GCC resolves through them and reports the original path. The property
is not "a path without spaces" but "a path GCC resolves to without spaces".

| Command | Result |
|---|---|
| `go build ./...` | pass |
| `go test ./...` | **140 packages, 0 failures** |
| `go run ./scripts/inttest` | **51 packages, one fresh database each, all passed, 0 failures** |
| `go run ./scripts/fuzzall -fuzztime=10s` | **29 targets, 0 failed** |
| `go test ./internal/archive/ -fuzz FuzzParseKey -fuzztime=45s` | pass, 68,139 execs, no new failures |
| `go run ./scripts/restoredrill` | **OK, at version 744, including a live state change on the restored database** |
| `go run ./scripts/fmtcheck .` | ok |
| `go run ./scripts/tool golangci-lint run` | 0 issues |
| `go run ./scripts/lintfin` | 0 findings |
| `go run ./scripts/configcheck -service api .env.example` | 247 variables, valid |
| `terraform validate` × dev, staging, prod | Success, all three |
| `terraform fmt -check -recursive` | clean |

**One failure is recorded here rather than dropped, because it is the kind that
looks like a regression.** An earlier run of this tier reported
`internal/capital` failing `TestTorture_MixedOperationsConserveCapital` with
`lock timeout (SQLSTATE 55P03)`, at 211s against a historical 88s. It was my own
doing: the fuzz tier was running concurrently on the same machine, and that test
drives twenty goroutines against a lock timeout. In isolation it passes in 3.8s,
and in the clean run above the package took **87.7s, matching its 87.7s baseline
exactly**. Nothing in this session touches a table `internal/capital` writes.

The transferable part is not the diagnosis: it is that **a torture test with a
lock timeout reports on the machine as much as on the code**, so a gate run
concurrent with anything else is not a gate run.

| `make race` | **7 packages, 0 data races** |
| `make integration-race` | **12 packages, a database each, 7m27s, 0 data races** |

**No race claim in this repository rests on CI alone any more.** Both tiers were
run on this host and both are clean.

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
| The audit binding cannot be forged | Fixed (F-42, F-128). The transition flag is a keyed tag over a secret no role but the owner can read, salted with the top-level transaction id, and EXECUTE on every function that touches it is revoked from PUBLIC. Seventeen audited tables. | `REAL_DB_INTEGRATION` |
| `security_events` retention | Fixed (F-105). Partitioned by month; retention is partition detachment, never row deletion, and the immutability trigger is unchanged for every role including the owner. | `REAL_DB_INTEGRATION` |
| A state column the application cannot write at all | **Done. Zero of seventeen bound tables still grant the application blanket UPDATE** (00743-00753), counted from the schema rather than from a list. The refusal moved from `AUDIT_TRANSITION_REQUIRED` at COMMIT to `permission denied` at the statement, everywhere. | `REAL_DB_INTEGRATION` |
| The race detector | Runs on this host, both tiers, 0 data races (F-125). | `LIVE_OBSERVED` |

Eighteen files under `test/security/` cover authority boundaries, dual control,
idempotency abuse and break scanning, and run as part of the 51-package
integration tier.

---

## 10 · Restore drill

Run at `652dd67`:

```
restoredrill: boot: version source=744 restored=744 verify=ok
restoredrill: reconciliation dry-run: tables=134 rowcounts_match=true
              balance_drift_accounts=0 journal_hash_match=true
restoredrill: state change on the restored database: ok
restoredrill: OK (11.835s)
```

**That last line is new and is the one worth reading.** The three above it
compare data, and none of them proves the restored database can still be USED.
A restore that lost `cp_transition_key` would pass all three and then refuse
every state change in the system (F-129). The drill now drives one real audited
transition on the restored database as the application role, and
`CP_DRILL_BREAK=lose_the_transition_key` empties the table so the probe can be
watched firing.

A restored database reaches the same schema version, the same table count, the
same row counts, **zero balance drift and identical journal hashes.** Report at
`dist/restore-drill.json`. The 134 includes the fourteen partitions of
`security_events`: the first partitioned table in this schema restores as a
partitioned table, not as an empty parent.

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

Any ceiling reaching its headroom is the signal to move.

**The database ceiling stopped being a countdown.** Before 00740, `security_events`
could not have a row removed by any role, so any steady rate eventually filled
the 500 MB quota and the failure arrived looking like a capacity refusal rather
than a retention failure. It is now partitioned by month and a month can be
dropped.

**Nothing is dropped yet, and that is deliberate.**
`CP_RETENTION_SECURITY_EVENT_DAYS` is `0` on the blueprint, which disables
pruning. The mechanism exists; the period does not, because ADR-0020 leaves it
open as a product and compliance question. Dropping a security audit trail
because nobody chose a number is worse than a table that grows — a growing table
costs a capacity refusal, which is fail-closed and visible. Setting it is one
value and a redeploy, with a floor of 90 days enforced twice: in `config.Validate`
at boot, where an operator sees it, and in the SQL function at call time, where
an attacker holding the operations credential would be.

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
| `SOFTWARE_COMPLETE` | **false** | **One named item remains.** The PII-read policy (F-47), whose answer depends on an encryption that has not been built, so it cannot be taken yet. F-118 came off this list when the destination, the timer and the heartbeat landed — what is left of it is a URL in a dashboard, and the deployment refuses to boot without it rather than booting unalerted. F-42's stronger remedy came off when the last of its seventeen tables landed; `security_events` partitioning came off with 00740. What is left of F-84 and F-95 is design work and a class of comparison most of the constraints cannot have. |
| `STRIPE_PRODUCTION_APPROVED` | **false** | Stripe's own review of a real business. Not submitted. `BLOCKED_EXTERNAL`. |
| `LEGAL_APPROVED` | **false** | Counsel. `BLOCKED_EXTERNAL`. **No legal approval is claimed anywhere.** |
| `PENTEST_COMPLETE` | **false** | An independent third party. **This audit is not one and does not claim to be.** `BLOCKED_EXTERNAL`. |
| `LIVE_READY` | **false** | The conjunction of all four, plus the capability gate activated by three principals against four approval references. |

In the literal form §43 asks for:

```
SOFTWARE_COMPLETE        = false
STRIPE_PRODUCTION_APPROVED = false
LEGAL_APPROVED           = false
PENTEST_COMPLETE         = false
LIVE_READY               = false
```

**Thirty-three findings closed this session moved none of these**, and that is
the honest headline. Not one of the four independent reasons `LIVE_READY` is
false was a thing this audit could fix.

What changed is the length of the list behind `SOFTWARE_COMPLETE`, from four
items to two — and the character of what is left. Everything remaining under that
flag is now a **decision** or a **design**, not an unwritten control. That is a
different position from where this session started, and it is still not
`SOFTWARE_COMPLETE = true`.

---

## 16 · Remaining human actions

1. **Paste a PII keyring** (F-47). `openssl rand -base64 32` into
   `NODAL_PII_KEYRING` as `{"active":1,"keys":{"1":"<that>"}}` in the Render
   dashboard. The policy itself is decided and in the schema (00754); the key
   is the one thing the repository cannot contain. Without it the next deploy
   refuses to start, like the alert destination and for the same reason.
2. **Paste an alert destination** (F-118). Set `NODAL_ALERT_WEBHOOK_URL` in the
   Render dashboard to a Slack or Discord incoming-webhook URL, an ntfy topic,
   or any endpoint that takes a JSON POST; the shape is chosen from the host.
   Until it is set, the next deploy of this build refuses to start and Render
   keeps the current one serving — by design, and stated.
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
9. **Sync the Render blueprint on the next deploy, not just the code.**
   `CP_RETENTION_SECURITY_EVENT_DAYS` is new and required outside LOCAL/TEST.
   It is in `render.yaml` with value `0`, so a blueprint sync carries it — but a
   code-only push leaves it unset and **`cmd/api` will refuse to start**, by
   design: a missing required variable is a startup error, never a silent
   default. This is the intended behaviour of the config contract and it is
   stated here so it is not discovered during a deploy.
10. **Give the deployed binary a commit identity.** `build_version` is `dev`, so
    the deployment cannot be pinned to a commit from outside.
11. **Install a GCC whose path has no space**, if race claims are ever to be
    checkable off CI (F-125).

---

## 17 · Resume here

**Start with `docs/build/MASTER_BUILD_STATE.md`.** It is the continuity
document; this file is the checkpoint that points into it.

The next four pieces of software work, in the order they are worth doing:

1. **~~F-42's stronger remedy~~ — done (00743-00753).** All seventeen bound
   tables. Four rules were paid for and are in the register under F-42, because
   they will apply to any table that acquires a transition binding later: a row
   lock needs UPDATE privilege, so budget one column grant per table and say it
   is for the lock; binding tests move to the migration role, which can still
   write the column and is still refused by the trigger; a column with no home
   on the transition row usually belongs ON it rather than granted back; and
   where a destination is constrained, mirror the constraint onto the transition
   so an incomplete row is refused where it is written.

   **Two are done and are the worked examples to copy: `credit_fundings`
   (00743) and `accounts` (00744).** The
   AFTER INSERT trigger on the transitions table performs the state change as
   SECURITY DEFINER; the application keeps UPDATE on `lot_id` and
   `provider_reference` and nothing else; the money stamps moved into the
   transition, so there is no longer a way to reach REVERSIBLE without
   `reversible_at`. The Go call site became a re-read. Two fixtures and one
   assertion had to move, and the pattern of what breaks is in the register
   under F-42.

   **The rules that were paid for, kept here because they outlive the task.**

   *The row-lock rule.* `SELECT ... FOR UPDATE` requires UPDATE privilege, and
   so do all three other row-lock modes. Every one of these transitions locks
   the row before checking legality, so a plain REVOKE breaks the lock — it broke
   four integration packages on `accounts`. A **column-level** grant restores the
   lock and still refuses the write. Budget one column grant per table, and say
   in the migration that it is for the lock, or a later reader will think the
   application is meant to write it. 00743 got this right by accident, because
   its two grant-back columns happened to supply the privilege.

   *The test-fallout rule.* Tests that prove the audit binding by driving a bare
   update must move to the migration role, which can still write the column and
   is still refused by the trigger. Tests that prove a legitimate transition
   should drop their UPDATE entirely, because the transition row is now the
   change. And watch for a subtest whose premise stops existing rather than
   failing.

   **Pick the next table by counting its state-change sites, and treat that as a
   lower bound rather than an estimate.** `kill_switches` has one and is a trap:
   `saveSwitch` writes `active` together with six other columns under optimistic
   concurrency (`WHERE version = $11 RETURNING version`), so moving one column
   into a trigger breaks the version check and the returned row at once.
   `agents` also has one, writing seven columns including the promotion
   evidence.
2. ~~An alert destination and something on a timer (F-118).~~ Done: the
   destination, the timer, the heartbeat alarm and the payload shapes. A URL
   remains, and it is an operator's to paste.
3. **Birth control for `wallets`, `assets` and `instruments`** (F-122 residual).
   These are named in an assertion that fails when one is closed, so the list
   cannot go stale.
4. **An agent can still be born SHADOW without its promotion evidence** (F-122
   residual). A provenance gap, not a money one — closing it is a decision about
   how the suite seeds agents, and `00739` records the reasoning in full.

Then F-93's inventory rows.

**Choose a security-event retention period, or decide not to.**
`CP_RETENTION_SECURITY_EVENT_DAYS` is 0 and the machinery behind it is built and
tested. That is not code; it is the question ADR-0020 left open.

**The thing worth carrying forward is not on any of these lists.** Six fixtures
this session encoded the exact defect they were meant to guard against; an ADR
got the one fact its decision rested on backwards; a migration granted a secret
to two roles by writing no `GRANT` at all; and a comment in the request path
described a guarantee the schema had stopped making. Every one surfaced only
when something was checked rather than read.

A green suite proves the assertions ran. It does not prove the fixture they ran
against was ever safe, and a document is not evidence of the thing it describes.
