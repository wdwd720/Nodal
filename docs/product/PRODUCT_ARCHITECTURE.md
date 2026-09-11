# Product architecture

Status: **living document, written during the productization goal** (2026-09-10).
It describes the product as the architecture defines it, and marks each
surface with its build state. It is reconciled against the code and the
deployed staging environment before `SOFTWARE_COMPLETE` is set again; until
then a surface marked *in progress* is a design, not a claim.

Companion documents: `USER_JOURNEY.md` (what a person does, screen by screen),
`CREDIT_ECONOMY.md` (how value enters, moves and is marked),
`VERIFICATION_AND_WITHDRAWAL.md` (the state machine and the provider
boundary), `UI_UX_SYSTEM.md` (the design system), `PROVIDER_BOUNDARY.md` (what
Nodal never does itself), `STAGING_E2E.md` (the scenarios and how they are run).

## 1. What Nodal is, in one paragraph

Nodal is a control plane between a person's capital and financial markets. In
its first product it is a **closed-loop economy**: a person buys Credits with a
card, uses them to trade Nodal-native internal assets on an off-chain market
Nodal operates, delegates bounded authority to agents, and — once verified and
only through a licensed payout provider under an approved capability — asks
for value to leave. Nodal is not an exchange, a broker, a bank or a custodian;
Credits are internal platform value; nothing in this architecture converts
value to money by itself.

## 2. The shape of the system

```
browser (apps/web, static, app-nodal.actorvia.xyz)
   │  same-site cookie session (__Host-nodal_session), CSRF, CORS allowlist
   ▼
control plane API (cmd/api, Go, Render free web service, api-nodal.actorvia.xyz)
   │  OIDC relying party ──► ZITADEL (authenticates; roles asserted, never stored twice)
   │  problem+json, Idempotency-Key on every command, deny-by-default authz
   ▼
PostgreSQL (Neon, free tier; roles cp_migrate / cp_app / cp_readonly / cp_ops)
   │  checksum-verified migrations; state changes only through transition
   │  functions and triggers (F-42); ledgers append-only
   ▼
providers, each behind a contract and a capability gate:
   acquiring (Stripe, sandbox mode outside PROD) ── identity (provider-hosted;
   sandbox provider on a sandbox tier) ── conversion/payout (licensed provider
   in PROD only; sandbox provider on a sandbox tier)
```

One process serves everything on the free tier: the periodic passes that
would be workers elsewhere (ledger verification, escalation sweeps, the
notification follower, the sandbox payout settlement) run inside `cmd/api`,
bounded and single-flight. There is no Redis, no queue, no second instance
and no blockchain in the internal product (goal §61).

## 3. Tiers and the sandbox tier

| Tier | `CP_ENV` | Providers | Legal policy | What can happen |
|---|---|---|---|---|
| LOCAL / TEST | local, test | fake | development | everything, against fakes |
| DEV | dev | sandbox | development | the economy, no payout |
| **STAGING (sandbox tier)** | staging | sandbox | **SANDBOX** (ADR-0023) | the whole journey: Credits, markets, verification, a payout that settles without moving value |
| PROD | prod | live | CONSERVATIVE | only what the gates and the legal policy permit — today, simulation |

A **sandbox tier** is any non-PROD deployment that declares
`CP_API_LEGAL_POLICY=SANDBOX`; configuration refuses that in PROD. On it, a
capability gate may be in the `SANDBOX` state — active for that deployment
only, never on the path to `ACTIVE`, carrying no approval and saying so in
every API response (`sandbox: true`). PROD reaches `ACTIVE` only through the
unchanged ceremony: three principals, four evidence references, step-ups.

## 4. Domains

Each domain is a Go package with its own tables, its own transition table for
every state column, its own tests, and an HTTP surface generated from
`openapi/openapi.yaml`. Build state: **built** (audited at `024c691`),
**extended** (this goal, merged), **in progress** (this goal, on a branch).

| Domain | Package(s) | Owns | State |
|---|---|---|---|
| Identity & session | `internal/auth`, `internal/security`, `internal/pii` | ZITADEL relying party, Nodal sessions, sealed PII, step-up, roles | built |
| Profile & account lifecycle | `internal/profile`, `internal/terms` | `user_profiles`, terms acceptances, onboarding progress, self-service account state, admin support view | in progress (P) |
| Credits | `internal/credit`, `internal/valuedomain` | the Credit ledger (11 origins, 5 finality states), funding states, balance buckets (gross / spendable / frozen / payout-eligible / ineligible), the value-domain isolation | built; sandbox payout policy extended |
| Payments | `internal/credit` (`credit_fundings`), `internal/provider/stripecredit` | PaymentIntents, webhooks, capture → mint, reversals — no separate package: a PaymentIntent lives beside the mint it causes | built |
| Native market | `internal/nativeasset`, `internal/nativemarket`, `internal/risk` | native assets and their lifecycle, the off-chain CPMM `(V+R)·Y ≥ K`, quotes that never price execution, the risk gate, safety limits, positions & P&L, price history & candles, discovery | built; positions/P&L/candles/safety/demo in progress (M) |
| Activity | `internal/activity` | one feed over the domain tables, fixed kinds | in progress (M) |
| Verification | `internal/verification`, `internal/eligibility`, `internal/provider/verifysandbox` | `compliance_profiles` state machine, evidence rows (identity, age, jurisdiction, sanctions), levels `NONE → NODAL_IDENTITY → PAYOUT_KYC → ENHANCED`, eligibility reasons | in progress (V) |
| Conversion & payout | `internal/payout`, `internal/provider/payoutsandbox`, registry | `payout_requests` (the conversion request), destinations (tokens only), quotes, provenance, the provider contract | built; destinations/quote/provenance in progress (V) |
| Agents | `internal/agents`, `internal/strategy` | strategies, compile attempts, agents with authority 1–3, lifecycle, limits; runtime deliberately without a caller (F-65) | tables built; surface in progress (A2) |
| Notifications & realtime | `internal/notifications`, SSE at `/v1/events/stream` | in-transaction notifications, preferences, per-user event stream | in progress (N) |
| Governance | `internal/gates`, `internal/killswitch`, `internal/legalrouter`, settlement compiler | capability gates (8 states incl. SANDBOX), kill switches, legal policies, the compiler that turns a request into permitted/denied/requires-verification | built; sandbox tier extended |
| Operations | `internal/alert`, `internal/reconciliation`, `internal/audit` | webhook alerts, ledger verification, escalation sweeps, audit streams, restore drill | built |

## 5. The three temperatures of a number

Every amount the product shows carries one of three temperatures, and the
API and the UI never mix them in one figure:

- **economy** — internal Credits and Credit-denominated values (balances,
  positions, P&L); never insured, never USD;
- **real** — money that touched a provider: a card charge in USD, a payout a
  provider reported settled;
- **simulated / sandbox** — anything a sandbox tier produced: sandbox
  Credits, sandbox verification outcomes, sandbox payouts. Labelled at the
  row, in the API (`sandbox: true`) and in the UI (`data-temp="simulated"`).

## 6. Value never converts inside Nodal

`internal/valuedomain` keeps `INTERNAL_CREDIT`, `PAYOUT_PENDING` and
`EXTERNAL_SETTLED` apart; a Credit becomes payout-pending only through a payout
request under an active `PAYOUT_RESERVE` capability and a permitting policy,
and settled only when a licensed provider says so. Verification changes
*eligibility*, never the value domain: a verified account holds the same
closed-loop Credits it held before. The ordering is fixed in the code and
recorded in `PROVIDER_BOUNDARY.md` and `VERIFICATION_AND_WITHDRAWAL.md`.

## 7. Security posture of the product surface

- Deny-by-default authorization per operation (`internal/httpapi/authz.go`);
  every new operation must be mapped or the invariants test fails.
- Every command idempotent by `Idempotency-Key`; every state change through a
  transition function that writes its own history; `cp_app` cannot UPDATE a
  state column.
- Personal data sealed with AES-256-GCM under a versioned keyring the
  database never holds; the profile stores only what the user chooses to
  show.
- Same-site session cookie; CSRF and CORS allowlists derived from one config
  list; HSTS with preload on the apex; CSP on the static site with
  `connect-src` limited to the API origin.
- Rate limits stated per class (general, auth, quote, command); capacity
  ceilings on money at risk; kill switches independent of the gates.
- Secrets exist only in the Render dashboard; the blueprint carries no value
  for them; `/v1/version` publishes the SHA-256 of the non-secret
  configuration so a deployment can be matched to its blueprint.

## 8. Frontend

`apps/web` (Vite, React 19, TanStack Query, react-router) is a static site
that talks only to the API origin. Money is strings end to end and a
source-scanning test bans numeric coercion of money, raw buttons, stray
`fetch` calls and decimal literals in UI files. The generated client
(`packages/generated-client`) is regenerated from `openapi.yaml` and CI
refuses drift. The design system is in `UI_UX_SYSTEM.md`; the public site,
onboarding, shell, dashboard, markets, trading, portfolio, activity, Buy
Credits, Withdraw, verification, settings, notifications and agents pages are
built against the live API in STAGING and verified in a browser (§44, §56).

## 9. What is deliberately absent

- No second authentication system and no account-linking problem (ADR-0022).
- No conversion, custody, brokerage or exchange function; no payout without a
  licensed provider and an approved capability (`PROVIDER_BOUNDARY.md`).
- No blockchain for internal trading; the settlement asset configuration
  belongs to the hosted rail, not to Credits.
- No agent execution path until the runtime has a caller under F-65; agents
  at authority levels 4–6 are refused by policy.
- No paid infrastructure: one free web service, one free static site, one free
  database, free identity and free alert delivery (goal §42).
