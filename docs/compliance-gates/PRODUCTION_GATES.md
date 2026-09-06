# PRODUCTION CAPABILITY GATES

Status: design fixed 2026-09-05; software enforcement implemented in `internal/gates` (see `docs/build/REQUIREMENTS_TRACEABILITY.md` for the verification state of each row). This document is the operator-facing contract: what each gate means, what evidence it requires, who may approve it, and the current state in every environment.

## 1. Principle

External legal and commercial questions become software-enforced gates. A capability is ACTIVE only when **all** of the following hold at the moment of evaluation:

1. the deployment configuration lists the capability as enabled for the environment;
2. the persisted gate row for `(capability, environment)` is in state `ACTIVE`;
3. the row is inside its validity window (`effective_at <= now < expires_at`) and not revoked;
4. every required evidence reference is present and non-empty;
5. the approval chain contains at least two distinct approvers, neither of whom is the proposer.

No single environment variable, feature flag, database edit, or operator can activate a capability alone. Agents (`ActorType = AGENT`) are rejected before any gate query. Fresh deployments persist a `DISABLED` row for every capability at startup (`gates.Bootstrap`).

## 2. Capabilities

| Capability | Meaning when ACTIVE | High-risk | Required evidence refs | External blockers |
|---|---|---|---|---|
| `LIVE_FUNDING` | real fiat-to-USDC onramp sessions may be created and settled into customer wallets | yes | legal review, provider contract (Stripe onramp), risk approval, security approval | EB-001, EB-003, EB-004 |
| `LIVE_MANUAL_TRADING` | user-initiated Solana spot trades may be signed and submitted with real capital | yes | legal review, provider contracts (wallet/signing, Jupiter, Helius), risk approval, security approval, custody analysis | EB-001, EB-002, EB-005, EB-006, EB-010, EB-011 |
| `LIVE_AGENT_TRADING` | autonomous strategies in CANARY/LIMITED/LIVE stages may create intents that reach signing | yes | everything above plus adviser/CTA analysis; strategy promotion evidence per agent | EB-001, EB-002, EB-005, EB-007 |
| `WITHDRAWALS` | customers may move value off the platform path | yes | legal review, off-ramp/custody partner contract, risk approval, security approval | EB-015, EB-002 |
| `SOCIAL_DATA_PERSISTENCE` | social/news content may be stored beyond ephemeral processing | no | data-licensing review per source | EB-008 |
| `MARKETPLACE` | strategies may be listed for other customers | yes | legal review (adviser/CTA), risk approval | EB-007 |
| `CROSS_CHAIN` | plans may include CONVERT/TRANSFER/WAIT_FINALITY across chains | yes | provider contract (e.g. CCTP), risk approval, security approval | out of V1 |
| `PREDICTION_MARKETS` | event-outcome instruments tradable | yes | regulated integration and legal review | out of V1 |
| `SECURITIES` | equities or tokenised securities | yes | licensing | out of V1 |
| `CEX_TRADING` | centralised-exchange execution adapters | yes | provider contract, custody analysis | out of V1 |

High-risk gates additionally require that the activating principal differs from both the proposer and the first approver, and that each approver performed step-up authentication within 15 minutes of acting.

## 3. State machine

```
DISABLED ──propose──▶ PENDING_APPROVAL ──approve──▶ APPROVED ──activate──▶ ACTIVE
   ▲                        │                          │                    │
   │                        └──────── revoke ──────────┴──── revoke ────────┤
   │                                                                        ├─ suspend ──▶ SUSPENDED ──resume──▶ APPROVED
   └──────────────────────────── (new approval version) ◀── REVOKED / EXPIRED ◀── expire ──┘
```

| Transition | Permission | Step-up | Dual control | Speed |
|---|---|---|---|---|
| propose | `gate:propose` (RISK, COMPLIANCE, ADMIN) | yes | n/a | normal |
| approve | `gate:approve` (dual-control permission; BREAK_GLASS while valid) | yes | approver ≠ proposer | normal |
| activate | `gate:approve` | yes | activator ≠ proposer, and ≠ first approver for high-risk | normal |
| suspend | `kill:activate` (OPERATIONS, RISK, SECURITY, ADMIN) | no | none | **seconds** |
| resume | `gate:approve` | yes | distinct principal | normal |
| revoke | `gate:approve` | yes | none | normal |
| expire | system | n/a | n/a | automatic |

Every transition is recorded in `capability_gate_transitions` (actor, reason, approval id, evidence hash) and appended to the `admin` audit stream.

## 4. Environment defaults

| Capability | LOCAL | TEST | DEV | STAGING | PROD |
|---|---|---|---|---|---|
| all live/high-risk capabilities | DISABLED (fakes allowed for development flows) | DISABLED | DISABLED | DISABLED | **DISABLED** |
| SOCIAL_DATA_PERSISTENCE | DISABLED | DISABLED | DISABLED | DISABLED | DISABLED |

There is no environment in which a capability starts ACTIVE. Development uses fake providers, which the runtime accepts only in LOCAL/TEST/DEV and rejects programmatically in STAGING/PROD.

## 5. Current state (2026-09-05)

| Capability | PROD | STAGING | Evidence on file | Notes |
|---|---|---|---|---|
| LIVE_FUNDING | DISABLED | DISABLED | none | Stripe onramp adapter to be CODE_COMPLETE + CONTRACT_TESTED; commercial approval outstanding |
| LIVE_MANUAL_TRADING | DISABLED | DISABLED | none | wallet/signing provider unverified; licensing undetermined |
| LIVE_AGENT_TRADING | DISABLED | DISABLED | none | as above plus adviser/CTA analysis |
| WITHDRAWALS | DISABLED | DISABLED | none | no off-ramp/custody partner |
| SOCIAL_DATA_PERSISTENCE | DISABLED | DISABLED | none | licensing unknown |
| MARKETPLACE / CROSS_CHAIN / PREDICTION_MARKETS / SECURITIES / CEX_TRADING | DISABLED | DISABLED | none | out of V1 by definition |

Platform readiness: **NOT_READY**. Capital authority: **DISABLED**.

## 6. Evidence handling

Evidence references are opaque identifiers (document ids, contract ids, ticket ids) plus SHA-256 hashes of the evidence artefacts stored in the WORM archive. The gate stores hashes, never the documents. Approvers attest to the hashes they reviewed; the audit chain binds approver, hash, and time.

## 7. Relationship to kill switches

Gates authorise a capability; kill switches stop new risk within an authorised capability. Suspending a gate is the strongest kill for that capability and is deliberately fast. Re-activation after suspension always requires a fresh dual-controlled approval.

## 8. Related

`docs/architecture/POLICY_AUTHORITY.md` §1–2, `docs/adr/0016-production-capability-gating.md`, `docs/build/BLOCKERS.md` (EB-001…EB-017), migration `00150_capability_gates.sql`, package `internal/gates`.
