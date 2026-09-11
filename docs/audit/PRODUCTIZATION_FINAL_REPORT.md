# Productization final report — 2026-09-11

The twenty-six-item report the productization goal (§65) requires, written
against observed evidence rather than intent. Every claim carries the evidence
class `docs/audit/PRODUCTION_EVIDENCE_INDEX.md` uses (`LIVE_OBSERVED`,
`STATIC_PROOF`, `BLOCKED_EXTERNAL`, `UNKNOWN`); where a class is `UNKNOWN` or
`BLOCKED_EXTERNAL` the item says so rather than rounding up.

**Status of this document: DRAFT, being finished while wave B of the audit
lands.** Items marked *(pending)* are refreshed when the last two fix branches
merge, the browser audit reports, and the human actions queue is done. The
headline does not change with them:

> `SOFTWARE_COMPLETE` is **false**. The customer product is built, merged,
> audited and green on every local tier; it has not been deployed to STAGING
> or walked in a browser there, because the deployment waits on two secrets
> only a person may set; the withdrawal area's second audit found four P1s
> whose fixes are on a branch; and CI cannot run because the repository's
> Actions minutes are spent. The other four launch flags are false for reasons
> no engineering clears.

---

## 1 · Final HEAD

Run `git log -1 productization` for the value; a hash cannot be inside the
object it hashes. The branch is `productization`, pushed to
`origin/productization`; `main` is at `9906c9f` locally and is the human's to
push (queue item 3). The commit this draft was written at is named in
`docs/build/MASTER_BUILD_STATE.md` "RESUME HERE". *(pending: the final value
after the wave-B merges.)*

## 2 · Product URLs

| Surface | URL | State |
|---|---|---|
| Landing and app (one static site; `/` is the public site, `/home` onward is the app) | `https://app-nodal.actorvia.xyz` | **not yet live**: the static site `nodal-web` is created by Render's blueprint sync when `productization` reaches `main`; the CNAME is queue item 4 |
| API | `https://api-nodal.actorvia.xyz` | live, serving the **pre-productization** build (`/v1/version` → `build_version: dev`, `environment: STAGING`; `healthz`/`readyz` 200 at 00:10 PDT) |
| Operator console | the static site's `/admin` | ships with the site |

Class: `LIVE_OBSERVED` for the API; `STATIC_PROOF` (`render.yaml`) for the rest.

## 3 · User account architecture

**ZITADEL authenticates; Neon owns the Nodal user** (ADR-0022). Sign-up is
"create a ZITADEL identity, then a Nodal profile in Neon", never a second
authentication account; the browser holds Nodal's `__Host-nodal_session`
cookie, not a provider token; personal data is sealed (`internal/pii`,
AES-256-GCM under a keyring the database never holds, F-47); the Nodal user
is `users` (identity) + `user_profiles` (product state, onboarding as
timestamps, D-053) + `compliance_profiles` (eligibility) + payout identity —
none of which grants a financial privilege by existing. Supabase was rejected
because a second authenticator is a second source of truth for who a person
is (ADR-0022 "Options considered"). Operators come only from `operator_roles`
(ADR-0024), bootstrapped by a declared subject at next login and never
rewritable by the application (F-175, 00799).

## 4 · Complete user flow

Sign up (ZITADEL hosted login) → profile → terms (the served documents, each
saying it is a draft pending counsel) → Home (Credits panel, markets, agents,
activity) → Buy Credits (Stripe Payment Element; on a tier without a key the
page says the provider is unavailable) → Markets → a market's ticket (quote →
order under an idempotency key; the fill, not the quote, is what the ticket
shows) → Portfolio (positions, cost basis, unrealised/realised, "as of") →
Activity (one feed, eighteen kinds) → Agents (strategy → compile → agent at an
authority level; higher levels disabled by policy and shown as such) →
Withdraw (eligibility with reasons; verification through a provider-hosted
flow — the sandbox provider on a sandbox tier; a destination held as a token;
a quote; a conversion request) → the request stops at the governance boundary:
on a sandbox tier a sandbox provider settles it after ten seconds and **no
value moves**; on any other tier `PAYOUT_RESERVE`/`PAYOUT_SETTLE` are not
ACTIVE and the request is refused with the capability's own words.
`docs/product/USER_JOURNEY.md` is the screen-by-screen version; scenarios A–J
in `apps/web/e2e/scenarios/` are the runnable one. Class: `STATIC_PROOF` +
`LIVE_OBSERVED` locally (154 Playwright cases); live staging *(pending)*.

## 5 · Credits

One Credit asset per deployment, six decimal places, every amount an exact
integer of base units (`numeric(38,0)`; no float on the path, enforced by
`TestNoFloatingPointInSource` and `lintfin`). Three books: the journal
(authoritative), provenance lots with eleven origins and five finality states
(authoritative), read models (derived, trigger-maintained, reconciled).
Balances are never one number: gross, spendable, frozen, reversed,
payout-eligible, ineligible, each with reasons. `DefaultPolicy` permits no
origin to leave; `SandboxPolicy` (sandbox tier only) permits `PURCHASED` and
the five earning origins at `PAYOUT_KYC` and refuses every grant. The
productization audit found the unit defect that made $10 buy 0.001 Credits
(F-151, P0) before any purchase was ever made against a provider.
`docs/product/CREDIT_ECONOMY.md`. Class: `STATIC_PROOF`.

## 6 · Markets

Nodal-native internal assets on an off-chain constant-product market
`(V+R)·Y ≥ K` that Nodal operates; discovery with search, sort and filters;
prints, candles, holders (shape only — rank, quantity, share; no identity,
F-197); market safety as a versioned policy (impact, slippage, creator rule,
breaker); a demo catalogue on sandbox tiers, labelled simulated everywhere it
appears. No external market, no on-chain asset, no order book. Class:
`STATIC_PROOF` (`internal/nativemarket`, ADR-0027).

## 7 · Trading

A quote prices a hypothetical trade and never prices execution; the customer's
protection is `min_output`. Execution re-reads and locks the market, re-prices,
runs the risk kernel (the two §47 concentration limits from the GLOBAL ∧
ACCOUNT policy), the market-safety policy, the value-domain check (a native
trade is a declared conversion under `NATIVE_MARKET_TRADING`), consumes lots
in the structural order (promotional before purchased before earned, spendable
finality only), and records the fill with the state version it produced
(F-195). A market whose price cannot be measured cannot open (F-193, D-108).
Class: `STATIC_PROOF`.

## 8 · Portfolio

Positions are derived read models — the sum of a holder's fills — maintained
by database triggers and reconciled against the journal
(`cp_native_positions_unreconciled()`). Per position: quantity, average cost,
market value at the last print, unrealised (market value − cost basis) and
realised (on sells, against the average cost); totals carry an "as of"
instant; every figure a string at the asset's own scale. The Credits half is
§5's buckets. Class: `STATIC_PROOF` (ADR-0027, `GET /v1/me/portfolio`).

## 9 · Agents

Seven authority levels are declared (`internal/agentauthority`: 0 research
only, 1 recommendation, 2 prepared transaction, 3 rule-based execution within
the user's limits, 4 bounded discretion, 5 autonomous selection, 6 autonomous
portfolio). This build supports up to level 3; levels 4–6 are disabled by
policy, each naming the capability gate it would need, and the product shows
them disabled with that sentence. An agent binds only to a strategy version
its owner owns and accepted (F-187, D-105); the runtime evaluator is
deliberately without a caller (F-65) and the agent page says so. **What the
audits exposed and what closed it (`4814887`):** no route accepted a version
and no deployment had a compiler, so "Create Agent" could not complete
anywhere (F-255, F-256), and the seeded risk policy permitted no venue, so
even a model-backed compiler could never have produced a version (F-257).
Now: `POST /v1/strategies/{id}/versions/{n}/accept` (owner-only, step-up,
the IR hash echoed, `strategy.version.accepted` on the audit stream —
D-128); a sandbox-tier **structured** compiler (`internal/provider/
compilersandbox`, D-129) that accepts one declared grammar — universe,
entry/exit as a price threshold or an interval, risk limits, capital limit,
frequency, PAPER mode — refuses unknown fields, names every missing one
(`STRUCTURED_CONSTRAINTS_REQUIRED`), interprets no prose, calls no model,
and is refused in PROD at construction, by configuration and by the schema
(`STRUCTURED_SANDBOX` lineage, `sandbox`/`environment` on the version); a
review screen showing the rendered strategy before acceptance (§18); Scenario
D end to end in Playwright (describe → compile → review → accept → create at
level 1 and 3 → pause → resume → disable). "Inspect decisions" stays the
honest empty state on every tier of this build because the runtime is not
deployed (D-130). Residual: `strategy_versions.status` is application-written
with no transition table (F-258). Class: `STATIC_PROOF` + `LIVE_OBSERVED`
(local).

## 10 · Purchases — Stripe sandbox evidence

No PaymentIntent, test-mode or live, was created by this goal. The tree holds
no Stripe secret key; the only Stripe value in it is the **publishable test
key** in `render.yaml` (`pk_test_…`, D-078), and the sandbox API used for
every local run had no key at all (scenario B reaches the provider-unavailable
branch, by design). The Stripe adapter's contract tests run against recorded
fixtures (`make contract`, green). Nodal's objects in the Stripe dashboard are
namespaced (§27) and the productization goal added none. Class: `STATIC_PROOF`;
a sandbox purchase on STAGING is *(pending)* the deploy.

## 11 · Verification

Ten states on `compliance_profiles.identity_state` (`UNVERIFIED → REQUIRED →
STARTED → PENDING → VERIFIED`, with `NEEDS_INFORMATION`, `REJECTED`,
`RESTRICTED`, `EXPIRED`, `SUSPENDED`); nothing reaches VERIFIED except from a
provider decision, a lifted restriction or a restored suspension. Levels are
earned from evidence: `NONE → NODAL_IDENTITY → PAYOUT_KYC` (four passed checks:
document, age, jurisdiction, sanctions) `→ ENHANCED`. The sanctions screen
rides the transition row (F-168, 00796). The second audit found the edge set
was enforced in Go only — `cp_app` could reach VERIFIED in one INSERT (F-227,
P1) — and migration 00806 now holds the legal edges in two tables the apply
functions consult (`AD001` on an edge not in them), paired with the Go tables
by the enum suite (D-121). `docs/product/VERIFICATION_AND_WITHDRAWAL.md` §4.

## 12 · Withdrawals

The software path is complete and reachable end to end on a sandbox tier:
eligibility (reasons per origin, with provenance) → destination (a provider
token, never an account number; the heuristic refuses account, card and seed
shapes) → quote (fee, net, minimum, expiry, sandbox label) → conversion
request (`payout_requests`, ADR-0026) → the provider. Kill switches reach the
path (F-163, D-092). **The exact external blockers**: a licensed payout
provider under contract (B-01/B-05), counsel's answer on the Credit float and
on withdrawable gains (B-02), Stripe's restricted-business determination
(B-09), the 1099 election and KYB (B-11). The second audit's four P1s — a
sub-minimum payout with the quote skipped, a provider told no amount or
destination, an open sanctions review that stopped nothing, and the transition
tables above — are fixed and merged (`8a1b701`, F-224–F-227, D-119–D-121): the
quote is required and its minimum is a domain fact, the provider is told the
net amount and the destination from the recorded quote, and the sanctions
screen, restrictions and jurisdiction block inside the reserving transaction.
Earned Credits now settle when what paid for them settles (F-230, D-124,
00809), so the conversion request is reachable for the origins the sandbox
policy permits. **Round two of the area's audit did not flatten:** eleven
findings, four P1 again, six of them in round one's remediation — above all
that a derived lot inherited finality but not origin, so a promotional grant
could be traded into withdrawable proceeds (§23's forbidden pattern), and
that the pool's FIFO draw-down handed a reversible purchase an earlier
contributor's settled provenance. `fix/withdrawal-2` is merged (`e27e674`):
a derived lot now carries an **origin floor** — the most restricted origin
among everything that funded it — and the payout policy must permit both the
lot's origin and its floor (D-131), so a promotional grant's proceeds can
never leave under any policy in this build, and the pool is drawn down
worst-first so no seller receives provenance better than the pool's worst
outstanding contribution (D-132). The cost, stated: on a seeded tier every
earning carries a PROMOTIONAL floor, so the sandbox journey ends at an honest
refusal naming that floor rather than at a settled rehearsal payout; a real
settled purchase is what would change it. A third audit round is running
*(pending)*.

## 13 · Conversion

The boundary is the value domain: `INTERNAL_CREDIT` becomes `PAYOUT_PENDING`
only through a conversion request under an ACTIVE `PAYOUT_RESERVE` and a
permitting origin policy, and `EXTERNAL_SETTLED` only when a licensed provider
says so under `PAYOUT_SETTLE`. Verification changes eligibility, never the
domain: a verified account holds the same closed-loop Credits it held before,
and KYC never retroactively turns old Credits into money. Reservations are of
specific units from specific lots, so a cancel returns exactly what was
taken. Class: `STATIC_PROOF` (`internal/valuedomain`, `test/security/valueboundary_test.go`).

## 14 · Providers

| Role | Real | Sandbox | Mocked | Unselected |
|---|---|---|---|---|
| A · acquiring (Credits) | — | Stripe, test mode, no key on any deployment this goal ran | `stripecredit` contract fixtures | — |
| B · identity verification | — | `sandbox_verification` (decides nothing on its own; the tester chooses the outcome, labelled) | — | a vendor (Veriff/Persona/Sumsub ranked in `PROVIDER_BOUNDARY.md` §5; B-06) |
| C · conversion/payout | — | `sandbox_payout` (settles after ten seconds, moves nothing) | `payouttest.Sandbox` | a licensed provider (Tilia/Thunes, Stripe Connect ranked; B-01/B-05) |
| Settlement asset | — | Solana **devnet** USDC | — | a mainnet mint (BLOCKED_EXTERNAL 7) |

Every sandbox provider refuses `EnvProd` at construction; every registry
refuses a provider with no contract reference outside a sandbox-allowed build.

## 15 · UI/UX

41 page components, 34 routes (D-077), the public site plus the app in one
static Vite/React 19 build; a design system (`docs/product/UI_UX_SYSTEM.md`)
with money as strings only, three temperatures (`economy` / `real` /
`simulated`), refusals rendered by reason with a recovery, no dead control,
375 px without horizontal scroll (the reflow sweep), axe on every route, a
focus move on navigation (F-211), an error boundary (F-218), and the markets
and agents areas as lazy chunks (main chunk 160 kB gzipped against a 180 kB
budget). The forbidden vocabulary (§59/§60) is a test (`honesty.spec.ts`).

## 16 · Chrome evidence

**Locally:** the merged Playwright suite against a sandbox-tier API on a fresh
database with demo markets — 154 passed, 4 skipped with stated reasons at
`60de57d` — and the browser end-to-end auditor's twelve journeys
(`audit-journey.spec.ts`, `audit-surface.spec.ts`, branch `audit/e2e-browser`):
public site, sign-up, terms, the sandbox line, Buy Credits' no-key branch, a
buy and a sell with the balance moving by the exact amount, portfolio and
activity with and without the stream, agents with levels 4–6 disabled by name,
Withdraw's explanation, verification through the sandbox provider, 375 px on
eight routes, sign-out — all proven; the conversion request itself is not
reachable until earned Credits can settle (F-e2e-1 = F-wv-3, on
`fix/withdrawal`). The deployed CSP was applied to the real bundle for the
first time and produced no violation.
**Live staging:** *not yet observed* — the deployment waits on the human queue;
this session's Chrome was used only to read the Stripe dashboard's publishable
key and GitHub's billing page. Class: `LIVE_OBSERVED` (local),
`BLOCKED_EXTERNAL` (staging).

## 17 · Security findings

Opened by this goal: **F-134–F-223, ninety findings** (1 P0, 12 P1, 40 P2, 37
P3), all FIXED with regression tests, from eight adversarial audits by agents
that had not written the area. Wave B: withdrawal-verification 11 (4 P1) →
`fix/withdrawal`; docs-vs-reality 14 (6 P2) → `fix/docs`; browser end-to-end
6 (1 P1, the same defect as the withdrawal audit's F-wv-3, and 5 P3) → two
fixed on `productization` (F-251, F-252), one to each fix branch (F-250,
F-253), one duplicate, one closed by the auditor's own test landing. Both
wave-B fix branches are merged (F-224–F-250, F-253); the agents gap is closed
(F-255–F-257, F-258 a residual). The withdrawal area's second round found 11
(4 P1, 4 P2, 3 P3; `F-wv2-n`, register numbers F-259–F-269 on
`fix/withdrawal-2`) and did **not** flatten; the third round is next
*(pending)*. Current open: the wave-B findings until their branches merge
*(pending)*. The register: `docs/audit/AUDIT_FINDINGS.md`; the P0/P1 list:
`docs/audit/LAUNCH_GATE_MATRIX.md` "What the productization wave changed".

## 18 · Financial invariants

Every journal transaction balances; `ledger_balances` is verified against the
journal by `VerifyInternal` every five minutes and a violation reaches the
alert webhook as a SEV1; provenance lots reconcile to `CREDIT_BALANCE`
(`VerifyProvenance`); positions reconcile to fills
(`cp_native_positions_unreconciled()` = 0); reservations reconcile
(`VerifyReservations`); the demo catalogue's Credits can be spent and can
never leave (`TestIntegration_DemoCreditsCanBeSpentAndCanNeverLeave`); the
value-boundary suite proves no conversion happens outside a gated request.
Results: green in the full integration run (58 packages) and
`make integration-race` (12 packages). Class: `LIVE_OBSERVED`.

## 19 · Restore

`make restore-drill` after every merge: the latest at migration **00818 —
OK, 160 tables, row counts identical, 0 accounts with balance drift, journal
hashes equal, one live state change on the restored database, 12.1 s**
(`docs/operations/BACKUP_RESTORE.md`, held to the migration head by
`TestDocs_CountsMatchTheCode`). Class: `LIVE_OBSERVED`.

## 20 · Test matrix

| Tier | Result |
|---|---|
| `make lint` | 0 issues |
| `go test ./...` | pass |
| full integration (`scripts/inttest`) | 58 packages, all passed, 15m42s |
| `make contract` | pass |
| `make race` | 10 packages, exit 0 |
| `make integration-race` | 12 packages, all passed |
| `make fuzz` (15 s/target) | 29 targets, 0 failed |
| `make e2e` (provisioned database) | 14 passed, 0 skipped |
| `make chaos` | pass; 2 host-specific skips (archive endpoint, broker) |
| web typecheck / unit / build | clean / 141 / 160 kB gzipped |
| Playwright, merged tree, sandbox API | 154 passed / 3 skipped |
| CI on GitHub | **could not run** — Actions minutes exhausted (queue item 7) |

Commits per run: `docs/audit/PRODUCTION_EVIDENCE_INDEX.md` §3. *(pending:
the same tiers after the wave-B merges.)*

## 21 · Live staging

Deployed commit: **the pre-productization build** (`build_version: dev` —
the commit stamp is D-117, not yet deployed); config hash
`e1ad81b6917dcc72c2851646f388011b6e1170aca1f288f81b7860106c241a39`;
`healthz` and `readyz` 200. The productization build reaches STAGING when the
human completes queue items 1–3b. Class: `LIVE_OBSERVED` (current),
`BLOCKED_EXTERNAL` (target).

## 22 · $0 cost

Render: one free web service and one free static site (both `plan: free` /
static); Neon free tier; ZITADEL free tier; GitHub Free (Actions minutes spent,
not paid for); ntfy.sh free topic for alerts; no AWS resource created
(`infra/` unchanged by this goal: `git log --stat 9906c9f..HEAD -- infra/` is
empty); no observability vendor. The one cost-shaped thing this goal
declined: raising the Actions spending limit (queue item 7). Class:
`STATIC_PROOF` for the tree; the account billing pages are the human's to
confirm.

## 23 · BLOCKED_EXTERNAL

`docs/audit/LAUNCH_GATE_MATRIX.md` rows 1–9: Stripe production approval,
legal review, provider contract, risk approval, security approval, an
independent penetration test, a mainnet settlement mint, the human queue
(secrets, push, CNAME, operator subject), and GitHub Actions minutes.

## 24 · Launch flags

```
SOFTWARE_COMPLETE          = false   (this report's headline; see §25/§26 for what closes it)
STRIPE_PRODUCTION_APPROVED = false   (BLOCKED_EXTERNAL 1)
LEGAL_APPROVED             = false   (BLOCKED_EXTERNAL 2; every served document says it is a draft)
PENTEST_COMPLETE           = false   (BLOCKED_EXTERNAL 6; the adversarial audits are not one and do not claim to be)
LIVE_READY                 = false   (the conjunction, plus the three-principal ceremony nobody has run)
```

## 25 · Remaining human actions

`docs/build/HUMAN_ACTIONS_QUEUE.md`, in order: (1–2) the two Render secrets;
(3) push `main`; (3b) merge PR #1 once Claude has merged the wave-B branches;
(4) the `app-nodal` CNAME; (5) delete the stale migrate DSN from the
dashboard; (6) the first operator's ZITADEL subject; (7) decide how CI gets
its minutes back. Nothing else needs a person.

## 26 · Resume here

*(pending — the exact next step after productization is written when the
wave-B branches have merged; until then the "RESUME HERE" note in
`docs/build/MASTER_BUILD_STATE.md` is current to the hour.)*
