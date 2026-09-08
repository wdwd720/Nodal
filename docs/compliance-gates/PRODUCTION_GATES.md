# PRODUCTION CAPABILITY GATES

Status: design fixed 2026-09-05; software enforcement implemented in `internal/gates`; database enforcement added 2026-09-06 by migration `00701_capability_gate_state_authority.sql` (see §8, and §9 for what this document previously claimed and did not do). See `docs/build/REQUIREMENTS_TRACEABILITY.md` for the verification state of each row. This document is the operator-facing contract: what each gate means, what evidence it requires, who may approve it, and the current state in every environment.

## 1. Principle

External legal and commercial questions become software-enforced gates. A capability is ACTIVE only when **all** of the following hold at the moment of evaluation:

1. the deployment configuration lists the capability as enabled for the environment;
2. the persisted gate row for `(capability, environment)` is in state `ACTIVE`;
3. the row is inside its validity window (`effective_at <= now < expires_at`) and not revoked;
4. every required evidence reference is present and non-empty;
5. the approval chain contains at least two distinct approvers, neither of whom is the proposer.

No single environment variable, feature flag, or operator can activate a capability alone, and no principal can both propose and approve, or both approve and activate, one approval version. Agents (`ActorType = AGENT`) are rejected before any gate query.

Where each condition is enforced, and against whom:

| Condition | Enforced in `internal/gates` | Enforced in PostgreSQL |
|---|---|---|
| 1. deployment configuration enables the capability | `Checker.IsActive`, before any query | not knowable in the database; configuration alone still decides nothing, because 2–5 must also hold |
| 2. state is `ACTIVE` | `Evaluate`, `CanTransition` | `cp_gate_transition` is the only writer of `capability_gates.state`; `cp_app` holds no `UPDATE` privilege on it |
| 3. inside the validity window, not revoked | `Evaluate`, `Admin.Activate` | `cp_gate_transition` refuses an activation that is revoked or whose `expires_at` has passed; the window columns are not writable by `cp_app` |
| 4. required evidence present | `Evaluate`, `Proposal.normalize` | `cp_gate_transition` refuses a propose or activate of a high-risk capability with a missing reference (`cp_gate_is_high_risk` mirrors `gates.IsHighRisk`, asserted for every declared capability by `TestIntegration_GoAndSQLAgreeOnEveryCapabilitysRisk`; the two had diverged on `MARKETPLACE`, F-43) |
| 5. two distinct approvers, neither the proposer | `approveRule`, `activateRule`, `Evaluate` | `cp_gate_transition` appends each chain entry to the **stored** chain, requires the entry to name the acting principal, and refuses an activation whose chain does not hold two distinct approvers, neither of them the proposer |

The Go layer is the first line and produces the operator-facing errors; the database is the line that holds when the first one is bypassed. Both run — a condition is never dropped from one because the other also checks it.

Fresh deployments have no gate rows until `gates.Bootstrap` runs or the first proposal creates one; an absent row is `DISABLED` by evaluation (`Evaluate` fails closed on `nil`), and PostgreSQL refuses to create a gate row in any state but `DISABLED` with an empty approval chain, so "fresh deployment = everything disabled" (PART 244) holds whether or not the bootstrap has run. **`gates.Bootstrap` currently has no caller**: it is available to the API's startup path but not yet wired, so a fresh deployment's gates are absent rather than persisted as `DISABLED`. This is safe but not yet the persisted fact the design calls for.

## 2. Capabilities

| Capability | Meaning when ACTIVE | High-risk | Required evidence refs | External blockers |
|---|---|---|---|---|
| `LIVE_FUNDING` | real fiat-to-USDC onramp sessions may be created and settled into customer wallets | yes | legal review, provider contract (Stripe onramp), risk approval, security approval | EB-001, EB-003, EB-004 |
| `LIVE_MANUAL_TRADING` | user-initiated Solana spot trades may be signed and submitted with real capital | yes | legal review, provider contracts (wallet/signing, Jupiter, Helius), risk approval, security approval, custody analysis | EB-001, EB-002, EB-005, EB-006, EB-010, EB-011 |
| `LIVE_AGENT_TRADING` | autonomous strategies in CANARY/LIMITED/LIVE stages may create intents that reach signing | yes | everything above plus adviser/CTA analysis; strategy promotion evidence per agent | EB-001, EB-002, EB-005, EB-007 |
| `WITHDRAWALS` | customers may move value off the platform path | yes | legal review, off-ramp/custody partner contract, risk approval, security approval | EB-015, EB-002 |
| `SOCIAL_DATA_PERSISTENCE` | social/news content may be stored beyond ephemeral processing | no | data-licensing review per source | EB-008 |
| `MARKETPLACE` | strategies may be listed for other customers | **no** (see note) | legal review (adviser/CTA), risk approval | EB-007 |
| `CROSS_CHAIN` | plans may include CONVERT/TRANSFER/WAIT_FINALITY across chains | yes | provider contract (e.g. CCTP), risk approval, security approval | out of V1 |
| `PREDICTION_MARKETS` | event-outcome instruments tradable | yes | regulated integration and legal review | out of V1 |
| `SECURITIES` | equities or tokenised securities | yes | licensing | out of V1 |
| `CEX_TRADING` | centralised-exchange execution adapters | yes | provider contract, custody analysis | out of V1 |

**Every** gate — not only the high-risk ones — requires that the activating principal differs from both the proposer and the approver who put the gate into `APPROVED` (`gates.activateRule`, and condition 5 of §1, which demands two distinct approvers for all capabilities). What "high-risk" changes is condition 4: only a high-risk capability is refused a proposal or an activation for a missing evidence reference. Approve, resume and activate additionally require step-up authentication within 15 minutes of acting; propose, suspend and revoke do not (see §3).

> **CORRECTION (F-43) — `MARKETPLACE` IS high-risk, and this note said the opposite.** The paragraph here used to read "`MARKETPLACE` is not high-risk in code… it can be activated with [evidence] empty", and presented itself as a correction to an earlier table, which is the form a reader trusts most. It was wrong when it was written: F-16 had already moved `MARKETPLACE` to high risk in `gates.IsHighRisk`, for the reason recorded there — it gates the minting of the only withdrawable creator-earning provenance, so leaving it low risk would let one approver switch that on.
>
> What WAS true, and worse than the note claimed, is that the database did not agree. `cp_gate_is_high_risk` listed seventeen capabilities and Go listed eighteen, and the one they disagreed about was `MARKETPLACE` — so the database-side evidence requirement (`GT003`), which migration 00701 describes as "the line that holds when the Go check is bypassed", did not fire for it. Migration `00716` restores the parity, and `TestIntegration_GoAndSQLAgreeOnEveryCapabilitysRisk` drives both lists and compares them so the two copies cannot drift again.

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
| propose | `gate:propose` (RISK, COMPLIANCE, ADMIN) | no | n/a | normal |
| approve | `gate:approve` (dual-control permission; BREAK_GLASS while valid) | yes | approver ≠ proposer | normal |
| activate | `gate:approve` | yes | activator ≠ proposer **and** ≠ the approver who put the gate into `APPROVED`, for every capability | normal |
| suspend | `kill:activate` (OPERATIONS, RISK, SECURITY, ADMIN) | no | none | **seconds** |
| resume | `gate:approve` | yes | resumer ≠ proposer; the resumer may not then activate | normal |
| revoke | `gate:approve` | no (revocation reduces risk) | none | normal |
| expire | system | n/a | n/a | automatic |

Every transition is recorded in `capability_gate_transitions` (actor, reason, approval id, evidence hash) and appended to the `admin` audit stream. Since migration 00701 the row and its transition are written by one statement inside `cp_gate_transition`, so an activation without its history entry is impossible rather than merely refused after the fact; migration 00603's deferred `AU001` binding remains as the general rule for every audited entity.

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

## 8. Database enforcement (migration 00701)

The activation invariant is enforced in the database as well as in `internal/gates`, so that it holds against anything holding the application's credential and not only against an operator using the console.

**What `cp_app` can do to `capability_gates`:** `SELECT` any row (the checker reads one on every live-money call); `INSERT` a row, which a `BEFORE INSERT` trigger constrains to `DISABLED` with an empty approval chain, no evidence and no validity window; `UPDATE (version)` — the optimistic-concurrency counter, granted only because PostgreSQL requires the `UPDATE` privilege to take a row lock with `SELECT … FOR UPDATE`; and `EXECUTE` on `cp_gate_transition`.

**What `cp_app` cannot do:** change `state`, the approval chain (`approvers`, `proposed_by_user_id`), the evidence references, `evidence_hashes`, `effective_at`, `expires_at`, `revoked_at` or `revoke_reason` by any statement of its own; `DELETE` a gate row; `INSERT` into `capability_gate_transitions`; or become `cp_migrate`, which owns the function and the tables.

**`cp_gate_transition(…)`** is `SECURITY DEFINER`, owned by `cp_migrate`, with `search_path` pinned and `EXECUTE` revoked from `PUBLIC`. It takes an *operation* (`propose`, `approve`, `resume`, `activate`, `suspend`, `revoke`, `expire`), not a desired row: columns the operation does not own keep their stored value, so a caller cannot supply a rewritten approval chain, a cleared `revoked_at` or a widened window alongside a legal transition. It re-derives the legal-transition table (`cp_gate_can_transition`, mirroring `gates.CanTransition`), the dual-control rules and conditions 2–5 from the **stored** row, appends each chain entry to the stored chain after checking it names the acting principal, and writes the gate row and its `capability_gate_transitions` record in one call. Custom SQLSTATEs: `GT001` malformed request, `GT002` illegal transition, `GT003` evidence or window, `GT004` dual control, `GT005` a gate not born `DISABLED`.

**What this does and does not buy.** Reaching `ACTIVE` now requires three separate calls naming three distinct principals, each leaving an immutable transition row, with the evidence and window checked server-side at each step. The database cannot authenticate an operator — that is the session layer's job — so a holder of the application credential can still name principals of its choosing; what it can no longer do is hold an `ACTIVE` gate whose approval history is absent, incomplete, self-approved or inconsistent with the row. The migration role and any superuser remain able to change anything: this is a control against the application's credential, not against the database's owner.

The adversarial control is `TestIntegration_DatabaseRefusesForgedActivation` in `internal/gates/integration_test.go`, which issues each forgery as `cp_app`, and `TestIntegration_CapabilityGateStateAuthority` in `test/integration/migrations/privileges_test.go`, which pins the privileges so a later migration cannot quietly restore them.

## 9. Corrections to this document (2026-09-06)

Recorded plainly, because each was a claim the code did not support:

- §1 said "no single … database edit … can activate a capability alone". It could: `00150` granted `cp_app` table-wide `UPDATE` on `capability_gates` plus `INSERT` on `capability_gate_transitions`, so one transaction could write a forged approval chain and the transition row that satisfied `00603`'s `AU001` binding, and `gates.Evaluate` then read back exactly what had been written and reported the capability `ACTIVE`. On a deployment with no gate rows a single `INSERT` did the same. Migration `00701` closes both; §1 and §8 now describe what actually holds, and against whom.
- §1 said fresh deployments persist a `DISABLED` row for every capability at startup via `gates.Bootstrap`. `gates.Bootstrap` has no caller, so they do not. The default is still `DISABLED` because an absent row fails closed and because the database now refuses a gate row born in any other state, but the sentence was describing a startup step that does not run. Wiring it into the API's startup remains open.
- §2 listed `MARKETPLACE` as high-risk; a later edit claimed the code did not. **The later edit was the wrong one** — see the correction under §2. The original table was right, the correction was false, and the database was missing the capability entirely (F-43).
- §2 said the distinct-activator rule is a high-risk extra; it applies to every capability.
- §3 listed step-up as required for propose and revoke; `Admin.Propose` and `Admin.Revoke` do not require it (revocation reduces risk, and proposing changes nothing that is live).

## 10. Related

`docs/architecture/POLICY_AUTHORITY.md` §1–2, `docs/adr/0016-production-capability-gating.md`, `docs/build/BLOCKERS.md` (EB-001…EB-017), migrations `00150_capability_gates.sql`, `00603_state_change_binding.sql` and `00701_capability_gate_state_authority.sql`, package `internal/gates`.
