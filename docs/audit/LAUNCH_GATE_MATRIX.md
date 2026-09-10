# Launch gate matrix

What stands between this repository and taking real money, and who can move
each one. Every row is either something software can finish, in which case it is
work and is tracked, or something a person outside this repository has to
produce, in which case it is BLOCKED_EXTERNAL and no amount of engineering
changes it.

Last audited: **2026-09-10**, against the working tree at migration 00739 and
the deployment at `https://api-nodal.actorvia.xyz`.

The five flags are unchanged and all still false. Twenty-three findings closed
since the previous audit did not move any of them, which is the honest
headline: **none of the four independent reasons `LIVE_READY` is false was a
thing this audit could fix**, and the fifth flag — `SOFTWARE_COMPLETE` — has a
shorter list behind it than it did, not an empty one.

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

`CREDIT_PURCHASE` is not activated. `capability_gates` has no row for it in the
deployed database, so `gates.Evaluate` returns `ReasonNoGateRow` and
`PurchaseService.StartPurchase` refuses with `CAPABILITY_NOT_APPROVED` before it
reaches pricing or the provider.

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

Nothing in this table moved as a result of this audit, and nothing in it can be
moved by more engineering.

## What software still owes

These are the reasons `SOFTWARE_COMPLETE` is false. Each is tracked with its
evidence in `docs/audit/AUDIT_FINDINGS.md`.

| Item | Finding | Shape |
|---|---|---|
| **Nothing pages anyone about anything** | F-118 | The largest of these. Alerts now log and both roots build the real instruments; what remains is a destination and something on a timer. Half deployment decision. Before the Terraform is ever applied, its five application alarms set `treat_missing_data = "notBreaching"`, so a metric that never arrives reads OK |
| A state column the application cannot write at all | F-42 | The audit binding can no longer be forged (00741), which is detection. This is privilege: revoke UPDATE on the state column and route changes through SECURITY DEFINER functions. **Three of seventeen remain** — `deposits`, `kill_switches`, `reconciliation_records`. Eight were done under this finding (00743-00750). Rules paid for: a row lock needs UPDATE privilege, so budget one column grant per table and say it is for the lock; binding tests move to the migration role; a column with no home on the transition row usually belongs ON it rather than granted back; and where the destination is constrained, mirror the constraint onto the transition so an incomplete row is refused where it is written |
| Choose a security-event retention period, or decide not to | ADR-0020 | `CP_RETENTION_SECURITY_EVENT_DAYS` is 0 and the machinery behind it is built and tested (00740). Not code; the question the ADR left open |
| `login_attempts` has a purge that this deployment never runs | F-105 | A cron on the blueprint, or a ticker in `cmd/api` the way `runCreditSettlement` already is |
| Request validation precedes authentication | F-84 | Authorising on the route pattern before the generated wrapper; wants its own design |
| `cmd/api` requires the schema-owner credential | F-93 | Make `CP_DATABASE_MIGRATE_URL` service-conditional; needs a blueprint change and a redeploy |
| A wrong Stripe account warns rather than refusing to start | F-93 | A real availability decision on a tier that cold-starts |
| Render replica count is an assumption, not an assertion | F-93 | A startup advisory lock would make it one |
| Neon pool has no idle or connect timeout | F-93 | Small change, needs measurements this repository does not have |
| Three retention classes are declared and unenforced | ADR-0020 | The ADR decides how; the migration and the partition manager remain |
| 121 enum CHECKs have no Go counterpart | F-95 | Most have no Go list to compare against, by their nature |
| An agent can be born SHADOW without its promotion evidence | F-122 | A provenance gap, not a money one; closing it is a decision about how the suite seeds agents |
| `wallets`, `assets` and `instruments` have no birth control | F-122 | Named in an assertion that fails when one is closed, so the list cannot go stale |
| Two statements about who may read encrypted PII contradict each other | F-47 | A policy decision |

Closed since the previous audit, and listed because their absence from this
table is the change: the buffered request body (F-85), the fifteen
destination-only transition bindings (F-94), the forged dual control that made
every other approval-bearing control conditional (F-121), the unprunable
security trail (F-105, 00740) and the forgeable audit binding itself (F-42,
00741, after four sessions open and three fixes tried and rejected).

**And one this audit caused and then found.** 00741 made every state change
depend on one row, and a restore that lost it would have passed every comparison
the restore drill makes before refusing every state change in the system. The
drill now drives a real transition on the restored database (F-129, 00742).
Comparing data does not prove a database can be used.

**None of these moved a launch flag**, which is the honest way to read the
change: `SOFTWARE_COMPLETE` has a shorter list behind it and the same value.

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
