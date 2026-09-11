# Launch gate matrix

What stands between this repository and taking real money, and who can move
each one. Every row is either something software can finish, in which case it is
work and is tracked, or something a person outside this repository has to
produce, in which case it is BLOCKED_EXTERNAL and no amount of engineering
changes it.

Last audited: **2026-09-10, late evening**, against the `productization`
branch at migration 00805 (the working tree; see the "RESUME HERE" note in
`docs/build/MASTER_BUILD_STATE.md` for the commit) and the deployment at
`https://api-nodal.actorvia.xyz`, which still serves the pre-productization
build.

The five flags are all still false. The productization goal added a customer
product on top of the audited backend — sign-up, Credits, native markets,
portfolio, activity, agents at levels 1–3, Withdraw, verification and the
conversion request at the governance boundary — and a sandbox tier on which a
STAGING deployment can exercise all of it without a single fabricated
approval. `SOFTWARE_COMPLETE` was true at `024c691` for the backend as then
audited; the goal set it to **false for its own duration** and it returns to
true only when the list under "What software still owes" is empty. Ninety
findings (one P0, twelve P1) were found by the goal's own adversarial audits
and fixed with regression tests; wave B of those audits is landing as this is
written. **None of the four external reasons `LIVE_READY` is false moved**, by
construction: a sandbox tier is the mechanism that makes that fact harmless
to the product's development and impossible to hide.

## The five launch flags

| Flag | Value | What would change it |
|---|---|---|
| `SOFTWARE_COMPLETE` | **false** | Named work remains. See "What software still owes" below. |
| `STRIPE_PRODUCTION_APPROVED` | **false** | Stripe's own review of a real business. BLOCKED_EXTERNAL. |
| `LEGAL_APPROVED` | **false** | Counsel. BLOCKED_EXTERNAL. |
| `PENTEST_COMPLETE` | **false** | An independent third party. This audit is not one and does not claim to be. BLOCKED_EXTERNAL. |
| `LIVE_READY` | **false** | All four above, plus the capability gate activated by three people. |

`LIVE_READY` is the conjunction. It is false today for four independent
reasons, and three of them cannot be resolved from inside this repository at
all.

## The gate that actually holds the money path shut

`CREDIT_PURCHASE` is not activated, and neither is any other product gate.
`capability_gates` has no row for it in the deployed database, so
`gates.Evaluate` returns `ReasonNoGateRow` and `PurchaseService.StartPurchase`
refuses with `CAPABILITY_NOT_APPROVED` before it reaches pricing or the
provider.

**What the sandbox tier adds, and does not add (ADR-0023, D-052).** A
deployment that declares `CP_API_LEGAL_POLICY=SANDBOX` may move a gate into a
sixth state, `SANDBOX`, at boot (`CP_API_SANDBOX_GATES`). The checker reads a
SANDBOX row as active **only on a sandbox tier**; the row carries no approval
version, no evidence digest and no revoke (00791, F-160–F-161); the operator
console renders it as "not an approval"; the payout provider on such a tier is
`sandbox_payout`, which moves nothing, and the verification provider is
`sandbox_verification`, which decides nothing on its own. PROD refuses every
part of it three times over: `config.Validate` refuses the policy value, the
gate function refuses the transition, and 00755's CHECK refuses the row. So
the ceremony below is unchanged, and the six product gates are exactly as far
from ACTIVE as they were — a sandbox row is a way to run the product without
lying about the gate, not a way past it.

Activating it requires, in this order and by construction:

| Step | Requires | Enforced where |
|---|---|---|
| Propose | `gate:propose`, a recent step-up, and all four evidence references | `internal/gates.Admin.Propose` (step-up added by F-99) |
| Approve | `gate:approve`, a recent step-up, a principal **distinct from the proposer** | `internal/gates.Admin.Approve`, re-derived in SQL by `cp_gate_transition` |
| Activate | `gate:approve`, a recent step-up, a principal distinct from **both** | `internal/gates.Admin.Activate`, re-derived in SQL |

The four evidence references are legal review, provider contract, risk approval
and security approval. They are the rows below marked BLOCKED_EXTERNAL, which is
why this gate is where the external blockers meet the code.

**Stated plainly, because the provider checkpoint did not:** "three distinct
principals" means three distinct `users.id` values. Distinctness is a string
comparison; there is no person entity and no uniqueness on email. Three identity
provider accounts under one person's control satisfy the ceremony in both Go and
Postgres. It is a control against one careless operator, not against one
determined one (F-93).

## BLOCKED_EXTERNAL

| # | Item | Why software cannot resolve it | What the code already does |
|---|---|---|---|
| 1 | Stripe production approval | A business relationship and a restricted-business review by Stripe. | The adapter, the webhook path, the evidence store and the reconciliation are built and exercised against Stripe's sandbox. Going live is a configuration change, not construction. |
| 2 | Legal review reference | Counsel's opinion on selling Credits in the intended jurisdictions. | `internal/legalrouter` refuses by default and `CP_API_LEGAL_POLICY` is CONSERVATIVE unless set. |
| 3 | Provider contract reference | A signed contract. `PurchaseRegistry` refuses a provider with no `ContractReference` outside LOCAL/TEST. | The refusal is built and tested. |
| 4 | Risk approval reference | A person with the authority to sign it. | The gate demands it and re-checks it at activation. |
| 5 | Security approval reference | Same. | Same. |
| 6 | Independent penetration test | A third party. | The adversarial suites in `test/security` are not a substitute and are not offered as one. |
| 7 | A mainnet settlement mint | `CP_API_SETTLEMENT_MINT` is a Solana **devnet** USDC mint. The mainnet address is a fact about Solana, not a value to invent. | The pair is validated against the registered stablecoins in the database; nothing refuses a devnet mint in PROD, which is recorded in F-93. |
| 8 | Two Render secrets, the push of `main`, the `app-nodal` CNAME, the first operator's subject | Dashboard, DNS and identity-provider actions a person takes; the values are secrets that must not pass through this session (`docs/build/HUMAN_ACTIONS_QUEUE.md` items 1–6). | STAGING refuses to boot without the two secrets (F-137); the blueprint declares every service, domain and header; `build_version` reports the deployed commit (F-142). The staging deployment of the productization build, and §56's browser walk of it, wait on these. |
| 9 | GitHub Actions minutes for the private repository | Exhausted for September on the free plan; restoring CI is a billing or a publication decision (queue item 7). | Every tier CI would run has been run on this machine and is recorded in `docs/audit/PRODUCTION_EVIDENCE_INDEX.md`; the cross-platform job now runs on `main` only so a pull request costs Linux minutes alone. |

Rows 1–7 did not move as a result of this goal, and cannot be moved by more
engineering. Rows 8 and 9 are new, and are human actions rather than
engineering.

## What software still owes

These are the reasons `SOFTWARE_COMPLETE` is false **now**, after the
productization goal's build and its first audit round. The table after them is
the pre-productization list, kept because its rows are still true and still
open.

| Item | Where it stands | What closes it |
|---|---|---|
| The withdrawal area is closed after four audit rounds | Round one (13 findings, 4 P1) fixed and merged (`8a1b701`); round two (`a9ad0af`) found 11, **4 P1 again**, six of them in round one's remediation: a derived lot inherited finality but not origin, so a promotional grant traded into withdrawable proceeds (§23's forbidden pattern), the pool's FIFO draw-down handed a reversible purchase a settled contributor's provenance, the promotion sweep starved after a hundred lots, a same-state transition row wrote money and renewed a verification window, and the destination machine had no legal-edge table | `fix/withdrawal-2` is merged (`e27e674`); round three (`6de84ef`) found **6 (1 P1)** against 11 (4 P1) — flattening, not flat: the P1 is the reservation drawing by origin while the decision is per lot, the same invariant failing at a third layer. `fix/withdrawal-3` is merged (`58d49c2`) and the narrow fourth round (`851cc64`) found 6 (1 P1 — a frozen derived lot never thawed when its dispute is won, a seam the fix walked into; five small) and declared the area flat: the three repaired invariants held under exhaustive attack (129,960 provenance combinations; 30 regression reproductions green). `fix/withdrawal-4` is merged (`5b2a414`; F-278–F-283; D-140, D-141) with its targeted verification green; nothing is owed here. What is recorded as residual is not engineering: `sanctions_state`'s legal edges are a compliance decision (B-02) |
| The productization build has not been deployed to STAGING or seen in a browser there | `api-nodal.actorvia.xyz` serves the pre-productization build; the human queue's items 1–4 gate the deploy and the CNAME | The human's queue, then §2's `healthz`/`readyz`/`version` check and §56's browser walk, recorded in `PRODUCTION_EVIDENCE_INDEX.md` |
| CI has not run on the productization tree | Actions minutes exhausted (BLOCKED_EXTERNAL row 9) | Queue item 7, then a green run on PR #1 |
| The final report (§65) and the `SOFTWARE_COMPLETE` decision | Written (`docs/audit/PRODUCTIZATION_FINAL_REPORT.md`): every §57 software condition is met on the local evidence; the one unmet condition — "live staging matches current HEAD" — is the human queue's, so the flag is false for a reason that is not engineering | The human's queue items 1–3b, then §2's check and §56's browser walk; the flag turns when the deployed `/v1/version` reports the merged commit and the walk is recorded |

### The pre-productization list (still open)

Each is tracked with its evidence in `docs/audit/AUDIT_FINDINGS.md`.

| Item | Finding | Shape |
|---|---|---|
| **Nothing pages anyone about anything** | F-118 | **Closed in software.** `internal/alert` delivers to a webhook in the shape the destination accepts (Slack, Discord, ntfy, generic); `cmd/api` runs the verification pass every five minutes; `verification_passes` is a heartbeat whose alarm breaches on missing data, so the five counter alarms keeping `notBreaching` can no longer read green over a system emitting nothing. Remaining: `NODAL_ALERT_WEBHOOK_URL` in the Render dashboard — the deployment refuses to boot without it |

| Choose a security-event retention period, or decide not to | ADR-0020 | `CP_RETENTION_SECURITY_EVENT_DAYS` is 0 and the machinery behind it is built and tested (00740). Not code; the question the ADR left open |
| Request validation precedes authentication | F-84 | Authorising on the route pattern before the generated wrapper; wants its own design |
| A wrong Stripe account warns rather than refusing to start | F-93 | **The silent half is fixed**: the path stays disabled and the service keeps serving, and the reason now reaches the alert destination as `credit_purchase_disabled` (SEV2). Refusing to start would make every cold start during a Stripe blip an outage, which is the code's own reasoning and stands |
| Three retention classes are declared and unenforced | ADR-0020 | The ADR decides how; the migration and the partition manager remain |
| 121 enum CHECKs have no Go counterpart | F-95 | Most have no Go list to compare against, by their nature |
| An agent can be born SHADOW without its promotion evidence | F-122 | A provenance gap, not a money one; closing it is a decision about how the suite seeds agents |
| `wallets`, `assets` and `instruments` have no birth control | F-122 | Named in an assertion that fails when one is closed, so the list cannot go stale |
| Two statements about who may read encrypted PII contradict each other | F-47 | **Resolved in the schema** (00754) once `internal/pii` made the columns ciphertext: neither role reads `identity_pii`, `cp_readonly` does not read `sessions`, `cp_ops` reads `sessions.expires_at` alone. Remaining: `NODAL_PII_KEYRING` in the Render dashboard; STAGING/PROD refuse to start without it |

Closed since the previous audit, and listed because their absence from this
table is the change: the buffered request body (F-85), the fifteen
destination-only transition bindings (F-94), the forged dual control that made
every other approval-bearing control conditional (F-121), the unprunable
security trail (F-105, 00740) and the forgeable audit binding itself (F-42,
00741, after four sessions open and three fixes tried and rejected). And on
2026-09-10: the `login_attempts` purge and the `security_events` partitions
run from `cmd/api` (F-105); the web service no longer holds the schema-owner
credential, the replica count is an advisory lock rather than an assumption,
and the Neon pool has a connect timeout and an idle time (F-93); alerts leave
the process (F-118); personal data is encrypted and withheld from the roles
with no use for it (F-47); and expired sessions are purged (F-133).

**F-42's stronger remedy is complete and has left this table.** Seventeen bound
tables, zero of which still grant the application blanket UPDATE (00743-00753).
Detection became privilege everywhere: the transition row is the state change,
and the application cannot write a state column at all. Four rules were paid for
along the way and are recorded in the register — a row lock needs UPDATE
privilege; binding tests move to the migration role; a column with no home on
the transition row usually belongs ON it; and where a destination is
constrained, mirror the constraint onto the transition so an incomplete row is
refused where it is written.

**And one this audit caused and then found.** 00741 made every state change
depend on one row, and a restore that lost it would have passed every comparison
the restore drill makes before refusing every state change in the system. The
drill now drives a real transition on the restored database (F-129, 00742).
Comparing data does not prove a database can be used.

**None of these moved a launch flag**, which is the honest way to read the
change: `SOFTWARE_COMPLETE` has a shorter list behind it and the same value.

## What the productization wave (2026-09-10, afternoon to night) changed

Ninety findings, F-134 to F-223, from eight adversarial audits of the goal's
own additions — config-deploy, credits-payments, governance-sandbox-tier,
platform-hardening, accounts-auth, agents-notifications, markets, frontend —
each by an agent that had not written the area, each finding reproduced by a
failing test on the auditor's own branch before a fix agent was given it, and
every fix merged with the auditor's test inverted to hold the fixed behaviour.
One P0 and twelve P1s. The ones that would have cost money, authority or the
service itself:

- the pricing policy minted a *count* of Credits into a field that means base
  units, so $10 bought 0.001 Credits (F-151, P0) — found before any purchase
  was ever made against a provider, which is the sandbox tier doing its job;
- a chargeback destroyed whichever lots sorted first and stranded the units it
  reversed (F-152); a failed provider call held the money-at-risk ceiling
  forever (F-153); the sweep that recovers a swallowed provider event had no
  caller in the deployed topology (F-154);
- the blueprint handed the internet-facing API the schema-owner DSN under a
  name the test that forbids it did not know (F-136), and STAGING booted with
  no alert destination and no PII keyring because both rules were satisfied by
  the *reference* (F-137);
- no kill switch reached the conversion-request path, so `WITHDRAWALS_DISABLE`,
  `GLOBAL_NEW_RISK_KILL` and `ACCOUNT_FREEZE` stopped none of it (F-163);
- any caller behind the platform router chose its own rate-limit key and its
  own audit address by writing one header (F-166);
- any authenticated person could end the API process by closing a stream while
  an event was published (F-185);
- a market whose marginal price truncates to zero could be opened, and every
  §47 control then failed open (F-193);
- and in the browser: the withdrawal bar claimed more than the total (F-201),
  a fill left the portfolio stale (F-202), and a refused order told the
  customer about an idempotency key instead of about the trade (F-203).

**Three things the wave got wrong and corrected**, in the tradition of the
section below:

1. The first merged browser run at full load failed at its own sign-in setup
   because two onboarding mutations fired their refetch and forgot it (F-222);
   the earlier, quieter runs had passed by timing.
2. The demo seeder, run as a command, refused every demo trade on a database
   whose gates were sandbox-active, because it built a ledger with no
   capability resolver — the resolver was private to `cmd/api` (F-223).
3. The follower's two fixes (F-167 on one branch, F-190 on another) met at the
   merge and disagreed about one line; the merged rule is D-118, and the wedge
   reproduction had to be inverted *and* re-stamped into the past because its
   future-stamped fixture was moving other tests' cursors.

Wave B — the withdrawal-verification area (eleven findings, four P1),
documentation against the tree (fourteen, none above P2, from a 4,704-row
claims ledger of which 4,461 held) and the browser end to end (six) — is fixed
and merged; its findings are F-224–F-253. The audits also exposed that no
route accepted a strategy version and no tier had a compiler, so "Create
Agent" could never complete (F-255–F-257); the acceptance route and a
sandbox-tier structured compiler that interprets no prose closed that
(`4814887`).

## What the 2026-09-10 audit changed

Twenty-three findings, F-100 to F-122, from eleven parallel read-only audits
whose claims were re-verified before anything was changed. **Ten were P1 and
every one of them was observed failing before it was believed.** The ones that
would have cost real money or real authority:

- a refund parked a funding for review, the next webhook un-parked it, and
  Credits were minted for money that had been returned — with no operator path
  out of review at all (F-100);
- one transition row licensed a second, unrelated edge, because the previous
  audit's own migration put its delimiters in band (F-101);
- one ADMIN session could cancel any customer's intent and move any seller's
  product, past a guard that matched one helper name of four (F-102);
- five configuration rules permitted what the deployment cannot survive,
  including live payment credentials in DEV (F-103);
- an unauthenticated caller chose how many permanent, undeletable rows the
  service wrote, on a deployment whose database ceiling halts every financial
  action (F-105);
- three money tables handed one account's record to another on a reused
  idempotency key (F-106);
- the seller set the platform's own commission, and a payout the provider may
  already have paid could be cancelled by its owner (F-107);
- two failed RPCs were read as proof a transaction never happened (F-108);
- the application role could rewrite an amount, a destination, or the definition
  of what counts as money (F-109);
- a shipped configuration removed the cookie prefix a takeover fix depends on
  (F-112);
- a revoked agent could return to live capital with no approval (F-114);
- the branch whose comment reads "do not resubmit" was the one that resubmitted
  (F-115);
- and dual control itself could be forged at INSERT, which is the missing half
  of a fully-evidenced agent promotion to LIVE (F-121).

**Three things this audit got wrong and corrected**, recorded because a matrix
that only lists successes is the kind of document this audit exists to
distrust:

1. An integration run was reported as passing when a pipe had hidden two package
   failures. Both were fixtures the audit's own grant migration exposed.
2. One reported finding — that a negative money ceiling could be silently
   accepted — was not true. Only its test was passing for the wrong reason, and
   F-104 records the narrower fact rather than the reported one.
3. The birth controls took three attempts. Two constraints asserted an ordering
   the money path does not have and were dropped with the reason written into
   00738; the replacement was keyed on the code's happy path rather than on the
   property being protected, and 00739 restates it against the lines the schema
   already draws.

**Six fixtures encoded the defect they were meant to guard against**, and every
one was found by closing a control rather than by reading the fixture. That is
the most transferable thing here: a suite's known-good fixture is an assertion
about what is safe, and nothing was checking it.

## What the 2026-09-09 audit changed about the matrix

Before it, four of the rows in "what software still owes" were not known to
exist, and four defects that would have stopped the product were not either:

- a planted OIDC callback signed the victim in as the attacker (F-87);
- every unauthenticated rate-limit bucket was one shared counter (F-88);
- the money-at-risk ceiling could only rise, and would have refused every
  purchase forever at $2,000 of lifetime sales (F-90);
- the 50-account launch cohort was enforced nowhere (F-91);
- and the Stripe call inside the purchase transaction meant eight concurrent
  purchases could each pass one ceiling (F-96).

None of those was visible from the provider activation checkpoint, which
recorded the surfaces they live on as VERIFIED PASS.
