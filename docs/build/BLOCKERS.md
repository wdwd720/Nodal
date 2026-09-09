# BLOCKERS

Things that cannot be completed from inside this repository, and exactly what would unblock each.

The bar is the one `gola.md` PART IV sets. `BLOCKED_EXTERNAL` is only for facts that genuinely require
a human, a provider or counsel. Missing code, tests, migrations, runbooks, mocks, contract tests,
chaos tests and admin tools are **not** blockers — they are work, and they are tracked in
`MASTER_BUILD_STATE.md` instead.

Every entry states what the repository already does, so that when the external fact arrives the
remaining work is activation rather than construction.

---

## B-01 — No payout provider contract exists · BLOCKED_EXTERNAL

**Decision needed.** Which licensed provider will actually send value to a user, under what
commercial terms, for which destination types and currencies.

**Why external.** A provider contract is signed by people. No amount of engineering produces one, and
PART XVIII explicitly forbids implementing an adapter against an unverified API — building one from
guessed endpoint names would be worse than having none, because it would look finished.

**What the code already supports.**
- `internal/payout.Provider` — the interface, shaped around what any payout provider must do (accept
  a request under a key we chose, say later what happened to that key, describe what it supports)
  rather than around one vendor's request body.
- `internal/payout.Registry` — refuses a provider with no `ContractReference` unless built with
  sandboxes allowed, which production never is. Refuses any provider that cannot answer a lookup,
  because a timed-out submission to one would be permanently ambiguous.
- `internal/payout/payouttest.Sandbox` — a faithful fake, including the inconvenient behaviours: the
  same key is the same payout, and a submission can succeed and lose its response on demand.
- The full request lifecycle, reservation, provenance consumption, settlement, failure return,
  `PAYOUT_STATUS_UNKNOWN` and reconciliation are implemented and tested against that sandbox.

**Evidence required to close.** A signed agreement naming the provider, the destination types, the
currencies, the jurisdictions, and the provider's own statement of idempotency and lookup semantics.

**Capabilities currently disabled.** `PAYOUT_RESERVE`, `PAYOUT_SETTLE`.

---

## B-02 — Whether Credits or trading proceeds may be paid out at all · BLOCKED_EXTERNAL

**Decision needed.** For each `CreditOrigin`, in each jurisdiction: may value of that provenance
leave the system, and under what conditions.

**Why external.** This is a legal determination about the economic substance of the internal economy.
`gola.md` PART IX is explicit that the mapping is policy, that counsel may approve a different one,
and that the code must not hardcode a business truth.

**What the code already supports.**
- `valuedomain.Policy` — versioned, hashed, covering every origin explicitly. `Validate` refuses a
  policy that omits an origin, that permits payout without naming a capability gate, or that permits
  payout below `PAYOUT_KYC` verification.
- `valuedomain.DefaultPolicy` — every origin forbidden. A fresh deployment pays nobody out, and the
  integration tests assert that even a fully verified user with settled funds and every capability
  active is refused under it.
- `payout.Engine` — decides per unit and records the policy version and hash on the decision, so a
  decision made under one policy can still be explained after two more have replaced it.

**Evidence required to close.** A written determination per origin and jurisdiction, recorded as a
policy version with an approval reference.

---

## B-03 — Whether pre-KYC participation in native markets is permissible · BLOCKED_EXTERNAL

**Decision needed.** May a user trade Nodal-native assets before any financial identity verification,
and in which jurisdictions.

**Why external.** PART XIX states the target experience (KYC at exit) and immediately warns against
assuming it is available. It is a money-transmission and consumer-protection question.

**What the code already supports.** `legalrouter` keys a capability on jurisdiction, product, value
origin, agent authority and verification level together, so the answer can be as narrow as the
determination is. The shipped `ConservativePolicy` denies native-market trading everywhere and
permits only simulation, which is the correct starting position rather than a placeholder.

**Capabilities currently disabled.** `NATIVE_MARKET_TRADING`, `NATIVE_ASSET_CREATION`.

---

## B-04 — Credit purchase provider production credentials · BLOCKED_EXTERNAL

**Decision needed.** Live credentials and a production account with the payment provider used to sell
Credits.

**What the code already supports.** `internal/credit` implements the eleven-state funding lifecycle,
mints on `CAPTURED → REVERSIBLE`, promotes to settled only when the funding settles, and handles
chargeback with a recorded `DEFICIT` rather than a negative balance. `internal/provider/stripe`
exists and is contract-tested against recorded fixtures.

**Capabilities currently disabled.** `CREDIT_PURCHASE`.

---

## B-05 — Hosted partner rail: no provider selected · BLOCKED_EXTERNAL

**Decision needed.** Which licensed partner carries hosted customer accounts.

**Why this is not merely unbuilt.** An adapter cannot be written against an unverified API, and the
partner's capability set (does it support hosted USD, hosted crypto, statements, tax artifacts,
webhooks) is a contractual fact rather than an engineering choice.

**What the code already supports.** `valuedomain.RailHostedPartner` is declared with its authority
model — the provider is authoritative for balances and Nodal holds a mirror — and the ledger's
value-domain isolation already refuses to let hosted value share a transaction with internal Credits.

**A defect this document found and closed.** `RailHostedPartner.Implemented()` returned `true` while
no adapter existed. Writing this entry is what surfaced it. It now returns `false`, so the Settlement
Compiler cannot route to a rail with nothing behind it — PART XXII's "do not implement live
unsupported products merely because an interface exists", enforced rather than intended.

---

## B-06 — Financial identity verification provider · BLOCKED_EXTERNAL

**Decision needed.** Who performs KYC, and whether their verification is accepted by the payout
provider (the two are not the same question).

**What the code already supports.** `valuedomain.VerificationLevel` separates Nodal identity from
financial identity from enhanced diligence. `payout.EligibilityInput` takes the level as an input and
`Decision.VerificationWouldSuffice` distinguishes "you cannot" from "you have not verified yet", so
the product has something useful to say the moment a provider exists.

**Current state.** `cmd/api` resolves the verification level Nodal can establish BY ITSELF and no
level above it: `NODAL_IDENTITY` when the identity provider asserted a verified email address
(persisted as `users.email_hash`), otherwise NONE. `PAYOUT_KYC` and `ENHANCED` are unreachable and
the payout engine refuses accordingly, which is what this blocker actually blocks.

It used to resolve NOTHING — every account `VerificationNone` — on the reasoning that a deployment
that cannot establish identity has not established it. That is right about a KYC provider and wrong
about this rung: `NODAL_IDENTITY` is "a verified email address and/or passkey", which this system
establishes at login. Every Domain A action requires exactly that level, so the over-wide reading
made the entire internal economy unreachable in every deployment (F-26). A blocker that blocks more
than it should is not conservative; it hides working software behind an external decision that has
nothing to do with it.

A passkey-only account with no email address still reports NONE, because only the email half is
recorded. That is under-reporting in the same direction and is named rather than hidden.

---

## B-07 — Age and jurisdiction matrix · BLOCKED_EXTERNAL

**Decision needed.** The per-jurisdiction age policy and the list of jurisdictions in which each
product may operate.

**What the code already supports.** `nativeasset.PolicyProfile.MinimumAge` defaults to 18 and is
frozen at activation; `legalrouter` carries jurisdiction as a first-class key dimension and matches
comma-separated sets, so a determination covering three states is one rule.

---

## B-08 — Independent security review and penetration test · BLOCKED_EXTERNAL

**Why external.** A review by the team that wrote the code is not an independent review.

**What the code already supports.** `docs/threat-model/THREAT_MODEL.md`, the `test/security` suite
(cross-tenant probes, forged sessions, SQL-source constancy analysis, agent escalation, webhook
forgery), `gosec`, `govulncheck`, `gitleaks` and a supply-chain script, all runnable today.

---

## Not blockers

Recorded because their absence might otherwise look like one:

| Item | Status | Where it is tracked |
|---|---|---|
| Internal commerce / creator economy | not built | `MASTER_BUILD_STATE.md` Stage 8 |
| Hosted partner rail adapter | not built (and see B-05) | Stage 10 |
| Rails unified behind FinancialIntent | not built | Stage 12 |
| Frontend for Domain A | not built | Stage 16 |
| Admin tooling for Domain A | not built | Stage 17 |
| Chaos, load, restore drill for the new subsystems | not run | Stages 20–21 |
| Terraform for the new tables | not needed (schema is migration-managed) | — |
| Retention of SOCIAL_DATA, MODEL_IO and OPERATIONAL_LOG | declared in config, enforced by nothing | `AUDIT_FINDINGS.md` F-79; `MASTER_BUILD_STATE.md` §4 |
| The ten transition bindings that still compare the destination only | `agents` is fixed (D-044); the rest are not | `AUDIT_FINDINGS.md` F-78; `MASTER_BUILD_STATE.md` §4 |
| 131 enum CHECK constraints with no Go list compared against them | named individually by `test/integration/enums` | `AUDIT_FINDINGS.md` F-74 |

**Checkpoint 2026-09-08.** The F-71..F-81 batch added **no** external blockers.
Every item it left undone is work, and each is named above or in
`MASTER_BUILD_STATE.md` §4 with the reason it was left. `CP_DATABASE_OPS_URL`
(D-045) is a new deployment input, not a blocker: it is a database role the
repository already creates, and the local default is in `.env.example`.
