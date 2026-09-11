# User journey

Status: **specification during the productization goal** (2026-09-10). This is
the journey the product implements, screen by screen, with the API each step
calls and the state the user is in. It is written against the domain design
in `PRODUCT_ARCHITECTURE.md`; before `SOFTWARE_COMPLETE` is set again it is
reconciled against the deployed staging site and every step is marked with
its Chrome evidence in `STAGING_E2E.md`.

Vocabulary the whole product uses: **Credits** (internal platform value; never
"balance in your account", never "deposit"), **Buy Credits** (never "add funds"),
**Withdraw** (a request that a licensed provider may settle; never "cash out"),
**verified** (a level, not a badge of approval), **sandbox** (anything a sandbox
tier produced, labelled everywhere). Each forbidden phrase is written here on one
line so a grep can find it: a case-insensitive search of `docs/product/` and
`apps/web/src` for any of them should return this sentence and nothing else.

## 0. The public site (unauthenticated)

`app-nodal.actorvia.xyz/` — Product · Markets · Agents · How it works ·
Security · Learn · **Sign in** · **Get started**.

- The hero says what Nodal is in the closed-loop product's own terms: buy
  Credits, trade Nodal-native assets, delegate bounded authority to agents,
  request withdrawal once verified. No claim of being an exchange, broker,
  bank, insured, regulated, approved, or of live crypto execution.
- "How it works" is the four steps below with the honest caveat on each.
- "Security" states what is true: OIDC sign-in with step-up, sealed personal
  data, deny-by-default authorization, append-only ledgers, provider-hosted
  verification, no custody.
- Markets and Agents pages are read-only previews of live sandbox data with
  the sandbox label visible.
- Footer: Terms, Privacy, Risk disclosure, Status, Contact.

## 1. Sign up and sign in (Scenario A)

| Step | Screen | Calls | State after |
|---|---|---|---|
| Get started | `/get-started` explains the two things that will happen: an identity is created with the identity provider, and a Nodal profile. One button. | `GET /v1/auth/login` (302 to ZITADEL) | — |
| Identity | ZITADEL hosted page: create account, verify e-mail, MFA if the provider requires it. Nodal never sees the password. | — | ZITADEL identity |
| Callback | `/auth/callback` → API sets `__Host-nodal_session` → redirects to `CP_AUTH_POST_LOGIN_URL` + return path | `GET /v1/auth/callback` | session; `GET /v1/me` → `profile: null`, `onboarding.complete: false` with `next_step: PROFILE` |
| Onboarding 1 | `/welcome`: display name (and optional handle), locale/timezone prefilled | `POST /v1/me/profile` | profile exists |
| Onboarding 2 | `/welcome/terms`: the current Terms, Privacy and Risk disclosure, each with a version; the two acknowledgements the product needs (Credits are internal and not withdrawable until eligible; the sandbox/internal-economy risk statement) | `POST /v1/me/terms-acceptances` | acceptances recorded, audited |
| Onboarding 3 | `/welcome/done`: what the dashboard shows and the three actions; "Explore markets" / "Buy Credits" / "Go to dashboard" | — | `onboarding.complete: true` with a `completed_at`, every entry in `onboarding.steps` complete, and no `next_step` |

Target: 60–90 seconds from landing to dashboard. Returning users skip 1–3.
Any later visit with an unaccepted new document version routes to the terms
step before anything financial.

## 2. The shell

Desktop: left navigation **Home · Markets · Agents · Portfolio · Activity**;
top bar: search, notifications (unread count from `GET
/v1/me/notifications/unread-count`, live via `/v1/events/stream`), profile
menu (Settings, Security, Sign out). Primary actions **Buy Credits** and
**Withdraw** are always visible; Withdraw is never hidden from an unverified
user — it opens the explanation (§7).

Mobile (≤ 768): bottom navigation with the same five destinations; primary
actions in the Home header; sheets instead of dialogs.

Every financial figure loads through a skeleton and is never shown as a
placeholder number; a figure carries its temperature (`economy | real |
simulated`) and an "as of" time.

## 3. Home dashboard

Modules, in order: **Credits** (`GET /v1/credits/balance`: gross, spendable,
frozen; payout-eligible shown separately with the sentence that it is not
USD), **Portfolio value** and **today's change** only when positions exist
(`GET /v1/me/portfolio`), **Holdings** (top positions), **Active agents**
(`GET /v1/agents`), **Markets** (movers from `GET /v1/native-markets`),
**Recent activity** (`GET /v1/me/activity?limit=5`). Empty states: "Explore
markets", "Create your first agent", "Your activity will appear here",
"Verify when you're ready to request withdrawals".

## 4. Buy Credits (Scenario B)

`/buy-credits` (page on mobile, dialog on desktop): presets $10 / $25 / $50 /
$100 and a custom amount within `GET /v1/credits/pricing` bounds; the line
"USD payment → Credits received" with the exact quantity; the sentence
"Credits are internal platform value and are not immediately
cash-withdrawable."

1. `POST /v1/payments` (Idempotency-Key) → PaymentIntent, `client_secret`
   once → Stripe Elements (sandbox: test cards).
2. Confirm in Stripe → webhook `POST /v1/webhooks/stripe_credit` → funding
   state `CAPTURED` → Credits minted `REVERSIBLE` → notification
   `CREDIT_PURCHASE_CAPTURED` → the balance on Home updates through the event
   stream, never by optimistic arithmetic.
3. States the page must render: pending provider, requires action (3-D
   Secure), failed with the provider's category (never the raw exception),
   succeeded-awaiting-webhook ("Balance updating"), captured.

The path passes the capacity guard, the `CREDIT_PURCHASE` capability (SANDBOX
state on staging; the page shows the sandbox label) and the legal router; if
any refuses, the page shows the refusal with the reason and the next step, not
a generic error.

## 5. Markets and trading (Scenario C)

- `/markets`: list with search, sort (volume, 24h change, liquidity, newest),
  filters; each row: name, symbol, last price, 24h change with sign glyph,
  volume, liquidity, sandbox/demo label. `GET /v1/native-markets`.
- `/markets/:marketId`: chart (candles `GET .../candles?interval=`), reserves
  and spot, recent trades, the safety limits in force, the asset's
  description and creator, and the **ticket**: buy/sell, amount in Credits or
  quantity, quote (`POST .../quotes` — a quote never prices execution and says
  so; it shows price impact, fee, minimum received, expiry), then
  `POST .../orders` (Idempotency-Key). Rejections rendered by reason:
  insufficient Credits, price moved beyond tolerance, market paused, limit
  exceeded, capability inactive.
- After a fill: position and Credits update via the stream; the ticket shows
  the fill, not the quote.

## 6. Portfolio and activity

- `/portfolio`: Credits breakdown, positions table (quantity, average cost,
  market value, unrealised, realised; strings, tabular figures), totals with
  "as of"; `GET /v1/me/portfolio`. Export as CSV from the existing account
  export.
- `/activity`: one feed, filter by kind, cursor paging; each item links to its
  object; `GET /v1/me/activity`.

## 7. Withdraw (Scenarios E, F)

`/withdraw` exists for everyone.

- Unverified (`GET /v1/me/eligibility` → `REQUIRES_VERIFICATION`): the page
  explains what withdrawal is, that it requires identity verification and an
  approved payout destination, what verification asks for, and offers "Start
  verification". There is no other path; the API refuses a payout request at
  this level with the same reason.
- Verification (`/verify`): `GET /v1/me/verification` shows the state and the
  checks; "Start" (`POST /v1/me/verification/sessions`) hands off to the
  provider-hosted flow. On the sandbox tier the sandbox provider's page is
  clearly labelled and lets the tester choose the outcome; the result comes
  back through the provider callback, never from the browser. States
  rendered: required, started, needs information, verified, rejected,
  restricted, suspended, with the next action for each.
- Verified: choose or add a payout destination (`/v1/me/payout-destinations`;
  tokens only, masked display), enter an amount within the eligible bucket
  (`GET /v1/me/eligibility` per origin with provenance), get a quote (`POST
  /v1/payouts/quote`; fees, sandbox label, expiry), submit (`POST
  /v1/payouts`), then follow the request through `PROVIDER_PENDING` to
  `SETTLED` (`GET /v1/payouts/{id}`; notifications `PAYOUT_ACCEPTED`,
  `PAYOUT_SETTLED`). On the sandbox tier the sandbox provider settles after
  ten seconds and no value moves; the page says so.

Refunds, disputes and reversals (Scenarios G, H) surface as `CREDIT_REVERSAL`
activity and a notification; a frozen bucket is shown as frozen with the
reason.

## 8. Agents (Scenario D)

- `/agents`: list; `/agents/new`: describe a strategy in words or structured
  constraints (`POST /v1/strategies`), compile (`POST
  /v1/strategies/{id}/compile`; on a tier without a compiler backend the
  attempt says `COMPILER_UNAVAILABLE` and the page says exactly that),
  review the compiled version, then create the agent with an authority level
  (1 observe · 2 propose · 3 execute within limits; 4–6 shown as disabled by
  policy), a Credits budget, per-trade cap, allowed assets, daily loss stop.
- `/agents/:id`: status, limits, budget used, runtime status (on staging:
  evaluator not deployed — stated), decisions/runs when they exist, pause /
  resume / disable.

## 9. Settings and security

`/settings`: profile, locale, notification preferences (in-app only, stated),
terms versions accepted; `/settings/security`: sessions with revoke, last
step-up, sign-in method strength, the user's own audit trail (`GET
/v1/me/audit`); `/settings/account`: status, restrictions with reasons, close
request with cooling-off.

## 10. Session expiry mid-action (Scenario J)

Any 401 during a sensitive action (quote → order, payout submit, agent
enable) keeps the form state in memory, routes to sign-in with a return path,
and on return re-validates (a new quote, a new eligibility read) before
allowing the submit. Idempotency keys are minted at confirm time, so a retry
after re-authentication cannot double-submit.

## 11. Errors the product must speak

Provider unavailable · Payment pending · Payment failed · Balance updating ·
Trade rejected · Insufficient Credits · Price changed · Market paused ·
Account restricted · Verification pending · Verification rejected ·
Withdrawal unavailable · Payout delayed · Session expired · Network offline.
Each has a fixed sentence, a recovery action and, where the API gives one, the
reason code; never a raw provider or server message.
