# Staging end-to-end scenarios

Status: **specification and run log during the productization goal**
(2026-09-10). The scenarios are the goal's (§45). Each has an automated
Playwright spec against a production build of `apps/web` and a real `api`
binary on a real database, and a browser run against the deployed staging
site recorded in the evidence table at the end. A scenario is done only when
both exist.

## How the automated suite runs

- `apps/web/playwright.config.ts`: a production build served by `vite
  preview`, a real API at `CP_WEB_API_TARGET`, Chrome (`channel: chrome`),
  one worker, nothing stubbed. Auth setup signs in through the real OIDC
  flow (dev identity provider locally).
- Local recipe (the CI job `web-e2e` in `.github/workflows/ci.yml` is the
  reference): provision a database with `go run ./scripts/testdb -name <n>
  -export`, run `scripts/seed`, `scripts/seedeconomy`, `scripts/riskpolicy`,
  start `cmd/api` as a **sandbox tier** (`CP_ENV=dev`,
  `CP_API_LEGAL_POLICY=SANDBOX`, `CP_API_PAYOUT_POLICY=SANDBOX`,
  `CP_API_ENABLED_CAPABILITIES` and `CP_API_SANDBOX_GATES` listing
  `CREDIT_PURCHASE,NATIVE_ASSET_CREATION,NATIVE_MARKET_TRADING,MARKETPLACE,PAYOUT_RESERVE,PAYOUT_SETTLE`,
  `CP_PROVIDER_PAYOUT_NAME=sandbox_payout`, fake or sandbox Stripe), then
  `pnpm --filter @controlplane/web e2e`.
- Files: `apps/web/e2e/scenarios/<letter>-<name>.spec.ts`, one per
  scenario; cross-cutting: `e2e/accessibility.spec.ts` (axe on every route,
  375 px no-overflow), `e2e/honesty.spec.ts` (forbidden phrasing, sandbox
  labels, no Credit figure beside a currency figure), `e2e/controls.spec.ts`
  (no dead control anywhere).
- No two suites run against the same database at once (goal §55);
  `CP_INTTEST_DB_SUFFIX` keeps parallel checkouts apart.

## The scenarios

| # | Name | Path | Proves | Must not happen |
|---|---|---|---|---|
| A | New user | landing → Get started → identity → callback → `/welcome` → terms → `/welcome/done` → `/home` | signup, profile, acceptance recorded, dashboard renders with empty states | frontloaded KYC; more than three onboarding screens |
| B | Buy Credits (sandbox) | `/home` → Buy Credits → `POST /v1/payments` → Stripe sandbox test card → webhook → funding `CAPTURED` → balance updates via the stream | the full purchase path through the capacity guard, the `CREDIT_PURCHASE` gate (SANDBOX) and the ledger; sandbox label shown | a real charge; balance changed by arithmetic in the browser |
| C | Trade | `/markets` → market → quote → buy → position → sell → `/portfolio` updated | quote never prices execution; fill updates position and Credits; P&L strings; activity rows | a float anywhere; a stale balance shown as current |
| D | Agent | `/agents/new` → strategy → compile (or `COMPILER_UNAVAILABLE`, stated) → review → create at level 1 → inspect | honest runtime state; levels 4–6 disabled | an execution path; a fabricated compile |
| E | Withdrawal, unverified | `/withdraw` → `REQUIRES_VERIFICATION` explanation → "Start verification"; direct `POST /v1/payouts` refused with the same reason | the gate cannot be bypassed from the UI or the API | hiding Withdraw; a payout at `NODAL_IDENTITY` |
| F | Verified sandbox identity | `/verify` → sandbox provider (labelled) → outcome VERIFIED via callback → `/withdraw` eligible per origin → destination (token) → quote → request → `PROVIDER_PENDING` → `SETTLED` after 10 s | eligibility calculation, provenance, provider boundary, no value moves | a real provider; a KYC result written by the browser |
| G | Refund after spending | purchase → trade → refund webhook → reversal → frozen/negative handling shown | ledger reversal semantics surface honestly | Credits vanishing without an activity row |
| H | Dispute/chargeback after trading | purchase → trade → dispute webhook → frozen bucket, restricted state, reconciliation record | the account sees the freeze and its reason | hidden freeze |
| I | Concurrent trade/purchase | two browsers: purchase webhook lands during a trade | idempotency and serialisation; balances agree with the ledger after both | double mint; double fill |
| J | Session expiry mid-action | quote → session revoked → submit → sign-in with return → re-quote → submit with the same key | no double submit; state kept | a submit against a stale quote |

## Browser run against staging (goal §44, §56)

Recorded per scenario when the deployed site is verified in Chrome: date,
build commit from `/v1/version`, config hash, viewport(s), what was clicked,
console state, network state, screenshot path under `docs/audit/evidence/`.

| Scenario | Date | Commit | Viewports | Result | Evidence |
|---|---|---|---|---|---|
| A–J | — | — | — | not yet run | — |
