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
| `237ad43` | `docs: the checkpoint after the F-42 remedy and five F-93 rows` |
| `8866127` | `api: every reconciliation alert leaves the process, and the pass that raises it runs here` — F-118 |
| `32cbdd4` | `infra: a heartbeat the counter alarms cannot fake` — F-118 |
| `5b62a46` | `docs: F-118 closes; what is left of it is a URL` |
| `fe20a4c` | `pii: personal data is encrypted in the application, or it is not stored` — F-47 |
| `cc3237b` | `db: who may read personal data, decided by what the columns now hold` — F-47, F-133 |
| `11c6ab6` | `docs: F-47 closes the way its own updates said it would; F-133 opens and closes` |
| `d56b6b8` | `config: the keyring reference follows the *_REF convention, and 00754's Down is protected` |
| `26a5ac9` | `api: a Credit path that a configured provider left disabled is a page, not a log line` — F-93 |
| `a4e438b` | `docs: the launch matrix stops listing what was fixed, and F-93's silent half is recorded as closed` |

**Forty-nine commits since `05ec7f3`**, the session's starting point; the last
ten close the two items `SOFTWARE_COMPLETE` was waiting on.

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

Opened this session: **F-100 through F-133 — 34 findings, 17 P1, 11 P2, 6 P3.** F-133 is the last: expired sessions were never purged on any tier, found by checking what `cp_ops` actually did with the table before deciding what it may read.
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
| **F-125** | P3 | fixed | The race detector links on this host now (a copy of the toolchain at a path GCC resolves to without spaces); both race tiers run here, and no race claim rests on CI alone. Listed because an earlier revision of this table said the opposite. |

Closed after this checkpoint was first written, and listed because their absence
from the table above is the change: **F-105** (`security_events` is now
partitioned by month and prunable by detachment, 00740), **F-42** (the
transition flag is now a keyed tag the application cannot forge, 00741, after
four sessions open and three fixes tried and rejected), and — in the last ten
commits — **F-118** (alerts leave the process, in the shape the destination
accepts, from a verification pass the API runs itself, with a heartbeat the
counter alarms cannot fake), **F-47** (personal data is encrypted in the
application and withheld from the roles with no use for it, the decision
derived from the encryption rather than chosen) and **F-133** (expired
sessions are purged). **F-93's** "warning nobody reads" row closed with them:
a Credit path a configured provider leaves disabled now pages.

What is left in the table is three inventory or partial rows whose remaining
items are each stated with the reason they do not block the launch tier: F-65
(a bridge into an agent runtime that has no production caller, watched by a
test that fails the day it acquires one), F-69 and F-93 (rows that are product
or identity-model decisions, an external fact about a mainnet mint, a
circuit-breaker that is latent because nothing on this tier calls `Disable`,
and a test-scope widening), and two P3s (F-84, F-95). None is a P0/P1/P2
software defect that blocks intended launch behaviour; each is
non-launch-impacting with the evidence beside it.

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
| Migration head | **`00754_who_may_read_personal_data.sql`** |
| Migration files | **86** |
| Bound tables the application may still UPDATE | **0 of 17** |
| Tables | **120**, plus **14 partitions** of `security_events` |
| CHECK constraints | 462 declared on parents |

`security_events` is the **first partitioned table in this schema** (00740). A
partition is a table, so a freshly migrated database now reports 134 relations
where it reported 119; the restore drill counts them and they come back.

Applied migrations are never edited — a correction is a new file, and `00738`
exists solely to drop two constraints `00736`/`00737` got wrong, with the
reasoning kept in the file rather than in a commit message.

00754 is a grant migration: `identity_pii` and `sessions` are withheld from
`cp_readonly` and `cp_ops`, and `cp_ops` keeps `SELECT (expires_at)` on
`sessions` for its retention DELETE. Its Down is protected: a Down is not where
that decision is retaken.

**Evidence:** `REAL_DB_INTEGRATION` — counted from a live PostgreSQL 16 at
version 754 by the restore drill, not from the files.

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
| `go test ./...` | **99 packages with tests, 0 failures** |
| `go run ./scripts/inttest` | **52 packages, one fresh database each, all passed, 12m07s, 0 failures** |
| `go run ./scripts/fuzzall -fuzztime=10s` | **29 targets, 0 failed** |
| `go run ./scripts/restoredrill` | **OK, at version 754**: 134 tables, row counts identical, 0 accounts with balance drift, journal hashes equal, one live state change on the restored database, 25.6 s |
| `go run ./scripts/fmtcheck ./cmd ./internal ./scripts ./test ./packages` | ok |
| `go run ./scripts/tool golangci-lint run ./...` (and again with `--build-tags integration`) | 0 issues, both |
| `go run ./scripts/lintfin ./...` | 0 findings |
| `go run ./scripts/configcheck -service api .env.example` | **252 variables, valid** |
| `terraform validate` × dev, staging, prod | Success, all three |
| `terraform fmt -check -recursive` | clean |
| `test/docs` (register and document consistency) | pass |

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

| `make race` | **10 packages, 0 data races** — run with `CC`/`CXX` on the space-free toolchain copy (F-125); the first attempt without them failed to *link*, not to test, with F-125 exact signature |
| `make integration-race` | **12 packages, a database each, 6m27s, 0 data races** |

**No race claim in this repository rests on CI alone any more.** Both tiers were
run on this host and both are clean.

Every tier above was run after the last code-bearing commit (`26a5ac9`) and
none concurrently with another, for the reason the paragraph above gives.

**Evidence:** `LIVE_OBSERVED` for every row above. `REAL_DB_INTEGRATION` for
the integration packages, the integration race tier and the restore drill.

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
deployment is at HEAD, and it is not — the session's commits are local and
unpushed, as every session's have been until the owner pushes. Re-observed at
the end of the session: `/v1/healthz` 200, `/v1/readyz` 200, `/v1/version` 200
with the same `config_hash`, so the service is up and unchanged.

**The next deploy of this HEAD refuses to start until two dashboard secrets
exist**, and that is by design rather than by accident: `NODAL_ALERT_WEBHOOK_URL`
(F-118) and `NODAL_PII_KEYRING` (F-47) are both required in STAGING and PROD by
`config.Validate`, because an unalerted deployment and an unencrypted one are
the two states this session closed and neither should be reachable by
omission. Render keeps the current deploy serving while the new one fails its
health check. Both values are one paste each and are listed in §16.

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
| The audit binding cannot be forged | Fixed (F-42, F-128). The transition flag is a keyed tag over a secret no role but the owner can read, salted with the top-level transaction id, and EXECUTE on every function that touches it is revoked from PUBLIC. Seventeen audited tables. | `REAL_DB_INTEGRATION` |
| `security_events` retention | Fixed (F-105). Partitioned by month; retention is partition detachment, never row deletion, and the immutability trigger is unchanged for every role including the owner. | `REAL_DB_INTEGRATION` |
| A state column the application cannot write at all | **Done. Zero of seventeen bound tables still grant the application blanket UPDATE** (00743-00753), counted from the schema rather than from a list. The refusal moved from `AUDIT_TRANSITION_REQUIRED` at COMMIT to `permission denied` at the statement, everywhere. | `REAL_DB_INTEGRATION` |
| The race detector | Runs on this host, both tiers, 0 data races (F-125). | `LIVE_OBSERVED` |
| Personal data at rest | Encrypted in the application (F-47): AES-256-GCM, each ciphertext bound to its row, column and key version, a versioned keyring the database never sees. `internal/pii/store.go` is the only writer of `identity_pii`, asserted by `test/security`. | `REAL_DB_INTEGRATION` |
| Who may read personal data | Decided in the schema (00754): neither `cp_readonly` nor `cp_ops` reads `identity_pii` or `sessions`; `cp_ops` reads `sessions.expires_at` alone, for the purge that now runs (F-133). Asserted in the strong direction, like `cp_transition_key`. | `REAL_DB_INTEGRATION` |
| Something is told | Every reconciliation alert leaves the process to a webhook the deployment names, in the shape the destination accepts, from a bounded queue that never blocks the transaction that raised it; only allowlisted fields egress (F-118). A configured-but-disabled Credit path pages (F-93). | `REAL_DB_INTEGRATION` for the end-to-end drift-to-webhook test; `LIVE_OBSERVED` for ntfy accepting the headed POST |

Eighteen files under `test/security/` cover authority boundaries, dual control,
idempotency abuse and break scanning, and run as part of the 51-package
integration tier.

---

## 10 · Restore drill

Run at `11c6ab6`, after 00754:

```
restoredrill: boot: version source=754 restored=754 verify=ok
restoredrill: reconciliation dry-run: tables=134 rowcounts_match=true
              balance_drift_accounts=0 journal_hash_match=true
restoredrill: state change on the restored database: ok
restoredrill: OK (25.556s)
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

### Three periodic jobs that would otherwise never run

This deployment has one process. `runCreditSettlement` (F-90),
`runOpsRetention` (F-105 for login attempts and `security_events` partitions,
F-133 for expired sessions) and `runInternalVerification` (F-118: Σ journal
entries against `ledger_balances`, Σ active reservations against their totals,
envelope allocation against its flow, then escalation of stale material
mismatches — every five minutes) all live inside `cmd/api` for that reason,
each running once at startup because a process that wakes, serves a login and
spins down would otherwise never run them at all. Retention **warns at WARN
naming the consequence** when unconfigured rather than refusing to boot — a web
service's job is serving requests — but it is never silent.

### What the deployment must now be told

Two values `config.Validate` refuses to start STAGING or PROD without, both
dashboard secrets, both $0:

| Secret | For | What to paste |
|---|---|---|
| `NODAL_ALERT_WEBHOOK_URL` | F-118 | a Slack or Discord incoming-webhook URL, an ntfy topic, or any HTTPS endpoint; the payload shape is chosen from the host |
| `NODAL_PII_KEYRING` | F-47 | `{"active":1,"keys":{"1":"<openssl rand -base64 32>"}}` |

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

The five application counter alarms keep `treat_missing_data = "notBreaching"`,
correctly — a mismatch counter that never arrives is a system with no
mismatches. What they could not tell apart was that from a system whose
instruments were never constructed, and `verification-heartbeat-missing` now
can: `verification_passes` is emitted once per completed verification pass and
its alarm breaches when fifteen minutes carry no sample (F-118). `test/infra`
proves every alarm bound to the application namespace names an instrument some
Go file constructs, in both construction styles the tree uses.

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
| `SOFTWARE_COMPLETE` | **false — productization in progress** | **The product goal (`product_goal.md`) adds a customer-facing surface from commit `9906c9f` onward, and its own rule sets this flag false the moment material product code lands, until that surface is built, deployed, audited and reconciled. The judgement below is the one that held at `024c691` for the backend as audited, and it still holds for that backend.** Every item the flag was waiting on is closed, and what remains is not engineering. Against §37's list: no known P0/P1/P2 software defect blocks intended launch behaviour (F-93 and F-69 are inventory rows whose remaining items are decisions, an external fact and a latent path with the reason beside each; F-65 is a bridge into a runtime with no production caller, watched; F-84 and F-95 are P3); every remaining finding is closed or explicitly non-launch-impacting with evidence; the state machines, schema, configuration and provider boundaries are coherent (0 of 17 bound tables writable by the application, 86 checksum-verified migrations, 252 validated variables, every provider `sandbox`); the financial invariants pass (§8); the live STAGING service is up and serves the last deployed build — HEAD is ahead of it by this session's unpushed commits and its next deploy needs two dashboard secrets, which is §16's first item and the owner's, not engineering's; the sandbox provider path is proven to the limit governance permits (§7); no fake can enter PROD (`RuleNoFakeProviders`, re-checked in the binary); the $0 cohort controls refuse rather than degrade (§11); restore is proven at 754 (§10); the AWS path validates and nothing is applied (§12); and the documents say what the tree does (`test/docs`). **This is not `LIVE_READY`**, and the four flags below are why. |
| `STRIPE_PRODUCTION_APPROVED` | **false** | Stripe's own review of a real business. Not submitted. `BLOCKED_EXTERNAL`. |
| `LEGAL_APPROVED` | **false** | Counsel. `BLOCKED_EXTERNAL`. **No legal approval is claimed anywhere.** |
| `PENTEST_COMPLETE` | **false** | An independent third party. **This audit is not one and does not claim to be.** `BLOCKED_EXTERNAL`. |
| `LIVE_READY` | **false** | The conjunction of all four, plus the capability gate activated by three principals against four approval references. |

In the literal form §43 asks for:

```
SOFTWARE_COMPLETE        = false   # productization in progress; true at 024c691 for the audited backend
STRIPE_PRODUCTION_APPROVED = false
LEGAL_APPROVED           = false
PENTEST_COMPLETE         = false
LIVE_READY               = false
```

**Thirty-four findings closed this session moved one of these**, and the
honest headline is which one: `SOFTWARE_COMPLETE`, the only flag engineering
can move. The list behind it went from four items to two to none — F-42's
remedy, `security_events` partitioning, an alert destination and the PII-read
policy — and the last two closed the way their own register entries said they
would: the destination once something raised into it and a timer ran on this
tier, and the policy once the encryption existed for it to be derived from.

Not one of the four independent reasons `LIVE_READY` is false was a thing this
session could fix, and none was touched: no Stripe attestation was submitted,
no legal approval is claimed, no pentest is claimed, no gate was activated, no
real money moved, and nothing paid was applied.

---

## 16 · Remaining human actions

Only what needs a person or an external party. The first two are one paste
each in the Render dashboard and are what the next deploy of this HEAD waits
on; everything after them is what `LIVE_READY` waits on.

1. **Paste an alert destination** (F-118): `NODAL_ALERT_WEBHOOK_URL` — a Slack
   or Discord incoming-webhook URL, an ntfy topic, or any HTTPS endpoint that
   takes a JSON POST. The payload shape is chosen from the host.
2. **Paste a PII keyring** (F-47): `NODAL_PII_KEYRING` =
   `{"active":1,"keys":{"1":"<openssl rand -base64 32>"}}`. The policy is in
   the schema; the key is the one thing the repository cannot contain.
3. **Push `main`.** Every commit of this session is local. Render deploys from
   `main`; with the two secrets above set, the deploy boots and STAGING is at
   HEAD. Without them it refuses, by design, and the current deploy keeps
   serving.
4. **Engage counsel** for B-02, B-03 and B-07.
5. **Submit the Stripe restricted-business review** (B-09). Deliberately not
   done here.
6. **Commission an independent penetration test** (B-08).
7. **Sign a payout provider contract** (B-01), and select the partner rail
   (B-05) and identity verification provider (B-06).
8. **Authenticate a non-root AWS role** (B-12), when scale-up is wanted.
9. **Activate `CREDIT_PURCHASE`** — three distinct principals, four approval
   references, step-up within 15 minutes. Not fabricable, and fabricating it
   would defeat the control.
10. **Give the deployed binary a commit identity.** `build_version` is `dev`, so
    the deployment cannot be pinned to a commit from outside.
11. **Choose a security-event retention period, or decide not to** (ADR-0020).
    `CP_RETENTION_SECURITY_EVENT_DAYS` is 0 and the machinery is built.

---

## 17 · Resume here

**Start with `docs/build/MASTER_BUILD_STATE.md`.** It is the continuity
document; this file is the checkpoint that points into it.

`SOFTWARE_COMPLETE` is true and nothing on the engineering list is owed for the
launch tier. If a session resumes here, this is the order the remaining
software work is worth doing in — none of it is a launch item, and each is
already named in an assertion or a register entry so it cannot go stale:

1. **F-84** — authorise on the chi route pattern before the generated wrapper,
   so an unauthenticated request answers 401 before it answers 400. A change
   to the boundary's structure; it wants its own design.
2. **F-122's residual** — birth control for `wallets`, `assets` and
   `instruments`, and an agent born SHADOW without its promotion evidence.
   `TestIntegration_NothingIsBornFinished` names the three tables.
3. **F-95** — the enum CHECKs that do have a Go list to compare against.
4. **F-93's test-scope row** — widen `test/infra`'s `CP_*` literal scan to
   `internal/` and to non-literal reads.
5. **F-65's bridge**, only when the agent runtime acquires a production caller;
   `TestDeferredBridge_TheAgentRuntimeIsStillInert` fails on that day and says
   what is owed.

Then the paid-tier inventory F-118's sweep turned up and F-69 holds:
`internal/notification`'s dispatcher, the Temporal escalation workflow and the
`reconciliation.record.transitioned` subscriber, none of which runs on a tier
with no Temporal and no relay.

**The thing worth carrying forward is not on any of these lists.** Twice this
session a control that existed was found to have nobody running it — the
alert seam with no caller, the session purge with no caller — and once a claim
about a destination ("accepts a JSON POST") was true of the transport and
false of the two most likely destinations. Each surfaced only when something
was checked rather than read: a grep for callers, a POST to the real endpoint.
A green suite proves the assertions ran. It does not prove the thing they
describe has anyone on the other end of it.
