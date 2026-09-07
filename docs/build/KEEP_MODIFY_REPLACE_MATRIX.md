# KEEP / MODIFY / REPLACE MATRIX

Classification of every major subsystem at baseline `b8da0c4`, against the final architecture in
`gola.md`. Required by PART V.

Legend: **KEEP** (correct and sufficient) · **KEEP_AND_HARDEN** (correct, needs additional controls or
tests) · **MODIFY** (structure survives, contract changes) · **REPLACE** · **DEPRECATE** · **REMOVE** ·
**UNKNOWN** (audit has not reached it).

Nothing is classified REPLACE or REMOVE in this matrix. The existing engineering is sound where it
exists; the gap is absence, not wrongness. That conclusion is itself an audit finding, and Stage 1
tries to falsify it.

---

## Financial core

| Subsystem | Class | Reason / required change |
|---|---|---|
| `internal/money` | **KEEP** | Integer USD + big.Int `Quantity` with explicit scale, 7 rounding modes, a source-grep test forbidding floats. Satisfies PART X. Credits and native assets reuse it unchanged. |
| `internal/ledger` | **MODIFY** | Double-entry journal with DB-trigger balance enforcement and immutability is exactly what PART X requires — keep the machinery. Must gain: a `value_domain` on accounts, a DB-enforced **domain-isolation** invariant (a journal transaction may not mix INTERNAL_CREDIT with HOSTED_FIAT etc. except through an explicit approved conversion account), and new account codes for Credits, native-asset inventory, market reserves, creator earnings and payout settlement. |
| `internal/assets` | **MODIFY** | Registry is chain/mint-shaped: identity is `(chain, mint_address)`, `kind` is a Solana enum, `decimals ≤ 18`. Nodal-native assets and Credits have no chain and no mint. Add `value_domain`, admit non-chain kinds, and relax identity so an internal asset is identified without a mint. Existing rows and the transition table stay. |
| `internal/capital` (reservations) | **KEEP_AND_HARDEN** | Reservation model with TTL and consumption is the right primitive for PART XXXVI. Needs the 100-concurrent-spend torture test *against Credits*, and reservation of native-asset inventory. |
| `internal/idempotency` | **KEEP_AND_HARDEN** | Add the PART X rule "same key + different semantic request MUST reject" as an explicit test if not already proven. |
| `internal/positions`, `valuation` | **KEEP** | FIFO lots with basis conservation. Native-asset positions reuse it. |
| `internal/funding` | **MODIFY** | Deposit lifecycle exists but is USD-deposit-shaped. PART XI needs an 11-state `CreditFundingState` and the four distinct amounts (credited / spendable / settled / payout-eligible). Generalise the provider interface away from Stripe specifics. |
| `internal/withdrawal` | **MODIFY** | Destinations + velocity limits are reusable, but PART XVIII–XXI require a separate `PayoutRequest` with a 12-state machine, a `PayoutEligibilityEngine` and provenance consumption. Withdrawal becomes one payout rail, not the payout system. |

## Policy, safety, evidence

| Subsystem | Class | Reason / required change |
|---|---|---|
| `internal/gates` | **MODIFY** | 7-state gate with five-condition activation and dual control is the right authority mechanism (PART LXIV). The capability *key* is currently a bare enum of 10 names; PART LXIV requires a composite key (jurisdiction, provider, rail, product, asset, authority level, value origin, payout mode, compensation model, verification level). Extend the key, keep the state machine. |
| `internal/killswitch` | **KEEP** | Kind × action-class matrix already stops new risk without stopping reconciliation (PART XXXII). |
| `internal/risk` | **KEEP_AND_HARDEN** | Deterministic, versioned, hashed inputs. Add native-market concentration and creator-concentration limits. |
| `internal/eligibility` | **KEEP_AND_HARDEN** | Already separate from risk per PART XXXIII. Add value-provenance and payout inputs. |
| `internal/compliance` | **MODIFY** | 248 LOC, 1 test — the thinnest financial package in the tree. Needs a real `ComplianceDecision` object to sit beside Eligibility and Risk. |
| `internal/proof`, `audit` | **KEEP_AND_HARDEN** | Hash chaining, Merkle, KMS signing, checkpoints. Extend evidence capture to Credit, native-market and payout actions. |
| `internal/event` | **KEEP** | Outbox + inbox dedupe + per-key ordering. PART XXXIX satisfied; new domains publish through it. |
| `internal/reconciliation` | **KEEP_AND_HARDEN** | Engine exists. Needs a reconciliation source per new provider category (credit purchase, payout, native market engine). |
| `internal/security`, `auth` | **KEEP** | RBAC, tenant scoping, step-up, OIDC+PKCE. PART XLVII needs `NodalIdentity` to be explicitly distinct from financial verification — a new type, not a change to these. |
| `internal/admin`, `adminplane` | **KEEP_AND_HARDEN** | Dual control exists. PART XLIX needs added workflows: freeze market, close-only market, disable asset, market investigation. |

## Rails and providers

| Subsystem | Class | Reason / required change |
|---|---|---|
| `internal/settlement` | **MODIFY** | The Settlement Compiler is the declared moat and its planner/executor/step model is right. It currently compiles to one rail shape. PART XXVI + XXII require it to dispatch on `CapitalRail` ∈ {SIMULATED, NATIVE_INTERNAL, HOSTED_PARTNER, SELF_CUSTODIAL_ONCHAIN}. Add rails; do not rewrite. |
| `internal/intent` | **MODIFY** | `FinancialIntent` exists. PART XXV requires added fields (`capital_domain`, `maximum_debit`, `minimum_receive`, `source_prediction`) and new action types for native assets, internal services and payout. |
| `internal/signing` + `inspect` | **KEEP** | Independent transaction decoding is exactly PART XXIV's "no blind signing". 2,110 LOC of inspector with 1,474 LOC of tests. Preserve as the self-custodial rail. |
| `internal/provider/{jupiter,solanarpc,helius,privy}` | **KEEP** | Self-custodial onchain rail. Demoted from "the product" to "one rail" — a documentation and routing change, not a code change. |
| `internal/provider/stripe` | **KEEP_AND_HARDEN** | Becomes one implementation of `CreditPurchaseProvider`, behind the generalised interface. |
| Hosted partner rail (`HostedFinancialProvider`) | **ABSENT — BUILD** | PART XXIII. No adapter exists for any hosted provider. |
| Payout providers | **ABSENT — BUILD** | PART XVIII. Interface + sandbox fake + contract tests + capability gate; live connectivity `BLOCKED_EXTERNAL`. |
| `internal/instruments` | **KEEP_AND_HARDEN** | External instrument registry per PART XXXV. 593 LOC with 1 test function — under-tested for its authority. |

## Intelligence

| Subsystem | Class | Reason / required change |
|---|---|---|
| `internal/reality` | **KEEP** | Point-in-time provenance, no lookahead (PART XLII). |
| `internal/prediction` | **KEEP** | Prediction Ledger (PART XLIII). |
| `internal/strategy` + `ir` | **KEEP_AND_HARDEN** | Typed IR with 73 tests on the IR alone. PART XXX needs rail restrictions and forbidden-instrument sets if absent. |
| `internal/agent` | **MODIFY** | Runtime exists; `AgentAuthorityLevel` 0–6 (PART XXVIII) does not. Add the level as a first-class, gate-controlled property with levels 4–6 disabled. |

## Absent — must be built (Domain A and payout)

| Subsystem | Class | Part |
|---|---|---|
| `ValueDomain` / `CapitalRail` / value provenance lots | **ABSENT — BUILD** | IX, XXII |
| Nodal Credit ledger + credit funding lifecycle | **ABSENT — BUILD** | XI, XII |
| Nodal-native asset registry + creation + moderation | **ABSENT — BUILD** | XIII, LI |
| Native market engine (one model, production quality) | **ABSENT — BUILD** | XIV, XV |
| Market integrity / surveillance | **ABSENT — BUILD** | XVI |
| Internal commerce / creator economy | **ABSENT — BUILD** | XVII |
| `PayoutEligibilityEngine`, `PayoutRequest`, `PayoutProvider` | **ABSENT — BUILD** | XVIII–XXI |
| `LegalCapabilityRouter` | **ABSENT — BUILD** | XXVII |
| Frontend for Domain A + value-domain-honest home | **ABSENT — BUILD** | LII–LVI |

## Interfaces

| Subsystem | Class | Reason |
|---|---|---|
| `apps/web`, `apps/admin` | **MODIFY** | 76 TS files against 41 REST paths. Must gain the Domain A surfaces and, critically, the PART LII rule that Nodal Economy / Simulated / Real Capital are never summed. |
| OpenAPI contract | **MODIFY** | Add Domain A commands; keep REST command semantics (PART LVII forbids GraphQL for money movement — the repo already complies). |
| Terraform / CI | **KEEP_AND_HARDEN** | Present; audited in Stage 1 for the claims made about it. |

## UNKNOWN at time of writing

`internal/httpapi` authorisation completeness, `internal/execution` crash-recovery behaviour, the
integration/chaos/security suites' actual (as opposed to claimed) coverage, and Terraform validity are
marked **UNKNOWN** until Stage 1 executes them. They are resolved in `docs/audit/INDEPENDENT_AUDIT.md`.
