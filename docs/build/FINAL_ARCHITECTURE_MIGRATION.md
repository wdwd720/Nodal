# FINAL ARCHITECTURE MIGRATION

How the repository at baseline `b8da0c4` becomes the architecture `gola.md` describes, what has been
done, and what has not. Required by PART V.

Companion documents: `CURRENT_SYSTEM_INVENTORY.md` (what was there), `KEEP_MODIFY_REPLACE_MATRIX.md`
(what happens to each subsystem), `docs/audit/INDEPENDENT_AUDIT.md` (what was found wrong).

---

## 1. The shape of the migration

The baseline implemented a USD-native Solana spot-trading and financial-agent control plane. Measured
against `gola.md` that is Domain B (simulated capital) and Domain C (real capital). Domain A — the
Nodal-native economy — did not exist.

The migration is therefore **additive at the domain level and surgical at the core**: the financial
core is extended with a value-domain type system that everything else then obeys, and Domain A is
built on top of it. Nothing in the existing Solana/agent/settlement stack is rewritten.

```
                          ┌─────────────────────────────────────────┐
                          │           internal/valuedomain          │
                          │  8 domains · 6 rails · 11 credit origins│
                          │  funding finality · versioned policy    │
                          │  isolation matrix (Go + SQL)            │
                          └────────────────┬────────────────────────┘
                                           │ every balance is typed
        ┌──────────────────────────────────┼──────────────────────────────────┐
        │                                  │                                  │
┌───────▼────────┐              ┌──────────▼──────────┐            ┌──────────▼─────────┐
│  DOMAIN A      │              │  DOMAIN B           │            │  DOMAIN C          │
│  NEW           │              │  EXISTING, KEPT     │            │  EXISTING, KEPT    │
├────────────────┤              ├─────────────────────┤            ├────────────────────┤
│ credit         │              │ reality             │            │ funding (Stripe)   │
│ nativeasset    │              │ prediction          │            │ withdrawal         │
│ nativemarket   │              │ strategy + IR       │            │ instruments        │
│ payout         │              │ agent               │            │ execution, quote   │
│ (commerce)     │              │ backtest/shadow     │            │ signing + inspect  │
└───────┬────────┘              └──────────┬──────────┘            │ chain, wallet      │
        │                                  │                       │ provider adapters  │
        │                                  │                       └──────────┬─────────┘
        └──────────────────────────────────┼──────────────────────────────────┘
                                           │
                          ┌────────────────▼────────────────────────┐
                          │  SHARED FINANCIAL CORE — kept, extended │
                          │  ledger · capital · idempotency · event │
                          │  gates · killswitch · risk · eligibility│
                          │  proof · audit · reconciliation         │
                          └─────────────────────────────────────────┘
```

## 2. What has landed

| Stage | Subject | State | Where |
|---|---|---|---|
| 0 | Baseline freeze, inventory, KEEP/MODIFY matrix | **done** | `CURRENT_SYSTEM_INVENTORY.md`, `KEEP_MODIFY_REPLACE_MATRIX.md` |
| 1 | Independent adversarial audit | **done, ongoing** | `docs/audit/INDEPENDENT_AUDIT.md` |
| 2 | ValueDomain / CapitalRail / provenance | **done** | `internal/valuedomain`, migration 00710 |
| 3 | Accounting hardening | **partial** | domain isolation in the ledger; reservations pre-existing and verified |
| 4 | Credit ledger + funding lifecycle | **done** | `internal/credit`, migration 00711 |
| 5 | Native asset registry + moderation | **done** | `internal/nativeasset`, migration 00712 |
| 6 | Native market engine | **done** | `internal/nativemarket`, migration 00712 |
| 7 | Market surveillance | **done** | `internal/nativemarket/surveillance.go` |
| 8 | Internal commerce / creator economy | **not started** | — |
| 9 | Payout eligibility + provider architecture | **done** | `internal/payout`, migration 00713 |
| 10 | Hosted partner rail | **not started** | — |
| 11 | Self-custodial onchain rail | **kept as-is**, re-classified | `internal/signing`, `internal/chain`, `internal/provider/*` |
| 12 | Rails unified behind FinancialIntent | **not started** | — |
| 13 | LegalCapabilityRouter / composite gates | **not started** | — |
| 14 | Agent authority levels | **not started** | — |
| 15 | Reality / Prediction / Proof integration | **pre-existing**, not extended to Domain A | — |
| 16 | Frontend migration | **not started** | — |
| 17 | Admin / operations tooling for Domain A | **not started** | — |
| 18 | Infrastructure / IAM hardening | **pre-existing**, audited | — |
| 19 | Property testing / fuzzing | **partial** | curve fuzzer, value-domain exhaustive property, credit torture test |
| 20–24 | Chaos, load, provider sandbox, re-audit, launch package | **not started** | — |

## 3. Architectural decisions this migration made

Each of these is a decision that could reasonably have gone another way, recorded so a future reader
knows it was a choice.

### D-A01 — Value domains are enforced in PostgreSQL, not only in Go

The claim "Credits are closed-loop" is a claim about what the ledger contains, and the ledger is a
database. Migration 00710 puts structural isolation in a deferred trigger, and the integration tests
prove it by writing a properly balanced Credits-to-SOL swap by hand through the migration role — the
schema owner — and watching it be refused five different ways.

Capability activation stays in Go, and the migration says why rather than pretending: a gate is keyed
by `(capability, environment)` and a database connection carries no environment the application could
not simply assert. Adding a session GUC would look like a control and be none.

### D-A02 — A cross-domain transaction must NAME its conversion

Direction matters. The first draft accepted a domain *pair* if a conversion existed in either
direction, which let the deliberately ungated payout-return path authorise the gated payout-reserve
path. Conversions are now directional, and a transaction spanning two domains must declare which one
it is performing — so a cross-domain movement is a stated intent in the journal rather than an
emergent property of which accounts were involved.

### D-A03 — Constant product with a virtual Credit reserve, not a bonding curve

PART XIV requires one model, chosen on stated criteria. Both are deterministic and exactly
integral; constant product answers "how much can I buy with 100 Credits" with one division, where a
linear bonding curve needs an integer square root. The product asks that question far more often than
the other one. Full reasoning in `internal/nativemarket/curve.go`.

Rounding is always in the pool's favour (ceiling division on the derived reserve), so K is
non-decreasing and no trade sequence can extract value that was not paid in. Fees round **down**, so
they can never consume an entire trade.

### D-A04 — Credits are held in provenance lots, consumed most-restricted-first

Payout eligibility is a property of units, not of a balance. Consumption order is structural
(promotional → purchased → earned) rather than "least payout-eligible first", because eligibility
comes from a versioned policy and ordering by it would make a spend replay differently after a policy
change. It also happens to be user-favourable: ordinary spending burns grants and leaves earnings
intact.

### D-A05 — The market reserve is a control account with a subsidiary ledger

Every market quotes against the same Credit asset, and the ledger keys a platform account by
`(code, asset)`, so all markets share one `MARKET_RESERVE`. Rather than invent a second owner type,
per-market attribution lives in `native_market_state` and `VerifyReserves` asserts the sum
reconciles with the ledger balance. This is the standard control-account pattern and it produces a
checkable invariant rather than a segregation that would have to be maintained.

### D-A06 — Surveillance alerts, it does not halt

An AMM has no order book, so self-trade prevention has nothing to prevent and front-running has no
queue to jump; the package says so rather than shipping stubs. What is detected — rapid round
tripping, creator self-dealing, concentration, anomalous volume — raises alerts. A heuristic that
halts a market is a denial-of-service vector against creators. Blocking is reserved for states an
operator or the asset's lifecycle chose, which the database enforces on every fill.

### D-A07 — A payout that cannot be recorded goes to MANUAL_REVIEW

When a provider settles and Nodal cannot post it, returning an error would leave the request in
SUBMITTED with value reserved and no record of why. It is parked for a human in its own transaction,
with the reason attached. `PAYOUT_STATUS_UNKNOWN` cannot transition back to SUBMITTED at all: the
state machine does not offer the move that would pay someone twice.

### D-A08 — Four account codes were retired before they were used

Migration 00710 sketched the internal-economy chart before the postings existed. Writing them settled
it: a native-market trade's counterparty is the platform pool, which is on our books, so it needs no
contra accounts, and `PLATFORM_FEE_RECEIVABLE` is already per-asset. `NATIVE_TRADING_OUTFLOW`,
`NATIVE_TRADING_INFLOW`, `CREDIT_FEES`, `PLATFORM_CREDIT_REVENUE` and `CREDIT_LIABILITY` were
removed. An account that exists and is always empty is worse than one that does not.

## 4. Changes made to pre-existing subsystems

Deliberately few, and each one is a widening rather than a redefinition:

| Subsystem | Change | Why |
|---|---|---|
| `internal/ledger` | `value_domain` on accounts; `Conversion` on `Posting`; 8 new account codes; 9 new transaction kinds; `CapabilityResolver` | Domain A posts through the same journal. Content hashes computed before 00710 are unchanged — the conversion is *omitted* from the canonical form when absent rather than emitted as null — so the existing audit chain still verifies. |
| `internal/assets` | `ValueDomain` field, required for held assets; `CREDIT` and `NATIVE_ASSET` kinds; `nodal-internal` chain sentinel | A native asset IS a registry entry, so there is one identity from draft to delisting. |
| migration 00603 binding | `cp_require_transition` takes an optional id-column name; the flag accumulates instead of overwriting | It read `NEW.id` and stored one state per entity, so it could not guard a table keyed by anything else and could not express two transitions in one transaction. Creating and launching an asset does exactly that. Both widenings are backward compatible. |
| `Makefile` | `integration` runs `scripts/inttest` | The old target ran one package while claiming to run the suite. See AUD-001. |
| `test/security` | cursor fixture selects by owner | It could not reach its own assertion. See AUD-002. |

## 5. What is deliberately NOT done

Stated so that absence is not mistaken for oversight:

- **No hosted-partner rail.** PART XXIII's interface is not written, because writing an adapter
  against an unverified API is what PART XVIII forbids. The rail is declared in
  `valuedomain.CapitalRail` and `Implemented()` returns true for it, which is currently generous —
  see BLOCKERS.
- **No live payout provider.** The interface, sandbox, registry guard and contract requirements
  exist; connectivity is `BLOCKED_EXTERNAL`.
- **No frontend for Domain A.** The API surface has to exist first.
- **No agent authority levels.** Stage 14.
- **Levels 4–6 of agent authority and every real-money capability remain DISABLED** in code default,
  which is the state PART LXIII requires of a fresh deployment.
